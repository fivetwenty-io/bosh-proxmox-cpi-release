package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// journalNamespaceFile returns the path of name in the namespace directory
// that holds allocation id's record.
func journalNamespaceFile(t *testing.T, f digestManaged, name string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.deps.Config.StorageAllocationJournalDir, "*", "allocation-"+f.id+".json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one record file for %s, got %v (%v)", f.id, matches, err)
	}
	return filepath.Join(filepath.Dir(matches[0]), name)
}

// writeCorruptRecord writes a record file for allocation id that holds no
// valid record, with the private mode the journal requires of its files.
func writeCorruptRecord(t *testing.T, f digestManaged, id string) {
	t.Helper()
	if err := os.WriteFile(journalNamespaceFile(t, f, "allocation-"+id+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hasDiskAtOnce runs has_disk on cid with a wait seam that records whether
// has_disk started to wait for a lifecycle, and fails the row when has_disk
// took longer than an answer that doesn't wait should.
func hasDiskAtOnce(t *testing.T, f digestManaged, cid string) (any, error, bool) {
	t.Helper()
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) { waited = true }})
	start := time.Now()
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("has_disk took %s, want it to answer at once", took)
	}
	return present, err, waited
}

// TestHasDiskIgnoresACorruptUnrelatedRecord covers a disk whose CID carries
// no allocation, whose birth name is gone, and which no guest carries, in a
// journal that also holds a corrupt record of another allocation. No record
// names the disk's token, so the corrupt record says nothing about the disk,
// and has_disk answers false at once, as it does in a journal without one.
func TestHasDiskIgnoresACorruptUnrelatedRecord(t *testing.T) {
	f, _ := unmarkedFixture(t)
	delete(f.client.state.configs[777], "scsi1")
	other, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	writeCorruptRecord(t, f, other)
	present, err, waited := hasDiskAtOnce(t, f, unmarkedDiskCID(t, f, "bpd-00000000000000aa"))
	if err != nil || present != false {
		t.Fatalf("has_disk beside a corrupt record of another allocation = %v, %v, want false", present, err)
	}
	if waited {
		t.Error("has_disk waited for a lifecycle, want it to answer at once")
	}
}

// TestHasDiskCorruptTokenRecordIsRetriable covers a disk whose CID carries
// no allocation, whose birth name is gone, and which no guest carries, when
// the record whose ID gives the disk's token can't be read. Nothing shows
// that no lifecycle holds that record, so has_disk returns a retriable error
// rather than false.
func TestHasDiskCorruptTokenRecordIsRetriable(t *testing.T) {
	f, token := unmarkedFixture(t)
	delete(f.client.state.configs[777], "scsi1")
	writeCorruptRecord(t, f, f.id)
	present, err, waited := hasDiskAtOnce(t, f, unmarkedDiskCID(t, f, token))
	if waited {
		t.Error("has_disk waited for a lifecycle on a record it couldn't read")
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk with the disk's record corrupt = %v, %v, want a retriable error", present, err)
	}
	requireText(t, err, "has_disk with the disk's record corrupt", []string{"couldn't read the allocation journal to check whether another operation holds disk", "rerun the cloud check once the journal can be read"})
}

// TestHasDiskFalseAnswerNeedsTheLock covers a disk on its way between two
// guests, which the first look finds absent, after which the disk's journal
// lock can't be read. has_disk can't show that no lifecycle holds the disk's
// record, so it returns a retriable error instead of false.
func TestHasDiskFalseAnswerNeedsTheLock(t *testing.T) {
	f := digestManagedFixture(t)
	slot := f.client.state.configs[777]["scsi1"]
	delete(f.client.state.configs[777], "scsi1")
	defer func() { f.client.state.configs[777]["scsi1"] = slot }()
	lock := journalNamespaceFile(t, f, f.id+".lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the disk's lock file: %v", err)
	}
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(refusal error) {
		waited = true
		if refusal != nil {
			t.Errorf("has_disk's first look refused with %v, want an absent answer", refusal)
		}
		// The journal refuses a file that other users can read, so the
		// lock can't be opened once its mode is no longer private.
		if err := os.Chmod(lock, 0o644); err != nil {
			t.Error(err)
		}
	}})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !waited {
		t.Fatalf("has_disk = %v, %v without checking for a lifecycle", present, err)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk with the disk's lock unreadable after an absent first look = %v, %v, want a retriable error", present, err)
	}
	requireText(t, err, "has_disk with the disk's lock unreadable", []string{f.cid, "couldn't check whether another operation holds", "the disk's journal lock couldn't be read", "rerun the cloud check once the allocation journal can be read"})
}

