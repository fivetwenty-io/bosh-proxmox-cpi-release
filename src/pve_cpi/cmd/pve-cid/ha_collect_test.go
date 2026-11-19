package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func runCollect(t *testing.T, f *fakeHA, cfg *config.CPIConfig) *haReport {
	t.Helper()
	return collectHA(context.Background(), f, cfg, haDirector, haNow)
}

// findings returns the findings of kind.
func findings(r *haReport, kind string) []haFinding {
	var out []haFinding
	for _, f := range r.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// want fails unless the report holds exactly one finding of kind, with the
// severity and a detail that contains each of the fragments, and returns it.
func want(t *testing.T, r *haReport, kind, severity string, fragments ...string) {
	t.Helper()
	got := findings(r, kind)
	if len(got) != 1 {
		t.Fatalf("findings of kind %s = %d, want 1; all findings: %+v", kind, len(got), r.Findings)
	}
	if got[0].Severity != severity {
		t.Errorf("%s severity = %s, want %s", kind, got[0].Severity, severity)
	}
	for _, frag := range fragments {
		if !strings.Contains(got[0].Detail+" "+got[0].Subject, frag) {
			t.Errorf("%s = %+v, want it to mention %q", kind, got[0], frag)
		}
	}
}

func wantNone(t *testing.T, r *haReport, kinds ...string) {
	t.Helper()
	for _, kind := range kinds {
		if got := findings(r, kind); len(got) != 0 {
			t.Errorf("unexpected %s findings: %+v", kind, got)
		}
	}
}

func localVM(f *fakeHA, node, tags string) {
	const vmid = 101
	f.guests = append(f.guests, pve.GuestRef{VMID: vmid, Node: node, Name: "vm", Tags: tags, Status: "running"})
	f.memory[vmid] = 4 * haGiB
	f.configs[vmid] = map[string]any{"cpu": "x86-64-v2-AES", "scsi0": "local-zfs:vm-" + itoa(vmid) + "-disk-0,size=10G"}
}

func itoa(n int) string { return strconv.Itoa(n) }

func pinRule(vmid int, comment string, nodes ...string) ha.Rule {
	r := ha.Rule{
		ID: "bosh-na-" + itoa(vmid), Type: ha.RuleTypeNodeAffinity, Strict: true, Comment: comment,
		Nodes: map[string]int{}, Resources: []string{"vm:" + itoa(vmid)},
	}
	for _, n := range nodes {
		r.Nodes[n] = 0
	}
	return r
}

func TestCollectHealthyClusterReportsNothing(t *testing.T) {
	t.Parallel()
	r := runCollect(t, haBaseFake(), haCfg("auto", "none"))
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
	if !strings.HasPrefix(r.Headroom, "ok(") || !strings.Contains(r.Profile, "headroom="+r.Headroom) {
		t.Fatalf("headroom = %q, profile = %q", r.Headroom, r.Profile)
	}
	if r.Mode != "auto" || r.Effective != "full" || len(r.Nodes) != 3 || len(r.CPUDomains) != 1 {
		t.Fatalf("header = %+v", r)
	}
	if r.Nodes[0].Health != "online" || !r.Nodes[0].Placeable || r.Nodes[0].Votes == nil || *r.Nodes[0].Votes != 1 {
		t.Fatalf("node line = %+v", r.Nodes[0])
	}
}

func TestCollectProfileUnreadableIsUnchecked(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile, f.profileErr = nil, errFake
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "profile", "fake read failure")
	if r.Problems() != 1 || r.Headroom != "unavailable" {
		t.Fatalf("report = %+v", r)
	}
}

func TestCollectObserveOnlyNamesTheReasonAndGaps(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Effective = ha.EffectiveObserveOnly
	f.profile.Reason = "pve-manager 8.4 is below the HA floor"
	f.profile.Gaps = []string{"pve-manager 8.4 is below the HA floor", "no QDevice"}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindObserveOnly, haSeverityProblem, "observe-only", "below the HA floor", "no QDevice")
}

func TestCollectNodeDownAndQuorumFragile(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Health.Nodes[2] = pve.NodeHealthEntry{
		Name: "n3", Health: pve.NodeHealthDead, DeadOrFenced: true, Reason: "offline in cluster status",
	}
	f.profile.Nodes[2].Online = false
	f.profile.Quorum = ha.Quorum{Known: true, Expected: 3, Participating: 2, SurvivesOne: true, FragileNodes: []string{"n1", "n2"}}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindNodeDown, haSeverityProblem, "n3", "offline in cluster status")
	got := findings(r, haKindQuorumFragile)
	if len(got) != 2 || got[0].Subject != "n1" || got[1].Subject != "n2" {
		t.Fatalf("quorum findings = %+v, want n1 and n2", got)
	}
	if !strings.Contains(r.Nodes[2].Health, "dead") || r.Nodes[2].Online {
		t.Fatalf("down node line = %+v", r.Nodes[2])
	}
}

func TestCollectNodeHealthUnbuiltIsUnchecked(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Health, f.profile.HealthErr = nil, "cluster status unreadable"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "node-health", "cluster status unreadable")
}

func TestCollectQDeviceAndPlainAdviceNeverFail(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Advice = "add-qdevice: the cluster has 4 voting nodes"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindQDeviceAdvice, haSeverityAdvice, "add-qdevice", "breaks an even split")
	if r.Problems() != 0 {
		t.Fatalf("problems = %d, want 0", r.Problems())
	}
	f.profile.Advice = "the CRM is idle until a resource exists"
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAdvice, haSeverityAdvice, "CRM is idle")
	wantNone(t, r, haKindQDeviceAdvice)
}

func TestCollectOddNodeCountGetsNoQDeviceAdvice(t *testing.T) {
	t.Parallel()
	r := runCollect(t, haBaseFake(), haCfg("auto", "none"))
	wantNone(t, r, haKindQDeviceAdvice, haKindAdvice)
}

func TestCollectCRSModeAdviceUnderDLB(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.CRS = ha.CRS{Known: true, Mode: "basic"}
	cfg := haCfg("auto", "none")
	on := true
	cfg.HA.DLB = &config.HADLBConfig{Enabled: &on}
	r := runCollect(t, f, cfg)
	want(t, r, haKindCRSMode, haSeverityAdvice, "basic", "never writes CRS")
	f.profile.CRS = ha.CRS{Known: true, Mode: "static"}
	r = runCollect(t, f, cfg)
	want(t, r, haKindCRSMode, haSeverityAdvice, "ha-auto-rebalance is off")
	f.profile.CRS = ha.CRS{Known: true, Mode: "static", AutoRebalance: true}
	wantNone(t, runCollect(t, f, cfg), haKindCRSMode)
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindCRSMode)
}

func TestCollectRuleProblems(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	disabled := pinRule(100, haMarker, "n1")
	disabled.Disabled = true
	conflicting := ha.Rule{
		ID: "bosh-aa-0123456789ab", Type: "resource-affinity", Affinity: "negative", Comment: haMarker,
		Resources: []string{"vm:100", "vm:101"}, Errors: []string{"resources: vm:101 is already pinned"},
	}
	unmarked := ha.Rule{ID: "bosh-na-777", Type: ha.RuleTypeNodeAffinity, Strict: true}
	foreign := pinRule(888, "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000", "n1")
	foreign.Disabled = true
	operators := ha.Rule{ID: "operator-rule", Disabled: true}
	f.rules = []ha.Rule{disabled, conflicting, unmarked, foreign, operators}
	r := runCollect(t, f, haCfg("auto", "none"))
	got := findings(r, haKindRuleProblem)
	if len(got) != 2 || got[0].Subject != "bosh-aa-0123456789ab" || got[1].Subject != "bosh-na-100" {
		t.Fatalf("rule findings = %+v", got)
	}
	if !strings.Contains(got[0].Detail, "already pinned") || !strings.Contains(got[1].Detail, "disabled") {
		t.Fatalf("rule findings = %+v", got)
	}
	want(t, r, haKindRuleUnmarked, haSeverityProblem, "bosh-na-777", "older CPI")
	for _, f := range findings(r, haKindRuleUnmarked) {
		if strings.Contains(f.Detail, "will not edit") || strings.Contains(f.Detail, "moves this Director's members") {
			t.Fatalf("an unmarked pin's finding must not describe the anti-affinity migration: %+v", f)
		}
	}
}

// TestCollectUnmarkedAntiAffinityRuleNamesTheMigration reports an unmarked
// anti-affinity rule with what create_vm does to it, which is to move this
// Director's members out and leave every other Director's members in place.
func TestCollectUnmarkedAntiAffinityRuleNamesTheMigration(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.rules = []ha.Rule{{ID: "bosh-aa-web", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
		Resources: []string{"vm:100", "vm:101"}}}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindRuleUnmarked, haSeverityProblem, "bosh-aa-web", "older CPI")
	got := findings(r, haKindRuleUnmarked)
	if len(got) != 1 {
		t.Fatalf("unmarked findings = %+v", got)
	}
	for _, part := range []string{
		"next creates a VM in the group", "moves this Director's members", "leaves other Directors' members in place",
		"fewer than two members",
	} {
		if !strings.Contains(got[0].Detail, part) {
			t.Errorf("detail %q does not say %q", got[0].Detail, part)
		}
	}
	if strings.Contains(got[0].Detail, "will not edit") {
		t.Errorf("detail %q still claims the CPI will not edit the rule", got[0].Detail)
	}
}

func TestCollectRulesUnreadable(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.rulesErr = errFake
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "ha-rules")
	if r.Problems() == 0 {
		t.Fatalf("an unreadable rules list must fail the run: %+v", r.Findings)
	}

	// A cluster that reports no rule support reads the same: the listing
	// failed, so the rules are unchecked, in every mode.
	f.profile.RulesSupported = false
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "ha-rules", "PVE 9.0")
	wantNone(t, r, haKindAdvice)

	off := haBaseFake()
	off.profile = haHealthyProfile("off")
	off.profile.Effective = ha.EffectiveOff
	off.resources = nil
	off.rulesErr = errFake
	off.profile.RulesSupported = false
	r = runCollect(t, off, haCfg("off", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "ha-rules")
	if r.Problems() == 0 {
		t.Fatalf("mode off must still fail on an unreadable rules list: %+v", r.Findings)
	}
}

func TestCollectPinMissingForLocalDiskVM(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi;bosh-ha-managed")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker})
	r := runCollect(t, f, haCfg("auto", "hold"))
	want(t, r, haKindPinMissing, haSeverityProblem, "vm:101", "no confinement pin")

	for name, mutate := range map[string]func(*ha.Rule){
		"disabled":   func(p *ha.Rule) { p.Disabled = true },
		"not strict": func(p *ha.Rule) { p.Strict = false },
		"wrong vm":   func(p *ha.Rule) { p.Resources = []string{"vm:555"} },
		"wrong node": func(p *ha.Rule) { p.Nodes = map[string]int{"n2": 0} },
	} {
		pin := pinRule(101, haMarker, "n1")
		mutate(&pin)
		f.rules = []ha.Rule{pin}
		r = runCollect(t, f, haCfg("auto", "hold"))
		if len(findings(r, haKindPinMissing)) != 1 {
			t.Errorf("%s: findings = %+v, want a pin_missing finding", name, r.Findings)
		}
	}
	f.rules = []ha.Rule{pinRule(101, haMarker, "n1")}
	wantNone(t, runCollect(t, f, haCfg("auto", "hold")), haKindPinMissing)
}

func TestCollectAntiAffinityOverflowAndShortGroup(t *testing.T) {
	t.Parallel()
	group := "bosh-aa-0123456789ab"
	f := haBaseFake()
	f.guests = nil
	f.resources = nil
	for i := 0; i < 4; i++ {
		id := 200 + i
		f.guests = append(f.guests, pve.GuestRef{VMID: id, Node: "n" + itoa(i%3+1), Tags: "bosh-cpi;" + group})
		f.memory[id] = haGiB
	}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAntiAffinitySkipped, haSeverityAdvice, group, "4 members", "3 nodes", "no rule")

	f.rules = []ha.Rule{
		{ID: group, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:200", "vm:201", "vm:202"}},
		{ID: group + "-2", Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:203"}},
	}
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAntiAffinitySplit, haSeverityAdvice, group, "2 rules")
	wantNone(t, r, haKindAntiAffinitySkipped)

	// Three members fit the cluster; a member with a resource that its rule does
	// not name leaves the group short.
	f.guests = f.guests[:3]
	f.resources = []HAResourceRow{{SID: "vm:200", State: "started"}, {SID: "vm:201", State: "started"}, {SID: "vm:202", State: "started"}}
	f.rules = []ha.Rule{{ID: group, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:200", "vm:201"}}}
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAntiAffinityShort, haSeverityProblem, group, "vm:202")

	f.rules[0].Resources = []string{"vm:200", "vm:201", "vm:202"}
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindAntiAffinityShort, haKindAntiAffinitySplit)
}

