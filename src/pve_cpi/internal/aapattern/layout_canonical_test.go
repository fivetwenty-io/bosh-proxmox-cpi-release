package aapattern

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"testing"
)

// canonicalPatterns are the patterns the canonical layout tests run, one or
// more of every kind.
var canonicalPatterns = []string{"block", "block:2", "stride:2", "stride:3", "window:2", "window:3",
	"sets:0.1.2/0.1.3", "stride:2+sets:0.1", "block+window:2"}

// wantBlocks returns the rules of the default block pattern over the indexes
// idx of one domain on nodes nodes, as the canonical layout fills them: the
// indexes in order, cut into rules of nodes-1, the n-th named for its place.
// With split unset only the first rule is written.
func wantBlocks(nodes int, split bool, idx []int) string {
	idx = slices.Sorted(slices.Values(idx))
	rules := map[string][]string{}
	for n := 1; (n-1)*(nodes-1) < len(idx); n++ {
		if n > 1 && !split {
			break
		}
		var keys []string
		for _, i := range idx[(n-1)*(nodes-1) : min(n*(nodes-1), len(idx))] {
			keys = append(keys, key(i))
		}
		if len(keys) >= 2 {
			rules[ChunkName(base, n)] = keys
		}
	}
	return render(rules)
}

// joinInOrder plans the group once for every member in order, the way each
// member's set_vm_metadata does, from the rules the plan before it asked for.
// A member waits as a member with no index and no way to join until its turn,
// the way a new VM does between its create and its set_vm_metadata.
func joinInOrder(in Input, order []int) map[string][]string {
	steps := make([][]int, 0, len(order))
	for n := range order {
		steps = append(steps, order[:n+1])
	}
	rules, _ := joinSteps(in, steps)
	return rules
}

// joinSteps plans the group once for each step, from the rules the plan
// before it asked for, with the indexes the step names known and every other
// member waiting with no index and no way to join. It returns the last plan's
// rules and the rendered rules of every plan.
func joinSteps(in Input, steps [][]int) (map[string][]string, []string) {
	rules := map[string][]string{}
	seen := make([]string, 0, len(steps))
	for _, known := range steps {
		ms := make([]Member, 0, len(in.Members))
		for _, m := range in.Members {
			if !slices.Contains(known, m.Index) {
				m.Index, m.Joinable = -1, false
			}
			ms = append(ms, m)
		}
		step := in
		step.Members, step.Rules = ms, rules
		rules = Layout(step).Rules
		seen = append(seen, render(rules))
	}
	return rules, seen
}

// TestLayout_AnyJoinOrderGivesTheCanonicalLayout pins that the rules are a
// function of the members and not of the order they joined in. For random
// orders of n members on N nodes, under every pattern kind and both overflow
// modes, the rules after the last join equal the plan of a group that starts
// with no rules, and under the default block pattern they equal the indexes
// cut into rules of N-1 from the lowest.
func TestLayout_AnyJoinOrderGivesTheCanonicalLayout(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(7))
	for iter := range 3000 {
		pattern := canonicalPatterns[r.Intn(len(canonicalPatterns))]
		nodes := 3 + r.Intn(5)
		size := 2 + r.Intn(11)
		split := r.Intn(4) != 0
		multi := r.Intn(4) == 0
		doms := oneDomain(nodes)
		if multi {
			doms = map[string]Domain{"az1": {Ord: 1, Nodes: nodes}, "az2": {Ord: 2, Nodes: 3 + r.Intn(3)}}
		}
		ms := make([]Member, 0, size)
		for i := range size {
			d := ""
			if multi {
				d = []string{"az1", "az2"}[i%2]
			}
			ms = append(ms, Member{Key: key(i), Index: i, Domain: d, Joinable: true})
		}
		in := Input{Base: base, Pattern: mustParse(t, pattern), Split: split, Domains: doms, Members: ms}
		want := render(Layout(in).Rules)
		order := r.Perm(size)
		got := render(joinInOrder(in, order))
		if got != want {
			t.Fatalf("case %d, %s on %v split=%v, join order %v: rules %s; want %s", iter, pattern, doms, split,
				order, got, want)
		}
		if pattern == "block" && !multi {
			if oracle := wantBlocks(nodes, split, seq(size)); got != oracle {
				t.Fatalf("case %d, block on %d nodes split=%v, join order %v: rules %s; want %s", iter, nodes, split,
					order, got, oracle)
			}
		}
	}
}

