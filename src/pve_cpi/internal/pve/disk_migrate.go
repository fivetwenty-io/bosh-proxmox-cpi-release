// Cross-node persistent-disk migration (A2): when a stable-ID disk sits on
// one node and its VM runs on another, the attach path moves the disk instead
// of erroring — the CPI does what an operator would do by hand. The move
// rides a MOVER: a fresh single-purpose parker VM created on the disk's node,
// holding exactly this disk, offline-migrated to the target node through the
// PVE migrate API (a metadata move on shared storage, a volume copy on
// node-local storage). Isolating the disk onto its own mover first is what
// keeps sibling parked disks from traveling: a node's shared parker is
// durable infrastructure and never migrates.
//
// The mover is never started, carries the parker provenance tags plus its own
// mover tag, and is destroyed after the attach lands — through a guard that
// refuses to destroy a mover still referencing any volume, so a mover
// teardown can never take a disk with it. A crash at any point leaves a
// provenance-tagged mover the next attach adopts (same node: the ordinary
// reassignment transfer; cross-node: this flow, skipping the isolation step)
// or an operator cleans by hand.
package pve

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// DiskMoverTag marks a parker VM as a single-purpose migration mover, on top
// of the ordinary parker tags. The tag is what separates "a transient vehicle
// this flow created and may migrate and destroy" from "a node's durable
// parker that must never travel": every adopt, migrate, and destroy decision
// keys on it, so a shared parker can never be mistaken for a mover.
const DiskMoverTag = "bosh-disk-mover"

// defaultDiskMigrateAwaitBudget backs an unset DiskMigrationSpec.AwaitBudget.
// Matches the config package's defaultDiskMigrateCapMs (30 minutes).
const defaultDiskMigrateAwaitBudget = 30 * time.Minute

// moverIsolationSlot is the mover bus slot the isolation transfer targets. A
// fresh mover has no disks, so scsi0 is always free; an adopted mover skips
// the isolation entirely and keeps whatever slot the disk already occupies.
const moverIsolationSlot = "scsi0"

// TagsMarkDiskMover reports whether a PVE tag string carries the mover tag as
// a whole token, using the same tokenizer and case-blind comparison as every
// other parker tag decision (PVE lowercases tags on write).
func TagsMarkDiskMover(tags string) bool {
	for _, t := range splitTagString(tags) {
		if strings.EqualFold(t, DiskMoverTag) {
			return true
		}
	}
	return false
}

// DiskMigrationSpec is everything MigrateDiskViaMover needs to move one disk.
type DiskMigrationSpec struct {
	// Holder is the parker currently referencing the volume, on a node other
	// than TargetNode. A holder already carrying the mover tag is adopted (a
	// previous run crashed between isolation and destroy); any other parker
	// has the disk isolated off it onto a fresh mover first.
	Holder DiskHolder
	// TargetNode is the node the VM runs on — where the disk must end up.
	TargetNode string
	// Volid is the volume's current bare volid.
	Volid string
	// StableID is the disk's bpd- identity token. Required: the migration
	// renames node-local volumes, and the serial riding the drive entry is
	// what survives the rename.
	StableID string
	// DiskCID is the Director's verbatim encoded CID, recorded in the
	// mover's provenance entry.
	DiskCID string
	// SourceLocal is true when the disk's storage is node-local: the migrate
	// request then asks PVE to copy local disks, mapping each source storage
	// to the storage of the same name on the target node. False (shared
	// storage) makes the migration a pure metadata move.
	SourceLocal bool
	// Opts carries the disk's recorded drive-option overrides into the
	// mover's provenance entry, so a crash mid-flow does not lose them.
	Opts map[string]string
	// MaxAttempts bounds transient retries of the migrate request itself.
	// 0 falls back to DefaultDiskMigrateMaxAttempts.
	MaxAttempts int
	// AwaitBudget is the wall-clock bound on awaiting the migrate task. When
	// it is exhausted while the task still runs, the error is retriable and
	// says the migration continues server-side; the retried attach re-enters
	// this flow, finds the mover mid-migration or already landed, and
	// continues. 0 falls back to 30 minutes.
	AwaitBudget time.Duration
}

