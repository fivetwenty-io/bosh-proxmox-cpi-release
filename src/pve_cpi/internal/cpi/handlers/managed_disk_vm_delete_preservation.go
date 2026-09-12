package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

var managedVMVolumeSlot = regexp.MustCompile(`^(scsi|virtio|sata|ide|unused|efidisk|tpmstate)\d+$`)

// detachManagedPersistentForVMDelete preserves independently identified disks
// outside the VM allocation's exact owned volume set. The caller holds the VM
// generation lock and supplies its original services. Each managed disk acquires
// its own allocation lock. Unknown volumes prevent every preservation mutation.
func detachManagedPersistentForVMDelete(ctx context.Context, deps Deps, node string, vmid int, ownedVolumes map[string]bool, handle *aj.Handle) error {
	config, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read VM disks before preservation: %w", err)
	}
	disks, err := managedVMPreservationCandidates(ctx, deps, node, vmid, config, ownedVolumes)
	if err != nil {
		return err
	}
	for _, disk := range disks {
		if err := detachManagedPersistentForVMDeleteOne(ctx, deps, node, vmid, disk, handle); err != nil {
			return err
		}
	}
	config, err = deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read VM after persistent disk preservation: %w", err)
	}
	volumes, err := managedVMConfigVolumes(config)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if !ownedVolumes[volume] {
			return fmt.Errorf("VM still references a volume outside its recorded allocation")
		}
	}
	return nil
}

func managedVMConfigVolumes(config map[string]any) (map[string]string, error) {
	volumes := map[string]string{}
	for slot, raw := range config {
		if !managedVMVolumeSlot.MatchString(slot) {
			continue
		}
		value, ok := pve.ConfigStringValue(raw)
		if !ok || value == "" {
			return nil, fmt.Errorf("invalid VM volume entry")
		}
		volume := strings.Split(value, ",")[0]
		if volume == "none" || volume == "cdrom" {
			continue
		}
		if _, _, err := pve.ParseDiskCID(volume); err != nil {
			return nil, fmt.Errorf("unrecognized VM volume reference")
		}
		volumes[slot] = volume
	}
	return volumes, nil
}

func managedVMPreservationCandidates(ctx context.Context, deps Deps, node string, vmid int, config map[string]any, owned map[string]bool) ([]resolvedDisk, error) {
	volumes, err := managedVMConfigVolumes(config)
	if err != nil {
		return nil, err
	}
	description, _ := pve.ConfigString(config, "description")
	provenance, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return nil, err
	}
	_, sentinel := pve.ParseSentinel(description)
	recorded := map[string]string{}
	if raw, exists := sentinel["bosh_attached_disks"]; exists {
		if err := json.Unmarshal(raw, &recorded); err != nil || recorded == nil {
			return nil, fmt.Errorf("invalid recorded persistent disk CIDs")
		}
	}
	slots := make([]string, 0, len(volumes))
	for slot := range volumes {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	seen := map[string]bool{}
	var result []resolvedDisk
	for _, slot := range slots {
		volume := volumes[slot]
		if seen[volume] {
			return nil, fmt.Errorf("ambiguous duplicate VM volume reference")
		}
		seen[volume] = true
		if owned[volume] {
			continue
		}
		disk, err := managedVMPreservationCandidate(ctx, deps, node, vmid, config, slot, volume, provenance, recorded)
		if err != nil {
			return nil, err
		}
		result = append(result, disk)
	}
	return result, nil
}

func detachManagedPersistentForVMDeleteOne(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk, handle *aj.Handle) (operationErr error) {
	if disk.allocation == nil {
		return preserveLegacyDiskForVMDelete(ctx, deps, node, vmid, disk, handle)
	}
	local, lifecycle, err := managedDiskOperation(ctx, deps, disk, "delete_vm.preserve_disk")
	if err != nil {
		return err
	}
	if lifecycle == nil {
		return fmt.Errorf("persistent disk preservation did not acquire allocation ownership")
	}
	defer func() { operationErr = lifecycle.finish(ctx, operationErr, false) }()
	current := lifecycle.disk
	if current.holder == nil || current.holder.Node != node || current.holder.VMID != vmid || current.volid != disk.volid {
		return fmt.Errorf("persistent disk ownership changed before preservation")
	}
	return handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, current)
}

// preserveLegacyDiskForVMDelete borrows the already admitted VM handle. It
// never closes or terminalizes that allocation, and marks every disk step
// external so its actual volume lineage cannot authorize VM-owned deletion.
func preserveLegacyDiskForVMDelete(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk, handle *aj.Handle) error {
	if handle == nil || handle.Record().Kind != "vm" || handle.Record().State != aj.Observed {
		return fmt.Errorf("legacy preservation requires an admitted VM journal handle")
	}
	storage, _, err := pve.ParseDiskCID(disk.volid)
	if err != nil {
		return err
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return err
	}
	present, err := observeManagedDiskVolume(ctx, deps, node, disk.volid, disk.meta)
	if err != nil || !present {
		return fmt.Errorf("legacy preservation requires observed live volume")
	}
	session := &storageLifecycle{handle: handle, operation: "delete_vm_preserve_legacy"}
	lifecycle := &managedDiskLifecycle{external: true, externalNode: node, externalBacking: backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = &managedDiskLifecycleClient{Client: guard.Client(), lifecycle: lifecycle}
	current, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_legacy", disk.diskCID, disk.birth, disk.meta)
	if err == nil && disk.stableID == "" {
		current = disk
	}
	if err == nil && (current.holder == nil || current.holder.Node != node || current.holder.VMID != vmid || current.volid != disk.volid || current.intent != nil) {
		err = fmt.Errorf("legacy disk ownership changed before preservation")
	}
	if err == nil {
		if disk.stableID == "" {
			err = unlinkLegacyPersistentForVMDelete(ctx, local, node, vmid, current)
		} else {
			err = handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, current)
		}
	}
	err = errors.Join(err, guard.Err())
	if err == nil {
		if disk.stableID != "" {
			err = verifyLegacyDiskPreservation(ctx, deps, disk)
		} else {
			present, readErr := observeManagedDiskVolume(ctx, deps, node, disk.volid, disk.meta)
			if readErr != nil || !present {
				err = fmt.Errorf("legacy unlinked volume not observed")
			}
		}
	}
	if err != nil {
		deps.recordStorageReconciliation(ctx, "required")
		return errors.Join(err, session.Uncertain("legacy disk preservation incomplete"))
	}
	return nil
}

