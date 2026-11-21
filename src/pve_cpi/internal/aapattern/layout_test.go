package aapattern

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

const base = "bosh-aa-0123456789ab"

// key names the member with index i in these tests.
func key(i int) string { return fmt.Sprintf("vm:%d", 100+i) }

// members returns joinable members with the given indexes in one domain.
func members(dom string, idx ...int) []Member {
	out := make([]Member, 0, len(idx))
	for _, i := range idx {
		out = append(out, Member{Key: key(i), Index: i, Domain: dom, Joinable: true})
	}
	return out
}

// seq returns 0..n-1.
func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// render prints a layout's rules as "name={i,j}" in name order, with the
// member keys turned back into indexes.
func render(rules map[string][]string) string {
	parts := make([]string, 0, len(rules))
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		idx := make([]string, 0, len(rules[name]))
		for _, k := range rules[name] {
			var n int
			_, _ = fmt.Sscanf(k, "vm:%d", &n)
			idx = append(idx, fmt.Sprint(n-100))
		}
		parts = append(parts, strings.TrimPrefix(name, base)+"={"+strings.Join(idx, ",")+"}")
	}
	return strings.Join(parts, " ")
}

func mustParse(t *testing.T, s string) Pattern {
	t.Helper()
	p, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return p
}

func oneDomain(nodes int) map[string]Domain {
	return map[string]Domain{"": {Nodes: nodes}}
}

// TestLayout_PatternTable pins the rule counts of the spec's pattern table
// for a domain that holds every index.
func TestLayout_PatternTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		n       int
		count   int
		want    string
	}{
		{"block", 3, 6, "={0,1} -2={2,3} -3={4,5}"},
		{"block", 5, 6, "={0,1,2,3} -2={4,5}"},
		{"block", 3, 10, "={0,1} -2={2,3} -3={4,5} -4={6,7} -5={8,9}"},
		{"block", 5, 10, "={0,1,2,3} -2={4,5,6,7} -3={8,9}"},
		{"block:2", 5, 6, "={0,1} -2={2,3} -3={4,5}"},
		{"stride:2", 3, 6, "-p100001={0,2} -p101001={1,3}"},
		{"stride:2", 5, 10, "-p100001={0,2,4,6} -p101001={1,3,5,7}"},
		{"stride:3", 3, 6, "-p100001={0,3} -p101001={1,4} -p102001={2,5}"},
		{"stride:3", 5, 10, "-p100001={0,3,6,9} -p101001={1,4,7} -p102001={2,5,8}"},
		{"window:2", 3, 6, "-p100000={0,1} -p100002={2,3} -p100004={4,5}"},
		{"window:2", 5, 6, "-p100000={0,1} -p100001={1,2} -p100002={2,3} -p100003={3,4} -p100004={4,5}"},
		{"window:3", 6, 6, "-p100000={0,1,2} -p100001={1,2,3} -p100002={2,3,4} -p100003={3,4,5}"},
		{"window:3", 6, 5, "-p100000={0,1,2} -p100001={1,2,3} -p100002={2,3,4}"},
		{"sets:0.1.2/0.1.3", 5, 6, "-p100001={0,1,2} -p100002={0,1,3}"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/N=%d/%d", tc.pattern, tc.n, tc.count), func(t *testing.T) {
			t.Parallel()
			out := Layout(Input{Base: base, Pattern: mustParse(t, tc.pattern), Split: true,
				Domains: oneDomain(tc.n), Members: members("", seq(tc.count)...)})
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %s (trimmed %v)", got, tc.want, out.Trimmed)
			}
		})
	}
}

// TestLayout_EveryMemberStaysDrainSafe checks the invariant over a sweep of
// patterns and sizes: no member of a drain-safe layout has more than N-2
// partners.
func TestLayout_EveryMemberStaysDrainSafe(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{"block", "stride:2", "stride:3", "window:2", "window:3", "window:4",
		"sets:0.1.2/0.1.3/2.3.4", "stride:2+sets:0.1", "window:3+stride:2"} {
		for n := 3; n <= 7; n++ {
			for count := 2; count <= 12; count++ {
				out := Layout(Input{Base: base, Pattern: mustParse(t, pattern), Split: true,
					Domains: oneDomain(n), Members: members("", seq(count)...)})
				for k, ps := range Partners(out.Rules) {
					if len(ps) > n-2 {
						t.Errorf("%s N=%d count=%d: %s has %d partners; want at most %d (%s)", pattern, n, count,
							k, len(ps), n-2, render(out.Rules))
					}
				}
				for name, keys := range out.Rules {
					if len(keys) > n-1 {
						t.Errorf("%s N=%d: rule %s holds %d members", pattern, n, name, len(keys))
					}
				}
			}
		}
	}
}

