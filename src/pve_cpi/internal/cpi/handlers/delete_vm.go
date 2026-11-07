package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// tagDeletingVM is the PVE tag stamped on a VM before an async fast-path
// destroy, and by a create_vm rollback before its teardown. Any operator or
// script can list VMs tagged bosh-deleting to find those whose disposal was
// issued but has not yet completed. sweepDeferredTeardowns consumes this tag
// at the start of every create_vm and delete_vm and tears each such VM down
// through the teardown protocol, making the tag part of a self-draining work
// queue.
const tagDeletingVM = "bosh-deleting"

// stampDeletingTag reads the VM's current tags, merges in tagDeletingVM, and
// writes back via UpdateQemuConfig. Failures are logged but never propagated:
// the caller treats tagging as best-effort. Existing operator tags are
// preserved via mergeTagList.
//
// The tag read-modify-write runs under a per-VMID cluster lock so a concurrent
// set_vm_metadata or set_disk_metadata call cannot interleave and overwrite the
// bosh-deleting marker. Lock acquisition failure is best-effort: logged, then
// the RMW proceeds unlocked so the marker is never silently dropped.
func stampDeletingTag(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) {
	// stampRMW is the actual read-modify-write body, called under the lock when
	// available and directly (unlocked) when the pool service is absent.
	stampRMW := func() {
		// Best-effort read of existing tags. On failure existing is nil and
		// mergeTagList still adds tagDeletingVM as the sole entry.
		var existing []string
		if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
			if s, ok := pve.ConfigString(cfg, jsonKeyTags); ok {
				existing = parseTagsField(s)
			}
		}
		// hoistHeadTags keeps the ownership record ahead of the tail the
		// byte cap truncates, whatever order PVE returned the tags in.
		tags := mergeTagList(hoistHeadTags(existing), []string{tagDeletingVM}, maxTagLength)
		if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmcid.PathOf(vmid),
			&sdknodes.UpdateQemuConfigParams{Tags: &tags}); err != nil {
			logger.Warn("delete_vm: fast-path tag write failed (non-fatal; destroy will proceed)",
				log.String("vmid", vmCID), log.Err(err))
		}
	}

	lockOwner := fmt.Sprintf("stampDeletingTag/%d", vmid)
	lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmid, lockOwner, logger, func() error {
		stampRMW()
		return nil
	})
	if lockErr != nil {
		// Pool service unavailable or cluster fault: proceed unlocked so the
		// diagnostic tag is never silently skipped.
		logger.Warn("delete_vm: stampDeletingTag: could not acquire VMID lock; tagging without lock (best-effort)",
			log.String("vmid", vmCID), log.Err(lockErr))
		stampRMW()
	}
}

// fastPathDeleteVM executes the fast-path delete for a single VM. It:
//  1. Clears the VM's CPI-managed HA rules.
//  2. Stamps bosh-deleting on the current VM (best-effort, fail-open).
//  3. Issues Stop fire-and-forget (UPID discarded, no await).
//  4. Issues DeleteQemu with skiplock=true (handles running/locked VMs) without
//     awaiting the returned task UPID.
//  5. Returns immediately — no unbounded get_task poll on any task.
//
// The fast path intentionally does NOT run the pve.pool_reap_empty reaper: it
// is the bounded-time, "known-simple" path and the reaper needs a pre-destroy
// pool lookup plus a post-destroy task await, neither of which fit its
// fire-and-forget contract. An empty pool left behind by a fast-path delete
// is still reaped the next time a sync-path delete_vm empties it, or by an
// operator running `pvesh delete /pools/<id>` directly.
//
// skiplock=true is restricted to the root@pam superuser authenticated via
// password; PVE rejects it for any other identity regardless of granted
// privileges — including an API token owned by root@pam (see
// pve.IsRootPamIdentity's doc comment for why) — so the CPI must authenticate
// as root@pam with a password to clear locked VMs here.
// Eventual consistency: has_vm may briefly still see this VM after return.
func fastPathDeleteVM(
	ctx context.Context, deps Deps, haState *pve.HAServiceState, node, vmCID string, vmid int, logger *log.Logger,
) error {
	// Clear the VM's CPI-managed HA rules. Only a VM that is not HA-managed
	// reaches the fast path, so this deregisters nothing a stop depends on,
	// and the node-affinity pin it removes would otherwise outlive the VM,
	// because the fast path returns before the sync path's cleanup. The calls
	// are synchronous and bounded, with no task await. It runs before the
	// tag stamp, so a refusal leaves the VM untagged.
	if err := cleanupHAMembership(ctx, deps, haState, vmid, logger); err != nil {
		return err
	}

	// Stamp diagnostic tag — fail-open.
	stampDeletingTag(ctx, deps, node, vmCID, vmid, logger)

	// Fire-and-forget stop. The UPID is discarded; no await. PVE may not finish
	// the stop before destroy arrives; the destroy task can still fail on a
	// running VM, in which case the bosh-deleting stamp above queues this VM for
	// sweepDeferredTeardowns to reap at the start of a later create_vm or delete_vm.
	logger.Debug("delete_vm: fast-path: issuing stop fire-and-forget")
	_, stopErr := deps.PVE.QEMU().Stop(ctx, node, vmid)
	if stopErr != nil && !pve.IsNotFound(stopErr) {
		// Log but do not abort: the skiplock destroy handles a still-running VM.
		logger.Warn("delete_vm: fast-path: stop issued but returned error (non-fatal; destroy proceeds with skiplock)",
			log.Err(stopErr))
	}
	if pve.IsNotFound(stopErr) {
		// The node no longer holds the VM. Success only once the VM is proven
		// gone from the whole cluster; a VM that moved is retried.
		if goneErr := confirmVMGone(ctx, deps, "delete_vm: fast-path", vmid, node, "the stop"); goneErr != nil {
			return goneErr
		}
		logger.Info("delete_vm: fast-path: VM not found during stop and absent from every member — already deleted")
		if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
			logger.Warn("delete_vm: agent.Remove failed after fast-path not-found stop", log.Err(agentErr))
		}
		return nil
	}

	// Protect attached persistent disks before the skiplock destroy. Synchronous
	// config PUTs only — does not introduce an unbounded await. When this detach
	// fails, the VM stays tagged bosh-deleting with the disk still attached;
	// sweepDeferredTeardowns defers exactly that shape (it re-checks for
	// foreign active disks before destroying), so the disk survives until a
	// delete_vm retry detaches it.
	if protErr := detachForeignActiveDisks(ctx, deps, node, vmCID, vmid, logger); protErr != nil {
		return deleteVMDriveDeletePendingError(protErr, vmCID, deleteVMStopIssued)
	}
	// Preserve the VM's own ephemeral disk when retain_ephemeral_on_delete is set
	// by transferring its volume to a parker before the destroy. Returns
	// retained=true whenever the retain tag is present, including when an
	// earlier attempt already parked the disk. The retained flag gates
	// DestroyUnreferencedDisks below.
	retained, retainErr := detachRetainedEphemeralDisk(ctx, deps, node, vmCID, vmid, logger)
	if retainErr != nil {
		return deleteVMDriveDeletePendingError(retainErr, vmCID, deleteVMStopIssued)
	}
	// Same unusedN guard the sync path runs. Without it the fast-path purge would
	// still destroy a persistent volume left in an unusedN slot — e.g. a foreign
	// disk whose detach above demoted it to unusedN but could not sweep it
	// because a snapshot references the volume. guardUnusedVolumes existence-
	// probes the configured pve.disk_storage and fails closed on any volume it
	// cannot confirm deleted. A transfer to a parker that stopped after the
	// slot delete is finished first, from the parker's record.
	if guardErr := guardUnusedVolumesResumingTransfers(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage, logger); guardErr != nil {
		return guardErr
	}

	// Issue destroy with skiplock=true. Discard the UPID; no await.
	// The HA rule cleanup already ran before the stop above.
	// Purge's pool-membership interplay: see the synchronous-path DeleteQemu
	// comment in HandleDeleteVM.
	//
	// DestroyUnreferencedDisks is pve.destroy_unreferenced_disks (default
	// false; see the config field doc for the cross-cluster shared-storage
	// data-loss hazard it introduces when enabled) AND-ed with !retained, so
	// retain semantics always win regardless of the knob. The retention above
	// has already transferred the ephemeral volume to a parker, which renames
	// it for the parker's VMID, and so this destroy neither references nor
	// owns it. Forcing the flag off on that path is a second guard.
	// skiplock would let the destroy run through a migration's config lock,
	// so a VM that holds one defers instead.
	if lockErr := refuseSkiplockDuringMigration(ctx, deps, "delete_vm: fast-path", node, vmid, logger); lockErr != nil {
		return lockErr
	}
	logger.Debug("delete_vm: fast-path: issuing skiplock destroy without await")
	purge := true
	destroyDisks := deps.Config.DestroyUnreferencedDisks && !retained
	skiplock := true
	_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, vmcid.PathOf(vmid), &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyDisks,
		Skiplock:                 &skiplock,
	})
	if delErr != nil {
		if pve.IsNotFound(delErr) {
			if goneErr := confirmVMGone(ctx, deps, "delete_vm: fast-path", vmid, node, "the destroy"); goneErr != nil {
				return goneErr
			}
			logger.Info("delete_vm: fast-path: VM not found during destroy and absent from every member — already deleted")
			if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
				logger.Warn("delete_vm: agent.Remove failed after fast-path idempotent destroy", log.Err(agentErr))
			}
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(delErr), fmt.Sprintf("delete_vm: fast-path destroy VM %s", vmCID))
	}

	// Agent cleanup is best-effort; VM may still be tearing down asynchronously.
	cleanupAgentForVM(ctx, deps, node, vmid, logger)
	logger.Info("delete_vm: fast-path: destroy issued, returning without task await (eventual consistency)")
	return nil
}

