package handlers

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type storagePlanVolumeReader interface {
	GetStorageContent(context.Context, string, string, string) (*sdknodes.GetStorageContentResponse, error)
}

// ObserveStorageRootSource reads virtual size, never storage listing file size.
// PVE Content.info -> volume_size_info -> filesystem Plugin.file_size_info
// returns qemu-img virtual-size for import/*.qcow2 and template disk images.
// The authenticated existing PVE client bounds the request through ctx/transport.
func ObserveStorageRootSource(ctx context.Context, reader storagePlanVolumeReader, node, volumeID string, templateVMID int) (StorageRootSource, error) {
	if reader == nil || node == "" || templateVMID < 0 {
		return StorageRootSource{}, planError(StoragePlanConfiguration, "invalid root observation request")
	}
	storage, volume, err := pve.ParseDiskCID(volumeID)
	if err != nil {
		return StorageRootSource{}, planError(StoragePlanConfiguration, "invalid root volume identifier")
	}
	if templateVMID == 0 && (!strings.HasPrefix(volume, "import/") || !strings.HasSuffix(volume, ".qcow2")) {
		return StorageRootSource{}, planError(StoragePlanConfiguration, "direct import requires import/*.qcow2")
	}
	if err = ctx.Err(); err != nil {
		return StorageRootSource{}, planError(StoragePlanObservation, "%v", err)
	}
	response, err := reader.GetStorageContent(ctx, node, storage, volume)
	if err != nil {
		return StorageRootSource{}, planError(StoragePlanObservation, "root virtual-size query failed for %s on %s", volumeID, node)
	}
	if response == nil || response.Size <= 0 {
		return StorageRootSource{}, planError(StoragePlanObservation, "root virtual-size response is absent or nonpositive")
	}
	if response.Format != "qcow2" && (templateVMID <= 0 || response.Format != "raw") {
		return StorageRootSource{}, planError(StoragePlanConfiguration, "unsupported root source format %q", response.Format)
	}
	return StorageRootSource{Node: node, StorageID: storage, VolumeID: volumeID, TemplateVMID: templateVMID, VirtualBytes: uint64(response.Size)}, nil
}

// ObserveStorageTemplateRoot reads the concrete configured root volume before
// querying its actual virtual size. It never trusts the stale size= display tag.
func ObserveStorageTemplateRoot(ctx context.Context, deps Deps, node string, vmid int, rootKey string) (StorageRootSource, error) {
	if deps.PVE == nil || vmid <= 0 || rootKey == "" {
		return StorageRootSource{}, planError(StoragePlanConfiguration, "template identity and root key are required")
	}
	config, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return StorageRootSource{}, planError(StoragePlanObservation, "cannot read template root configuration")
	}
	root, ok := pve.ConfigString(config, rootKey)
	if !ok || root == "" {
		return StorageRootSource{}, planError(StoragePlanObservation, "template root device is absent")
	}
	volume := strings.Split(root, ",")[0]
	source, err := ObserveStorageRootSource(ctx, deps.PVE.Nodes(), node, volume, vmid)
	if err != nil {
		return StorageRootSource{}, err
	}
	// Clone carries all source disks. Measure and charge auxiliary target disks
	// conservatively at their full virtual size, separately from root expansion.
	for key, value := range config {
		if key == rootKey || (!strings.HasPrefix(key, "scsi") && !strings.HasPrefix(key, "virtio") && !strings.HasPrefix(key, "sata") && !strings.HasPrefix(key, "ide") && !strings.HasPrefix(key, "efidisk") && !strings.HasPrefix(key, "tpmstate")) {
			continue
		}
		drive, ok := pve.ConfigStringValue(value)
		if !ok || strings.Contains(drive, "media=cdrom") {
			continue
		}
		vol := strings.Split(drive, ",")[0]
		if !strings.Contains(vol, ":") {
			continue
		}
		auxiliary, e := ObserveStorageRootSource(ctx, deps.PVE.Nodes(), node, vol, vmid)
		if e != nil {
			return StorageRootSource{}, e
		}
		rounded, e := inv.RoundBytes(auxiliary.VirtualBytes, 1<<20)
		if e != nil || source.AuxiliaryBytes > math.MaxUint64-rounded {
			return StorageRootSource{}, planError(StoragePlanConfiguration, "template auxiliary size overflow")
		}
		source.AuxiliaryBytes += rounded
		source.AuxiliaryVolumes = append(source.AuxiliaryVolumes, StorageExistingVolume{StorageID: auxiliary.StorageID, Node: node, VolumeID: vol, Device: key, VirtualBytes: auxiliary.VirtualBytes})
	}
	slices.SortFunc(source.AuxiliaryVolumes, func(a, b StorageExistingVolume) int { return strings.Compare(a.VolumeID, b.VolumeID) })
	return source, nil
}

