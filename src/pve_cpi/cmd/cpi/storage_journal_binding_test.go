package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

func bindingAuditJournal(t *testing.T) (*aj.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := aj.Initialize(context.Background(), dir, "director", aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	sum := sha256.Sum256([]byte("director"))
	return j, filepath.Join(dir, hex.EncodeToString(sum[:]), "entry-binding.json")
}

func bindingAuditConfig(vmPrefix string) *config.CPIConfig {
	return &config.CPIConfig{VMPrefix: vmPrefix, VMIDRangeStart: 100, VMIDRangeEnd: 8999, DiskVMIDRangeStart: 9000, DiskVMIDRangeEnd: 29999}
}

func noteBinding(t *testing.T, j *aj.Journal, b aj.EntryBinding, at time.Time) {
	t.Helper()
	if _, err := j.NoteEntryBinding(context.Background(), b, at); err != nil {
		t.Fatal(err)
	}
}

func TestAuditStorageJournalEntryBinding(t *testing.T) {
	firstSeen := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	t.Run("no journal", func(t *testing.T) {
		if got := auditStorageJournalEntryBinding(nil, bindingAuditConfig("az1")); got.State != nil || got.Finding != "" {
			t.Fatalf("nil journal = %+v", got)
		}
	})
	t.Run("nothing recorded", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		if got := auditStorageJournalEntryBinding(j, bindingAuditConfig("az1")); got.State != nil || got.Finding != "" {
			t.Fatalf("fresh namespace = %+v", got)
		}
	})
	t.Run("one matching binding", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		cfg := bindingAuditConfig("az1")
		noteBinding(t, j, handlers.JournalEntryBinding(cfg), firstSeen)
		got := auditStorageJournalEntryBinding(j, cfg)
		if got.State == nil || len(got.State.Bindings) != 1 || got.State.Bindings[0].Binding != handlers.JournalEntryBinding(cfg) || got.Finding != "" {
			t.Fatalf("matching binding = %+v", got)
		}
	})
	t.Run("one differing binding", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		noteBinding(t, j, handlers.JournalEntryBinding(bindingAuditConfig("az2")), firstSeen)
		got := auditStorageJournalEntryBinding(j, bindingAuditConfig("az1"))
		if got.State == nil || len(got.State.Bindings) != 1 || got.State.Bindings[0].Binding.VMPrefix != "az2" {
			t.Fatalf("state = %+v", got.State)
		}
		for _, want := range []string{"differs from this configuration in vm_prefix,parker_prefix", "vm_prefix=az2", "first seen 2026-10-01T00:00:00Z", "configured vm_prefix=az1", "storage_placement_namespace", "job-level configuration only"} {
			if !strings.Contains(got.Finding, want) {
				t.Fatalf("finding %q lacks %q", got.Finding, want)
			}
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		j, path := bindingAuditJournal(t)
		if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := auditStorageJournalEntryBinding(j, bindingAuditConfig("az1"))
		if got.State != nil || !strings.Contains(got.Finding, "entry binding unreadable") {
			t.Fatalf("unreadable sidecar = %+v", got)
		}
	})
	t.Run("written by a newer release", func(t *testing.T) {
		j, path := bindingAuditJournal(t)
		raw := []byte(`{"version":99,"sha256":"","payload":{"future":true}}`)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got := auditStorageJournalEntryBinding(j, bindingAuditConfig("az1"))
		if got.State != nil || !strings.Contains(got.Finding, "written by a newer CPI release") {
			t.Fatalf("newer sidecar = %+v", got)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, raw) {
			t.Fatalf("audit changed a newer sidecar: %q, %v", after, err)
		}
	})
}

// TestAuditStorageJournalEntryBindingReportsEverySighting covers two entries
// that alternate creates in one namespace. Both bindings stay in the set, so
// the audit reports them whichever entry wrote last and whether or not the
// job-level configuration is one of them.
func TestAuditStorageJournalEntryBindingReportsEverySighting(t *testing.T) {
	firstSeen := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, prefix, standing string
	}{
		{"job-level among them", "az1", "matches binding 2"},
		{"job-level not among them", "az9", "matches none of them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := bindingAuditJournal(t)
			noteBinding(t, j, handlers.JournalEntryBinding(bindingAuditConfig("az2")), firstSeen)
			noteBinding(t, j, handlers.JournalEntryBinding(bindingAuditConfig("az1")), firstSeen.Add(time.Hour))
			noteBinding(t, j, handlers.JournalEntryBinding(bindingAuditConfig("az2")), firstSeen.Add(2*time.Hour))
			got := auditStorageJournalEntryBinding(j, bindingAuditConfig(tc.prefix))
			if got.State == nil || len(got.State.Bindings) != 2 || got.State.Bindings[0].Binding.VMPrefix != "az2" || got.State.Bindings[1].Binding.VMPrefix != "az1" {
				t.Fatalf("state = %+v", got.State)
			}
			for _, want := range []string{"recorded 2 entry bindings", tc.standing, "storage_placement_namespace", "job-level configuration only", "delete entry-binding.json"} {
				if !strings.Contains(got.Finding, want) {
					t.Fatalf("finding %q lacks %q", got.Finding, want)
				}
			}
		})
	}
}