// refuseStampedManagedDelete runs the ownership check for a stamped CID
// before the managed delete, which judges a VM by its VMID, its location, and
// its allocation marker. Every VM this CPI allocated passes those, so without
// this check a stamped CID that names another cluster, or an older VM whose
// VMID was reused, would reach this cluster's journaled VM and destroy it.
//
// It runs only when the allocation journal is configured, because the legacy
// path makes the same check itself. A bare CID is left to the journal, as it
// was before CIDs carried a stamp. A VMID proven absent is answered by
// refuseAbsentElsewhere, so a CID naming another cluster is refused and any
// other CID goes on to the managed delete, which closes its record. A present
// VM gets refuseVerdict's answers, which are VMNotFound for a token mismatch
// and a non-retriable refusal for a foreign VM. A failed read is retriable.
func refuseStampedManagedDelete(ctx context.Context, deps Deps, vmCID string, cid vmcid.VMCID) error {
	if !cid.Stamped() || deps.Config == nil ||
		deps.Config.StoragePlacementNamespace == "" && deps.Config.StorageAllocationJournalDir == "" {
		return nil
	}
	logger := deps.Log(ctx).With(log.String("vm_cid", vmCID), log.Int("vmid", cid.VMID))
	loc, err := findVMAuthoritative(ctx, deps, cid.VMID)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("delete_vm: locate VM %s", vmCID))
	}
	if !loc.Found || loc.Node == "" {
		return refuseAbsentElsewhere(ctx, deps, "delete_vm", vmCID, cid, logger)
	}
	verdict, err := checkVMOwnershipByConfig(ctx, deps, "delete_vm", cid, loc)
	if err != nil {
		return err
	}
	if verdict.Absent {
		return refuseAbsentElsewhere(ctx, deps, "delete_vm", vmCID, cid, logger)
	}
	return refuseVerdict("delete_vm", vmCID, cid, verdict, logger)
}

// HandleDeleteVM returns a handler for the delete_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//
// Logic:
//  1. Parse vm_cid -> vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     Not-found -> nil (idempotent; VM is already absent).
//     Transport error -> propagate.
//  3. Stop VM via qemu.Stop + pve.AwaitTask (idempotent on 404 -> return nil).
//  4. Delete VM via nodes.DeleteQemu (purge=true, destroy-unreferenced-disks=true).
//  5. Call deps.Agent.Remove to clean up registry/cloud-init state.
//
// Returns nil result on success (including when the VM was already absent).
func HandleDeleteVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		vmCID, cid, err := parseDeleteVMArgs(args)
		if err != nil {
			return nil, err
		}
		vmid := cid.VMID
		// Drain the queue of disposals an earlier request could not finish
		// before this delete starts. Best-effort and bounded.
		sweepDeferredTeardowns(ctx, deps, deps.Log(ctx))
		// The journal names a VM by its VMID alone, so a stamped CID's
		// cluster and token are checked before the journal is consulted.
		if err := refuseStampedManagedDelete(ctx, deps, vmCID, cid); err != nil {
			return nil, err
		}
		// A VM the allocation journal records is judged by the journal: the
		// managed delete refuses a live VM whose location or allocation
		// marker differs from the record, and only this CPI writes that
		// marker, so a foreign VM at the VMID never passes it. The ownership
		// guard covers the legacy path below, which has no record to check.
		if handled, managedErr := deleteManagedVMIfRecorded(ctx, deps, vmcid.PathOf(vmid), vmid); handled || managedErr != nil {
			return nil, managedDeleteVMError(managedErr)
		}

		return deleteLegacyVM(ctx, deps, vmCID, cid)
	})
}

// managedDeleteVMError decides what the Director reads when a journal-managed
// delete fails. A typed CPI error, such as an audit gate's findings or a lock
// timeout, and a refusal the CPI wrote itself, such as the one naming an
// unsettled step, keep their own text, because a retry is useless unless it
// says what refused it. Anything else, such as a journal write that failed or
// a PVE answer no classifier recognized, keeps the generic evidence line, so
// its backend text never reaches the Director.
func managedDeleteVMError(err error) error {
	var typed *cpierrors.Error
	var refusal *storageRefusalError
	if errors.As(err, &typed) || errors.As(err, &refusal) {
		return err
	}
	return storageDecisionSourceError(err)
}

