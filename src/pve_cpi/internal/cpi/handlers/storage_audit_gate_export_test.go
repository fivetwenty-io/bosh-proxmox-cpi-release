package handlers

import (
	"context"
	"errors"
	"testing"
)

func TestStorageAuditFindingSummaryMatchesTheCompleteGate(t *testing.T) {
	if got := StorageAuditFindingSummary(StorageAllocationAudit{Complete: true, VMScanComplete: true}); got != "" {
		t.Fatalf("a clean audit produced a summary: %q", got)
	}
	report := gateTestReport()
	err := storageAuditGateError(context.Background(), Deps{}, "storage-journal", report, storageAuditGateAll)
	var gate *storageAuditGateFailure
	if !errors.As(err, &gate) {
		t.Fatalf("gate error %v carries no findings", err)
	}
	if got := StorageAuditFindingSummary(report); got != gate.summary {
		t.Fatalf("summary = %q, want the gate's %q", got, gate.summary)
	}
	const want = "2 audit conflicts, 2 audit issues; VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch); VM 7014 on pvupvecf103, recorded pvupvecf102 (node_mismatch); some cluster nodes could not be inspected: pvupvecf104 (reported offline by /cluster/status); and 1 more"
	if got := StorageAuditFindingSummary(report); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	incomplete := StorageAllocationAudit{Complete: false, VMScanComplete: true}
	if got := StorageAuditFindingSummary(incomplete); got != "audit incomplete without a recorded finding" {
		t.Fatalf("an incomplete audit without findings summarized as %q", got)
	}
}
