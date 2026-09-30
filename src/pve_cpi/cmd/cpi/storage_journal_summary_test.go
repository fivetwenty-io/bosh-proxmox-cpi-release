package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

func TestStorageJournalSummaryIsRejectedOutsideAudit(t *testing.T) {
	for _, command := range [][]string{
		{"initialize", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"},
		{"recover-authority", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced"},
		{"recover-index", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"},
		{"resolve-missing-vm", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--agent-id", "agent", "--allocation-id", "allocation", "--index-first-crash-confirmed"},
		{"adopt", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket", "--expected-cid", "cid"},
		{"finalize-cleanup", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket"},
		{"cleanup", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket"},
	} {
		t.Run(command[0], func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := runStorageJournal(command, &out, &stderr, runOptions{}); code == 2 {
				// Without --summary the same flags pass validation and fail
				// later, on the unopened config, with exit code 1.
				t.Fatalf("valid flags refused: %s", stderr.String())
			}
			stderr.Reset()
			code := runStorageJournal(append(command, "--summary"), &out, &stderr, runOptions{})
			if code != 2 || !strings.Contains(stderr.String(), "usage: cpi storage-journal ") || !strings.Contains(stderr.String(), "--summary applies only to audit and audit-enrollment") {
				t.Fatalf("--summary accepted for %s: %d %q", command[0], code, stderr.String())
			}
		})
	}
	for _, action := range []string{"audit", "audit-enrollment"} {
		var out, stderr bytes.Buffer
		if code := runStorageJournal([]string{action, "--config", "/unopened", "--summary"}, &out, &stderr, runOptions{}); code == 2 {
			t.Fatalf("--summary refused for %s: %s", action, stderr.String())
		}
	}
}

func TestStorageJournalAuditSummaryCleanAudit(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, created, created)},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("clean audit exited %d: %s", code, stderr.String())
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=1\n" +
		"charging: none\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
}

func TestStorageJournalAuditSummaryFieldCase(t *testing.T) {
	now := time.Now().UTC()
	vmScanIssue := "some cluster nodes could not be inspected: pvupvecf104 (reported offline by /cluster/status)"
	report := handlers.StorageAllocationAudit{
		Complete: false, VMScanComplete: false,
		Conflicts: []string{
			"disk ownership provenance disagrees with actual holder node: disk allocation 9c2e (volume nas:vm-4626-disk-1) held by VM 4626 on pvupvecf102, provenance names pvupvecf101",
			"remote allocation 180f7d1e (VM 4626) is outside recorded mutation targets: observed on pvupvecf102, recorded pvupvecf101 (node_mismatch)",
			"remote allocation 2a4b0f7c (VM 7014) is outside recorded mutation targets: observed on pvupvecf103, recorded pvupvecf102 (node_mismatch)\nwith an injected line",
		},
		Issues:                  []string{vmScanIssue, `storage "nas" on node "pvupvecf101" could not be inspected: context deadline exceeded`},
		VMScanIssues:            []string{vmScanIssue},
		SkippedDisabledStorages: []string{"archive", "old-nfs"},
		Records: []aj.Record{
			storageJournalAuditTestRecord("planned-1", aj.Planned, now.Add(-2*time.Hour), now),
			storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, now, now),
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 1 {
		t.Fatalf("incomplete audit exited %d", code)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		"audit: complete=false vm_scan_complete=false generation_index_healthy=true cluster_continuity=true records=2",
		"conflict: " + report.Conflicts[0],
		"conflict: " + report.Conflicts[1],
		"conflict: remote allocation 2a4b0f7c (VM 7014) is outside recorded mutation targets: observed on pvupvecf103, recorded pvupvecf102 (node_mismatch) with an injected line",
		"vm-scan issue: " + vmScanIssue,
		`issue: storage "nas" on node "pvupvecf101" could not be inspected: context deadline exceeded`,
	}
	if len(lines) != len(want)+2 {
		t.Fatalf("summary has %d lines, want %d: %q", len(lines), len(want)+2, out.String())
	}
	for i, line := range want {
		if lines[i] != line {
			t.Fatalf("line %d = %q, want %q", i, lines[i], line)
		}
	}
	if !strings.HasPrefix(lines[len(want)], "charging: 1 record, oldest planned-1 opened 2h0m") {
		t.Fatalf("charging line = %q", lines[len(want)])
	}
	if lines[len(want)+1] != "skipped disabled storages: archive, old-nfs" {
		t.Fatalf("skipped storage line = %q", lines[len(want)+1])
	}
}

func TestStorageJournalAuditSummaryNamesAnUnhealthyIndex(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, aj.ErrCorrupt, false, true); code != 1 {
		t.Fatalf("unhealthy index exited %d", code)
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=false cluster_continuity=false records=0\n" +
		"generation index: generation index invalid or unavailable; record listing does not establish healthy authority\n" +
		"charging: none\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
}

func TestStorageJournalEnrollmentSummary(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Evidence: []handlers.StorageAllocationEvidence{
			{AllocationID: "a1", Kind: "vm", Node: "pve1", VMID: 4626},
			{AllocationID: "a1", Kind: "volume", Node: "pve1", VMID: 4626, VolumeID: "nas:vm-4626-disk-0"},
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalEnrollment(&out, &stderr, report, true); code != 0 {
		t.Fatalf("complete enrollment audit exited %d", code)
	}
	want := "audit-enrollment: complete=true vm_scan_complete=true evidence=2\n" +
		"provenance: allocation a1 (vm) on pve1, VM 4626\n" +
		"provenance: allocation a1 (volume) on pve1, VM 4626, volume nas:vm-4626-disk-0\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
	out.Reset()
	incomplete := handlers.StorageAllocationAudit{Complete: false, VMScanComplete: false, Issues: []string{"visibility unproven"}, VMScanIssues: []string{"visibility unproven"}}
	if code := writeStorageJournalEnrollment(&out, &stderr, incomplete, true); code != 1 {
		t.Fatalf("incomplete enrollment audit exited %d", code)
	}
	if out.String() != "audit-enrollment: complete=false vm_scan_complete=false evidence=0\nvm-scan issue: visibility unproven\n" {
		t.Fatalf("incomplete enrollment summary = %q", out.String())
	}
}

// One fixture runs in both modes, so the text summary cannot drift from the
// JSON report's exit-code rule.
func TestStorageJournalSummaryAndJSONShareExitCodes(t *testing.T) {
	f := newStorageJournalFixture(t)
	compare := func(stage string, actions ...string) {
		t.Helper()
		for _, action := range actions {
			jsonCode, jsonOut := f.run(action)
			textCode, textOut := f.run(action, "--summary")
			if jsonCode != textCode {
				t.Fatalf("%s %s: JSON exited %d, summary exited %d\n%s\n%s", stage, action, jsonCode, textCode, jsonOut, textOut)
			}
			if !strings.HasPrefix(jsonOut, "{") || strings.HasPrefix(textOut, "{") || !strings.HasPrefix(textOut, action+": complete=") {
				t.Fatalf("%s %s: output modes not honored\n%s\n%s", stage, action, jsonOut, textOut)
			}
		}
	}
	compare("healthy", "audit", "audit-enrollment")
	if code, out := f.run("audit", "--summary"); code != 0 {
		t.Fatalf("healthy summary exited %d: %s", code, out)
	}
	journalFixtureVM(t, f)
	f.restricted.Store(true)
	compare("restricted", "audit", "audit-enrollment")
	if code, out := f.run("audit", "--summary"); code != 1 || !strings.Contains(out, "vm-scan issue: ") {
		t.Fatalf("restricted summary exited %d: %s", code, out)
	}
	f.restricted.Store(false)
	if err := os.Remove(journalFixtureFile(t, f.directory, "index.json")); err != nil {
		t.Fatal(err)
	}
	compare("index lost", "audit")
	if code, out := f.run("audit", "--summary"); code != 1 || !strings.Contains(out, "generation_index_healthy=false") {
		t.Fatalf("index-lost summary exited %d: %s", code, out)
	}
}