// TestLayout_TheLabJoinOrderGivesTheCanonicalLayout replays the parallel
// deploy of five members on three nodes as the lab recorded it. The members'
// set_vm_metadata calls wrote their index tags in the order index 1, 4, 3, 0,
// and 2, and their plans landed in the order index 4, 1, 3, 0, and 2, so the
// first two plans each saw indexes 1 and 4. Each plan before the last asks
// for what the lab saw, and the last one leaves {0,1} and {2,3}, with index 4
// alone, where the lab was left with {0,1} and {3,4}.
func TestLayout_TheLabJoinOrderGivesTheCanonicalLayout(t *testing.T) {
	t.Parallel()
	in := Input{Base: base, Pattern: Default(), Split: true, Domains: oneDomain(3), Members: members("", seq(5)...)}
	rules, seen := joinSteps(in, [][]int{{1, 4}, {1, 4}, {1, 3, 4}, {0, 1, 3, 4}, {0, 1, 2, 3, 4}})
	want := []string{"={1,4}", "={1,4}", "={1,3}", "={0,1} -2={3,4}", "={0,1} -2={2,3}"}
	if !slices.Equal(seen, want) {
		t.Fatalf("plans = %q; want %q", seen, want)
	}
	if got := render(rules); got != want[len(want)-1] {
		t.Fatalf("rules = %s; want %s", got, want[len(want)-1])
	}
}

// TestLayout_AStickyLayoutMovesToTheCanonicalOneInOnePlan pins that a group
// an earlier release laid out by the order its members joined in, with
// indexes 3 and 4 in the second rule and index 2 in no rule, is planned into
// the canonical layout by its next plan, and that the plan after that writes
// nothing.
func TestLayout_AStickyLayoutMovesToTheCanonicalOneInOnePlan(t *testing.T) {
	t.Parallel()
	in := Input{Base: base, Split: true, Domains: oneDomain(3), Members: members("", seq(5)...),
		Rules: map[string][]string{base: {key(0), key(1)}, base + "-2": {key(3), key(4)}}}
	out := Layout(in)
	if got, want := render(out.Rules), "={0,1} -2={2,3}"; got != want {
		t.Fatalf("rules = %s; want %s", got, want)
	}
	in.Rules = out.Rules
	if again := Layout(in); render(again.Rules) != render(out.Rules) {
		t.Fatalf("the next plan asks for %s; want %s again", render(again.Rules), render(out.Rules))
	}
}

// rollEvent is one step of a roll: a delete, the stage of the replacement's
// HA resource, or the replacement's set_vm_metadata.
type rollEvent struct {
	index int
	kind  string
}

// rollBatch returns the events of one batch of a roll in a random order in
// which each member's delete comes before its stage and its stage before its
// set_vm_metadata.
func rollBatch(r *rand.Rand, batch []int) []rollEvent {
	next := map[int]int{}
	var out []rollEvent
	for len(out) < 3*len(batch) {
		i := batch[r.Intn(len(batch))]
		if next[i] == 3 {
			continue
		}
		out = append(out, rollEvent{index: i, kind: []string{"delete", "stage", "join"}[next[i]]})
		next[i]++
	}
	return out
}

// TestLayout_RollsMoveNoMemberButTheRecreatedOnes pins that a roll at a
// max_in_flight of 1 to 3 moves no member but the ones being recreated, in
// any interleaving of the deletes, the stages, and the joins of a batch. A
// delete takes its member out of its rules and leaves a hold, which the plans
// see as a reserved place until the replacement's join consumes it. Between
// its stage and its join, a replacement is a member with no index that may not
// join. No member is ever in a rule its index was not in before the roll, and
// a member that is present and indexed is in every such rule that is still
// written. After the roll every index is back in the rules it started in.
func TestLayout_RollsMoveNoMemberButTheRecreatedOnes(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(11))
	for iter := range 1500 {
		pattern := canonicalPatterns[r.Intn(len(canonicalPatterns))]
		nodes := 3 + r.Intn(4)
		size := 2 + r.Intn(10)
		mif := 1 + r.Intn(3)
		split := r.Intn(4) != 0
		in := Input{Base: base, Pattern: mustParse(t, pattern), Split: split, Domains: oneDomain(nodes),
			Members: members("", seq(size)...)}
		name := fmt.Sprintf("case %d, %s on %d nodes, %d members, max_in_flight %d, split=%v", iter, pattern, nodes,
			size, mif, split)
		testRoll(t, r, name, in, size, mif)
	}
}

