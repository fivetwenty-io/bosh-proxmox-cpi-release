package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

func resizeCreatedVMRoot(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int) error {
	if parsed.storageRuntime == nil {
		return resizeRootDisk(ctx, deps, logger, shape, vmid)
	}
	m := parsed.storageRuntime
	target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleRoot)
	volume := m.volumes[storageRoleRoot]
	size, err := m.observeTargetVolume(ctx, target, volume, 1)
	if err != nil {
		return err
	}
	if size > target.VirtualBytes {
		return fmt.Errorf("actual root exceeds frozen virtual size")
	}
	if size == target.VirtualBytes {
		return m.acquireCharge(ctx, "root_growth", volume)
	}
	raw, err := deps.PVE.Nodes().UpdateQemuResize(ctx, shape.node, strconv.Itoa(vmid), &nodes.UpdateQemuResizeParams{Disk: shape.rootDiskKey, Size: fmt.Sprintf("%dG", target.VirtualBytes/(1<<30))})
	if err != nil {
		return err
	}
	_, err = managedMutationUPID(raw)
	return err
}
func attachCreatedVMEphemeral(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int) (string, error) {
	if parsed.storageRuntime == nil {
		return attachEphemeralDisk(ctx, deps, logger, shape, vmid)
	}
	m := parsed.storageRuntime
	if shape.ephemeralDiskGiB <= 0 {
		return "", nil
	}
	volume := m.volumes[storageRoleEphemeral]
	if volume == "" {
		definition, ok := m.prepared.plan.Definitions[shape.ephemeralStorage]
		if !ok {
			return "", fmt.Errorf("ephemeral storage definition is absent from the frozen plan")
		}
		format, err := pve.EphemeralVolumeFormat(definition.Type, shape.vmDiskFormat)
		if err != nil {
			return "", err
		}
		name, _, err := pve.ManagedEphemeralVolumeName(definition.Type, shape.vmDiskFormat, vmid, m.handle.Record().Namespace, m.handle.Record().ID)
		if err != nil {
			return "", err
		}
		volume, err = deps.PVE.Storage().CreateVolume(ctx, shape.node, shape.ephemeralStorage, shape.ephemeralDiskGiB, format, vmid, name)
		if err != nil {
			return "", err
		}
	}
	// Both views. The VM has never run, so it can't have a pending delete,
	// and a slot of this volume whose delete is pending is a state this path
	// doesn't produce, so it fails closed rather than attach or revert.
	views, err := pve.ReadQemuViews(ctx, m.deps.PVE, shape.node, vmid)
	if err != nil {
		return "", err
	}
	slot := ""
	for _, key := range views.SlotsNaming(volume) {
		if !strings.HasPrefix(key, "scsi") {
			continue
		}
		if slot != "" {
			return "", fmt.Errorf("ephemeral volume has ambiguous devices")
		}
		slot = key
	}
	if slot != "" && views.PendingDelete(slot) {
		return "", fmt.Errorf("ephemeral volume's device %s has a pending delete", slot)
	}
	if slot == "" {
		index := nextFreeSCSIIndexInViews(views)
		if index >= 29 {
			return "", fmt.Errorf("no ephemeral SCSI device available")
		}
		slot = fmt.Sprintf("scsi%d", index)
		actual, err := deps.PVE.QEMU().AttachDisk(ctx, shape.node, vmid, volume, "scsi", &qemu.AttachOpts{DiskID: slot})
		if err != nil {
			return "", err
		}
		if actual != slot {
			return "", fmt.Errorf("ephemeral attachment changed device")
		}
	}
	return devicePathByID(slot)
}