// MigrateDiskViaMover moves one parked stable-ID disk to spec.TargetNode and
// returns the mover now holding it there plus the volid the volume landed
// under. On return the disk is parked on the mover ON the target node — the
// caller finishes with the ordinary same-node reassignment transfer and then
// destroys the mover via DestroyEmptyMover.
//
// Re-entry safe: every step re-derives its state from the cluster, so a
// Director retry after a crash or an exhausted await budget resumes rather
// than duplicating work — an adopted mover skips isolation, a migration still
// running returns retriable, and a mover already on the target node skips the
// migrate call entirely.
func MigrateDiskViaMover(
	ctx context.Context, c Client, logger *log.Logger,
	spec DiskMigrationSpec, cfg ParkerConfig,
) (DiskHolder, string, error) {
	if c == nil {
		return DiskHolder{}, "", cpierrors.Cloud("MigrateDiskViaMover: client must not be nil")
	}
	if !spec.Holder.Found || !spec.Holder.IsParker {
		return DiskHolder{}, "", cpierrors.Cloud("MigrateDiskViaMover: holder is not a parker")
	}
	if spec.TargetNode == "" || spec.Volid == "" {
		return DiskHolder{}, "", cpierrors.Cloud("MigrateDiskViaMover: target node and volid are required")
	}
	if spec.StableID == "" {
		return DiskHolder{}, "", cpierrors.Cloud(
			"MigrateDiskViaMover: a stable ID is required; the migration renames node-local volumes, and a legacy CID is the volume name")
	}
	if spec.Holder.Node == spec.TargetNode {
		return DiskHolder{}, "", cpierrors.Cloud(
			"MigrateDiskViaMover: holder is already on node %s; use the same-node reassignment transfer", spec.TargetNode)
	}
	if spec.AwaitBudget <= 0 {
		spec.AwaitBudget = defaultDiskMigrateAwaitBudget
	}

	pctx := ParkContext{DiskCID: spec.DiskCID, StableID: spec.StableID, Opts: spec.Opts}

	mover := spec.Holder
	volid := spec.Volid
	if TagsMarkDiskMover(spec.Holder.Tags) {
		// A previous run already isolated the disk (and possibly started the
		// migration); adopt its mover rather than stacking a second one.
		if logger != nil {
			logger.Info("disk migrate: adopting an existing mover from an interrupted migration",
				log.Int("mover_vmid", mover.VMID),
				log.String("mover_node", mover.Node),
				log.String("volid", volid),
			)
		}
	} else {
		var isoErr error
		mover, volid, isoErr = isolateDiskOntoMover(ctx, c, logger, spec, cfg, pctx)
		if isoErr != nil {
			return DiskHolder{}, "", isoErr
		}
	}

	landedSlot, landedVolid, migErr := migrateMoverToNode(ctx, c, logger, mover, volid, spec, cfg, pctx)
	if migErr != nil {
		return DiskHolder{}, "", migErr
	}

	result := DiskHolder{
		Found:    true,
		VMID:     mover.VMID,
		Node:     spec.TargetNode,
		IsParker: true,
		Slot:     landedSlot,
		Tags:     buildMoverTags(cfg),
	}
	if logger != nil {
		logger.Info("disk migrate: disk landed on the target node",
			log.Int("mover_vmid", mover.VMID),
			log.String("target_node", spec.TargetNode),
			log.String("volid_before", spec.Volid),
			log.String("volid_after", landedVolid),
			log.String("slot", landedSlot),
		)
	}
	return result, landedVolid, nil
}

