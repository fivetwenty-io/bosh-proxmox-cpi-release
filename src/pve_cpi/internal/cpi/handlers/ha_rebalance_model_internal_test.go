package handlers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
)

// rbModelVM observes f and returns the planner's VM vmid and its plan.
func rbModelVM(t *testing.T, r *rebalanceRun, vmid int) (harebalance.VM, *rebalanceVMPlan) {
	t.Helper()
	view, err := r.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range view.model.VMs {
		if view.model.VMs[i].VMID == vmid {
			return view.model.VMs[i], view.plans[vmid]
		}
	}
	t.Fatalf("VM %d is not in the model", vmid)
	return harebalance.VM{}, nil
}

func TestRebalanceModel_EachExclusionNamesItsKind(t *testing.T) {
	cases := []struct {
		name   string
		breakk func(f *fakeRebalanceIO, v *rbVM)
		kind   harebalance.SkipKind
		reason string
	}{
		{"no resource", func(_ *fakeRebalanceIO, v *rbVM) { v.noResource = true }, harebalance.SkipLocalDisk,
			"no HA resource"},
		{"replica tag", func(_ *fakeRebalanceIO, v *rbVM) { v.tags += ";" + haReplicaTag },
			harebalance.SkipReplicate, "replicate VM"},
		{"replica pin token", func(f *fakeRebalanceIO, _ *rbVM) {
			f.pins[101].Comment = ha.WithReplicaPartner(f.pins[101].Comment, "pve02")
		}, harebalance.SkipReplicate, "token of the replicate policy"},
		{"marked", func(f *fakeRebalanceIO, _ *rbVM) { rbMarked(f, 101, "pve02") },
			harebalance.SkipRebalancePending, "moves it to pve02"},
		{"failback hold", func(f *fakeRebalanceIO, v *rbVM) {
			v.comment = ha.WithFailbackHold(v.comment, f.clock.Add(-time.Minute))
		}, harebalance.SkipFailbackHeld, "fresh failback hold"},
		{"narrowing", func(_ *fakeRebalanceIO, v *rbVM) {
			v.comment += " bosh-narrow-from=started"
			v.state = "ignored"
		}, harebalance.SkipNarrowing, "narrowing barrier"},
		{"requested stopped", func(_ *fakeRebalanceIO, v *rbVM) { v.state = "stopped" },
			harebalance.SkipNotStarted, "requests stopped"},
		{"manager reports error", func(_ *fakeRebalanceIO, v *rbVM) { v.raw = "error" },
			harebalance.SkipNotStarted, "reports it as error"},
		{"config lock", func(_ *fakeRebalanceIO, v *rbVM) { v.cfg["lock"] = "backup" },
			harebalance.SkipLocked, `"backup"`},
		{"DLB member", func(_ *fakeRebalanceIO, v *rbVM) { v.dlb = true }, harebalance.SkipDLB, "DLB member"},
		{"positive affinity", func(f *fakeRebalanceIO, _ *rbVM) {
			f.extraRules = append(f.extraRules, ha.Rule{ID: "keep-together", Type: ha.RuleTypeResourceAffinity,
				Affinity: ha.AffinityPositive, Resources: []string{"vm:101", "vm:900"}})
		}, harebalance.SkipPositiveAffinity, "positive resource affinity"},
		{"no pin", func(f *fakeRebalanceIO, _ *rbVM) { delete(f.pins, 101) }, harebalance.SkipUndecided,
			"no enabled pin"},
		{"disabled pin", func(f *fakeRebalanceIO, _ *rbVM) { f.pins[101].Disabled = true },
			harebalance.SkipUndecided, "no enabled pin"},
		{"pin of another Director", func(f *fakeRebalanceIO, _ *rbVM) {
			f.pins[101].Comment = ha.WithVolumeRecord(haIdentity{Owner: "dir-2"}.marker(), []string{"nfs"})
		}, harebalance.SkipUndecided, "no enabled pin"},
		{"hold pin", func(f *fakeRebalanceIO, _ *rbVM) {
			f.pins[101].Nodes, f.pins[101].Strict = map[string]int{"pve01": 0}, true
		}, harebalance.SkipHold, "pinned to pve01 alone"},
		{"failback pending", func(f *fakeRebalanceIO, _ *rbVM) {
			f.pins[101].Nodes = map[string]int{"pve01": 0, "pve02": ha.PinPriorityPlaced, "pve03": 0}
		}, harebalance.SkipFailbackPending, "returns it to pve02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			c.breakk(f, f.vms[101])
			v, plan := rbModelVM(t, rbRun(f, RebalanceOptions{}), 101)
			if v.Skip != c.kind || !strings.Contains(v.SkipReason, c.reason) || plan != nil {
				t.Fatalf("skip %q %q plan %v, want %q naming %q", v.Skip, v.SkipReason, plan, c.kind, c.reason)
			}
		})
	}
}

func TestRebalanceModel_AnExpiredFailbackHoldLeavesTheVMACandidate(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.vms[101].comment = ha.WithFailbackHold(f.vms[101].comment, f.clock.Add(-time.Hour))
	if v, plan := rbModelVM(t, rbRun(f, RebalanceOptions{}), 101); v.Skip != "" || plan == nil {
		t.Fatalf("skip %q %q plan %v", v.Skip, v.SkipReason, plan)
	}
}