func TestCollectAntiAffinitySplitNamesUncoveredMember(t *testing.T) {
	t.Parallel()
	group := "bosh-aa-0123456789ab"
	f := haBaseFake()
	f.guests = nil
	f.resources = nil
	for i := 0; i < 4; i++ {
		id := 200 + i
		f.guests = append(f.guests, pve.GuestRef{VMID: id, Node: "n" + itoa(i%3+1), Tags: "bosh-cpi;" + group})
		f.memory[id] = haGiB
		f.resources = append(f.resources, HAResourceRow{SID: "vm:" + itoa(id), State: "started"})
	}
	f.rules = []ha.Rule{
		{ID: group, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:200", "vm:201", "vm:202"}},
		{ID: group + "-2", Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:200"}},
	}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindAntiAffinitySplit, haSeverityAdvice, group, "vm:203 is in no anti-affinity rule for group "+group)
}

func TestCollectCPUFindings(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.UnknownCPUNodes = []string{"n3"}
	f.profile.CPUDomains = []ha.CPUDomain{
		{Model: "EPYC", Nodes: []string{"n1", "n2"}},
		{Model: "XEON", Nodes: []string{"n4"}},
	}
	f.configs[100]["cpu"] = "host"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindCPUUnknown, haSeverityAdvice, "n3")
	want(t, r, haKindCPUHostMulti, haSeverityProblem, "vm:100", "host", "2 CPU migration domains", "n1, n2", "bosh recreate")
	wantNone(t, r, haKindCPUDomainTooSmall)

	// A max-typed VM with no HA resource in a one-node domain got no HA.
	f2 := haBaseFake()
	f2.profile.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"n1"}}, {Model: "XEON", Nodes: []string{"n2", "n3"}}}
	f2.resources = nil
	f2.configs[100]["cpu"] = "max"
	r = runCollect(t, f2, haCfg("auto", "none"))
	want(t, r, haKindCPUDomainTooSmall, haSeverityProblem, "vm:100", "max", "1 node(s)")
	want(t, r, haKindCPUHostMulti, haSeverityAdvice, "vm:100")

	// An observe-only profile registers nothing, so the profile is the cause.
	obs := haBaseFake()
	obs.profile.Effective = ha.EffectiveObserveOnly
	obs.profile.CPUDomains = f2.profile.CPUDomains
	obs.resources = nil
	obs.configs[100]["cpu"] = "max"
	wantNone(t, runCollect(t, obs, haCfg("auto", "none")), haKindCPUDomainTooSmall)

	// A create still running has not been registered yet.
	young := haBaseFake()
	young.profile.CPUDomains = f2.profile.CPUDomains
	young.resources = nil
	young.configs[100]["cpu"] = "max"
	young.guests[0].Tags = "bosh-cpi;bosh-creating-" + itoa(int(haNow.Add(-time.Minute).Unix()))
	wantNone(t, runCollect(t, young, haCfg("auto", "none")), haKindCPUDomainTooSmall)

	// In mode off the CPI registers nothing, so a missing resource is expected.
	f2.profile.Mode, f2.profile.Effective = "off", ha.EffectiveOff
	wantNone(t, runCollect(t, f2, haCfg("off", "none")), haKindCPUDomainTooSmall)
}

func TestCollectLocalUnprotected(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n2", "bosh-cpi")
	r := runCollect(t, f, haCfg("auto", "hold"))
	want(t, r, haKindLocalUnprotected, haSeverityProblem, "vm:101", "local-zfs:vm-101-disk-0", "local_policy is hold")
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindLocalUnprotected, haSeverityAdvice, "local_policy is none")
	// A create still in flight is not yet expected to be under HA.
	f.guests[1].Tags = "bosh-cpi;bosh-creating-" + itoa(int(haNow.Add(-time.Minute).Unix()))
	wantNone(t, runCollect(t, f, haCfg("auto", "hold")), haKindLocalUnprotected, haKindBirthTagStale)
	// A CD-ROM on local storage is an ISO, not a disk.
	f.guests[1].Tags = "bosh-cpi"
	f.configs[101] = map[string]any{"ide2": "local-zfs:iso/stemcell.iso,media=cdrom"}
	wantNone(t, runCollect(t, f, haCfg("auto", "hold")), haKindLocalUnprotected)
}

func TestCollectLocalUnprotectedSkipsParker(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n2", "bosh-cpi")
	want(t, runCollect(t, f, haCfg("auto", "hold")), haKindLocalUnprotected, haSeverityProblem, "vm:101")
	f.guests[1].Tags = "bosh-cpi;" + pve.ParkerTag
	wantNone(t, runCollect(t, f, haCfg("auto", "hold")), haKindLocalUnprotected)
}

func maintenanceFake() *fakeHA {
	f := haBaseFake()
	f.profile.Health.Nodes[0] = pve.NodeHealthEntry{Name: "n1", Health: pve.NodeHealthMaintenance, Maintenance: true, ProxyTarget: true}
	return f
}

func TestCollectMaintenanceStranded(t *testing.T) {
	t.Parallel()
	f := maintenanceFake()
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindMaintenanceStranded, haSeverityProblem, "vm:100", "n1", "in maintenance", "can't restart")
	// A pin to a single node is named as the reason.
	f.rules = []ha.Rule{pinRule(100, haMarker, "n1")}
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindMaintenanceStranded, haSeverityProblem, "pin names no other node")
}

// TestCollectMaintenanceStrandedNamesASoftSinglePin: a soft pin whose top
// priority class holds only the draining node keeps the VM there, and the
// finding says so. A soft pin that ranks two nodes first does not.
func TestCollectMaintenanceStrandedNamesASoftSinglePin(t *testing.T) {
	t.Parallel()
	soft := func(prios map[string]int) ha.Rule {
		r := pinRule(100, haMarker)
		r.Strict, r.Nodes = false, prios
		return r
	}
	f := maintenanceFake()
	f.rules = []ha.Rule{soft(map[string]int{"n1": 2, "n2": 1, "n3": 1})}
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindMaintenanceStranded, haSeverityProblem,
		"soft node-affinity pin", "only n1", "highest priority class")
	f.rules = []ha.Rule{soft(map[string]int{"n1": 2, "n2": 2})}
	got := findings(runCollect(t, f, haCfg("auto", "none")), haKindMaintenanceStranded)
	if len(got) != 1 || strings.Contains(got[0].Detail, "soft node-affinity pin") {
		t.Errorf("a shared top class must give no soft-pin reason, got %+v", got)
	}
}

func TestCollectMaintenanceStrandedSkipsStoppedVM(t *testing.T) {
	t.Parallel()
	f := maintenanceFake()
	f.resources[0].State = "stopped"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindMaintenanceStranded)
}

func TestCollectMaintenanceStrandedSkipsNormalNode(t *testing.T) {
	t.Parallel()
	wantNone(t, runCollect(t, haBaseFake(), haCfg("auto", "none")), haKindMaintenanceStranded)
}

func TestCollectMaintenanceStrandedSkipsOutOfScope(t *testing.T) {
	t.Parallel()
	f := maintenanceFake()
	f.guests[0].Tags = "bosh-cpi;bosh-ha-managed;bosh-owner--bbbbbbbb-0000"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindMaintenanceStranded)
}

func TestCollectMaintenanceStrandedUnreadableStatus(t *testing.T) {
	t.Parallel()
	f := maintenanceFake()
	f.profile.Health, f.profile.HealthErr = nil, "cluster status unreadable"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "node-health", "cluster status unreadable")
	wantNone(t, r, haKindMaintenanceStranded)
}

func recoveryFake() *fakeHA {
	f := haBaseFake()
	f.states = map[string]string{"vm:100": "recovery"}
	return f
}

func TestCollectRecoveryStranded(t *testing.T) {
	t.Parallel()
	f := recoveryFake()
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindRecoveryStranded, haSeverityProblem, "vm:100", "n1", "waits in recovery", "no node it may run on")
	wantNone(t, r, haKindUnchecked)
	// A pin to the failed node alone is named as the reason.
	f.rules = []ha.Rule{pinRule(100, haMarker, "n1")}
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded, haSeverityProblem, "strict pin names only n1")
}

func TestCollectRecoveryStrandedSkipsStartedService(t *testing.T) {
	t.Parallel()
	f := recoveryFake()
	f.states["vm:100"] = "started"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded)
}

func TestCollectRecoveryStrandedSkipsOutOfScope(t *testing.T) {
	t.Parallel()
	f := recoveryFake()
	f.guests[0].Tags = "bosh-cpi;bosh-ha-managed;bosh-owner--bbbbbbbb-0000"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded)
}

func TestCollectRecoveryStrandedUnreadableStatus(t *testing.T) {
	t.Parallel()
	f := recoveryFake()
	f.states, f.statesErr = nil, errors.New("manager status unreadable")
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "ha-service-state", "manager status unreadable")
	wantNone(t, r, haKindRecoveryStranded)
}

// fencedRecoveryFake is a cluster where n1 is down and fenced. The per-node
// guest listing cannot ask n1, so VM 100 is missing from it, while the cluster
// resource list still names the VM on n1.
func fencedRecoveryFake() *fakeHA {
	f := recoveryFake()
	f.clusterVMs = append([]pve.GuestRef(nil), f.guests...)
	f.guests, f.excluded = nil, []string{"n1"}
	f.profile.Nodes[0].Online = false
	f.profile.Health.Nodes[0].Health = pve.NodeHealthDead
	return f
}

func TestCollectRecoveryStrandedOnOfflineNode(t *testing.T) {
	t.Parallel()
	f := fencedRecoveryFake()
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded, haSeverityProblem,
		"vm:100", "was on n1", "waits in recovery")
	// A VM of another Director on the offline node is out of scope.
	f.clusterVMs[0].Tags = "bosh-cpi;bosh-ha-managed;bosh-owner--bbbbbbbb-0000"
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded)
}

func TestCollectRecoveryStrandedAntiAffinityReason(t *testing.T) {
	t.Parallel()
	const group = "bosh-aa-0123456789ab"
	f := fencedRecoveryFake()
	f.clusterVMs[0].Tags += ";" + group
	f.clusterVMs = append(f.clusterVMs,
		pve.GuestRef{VMID: 101, Node: "n2", Name: "vm-101", Tags: "bosh-cpi;" + group, Status: "running"},
		pve.GuestRef{VMID: 102, Node: "n3", Name: "vm-102", Tags: "bosh-cpi;" + group, Status: "running"})
	f.rules = []ha.Rule{{
		ID: group, Type: ha.RuleTypeResourceAffinity, Affinity: "negative", Comment: haMarker,
		Resources: []string{"vm:100", "vm:101", "vm:102"},
	}}
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded, haSeverityProblem,
		"every other node holds a member of its anti-affinity group")
	// With n3 free of a member the reason is left out.
	f.clusterVMs = f.clusterVMs[:2]
	got := findings(runCollect(t, f, haCfg("auto", "none")), haKindRecoveryStranded)
	if len(got) != 1 || strings.Contains(got[0].Detail, "anti-affinity") {
		t.Errorf("want one finding without the anti-affinity reason, got %+v", got)
	}
}

func TestCollectHoldBlocksMaintenance(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n2", "bosh-cpi;bosh-ha-managed")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker})
	f.rules = []ha.Rule{pinRule(101, haMarker, "n2")}
	f.profile.Health.Nodes[1] = pve.NodeHealthEntry{Name: "n2", Health: pve.NodeHealthMaintenance, Maintenance: true, ProxyTarget: true}
	r := runCollect(t, f, haCfg("auto", "hold"))
	want(t, r, haKindHoldMaintenance, haSeverityProblem, "vm:101", "n2", "entering maintenance")
	if !r.Nodes[1].Maintenance {
		t.Fatalf("node line = %+v, want maintenance", r.Nodes[1])
	}
	// A replicate VM can move to its partner, so it does not block the drain.
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaPartnerKey+"=n3", "n2")}
	wantNone(t, runCollect(t, f, haCfg("auto", "replicate")), haKindHoldMaintenance)
}

// drainFake is the lab's shape. Node n2 is in maintenance and holds a hold VM
// under HA, whose pin names n2 alone, and a member of a full three-member
// negative anti-affinity group. The group has one member on each node, so the
// member on n2 has no node to move to.
func drainFake(holdState string) *fakeHA {
	const group = "bosh-aa-0123456789ab"
	f := haBaseFake()
	f.guests = nil
	f.resources = nil
	f.rules = nil
	for i, node := range []string{"n1", "n2", "n3"} {
		vmid := 117 + i*100
		f.guests = append(f.guests, pve.GuestRef{
			VMID: vmid, Node: node, Name: "member", Tags: "bosh-cpi;bosh-ha-managed;" + group, Status: "running",
		})
		f.memory[vmid] = 4 * haGiB
		f.configs[vmid] = map[string]any{"cpu": "x86-64-v2-AES", "scsi0": "nfs1:" + itoa(vmid) + "/vm-" + itoa(vmid) + "-disk-0.raw,size=10G"}
		f.resources = append(f.resources, HAResourceRow{SID: "vm:" + itoa(vmid), State: "started", Comment: haMarker})
	}
	f.rules = []ha.Rule{{
		ID: group, Type: ha.RuleTypeResourceAffinity, Affinity: "negative", Comment: haMarker,
		Resources: []string{"vm:117", "vm:217", "vm:317"},
	}}
	// The hold VM sits on n2 with a node-local volume and a pin to n2 alone.
	f.guests = append(f.guests, pve.GuestRef{
		VMID: 40, Node: "n2", Name: "hold", Tags: "bosh-cpi;bosh-ha-managed", Status: "running",
	})
	f.memory[40] = 4 * haGiB
	f.configs[40] = map[string]any{"cpu": "x86-64-v2-AES", "scsi0": "local-zfs:vm-40-disk-0,size=10G"}
	f.resources = append(f.resources, HAResourceRow{SID: "vm:40", State: holdState, Comment: haMarker})
	f.rules = append(f.rules, pinRule(40, haMarker, "n2"))
	f.clusterVMs = append([]pve.GuestRef(nil), f.guests...)
	f.profile.Health.Nodes[1] = pve.NodeHealthEntry{
		Name: "n2", Health: pve.NodeHealthMaintenance, Maintenance: true, ProxyTarget: true,
		Watchdog: pve.WatchdogUnknown, LRMMode: "maintenance", LRMState: "maintenance",
	}
	return f
}

func TestCollectHoldBlocksMaintenanceOnlyWhileItsStateKeepsItRunning(t *testing.T) {
	t.Parallel()
	started := runCollect(t, drainFake("started"), haCfg("auto", "hold"))
	want(t, started, haKindHoldMaintenance, haSeverityProblem, "vm:40", "n2")

	// A stopped HA service does not block the drain.
	stopped := runCollect(t, drainFake("stopped"), haCfg("auto", "hold"))
	wantNone(t, stopped, haKindHoldMaintenance)
	for _, f := range findings(stopped, haKindMaintenanceStranded) {
		if f.Subject == "vm:40" {
			t.Errorf("a stopped hold VM was reported as stranded: %+v", f)
		}
	}

}

