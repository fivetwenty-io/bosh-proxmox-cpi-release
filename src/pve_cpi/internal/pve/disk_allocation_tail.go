package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// configDigestRefusalText is the message qemu-server's update_vm_api dies with
// when a configuration update carries a digest that no longer matches the
// configuration it loads under the VM's lock. The synchronous PUT on
// {vmid}/config runs that check in the request, before any write and without
// a task, so the text comes back as the request's own error answer.
const configDigestRefusalText = "checksum mismatch (file change by other user?)"

// IsConfigDigestRefusal reports whether err is PVE's answer to a configuration
// update whose digest was stale. Only an answer counts: a transport fault, a
// timeout, an ended context, or a gateway or relay status may hide a write
// that landed, so none of them matches.
func IsConfigDigestRefusal(err error) bool {
	if err == nil || !strings.Contains(err.Error(), configDigestRefusalText) {
		return false
	}
	_, answered := pveAnswered(err)
	return answered
}

// ReadParkerSourceVMID returns the VM a parker's landed entry for a disk says
// the disk was moved off, when the entry names one. A parker whose config is
// gone has no entry to read, and reports none.
func ReadParkerSourceVMID(ctx context.Context, c Client, node string, parkerVMID int, bareVolid, stableID string) (int, bool, error) {
	if c == nil || node == "" || parkerVMID <= 0 || bareVolid == "" {
		return 0, false, fmt.Errorf("parker source lookup requires a client, node, parker VMID, and volid")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if parkerConfigGone(err) {
			return 0, false, nil
		}
		return 0, false, WrapConfigReadError(err)
	}
	_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	key, found := matchParkerOverlayEntry(disks, bareVolid, stableID)
	if !found {
		return 0, false, nil
	}
	vmid, ok := provenanceSourceVMID(disks[key])
	return vmid, ok, nil
}

// VMConfigGone reports whether err is an answer that parkerConfigGone reads as
// a VM whose config is gone, either a 404 or pmxcfs saying the configuration
// file doesn't exist.
func VMConfigGone(err error) bool {
	return parkerConfigGone(err)
}

// RemoveDiskAllocationEntry removes the allocation entry stored under key from
// a VM whose configuration the caller has already read as cfg, along with the
// attached-disk entries under attachedKeys and the drive-option overlays under
// overlayKeys, in the one write RemoveDescriptionNotes sends. The entry must
// still be exactly the one the caller checked.
func RemoveDiskAllocationEntry(ctx context.Context, c Client, node string, vmid int, key string, expected DiskAllocationProvenance, cfg map[string]any, attachedKeys, overlayKeys []string) error {
	if key == "" || cfg == nil {
		return fmt.Errorf("managed disk provenance requires a concrete holder")
	}
	entries, err := ParseDiskAllocationProvenance(DescriptionFromConfig(cfg))
	if err != nil {
		return err
	}
	if prior, exists := entries[key]; !exists || prior != expected {
		return fmt.Errorf("managed disk provenance location changed before removal")
	}
	return RemoveDescriptionNotes(ctx, c, node, vmid, cfg, DescriptionNoteKeys{
		Allocations:   []string{key},
		AttachedDisks: attachedKeys,
		Overlays:      overlayKeys,
	})
}

// DescriptionNoteKeys names the notes one description write removes, by the
// key each carrier files them under. Allocations holds bosh_disk_allocations
// keys, AttachedDisks holds bosh_attached_disks keys, and Overlays holds
// bosh_disk_opt_overlays keys, each a stable ID or a bare volid.
type DescriptionNoteKeys struct {
	Allocations   []string
	AttachedDisks []string
	Overlays      []string
}

// ErrDescriptionNoteReturned marks a description write that PVE accepted,
// after which the VM's description carries one of the removed notes again.
// Only a later writer can have put it back, so the caller can read the VM
// again and retry.
var ErrDescriptionNoteReturned = errors.New("a removed description note is back")

// RemoveDescriptionNotes removes the notes keys names from a VM whose
// configuration the caller has already read as cfg. It builds one description
// from that read and sends it with the read's digest, so PVE refuses it if the
// configuration changed since, and no note another writer added or removed in
// the meantime comes back. A key the read doesn't carry is skipped, and when
// nothing is left to remove it writes nothing. After PVE accepts the write, it
// reads the VM again and checks only that the notes it removed are gone,
// because any other change to the description is another writer's.
//
// A failed write keeps its cause, so the caller can tell PVE's digest refusal,
// which IsConfigDigestRefusal matches, from a write whose outcome is unknown.
// A note that is back after the write comes back as ErrDescriptionNoteReturned.
func RemoveDescriptionNotes(ctx context.Context, c Client, node string, vmid int, cfg map[string]any, keys DescriptionNoteKeys) error {
	if c == nil || c.Nodes() == nil || c.QEMU() == nil || node == "" || vmid <= 0 || cfg == nil {
		return fmt.Errorf("description note removal requires a client, a node, a VMID, and a config read")
	}
	digest, ok := ConfigString(cfg, "digest")
	if !ok || digest == "" {
		return fmt.Errorf("description note removal needs a config read that carries a digest")
	}
	description := DescriptionFromConfig(cfg)
	newDesc, err := withoutDescriptionNotes(description, keys)
	if err != nil {
		return err
	}
	removed, ok := DescriptionNotesRemoved(description, newDesc)
	if !ok {
		return nil
	}
	params := &sdknodes.UpdateQemuConfigParams{Description: &newDesc, Digest: &digest}
	if err := c.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), params); err != nil {
		return fmt.Errorf("cannot remove description notes: %w", err)
	}
	check, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil || check == nil {
		return errors.Join(fmt.Errorf("cannot read back description note removal"), err)
	}
	return requireNotesGone(DescriptionFromConfig(check), removed)
}

