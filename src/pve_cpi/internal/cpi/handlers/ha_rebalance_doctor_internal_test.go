package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
)

// rbDoctorPlanFixture is the dry run that the tests of pve-ops ha doctor read
// in place of a stub, relative to this package.
var rbDoctorPlanFixture = filepath.Join("..", "..", "..", "cmd", "pve-ops", "testdata", "rebalance_plan.json")

// rbDoctorCluster is a cluster on which a rebalance makes one move of each
// phase. VMs 101 and 102 share pve01 under one anti-affinity rule, VM 103
// fills pve01 further, and VM 104 runs on pve03 outside its AZ z1, whose pin
// only raises priority.
func rbDoctorCluster(opts RebalanceOptions) *rebalanceRun {
	f := newFakeRebalanceIO()
	f.profileVal.ClusterName = "lab"
	rbSkewed(f)
	f.extraRules = append(f.extraRules, ha.Rule{ID: "bosh-aa-0123456789ab", Type: ha.RuleTypeResourceAffinity,
		Affinity: ha.AffinityNegative, Resources: []string{"vm:101", "vm:102"}})
	f.addVM(104, "pve03", 4)
	f.pins[104].Nodes = map[string]int{"pve03": ha.PinPriorityPlaced, "pve01": ha.PinPriorityPreferred,
		"pve02": ha.PinPriorityPreferred}
	r := rbRun(f, opts)
	r.deps.Config.HA.PinAZStrict = new(bool)
	r.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{
		"z1": {"pve01", "pve02"}, "z2": {"pve03"},
	}}
	return r
}

// rbDoctorView keeps the parts of a report that pve-ops ha doctor reads.
func rbDoctorView(rep RebalanceReport) RebalanceReport {
	view := RebalanceReport{Cluster: rep.Cluster, SpreadBefore: rep.SpreadBefore, ExitCode: rep.ExitCode}
	for _, g := range rep.Gates {
		view.Gates = append(view.Gates, RebalanceGate{Name: g.Name, OK: g.OK})
	}
	for i := range rep.Moves {
		m := &rep.Moves[i]
		view.Moves = append(view.Moves, RebalanceMove{VMID: m.VMID, From: m.From, To: m.To, Phase: m.Phase,
			Outcome: m.Outcome})
	}
	return view
}

// TestRebalanceRun_TheDoctorsFixtureIsARealPlan pins that the dry run the
// doctor's tests read is what the planner makes of rbDoctorCluster, under the
// options the doctor passes and under the defaults of pve-ops ha rebalance,
// so the doctor's counts are tested against a real plan and not a stub. Set
// PVE_OPS_REBALANCE_FIXTURE_WRITE=1 to write the fixture again after a change
// to the planner.
func TestRebalanceRun_TheDoctorsFixtureIsARealPlan(t *testing.T) {
	doctor := rbDoctorCluster(RebalanceOptions{MaxMoves: 1 << 16}).run(context.Background())
	cli := rbDoctorCluster(RebalanceOptions{}).run(context.Background())
	got := rbDoctorView(doctor)
	if want := rbDoctorView(cli); !slices.Equal(rbMoves(got.Moves), rbMoves(want.Moves)) ||
		got.SpreadBefore != want.SpreadBefore {
		t.Fatalf("the doctor's dry run plans %v over %v, and the CLI's plans %v over %v",
			rbMoves(got.Moves), got.SpreadBefore, rbMoves(want.Moves), want.SpreadBefore)
	}
	phases := map[harebalance.Phase]int{}
	for _, m := range got.Moves {
		phases[m.Phase]++
	}
	if phases[harebalance.PhaseRestore] != 1 || phases[harebalance.PhaseSeparate] != 1 ||
		phases[harebalance.PhaseSpread] == 0 || got.SpreadBefore <= RebalanceDefaultStart {
		t.Fatalf("the plan %v over a spread of %v lacks a move of each phase or a skew", phases, got.SpreadBefore)
	}
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if os.Getenv("PVE_OPS_REBALANCE_FIXTURE_WRITE") == "1" {
		if err := os.WriteFile(rbDoctorPlanFixture, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := os.ReadFile(rbDoctorPlanFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, data) {
		t.Fatalf("%s differs from the real plan; write it again with PVE_OPS_REBALANCE_FIXTURE_WRITE=1:\n%s",
			rbDoctorPlanFixture, data)
	}
}