// isolateDiskOntoMover creates a fresh mover on the disk's node and moves the
// disk onto it by same-node reassignment — a metadata-only operation, so
// sibling disks on the shared parker never travel. The mover's provenance
// record is written BEFORE the move (the same intent-first ordering the
// detach-side transfer uses) and the shared parker's record is removed only
// after the mover's is finalized, so a crash anywhere in the window leaves at
// least one carrier of the disk's identity.
func isolateDiskOntoMover(
	ctx context.Context, c Client, logger *log.Logger,
	spec DiskMigrationSpec, cfg ParkerConfig, pctx ParkContext,
) (DiskHolder, string, error) {
	moverVMID, createErr := createMoverVM(ctx, c, logger, spec.Holder.Node, cfg)
	if createErr != nil {
		return DiskHolder{}, "", cpierrors.Wrap(createErr, "disk migrate: create mover")
	}

	// Intent record, strict: once the volume moves it is mover-named, and the
	// record (plus the serial riding the drive entry) is what the identity
	// scan finds after a crash.
	intent := buildParkerProvEntry(spec.Holder.Node, spec.Volid, moverIsolationSlot, cfg, pctx)
	if provErr := writeParkerProvenance(ctx, c, spec.Holder.Node, moverVMID, spec.StableID, intent); provErr != nil {
		destroyMoverBestEffort(ctx, c, logger, moverVMID, spec.Holder.Node, cfg)
		return DiskHolder{}, "", cpierrors.Wrap(provErr,
			fmt.Sprintf("disk migrate: write intent record on mover vmid %d (fail-closed: the record is the crash-window identity carrier)", moverVMID))
	}

	optStr := spec.Volid + ",serial=" + spec.StableID
	landed, terr := TransferDiskFromParker(ctx, c, logger, spec.Holder, moverVMID, moverIsolationSlot, spec.Volid, optStr, cfg)
	if terr != nil {
		// The disk may or may not have left the shared parker; the destroy
		// guard refuses a mover that holds it, so this cannot lose the disk.
		destroyMoverBestEffort(ctx, c, logger, moverVMID, spec.Holder.Node, cfg)
		return DiskHolder{}, "", cpierrors.Wrap(terr,
			fmt.Sprintf("disk migrate: isolate disk %s onto mover vmid %d", spec.Volid, moverVMID))
	}

	// Finalize the mover's record with the landed (mover-named) volid, then
	// drop the shared parker's entry — receiving side first, both
	// best-effort now that the serial rides the mover's drive entry.
	final := intent
	final.Volid = landed
	if provErr := writeParkerProvenance(ctx, c, spec.Holder.Node, moverVMID, spec.StableID, final); provErr != nil && logger != nil {
		logger.Warn("disk migrate: could not finalize the mover's provenance record (non-fatal; the drive serial is authoritative)",
			log.Int("mover_vmid", moverVMID),
			log.String("volid", landed),
			log.Err(provErr),
		)
	}
	RemoveParkerProvenanceEntry(ctx, c, logger, spec.Holder.Node, spec.Holder.VMID, spec.Volid, cfg)

	if logger != nil {
		logger.Info("disk migrate: disk isolated onto a fresh mover",
			log.Int("parker_vmid", spec.Holder.VMID),
			log.Int("mover_vmid", moverVMID),
			log.String("volid_before", spec.Volid),
			log.String("volid_after", landed),
		)
	}
	return DiskHolder{
		Found:    true,
		VMID:     moverVMID,
		Node:     spec.Holder.Node,
		IsParker: true,
		Slot:     moverIsolationSlot,
		Tags:     buildMoverTags(cfg),
	}, landed, nil
}

