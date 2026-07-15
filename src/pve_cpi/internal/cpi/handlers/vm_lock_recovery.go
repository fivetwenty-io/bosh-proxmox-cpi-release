// Shared root@pam-password-only skiplock recovery for VMs stuck under a PVE
// guest-config lock (lock: clone|create|backup|migrate|snapshot|rollback).
// PVE's HTTP API has no unlock endpoint; the only in-process recovery is a
// skiplock=true retry, which PVE honors only for the root@pam superuser
// authenticated via password (PVE::API2::Qemu rejects it for every other
// identity regardless of granted privileges — including an API token owned
// by root@pam; see pve.IsRootPamIdentity's doc comment for why). Used by
// cleanupVM (create_vm.go rollback) and the delete_vm.go synchronous destroy
// path — both call into these helpers so the retry behavior, logging, and
// operator-facing recovery message stay identical across both call sites.
package handlers

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// retryDestroyWithSkiplock retries a lock-rejected DeleteQemu with
// skiplock=true, but only when the CPI's configured PVE identity is
// root@pam authenticated via password (pve.IsRootPamIdentity). Any other
// identity — including an API token owned by root@pam — never qualifies, so
// origErr is returned unretried: PVE would reject skiplock=true for that
// identity anyway, and attempting it would only add a guaranteed-to-fail API
// call and a confusing log line. The caller's pve.IsVMConfigLocked check
// still fires against the SAME unretried error and drives its
// actionable-error / tagging path.
func retryDestroyWithSkiplock(
	ctx context.Context, deps Deps, node, vmCID string, vmid int,
	purge, destroyUnref bool, origErr error, logger *log.Logger,
) (*sdknodes.DeleteQemuResponse, error) {
	if !pve.IsRootPamIdentity(deps.Config) {
		return nil, origErr
	}
	lockType := pve.VMConfigLockType(origErr)
	logger.Warn("destroy rejected -- VM config is locked; retrying with skiplock (root@pam)",
		log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("lock_type", lockType))
	skiplock := true
	return deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyUnref,
		Skiplock:                 &skiplock,
	})
}

// retryStopWithSkiplock is the Stop analogue of retryDestroyWithSkiplock. The
// higher-level qemu.Service.Stop used elsewhere has no params variant, so the
// retry goes through the raw nodes endpoint (POST
// /nodes/{node}/qemu/{vmid}/status/stop), which does expose Skiplock. Returns
// the UPID (empty string for a synchronous completion) on success, or
// origErr unretried when identity does not qualify (not root@pam via
// password — see retryDestroyWithSkiplock and pve.IsRootPamIdentity).
func retryStopWithSkiplock(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, origErr error, logger *log.Logger,
) (string, error) {
	if !pve.IsRootPamIdentity(deps.Config) {
		return "", origErr
	}
	lockType := pve.VMConfigLockType(origErr)
	logger.Warn("stop rejected -- VM config is locked; retrying with skiplock (root@pam)",
		log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("lock_type", lockType))
	skiplock := true
	resp, err := deps.PVE.Nodes().CreateQemuStatusStop(ctx, node, vmCID, &sdknodes.CreateQemuStatusStopParams{
		Skiplock: &skiplock,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	upid, upidErr := pve.UPIDFromRaw(*resp)
	if upidErr != nil {
		logger.Warn("cannot parse UPID from skiplock stop response -- skipping await",
			log.Int(metadataKeyVMID, vmid), log.Err(upidErr))
		return "", nil
	}
	return upid, nil
}

// logUnresolvedVMLock logs the actionable error surfaced when a stop/destroy
// call is rejected by PVE's guest-config lock and no further in-process
// recovery is possible — either the CPI is not root@pam (skiplock could not
// be attempted) or a skiplock retry was attempted and still failed. It names
// the lock type and the `qm unlock <vmid>` recovery command PVE's HTTP API
// has no endpoint equivalent for.
func logUnresolvedVMLock(logger *log.Logger, action string, vmid int, node string, err error) {
	logger.Error(action+": VM config is locked; manual recovery required",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.String("lock_type", pve.VMConfigLockType(err)),
		log.String("recovery", fmt.Sprintf("qm unlock %d", vmid)),
		log.Err(err),
	)
}
