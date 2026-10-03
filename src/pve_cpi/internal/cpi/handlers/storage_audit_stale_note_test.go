package handlers

// An earlier release may have refused delete_disk for a renamed disk at its
// completion audit. That disk can still carry its allocation entry on the VM
// it was moved off, under its old name, after its volume is gone. A retry
// reaches delete_disk's absence path. That path removes the entry and the
// attached-disk notes that map to the disk's own CID in one write, pinned to
// the digest of its read, before the completion audit runs. Most rows run
// that retry with a hand-attached volume on the old name. The last row leaves
// the old name free, and the retry heals it the same way.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// TestDeleteDiskStaleNoteWithoutHolderHealsOnRetry covers the disk's own stale
// entry and attached-disk note on 777 with no other disk on the old name,
// which is the plain shape of a delete refused after its volume is gone. The
// retry removes both notes in one write and ends Deleted, and the rest of
// 777's configuration stays as it was.
func TestDeleteDiskStaleNoteWithoutHolderHealsOnRetry(t *testing.T) {
	s := newStaleNote(t, false, true)
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with the stale entry and a free old name: %v", err)
	}
	s.requireState(t, aj.Deleted)
	cfg := s.f.client.state.configs[777]
	notes := descriptionNotes(t, cfg)
	for _, note := range []string{"bosh_disk_allocations/" + s.key, "bosh_attached_disks/" + s.key} {
		if _, found := notes[note]; found {
			t.Fatalf("777 still carries %s", note)
		}
	}
	if len(notes) != len(s.before)-2 {
		t.Fatalf("777's notes went from %v to %v, want only the disk's two gone", s.before, notes)
	}
	if rest := configWithoutNotes(cfg); rest != s.rest {
		t.Fatalf("777's config changed beyond its notes:\nbefore %s\nafter  %s", s.rest, rest)
	}
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// TestDeleteDiskStaleNoteRetryCreatedDiskHeals covers a disk that create_disk
// made on a retry. PVE rejected the first create before it ran, so the
// record's first attempt closed with proof that it left nothing behind, and
// its create step stays planned for good. That step can't put the disk
// anywhere and can never settle, so the retry removes the stale notes from
// 777 in one write and ends Deleted.
func TestDeleteDiskStaleNoteRetryCreatedDiskHeals(t *testing.T) {
	s := newStaleNote(t, false, false)
	record, err := s.f.journal.Inspect(s.f.id)
	if err != nil {
		t.Fatal(err)
	}
	record, _ = withRejectedFirstAttempt(t, s.f.id, record)
	rewriteJournalRecord(t, s.f.deps, s.f.id, record)
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk of a disk created on a retry: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// withRejectedFirstAttempt returns record as a create_disk retry leaves it
// after PVE rejected the first create before it ran. The first attempt closed
// with proof that it left nothing behind, and it holds the create step, which
// stays planned for good. Every step the record already had moves to the
// second attempt. The function also returns the create step that it adds, so a
// caller can tell that step from the others.
func withRejectedFirstAttempt(t *testing.T, id string, record aj.Record) (aj.Record, aj.Step) {
	t.Helper()
	if len(record.Attempts) > 1 || len(record.Steps) == 0 {
		t.Fatalf("record %s has %d attempts and %d steps, want one attempt with steps", id, len(record.Attempts), len(record.Steps))
	}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(map[string]any{"operation": "validated pre-execution rejection", "allocation_id": id, "absence": true})
	if err != nil {
		t.Fatal(err)
	}
	proof := aj.AttemptVerification{Verification: aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}, OutcomesKnown: true, NoSubmissionVerified: true}
	plan := record.ActivePlan()
	rejected := aj.Step{ID: "rejected-create", Kind: record.Steps[0].Kind, Target: record.Steps[0].Target, State: aj.Planned}
	steps := slices.Clone(record.Steps)
	for i := range steps {
		steps[i].Attempt = 1
	}
	record.Steps = append([]aj.Step{rejected}, steps...)
	record.Attempts = []aj.Attempt{{Number: 0, Plan: plan, Completion: &proof}, {Number: 1, Plan: plan, Admission: &proof}}
	record.Verifications = append(slices.Clone(record.Verifications), proof.Verification)
	return record, rejected
}

// rewriteJournalRecord writes record over id's record file without going
// through the journal's save gate, so the record reads back the way one that
// an earlier release left behind does.
func rewriteJournalRecord(t *testing.T, deps Deps, id string, record aj.Record) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(deps.Config.StorageAllocationJournalDir, "*", "allocation-"+id+".json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one record file for %s, got %v (%v)", id, matches, err)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	encoded, err := json.Marshal(map[string]any{"version": aj.Version, "sha256": hex.EncodeToString(sum[:]), "payload": json.RawMessage(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matches[0], encoded, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
}
