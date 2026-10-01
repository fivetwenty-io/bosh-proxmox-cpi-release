package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// brokenPipe is the write error a closed stdout produces.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
}

func (f *storageJournalFixture) runTo(stdout *bytes.Buffer, action string, args ...string) (int, string) {
	var stderr bytes.Buffer
	command := append([]string{action, "--config", f.configPath}, args...)
	code := runStorageJournal(command, stdout, &stderr, runOptions{})
	return code, stderr.String()
}

// namespaceDirectory returns the one namespace directory initialize created.
func (f *storageJournalFixture) namespaceDirectory(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(f.directory)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if entry.IsDir() {
			found = append(found, filepath.Join(f.directory, entry.Name()))
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected one namespace directory, found %v", found)
	}
	return found[0]
}

func writeStorageJournalConfig(t *testing.T, cfg *config.CPIConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(provisionTemp(t), "cpi.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorageJournalConfigFailuresPrintNoValues(t *testing.T) {
	missing := filepath.Join(provisionTemp(t), "absent.json")
	var out, stderr bytes.Buffer
	if code := runStorageJournal([]string{"audit", "--config", missing}, &out, &stderr, runOptions{}); code != 1 || stderr.String() != "config "+missing+" not found\n" {
		t.Fatalf("missing config reported as %d %q", code, stderr.String())
	}

	invalid := minimalCfg()
	invalid.NodeEndpoints = map[string]string{"pve": "https://SECRET-ENDPOINT-VALUE/"}
	path := writeStorageJournalConfig(t, invalid)
	if _, err := config.LoadFile(path); err == nil || !strings.Contains(err.Error(), "SECRET-ENDPOINT-VALUE") {
		t.Fatalf("fixture must fail validation with a value-echoing error, got %v", err)
	}
	out.Reset()
	stderr.Reset()
	code := runStorageJournal([]string{"audit", "--config", path}, &out, &stderr, runOptions{})
	if code != 1 || stderr.String() != "configuration invalid; check it with the CPI's startup validation\n" {
		t.Fatalf("invalid config reported as %d %q", code, stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), "SECRET-ENDPOINT-VALUE") || strings.Contains(out.String()+stderr.String(), "root@pam!tok=secret") {
		t.Fatal("config failure printed a configuration value")
	}

	unplaced := minimalCfg()
	path = writeStorageJournalConfig(t, unplaced)
	out.Reset()
	stderr.Reset()
	code = runStorageJournal([]string{"audit", "--config", path}, &out, &stderr, runOptions{})
	if code != 1 || stderr.String() != "storage journal configuration incomplete: storage_placement_namespace is required for set-enabled allocation\n" {
		t.Fatalf("config without a namespace reported as %d %q", code, stderr.String())
	}

	noJournal := minimalCfg()
	noJournal.StoragePlacementNamespace = "director"
	noJournal.StorageAllocationJournalDir = ""
	path = writeStorageJournalConfig(t, noJournal)
	out.Reset()
	stderr.Reset()
	code = runStorageJournal([]string{"audit", "--config", path}, &out, &stderr, runOptions{})
	if code != 1 || stderr.String() != "storage journal configuration incomplete: storage_allocation_journal_dir must be an absolute durable directory for set-enabled allocation\n" {
		t.Fatalf("config without a journal directory reported as %d %q", code, stderr.String())
	}
}

func TestStorageJournalEnrollmentFailuresAreNamed(t *testing.T) {
	t.Run("missing journal directory", func(t *testing.T) {
		f := newStorageJournalFixture(t)
		if err := os.Rename(f.directory, f.directory+".moved"); err != nil {
			t.Fatal(err)
		}
		want := "journal directory " + f.directory + " is missing; run provision-journal"
		for _, command := range [][]string{{"audit"}, {"initialize", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"}} {
			var out bytes.Buffer
			code, stderr := f.runTo(&out, command[0], command[1:]...)
			if code != 1 || stderr != want+"\n" {
				t.Fatalf("%s on a missing directory reported as %d %q", command[0], code, stderr)
			}
		}
	})
	t.Run("missing namespace", func(t *testing.T) {
		f := newStorageJournalFixture(t)
		if err := os.RemoveAll(f.namespaceDirectory(t)); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code, stderr := f.runTo(&out, "audit")
		if code != 1 || stderr != "namespace director is not enrolled in "+f.directory+"; run audit-enrollment, then initialize\n" {
			t.Fatalf("missing namespace reported as %d %q", code, stderr)
		}
	})
	t.Run("missing authority", func(t *testing.T) {
		f := newStorageJournalFixture(t)
		if err := os.Remove(filepath.Join(f.namespaceDirectory(t), "authority.json")); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code, stderr := f.runTo(&out, "audit")
		if code != 1 || !strings.Contains(stderr, "namespace director is not enrolled in "+f.directory) {
			t.Fatalf("missing authority reported as %d %q", code, stderr)
		}
	})
	t.Run("corrupt authority", func(t *testing.T) {
		f := newStorageJournalFixture(t)
		if err := os.WriteFile(filepath.Join(f.namespaceDirectory(t), "authority.json"), []byte("authority-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code, stderr := f.runTo(&out, "audit")
		if code != 1 || !strings.Contains(stderr, "existing journal enrollment unavailable") || !strings.Contains(stderr, "invalid journal evidence: malformed envelope") {
			t.Fatalf("corrupt authority reported as %d %q", code, stderr)
		}
		if strings.Contains(stderr, "authority-secret") {
			t.Fatal("corrupt authority content was printed")
		}
	})
	t.Run("widened permissions", func(t *testing.T) {
		f := newStorageJournalFixture(t)
		if err := os.Chmod(f.directory, 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code, stderr := f.runTo(&out, "audit")
		if code != 1 || !strings.Contains(stderr, "journal: unsafe permissions: "+f.directory+" has mode 0755") {
			t.Fatalf("widened journal reported as %d %q", code, stderr)
		}
	})
}

func TestStorageJournalOutputFailuresAreReported(t *testing.T) {
	var stderr bytes.Buffer
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	if code := writeStorageJournalAudit(brokenPipe{}, &stderr, report, nil, true, false); code != 1 || stderr.String() != "audit output could not be written: write /dev/stdout: broken pipe\n" {
		t.Fatalf("broken audit output reported as %d %q", code, stderr.String())
	}
	f := newStorageJournalFixture(t)
	var failing bytes.Buffer
	code := runStorageJournal([]string{"audit-enrollment", "--config", f.configPath}, brokenPipe{}, &failing, runOptions{})
	if code != 1 || !strings.Contains(failing.String(), "audit-enrollment output could not be written: write /dev/stdout: broken pipe") {
		t.Fatalf("broken enrollment output reported as %d %q", code, failing.String())
	}
}

func TestStorageJournalInitializeRefusalNamesExistingProvenance(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, Evidence: []handlers.StorageAllocationEvidence{
		{AllocationID: "d4", Kind: "vm"}, {AllocationID: "a1", Kind: "vm"}, {AllocationID: "a1", Kind: "volume"},
		{AllocationID: "c3", Kind: "vm"}, {AllocationID: "b2", Kind: "disk"},
	}}
	var stderr bytes.Buffer
	storageJournalRefuse(&stderr, "initialization refused: complete historical absence is unproven; inspect audit-enrollment output", storageJournalInitializePreconditions(report), report)
	want := "initialization refused: complete historical absence is unproven; inspect audit-enrollment output\n" +
		"precondition failed: existing allocation provenance: 5 evidence entries name 4 allocations: a1, b2, c3 and 1 more\n"
	if stderr.String() != want {
		t.Fatalf("refusal = %q, want %q", stderr.String(), want)
	}

	incomplete := handlers.StorageAllocationAudit{Complete: false, VMScanComplete: false, Issues: []string{"cluster-wide VM and storage audit visibility is unproven: allocation audit requires Datastore.Audit at /storage"}, VMScanIssues: []string{"cluster-wide VM and storage audit visibility is unproven: allocation audit requires Datastore.Audit at /storage"}, Conflicts: []string{"remote allocation x (VM 4626) is outside recorded mutation targets"}}
	stderr.Reset()
	storageJournalRefuse(&stderr, "initialization refused", storageJournalInitializePreconditions(incomplete), incomplete)
	for _, line := range []string{
		"precondition failed: the VM scan is incomplete",
		"precondition failed: the audit raised 1 conflict",
		"audit findings: 1 audit conflict, 1 audit issue; remote allocation x (VM 4626) is outside recorded mutation targets; cluster-wide VM and storage audit visibility is unproven: allocation audit requires Datastore.Audit at /storage; " + storageJournalTestRunbook,
	} {
		if !strings.Contains(stderr.String(), line+"\n") {
			t.Fatalf("refusal %q lacks %q", stderr.String(), line)
		}
	}
}

func TestStorageJournalRecoveryRefusalNamesItsPrecondition(t *testing.T) {
	clean := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	indexErr := &aj.DurabilityError{Err: &fs.PathError{Op: "open", Path: "/j/index.json", Err: syscall.ENOENT}}
	if got := storageJournalRecoveryPreconditions("recover-authority", clean, indexErr); len(got) != 1 || got[0] != "the generation index is invalid or unavailable: journal durability failure; reconcile before mutation (open /j/index.json: no such file or directory)" {
		t.Fatalf("recover-authority index precondition = %q", got)
	}
	if got := storageJournalRecoveryPreconditions("recover-index", clean, indexErr); len(got) != 0 {
		t.Fatalf("recover-index refused on the index it repairs: %q", got)
	}

	f := newStorageJournalFixture(t)
	journalFixtureVM(t, f)
	f.restricted.Store(true)
	code, out := f.run("recover-authority", "--authority-id", "new-writer", "--previous-writer-fenced")
	if code != 1 {
		t.Fatalf("partial audit permitted recovery: %s", out)
	}
	for _, want := range []string{"recovery requires a complete consistent historical audit\n", "precondition failed: the VM scan is incomplete\n", "audit findings: 1 audit issue; cluster-wide VM and storage audit visibility is unproven: allocation audit visibility is restricted at /storage; " + storageJournalTestRunbook + "\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("recovery refusal %q lacks %q", out, want)
		}
	}
}

func TestStorageJournalMissingGenerationRefusalNamesItsPrecondition(t *testing.T) {
	now := time.Now()
	clean := handlers.StorageAllocationAudit{StartedAt: now, CompletedAt: now, Complete: true, VMScanComplete: true}
	request := storageJournalRequest{AgentID: "agent", AllocationID: "allocation", IndexFirstConfirmed: true}
	for _, tc := range []struct {
		name               string
		request            storageJournalRequest
		authority          string
		fenced             bool
		clusterID          string
		report             handlers.StorageAllocationAudit
		want               string
		wantAuditFindings  bool
		wantNoOtherFailure bool
	}{
		{"unfenced", request, "writer", false, "cluster", clean, "precondition failed: the previous writer is not attested as fenced", false, true},
		{"blank authority", request, " ", true, "cluster", clean, "precondition failed: authority-id is blank", false, true},
		{"unconfirmed crash", storageJournalRequest{AgentID: "agent", AllocationID: "allocation"}, "writer", true, "cluster", clean, "precondition failed: the index-first crash is not confirmed", false, true},
		{"cluster continuity", request, "writer", true, "other-cluster", clean, "precondition failed: cluster continuity is lost; PVE reports a cluster identity other than the enrolled one", false, true},
		{"unstamped audit", request, "writer", true, "cluster", handlers.StorageAllocationAudit{CompletedAt: now, Complete: true, VMScanComplete: true}, "precondition failed: the audit has no start time", false, true},
		{"backwards audit", request, "writer", true, "cluster", handlers.StorageAllocationAudit{StartedAt: now, CompletedAt: now.Add(-time.Second), Complete: true, VMScanComplete: true}, "precondition failed: the audit completed before it started", false, true},
		{"incomplete audit", request, "writer", true, "cluster", handlers.StorageAllocationAudit{StartedAt: now, CompletedAt: now, Issues: []string{`storage "nas" on node "pve1" could not be inspected: context deadline exceeded`}, VMScanComplete: true}, "precondition failed: the storage audit is incomplete", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := resolveStorageJournalMissingVM(t.Context(), minimalCfg(), nil, tc.clusterID, "cluster", tc.report, tc.request, tc.authority, tc.fenced, &out, &stderr)
			text := stderr.String()
			if code != 1 || !strings.HasPrefix(text, "missing-generation recovery requires fencing, cluster continuity and a complete fresh audit\n") || !strings.Contains(text, tc.want+"\n") {
				t.Fatalf("refusal %d %q lacks %q", code, text, tc.want)
			}
			if strings.Contains(text, "audit findings: ") != tc.wantAuditFindings {
				t.Fatalf("refusal %q audit findings presence, want %t", text, tc.wantAuditFindings)
			}
			if tc.wantNoOtherFailure && strings.Count(text, "precondition failed: ") != 1 {
				t.Fatalf("refusal %q names preconditions that held", text)
			}
		})
	}
}
