package allocationjournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBinding() EntryBinding {
	return EntryBinding{
		VMPrefix:             "az1",
		ParkerPrefix:         "az1",
		VMIDRangeStart:       100,
		VMIDRangeEnd:         8999,
		DiskVMIDRangeStart:   9000,
		DiskVMIDRangeEnd:     29999,
		ParkedVMIDRangeStart: 90000,
		ParkedVMIDRangeEnd:   90999,
	}
}

func sidecarPath(dir string) string {
	return filepath.Join(dir, pathKey("namespace"), entryBindingFile)
}

// TestEntryBindingSidecarNameMatchesNoJournalReader pins the sidecar's name
// against every name pattern a reader of the namespace directory acts on. The
// record scans in List, lookupRecords, and InspectDiskToken decode only
// "allocation-*.json"; authority.json and index.json are read by exact name;
// lock files end in ".lock"; and atomicJSON's scratch files start with
// ".tmp-". A rename that collided with any of these would let a reader decode,
// count, or remove the sidecar.
func TestEntryBindingSidecarNameMatchesNoJournalReader(t *testing.T) {
	name := entryBindingFile
	if strings.HasPrefix(name, "allocation-") && strings.HasSuffix(name, ".json") {
		t.Fatalf("%s matches the record scan filter", name)
	}
	if _, ok := strings.CutPrefix(name, "allocation-"); ok {
		t.Fatalf("%s carries the record prefix", name)
	}
	for _, reserved := range []string{authorityFile, "index.json", "index.lock", "initialize.lock"} {
		if name == reserved {
			t.Fatalf("%s collides with %s", name, reserved)
		}
	}
	if strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, "audit-") {
		t.Fatalf("%s matches a lock, scratch, or audit name", name)
	}
}

