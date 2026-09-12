package handlers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

const storageMechanismFullClone = "full_clone"

// createManagedVMRoot consumes the frozen mechanism and source. The supplied
// client must be guarded; it never retries, reselects a source, or cleans up.
func createManagedVMRoot(ctx context.Context, deps Deps, parsed *createVMParsedArgs, shape *createVMShape, target StoragePlanTarget, vmid int, marker string) error {
	if target.Role != storageRoleRoot || target.Source == nil || target.StorageID != shape.vmStorage || target.Node != shape.node || vmid <= 0 {
		return fmt.Errorf("managed root does not match frozen VM shape")
	}
	if _, found, err := pve.ParseStorageAllocationMarker(marker); err != nil || !found {
		return fmt.Errorf("managed root requires allocation provenance")
	}
	name := candidateVMName(shape.initialName, parsed.agentID, vmid)
	if err := ensureResolvedPool(ctx, deps, shape, deps.Log(ctx)); err != nil {
		return err
	}
	switch target.Mechanism {
	case "import":
		// Only the observed source may be imported, regardless of mutable template
		// cache preference or import fallback configuration.
		sourceParsed := *parsed
		sourceParsed.rawVolid = target.Source.VolumeID
		params := buildVMImportParams(&sourceParsed, shape, vmid, name)
		applyOptionalCreateParams(params, shape)
		params["description"] = marker
		upid, err := deps.PVE.QEMU().Create(ctx, shape.node, params)
		if err != nil {
			return err
		}
		if _, err = managedMutationUPID(upid); err != nil {
			return err
		}
	case storageMechanismFullClone, "linked_clone":
		if err := cloneManagedVMRoot(ctx, deps, shape, target, vmid, name, marker); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported frozen managed root mechanism")
	}
	if err := applyPVEConfigPassthrough(ctx, deps, shape.node, vmid, parsed.cloudProps.PVEConfig, deps.Log(ctx)); err != nil {
		return err
	}
	return applyPCIPassthrough(ctx, deps, shape.node, vmid, parsed.cloudProps.PCIPassthroughs, deps.Log(ctx))
}

func cloneManagedVMRoot(ctx context.Context, deps Deps, shape *createVMShape, target StoragePlanTarget, vmid int, name, marker string) error {
	if target.Source.TemplateVMID <= 0 {
		return fmt.Errorf("managed clone lacks frozen template identity")
	}
	full := target.Mechanism == storageMechanismFullClone
	params := &sdknodes.CreateQemuCloneParams{Newid: int64(vmid), Name: &name, Full: &full, Description: &marker}
	if full {
		params.Storage = &shape.vmStorage
		params.Format = &shape.vmDiskFormat
	}
	if shape.vmPool != "" {
		params.Pool = &shape.vmPool
	}
	if target.Source.Node != shape.node {
		params.Target = &shape.node
	}
	raw, err := deps.PVE.Nodes().CreateQemuClone(ctx, target.Source.Node, strconv.Itoa(target.Source.TemplateVMID), params)
	if err != nil {
		return err
	}
	if _, err := managedMutationUPID(raw); err != nil {
		return err
	}
	// Retain the submission marker while applying the target shape. The clone
	// request already carried provenance if its response was lost.
	mem := strconv.Itoa(shape.memMiB)
	cores := int64(shape.cores)
	sockets := int64(shape.sockets)
	enabled := "enabled=1"
	tablet := false
	update := &sdknodes.UpdateQemuConfigParams{Description: &marker, Memory: &mem, Cores: &cores, Sockets: &sockets, Agent: &enabled, Tablet: &tablet, Tags: &shape.initialTags, Serial: map[int]string{0: "socket"}, Hotplug: &shape.hotplug, Scsihw: &shape.scsihw, Numa: &shape.numaEnabled}
	if shape.cpuType != "" {
		update.Cpu = &shape.cpuType
	}
	applyCloneBalloon(update, shape.balloonMiB)
	if len(shape.rootDiskPerfOpts) > 0 {
		current, err := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
		if err != nil {
			return err
		}
		drive, ok := pve.ConfigString(current, shape.rootDiskKey)
		if !ok || drive == "" {
			return fmt.Errorf("cloned root device missing")
		}
		volume, _ := splitDiskOptStr(drive)
		updated := buildDiskOptStr(volume, shape.rootDiskPerfOpts)
		if shape.rootDiskKey == diskKeyScsi0 {
			update.Scsi = map[int]string{0: updated}
		} else {
			update.Virtio = map[int]string{0: updated}
		}
	}

	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, shape.node, strconv.Itoa(vmid), update); err != nil {
		return err
	}
	return nil
}
