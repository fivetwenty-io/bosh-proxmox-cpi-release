package handlers

import (
	"context"
	"fmt"
	"slices"
	"strconv"
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

// storageAuditGateListLimit caps the findings one gate error lists in full.
// The remainder names what it left out, the full report goes to the log, and
// the audit command prints it on demand.
const storageAuditGateListLimit = 3

// StorageAuditRunbook points an operator at the runbook section for audit
// refusals. The docs do not ship in the release, so it names the repository
// as well as the file, and a test pins the heading it quotes.
const StorageAuditRunbook = `see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// storageAuditGateFallbackHint stands in for the audit command when the CPI
// could not render the exact one at startup.
const storageAuditGateFallbackHint = "run storage-journal audit --summary as the journal owner on the host that runs this CPI"

func (g storageAuditGate) admits(report StorageAllocationAudit) bool {
	return (g&storageAuditGateVMScan == 0 || report.VMScanComplete) &&
		(g&storageAuditGateComplete == 0 || report.Complete && len(report.Issues) == 0) &&
		(g&storageAuditGateConflicts == 0 || len(report.Conflicts) == 0)
}

// storageAuditGateFailure is the part of a refused gate that names findings.
// Its text is the summary, the runbook pointer, and the hint, in that order,
// so the pasteable command stays last. StorageAllocationDecisionFailure reads
// summary so the storage-journal CLI can print it without the command that
// the CLI's own user just ran.
type storageAuditGateFailure struct {
	summary string
	hint    string
}

func (f *storageAuditGateFailure) Error() string {
	return f.summary + "; " + StorageAuditRunbook + "; " + f.hint
}

// storageAuditGateFinding is one finding a gate error may list, with the
// noun its kind is counted by.
type storageAuditGateFinding struct {
	noun, brief string
}

// storageAuditGateError returns nil when report passes gate. Otherwise it logs
// the full report once at Error level and returns a CloudError that names only
// the findings the gate refused on. The message leads with the operation, the
// counts, and the first finding's brief, because the Director's task list
// keeps only about the first 75 characters of a CPI error.
func storageAuditGateError(ctx context.Context, deps Deps, operation string, report StorageAllocationAudit, gate storageAuditGate) error {
	if gate.admits(report) {
		return nil
	}
	var counts []string
	var findings []storageAuditGateFinding
	add := func(noun string, briefs []string) {
		counts = append(counts, storageAuditCount(len(briefs), noun))
		for _, brief := range briefs {
			findings = append(findings, storageAuditGateFinding{noun: noun, brief: brief})
		}
	}
	if gate&(storageAuditGateConflicts|storageAuditGateComplete) != 0 && len(report.Conflicts) > 0 {
		briefs := make([]string, 0, len(report.Conflicts))
		for _, conflict := range report.Conflicts {
			briefs = append(briefs, report.brief(conflict))
		}
		add("audit conflict", briefs)
	}
	switch {
	case gate&storageAuditGateComplete != 0 && len(report.Issues) > 0:
		add("audit issue", report.Issues)
	case gate&storageAuditGateVMScan != 0 && !report.VMScanComplete && len(report.VMScanIssues) > 0:
		add("VM-scan issue", report.VMScanIssues)
	}
	summary := "audit incomplete without a recorded finding"
	if len(counts) > 0 {
		listed, hidden := storageAuditGateSelect(findings)
		summary = strings.Join(counts, ", ") + "; " + strings.Join(listed, "; ")
		if len(hidden) > 0 {
			summary += "; and " + storageAuditGateRemainder(hidden)
		}
	}
	hint := storageAuditGateFallbackHint
	if deps.StorageAuditCommand != "" {
		hint = "run " + storageAuditQuotedCommand(deps.StorageAuditCommand) + " for the full report"
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
		log.String("issues", log.ScrubMessage(strings.Join(report.Issues, " | "))),
		log.String("observed_moves", log.ScrubMessage(strings.Join(storageAuditMoveLines(report.ObservedMoves), " | "))))
	return cpierrors.WrapAs(&storageAuditGateFailure{summary: log.ScrubMessage(summary), hint: hint}, cpierrors.TypeCloud, operation+" refused")
}

// storageAuditGateSelect picks the findings a gate error lists in full. It
// fills the slots with at most one finding per subject first, so the several
// conflicts of one moved VM cannot crowd out another VM, and then with the
// findings it passed over. Both passes keep the report's order, so the first
// finding always leads. It returns the listed briefs in that order and the
// findings it left out.
func storageAuditGateSelect(findings []storageAuditGateFinding) ([]string, []storageAuditGateFinding) {
	picked := make([]bool, len(findings))
	taken := 0
	seen := map[string]bool{}
	for index, finding := range findings {
		if taken == storageAuditGateListLimit {
			break
		}
		subject := storageAuditBriefSubject(finding.brief)
		if subject != "" && seen[subject] {
			continue
		}
		seen[subject] = true
		picked[index] = true
		taken++
	}
	for index := range findings {
		if taken == storageAuditGateListLimit {
			break
		}
		if !picked[index] {
			picked[index] = true
			taken++
		}
	}
	var listed []string
	var hidden []storageAuditGateFinding
	for index, finding := range findings {
		if picked[index] {
			listed = append(listed, finding.brief)
		} else {
			hidden = append(hidden, finding)
		}
	}
	return listed, hidden
}

// storageAuditGateRemainder counts the findings a gate error left out by
// kind, in the order the counts use, and names the distinct subjects of each
// kind's hidden findings without a cap, as in "2 more audit conflicts (VM
// 7015, VM 7018), 1 more VM-scan issue".
func storageAuditGateRemainder(hidden []storageAuditGateFinding) string {
	var parts []string
	for start := 0; start < len(hidden); {
		noun := hidden[start].noun
		end := start
		var subjects []string
		for ; end < len(hidden) && hidden[end].noun == noun; end++ {
			if subject := storageAuditBriefSubject(hidden[end].brief); subject != "" && !slices.Contains(subjects, subject) {
				subjects = append(subjects, subject)
			}
		}
		part := storageAuditCount(end-start, "more "+noun)
		if len(subjects) > 0 {
			part += " (" + strings.Join(subjects, ", ") + ")"
		}
		parts = append(parts, part)
		start = end
	}
	return strings.Join(parts, ", ")
}

// storageAuditBriefSubject returns the VM or volume a brief leads with, such
// as "VM 123" or "volume a:123/vm-123-disk-0.qcow2", or "" when it leads with
// neither.
func storageAuditBriefSubject(brief string) string {
	kind, rest, _ := strings.Cut(brief, " ")
	name, _, _ := strings.Cut(rest, " ")
	switch {
	case name == "":
		return ""
	case kind == "VM":
		if _, err := strconv.Atoi(name); err == nil {
			return "VM " + name
		}
	case kind == "volume":
		return "volume " + strings.TrimRight(name, ",;")
	}
	return ""
}

// storageAuditQuotedCommand sets the rendered command off from the refusal in
// single quotes, or in double quotes when the command quotes one of its own
// arguments, so what sits between the quotes can always be pasted as is.
func storageAuditQuotedCommand(command string) string {
	if strings.Contains(command, "'") {
		return `"` + command + `"`
	}
	return "'" + command + "'"
}

// storageAuditMoveLines renders each accepted move as one line.
func storageAuditMoveLines(moves []StorageAllocationMove) []string {
	lines := make([]string, 0, len(moves))
	for _, move := range moves {
		lines = append(lines, move.String())
	}
	return lines
}

func storageAuditCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
