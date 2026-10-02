package handlers

// A renamed disk whose delete_disk an earlier release refused at its
// completion audit can still carry its allocation entry on the VM it was
// moved off, under its old name, after its volume is gone. When another disk
// holds the old name on a slot of that VM, a retry reaches delete_disk's
// absence path, which removes that entry, and the attached-disk notes that
// map to the disk's own CID, in one write pinned to the digest of its read,
// before the completion audit runs. Most rows run that retry with a
// hand-attached volume on the old name. The last row leaves the old name
// free and shows that the retry still refuses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// staleNote is a renamed disk in reconciliation_required with its volume
// gone, its allocation entry and its attached-disk note back on 777, and,
// unless the row asks for none, a hand-attached volume on its old name in
// 777's scsi3.
type staleNote struct {
	f   digestManaged
	key string
	// before is 777's notes once the stale notes are back, and rest is the
	// rest of 777's configuration then.
	before map[string]string
	rest   string
	// writes counts the configuration writes to 777.
	writes int
}

// newStaleNote builds a staleNote. legacyNotes also files a legacy disk's
// attached-disk note and drive-option overlay under the old name, which are
// the hand-attached volume's, and noHolder leaves the old name free.
func newStaleNote(t *testing.T, legacyNotes, noHolder bool) *staleNote {
	t.Helper()
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	ctx := context.Background()
	if !noHolder {
		f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
		f.client.state.configs[777]["scsi3"] = f.volume + ",size=1G"
	}
	if legacyNotes {
		pve.UpdateAttachedDiskCID(ctx, f.client, nil, "n1", 777, f.volume, "legacy-disk-cid")
		if err := pve.SetVMDiskOptOverlay(ctx, f.client, "n1", 777, f.volume, map[string]string{"cache": "none"}); err != nil {
			t.Fatal(err)
		}
	}
	f.client.visibilityErrAfterDelete = errors.New("visibility lost")
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err == nil {
		t.Fatal("delete_disk without audit visibility after the deletion succeeded, want a refusal")
	}
	f.client.visibilityErrAfterDelete, f.client.visibilityErr = nil, nil
	if f.client.state.volumes[landed] != nil {
		t.Fatalf("the refused delete_disk left the landed volume %s", landed)
	}
	// An earlier release left the entry and the attached-disk note in place
	// when it refused.
	if err := pve.WriteDiskAllocationProvenance(ctx, f.client, "n1", 777, key, entry); err != nil {
		t.Fatal(err)
	}
	pve.UpdateAttachedDiskCID(ctx, f.client, nil, "n1", 777, key, f.cid)
	s := &staleNote{f: f, key: key}
	s.requireState(t, aj.ReconciliationRequired)
	cfg := f.client.state.configs[777]
	s.before, s.rest = descriptionNotes(t, cfg), configWithoutNotes(cfg)
	for _, note := range []string{"bosh_disk_allocations/" + key, "bosh_attached_disks/" + key} {
		if _, found := s.before[note]; !found {
			t.Fatalf("777 carries %v, want the stale %s", s.before, note)
		}
	}
	f.client.afterConfigWrite = func(vmid int) {
		if vmid == 777 {
			s.writes++
		}
	}
	return s
}

func (s *staleNote) deleteDisk(t *testing.T) error {
	t.Helper()
	_, err := HandleDeleteDisk(s.f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, s.f.cid)}, jsonrpc.Context{})
	return err
}