func deleteLegacyVM(ctx context.Context, deps Deps, vmCID string, cid vmcid.VMCID) (any, error) {
	vmid := cid.VMID
	logger := deps.Log(ctx).With(log.String("vm_cid", vmCID), log.Int("vmid", vmid))

	// --- locate VM authoritatively ---
	// The cluster scan's hit is authoritative (correct even after an HA
	// failover), but its miss is not: /cluster/resources lags node-local
	// state by minutes on loaded clusters, and treating a stale miss as
	// "already deleted" makes the Director drop its record while the live
	// VM keeps running with its IP. FindVMAuthoritative therefore proves
	// absence on a miss with per-node config probes before this handler
	// may take the idempotent-success branch; any probe failure surfaces
	// retriable instead. Transport error -> propagate.
	logger.Debug("delete_vm: locating VM (cluster scan, per-node probes on miss)")
	loc, lookupErr := findVMAuthoritative(ctx, deps, vmid)
	if lookupErr != nil {
		return nil, cpierrors.Wrap(lookupErr, fmt.Sprintf("delete_vm: locate VM %s", vmCID))
	}
	node := loc.Node
	if !loc.Found || node == "" {
		// A stamped CID whose cluster stamp names another cluster is refused
		// here. Its VM may still run there, so answering success would drop
		// the Director's only record of it.
		if err := refuseAbsentElsewhere(ctx, deps, "delete_vm", vmCID, cid, logger); err != nil {
			return nil, err
		}
		logger.Info("delete_vm: VM absent from cluster index and every node's config probe — already deleted, returning success")
		// Best-effort agent cleanup: registry/cloud-init state may still exist.
		if agentErr := deps.Agent.Remove(ctx, deps.Config.Node, vmid); agentErr != nil {
			logger.Warn("delete_vm: agent.Remove failed after cluster-not-found", log.Err(agentErr))
		}
		return nil, nil
	}
	logger.Debug("delete_vm: VM located", log.String("node", node))

	// --- HA state gate ---
	// The gate reads only, so it runs before any config read: a VM on a dead
	// or fenced node answers its config read with a failure, and the HA state
	// is what says why. HA membership comes from the normalized HA state,
	// never from the pve.ha knobs, so a resource an operator registered by
	// hand counts too. The state table defers a VM in fence, on a node that is
	// not online, or in an unknown state, waits out HA motion, and runs the
	// recovery grace window, all before anything below stops, detaches, or
	// destroys it.
	haState, haErr := gateHADelete(ctx, deps, "delete_vm", vmid, node, logger)
	if haErr != nil {
		return nil, haErr
	}
	gatedNode := node

	// --- parker VM refusal (belt-and-braces PVE-level backstop) ---
	parkerLoc, alreadyGone, parkerErr := refuseIfParkerVM(ctx, deps, loc, vmid)
	if parkerErr != nil {
		return nil, parkerErr
	}
	if alreadyGone {
		// The VM's config is gone from the node the scan named, and two
		// sweeps of every member's config found it nowhere. A stamped CID
		// that names another cluster is refused here too, because the CID's
		// VM was never this one and may still run there.
		if err := refuseAbsentElsewhere(ctx, deps, "delete_vm", vmCID, cid, logger); err != nil {
			return nil, err
		}
		logger.Info("delete_vm: VM config gone from its node and absent from every member's config probe — already deleted, returning success")
		return nil, nil
	}
	if parkerLoc.Node != node {
		// The VM moved between the scan and the config read. The rest of
		// the delete runs against the node that holds it now.
		logger.Info("delete_vm: VM moved after the cluster scan, continuing on its current node",
			log.String("scan_node", node), log.String("node", parkerLoc.Node))
	}
	// A removed placement configuration cannot turn an allocated VM into a
	// legacy guest and bypass its durable cleanup authority. Both views count,
	// so a slot whose delete is pending still decides what the destroy takes.
	loc, holding, gone, holdErr := readLegacyDeleteHolding(ctx, deps, vmCID, cid, parkerLoc, logger)
	if holdErr != nil || gone {
		return nil, holdErr
	}
	node = loc.Node
	vmTags := loc.Tags
	liveConfig := holding.Config
	if node != gatedNode {
		// The VM moved after the gate read its HA state, so that state is
		// about the node it left. Read it again on the node that holds it now.
		haState, haErr = gateHADelete(ctx, deps, "delete_vm", vmid, node, logger)
		if haErr != nil {
			return nil, haErr
		}
	}

	// --- ownership guard ---
	// Before anything that stops, tags, or destroys the VM. The config just
	// read supplies the tags and notes, so the guard costs no extra call. A
	// token mismatch answers VMNotFound with no side effects, not even the
	// agent cleanup the proven-absence branch above runs, because the agent
	// state for this VMID belongs to whatever VM holds it now. Every other
	// foreign verdict is a refusal in both guard modes.
	if ownErr := refuseUnownedVM(ctx, deps, "delete_vm", vmCID, cid, loc, liveConfig, logger); ownErr != nil {
		return nil, ownErr
	}

	// The ownership guard above proved this VM is ours, so its config-drive ISO
	// may go even when a band move left the VMID outside every band. The
	// permit is set before any destroy, because the cluster stops holding the
	// VM once the destroy lands.
	ctx = pve.WithOwnedConfigDriveISO(ctx, vmid)

	if _, managed, markerErr := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(liveConfig)); markerErr != nil || managed {
		return nil, cpierrors.Cloud("delete_vm: allocation provenance requires its original storage journal authority")
	}
	// A legacy persistent disk named for this VM is one PVE counts as the
	// VM's own, so either destroy below would free it. Refusing here, before
	// the stop and before the fast path stamps bosh-deleting, leaves the VM
	// exactly as it was. That includes a disk only a pending value names,
	// because the destroy frees an owned drive from the pending section too.
	// A pending drive change on its own isn't refused here: the stop below
	// applies it, and the reads after the stop refuse while it's still there.
	if err := refuseOwnedLegacyDestroy(liveConfig, vmCID, vmid); err != nil {
		return nil, err
	}
	if err := refuseOwnedLegacyDestroy(holding.WithReplacements(), vmCID, vmid); err != nil {
		return nil, err
	}

	// --- per-node in-flight gate (opt-in; limit=0 → unlimited, no gating) ---
	if deps.Config != nil {
		inflightRelease, inflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
		if inflightErr != nil {
			return nil, cpierrors.Retriable("delete_vm: in-flight limit exceeded or context cancelled on node %s: %s", node, inflightErr.Error())
		}
		defer inflightRelease()
	}

	// --- fast-path branch: tag-and-return without terminal-state poll ---
	// When fast_path_delete is enabled the handler must return in bounded time
	// regardless of PVE task behaviour. This requires bypassing ALL unbounded
	// AwaitTaskWithLogger calls — including the stop-task await that
	// stopVMBeforeDelete performs. The fast path therefore takes its own stop
	// approach: issue Stop fire-and-forget (UPID discarded, no await), then
	// issue DeleteQemu with skiplock=true so PVE destroys the VM even if it is
	// still running or holds a config lock. The destroy call itself is NOT
	// awaited — the UPID is discarded and the handler returns immediately.
	//
	// skiplock=true is restricted to the root@pam superuser authenticated via
	// password — PVE rejects it for any other identity regardless of role or
	// privilege, including an API token owned by root@pam, so no ACL grant on
	// a least-privilege token (or any token at all) can enable it; run the
	// CPI as root@pam with a password to use it.
	//
	// Eventual consistency: a subsequent has_vm call may briefly still see the
	// VM while PVE's async destroy runs. The bosh-deleting tag marks VMs whose
	// fast-path destroy was issued but may not have completed; sweepDeferredTeardowns
	// reaps them at the start of the next create_vm or delete_vm (a self-draining
	// work queue), so a
	// stalled async destroy is retried automatically rather than left for manual
	// `qm destroy <vmid>` cleanup.
	//
	// The fast path bypasses the §7.15 operation-timeout envelope naturally:
	// no poll loop runs; the handler returns as soon as the destroy API call
	// returns (which itself is bounded by the HTTP transport timeout).
	//
	// An HA-managed VM never takes the fast path. Its stop goes through PVE HA
	// and must be awaited, or the CRM could start the VM again before the
	// destroy, and the fire-and-forget stop cannot wait for it.
	if deps.Config.FastPathDeleteEnabled() {
		if !haState.Managed() {
			if fpErr := fastPathDeleteVM(ctx, deps, haState, node, vmCID, vmid, logger); fpErr != nil {
				return nil, fpErr
			}
			cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)
			return nil, nil
		}
		logger.Info("delete_vm: VM is HA-managed, so it takes the synchronous delete path despite fast_path_delete",
			log.String("sid", haState.SID))
	}

	// Step 4 of the teardown runs the ownership guard and the owned-disk
	// refusal again on the config the owner node holds right before the
	// destroy, so nothing the stop or the detaches changed slips past them.
	owned := func(ctx context.Context, holding pve.QemuHolding) error {
		checkLoc := loc
		checkLoc.Node = node
		if err := refuseUnownedVM(ctx, deps, "delete_vm", vmCID, cid, checkLoc, holding.Config, logger); err != nil {
			return err
		}
		if err := refuseOwnedLegacyDestroy(holding.Config, vmCID, vmid); err != nil {
			return err
		}
		return refuseOwnedLegacyDestroy(holding.WithReplacements(), vmCID, vmid)
	}
	return deleteLegacyVMAndArtifacts(ctx, deps, haState, node, vmCID, vmid, vmTags, owned, logger)
}

// readLegacyDeleteHolding reads the holding of the VM that loc names. When the
// node holds no config for it, the VM may have moved since the scan, or the
// index row may be the stale one, so it locates the VM again before deciding
// anything. It returns gone when the VM is proven absent from every member,
// and the location of the VM it read otherwise.
func readLegacyDeleteHolding(
	ctx context.Context, deps Deps, vmCID string, cid vmcid.VMCID, loc pve.VMLocation, logger *log.Logger,
) (pve.VMLocation, pve.QemuHolding, bool, error) {
	vmid := cid.VMID
	node := loc.Node
	holding, configErr := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if configErr != nil && (pve.IsNotFound(configErr) || pve.IsPmxcfsConfigMissing(configErr)) {
		moved, lookErr := locateAfterConfigMissing(ctx, deps, "delete_vm: verify allocation provenance", vmid, node)
		if lookErr != nil {
			return loc, holding, false, lookErr
		}
		if !moved.Found {
			if err := refuseAbsentElsewhere(ctx, deps, "delete_vm", vmCID, cid, logger); err != nil {
				return loc, holding, false, err
			}
			logger.Info("delete_vm: VM config gone from its node and absent from every member's config probe — already deleted, returning success")
			return loc, holding, true, nil
		}
		// The probe read the whole config on the node that holds the VM
		// now, so its tags decide the parker check there.
		if pve.TagsMarkParker(moved.Tags) {
			return loc, holding, false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
		}
		logger.Info("delete_vm: VM moved after the cluster scan, continuing on its current node",
			log.String("scan_node", node), log.String("node", moved.Node))
		loc = moved
		holding, configErr = pve.ReadQemuHolding(ctx, deps.PVE, loc.Node, vmid)
	}
	if configErr != nil {
		return loc, holding, false, cpierrors.Retriable(
			"delete_vm: cannot verify allocation provenance for VM %s", vmCID)
	}
	return loc, holding, false, nil
}