func verifyLegacyDiskPreservation(ctx context.Context, deps Deps, disk resolvedDisk) error {
	current, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_legacy_complete", disk.diskCID, disk.birth, disk.meta)
	if err != nil {
		return err
	}
	if current.holder == nil || !current.holder.IsParker || current.intent != nil {
		return fmt.Errorf("legacy disk preservation has no committed parker")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, current.holder.Node, current.holder.VMID)
	if err != nil {
		return err
	}
	_, records := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var entries map[string]struct {
		DiskCID string `json:"disk_cid"`
		Volid   string `json:"volid"`
		Node    string `json:"node"`
		Slot    string `json:"slot"`
	}
	if err := json.Unmarshal(records["bosh_parked_disks"], &entries); err != nil {
		return fmt.Errorf("legacy parker provenance unreadable")
	}
	entry, ok := entries[disk.stableID]
	if !ok || entry.DiskCID != disk.diskCID || entry.Volid != current.volid || entry.Node != current.holder.Node || entry.Slot == "" {
		return fmt.Errorf("legacy parker provenance differs from preserved identity")
	}
	if err := managedAttachedVolume(cfg, entry.Slot, current.volid, disk.stableID); err != nil {
		return err
	}
	present, err := observeManagedDiskVolume(ctx, deps, current.holder.Node, current.volid, disk.meta)
	if err != nil || !present {
		return fmt.Errorf("legacy parked volume not observed")
	}
	return nil
}

// An active non-force config delete cannot free an ordinary disk. If PVE
// registers an unused entry, the VM owns its physical volume: deleting that
// entry would free it, so preserve it and stop instead of sweeping.
func unlinkLegacyPersistentForVMDelete(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return err
	}
	volumes, err := managedVMConfigVolumes(cfg)
	if err != nil {
		return err
	}
	slot := ""
	for key, volume := range volumes {
		if volume != disk.volid {
			continue
		}
		if slot != "" || strings.HasPrefix(key, "unused") {
			return fmt.Errorf("legacy disk cannot be safely unlinked from this VM")
		}
		slot = key
	}
	if slot == "" {
		return fmt.Errorf("legacy disk moved before preservation")
	}
	if err := managedDeleteSlot(ctx, deps.PVE, node, vmid, slot, cfg); err != nil {
		return err
	}
	after, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return err
	}
	remaining, err := managedVMConfigVolumes(after)
	if err != nil {
		return err
	}
	for _, volume := range remaining {
		if volume == disk.volid {
			return fmt.Errorf("legacy disk remains VM-owned; safe preservation requires reconciliation")
		}
	}
	return nil
}

func managedVMPreservationCandidate(ctx context.Context, deps Deps, node string, vmid int, config map[string]any, slot, volume string, provenance map[string]pve.DiskAllocationProvenance, recorded map[string]string) (resolvedDisk, error) {
	var err error
	value, _ := pve.ConfigString(config, slot)
	token, _ := pve.StableIDFromDriveOptStr(value)
	for key, entry := range provenance {
		if entry.Volid != volume {
			continue
		}
		if token != "" && token != key {
			return resolvedDisk{}, fmt.Errorf("conflicting persistent disk identity")
		}
		token = key
	}
	cid := recorded[token]
	if cid == "" {
		cid = recorded[volume]
	}
	if cid == "" {
		cid, err = pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
		if err != nil {
			return resolvedDisk{}, err
		}
	}
	birth, meta, err := decodeDiskCID(ctx, deps, "delete_vm.preserve_disk", cid)
	if err != nil {
		return resolvedDisk{}, err
	}
	if token != "" {
		if meta == nil {
			meta = &pve.DiskCIDMeta{ID: token}
		} else if meta.ID != token {
			return resolvedDisk{}, fmt.Errorf("recorded persistent disk CID contradicts live identity")
		}
	}
	disk, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_disk", cid, birth, meta)
	if err != nil {
		return resolvedDisk{}, err
	}
	if disk.stableID == "" && disk.allocation == nil {
		if recorded[volume] == "" || birth != volume || strings.HasPrefix(slot, "unused") {
			return resolvedDisk{}, fmt.Errorf("legacy disk lacks safe active-volume preservation proof")
		}
		present, err := observeManagedDiskVolume(ctx, deps, node, volume, meta)
		if err != nil || !present {
			return resolvedDisk{}, fmt.Errorf("legacy persistent volume is not observed")
		}
		disk.holder = &pve.DiskHolder{Found: true, Node: node, VMID: vmid, Slot: slot}
	}
	if disk.holder == nil || disk.holder.Node != node || disk.holder.VMID != vmid || disk.volid != volume || disk.intent != nil {
		return resolvedDisk{}, fmt.Errorf("persistent disk has no unambiguous current ownership proof")
	}
	if disk.allocation == nil && recorded[token] == "" && recorded[volume] == "" {
		return resolvedDisk{}, fmt.Errorf("legacy persistent disk lacks its recorded CPI CID")
	}
	return disk, nil
}