func (s *staleNote) requireState(t *testing.T, want aj.State) {
	t.Helper()
	record, err := s.f.journal.Inspect(s.f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != want {
		t.Fatalf("record state = %s (%s), want %s", record.State, record.Reason, want)
	}
}

// requireDeleted checks that the record ended Deleted, that 777 lost only the
// disk's allocation entry and its attached-disk note, and that the
// hand-attached volume, its slot, and every other note stayed as they were.
func (s *staleNote) requireDeleted(t *testing.T) {
	t.Helper()
	s.requireState(t, aj.Deleted)
	cfg := s.f.client.state.configs[777]
	notes := descriptionNotes(t, cfg)
	gone := map[string]bool{"bosh_disk_allocations/" + s.key: true, "bosh_attached_disks/" + s.key: true}
	for note, was := range s.before {
		now, found := notes[note]
		switch {
		case gone[note] && found:
			t.Fatalf("777 still carries %s", note)
		case !gone[note] && now != was:
			t.Fatalf("777's %s = %q, want %s left as it was", note, now, was)
		}
	}
	if len(notes) != len(s.before)-len(gone) {
		t.Fatalf("777's notes went from %v to %v, want only %v gone", s.before, notes, gone)
	}
	if rest := configWithoutNotes(cfg); rest != s.rest {
		t.Fatalf("777's config changed beyond its notes:\nbefore %s\nafter  %s", s.rest, rest)
	}
	if s.f.client.state.volumes[s.f.volume] == nil {
		t.Fatalf("delete_disk destroyed the hand-attached volume %s", s.f.volume)
	}
}

// TestDeleteDiskRemovesStaleNoteOnRetry is the disk's own stale entry and
// attached-disk note under its old name. The retry removes both in one write
// and ends Deleted.
func TestDeleteDiskRemovesStaleNoteOnRetry(t *testing.T) {
	s := newStaleNote(t, false, false)
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with the disk's stale entry on 777: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// TestDeleteDiskStaleNoteKeepsOtherDisksNotes adds a legacy disk's notes under
// the old name, which belong to the volume 777 holds there. The retry removes
// only the renamed disk's own notes, and the legacy disk's attached-disk note
// and overlay come through byte for byte.
func TestDeleteDiskStaleNoteKeepsOtherDisksNotes(t *testing.T) {
	s := newStaleNote(t, true, false)
	for _, note := range []string{"bosh_attached_disks/" + s.f.volume, pve.DiskOptOverlaysSentinelKey + "/" + s.f.volume} {
		if _, found := s.before[note]; !found {
			t.Fatalf("777 carries %v, want the legacy disk's %s", s.before, note)
		}
	}
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with the legacy disk's notes on the old name: %v", err)
	}
	s.requireDeleted(t)
}

// TestDeleteDiskStaleNoteWriteOutcomeUnknown is a removal whose answer never
// comes back. In the first row PVE never got the write, and in the second the
// write landed and its readback failed. Either way the call fails retriable
// and leaves the record in reconciliation_required, and a rerun ends Deleted
// with the notes written off exactly once.
func TestDeleteDiskStaleNoteWriteOutcomeUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{name: "write not applied"},
		{name: "write applied and its readback lost", landed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStaleNote(t, false, false)
			client := s.f.client
			lost := errors.New("connection reset by peer")
			if tc.landed {
				client.afterConfigWrite = func(vmid int) {
					if vmid != 777 {
						return
					}
					s.writes++
					client.onConfigRead = func(read int) error {
						if read == 777 {
							return lost
						}
						return nil
					}
				}
			} else {
				client.descriptionErr = lost
			}
			err := s.deleteDisk(t)
			requireRetriable(t, err, "delete_disk whose stale-note removal lost its answer")
			client.descriptionErr, client.onConfigRead = nil, nil
			s.requireState(t, aj.ReconciliationRequired)
			if carries := strings.Contains(descriptionNotesText(t, s.f), s.key); carries == tc.landed {
				t.Fatalf("after the lost answer 777 carries the stale notes = %t, want %t", carries, !tc.landed)
			}
			if err := s.deleteDisk(t); err != nil {
				t.Fatalf("rerun of delete_disk: %v", err)
			}
			s.requireDeleted(t)
			if s.writes != 1 {
				t.Fatalf("777 was written %d times, want once", s.writes)
			}
		})
	}
}

