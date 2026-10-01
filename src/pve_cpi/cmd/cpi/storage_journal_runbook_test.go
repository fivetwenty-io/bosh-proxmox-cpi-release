package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// storageJournalTestRunbook is the pointer to the runbook section for audit
// refusals, as an operator reads it.
const storageJournalTestRunbook = `see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// TestStorageJournalAuditSummaryNamesTheRunbook pins the runbook line right
// after the findings when the audit raised a conflict or an issue, and its
// absence for a clean audit and for one that only accepted moves.
func TestStorageJournalAuditSummaryNamesTheRunbook(t *testing.T) {
	const runbook = "runbook: " + storageJournalTestRunbook
	for _, tc := range []struct {
		name   string
		report handlers.StorageAllocationAudit
		after  string
	}{
		{"conflict", handlers.StorageAllocationAudit{Complete: false, VMScanComplete: true, Conflicts: []string{"remote allocation x (VM 4626) is outside recorded mutation targets"}},
			"conflict: remote allocation x (VM 4626) is outside recorded mutation targets"},
		{"issue", handlers.StorageAllocationAudit{Complete: false, VMScanComplete: true, Issues: []string{`storage "nas" on node "pve1" could not be inspected: context deadline exceeded`}},
			`issue: storage "nas" on node "pve1" could not be inspected: context deadline exceeded`},
		{"clean", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}, ""},
		{"observed moves only", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, ObservedMoves: []handlers.StorageAllocationMove{
			{AllocationID: "180f7d1e", Kind: "vm", VMID: 4626, RecordedNodes: []string{"pvupvecf101"}, ObservedNode: "pvupvecf102"},
		}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			writeStorageJournalAudit(&out, &stderr, tc.report, nil, true, true)
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if tc.after == "" {
				if strings.Contains(out.String(), "runbook:") {
					t.Fatalf("summary %q names the runbook without a finding", out.String())
				}
				return
			}
			for i, line := range lines {
				if line == tc.after {
					if i+1 >= len(lines) || lines[i+1] != runbook {
						t.Fatalf("summary %q lacks %q right after its findings", out.String(), runbook)
					}
					return
				}
			}
			t.Fatalf("summary %q lacks the finding %q", out.String(), tc.after)
		})
	}
}

// TestStorageJournalRefusalNamesTheRunbookOnItsAuditFindings pins the runbook
// pointer on the "audit findings:" line of a refusal.
func TestStorageJournalRefusalNamesTheRunbookOnItsAuditFindings(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: false, VMScanComplete: true, Issues: []string{`storage "nas" on node "pve1" could not be inspected: context deadline exceeded`}}
	var stderr bytes.Buffer
	storageJournalRefuse(&stderr, "recovery requires a complete consistent historical audit", storageJournalAuditPreconditions(report), report)
	want := `audit findings: 1 audit issue; storage "nas" on node "pve1" could not be inspected: context deadline exceeded; ` + storageJournalTestRunbook + "\n"
	if !strings.HasSuffix(stderr.String(), want) {
		t.Fatalf("refusal = %q, want it to end with %q", stderr.String(), want)
	}
}
