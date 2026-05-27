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
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

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

		// --- stop VM ---
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
				return nil, nil
			}
			return nil, cpierrors.Wrap(pve.WrapError(stopErr), fmt.Sprintf("delete_vm: stop VM %s", vmCID))
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
					return nil, nil
				}
				return nil, cpierrors.Wrap(pve.WrapError(awaitErr),
					fmt.Sprintf("delete_vm: await stop task for VM %s", vmCID))
			}
		}

		// --- guard: refuse to destroy if a persistent volume is still attached ---
		// PVE's PUT config delete:scsiN demotes a disk to unusedN rather than
		// fully clearing the config entry. A DELETE /qemu/{vmid} then destroys
		// every disk still referenced -- unusedN included -- silently nuking the
		// volume. The SDK's DetachDisk sweeps unusedN automatically, but this
		// guard catches future SDK regressions or any other code path that
		// reaches delete_vm with a dangling volume reference.
		//
		// Belt-and-suspenders: we ALWAYS scan unusedN entries regardless of
		// whether diskStorage is configured. When diskStorage is known we can
		// run an existence probe and safely skip stale dangling references.
		// When diskStorage is empty (operator did not configure pve_disk_storage)
		// we cannot resolve which storage backs the volume, so we cannot probe
		// existence -- we fail CLOSED to avoid silently nuking a live volume.
		// A storage mismatch is treated identically: the slot may belong to an
		// unrecognised storage pool we cannot probe, so it also fails closed.
		diskStorage := deps.Config.DiskStorage
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
		if cfgErr != nil {
			if !pve.IsNotFound(cfgErr) {
				return nil, cpierrors.Wrap(pve.WrapError(cfgErr),
					fmt.Sprintf("delete_vm: read config for VM %s before destroy", vmCID))
			}
			// 404 here means the VM is gone -- fall through to the destroy
			// call below, which handles the NotFound case idempotently.
		} else {
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
			for slot, volid := range pve.FindUnusedDiskEntries(cfg) {
				storage, _, parseErr := pve.ParseDiskCID(volid)
				if parseErr != nil {
					// Unparseable volid -- skip; can't determine storage.
					logger.Warn("delete_vm: unused-slot has unparseable volid -- skipping",
						log.String("slot", slot), log.String("volid", volid))
					continue
				}
				if diskStorage == "" {
					// No configured disk storage: cannot probe existence.
					// Fail closed -- block destroy to avoid data loss.
					logger.Warn("delete_vm: unused-slot present but pve_disk_storage not configured -- failing closed",
						log.String("slot", slot), log.String("volid", volid))
					protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
					continue
				}
				if storage != diskStorage {
					// Storage doesn't match configured disk storage: we cannot
					// probe existence on an unknown pool. Fail closed.
					logger.Warn("delete_vm: unused-slot storage does not match pve_disk_storage -- failing closed",
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
					logger.Warn("delete_vm: unused-slot volume existence probe failed -- treating slot as present (fail-closed)",
						log.String("slot", slot), log.String("volid", volid), log.Err(existErr))
				} else if !exists {
					logger.Info("delete_vm: ignoring stale unused slot -- volume already deleted",
						log.String("slot", slot), log.String("volid", volid))
					continue
				}
				protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
			}
			if len(protected) > 0 {
				return nil, cpierrors.Cloud(
					"delete_vm: refusing to destroy VM %s -- persistent volumes still attached as unused slots: %v (call detach_disk first or verify pve_disk_storage configuration)",
					vmCID, protected,
				)
			}
		}

		// --- delete VM ---
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
		if deleteResp != nil {
			deleteUPID, upidErr := pve.UPIDFromRaw(*deleteResp)
			if upidErr != nil {
				// Malformed UPID is unexpected but non-fatal: the delete call
				// already succeeded; log and continue rather than fail the operation.
				logger.Warn("delete_vm: cannot parse UPID from delete response -- skipping await",
					log.Err(upidErr))
			} else if deleteUPID != "" {
				if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, deleteUPID, logger); awaitErr != nil {
					// NotFound or PmxcfsConfigMissing during destroy-await means
					// the VM was already gone by the time we polled -- idempotent.
					if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
						logger.Info("delete_vm: VM config missing during destroy await -- treating as already deleted")
					} else {
						return nil, cpierrors.Wrap(pve.WrapError(awaitErr),
							fmt.Sprintf("delete_vm: await destroy task for VM %s", vmCID))
					}
				}
			}
		}

		// --- agent cleanup ---
		logger.Debug("delete_vm: calling agent.Remove")
		if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
			// Agent errors are non-fatal: the VM is already destroyed in PVE.
			// Log at warn and continue; BOSH Director does not retry delete_vm on error.
			logger.Warn("delete_vm: agent.Remove returned error (VM already destroyed)", log.Err(agentErr))
		}

		logger.Info("delete_vm: VM deleted successfully")
		return nil, nil
	})
}