// TestLayout_TrimmingKeepsTheLowestTwoApart pins that window:3 on three
// nodes, which trims every run of three, adds the rule over 0 and 1. The run
// from 4 holds only 4 and 5, so it fits on its own once the trim drops the run
// from 3 that covers it.
func TestLayout_TrimmingKeepsTheLowestTwoApart(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Pattern: mustParse(t, "window:3"), Split: true, Domains: oneDomain(3),
		Members: members("", seq(6)...)})
	if got := render(out.Rules); got != "-p0={0,1} -p100004={4,5}" {
		t.Fatalf("rules = %s; want the added rule over 0 and 1 and the run from 4", got)
	}
	if len(out.Trimmed) == 0 || len(out.AddedFirst) != 1 {
		t.Fatalf("trimmed = %v, added = %v; want the windows trimmed and the pair rule added", out.Trimmed,
			out.AddedFirst)
	}
	if len(out.LowFree) != 0 {
		t.Fatalf("low free = %v; want none once the pair rule holds 0 and 1", out.LowFree)
	}
}

func TestLayout_StrideReportsTheLowestTwoFree(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Pattern: mustParse(t, "stride:2"), Split: true, Domains: oneDomain(3),
		Members: members("", seq(6)...)})
	if !slices.Equal(out.LowFree, []string{""}) {
		t.Fatalf("low free = %v; want the domain reported", out.LowFree)
	}
	if len(out.AddedFirst) != 0 {
		t.Fatalf("added = %v; want no pair rule for a pattern that leaves 0 and 1 free by design", out.AddedFirst)
	}
	if !slices.Equal(out.Alone, []string{key(4), key(5)}) {
		t.Fatalf("alone = %v; want 4 and 5 unprotected", out.Alone)
	}
}

func TestLayout_UnsafePatternKeepsWhatFitsOnN(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Pattern: mustParse(t, "window:3!unsafe"), Split: true, Domains: oneDomain(3),
		Members: members("", seq(6)...)})
	want := "-p100000={0,1,2} -p100001={1,2,3} -p100002={2,3,4} -p100003={3,4,5}"
	if got := render(out.Rules); got != want {
		t.Fatalf("rules = %s; want %s", got, want)
	}
	if !out.Unsafe {
		t.Fatal("Unsafe = false; want true")
	}
	// Four members in one rule cannot fit on three nodes, so the set is
	// trimmed and the pair rule keeps 0 and 1 apart.
	out = Layout(Input{Base: base, Pattern: mustParse(t, "sets:0.1.2.3!unsafe"), Split: true,
		Domains: oneDomain(3), Members: members("", seq(4)...)})
	if got := render(out.Rules); got != "-p0={0,1}" || len(out.Trimmed) != 1 {
		t.Fatalf("rules = %s, trimmed = %v; want the oversized set trimmed", got, out.Trimmed)
	}
}

func TestLayout_SmallDomainWritesNoRule(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(2), Members: members("", 0, 1)})
	if len(out.Rules) != 0 || !slices.Equal(out.Small, []string{""}) {
		t.Fatalf("rules = %s, small = %v; want none on two nodes", render(out.Rules), out.Small)
	}
	out = Layout(Input{Base: base, Pattern: mustParse(t, "block:2!unsafe"), Split: true, Domains: oneDomain(2),
		Members: members("", 0, 1)})
	if got := render(out.Rules); got != "={0,1}" {
		t.Fatalf("rules = %s; want the pair once drain safety is given up", got)
	}
}

// TestLayout_ParallelCreatesSettleByIndex pins that a higher index that got
// into the first rule first gives its place to the lowest indexes.
func TestLayout_ParallelCreatesSettleByIndex(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: members("", seq(4)...),
		Rules: map[string][]string{base: {key(3), key(2)}}})
	if got := render(out.Rules); got != "={0,1} -2={2,3}" {
		t.Fatalf("rules = %s; want 0 and 1 in the first rule", got)
	}
}