// stripLandingMarks removes the disk's serial from 777's drive line and the
// disk's notes from 777's description, the way a lifecycle leaves the VM when
// it stops after a move landed the disk there and before it wrote either.
func stripLandingMarks(t *testing.T, f digestManaged) {
	t.Helper()
	line, ok := f.client.state.configs[777]["scsi1"].(string)
	if !ok || !strings.Contains(line, ",serial=") {
		t.Fatalf("777's scsi1 holds %v, want a drive line with a serial", f.client.state.configs[777]["scsi1"])
	}
	var kept []string
	for _, part := range strings.Split(line, ",") {
		if !strings.HasPrefix(part, "serial=") {
			kept = append(kept, part)
		}
	}
	f.client.state.configs[777]["scsi1"] = strings.Join(kept, ",")
	delete(f.client.state.configs[777], "description")
}

// withFallbackCreate rewrites f's record the way create_disk leaves it when
// PVE rejected the first create and the fallback made the disk under another
// name. The first attempt closed with proof that it left nothing behind, and
// its create step stays planned for good under a name the disk never had.
func withFallbackCreate(t *testing.T, f digestManaged) {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	record, _ = withRejectedFirstAttempt(t, f.id, record)
	storage, _, ok := strings.Cut(f.volume, ":")
	if !ok {
		t.Fatalf("volume %s names no storage", f.volume)
	}
	record.Steps[0].Target.IntendedVolume = storage + ":9001/vm-9001-disk-0.raw"
	record.Steps[0].VolIDs = nil
	rewriteJournalRecord(t, f.deps, f.id, record)
}

// hasDiskRecordNameRows are the shapes of a disk that a lifecycle left under
// the name its record last gave it, after the lifecycle stopped before it
// wrote the disk's serial and notes. When the record observed the move, the
// disk is under its landing name, and has_disk answers true. When the move
// isn't settled, the record holds no landing, so has_disk refuses for audit
// rather than report the disk missing. When the record's name is gone and no
// move is open, the disk is missing. The fallback rows put a create that PVE
// rejected first in the record, which names a volume the disk never had.
var hasDiskRecordNameRows = []struct {
	name    string
	setup   func(t *testing.T, f digestManaged)
	present bool
	refusal []string
}{
	{name: "observed move", present: true},
	{name: "unsettled move", setup: func(t *testing.T, f digestManaged) { unsettleAttachLanding(t, f, nil) },
		refusal: []string{missingVolumeAudit, "move step attempt-0-step-", "isn't settled", unsettledMoveRunbook}},
	{name: "landing gone", setup: func(t *testing.T, f digestManaged) {
		delete(f.client.state.volumes, f.volume)
		delete(f.client.state.configs[777], "scsi1")
	}},
	{name: "observed move after a fallback create", setup: withFallbackCreate, present: true},
	{name: "unsettled move after a fallback create", setup: func(t *testing.T, f digestManaged) {
		unsettleAttachLanding(t, f, nil)
		withFallbackCreate(t, f)
	}, refusal: []string{missingVolumeAudit, "move step attempt-0-step-", "isn't settled", unsettledMoveRunbook}},
}

// TestHasDiskProbesTheRecordsName covers a disk whose CID carries no
// allocation and whose birth name is gone, when a lifecycle stopped after a
// move renamed the disk and before it wrote the disk's serial and notes, so
// nothing but the record leads to the disk. has_disk's second look probes the
// name the record last gave the disk, as hasDiskRecordNameRows describes.
func TestHasDiskProbesTheRecordsName(t *testing.T) {
	for _, tc := range hasDiskRecordNameRows {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			record, err := f.journal.Inspect(f.id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, f)
			}
			if _, ok := f.client.state.configs[777]["scsi1"]; ok {
				stripLandingMarks(t, f)
			}
			requireHasDiskRecordName(t, f, unmarkedDiskCID(t, f, record.DiskToken), tc.present, tc.refusal)
		})
	}
}

