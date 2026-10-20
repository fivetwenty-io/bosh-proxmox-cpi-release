package handlers

import (
	"slices"
	"strings"
	"testing"
)

// coverTestScan is anchor 130 and its replica 131 of one sha8. The replica
// passes every --vmid precondition unless clones is non-empty.
func coverTestScan(clones []int64) StemcellRefsScan {
	return StemcellRefsScan{
		CloneScanComplete: true,
		Templates: []StemcellRefsTemplate{
			{VMID: 130, Node: "pve", Name: "bosh-stemcell-a-1-aaaa1111", SHA8: "aaaa1111",
				DescriptionState: stemcellRefsDescProvenance},
			{VMID: 131, Node: "pve-node2", Name: "bosh-stemcell-a-1-aaaa1111", SHA8: "aaaa1111", Replica: true,
				DescriptionState: stemcellRefsDescProvenance, LinkedClones: clones},
		},
	}
}

// coverTestPlan destroys 130 and carries 131 as a replica that the planner
// said 130's sweep frees.
func coverTestPlan() StemcellRefsPlan {
	return StemcellRefsPlan{Mode: stemcellRefsModeVMID, Actions: []StemcellRefsAction{
		{Kind: StemcellRefsActionDestroy, VMID: 130, Node: "pve", SHA8: "aaaa1111"},
		{Kind: StemcellRefsActionSkip, VMID: 131, Node: "pve-node2", SHA8: "aaaa1111", Replica: true,
			Reason:    "replica of template 130, which this prune destroys; it is freed with template 130 in the same sweep",
			sweptWith: 130},
	}}
}

func TestAttachReplicaSweepsUncoversAReplicaTheSweepLeaves(t *testing.T) {
	t.Parallel()
	plan := coverTestPlan()
	attachReplicaSweeps(coverTestScan([]int64{400}), &plan)

	anchor, replica := plan.Actions[0], plan.Actions[1]
	if len(anchor.SweepReplicas) != 0 || len(anchor.KeptReplicas) != 1 || anchor.KeptReplicas[0].VMID != 131 {
		t.Fatalf("130 action = %+v; want 131 kept, not swept", anchor)
	}
	if replica.Kind != StemcellRefsActionBlocked || !replica.Failure || replica.sweptWith != 0 ||
		!strings.Contains(replica.Reason, "does not sweep it") ||
		!strings.Contains(replica.Reason, "prune --vmid 131 once template 130 is gone") ||
		!slices.Equal(replica.LinkedClones, []int64{400}) {
		t.Fatalf("131 action = %+v; want a failing block that names the clone and the later --vmid", replica)
	}
	if !plan.HasFailures() {
		t.Fatal("plan reports no failures; want the uncovered replica to count")
	}
}

func TestAttachReplicaSweepsKeepsACoveredReplicaTheSweepTakes(t *testing.T) {
	t.Parallel()
	plan := coverTestPlan()
	attachReplicaSweeps(coverTestScan(nil), &plan)

	anchor, replica := plan.Actions[0], plan.Actions[1]
	if len(anchor.SweepReplicas) != 1 || anchor.SweepReplicas[0].VMID != 131 {
		t.Fatalf("130 action = %+v; want it to sweep 131", anchor)
	}
	if replica.Kind != StemcellRefsActionSkip || replica.Failure || replica.sweptWith != 130 {
		t.Fatalf("131 action = %+v; want the non-failing skip kept", replica)
	}
}