// migrateMoverToNode offline-migrates the mover to spec.TargetNode and
// converges the post-migration state: slot and landed volid re-derived from
// the target-side config by the drive serial, protection re-asserted, and the
// provenance record rewritten with the renamed volid and new node (the record
// is otherwise stale exactly when a crash makes it load-bearing).
//
// Idempotent entry points, in the order they are checked: a mover whose
// config already lives on the target node skips straight to convergence; a
// mover locked by an in-flight migration returns retriable; only a settled
// mover on the source node issues a new migrate request.
func migrateMoverToNode(
	ctx context.Context, c Client, logger *log.Logger,
	mover DiskHolder, volid string, spec DiskMigrationSpec, cfg ParkerConfig, pctx ParkContext,
) (string, string, error) {
	srcCfg, srcErr := c.QEMU().Config(ctx, mover.Node, mover.VMID)
	switch {
	case srcErr != nil && parkerConfigGone(srcErr):
		// Not on the source node any more: a previous run's migration
		// completed after its await budget ran out. Converge on the target.
		return convergeMigratedMover(ctx, c, logger, mover.VMID, spec, cfg, pctx)
	case srcErr != nil:
		return "", "", cpierrors.Wrap(WrapConfigReadError(srcErr),
			fmt.Sprintf("disk migrate: config read for mover vmid %d on node %s", mover.VMID, mover.Node))
	}

	if lock, _ := srcCfg["lock"].(string); lock == "migrate" {
		return "", "", cpierrors.Retriable(
			"disk migrate: mover vmid %d is mid-migration on the PVE side (config lock %q); the copy continues server-side — retry the attach and it completes once the migration lands",
			mover.VMID, lock)
	}

	if _, onBus := FindDiskIDByVolID(qemu.ParseDisks(srcCfg), volid); !onBus {
		return "", "", cpierrors.Retriable(
			"disk migrate: volume %q is not on an active slot of mover vmid %d (concurrent transfer?); re-resolve and retry",
			volid, mover.VMID)
	}

	// PVE's protection flag disables "the remove VM and remove disk
	// operations", and a local-disk migration removes the source copies once
	// they land on the target — so the flag comes down for the migrate and
	// goes back up as part of target-side convergence, the same bounded
	// window every parker transfer opens.
	if protErr := setParkerProtection(ctx, c, logger, mover.Node, mover.VMID, false); protErr != nil {
		return "", "", cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("disk migrate: clear protection on mover vmid %d", mover.VMID))
	}

	maxAttempts := spec.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultDiskMigrateMaxAttempts
	}
	params := &sdknodes.CreateQemuMigrateParams{Target: spec.TargetNode}
	if spec.SourceLocal {
		withLocal := true
		// "1" maps each source storage to itself: PVE storage IDs are
		// cluster-scoped, so the disk lands on the storage of the same name
		// on the target node.
		targetStorage := "1"
		params.WithLocalDisks = &withLocal
		params.Targetstorage = &targetStorage
	}
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return "", "", cpierrors.Cloud("disk migrate: nodes service not available")
	}
	var raw *sdknodes.CreateQemuMigrateResponse
	migErr := RetryOnTransient(ctx, logger, "disk_migrate", maxAttempts, func() error {
		var inner error
		raw, inner = nodesSvc.CreateQemuMigrate(ctx, mover.Node, strconv.Itoa(mover.VMID), params)
		return inner
	})
	if migErr != nil {
		reassertParkerProtection(context.WithoutCancel(ctx), c, logger, mover.Node, mover.VMID)
		return "", "", cpierrors.Wrap(WrapMutationError(migErr),
			fmt.Sprintf("disk migrate: migrate mover vmid %d from node %s to node %s", mover.VMID, mover.Node, spec.TargetNode))
	}

	var upid string
	if raw != nil {
		var upidErr error
		upid, upidErr = UPIDFromRaw(*raw)
		if upidErr != nil {
			return "", "", cpierrors.Wrap(upidErr, "disk migrate: parse migrate task UPID")
		}
	}
	if upid != "" {
		if logger != nil {
			logger.Info("disk migrate: offline migration task started",
				log.Int("mover_vmid", mover.VMID),
				log.String("source_node", mover.Node),
				log.String("target_node", spec.TargetNode),
				log.String("upid", upid),
				log.Bool("with_local_disks", spec.SourceLocal),
			)
		}
		if awaitErr := AwaitTaskWithLogger(ctx, c, mover.Node, upid, logger, WithMaxWait(spec.AwaitBudget)); awaitErr != nil {
			if isRetriableCPIError(awaitErr) {
				// Budget exhausted (or a transient poll fault) while the task
				// may still be running: the migration continues server-side.
				// Protection deliberately stays down — the retried attach
				// re-enters this flow and convergence re-asserts it once the
				// mover lands.
				return "", "", cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("disk migrate: the migrate task for mover vmid %d is still running server-side; the copy continues — retry the attach and it completes once the migration lands", mover.VMID))
			}
			// The task failed for real; the mover stayed on the source node.
			reassertParkerProtection(context.WithoutCancel(ctx), c, logger, mover.Node, mover.VMID)
			return "", "", cpierrors.Wrap(awaitErr,
				fmt.Sprintf("disk migrate: migrate task for mover vmid %d to node %s", mover.VMID, spec.TargetNode))
		}
	}

	return convergeMigratedMover(ctx, c, logger, mover.VMID, spec, cfg, pctx)
}

