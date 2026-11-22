package aapattern

import (
	"maps"
	"slices"
	"testing"
)

// finalNow is the clock of the final join tests, in Unix seconds.
const finalNow = int64(2_000_000_000)

// finalWindow is the window the planner passes, thirty minutes.
const finalWindow = int64(30 * 60)

// finalInput returns the input of a group of size members on nodes nodes of
// one domain with final joins on, each member born at born.
func finalInput(nodes, size int, born int64) Input {
	ms := make([]Member, 0, size)
	for i := range size {
		ms = append(ms, Member{Key: key(i), Index: i, Born: born, Joinable: true})
	}
	return Input{Base: base, Split: true, Domains: oneDomain(nodes), Members: ms, FinalWindow: finalWindow,
		Now: finalNow}
}

// finalJoins plans the group once per join in order, the way each member's
// set_vm_metadata does, from the rules the plan before it asked for. Every
// member that has not joined yet waits with no index and no way to join, the
// way a new VM does between its create and its set_vm_metadata. It fails the
// test when a plan takes a member out of a rule or splits two members that a
// rule already pairs, which is a rewrite, and returns the last plan's rules.
func finalJoins(t *testing.T, in Input, order []int) map[string][]string {
	t.Helper()
	rules := map[string][]string{}
	for n := range order {
		known := order[:n+1]
		ms := make([]Member, 0, len(in.Members))
		for _, m := range in.Members {
			if !slices.Contains(known, m.Index) {
				m.Index, m.Joinable = -1, false
			}
			ms = append(ms, m)
		}
		step := in
		step.Members, step.Rules = ms, rules
		next := Layout(step).Rules
		for name, keys := range rules {
			for _, k := range keys {
				if !slices.Contains(next[name], k) {
					t.Fatalf("join order %v, after %v: the plan takes %s out of %s (%s -> %s)", order, known, k, name,
						render(rules), render(next))
				}
			}
		}
		rules = next
	}
	return rules
}

// permutations returns every order of 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Concat(p[:i], []int{n - 1}, p[i:])
			out = append(out, q)
		}
	}
	return out
}

// TestFinal_ScrambledParallelDeployWritesEachRuleOnce pins the point of final
// joins. For every order in which six members on three nodes, or seven on
// four, can join during a parallel deploy, no plan takes a member out of a
// rule an earlier plan wrote, and the last plan is the canonical layout.
func TestFinal_ScrambledParallelDeployWritesEachRuleOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ nodes, size int }{{3, 6}, {4, 7}, {3, 5}} {
		in := finalInput(tc.nodes, tc.size, finalNow-60)
		want := wantBlocks(tc.nodes, true, seq(tc.size))
		for _, order := range permutations(tc.size) {
			if got := render(finalJoins(t, in, order)); got != want {
				t.Fatalf("%d nodes, join order %v: rules %s; want %s", tc.nodes, order, got, want)
			}
		}
	}
}

// TestFinal_ThePlanWithoutFinalJoinsRewrites pins that the test above tests
// something: without final joins, the day 5 order rewrites a rule.
func TestFinal_ThePlanWithoutFinalJoinsRewrites(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 6, finalNow-60)
	in.FinalWindow = 0
	rules := joinInOrder(in, []int{1, 2, 3, 4})
	if got := render(rules); got == render(joinInOrder(in, []int{1, 2, 3, 4, 0})) {
		t.Fatalf("rules %s stay when index 0 joins; want the partial plan rewritten", got)
	}
}

// TestFinal_WaitingMembersAreReported pins the report of a partial plan. With
// index 2 missing, 3 and 4 wait, index 2 is open, and 0 and 1 are written.
func TestFinal_WaitingMembersAreReported(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 5, finalNow-60)
	in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 2 })
	out := Layout(in)
	if got := render(out.Rules); got != render(map[string][]string{base: {key(0), key(1)}}) {
		t.Fatalf("rules %s; want only the base rule of 0 and 1", got)
	}
	if !slices.Equal(out.Waiting, []string{key(3), key(4)}) || !slices.Equal(out.Open, []int{2}) {
		t.Fatalf("waiting %v open %v; want 3 and 4 waiting on index 2", out.Waiting, out.Open)
	}
	if len(out.Overflow)+len(out.Alone)+len(out.Displaced) != 0 {
		t.Fatalf("output %+v; want a waiting member reported only as waiting", out)
	}
}

// TestFinal_AGapUnderRuledMembersIsNotOpen pins the gap that a lapsed hold
// leaves after a scale-down. The members above it sit in rules, so the gap is
// not open, and they move down one position as the canonical layout asks,
// however young they are.
func TestFinal_AGapUnderRuledMembersIsNotOpen(t *testing.T) {
	t.Parallel()
	for _, born := range []int64{finalNow - 60, finalNow - 7200} {
		in := finalInput(3, 6, born)
		in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 2 })
		in.Rules = map[string][]string{base: {key(0), key(1)}, ChunkName(base, 3): {key(4), key(5)}}
		out := Layout(in)
		want := render(map[string][]string{base: {key(0), key(1)}, ChunkName(base, 2): {key(3), key(4)}})
		if got := render(out.Rules); got != want || len(out.Open) != 0 {
			t.Fatalf("born %d: rules %s open %v; want %s with nothing open", born, got, out.Open, want)
		}
	}
}