func TestCollectNamesEveryServiceThatCannotLeaveAMaintenanceNode(t *testing.T) {
	t.Parallel()
	r := runCollect(t, drainFake("started"), haCfg("auto", "hold"))
	// The hold VM is named once, as hold_blocks_maintenance, and the group
	// member is named once, as maintenance_stranded, with its reason.
	want(t, r, haKindHoldMaintenance, haSeverityProblem, "vm:40")
	want(t, r, haKindMaintenanceStranded, haSeverityProblem, "vm:217", "n2", "anti-affinity group")

	// A strict pin that names no other node is named as the reason.
	f := drainFake("started")
	f.rules = append(f.rules, pinRule(217, haMarker, "n2"))
	got := findings(runCollect(t, f, haCfg("auto", "hold")), haKindMaintenanceStranded)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "strict node-affinity pin") {
		t.Errorf("want one finding that names the strict pin, got %+v", got)
	}

	// A member of a group with a free node is not told it has no place to go.
	g := drainFake("started")
	g.clusterVMs = g.clusterVMs[:1]
	got = findings(runCollect(t, g, haCfg("auto", "hold")), haKindMaintenanceStranded)
	if len(got) != 1 || strings.Contains(got[0].Detail, "anti-affinity") {
		t.Errorf("want one finding without the anti-affinity reason, got %+v", got)
	}
}

func replicaFake() *fakeHA {
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi;bosh-ha-managed")
	// create_vm registers a replicate VM with failback off.
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker,
		Failback: failbackRow(false)})
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaPartnerKey+"=n2", "n1", "n2")}
	f.jobs = []ha.ReplicaJobState{{
		ID: "101-0", Guest: 101, JobNum: 0, Source: "n1", Target: "n2", Comment: haMarker + " gen=1",
	}}
	f.schedules = map[string]string{"101-0": "*/5"}
	f.status = map[string]ha.ReplicaSyncStatus{"101-0": {LastSync: haNow.Add(-2 * time.Minute)}}
	f.storages = []pve.StorageInfo{
		{Name: "nfs1", Type: "nfs", Shared: true, Content: "images"},
		{Name: "local-zfs", Type: "zfspool", Content: "images"},
	}
	f.volumes = map[string]map[string]bool{}
	return f
}

func TestCollectReplicationHealthy(t *testing.T) {
	t.Parallel()
	r := runCollect(t, replicaFake(), haCfg("auto", "replicate"))
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
}

func TestCollectReplicaOnPartnerAndAwaitingPinWidening(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.guests[1].Node = "n2"
	f.journal = JournalView{Configured: true, Records: []aj.Record{{Replication: &aj.Replication{
		Owner: haDirector, Job: &aj.ReplicaJob{ID: "101-0", Guest: 101, Source: "n1", Target: "n2", Generation: 1},
	}}}}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaOnPartner, haSeverityProblem, "vm:101", "partner n2", "from n2 back to n1")
	// The job has synced and the journal holds no verified sync yet, so the pin
	// has not been widened: that is a wait for the next call, not a fault, and
	// the advice names the command that widens it now.
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "synced at",
		"the next CPI call that takes the HA rules lock", "pve-cid ha --widen-replicas --apply")
	wantNone(t, r, haKindReplicaNotRecover)

	// With no pin token and no journal proof, the VM is awaiting the same way.
	g := replicaFake()
	g.rules = []ha.Rule{pinRule(101, haMarker, "n1", "n2")}
	r = runCollect(t, g, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "the next CPI call that takes the HA rules lock")
	wantNone(t, r, haKindReplicaNotRecover, haKindReplicaOnPartner, haKindReplicaJobStale)
	if r.Problems() != 0 {
		t.Fatalf("a healthy job awaiting its pin must not fail the run: %+v", r.Findings)
	}
}

// TestCollectReplicaOnPartnerAfterAMove judges a replicate VM that runs on its
// pin's partner n2 and whose job PVE has swapped to run from n2 back to n1,
// by the sync from n2 and the migration and start tasks of the two nodes.
func TestCollectReplicaOnPartnerAfterAMove(t *testing.T) {
	t.Parallel()
	changed := haNow.Add(-2 * time.Hour)
	migrated := pve.VMTask{UPID: "UPID:n1:00000001:00000001:00000001:qmigrate:101:root@pam:", Node: "n1",
		Type: "qmigrate", ID: "101", Status: "OK", StartTime: haNow.Add(-61 * time.Minute).Unix(),
		EndTime: haNow.Add(-time.Hour).Unix()}
	started := func(node string, at time.Time) pve.VMTask {
		return pve.VMTask{UPID: fmt.Sprintf("UPID:%s:00000001:00000001:%08X:qmstart:101:root@pam:", node, at.Unix()),
			Node: node, Type: "qmstart", ID: "101", Status: "OK", StartTime: at.Unix(),
			EndTime: at.Add(5 * time.Second).Unix()}
	}
	// logged gives each start the log PVE writes for it, which shows whether
	// it received a live migration.
	logged := func(f *fakeHA, incoming bool, tasks ...pve.VMTask) {
		if f.taskLogs == nil {
			f.taskLogs = map[string][]string{}
		}
		for _, task := range tasks {
			f.taskLogs[task.UPID] = []string{"TASK OK"}
			if incoming {
				f.taskLogs[task.UPID] = []string{"migration listens on unix:/run/qemu-server/101.migrate", "TASK OK"}
			}
		}
	}
	incoming := started("n2", haNow.Add(-61*time.Minute+2*time.Second))
	moved := func() *fakeHA {
		f := replicaFake()
		f.guests[1].Node = "n2"
		f.rules = []ha.Rule{pinRule(101, ha.WithReplicaChanged(haMarker+" "+ha.ReplicaPartnerKey+"=n2", changed),
			"n1", "n2")}
		f.jobs[0].Source, f.jobs[0].Target = "n2", "n1"
		f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-10 * time.Minute)}
		f.migrations = map[string][]pve.VMTask{"n1": {migrated}}
		// The sync back to n1 has copied the VM's volume there.
		f.volumes["n1/local-zfs"] = map[string]bool{"local-zfs:vm-101-disk-0": true}
		return f
	}
	for _, tc := range []struct {
		name     string
		edit     func(f *fakeHA)
		severity string
		mentions []string
		absent   string
	}{
		{name: "planned migration", severity: haSeverityAdvice,
			mentions: []string{"partner n2 after a planned migration",
				"names n1 as the partner once it confirms that sync", "covers the VM's volumes"},
			absent: "failover"},
		{name: "no migration task", edit: func(f *fakeHA) { f.migrations = nil }, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"no migration task on n1", "were lost", "names n1 as the partner"},
			absent: "rolls"},
		{name: "migration tasks unreadable", edit: func(f *fakeHA) {
			f.migrationErr = map[string]error{"n1": errors.New("needs Sys.Audit")}
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"could not be read", "needs Sys.Audit", "may have been lost"},
			absent: "rolls"},
		{name: "planned migration with the target's incoming start", edit: func(f *fakeHA) {
			f.starts = map[string][]pve.VMTask{"n2": {incoming}}
			logged(f, true, incoming)
		}, severity: haSeverityAdvice,
			mentions: []string{"partner n2 after a planned migration", "names n1 as the partner"},
			absent:   "failover"},
		{name: "HA recoveries back to n1 and to n2 again after the migration", edit: func(f *fakeHA) {
			recovery := started("n2", haNow.Add(-40*time.Minute))
			f.starts = map[string][]pve.VMTask{
				"n1": {started("n1", haNow.Add(-50*time.Minute))},
				"n2": {incoming, recovery},
			}
			logged(f, true, incoming)
			logged(f, false, recovery)
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"a migration from n1 explains the move", "started outside a migration since the pin last changed",
				"may have been lost", "names n1 as the partner"},
			absent: "planned migration"},
		{name: "an HA recovery on n2 soon after the migration", edit: func(f *fakeHA) {
			recovery := started("n2", haNow.Add(-58*time.Minute))
			f.starts = map[string][]pve.VMTask{"n2": {incoming, recovery}}
			logged(f, true, incoming)
			logged(f, false, recovery)
			f.taskLogs[migrated.UPID] = []string{"starting VM 101 on remote node 'n2'", "TASK OK"}
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"a migration from n1 explains the move", "started outside a migration since the pin last changed",
				"may have been lost", "names n1 as the partner"},
			absent: "planned migration"},
		{name: "start tasks unreadable", edit: func(f *fakeHA) {
			f.startErr = map[string]error{"n2": errors.New("needs Sys.Audit")}
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"could not be read", "needs Sys.Audit", "may have been lost"},
			absent: "rolls"},
		{name: "a start's log unreadable", edit: func(f *fakeHA) {
			f.starts = map[string][]pve.VMTask{"n2": {incoming}}
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"could not be read", "no log for task", "may have been lost"},
			absent: "rolls"},
		{name: "no sync from n2 yet", edit: func(f *fakeHA) {
			f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: changed.Add(-time.Hour)}
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"rolls n1's copy back to match"},
			absent: "planned"},
		{name: "a volume missing on n1", edit: func(f *fakeHA) { delete(f.volumes, "n1/local-zfs") },
			severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"no sync from n2 back to n1 that covers the VM's volumes is confirmed yet",
				"local-zfs:vm-101-disk-0 is not on node n1"},
			absent: "planned"},
		{name: "a job whose marker names no Director", edit: func(f *fakeHA) {
			f.jobs[0].Comment = "bosh-cpi:v1 owner=- gen=1"
			f.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
		}, severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet"},
			absent:   "copies from"},
		{name: "two jobs carry this Director's marker", edit: func(f *fakeHA) {
			second := f.jobs[0]
			second.ID, second.JobNum = "101-1", 1
			f.jobs = append(f.jobs, second)
		}, severity: haSeverityProblem,
			mentions: []string{"the move can't be judged", "replication jobs 101-0 and 101-1",
				"refuses to pick one"},
			absent: "planned"},
		{name: "job not swapped", edit: func(f *fakeHA) { f.jobs[0].Source, f.jobs[0].Target = "n1", "n2" },
			severity: haSeverityProblem,
			mentions: []string{"after a failover, or after a move the CPI has not reconciled yet",
				"copies from n2 back to n1"},
			absent: "planned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := moved()
			if tc.edit != nil {
				tc.edit(f)
			}
			r := runCollect(t, f, haCfg("auto", "replicate"))
			want(t, r, haKindReplicaOnPartner, tc.severity, append([]string{"vm:101"}, tc.mentions...)...)
			if got := findings(r, haKindReplicaOnPartner)[0].Detail; strings.Contains(got, tc.absent) {
				t.Fatalf("finding %q mentions %q", got, tc.absent)
			}
		})
	}
}

func TestCollectReplicaAwaitingFirstSync(t *testing.T) {
	t.Parallel()
	changed := haNow.Add(-time.Minute).Unix()
	fresh := func() *fakeHA {
		f := replicaFake()
		f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaChangedKey+"="+itoa(int(changed)), "n1")}
		f.status["101-0"] = ha.ReplicaSyncStatus{}
		return f
	}
	r := runCollect(t, fresh(), haCfg("auto", "replicate"))
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "awaiting its first sync", "101-0", "has not completed its first sync")
	wantNone(t, r, haKindReplicaNotRecover, haKindReplicaJobStale)
	// The source-only pin leaves headroom short, which is a separate fact.
	for _, f := range r.Findings {
		if f.Severity == haSeverityProblem && f.Kind != haKindHeadroomShort {
			t.Fatalf("a new replicate VM must not fail the run: %+v", r.Findings)
		}
	}

	// A sync that started before the volume change does not cover it.
	f := fresh()
	f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour)}
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaChangedKey+"="+itoa(int(changed)), "n1")}
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "after the last volume change")

	// A first sync that is more than twice the schedule overdue is a fault.
	old := haNow.Add(-time.Hour).Unix()
	f = fresh()
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaChangedKey+"="+itoa(int(old)), "n1")}
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaNotRecover, haSeverityProblem, "vm:101", "101-0", "more than twice its schedule")
	wantNone(t, r, haKindReplicaAwaiting)
	want(t, r, haKindReplicaJobStale, haSeverityAdvice, "101-0")

	// A schedule the doctor cannot measure never makes the first sync overdue.
	f = fresh()
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaChangedKey+"="+itoa(int(old)), "n1")}
	f.schedules["101-0"] = "mon..fri 08:00"
	wantNone(t, runCollect(t, f, haCfg("auto", "replicate")), haKindReplicaNotRecover)
}

func TestCollectReplicaTagFindsVMWithoutJob(t *testing.T) {
	t.Parallel()
	// The job is gone, so only the tag says the VM was meant to replicate.
	f := replicaFake()
	f.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
	f.jobs = nil
	f.rules = []ha.Rule{pinRule(101, haMarker, "n1")}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaNotRecover, haSeverityProblem, "vm:101", "no replication job the CPI marked")

	// A job row with a fault or a removal pending is no job.
	for name, mutate := range map[string]func(*ha.ReplicaJobState){
		"fault":   func(j *ha.ReplicaJobState) { j.Fault = "id listed twice" },
		"removal": func(j *ha.ReplicaJobState) { j.RemoveJob = "full" },
		"foreign": func(j *ha.ReplicaJobState) {
			j.Comment = "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000 gen=1"
		},
	} {
		g := replicaFake()
		g.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
		g.rules = []ha.Rule{pinRule(101, haMarker, "n1")}
		mutate(&g.jobs[0])
		want(t, runCollect(t, g, haCfg("auto", "replicate")), haKindReplicaNotRecover, haSeverityProblem, "vm:101")
		_ = name
	}

	// A create still in flight is not yet expected to have its job.
	h := replicaFake()
	h.guests[1].Tags = "bosh-cpi;bosh-ha-replica;bosh-creating-" + itoa(int(haNow.Add(-time.Minute).Unix()))
	h.jobs = nil
	wantNone(t, runCollect(t, h, haCfg("auto", "replicate")), haKindReplicaNotRecover)

	// A disabled job produces no sync.
	d := replicaFake()
	d.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
	d.rules = []ha.Rule{pinRule(101, haMarker, "n1")}
	d.jobs[0].Disabled = true
	want(t, runCollect(t, d, haCfg("auto", "replicate")), haKindReplicaNotRecover, haSeverityProblem, "101-0", "disabled")

	// An untagged VM, a tag on a VM the CPI did not create, and a recoverable
	// tagged VM raise nothing.
	u := replicaFake()
	u.guests[1].Tags = "bosh-ha-replica"
	u.jobs = nil
	wantNone(t, runCollect(t, u, haCfg("auto", "replicate")), haKindReplicaNotRecover)
	ok := replicaFake()
	ok.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
	if r := runCollect(t, ok, haCfg("auto", "replicate")); len(r.Findings) != 0 {
		t.Fatalf("a recoverable tagged VM = %+v, want no findings", r.Findings)
	}
}

