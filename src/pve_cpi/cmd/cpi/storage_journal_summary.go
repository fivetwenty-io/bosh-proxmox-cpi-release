package main

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
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
// jq: an overview line, one line per finding, the runbook pointer when the
// audit raised a conflict or an issue, one record line for each record that
// needs an operator with its evidence lines under it, the charging summary,
// and the skipped disabled storages. Every line is flattened, so a finding
// that carries a line break or a terminal escape still prints as one line.
func writeStorageJournalAuditText(w io.Writer, output storageJournalAuditReport) error {
	audit := output.Audit
	lines := []string{fmt.Sprintf("audit: complete=%t vm_scan_complete=%t generation_index_healthy=%t cluster_continuity=%t records=%d",
		audit.Complete, audit.VMScanComplete, output.IndexHealthy, output.ClusterContinuity, len(output.Records))}
	if output.IndexFinding != "" {
		lines = append(lines, "generation index: "+output.IndexFinding)
	}
	lines = append(lines, storageJournalFindingLines(audit)...)
	if len(audit.Conflicts) > 0 || len(audit.Issues) > 0 {
		lines = append(lines, "runbook: "+handlers.StorageAuditRunbook)
	}
	lines = append(lines, storageJournalAttentionLines(output.Attention, audit.Evidence)...)
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
// VM scan incomplete, then every other issue, then the moves the audit
// accepted. Issues holds the VM-scan issues too, so they are printed once,
// under their own prefix.
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
	for _, move := range report.ObservedMoves {
		lines = append(lines, "observed move: "+move.String())
	}
	return lines
}

// storageJournalAttention is one record the text summary prints in full.
type storageJournalAttention struct {
	ID, Kind, State, CID, Reason string
	Charging                     bool
}

// storageJournalAttentionRecords picks the records that need an operator.
// Those are records in any state other than ready_to_return, adopted,
// cleaned, or deleted, and any record that charges bytes against new creates.
// A healthy record is left out, so the summary stays short. The records come
// back sorted by ID.
func storageJournalAttentionRecords(report handlers.StorageAllocationAudit) []storageJournalAttention {
	records := make([]storageJournalAttention, 0, len(report.Records))
	for i := range report.Records {
		r := &report.Records[i]
		charging := handlers.StorageAllocationCharging(r.State)
		switch r.State {
		case aj.ReadyToReturn, aj.Adopted, aj.Cleaned, aj.Deleted:
			if !charging {
				continue
			}
		}
		records = append(records, storageJournalAttention{ID: r.ID, Kind: r.Kind, State: string(r.State), CID: r.CID, Reason: r.Reason, Charging: charging})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records
}

// storageJournalAttentionLines prints one record line per record that needs
// an operator, followed by one evidence line for each volume, VM, or parker
// the audit found still carrying that allocation. The CID is printed in full,
// because adopt needs it exactly. The reason is the record's own, scrubbed and
// flattened like every other line of the summary.
func storageJournalAttentionLines(records []storageJournalAttention, evidence []handlers.StorageAllocationEvidence) []string {
	var lines []string
	for _, record := range records {
		cid := record.CID
		if cid == "" {
			cid = "none"
		}
		reason := "none"
		if record.Reason != "" {
			reason = fmt.Sprintf("%q", boundStorageJournalText(record.Reason))
		}
		lines = append(lines, fmt.Sprintf("record: id=%s kind=%s state=%s charging=%t cid=%s reason=%s", record.ID, record.Kind, record.State, record.Charging, cid, reason))
		var own []handlers.StorageAllocationEvidence
		for _, item := range evidence {
			if item.AllocationID == record.ID {
				own = append(own, item)
			}
		}
		sort.Slice(own, func(i, j int) bool {
			a, b := own[i], own[j]
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			if a.Node != b.Node {
				return a.Node < b.Node
			}
			if a.VolumeID != b.VolumeID {
				return a.VolumeID < b.VolumeID
			}
			return a.VMID < b.VMID
		})
		for _, item := range own {
			lines = append(lines, "evidence: "+storageJournalAttentionEvidence(item))
		}
	}
	return lines
}

// storageJournalAttentionEvidence names what still carries an allocation: the
// kind of evidence, the volume, the node, and the VMID that holds it.
func storageJournalAttentionEvidence(item handlers.StorageAllocationEvidence) string {
	volume, node, holder := item.VolumeID, item.Node, "none"
	if volume == "" {
		volume = "none"
	}
	if node == "" {
		node = "none"
	}
	if item.VMID != 0 {
		holder = fmt.Sprint(item.VMID)
	}
	return fmt.Sprintf("allocation=%s kind=%s volume=%s node=%s holder_vmid=%s", item.AllocationID, item.Kind, volume, node, holder)
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
