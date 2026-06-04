package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// tagDeletingVM is the PVE tag stamped on a VM before an async fast-path destroy.
// It is a diagnostic marker: any operator or script can list VMs tagged
// bosh-deleting to find those whose fast-path destroy was issued but has not
// yet completed. The sweepFastDeleteStragglers function consumes this tag on
// subsequent fast-path deletes to re-issue destroy for any straggler VM, making
// the tag part of a self-draining work queue.
const tagDeletingVM = "bosh-deleting"

// stampDeletingTag reads the VM's current tags, merges in tagDeletingVM, and
// writes back via UpdateQemuConfig. Failures are logged but never propagated:
// the caller treats tagging as best-effort. Existing operator tags are
// preserved via mergeTagList.
func stampDeletingTag(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) {
	// Best-effort read of existing tags. On failure existing is nil and
	// mergeTagList still adds tagDeletingVM as the sole entry.
	var existing []string
	if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
		if v, ok := cfg["tags"]; ok {
			if s, ok := v.(string); ok {
				existing = parseTagsField(s)
			}
		}
	}
	tags := mergeTagList(existing, []string{tagDeletingVM}, maxTagLength)
	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID,
		&sdknodes.UpdateQemuConfigParams{Tags: &tags}); err != nil {
		logger.Warn("delete_vm: fast-path tag write failed (non-fatal; destroy will proceed)",
			log.String("vmid", vmCID), log.Err(err))
	}
}

// sweepFastDeleteStragglers scans the cluster for VMs carrying tagDeletingVM
// (bosh-deleting) and re-issues a skiplock+purge destroy for each one. This
// makes fast-path deletes self-healing: a VM whose async destroy stalled is
// reaped by the next fast-path delete call. The sweep is:
//   - Best-effort: all errors are logged, never propagated.
//   - Bounded: no AwaitTaskWithLogger call; the destroy is fire-and-forget.
//   - Idempotent: a VM already gone (IsNotFound) is silently skipped.
//
// Call from the fast path before issuing the current delete so a straggler
// accumulation does not grow unbounded across deployments.
func sweepFastDeleteStragglers(ctx context.Context, deps Deps, logger *log.Logger) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		return
	}
	resp, err := deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		logger.Warn("delete_vm: straggler sweep: ListResources failed (non-fatal)", log.Err(err))
		return
	}
	if resp == nil {
		return
	}

	type clusterItem struct {
		Type string `json:"type"`
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Tags string `json:"tags"`
	}

	purge := true
	destroyDisks := true
	skiplock := true
	for _, raw := range *resp {
		var item clusterItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type != "qemu" || item.VMID == 0 || item.Node == "" {
			continue
		}
		if !tagsContain(item.Tags, tagDeletingVM) {
			continue
		}
		vmIDStr := strconv.FormatInt(item.VMID, 10)
		sweepLogger := logger.With(
			log.String("node", item.Node),
			log.String("vmid", vmIDStr),
		)
		// Fire-and-forget: discard the UPID, no await.
		_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, item.Node, vmIDStr, &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyDisks,
			Skiplock:                 &skiplock,
		})
		if delErr != nil {
			if pve.IsNotFound(delErr) {
				sweepLogger.Debug("delete_vm: straggler sweep: VM already gone")
			} else {
				sweepLogger.Warn("delete_vm: straggler sweep: destroy failed (non-fatal)", log.Err(delErr))
			}
			continue
		}
		sweepLogger.Info("delete_vm: straggler sweep: re-issued destroy for bosh-deleting VM")
	}
}

