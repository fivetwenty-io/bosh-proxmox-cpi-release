package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// entryBindingSidecar returns the sidecar path for the namespace deps names.
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

func seedEntryBinding(t *testing.T, j *aj.Journal, b aj.EntryBinding, at time.Time) {
	t.Helper()
	if note, err := j.NoteEntryBinding(context.Background(), b, at); err != nil || !note.Added {
		t.Fatalf("seed binding = %+v, %v", note, err)
	}
}

func recordedBindings(t *testing.T, j *aj.Journal) []aj.EntryBinding {
	t.Helper()
	state, found, err := j.EntryBinding()
	if err != nil {
		t.Fatalf("EntryBinding: %v", err)
	}
	if !found {
		return nil
	}
	out := make([]aj.EntryBinding, 0, len(state.Bindings))
	for _, seen := range state.Bindings {
		out = append(out, seen.Binding)
	}
	return out
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
	want := JournalEntryBinding(deps.Config)
	if got := recordedBindings(t, j); fmt.Sprint(got) != fmt.Sprint([]aj.EntryBinding{want}) {
		t.Fatalf("recorded %v, want [%v]", got, want)
	}
	if want.ParkerPrefix != "az1" || want.ParkedVMIDRangeStart == 0 {
		t.Fatalf("binding did not resolve the parker prefix and band: %+v", want)
	}
	if w := bindingWarnings(obs); len(w) != 0 {
		t.Fatalf("first binding warned: %+v", w)
	}
}

// TestAdmissionWarnsWhenAnotherEntryJoinsTheNamespace models a second
// cpi_config entry on the same cluster sharing the namespace. The VM path
// warns once, names what differs, keeps both bindings, and still admits.
// Further creates from either entry find their binding in the set and stay
// quiet, so two entries alternating never repeat the warning.
func TestAdmissionWarnsWhenAnotherEntryJoinsTheNamespace(t *testing.T) {
	deps, j, _ := auditFixture(t)
	first := otherEntryBinding()
	firstAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedEntryBinding(t, j, first, firstAt)
	deps.Config.VMIDRangeStart, deps.Config.VMIDRangeEnd = 100, 8999
	deps.Config.DiskVMIDRangeStart, deps.Config.DiskVMIDRangeEnd = 9000, 29999
	deps.Config.VMPrefix = "az2"
	deps.Config.ParkedDiskVMIDRangeStart, deps.Config.ParkedDiskVMIDRangeEnd = 91000, 91999
	deps, obs := observedDeps(deps)
	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatalf("a new binding failed admission: %v", err)
	}
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 {
		t.Fatalf("want one binding warning, got %+v", warnings)
	}
	w := warnings[0]
	if w.Level != log.LevelWarn || !strings.Contains(w.Message, "new to this namespace") {
		t.Fatalf("warning = %+v", w)
	}
	if got := w.Attrs["changed_fields"]; got != "vm_prefix,parker_prefix,parked_disk_vmid_range" {
		t.Fatalf("changed_fields = %v", got)
	}
	if got, _ := w.Attrs["recorded_bindings"].(string); !strings.Contains(got, "vm_prefix=az1") || !strings.Contains(got, "first seen 2026-10-01T00:00:00Z") {
		t.Fatalf("recorded_bindings = %v", w.Attrs["recorded_bindings"])
	}
	if got := w.Attrs["recorded_bindings_count"]; got != int64(1) {
		t.Fatalf("recorded_bindings_count = %v", got)
	}
	if got, _ := w.Attrs["new_binding"].(string); !strings.Contains(got, "parked_vmids=91000-91999") {
		t.Fatalf("new_binding = %v", w.Attrs["new_binding"])
	}
	want := []aj.EntryBinding{first, JournalEntryBinding(deps.Config)}
	if got := recordedBindings(t, j); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bindings = %v, want %v", got, want)
	}

	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatal(err)
	}
	firstEntry := deps
	firstEntry.Config = &config.CPIConfig{
		StoragePlacementNamespace: deps.Config.StoragePlacementNamespace, StorageAllocationJournalDir: deps.Config.StorageAllocationJournalDir,
		Node: "pve1", VMPrefix: "az1", VMIDRangeStart: 100, VMIDRangeEnd: 8999, DiskVMIDRangeStart: 9000, DiskVMIDRangeEnd: 29999,
		ParkedDiskVMIDRangeStart: 90000, ParkedDiskVMIDRangeEnd: 90999,
	}
	if JournalEntryBinding(firstEntry.Config) != first {
		t.Fatalf("first entry's config resolves %+v, want %+v", JournalEntryBinding(firstEntry.Config), first)
	}
	if err := admitStorageVMAllocation(context.Background(), firstEntry, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatal(err)
	}
	if got := len(bindingWarnings(obs)); got != 1 {
		t.Fatalf("alternating entries warned again: %d warnings", got)
	}
	if got := recordedBindings(t, j); len(got) != 2 {
		t.Fatalf("alternating entries changed the set: %v", got)
	}
}

