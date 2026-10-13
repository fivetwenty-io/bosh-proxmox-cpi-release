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

func TestAuditStorageJournalEntryBinding(t *testing.T) {
	recordedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

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
	t.Run("matching", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		cfg := bindingAuditConfig("az1")
		if err := j.RecordEntryBinding(handlers.JournalEntryBinding(cfg), nil, recordedAt); err != nil {
			t.Fatal(err)
		}
		got := auditStorageJournalEntryBinding(j, cfg)
		if got.State == nil || got.State.Binding != handlers.JournalEntryBinding(cfg) || got.Finding != "" {
			t.Fatalf("matching binding = %+v", got)
		}
	})
	t.Run("differs", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		if err := j.RecordEntryBinding(handlers.JournalEntryBinding(bindingAuditConfig("az2")), nil, recordedAt); err != nil {
			t.Fatal(err)
		}
		got := auditStorageJournalEntryBinding(j, bindingAuditConfig("az1"))
		if got.State == nil || got.State.Binding.VMPrefix != "az2" {
			t.Fatalf("state = %+v", got.State)
		}
		for _, want := range []string{"differs from this configuration in vm_prefix,parker_prefix", "vm_prefix=az2", "2026-10-01T00:00:00Z", "configured vm_prefix=az1", "storage_placement_namespace"} {
			if !strings.Contains(got.Finding, want) {
				t.Fatalf("finding %q lacks %q", got.Finding, want)
			}
		}
	})
	t.Run("matching after a replacement", func(t *testing.T) {
		j, _ := bindingAuditJournal(t)
		other := handlers.JournalEntryBinding(bindingAuditConfig("az2"))
		if err := j.RecordEntryBinding(other, nil, recordedAt); err != nil {
			t.Fatal(err)
		}
		first, _, err := j.EntryBinding()
		if err != nil {
			t.Fatal(err)
		}
		cfg := bindingAuditConfig("az1")
		if err := j.RecordEntryBinding(handlers.JournalEntryBinding(cfg), &first, recordedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got := auditStorageJournalEntryBinding(j, cfg)
		for _, want := range []string{"replaced a different one at 2026-10-01T01:00:00Z", "previous vm_prefix=az2", "recorded at 2026-10-01T00:00:00Z"} {
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
}

// TestStorageJournalAuditReportsEntryBinding checks both output modes carry
// the binding and its finding, and that a finding leaves a clean audit's exit
// code at 0.
func TestStorageJournalAuditReportsEntryBinding(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	state := aj.EntryBindingState{Version: 1, Namespace: "director", Binding: handlers.JournalEntryBinding(bindingAuditConfig("az2")), RecordedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	binding := storageJournalBindingAudit{State: &state, Finding: "recorded binding differs from this configuration in vm_prefix"}

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
	if err := json.Unmarshal(decoded["entry_binding"], &roundTrip); err != nil || roundTrip.Binding != state.Binding {
		t.Fatalf("entry_binding = %s, %v", decoded["entry_binding"], err)
	}

	var text bytes.Buffer
	if code := writeStorageJournalAudit(&text, &stderr, report, nil, true, true, binding); code != 0 {
		t.Fatalf("summary audit with a binding finding exited %d", code)
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=0\n" +
		"entry binding: " + binding.Finding + "\n" +
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
// Both output modes report the finding, and the exit code matches the one the
// same audit returns before the sidecar exists.
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
	if err := j.RecordEntryBinding(other, nil, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, stderr := f.runTo(&out, "audit")
	if code != baselineCode {
		t.Fatalf("binding finding changed the exit code: %d, baseline %d (%s / %s)", code, baselineCode, stderr, baselineErr)
	}
	var decoded struct {
		EntryBinding        *aj.EntryBindingState `json:"entry_binding"`
		EntryBindingFinding string                `json:"entry_binding_finding"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("audit JSON: %v: %s", err, out.String())
	}
	if decoded.EntryBinding == nil || decoded.EntryBinding.Binding != other || !strings.Contains(decoded.EntryBindingFinding, "vm_prefix=other-entry") {
		t.Fatalf("audit JSON binding = %+v, finding %q", decoded.EntryBinding, decoded.EntryBindingFinding)
	}

	out.Reset()
	code, _ = f.runTo(&out, "audit", "--summary")
	if code != baselineCode || !strings.Contains(out.String(), "\nentry binding: recorded binding differs from this configuration in ") {
		t.Fatalf("summary exited %d: %s", code, out.String())
	}
}
