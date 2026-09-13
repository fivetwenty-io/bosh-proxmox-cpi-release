package handlers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func retainLegacyEphemeralDisks(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) (bool, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if pve.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tags, _ := pve.ConfigString(cfg, jsonKeyTags)
	if !tagsContain(tags, tagRetainEphemeral) {
		return false, nil
	}
	slots := findEphemeralActiveDisks(cfg, vmid)
	for slot, volume := range pve.FindUnusedDiskEntries(cfg) {
		_, name, err := pve.ParseDiskCID(volume)
		if err != nil {
			return false, err
		}
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			name = name[slash+1:]
		}
		if strings.HasPrefix(name, fmt.Sprintf("vm-%d%s", vmid, ephemeralVolidInfix)) {
			slots[slot] = volume
		}
	}
	volumes := map[string]bool{}
	for _, volume := range slots {
		volumes[volume] = true
	}
	// A transfer can have finished before the original VM's delete. Verify its
	// durable source CID on retry, including a transfer left in an unused slot.
	for birth, cid := range pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg)) {
		_, name, err := pve.ParseDiskCID(birth)
		if err != nil {
			continue
		}
		if slash := strings.LastIndex(name, "/"); slash >= 0 {
			name = name[slash+1:]
		}
		if !strings.HasPrefix(name, fmt.Sprintf("vm-%d%s", vmid, ephemeralVolidInfix)) {
			continue
		}
		if _, meta, err := decodeDiskCID(ctx, deps, "retain_ephemeral", cid); err != nil || meta == nil || meta.ID == "" {
			return false, fmt.Errorf("retained ephemeral source CID is malformed")
		}
		volumes[birth] = true
	}
	ordered := make([]string, 0, len(volumes))
	for volume := range volumes {
		ordered = append(ordered, volume)
	}
	sort.Strings(ordered)
	for _, volume := range ordered {
		if err := retainLegacyEphemeralVolume(ctx, deps, node, vmCID, vmid, volume, logger); err != nil {
			return false, err
		}
	}
	return true, nil
}

func retainLegacyEphemeralVolume(ctx context.Context, deps Deps, node, vmCID string, vmid int, volume string, logger *log.Logger) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return err
	}
	if _, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(cfg)); err != nil {
		return err
	}
	cid := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg))[volume]
	var meta *pve.DiskCIDMeta
	if cid != "" {
		birth, parsed, err := decodeDiskCID(ctx, deps, "retain_ephemeral", cid)
		if err != nil || birth != volume || parsed == nil || parsed.ID == "" {
			return fmt.Errorf("retained ephemeral source CID differs")
		}
		meta = parsed
	} else {
		present, err := observeManagedDiskVolume(ctx, deps, node, volume, nil)
		if err != nil || !present {
			return fmt.Errorf("retained ephemeral volume not observed")
		}
		token, err := pve.GenerateDiskStableID()
		if err != nil {
			return err
		}
		meta = &pve.DiskCIDMeta{ID: token}
		cid, err = pve.EncodeDiskCID(volume, meta)
		if err != nil {
			return err
		}
		pve.UpdateAttachedDiskCID(ctx, deps.PVE, logger, node, vmid, volume, cid)
		after, err := deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil || pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(after))[volume] != cid {
			return fmt.Errorf("retained ephemeral source provenance was not persisted")
		}
	}
	identity, err := resolveDiskForOp(ctx, deps, "retain_ephemeral", cid, volume, meta)
	if err != nil {
		return err
	}
	parkContext := pve.ParkContext{DiskCID: cid, SourceVMCID: vmCID, StableID: meta.ID}
	switch {
	case identity.intent != nil:
		if _, err := pve.ResumeDiskTransferToParker(ctx, deps.PVE, logger, *identity.intent, meta.ID, parkerWriteConfigFor(deps), parkContext); err != nil {
			return err
		}
	case identity.holder != nil && identity.holder.Node == node && identity.holder.VMID == vmid:
		parkerCfg := parkerWriteConfigFor(deps)
		if _, err := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, identity.volid, parkerCfg, parkContext); err != nil {
			return err
		}
		sweepParkerPool(ctx, deps, node, parkerCfg)
	case identity.holder == nil || !identity.holder.IsParker:
		return fmt.Errorf("retained ephemeral ownership is ambiguous")
	}
	disk := resolvedDisk{diskCID: cid, birth: volume, volid: volume, meta: meta, stableID: meta.ID}
	return verifyLegacyDiskPreservation(ctx, deps, disk)
}
