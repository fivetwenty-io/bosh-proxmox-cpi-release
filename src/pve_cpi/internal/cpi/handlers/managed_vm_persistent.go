package handlers

import (
	"context"
	"crypto/sha256"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

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
	slot, err := attachExistingDiskToManagedVM(ctx, m.deps, m.handle, disk, m.shape.node, m.vmid)
	if err != nil {
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
