package aapattern

import (
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"testing"
)

// holdKey names the reserved place of index i in these tests.
func holdKey(i int) string { return fmt.Sprintf("hold/%d", i) }

// partnerList returns the sorted keys of partners[k].
func partnerList(partners map[string]map[string]bool, k string) []string {
	return slices.Sorted(maps.Keys(partners[k]))
}

// partnerCase is one random group for the HeldPartners property tests.
type partnerCase struct {
	name string
	in   Input
	size int
	// domainOf maps each index to its domain.
	domainOf map[int]string
}

// randomPartnerCase returns a random group: size indexed members under a
// random pattern on one or two domains, sometimes with a member that has no
// index and an older member of an index, laid out once from no rules.
func randomPartnerCase(t *testing.T, r *rand.Rand, iter int) partnerCase {
	t.Helper()
	pattern := canonicalPatterns[r.Intn(len(canonicalPatterns))]
	nodes := 3 + r.Intn(5)
	size := 2 + r.Intn(11)
	split := r.Intn(4) != 0
	multi := r.Intn(4) == 0
	doms := oneDomain(nodes)
	if multi {
		doms = map[string]Domain{"az1": {Ord: 1, Nodes: nodes}, "az2": {Ord: 2, Nodes: 3 + r.Intn(3)}}
	}
	c := partnerCase{size: size, domainOf: map[int]string{}}
	ms := make([]Member, 0, size+2)
	for i := range size {
		d := ""
		if multi {
			d = []string{"az1", "az2"}[i%2]
		}
		c.domainOf[i] = d
		ms = append(ms, Member{Key: key(i), Index: i, Born: 100, Domain: d, Joinable: true})
	}
	if r.Intn(3) == 0 {
		ms = append(ms, Member{Key: "vm:900", Index: -1, Domain: c.domainOf[0], Joinable: true})
	}
	if r.Intn(4) == 0 {
		i := r.Intn(size)
		ms = append(ms, Member{Key: "vm:950", Index: i, Born: 50, Domain: c.domainOf[i], Joinable: true})
	}
	c.in = Input{Base: base, Pattern: mustParse(t, pattern), Split: split, Domains: doms, Members: ms}
	c.in.Rules = Layout(c.in).Rules
	c.name = fmt.Sprintf("case %d, %s on %v, %d members, split=%v", iter, pattern, doms, size, split)
	return c
}

// TestHeldPartners_MatchThePlanOfTheRecreate pins that the partners
// HeldPartners reports for a hold are the members the planner puts in a rule
// with the hold's recreate once it joins. For random groups, under every
// pattern kind, with one to three members deleted at once and sometimes their
// replacements already created with no index, the query's answer for each
// hold equals the partners of a joinable member with the hold's index in the
// plan made from the rules the group has while the holds are live.
func TestHeldPartners_MatchThePlanOfTheRecreate(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(13))
	withPartners := 0
	for iter := range 3000 {
		c := randomPartnerCase(t, r, iter)
		held := r.Perm(c.size)[:1+r.Intn(min(3, c.size))]
		live := c.in
		live.Members = nil
		for _, m := range c.in.Members {
			// A member with a held index would consume the hold, so the
			// planner holds no place for an index that still has one.
			if m.Index < 0 || !slices.Contains(held, m.Index) {
				live.Members = append(live.Members, m)
			}
		}
		live.Rules = c.in.Rules
		for _, i := range held {
			live.Rules = withoutMember(live.Rules, key(i))
			live.Members = append(live.Members, Member{Key: holdKey(i), Index: i, Domain: c.domainOf[i], Reserved: true})
			if r.Intn(2) == 0 {
				live.Members = append(live.Members, Member{Key: fmt.Sprintf("vm:%d", 500+i), Index: -1,
					Born: 200, Domain: c.domainOf[i]})
			}
		}
		planned := Layout(live).Rules
		for _, i := range held {
			got := HeldPartners(live, holdKey(i))
			joined := live
			joined.Rules = planned
			joined.Members = nil
			for _, m := range live.Members {
				if m.Key == holdKey(i) {
					m = Member{Key: "vm:700", Index: i, Born: 300, Domain: c.domainOf[i], Joinable: true}
				}
				joined.Members = append(joined.Members, m)
			}
			want := partnerList(Partners(Layout(joined).Rules), "vm:700")
			if !slices.Equal(got, want) && (len(got) > 0 || len(want) > 0) {
				t.Fatalf("%s, holds %v: HeldPartners(%s) = %v; the recreate's plan puts it with %v (rules %s)",
					c.name, held, holdKey(i), got, want, render(planned))
			}
			if len(got) > 0 {
				withPartners++
			}
		}
	}
	// The property means something only when most holds have partners.
	if withPartners < 1500 {
		t.Fatalf("only %d holds had partners; the cases are too sparse to test the query", withPartners)
	}
}

