package handlers

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// negativeRule is an enabled negative resource-affinity rule over vmids.
func negativeRule(id string, vmids ...int) ha.Rule {
	r := ha.Rule{ID: id, Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative}
	for _, v := range vmids {
		r.Resources = append(r.Resources, haResourceSid(v))
	}
	slices.Sort(r.Resources)
	return r
}

// drainFatesOf decides the fates of the fake's VMs on pve01.
func drainFatesOf(t *testing.T, f *fakeRebalanceIO) map[int]drainVMFate {
	t.Helper()
	snap, err := f.snapshot(context.Background(), f.profileVal)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]drainVMFate{}
	for _, fate := range decideDrainFates(&config.CPIConfig{}, f.profileVal, snap, "pve01") {
		if _, dup := out[fate.VMID]; dup {
			t.Fatalf("VM %d decided twice", fate.VMID)
		}
		out[fate.VMID] = fate
	}
	return out
}

func wantFate(t *testing.T, fates map[int]drainVMFate, vmid int, kind DrainFate, reason string, targets ...string) {
	t.Helper()
	got, ok := fates[vmid]
	if !ok {
		t.Fatalf("VM %d has no fate", vmid)
	}
	if got.Fate != kind || !strings.Contains(got.Reason, reason) || !slices.Equal(got.Targets, targets) {
		t.Fatalf("VM %d: fate %s reason %q targets %v, want %s with %q and %v", vmid, got.Fate, got.Reason,
			got.Targets, kind, reason, targets)
	}
}

func TestDrainFatesFollowPVERulesAndPlacementFacts(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	f.foreign = map[int]string{}
	f.foreignState = map[int]string{}

	// A shared VM with a flat pin may go to either other node.
	f.addVM(101, "pve01", 4)

	// A hold VM's strict pin names only the drained node.
	f.addVM(102, "pve01", 4)
	f.pins[102] = rbPin(102, map[string]int{"pve01": 0})
	f.pins[102].Strict = true

	// Every other node holds a member of its anti-affinity rule.
	f.addVM(103, "pve01", 4)
	f.addVM(104, "pve02", 4)
	f.addVM(105, "pve03", 4)
	f.extraRules = append(f.extraRules, negativeRule("bosh-aa-one", 103, 104, 105))

	// Two rules name the VM, and neither covers every node alone.
	f.addVM(106, "pve01", 4)
	f.foreign[206], f.foreign[207] = "pve02", "pve03"
	f.extraRules = append(f.extraRules, negativeRule("bosh-aa-a", 106, 206), negativeRule("bosh-aa-b", 106, 207))

	// A stopped partner doesn't hold its node.
	f.addVM(109, "pve01", 4)
	f.foreign[210], f.foreign[211] = "pve02", "pve03"
	f.foreignState[210] = "stopped"
	f.extraRules = append(f.extraRules, negativeRule("bosh-aa-stopped", 109, 210, 211))

	// A rule PVE reports infeasible is not applied.
	f.addVM(117, "pve01", 4)
	f.foreign[217], f.foreign[218] = "pve02", "pve03"
	bad := negativeRule("bosh-aa-bad", 117, 217, 218)
	bad.Errors = []string{"resources: too many"}
	f.extraRules = append(f.extraRules, bad)

	// A node-local disk with no replica, and one with a replica on pve02.
	f.addVM(112, "pve01", 4).cfg["scsi0"] = testPinStorage + ":vm-112-disk-0,size=10G"
	f.addVM(113, "pve01", 4).cfg["scsi0"] = testPinStorage + ":vm-113-disk-0,size=10G"
	f.jobs = []ha.ReplicaJobState{{ID: "113-0", Guest: 113, Target: "pve02", Source: "pve01"}}

	// A positive rule keeps a shared VM with the hold VM 102.
	f.addVM(116, "pve01", 4)
	f.extraRules = append(f.extraRules, ha.Rule{ID: "together", Type: ha.RuleTypeResourceAffinity,
		Affinity: ha.AffinityPositive, Resources: []string{haResourceSid(102), haResourceSid(116)}})

	// No HA resource, a stopped resource, and a VM on another node.
	f.addVM(120, "pve01", 4).noResource = true
	f.addVM(121, "pve01", 4).state = "stopped"
	f.addVM(122, "pve02", 4)

	fates := drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainLeaves, "", "pve02", "pve03")
	wantFate(t, fates, 102, DrainStays, "strict node-affinity rule "+naRuleNameFor(102))
	wantFate(t, fates, 103, DrainStays, "VM 104 on pve02 in bosh-aa-one, and VM 105 on pve03 in bosh-aa-one")
	wantFate(t, fates, 106, DrainStays, "VM 206 on pve02 in bosh-aa-a, and VM 207 on pve03 in bosh-aa-b")
	wantFate(t, fates, 109, DrainLeaves, "", "pve02")
	wantFate(t, fates, 117, DrainLeaves, "", "pve02", "pve03")
	wantFate(t, fates, 112, DrainStays, "node-local storage "+testPinStorage)
	wantFate(t, fates, 113, DrainLeaves, "", "pve02")
	wantFate(t, fates, 116, DrainStays, "positive resource-affinity rule together keeps it with VM 102")
	wantFate(t, fates, 120, DrainNotHA, "no HA resource")
	wantFate(t, fates, 121, DrainNotStarted, "stopped")
	if _, ok := fates[122]; ok {
		t.Fatal("a VM on another node has a fate")
	}
	if _, ok := fates[206]; ok {
		t.Fatal("a guest of no VM of this Director has a fate")
	}
}