// TestEntryBindingSidecarIsInvisibleToRecordScans puts an undecodable file at
// the sidecar's name next to real records and drives every directory scan the
// package has. Each one would fail if it tried to decode the file, and the
// record counts would change if it counted it. The file must also survive
// index recovery, the one path that rewrites the namespace wholesale.
func TestEntryBindingSidecarIsInvisibleToRecordScans(t *testing.T) {
	j, dir := fixture(t)
	vm := acquireVM(t, j)
	vmID := vm.Record().ID
	closeHandle(t, vm)
	diskID, disk := createdDisk(t, j)
	closeHandle(t, disk)
	garbage := []byte("not a journal envelope")
	if err := os.WriteFile(sidecarPath(dir), garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	records, err := j.List()
	if err != nil {
		t.Fatalf("List decoded the sidecar: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("List counted %d records, want 2", len(records))
	}
	got, found, err := j.InspectVMContext(context.Background(), "agent")
	if err != nil || !found || got.ID != vmID {
		t.Fatalf("InspectVMContext = %s, %v, %v; want %s", got.ID, found, err, vmID)
	}
	byToken, found, err := j.InspectDiskToken(tokenOf(t, diskID))
	if err != nil || !found || byToken.ID != diskID {
		t.Fatalf("InspectDiskToken = %s, %v, %v; want %s", byToken.ID, found, err, diskID)
	}

	if err := os.WriteFile(filepath.Join(dir, pathKey("namespace"), "index.json"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := enrollment()
	e.ProvenanceIDs = []string{vmID, diskID}
	e.AuditID = "full-historical-index-recovery"
	if err := j.RecoverIndex(context.Background(), e); err != nil {
		t.Fatalf("RecoverIndex with a sidecar present: %v", err)
	}
	after, err := os.ReadFile(sidecarPath(dir))
	if err != nil {
		t.Fatalf("sidecar removed by index recovery: %v", err)
	}
	if string(after) != string(garbage) {
		t.Fatalf("sidecar rewritten by index recovery: %q", after)
	}
	if _, _, err := j.EntryBinding(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("EntryBinding on garbage = %v, want ErrCorrupt", err)
	}
}

// TestEntryBindingValidSidecarLeavesScansUnchanged repeats the scan check
// with a sidecar the package itself wrote, and reopens the namespace to show
// the enrollment path ignores it too.
func TestEntryBindingValidSidecarLeavesScansUnchanged(t *testing.T) {
	j, dir := fixture(t)
	vm := acquireVM(t, j)
	closeHandle(t, vm)
	if err := j.RecordEntryBinding(testBinding(), nil, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	records, err := j.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("List = %d records, %v; want 1", len(records), err)
	}
	reopened, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatalf("Open with sidecar present: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	state, found, err := reopened.EntryBinding()
	if err != nil || !found || state.Binding != testBinding() {
		t.Fatalf("reopened EntryBinding = %+v, %v, %v", state, found, err)
	}
}

func TestEntryBindingAbsentIsNotFound(t *testing.T) {
	j, _ := fixture(t)
	state, found, err := j.EntryBinding()
	if err != nil || found || state != (EntryBindingState{}) {
		t.Fatalf("EntryBinding on a fresh namespace = %+v, %v, %v", state, found, err)
	}
}

// TestRecordEntryBindingPersistsAndKeepsPrevious checks the round trip, the
// private file mode the journal requires of every file it writes, and that a
// replacement carries the binding it replaced.
func TestRecordEntryBindingPersistsAndKeepsPrevious(t *testing.T) {
	j, dir := fixture(t)
	first := testBinding()
	firstAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if err := j.RecordEntryBinding(first, nil, firstAt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode %v, want 0600", info.Mode().Perm())
	}
	state, found, err := j.EntryBinding()
	if err != nil || !found {
		t.Fatalf("EntryBinding = %v, %v", found, err)
	}
	if state.Binding != first || !state.RecordedAt.Equal(firstAt) || state.RecordedAt.Location() != time.UTC {
		t.Fatalf("round trip = %+v", state)
	}
	if state.Previous != nil || state.PreviousRecordedAt != nil || state.Namespace != "namespace" {
		t.Fatalf("first state = %+v", state)
	}

	second := first
	second.VMPrefix = "az2"
	second.ParkerPrefix = "az2"
	secondAt := firstAt.Add(time.Hour)
	if err := j.RecordEntryBinding(second, &state, secondAt); err != nil {
		t.Fatal(err)
	}
	next, found, err := j.EntryBinding()
	if err != nil || !found {
		t.Fatalf("EntryBinding after replace = %v, %v", found, err)
	}
	if next.Binding != second || !next.RecordedAt.Equal(secondAt) {
		t.Fatalf("replaced binding = %+v", next)
	}
	if next.Previous == nil || *next.Previous != first || next.PreviousRecordedAt == nil || !next.PreviousRecordedAt.Equal(firstAt) {
		t.Fatalf("previous = %+v at %v", next.Previous, next.PreviousRecordedAt)
	}
}

func TestRecordEntryBindingRejectsUnusableInput(t *testing.T) {
	j, dir := fixture(t)
	now := time.Unix(1_700_000_000, 0)
	noParker := testBinding()
	noParker.ParkerPrefix = " "
	inverted := testBinding()
	inverted.ParkedVMIDRangeStart, inverted.ParkedVMIDRangeEnd = 90999, 90000
	negative := testBinding()
	negative.VMIDRangeStart = -1
	control := testBinding()
	control.VMPrefix = "az1\nforged"
	for name, b := range map[string]EntryBinding{"no parker prefix": noParker, "inverted band": inverted, "negative": negative, "control": control} {
		if err := j.RecordEntryBinding(b, nil, now); err == nil {
			t.Errorf("%s: RecordEntryBinding accepted %+v", name, b)
		}
	}
	if err := j.RecordEntryBinding(testBinding(), nil, time.Time{}); err == nil {
		t.Error("RecordEntryBinding accepted a zero time")
	}
	if _, err := os.Stat(sidecarPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected input left a sidecar: %v", err)
	}
}

// TestEntryBindingRejectsForeignContent covers the content a strict reader has
// to refuse: a sidecar naming another namespace, a sidecar from a later schema,
// and a sidecar whose binding is unusable.
func TestEntryBindingRejectsForeignContent(t *testing.T) {
	cases := map[string]EntryBindingState{
		"misplaced":      {Version: entryBindingVersion, Namespace: "elsewhere", Binding: testBinding(), RecordedAt: time.Unix(1, 0).UTC()},
		"future version": {Version: entryBindingVersion + 1, Namespace: "namespace", Binding: testBinding(), RecordedAt: time.Unix(1, 0).UTC()},
		"unusable":       {Version: entryBindingVersion, Namespace: "namespace", Binding: EntryBinding{}, RecordedAt: time.Unix(1, 0).UTC()},
		"bad previous":   {Version: entryBindingVersion, Namespace: "namespace", Binding: testBinding(), Previous: &EntryBinding{}, RecordedAt: time.Unix(1, 0).UTC()},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			j, _ := fixture(t)
			if err := atomicJSON(j.root, entryBindingFile, state, j.ops); err != nil {
				t.Fatal(err)
			}
			if _, found, err := j.EntryBinding(); !errors.Is(err, ErrCorrupt) || found {
				t.Fatalf("EntryBinding = %v, %v; want ErrCorrupt", found, err)
			}
		})
	}
}

// TestRecordEntryBindingFailedWriteKeepsOldSidecar checks that a failed
// rename reports a DurabilityError and leaves the previous sidecar readable,
// which is the atomic-replace property the journal relies on everywhere.
func TestRecordEntryBindingFailedWriteKeepsOldSidecar(t *testing.T) {
	j, dir := fixture(t)
	if err := j.RecordEntryBinding(testBinding(), nil, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	j.ops.rename = func(*os.Root, string, string) error { return errors.New("simulated crash before rename") }
	changed := testBinding()
	changed.ParkerPrefix = "az2"
	var durability *DurabilityError
	if err := j.RecordEntryBinding(changed, nil, time.Unix(1_700_000_100, 0)); !errors.As(err, &durability) {
		t.Fatalf("failed write = %v, want DurabilityError", err)
	}
	state, found, err := j.EntryBinding()
	if err != nil || !found || state.Binding != testBinding() {
		t.Fatalf("old sidecar after failed write = %+v, %v, %v", state, found, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, pathKey("namespace")))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("failed write left scratch file %s", e.Name())
		}
	}
}
