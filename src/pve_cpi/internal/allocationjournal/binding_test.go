package allocationjournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func otherBinding(prefix string) EntryBinding {
	b := testBinding()
	b.VMPrefix, b.ParkerPrefix = prefix, prefix
	return b
}

func sidecarPath(dir string) string {
	return filepath.Join(dir, pathKey("namespace"), entryBindingFile)
}

func readSidecar(t *testing.T, j *Journal) EntryBindingState {
	t.Helper()
	state, found, err := j.EntryBinding()
	if err != nil || !found {
		t.Fatalf("EntryBinding = %v, %v", found, err)
	}
	return state
}

func bindingsOf(state EntryBindingState) []EntryBinding {
	out := make([]EntryBinding, 0, len(state.Bindings))
	for _, seen := range state.Bindings {
		out = append(out, seen.Binding)
	}
	return out
}

// TestEntryBindingFileNamesMatchNoJournalReader pins the sidecar's name and
// its lock's name against every name pattern a reader of the namespace
// directory acts on. The record scans in List, lookupRecords, and
// InspectDiskToken decode only "allocation-*.json"; authority.json and
// index.json are read by exact name; atomicJSON's scratch files start with
// ".tmp-". A rename that collided with any of these would let a reader
// decode, count, or remove the file.
func TestEntryBindingFileNamesMatchNoJournalReader(t *testing.T) {
	for _, name := range []string{entryBindingFile, entryBindingLockFile} {
		if strings.HasPrefix(name, "allocation-") {
			t.Fatalf("%s carries the record prefix", name)
		}
		for _, reserved := range []string{authorityFile, "index.json", "index.lock", "initialize.lock"} {
			if name == reserved {
				t.Fatalf("%s collides with %s", name, reserved)
			}
		}
		if strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, "audit-") {
			t.Fatalf("%s matches a scratch or audit name", name)
		}
	}
	if strings.HasSuffix(entryBindingFile, ".lock") {
		t.Fatalf("%s reads as a lock file", entryBindingFile)
	}
	if !strings.HasSuffix(entryBindingLockFile, ".lock") {
		t.Fatalf("%s does not follow the journal's lock naming", entryBindingLockFile)
	}
}

// TestEntryBindingSidecarIsInvisibleToRecordScans puts an undecodable file at
// the sidecar's name, and a lock file at the lock's name, next to real
// records and drives every directory scan the package has. Each one would
// fail if it tried to decode the sidecar, and the record counts would change
// if it counted it. Both files must also survive index recovery, the one path
// that rewrites the namespace wholesale.
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
	lockPath := filepath.Join(dir, pathKey("namespace"), entryBindingLockFile)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
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
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file removed by index recovery: %v", err)
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
	if _, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0)); err != nil {
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
	state := readSidecar(t, reopened)
	if fmt.Sprint(bindingsOf(state)) != fmt.Sprint([]EntryBinding{testBinding()}) {
		t.Fatalf("reopened EntryBinding = %+v", state)
	}
}

func TestEntryBindingAbsentIsNotFound(t *testing.T) {
	j, _ := fixture(t)
	state, found, err := j.EntryBinding()
	if err != nil || found || len(state.Bindings) != 0 || state.Namespace != "" {
		t.Fatalf("EntryBinding on a fresh namespace = %+v, %v, %v", state, found, err)
	}
}