func TestRebalanceModel_FailbackThatCannotReturnTheVMLeavesItACandidate(t *testing.T) {
	cases := map[string]func(f *fakeRebalanceIO, v *rbVM){
		"failback off": func(_ *fakeRebalanceIO, v *rbVM) { v.failback = false },
		"failback held": func(f *fakeRebalanceIO, v *rbVM) {
			v.comment = ha.WithFailbackHold(v.comment, f.clock)
		},
		"top node offline": func(f *fakeRebalanceIO, _ *rbVM) { f.nodes[1].Online = false },
		"top node in maintenance": func(f *fakeRebalanceIO, _ *rbVM) {
			f.profileVal.Health.Nodes[1].Maintenance = true
		},
		"the VM sits on the top node": func(f *fakeRebalanceIO, _ *rbVM) {
			f.pins[101].Nodes["pve01"] = ha.PinPriorityPlaced
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			f.pins[101].Nodes = map[string]int{"pve01": 0, "pve02": ha.PinPriorityPlaced, "pve03": 0}
			setup(f, f.vms[101])
			v, _ := rbModelVM(t, rbRun(f, RebalanceOptions{}), 101)
			if v.Skip == harebalance.SkipFailbackPending {
				t.Fatalf("skip %q %q", v.Skip, v.SkipReason)
			}
		})
	}
}

func TestRebalanceModel_ACandidateCarriesItsTargetsAndMemory(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	big := f.addVM(104, "pve01", 16)
	big.failback = false
	v, plan := rbModelVM(t, rbRun(f, RebalanceOptions{}), 104)
	if v.Skip != "" || v.MemBytes != 16*rbGiB || !slices.Equal(v.Targets, []string{"pve01", "pve02", "pve03"}) {
		t.Fatalf("vm %+v", v)
	}
	if plan == nil || plan.pin == nil || len(plan.rewrites) != 0 || v.Rewrite != nil {
		t.Fatalf("a flat pin needs no rewrite: plan %+v vm %+v", plan, v)
	}
}

func TestRebalanceModel_NodesThatCannotTakeWorkCountAsMaintenance(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.profileVal.Health.Nodes[1].Placeable = false
	f.profileVal.Health.Nodes[2].DeadOrFenced = true
	view, err := rbRun(f, RebalanceOptions{}).observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range view.model.Nodes {
		if n.Maintenance != (i > 0) {
			t.Fatalf("nodes %+v", view.model.Nodes)
		}
	}
}

func rbAZConfig() Deps {
	cfg := icMinConfig()
	cfg.Placement = &config.PlacementConfig{AZMap: map[string][]string{
		"z1": {"pve01", "pve02"}, "z2": {"pve03"},
	}}
	return Deps{Config: cfg}
}

func TestRebalanceAZ(t *testing.T) {
	cases := []struct {
		name  string
		nodes map[string]int
		az    string
		want  []string
	}{
		{"flat", map[string]int{"pve01": 0, "pve02": 0, "pve03": 0}, "", nil},
		{"placed and preferred in one AZ", map[string]int{"pve01": 2, "pve02": 1, "pve03": 0}, "z1",
			[]string{"pve01", "pve02"}},
		{"placed outside the preferred AZ", map[string]int{"pve03": 2, "pve01": 1, "pve02": 1}, "z1",
			[]string{"pve01", "pve02"}},
		{"preferred across AZs", map[string]int{"pve02": 2, "pve01": 1, "pve03": 1}, "",
			[]string{"pve01", "pve03"}},
		{"no preferred node takes the placed node's AZ", map[string]int{"pve03": 2, "pve01": 0, "pve02": 0}, "z2",
			[]string{"pve03"}},
		{"no preferred node and an unmapped placed node", map[string]int{"pve09": 2, "pve01": 0}, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			az, nodes := rebalanceAZ(rbAZConfig(), &ha.Rule{Nodes: c.nodes})
			if az != c.az || !slices.Equal(nodes, c.want) {
				t.Fatalf("az %q nodes %v, want %q %v", az, nodes, c.az, c.want)
			}
		})
	}
}

