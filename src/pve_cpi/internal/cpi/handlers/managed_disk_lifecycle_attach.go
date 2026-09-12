package handlers

import (
	"context"
	"fmt"
)

// attachManagedPersistentDisk is the create_vm existing-disk adapter. It owns
// the disk allocation lock and preserves its UUID while the caller owns the VM
// generation. Supplied mutation services may additionally guard the new VM.
func attachManagedPersistentDisk(ctx context.Context, deps Deps, vmCID, node string, vmid int, rd resolvedDisk) (diskID string, operationErr error) {
	if rd.allocation == nil {
		return "", fmt.Errorf("managed persistent attachment requires allocation identity")
	}
	local, lifecycle, err := managedDiskOperation(ctx, deps, rd, "attach_disk")
	if err != nil {
		return "", err
	}
	if lifecycle == nil {
		return "", fmt.Errorf("managed persistent attachment did not acquire allocation ownership")
	}
	defer func() { operationErr = lifecycle.finish(ctx, operationErr, false) }()
	current := lifecycle.disk
	plan, err := guardAndUnparkBeforeAttach(ctx, local, "create_vm.attach_disk", &current, node, vmid)
	if err != nil {
		return "", err
	}
	if err := attachDiskSnapshotGuard(ctx, local, vmCID, node, vmid, local.Config, local.Log(ctx)); err != nil {
		return "", err
	}
	slot, _, err := attachDiskCore(ctx, local, "create_vm.attach_disk", vmCID, node, vmid, current.diskCID, current, plan)
	return slot, err
}