// TestDeleteDiskStaleNoteCrashBeforeAudit is a call that stops after the
// removal landed and before the completion audit could finish, which an audit
// that loses its visibility stands in for. A rerun finds the notes gone,
// writes nothing, and ends Deleted.
func TestDeleteDiskStaleNoteCrashBeforeAudit(t *testing.T) {
	s := newStaleNote(t, false, false)
	client := s.f.client
	client.afterConfigWrite = func(vmid int) {
		if vmid == 777 {
			s.writes++
			client.visibilityErr = errors.New("visibility lost")
		}
	}
	if err := s.deleteDisk(t); err == nil {
		t.Fatal("delete_disk whose audit lost its visibility succeeded, want a refusal")
	}
	client.visibilityErr = nil
	s.requireState(t, aj.ReconciliationRequired)
	if strings.Contains(descriptionNotesText(t, s.f), s.key) {
		t.Fatal("the removal didn't land before the audit")
	}
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("rerun of delete_disk after the audit stopped: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 1 {
		t.Fatalf("777 was written %d times, want once", s.writes)
	}
}

// TestDeleteDiskStaleNoteDigestMoved is another writer that files a note on
// 777 after the retry reads it and before the removal reaches PVE. PVE refuses
// the removal's digest, so the call fails retriable and leaves the record in
// reconciliation_required, and a rerun reads 777 again, ends Deleted, and
// keeps the other writer's note.
func TestDeleteDiskStaleNoteDigestMoved(t *testing.T) {
	s := newStaleNote(t, false, false)
	client := s.f.client
	const other = "bosh_attached_disks/other-key"
	moved := false
	client.beforeConfigWrite = func(vmid int) {
		if vmid != 777 || moved {
			return
		}
		moved = true
		pve.UpdateAttachedDiskCID(context.Background(), client, nil, "n1", 777, "other-key", "other-disk-cid")
		note, found := descriptionNotes(t, client.state.configs[777])[other]
		if !found {
			t.Fatalf("the other writer's note didn't land on 777")
		}
		s.before[other] = note
	}
	err := s.deleteDisk(t)
	requireRetriable(t, err, "delete_disk whose stale-note removal met a moved digest")
	if !moved {
		t.Fatal("the removal never reached PVE")
	}
	s.requireState(t, aj.ReconciliationRequired)
	if !strings.Contains(descriptionNotesText(t, s.f), s.key) {
		t.Fatal("777 lost the stale notes to a write PVE refused")
	}
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("rerun of delete_disk after the digest moved: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 2 {
		t.Fatalf("777 was written %d times, want the other writer's write and one removal", s.writes)
	}
}

// descriptionNotesText returns 777's notes as one string.
func descriptionNotesText(t *testing.T, f digestManaged) string {
	t.Helper()
	var text strings.Builder
	for note := range descriptionNotes(t, f.client.state.configs[777]) {
		text.WriteString(note + "\n")
	}
	return text.String()
}

// TestDeleteDiskStaleNoteWithoutHolderStillRefuses is the disk's own stale
// entry and attached-disk note on 777 with no other disk on the old name. The
// removal covers only an old name that another disk holds on a slot, so the
// retry writes nothing to 777, refuses at its completion audit, and leaves
// the record in reconciliation_required with both notes in place.
func TestDeleteDiskStaleNoteWithoutHolderStillRefuses(t *testing.T) {
	s := newStaleNote(t, false, true)
	if err := s.deleteDisk(t); err == nil {
		t.Fatal("retried delete_disk with the stale entry and a free old name succeeded, want a refusal")
	}
	s.requireState(t, aj.ReconciliationRequired)
	if notes := descriptionNotes(t, s.f.client.state.configs[777]); fmt.Sprint(notes) != fmt.Sprint(s.before) {
		t.Fatalf("777's notes went from %v to %v, want them left as they were", s.before, notes)
	}
	if s.writes != 0 {
		t.Fatalf("the retry wrote 777 %d times, want none", s.writes)
	}
}
