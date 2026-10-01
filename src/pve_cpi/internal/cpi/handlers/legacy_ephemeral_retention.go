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
	// Both views, so an ephemeral slot whose delete is pending is still found
	// and retained rather than left for the destroy.
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if pve.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := refusePendingDriveReplacement("delete_vm", vmCID, holding); err != nil {
		return false, err
	}
	cfg := holding.Config
	tags, _ := pve.ConfigString(cfg, jsonKeyTags)
	if !tagsContain(tags, tagRetainEphemeral) {
		return false, nil
	}
	slots := findEphemeralActiveDisks(cfg, vmid)
	for slot, volume := range pve.FindUnusedDiskEntries(cfg) {
		if _, _, err := pve.ParseDiskCID(volume); err != nil {
			return false, err
		}
		if pve.IsOwnEphemeralVolume(volume, vmid) {
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
		if !pve.IsOwnEphemeralVolume(birth, vmid) {
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
		// The retention moves the volume off the VM it is destroying, so a
		// resume applies a pending delete it finds on the stopped source.
		parkContext.ApplyFoundPendingDelete = true
		parkerCfg := parkerWriteConfigFor(deps)
		if _, err := resumeDiskTransferToParker(ctx, deps.PVE, logger, *identity.intent, meta.ID, parkerCfg, parkContext); err != nil {
			return err
		}
		// The resume lands the disk on a parker the interrupted transfer had
		// already created, and that parker is outside the pool until somebody
		// sweeps, so we sweep here as every other park funnel does. The node is
		// the parker's own, which on a cluster is not always the node this
		// delete is running against.
		sweepParkerPool(ctx, deps, identity.intent.ParkerNode, parkerCfg)
	case identity.holder != nil && identity.holder.Node == node && identity.holder.VMID == vmid:
		parkerCfg := parkerWriteConfigFor(deps)
		if _, err := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, identity.volid, parkerCfg, parkContext); err != nil {
			return err
		}
		sweepParkerPool(ctx, deps, node, parkerCfg)
	case identity.holder == nil:
		sole, err := legacyUnusedSoleHolder(ctx, deps, node, vmid, volume, identity.volid)
		if err != nil {
			return err
		}
		if !sole {
			return fmt.Errorf("retained ephemeral ownership is ambiguous")
		}
		parkerCfg := parkerWriteConfigFor(deps)
		if _, err := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, volume, parkerCfg, parkContext); err != nil {
			return err
		}
		sweepParkerPool(ctx, deps, node, parkerCfg)
	case !identity.holder.IsParker:
		return fmt.Errorf("retained ephemeral ownership is ambiguous")
	}
	disk := resolvedDisk{diskCID: cid, birth: volume, volid: volume, meta: meta, stableID: meta.ID}
	return verifyLegacyDiskPreservation(ctx, deps, disk)
}

// legacyUnusedSoleHolder reports whether this VM holds volume as its own
// ephemeral disk even though no active slot anywhere names it, so the identity
// scan found no holder. A retention transfer that stopped after deleting the
// source slot leaves that state, because PVE turns the deleted slot into an
// unusedN entry, and so does an operator's detach without force. The volume
// must carry this VM's ephemeral name, the scan must not have resolved it to
// another name, and the only reference to it in any guest's current config
// must be one unusedN key of this VM on this node. Anything else stays
// ambiguous, because another guest, or a second slot of this one, could be
// the volume's real owner.
func legacyUnusedSoleHolder(ctx context.Context, deps Deps, node string, vmid int, volume, resolved string) (bool, error) {
	if resolved != volume || !pve.IsOwnEphemeralVolume(volume, vmid) {
		return false, nil
	}
	refs, err := pve.FindVolumeReferences(ctx, deps.PVE, volume)
	if err != nil {
		return false, err
	}
	if len(refs) != 1 {
		return false, nil
	}
	ref := refs[0]
	return ref.VMID == vmid && ref.Node == node && strings.HasPrefix(ref.Slot, "unused"), nil
}