// convergeMigratedMover finishes a migration whose task has completed (this
// run or a previous one): it re-derives the disk's slot and renamed volid
// from the target-side config by the drive serial, re-asserts the protection
// flag the migrate window cleared, and rewrites the provenance record with
// the new node and volid.
func convergeMigratedMover(
	ctx context.Context, c Client, logger *log.Logger,
	moverVMID int, spec DiskMigrationSpec, cfg ParkerConfig, pctx ParkContext,
) (string, string, error) {
	var tgtCfg map[string]any
	readErr := RetryOnTransient(ctx, logger, "disk_migrate_converge", 0, func() error {
		var inner error
		tgtCfg, inner = c.QEMU().Config(ctx, spec.TargetNode, moverVMID)
		return inner
	})
	if readErr != nil {
		return "", "", cpierrors.Wrap(WrapConfigReadError(readErr),
			fmt.Sprintf("disk migrate: config read for mover vmid %d on target node %s after migration", moverVMID, spec.TargetNode))
	}

	// The migration renames node-local volumes for the target storage's
	// naming; the serial= drive option is the authoritative carrier, so the
	// slot and landed volid come from it rather than from anything recorded
	// pre-migration.
	slot, landed := "", ""
	for s, optStr := range qemu.ParseDisks(tgtCfg) {
		if serial, ok := StableIDFromDriveOptStr(optStr); ok && serial == spec.StableID {
			slot = s
			landed, _ = slotBareVolid(tgtCfg, s)
			break
		}
	}
	if slot == "" || landed == "" {
		return "", "", cpierrors.Cloud(
			"disk migrate: mover vmid %d is on node %s but no drive entry carries serial %s; inspect the mover's slots by hand before retrying",
			moverVMID, spec.TargetNode, spec.StableID)
	}

	reassertParkerProtection(ctx, c, logger, spec.TargetNode, moverVMID)

	entry := buildParkerProvEntry(spec.TargetNode, landed, slot, cfg, pctx)
	if provErr := writeParkerProvenance(ctx, c, spec.TargetNode, moverVMID, spec.StableID, entry); provErr != nil && logger != nil {
		logger.Warn("disk migrate: could not rewrite the mover's provenance record for its new node (non-fatal; the drive serial is authoritative)",
			log.Int("mover_vmid", moverVMID),
			log.String("node", spec.TargetNode),
			log.String("volid", landed),
			log.Err(provErr),
		)
	}
	return slot, landed, nil
}

// buildMoverTags is buildParkerTags plus the mover tag: a mover is a parker
// (every parker guard must fire for it) that is additionally marked as a
// transient migration vehicle.
func buildMoverTags(cfg ParkerConfig) string {
	return buildParkerTags(cfg) + ";" + DiskMoverTag
}

