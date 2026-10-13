package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// entryBindingSidecar returns the sidecar path for the "director" namespace
// auditFixture enrolls.
func entryBindingSidecar(deps Deps) string {
	sum := sha256.Sum256([]byte(deps.Config.StoragePlacementNamespace))
	return filepath.Join(deps.Config.StorageAllocationJournalDir, hex.EncodeToString(sum[:]), "entry-binding.json")
}

func observedDeps(deps Deps) (Deps, *log.Observer) {
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps.Logger = logger
	return deps, obs
}

func bindingWarnings(obs *log.Observer) []log.Entry {
	var out []log.Entry
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "entry binding") {
			out = append(out, e)
		}
	}
	return out
}

func otherEntryBinding() aj.EntryBinding {
	return aj.EntryBinding{
		VMPrefix: "az1", ParkerPrefix: "az1",
		VMIDRangeStart: 100, VMIDRangeEnd: 8999,
		DiskVMIDRangeStart: 9000, DiskVMIDRangeEnd: 29999,
		ParkedVMIDRangeStart: 90000, ParkedVMIDRangeEnd: 90999,
	}
}

// TestAdmissionRecordsTheEntryBindingSilently checks that the first admitted
// create records the effective entry's binding and logs nothing about it.
func TestAdmissionRecordsTheEntryBindingSilently(t *testing.T) {
	deps, j, _ := auditFixture(t)
	deps.Config.VMPrefix = "az1"
	deps, obs := observedDeps(deps)
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatal(err)
	}
	state, found, err := j.EntryBinding()
	if err != nil || !found {
		t.Fatalf("EntryBinding = %v, %v", found, err)
	}
	want := JournalEntryBinding(deps.Config)
	if state.Binding != want || state.Previous != nil || state.RecordedAt.IsZero() {
		t.Fatalf("recorded %+v, want %+v", state, want)
	}
	if want.ParkerPrefix != "az1" || want.ParkedVMIDRangeStart == 0 {
		t.Fatalf("binding did not resolve the parker prefix and band: %+v", want)
	}
	if w := bindingWarnings(obs); len(w) != 0 {
		t.Fatalf("first binding warned: %+v", w)
	}
}

// TestAdmissionWarnsWhenAnotherEntryBoundTheNamespace models a second
// cpi_config entry on the same cluster sharing the namespace. The VM path
// warns, names what differs, records both sides, and still admits. A second
// create from the same entry finds its binding recorded and stays quiet.
func TestAdmissionWarnsWhenAnotherEntryBoundTheNamespace(t *testing.T) {
	deps, j, _ := auditFixture(t)
	first := otherEntryBinding()
	firstAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := j.RecordEntryBinding(first, nil, firstAt); err != nil {
		t.Fatal(err)
	}
	deps.Config.VMIDRangeStart, deps.Config.VMIDRangeEnd = 100, 8999
	deps.Config.DiskVMIDRangeStart, deps.Config.DiskVMIDRangeEnd = 9000, 29999
	deps.Config.VMPrefix = "az2"
	deps.Config.ParkedDiskVMIDRangeStart, deps.Config.ParkedDiskVMIDRangeEnd = 91000, 91999
	deps, obs := observedDeps(deps)
	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatalf("a binding mismatch failed admission: %v", err)
	}
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 {
		t.Fatalf("want one binding warning, got %+v", warnings)
	}
	w := warnings[0]
	if w.Level != log.LevelWarn || !strings.Contains(w.Message, "differs") {
		t.Fatalf("warning = %+v", w)
	}
	if got := w.Attrs["changed_fields"]; got != "vm_prefix,parker_prefix,parked_disk_vmid_range" {
		t.Fatalf("changed_fields = %v", got)
	}
	if got, _ := w.Attrs["recorded_binding"].(string); !strings.Contains(got, "vm_prefix=az1") {
		t.Fatalf("recorded_binding = %v", w.Attrs["recorded_binding"])
	}
	if got, _ := w.Attrs["current_binding"].(string); !strings.Contains(got, "parked_vmids=91000-91999") {
		t.Fatalf("current_binding = %v", w.Attrs["current_binding"])
	}
	state, found, err := j.EntryBinding()
	if err != nil || !found {
		t.Fatalf("EntryBinding = %v, %v", found, err)
	}
	if state.Binding != JournalEntryBinding(deps.Config) || state.Previous == nil || *state.Previous != first || !state.PreviousRecordedAt.Equal(firstAt) {
		t.Fatalf("state after mismatch = %+v", state)
	}

	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatal(err)
	}
	if got := len(bindingWarnings(obs)); got != 1 {
		t.Fatalf("an unchanged binding warned again: %d warnings", got)
	}
}