// fastPathDeleteVM executes the fast-path delete for a single VM. It:
//  1. Runs sweepFastDeleteStragglers to reap any prior stalled fast-path destroys.
//  2. Stamps bosh-deleting on the current VM (best-effort, fail-open).
//  3. Issues Stop fire-and-forget (UPID discarded, no await).
//  4. Issues DeleteQemu with skiplock=true (handles running/locked VMs) without
//     awaiting the returned task UPID.
//  5. Returns immediately — no unbounded get_task poll on any task.
//
// skiplock=true requires root@pam or a token with Sys.Modify privilege.
// Eventual consistency: has_vm may briefly still see this VM after return.
func fastPathDeleteVM(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) error {
	// Reap any straggler fast-path VMs cluster-wide before issuing our own
	// destroy. Best-effort; never blocks the current delete.
	sweepFastDeleteStragglers(ctx, deps, logger)

	// Stamp diagnostic tag — fail-open.
	stampDeletingTag(ctx, deps, node, vmCID, vmid, logger)

	// Fire-and-forget stop. The UPID is discarded; no await. PVE may not finish
	// the stop before destroy arrives; skiplock=true on DeleteQemu handles that.
	logger.Debug("delete_vm: fast-path: issuing stop fire-and-forget")
	_, stopErr := deps.PVE.QEMU().Stop(ctx, node, vmid)
	if stopErr != nil && !pve.IsNotFound(stopErr) {
		// Log but do not abort: the skiplock destroy handles a still-running VM.
		logger.Warn("delete_vm: fast-path: stop issued but returned error (non-fatal; destroy proceeds with skiplock)",
			log.Err(stopErr))
	}
	if pve.IsNotFound(stopErr) {
		// VM already gone — clean up agent state and return success.
		logger.Info("delete_vm: fast-path: VM not found during stop — already deleted")
		if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
			logger.Warn("delete_vm: agent.Remove failed after fast-path not-found stop", log.Err(agentErr))
		}
		return nil
	}

	// Issue destroy with skiplock=true. Discard the UPID; no await.
	logger.Debug("delete_vm: fast-path: issuing skiplock destroy without await")
	purge := true
	destroyDisks := true
	skiplock := true
	_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyDisks,
		Skiplock:                 &skiplock,
	})
	if delErr != nil {
		if pve.IsNotFound(delErr) {
			logger.Info("delete_vm: fast-path: VM not found during destroy — already deleted")
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
//
//nolint:gocognit // Orchestration shell: locate+stop+guard+delete+await+agent-cleanup. Steps are individually simple; combined complexity is inherent to the idempotent delete contract.
func HandleDeleteVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// --- argument extraction ---
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_vm: missing required argument vm_cid")
		}
		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Cloud("delete_vm: vm_cid must be a string: %s", err.Error())
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("delete_vm: vm_cid must not be empty")
		}

		vmid, err := strconv.Atoi(vmCID)
		if err != nil {
			return nil, cpierrors.Cloud("delete_vm: vm_cid %q is not a valid integer VMID: %s", vmCID, err.Error())
		}
		if vmid <= 0 {
			return nil, cpierrors.Cloud("delete_vm: vm_cid %q must be a positive integer", vmCID)
		}

		logger := deps.Logger.With(log.String("method", "delete_vm"), log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- locate VM via cluster scan ---
		// Queries /cluster/resources for the authoritative node, correct even
		// after an HA failover. Not-found is idempotent for delete: the VM is
		// already gone, so clean up agent state and return success.
		// Transport error -> propagate.
		logger.Debug("delete_vm: locating VM via cluster scan")
		node, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(lookupErr), fmt.Sprintf("delete_vm: locate VM %s", vmCID))
		}
		if !found || node == "" {
			logger.Info("delete_vm: VM not found in cluster — already deleted, returning success")
			// Best-effort agent cleanup: registry/cloud-init state may still exist.
			if agentErr := deps.Agent.Remove(ctx, deps.Config.Node, vmid); agentErr != nil {
				logger.Warn("delete_vm: agent.Remove failed after cluster-not-found", log.Err(agentErr))
			}
			return nil, nil
		}
		logger.Debug("delete_vm: VM located", log.String("node", node))

		// --- per-node in-flight gate (opt-in; limit=0 → unlimited, no gating) ---
		if deps.Config != nil {
			inflightRelease, inflightErr := inflightSems.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
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
		// skiplock=true requires root@pam or a PVEAdmin-role token. Operators who
		// use a least-privilege token without Sys.Modify may need to grant it.
		//
		// Eventual consistency: a subsequent has_vm call may briefly still see the
		// VM while PVE's async destroy runs. The bosh-deleting tag marks VMs whose
		// fast-path destroy was issued but may not have completed; sweepFastDeleteStragglers
		// reaps them on the next fast-path delete (a self-draining work queue), so a
		// stalled async destroy is retried automatically rather than left for manual
		// `qm destroy <vmid>` cleanup.
		//
		// The fast path bypasses the §7.15 operation-timeout envelope naturally:
		// no poll loop runs; the handler returns as soon as the destroy API call
		// returns (which itself is bounded by the HTTP transport timeout).
		if deps.Config.FastPathDeleteEnabled() {
			return nil, fastPathDeleteVM(ctx, deps, node, vmCID, vmid, logger)
		}

		// --- stop VM (synchronous path) ---
		if stopDone, stopErr := stopVMBeforeDelete(ctx, deps, node, vmid, vmCID, logger); stopErr != nil {
			return nil, stopErr
		} else if stopDone {
			return nil, nil
		}

		// --- guard: refuse to destroy if a persistent volume is still attached ---
		if guardErr := guardUnusedVolumes(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage); guardErr != nil {
			return nil, guardErr
		}

		// --- HA anti-affinity / DLB cleanup (opt-in: anti_affinity.use_ha_rules or placement.dlb) ---
		// Remove the VM from any CPI-managed negative-affinity rule and
		// deregister its HA resource before destroying it. Also covers DLB-only
		// VMs: removeAntiAffinityMembership purges the HA resource and prunes any
		// associated rules; for a DLB-only VM with no affinity rule it simply
		// deregisters the HA resource. Keyed on vmid (the group name is unavailable
		// at delete time). Best-effort: HA failures are logged and never block VM deletion.
		if deps.Config.AntiAffinityUseHaRulesEnabled() || deps.Config.DLBConfigured() {
			if aaErr := removeAntiAffinityMembership(ctx, deps, vmid, logger); aaErr != nil {
				logger.Warn("delete_vm: HA anti-affinity/DLB cleanup incomplete (non-fatal)", log.Err(aaErr))
			}
		}

		// --- HA node-affinity pin cleanup (opt-in: placement.pin_az_via_ha_rules) ---
		// Remove the per-VM node-affinity pin rule and deregister its HA resource.
		// Keyed on vmid; idempotent and best-effort. Safe alongside the
		// anti-affinity cleanup above (both tolerate a not-found HA resource).
		if deps.Config.HANodeAffinityPinEnabled() {
			if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
				logger.Warn("delete_vm: HA node-affinity pin cleanup incomplete (non-fatal)", log.Err(pinErr))
			}
		}

		// --- delete VM (synchronous path) ---
		// Purge removes VMID from backup/HA/replication configs.
		// DestroyUnreferencedDisks removes orphaned volumes from storage.
		//
		// DestroyUnreferencedDisks=true triggers pvesm free under the
		// per-storage lockfile for every attached volume, so on bursty
		// deploys this can surface "can't lock file ... got timeout".
		// Retry on that signal; everything else propagates immediately.
		purge := true
		destroyDisks := true
		logger.Debug("delete_vm: deleting VM")
		var deleteResp *sdknodes.DeleteQemuResponse
		deleteErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_vm", 0, func() error {
			var innerErr error
			deleteResp, innerErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
				Purge:                    &purge,
				DestroyUnreferencedDisks: &destroyDisks,
			})
			return innerErr
		})
		if deleteErr != nil {
			if pve.IsNotFound(deleteErr) {
				logger.Info("delete_vm: VM not found during delete -- already deleted, returning success")
				// Still call agent.Remove so registry/cloud-init state is cleaned up
				// if it exists; errors here are logged but not fatal.
				if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
					logger.Warn("delete_vm: agent.Remove failed after idempotent delete", log.Err(agentErr))
				}
				return nil, nil
			}
			return nil, cpierrors.Wrap(pve.WrapError(deleteErr), fmt.Sprintf("delete_vm: delete VM %s", vmCID))
		}

		// Await the destroy task so the VM is fully purged from PVE before we
		// return. DeleteQemu returns a UPID as a json.RawMessage; an empty or
		// null response means PVE completed synchronously and no await is needed.
		if awaitErr := awaitDeleteTask(ctx, deps, node, vmCID, deleteResp, logger); awaitErr != nil {
			return nil, awaitErr
		}

		// --- agent cleanup ---
		cleanupAgentForVM(ctx, deps, node, vmid, logger)

		logger.Info("delete_vm: VM deleted successfully")
		return nil, nil
	})
}