// createMoverVM allocates a VMID in the parker band and creates a fresh,
// never-started mover there: parker shape (protection=1, onboot=0, minimal
// resources), parker tags plus the mover tag.
//
// Unlike createParkerVM it never adopts a conflicting VM: a VMID conflict
// means an ordinary parker (or a concurrent mover) won that VMID, and
// adopting a shared parker as a mover would migrate a node's durable parker
// away with every disk it holds. Conflicts regenerate a fresh VMID instead.
func createMoverVM(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if node == "" {
		return 0, cpierrors.Cloud("createMoverVM: node must not be empty")
	}
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return 0, cpierrors.Cloud("createMoverVM: invalid VMID range [%d, %d]",
			cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	tags := buildMoverTags(cfg)
	protection := 1
	onboot := 0
	memory := 16
	cores := 1
	scsihw := "virtio-scsi-pci"

	return AllocateWithRetry(
		ctx,
		c,
		func(vmid int) error {
			params := map[string]any{
				"vmid":          vmid,
				"name":          parkerVMName(vmid),
				cfgKeyTags:      tags,
				paramProtection: protection,
				"onboot":        onboot,
				"memory":        memory,
				"cores":         cores,
				"scsihw":        scsihw,
			}
			var upid string
			var innerErr error
			retryErr := RetryOnTransientOrLock(ctx, logger, "disk_migrate_create_mover", 0, func() error {
				upid, innerErr = c.QEMU().Create(ctx, node, params)
				return innerErr
			})
			if retryErr != nil {
				return retryErr
			}
			if upid != "" {
				if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
					return cpierrors.Wrap(WrapMutationError(awaitErr),
						fmt.Sprintf("createMoverVM: await create task for vmid %d", vmid))
				}
			}
			return nil
		},
		// Regenerate-identity on conflict (see the doc comment): the loser of
		// a VMID race allocates a fresh VMID rather than adopting the winner.
		IsVMIDConflict,
		DefaultDiskMigrateMaxAttempts,
		WithRange(cfg.VMIDRangeStart, cfg.VMIDRangeEnd),
		WithStorageScan(node, cfg.DiskStorage),
	)
}

// destroyMoverBestEffort cleans up a mover a failed isolation leaves behind.
// It routes through DestroyEmptyMover, whose guard refuses a mover that holds
// any volume, so a partially-completed isolation can never lose the disk —
// the mover is then simply left for the next attach to adopt. Failures are
// logged, never returned: the caller is already unwinding its own error.
func destroyMoverBestEffort(ctx context.Context, c Client, logger *log.Logger, moverVMID int, node string, cfg ParkerConfig) {
	mover := DiskHolder{Found: true, VMID: moverVMID, Node: node, IsParker: true, Tags: buildMoverTags(cfg)}
	if err := DestroyEmptyMover(context.WithoutCancel(ctx), c, logger, mover); err != nil && logger != nil {
		logger.Warn("disk migrate: could not clean up the mover after a failed isolation; the next attach adopts it",
			log.Int("mover_vmid", moverVMID),
			log.String("node", node),
			log.Err(err),
		)
	}
}