// TestAdmissionDoesNotRewriteAnUnchangedBinding pins the steady state: the
// sidecar's inode survives an admission whose binding matches, so the common
// path costs no write and no fsync.
func TestAdmissionDoesNotRewriteAnUnchangedBinding(t *testing.T) {
	deps, j, _ := auditFixture(t)
	if err := j.RecordEntryBinding(JournalEntryBinding(deps.Config), nil, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(entryBindingSidecar(deps))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(entryBindingSidecar(deps))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("an unchanged binding was rewritten")
	}
}

// TestEntryBindingCorruptSidecarIsReplacedWithAWarning covers a sidecar that
// fails the journal's envelope checks. Admission warns, records the current
// binding over it, and proceeds.
func TestEntryBindingCorruptSidecarIsReplacedWithAWarning(t *testing.T) {
	deps, j, _ := auditFixture(t)
	if err := os.WriteFile(entryBindingSidecar(deps), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps, obs := observedDeps(deps)
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatalf("a corrupt sidecar failed admission: %v", err)
	}
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "unreadable") {
		t.Fatalf("warnings = %+v", warnings)
	}
	state, found, err := j.EntryBinding()
	if err != nil || !found || state.Binding != JournalEntryBinding(deps.Config) || state.Previous != nil {
		t.Fatalf("replacement = %+v, %v, %v", state, found, err)
	}
}

// TestEntryBindingUnsafeSidecarIsLeftAloneWithAWarning covers a sidecar the
// journal refuses to open because its mode is not private. That is a
// permission problem rather than bad content, so the check is skipped for the
// call and the file is not touched.
func TestEntryBindingUnsafeSidecarIsLeftAloneWithAWarning(t *testing.T) {
	deps, j, _ := auditFixture(t)
	path := entryBindingSidecar(deps)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	deps, obs := observedDeps(deps)
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatalf("an unsafe sidecar failed admission: %v", err)
	}
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "could not be read") {
		t.Fatalf("warnings = %+v", warnings)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{}" {
		t.Fatalf("unsafe sidecar was modified: %q, %v", data, err)
	}
}

// TestEntryBindingWriteFailureOnlyWarns makes the namespace directory
// unwritable, so recording the binding fails, and checks the note returns
// after a warning rather than panicking or surfacing an error.
func TestEntryBindingWriteFailureOnlyWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}
	deps, j, _ := auditFixture(t)
	dir := filepath.Dir(entryBindingSidecar(deps))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	deps, obs := observedDeps(deps)
	noteJournalEntryBinding(context.Background(), deps, j)
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "could not be recorded") {
		t.Fatalf("warnings = %+v", warnings)
	}
	if _, err := os.Stat(entryBindingSidecar(deps)); !os.IsNotExist(err) {
		t.Fatalf("sidecar exists after a failed write: %v", err)
	}
}

func TestNoteJournalEntryBindingToleratesMissingInputs(t *testing.T) {
	deps, j, _ := auditFixture(t)
	noteJournalEntryBinding(context.Background(), deps, nil)
	noteJournalEntryBinding(context.Background(), Deps{}, j)
	if _, found, err := j.EntryBinding(); err != nil || found {
		t.Fatalf("missing inputs recorded a binding: %v, %v", found, err)
	}
	if got := JournalEntryBinding(nil); got.ParkerPrefix == "" {
		t.Fatalf("nil config binding has no parker prefix: %+v", got)
	}
}
