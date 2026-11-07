package handlers

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// birthTagPrefix starts the birth tag every legacy create_vm writes with the
// VM's first workload tags, bosh-creating-<unix seconds>. A create that
// succeeds removes it as its last mutation, so a VM that still carries it is
// a failed or abandoned attempt, and the deferred-teardown sweep reaps it once
// it is older than any create could still be running.
const birthTagPrefix = "bosh-creating-"

// birthTagPattern matches a birth tag and nothing else, so a custom tag that
// only starts with the prefix never queues a VM for teardown.
var birthTagPattern = regexp.MustCompile(`^bosh-creating-(\d{1,19})$`)

// haReaddTagPrefix starts the tag of a VM whose HA re-registration has not
// finished. Such a VM is finished by the re-registration, never torn down by
// the sweep.
const haReaddTagPrefix = "bosh-ha-readd-"

// tagCreateFailed is the tag a failed create leaves on a VM it preserved.
const tagCreateFailed = "bosh-create-failed"

// sweepStragglersMaxPerSweep caps how many queued VMs one sweep tears down.
// The rest wait for the next sweep, which runs on every create_vm and
// delete_vm, so they converge without holding one request for more than this
// many teardowns.
const sweepStragglersMaxPerSweep = 5

// deferredTeardownSweepBudgetNs bounds one sweep, stored as nanoseconds so
// tests can shrink it race-safely. The sweep also leaves at least half of the
// request's own deadline to the request.
var deferredTeardownSweepBudgetNs atomic.Int64

func init() { deferredTeardownSweepBudgetNs.Store(int64(4 * time.Minute)) }

// SetDeferredTeardownSweepBudgetForTest replaces the sweep budget and returns
// a restore function. Test-only.
func SetDeferredTeardownSweepBudgetForTest(d time.Duration) func() {
	prev := deferredTeardownSweepBudgetNs.Swap(int64(d))
	return func() { deferredTeardownSweepBudgetNs.Store(prev) }
}

// birthTagFor renders the birth tag for t.
func birthTagFor(t time.Time) string {
	return birthTagPrefix + strconv.FormatInt(t.Unix(), 10)
}

// birthTagTime parses a birth tag and reports whether tag is one.
func birthTagTime(tag string) (time.Time, bool) {
	m := birthTagPattern.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// isBirthTag reports whether tag is a birth tag.
func isBirthTag(tag string) bool {
	_, ok := birthTagTime(tag)
	return ok
}

// withoutBirthTags returns entries with every birth tag removed.
func withoutBirthTags(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !isBirthTag(e) {
			out = append(out, e)
		}
	}
	return out
}

// withBirthTag places tag right behind the head tags of the semicolon list
// tags, the ownership tag and the owner and stamp tags, so the byte cap that
// truncates from the tail never drops it. It returns the new list and the
// entries the cap dropped to make room.
func withBirthTag(tags, tag string) (string, []string) {
	entries := hoistHeadTags(withoutBirthTags(parseTagsField(tags)))
	head := 0
	for head < len(entries) && (entries[head] == ownershipTag || hasHeadTagPrefix(entries[head])) {
		head++
	}
	ordered := make([]string, 0, len(entries)+1)
	ordered = append(ordered, entries[:head]...)
	ordered = append(ordered, tag)
	ordered = append(ordered, entries[head:]...)
	return mergeTagListReporting(ordered, nil, maxTagLength)
}

// deferredTeardownAge is how old a birth tag must be before the sweep treats
// its VM as abandoned: the create_vm operation timeout plus the HA motion
// wait, so the sweep never touches a create that another process still runs.
func deferredTeardownAge(deps Deps) time.Duration {
	return time.Duration(deps.Config.OperationTimeoutCreateSec()+deps.Config.HAMotionWaitSecValue()) * time.Second
}

// deferredTeardownQueue decides which tags queue a VM for the sweep.
type deferredTeardownQueue struct {
	now        time.Time
	age        time.Duration
	keepFailed bool
}

// queued reports whether tagsRaw queues its VM: bosh-deleting, or a birth tag
// older than the queue's age. A VM whose HA re-registration has not finished
// is never queued. With debug.keep_failed_vms on, a birth tag alone does not
// queue a VM, because the failed create may have meant to keep it and its
// keep tag write may not have landed.
func (q deferredTeardownQueue) queued(tagsRaw string) bool {
	tags := lowerTags(parseTagsField(tagsRaw))
	deleting := false
	expired := false
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, haReaddTagPrefix):
			return false
		case tag == tagCreateFailed && q.keepFailed:
			return false
		case tag == tagDeletingVM:
			deleting = true
		}
		if born, ok := birthTagTime(tag); ok && q.now.Sub(born) > q.age {
			expired = true
		}
	}
	return deleting || (expired && !q.keepFailed)
}

