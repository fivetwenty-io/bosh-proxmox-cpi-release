package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// writeStorageJournalEnrollment prints the enrollment audit as JSON, or as a
// text summary when textSummary is set. Both modes exit 1 when the audit or
// its VM scan is incomplete, and 0 otherwise.
func writeStorageJournalEnrollment(stdout, stderr io.Writer, report handlers.StorageAllocationAudit, textSummary bool) int {
	var err error
	if textSummary {
		err = writeStorageJournalEnrollmentText(stdout, report)
	} else {
		err = json.NewEncoder(stdout).Encode(report)
	}
	if err != nil {
		storageJournalFail(stderr, "audit-enrollment output could not be written", err)
		return 1
	}
	if !report.Complete || !report.VMScanComplete {
		return 1
	}
	return 0
}

// writeStorageJournalAuditText prints the audit for a Director that has no
// jq: an overview line, one line per finding, the charging summary, and the
// skipped disabled storages. Every line is flattened, so a finding that
// carries a line break or a terminal escape still prints as one line.
func writeStorageJournalAuditText(w io.Writer, output storageJournalAuditReport) error {
	audit := output.Audit
	lines := []string{fmt.Sprintf("audit: complete=%t vm_scan_complete=%t generation_index_healthy=%t cluster_continuity=%t records=%d",
		audit.Complete, audit.VMScanComplete, output.IndexHealthy, output.ClusterContinuity, len(output.Records))}
	if output.IndexFinding != "" {
		lines = append(lines, "generation index: "+output.IndexFinding)
	}
	lines = append(lines, storageJournalFindingLines(audit)...)
	lines = append(lines, storageJournalChargingLine(output.ChargingSummary))
	lines = append(lines, storageJournalSkippedLines(audit)...)
	return writeStorageJournalLines(w, lines)
}

// writeStorageJournalEnrollmentText prints the enrollment audit, including
// one line per existing provenance entry, because existing provenance is
// what makes initialize refuse.
func writeStorageJournalEnrollmentText(w io.Writer, report handlers.StorageAllocationAudit) error {
	findings := storageJournalFindingLines(report)
	lines := make([]string, 0, 2+len(findings)+len(report.Evidence))
	lines = append(lines, fmt.Sprintf("audit-enrollment: complete=%t vm_scan_complete=%t evidence=%d",
		report.Complete, report.VMScanComplete, len(report.Evidence)))
	lines = append(lines, findings...)
	for _, evidence := range report.Evidence {
		lines = append(lines, "provenance: "+storageJournalEvidenceText(evidence))
	}
	lines = append(lines, storageJournalSkippedLines(report)...)
	return writeStorageJournalLines(w, lines)
}

// storageJournalFindingLines lists conflicts, then the issues that left the
// VM scan incomplete, then every other issue. Issues holds the VM-scan
// issues too, so they are printed once, under their own prefix.
func storageJournalFindingLines(report handlers.StorageAllocationAudit) []string {
	var lines []string
	for _, conflict := range report.Conflicts {
		lines = append(lines, "conflict: "+conflict)
	}
	for _, issue := range report.VMScanIssues {
		lines = append(lines, "vm-scan issue: "+issue)
	}
	for _, issue := range report.Issues {
		if !slices.Contains(report.VMScanIssues, issue) {
			lines = append(lines, "issue: "+issue)
		}
	}
	return lines
}

func storageJournalChargingLine(summary storageJournalChargingSummary) string {
	if summary.Count == 0 {
		return "charging: none"
	}
	noun := "records"
	if summary.Count == 1 {
		noun = "record"
	}
	return fmt.Sprintf("charging: %d %s, oldest %s opened %s ago", summary.Count, noun, summary.OldestID, summary.OldestAge)
}

func storageJournalSkippedLines(report handlers.StorageAllocationAudit) []string {
	if len(report.SkippedDisabledStorages) == 0 {
		return nil
	}
	return []string{"skipped disabled storages: " + strings.Join(report.SkippedDisabledStorages, ", ")}
}

func storageJournalEvidenceText(evidence handlers.StorageAllocationEvidence) string {
	text := fmt.Sprintf("allocation %s (%s)", evidence.AllocationID, evidence.Kind)
	if evidence.Node != "" {
		text += " on " + evidence.Node
	}
	if evidence.VMID != 0 {
		text += fmt.Sprintf(", VM %d", evidence.VMID)
	}
	if evidence.VolumeID != "" {
		text += ", volume " + evidence.VolumeID
	}
	return text
}

// writeStorageJournalLines writes lines in one call, so a failed write is
// reported once rather than after a partial report.
func writeStorageJournalLines(w io.Writer, lines []string) error {
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(storageJournalLine(line))
		text.WriteByte('\n')
	}
	_, err := io.WriteString(w, text.String())
	return err
}