// TestLayout_ARecreatedIndexFindsItsOldRule pins that the hold a delete
// leaves keeps the deleted index's place, so the gap in its rule waits for
// the index that left it and no other member moves into it.
func TestLayout_ARecreatedIndexFindsItsOldRule(t *testing.T) {
	t.Parallel()
	rules := map[string][]string{
		base:        {key(0), key(1)},
		base + "-2": {key(2), key(3)},
		base + "-3": {key(4), key(5)},
	}
	ms := members("", seq(6)...)
	ms[3] = Member{Key: "hold/3", Index: 3, Reserved: true}
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: ms,
		Rules: map[string][]string{base: rules[base], base + "-3": rules[base+"-3"]}})
	if got := render(out.Rules); got != "={0,1} -3={4,5}" {
		t.Fatalf("rules = %s; want index 2 waiting in its place for index 3", got)
	}
	out = Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: members("", seq(6)...),
		Rules: out.Rules})
	if got := render(out.Rules); got != "={0,1} -2={2,3} -3={4,5}" {
		t.Fatalf("rules = %s; want the recreated index back in its old rule", got)
	}
}

func TestLayout_SkipWritesOnlyTheFirstRule(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Domains: oneDomain(3), Members: members("", seq(5)...),
		Rules: map[string][]string{base + "-2": {key(2), key(3)}}})
	if got := render(out.Rules); got != "={0,1}" {
		t.Fatalf("rules = %s; want only the first rule", got)
	}
	if !slices.Equal(out.Overflow, []string{key(2), key(3), key(4)}) {
		t.Fatalf("overflow = %v", out.Overflow)
	}
}

func TestLayout_DuplicateIndexNewerTakesThePlace(t *testing.T) {
	t.Parallel()
	ms := members("", 0, 1, 2)
	ms[1].Born = 100
	ms = append(ms, Member{Key: "vm:200", Index: 1, Born: 200, Joinable: true})
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: ms,
		Rules: map[string][]string{base: {key(0), key(1)}}})
	if got := out.Rules[base]; !slices.Equal(got, []string{key(0), "vm:200"}) {
		t.Fatalf("first rule = %v; want 0 and the newer 1", got)
	}
	if !slices.Contains(out.Displaced, key(1)) {
		t.Fatalf("displaced = %v; want the older 1", out.Displaced)
	}
}

func TestLayout_UnindexedJoinAfterIndexedAndOnlyUnderBlock(t *testing.T) {
	t.Parallel()
	ms := append(members("", 0, 1), Member{Key: "vm:050", Index: -1, Joinable: true})
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(4), Members: ms})
	if got := out.Rules[base]; !slices.Equal(got, []string{"vm:050", key(0), key(1)}) {
		t.Fatalf("first rule = %v; want the unindexed member after the indexed ones", got)
	}
	out = Layout(Input{Base: base, Pattern: mustParse(t, "stride:2"), Split: true, Domains: oneDomain(4),
		Members: append(members("", 0, 2), Member{Key: "vm:050", Index: -1, Joinable: true})})
	if !slices.Equal(out.Unindexed, []string{"vm:050"}) {
		t.Fatalf("unindexed = %v; want the member reported", out.Unindexed)
	}
}

// TestLayout_UnindexedJoinersOnlyFillRoom pins that a member whose index is
// unknown never takes a kept member's place in the first rule, because the
// key order it ranks by says nothing about which member should lead.
func TestLayout_UnindexedJoinersOnlyFillRoom(t *testing.T) {
	t.Parallel()
	ms := make([]Member, 0, 5)
	for i := range 5 {
		ms = append(ms, Member{Key: key(i), Index: -1, Joinable: true})
	}
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(4), Members: ms,
		Rules: map[string][]string{base: {key(2), key(4)}}})
	if got := render(out.Rules); got != "={0,2,4} -2={1,3}" {
		t.Fatalf("rules = %s; want 2 and 4 kept and the unindexed joiners filling the room", got)
	}
	// One already in a later rule keeps it, though the first rule has room.
	out = Layout(Input{Base: base, Split: true, Domains: oneDomain(5), Members: ms,
		Rules: map[string][]string{base: {key(2), key(4)}, base + "-2": {key(1), key(3)}}})
	if got := render(out.Rules); got != "={0,2,4} -2={1,3}" {
		t.Fatalf("rules = %s; want the later rule kept whole", got)
	}
}

