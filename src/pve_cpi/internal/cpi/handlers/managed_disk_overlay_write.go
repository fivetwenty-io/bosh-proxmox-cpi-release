package handlers

import (
	"bytes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// lifecycleMutationTouchesDisk reports whether a mutation the lifecycle guard
// just admitted counts as a disk mutation for cleanLockTimeout. A bosh-lock-
// sentinel create or delete does not, and neither does a config write of the
// drive-option overlay note alone, nor a pinned removal of notes from a VM
// that doesn't name the volume. Everything else does.
//
// The removal is the detach tail's write to the VM a parked disk came off. The
// guard has checked that the removal only drops notes from the description it
// read. That read shows the disk on no slot, unused entry, or pending change,
// and PVE applies the removal only against that read. So the removal leaves
// the disk, and whatever holds it, where they were, and a parker lock timeout
// that follows it can still be clean. The tail runs again on the retry and finds nothing
// left to remove.
func lifecycleMutationTouchesDisk(call ManagedAllocationMutation, observation managedDiskMutationObservation, volume string) bool {
	if call.Service == managedDiskServicePool {
		return false
	}
	if call.Service+"."+call.Method != "Nodes.UpdateQemuConfig" {
		return true
	}
	if observation.notesRemoval && !lifecycleConfigHasVolume(observation.before, volume) {
		return false
	}
	return !lifecycleOverlayOnlyConfigWrite(observation.before, observation.fields)
}

// lifecycleOverlayOnlyConfigWrite reports whether a guarded config write
// changes nothing on the holder but its bosh_disk_opt_overlays note. That is
// the note attachOverlayForHolder writes onto the receiving VM before an
// attach from a parker takes the parker lock. The guard records such a write
// as a step exactly as it records any other, but it does not count it as a
// disk mutation, so a parker lock timeout that follows it can still be clean.
//
// The write qualifies only when its fields are the description and nothing
// else, apart from the digest the guard stamps. The description the guard read
// just before the write and the one being written must then parse, through the
// shared sentinel codec, to the same text outside the sentinel and to
// byte-identical raw values for every sentinel key except the overlay key.
// Anything else, including a description write that also changes provenance or
// any other key, still counts as a disk mutation.
//
// This is sound for three reasons. First, nothing that decides where the disk
// is reads the overlay key. Its only readers are attach_disk's drive-string
// merge, detach_disk and delete_vm copying the overrides onto a park, and
// update_disk's read-modify-write. No allocation decision, placement charge,
// holder resolution, or storage audit reads it. Second, a stale entry on a VM
// the attach never completes onto is keyed by the disk and is overwritten by
// the next attach. Third, the guard stamps the digest of the config it just
// read onto the write, and PVE applies a digest-stamped write only against that
// exact config, so comparing the two descriptions is proof of what the write
// changes rather than trust in the caller.
func lifecycleOverlayOnlyConfigWrite(before map[string]any, fields map[string]any) bool {
	written, ok := fields[pveConfigKeyDescription].(string)
	if !ok {
		return false
	}
	for key := range fields {
		if key != pveConfigKeyDescription && key != "digest" {
			return false
		}
	}
	beforeText, beforeRaw := pve.ParseSentinel(pve.DescriptionFromConfig(before))
	afterText, afterRaw := pve.ParseSentinel(written)
	if beforeText != afterText {
		return false
	}
	delete(beforeRaw, pve.DiskOptOverlaysSentinelKey)
	delete(afterRaw, pve.DiskOptOverlaysSentinelKey)
	if len(beforeRaw) != len(afterRaw) {
		return false
	}
	for key, value := range beforeRaw {
		other, present := afterRaw[key]
		if !present || !bytes.Equal(value, other) {
			return false
		}
	}
	return true
}
