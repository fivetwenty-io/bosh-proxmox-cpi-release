package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
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
	digest := sha256.Sum256([]byte(disk.diskCID))
	step, err := storageMutationIntent(m.handle, fmt.Sprintf("vm.persistent.%x", digest), aj.Target{Node: m.shape.node, VMID: m.vmid}, nil)
	if err != nil {
		return err
	}
	slot, err := attachExistingDiskForVM(ctx, m.deps, m.handle, disk, m.shape.node, m.vmid)
	if err != nil {
		if isDiskReturnedAfterLockTimeout(err) && m.guard.Err() == nil {
			// The disk's lifecycle waited out another request's parker window
			// and returned the disk unchanged, so the handoff this step
			// records never touched the VM. Settle the step and hand the
			// retriable timeout back without poisoning the VM allocation, so
			// its attempt retry or the Director's retry resumes from here.
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
