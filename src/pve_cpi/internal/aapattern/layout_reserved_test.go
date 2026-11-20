package aapattern

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

// reservedAt returns the reserved place of the member with index i, under a
// key no live member uses.
func reservedAt(i int) Member {
	return Member{Key: fmt.Sprintf("vm:%d", 900+i), Index: i, Reserved: true}
}

// The place of a member that just left stays free for the recreate of its
// index, whether the plan finds it in its old rule or ranks it by index, so
// nobody moves into it and the recreate takes it back with one write.
func TestLayout_ReservedPlaceWaitsForTheRecreate(t *testing.T) {
	t.Parallel()
	gone := reservedAt(0).Key
	for name, tc := range map[string]struct {
		nodes   int
		present []int
		rules   map[string][]string
		want    string
	}{
		"three nodes, in its rule": {3, []int{1, 2, 3, 4, 5, 6, 7}, map[string][]string{
			base: {gone, key(1)}, base + "-2": {key(2), key(3)}, base + "-3": {key(4), key(5)},
			base + "-4": {key(6), key(7)},
		}, "-2={2,3} -3={4,5} -4={6,7}"},
		"three nodes, ranked by index": {3, []int{1, 2, 3, 4, 5, 6, 7}, map[string][]string{
			base: {key(1)}, base + "-2": {key(2), key(3)}, base + "-3": {key(4), key(5)},
			base + "-4": {key(6), key(7)},
		}, "-2={2,3} -3={4,5} -4={6,7}"},
		"five nodes": {5, []int{1, 2, 3, 4, 5, 6, 7}, map[string][]string{
			base: {key(1), key(2), key(3)}, base + "-2": {key(4), key(5), key(6), key(7)},
		}, "={1,2,3} -2={4,5,6,7}"},
		"a member in no rule stays out of the freed place": {4, []int{1, 2, 3}, map[string][]string{
			base: {key(1), key(2)},
		}, "={1,2}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ms := append(members("", tc.present...), reservedAt(0))
			out := Layout(Input{Base: base, Split: true, Domains: oneDomain(tc.nodes), Members: ms, Rules: tc.rules})
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %s", got, tc.want)
			}
			for _, list := range [][]string{out.Overflow, out.Alone, out.Displaced, out.Unindexed} {
				if slices.Contains(list, gone) {
					t.Fatalf("the reserved place is reported as a member: %+v", out)
				}
			}
			back := Layout(Input{Base: base, Split: true, Domains: oneDomain(tc.nodes),
				Members: members("", append([]int{0}, tc.present...)...), Rules: out.Rules})
			if n := len(changedRules(out.Rules, back.Rules)); n != 1 {
				t.Fatalf("the recreate of index 0 wrote %d rules (%s); want 1", n, render(back.Rules))
			}
		})
	}
}

// A reserved place never takes an index from a live member that holds it,
// and no rule's join check sees it.
func TestLayout_ReservedPlaceYieldsToALiveMember(t *testing.T) {
	t.Parallel()
	ms := append(members("", 0, 1, 2), reservedAt(0))
	ms[3].Born = 1 << 40
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(4), Members: ms,
		CanJoin: func(_ string, keys []string, key string) bool {
			if key == reservedAt(0).Key || slices.Contains(keys, reservedAt(0).Key) {
				t.Errorf("the join check saw the reserved place: %s joining %v", key, keys)
			}
			return true
		}})
	if got := render(out.Rules); got != "={0,1,2}" {
		t.Fatalf("rules = %s; want the live members", got)
	}
}

// A rolling update deletes and recreates each member in turn, the recreate
// under a new key with the same index. The delete's plan holds the member's
// place, so each step writes only the rules that hold the member it
// replaces, no other member changes rules, and every member ends where its
// index began.
func TestLayout_RollingUpdateMovesNoOtherMember(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ nodes, size int }{{3, 8}, {3, 7}, {5, 8}, {5, 11}, {4, 9}} {
		for _, held := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d nodes %d members held %v", tc.nodes, tc.size, held), func(t *testing.T) {
				t.Parallel()
				rollingUpdate(t, tc.nodes, tc.size, held)
			})
		}
	}
}

func rollingUpdate(t *testing.T, nodes, size int, held bool) {
	t.Helper()
	keys := map[int]string{}
	for i := range size {
		keys[i] = key(i)
	}
	ms := func(skip int) []Member {
		var out []Member
		for i := range size {
			if i != skip {
				out = append(out, Member{Key: keys[i], Index: i, Joinable: true})
			}
		}
		return out
	}
	rules := Layout(Input{Base: base, Split: true, Domains: oneDomain(nodes), Members: ms(-1)}).Rules
	start := ruleOf(rules)
	writes := 0
	for i := range size {
		gone := keys[i]
		// The delete takes the member out of its rule, and the plan after
		// it sees the member's place either back in that rule or by index.
		purged := map[string][]string{}
		for name, rk := range rules {
			purged[name] = slices.DeleteFunc(slices.Clone(rk), func(k string) bool { return k == gone })
		}
		in := purged
		if held {
			in = rules
		}
		del := Layout(Input{Base: base, Split: true, Domains: oneDomain(nodes),
			Members: append(ms(i), Member{Key: gone, Index: i, Reserved: true}), Rules: in})
		writes += len(changedRules(rules, del.Rules))
		assertOnlyMoved(t, fmt.Sprintf("delete of %d", i), rules, del.Rules, gone)
		keys[i] = fmt.Sprintf("vm:%d", 1000+i)
		back := Layout(Input{Base: base, Split: true, Domains: oneDomain(nodes), Members: ms(-1),
			Rules: del.Rules})
		writes += len(changedRules(del.Rules, back.Rules))
		assertOnlyMoved(t, fmt.Sprintf("recreate of %d", i), del.Rules, back.Rules, keys[i])
		rules = back.Rules
	}
	end := ruleOf(rules)
	for i := range size {
		if start[key(i)] != end[keys[i]] {
			t.Errorf("index %d: rule %q; started in %q", i, end[keys[i]], start[key(i)])
		}
	}
	// One write when the member leaves and one when it comes back.
	if writes > 2*size {
		t.Errorf("the update wrote %d rules for %d members; want at most %d", writes, size, 2*size)
	}
}

// changedRules names the rules whose members differ between a and b,
// including rules only one of them has.
func changedRules(a, b map[string][]string) []string {
	var out []string
	for name := range a {
		if !slices.Equal(a[name], b[name]) {
			out = append(out, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ruleOf maps each member key to the rule that holds it.
func ruleOf(rules map[string][]string) map[string]string {
	out := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		for _, k := range rules[name] {
			out[k] = name
		}
	}
	return out
}

// assertOnlyMoved fails when a member other than self is in a different rule
// in after than in before, counting no rule as a rule of its own. A member
// that its rule left alone when self left it, and that joins self's rule
// again, did not move.
func assertOnlyMoved(t *testing.T, step string, before, after map[string][]string, self string) {
	t.Helper()
	b, a := ruleOf(before), ruleOf(after)
	for k := range a {
		// A partner its rule left alone comes back with the member.
		if k != self && b[k] != a[k] && (b[k] != "" || a[k] != a[self]) {
			t.Errorf("%s: member %s moved from %q to %q", step, k, b[k], a[k])
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && k != self && b[k] != "" {
			// A member left alone when its rule dropped below two members
			// is in no rule, which is not a move to another rule.
			if len(after[b[k]]) == 0 && len(before[b[k]]) == 2 {
				continue
			}
			t.Errorf("%s: member %s left rule %q", step, k, b[k])
		}
	}
}
