package handlers

import (
	"context"
	"fmt"
	"maps"
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
	// We read both views, because the scan reports a slot whose delete is
	// pending from the current view, and the own-slot check below has to find
	// it there.
	views, err := pve.ReadQemuViews(ctx, deps.PVE, node, vmid)
	if err != nil {
		return err
	}
	cfg := views.Applied()
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
		after, err := pve.ReadQemuViews(ctx, deps.PVE, node, vmid)
		if err != nil || pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(after.Applied()))[volume] != cid {
			return fmt.Errorf("retained ephemeral source provenance was not persisted")
		}
		// The note write moved the config on, so the read that confirmed it
		// is the config the resolution below has to agree with.
		views, cfg = after, after.Applied()
	}
	// A legacy ephemeral slot carries no serial, and a token minted above is
	// on no slot yet, so the resolution refuses the volume whenever this VM's
	// own slot is what names it. We take that refusal as this VM's slot only
	// when that slot is the one entry naming the volume, in the same config we
	// read above, so a volume that another guest or a second slot here also
	// names is never retained. Every other error, the offline-node one
	// included, goes back unchanged. The digest check sees only this VM's
	// config as the scan read it, so two changes get past it. Another guest can
	// gain a slot for the volume after the scan has read that guest. This VM
	// can also change after the scan reads it and before the transfer reads it
	// again. The transfer's own checks cover the second change, and no test row
	// covers either one.
	identity, err := resolveDiskForOp(ctx, deps, "retain_ephemeral", cid, volume, meta)
	if held, ok := pve.IsDiskBirthNameHeld(err); ok {
		// The scan reports a slot from the applied view and, while the slot's
		// delete is pending, from the current view, so we read the own slot
		// the same way. The applied view's keys, the digest among them, win.
		seen := views.Current()
		maps.Copy(seen, cfg)
		identity, err = retainedOwnSlotIdentity(held, seen, node, vmid, cid, volume, meta)
	}
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

// retainedOwnSlotIdentity resolves a retained ephemeral volume whose only
// name holder is a slot of the VM being deleted. The resolution's refusal
// lists every entry naming the volume, and we accept it only when that list
// is one serial-less slot of this VM on this node, read from a config whose
// digest matches cfg's, and cfg names the volume in that slot. The scan
// reads every guest's config again, so the digest is what proves both reads
// saw the same config. A bus slot becomes the holder. An unused entry leaves
// the holder empty, so the caller's sole-reference proof decides it, as
// before.
func retainedOwnSlotIdentity(
	held *pve.DiskBirthNameHeldError, cfg map[string]any, node string, vmid int, cid, volume string, meta *pve.DiskCIDMeta,
) (resolvedDisk, error) {
	if len(held.Holders) != 1 {
		return resolvedDisk{}, fmt.Errorf("retained ephemeral ownership is ambiguous")
	}
	own := held.Holders[0]
	value, _ := cfg[own.Slot].(string)
	if own.VMID != vmid || own.Node != node || own.Serial != "" || strings.Split(value, ",")[0] != volume {
		return resolvedDisk{}, fmt.Errorf("retained ephemeral ownership is ambiguous")
	}
	if digest, _ := pve.ConfigString(cfg, "digest"); digest == "" || own.Digest != digest {
		return resolvedDisk{}, fmt.Errorf("retained ephemeral config changed while its volume was resolved")
	}
	rd := resolvedDisk{diskCID: cid, birth: volume, volid: volume, meta: meta, stableID: meta.ID}
	if !own.Unused {
		rd.holder = &pve.DiskHolder{Found: true, VMID: vmid, Node: node}
	}
	return rd, nil
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
