package main

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// optOutKindsThatMustNotFire are the findings an opted-out VM could trip only
// because it is out of HA.
var optOutKindsThatMustNotFire = []string{
	haKindManagedNoResource, haKindLocalUnprotected, haKindAntiAffinityShort, haKindAntiAffinitySplit,
	haKindAntiAffinitySkipped, haKindCPUDomainTooSmall, haKindSpreadSkewed,
}

// optOutGuest appends a CPI VM with the tags given on shared storage.
func optOutGuest(f *fakeHA, vmid int, node, tags string) {
	f.guests = append(f.guests, pve.GuestRef{VMID: vmid, Node: node, Name: "vm", Tags: tags, Status: "running"})
	f.memory[vmid] = 4 * haGiB
	f.configs[vmid] = map[string]any{"cpu": "x86-64-v2-AES",
		"scsi0": "nfs1:" + itoa(vmid) + "/vm-" + itoa(vmid) + "-disk-0.raw,size=10G"}
}

// TestCollectOptedOutVM pins that a CPI VM tagged bosh-ha-off gets one
// ha_opt_out advice that names it, and no finding that it would get only
// because it is out of HA, on shared storage, on node-local storage under the
// hold policy, and with a CPU whose domain is too small for HA.
func TestCollectOptedOutVM(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		policy string
		build  func(*fakeHA)
	}{
		"shared storage": {policy: "none", build: func(f *fakeHA) {
			optOutGuest(f, 101, "n2", "bosh-cpi;bosh-ha-off")
		}},
		"local under hold": {policy: "hold", build: func(f *fakeHA) {
			localVM(f, "n2", "bosh-cpi;bosh-ha-off")
		}},
		"cpu domain too small": {policy: "none", build: func(f *fakeHA) {
			optOutGuest(f, 101, "n2", "bosh-cpi;bosh-ha-off")
			f.configs[101]["cpu"] = "host"
			f.profile.CPUDomains = nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			tc.build(f)
			r := runCollect(t, f, haCfg("auto", tc.policy))
			want(t, r, haKindHAOptOut, haSeverityAdvice, "vm:101", "VM 101", "bosh-ha-off")
			wantNone(t, r, optOutKindsThatMustNotFire...)
		})
	}
}

// TestCollectOptedOutVMWithResource pins that a VM tagged bosh-ha-off that
// still has an HA resource is a problem, because the opt-out does not hold.
func TestCollectOptedOutVMWithResource(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	optOutGuest(f, 101, "n2", "bosh-cpi;bosh-ha-off")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker})
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindHAOptOut, haSeverityProblem, "vm:101", "HA resource")
}

// TestCollectOptedOutVMWithoutOwnershipTag pins that a VM the CPI did not
// create gets no ha_opt_out finding, whatever tags it carries.
func TestCollectOptedOutVMWithoutOwnershipTag(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	optOutGuest(f, 101, "n2", "bosh-ha-off")
	wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindHAOptOut)
}

// TestCollectOptedOutGroupMemberIsNotAnHAMember pins that an opted-out member
// of an anti-affinity group does not count toward the group's HA members, so
// a group whose rule holds every member with an HA resource is not reported
// as split or short.
func TestCollectOptedOutGroupMemberIsNotAnHAMember(t *testing.T) {
	t.Parallel()
	const group = "bosh-aa-0123456789ab"
	f := haBaseFake()
	optOutGuest(f, 101, "n1", "bosh-cpi;bosh-ha-managed;"+group)
	optOutGuest(f, 102, "n2", "bosh-cpi;bosh-ha-managed;"+group)
	optOutGuest(f, 103, "n3", "bosh-cpi;bosh-ha-off;"+group)
	f.resources = append(f.resources,
		HAResourceRow{SID: "vm:101", State: "started", Comment: haMarker},
		HAResourceRow{SID: "vm:102", State: "started", Comment: haMarker})
	f.rules = []ha.Rule{{
		ID: group, Type: ha.RuleTypeResourceAffinity, Affinity: "negative", Comment: haMarker,
		Resources: []string{"vm:101", "vm:102"},
	}}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindHAOptOut, haSeverityAdvice, "vm:103")
	wantNone(t, r, optOutKindsThatMustNotFire...)
}