// TestHasDiskProbesTheRecordsNameForAnAllocationCID covers the same shapes for
// a disk asked about by the CID create_disk returned, whose birth name names
// its allocation. The identity check finds that name gone and nothing that
// carries the disk, so the second look probes the name the record last gave
// the disk, as it does for a disk whose name doesn't lead to its record.
func TestHasDiskProbesTheRecordsNameForAnAllocationCID(t *testing.T) {
	for _, tc := range hasDiskRecordNameRows {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			if _, ok := f.client.state.configs[777]["scsi1"]; ok {
				stripLandingMarks(t, f)
			}
			requireHasDiskRecordName(t, f, f.cid, tc.present, tc.refusal)
		})
	}
}

// requireHasDiskRecordName runs has_disk on cid and requires that it took a
// second look under the record's hold, wrote nothing to PVE, and answered
// present, or refused for audit with refusal when refusal is set.
func requireHasDiskRecordName(t *testing.T, f digestManaged, cid string, present bool, refusal []string) {
	t.Helper()
	fakeBefore := flowFakeWrites(t, f.client)
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) { waited = true }})
	got, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if !waited {
		t.Errorf("has_disk = %v, %v without a second look under the record's hold", got, err)
	}
	if refusal != nil {
		if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) {
			t.Fatalf("has_disk with the move unsettled = %v, %v, want the permanent refusal for audit", got, err)
		}
		requireText(t, err, "has_disk with the move unsettled", refusal)
		return
	}
	if err != nil || got != present {
		t.Fatalf("has_disk = %v, %v, want %v", got, err, present)
	}
}

// TestHasDiskRereadsTheRecordUnderTheHold covers the record that names a
// disk's token changing after has_disk found it and before its second look
// probes the disk, with the disk on no guest and a volume still under the
// name the record last gave it. The second look reads the record again under
// the hold. A record that a delete made terminal answers false, because that
// name may be another disk's by now, and a record it can't read gives a
// retriable error rather than an answer from the record it read before.
func TestHasDiskRereadsTheRecordUnderTheHold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f digestManaged, record aj.Record)
	}{
		{name: "deleted", change: func(t *testing.T, f digestManaged, record aj.Record) {
			record.State = aj.Deleted
			record.Verifications = append(record.Verifications, aj.Verification{EvidenceID: "delete-audit", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
			rewriteJournalRecord(t, f.deps, f.id, record)
		}},
		{name: "unreadable", change: func(t *testing.T, f digestManaged, _ aj.Record) { writeCorruptRecord(t, f, f.id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			record, err := f.journal.Inspect(f.id)
			if err != nil {
				t.Fatal(err)
			}
			delete(f.client.state.configs[777], "scsi1")
			held := false
			ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, held: func() {
				held = true
				tc.change(t, f, record)
			}})
			present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, unmarkedDiskCID(t, f, record.DiskToken))}, jsonrpc.Context{})
			if !held {
				t.Fatalf("has_disk = %v, %v without a second look under the record's hold", present, err)
			}
			if tc.name == "deleted" {
				if err != nil || present != false {
					t.Fatalf("has_disk after the record became terminal = %v, %v, want false", present, err)
				}
				return
			}
			if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
				t.Fatalf("has_disk with the record unreadable under the hold = %v, %v, want a retriable error", present, err)
			}
			requireText(t, err, "has_disk with the record unreadable under the hold", []string{"couldn't read allocation record " + f.id, "rerun the cloud check once the allocation journal can be read"})
		})
	}
}