func TestRebalanceRules_GroupsSplitRulesAndKeepsOnlyEnabledNegativeOnes(t *testing.T) {
	set := &ha.RuleSet{Rules: []ha.Rule{
		{ID: "bosh-aa-0123456789ab", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Resources: []string{"vm:101", "vm:102", "ct:500"}},
		{ID: "bosh-aa-0123456789ab-2", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Resources: []string{"vm:101", "vm:103"}},
		{ID: "bosh-aa-abcdef012345-p3", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Resources: []string{"vm:107", "vm:108"}},
		{ID: "bosh-aa-abcdef012345-p0", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Resources: []string{"vm:107", "vm:109"}},
		{ID: "operator-spread", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Resources: []string{"vm:104", "vm:x"}},
		{ID: "bosh-aa-ffffffffffff", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
			Disabled: true, Resources: []string{"vm:105"}},
		{ID: "together", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityPositive,
			Resources: []string{"vm:106"}},
		{ID: "bosh-pin-101", Type: ha.RuleTypeNodeAffinity, Affinity: ha.AffinityPositive,
			Resources: []string{"vm:101"}},
	}}
	got := rebalanceRules(set)
	want := []harebalance.Rule{
		{ID: "bosh-aa-0123456789ab", Group: "bosh-aa-0123456789ab", Members: []int{101, 102}},
		{ID: "bosh-aa-0123456789ab-2", Group: "bosh-aa-0123456789ab", Members: []int{101, 103}},
		{ID: "bosh-aa-abcdef012345-p3", Group: "bosh-aa-abcdef012345", Members: []int{107, 108}},
		{ID: "bosh-aa-abcdef012345-p0", Group: "bosh-aa-abcdef012345", Members: []int{107, 109}},
		{ID: "operator-spread", Group: "operator-spread", Members: []int{104}},
	}
	if len(got) != len(want) {
		t.Fatalf("rules %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Group != want[i].Group || !slices.Equal(got[i].Members, want[i].Members) {
			t.Fatalf("rule %d %+v, want %+v", i, got[i], want[i])
		}
	}
	if rebalanceRules(nil) != nil {
		t.Fatal("a nil rule set has rules")
	}
}

func TestInPositiveAffinityRule(t *testing.T) {
	rule := func(affinity string, disabled bool) *ha.RuleSet {
		return &ha.RuleSet{Rules: []ha.Rule{{ID: "r", Type: ha.RuleTypeResourceAffinity, Affinity: affinity,
			Disabled: disabled, Resources: []string{"vm:101"}}}}
	}
	switch {
	case inPositiveAffinityRule(nil, "vm:101"):
		t.Fatal("a nil rule set")
	case !inPositiveAffinityRule(rule(ha.AffinityPositive, false), "vm:101"):
		t.Fatal("an enabled positive rule")
	case inPositiveAffinityRule(rule(ha.AffinityPositive, true), "vm:101"):
		t.Fatal("a disabled positive rule")
	case inPositiveAffinityRule(rule(ha.AffinityNegative, false), "vm:101"):
		t.Fatal("a negative rule")
	case inPositiveAffinityRule(rule(ha.AffinityPositive, false), "vm:102"):
		t.Fatal("a rule that names another VM")
	}
}

func TestPinPlacedAndIntersectNodes(t *testing.T) {
	if placed, flat := pinPlaced(&ha.Rule{Nodes: map[string]int{"a": 2, "b": 1}}); placed != "a" || flat {
		t.Fatalf("placed %q flat %v", placed, flat)
	}
	if placed, flat := pinPlaced(&ha.Rule{Nodes: map[string]int{"a": 0, "b": 0}}); placed != "" || !flat {
		t.Fatalf("placed %q flat %v", placed, flat)
	}
	if got := intersectNodes([]string{"c", "a", "b"}, []string{"a", "c"}); !slices.Equal(got, []string{"c", "a"}) {
		t.Fatalf("intersect %v", got)
	}
	if orNothing("") != "nothing" || orNothing("x") != "x" {
		t.Fatal("orNothing")
	}
}

// rbRunAZ is rbRun with an az_map of z1 over pve01 to pve03 that only raises
// priority, as pin_az_strict false makes it.
func rbRunAZ(f *fakeRebalanceIO, opts RebalanceOptions) *rebalanceRun {
	r := rbRun(f, opts)
	r.deps.Config.HA.PinAZStrict = new(bool)
	r.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{"z1": {"pve01", "pve02", "pve03"}}}
	return r
}

func TestRebalanceModel_AMoveWithinTheAZRewritesThePinPriorities(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.pins[101].Nodes = map[string]int{"pve01": ha.PinPriorityPlaced, "pve02": ha.PinPriorityPreferred,
		"pve03": ha.PinPriorityPreferred}
	v, plan := rbModelVM(t, rbRunAZ(f, RebalanceOptions{}), 101)
	if v.Skip != "" || v.AZName != "z1" || !v.Rewrite["pve02"] || !v.Rewrite["pve03"] || v.Rewrite["pve01"] {
		t.Fatalf("vm %+v", v)
	}
	got := plan.rewrites["pve02"].pin
	if got == nil || got.Nodes["pve02"] != ha.PinPriorityPlaced || got.Nodes["pve01"] != ha.PinPriorityPreferred {
		t.Fatalf("pin for pve02 %+v", got)
	}
}

func TestRebalanceModel_APinRewriteThatChangesMoreThanPrioritiesRefusesTheTarget(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.pins[101].Nodes = map[string]int{"pve01": ha.PinPriorityPlaced, "pve02": ha.PinPriorityPreferred}
	v, plan := rbModelVM(t, rbRunAZ(f, RebalanceOptions{}), 101)
	if v.Skip != "" || !strings.Contains(v.PinRefused["pve02"], "changes more than priorities") {
		t.Fatalf("vm %+v", v)
	}
	if _, ok := plan.rewrites["pve02"]; ok {
		t.Fatalf("plan %+v", plan)
	}
}
