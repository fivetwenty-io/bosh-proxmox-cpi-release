package main

import (
	"fmt"
	"io"
	"strings"
)

// Severities of a doctor finding. A problem is a state the operator should
// fix, and it makes the run exit with a failure status. Advice is a
// recommendation or a deliberate setting the doctor lists and never counts as
// a failure.
const (
	haSeverityProblem = "problem"
	haSeverityAdvice  = "advice"
)

// The kinds of doctor finding. Each is a stable string that scripts can match.
const (
	haKindUnchecked           = "unchecked"
	haKindObserveOnly         = "observe_only"
	haKindNodeDown            = "node_down"
	haKindQuorumFragile       = "quorum_fragile"
	haKindQDeviceAdvice       = "qdevice_advice"
	haKindAdvice              = "advice"
	haKindCRSMode             = "crs_mode"
	haKindRuleProblem         = "rule_problem"
	haKindPinMissing          = "pin_missing"
	haKindAntiAffinitySkipped = "anti_affinity_skipped"
	haKindAntiAffinitySplit   = "anti_affinity_split"
	haKindAntiAffinityShort   = "anti_affinity_short"
	haKindCPUUnknown          = "cpu_unknown"
	haKindCPUHostMulti        = "cpu_host_multi_domain"
	haKindCPUDomainTooSmall   = "cpu_domain_too_small"
	haKindLocalUnprotected    = "local_unprotected"
	haKindReplicaOnPartner    = "replica_on_partner"
	haKindReplicaNotRecover   = "replica_not_recoverable"
	haKindReplicaAwaiting     = "replica_awaiting_sync"
	haKindReplicaJobStale     = "replica_job_stale"
	haKindReplicaJobFailing   = "replica_job_failing"
	haKindReplicaScheduleSkip = "replica_schedule_unjudged"
	haKindReplicaOrphanVolume = "replica_orphan_volume"
	haKindReplicaRecordedOrph = "replica_recorded_orphan"
	haKindReplicaSuperseded   = "replica_superseded_copy"
	haKindReplicaNoJournal    = "replica_journal_unconfigured"
	haKindHoldMaintenance     = "hold_blocks_maintenance"
	haKindHeadroomShort       = "headroom_short"
	haKindHeadroomUnavailable = "headroom_unavailable"
	haKindRuleUnmarked        = "rule_unmarked"
	haKindResourceIgnored     = "resource_ignored"
	haKindBirthTagStale       = "birth_tag_stale"
	haKindReaddTag            = "readd_tag"
	haKindManagedNoResource   = "managed_without_resource"
	haKindOffModeResource     = "off_mode_resource"
	haKindOffModeRule         = "off_mode_rule"
	haKindGuestsNotListed     = "guests_not_enumerated"
)

// haFinding is one thing the doctor found.
type haFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Subject  string `json:"subject,omitempty"`
	Detail   string `json:"detail"`
}

// haNode is the doctor's line for one cluster node.
type haNode struct {
	Name        string `json:"name"`
	Online      bool   `json:"online"`
	Votes       *int   `json:"votes,omitempty"`
	Health      string `json:"health,omitempty"`
	Placeable   bool   `json:"placeable"`
	Maintenance bool   `json:"maintenance"`
	Watchdog    string `json:"watchdog,omitempty"`
	Version     string `json:"version,omitempty"`
	CPU         string `json:"cpu,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// haDomain is one CPU migration domain.
type haDomain struct {
	Model string   `json:"model"`
	Flags []string `json:"flags,omitempty"`
	Nodes []string `json:"nodes"`
}

// haReport is the doctor's report on one cluster.
type haReport struct {
	// Cluster names the cpi_config entry the report is about. It is empty for a
	// single-cluster run.
	Cluster         string      `json:"cluster,omitempty"`
	Profile         string      `json:"profile"`
	Mode            string      `json:"mode"`
	Effective       string      `json:"effective"`
	Reason          string      `json:"reason,omitempty"`
	Headroom        string      `json:"headroom"`
	Nodes           []haNode    `json:"nodes"`
	CPUDomains      []haDomain  `json:"cpu_domains"`
	UnknownCPUNodes []string    `json:"unknown_cpu_nodes,omitempty"`
	Findings        []haFinding `json:"findings"`
}

// add records a finding.
func (r *haReport) add(kind, severity, subject, format string, args ...any) {
	r.Findings = append(r.Findings, haFinding{
		Kind: kind, Severity: severity, Subject: subject, Detail: fmt.Sprintf(format, args...),
	})
}

// problem records a finding that fails the run.
func (r *haReport) problem(kind, subject, format string, args ...any) {
	r.add(kind, haSeverityProblem, subject, format, args...)
}

// advise records a finding that never fails the run.
func (r *haReport) advise(kind, subject, format string, args ...any) {
	r.add(kind, haSeverityAdvice, subject, format, args...)
}

// unchecked records a source the doctor could not read, which fails the run
// because the report cannot show the state the source holds is healthy.
func (r *haReport) unchecked(source, format string, args ...any) {
	r.problem(haKindUnchecked, source, format, args...)
}

// Problems counts the findings that fail the run.
func (r *haReport) Problems() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == haSeverityProblem {
			n++
		}
	}
	return n
}

// Has reports whether the report holds a finding of kind.
func (r *haReport) Has(kind string) bool {
	for _, f := range r.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// writeText prints the report, indenting every line by indent.
func (r *haReport) writeText(w io.Writer, indent string) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, indent+format+"\n", args...) }
	p("%s", r.Profile)
	p("nodes:")
	for _, n := range r.Nodes {
		p("  %s", nodeLine(n))
	}
	p("cpu migration domains:")
	if len(r.CPUDomains) == 0 {
		p("  none read")
	}
	for i, d := range r.CPUDomains {
		flags := "no flags"
		if len(d.Flags) > 0 {
			flags = "flags " + strings.Join(d.Flags, ",")
		}
		p("  domain %d: %s (%s) on %s", i+1, d.Model, flags, strings.Join(d.Nodes, ", "))
	}
	if len(r.UnknownCPUNodes) > 0 {
		p("  CPU model unreadable on %s", strings.Join(r.UnknownCPUNodes, ", "))
	}
	p("findings:")
	if len(r.Findings) == 0 {
		p("  none")
	}
	for _, f := range r.Findings {
		subject := ""
		if f.Subject != "" {
			subject = " " + f.Subject
		}
		p("  [%s] %s%s: %s", f.Severity, f.Kind, subject, f.Detail)
	}
	p("result: %d problem(s), %d advice", r.Problems(), len(r.Findings)-r.Problems())
}

func nodeLine(n haNode) string {
	state := "offline"
	if n.Online {
		state = "online"
	}
	parts := []string{n.Name, state}
	if n.Votes != nil {
		parts = append(parts, fmt.Sprintf("votes=%d", *n.Votes))
	}
	if n.Health != "" {
		parts = append(parts, "health="+n.Health)
	}
	if n.Health != "" {
		parts = append(parts, "placeable="+yesNo(n.Placeable))
	}
	if n.Maintenance {
		parts = append(parts, "maintenance=yes")
	}
	if n.Watchdog != "" {
		parts = append(parts, "watchdog="+n.Watchdog)
	}
	if n.Version != "" {
		parts = append(parts, "pve="+n.Version)
	}
	if n.CPU != "" {
		parts = append(parts, "cpu="+n.CPU)
	}
	if n.Reason != "" {
		parts = append(parts, "("+n.Reason+")")
	}
	return strings.Join(parts, " ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