// TestNoteEntryBindingKeepsEveryBinding checks the set: the first binding is
// added with no others, a repeat writes nothing, and a second binding is added
// beside the first rather than replacing it. The file is private, versioned
// with the sidecar's own schema, and stamps first-seen times in UTC.
func TestNoteEntryBindingKeepsEveryBinding(t *testing.T) {
	j, dir := fixture(t)
	ctx := context.Background()
	firstAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	note, err := j.NoteEntryBinding(ctx, testBinding(), firstAt)
	if err != nil || !note.Added || len(note.Others) != 0 || note.Replaced != nil {
		t.Fatalf("first note = %+v, %v", note, err)
	}
	info, err := os.Stat(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var env diskEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != entryBindingVersion {
		t.Fatalf("sidecar envelope version = %d, %v; want the sidecar schema %d", env.Version, err, entryBindingVersion)
	}

	again, err := j.NoteEntryBinding(ctx, testBinding(), firstAt.Add(time.Hour))
	if err != nil || again.Added {
		t.Fatalf("repeat note = %+v, %v", again, err)
	}
	unchanged, err := os.Stat(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, unchanged) || !unchanged.ModTime().Equal(info.ModTime()) {
		t.Fatal("a known binding rewrote the sidecar")
	}

	secondAt := firstAt.Add(2 * time.Hour)
	second, err := j.NoteEntryBinding(ctx, otherBinding("az2"), secondAt)
	if err != nil || !second.Added || len(second.Others) != 1 || second.Others[0].Binding != testBinding() {
		t.Fatalf("second note = %+v, %v", second, err)
	}
	state := readSidecar(t, j)
	if state.Namespace != "namespace" || len(state.Bindings) != 2 {
		t.Fatalf("state = %+v", state)
	}
	if state.Bindings[0].Binding != testBinding() || !state.Bindings[0].FirstSeen.Equal(firstAt) ||
		state.Bindings[0].FirstSeen.Location() != time.UTC {
		t.Fatalf("first sighting = %+v", state.Bindings[0])
	}
	if state.Bindings[1].Binding != otherBinding("az2") || !state.Bindings[1].FirstSeen.Equal(secondAt) {
		t.Fatalf("second sighting = %+v", state.Bindings[1])
	}
	if back, err := j.NoteEntryBinding(ctx, testBinding(), secondAt.Add(time.Hour)); err != nil || back.Added {
		t.Fatalf("the first binding returning = %+v, %v; it is already in the set", back, err)
	}
}

// TestNoteEntryBindingDropsTheOldestPastTheBound fills the set past its bound
// and checks that the oldest binding goes and is reported.
func TestNoteEntryBindingDropsTheOldestPastTheBound(t *testing.T) {
	j, _ := fixture(t)
	ctx := context.Background()
	start := time.Unix(1_700_000_000, 0)
	for i := range maxEntryBindings {
		if _, err := j.NoteEntryBinding(ctx, otherBinding(fmt.Sprintf("p%d", i)), start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	note, err := j.NoteEntryBinding(ctx, otherBinding("overflow"), start.Add(time.Hour))
	if err != nil || !note.Added || len(note.Dropped) != 1 || note.Dropped[0].Binding != otherBinding("p0") {
		t.Fatalf("overflow note = %+v, %v", note, err)
	}
	state := readSidecar(t, j)
	if len(state.Bindings) != maxEntryBindings || state.Bindings[0].Binding != otherBinding("p1") ||
		state.Bindings[maxEntryBindings-1].Binding != otherBinding("overflow") {
		t.Fatalf("bounded set = %v", bindingsOf(state))
	}
}

// TestNoteEntryBindingConcurrentWritersKeepEachOther runs several journal
// handles, as separate CPI processes would have, each adding its own binding
// at once. The lock and the second read under it keep every binding.
func TestNoteEntryBindingConcurrentWritersKeepEachOther(t *testing.T) {
	j, dir := fixture(t)
	const writers = 8
	handles := make([]*Journal, writers)
	for i := range handles {
		h, err := Open(dir, "namespace", "cluster-identity")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		// The eight writes serialize on the lock, each with a file and a
		// directory fsync. A loaded shared runner can stretch that past the
		// production one-second budget, which would turn a correct lock into
		// a flaky ErrEntryBindingBusy, so these handles wait longer.
		h.binding.lockWait = testEntryBindingLockWait
		handles[i] = h
	}
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i, h := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.NoteEntryBinding(context.Background(), otherBinding(fmt.Sprintf("w%d", i)), time.Unix(1_700_000_000, 0))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state := readSidecar(t, j)
	if len(state.Bindings) != writers {
		t.Fatalf("concurrent writers left %d of %d bindings: %v", len(state.Bindings), writers, bindingsOf(state))
	}
	for i := range writers {
		if state.Index(otherBinding(fmt.Sprintf("w%d", i))) < 0 {
			t.Fatalf("binding w%d was dropped: %v", i, bindingsOf(state))
		}
	}
}

// TestNoteEntryBindingRereadsUnderTheLock holds the lock as another process
// would, lets a note read the old set and queue on the lock, adds a binding
// while it waits, and checks that the note's write keeps that binding. A note
// that wrote from its first read would erase it.
func TestNoteEntryBindingRereadsUnderTheLock(t *testing.T) {
	j, dir := fixture(t)
	ctx := context.Background()
	if _, err := j.NoteEntryBinding(ctx, testBinding(), time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	other, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	held, ok, err := tryLock(other.root, entryBindingLockFile)
	if err != nil || !ok {
		t.Fatalf("tryLock = %v, %v", ok, err)
	}
	// The note must outwait the 100 ms hold and the fsynced write under it,
	// however loaded the runner is.
	j.binding.lockWait = testEntryBindingLockWait
	done := make(chan error, 1)
	go func() {
		_, err := j.NoteEntryBinding(ctx, otherBinding("az2"), time.Unix(1_700_000_100, 0))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("note finished while another process held the lock: %v", err)
	default:
	}
	if _, err := other.addEntryBindingLocked(otherBinding("az3"), time.Unix(1_700_000_050, 0)); err != nil {
		t.Fatal(err)
	}
	if err := held.close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := bindingsOf(readSidecar(t, j))
	want := []EntryBinding{testBinding(), otherBinding("az3"), otherBinding("az2")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bindings = %v, want %v", got, want)
	}
}

// TestNoteEntryBindingGivesUpOnABusyLock checks that a lock held past the
// bound returns ErrEntryBindingBusy and changes nothing, so a stalled peer
// can delay a create by at most the bound.
func TestNoteEntryBindingGivesUpOnABusyLock(t *testing.T) {
	j, dir := fixture(t)
	held, ok, err := tryLock(j.root, entryBindingLockFile)
	if err != nil || !ok {
		t.Fatalf("tryLock = %v, %v", ok, err)
	}
	defer func() {
		if err := held.close(); err != nil {
			t.Error(err)
		}
	}()
	other, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.binding.lockWait = 100 * time.Millisecond
	begin := time.Now()
	if _, err := other.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0)); !errors.Is(err, ErrEntryBindingBusy) {
		t.Fatalf("busy lock = %v, want ErrEntryBindingBusy", err)
	}
	// The bound only proves the wait ends; a loaded runner gets generous
	// slack over the 100 ms budget.
	if waited := time.Since(begin); waited > 10*time.Second {
		t.Fatalf("waited %v on a busy lock", waited)
	}
	if _, err := os.Stat(sidecarPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a busy lock still wrote the sidecar: %v", err)
	}
}

func TestNoteEntryBindingRejectsUnusableInput(t *testing.T) {
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
		if _, err := j.NoteEntryBinding(context.Background(), b, now); err == nil {
			t.Errorf("%s: NoteEntryBinding accepted %+v", name, b)
		}
	}
	if _, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Time{}); err == nil {
		t.Error("NoteEntryBinding accepted a zero time")
	}
	if _, err := os.Stat(sidecarPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected input left a sidecar: %v", err)
	}
}