// DestroyEmptyMover destroys a single-purpose migration mover once the disk
// it carried has moved off it. Fail-closed on every count that could cost a
// volume:
//
//   - only a VM whose own config carries BOTH the parker tag and the mover
//     tag is destroyed — a node's durable parker is never a valid target;
//   - a mover holding any volume, on an active slot or an unusedN entry, is
//     refused (the disk would ride the purge into deletion);
//   - a mover under a destructive config lock — an in-flight migration
//     included — is deferred with a retriable error.
//
// The destroy itself clears the protection flag, then deletes with
// purge=true and destroy-unreferenced-disks=false, so even a racing volume
// that appears between the check and the delete is not swept by VMID match.
// A mover that is already gone is idempotent success.
func DestroyEmptyMover(ctx context.Context, c Client, logger *log.Logger, mover DiskHolder) error {
	if c == nil {
		return cpierrors.Cloud("DestroyEmptyMover: client must not be nil")
	}
	if mover.VMID <= 0 || mover.Node == "" {
		return cpierrors.Cloud("DestroyEmptyMover: mover VMID and node are required")
	}
	if !TagsMarkDiskMover(mover.Tags) {
		return cpierrors.Cloud(
			"DestroyEmptyMover: vmid %d does not carry the %s tag; refusing to destroy what may be a durable parker",
			mover.VMID, DiskMoverTag)
	}

	vmCfg, cfgErr := c.QEMU().Config(ctx, mover.Node, mover.VMID)
	if cfgErr != nil {
		if parkerConfigGone(cfgErr) {
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(cfgErr),
			fmt.Sprintf("DestroyEmptyMover: config read for mover vmid %d", mover.VMID))
	}
	// The caller's tags came from a scan; the config is authoritative. Both
	// tags must be present, or this is not a mover no matter what the caller
	// believed.
	cfgTags, _ := vmCfg["tags"].(string)
	if !tagContainsParker(cfgTags) || !TagsMarkDiskMover(cfgTags) {
		return cpierrors.Cloud(
			"DestroyEmptyMover: vmid %d (node %s) config tags %q do not mark a migration mover; refusing to destroy it",
			mover.VMID, mover.Node, cfgTags)
	}
	if lock, _ := vmCfg["lock"].(string); isDestructiveDiskLock(lock) {
		return cpierrors.Retriable(
			"DestroyEmptyMover: mover vmid %d holds config lock %q (an in-flight operation); deferring the destroy",
			mover.VMID, lock)
	}
	if disks := qemu.ParseDisks(vmCfg); len(disks) > 0 {
		return cpierrors.Cloud(
			"DestroyEmptyMover: mover vmid %d still references volumes on active slots %v; refusing to destroy it",
			mover.VMID, sortedKeys(disks))
	}
	if unused := FindUnusedDiskEntries(vmCfg); len(unused) > 0 {
		return cpierrors.Cloud(
			"DestroyEmptyMover: mover vmid %d still references volumes on unused entries %v; refusing to destroy it",
			mover.VMID, sortedKeys(unused))
	}

	if protErr := setParkerProtection(ctx, c, logger, mover.Node, mover.VMID, false); protErr != nil {
		return cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("DestroyEmptyMover: clear protection on mover vmid %d", mover.VMID))
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return cpierrors.Cloud("DestroyEmptyMover: nodes service not available")
	}
	purge := true
	destroyDisks := false
	var resp *sdknodes.DeleteQemuResponse
	delErr := RetryOnTransientOrLock(ctx, logger, "disk_migrate_destroy_mover", parkerWindowMaxAttempts, func() error {
		var inner error
		resp, inner = nodesSvc.DeleteQemu(ctx, mover.Node, strconv.Itoa(mover.VMID), &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyDisks,
		})
		return inner
	})
	if delErr != nil {
		if IsNotFound(delErr) {
			return nil
		}
		return cpierrors.Wrap(WrapMutationError(delErr),
			fmt.Sprintf("DestroyEmptyMover: delete mover vmid %d", mover.VMID))
	}
	if resp != nil {
		upid, upidErr := UPIDFromRaw(*resp)
		if upidErr == nil && upid != "" {
			if awaitErr := AwaitTaskWithLogger(ctx, c, mover.Node, upid, logger); awaitErr != nil {
				return cpierrors.Wrap(WrapError(awaitErr),
					fmt.Sprintf("DestroyEmptyMover: destroy task for mover vmid %d", mover.VMID))
			}
		}
	}
	if logger != nil {
		logger.Info("disk migrate: mover destroyed after the attach landed",
			log.Int("mover_vmid", mover.VMID),
			log.String("node", mover.Node),
		)
	}
	return nil
}

// sortedKeys returns a map's keys in ascending order for stable error text.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
