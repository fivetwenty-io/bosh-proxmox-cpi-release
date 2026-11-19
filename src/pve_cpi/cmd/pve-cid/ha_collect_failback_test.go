package main

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// failbackRow returns a pointer to b, the failback flag of a resource row.
func failbackRow(b bool) *bool { return &b }

// TestCollectFailbackOnAReplicateVMIsAProblem reports a replicate VM whose HA
// resource has failback on, because PVE moves such a VM back once its node
// returns and the reversed replication job then overwrites the volumes there.
// The VM is judged a replicate VM by its pin's partner token, by a marked
// replication job, or by its replicate tag.
func TestCollectFailbackOnAReplicateVMIsAProblem(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(f *fakeHA){
		"pin and job": func(*fakeHA) {},
		"job alone":   func(f *fakeHA) { f.rules = nil },
		"pin alone":   func(f *fakeHA) { f.jobs = nil },
		"tag alone": func(f *fakeHA) {
			f.rules, f.jobs = nil, nil
			f.guests[1].Tags = "bosh-cpi;bosh-ha-managed;bosh-ha-replica"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := replicaFake()
			edit(f)
			f.resources[1].Failback = nil
			r := runCollect(t, f, haCfg("auto", "replicate"))
			want(t, r, haKindFailbackOnReplica, haSeverityProblem, "vm:101", "failback on",
				"reverses the replication job", "next set_vm_metadata")
			wantNone(t, r, haKindFailbackDrift)
		})
	}
}

// TestCollectFailbackDriftAdvises reports a CPI resource whose failback flag
// differs from the one the CPI writes for it, as advice, because the next call
// that touches the VM sets the flag.
func TestCollectFailbackDriftAdvises(t *testing.T) {
	t.Parallel()

	// A VM with every volume on shared storage gets failback on.
	f := haBaseFake()
	f.resources[0].Failback = failbackRow(false)
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindFailbackDrift, haSeverityAdvice, "vm:100", "failback off", "every volume is on shared storage")

	// pve.ha.failback false turns the flag off on every resource.
	g := haBaseFake()
	r = runCollect(t, g, haCfgWith("auto", "none", `,"failback":false`, ""))
	want(t, r, haKindFailbackDrift, haSeverityAdvice, "vm:100", "failback on", "pve.ha.failback is false")

	// A VM with a node-local volume gets failback off.
	h := haBaseFake()
	localVM(h, "n1", "bosh-cpi;bosh-ha-managed")
	h.resources = append(h.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker})
	r = runCollect(t, h, haCfg("auto", "hold"))
	want(t, r, haKindFailbackDrift, haSeverityAdvice, "vm:101", "failback on", "local-zfs:vm-101-disk-0")
	wantNone(t, r, haKindFailbackOnReplica)
}

// TestCollectFailbackReportsNothingWhenTheFlagMatches reports nothing for a
// resource whose flag is the one the CPI writes: on for a shared VM, whether
// the row reads it as set or carries none, and off for a replicate VM and for
// any VM under pve.ha.failback false.
func TestCollectFailbackReportsNothingWhenTheFlagMatches(t *testing.T) {
	t.Parallel()
	shared := haBaseFake()
	shared.resources[0].Failback = failbackRow(true)
	replica := replicaFake()
	replica.resources[1].Failback = failbackRow(false)
	keyOff := haBaseFake()
	keyOff.resources[0].Failback = failbackRow(false)
	for name, run := range map[string]func(t *testing.T) *haReport{
		"shared, set":   func(t *testing.T) *haReport { return runCollect(t, shared, haCfg("auto", "none")) },
		"shared, unset": func(t *testing.T) *haReport { return runCollect(t, haBaseFake(), haCfg("auto", "none")) },
		"replicate":     func(t *testing.T) *haReport { return runCollect(t, replica, haCfg("auto", "replicate")) },
		"key false": func(t *testing.T) *haReport {
			return runCollect(t, keyOff, haCfgWith("auto", "none", `,"failback":false`, ""))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wantNone(t, run(t), haKindFailbackOnReplica, haKindFailbackDrift)
		})
	}
}