func TestCollectReplicaTagAloneEnablesVolumeScanAndJournalAdvice(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
	f.jobs = nil
	f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaPartnerKey+"=n2", "n1", "n2")}
	f.journal = JournalView{}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindReplicaNoJournal, haSeverityAdvice)

	f.journal = JournalView{Configured: true}
	f.volumes = map[string]map[string]bool{"n2/local-zfs": {"local-zfs:vm-101-disk-0": true}}
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindReplicaOrphanVolume, haSeverityProblem, "local-zfs:vm-101-disk-0")
}

func TestCollectForeignDirectorJobsAreIgnored(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.jobs[0].Comment = "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000 gen=1"
	f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour), FailCount: 3}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	wantNone(t, r, haKindReplicaJobFailing, haKindReplicaJobStale)
	r = collectHA(context.Background(), f, haCfg("auto", "replicate"), "", haNow)
	want(t, r, haKindReplicaJobFailing, haSeverityProblem, "101-0")
}

func TestCollectReplicaJobStaleFailingAndUnjudged(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour), FailCount: 3, Error: "zfs send failed"}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaJobFailing, haSeverityProblem, "101-0", "3 time(s)", "zfs send failed")
	want(t, r, haKindReplicaJobStale, haSeverityProblem, "101-0", "twice its 5m0s schedule")

	f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour)}
	f.schedules["101-0"] = "mon..fri 08:00"
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaScheduleSkip, haSeverityAdvice, "101-0", "mon..fri 08:00")
	wantNone(t, r, haKindReplicaJobStale)

	f.status["101-0"] = ha.ReplicaSyncStatus{}
	f.schedules["101-0"] = "*/5"
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaJobStale, haSeverityAdvice, "101-0", "not completed a sync")
}

func TestCollectReplicationReadsUnreadableAreUnchecked(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.jobsErr = errFake
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindUnchecked, haSeverityProblem, "replication")

	f = replicaFake()
	f.schedulesErr = errFake
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindUnchecked, haSeverityProblem, "replication-schedules")

	f = replicaFake()
	f.statusErr = map[string]error{"101-0": errFake}
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindUnchecked, haSeverityProblem, "job 101-0")

	f = replicaFake()
	f.journalErr = errFake
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindUnchecked, haSeverityProblem, "journal")
}

func TestCollectOrphanReplicaVolumes(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	// n2 holds the covered replica of 101 and a copy of 100's neighbour that no
	// job targets; n3 holds a stray copy of 101.
	f.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-101-disk-0": true}
	f.volumes["n3/local-zfs"] = map[string]bool{"local-zfs:vm-101-disk-0": true, "local-zfs:vm-9999-disk-0": true}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaOrphanVolume, haSeverityProblem, "local-zfs:vm-101-disk-0", "n3", "no replication job targets")

	// An unreadable storage on one node is unchecked, not clean.
	f.volumesErr = map[string]error{"n3/local-zfs": errFake}
	r = runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindUnchecked, haSeverityProblem, "storage local-zfs@n3")
	wantNone(t, r, haKindReplicaOrphanVolume)

	// A cluster that never replicates is not scanned.
	g := haBaseFake()
	g.volumes = map[string]map[string]bool{"n2/local-zfs": {"local-zfs:vm-100-disk-0": true}}
	wantNone(t, runCollect(t, g, haCfg("auto", "none")), haKindReplicaOrphanVolume, haKindUnchecked)
}

func TestCollectRecordedCopiesAndMissingJournal(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	recorded := haNow.Add(-48 * time.Hour)
	f.journal = JournalView{Configured: true, Records: []aj.Record{{Replication: &aj.Replication{
		Owner: haDirector, Retired: true,
		Job: &aj.ReplicaJob{ID: "101-0", Guest: 101, Source: "n1", Target: "n2", Generation: 1},
		Copies: []aj.ReplicaCopy{
			{Kind: aj.ReplicaCopyOrphan, Node: "n2", JobID: "101-0", VolIDs: []string{"local-zfs:vm-101-disk-0"}, RecordedAt: recorded},
			{Kind: aj.ReplicaCopySuperseded, Node: "n3", JobID: "101-1", VolIDs: []string{"local-zfs:vm-101-disk-1"}, RecordedAt: recorded},
		},
	}}}}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaRecordedOrph, haSeverityProblem, "local-zfs:vm-101-disk-0@n2", "forced job removal")
	want(t, r, haKindReplicaSuperseded, haSeverityAdvice, "local-zfs:vm-101-disk-1@n3", "later retirement")

	g := replicaFake()
	g.journal = JournalView{Configured: false}
	r = runCollect(t, g, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaNoJournal, haSeverityAdvice, "no allocation journal")
	wantNone(t, r, haKindUnchecked)
	// A cluster that does not replicate needs no journal.
	wantNone(t, runCollect(t, &fakeHA{}, haCfg("auto", "none")), haKindReplicaNoJournal)
}

func TestCollectHeadroomShortAndUnavailable(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.memory[100] = 60 * haGiB
	f.guests = append(f.guests,
		pve.GuestRef{VMID: 102, Node: "n2", Tags: "bosh-cpi;bosh-ha-managed"},
		pve.GuestRef{VMID: 103, Node: "n3", Tags: "bosh-cpi;bosh-ha-managed"})
	f.memory[102], f.memory[103] = 60*haGiB, 60*haGiB
	f.resources = append(f.resources,
		HAResourceRow{SID: "vm:102", State: "started", Comment: haMarker}, HAResourceRow{SID: "vm:103", State: "started", Comment: haMarker})
	for _, id := range []int{102, 103} {
		f.configs[id] = map[string]any{"cpu": "x86-64-v2-AES"}
	}
	r := runCollect(t, f, haCfg("auto", "none"))
	if !strings.HasPrefix(r.Headroom, "short(") || !strings.Contains(r.Profile, "headroom="+r.Headroom) {
		t.Fatalf("headroom = %q, profile = %q", r.Headroom, r.Profile)
	}
	if got := findings(r, haKindHeadroomShort); len(got) != 3 || got[0].Severity != haSeverityProblem {
		t.Fatalf("headroom findings = %+v, want one per failing node", got)
	}

	for _, line := range findings(r, haKindHeadroomShort) {
		if strings.Contains(line.Detail, "is online") && !strings.Contains(line.Detail, "out of maintenance") {
			t.Errorf("headroom detail %q calls the surviving nodes offline", line.Detail)
		}
	}

	g := haBaseFake()
	g.memoryErr = errFake
	r = runCollect(t, g, haCfg("auto", "none"))
	want(t, r, haKindHeadroomUnavailable, haSeverityAdvice, "guest memory")
	if r.Headroom != "unavailable" {
		t.Fatalf("headroom = %q, want unavailable", r.Headroom)
	}
	for name, mutate := range map[string]func(*fakeHA){
		"guests":    func(f *fakeHA) { f.guestsErr = errFake },
		"resources": func(f *fakeHA) { f.resourcesErr = errFake },
		"facts":     func(f *fakeHA) { f.factsErr = errFake },
	} {
		h := haBaseFake()
		mutate(h)
		r = runCollect(t, h, haCfg("auto", "none"))
		if r.Headroom != "unavailable" {
			t.Errorf("%s: headroom = %q, want unavailable", name, r.Headroom)
		}
	}
}

func TestCollectHeadroomLeavesOutGuestHeldToOwnNode(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.memory[100] = 60 * haGiB
	f.rules = []ha.Rule{pinRule(100, haMarker, "n1")}
	r := runCollect(t, f, haCfg("auto", "none"))
	wantNone(t, r, haKindHeadroomShort)
	if !strings.HasPrefix(r.Headroom, "ok(") {
		t.Fatalf("headroom = %q, want ok", r.Headroom)
	}

	// A pin to a different single node still counts the guest as HA.
	g := haBaseFake()
	g.memory[100] = 60 * haGiB
	g.guests = append(g.guests, pve.GuestRef{VMID: 102, Node: "n2", Tags: "bosh-cpi"})
	g.memory[102] = 60 * haGiB
	g.configs[102] = map[string]any{"cpu": "x86-64-v2-AES"}
	g.rules = []ha.Rule{pinRule(100, haMarker, "n2")}
	r = runCollect(t, g, haCfg("auto", "none"))
	if len(findings(r, haKindHeadroomShort)) == 0 {
		t.Fatalf("headroom = %q, want a headroom_short finding for a guest pinned to another node", r.Headroom)
	}
}

func TestCollectHeadroomSkipsNonHAGuestAdmission(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.resources = nil
	r := runCollect(t, f, haCfg("auto", "none"))
	if !strings.HasPrefix(r.Headroom, "ok(") {
		t.Fatalf("headroom = %q, want ok", r.Headroom)
	}
}

func TestCollectLeftovers(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	stale := haNow.Add(-24 * time.Hour).Unix()
	fresh := haNow.Add(-time.Minute).Unix()
	f.guests = append(f.guests,
		pve.GuestRef{VMID: 110, Node: "n1", Tags: "bosh-cpi;bosh-creating-" + itoa(int(stale))},
		pve.GuestRef{VMID: 111, Node: "n1", Tags: "bosh-cpi;bosh-creating-" + itoa(int(fresh))},
		pve.GuestRef{VMID: 112, Node: "n2", Tags: "bosh-cpi;bosh-ha-readd-" + itoa(int(stale))},
		pve.GuestRef{VMID: 113, Node: "n3", Tags: "bosh-cpi;bosh-ha-managed"},
		pve.GuestRef{VMID: 114, Node: "n3", Tags: "bosh-cpi;bosh-ha-readd-" + itoa(int(fresh))},
	)
	for _, id := range []int{110, 111, 112, 113, 114} {
		f.memory[id] = haGiB
		f.configs[id] = map[string]any{"cpu": "x86-64-v2-AES"}
	}
	f.resources = append(f.resources,
		HAResourceRow{SID: "vm:110", State: "ignored", Comment: haMarker},
		HAResourceRow{SID: "vm:555", State: "ignored", Comment: "operator made this"},
		HAResourceRow{SID: "vm:556", State: "ignored", Comment: "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000"},
	)
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindResourceIgnored, haSeverityProblem, "vm:110")
	want(t, r, haKindBirthTagStale, haSeverityProblem, "vm:110", "bosh-creating-")
	want(t, r, haKindReaddTag, haSeverityProblem, "vm:112", "re-registration")
	// A re-registration that started a minute ago is still running.
	for _, f := range findings(r, haKindReaddTag) {
		if f.Subject == "vm:114" {
			t.Errorf("a young re-registration tag was reported: %+v", f)
		}
	}
	want(t, r, haKindManagedNoResource, haSeverityProblem, "vm:113", "no HA resource")
	wantNone(t, r, haKindOffModeResource, haKindOffModeRule)
}

func TestCollectOffModeLeavesObjectsBehind(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile = haHealthyProfile("off")
	f.profile.Effective = ha.EffectiveOff
	f.rules = []ha.Rule{pinRule(100, haMarker, "n1"), pinRule(777, "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000", "n1")}
	r := runCollect(t, f, haCfg("off", "none"))
	want(t, r, haKindOffModeResource, haSeverityProblem, "vm:100", "pve.ha.mode is off")
	want(t, r, haKindOffModeRule, haSeverityProblem, "bosh-na-100", "pve.ha.mode is off")
	wantNone(t, r, haKindObserveOnly)
	if r.Mode != "off" || r.Effective != "off" {
		t.Fatalf("header = %s/%s", r.Mode, r.Effective)
	}

	clean := haBaseFake()
	clean.profile = haHealthyProfile("off")
	clean.profile.Effective = ha.EffectiveOff
	clean.resources = nil
	clean.guests[0].Tags = "bosh-cpi"
	r = runCollect(t, clean, haCfg("off", "none"))
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
}

func TestCollectGuestListingGaps(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.excluded = []string{"n3"}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindGuestsNotListed, haSeverityAdvice, "n3", "offline")

	f = haBaseFake()
	f.guestsErr = errFake
	r = runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindUnchecked, haSeverityProblem, "guests")
}

func TestCollectOtherUncheckedSources(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.resourcesErr = errFake
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindUnchecked, haSeverityProblem, "ha-resources")

	f = haBaseFake()
	f.storagesErr = errFake
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindUnchecked, haSeverityProblem, "storages")

	f = haBaseFake()
	f.configErr = map[int]error{100: errFake}
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindUnchecked, haSeverityProblem, "vm:100")
}

func TestCollectOwnerFilterIgnoresOtherDirectorsOnlyWhenKnown(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile = haHealthyProfile("off")
	f.profile.Effective = ha.EffectiveOff
	other := "bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000"
	f.resources = []HAResourceRow{{SID: "vm:900", State: "started", Comment: other}}
	f.rules = nil
	cfg := haCfg("off", "none")
	if got := collectHA(context.Background(), f, cfg, haDirector, haNow); len(findings(got, haKindOffModeResource)) != 0 {
		t.Fatalf("a run for %s reported another Director's resource: %+v", haDirector, got.Findings)
	}
	if got := collectHA(context.Background(), f, cfg, "", haNow); len(findings(got, haKindOffModeResource)) != 1 {
		t.Fatalf("a run with no Director should report every marked resource: %+v", got.Findings)
	}
}

