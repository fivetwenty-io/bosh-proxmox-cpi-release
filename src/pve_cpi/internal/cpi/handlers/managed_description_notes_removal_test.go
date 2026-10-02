package handlers

import (
	"context"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestGuardSkipsReadbackOnlyForPinnedNoteRemoval pins how narrow the guard's
// readback skip is. Only a description-only write that carries its caller's
// digest and drops notes from the guard's own read, the write
// pve.RemoveDescriptionNotes sends, is observed without reading the VM back.
// A write the guard stamps with its own digest, a write that adds a note or
// changes other text, and a write with another field all keep the exact
// readback match they had before.
func TestGuardSkipsReadbackOnlyForPinnedNoteRemoval(t *testing.T) {
	const (
		volume  = "nfs-persistent-1:20768/owned.qcow2"
		digest  = "d1"
		notes   = `<!--BOSH:{"bosh_attached_disks":{"tok-a":"cid-a","tok-b":"cid-b"},"bosh_disk_opt_overlays":{"tok-a":{"cache":"writeback"}}}-->`
		removed = `<!--BOSH:{"bosh_attached_disks":{"tok-b":"cid-b"}}-->`
		added   = `<!--BOSH:{"bosh_attached_disks":{"tok-a":"cid-a","tok-b":"cid-b","tok-c":"cid-c"},"bosh_disk_opt_overlays":{"tok-a":{"cache":"writeback"}}}-->`
	)
	before := "kept text\n" + notes
	pinned := digest
	on := true
	for _, row := range []struct {
		name   string
		params sdknodes.UpdateQemuConfigParams
		skip   bool
	}{
		{"pinned removal", sdknodes.UpdateQemuConfigParams{Description: new("kept text\n" + removed), Digest: &pinned}, true},
		{"removal the guard stamps", sdknodes.UpdateQemuConfigParams{Description: new("kept text\n" + removed)}, false},
		{"pinned addition", sdknodes.UpdateQemuConfigParams{Description: new("kept text\n" + added), Digest: &pinned}, false},
		{"pinned removal that changes other text", sdknodes.UpdateQemuConfigParams{Description: new("other text\n" + removed), Digest: &pinned}, false},
		{"pinned removal with protection", sdknodes.UpdateQemuConfigParams{Description: new("kept text\n" + removed), Digest: &pinned, Protection: &on}, false},
		{"pinned write that changes nothing", sdknodes.UpdateQemuConfigParams{Description: new(before), Digest: &pinned}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			guard := managedDiskLifecycleGuard{lifecycle: &managedDiskLifecycle{disk: resolvedDisk{volid: volume}}}
			params := row.params
			call := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig", Args: map[string]any{managedArgumentParams: &params}}
			observation := managedDiskMutationObservation{before: map[string]any{"digest": digest, "description": before}}
			if err := guard.prepareConfig(call, &observation); err != nil {
				t.Fatalf("prepareConfig: %v", err)
			}
			if observation.notesRemoval != row.skip {
				t.Fatalf("the guard marks the write as a pinned note removal: %t, want %t", observation.notesRemoval, row.skip)
			}
			if !row.skip {
				return
			}
			// The guard has no client here, so a readback would panic.
			volumes, err := guard.observeConfigResult(context.Background(), call, observation, nil)
			if err != nil || len(volumes) != 1 || volumes[0] != volume {
				t.Fatalf("pinned note removal observed as %v, %v; want [%s] with no readback", volumes, err, volume)
			}
		})
	}
}