// TestHasDiskRecordVolumeFollowsTheMoves checks which name a record last
// gave its disk, and which unsettled move leaves that name in doubt.
func TestHasDiskRecordVolumeFollowsTheMoves(t *testing.T) {
	const (
		birth  = "b:123/vm-123-disk-0.raw"
		parked = "b:90988/vm-90988-disk-0.raw"
		landed = "b:777/vm-777-disk-1.raw"
		move   = "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk"
		attach = "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk"
	)
	const rejected = "a:9001/vm-9001-disk-0.raw"
	step := func(id, kind string, state aj.State, from string, volids ...string) aj.Step {
		return aj.Step{ID: id, Kind: kind, State: state, Target: aj.Target{IntendedVolume: from}, VolIDs: volids}
	}
	// second puts a step in the second attempt, the one create_disk's
	// fallback runs after PVE rejects the first create.
	second := func(s aj.Step) aj.Step {
		s.Attempt = 1
		return s
	}
	closed := aj.AttemptVerification{
		Verification:  aj.Verification{EvidenceID: "close", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true},
		OutcomesKnown: true,
	}
	fallback := []aj.Attempt{{Number: 0, Completion: &aj.AttemptVerification{
		Verification:         closed.Verification,
		OutcomesKnown:        true,
		NoSubmissionVerified: true,
	}}, {Number: 1}}
	for _, tc := range []struct {
		name    string
		record  aj.Record
		volume  string
		pending string
	}{
		{name: "no steps"},
		{name: "created only", record: aj.Record{Steps: []aj.Step{step("s0", "create", aj.Observed, birth, birth)}}, volume: birth},
		{name: "two observed moves", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Observed, birth, birth, parked),
			step("s2", "lifecycle_detach_disk_Nodes_UpdateQemuConfig", aj.Observed, parked, parked),
			step("s3", attach, aj.Observed, parked, parked, landed),
		}}, volume: landed},
		{name: "refused move", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Observed, birth, birth),
		}}, volume: birth},
		{name: "unsettled move", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Observed, birth, birth, parked),
			step("s2", attach, aj.Submitted, parked),
		}}, volume: parked, pending: "s2"},
		{name: "later landing clears an unsettled move", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Planned, birth),
			step("s2", move, aj.Observed, birth, birth, parked),
		}}, volume: parked},
		{name: "later refused move keeps an unsettled move", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Planned, birth),
			step("s2", move, aj.Observed, birth, birth),
		}}, volume: birth, pending: "s1"},
		{name: "unsettled move from another name", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", move, aj.Planned, landed),
		}}, volume: birth},
		{name: "closed attempt's move", record: aj.Record{
			Attempts: []aj.Attempt{{Number: 0, Completion: &closed}, {Number: 1}},
			Steps: []aj.Step{
				second(step("s0", "create", aj.Observed, birth, birth)),
				step("s1", move, aj.Submitted, birth),
			},
		}, volume: birth},
		{name: "observed move after a fallback create", record: aj.Record{
			Attempts: fallback,
			Steps: []aj.Step{
				step("s0", "create", aj.Planned, rejected),
				second(step("s1", "create", aj.Observed, birth, birth)),
				second(step("s2", move, aj.Observed, birth, birth, parked)),
			},
		}, volume: parked},
		{name: "unsettled move after a fallback create", record: aj.Record{
			Attempts: fallback,
			Steps: []aj.Step{
				step("s0", "create", aj.Planned, rejected),
				second(step("s1", "create", aj.Observed, birth, birth)),
				second(step("s2", move, aj.Submitted, birth)),
			},
		}, volume: birth, pending: "s2"},
		{name: "create of a closed attempt that isn't settled", record: aj.Record{
			Attempts: []aj.Attempt{{Number: 0, Completion: &aj.AttemptVerification{Verification: aj.Verification{EvidenceID: "close", Complete: true}, OutcomesKnown: true}}, {Number: 1}},
			Steps: []aj.Step{
				step("s0", "create", aj.Planned, rejected),
				second(step("s1", "create", aj.Observed, birth, birth)),
			},
		}, volume: rejected},
		{name: "observed migrate", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", "lifecycle_update_disk_Nodes_CreateQemuMigrate", aj.Observed, birth, birth, landed),
		}}, volume: landed},
		{name: "unsettled migrate", record: aj.Record{Steps: []aj.Step{
			step("s0", "create", aj.Observed, birth, birth),
			step("s1", "lifecycle_update_disk_Nodes_CreateQemuMigrate", aj.Submitted, birth),
		}}, volume: birth},
		{name: "external step", record: aj.Record{Steps: []aj.Step{
			{ID: "s0", Kind: "preserve", State: aj.Observed, Target: aj.Target{External: true, IntendedVolume: landed}, VolIDs: []string{landed}},
			step("s1", "create", aj.Observed, birth, birth),
		}}, volume: birth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume, pending := hasDiskRecordVolume(tc.record)
			got := ""
			if pending != nil {
				got = pending.ID
			}
			if volume != tc.volume || got != tc.pending {
				t.Errorf("hasDiskRecordVolume = %q, pending %q, want %q, pending %q", volume, got, tc.volume, tc.pending)
			}
		})
	}
}