func TestMarkerParsing(t *testing.T) {
	t.Parallel()
	if owner, ok := haMarkerOwner(haMarker + " extra"); !ok || owner != haDirector {
		t.Fatalf("owner = %q ok=%v", owner, ok)
	}
	if owner, ok := haMarkerOwner("bosh-cpi:v1 owner=-"); !ok || owner != "" {
		t.Fatalf("owner = %q ok=%v, want marked with no owner", owner, ok)
	}
	if _, ok := haMarkerOwner("hand written"); ok {
		t.Fatal("a plain comment is not a marker")
	}
	if _, ok := haMarkerOwner(""); ok {
		t.Fatal("an empty comment is not a marker")
	}
	for comment, want := range map[string]bool{
		haMarker + " gen=2":         true,
		haMarker:                    false,
		haMarker + " gen=0":         false,
		haMarker + " gen=abc":       false,
		"bosh-cpi:v1 gen=1":         false,
		"bosh-cpi:v1 owner=- gen=1": false,
		"":                          false,
		"other " + haMarker:         false,
	} {
		if got := haJobMarker(comment); got != want {
			t.Errorf("haJobMarker(%q) = %v, want %v", comment, got, want)
		}
	}
	for state, want := range map[string]bool{"": true, "started": true, "enabled": true, "stopped": false, "ignored": false, "disabled": false} {
		if haRunsUnderHA(state) != want {
			t.Errorf("haRunsUnderHA(%q) != %v", state, want)
		}
	}
}

func TestScheduleInterval(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"*": time.Minute, "minutely": time.Minute, "hourly": time.Hour, "daily": 24 * time.Hour,
		"weekly": 7 * 24 * time.Hour, "monthly": 31 * 24 * time.Hour, "quarterly": 92 * 24 * time.Hour,
		"semiannually": 184 * 24 * time.Hour, "yearly": 366 * 24 * time.Hour, "annually": 366 * 24 * time.Hour,
		"*/15": 15 * time.Minute, "*:0/30": 30 * time.Minute, "*:15": time.Hour, "02:00": 24 * time.Hour,
	}
	for spec, want := range cases {
		got, ok := scheduleInterval(spec)
		if !ok || got != want {
			t.Errorf("scheduleInterval(%q) = %v, %v; want %v", spec, got, ok, want)
		}
	}
	for _, spec := range []string{"", "mon..fri 08:00", "*/0", "sat,sun", "*-*-01 00:00", "*/x"} {
		if _, ok := scheduleInterval(spec); ok {
			t.Errorf("scheduleInterval(%q) judged, want unjudged", spec)
		}
	}
}

func TestBirthTagTime(t *testing.T) {
	t.Parallel()
	if got, ok := birthTagTime("bosh-creating-1700000000"); !ok || got.Unix() != 1700000000 {
		t.Fatalf("birth tag = %v %v", got, ok)
	}
	for _, tag := range []string{"bosh-creating-", "bosh-creating-abc", "other", "bosh-creating-99999999999999999999"} {
		if _, ok := birthTagTime(tag); ok {
			t.Errorf("birthTagTime(%q) parsed", tag)
		}
	}
}

func TestReportTextCoversEveryFinding(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.UnknownCPUNodes = []string{"n3"}
	f.profile.Advice = "add-qdevice: even"
	f.rules = []ha.Rule{{ID: "bosh-na-1", Comment: haMarker, Disabled: true}}
	r := runCollect(t, f, haCfg("auto", "none"))
	var out strings.Builder
	r.writeText(&out, "  ")
	text := out.String()
	for _, frag := range []string{"  ha: mode=auto", "  nodes:", "n1 online votes=1 health=online placeable=yes", "cpu migration domains:",
		"domain 1: EPYC (no flags) on n1, n2, n3", "CPU model unreadable on n3", "[problem] rule_problem bosh-na-1", "[advice] qdevice_advice",
		"result: 1 problem(s), 2 advice"} {
		if !strings.Contains(text, frag) {
			t.Errorf("text report lacks %q:\n%s", frag, text)
		}
	}
	empty := &haReport{}
	out.Reset()
	empty.writeText(&out, "")
	if !strings.Contains(out.String(), "none read") || !strings.Contains(out.String(), "  none") {
		t.Fatalf("empty report = %s", out.String())
	}
	// A node whose health was not built prints no placeable answer.
	if got := nodeLine(haNode{Name: "x", Maintenance: true, Watchdog: "active", Version: "9", CPU: "c", Reason: "why"}); got !=
		"x offline maintenance=yes watchdog=active pve=9 cpu=c (why)" {
		t.Fatalf("node line = %q", got)
	}
	if got := nodeLine(haNode{Name: "y", Online: true, Health: "online"}); got != "y online health=online placeable=no" {
		t.Fatalf("node line = %q", got)
	}
}

func TestCollectReportsACacheDirectoryTheCPICannotUse(t *testing.T) {
	// With no journal, whether the CPI creates a missing override depends on
	// the temporary directory's owner, so the check gets one this user owns.
	t.Setenv("TMPDIR", t.TempDir())
	private := filepath.Join(t.TempDir(), "private")
	wide := filepath.Join(t.TempDir(), "wide")
	file := filepath.Join(t.TempDir(), "afile")
	link := filepath.Join(t.TempDir(), "link")
	for _, dir := range []string{private, wide} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(wide, 0o755); err != nil { //nolint:gosec // the test makes the mode unsafe on purpose
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	// The parent is missing too, so the CPI does not create the directory.
	missing := filepath.Join(t.TempDir(), "no-parent", "absent")
	creatable := filepath.Join(t.TempDir(), "absent")

	cases := map[string]struct {
		dir       string
		fragments []string
	}{
		"mode wider than 0700": {wide, []string{"mode 0755", "chmod 0700", "dead-node handover cannot start"}},
		"missing directory": {missing, []string{"does not exist", "does not create it because its parent directory",
			"restarts any grace window"}},
		"missing directory the CPI creates": {creatable, []string{"creates it with mode 0700 on first use",
			"same mount as this check's temporary directory", "only when the CPI's TMPDIR names the same directory",
			"director.cpi_additional_volumes"}},
		"not a directory": {file, []string{"not a directory"}},
		"symbolic link":   {link, []string{"symbolic link"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if os.Geteuid() == 0 && (tc.dir == missing || tc.dir == creatable) {
				t.Skip("run as root with no journal, the check cannot tell whether the CPI creates a missing directory")
			}
			f := haBaseFake()
			cfg := haCfg("auto", "none")
			cfg.EndpointCacheDir = tc.dir
			r := runCollect(t, f, cfg)
			want(t, r, haKindCacheDirectory, haSeverityAdvice, append([]string{tc.dir}, tc.fragments...)...)
			if r.Problems() != 0 {
				t.Errorf("advice about the cache directory must not fail the run: %+v", r.Findings)
			}
		})
	}

	t.Run("private directory and unset", func(t *testing.T) {
		t.Parallel()
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = private
		wantNone(t, runCollect(t, haBaseFake(), cfg), haKindCacheDirectory)
		cfg.EndpointCacheDir = ""
		wantNone(t, runCollect(t, haBaseFake(), cfg), haKindCacheDirectory)
	})
}

// journalCacheLayout makes a journal directory path and, when sibling is not
// empty, the endpoint-cache directory beside it with that mode.
func journalCacheLayout(t *testing.T, sibling os.FileMode) (journal, siblingPath string) {
	t.Helper()
	base := t.TempDir()
	journal = filepath.Join(base, "journal")
	siblingPath = filepath.Join(base, "endpoint-cache")
	if sibling != 0 {
		if err := os.Mkdir(siblingPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(siblingPath, sibling); err != nil { //nolint:gosec // the mode is the test's input
			t.Fatal(err)
		}
	}
	return journal, siblingPath
}

func TestCollectNamesTheLayerAMissingCacheOverrideFallsBackTo(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	missing := filepath.Join(t.TempDir(), "absent")

	t.Run("directory beside the journal", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o700)
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = missing
		cfg.StorageAllocationJournalDir = journal
		r := runCollect(t, haBaseFake(), cfg)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, missing, "does not exist", "beside the allocation journal")
		for _, f := range r.Findings {
			if f.Kind == haKindCacheDirectory && strings.Contains(f.Detail, "temporary") {
				t.Errorf("the record is beside the journal, but the finding names the temporary one: %s", f.Detail)
			}
		}
	})

	t.Run("temporary directory", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0)
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = missing
		cfg.StorageAllocationJournalDir = journal
		r := runCollect(t, haBaseFake(), cfg)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, missing, "does not exist", "temporary cache directory")
	})

	t.Run("refused fallback", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o755)
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = missing
		cfg.StorageAllocationJournalDir = journal
		r := runCollect(t, haBaseFake(), cfg)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, missing, "cannot be used either", "mode 0755",
			"dead-node handover cannot start")
	})
}

func TestCollectReportsARefusedDefaultCacheDirectory(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	t.Run("journal sibling with a wide mode and no override", func(t *testing.T) {
		journal, sibling := journalCacheLayout(t, 0o755)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		r := runCollect(t, haBaseFake(), cfg)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "beside the allocation journal", "mode 0755",
			"dead-node handover cannot start")
	})

	t.Run("private journal sibling and absent temporary directory", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o700)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		wantNone(t, runCollect(t, haBaseFake(), cfg), haKindCacheDirectory)
	})

	t.Run("nothing is created for an absent temporary directory", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		wantNone(t, runCollect(t, haBaseFake(), haCfg("auto", "none")), haKindCacheDirectory)
		if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
			t.Fatalf("the doctor left %v (%v) in the temporary directory", entries, err)
		}
	})

	t.Run("override that passes the mode check but overlaps the journal", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0)
		if err := os.Mkdir(journal, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = journal
		cfg.StorageAllocationJournalDir = journal
		r := runCollect(t, haBaseFake(), cfg)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, journal, "refused")
	})
}

func TestCacheOwnerNoteSaysWhoRanTheCheck(t *testing.T) {
	t.Parallel()
	owner := &endpointcache.Refusal{Layer: endpointcache.LayerOverride, Path: "/x", Err: &privatefile.UnsafePathError{
		Path: "/x", UID: 1234, EUID: 0, Cause: privatefile.ErrUnsafeOwnership, WantDirectory: true,
	}}
	unknown := cacheJudge{uid: 0, doctor: 0}
	if got := unknown.ownerNote(owner); !strings.Contains(got, "uid 0") || !strings.Contains(got, "CPI's user") {
		t.Errorf("owner note = %q", got)
	}
	known := cacheJudge{uid: 1000, known: true, doctor: 0}
	if got := known.ownerNote(owner); !strings.Contains(got, "uid 1000") ||
		!strings.Contains(got, "owns the allocation journal directory") {
		t.Errorf("owner note with the CPI's user known = %q", got)
	}
	mode := &endpointcache.Refusal{Err: &privatefile.UnsafePathError{Cause: privatefile.ErrUnsafeMode}}
	if got := known.ownerNote(mode); got != "" {
		t.Errorf("a mode refusal needs no owner note, got %q", got)
	}
}

// cacheFindingsAs runs the cache directory checks for cfg with the doctor
// running as doctor.
func cacheFindingsAs(cfg *config.CPIConfig, doctor int) *haReport {
	c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}}, workers: unknownWorkers}
	c.cacheDirectoryFindings()
	return c.rep
}

// unknownWorkers is a host with no Director job to say where its workers run.
func unknownWorkers() workerLayout { return workerLayout{} }

// judgeWorkers returns a collector for cfg and its judge, with the Director's
// workers laid out as layout and BPM's store at store.
func judgeWorkers(cfg *config.CPIConfig, doctor int, store string, layout workerLayout) (*haCollector, cacheJudge) {
	c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}},
		workers: func() workerLayout { return layout }}
	j := c.cacheJudge()
	j.bpmStore = store
	return c, j
}

// fakeWorker is a look inside a BPM worker that always finds found, or fails
// with err.
func fakeWorker(found workerMount, err error) func(string, string) (workerMount, error) {
	return func(string, string) (workerMount, error) { return found, err }
}

// otherDoctor is a user the test process is not, standing in for the doctor
// running as root while the files belong to the CPI's user, which here is the
// test's own user.
func otherDoctor(t *testing.T) int {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the test needs to run as a user other than root, so root can stand in for the doctor")
	}
	return 0
}

// The doctor usually runs as root, and the private checks require the user
// that runs them as the owner. It judges as the owner of the allocation
// journal directory, which is the CPI's user, so a healthy directory that user
// owns gives no finding, and a real refusal is still reported.
func TestCollectJudgesTheCacheDirectoryAsTheCPIsUser(t *testing.T) {
	doctor := otherDoctor(t)
	t.Setenv("TMPDIR", t.TempDir())

	t.Run("private journal sibling owned by the CPI's user", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o700)
		makeDir(t, journal)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		wantNone(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory)
	})

	t.Run("private override owned by the CPI's user", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0)
		makeDir(t, journal)
		override := filepath.Join(t.TempDir(), "cache")
		makeDir(t, override)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = override
		wantNone(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory)
	})

	t.Run("journal sibling with a wide mode", func(t *testing.T) {
		journal, sibling := journalCacheLayout(t, 0o755)
		makeDir(t, journal)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		want(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory, haSeverityAdvice, sibling, "mode 0755",
			"dead-node handover cannot start")
	})
}

// The temporary directory's name carries the user it belongs to, and it sits
// under the CPI's own temporary directory, so a doctor that is not the CPI's
// user says nothing about it. One that is the CPI's user still reports it.
func TestCollectLeavesTheTemporaryDirectoryToTheCPIsUser(t *testing.T) {
	doctor := otherDoctor(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	for _, uid := range []int{doctor, os.Geteuid()} {
		path := filepath.Join(tmp, "pve_cpi-endpoint-cache."+itoa(uid))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // the test makes the directory unsafe
			t.Fatal(err)
		}
	}
	journal, sibling := journalCacheLayout(t, 0)
	makeDir(t, journal)
	// The group may write beside the journal, so the CPI does not create the
	// directory there and the temporary layer applies.
	if err := os.Chmod(filepath.Dir(journal), 0o770); err != nil {
		t.Fatal(err)
	}
	withJournal := haCfg("auto", "none")
	withJournal.StorageAllocationJournalDir = journal

	r := cacheFindingsAs(withJournal, doctor)
	want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "does not create it because",
		"temporary cache directory")
	for _, f := range findings(r, haKindCacheDirectory) {
		if strings.Contains(f.Subject, "pve_cpi-endpoint-cache.") || strings.Contains(f.Detail, "mode 0755") {
			t.Errorf("a doctor that is not the CPI's user judged the temporary directory: %+v", f)
		}
	}
	wantNone(t, cacheFindingsAs(haCfg("auto", "none"), doctor), haKindCacheDirectory)
	want(t, cacheFindingsAs(haCfg("auto", "none"), os.Geteuid()), haKindCacheDirectory, haSeverityAdvice,
		"pve_cpi-endpoint-cache."+itoa(os.Geteuid()), "mode 0755")
}