// TestFinal_ScaleUpAboveAFullGroup pins a scale-up. The old members keep
// their rules, the new index 5 waits for index 4, and the pair is written
// once 4 joins.
func TestFinal_ScaleUpAboveAFullGroup(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 6, finalNow-60)
	for i := range in.Members {
		if in.Members[i].Index < 4 {
			in.Members[i].Born = finalNow - 86400
		}
	}
	old := map[string][]string{base: {key(0), key(1)}, ChunkName(base, 2): {key(2), key(3)}}
	step := in
	step.Rules = old
	step.Members = slices.DeleteFunc(slices.Clone(in.Members), func(m Member) bool { return m.Index == 4 })
	out := Layout(step)
	if got := render(out.Rules); got != render(old) || !slices.Equal(out.Waiting, []string{key(5)}) {
		t.Fatalf("rules %s waiting %v; want the old rules and 5 waiting", got, out.Waiting)
	}
	step.Members, step.Rules = in.Members, out.Rules
	if got, want := render(Layout(step).Rules), wantBlocks(3, true, seq(6)); got != want {
		t.Fatalf("rules %s; want %s", got, want)
	}
}

// TestFinal_ScaleUpAboveALapsedGap pins a scale-up of a group whose gap a
// lapsed hold left. The old gap is not open, so the new indexes wait only for
// each other.
func TestFinal_ScaleUpAboveALapsedGap(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 7, finalNow-60)
	for i := range in.Members {
		if in.Members[i].Index < 5 {
			in.Members[i].Born = finalNow - 86400
		}
	}
	in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 2 })
	old := map[string][]string{base: {key(0), key(1)}, ChunkName(base, 2): {key(3), key(4)}}
	step := in
	step.Rules = old
	step.Members = slices.DeleteFunc(slices.Clone(in.Members), func(m Member) bool { return m.Index == 5 })
	out := Layout(step)
	if got := render(out.Rules); got != render(old) || !slices.Equal(out.Open, []int{5}) {
		t.Fatalf("rules %s open %v; want the old rules and only index 5 open", got, out.Open)
	}
	step.Members, step.Rules = in.Members, out.Rules
	want := maps.Clone(old)
	want[ChunkName(base, 3)] = []string{key(5), key(6)}
	if got := render(Layout(step).Rules); got != render(want) {
		t.Fatalf("rules %s; want %s", got, render(want))
	}
}

// TestFinal_AHoldKeepsTheIndexFromOpening pins a roll. The deleted index is
// held, so nothing is open and the recreate's partners keep their rule.
func TestFinal_AHoldKeepsTheIndexFromOpening(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 4, finalNow-60)
	for i := range in.Members {
		if in.Members[i].Index == 0 || in.Members[i].Index == 2 {
			in.Members[i] = Member{Key: "hold:" + key(in.Members[i].Index), Index: in.Members[i].Index, Reserved: true}
		}
	}
	in.Rules = map[string][]string{base: {key(1)}, ChunkName(base, 2): {key(3)}}
	out := Layout(in)
	if len(out.Open) != 0 || len(out.Waiting) != 0 {
		t.Fatalf("open %v waiting %v; want nothing open while the holds keep 0 and 2", out.Open, out.Waiting)
	}
}

// TestFinal_TheWaitEnds pins the limit. Once every member above a missing
// index is older than the window, the index is not open, so a deploy that
// never creates it does not leave the members above it out for good.
func TestFinal_TheWaitEnds(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 4, finalNow-finalWindow-1)
	in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 1 })
	out := Layout(in)
	want := render(map[string][]string{base: {key(0), key(2)}})
	if got := render(out.Rules); got != want || len(out.Waiting) != 0 {
		t.Fatalf("rules %s waiting %v; want %s with no member waiting", got, out.Waiting, want)
	}
}

// TestFinal_AnUnindexedMemberWaitsWhileAnIndexIsOpen pins that a member with
// no index, which takes a place after every indexed member, waits too.
func TestFinal_AnUnindexedMemberWaitsWhileAnIndexIsOpen(t *testing.T) {
	t.Parallel()
	in := finalInput(4, 3, finalNow-60)
	in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 1 })
	in.Members = append(in.Members, Member{Key: "vm:999", Index: -1, Joinable: true})
	out := Layout(in)
	if !slices.Contains(out.Waiting, "vm:999") {
		t.Fatalf("waiting %v; want the unindexed member waiting", out.Waiting)
	}
}

// TestFinal_HeldPartnersSeeTheWait pins that create_vm's partner read and the
// layout agree on a waiting member.
func TestFinal_HeldPartnersSeeTheWait(t *testing.T) {
	t.Parallel()
	in := finalInput(3, 4, finalNow-60)
	in.Members[0] = Member{Key: "hold:0", Index: 0, Reserved: true}
	in.Members = slices.DeleteFunc(in.Members, func(m Member) bool { return m.Index == 2 })
	if got := HeldPartners(in, "hold:0"); !slices.Equal(got, []string{key(1)}) {
		t.Fatalf("partners %v; want %s, with 3 waiting for index 2", got, key(1))
	}
}