// TestLayout_NamesTheRuleAMemberIsAloneIn pins that the layout names the
// rule a member was put in alone, so the warning about it can name the rule
// the next member completes.
func TestLayout_NamesTheRuleAMemberIsAloneIn(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(4), Members: members("", seq(4)...)})
	if !slices.Equal(out.Alone, []string{key(3)}) || out.AloneIn[key(3)] != base+"-2" {
		t.Fatalf("alone = %v in %v; want index 3 alone in the second rule", out.Alone, out.AloneIn)
	}
}

func TestLayout_NotJoinableKeepsOnlyItsPlace(t *testing.T) {
	t.Parallel()
	ms := members("", 0, 1, 2)
	ms[0].Joinable = false
	ms[2].Joinable = false
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(4), Members: ms,
		Rules: map[string][]string{base: {key(0)}}})
	if got := render(out.Rules); got != "={0,1}" {
		t.Fatalf("rules = %s; want 0 kept, 1 joined, and 2 left out", got)
	}
}

// TestLayout_RefusedJoinKeepsItsPlace pins that a member the join check
// refuses is left out of its place's rule and moves to no other rule, so the
// members after it keep their places.
func TestLayout_RefusedJoinKeepsItsPlace(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: members("", seq(4)...),
		CanJoin: func(rule string, _ []string, k string) bool { return rule != base || k != key(1) }})
	if got := render(out.Rules); got != "-2={2,3}" {
		t.Fatalf("rules = %s; want 1 refused and 2 and 3 in their own rule", got)
	}
	if got := out.Refused[key(1)]; !slices.Equal(got, []string{base}) {
		t.Fatalf("refused = %v; want 1 refused by the first rule", out.Refused)
	}
}

func TestLayout_PlaceableLimitsJoins(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: map[string]Domain{"": {Nodes: 5, Placeable: 2}},
		Members: members("", seq(4)...)})
	if got := render(out.Rules); got != "={0,1}" {
		t.Fatalf("rules = %s; want two joins while only two nodes take VMs", got)
	}
	if !slices.Equal(out.Overflow, []string{key(2), key(3)}) {
		t.Fatalf("overflow = %v; want 2 and 3 waiting for room in their place's rule", out.Overflow)
	}
}

// TestLayout_NoPlaceableNodeHoldsEveryRule pins that a domain whose health
// read found no node that can take a VM, such as an AZ whose nodes are all in
// maintenance, lets no member join a rule there. A member keeps the place it
// has. With the count unknown, the rules fill to the cap as before.
func TestLayout_NoPlaceableNodeHoldsEveryRule(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		dom  Domain
		want string
	}{
		"none placeable": {Domain{Nodes: 5, Placeable: 0, PlaceableKnown: true}, "={0,1}"},
		"unknown":        {Domain{Nodes: 5}, "={0,1,2,3}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := Layout(Input{Base: base, Split: true, Domains: map[string]Domain{"": tc.dom},
				Members: members("", seq(4)...), Rules: map[string][]string{base: {key(0), key(1)}}})
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %s", got, tc.want)
			}
		})
	}
}

// A domain known to hold no configured node, such as an AZ whose nodes all
// left the cluster, has no room for any rule, so the plan writes none there
// and reports it, whatever the pattern says about drain safety. A domain
// whose count is unknown keeps its uncapped plan.
func TestLayout_DomainWithNoNodeGetsNoRule(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pattern string
		dom     Domain
		want    string
		small   bool
	}{
		"no node":              {"", Domain{Ord: 1, NodesKnown: true}, "", true},
		"no node, not drained": {"block:3!unsafe", Domain{Ord: 1, NodesKnown: true}, "", true},
		"unknown":              {"", Domain{Ord: 1}, "={0,1,2,3}", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p Pattern
			if tc.pattern != "" {
				p = mustParse(t, tc.pattern)
			}
			out := Layout(Input{Base: base, Pattern: p, Split: true,
				Domains: map[string]Domain{"az1": tc.dom}, Members: members("az1", seq(4)...),
				Rules: map[string][]string{base: {key(0), key(1), key(2)}}})
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %q", got, tc.want)
			}
			if got := slices.Contains(out.Small, "az1"); got != tc.small {
				t.Fatalf("small = %v (%v); want %v", got, out.Small, tc.small)
			}
		})
	}
}

