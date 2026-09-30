package handlers

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// storageAuditGate names the report fields an audit-gated operation requires.
type storageAuditGate uint8

const (
	// storageAuditGateVMScan requires a complete VM scan.
	storageAuditGateVMScan storageAuditGate = 1 << iota
	// storageAuditGateComplete requires a complete audit with no issues.
	// Every issue clears Complete, so this also covers the VM scan's issues.
	storageAuditGateComplete
	// storageAuditGateConflicts requires no conflicts.
	storageAuditGateConflicts

	storageAuditGateAll = storageAuditGateVMScan | storageAuditGateComplete | storageAuditGateConflicts
)

// storageAuditGateListLimit caps the findings one gate error names. The full
// report goes to the log, and the audit command prints it on demand.
const storageAuditGateListLimit = 3

// storageAuditGateFallbackHint stands in for the audit command when the CPI
// could not render the exact one at startup.
const storageAuditGateFallbackHint = "run storage-journal audit --summary as the journal owner on the host that runs this CPI"

func (g storageAuditGate) admits(report StorageAllocationAudit) bool {
	return (g&storageAuditGateVMScan == 0 || report.VMScanComplete) &&
		(g&storageAuditGateComplete == 0 || report.Complete && len(report.Issues) == 0) &&
		(g&storageAuditGateConflicts == 0 || len(report.Conflicts) == 0)
}

// storageAuditGateFailure is the part of a refused gate that names findings.
// StorageAllocationDecisionFailure reads summary so the storage-journal CLI can
// print it without the command that the CLI's own user just ran.
type storageAuditGateFailure struct {
	summary string
	hint    string
}

func (f *storageAuditGateFailure) Error() string { return f.summary + "; " + f.hint }

// storageAuditGateError returns nil when report passes gate. Otherwise it logs
// the full report once at Error level and returns a CloudError that names only
// the findings the gate refused on. The message leads with the operation, the
// counts, and the first finding's brief, because the Director's task list
// keeps only about the first 75 characters of a CPI error.
func storageAuditGateError(ctx context.Context, deps Deps, operation string, report StorageAllocationAudit, gate storageAuditGate) error {
	if gate.admits(report) {
		return nil
	}
	var counts, findings []string
	if gate&(storageAuditGateConflicts|storageAuditGateComplete) != 0 && len(report.Conflicts) > 0 {
		counts = append(counts, storageAuditCount(len(report.Conflicts), "audit conflict"))
		for _, conflict := range report.Conflicts {
			findings = append(findings, report.brief(conflict))
		}
	}
	switch {
	case gate&storageAuditGateComplete != 0 && len(report.Issues) > 0:
		counts = append(counts, storageAuditCount(len(report.Issues), "audit issue"))
		findings = append(findings, report.Issues...)
	case gate&storageAuditGateVMScan != 0 && !report.VMScanComplete && len(report.VMScanIssues) > 0:
		counts = append(counts, storageAuditCount(len(report.VMScanIssues), "VM-scan issue"))
		findings = append(findings, report.VMScanIssues...)
	}
	summary := "audit incomplete without a recorded finding"
	if len(counts) > 0 {
		listed := findings[:min(len(findings), storageAuditGateListLimit)]
		summary = strings.Join(counts, ", ") + "; " + strings.Join(listed, "; ")
		if more := len(findings) - len(listed); more > 0 {
			summary += fmt.Sprintf("; and %d more", more)
		}
	}
	hint := storageAuditGateFallbackHint
	if deps.StorageAuditCommand != "" {
		hint = "run '" + deps.StorageAuditCommand + "' for the full report"
	}
	deps.Log(ctx).Error("allocation audit gate refused",
		log.String("operation", operation),
		log.Bool("vm_scan_complete", report.VMScanComplete),
		log.Bool("complete", report.Complete),
		log.Int("conflict_count", len(report.Conflicts)),
		log.Int("vm_scan_issue_count", len(report.VMScanIssues)),
		log.Int("issue_count", len(report.Issues)),
		log.String("conflicts", log.ScrubMessage(strings.Join(report.Conflicts, " | "))),
		log.String("vm_scan_issues", log.ScrubMessage(strings.Join(report.VMScanIssues, " | "))),
		log.String("issues", log.ScrubMessage(strings.Join(report.Issues, " | "))))
	return cpierrors.WrapAs(&storageAuditGateFailure{summary: log.ScrubMessage(summary), hint: hint}, cpierrors.TypeCloud, operation+" refused")
}

func storageAuditCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