// A missing directory the CPI would create on first use is reported as such,
// judged as the owner of the journal directory, and one it would not create
// names the reason, so the operator knows what the next call does.
func TestCollectSaysWhetherTheCPICreatesAMissingDirectory(t *testing.T) {
	doctor := otherDoctor(t)
	t.Setenv("TMPDIR", t.TempDir())
	cpiUser := "uid " + itoa(os.Geteuid())

	t.Run("directory beside the journal is created", func(t *testing.T) {
		journal, sibling := journalCacheLayout(t, 0)
		makeDir(t, journal)
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		r := cacheFindingsAs(cfg, doctor)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "creates it with mode 0700 on first use",
			cpiUser, "same mount as the allocation journal directory", "director.cpi_additional_volumes")
		if n := len(findings(r, haKindCacheDirectory)); n != 1 {
			t.Errorf("%d findings, want one: %+v", n, r.Findings)
		}
		if _, err := os.Lstat(sibling); !os.IsNotExist(err) {
			t.Errorf("the doctor created the directory: %v", err)
		}
	})

	t.Run("directory beside the journal is not created", func(t *testing.T) {
		journal, sibling := journalCacheLayout(t, 0)
		makeDir(t, journal)
		if err := os.Chmod(filepath.Dir(journal), 0o770); err != nil {
			t.Fatal(err)
		}
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		want(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory, haSeverityAdvice, sibling,
			"does not create it because its parent directory", "mode 0770", "cpi provision-journal")
	})

	t.Run("override is created", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o700)
		makeDir(t, journal)
		override := filepath.Join(filepath.Dir(journal), "override")
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = override
		r := cacheFindingsAs(cfg, doctor)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, override, "creates it with mode 0700 on first use",
			cpiUser, "same mount as the allocation journal directory", "restarts once")
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "judged as its own user") {
			t.Errorf("the journal names the CPI's user, so the finding must not say otherwise: %q", got)
		}
		if _, err := os.Lstat(override); !os.IsNotExist(err) {
			t.Errorf("the doctor created the directory: %v", err)
		}
	})

	t.Run("override judged as a user that does not own the journal is not created", func(t *testing.T) {
		journal, _ := journalCacheLayout(t, 0o700)
		makeDir(t, journal)
		override := filepath.Join(filepath.Dir(journal), "override")
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = override
		c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}}}
		j := c.cacheJudge()
		j.uid = os.Geteuid() + 1
		j.ancestors = func(string, int) (*searchBlock, error) { return nil, nil }
		c.cacheFindingsAs(j)
		want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, override,
			"does not create it because the allocation journal directory", "belongs to "+cpiUser,
			"it would be made by uid "+itoa(os.Geteuid()+1))
	})

	// storeLayout lays out a store with the journal in pve_cpi and the
	// Director's own directory beside it, as a Director does on one disk.
	storeLayout := func(t *testing.T) (store, journal string) {
		store = t.TempDir()
		for _, dir := range []string{"pve_cpi", "director"} {
			makeDir(t, filepath.Join(store, dir))
		}
		journal = filepath.Join(store, "pve_cpi", "allocations")
		makeDir(t, journal)
		return store, journal
	}
	overrideCfg := func(journal, override string) *config.CPIConfig {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = override
		return cfg
	}
	notCreated := func(t *testing.T, r *haReport, override string, fragments ...string) {
		t.Helper()
		want(t, r, haKindCacheDirectory, haSeverityAdvice, append([]string{override}, fragments...)...)
		for _, f := range findings(r, haKindCacheDirectory) {
			if strings.Contains(f.Detail, "creates it with mode 0700") {
				t.Errorf("the finding says the CPI creates a directory its workers see on another mount: %+v", f)
			}
		}
		if _, err := os.Lstat(override); !os.IsNotExist(err) {
			t.Errorf("the doctor created the directory: %v", err)
		}
	}

	t.Run("override in another BPM volume is not created", func(t *testing.T) {
		store, journal := storeLayout(t)
		override := filepath.Join(store, "director", "pve-cache")
		c, j := judgeWorkers(overrideCfg(journal, override), doctor, store, workerLayout{mode: workersUnderBPM})
		c.cacheFindingsAs(j)
		notCreated(t, c.rep, override, "does not create it because the Director runs its workers under BPM, and "+
			"its parent directory "+filepath.Join(store, "director")+" is outside "+filepath.Join(store, "pve_cpi"),
			"a mount of its own", "although this host shows one")

		inside := filepath.Join(store, "pve_cpi", "pve-cache")
		c, j = judgeWorkers(overrideCfg(journal, inside), doctor, store, workerLayout{mode: workersUnderBPM})
		c.cacheFindingsAs(j)
		want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, inside, "creates it with mode 0700 on first use",
			"the Director runs its workers under BPM",
			"inside the same volume as the allocation journal directory")
	})

	// Outside BPM the workers see the host's mounts, so the volume rule does
	// not apply and nothing about BPM is said.
	t.Run("override beside the journal's volume is created when the workers run outside BPM", func(t *testing.T) {
		store, journal := storeLayout(t)
		override := filepath.Join(store, "director", "pve-cache")
		c, j := judgeWorkers(overrideCfg(journal, override), doctor, store, workerLayout{mode: workersOnHost})
		c.cacheFindingsAs(j)
		want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, override, "creates it with mode 0700 on first use",
			"same mount as the allocation journal directory; the HA recovery episode record")
		if got := findings(c.rep, haKindCacheDirectory)[0].Detail; strings.Contains(got, "BPM") {
			t.Errorf("with the workers outside BPM the finding must not talk about BPM: %q", got)
		}
	})

	// When the doctor cannot tell where the workers run, it says what happens
	// in both cases.
	t.Run("override in another BPM volume is described for both cases when the workers are unknown",
		func(t *testing.T) {
			store, journal := storeLayout(t)
			override := filepath.Join(store, "director", "pve-cache")
			c, j := judgeWorkers(overrideCfg(journal, override), doctor, store, workerLayout{})
			c.cacheFindingsAs(j)
			want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, override,
				"creates it with mode 0700 on first use", "cannot tell whether the Director runs its workers "+
					"under BPM", "director.use_bpm_for_workers is true", "if it does, the CPI does not create it, "+
					"because its parent directory "+filepath.Join(store, "director")+" is outside",
				"create it with mode 0700 for the CPI's user")
		})

	// A look inside a running worker decides over the volume rule, so an
	// operator who lists the whole store as a volume gets the creation, and a
	// worker that cannot see the parent at all is named.
	t.Run("a look inside a running BPM worker decides", func(t *testing.T) {
		store, journal := storeLayout(t)
		override := filepath.Join(store, "director", "pve-cache")
		cfg := overrideCfg(journal, override)

		c, j := judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM,
			inWorker: fakeWorker(workerSameMount, nil)})
		c.cacheFindingsAs(j)
		want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, override, "creates it with mode 0700 on first use",
			"same mount as the allocation journal directory; the Director runs its workers under BPM, and before "+
				"BPM v1.4.21 the seccomp profile refuses the statx call", "so on an older BPM the CPI does not "+
				"create it; the HA recovery episode record")

		c, j = judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM,
			inWorker: fakeWorker(workerOtherMount, nil)})
		c.cacheFindingsAs(j)
		notCreated(t, c.rep, override, "inside a running worker its parent directory "+
			filepath.Join(store, "director")+" is not on the same mount as the allocation journal directory "+journal)

		c, j = judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM,
			inWorker: fakeWorker(workerNoParent, nil)})
		c.cacheFindingsAs(j)
		notCreated(t, c.rep, override, "inside a running worker its parent directory "+
			filepath.Join(store, "director")+" does not exist, because no volume the worker sees holds it")

		// A worker that cannot be looked into leaves the volume rule.
		c, j = judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM,
			inWorker: fakeWorker(0, errors.New("permission denied"))})
		c.cacheFindingsAs(j)
		notCreated(t, c.rep, override, "is outside "+filepath.Join(store, "pve_cpi"))
	})

	// A journal directly below the store is a volume of its own under BPM, so
	// the directory beside it, in the store itself, is not created there.
	t.Run("directory beside a journal that is its own BPM volume is not created", func(t *testing.T) {
		store := t.TempDir()
		journal := filepath.Join(store, "allocations")
		makeDir(t, journal)
		sibling := filepath.Join(store, "endpoint-cache")
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		c, j := judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM})
		c.cacheFindingsAs(j)
		var found bool
		for _, f := range findings(c.rep, haKindCacheDirectory) {
			if f.Subject != sibling {
				continue
			}
			found = true
			for _, frag := range []string{"does not create it because the Director runs its workers under BPM",
				"temporary cache directory instead", "mount_only: true"} {
				if !strings.Contains(f.Detail, frag) {
					t.Errorf("%+v, want it to mention %q", f, frag)
				}
			}
		}
		if !found {
			t.Fatalf("no finding for %s: %+v", sibling, c.rep.Findings)
		}

		c, j = judgeWorkers(cfg, doctor, store, workerLayout{mode: workersUnderBPM,
			inWorker: fakeWorker(workerSameMount, nil)})
		c.cacheFindingsAs(j)
		want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, sibling, "creates it with mode 0700 on first use")
	})
}

// With no journal to name the CPI's user, a directory that fails only because
// another user owns it is judged again as its owner. It passes, so the finding
// says the CPI uses it only when it runs as that owner, and does not call it
// refused.
func TestCollectJudgesAnOverrideAsItsOwnerWhenTheCPIsUserIsUnknown(t *testing.T) {
	doctor := otherDoctor(t)
	t.Setenv("TMPDIR", t.TempDir())
	override := filepath.Join(t.TempDir(), "cache")
	makeDir(t, override)
	cfg := haCfg("auto", "none")
	cfg.EndpointCacheDir = override
	r := cacheFindingsAs(cfg, doctor)
	owner := "uid " + itoa(os.Geteuid())
	want(t, r, haKindCacheDirectory, haSeverityAdvice, override, "owned by "+owner, "ran as uid 0",
		"confirm "+owner+" is the CPI's user")
	if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "refused") {
		t.Errorf("a directory that passes for its owner must not read as refused: %q", got)
	}

	wide := filepath.Join(t.TempDir(), "cache")
	makeDir(t, wide)
	if err := os.Chmod(wide, 0o750); err != nil { //nolint:gosec // the test makes the directory unsafe
		t.Fatal(err)
	}
	cfg.EndpointCacheDir = wide
	want(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory, haSeverityAdvice, wide, "mode 0750")
}

// A missing override falls back to the layers after it, and the fallback is
// judged the same way. A sibling beside a journal that does not exist yet
// leaves the CPI's user unknown, so it is judged as its owner, and a temporary
// directory a root doctor cannot judge is named as where the record goes.
func TestCollectJudgesTheFallbackOfAMissingOverrideAsTheCPIsUser(t *testing.T) {
	doctor := otherDoctor(t)
	t.Setenv("TMPDIR", t.TempDir())
	missing := filepath.Join(t.TempDir(), "absent")

	t.Run("sibling of a journal that does not exist yet", func(t *testing.T) {
		journal, sibling := journalCacheLayout(t, 0o700)
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = missing
		cfg.StorageAllocationJournalDir = journal
		r := cacheFindingsAs(cfg, doctor)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, missing, "beside the allocation journal",
			"owned by uid "+itoa(os.Geteuid()), "ran as uid 0")
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "cannot be used") {
			t.Errorf("a sibling that passes for its owner must not read as unusable: %q", got)
		}
		if _, err := os.Lstat(sibling); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("temporary directory", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = missing
		want(t, cacheFindingsAs(cfg, doctor), haKindCacheDirectory, haSeverityAdvice, missing,
			"sits in the temporary cache directory")
	})
}

// A directory the doctor cannot read while it judges for another user is not
// reported as refused. The finding says the check could not judge it.
func TestCollectSaysWhenItCouldNotReadADirectoryForTheCPIsUser(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	parent := t.TempDir()
	journal := filepath.Join(parent, "journal")
	makeDir(t, journal)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // restores the test's own directory
	if _, err := os.Lstat(journal); err == nil {
		t.Skip("this user can search a directory with no permissions")
	}
	cfg := haCfg("auto", "none")
	cfg.StorageAllocationJournalDir = journal
	c := &haCollector{cfg: cfg, euid: os.Geteuid() + 1, rep: &haReport{Findings: []haFinding{}}}
	c.cacheFindingsAs(cacheJudge{uid: os.Geteuid(), known: true, doctor: os.Geteuid() + 1})
	want(t, c.rep, haKindCacheDirectory, haSeverityAdvice, "could not be read by this check",
		"uid "+itoa(os.Geteuid()+1), "uid "+itoa(os.Geteuid()))
}