func TestDrainFatesLeaveOutNodesThatCantTakeWork(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	f.addVM(101, "pve01", 4)
	f.addVM(102, "pve01", 4)
	f.addVM(103, "pve02", 4)
	f.extraRules = append(f.extraRules, negativeRule("bosh-aa-one", 102, 103))
	f.profileVal.Health.Nodes[2].Maintenance = true
	f.profileVal.Health.Nodes[2].Placeable = false

	fates := drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainLeaves, "", "pve02")
	wantFate(t, fates, 102, DrainStays, "VM 103 on pve02 in bosh-aa-one")

	f.profileVal.Health.Nodes[1].Placeable = false
	fates = drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainStays, "no other node is online, out of maintenance, and able to take work")
}

func TestDrainFatesOfALocalISOOrAnUnreplicatedDriveStay(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	f.addVM(101, "pve01", 4).cfg["ide2"] = testPinStorage + ":vm-101-cloudinit,media=cdrom"
	f.addVM(102, "pve01", 4).cfg["scsi1"] = testPinStorage + ":vm-102-disk-1,size=1G,replicate=0"
	f.jobs = []ha.ReplicaJobState{{ID: "101-0", Guest: 101, Target: "pve02"},
		{ID: "102-0", Guest: 102, Target: "pve02"}}
	fates := drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainStays, "replication doesn't copy it")
	wantFate(t, fates, 102, DrainStays, "replication doesn't copy it")

	// A disabled or faulted job holds no replica.
	f.addVM(103, "pve01", 4).cfg["scsi0"] = testPinStorage + ":vm-103-disk-0,size=10G"
	f.jobs = []ha.ReplicaJobState{{ID: "103-0", Guest: 103, Target: "pve02", Disabled: true},
		{ID: "103-1", Guest: 103, Target: "pve03", Fault: "bad"}}
	fates = drainFatesOf(t, f)
	wantFate(t, fates, 103, DrainStays, "no replica")
}

func TestDrainFatesOfAVMOnStorageOfUnknownKindAreUndecided(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	f.addVM(101, "pve01", 4)
	// A strict pin that names only the drained node decides the VM before
	// its storage matters.
	drHold(f, 102)
	f.profileVal.StoragesErr = "GET /storage: 500 Internal Server Error"
	fates := drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainUndecided, "the storage definitions couldn't be read")
	wantFate(t, fates, 102, DrainStays, "strict node-affinity rule")

	// A storage the list doesn't name leaves only its VM undecided, and a
	// positive partner of that VM can't be decided either.
	f.profileVal.StoragesErr = ""
	f.addVM(103, "pve01", 4).cfg["scsi1"] = "ghost:vm-103-disk-1,size=1G"
	f.addVM(104, "pve01", 4)
	f.extraRules = append(f.extraRules, ha.Rule{ID: "together", Type: ha.RuleTypeResourceAffinity,
		Affinity: ha.AffinityPositive, Resources: []string{haResourceSid(103), haResourceSid(104)}})
	fates = drainFatesOf(t, f)
	wantFate(t, fates, 101, DrainLeaves, "", "pve02", "pve03")
	wantFate(t, fates, 103, DrainUndecided, "storage ghost")
	wantFate(t, fates, 104, DrainUndecided, "positive resource-affinity rule together keeps it with VM 103")
}