// testRoll runs one case of TestLayout_RollsMoveNoMemberButTheRecreatedOnes:
// a roll of the size members of in, max_in_flight at a time, in a random
// order and interleaving.
func testRoll(t *testing.T, r *rand.Rand, name string, in Input, size, mif int) {
	t.Helper()
	rules := Layout(in).Rules
	start := rulesOf(rules)
	keys := map[int]string{}
	for i := range size {
		keys[i] = key(i)
	}
	order := r.Perm(size)
	for b := 0; b < size; b += mif {
		batch := order[b:min(b+mif, size)]
		state := map[int]string{}
		for _, ev := range rollBatch(r, batch) {
			state[ev.index] = ev.kind
			switch ev.kind {
			case "delete":
				rules = withoutMember(rules, keys[ev.index])
			case "stage":
				keys[ev.index] = fmt.Sprintf("vm:%d", 1000*(b+1)+ev.index)
			}
			step := in
			step.Members, step.Rules = rollMembers(size, state, keys), rules
			rules = Layout(step).Rules
			where := fmt.Sprintf("%s, batch %v, after the %s of %d", name, batch, ev.kind, ev.index)
			checkRollStep(t, where, rules, start, state, keys, size)
		}
	}
	end := rulesOf(rules)
	for i := range size {
		if !slices.Equal(end[keys[i]], start[key(i)]) {
			t.Fatalf("%s: index %d ends in %v; it started in %v (rules %s)", name, i, end[keys[i]],
				start[key(i)], render(rules))
		}
	}
}

// withoutMember returns rules with gone taken out, and with every rule that
// is left with fewer than two members dropped, the way a delete leaves them.
func withoutMember(rules map[string][]string, gone string) map[string][]string {
	out := map[string][]string{}
	for rn, rk := range rules {
		rest := slices.DeleteFunc(slices.Clone(rk), func(k string) bool { return k == gone })
		if len(rest) >= 2 {
			out[rn] = rest
		}
	}
	return out
}

// checkRollStep fails the test when a member is in a rule its index did not
// start in, or when a member that is present and indexed is out of a rule it
// started in that is still written.
func checkRollStep(t *testing.T, where string, rules, start map[string][]string, state map[int]string,
	keys map[int]string, size int) {
	t.Helper()
	placed := rulesOf(rules)
	for i := range size {
		from := start[key(i)]
		for _, rn := range placed[keys[i]] {
			if !slices.Contains(from, rn) {
				t.Fatalf("%s: index %d is in %s; it started in %v (rules %s)", where, i, rn, from, render(rules))
			}
		}
		if state[i] != "" && state[i] != "join" {
			continue
		}
		for _, rn := range from {
			if rules[rn] != nil && !slices.Contains(placed[keys[i]], rn) {
				t.Fatalf("%s: index %d is out of %s, which is still written (rules %s)", where, i, rn, render(rules))
			}
		}
	}
}

// rollMembers returns the members of a roll's plan: a reserved place for each
// index whose delete has run, the replacement with no index and no way to join
// once its stage has run, and every other index as a joinable member.
func rollMembers(size int, state map[int]string, keys map[int]string) []Member {
	var ms []Member
	for i := range size {
		switch state[i] {
		case "delete":
			ms = append(ms, Member{Key: fmt.Sprintf("hold/%d", i), Index: i, Reserved: true})
		case "stage":
			ms = append(ms, Member{Key: fmt.Sprintf("hold/%d", i), Index: i, Reserved: true},
				Member{Key: keys[i], Index: -1})
		default:
			ms = append(ms, Member{Key: keys[i], Index: i, Joinable: true})
		}
	}
	return ms
}

// rulesOf maps each member key to the sorted names of the rules that hold it.
func rulesOf(rules map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		for _, k := range rules[name] {
			out[k] = append(out[k], name)
		}
	}
	return out
}