// stragglerRowForeign reports whether a queued row's own tags already prove
// the VM is not this Director's to reap: an owner tag naming another
// Director, two different owners, a parker, or a stemcell template. A row
// that proves none of these goes on to the full ownership check on the
// authoritative config.
func stragglerRowForeign(rowTags, requestUUID string) bool {
	tags := lowerTags(parseTagsField(rowTags))
	if contains(tags, pve.ParkerTag) || contains(tags, stemcellCacheTag) || contains(tags, stemcellMarkerTag) {
		return true
	}
	return otherOwner(tagValues(tags, ownerTagPrefix), strings.ToLower(sanitizeTagValue(requestUUID)))
}

// stragglerRow is the part of a /cluster/resources row the sweep reads.
type stragglerRow struct {
	Type string           `json:"type"`
	VMID sdkclient.PVEInt `json:"vmid"`
	Node string           `json:"node"`
	Tags string           `json:"tags"`
}

// candidate reports whether the row is a VM the sweep may consider: a QEMU
// guest with a VMID and a node whose tags queue it. Another Director's VM, a
// parker, or a template is never ours to reap, and the row's tags settle those
// before the row counts against the per-sweep cap, so a foreign VM cannot hold
// a slot on every sweep. The journal's protection is applied later, and only
// when a candidate exists, because reading the journal costs a cluster lock.
func (r stragglerRow) candidate(queue deferredTeardownQueue, requestUUID string) bool {
	if r.Type != resourceTypeQemu || r.VMID.Int() == 0 || r.Node == "" {
		return false
	}
	return queue.queued(r.Tags) && !stragglerRowForeign(r.Tags, requestUUID)
}

// stragglerOwned runs the full ownership check on a queued VM's
// authoritative config and reports whether the sweep may reap it. A legacy
// VM, one with bosh-cpi or Director notes and no owner tag, stays in scope,
// because nothing tells it apart from our own until it is stamped. Any other
// foreign verdict is left alone, and a check that cannot finish defers the VM
// to the next sweep.
func stragglerOwned(ctx context.Context, deps Deps, vmid int, loc pve.VMLocation, cfg map[string]any, sweepLogger *log.Logger) bool {
	verdict, err := checkVMOwnership(ctx, deps, "delete_vm", vmcid.VMCID{VMID: vmid}, loc, cfg)
	if err != nil {
		sweepLogger.Warn("deferred teardown sweep: could not check the VM's ownership; deferring it to the next sweep (non-fatal)",
			log.Err(err))
		return false
	}
	if !verdict.Owned {
		sweepLogger.Info("deferred teardown sweep: the VM is not owned by this Director; leaving it",
			log.String("ownership_reason", verdict.Reason),
			log.String("ownership_detail", verdict.Detail))
		return false
	}
	return true
}

// sweepContext bounds the sweep by its budget and by half of what remains of
// the request's deadline.
func sweepContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := time.Duration(deferredTeardownSweepBudgetNs.Load())
	if dl, ok := ctx.Deadline(); ok {
		if half := time.Until(dl) / 2; half < budget {
			budget = half
		}
	}
	return context.WithTimeout(ctx, budget)
}

// sweepDeferredTeardowns drains the queue of VMs a disposal could not finish:
// those that carry bosh-deleting, which the fast path and the rollback stamp,
// and those whose birth tag is older than any create could still be running.
// It runs at the start of every legacy create_vm and every delete_vm, in every
// HA mode. Each queued VM this Director owns goes through the teardown
// protocol without skiplock, with the HA state read once and no wait, and a VM
// the protocol defers stays queued for the next sweep. The sweep is:
//   - Best-effort: every failure is logged, never propagated.
//   - Bounded: at most sweepStragglersMaxPerSweep VMs, within its budget.
//   - Fail-closed: an index row is re-checked on the VM's authoritative
//     config, a VM the allocation journal names is left to the journal, and a
//     VM the protocol cannot prove safe to destroy is left as it is.
func sweepDeferredTeardowns(ctx context.Context, deps Deps, logger *log.Logger) {
	if deps.PVE == nil || deps.Config == nil || deps.PVE.Cluster() == nil {
		return
	}
	queue := deferredTeardownQueue{
		now: time.Now(), age: deferredTeardownAge(deps), keepFailed: deps.Config.KeepFailedVMsEnabled(),
	}
	// type=vm lists only the guests, which is every row the sweep can queue.
	guestType := "vm"
	resp, err := deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &guestType})
	if err != nil {
		logger.Warn("deferred teardown sweep: ListResources failed (non-fatal)", log.Err(err))
		return
	}
	if resp == nil {
		return
	}
	var rows []stragglerRow
	for _, raw := range *resp {
		var row stragglerRow
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		if row.candidate(queue, deps.RequestDirectorUUID) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return
	}
	protected, err := legacySweepJournalVMIDs(ctx, deps)
	if err != nil {
		logger.Warn("deferred teardown sweep: journal history unavailable; deferring the sweep", log.Err(err))
		return
	}
	sweepCtx, cancel := sweepContext(ctx)
	defer cancel()
	processed, deferred := 0, 0
	for _, row := range rows {
		if protected[int(row.VMID.Int())] {
			continue
		}
		if processed >= sweepStragglersMaxPerSweep || sweepCtx.Err() != nil {
			deferred++
			continue
		}
		processed++
		sweepDeferredTeardown(sweepCtx, deps, row, queue, logger.With(
			log.String("node", row.Node), log.String("vmid", strconv.FormatInt(row.VMID.Int(), 10))))
	}
	if deferred > 0 {
		logger.Info("deferred teardown sweep: cap or budget reached; remaining VMs deferred to the next sweep",
			log.Int("processed", processed), log.Int("deferred", deferred))
	}
}

