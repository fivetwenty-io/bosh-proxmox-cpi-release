// detach_disk_birth_held_stranded_internal_test.go covers the notes cleanup
// after attach_disk moves a stranded stable-ID disk off the VM that holds it on
// an unused entry. The cleanup is the one detach_disk runs, so it must leave a
// name that another disk on that VM holds to the disk that holds it.
package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// strandedBirthNotes rewrites VM 777's description so it carries, besides the
// note that ties the stranded disk A to its entry, an overlay for A under its
// stable ID and a note and an overlay for a legacy disk B under A's birth
// name. It returns the raw entries B owns.
func strandedBirthNotes(t *testing.T, s *strandedDisk) (note, overlay json.RawMessage) {
	t.Helper()
	cfg := s.client.state.configs[777]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	notes := map[string]string{}
	if err := json.Unmarshal(raw["bosh_attached_disks"], &notes); err != nil {
		t.Fatal(err)
	}
	notes[s.stranded] = "cid-for-b"
	rawNotes, err := json.Marshal(notes)
	if err != nil {
		t.Fatal(err)
	}
	rawOverlays, err := json.Marshal(map[string]map[string]string{
		s.token:    {"cache": "none"},
		s.stranded: {"cache": "unsafe", "mbps_rd": s.stranded},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_attached_disks"] = rawNotes
	raw[pve.DiskOptOverlaysSentinelKey] = rawOverlays
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	gotNotes, gotOverlays := birthHeldEntries(t, description)
	return gotNotes[s.stranded], gotOverlays[s.stranded]
}

// TestStrandedAttachKeepsNotesOnAHeldBirthName strands A on VM 777's unused
// entry under its birth name and recovers it with an attach_disk to VM 888. The
// move frees that name, and a legacy disk B takes it on 777's scsi2 before the
// cleanup reads the VM. A's notes under its stable ID come off, and B's note
// and overlay under the birth name come through byte for byte.
func TestStrandedAttachKeepsNotesOnAHeldBirthName(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	wantNote, wantOverlay := strandedBirthNotes(t, s)
	birth := s.stranded
	const legacyDrive = "size=3G"
	s.client.afterMove = func() {
		cfg := s.client.state.configs[777]
		if _, placed := cfg["scsi2"]; placed {
			return
		}
		cfg["scsi2"] = birth + "," + legacyDrive
		s.client.state.volumes[birth] = &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
	}

	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach_disk(888): %v", err)
	}

	cfg := s.client.state.configs[777]
	if got, _ := cfg["scsi2"].(string); got != birth+","+legacyDrive {
		t.Fatalf("scsi2 = %q, want the legacy disk left in place", got)
	}
	notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(cfg))
	if string(notes[birth]) != string(wantNote) {
		t.Errorf("legacy disk's note = %s, want %s unchanged", notes[birth], wantNote)
	}
	if string(overlays[birth]) != string(wantOverlay) {
		t.Errorf("legacy disk's overlay = %s, want %s unchanged", overlays[birth], wantOverlay)
	}
	if _, kept := notes[s.token]; kept {
		t.Error("the stranded disk's note under its stable ID is still there")
	}
	if _, kept := overlays[s.token]; kept {
		t.Error("the stranded disk's overlay under its stable ID is still there")
	}
}
