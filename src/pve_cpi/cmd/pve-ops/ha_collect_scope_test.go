package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestCollectScopedRunIgnoresVMsAndJobsOutsideTheBand builds each shape that
// raised a finding for a VM outside the entry's VM ID band when the run named
// no owner. Every shape raises its kind with no scope, which proves the shape
// is real, and none raises it under an entry whose band does not hold VM 101.
func TestCollectScopedRunIgnoresVMsAndJobsOutsideTheBand(t *testing.T) {
	t.Parallel()
	stale := haNow.Add(-time.Hour).Unix()
	cases := []struct {
		name   string
		kind   string
		policy string
		build  func(t *testing.T) *fakeHA
	}{
		{name: "replica awaiting sync", kind: haKindReplicaAwaiting, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.rules = []ha.Rule{pinRule(101, haMarker, "n1", "n2")}
			return f
		}},
		{name: "replica on partner", kind: haKindReplicaOnPartner, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.guests[1].Node = "n2"
			return f
		}},
		{name: "replica not recoverable", kind: haKindReplicaNotRecover, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaChangedKey+"="+itoa(int(stale)), "n1")}
			f.status["101-0"] = ha.ReplicaSyncStatus{}
			return f
		}},
		{name: "replica pin unshaped", kind: haKindReplicaPinUnshaped, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.rules = []ha.Rule{pinRule(101, haMarker+" "+ha.ReplicaPartnerKey+"=n2", "n1")}
			return f
		}},
		{name: "replica job failing", kind: haKindReplicaJobFailing, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour), FailCount: 3, Error: "zfs send failed"}
			return f
		}},
		{name: "replica job stale", kind: haKindReplicaJobStale, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour)}
			return f
		}},
		{name: "replica schedule unjudged", kind: haKindReplicaScheduleSkip, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour)}
			f.schedules["101-0"] = "mon..fri 08:00"
			return f
		}},
		{name: "replica marker encoded", kind: haKindReplicaMarkerEncode, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.jobs[0].Comment = pveEncoded(haMarker+" gen=1", 2)
			return f
		}},
		{name: "replica orphan volume", kind: haKindReplicaOrphanVolume, policy: "replicate", build: func(*testing.T) *fakeHA {
			f := replicaFake()
			f.volumes["n3/local-zfs"] = map[string]bool{"local-zfs:vm-101-disk-0": true}
			return f
		}},
		{name: "managed without resource", kind: haKindManagedNoResource, policy: "none", build: func(*testing.T) *fakeHA {
			f := haBaseFake()
			f.guests[0].VMID = 101
			f.memory = map[int]int64{101: haGiB}
			f.configs = map[int]map[string]any{101: {"cpu": "x86-64-v2-AES"}}
			f.resources = nil
			return f
		}},
		{name: "birth tag stale", kind: haKindBirthTagStale, policy: "none", build: func(*testing.T) *fakeHA {
			f := haBaseFake()
			f.guests[0].VMID = 101
			f.guests[0].Tags = "bosh-cpi;bosh-creating-" + itoa(int(stale))
			f.memory = map[int]int64{101: haGiB}
			f.configs = map[int]map[string]any{101: {"cpu": "x86-64-v2-AES"}}
			f.resources = nil
			return f
		}},
		{name: "readd tag", kind: haKindReaddTag, policy: "none", build: func(*testing.T) *fakeHA {
			f := haBaseFake()
			f.guests[0].VMID = 101
			f.guests[0].Tags = "bosh-cpi;bosh-ha-readd-" + itoa(int(stale))
			f.memory = map[int]int64{101: haGiB}
			f.configs = map[int]map[string]any{101: {"cpu": "x86-64-v2-AES"}}
			f.resources = nil
			return f
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unscoped := collectHAScoped(context.Background(), tc.build(t), haCfg("auto", tc.policy), "", haNow, nil)
			if len(findings(unscoped, tc.kind)) == 0 {
				t.Fatalf("the shape raises no %s without a scope: %+v", tc.kind, unscoped.Findings)
			}
			scope := &haEntryScope{Band: pve.VMIDBand{Start: 210000, End: 210199}, Leader: true}
			scoped := collectHAScoped(context.Background(), tc.build(t), haCfg("auto", tc.policy), "", haNow, scope)
			if got := findings(scoped, tc.kind); len(got) != 0 {
				t.Errorf("a VM outside the entry's band must not raise %s, got %+v", tc.kind, got)
			}
		})
	}
}

// TestCollectScopedRunJudgesOnlyTheBandMembersOfAMixedGroup puts one member of
// an anti-affinity group outside the entry's band. The entry judges the group
// for its own members and leaves the other one to the entry that owns it.
func TestCollectScopedRunJudgesOnlyTheBandMembersOfAMixedGroup(t *testing.T) {
	t.Parallel()
	group := "bosh-aa-0123456789ab"
	f := haBaseFake()
	f.guests, f.resources = nil, nil
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
	scope := &haEntryScope{Band: pve.VMIDBand{Start: 200, End: 202}, Leader: true}
	r := collectHAScoped(context.Background(), f, haCfg("auto", "none"), "", haNow, scope)
	for _, fd := range r.Findings {
		if strings.Contains(fd.Detail, "vm:203") || fd.Subject == "vm:203" {
			t.Errorf("a member outside the entry's band must not be judged: %+v", fd)
		}
	}
}

// TestCollectAdvisesOwnerTagMissingForAVMThatPredatesTheOwnerTag: a VM with the
// bosh-cpi tag and no owner tag is judged as this Director's, and a run that
// names its Director points the operator at adopt-owners.
func TestCollectAdvisesOwnerTagMissingForAVMThatPredatesTheOwnerTag(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n1", "bosh-cpi")
	f.guests = append(f.guests, pve.GuestRef{VMID: 102, Node: "n1", Name: "mine", Tags: "bosh-cpi;bosh-owner--aaaaaaaa-0000", Status: "running"})
	f.memory[102] = haGiB
	f.configs[102] = map[string]any{"cpu": "x86-64-v2-AES"}
	r := collectHA(context.Background(), f, haCfg("auto", "hold"), "aaaaaaaa-0000", haNow)
	got := map[string]haFinding{}
	for _, fd := range findings(r, haKindOwnerTagMissing) {
		got[fd.Subject] = fd
	}
	// VM 100 is the base fixture's VM, which carries no owner tag either.
	if len(got) != 2 || got["vm:100"].Severity != haSeverityAdvice ||
		got["vm:101"].Severity != haSeverityAdvice || !strings.Contains(got["vm:101"].Detail, "cpi stemcell-refs adopt-owners") {
		t.Errorf("want advice for vm:100 and vm:101 only, naming adopt-owners, got %+v", got)
	}
	// The VM is still judged, so its local-disk finding stays.
	want(t, r, haKindLocalUnprotected, haSeverityProblem, "vm:101")
	// A run that names no Director cannot say whose the VM is.
	wantNone(t, collectHA(context.Background(), f, haCfg("auto", "hold"), "", haNow), haKindOwnerTagMissing)
}