// TestStorageJournalAuditReportsEntryBinding checks both output modes carry
// every binding and the finding, and that a finding leaves a clean audit's
// exit code at 0.
func TestStorageJournalAuditReportsEntryBinding(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	state := aj.EntryBindingState{Namespace: "director", Bindings: []aj.EntryBindingSighting{
		{Binding: handlers.JournalEntryBinding(bindingAuditConfig("az2")), FirstSeen: first},
		{Binding: handlers.JournalEntryBinding(bindingAuditConfig("az1")), FirstSeen: first.Add(time.Hour)},
	}}
	binding := storageJournalBindingAudit{State: &state, Finding: "namespace has recorded 2 entry bindings"}

	var jsonOut, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&jsonOut, &stderr, report, nil, true, false, binding); code != 0 {
		t.Fatalf("JSON audit with a binding finding exited %d: %s", code, stderr.String())
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	var finding string
	if err := json.Unmarshal(decoded["entry_binding_finding"], &finding); err != nil || finding != binding.Finding {
		t.Fatalf("entry_binding_finding = %s, %v", decoded["entry_binding_finding"], err)
	}
	var roundTrip aj.EntryBindingState
	if err := json.Unmarshal(decoded["entry_bindings"], &roundTrip); err != nil || len(roundTrip.Bindings) != 2 ||
		roundTrip.Bindings[0] != state.Bindings[0] || roundTrip.Bindings[1].Binding != state.Bindings[1].Binding {
		t.Fatalf("entry_bindings = %s, %v", decoded["entry_bindings"], err)
	}

	var text bytes.Buffer
	if code := writeStorageJournalAudit(&text, &stderr, report, nil, true, true, binding); code != 0 {
		t.Fatalf("summary audit with a binding finding exited %d", code)
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=0\n" +
		"entry binding 1: " + state.Bindings[0].Binding.String() + " first seen 2026-10-01T00:00:00Z\n" +
		"entry binding 2: " + state.Bindings[1].Binding.String() + " first seen 2026-10-01T01:00:00Z\n" +
		"entry binding finding: " + binding.Finding + "\n" +
		"charging: none\n"
	if text.String() != want {
		t.Fatalf("summary = %q, want %q", text.String(), want)
	}

	jsonOut.Reset()
	if code := writeStorageJournalAudit(&jsonOut, &stderr, report, nil, true, false, storageJournalBindingAudit{}); code != 0 {
		t.Fatalf("clean audit exited %d", code)
	}
	if strings.Contains(jsonOut.String(), "entry_binding") {
		t.Fatalf("empty binding audit printed entry_binding keys: %s", jsonOut.String())
	}
}

// TestStorageJournalAuditCommandReadsTheEntryBinding drives the audit command
// end to end against an enrolled namespace whose sidecar another entry wrote.
// Both output modes report the binding and the finding, and the exit code
// matches the one the same audit returns before the sidecar exists.
func TestStorageJournalAuditCommandReadsTheEntryBinding(t *testing.T) {
	f := newStorageJournalFixture(t)
	var baseline bytes.Buffer
	baselineCode, baselineErr := f.runTo(&baseline, "audit")
	if strings.Contains(baseline.String(), "entry_binding") {
		t.Fatalf("audit before any binding printed one: %s", baseline.String())
	}

	status, err := aj.InspectEnrollment(f.directory, "director")
	if err != nil {
		t.Fatal(err)
	}
	j, err := aj.Open(f.directory, "director", status.Enrollment.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	other := aj.EntryBinding{VMPrefix: "other-entry", ParkerPrefix: "other-entry", VMIDRangeStart: 100, VMIDRangeEnd: 8999, DiskVMIDRangeStart: 9000, DiskVMIDRangeEnd: 29999, ParkedVMIDRangeStart: 90000, ParkedVMIDRangeEnd: 90999}
	noteBinding(t, j, other, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, stderr := f.runTo(&out, "audit")
	if code != baselineCode {
		t.Fatalf("binding finding changed the exit code: %d, baseline %d (%s / %s)", code, baselineCode, stderr, baselineErr)
	}
	var decoded struct {
		EntryBindings       *aj.EntryBindingState `json:"entry_bindings"`
		EntryBindingFinding string                `json:"entry_binding_finding"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("audit JSON: %v: %s", err, out.String())
	}
	if decoded.EntryBindings == nil || len(decoded.EntryBindings.Bindings) != 1 || decoded.EntryBindings.Bindings[0].Binding != other ||
		!strings.Contains(decoded.EntryBindingFinding, "vm_prefix=other-entry") || !strings.Contains(decoded.EntryBindingFinding, "job-level configuration only") {
		t.Fatalf("audit JSON bindings = %+v, finding %q", decoded.EntryBindings, decoded.EntryBindingFinding)
	}

	out.Reset()
	code, _ = f.runTo(&out, "audit", "--summary")
	for _, want := range []string{"\nentry binding 1: " + other.String() + " first seen 2026-10-01T00:00:00Z\n", "\nentry binding finding: recorded binding differs from this configuration in "} {
		if code != baselineCode || !strings.Contains(out.String(), want) {
			t.Fatalf("summary exited %d, lacks %q: %s", code, want, out.String())
		}
	}
}

// TestStorageJournalHelpNamesTheJobLevelComparison checks the flag help says
// the audit compares bindings against the job-level configuration only.
func TestStorageJournalHelpNamesTheJobLevelComparison(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runStorageJournal([]string{"audit", "-h"}, &stdout, &stderr, runOptions{}); code != 0 {
		t.Fatalf("help exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "job-level values only") {
		t.Fatalf("help lacks the job-level note: %s", stderr.String())
	}
}