func TestLayout_DomainsGetTheirOwnRules(t *testing.T) {
	t.Parallel()
	ms := append(members("az1", 0, 2, 4), members("az2", 1, 3, 5)...)
	doms := map[string]Domain{"az1": {Ord: 1, Nodes: 3}, "az2": {Ord: 2, Nodes: 3}}
	out := Layout(Input{Base: base, Split: true, Domains: doms, Members: ms})
	if got := render(out.Rules); got != "={0,2} -2={1,3}" {
		t.Fatalf("rules = %s; want one rule per AZ", got)
	}
	out = Layout(Input{Base: base, Pattern: mustParse(t, "sets:0.2"), Split: true, Domains: doms, Members: ms})
	if got := render(out.Rules); got != "-p10100001={0,2}" {
		t.Fatalf("rules = %s; want the set named for its AZ", got)
	}
}

func TestLayout_CapsTheRulesOfAGroup(t *testing.T) {
	t.Parallel()
	out := Layout(Input{Base: base, Split: true, Domains: oneDomain(3), Members: members("", seq(10)...),
		MaxRules: 2})
	if got := render(out.Rules); got != "={0,1} -2={2,3}" || len(out.Capped) != 3 {
		t.Fatalf("rules = %s, capped = %v; want the two lowest rules kept", got, out.Capped)
	}
}

func TestLayout_IsAFixedPoint(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{"block", "stride:2", "window:2", "window:3", "sets:0.1.2/0.1.3",
		"stride:2+sets:0.1"} {
		for n := 3; n <= 6; n++ {
			in := Input{Base: base, Pattern: mustParse(t, pattern), Split: true, Domains: oneDomain(n),
				Members: members("", seq(9)...)}
			first := Layout(in)
			in.Rules = first.Rules
			second := Layout(in)
			if render(first.Rules) != render(second.Rules) {
				t.Errorf("%s N=%d: second plan %s differs from first %s", pattern, n, render(second.Rules),
					render(first.Rules))
			}
		}
	}
}

// TestLayout_EvensOfTen pins the last column of the spec's pattern table, a
// cluster that sees only the even indexes of ten.
func TestLayout_EvensOfTen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		want    string
		alone   []string
	}{
		{"block", "={0,2} -2={4,6}", []string{key(8)}},
		{"stride:2", "-p100001={0,2} -p100002={4,6}", []string{key(8)}},
		{"stride:3", "-p100001={0,6} -p102001={2,8}", []string{key(4)}},
		{"window:2", "", nil},
		{"window:3", "-p100000={0,2} -p100004={4,6}", nil},
		{"sets:0.1.2/0.1.3", "-p100001={0,2}", nil},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()
			out := Layout(Input{Base: base, Pattern: mustParse(t, tc.pattern), Split: true, Domains: oneDomain(3),
				Members: members("", 0, 2, 4, 6, 8)})
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %s", got, tc.want)
			}
			if !slices.Equal(out.Alone, tc.alone) {
				t.Fatalf("alone = %v; want %v", out.Alone, tc.alone)
			}
		})
	}
}

// TestLayout_WindowKeepsACoveredRunWhenTrimDropsItsCover pins that a window
// run whose members another run holds is dropped only when that other run is
// kept. On five nodes, window:3 trims the runs from 2 and 3, and the run from
// 4, which the run from 3 covers, then protects 4 and 5 on its own. Every run
// the bound allows is kept or covered by a kept rule.
func TestLayout_WindowKeepsACoveredRunWhenTrimDropsItsCover(t *testing.T) {
	t.Parallel()
	cases := []struct {
		count int
		want  string
	}{
		{6, "-p100000={0,1,2} -p100001={1,2,3} -p100004={4,5}"},
		{10, "-p100000={0,1,2} -p100001={1,2,3} -p100004={4,5,6} -p100005={5,6,7} -p100008={8,9}"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			t.Parallel()
			in := Input{Base: base, Pattern: mustParse(t, "window:3"), Split: true, Domains: oneDomain(5),
				Members: members("", seq(tc.count)...)}
			out := Layout(in)
			if got := render(out.Rules); got != tc.want {
				t.Fatalf("rules = %s; want %s", got, tc.want)
			}
			for k, ps := range Partners(out.Rules) {
				if len(ps) > 3 {
					t.Fatalf("%s has %d partners; want at most 3", k, len(ps))
				}
			}
			for s := range tc.count {
				run := []string{}
				for i := s; i < min(s+3, tc.count); i++ {
					run = append(run, key(i))
				}
				if len(run) < 2 || inSomeRule(out.Rules, run) || !fitsBound(out.Rules, run, 3) {
					continue
				}
				t.Fatalf("run from %d fits the bound but no rule holds it: %s", s, render(out.Rules))
			}
		})
	}
}