// ConfigureStorageVMPlanRequest applies the existing shape and headroom policy
// to a request whose source and inventory facts have already been observed.
// isoBytes is the actual generated config-drive artifact size supplied by runtime.
func ConfigureStorageVMPlanRequest(deps Deps, parsed *createVMParsedArgs, r StoragePlanRequest, isoBytes uint64) (StoragePlanRequest, error) {
	if deps.Config == nil || parsed == nil {
		return r, planError(StoragePlanConfiguration, "VM sizing dependencies are required")
	}
	resolver, err := newLayeredResolver(parsed.cloudPropsMap, deps.Config)
	if err != nil {
		return r, err
	}
	cp := parsed.cloudProps
	root := cp.RootDiskSize
	if v, ok := resolver.Int("root_disk_size"); ok {
		root = v
	}
	if root == 0 {
		root = cp.Disk
	}
	toBytes := func(mib int) (uint64, error) {
		if mib < 0 || uint64(mib) > math.MaxUint64/(1<<20) {
			return 0, planError(StoragePlanConfiguration, "disk size overflow or negative size")
		}
		return inv.RoundBytes(uint64(mib)*(1<<20), 1<<30)
	}
	r.RootBytes, err = toBytes(root)
	if err != nil {
		return r, err
	}
	r.EphemeralBytes, err = toBytes(cp.EphemeralDiskSizeMB)
	if err != nil {
		return r, err
	}
	r.CloneMode, err = resolveCloneMode(deps.Config, parsed.cloudPropsMap)
	if err != nil {
		return r, err
	}
	r.ISOBytes = isoBytes
	r.OriginalISOStorage = deps.Config.OriginalISOStorage()
	r.ISOFollowRoot = deps.Config.ISOStorageFollowVMStorageEnabled()
	r.RequireSharedISO = deps.Config.RequireSharedISOForHAEnabled()
	r.ExtraLimits.MaxUtilizationPct = deps.Config.MaxUtilizationPctValue()
	r.RoleLimits = map[string]inv.Limits{}
	if deps.Config.ReserveStorageHeadroomEnabled() {
		margin := deps.Config.StorageHeadroomMBValue()
		if margin < 0 || uint64(margin) > math.MaxUint64/(1<<20) {
			return r, planError(StoragePlanConfiguration, "storage headroom overflow")
		}
		reserve := uint64(margin) * (1 << 20)
		r.RoleLimits[storageRoleRoot] = inv.Limits{ReserveBytes: reserve}
		if cp.EphemeralDiskSizeMB > 0 {
			_, _, mem := resolveVMShapeCPUMem(cp)
			if mem < 0 || uint64(mem) > (math.MaxUint64-reserve)/(1<<20) {
				return r, planError(StoragePlanConfiguration, "swap headroom overflow")
			}
			r.RoleLimits[storageRoleEphemeral] = inv.Limits{ReserveBytes: reserve + uint64(mem)*(1<<20)}
		}
	}
	return r, nil
}

// ObserveStorageExistingVolumes resolves identities without resuming transfers.
func ObserveStorageExistingVolumes(ctx context.Context, deps Deps, cids []string) ([]StorageExistingVolume, error) {
	out := make([]StorageExistingVolume, 0, len(cids))
	for _, cid := range cids {
		bare, meta, err := decodeDiskCID(ctx, deps, "create_vm", cid)
		if err != nil {
			return nil, planError(StoragePlanConfiguration, "invalid persistent disk CID")
		}
		resolved, err := resolveDiskForOp(ctx, deps, "create_vm", cid, bare, meta)
		if err != nil {
			return nil, err
		}
		if resolved.intent != nil {
			return nil, planError(StoragePlanReconciliation, "existing disk has interrupted transfer: %s", cid)
		}
		holder := resolved.holder
		if holder == nil {
			found, err := pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), resolved.volid, parkerReadConfigFor(deps))
			if err != nil {
				return nil, err
			}
			if found.Found {
				holder = &found
			}
		}
		storage, _, err := pve.ParseDiskCID(resolved.volid)
		if err != nil {
			return nil, fmt.Errorf("resolved disk volume: %w", err)
		}
		node := ""
		if holder != nil {
			node = holder.Node
		} else if meta != nil {
			node = meta.Node
		}
		out = append(out, StorageExistingVolume{StorageID: storage, Node: node, VolumeID: resolved.volid})
	}
	return out, nil
}