// sweepDeferredTeardown tears down one queued VM through the protocol.
func sweepDeferredTeardown(
	ctx context.Context, deps Deps, row stragglerRow, queue deferredTeardownQueue, sweepLogger *log.Logger,
) {
	vmid := int(row.VMID.Int())
	vmCID := strconv.Itoa(vmid)
	// The authoritative config read comes first, before anything this VM
	// triggers. The index row that selected it can be minutes stale: the
	// queued VM may already be destroyed and the VMID reused by a live VM
	// that no tag queues.
	cfg, authTags, allowed := legacySweepConfig(ctx, deps, row.Node, vmid, queue, sweepLogger)
	if !allowed {
		return
	}
	loc := pve.VMLocation{Node: row.Node, Tags: authTags, Found: true, Type: row.Type}
	if !stragglerOwned(ctx, deps, vmid, loc, cfg, sweepLogger) {
		return
	}
	if lock, _ := pve.ConfigString(cfg, "lock"); strings.TrimSpace(lock) == vmLockMigrate {
		sweepLogger.Info("deferred teardown sweep: the VM holds a migrate lock; deferring it")
		return
	}
	// A VM holding a legacy persistent disk named for it would lose that disk
	// to the purge, and no retry can make the destroy safe. It stays queued
	// for the operator, before anything below changes it.
	if owned := pve.FindOwnedLegacyPersistentDisks(cfg, vmid); len(owned) > 0 {
		sweepLogger.Warn("deferred teardown sweep: a legacy persistent disk is named for this VM, so the destroy "+
			"would free it; leaving the VM for the operator", log.String("slots", ownedLegacySlots(owned)))
		return
	}
	// A foreign persistent disk on an active slot defers the VM before it is
	// stopped. The Director's delete_vm retry runs the real detach, including
	// the parker transfer, and a later sweep reaps the VM once it is empty.
	if foreign := pve.FindForeignActiveDisks(cfg, vmid); len(foreign) > 0 {
		sweepLogger.Warn("deferred teardown sweep: foreign persistent disks still attached; deferring to the "+
			"delete_vm retry to detach them", log.Any("slots", foreign))
		return
	}
	// The ownership check proved the VM ours, so its config-drive ISO may go
	// with it.
	ctx = pve.WithOwnedConfigDriveISO(ctx, vmid)
	teardown := &vmTeardown{
		caller:              "deferred teardown sweep",
		vmCID:               vmCID,
		vmid:                vmid,
		node:                row.Node,
		logger:              sweepLogger,
		gate:                teardownGateNoWait,
		destroyUnreferenced: deps.Config.DestroyUnreferencedDisks,
		preserve: func(ctx context.Context) (bool, error) {
			return preserveForSweep(ctx, deps, row.Node, vmCID, vmid, authTags, sweepLogger)
		},
		owned: func(ctx context.Context, holding pve.QemuHolding) error {
			return sweepStillOwned(ctx, deps, row, vmid, holding, queue, sweepLogger)
		},
	}
	result, err := teardown.run(ctx, deps)
	if err != nil {
		sweepLogger.Warn("deferred teardown sweep: the teardown did not finish; the VM stays queued for the next sweep",
			log.String("stage", string(result.stage)), log.Err(err))
		return
	}
	if !result.gone || result.stage == teardownStageDestroy {
		cleanupAgentForVM(ctx, deps, row.Node, vmid, sweepLogger)
	}
	cleanupHAMembershipAfterDestroy(ctx, deps, teardown.haState, vmid, sweepLogger)
	cleanupAdvertisedRoutes(ctx, deps, vmid, authTags, sweepLogger)
	sweepLogger.Info("deferred teardown sweep: VM torn down", log.Bool("already_gone", result.gone))
}