// deleteLegacyVMAndArtifacts stops and destroys a legacy VM that the HA gate
// allowed. An HA-managed VM is stopped through PVE HA and keeps its HA
// resource until the destroy, which removes it with purge; its CPI-managed HA
// rules are cleaned up after the destroy. Any other VM is stopped directly.
func deleteLegacyVMAndArtifacts(
	ctx context.Context, deps Deps, haState *pve.HAServiceState, node, vmCID string, vmid int, vmTags string,
	owned func(context.Context, pve.QemuHolding) error, logger *log.Logger,
) (any, error) {
	// --- opt-in empty-pool reaper: capture membership BEFORE destroy ---
	// The /cluster/resources row for this vmid (and therefore its pool
	// membership) disappears once the VM is destroyed, and delete_vm has
	// no env.bosh to re-derive a pool name from -- so the lookup must run
	// here, before the teardown below.
	reapPool := capturePoolForReap(ctx, deps, vmid, logger)

	// --- teardown: stop (through PVE HA for an HA-managed VM), protect the
	//     disks that must outlive the VM, re-check it on its owner node,
	//     destroy with purge, and prove it absent (see vmTeardown) ---
	teardown := &vmTeardown{
		caller:              "delete_vm",
		vmCID:               vmCID,
		vmid:                vmid,
		node:                node,
		logger:              logger,
		gate:                teardownGated,
		haState:             haState,
		skiplock:            true,
		destroyUnreferenced: deps.Config != nil && deps.Config.DestroyUnreferencedDisks,
		preserve: func(ctx context.Context) (bool, error) {
			return preserveForLegacyDelete(ctx, deps, node, vmCID, vmid, logger)
		},
		owned: owned,
	}
	result, err := teardown.run(ctx, deps)
	if err != nil {
		return nil, err
	}
	if result.gone {
		if result.stage == teardownStageDestroy {
			cleanupAgentForVM(ctx, deps, node, vmid, logger)
		}
		cleanupHAMembershipAfterDestroy(ctx, deps, haState, vmid, logger)
		cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)
		return nil, nil
	}

	// --- HA anti-affinity / DLB / node-affinity pin cleanup (best-effort) ---
	// The destroy's purge removed the HA resource, so this only clears CPI
	// rules that still name the VM.
	cleanupHAMembershipAfterDestroy(ctx, deps, haState, vmid, logger)

	// --- opt-in empty-pool reaper: run AFTER the destroy has completed so
	//     PVE has already dropped this VM's pool membership. Never fails
	//     delete_vm -- every branch is logged and swallowed.
	reapEmptyPoolIfManaged(ctx, deps, reapPool, logger)

	// --- agent cleanup ---
	cleanupAgentForVM(ctx, deps, node, vmid, logger)

	// --- advertised-route SDN subnet cleanup (provenance-tagged, refcounted,
	//     entirely fail-open — see delete_vm_routes.go) ---
	cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)

	logger.Info("delete_vm: VM deleted successfully", log.String("node", node))
	return nil, nil
}

// preserveForLegacyDelete is step 3 of delete_vm's teardown. It detaches
// foreign-VMID volumes so the destroy cannot take them, retains the VM's own
// ephemeral disk when retain_ephemeral_on_delete is set, and refuses the
// destroy while an unusedN slot still holds a persistent volume, after
// finishing any transfer to a parker that stopped after the slot delete. A
// detach that is not guaranteed fails closed with a retriable error. It
// reports whether the ephemeral disk was retained, which is true whenever the
// retain tag is present, including when an earlier attempt already parked the
// disk, and which forces DestroyUnreferencedDisks off.
func preserveForLegacyDelete(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) (bool, error) {
	if protErr := detachForeignActiveDisks(ctx, deps, node, vmCID, vmid, logger); protErr != nil {
		return false, deleteVMDriveDeletePendingError(protErr, vmCID, deleteVMStopAwaited)
	}
	retained, retainErr := detachRetainedEphemeralDisk(ctx, deps, node, vmCID, vmid, logger)
	if retainErr != nil {
		return false, deleteVMDriveDeletePendingError(retainErr, vmCID, deleteVMStopAwaited)
	}
	if guardErr := guardUnusedVolumesResumingTransfers(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage, logger); guardErr != nil {
		return false, guardErr
	}
	return retained, nil
}

// parseDeleteVMArgs extracts and validates the vm_cid argument, returning the
// CID string and its parsed form.
func parseDeleteVMArgs(args []json.RawMessage) (string, vmcid.VMCID, error) {
	if len(args) < 1 {
		return "", vmcid.VMCID{}, cpierrors.Cloud("delete_vm: missing required argument vm_cid")
	}
	var vmCID string
	if err := json.Unmarshal(args[0], &vmCID); err != nil {
		return "", vmcid.VMCID{}, cpierrors.Cloud("delete_vm: vm_cid must be a string: %s", err.Error())
	}
	if vmCID == "" {
		return "", vmcid.VMCID{}, cpierrors.Cloud("delete_vm: vm_cid must not be empty")
	}
	cid, err := parseVMCIDArg("delete_vm", vmCID)
	if err != nil {
		return "", vmcid.VMCID{}, err
	}
	return vmCID, cid, nil
}

// refuseIfParkerVM is the belt-and-braces PVE-level backstop against deleting
// a parker VM. The Director should never hand a parker CID to delete_vm
// (parkers are internal CPI state), but `bosh delete-vm` and cloud-check's
// "delete VM reference" both reach this handler with an operator-supplied CID.
//
// Classification is by tag, not by band membership. The band is a
// configuration value and the tag is a fact about the VM: an operator who
// opts out of parking without carrying the band forward would otherwise
// disarm this guard over exactly the parkers their change stranded. The fast
// path issues skiplock=true + purge=true, which bypasses protection=1 and
// would destroy every disk that parker holds -- up to 31 of them, from
// deployments this call has nothing to do with.
//
// vmTags comes from the /cluster/resources?type=vm row the handler has already
// read to locate the VM, so the check normally costs nothing. PVE populates
// tags on that row (cleanupAdvertisedRoutes relies on the same field), and a
// non-empty value is authoritative. An empty one is not: it cannot distinguish
// a genuinely untagged VM from a PVE that omits the field, so it falls back to
// reading the config. Every parker the CPI creates is tagged, so on a
// CPI-managed cluster that fallback never fires.
//
// The fallback read happens at the node the scan named. A not-found or
// pmxcfs config-missing answer there does not prove the VM is gone, because
// it may have migrated since the scan. So the VM is located again with
// locateAfterConfigMissing, which requires the cluster to report the node
// able to answer, skips the contradicted index row, and needs two clean
// sweeps of every member and no HA hold on the VMID before it reports absence.
//
// It returns the location the rest of the delete must use, which is loc
// unless the VM moved, and alreadyGone=true only when the probe proved the VM
// absent. A probe failure, including a member the cluster reports offline,
// is retriable, because a success answer on partial evidence would drop the
// Director's record of a VM that may still run.
func refuseIfParkerVM(ctx context.Context, deps Deps, loc pve.VMLocation, vmid int) (pve.VMLocation, bool, error) {
	if deps.Config == nil {
		return loc, false, nil
	}
	if pve.TagsMarkParker(loc.Tags) {
		return loc, false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
	}
	if strings.TrimSpace(loc.Tags) != "" {
		// The row carried tags and they do not mark a parker: authoritative.
		return loc, false, nil
	}
	// No tags on the row. That is either a VM with no tags -- which no parker
	// ever is, since the CPI stamps bosh-cpi and bosh-parker at creation -- or a
	// PVE that does not populate the field, in which case the row proves
	// nothing and the config is the only place the answer lives. Read it. The
	// cost lands only on untagged VMs, which for a CPI-managed cluster is none,
	// and the alternative is a band-gated fallback that goes quiet for exactly
	// the operator who dropped the band while parkers were still standing.
	// Fail-closed: a transient config read error must NOT proceed to a purge.
	// Any doubt is retriable so the Director retries when PVE recovers.
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, loc.Node, vmid)
	if cfgErr != nil {
		if !pve.IsNotFound(cfgErr) && !pve.IsPmxcfsConfigMissing(cfgErr) {
			return loc, false, cpierrors.Retriable(
				"delete_vm: could not read config for VMID %d to verify parker status: %s (retry when PVE recovers)",
				vmid, cfgErr.Error())
		}
		moved, lookErr := locateAfterConfigMissing(ctx, deps, "delete_vm: verify parker status", vmid, loc.Node)
		if lookErr != nil {
			return loc, false, lookErr
		}
		if !moved.Found {
			return loc, true, nil
		}
		// The probe read the whole config on the node that holds the VM now,
		// so its tags are the config's tags.
		if pve.TagsMarkParker(moved.Tags) {
			return moved, false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
		}
		return moved, false, nil
	}
	tagsRaw, _ := pve.ConfigString(vmCfg, jsonKeyTags)
	if pve.TagsMarkParker(tagsRaw) {
		return loc, false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
	}
	return loc, false, nil
}