// makeDir creates a private directory.
func makeDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestCollectReportsTheVolumeOfAGoneVM covers a replica copy whose VM is gone:
// PVE purged the job with the VM and left the copy on the target node.
func TestCollectReportsTheVolumeOfAGoneVM(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.volumes["n2/local-zfs"] = map[string]bool{
		"local-zfs:vm-101-disk-0":                true,
		"local-zfs:vm-559-ephemeral-0":           true,
		"local-zfs:vm-560-disk-0":                true,
		"local-zfs:vm-561-bosh-ns-alloc-abc.raw": true,
		"local-zfs:vm-562-state-snap1":           true,
		"local-zfs:vm-9500-disk-0":               true,
		"local-zfs:vm-99-disk-0":                 true,
	}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	got := map[string]haFinding{}
	for _, fd := range findings(r, haKindReplicaGoneVM) {
		got[fd.Subject] = fd
	}
	wantSubjects := []string{
		"local-zfs:vm-559-ephemeral-0", "local-zfs:vm-560-disk-0", "local-zfs:vm-561-bosh-ns-alloc-abc.raw",
	}
	if len(got) != len(wantSubjects) {
		t.Fatalf("gone-VM findings %+v, want exactly %v; a name the CPI does not give, a VMID outside the band, "+
			"or a live VM's volume is not reported", got, wantSubjects)
	}
	for _, subject := range wantSubjects {
		if fd, ok := got[subject]; !ok || fd.Severity != haSeverityAdvice {
			t.Errorf("finding for %s = %+v, want advice", subject, fd)
		}
	}
	if d := got["local-zfs:vm-559-ephemeral-0"].Detail; !strings.Contains(d, "on n2") ||
		!strings.Contains(d, "VMID 559") || !strings.Contains(d, "100 to 8999") {
		t.Errorf("detail %q, want the node, the VMID, and the band", d)
	}
	wantNone(t, r, haKindReplicaOrphanVolume)

	// A copy the journal recorded is reported as recorded, not again.
	g := replicaFake()
	g.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-559-ephemeral-0": true}
	g.journal = JournalView{Configured: true, Records: []aj.Record{{Replication: &aj.Replication{
		Owner: haDirector, Retired: true,
		Job: &aj.ReplicaJob{ID: "559-0", Guest: 559, Source: "n1", Target: "n2", Generation: 1},
		Copies: []aj.ReplicaCopy{{Kind: aj.ReplicaCopyOrphan, Node: "n2", JobID: "559-0",
			VolIDs: []string{"local-zfs:vm-559-ephemeral-0"}, RecordedAt: haNow}},
	}}}}
	r = runCollect(t, g, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaRecordedOrph, haSeverityProblem, "local-zfs:vm-559-ephemeral-0@n2")
	wantNone(t, r, haKindReplicaGoneVM)

	// A VMID a job still names, a volume a read config references, and a run
	// that could not list every node's guests report nothing.
	h := replicaFake()
	h.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-559-ephemeral-0": true}
	h.jobs = append(h.jobs, ha.ReplicaJobState{ID: "559-0", Guest: 559, Source: "n1", Target: "n3",
		Comment: haMarker + " gen=1"})
	wantNone(t, runCollect(t, h, haCfg("auto", "replicate")), haKindReplicaGoneVM)

	k := replicaFake()
	k.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-559-disk-0": true}
	k.configs[101]["scsi1"] = "local-zfs:vm-559-disk-0,size=1G"
	wantNone(t, runCollect(t, k, haCfg("auto", "replicate")), haKindReplicaGoneVM)

	m := replicaFake()
	m.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-559-ephemeral-0": true}
	m.excluded = []string{"n3"}
	wantNone(t, runCollect(t, m, haCfg("auto", "replicate")), haKindReplicaGoneVM)
}