// TestAdmissionDoesNotRewriteAKnownBinding pins the steady state: the
// sidecar's inode survives an admission whose binding is already in the set,
// so the common path costs no lock, no write, and no fsync.
func TestAdmissionDoesNotRewriteAKnownBinding(t *testing.T) {
	deps, j, _ := auditFixture(t)
	seedEntryBinding(t, j, otherEntryBinding(), time.Unix(1_699_000_000, 0))
	seedEntryBinding(t, j, JournalEntryBinding(deps.Config), time.Unix(1_700_000_000, 0))
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
		t.Fatal("a known binding was rewritten")
	}
}

// TestRefusedAdmissionRecordsNoBinding pins the order of the gates. The
// sidecar holds another entry's binding, and admission is refused by an
// unindexed VM conflict, by an unindexed volume conflict, and by the
// existing-provenance check. None of the refusals may add this entry's
// binding or warn about it, because a refused create admitted nothing.
func TestRefusedAdmissionRecordsNoBinding(t *testing.T) {
	recorded := otherEntryBinding()
	check := func(t *testing.T, deps Deps, j *aj.Journal, admit func(Deps) error) {
		t.Helper()
		if JournalEntryBinding(deps.Config) == recorded {
			t.Fatal("fixture binding equals the recorded one, so a refusal write would be invisible")
		}
		deps, obs := observedDeps(deps)
		if err := admit(deps); err == nil {
			t.Fatal("admission was not refused")
		}
		if got := recordedBindings(t, j); fmt.Sprint(got) != fmt.Sprint([]aj.EntryBinding{recorded}) {
			t.Fatalf("a refused admission changed the bindings to %v", got)
		}
		if w := bindingWarnings(obs); len(w) != 0 {
			t.Fatalf("a refused admission warned about bindings: %+v", w)
		}
	}

	t.Run("vm conflict", func(t *testing.T) {
		deps, j, c := auditFixture(t)
		seedEntryBinding(t, j, recorded, time.Unix(1_700_000_000, 0))
		sum := sha256.Sum256([]byte("agent"))
		marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "director",
			AllocationID: "12345678-1234-4234-8234-123456789abc", AgentSHA256: hex.EncodeToString(sum[:])})
		if err != nil {
			t.Fatal(err)
		}
		c.configs[123] = map[string]any{"description": marker}
		check(t, deps, j, func(d Deps) error {
			return admitStorageVMAllocation(context.Background(), d, j, []string{"pve1"}, "agent")
		})
	})
	t.Run("volume conflict", func(t *testing.T) {
		deps, j, c := auditFixture(t)
		seedEntryBinding(t, j, recorded, time.Unix(1_700_000_000, 0))
		name, err := pve.AllocationVolumeName(9001, "director", "12345678-1234-4234-8234-123456789abc", "qcow2")
		if err != nil {
			t.Fatal(err)
		}
		c.nodesRead.content = append(c.nodesRead.content, planJSON(t, map[string]any{"volid": "a:9001/" + name}))
		check(t, deps, j, func(d Deps) error {
			_, err := admitStorageAllocation(context.Background(), d, j, []string{"pve1"})
			return err
		})
	})
	t.Run("existing provenance", func(t *testing.T) {
		deps, j, c, _, _ := resumeVMFixture(t)
		moveResumeVM(c)
		seedEntryBinding(t, j, recorded, time.Unix(1_700_000_000, 0))
		check(t, deps, j, func(d Deps) error {
			err := admitStorageVMAllocation(t.Context(), d, j, []string{"pve1", "pve2"}, "agent")
			if err != nil && !strings.Contains(err.Error(), "already has remote VM provenance") {
				t.Fatalf("refused for another reason: %v", err)
			}
			return err
		})
	})
}

