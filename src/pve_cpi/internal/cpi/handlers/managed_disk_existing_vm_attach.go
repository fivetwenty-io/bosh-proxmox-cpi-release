package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// attachExistingDiskToManagedVM borrows a held VM allocation for legacy disk
// mutation evidence. Managed disks acquire their own allocation instead. The
// caller persists the enclosing attachment intent and owns VM completion.
func attachExistingDiskToManagedVM(ctx context.Context, deps Deps, handle *aj.Handle, disk resolvedDisk, node string, vmid int) (slot string, operationErr error) {
	if disk.allocation != nil {
		slot, err := attachManagedPersistentDisk(ctx, deps, strconv.Itoa(vmid), node, vmid, disk)
		return slot, err
	}
	if handle == nil || handle.Record().Kind != "vm" {
		return "", fmt.Errorf("existing disk attachment requires VM allocation ownership")
	}
	state := handle.Record().State
	if state != aj.Planned && state != aj.Observed {
		return "", fmt.Errorf("VM allocation cannot admit existing disk attachment")
	}
	var sourceNode string
	if disk.holder != nil {
		sourceNode = disk.holder.Node
	} else {
		var err error
		sourceNode, err = resolveNodeForDetachedDisk(ctx, deps, disk.volid)
		if err != nil {
			return "", err
		}
	}
	storage, _, err := pve.ParseDiskCID(disk.volid)
	if err != nil {
		return "", err
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return "", err
	}
	present, err := observeManagedDiskVolume(ctx, deps, sourceNode, disk.volid, disk.meta)
	if err != nil || !present {
		return "", fmt.Errorf("existing persistent volume is not observed")
	}
	session := &storageLifecycle{handle: handle, operation: "create_vm_attach_legacy"}
	lifecycle := &managedDiskLifecycle{external: true, externalNode: sourceNode, externalBacking: backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return "", err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = wrapManagedDiskClient(guard, lifecycle)
	defer func() {
		operationErr = errors.Join(operationErr, guard.Err())
		if operationErr != nil {
			deps.recordStorageReconciliation(ctx, "required")
			operationErr = errors.Join(operationErr, session.Uncertain("existing disk attachment incomplete"))
		}
	}()
	current := disk
	plan, err := guardAndUnparkBeforeAttach(ctx, local, "create_vm.attach_existing", &current, node, vmid)
	if err != nil {
		return "", err
	}
	if err := attachDiskSnapshotGuard(ctx, local, strconv.Itoa(vmid), node, vmid, local.Config, local.Log(ctx)); err != nil {
		return "", err
	}
	slot, _, err = attachDiskCore(ctx, local, "create_vm.attach_existing", strconv.Itoa(vmid), node, vmid, disk.diskCID, current, plan)
	if err != nil {
		return "", err
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", err
	}
	if err := managedAttachedVolume(cfg, slot, lifecycle.disk.volid, disk.stableID); err != nil {
		return "", err
	}
	recorded := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg))
	if recorded[disk.sentinelKey()] != disk.diskCID && recorded[disk.volid] != disk.diskCID {
		return "", fmt.Errorf("attached legacy disk CID provenance missing")
	}
	present, err = observeManagedDiskVolume(ctx, deps, node, lifecycle.disk.volid, disk.meta)
	if err != nil || !present {
		return "", fmt.Errorf("attached persistent volume is not observed")
	}
	return slot, nil
}
