package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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
	f.profile.RulesSupported = false
	r = runCollect(t, f, haCfg("auto", "none"))
	wantNone(t, r, haKindUnchecked)
	want(t, r, haKindAdvice, haSeverityAdvice, "PVE 9.0")
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

func TestCollectReplicaOnPartnerAndNotRecoverable(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.guests[1].Node = "n2"
	f.journal = JournalView{Configured: true, Records: []aj.Record{{Replication: &aj.Replication{
		Owner: haDirector, Job: &aj.ReplicaJob{ID: "101-0", Guest: 101, Source: "n1", Target: "n2", Generation: 1},
	}}}}
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaOnPartner, haSeverityProblem, "vm:101", "partner n2", "from n2 back to n1")
	want(t, r, haKindReplicaNotRecover, haSeverityProblem, "vm:101", "not yet recoverable")

	// With no pin token and no journal proof, the VM is not recoverable either.
	g := replicaFake()
	g.rules = []ha.Rule{pinRule(101, haMarker, "n1", "n2")}
	r = runCollect(t, g, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaNotRecover, haSeverityProblem, "vm:101")
	wantNone(t, r, haKindReplicaOnPartner)
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
	)
	for _, id := range []int{110, 111, 112, 113} {
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
	if nodeLine(haNode{Name: "x", Maintenance: true, Watchdog: "active", Version: "9", CPU: "c", Reason: "why"}) !=
		"x offline placeable=no maintenance=yes watchdog=active pve=9 cpu=c (why)" {
		t.Fatalf("node line = %q", nodeLine(haNode{Name: "x", Maintenance: true, Watchdog: "active", Version: "9", CPU: "c", Reason: "why"}))
	}
}