// preserveForSweep is step 3 of the sweep's teardown. It re-reads both config
// views and defers a VM that still holds a foreign persistent disk, keeps a
// retained ephemeral disk through the retain-ephemeral transfer, and refuses
// while an unusedN slot holds a persistent volume.
func preserveForSweep(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, authTags string, sweepLogger *log.Logger,
) (bool, error) {
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil && !pve.IsNotFound(err) && !pve.IsPmxcfsConfigMissing(err) {
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			"deferred teardown sweep: read the VM's config views before the destroy")
	}
	if err == nil {
		if foreign := pve.FindForeignActiveDisks(holding.WithReplacements(), vmid); len(foreign) > 0 {
			return false, cpierrors.Retriable(
				"deferred teardown sweep: VM %s holds foreign persistent disks on %v; deferring to the delete_vm retry",
				vmCID, foreign)
		}
	}
	retained := false
	if tagsContain(authTags, tagRetainEphemeral) {
		var retainErr error
		retained, retainErr = detachRetainedEphemeralDisk(ctx, deps, node, vmCID, vmid, sweepLogger)
		if retainErr != nil {
			return false, retainErr
		}
	}
	if err := guardUnusedVolumes(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage); err != nil {
		return false, err
	}
	return retained, nil
}

// sweepStillOwned is step 4's ownership check for the sweep: the config the
// owner node holds right before the destroy must still queue the VM, still be
// this Director's, and still hold no persistent disk the destroy would free.
func sweepStillOwned(
	ctx context.Context, deps Deps, row stragglerRow, vmid int, holding pve.QemuHolding,
	queue deferredTeardownQueue, sweepLogger *log.Logger,
) error {
	tags, _ := pve.ConfigString(holding.Config, jsonKeyTags)
	if !queue.queued(tags) {
		return cpierrors.Retriable(
			"deferred teardown sweep: VM %d no longer carries a teardown queue tag; leaving it", vmid)
	}
	loc := pve.VMLocation{Node: row.Node, Tags: tags, Found: true, Type: row.Type}
	if !stragglerOwned(ctx, deps, vmid, loc, holding.Config, sweepLogger) {
		return cpierrors.Retriable("deferred teardown sweep: VM %d is not proven this Director's; leaving it", vmid)
	}
	for _, cfg := range []map[string]any{holding.Config, holding.WithReplacements()} {
		if owned := pve.FindOwnedLegacyPersistentDisks(cfg, vmid); len(owned) > 0 {
			return cpierrors.Cloud(
				"deferred teardown sweep: VM %d holds legacy persistent disks named for it on %s; leaving it",
				vmid, ownedLegacySlots(owned))
		}
	}
	return nil
}

// legacySweepConfig reads a queued VM's authoritative config views and
// reports whether the sweep may go on with it. A VM already gone, a read that
// fails, a pending drive change that names a second volume, an allocation
// marker, and tags that no longer queue the VM all stop the sweep for it.
func legacySweepConfig(
	ctx context.Context, deps Deps, node string, vmid int, queue deferredTeardownQueue, sweepLogger *log.Logger,
) (map[string]any, string, bool) {
	// Both views, so a VM whose disk slot has a pending delete still shows
	// that disk to the sweep's guards.
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			sweepLogger.Debug("deferred teardown sweep: VM already gone")
			return nil, "", false
		}
		sweepLogger.Warn("deferred teardown sweep: config read failed; deferring the VM to the next sweep (non-fatal)",
			log.Err(err))
		return nil, "", false
	}
	if err := refusePendingDriveReplacement("deferred teardown sweep", strconv.Itoa(vmid), holding); err != nil {
		sweepLogger.Warn("deferred teardown sweep: a pending drive change names a second volume; deferring the VM "+
			"to the next sweep (non-fatal)", log.Err(err))
		return nil, "", false
	}
	cfg := holding.Config
	authTags, _ := pve.ConfigString(cfg, jsonKeyTags)
	if _, managed, markerErr := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(cfg)); markerErr != nil || managed {
		sweepLogger.Warn("deferred teardown sweep: allocation provenance requires journal reconciliation; skipping the VM")
		return nil, "", false
	}
	// Re-test the queue tags on the authoritative config, not the index row.
	// A row whose tags no longer hold on the config is stale: the VMID's
	// current occupant was never queued, so it must not be destroyed.
	if !queue.queued(authTags) {
		sweepLogger.Warn("deferred teardown sweep: index row is stale — the VM's authoritative config does not " +
			"queue it; skipping (VMID likely reused by a live VM)")
		return nil, "", false
	}
	return cfg, authTags, true
}