// TestCollectFailbackLeavesResourcesItCannotJudge reports nothing for a
// resource the CPI does not set the flag of, or whose expected flag the doctor
// cannot settle.
func TestCollectFailbackLeavesResourcesItCannotJudge(t *testing.T) {
	t.Parallel()
	foreign := "bosh-cpi:v1 owner=99999999-2222-3333-4444-555555555555"
	for name, tc := range map[string]struct {
		edit func(f *fakeHA)
		mode string
	}{
		"mode off":               {edit: func(f *fakeHA) { f.profile = haHealthyProfile("off") }, mode: "off"},
		"operator's resource":    {edit: func(f *fakeHA) { f.resources[0].Comment = "operator made this" }},
		"other Director's":       {edit: func(f *fakeHA) { f.resources[0].Comment = foreign }},
		"barrier holds it":       {edit: func(f *fakeHA) { f.resources[0].Comment = haMarker + " bosh-narrow-from=started" }},
		"VM of other Director":   {edit: func(f *fakeHA) { f.guests[0].Tags = "bosh-cpi;bosh-ha-managed;bosh-owner--bbbbbbbb-0000" }},
		"not the CPI's VM":       {edit: func(f *fakeHA) { f.guests[0].Tags = "bosh-ha-managed" }},
		"template":               {edit: func(f *fakeHA) { f.guests[0].Template = true }},
		"storage index unread":   {edit: func(f *fakeHA) { f.storagesErr = errFake }},
		"config unread":          {edit: func(f *fakeHA) { f.configErr = map[int]error{100: errFake} }},
		"rules unread":           {edit: func(f *fakeHA) { f.rulesErr = errFake }},
		"jobs unread":            {edit: func(f *fakeHA) { f.jobsErr = errFake }},
		"storage not in index":   {edit: func(f *fakeHA) { f.configs[100]["scsi1"] = "gone:100/vm-100-disk-1.raw,size=1G" }},
		"strict single-node pin": {edit: func(f *fakeHA) { f.rules = []ha.Rule{pinRule(100, haMarker, "n1")} }},
		"no resource":            {edit: func(f *fakeHA) { f.resources = nil }},
		// A parker stays stopped, so failback never moves it, and no
		// set_vm_metadata or reboot_vm reaches it to set the flag.
		"parker": {edit: func(f *fakeHA) { f.guests[0].Tags = "bosh-cpi;bosh-ha-managed;" + pve.ParkerTag }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.resources = []HAResourceRow{{SID: "vm:100", State: "started", Comment: haMarker, Failback: failbackRow(false)}}
			tc.edit(f)
			mode := tc.mode
			if mode == "" {
				mode = "auto"
			}
			wantNone(t, runCollect(t, f, haCfg(mode, "none")), haKindFailbackOnReplica, haKindFailbackDrift)
		})
	}
}

// TestReaderResourcesReadsTheFailbackFlag reads the failback flag of a list
// row as PVE writes it, a number or a string, and leaves it unset when the row
// carries none.
func TestReaderResourcesReadsTheFailbackFlag(t *testing.T) {
	t.Parallel()
	for raw, exp := range map[string]*bool{
		`{"sid":"vm:1","failback":0}`:   failbackRow(false),
		`{"sid":"vm:2","failback":1}`:   failbackRow(true),
		`{"sid":"vm:3","failback":"0"}`: failbackRow(false),
		`{"sid":"vm:4"}`:                nil,
	} {
		row, err := parseHAResourceRow([]byte(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if got := row.Failback; (got == nil) != (exp == nil) || (got != nil && *got != *exp) {
			t.Errorf("%s failback = %s, want %s", row.SID, flagText(got), flagText(exp))
		}
	}
}

// flagText spells a failback flag for a test message.
func flagText(b *bool) string {
	switch {
	case b == nil:
		return "unset"
	case *b:
		return "on"
	}
	return "off"
}

// TestCollectFailbackLeavesAVMTheDecisionGivesNoSharedHA reports nothing for
// a VM on shared storage that the CPI's HA decision does not register as a
// plain shared VM, such as one whose PCI passthrough device or host CPU type
// ties it to one node. The CPI keeps failback off for such a VM, so drift
// against an expected on would never clear.
func TestCollectFailbackLeavesAVMTheDecisionGivesNoSharedHA(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(f *fakeHA){
		"PCI passthrough": func(f *fakeHA) { f.configs[100]["hostpci0"] = "0000:01:00.0" },
		"host CPU in a one-node CPU domain": func(f *fakeHA) {
			f.configs[100]["cpu"] = "host"
			f.profile.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"n1"}},
				{Model: "XEON", Nodes: []string{"n2", "n3"}}}
			for i := range f.profile.Nodes {
				if f.profile.Nodes[i].Name != "n1" {
					f.profile.Nodes[i].CPU.Model = "XEON"
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.resources[0].Failback = failbackRow(false)
			edit(f)
			wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindFailbackDrift, haKindFailbackOnReplica)
		})
	}
}

// TestCollectFailbackJudgesAPinThatRecordsAReplicaChangeAsReplicate reports
// nothing for a VM whose pin records a change to its replicated volume set and
// names no partner yet, because the CPI treats that token as the replicate
// policy and keeps failback off.
func TestCollectFailbackJudgesAPinThatRecordsAReplicaChangeAsReplicate(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.resources[0].Failback = failbackRow(false)
	f.rules = []ha.Rule{pinRule(100, ha.WithReplicaChanged(haMarker, haNow), "n1", "n2")}
	wantNone(t, runCollect(t, f, haCfg("auto", "replicate")), haKindFailbackDrift, haKindFailbackOnReplica)
}
