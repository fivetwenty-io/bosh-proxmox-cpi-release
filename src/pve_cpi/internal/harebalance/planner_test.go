package harebalance

import (
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

const gib = int64(1) << 30

// rig builds a model of three nodes of 100 GiB, one per AZ.
type rig struct{ m Model }

func newRig() *rig {
	return &rig{m: Model{Nodes: []Node{
		{Name: "n1", TotalMemBytes: 100 * gib, Online: true},
		{Name: "n2", TotalMemBytes: 100 * gib, Online: true},
		{Name: "n3", TotalMemBytes: 100 * gib, Online: true},
	}}}
}

var all = []string{"n1", "n2", "n3"}

// vm adds VM vmid of mem GiB on node, which may run on every node, has a
// flat pin, and so has no AZ.
func (r *rig) vm(vmid int, node string, mem int64) *VM {
	r.m.Guests = append(r.m.Guests, Guest{VMID: vmid, Node: node, MemBytes: mem * gib, HA: true,
		Admissible: all})
	r.m.VMs = append(r.m.VMs, VM{VMID: vmid, Name: "vm-" + string(rune('a'+vmid%26)), Node: node,
		MemBytes: mem * gib, Targets: all, Hard: all})
	return &r.m.VMs[len(r.m.VMs)-1]
}

// inAZ adds VM vmid on node whose AZ az is the one node home.
func (r *rig) inAZ(vmid int, node string, mem int64, az, home string) *VM {
	v := r.vm(vmid, node, mem)
	v.AZName, v.AZ = az, []string{home}
	return v
}

// other adds a guest that is not this Director's.
func (r *rig) other(vmid int, node string, mem int64) {
	r.m.Guests = append(r.m.Guests, Guest{VMID: vmid, Node: node, MemBytes: mem * gib})
}

func (r *rig) plan(t *testing.T, o Options) Plan {
	t.Helper()
	if o.MaxMoves == 0 {
		o.MaxMoves = 100
	}
	if o.StartThreshold == 0 {
		o.StartThreshold, o.StopThreshold = 0.15, 0.05
	}
	p := Build(r.m, o)
	checkPlanInvariants(t, r.m, p)
	return p
}

// checkPlanInvariants replays the plan and checks that no move ever puts two
// members of one rule on one node, and that every move stays inside the VM's
// targets and AZ.
func checkPlanInvariants(t *testing.T, m Model, p Plan) {
	t.Helper()
	at := map[int]string{}
	for _, g := range m.Guests {
		at[g.VMID] = g.Node
	}
	vms := map[int]VM{}
	for i := range m.VMs {
		vms[m.VMs[i].VMID] = m.VMs[i]
	}
	for i, mv := range p.Moves {
		v := vms[mv.VMID]
		if at[mv.VMID] != mv.From {
			t.Fatalf("move %d of VM %d starts on %s, but the VM is on %s", i, mv.VMID, mv.From, at[mv.VMID])
		}
		if !contains(v.Targets, mv.To) || (len(v.AZ) > 0 && !contains(v.AZ, mv.To)) {
			t.Fatalf("move %d takes VM %d to %s, outside its targets %v or its AZ %v", i, mv.VMID, mv.To,
				v.Targets, v.AZ)
		}
		at[mv.VMID] = mv.To
		for _, rule := range m.Rules {
			if !contains(rule.Members, mv.VMID) {
				continue
			}
			for _, other := range rule.Members {
				if other != mv.VMID && at[other] == mv.To {
					t.Fatalf("move %d puts VM %d on %s with VM %d of rule %s", i, mv.VMID, mv.To, other, rule.ID)
				}
			}
		}
	}
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestBuild_SkippedVMsAreListedAndNeverMove(t *testing.T) {
	r := newRig()
	kinds := []SkipKind{SkipReplicate, SkipHold, SkipLocalDisk, SkipDLB, SkipNotStarted, SkipLocked,
		SkipNarrowing, SkipPositiveAffinity, SkipFailbackPending, SkipFailbackHeld}
	for i, k := range kinds {
		v := r.inAZ(100+i, "n2", 5, "z1", "n1")
		v.Skip, v.SkipReason = k, "because of "+string(k)
	}
	p := r.plan(t, Options{})
	if len(p.Moves) != 0 {
		t.Fatalf("moves %+v, want none", p.Moves)
	}
	if len(p.Skipped) != len(kinds) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	for i, s := range p.Skipped {
		if s.VMID != 100+i || s.Kind != kinds[i] || s.Node != "n2" || s.Reason != "because of "+string(kinds[i]) {
			t.Fatalf("skip %d is %+v", i, s)
		}
	}
	if got := p.Before["n2"].Committed; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("the skipped VMs' memory must count in n2's load, got %v", got)
	}
}

func TestBuild_RestoresAVMThatStayedAwayFromItsAZ(t *testing.T) {
	r := newRig()
	r.inAZ(101, "n2", 10, "z1", "n1")
	r.inAZ(102, "n2", 10, "z2", "n2")
	p := r.plan(t, Options{})
	if len(p.Moves) != 1 {
		t.Fatalf("moves %+v, want one", p.Moves)
	}
	mv := p.Moves[0]
	if mv.VMID != 101 || mv.From != "n2" || mv.To != "n1" || mv.Phase != PhaseRestore || mv.PinRewritten {
		t.Fatalf("move %+v", mv)
	}
	if !strings.Contains(mv.Reason, "its AZ z1 is n1") {
		t.Fatalf("reason %q", mv.Reason)
	}
}

func TestBuild_RestoreRewritesThePinOfAVMCreatedAwayFromItsAZ(t *testing.T) {
	r := newRig()
	v := r.inAZ(101, "n3", 10, "z1", "n1")
	v.Rewrite = map[string]bool{"n1": true}
	p := r.plan(t, Options{})
	if len(p.Moves) != 1 || !p.Moves[0].PinRewritten || p.Moves[0].To != "n1" {
		t.Fatalf("moves %+v", p.Moves)
	}
	if !strings.Contains(p.Moves[0].Reason, "pin is rewritten") {
		t.Fatalf("reason %q", p.Moves[0].Reason)
	}
}

func TestBuild_RefusesAPinRewriteThatChangesMoreThanPriorities(t *testing.T) {
	r := newRig()
	v := r.inAZ(101, "n3", 10, "z1", "n1")
	v.PinRefused = map[string]string{"n1": "the pin the CPI would write for n1 changes more than priorities"}
	p := r.plan(t, Options{})
	if len(p.Moves) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Kind != SkipPinRewrite ||
		!strings.Contains(p.Skipped[0].Reason, "more than priorities") {
		t.Fatalf("plan %+v", p)
	}
}

func TestBuild_RestoreOrdersBySourceLoadThenMemoryThenVMID(t *testing.T) {
	r := newRig()
	r.inAZ(105, "n2", 10, "z1", "n1")
	r.inAZ(104, "n3", 10, "z1", "n1")
	r.inAZ(103, "n3", 20, "z1", "n1")
	r.inAZ(102, "n3", 10, "z1", "n1")
	r.other(900, "n2", 5)
	p := r.plan(t, Options{RestoreOnly: true})
	got := make([]int, 0, len(p.Moves))
	for _, mv := range p.Moves {
		got = append(got, mv.VMID)
	}
	if want := []int{103, 102, 104, 105}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestBuild_SpreadMakesNoMoveBetweenTheThresholds(t *testing.T) {
	r := newRig()
	r.vm(101, "n1", 50)
	r.vm(102, "n2", 40)
	r.vm(103, "n3", 40)
	p := r.plan(t, Options{})
	if len(p.Moves) != 0 || math.Abs(p.SpreadBefore-0.10) > 1e-9 {
		t.Fatalf("plan %+v", p)
	}
}

func TestBuild_SpreadMovesUntilTheStopThreshold(t *testing.T) {
	r := newRig()
	for i := range 8 {
		r.vm(110+i, "n1", 10)
	}
	r.vm(120, "n2", 10)
	p := r.plan(t, Options{})
	if p.SpreadBefore < 0.69 || p.SpreadAfter > 0.05+1e-9 {
		t.Fatalf("spread %v to %v", p.SpreadBefore, p.SpreadAfter)
	}
	for _, mv := range p.Moves {
		if mv.Phase != PhaseSpread || mv.From != "n1" {
			t.Fatalf("move %+v", mv)
		}
	}
	if len(p.Moves) != 5 {
		t.Fatalf("moves %d, want 5 (n1 30, n2 30, n3 30)", len(p.Moves))
	}

	// A plan that is already spreading goes on below the start threshold.
	r = newRig()
	r.vm(101, "n1", 30)
	r.vm(102, "n1", 10)
	r.vm(103, "n2", 30)
	r.vm(104, "n3", 30)
	if p := r.plan(t, Options{}); len(p.Moves) != 0 {
		t.Fatalf("a spread of 10 points between the thresholds must not start: %+v", p.Moves)
	}
	if p := r.plan(t, Options{Spreading: true}); len(p.Moves) != 0 {
		// Moving 10 GiB from n1 to n2 makes n2 40, so the target ends at the
		// source's load, and the move is not allowed.
		t.Fatalf("a move that leaves the target no lower than the source: %+v", p.Moves)
	}
	// n1 at 45 points and the others at 30 sits at the start threshold, so
	// a plan that is not spreading yet makes no move, and one that is goes on.
	r.vm(105, "n1", 5)
	if p := r.plan(t, Options{}); len(p.Moves) != 0 {
		t.Fatalf("a spread at the start threshold must not start: %+v", p.Moves)
	}
	if p := r.plan(t, Options{Spreading: true}); len(p.Moves) == 0 || p.Moves[0].From != "n1" {
		t.Fatalf("a run that is spreading must go on toward the stop threshold: %+v", p.Moves)
	}
}

func TestBuild_RestoreOnlySkipsTheSpread(t *testing.T) {
	r := newRig()
	for i := range 8 {
		r.vm(110+i, "n1", 10)
	}
	if p := r.plan(t, Options{RestoreOnly: true}); len(p.Moves) != 0 {
		t.Fatalf("moves %+v", p.Moves)
	}
}

func TestBuild_CapDefersTheRest(t *testing.T) {
	r := newRig()
	for i := range 5 {
		r.inAZ(101+i, "n2", 5, "z1", "n1")
	}
	p := r.plan(t, Options{MaxMoves: 2})
	if len(p.Moves) != 2 || p.Deferred != 3 {
		t.Fatalf("moves %d deferred %d", len(p.Moves), p.Deferred)
	}
}

func TestBuild_EndsOnAnyInput(t *testing.T) {
	rnd := rand.New(rand.NewPCG(7, 11))
	for range 200 {
		r := newRig()
		for i := range 1 + rnd.IntN(25) {
			node := all[rnd.IntN(3)]
			v := r.vm(100+i, node, 1+rnd.Int64N(12))
			if rnd.IntN(3) == 0 {
				home := all[rnd.IntN(3)]
				v.AZName, v.AZ = "z-"+home, []string{home}
			}
		}
		if rnd.IntN(2) == 0 {
			r.m.Rules = append(r.m.Rules, Rule{ID: "bosh-aa-x", Group: "bosh-aa-x", Members: []int{100, 101}})
		}
		p := r.plan(t, Options{MaxMoves: 1000})
		if len(p.Moves) > 200 {
			t.Fatalf("a plan of %d moves", len(p.Moves))
		}
	}
}

func TestBuild_SeparatesMembersOfOneRuleThatShareANode(t *testing.T) {
	r := newRig()
	r.vm(101, "n1", 5)
	r.vm(102, "n1", 5)
	r.vm(103, "n2", 5)
	r.m.Rules = []Rule{{ID: "bosh-aa-abc", Group: "bosh-aa-abc", Members: []int{101, 102, 103}}}
	p := r.plan(t, Options{})
	if len(p.Moves) != 1 || p.Moves[0].Phase != PhaseSeparate || p.Moves[0].To != "n3" {
		t.Fatalf("moves %+v", p.Moves)
	}
}

func TestBuild_SeparationHoldsCeilAcrossSplitRules(t *testing.T) {
	r := newRig()
	r.vm(101, "n1", 5)
	r.vm(102, "n2", 5)
	r.vm(103, "n1", 5)
	r.m.Rules = []Rule{
		{ID: "bosh-aa-abc", Group: "bosh-aa-abc", Members: []int{101, 102}},
		{ID: "bosh-aa-abc-2", Group: "bosh-aa-abc", Members: []int{103}},
	}
	p := r.plan(t, Options{RestoreOnly: true})
	if len(p.Moves) != 1 || p.Moves[0].Phase != PhaseSeparate || p.Moves[0].To != "n3" {
		t.Fatalf("moves %+v", p.Moves)
	}
}

func TestBuild_SpreadNeverPutsTwoMembersOfARuleOnANode(t *testing.T) {
	r := newRig()
	r.vm(101, "n1", 40)
	r.vm(102, "n1", 40)
	r.vm(103, "n2", 1)
	r.vm(104, "n3", 1)
	// 103 and 104 sit alone on the empty nodes, and 101 shares their rule.
	r.m.Rules = []Rule{{ID: "bosh-aa-a", Group: "bosh-aa-a", Members: []int{101, 103, 104}}}
	p := r.plan(t, Options{})
	for _, mv := range p.Moves {
		if mv.VMID == 101 {
			t.Fatalf("VM 101 has a partner on every other node: %+v", mv)
		}
	}
	if len(p.Moves) == 0 || p.Moves[0].VMID != 102 {
		t.Fatalf("moves %+v, want VM 102 to move", p.Moves)
	}
}

func TestBuild_TheMemoryFilterLimitsTheTargets(t *testing.T) {
	r := newRig()
	r.inAZ(101, "n2", 30, "z1", "n1")
	r.other(900, "n1", 80)
	p := r.plan(t, Options{})
	if len(p.Moves) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Kind != SkipNoTarget {
		t.Fatalf("plan %+v", p)
	}
}

func TestBuild_TheHeadroomFilterLimitsTheTargets(t *testing.T) {
	r := newRig()
	r.m.HeadroomMode = placement.HeadroomEnforce
	// n3 holds a big HA guest that only n1 can take. Moving VM 101 to n1
	// leaves n1 too little to recover it when n3 fails.
	r.m.Guests = append(r.m.Guests, Guest{VMID: 900, Node: "n3", MemBytes: 60 * gib, HA: true,
		Admissible: []string{"n1", "n3"}})
	r.inAZ(101, "n2", 50, "z1", "n1")
	p := r.plan(t, Options{})
	if len(p.Moves) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Kind != SkipNoTarget ||
		!strings.Contains(p.Skipped[0].Reason, "headroom") {
		t.Fatalf("plan %+v", p)
	}
	r.m.HeadroomMode = placement.HeadroomWarn
	if p := r.plan(t, Options{}); len(p.Moves) != 1 {
		t.Fatalf("warn mode must not filter: %+v", p)
	}
}

func TestBuild_TheCPUDomainLimitsTheTargets(t *testing.T) {
	r := newRig()
	for i := range 8 {
		v := r.vm(110+i, "n1", 10)
		v.Targets = []string{"n1", "n2"}
	}
	p := r.plan(t, Options{})
	for _, mv := range p.Moves {
		if mv.To == "n3" {
			t.Fatalf("a move outside the CPU domain: %+v", mv)
		}
	}
	if len(p.Moves) == 0 {
		t.Fatal("the VMs may still even out between n1 and n2")
	}
}

func TestBuild_IsDeterministic(t *testing.T) {
	build := func(seed uint64) Plan {
		r := newRig()
		for i := range 12 {
			r.vm(100+i, all[i%2], int64(3+i%4))
		}
		for i := range 4 {
			r.inAZ(200+i, "n1", 4, "z3", "n3")
		}
		r.m.Rules = []Rule{{ID: "bosh-aa-q", Group: "bosh-aa-q", Members: []int{100, 102}}}
		rnd := rand.New(rand.NewPCG(seed, seed))
		rnd.Shuffle(len(r.m.VMs), func(i, j int) { r.m.VMs[i], r.m.VMs[j] = r.m.VMs[j], r.m.VMs[i] })
		rnd.Shuffle(len(r.m.Guests), func(i, j int) { r.m.Guests[i], r.m.Guests[j] = r.m.Guests[j], r.m.Guests[i] })
		rnd.Shuffle(len(r.m.Nodes), func(i, j int) { r.m.Nodes[i], r.m.Nodes[j] = r.m.Nodes[j], r.m.Nodes[i] })
		return r.plan(t, Options{})
	}
	first := build(1)
	for seed := uint64(2); seed < 12; seed++ {
		if got := build(seed); !reflect.DeepEqual(got, first) {
			t.Fatalf("seed %d gave another plan:\n%+v\nwant\n%+v", seed, got, first)
		}
	}
	if len(first.Moves) == 0 {
		t.Fatal("the plan must make moves")
	}
}

func TestBuild_IgnoresNodesThatCannotTakeWork(t *testing.T) {
	r := newRig()
	r.m.Nodes[2].Online = false
	for i := range 6 {
		r.vm(110+i, "n1", 10)
	}
	p := r.plan(t, Options{})
	for _, mv := range p.Moves {
		if mv.To == "n3" {
			t.Fatalf("a move to an offline node: %+v", mv)
		}
	}
	if _, ok := p.Before["n3"]; ok {
		t.Fatal("an offline node is not part of the spread")
	}
}
