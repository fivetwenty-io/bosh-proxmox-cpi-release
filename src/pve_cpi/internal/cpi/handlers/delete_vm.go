// Package handlers contains the 22 BOSH CPI v2 method implementations.
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
//  1. Parse vm_cid → vmid int.
//  2. Stop VM via qemu.Stop + pve.AwaitTask (idempotent on 404 → return nil).
//  3. Delete VM via nodes.DeleteQemu (purge=true, destroy-unreferenced-disks=true).
//  4. Call deps.Agent.Remove to clean up registry/cloud-init state.
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

		node := deps.Config.Node
		logger := deps.Logger.With(log.String("method", "delete_vm"), log.String("vm_cid", vmCID), log.Int("vmid", vmid))

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
				return nil, cpierrors.Wrap(awaitErr, fmt.Sprintf("delete_vm: await stop task for VM %s", vmCID))
			}
		}

		// --- guard: refuse to destroy if a persistent volume is still attached ---
		// PVE's PUT config delete:scsiN demotes a disk to unusedN rather than
		// fully clearing the config entry. A DELETE /qemu/{vmid} then destroys
		// every disk still referenced — unusedN included — silently nuking the
		// volume. The SDK's DetachDisk sweeps unusedN automatically, but this
		// guard catches future SDK regressions or any other code path that
		// reaches delete_vm with a dangling volume reference.
		diskStorage := deps.Config.DiskStorage
		if diskStorage != "" {
			cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
			if cfgErr != nil {
				if !pve.IsNotFound(cfgErr) {
					return nil, cpierrors.Wrap(pve.WrapError(cfgErr),
						fmt.Sprintf("delete_vm: read config for VM %s before destroy", vmCID))
				}
				// 404 here means the VM is gone — fall through to the destroy
				// call below, which handles the NotFound case idempotently.
			} else {
				var protected []string
				for slot, volid := range pve.FindUnusedDiskEntries(cfg) {
					storage, _, parseErr := pve.ParseDiskCID(volid)
					if parseErr != nil || storage != diskStorage {
						continue
					}
					protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
				}
				if len(protected) > 0 {
					return nil, cpierrors.Cloud(
						"delete_vm: refusing to destroy VM %s — persistent volumes still attached as unused slots on storage %q: %v (call detach_disk first)",
						vmCID, diskStorage, protected,
					)
				}
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
				logger.Info("delete_vm: VM not found during delete — already deleted, returning success")
				// Still call agent.Remove so registry/cloud-init state is cleaned up
				// if it exists; errors here are logged but not fatal.
				if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
					logger.Warn("delete_vm: agent.Remove failed after idempotent delete", log.Err(agentErr))
				}
				return nil, nil
			}
			return nil, cpierrors.Wrap(pve.WrapError(deleteErr), fmt.Sprintf("delete_vm: delete VM %s", vmCID))
		}
		_ = deleteResp // response is a raw JSON blob; no fields needed

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