// stopVMBeforeDelete stops the VM and awaits the stop task.
// Returns (true, nil) when the caller should treat the operation as already complete
// (idempotent not-found paths). Returns (false, nil) on clean stop. Returns (false, err) on failure.
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
			logger.Info("delete_vm: VM not found during stop — already deleted, returning success")
			return true, nil
		}
		return false, cpierrors.Wrap(pve.WrapError(stopErr), fmt.Sprintf("delete_vm: stop VM %s", vmCID))
	}
	if stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			// A qmstop task can be accepted (Stop returns a UPID) yet fail
			// when the VM's config has already been removed — surfaced as
			// "unable to find configuration file for VM ...". For delete_vm
			// that means the target is already gone, so treat it as success.
			if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
				logger.Info("delete_vm: VM config missing during stop await — already deleted, returning success")
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
// (operator did not configure pve_disk_storage) we cannot probe existence --
// we fail CLOSED to avoid silently nuking a live volume. A storage mismatch is
// treated identically: the slot may belong to an unrecognised storage pool we
// cannot probe, so it also fails closed.
//
// Returns nil on success (no protected volumes found or VM config is 404).
// Returns cpierrors.Cloud when protected volumes are present.
// Returns a wrapped error when the config read fails for reasons other than 404.
func guardUnusedVolumes(ctx context.Context, deps Deps, node, vmCID string, vmid int, diskStorage string) error {
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		if !pve.IsNotFound(cfgErr) {
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("delete_vm: read config for VM %s before destroy", vmCID))
		}
		// 404 here means the VM is gone -- fall through to the destroy
		// call below, which handles the NotFound case idempotently.
		return nil
	}

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
	var protected []string
	for slot, volid := range pve.FindUnusedDiskEntries(vmCfg) {
		storage, _, parseErr := pve.ParseDiskCID(volid)
		if parseErr != nil {
			// Unparseable volid -- skip; can't determine storage.
			deps.Logger.Warn("delete_vm: unused-slot has unparseable volid -- skipping",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		if diskStorage == "" {
			// No configured disk storage: cannot probe existence.
			// Fail closed -- block destroy to avoid data loss.
			deps.Logger.Warn("delete_vm: unused-slot present but pve_disk_storage not configured -- failing closed",
				log.String("slot", slot), log.String("volid", volid))
			protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
			continue
		}
		if storage != diskStorage {
			// Storage doesn't match configured disk storage: we cannot
			// probe existence on an unknown pool. Fail closed.
			deps.Logger.Warn("delete_vm: unused-slot storage does not match pve_disk_storage -- failing closed",
				log.String("slot", slot), log.String("volid", volid),
				log.String("slot_storage", storage), log.String("disk_storage", diskStorage))
			protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
			continue
		}
		// ExistsTolerant: block-backed storages return 500 with
		// "Failed to find logical volume" for missing LVs rather
		// than 404. Treat those as "volume gone" so a stale unused
		// slot pointing at a deleted volume does not wedge delete_vm.
		exists, existErr := pve.ExistsTolerant(ctx, deps.PVE, node, diskStorage, volid)
		if existErr != nil {
			deps.Logger.Warn("delete_vm: unused-slot volume existence probe failed -- treating slot as present (fail-closed)",
				log.String("slot", slot), log.String("volid", volid), log.Err(existErr))
		} else if !exists {
			deps.Logger.Info("delete_vm: ignoring stale unused slot -- volume already deleted",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
	}
	if len(protected) > 0 {
		return cpierrors.Cloud(
			"delete_vm: refusing to destroy VM %s -- persistent volumes still attached as unused slots: %v (call detach_disk first or verify pve_disk_storage configuration)",
			vmCID, protected,
		)
	}
	return nil
}

// awaitDeleteTask extracts the UPID from the DeleteQemu response and awaits the
// destroy task. An empty or null response means PVE completed synchronously;
// NotFound or PmxcfsConfigMissing during the poll is treated as idempotent success.
// Returns nil on success or idempotent not-found. Returns a wrapped error otherwise.
func awaitDeleteTask(ctx context.Context, deps Deps, node, vmCID string, deleteResp *sdknodes.DeleteQemuResponse, logger *log.Logger) error {
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
		// NotFound or PmxcfsConfigMissing during destroy-await means
		// the VM was already gone by the time we polled -- idempotent.
		if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
			logger.Info("delete_vm: VM config missing during destroy await -- treating as already deleted")
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