// TestHeldPartners_OfAPresentMemberAreItsRulePartners pins that, for a member
// that is present and not reserved, HeldPartners reports exactly the members
// that share a rule with it in the layout's plan.
func TestHeldPartners_OfAPresentMemberAreItsRulePartners(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(17))
	for iter := range 1500 {
		c := randomPartnerCase(t, r, iter)
		partners := Partners(Layout(c.in).Rules)
		for _, m := range c.in.Members {
			got := HeldPartners(c.in, m.Key)
			want := partnerList(partners, m.Key)
			if !slices.Equal(got, want) && (len(got) > 0 || len(want) > 0) {
				t.Fatalf("%s: HeldPartners(%s) = %v; the plan puts it with %v (rules %s)", c.name, m.Key, got,
					want, render(Layout(c.in).Rules))
			}
		}
	}
}

// TestHeldPartners_TheLabRoll pins the partners of each recreate of the lab
// group, five members on three nodes with {0,1} and {2,3} and index 4 alone,
// rolled one batch at a time as the lab rolls it: index 0 alone, then 1 and
// 4 together, then 2 and 3 together. Each hold reports the present members
// its index shares a rule with, and never another hold or a member with no
// index.
func TestHeldPartners_TheLabRoll(t *testing.T) {
	t.Parallel()
	in := Input{Base: base, Pattern: Default(), Split: true, Domains: oneDomain(3), Members: members("", seq(5)...)}
	in.Rules = Layout(in).Rules
	if got := render(in.Rules); got != "={0,1} -2={2,3}" {
		t.Fatalf("start rules = %s; want ={0,1} -2={2,3}", got)
	}
	cases := []struct {
		held []int
		want map[int][]string
	}{
		{held: []int{0}, want: map[int][]string{0: {key(1)}}},
		{held: []int{1, 4}, want: map[int][]string{1: {key(0)}, 4: nil}},
		{held: []int{2, 3}, want: map[int][]string{2: nil, 3: nil}},
	}
	for _, tc := range cases {
		live := in
		live.Members = nil
		for _, m := range in.Members {
			if !slices.Contains(tc.held, m.Index) {
				live.Members = append(live.Members, m)
			}
		}
		for _, i := range tc.held {
			live.Rules = withoutMember(live.Rules, key(i))
			live.Members = append(live.Members, Member{Key: holdKey(i), Index: i, Reserved: true},
				Member{Key: fmt.Sprintf("vm:%d", 500+i), Index: -1})
		}
		for _, i := range tc.held {
			if got := HeldPartners(live, holdKey(i)); !slices.Equal(got, tc.want[i]) {
				t.Errorf("batch %v: HeldPartners(%s) = %v; want %v", tc.held, holdKey(i), got, tc.want[i])
			}
		}
	}
}

// TestHeldPartners_NoneWithoutAPlace pins that HeldPartners reports no
// partners for a key that is not a member, for a member of a domain too small
// for any rule, and for a hold whose index has no rule to share.
func TestHeldPartners_NoneWithoutAPlace(t *testing.T) {
	t.Parallel()
	in := Input{Base: base, Pattern: Default(), Split: true, Domains: oneDomain(3), Members: members("", seq(4)...)}
	if got := HeldPartners(in, "vm:999"); got != nil {
		t.Errorf("unknown key: partners = %v; want none", got)
	}
	small := in
	small.Domains = oneDomain(2)
	if got := HeldPartners(small, key(0)); got != nil {
		t.Errorf("two-node domain: partners = %v; want none", got)
	}
	alone := in
	alone.Members = append(members("", 0, 1, 2, 3), Member{Key: holdKey(4), Index: 4, Reserved: true})
	if got := HeldPartners(alone, holdKey(4)); got != nil {
		t.Errorf("hold alone in its block: partners = %v; want none", got)
	}
}