func TestCollectAntiAffinityOffJudgesNoGroup(t *testing.T) {
	t.Parallel()
	group := "bosh-aa-0123456789ab"
	f := haBaseFake()
	f.guests = []pve.GuestRef{
		{VMID: 200, Node: "n1", Tags: "bosh-cpi;" + group},
		{VMID: 201, Node: "n2", Tags: "bosh-cpi;" + group},
	}
	f.memory[200], f.memory[201] = haGiB, haGiB
	f.resources = []HAResourceRow{{SID: "vm:200", State: "started"}, {SID: "vm:201", State: "started"}}
	f.rules = []ha.Rule{{ID: group, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:200"}}}

	// The same cluster is short under anti-affinity on.
	want(t, runCollect(t, f, haCfg("auto", "none")), haKindAntiAffinityShort, haSeverityProblem, group, "vm:201")

	// pve.ha.anti_affinity off, or scheduler-soft anti-affinity disabled, means
	// the CPI writes no rule, so no group is short of one.
	off := haCfgWith("auto", "none", `,"anti_affinity":"off"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	wantNone(t, runCollect(t, f, off), haKindAntiAffinityShort, haKindAntiAffinitySkipped, haKindAntiAffinitySplit)
	wantNone(t, runCollect(t, f, haCfgWith("auto", "none", "", "")), haKindAntiAffinityShort)
}

func TestCollectReplicaWithoutPinIsNotAwaitingWidening(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.rules = nil
	wantNone(t, runCollect(t, f, haCfg("auto", "replicate")), haKindReplicaAwaiting)

	// The same VM with a pin that names no replica is awaiting the widening.
	g := replicaFake()
	g.rules = []ha.Rule{pinRule(101, haMarker, "n1", "n2")}
	want(t, runCollect(t, g, haCfg("auto", "replicate")), haKindReplicaAwaiting, haSeverityAdvice, "vm:101")
}

func TestCollectLocalPolicyJudgesOnlyTheOwningDirector(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi;bosh-owner--bbbbbbbb-0000")
	none := collectHA(context.Background(), f, haCfg("auto", "hold"), "", haNow)
	want(t, none, haKindLocalUnprotected, haSeverityProblem, "vm:101", "local_policy is hold")

	// A run for another Director leaves the VM to the Director that owns it.
	other := collectHA(context.Background(), f, haCfg("auto", "hold"), "aaaaaaaa-0000", haNow)
	wantNone(t, other, haKindLocalUnprotected)
	mine := collectHA(context.Background(), f, haCfg("auto", "hold"), "BBBBBBBB-0000", haNow)
	want(t, mine, haKindLocalUnprotected, haSeverityProblem, "vm:101", "local_policy is hold")
}

func TestCollectLocalPolicyIsJudgedUnderTheOwningEntry(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi")
	run := func(scope *haEntryScope, policy string) *haReport {
		return collectHAScoped(context.Background(), f, haCfg("auto", policy), "", haNow, scope)
	}

	// An entry whose VM band does not hold the VM says nothing about it.
	wantNone(t, run(&haEntryScope{Band: pve.VMIDBand{Start: 200, End: 299}, Policy: "hold", Leader: true}, "hold"),
		haKindLocalUnprotected)
	want(t, run(&haEntryScope{Band: pve.VMIDBand{Start: 100, End: 199}, Policy: "hold", Leader: true}, "hold"),
		haKindLocalUnprotected, haSeverityProblem, "vm:101", "local_policy is hold")

	// Two entries on one band with different policies cannot say who owns the
	// VM, so the first reports it once without naming either policy as its own.
	band := pve.VMIDBand{Start: 100, End: 199}
	first := run(&haEntryScope{Band: band, Policy: "replicate", Peers: []string{"az2-none"}, PeerPolicies: []string{"none"}, Leader: true}, "replicate")
	want(t, first, haKindLocalUnprotected, haSeverityAdvice, "vm:101", "az2-none", "none or replicate", "cannot tell which entry owns it")
	second := run(&haEntryScope{Band: band, Policy: "none", Peers: []string{"az2"}, PeerPolicies: []string{"replicate"}}, "none")
	wantNone(t, second, haKindLocalUnprotected)
}

// TestCollectNoneVMIsNeverAProblemUnderAnotherDirectorsEntries builds the shape
// of two Directors on one cluster. Director A owns the band 210000 to 210499
// with a hold entry, and Director B owns 210500 to 210999 with a replicate
// entry and a none entry. A healthy none VM sits in B's band.
func TestCollectNoneVMIsNeverAProblemUnderAnotherDirectorsEntries(t *testing.T) {
	t.Parallel()
	entry := func(name, policy string, start, end int) cpiConfigEntry {
		return cpiConfigEntry{Name: name, Type: "pve", Properties: map[string]any{
			"pve_vmid_range_start": start, "pve_vmid_range_end": end,
			"pve_ha": map[string]any{"local_policy": policy},
		}}
	}
	base := haCfg("auto", "none")
	run := func(entries []cpiConfigEntry, i int, policy string) *haReport {
		f := haBaseFake()
		const vmid = 210511
		f.guests = []pve.GuestRef{{VMID: vmid, Node: "n2", Name: "none-vm", Tags: "bosh-cpi", Status: "running"}}
		f.memory[vmid] = 4 * haGiB
		f.configs[vmid] = map[string]any{"cpu": "x86-64-v2-AES", "scsi0": "local-zfs:vm-210511-disk-0,size=10G"}
		f.resources = nil
		scopes := entryScopes(entries, base)
		return collectHAScoped(context.Background(), f, haCfg("auto", policy), "", haNow, scopes[i])
	}
	entries := []cpiConfigEntry{
		entry("a-hold", "hold", 210000, 210499),
		entry("b-replicate", "replicate", 210500, 210999),
		entry("b-none", "none", 210500, 210999),
	}
	// Director A's hold entry does not own the band, so it says nothing.
	wantNone(t, run(entries, 0, "hold"), haKindLocalUnprotected)
	// Director B's two entries disagree, so the first reports one advice and
	// the second stays silent.
	first := run(entries, 1, "replicate")
	want(t, first, haKindLocalUnprotected, haSeverityAdvice, "vm:210511", "b-none", "cannot tell which entry owns it")
	if first.Problems() != 0 {
		t.Errorf("problems = %d, want a healthy none VM to fail nothing", first.Problems())
	}
	wantNone(t, run(entries, 2, "none"), haKindLocalUnprotected)
	// Alone in its band, the none entry still reports advice and never a problem.
	want(t, run(entries[2:], 0, "none"), haKindLocalUnprotected, haSeverityAdvice, "vm:210511")
}

func TestEntryScopesGroupEntriesByClusterAndBand(t *testing.T) {
	t.Parallel()
	base := haCfg("auto", "none")
	entries := []cpiConfigEntry{
		{Name: "az2", Type: "pve", Properties: map[string]any{"pve_ha": map[string]any{"local_policy": "replicate"}}},
		{Name: "az2-none", Type: "pve", Properties: map[string]any{"pve_ha": map[string]any{"local_policy": "none"}}},
		{Name: "far", Type: "pve", Properties: map[string]any{"pve_vmid_range_start": 5000, "pve_vmid_range_end": 5999}},
	}
	got := entryScopes(entries, base)
	if got[0] == nil || got[1] == nil || got[2] == nil {
		t.Fatalf("scopes = %+v, want one for each entry", got)
	}
	if !got[0].Leader || got[1].Leader || !got[2].Leader {
		t.Errorf("leaders = %v %v %v, want true false true", got[0].Leader, got[1].Leader, got[2].Leader)
	}
	if len(got[0].Peers) != 1 || got[0].Peers[0] != "az2-none" || !got[0].ambiguous() {
		t.Errorf("first scope = %+v, want az2-none as an ambiguous peer", got[0])
	}
	if len(got[2].Peers) != 0 || got[2].ambiguous() {
		t.Errorf("far scope = %+v, want no peers", got[2])
	}
}

func TestCollectAntiAffinityAcrossEntriesThatShareABand(t *testing.T) {
	t.Parallel()
	tagged := "bosh-aa-0123456789ab"
	ruled := "bosh-aa-ba9876543210"
	f := haBaseFake()
	f.guests = []pve.GuestRef{
		{VMID: 200, Node: "n1", Tags: "bosh-cpi;" + tagged},
		{VMID: 201, Node: "n2", Tags: "bosh-cpi;" + tagged},
		{VMID: 210, Node: "n1", Tags: "bosh-cpi;" + ruled},
		{VMID: 211, Node: "n2", Tags: "bosh-cpi;" + ruled},
	}
	for _, id := range []int{200, 201, 210, 211} {
		f.memory[id] = haGiB
	}
	f.resources = []HAResourceRow{
		{SID: "vm:200", State: "started"}, {SID: "vm:201", State: "started"},
		{SID: "vm:210", State: "started"}, {SID: "vm:211", State: "started"},
	}
	// The second group has a rule that misses one member, the first has none.
	f.rules = []ha.Rule{{ID: ruled, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:210"}}}
	band := pve.VMIDBand{Start: 200, End: 299}
	run := func(scope *haEntryScope, extra string) *haReport {
		return collectHAScoped(context.Background(), f, haCfgWith("auto", "none", extra, `,"placement":{"anti_affinity":{"enabled":true}}`), "", haNow, scope)
	}
	on := `,"anti_affinity":"on"`
	off := `,"anti_affinity":"off"`

	// One entry, or every entry with rules on, judges both groups.
	shortFor := func(r *haReport, group string) bool {
		for _, fd := range r.Findings {
			if fd.Kind == haKindAntiAffinityShort && fd.Subject == group && fd.Severity == haSeverityProblem {
				return true
			}
		}
		return false
	}
	r := run(nil, on)
	if !shortFor(r, tagged) || !shortFor(r, ruled) {
		t.Errorf("single entry must judge both groups: %+v", r.Findings)
	}
	wantNone(t, r, haKindAntiAffinityAmbiguous)
	agree := &haEntryScope{Band: band, AntiAffinity: true, Peers: []string{"b"}, PeerAntiAffinity: []bool{true}, Leader: true}
	if r = run(agree, on); !shortFor(r, tagged) || !shortFor(r, ruled) {
		t.Errorf("agreeing entries must judge both groups: %+v", r.Findings)
	}

	// Every entry with rules off judges nothing.
	allOff := &haEntryScope{Band: band, Peers: []string{"b"}, PeerAntiAffinity: []bool{false}, Leader: true}
	wantNone(t, run(allOff, off), haKindAntiAffinityShort, haKindAntiAffinityAmbiguous)

	// Entries that disagree cannot tell who made the group with no rule, so it
	// gets one advice that names the peers, while the group whose rule exists
	// but is short is still a problem.
	mixed := &haEntryScope{Band: band, AntiAffinity: true, Peers: []string{"az2-p4"}, PeerAntiAffinity: []bool{false}, Leader: true}
	r = run(mixed, on)
	want(t, r, haKindAntiAffinityAmbiguous, haSeverityAdvice, band.String(), "az2-p4", "pve.ha.anti_affinity")
	want(t, r, haKindAntiAffinityShort, haSeverityProblem, ruled, "vm:211")
	if shortFor(r, tagged) {
		t.Errorf("group %s has no rule and must not be judged: %+v", tagged, r.Findings)
	}

	// The rules-off leader and a non-leader report the same once, or not at all.
	offLeader := &haEntryScope{Band: band, Peers: []string{"az2"}, PeerAntiAffinity: []bool{true}, Leader: true}
	r = run(offLeader, off)
	want(t, r, haKindAntiAffinityAmbiguous, haSeverityAdvice, "az2")
	want(t, r, haKindAntiAffinityShort, haSeverityProblem, ruled, "vm:211")
	follower := &haEntryScope{Band: band, AntiAffinity: true, Peers: []string{"az2-p4"}, PeerAntiAffinity: []bool{false}}
	wantNone(t, run(follower, on), haKindAntiAffinityShort, haKindAntiAffinityAmbiguous)
}

func TestEntryScopesRecordWhichEntriesWriteAntiAffinityRules(t *testing.T) {
	t.Parallel()
	base := haCfg("auto", "none")
	entries := []cpiConfigEntry{
		{Name: "az2", Type: "pve", Properties: map[string]any{"pve_ha": map[string]any{"anti_affinity": "on"}}},
		{Name: "az2-p4", Type: "pve", Properties: map[string]any{"pve_ha": map[string]any{"anti_affinity": "off"}}},
		{Name: "far", Type: "pve", Properties: map[string]any{"pve_vmid_range_start": 5000, "pve_vmid_range_end": 5999}},
	}
	got := entryScopes(entries, base)
	if !got[0].antiAffinityAmbiguous() || !got[1].antiAffinityAmbiguous() {
		t.Errorf("scopes = %+v %+v, want both ambiguous", got[0], got[1])
	}
	if !got[0].AntiAffinity || got[1].AntiAffinity {
		t.Errorf("writes = %v %v, want true false", got[0].AntiAffinity, got[1].AntiAffinity)
	}
	if got[2].antiAffinityAmbiguous() {
		t.Errorf("far scope = %+v, want no disagreement", got[2])
	}
}

func TestCollectCPUPinAgainstDomain(t *testing.T) {
	t.Parallel()
	pinRule := func(strict bool, nodes ...string) []ha.Rule {
		m := map[string]int{}
		for _, n := range nodes {
			m[n] = 0
		}
		return []ha.Rule{{ID: haPinRulePrefix + "100", Type: ha.RuleTypeNodeAffinity, Strict: strict, Nodes: m,
			Comment: haMarker, Resources: []string{"vm:100"}}}
	}
	tests := []struct {
		name      string
		rules     []ha.Rule
		noHA      bool
		kind      string
		severity  string
		fragments []string
		absent    []string
	}{
		{name: "crossing strict pin", rules: pinRule(true, "n1", "n3"), kind: haKindCPUPinCrossesDomain,
			severity: haSeverityProblem, fragments: []string{"vm:100", haPinRulePrefix + "100", "n3", "bosh recreate", "n1, n2"}},
		{name: "non-strict pin naming every node", rules: pinRule(false, "n1", "n2", "n3"), kind: haKindCPUPinCrossesDomain,
			severity: haSeverityProblem, fragments: []string{"vm:100", haPinRulePrefix + "100", "n3"}, absent: []string{"n1, n2, n3"}},
		{name: "confined pin", rules: pinRule(true, "n1"), kind: haKindCPUHostMulti,
			severity: haSeverityAdvice, fragments: []string{haPinRulePrefix + "100", "only nodes inside"}},
		{name: "no HA resource", noHA: true, kind: haKindCPUHostMulti,
			severity: haSeverityAdvice, fragments: []string{"not under HA"}, absent: []string{"pin"}},
		{name: "resource without a pin", kind: haKindCPUHostMulti,
			severity: haSeverityProblem, fragments: []string{"no enabled node-affinity pin", "bosh recreate"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.profile.CPUDomains = []ha.CPUDomain{
				{Model: "EPYC", Nodes: []string{"n1", "n2"}},
				{Model: "XEON", Nodes: []string{"n3"}},
			}
			f.configs[100]["cpu"] = "host"
			f.rules = tc.rules
			if tc.noHA {
				f.resources = nil
			}
			r := runCollect(t, f, haCfg("auto", "none"))
			want(t, r, tc.kind, tc.severity, tc.fragments...)
			other := haKindCPUHostMulti
			if tc.kind == haKindCPUHostMulti {
				other = haKindCPUPinCrossesDomain
			}
			wantNone(t, r, other)
			for _, frag := range tc.absent {
				if got := findings(r, tc.kind)[0].Detail; strings.Contains(got, frag) {
					t.Errorf("detail %q must not mention %q", got, frag)
				}
			}
		})
	}
}

func TestCollectEntryClockBorrowedIsAdviceAfterASecondFailedRead(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Health.EntryNode = "n1"
	f.profile.Health.ClockNode = "n2"
	f.profile.Health.ClockFallback = "the entry node's clock read failed (timedatectl exit code 1), so timestamps are measured on node n2's clock"
	f.clockErr = errFake
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindEntryClock, haSeverityAdvice, "n1", "timedatectl", "n2", "systemd-timedated", "pve.endpoints")
	if f.clockReads != 1 {
		t.Errorf("entry clock reads = %d, want one second read", f.clockReads)
	}
}

func TestCollectEntryClockOneOffErrorReportsNothing(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Health.EntryNode = "n1"
	f.profile.Health.ClockNode = "n2"
	f.profile.Health.ClockFallback = "the entry node's clock read failed (500), so timestamps are measured on node n2's clock"
	r := runCollect(t, f, haCfg("auto", "none"))
	wantNone(t, r, haKindEntryClock)
	if f.clockReads != 1 {
		t.Errorf("entry clock reads = %d, want one second read", f.clockReads)
	}
}

func TestCollectEntryClockNoClockAnsweredIsAProblem(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Health.EntryNode = "n1"
	f.profile.Health.HA = pve.HAViewUnreadable
	f.profile.Health.HAReason = "entry node clock unreadable, so timestamps cannot be measured: 500"
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindEntryClock, haSeverityProblem, "n1", "no node's clock answered", "observe-only")
}

func TestCollectEntryClockHealthyEntryReportsNothing(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	r := runCollect(t, f, haCfg("auto", "none"))
	wantNone(t, r, haKindEntryClock)
	if f.clockReads != 0 {
		t.Errorf("entry clock reads = %d, want none for a healthy entry", f.clockReads)
	}
}

// A VM whose vm_type set target_node and whose resource carries auto-rebalance
// off under pve.ha.dlb.enabled is healthy. The doctor reads no per-resource
// auto-rebalance flag, so it reports nothing about it.
func TestCollectAcceptsATargetedMemberUnderDLB(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.CRS = ha.CRS{Known: true, Mode: "dynamic", AutoRebalance: true}
	cfg := haCfg("auto", "none")
	on := true
	cfg.HA.DLB = &config.HADLBConfig{Enabled: &on}
	r := runCollect(t, f, cfg)
	if len(r.Findings) != 0 {
		t.Fatalf("a healthy resource gets no finding: %+v", r.Findings)
	}
}

// TestCollectAntiAffinityJudgesOnlyGroupsInTheEntryBand: a group whose members
// all lie in another entry's band was made under that entry's policy, so an
// entry that writes rules in its own band does not report it short.
func TestCollectAntiAffinityJudgesOnlyGroupsInTheEntryBand(t *testing.T) {
	t.Parallel()
	mine := "bosh-aa-0123456789ab"
	theirs := "bosh-aa-ba9876543210"
	f := haBaseFake()
	f.guests = []pve.GuestRef{
		{VMID: 1100, Node: "n1", Tags: "bosh-cpi;" + mine},
		{VMID: 1101, Node: "n2", Tags: "bosh-cpi;" + mine},
		{VMID: 2100, Node: "n1", Tags: "bosh-cpi;" + theirs},
		{VMID: 2101, Node: "n2", Tags: "bosh-cpi;" + theirs},
	}
	for _, id := range []int{1100, 1101, 2100, 2101} {
		f.memory[id] = haGiB
	}
	f.resources = []HAResourceRow{
		{SID: "vm:1100", State: "started"}, {SID: "vm:1101", State: "started"},
		{SID: "vm:2100", State: "started"}, {SID: "vm:2101", State: "started"},
	}
	f.rules = []ha.Rule{
		{ID: mine, Type: "resource-affinity", Affinity: "negative", Comment: haMarker, Resources: []string{"vm:1100"}},
	}
	scope := &haEntryScope{Band: pve.VMIDBand{Start: 1000, End: 1999}, AntiAffinity: true, Leader: true}
	cfg := haCfgWith("auto", "none", `,"anti_affinity":"on"`, `,"placement":{"anti_affinity":{"enabled":true}}`)
	r := collectHAScoped(context.Background(), f, cfg, "", haNow, scope)
	var short []string
	for _, fd := range r.Findings {
		if fd.Kind == haKindAntiAffinityShort {
			short = append(short, fd.Subject)
		}
	}
	if len(short) != 1 || short[0] != mine {
		t.Errorf("short groups = %v, want only %s", short, mine)
	}
}

// TestCollectSkipsGoneVMVolumesWhenAnOwnedConfigCouldNotBeRead: a volume that
// the unread config might reference cannot be called a leftover, so the check
// is skipped and the skip is named as unchecked.
func TestCollectSkipsGoneVMVolumesWhenAnOwnedConfigCouldNotBeRead(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-559-disk-0": true}
	f.configErr = map[int]error{101: errors.New("config read failed")}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	wantNone(t, r, haKindReplicaGoneVM)
	named := false
	for _, fd := range findings(r, haKindUnchecked) {
		named = named || (fd.Subject == "replica_gone_vm_volume" && strings.Contains(fd.Detail, "skipped"))
	}
	if !named {
		t.Errorf("the skipped check must be named as unchecked: %+v", r.Findings)
	}
}

// TestCollectReadsTheDrainFromTheServicesOnTheNode covers the LRM's own state.
// PVE puts the LRM into state maintenance the moment maintenance is requested,
// and it keeps the agent lock and the watchdog until no service is left on the
// node, so the entry reads maintenance/maintenance for the whole drain.
func TestCollectReadsTheDrainFromTheServicesOnTheNode(t *testing.T) {
	t.Parallel()
	setState := func(f *fakeHA, sid, state string) {
		for i := range f.resources {
			if f.resources[i].SID == sid {
				f.resources[i].State = state
			}
		}
	}
	tests := []struct {
		name      string
		build     func() *fakeHA
		wantKinds map[string]string // kind to the subject it must name
		wantDrain bool
		wantDog   string
		wantText  string
	}{
		{
			name:      "a stuck hold VM keeps the drain running",
			build:     func() *fakeHA { f := drainFake("started"); setState(f, "vm:217", "stopped"); return f },
			wantKinds: map[string]string{haKindHoldMaintenance: "vm:40"},
			wantDog:   "armed",
			wantText:  "PVE cannot drain the node",
		},
		{
			name:      "a full anti-affinity group member that cannot leave keeps the drain running",
			build:     func() *fakeHA { f := drainFake("stopped"); return f },
			wantKinds: map[string]string{haKindMaintenanceStranded: "vm:217"},
			wantDog:   "armed",
			wantText:  "anti-affinity group",
		},
		{
			name: "a VM that is still moving is reported while it runs on the node",
			build: func() *fakeHA {
				f := drainFake("stopped")
				f.clusterVMs = f.clusterVMs[:1]
				return f
			},
			wantKinds: map[string]string{haKindMaintenanceStranded: "vm:217"},
			wantDog:   "armed",
		},
		{
			name: "a running HA container is the only service left on the node",
			build: func() *fakeHA {
				f := drainFake("stopped")
				setState(f, "vm:217", "stopped")
				f.resources = append(f.resources, HAResourceRow{SID: "ct:500", State: "started", Comment: haMarker})
				f.serviceNodes = map[string]string{
					"vm:117": "n1", "vm:217": "n2", "vm:317": "n3", "vm:40": "n2", "ct:500": "n2",
				}
				return f
			},
			wantDog: "armed",
		},
		{
			name: "an unreadable manager status is never read as drained",
			build: func() *fakeHA {
				f := drainFake("stopped")
				setState(f, "vm:217", "stopped")
				f.serviceNodesErr = errors.New("manager status unavailable")
				return f
			},
			wantDog: "unknown",
		},
		{
			name: "a service with no node in the manager status is never read as drained",
			build: func() *fakeHA {
				f := drainFake("stopped")
				setState(f, "vm:217", "stopped")
				f.resources = append(f.resources, HAResourceRow{SID: "ct:500", State: "started", Comment: haMarker})
				f.serviceNodes = map[string]string{
					"vm:117": "n1", "vm:217": "n2", "vm:317": "n3", "vm:40": "n2", "ct:500": "",
				}
				return f
			},
			wantDog: "unknown",
		},
		{
			name: "a node with no started service has finished draining",
			build: func() *fakeHA {
				f := drainFake("stopped")
				setState(f, "vm:217", "stopped")
				return f
			},
			wantDrain: true,
			wantDog:   "idle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := runCollect(t, tt.build(), haCfg("auto", "hold"))
			for kind, subject := range tt.wantKinds {
				got := findings(r, kind)
				if len(got) != 1 || got[0].Subject != subject || !strings.Contains(got[0].Detail, tt.wantText) {
					t.Errorf("%s: want one finding for %s mentioning %q, got %+v", kind, subject, tt.wantText, got)
				}
				if got := findings(r, kind); len(got) == 1 && got[0].Severity != haSeverityProblem {
					t.Errorf("%s: severity %s, want problem", kind, got[0].Severity)
				}
			}
			if len(tt.wantKinds) == 0 {
				wantNone(t, r, haKindHoldMaintenance, haKindMaintenanceStranded)
			}
			n := r.Nodes[1]
			if n.DrainComplete != tt.wantDrain || n.Watchdog != tt.wantDog {
				t.Errorf("node line = %+v, want drain_complete=%v and watchdog=%s", n, tt.wantDrain, tt.wantDog)
			}
		})
	}
}