// inSomeRule reports whether one rule of rules holds every key of run.
func inSomeRule(rules map[string][]string, run []string) bool {
	for _, keys := range rules {
		if !slices.ContainsFunc(run, func(k string) bool { return !slices.Contains(keys, k) }) {
			return true
		}
	}
	return false
}

// fitsBound reports whether adding a rule over run keeps every member within
// limit partners.
func fitsBound(rules map[string][]string, run []string, limit int) bool {
	with := maps.Clone(rules)
	with["probe"] = run
	for _, ps := range Partners(with) {
		if len(ps) > limit {
			return false
		}
	}
	return true
}

// TestLayout_SkipPlacesAnUnindexedMemberOfALaterRuleAtOnce pins that skip
// mode, which writes only the first rule, places an unindexed member that a
// later rule holds in the same plan that drops the later rule. Before, the
// first plan left it out and the second plan added it, so a switch from split
// to skip took two writes.
func TestLayout_SkipPlacesAnUnindexedMemberOfALaterRuleAtOnce(t *testing.T) {
	t.Parallel()
	ms := members("", 0, 1)
	ms = append(ms, Member{Key: key(9), Index: -1, Domain: "", Joinable: true})
	in := Input{Base: base, Domains: oneDomain(4), Members: ms,
		Rules: map[string][]string{base: {key(0), key(1)}, ChunkName(base, 2): {key(9), key(1)}}}
	first := Layout(in)
	if got := render(first.Rules); got != "={0,1,9}" {
		t.Fatalf("rules = %s; want the unindexed member in the first rule", got)
	}
	in.Rules = first.Rules
	if second := Layout(in); render(second.Rules) != render(first.Rules) {
		t.Fatalf("second plan %s differs from first %s", render(second.Rules), render(first.Rules))
	}
}

// TestLayout_PlanOfItsOwnPlanIsTheSame checks over random groups that a plan
// is a fixed point: planning again from the rules a plan asks for asks for the
// same rules. The groups span one or two domains, carry duplicate and unknown
// indexes, and start from random rules, as a group from an older release or
// one mid-deploy does. A plan that is not a fixed point makes the rule writer
// spend extra writes, and each one can migrate a member.
func TestLayout_PlanOfItsOwnPlanIsTheSame(t *testing.T) {
	t.Parallel()
	patterns := []string{"block", "block:2", "stride:2", "stride:3", "window:2", "window:3", "sets:0.1.2/0.1.3",
		"stride:2+sets:0.1", "window:3+stride:2", "block+window:2", "window:3!unsafe"}
	r := rand.New(rand.NewSource(1))
	for iter := range 4000 {
		pattern := patterns[r.Intn(len(patterns))]
		doms := oneDomain(2 + r.Intn(6))
		multi := r.Intn(3) == 0
		if multi {
			doms = map[string]Domain{"az1": {Ord: 1, Nodes: 2 + r.Intn(6)}, "az2": {Ord: 2, Nodes: 3 + r.Intn(3)}}
		}
		count := 1 + r.Intn(10)
		ms := make([]Member, 0, count)
		for i := range count {
			idx := r.Intn(12)
			if r.Intn(8) == 0 {
				idx = -1
			}
			d := ""
			if multi {
				d = []string{"az1", "az2"}[r.Intn(2)]
			}
			ms = append(ms, Member{Key: key(i), Index: idx, Born: int64(r.Intn(3)), Domain: d, Joinable: true})
		}
		rules := map[string][]string{}
		if r.Intn(2) == 0 {
			for n := 1; n <= 3; n++ {
				var keys []string
				for _, m := range ms {
					if r.Intn(3) == 0 {
						keys = append(keys, m.Key)
					}
				}
				if len(keys) > 0 {
					rules[ChunkName(base, n)] = keys
				}
			}
		}
		in := Input{Base: base, Pattern: mustParse(t, pattern), Split: r.Intn(4) != 0, Domains: doms, Members: ms,
			Rules: rules}
		first := Layout(in)
		in.Rules = first.Rules
		if second := Layout(in); render(second.Rules) != render(first.Rules) {
			t.Fatalf("case %d, %s split=%v domains=%v members=%+v rules=%v: second plan %s differs from first %s",
				iter, pattern, in.Split, doms, ms, rules, render(second.Rules), render(first.Rules))
		}
	}
}
