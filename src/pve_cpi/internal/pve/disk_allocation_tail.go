package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// IsAnsweredConfigRefusal reports whether err is PVE's own answer refusing a
// configuration update. That's any 4xx status, such as a token that lacks the
// privilege or a parameter PVE won't take, and a 500 whose message says the VM
// is locked, which qemu-server's check_lock gives before the update writes
// anything. A transport fault, a timeout, an ended context, a gateway or relay
// status, and any other 500 may hide a write that landed, so none of them
// matches. A match says only that PVE answered. It doesn't prove that nothing
// was written, and only a read of the VM afterwards can show that.
//
// err has to be the update's own error. A read that fails after PVE accepted
// the update is no refusal, so RemoveDescriptionNotes returns it as
// ErrDescriptionReadbackFailed, which never matches.
func IsAnsweredConfigRefusal(err error) bool {
	code, answered := pveAnswered(err)
	if !answered {
		return false
	}
	return code >= 400 && code < 500 || code == 500 && IsVMConfigLocked(err)
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

// ErrDescriptionReadbackFailed marks a description write that PVE accepted
// and that the read after it couldn't check. Nothing then shows what the
// write changed, so the caller treats its outcome as unknown.
var ErrDescriptionReadbackFailed = errors.New("cannot read back description note removal")

// RemoveDescriptionNotes removes the notes keys names from a VM whose
// configuration the caller has already read as cfg. The function builds one
// description from that read and renders it the way withoutDescriptionNotes
// does, which can move or drop free text that sits after the sentinel. The
// function sends that description with the read's digest, so PVE refuses it
// if the configuration changed since, and the write never reverts a note that
// another writer added or removed in the meantime. A key the read doesn't
// carry is skipped, and when nothing is left to remove, the function writes
// nothing. After PVE accepts the write, the function reads the VM again and
// checks only that the notes it removed are gone, because any other change to the
// description is another writer's.
//
// A failed write keeps its cause, so the caller can tell PVE's digest refusal,
// which IsConfigDigestRefusal matches, from a write whose outcome is unknown.
// A note that is back after the write comes back as ErrDescriptionNoteReturned.
// A readback that fails after PVE accepted the write comes back as
// ErrDescriptionReadbackFailed, with its cause in the text only, so no status
// in that cause can make the accepted write look refused.
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
	if err != nil {
		// The %v keeps the read's status out of the chain, so it can't make the
		// accepted write look refused. It also flattens the class of a typed
		// cause to retriable on purpose, because a readback after an accepted
		// write is a read we can safely repeat.
		return fmt.Errorf("%w: %v", ErrDescriptionReadbackFailed, err) //nolint:errorlint // The read's cause stays as text, so its status can't make the accepted write look refused.
	}
	if check == nil {
		return ErrDescriptionReadbackFailed
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

// DescriptionNotesKept reports whether the after description still carries
// every note that keys names, each exactly as the before description carried
// it. If before doesn't carry a note, that note doesn't count as kept. A
// carrier that fails to decode doesn't count either when keys names one of its
// notes.
func DescriptionNotesKept(before, after string, keys DescriptionNoteKeys) bool {
	if len(keys.Allocations) > 0 {
		beforeEntries, err := ParseDiskAllocationProvenance(before)
		if err != nil {
			return false
		}
		afterEntries, err := ParseDiskAllocationProvenance(after)
		if err != nil {
			return false
		}
		if !notesKept(beforeEntries, afterEntries, keys.Allocations, func(a, b DiskAllocationProvenance) bool { return a == b }) {
			return false
		}
	}
	_, beforeDisks, _ := parseAttachedDisksSentinel(before)
	_, afterDisks, _ := parseAttachedDisksSentinel(after)
	if !notesKept(beforeDisks, afterDisks, keys.AttachedDisks, func(a, b string) bool { return a == b }) {
		return false
	}
	_, beforeOverlays, _ := parseDiskOptOverlaysSentinel(before)
	_, afterOverlays, _ := parseDiskOptOverlaysSentinel(after)
	return notesKept(beforeOverlays, afterOverlays, keys.Overlays, maps.Equal[map[string]string])
}

// notesKept reports whether the after map carries every key in keys with the
// value that the before map carries under the same key.
func notesKept[V any](before, after map[string]V, keys []string, equal func(a, b V) bool) bool {
	for _, key := range keys {
		was, found := before[key]
		if !found {
			return false
		}
		now, found := after[key]
		if !found || !equal(was, now) {
			return false
		}
	}
	return true
}

// requireNotesGone returns ErrDescriptionNoteReturned when desc still carries
// any of the notes removed names.
func requireNotesGone(desc string, removed DescriptionNoteKeys) error {
	if len(removed.Allocations) > 0 {
		entries, err := ParseDiskAllocationProvenance(desc)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrDescriptionReadbackFailed, err)
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
//
// A carrier that loses a note is rendered again, and the text outside the
// sentinel doesn't always come through as it was. When an allocation entry
// comes off, the strict codec joins the text before and after the sentinel,
// trims it, and puts all of it before the new sentinel. When only an
// attached-disk entry or an overlay comes off, the shared codec keeps the
// trimmed text before the sentinel and drops any text after it, as every
// other writer of those two carriers does.
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