// cleanupHAMembership removes the VM's CPI-managed HA state: its
// anti-affinity or DLB rule membership and HA resource, and its per-VM
// node-affinity pin. It never deregisters an HA-managed VM before its stop:
// the synchronous delete stops such a VM through PVE HA and runs this cleanup
// after the destroy, and the fast path takes only VMs that are not HA-managed.
//
// It refuses, with a retriable error and without touching anything, when the
// HA state is not known or the service or its node is in fence. Removing a
// resource in fence leaves PVE with a fenced node and no service to recover,
// and the VM config stays stranded on the dead node.
//
// HA membership comes from the normalized HA state, not the pve.ha knobs, so a
// resource registered by hand is cleaned up too. The anti-affinity and DLB
// removal runs when the state shows the VM HA-managed, because only an HA
// resource can be a rule member. The node-affinity pin removal runs for every
// VM, because two writers create that rule, the AZ pin and the PCI strict pin,
// and delete_vm has no cloud_properties to tell which one ran. For a VM that
// never had a pin, it costs two cheap not-found no-ops.
//
// Past the refusal everything is best-effort: HA failures are logged and never
// block the delete.
func cleanupHAMembership(ctx context.Context, deps Deps, haState *pve.HAServiceState, vmid int, logger *log.Logger) error {
	if err := haRefusesMembershipCleanup(haState, vmid); err != nil {
		return err
	}
	if haState.Managed() {
		if aaErr := removeAntiAffinityMembership(ctx, deps, vmid, logger); aaErr != nil {
			logger.Warn("delete_vm: HA anti-affinity/DLB cleanup incomplete (non-fatal)", log.Err(aaErr))
		}
	}
	if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
		logger.Warn("delete_vm: HA node-affinity pin cleanup incomplete (non-fatal)", log.Err(pinErr))
	}
	return nil
}

// cleanupHAMembershipAfterDestroy runs cleanupHAMembership once the VM is gone,
// when the delete can no longer fail on it, so a refusal is only logged.
func cleanupHAMembershipAfterDestroy(ctx context.Context, deps Deps, haState *pve.HAServiceState, vmid int, logger *log.Logger) {
	if err := cleanupHAMembership(ctx, deps, haState, vmid, logger); err != nil {
		logger.Warn("delete_vm: HA membership cleanup after the destroy refused", log.Err(err))
	}
}

// stopVMBeforeDelete stops the VM and awaits the stop task.
// Returns (true, nil) when the caller should treat the operation as already complete
// (idempotent not-found paths, taken only once confirmVMGone proves the VM
// absent from the whole cluster). Returns (false, nil) on clean stop. Returns
// (false, err) on failure, including a not-found answer for a VM that moved.
func stopVMBeforeDelete(ctx context.Context, deps Deps, node string, vmid int, vmCID string, logger *log.Logger) (done bool, err error) {
	// Stop returns a UPID; if VM is not found (already deleted), treat as success.
	// Wrap in RetryOnTransient so a pvedaemon worker-recycle (HTTP 5xx /
	// "got no worker upid - start worker failed") under burst load is
	// absorbed in-process rather than surfacing as RetriableCloudError to
	// the director.
	logger.Debug("delete_vm: stopping VM")
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "delete_vm.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	if stopErr != nil {
		if pve.IsNotFound(stopErr) {
			if goneErr := confirmVMGone(ctx, deps, "delete_vm", vmid, node, "the stop"); goneErr != nil {
				return false, goneErr
			}
			logger.Info("delete_vm: VM not found during stop and absent from every member — already deleted, returning success")
			return true, nil
		}
		return false, cpierrors.Wrap(pve.WrapError(stopErr), fmt.Sprintf("delete_vm: stop VM %s", vmCID))
	}
	if stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			// A qmstop task can be accepted (Stop returns a UPID) yet fail
			// when the VM's config has already been removed — surfaced as
			// "unable to find configuration file for VM ...". The config may
			// have left the node in a migration rather than a delete, so this
			// is success only once the VM is proven gone from every member.
			if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
				if goneErr := confirmVMGone(ctx, deps, "delete_vm", vmid, node, "the stop task"); goneErr != nil {
					return false, goneErr
				}
				logger.Info("delete_vm: VM config missing during stop await and absent from every member — already deleted, returning success")
				if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
					logger.Warn("delete_vm: agent.Remove failed after idempotent stop-await", log.Err(agentErr))
				}
				return true, nil
			}
			return false, cpierrors.Wrap(pve.WrapError(awaitErr),
				fmt.Sprintf("delete_vm: await stop task for VM %s", vmCID))
		}
	}

	return false, nil
}

// waitForVMStopped polls the VM's current status until it reports "stopped"
// (or the VM disappears, which the destroy retry handles idempotently). Only
// called on the "VM is running - destroy failed" path — the common case never
// issues a status read. Bounded by deleteStopWaitBudget; polls every
// deleteStopPollInterval. On budget exhaustion returns a retriable error —
// delete_vm is idempotent, so the Director can safely retry once the VM
// settles.
func waitForVMStopped(ctx context.Context, deps Deps, node string, vmid int, vmCID string, logger *log.Logger) error {
	deadline := time.Now().Add(deleteStopWaitBudget())
	for {
		st, statusErr := deps.PVE.QEMU().Status(ctx, node, vmid)
		if statusErr != nil {
			if pve.IsNotFound(statusErr) {
				// VM gone — nothing left to wait for.
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(statusErr),
				fmt.Sprintf("delete_vm: status VM %s while waiting for stop", vmCID))
		}
		state, _ := pve.ConfigString(st, "status")
		if state == vmPowerStateStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return cpierrors.Retriable(
				"delete_vm: VM %s still %q after stop task completed (waited %s); retry when it settles",
				vmCID, state, deleteStopWaitBudget())
		}
		logger.Debug("delete_vm: waiting for VM to stop", log.String("state", state))
		select {
		case <-ctx.Done():
			return cpierrors.Retriable("delete_vm: context cancelled while waiting for VM %s to stop", vmCID)
		case <-time.After(deleteStopPollInterval()):
		}
	}
}

// guardUnusedVolumes reads the VM config and refuses to destroy if any unusedN
// slot still references a live persistent volume.
//
// PVE's PUT config delete:scsiN demotes a disk to unusedN rather than fully
// clearing the config entry. A DELETE /qemu/{vmid} then destroys every disk
// still referenced -- unusedN included -- silently nuking the volume. The
// SDK's DetachDisk sweeps unusedN automatically, but this guard catches future
// SDK regressions or any other code path that reaches delete_vm with a dangling
// volume reference.
//
// Belt-and-suspenders: we ALWAYS scan unusedN entries regardless of whether
// diskStorage is configured. When diskStorage is known we can run an existence
// probe and safely skip stale dangling references. When diskStorage is empty
// (operator did not configure pve.disk_storage) we cannot probe existence --
// we fail CLOSED to avoid silently nuking a live volume. A storage mismatch is
// treated identically: the slot may belong to an unrecognised storage pool we
// cannot probe, so it also fails closed.
//
// Returns nil on success (no protected volumes found or VM config is 404).
// Returns cpierrors.Cloud when protected volumes are present.
// Returns a wrapped error when the config read fails for reasons other than 404.
func guardUnusedVolumes(ctx context.Context, deps Deps, node, vmCID string, vmid int, diskStorage string) error {
	protected, err := protectedUnusedVolumes(ctx, deps, node, vmCID, vmid, diskStorage)
	if err != nil {
		return err
	}
	return unusedVolumesRefusal(vmCID, protected)
}

// unusedVolume is one unusedN entry the guard protects.
type unusedVolume struct {
	slot, volid string
}

