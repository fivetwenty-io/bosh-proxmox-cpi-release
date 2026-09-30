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
	// The disposition the record had before cleanup admitted its lifecycle,
	// which a clean lock wait failure puts back.
	prior := handle.Record()
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
	ctx = managedLockWaitContext(ctx)
	defer func() {
		// A parker lock wait that failed before this cleanup changed the
		// disk, judged by the same rules attach_disk uses, leaves the record
		// as it was and hands the retriable failure back, so the operator
		// reruns cleanup once the other request's window closes.
		if lifecycle.cleanLockTimeout(operationErr) {
			restoreErr := restoreExplicitCleanupDisposition(handle, prior)
			if restoreErr == nil {
				return
			}
			operationErr = errors.Join(operationErr, restoreErr)
		}
		operationErr = errors.Join(operationErr, guard.Err())
		if operationErr != nil {
			deps.recordStorageReconciliation(ctx, "required")
			operationErr = errors.Join(operationErr, session.Uncertain("explicit disk cleanup incomplete"))
		}
	}()
	if hook := explicitCleanupBeforeUnparkFrom(ctx); hook != nil {
		hook(ctx, local, disk)
	}
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

// explicitCleanupBeforeUnparkKey carries a test's hook on the request context.
type explicitCleanupBeforeUnparkKey struct{}

// explicitCleanupHook runs after explicit cleanup admits its lifecycle and
// before it touches the disk.
type explicitCleanupHook func(ctx context.Context, local Deps, disk resolvedDisk)

// withExplicitCleanupBeforeUnpark returns a context that runs hook at that
// point. Only tests call it, to admit a guarded write ahead of the parker lock
// wait. It rides the context, as WithTestBackoff does, so tests that set it can
// run in parallel.
func withExplicitCleanupBeforeUnpark(ctx context.Context, hook explicitCleanupHook) context.Context {
	return context.WithValue(ctx, explicitCleanupBeforeUnparkKey{}, hook)
}

// explicitCleanupBeforeUnparkFrom returns the hook ctx carries, or nil.
func explicitCleanupBeforeUnparkFrom(ctx context.Context) explicitCleanupHook {
	hook, _ := ctx.Value(explicitCleanupBeforeUnparkKey{}).(explicitCleanupHook)
	return hook
}

// restoreExplicitCleanupDisposition puts back the state and reason the record
// had before explicit cleanup admitted its lifecycle. The admission and
// ownership evidence cleanup appended stay, because the journal only ever
// appends verifications. A record that was adopted passes through
// reconciliation_required on the way back, the only route the journal allows
// from the observed state admission leaves it in.
func restoreExplicitCleanupDisposition(handle *aj.Handle, prior aj.Record) error {
	record := handle.Record()
	if record.State == prior.State && record.Reason == prior.Reason {
		return nil
	}
	if prior.State == aj.Adopted {
		record.State = aj.ReconciliationRequired
		record.Reason = "explicit disk cleanup lock wait ended; restoring adoption"
		if err := handle.Save(record); err != nil {
			return err
		}
		record = handle.Record()
	}
	record.State = prior.State
	record.Reason = prior.Reason
	return handle.Save(record)
}