// TestEntryBindingCorruptSidecarIsReplacedWithAWarning covers a sidecar that
// fails the journal's envelope checks. Admission warns, records the current
// binding in a fresh set, and proceeds.
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
	if got := recordedBindings(t, j); fmt.Sprint(got) != fmt.Sprint([]aj.EntryBinding{JournalEntryBinding(deps.Config)}) {
		t.Fatalf("replacement = %v", got)
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

// TestEntryBindingNewerSidecarIsLeftAloneWithAWarning covers a downgrade. A
// newer release wrote the sidecar with a schema this one can't read, so
// admission warns once, skips the check, and never overwrites the file.
func TestEntryBindingNewerSidecarIsLeftAloneWithAWarning(t *testing.T) {
	deps, j, _ := auditFixture(t)
	newer := []byte(`{"version":99,"sha256":"00","payload":{"namespace":"director","sets":[]}}`)
	if err := os.WriteFile(entryBindingSidecar(deps), newer, 0o600); err != nil {
		t.Fatal(err)
	}
	deps, obs := observedDeps(deps)
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatalf("a newer sidecar failed admission: %v", err)
	}
	warnings := bindingWarnings(obs)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "newer CPI release") {
		t.Fatalf("warnings = %+v", warnings)
	}
	data, err := os.ReadFile(entryBindingSidecar(deps))
	if err != nil || string(data) != string(newer) {
		t.Fatalf("a newer sidecar was modified: %q, %v", data, err)
	}
}

// TestEntryBindingWriteFailureOnlyWarns makes the namespace directory
// unwritable after the lock file exists, so adding a new binding fails at the
// write, and checks the note returns after a warning with the old set intact.
func TestEntryBindingWriteFailureOnlyWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}
	deps, j, _ := auditFixture(t)
	seedEntryBinding(t, j, otherEntryBinding(), time.Unix(1_700_000_000, 0))
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
	var recordedWarning bool
	for _, w := range bindingWarnings(obs) {
		if strings.Contains(w.Message, "could not be recorded") {
			recordedWarning = true
		}
	}
	if !recordedWarning {
		t.Fatalf("warnings = %+v", bindingWarnings(obs))
	}
	if got := recordedBindings(t, j); fmt.Sprint(got) != fmt.Sprint([]aj.EntryBinding{otherEntryBinding()}) {
		t.Fatalf("bindings after a failed write = %v", got)
	}
}

// TestJournalEntryBindingFoldsParkerPrefixCase checks that two entries whose
// parker prefixes differ only in case, which the parker code treats as one
// family, produce one binding, while VM prefixes keep their case.
func TestJournalEntryBindingFoldsParkerPrefixCase(t *testing.T) {
	upper := JournalEntryBinding(&config.CPIConfig{ParkerPrefix: "AZ1", VMPrefix: "Prod"})
	lower := JournalEntryBinding(&config.CPIConfig{ParkerPrefix: "az1", VMPrefix: "Prod"})
	if upper != lower || upper.ParkerPrefix != "az1" {
		t.Fatalf("bindings %+v and %+v differ", upper, lower)
	}
	if other := JournalEntryBinding(&config.CPIConfig{ParkerPrefix: "az1", VMPrefix: "prod"}); other == lower {
		t.Fatal("VM prefixes that differ in case produced one binding")
	}
}

func TestNoteJournalEntryBindingToleratesMissingInputs(t *testing.T) {
	deps, j, _ := auditFixture(t)
	noteJournalEntryBinding(context.Background(), deps, nil)
	noteJournalEntryBinding(context.Background(), Deps{}, j)
	if _, found, err := j.EntryBinding(); err != nil || found {
		t.Fatalf("missing inputs recorded a binding: %v, %v", found, err)
	}
	if got, want := JournalEntryBinding(nil), JournalEntryBinding(&config.CPIConfig{}); got != want || got.ParkerPrefix != "bosh" {
		t.Fatalf("nil config binding = %+v, want the empty configuration's %+v", got, want)
	}
}
