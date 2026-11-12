package main

import (
	"context"
	"errors"
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
	want(t, r, haKindCPUHostMulti, haSeverityAdvice, "vm:100", "host", "2 CPU migration domains", "n1, n2")
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

func replicaFake() *fakeHA {
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi;bosh-ha-managed")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker})
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
	// has not been widened: that is a wait for the next call, not a fault.
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "synced at", "reboot_vm or set_vm_metadata")
	wantNone(t, r, haKindReplicaNotRecover)

	// With no pin token and no journal proof, the VM is awaiting the same way.
	g := replicaFake()
	g.rules = []ha.Rule{pinRule(101, haMarker, "n1", "n2")}
	r = runCollect(t, g, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaAwaiting, haSeverityAdvice, "vm:101", "reboot_vm or set_vm_metadata")
	wantNone(t, r, haKindReplicaNotRecover, haKindReplicaOnPartner, haKindReplicaJobStale)
	if r.Problems() != 0 {
		t.Fatalf("a healthy job awaiting its pin must not fail the run: %+v", r.Findings)
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
	t.Parallel()
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
			"same mount as the allocation journal directory; the HA recovery episode record")

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