// DescriptionNotesRemoved reports which notes sent drops from before, and
// whether sent is exactly before with those notes removed, rendered the way
// RemoveDescriptionNotes renders it. A write that adds or changes anything,
// or that removes nothing, doesn't count.
func DescriptionNotesRemoved(before, sent string) (DescriptionNoteKeys, bool) {
	var keys DescriptionNoteKeys
	if beforeEntries, err := ParseDiskAllocationProvenance(before); err == nil {
		sentEntries, err := ParseDiskAllocationProvenance(sent)
		if err != nil {
			return DescriptionNoteKeys{}, false
		}
		keys.Allocations = keysMissingFrom(beforeEntries, sentEntries)
	}
	_, beforeDisks, _ := parseAttachedDisksSentinel(before)
	_, sentDisks, _ := parseAttachedDisksSentinel(sent)
	keys.AttachedDisks = keysMissingFrom(beforeDisks, sentDisks)
	_, beforeOverlays, _ := parseDiskOptOverlaysSentinel(before)
	_, sentOverlays, _ := parseDiskOptOverlaysSentinel(sent)
	keys.Overlays = keysMissingFrom(beforeOverlays, sentOverlays)
	if len(keys.Allocations) == 0 && len(keys.AttachedDisks) == 0 && len(keys.Overlays) == 0 {
		return DescriptionNoteKeys{}, false
	}
	rebuilt, err := withoutDescriptionNotes(before, keys)
	if err != nil || rebuilt != sent {
		return DescriptionNoteKeys{}, false
	}
	return keys, true
}

// requireNotesGone returns ErrDescriptionNoteReturned when desc still carries
// any of the notes removed names.
func requireNotesGone(desc string, removed DescriptionNoteKeys) error {
	if len(removed.Allocations) > 0 {
		entries, err := ParseDiskAllocationProvenance(desc)
		if err != nil {
			return fmt.Errorf("cannot read back description note removal: %w", err)
		}
		if carriesAnyKey(entries, removed.Allocations) {
			return fmt.Errorf("%w: an allocation entry", ErrDescriptionNoteReturned)
		}
	}
	if _, disks, _ := parseAttachedDisksSentinel(desc); carriesAnyKey(disks, removed.AttachedDisks) {
		return fmt.Errorf("%w: an attached-disk entry", ErrDescriptionNoteReturned)
	}
	if _, overlays, _ := parseDiskOptOverlaysSentinel(desc); carriesAnyKey(overlays, removed.Overlays) {
		return fmt.Errorf("%w: a drive-option overlay", ErrDescriptionNoteReturned)
	}
	return nil
}

// withoutDescriptionNotes removes the notes keys names from desc. The
// allocation carrier is read strictly, and only when keys names an allocation
// entry. The attached-disk and overlay carriers use the same codecs as
// RemoveAttachedDiskCID and RemoveVMDiskOptOverlay. A carrier with nothing to
// remove, or a note carrier whose JSON doesn't decode, is left as it was.
func withoutDescriptionNotes(desc string, keys DescriptionNoteKeys) (string, error) {
	if len(keys.Allocations) > 0 {
		nonBOSH, raw, err := strictDiskAllocationSentinel(desc)
		if err != nil {
			return "", err
		}
		entries, err := ParseDiskAllocationProvenance(desc)
		if err != nil {
			return "", err
		}
		if deleteNoteKeys(entries, keys.Allocations) {
			if len(entries) == 0 {
				delete(raw, diskAllocationsKey)
			} else {
				encoded, err := json.Marshal(entries)
				if err != nil {
					return "", err
				}
				raw[diskAllocationsKey] = encoded
			}
			if desc, err = RenderSentinel(nonBOSH, raw); err != nil {
				return "", err
			}
		}
	}
	nonBOSH, disks, raw := parseAttachedDisksSentinel(desc)
	if deleteNoteKeys(disks, keys.AttachedDisks) {
		rendered, err := renderAttachedDisksSentinel(nonBOSH, disks, raw)
		if err != nil {
			return "", err
		}
		desc = rendered
	}
	nonBOSH, overlays, raw := parseDiskOptOverlaysSentinel(desc)
	if deleteNoteKeys(overlays, keys.Overlays) {
		rendered, err := renderDiskOptOverlaysSentinel(nonBOSH, overlays, raw)
		if err != nil {
			return "", err
		}
		desc = rendered
	}
	return desc, nil
}

// deleteNoteKeys deletes every non-empty key in keys from notes and reports
// whether any was there.
func deleteNoteKeys[V any](notes map[string]V, keys []string) bool {
	removed := false
	for _, key := range keys {
		if _, found := notes[key]; key != "" && found {
			delete(notes, key)
			removed = true
		}
	}
	return removed
}

// keysMissingFrom returns, in sorted order, the keys of before that after
// doesn't carry.
func keysMissingFrom[V any](before, after map[string]V) []string {
	var missing []string
	for key := range before {
		if _, kept := after[key]; !kept {
			missing = append(missing, key)
		}
	}
	slices.Sort(missing)
	return missing
}

// carriesAnyKey reports whether notes carries any of keys.
func carriesAnyKey[V any](notes map[string]V, keys []string) bool {
	for _, key := range keys {
		if _, found := notes[key]; found {
			return true
		}
	}
	return false
}