// TestEntryBindingRejectsForeignContent covers the content a strict reader has
// to refuse as corrupt.
func TestEntryBindingRejectsForeignContent(t *testing.T) {
	seen := func(b EntryBinding) EntryBindingSighting {
		return EntryBindingSighting{Binding: b, FirstSeen: time.Unix(1, 0).UTC()}
	}
	tooMany := make([]EntryBindingSighting, 0, maxEntryBindings+1)
	for i := range maxEntryBindings + 1 {
		tooMany = append(tooMany, seen(otherBinding(fmt.Sprintf("p%d", i))))
	}
	cases := map[string]struct {
		version int
		state   EntryBindingState
	}{
		"misplaced":       {entryBindingVersion, EntryBindingState{Namespace: "elsewhere", Bindings: []EntryBindingSighting{seen(testBinding())}}},
		"empty set":       {entryBindingVersion, EntryBindingState{Namespace: "namespace"}},
		"unusable":        {entryBindingVersion, EntryBindingState{Namespace: "namespace", Bindings: []EntryBindingSighting{seen(EntryBinding{})}}},
		"no first seen":   {entryBindingVersion, EntryBindingState{Namespace: "namespace", Bindings: []EntryBindingSighting{{Binding: testBinding()}}}},
		"repeat":          {entryBindingVersion, EntryBindingState{Namespace: "namespace", Bindings: []EntryBindingSighting{seen(testBinding()), seen(testBinding())}}},
		"over the bound":  {entryBindingVersion, EntryBindingState{Namespace: "namespace", Bindings: tooMany}},
		"version zero":    {0, EntryBindingState{Namespace: "namespace", Bindings: []EntryBindingSighting{seen(testBinding())}}},
		"negative schema": {-1, EntryBindingState{Namespace: "namespace", Bindings: []EntryBindingSighting{seen(testBinding())}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j, _ := fixture(t)
			if err := atomicEnvelope(j.root, entryBindingFile, tc.version, tc.state, j.ops); err != nil {
				t.Fatal(err)
			}
			if _, found, err := j.EntryBinding(); !errors.Is(err, ErrCorrupt) || found {
				t.Fatalf("EntryBinding = %v, %v; want ErrCorrupt", found, err)
			}
		})
	}
}