// unusedVolumesRefusal is the guard's refusal over the protected entries, or
// nil when there are none.
func unusedVolumesRefusal(vmCID string, protected []unusedVolume) error {
	if len(protected) == 0 {
		return nil
	}
	named := make([]string, 0, len(protected))
	for _, entry := range protected {
		named = append(named, fmt.Sprintf("%s=%s", entry.slot, entry.volid))
	}
	return cpierrors.Cloud(
		"delete_vm: refusing to destroy VM %s -- persistent volumes still attached as unused slots: %v (call detach_disk first or verify pve.disk_storage configuration; "+
			"if detach_disk succeeds and the slot stays, do not remove the slot or destroy the VM by hand, because PVE then deletes a volume named for the VM; %s)",
		vmCID, named, unusedSlotRecoveryRunbook,
	)
}

// protectedUnusedVolumes is the guard's scan. It returns every unusedN entry
// on the VM whose volume the destroy could free, and nil when the VM is gone.
func protectedUnusedVolumes(ctx context.Context, deps Deps, node, vmCID string, vmid int, diskStorage string) ([]unusedVolume, error) {
	// Both views: a slot whose delete is pending is still in the config the
	// destroy works from, and a stop applies the delete, which can turn an
	// owned volume into an unused entry the destroy would free.
	holding, cfgErr := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if cfgErr != nil {
		if !pve.IsNotFound(cfgErr) && !pve.IsPmxcfsConfigMissing(cfgErr) {
			return nil, cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("delete_vm: read config for VM %s before destroy", vmCID))
		}
		// A 404 -- or pmxcfs's "Configuration file ... does not exist" 500,
		// which is the same condition wearing a different status code --
		// means this node holds no config for the VM. Only a VM proven gone
		// from every member falls through to the destroy call below, which
		// handles the NotFound case idempotently; one that moved is retried.
		if goneErr := confirmVMGone(ctx, deps, "delete_vm", vmid, node, "the config read before destroy"); goneErr != nil {
			return nil, goneErr
		}
		return nil, nil
	}
	if err := refusePendingDriveReplacement("delete_vm", vmCID, holding); err != nil {
		return nil, err
	}
	vmCfg := holding.Config

	// Only an unusedN slot whose volume STILL EXISTS represents a real
	// persistent disk that the DELETE below would silently destroy. PVE
	// demotes a disk to unusedN on detach; when a snapshot still
	// references the volume the SDK's sweep cannot remove that slot, so
	// it can linger. Once the volume itself is deleted (e.g. delete_disk
	// runs before delete_vm) the slot is a dangling reference pointing at
	// nothing -- destroying the VM cannot lose data, so it must not block
	// delete_vm. Existence-probe failures fail closed (treated as
	// present) so a transient error never green-lights destroying a live
	// volume. Probe on the VM's node: any volume still referenced by this
	// VM's config is reachable from there.
	// The absence proof classifies the storage before it will read a content
	// listing that omits a volume as an absence. Every slot below is probed
	// against the one configured disk storage, so they share one classifier,
	// which reads the storage index lazily and at most once. A loop whose
	// point probes all answer never reads it.
	classifySlotStorage := handlerStorageClassifier(deps, diskStorage)
	// The second opinions on an empty listing are built once for the whole
	// loop, for the same reason: every slot asks the same sources about the
	// same storage, and the allocation journal among them reads a file on
	// local disk to answer.
	slotCorroborators := emptyListingCorroborators(deps, nil)

	var protected []unusedVolume
	for slot, volid := range pve.FindUnusedDiskEntries(vmCfg) {
		storage, _, parseErr := pve.ParseDiskCID(volid)
		if parseErr != nil {
			// Unparseable volid -- skip; can't determine storage.
			deps.Log(ctx).Warn("delete_vm: unused-slot has unparseable volid -- skipping",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		if diskStorage == "" {
			// No configured disk storage: cannot probe existence.
			// Fail closed -- block destroy to avoid data loss.
			deps.Log(ctx).Warn("delete_vm: unused-slot present but pve.disk_storage not configured -- failing closed",
				log.String("slot", slot), log.String("volid", volid))
			protected = append(protected, unusedVolume{slot: slot, volid: volid})
			continue
		}
		if storage != diskStorage {
			// Storage doesn't match configured disk storage: we cannot
			// probe existence on an unknown pool. Fail closed.
			deps.Log(ctx).Warn("delete_vm: unused-slot storage does not match pve.disk_storage -- failing closed",
				log.String("slot", slot), log.String("volid", volid),
				log.String("slot_storage", storage), log.String("disk_storage", diskStorage))
			protected = append(protected, unusedVolume{slot: slot, volid: volid})
			continue
		}
		// ProveVolumeAbsent: block-backed storages return 500 with
		// "Failed to find logical volume" for missing LVs rather than 404,
		// and file storages return 500 naming volume_size_info for a stat
		// that did not work. The first folds through the tolerant point
		// probe and the second is settled by a storage content listing, so
		// a stale unused slot pointing at a deleted volume no longer wedges
		// delete_vm on either kind of storage.
		absent, probeErr := pve.ProveVolumeAbsent(ctx, deps.PVE, node, diskStorage, volid, classifySlotStorage,
			slotCorroborators...)
		if probeErr != nil {
			deps.Log(ctx).Warn("delete_vm: unused-slot volume existence probe failed -- treating slot as present (fail-closed)",
				log.String("slot", slot), log.String("volid", volid), log.Err(probeErr))
		} else if absent {
			deps.Log(ctx).Info("delete_vm: ignoring stale unused slot -- volume already deleted",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		protected = append(protected, unusedVolume{slot: slot, volid: volid})
	}
	return protected, nil
}

// unusedSlotRecoveryRunbook points the unused-slot refusal at the recovery for
// a stable-ID disk whose transfer to a parker stopped after its slot was
// deleted. Such a disk sits on an unused entry with no serial, so detach_disk
// can report success without finding it, and the obvious manual fix frees it.
// The docs do not ship in the release, so it names the repository as well as
// the file, and a test pins the heading it quotes.
const unusedSlotRecoveryRunbook = `see "delete_vm refuses to destroy VM with attached unused disks" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// detachForeignActiveDisks protects persistent disks the BOSH Director attached
// to this VM but has not yet detached (e.g. an interrupted recreate). PVE's
// DELETE /qemu/{vmid} with purge=true destroys EVERY disk referenced by the VM
// config, including persistent volumes on active bus slots (scsi1, ...). Such a
// volume is recognised two ways: by its embedded VMID label differing from the
// VM's own VMID (create_disk allocates persistent volumes under a synthetic
// free VMID, so a disk "zfs-1:vm-15689-disk-0" attached to VM 6031 is
// foreign), or by a bpd- stable-ID serial on the drive entry — a move_disk
// reassignment renames a persistent volume for its new owner, so a
// transferred disk fails the VMID heuristic and the serial is what still
// marks it persistent.
//
// A legacy foreign disk is detached: DetachDisk deletes the bus slot, and the
// volume stays on storage only because this VM does not own it. PVE creates an
// unusedN entry, which the SDK's sweep would delete and so free the volume,
// only when the volume's name carries this VMID, and a foreign disk's name
// carries another. A legacy disk named for this VM is not foreign by this
// test; deleteLegacyVM refuses it before this runs, and the stable-ID branch
// takes every renamed one. A stable-ID disk is instead transferred to a
// parker, because its volume is owner-named and the detach's unusedN sweep
// would let PVE deallocate it. Only after every foreign disk is off the VM does
// delete_vm proceed to destroy it.
//
// Fail-closed: if a foreign disk cannot be detached, or any foreign disk still
// remains on an active slot after the attempt, a RETRIABLE error is returned
// and the VM is NOT destroyed — a transient PVE error never escalates to silent
// data loss. The Director retries delete_vm. When the disk is still on its
// slot, the next attempt detaches or transfers it again. When a transfer
// stopped after deleting the slot of a volume this VM owns, the disk sits on
// an unused entry with no serial, and the next attempt's
// guardUnusedVolumesResumingTransfers finishes the move from the parker's
// transfer record. A volume this VM doesn't own leaves no entry, so the next
// attempt destroys the VM, and the parker's record leads the next disk call
// back to the volume.
func detachForeignActiveDisks(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) error {
	// Both views: the config endpoint leaves out a slot whose delete is
	// pending, while PVE's destroy works from the current config that still
	// has it, so a foreign disk a crash left that way still counts.
	holding, cfgErr := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if cfgErr != nil {
		if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
			// This node holds no config for the VM (404, or pmxcfs's
			// config-missing 500). A VM proven gone from every member leaves
			// nothing to detach, and the destroy path handles the absence
			// idempotently. A VM that moved keeps its disks attached on the
			// member that holds it now, so the delete is retried there.
			return confirmVMGone(ctx, deps, "delete_vm", vmid, node, "the config read before foreign-disk detach")
		}
		return cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("delete_vm: read config for VM %s before foreign-disk detach", vmCID))
	}
	if err := refusePendingDriveReplacement("delete_vm", vmCID, holding); err != nil {
		return err
	}
	cfg := holding.Config
	foreign := pve.FindForeignActiveDiskDetails(cfg, vmid)
	if len(foreign) == 0 {
		return nil
	}
	// The doomed VM's description is the disks' override-record carrier; it
	// dies with the VM, so any recorded overrides must move now or be lost.
	desc := pve.DescriptionFromConfig(cfg)
	slots := make([]string, 0, len(foreign))
	for slot := range foreign {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	// Every transfer below targets this one node, so the parker pool sweep
	// runs once after the loop rather than once per disk, and transferred
	// records whether there is anything for it to do.
	parkerCfg := parkerWriteConfigFor(deps)
	transferred := false
	for _, slot := range slots {
		entry := foreign[slot]
		// A stable-ID disk moves to a parker by reassignment instead of a
		// plain detach. Two reasons: after a reassignment the volume is named
		// for THIS VM, and the SDK detach's unusedN sweep would let PVE
		// deallocate a volume its holder owns — the exact data loss this
		// guard exists to prevent; and the disk's CID promises a parker
		// anchor, which a plain detach would leave unhonored.
		if entry.StableID != "" {
			logger.Warn("delete_vm: stable-ID persistent disk still attached on active slot -- transferring to a parker to preserve it before destroy",
				log.String("slot", slot), log.String("volid", entry.Volid), log.String("stable_id", entry.StableID))
			pctx := pve.ParkContext{
				DiskCID:     recordedDiskCID(desc, entry.StableID, entry.Volid),
				SourceVMCID: vmcid.PathOf(vmid),
				StableID:    entry.StableID,
				Opts:        pve.DiskOptOverlayFromDesc(desc, entry.StableID, entry.Volid),
			}
			_, transferErr := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, entry.Volid, parkerCfg, pctx)
			if elsewhere, ok := pve.IsDiskAttachedElsewhere(transferErr); ok {
				// The disk left this VM while the transfer waited for its
				// lock, and another VM holds it now, so the destroy can't
				// take it.
				logger.Warn("delete_vm: persistent disk left this VM for another one before the transfer moved it -- nothing to preserve here",
					log.String("slot", slot), log.String("stable_id", entry.StableID), log.Int("holder_vmid", elsewhere.VMID))
				continue
			}
			if transferErr != nil {
				return retriableUnlessPermanent(transferErr, fmt.Sprintf(
					"delete_vm: refusing to destroy VM %s -- could not transfer persistent disk %s=%s to a parker to preserve it, so nothing was destroyed. "+
						"A delete_vm retry transfers the disk again while it is still on its slot. If the slot was already deleted and this VM owns the volume, "+
						"the volume sits on an unused entry, and the retry finishes the move from the parker's transfer record, or refuses and leaves the volume there "+
						"when no record proves it is this disk. If this VM doesn't own the volume, PVE keeps no entry for it, so the retry destroys the VM, "+
						"and the next disk call for the disk finishes the move from the parker's record (%s)",
					vmCID, slot, entry.Volid, unusedSlotRecoveryRunbook))
			}
			transferred = true
			pve.RemoveAttachedDiskCID(ctx, deps.PVE, logger, node, vmid, entry.StableID, entry.Volid)
			continue
		}
		// A legacy foreign disk is plain-detached and left free-floating, and
		// its override record dies with this VM's description — there is no
		// carrier to move it to on this path.
		if lost := pve.DiskOptOverlayFromDesc(desc, entry.Volid); len(lost) > 0 {
			logger.Warn("delete_vm: recorded option overrides for this disk are lost with the VM -- re-apply them with update_disk after the next attach",
				log.String("slot", slot), log.String("volid", entry.Volid))
		}
		logger.Warn("delete_vm: persistent disk still attached on active slot -- detaching to preserve volume before destroy",
			log.String("slot", slot), log.String("volid", entry.Volid))
		// On the fast path the stop above is fire-and-forget, so the guest can
		// still be running here, and the detach goes through the pending-delete
		// helper like every other slot delete that can reach a running VM. A
		// pending delete ends the retry loop at once, because deleting the slot
		// again can't change it, and the caller gives it the delete_vm text.
		var pendingErr error
		detachErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.foreign_detach", 0, func() error {
			err := detachDriveSlot(ctx, deps, node, vmid, slot, entry.Volid)
			if _, pending := pve.IsDriveDeletePending(err); pending {
				pendingErr = err
				return nil
			}
			return err
		})
		if pendingErr != nil {
			return cpierrors.WrapAs(pendingErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"delete_vm: refusing to destroy VM %s -- could not detach persistent disk %s=%s to preserve it", vmCID, slot, entry.Volid))
		}
		if detachErr != nil {
			return cpierrors.Retriable(
				"delete_vm: refusing to destroy VM %s -- could not detach persistent disk %s=%s to preserve it: %s (the volume would otherwise be destroyed; retry re-attempts detach)",
				vmCID, slot, entry.Volid, detachErr.Error())
		}
	}
	if transferred {
		// Once, for the whole loop, because every transfer above landed on the
		// same node. A transfer that failed returned already, so a delete_vm
		// that stops halfway leaves its parkers unpooled until the Director
		// retries, which is the right trade for a membership that is cosmetic.
		sweepParkerPool(ctx, deps, node, parkerCfg)
	}
	// Re-read config: a detach that silently no-ops (SDK regression / race) must
	// not let the destroy take the volume while it is still on an active slot. A
	// detach that demoted the disk to unusedN but could not sweep it (a snapshot
	// reference blocks the sweep) is caught by the guardUnusedVolumes pass that
	// follows this call on BOTH the sync and fast paths.
	confirmHolding, confErr := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if confErr != nil {
		if pve.IsNotFound(confErr) {
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(confErr),
			fmt.Sprintf("delete_vm: re-read config for VM %s after foreign-disk detach", vmCID))
	}
	if err := refusePendingDriveReplacement("delete_vm", vmCID, confirmHolding); err != nil {
		return err
	}
	confirmCfg := confirmHolding.Config
	if remaining := pve.FindForeignActiveDisks(confirmCfg, vmid); len(remaining) > 0 {
		return cpierrors.Retriable(
			"delete_vm: refusing to destroy VM %s -- persistent disks still attached after detach attempt: %v (retry)",
			vmCID, remaining)
	}
	return nil
}

// detachRetainedEphemeralDisk transfers retained ephemeral volumes to parkers
// before destroying their original VM. Removing unused entries would physically
// delete VM-owned volumes, so retention never uses the old unused-slot sweep.
func detachRetainedEphemeralDisk(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) (bool, error) {
	return retainLegacyEphemeralDisks(ctx, deps, node, vmCID, vmid, logger)
}

// findEphemeralActiveDisks returns every (slot -> bare volid) on an active bus slot
// of cfg that pve.IsOwnEphemeralVolume reads as ownerVMID's own ephemeral volume,
// in the block or the file form. Foreign-VMID volumes are left for
// detachForeignActiveDisks. The retention loops over unused slots and recorded
// source CIDs use the same matcher, so all three read a volume's name the same
// way.
func findEphemeralActiveDisks(cfg map[string]any, ownerVMID int) map[string]string {
	out := make(map[string]string)
	for slot, optstr := range sdkqemu.ParseDisks(cfg) {
		if pve.IsOwnEphemeralVolume(optstr, ownerVMID) {
			bare, _, _ := strings.Cut(optstr, ",")
			out[slot] = bare
		}
	}
	return out
}

// awaitDeleteTask extracts the UPID from the DeleteQemu response and awaits the
// destroy task. An empty or null response means PVE completed synchronously;
// NotFound or PmxcfsConfigMissing during the poll is idempotent success once
// confirmVMGone proves the VM absent from every member, and retriable otherwise.
// Returns nil on success or idempotent not-found. Returns a wrapped error otherwise.
func awaitDeleteTask(ctx context.Context, deps Deps, node, vmCID string, vmid int, deleteResp *sdknodes.DeleteQemuResponse, logger *log.Logger) error {
	if deleteResp == nil {
		return nil
	}
	deleteUPID, upidErr := pve.UPIDFromRaw(*deleteResp)
	if upidErr != nil {
		// Malformed UPID is unexpected but non-fatal: the delete call
		// already succeeded; log and continue rather than fail the operation.
		logger.Warn("delete_vm: cannot parse UPID from delete response -- skipping await",
			log.Err(upidErr))
		return nil
	}
	if deleteUPID == "" {
		return nil
	}
	if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, deleteUPID, logger); awaitErr != nil {
		// NotFound or PmxcfsConfigMissing during destroy-await means the
		// node held no config for the VM by the time the task ran. That is
		// idempotent success only when the VM is gone from every member,
		// because a migration moves the config off the node the same way.
		if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
			if goneErr := confirmVMGone(ctx, deps, "delete_vm", vmid, node, "the destroy task"); goneErr != nil {
				return goneErr
			}
			logger.Info("delete_vm: VM config missing during destroy await and absent from every member -- treating as already deleted")
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(awaitErr),
			fmt.Sprintf("delete_vm: await destroy task for VM %s", vmCID))
	}
	return nil
}

// cleanupAgentForVM calls agent.Remove to purge registry/cloud-init state for
// the destroyed VM. Errors are non-fatal: the VM is already destroyed in PVE.
// The BOSH Director does not retry delete_vm on agent errors.
func cleanupAgentForVM(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) {
	logger.Debug("delete_vm: calling agent.Remove")
	if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
		logger.Warn("delete_vm: agent.Remove returned error (VM already destroyed)", log.Err(agentErr))
	}
}

// capturePoolForReap returns the VM's current pool membership for the
// post-destroy reaper, or "" when the reaper is disabled or the lookup fails.
// Only runs when pve.pool_reap_empty is enabled (the release default);
// best-effort: a lookup error just means the reaper no-ops after destroy.
func capturePoolForReap(ctx context.Context, deps Deps, vmid int, logger *log.Logger) string {
	if deps.Config == nil || !deps.Config.PoolReapEmpty {
		return ""
	}
	reapPool, _, poolErr := pve.FindVMPoolViaCluster(ctx, deps.PVE, vmid)
	if poolErr != nil {
		logger.Debug("delete_vm: pre-destroy pool lookup failed (non-fatal; reaper will no-op)", log.Err(poolErr))
		return ""
	}
	return reapPool
}

// reapEmptyPoolIfManaged deletes poolID when it is empty AND was created by
// this CPI (provenance comment per pve.IsCPIManagedPoolComment, which accepts
// the current prefix and the legacy pre-rename one), tolerating
// the two live PVE races this can hit. It is the delete_vm reaper for
// pve.pool_reap_empty (release default true; per-deployment pools go away
// with their deployments) and is called ONLY from the synchronous (non-fast-path)
// delete after the destroy task has been awaited, so PVE has already dropped
// the destroyed VM's pool membership by the time GetPoolComment/DeletePool run.
//
// No-ops (zero PVE calls) when:
//   - the reaper is disabled (deps.Config == nil or !PoolReapEmpty), or
//   - poolID == "" (the VM was not in any pool, or the pre-destroy lookup
//     failed and the caller already reset reapPool to ""), or
//   - deps.PVE.Pools() is nil (test fixtures / wiring gaps that never
//     configured a pool service), or
//   - poolID names the static vm_pool, the stemcell_template_pool, or the
//     resolved parker pool (shared long-lived pools, never reaped regardless
//     of emptiness or comment).
//
// Otherwise:
//  1. GetPoolComment(poolID): a lookup error or a not-found pool both return
//     immediately (logged at debug) -- nothing to reap.
//  2. Comment prefix check: a pool whose comment fails
//     pve.IsCPIManagedPoolComment (neither the current nor the legacy
//     provenance prefix) is an operator's own pool and is NEVER deleted by
//     the CPI, regardless of emptiness.
//  3. DeletePool(poolID):
//     - nil error: the pool was empty and CPI-managed -- reaped, logged Info.
//     - pve.IsPoolNotEmpty / pve.IsPoolNotFound: PVE returns HTTP 500 + text
//     for both (never 404 -- IsPoolNotFound is the substring classifier,
//     not the generic 404-based IsNotFound), covering the two expected
//     races: another VM joined the pool between destroy and this call, or
//     a concurrent delete_vm/operator action already removed the pool.
//     Both are tolerated at debug -- not a failure.
//     - any other error: logged at Warn. Every branch is non-fatal: the
//     reaper must never fail delete_vm, which has already destroyed the VM
//     by the time this runs.
func reapEmptyPoolIfManaged(ctx context.Context, deps Deps, poolID string, logger *log.Logger) {
	if deps.Config == nil || !deps.Config.PoolReapEmpty || poolID == "" {
		return
	}
	if deps.PVE == nil || deps.PVE.Pools() == nil {
		return
	}
	// The static vm_pool, the stemcell template pool, and the parker pool are
	// long-lived shared pools (create-if-missing at boot/first use) rather than
	// per-deployment ones. Reaping any of them would churn create/delete on
	// every last-VM delete and, for the stemcell pool, momentarily drop the ACL
	// boundary templates live behind. Refuse all three by name before any API
	// call.
	//
	// The parker pool cannot be reached here in normal operation, because the
	// reaper fires on a workload VM delete against that VM's own captured
	// membership and nothing ever deletes a parker. We refuse it anyway. It
	// costs one comparison and it closes a whole class of future accident, in
	// the same spirit as the two names that were already here. The comparison
	// is request-scoped, so a per-entry pve_parker_pool override is honored on
	// a delete routed to that entry. What it cannot see is a sibling entry's
	// parker pool, and that exposure is theoretical for the reason above.
	parkerPool := deps.Config.ParkerPoolValue()
	if poolID == deps.Config.VMPool || poolID == deps.Config.StemcellTemplatePool ||
		(parkerPool != "" && poolID == parkerPool) {
		logger.Debug("delete_vm: reaper: pool is the static vm_pool, the stemcell_template_pool, "+
			"or the parker pool; never reaped",
			log.String("pool", poolID))
		return
	}

	comment, found, err := deps.PVE.Pools().GetPoolComment(ctx, poolID)
	if err != nil {
		logger.Debug("delete_vm: reaper: GetPoolComment failed (non-fatal; not reaping)",
			log.String("pool", poolID), log.Err(err))
		return
	}
	if !found {
		logger.Debug("delete_vm: reaper: pool already gone before reap attempt",
			log.String("pool", poolID))
		return
	}
	if !pve.IsCPIManagedPoolComment(comment) {
		logger.Debug("delete_vm: reaper: pool not CPI-managed, not reaping",
			log.String("pool", poolID), log.String("comment", comment))
		return
	}

	// Pool deletes serialize on PVE's cluster-wide user_cfg lock, so a lock
	// timeout under concurrent deploys is contention, not a verdict; ride
	// RetryOnTransientOrLock instead of leaking the empty pool on the first
	// timeout. The two resolved verdicts (not empty, already gone) are
	// short-circuited to nil inside the closure so the retry budget is never
	// spent on them; the switch below still sees them via deleteErr. The
	// budget is the small sweep budget, not the full lock curve: this runs on
	// the request path after the destroy already succeeded, every branch
	// below is swallowed, and a leaked empty pool is cosmetic, so minutes of
	// backoff here would only slow delete_vm down.
	var deleteErr error
	_ = pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.pool_reap", cleanupSweepMaxAttempts, func() error {
		deleteErr = deps.PVE.Pools().DeletePool(ctx, poolID)
		if deleteErr != nil && (pve.IsPoolNotEmpty(deleteErr) || pve.IsPoolNotFound(deleteErr)) {
			return nil
		}
		return deleteErr
	})
	switch {
	case deleteErr == nil:
		logger.Info("delete_vm: reaper: reaped empty pool", log.String("pool", poolID))
	case pve.IsPoolNotEmpty(deleteErr) || pve.IsPoolNotFound(deleteErr):
		logger.Debug("delete_vm: reaper: pool not reaped -- still has members or already gone (race); tolerated",
			log.String("pool", poolID), log.Err(deleteErr))
	default:
		logger.Warn("delete_vm: reaper: empty-pool reap failed (non-fatal)",
			log.String("pool", poolID), log.Err(deleteErr))
	}
}
