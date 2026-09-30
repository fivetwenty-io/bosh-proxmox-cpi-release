package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestDiagnosticJournalListsAuditFindings pins that the storage-plan
// diagnostic names the audit's findings, not only their counts, so an
// operator reading pve-cid output learns which VM or storage blocks
// allocation.
func TestDiagnosticJournalListsAuditFindings(t *testing.T) {
	deps, _, c := auditFixture(t)
	c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
	c.nodesRead.failure = errors.New("transport response secret-password")
	var out StoragePlanDiagnostic
	observeDiagnosticJournal(context.Background(), deps, []string{"pve1"}, &out)
	for _, want := range []string{
		"historical audit complete=false; conflicts=1; issues=1",
		"historical audit conflict: remote allocation " + findingAllocationID + " is missing from retained journal; audit required: observed VM 123 on pve1",
		`historical audit issue: storage "a" on node "pve1" could not be inspected: unclassified error`,
	} {
		if !slices.Contains(out.Findings, want) {
			t.Fatalf("diagnostic findings lack %q:\n%s", want, strings.Join(out.Findings, "\n"))
		}
	}
	if strings.Contains(strings.Join(out.Findings, "\n"), "secret-password") {
		t.Fatal("transport error text reached the diagnostic")
	}
}

func TestDiagnosticAuditFindingsCapEachList(t *testing.T) {
	report := StorageAllocationAudit{}
	for i := range 5 {
		report.Conflicts = append(report.Conflicts, fmt.Sprintf("conflict %d", i))
	}
	report.Issues = []string{"issue 0"}
	got := storageDiagnosticAuditFindings(report)
	want := []string{
		"historical audit complete=false; conflicts=5; issues=1",
		"historical audit conflict: conflict 0",
		"historical audit conflict: conflict 1",
		"historical audit conflict: conflict 2",
		"historical audit conflicts: 2 more not listed",
		"historical audit issue: issue 0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("findings = %q, want %q", got, want)
	}
}