// TestNoteEntryBindingLeavesANewerSidecarAlone writes a sidecar with a newer
// schema, whose payload this release can't even decode, and checks that the
// read reports it as unsupported rather than corrupt and that a note leaves
// the file byte for byte as it was.
func TestNoteEntryBindingLeavesANewerSidecarAlone(t *testing.T) {
	j, dir := fixture(t)
	future := map[string]any{"namespace": "namespace", "sets": []string{"a", "b"}}
	if err := atomicEnvelope(j.root, entryBindingFile, entryBindingVersion+1, future, j.ops); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := j.EntryBinding(); !errors.Is(err, ErrUnsupportedEntryBinding) || errors.Is(err, ErrCorrupt) || found {
		t.Fatalf("EntryBinding on a newer sidecar = %v, %v", found, err)
	}
	note, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0))
	if !errors.Is(err, ErrUnsupportedEntryBinding) || note.Added {
		t.Fatalf("NoteEntryBinding on a newer sidecar = %+v, %v", note, err)
	}
	after, err := os.ReadFile(sidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a newer sidecar was overwritten")
	}
}

// TestNoteEntryBindingReplacesACorruptSidecar checks that garbage is replaced
// by a fresh set holding the current binding, and that the reason is
// reported.
func TestNoteEntryBindingReplacesACorruptSidecar(t *testing.T) {
	j, dir := fixture(t)
	if err := os.WriteFile(sidecarPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	note, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0))
	if err != nil || !note.Added || len(note.Others) != 0 || !errors.Is(note.Replaced, ErrCorrupt) {
		t.Fatalf("note over garbage = %+v, %v", note, err)
	}
	if got := bindingsOf(readSidecar(t, j)); fmt.Sprint(got) != fmt.Sprint([]EntryBinding{testBinding()}) {
		t.Fatalf("bindings = %v", got)
	}
}

