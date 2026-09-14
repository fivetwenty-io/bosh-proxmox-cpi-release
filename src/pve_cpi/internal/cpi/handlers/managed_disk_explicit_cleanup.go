package handlers

import (
	"context"
	"errors"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// resolveManagedDiskForCleanup also accepts an unreturned allocation. Its
// recorded full UUID birth name and immutable token supply the internal CID;
// this never changes the caller's CID or invents a new allocation identity.
func resolveManagedDiskForCleanup(ctx context.Context, deps Deps, record aj.Record) (resolvedDisk, error) {
	if record.Kind != "disk" || record.State == aj.Deleted || record.State == aj.Cleaned {
		return resolvedDisk{}, fmt.Errorf("cleanup requires live disk allocation")
	}
	if err := storageCleanupSettled(ctx, record); err != nil {
		return resolvedDisk{}, err
	}
	if record.CID != "" {
		return resolveDeleteDiskCID(ctx, deps, record.CID)
	}
	birth := ""
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		candidates := append([]string{step.Target.IntendedVolume}, step.VolIDs...)
		for _, volume := range candidates {
			locator, id, ok := pve.ParseAllocationVolumeID(volume)
			if !ok || id != record.ID || locator != pve.AllocationNamespaceLocator(record.Namespace) {
				continue
			}
			if birth != "" && birth != volume {
				return resolvedDisk{}, fmt.Errorf("cleanup birth identity is ambiguous")
			}
			birth = volume
		}
	}
	if birth == "" {
		return resolvedDisk{}, fmt.Errorf("unreturned disk has no recorded full allocation birth identity")
	}
	meta := &pve.DiskCIDMeta{ID: record.DiskToken}
	cid, err := pve.EncodeDiskCID(birth, meta)
	if err != nil {
		return resolvedDisk{}, err
	}
	return resolveDiskForOp(ctx, deps, "explicit_disk_cleanup", cid, birth, meta)
}

// cleanupManagedDiskAllocation borrows the explicit operator command's handle.
// It never settles an unknown submission or closes the handle. The returned
// complete historical absence proof permits the caller's cleanup tombstone.
func cleanupManagedDiskAllocation(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle) (proof aj.Verification, operationErr error) {
	if journal == nil || handle == nil {
		return proof, fmt.Errorf("disk cleanup requires held journal authority")
	}
	disk, err := resolveManagedDiskForCleanup(ctx, deps, handle.Record())
	if err != nil {
		return proof, err
	}
	if disk.allocation == nil || disk.allocation.record.ID != handle.Record().ID || disk.intent != nil {
		return proof, fmt.Errorf("cleanup disk ownership is unresolved")
	}
	lifecycle := &managedDiskLifecycle{requestContext: ctx, deps: deps, disk: disk, journal: journal, handle: handle}
	if disk.allocation.absent {
		return lifecycle.deletionProof(ctx)
	}
	if disk.holder != nil && !disk.holder.IsParker {
		return proof, fmt.Errorf("cleanup requires detached or parked persistent disk")
	}
	ownership, err := managedDiskOwnershipProof(disk)
	if err != nil {
		return proof, err
	}
	session, err := beginStorageLifecycleCleanup(ctx, handle, "delete_disk", ownership)
	if err != nil {
		return proof, err
	}
	lifecycle.session = session
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return proof, err
	}
	lifecycle.guard = guard
	local := deps
	copied := *deps.Config
	disabled := false
	copied.FastPathDelete = &disabled
	local.Config = &copied
	local.PVE = wrapManagedDiskClient(guard, lifecycle)
	defer func() {
		operationErr = errors.Join(operationErr, guard.Err())
		if operationErr != nil {
			deps.recordStorageReconciliation(ctx, "required")
			operationErr = errors.Join(operationErr, session.Uncertain("explicit disk cleanup incomplete"))
		}
	}()
	node := disk.allocation.provenance.Node
	deleted := false
	// Explicit cleanup has already audited ownership and current references.
	// A prior deletion can remove the holder before failing; an anchored CID
	// must not force this proven free volume back through ordinary unpark.
	// The guarded storage deletion repeats identity and reference checks.
	if disk.holder != nil {
		deleted, err = unparkBeforeDelete(ctx, local, disk, node)
		if err != nil {
			return proof, err
		}
	}
	if !deleted {
		storage, _, err := pve.ParseDiskCID(disk.volid)
		if err != nil {
			return proof, err
		}
		if err := deleteDiskVolume(ctx, local, disk.diskCID, disk.volid, storage, node); err != nil {
			return proof, err
		}
	}
	if err := guard.Err(); err != nil {
		return proof, err
	}
	return lifecycle.deletionProof(ctx)
}
