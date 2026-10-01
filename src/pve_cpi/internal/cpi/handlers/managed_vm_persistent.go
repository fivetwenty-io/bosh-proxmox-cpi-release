package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// attachExistingDiskForVM is the disk attach attachPersistent runs. Tests
// replace it to drive the VM allocation's side of a disk outcome.
var attachExistingDiskForVM = attachExistingDiskToManagedVM

// The persistent allocation owns its mutations and any parker. The VM records
// the handoff without treating the independently owned disk as a VM allocation.
func (m *managedVMAllocation) attachPersistent(ctx context.Context, disk resolvedDisk) error {
	if err := m.guard.Err(); err != nil {
		return err
	}
	if err := m.revalidate(ctx); err != nil {
		return err
	}
	cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
	if err != nil {
		return err
	}
	if err := m.verifyMarker(cfg); err != nil {
		return err
	}
	// The attach below refuses a legacy volume named for this VM too, but by
	// then the step is recorded and a refusal would leave the allocation
	// uncertain. Nothing has changed at this point, so refuse cleanly first.
	if err := refuseOwnedLegacyAttach("create_vm", disk, m.shape.node, m.vmid); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(disk.diskCID))
	step, err := storageMutationIntent(m.handle, fmt.Sprintf("vm.persistent.%x", digest), aj.Target{Node: m.shape.node, VMID: m.vmid}, nil)
	if err != nil {
		return err
	}
	slot, err := attachExistingDiskForVM(ctx, m.deps, m.handle, disk, m.shape.node, m.vmid)
	if err != nil {
		if m.guard.Err() == nil {
			if pending, ok := m.protectionPending(ctx, disk, step, slot, err); ok {
				return pending
			}
		}
		if isDiskReturnedAfterLockTimeout(err) && m.guard.Err() == nil {
			// The disk's lifecycle waited out another request's parker window
			// and returned the disk unchanged, so the handoff this step
			// records never touched the VM. Settle the step and hand the
			// retriable timeout back without poisoning the VM allocation.
			// create_vm never resumes this VM. It disposes of the attempt,
			// preserving any disk already attached. A fallback attempt then
			// places a new VM, and on the last attempt the generation is
			// closed before the timeout reaches the Director, whose retry
			// builds a fresh VM.
			if observeErr := storageMutationObserved(m.handle, step, nil, false); observeErr != nil {
				return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk attachment"))
			}
			return err
		}
		return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk attachment"))
	}
	current, err := resolveDiskForOp(ctx, m.deps, "create_vm", disk.diskCID, disk.birth, disk.meta)
	if err != nil {
		return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk readback"))
	}
	cfg, err = m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
	if err == nil {
		err = m.verifyMarker(cfg)
	}
	if err == nil {
		err = managedAttachedVolume(cfg, slot, current.volid, current.stableID)
	}
	if err != nil {
		return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk binding"))
	}
	return storageMutationObserved(m.handle, step, nil, false)
}

// managedDiskProtectionPending is the error attachPersistent returns when the
// only thing a persistent disk's attach left open is its parker's protection.
// Either the disk reached the VM and only the protection restore behind it was
// cut off, or a retry found the disk already attached and its readmission
// waits for the parker to read back protected. The handoff is observed in both
// cases, and the VM allocation is left resumable. create_vm hands the error
// back as it is, with no rollback and no fallback attempt, because the VM is
// placed and working, and the Director's retry resumes it. It is not the
// returned-disk marker, whose rollback would dispose of this VM.
type managedDiskProtectionPending struct{ err error }

func (e *managedDiskProtectionPending) Error() string { return e.err.Error() }

func (e *managedDiskProtectionPending) Unwrap() error { return e.err }

func isManagedDiskProtectionPending(err error) bool {
	var pending *managedDiskProtectionPending
	return errors.As(err, &pending)
}

// protectionPending decides whether a failed persistent attach left only the
// parker's protection open, and if so observes the handoff step and returns
// the error create_vm hands back. It reports false for every other failure,
// which then takes the handling it always has. A disk that landed has its
// binding read back exactly as a successful attach does, and a binding that
// does not read back locks the guard.
func (m *managedVMAllocation) protectionPending(ctx context.Context, disk resolvedDisk, step, slot string, attachErr error) (error, bool) {
	var cutOff *pve.ProtectionRestoreCutOffError
	var refused *protectionPendingRefusal
	switch {
	case slot != "" && errors.As(attachErr, &cutOff) && cutOff.WorkCompleted:
		current, err := resolveDiskForOp(ctx, m.deps, "create_vm", disk.diskCID, disk.birth, disk.meta)
		if err != nil {
			return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk readback")), true
		}
		cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
		if err == nil {
			err = m.verifyMarker(cfg)
		}
		if err == nil {
			err = managedAttachedVolume(cfg, slot, current.volid, current.stableID)
		}
		if err != nil {
			return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk binding")), true
		}
		if err := storageMutationObserved(m.handle, step, nil, false); err != nil {
			return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk attachment")), true
		}
		return &managedDiskProtectionPending{err: attachErr}, true
	case errors.As(attachErr, &refused):
		// Readmission refused before the disk lifecycle changed anything, so
		// this handoff never touched the VM.
		if err := storageMutationObserved(m.handle, step, nil, false); err != nil {
			return m.guard.Poison(storageAllocationUncertain(m.handle, "persistent disk attachment")), true
		}
		return &managedDiskProtectionPending{err: cpierrors.WrapAs(attachErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: persistent disk %s waits for its parker's protection", disk.diskCID))}, true
	default:
		return nil, false
	}
}