// TestNoteEntryBindingFailedWriteKeepsOldSidecar checks that a failed rename
// reports a DurabilityError and leaves the previous set readable, which is the
// atomic-replace property the journal relies on everywhere.
func TestNoteEntryBindingFailedWriteKeepsOldSidecar(t *testing.T) {
	j, dir := fixture(t)
	if _, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	j.ops.rename = func(*os.Root, string, string) error { return errors.New("simulated crash before rename") }
	var durability *DurabilityError
	note, err := j.NoteEntryBinding(context.Background(), otherBinding("az2"), time.Unix(1_700_000_100, 0))
	if !errors.As(err, &durability) || !note.Added {
		t.Fatalf("failed write = %+v, %v; want an added note and a DurabilityError", note, err)
	}
	if got := bindingsOf(readSidecar(t, j)); fmt.Sprint(got) != fmt.Sprint([]EntryBinding{testBinding()}) {
		t.Fatalf("old sidecar after failed write = %v", got)
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

// testEntryBindingLockWait is the lock budget the lock tests give themselves
// so that runner load can't masquerade as a busy lock.
const testEntryBindingLockWait = 30 * time.Second

// TestEntryBindingLockWaitDefaultsToOneSecond pins the production budget, so
// the test-only override can't drift into the shipped behavior.
func TestEntryBindingLockWaitDefaultsToOneSecond(t *testing.T) {
	if got := (entryBindingTuning{}).wait(); got != time.Second {
		t.Fatalf("default entry binding lock wait = %v, want 1s", got)
	}
	if got := (entryBindingTuning{lockWait: time.Minute}).wait(); got != time.Minute {
		t.Fatalf("override = %v", got)
	}
}

// TestNoteEntryBindingReportsAFailedReleaseApart covers a release that fails
// after a good write. The error is ErrEntryBindingUnlock and not a
// DurabilityError, the note still says the binding was added, the sidecar
// holds it, and the lock is free afterwards.
func TestNoteEntryBindingReportsAFailedReleaseApart(t *testing.T) {
	j, _ := fixture(t)
	injected := errors.New("injected release failure")
	j.binding.release = func(l *fileLock) error { return errors.Join(injected, l.close()) }
	note, err := j.NoteEntryBinding(context.Background(), testBinding(), time.Unix(1_700_000_000, 0))
	if !errors.Is(err, ErrEntryBindingUnlock) || !errors.Is(err, injected) {
		t.Fatalf("release failure = %v, want ErrEntryBindingUnlock wrapping the cause", err)
	}
	var durability *DurabilityError
	if errors.As(err, &durability) {
		t.Fatalf("a release failure after a good write reads as a lost write: %v", err)
	}
	if !note.Added {
		t.Fatalf("note = %+v, want Added", note)
	}
	if got := bindingsOf(readSidecar(t, j)); fmt.Sprint(got) != fmt.Sprint([]EntryBinding{testBinding()}) {
		t.Fatalf("bindings = %v", got)
	}
	held, ok, lockErr := tryLock(j.root, entryBindingLockFile)
	if lockErr != nil || !ok {
		t.Fatalf("lock after a failed release = %v, %v", ok, lockErr)
	}
	if err := held.close(); err != nil {
		t.Fatal(err)
	}
}

// TestNoteEntryBindingReportsAnAbandonedWait covers a caller whose context
// ends while another process holds the lock. The error says the wait was
// abandoned and wraps the context's error, it is not ErrEntryBindingBusy, and
// nothing is written.
func TestNoteEntryBindingReportsAnAbandonedWait(t *testing.T) {
	j, dir := fixture(t)
	held, ok, err := tryLock(j.root, entryBindingLockFile)
	if err != nil || !ok {
		t.Fatalf("tryLock = %v, %v", ok, err)
	}
	defer func() {
		if err := held.close(); err != nil {
			t.Error(err)
		}
	}()
	other, err := Open(dir, "namespace", "cluster-identity")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	other.binding.lockWait = testEntryBindingLockWait
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = other.NoteEntryBinding(ctx, testBinding(), time.Unix(1_700_000_000, 0))
	if !errors.Is(err, ErrEntryBindingWaitAbandoned) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrEntryBindingBusy) {
		t.Fatalf("abandoned wait = %v, want ErrEntryBindingWaitAbandoned wrapping context.Canceled", err)
	}
	if _, err := os.Stat(sidecarPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an abandoned wait wrote the sidecar: %v", err)
	}
}
