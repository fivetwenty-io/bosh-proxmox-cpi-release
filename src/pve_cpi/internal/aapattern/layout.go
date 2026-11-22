package aapattern

import (
	"cmp"
	"maps"
	"math"
	"slices"
)

// DefaultMaxRules is the most rules the layout writes for one group. A
// pattern that needs more keeps the rules with the lowest indexes, which
// keeps rules.cfg from growing without bound.
const DefaultMaxRules = 64

// Member is one member of a group, as the layout sees it.
type Member struct {
	// Key identifies the member, such as its HA resource id vm:<vmid>.
	Key string
	// Index is the member's BOSH instance index, or -1 when no index can be
	// read for it.
	Index int
	// Born is when the member was created, in Unix seconds, or 0 when that is
	// unknown. Of two members with the same index, the newer one takes the
	// index's place.
	Born int64
	// Domain keys the member's placement domain (see Domain).
	Domain string
	// Joinable says the member may be added to a rule that does not hold it
	// now. A member that is not joinable may only keep a place it has.
	Joinable bool
	// Reserved marks the place of a member that just left the group, such as
	// one a delete took out of its rules. The layout places it like a
	// joinable member, in the rule that holds it or else by its index, so
	// the plan keeps that place free for the recreate of the index instead
	// of moving another member into it, which the recreate would undo. No
	// rule or list of the plan names it, no join check sees it, and it never
	// takes an index's place from a member that is not reserved.
	Reserved bool
}

// Domain is a set of nodes a group's members may run on. Rules only ever
// name members of one domain.
type Domain struct {
	Key string
	// Ord numbers the domain in pattern rule names: 0 for the whole
	// cluster, and an AZ's position plus one otherwise.
	Ord int
	// Nodes is the domain's configured node count, N. A count of 0 means
	// unknown, which turns off the cap and the drain-safety check, unless
	// NodesKnown is set.
	Nodes int
	// NodesKnown says Nodes was read from the cluster's node list, so a count
	// of 0 means that none of the domain's nodes is in the cluster and the
	// domain has room for no rule.
	NodesKnown bool
	// Placeable is how many of the domain's nodes can take a VM now. A member
	// joins a rule only while the rule holds fewer members than this. It is
	// read only when PlaceableKnown is set or it is above 0, so a zero value
	// alone means the count is unknown.
	Placeable int
	// PlaceableKnown says Placeable was read, so a count of 0 means that no
	// node of the domain can take a VM now, and no member joins a rule there.
	PlaceableKnown bool
}

// Input is what the layout plans from.
type Input struct {
	// Base is the group's base rule name, bosh-aa-<hash12>.
	Base    string
	Pattern Pattern
	// Split lets a block or stride class spill into further rules. Without
	// it each block or stride class writes only its first rule.
	Split   bool
	Domains map[string]Domain
	Members []Member
	// Rules holds the group's rules as they are now, by name, with their
	// member keys. Keys of members not in Members are ignored.
	Rules map[string][]string
	// CanJoin reports whether key may join the rule named rule, which the
	// layout has given members so far. It is asked only when members is not
	// empty, and nil lets every join through.
	CanJoin func(rule string, members []string, key string) bool
	// MaxRules caps the rules written for the group. 0 means
	// DefaultMaxRules.
	MaxRules int
	// Spread is how many domains the group's block rules take turns
	// numbering among, the AZs the configuration defines, so that a
	// domain's rules keep their names whichever domains hold members. 0
	// means the highest Ord in Domains.
	Spread int
	// FinalWindow turns on final joins when it is above 0. It is how long, in
	// seconds, a member's birth keeps a missing index below it open (see
	// Waiting), so that a member joins a rule only once its position can no
	// longer change.
	FinalWindow int64
	// Now is when the plan is made, in Unix seconds, which FinalWindow counts
	// back from.
	Now int64
}

// Output is the layout's plan.
type Output struct {
	// Rules holds every rule the group should have, by name, each with at
	// least two member keys in sorted order. A rule of the group missing
	// here should not exist.
	Rules map[string][]string
	// Trimmed names the rules the pattern asked for that the drain-safety
	// check, or the fit check of a pattern that gave up drain safety, left
	// out.
	Trimmed []string
	// Capped names the rules left out because the group reached MaxRules.
	Capped []string
	// Overflow holds the joinable members left out of every rule because no
	// rule had room for them.
	Overflow []string
	// Refused maps each joinable member that the rule of its place refused
	// to the rule that refused it. The member keeps its place, so no other
	// member moves for it.
	Refused map[string][]string
	// Alone holds the members the plan put alone in a block or stride rule,
	// which writes no rule, while another rule of the same kind holds two.
	Alone []string
	// AloneIn names the rule each member of Alone was put in alone.
	AloneIn map[string]string
	// Displaced holds the members that lost their place and may not join
	// another rule.
	Displaced []string
	// Unindexed holds the joinable members with no index that a pattern
	// without a block term could not place.
	Unindexed []string
	// LowFree holds the domains in which the pattern leaves the two lowest
	// indexes free to share a node.
	LowFree []string
	// AddedFirst names the rules added to keep the two lowest indexes apart
	// after trimming separated them.
	AddedFirst []string
	// Small holds the domains whose node count leaves no room for any rule.
	Small []string
	// Unsafe says the pattern gave up drain safety.
	Unsafe bool
	// Waiting holds the joinable members that join no rule yet, because an
	// open index lies below their position (see Input.FinalWindow).
	Waiting []string
	// Open holds the open indexes, in ascending order.
	Open []int
}

// mstate is a member during the layout.
type mstate struct {
	Member
	primary  bool
	joinable bool
}

// candRule is a rule the layout asks for before trimming.
type candRule struct {
	name    string
	members []*mstate
	term    int
	low     int
	fixed   bool
	// window marks a window run, which dropCoveredRuns leaves out when a
	// kept run of the same term holds its members.
	window bool
	// current holds the rule's members now, for a fixed rule's joins.
	current map[string]bool
}

// layout carries the state of one run.
type layout struct {
	in      Input
	p       Pattern
	members map[string]*mstate
	chunks  map[int][]string
	pats    map[int][]string
	out     *Output
}

// settleRounds bounds the plans Layout makes to reach a plan that planning
// again leaves as it is.
const settleRounds = 8

// Layout plans the rules of one group. It plans again from the rules each
// plan asks for until the plan stops changing, so the plan it returns is one
// that the next plan of the same group keeps. A single plan can stop short of
// that, for example when the rules one term plans change what the join check
// lets into another term's rules, and every extra plan the rule writer made to
// get there would cost a write that can migrate a member. The members a plan
// displaced stay reported, because a later round no longer sees their place.
func Layout(in Input) Output {
	out := settle(in)
	out.dropReserved(in.Members)
	return out
}

// settle makes the plan Layout returns, with the reserved places of members
// still in its rules.
func settle(in Input) Output {
	open, waiting := openIndexes(in)
	if len(waiting) > 0 {
		in.Members = slices.Clone(in.Members)
		for i := range in.Members {
			if waiting[in.Members[i].Key] {
				in.Members[i].Joinable = false
			}
		}
	}
	out := settlePlans(in)
	out.Open = open
	out.Waiting = slices.Sorted(maps.Keys(waiting))
	return out
}

// openIndexes returns the open indexes of in, in ascending order, and the
// joinable members that wait for them. An index is open when no member or
// hold has it, a member has a higher one, no member with a higher index is in
// one of in.Rules, and a member with a higher index that is not a hold was
// born less than FinalWindow seconds before Now. The layout places a member
// by its rank among the indexes and holds below it, so while an index below a
// member is open, the member's position can still change, and a rule written
// for it now would be rewritten once that index joins. A gap below a member
// that a rule holds is one the group has outlived, such as the gap a lapsed
// hold leaves after a scale-down, so it is not open. A member waits while an
// open index lies below its own, and a member with no index waits while any
// index is open, because it takes a place after every indexed member. A
// waiting member joins no rule and keeps its position, so no other member
// shifts for it.
func openIndexes(in Input) ([]int, map[string]bool) {
	if in.FinalWindow <= 0 {
		return nil, nil
	}
	present := map[int]bool{}
	inRule := map[string]bool{}
	for _, keys := range in.Rules {
		for _, k := range keys {
			inRule[k] = true
		}
	}
	top := -1
	for _, m := range in.Members {
		if m.Index >= 0 {
			present[m.Index] = true
			top = max(top, m.Index)
		}
	}
	var open []int
	for g := range top {
		if present[g] {
			continue
		}
		ruled, young := false, false
		for _, m := range in.Members {
			if m.Index <= g {
				continue
			}
			ruled = ruled || inRule[m.Key]
			young = young || !m.Reserved && m.Born > 0 && in.Now-m.Born < in.FinalWindow
		}
		if !ruled && young {
			open = append(open, g)
		}
	}
	if len(open) == 0 {
		return nil, nil
	}
	waiting := map[string]bool{}
	for _, m := range in.Members {
		if m.Joinable && !m.Reserved && !inRule[m.Key] && (m.Index < 0 || m.Index > open[0]) {
			waiting[m.Key] = true
		}
	}
	return open, waiting
}

// settlePlans plans in until planning again leaves the plan as it is.
func settlePlans(in Input) Output {
	out := layoutOnce(in)
	displaced := slices.Clone(out.Displaced)
	for range settleRounds - 1 {
		next := in
		next.Rules = out.Rules
		again := layoutOnce(next)
		if sameRules(again.Rules, out.Rules) {
			break
		}
		displaced = append(displaced, again.Displaced...)
		out = again
	}
	out.Displaced = displaced
	return out
}

// HeldPartners returns the members that the plan of in puts in a rule with
// the member keyed key, in sorted order. It leaves out every reserved place,
// as those name no member that is present. It makes the same plan Layout
// makes, so its answer and Layout's rules never disagree.
//
// Its use is for key to be the reserved place of a hold. Layout drops that
// place from the rules it returns, but it places the hold where the recreate
// of the hold's index will go, so the hold's partners here are the present
// members the recreate shares a rule with once it joins. A caller that does
// not know a new member's index yet can read them before the member exists.
// A key that is not a member, or one the plan puts in no rule, has none.
func HeldPartners(in Input, key string) []string {
	reserved := map[string]bool{}
	found := false
	for _, m := range in.Members {
		found = found || m.Key == key
		if m.Reserved {
			reserved[m.Key] = true
		}
	}
	if !found {
		return nil
	}
	var out []string
	for _, keys := range settle(in).Rules {
		if !slices.Contains(keys, key) {
			continue
		}
		for _, k := range keys {
			if k != key && !reserved[k] && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

// dropReserved takes the reserved places of members out of the plan. A rule
// left with fewer than two members goes, as a rule that holds one member
// spreads nothing.
func (o *Output) dropReserved(members []Member) {
	reserved := map[string]bool{}
	for _, m := range members {
		if m.Reserved {
			reserved[m.Key] = true
		}
	}
	if len(reserved) > 0 {
		isReserved := func(k string) bool { return reserved[k] }
		for name, keys := range o.Rules {
			keys = slices.DeleteFunc(keys, isReserved)
			if len(keys) < 2 {
				delete(o.Rules, name)
				continue
			}
			o.Rules[name] = keys
		}
		for _, list := range []*[]string{&o.Overflow, &o.Alone, &o.Displaced, &o.Unindexed} {
			*list = slices.DeleteFunc(*list, isReserved)
		}
		o.AddedFirst = slices.DeleteFunc(o.AddedFirst, func(name string) bool { return o.Rules[name] == nil })
		for k := range reserved {
			delete(o.AloneIn, k)
			delete(o.Refused, k)
		}
	}
	o.finish()
}

// sameRules reports whether a and b name the same rules with the same
// members, each list in sorted order.
func sameRules(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, slices.Equal[[]string])
}

// layoutOnce makes one plan of the group's rules from in.
func layoutOnce(in Input) Output {
	l := &layout{
		in:      in,
		p:       in.Pattern.OrDefault(),
		members: map[string]*mstate{},
		chunks:  map[int][]string{},
		pats:    map[int][]string{},
		out:     &Output{Rules: map[string][]string{}, Refused: map[string][]string{}},
	}
	l.out.Unsafe = l.p.Unsafe
	l.loadMembers()
	l.loadRules()
	domains := l.domainOrder()
	all := make([]candRule, 0, len(in.Rules))
	for _, dk := range domains {
		all = append(all, l.planDomain(dk)...)
	}
	l.capRules(all)
	l.out.finish()
	return *l.out
}

// loadMembers records the members and picks, for every index that two
// members share, the newer one as the index's primary.
func (l *layout) loadMembers() {
	byIndex := map[int]*mstate{}
	for _, m := range l.in.Members {
		if m.Key == "" || l.members[m.Key] != nil {
			continue
		}
		s := &mstate{Member: m, primary: true, joinable: m.Joinable || m.Reserved}
		l.members[m.Key] = s
		if m.Index < 0 {
			continue
		}
		prev := byIndex[m.Index]
		if prev == nil {
			byIndex[m.Index] = s
			continue
		}
		newer, older := s, prev
		switch {
		case s.Reserved != prev.Reserved:
			if s.Reserved {
				newer, older = prev, s
			}
		case prev.Born > s.Born || prev.Born == s.Born && prev.Key > s.Key:
			newer, older = prev, s
		}
		older.primary, older.joinable = false, false
		byIndex[m.Index] = newer
	}
}

// loadRules records the group's rules now, keeping only present members.
func (l *layout) loadRules() {
	for name, keys := range l.in.Rules {
		var present []string
		for _, k := range keys {
			if l.members[k] != nil && !slices.Contains(present, k) {
				present = append(present, k)
			}
		}
		if n, ok := ChunkIndex(l.in.Base, name); ok {
			l.chunks[n] = present
			continue
		}
		if n, ok := PatternRuleIndex(l.in.Base, name); ok {
			l.pats[n] = present
		}
	}
}

// domainOrder returns the domains that hold members, by their number in
// rule names and then their key.
func (l *layout) domainOrder() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range l.members {
		if !seen[m.Domain] {
			seen[m.Domain] = true
			out = append(out, m.Domain)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(cmp.Compare(l.domain(a).Ord, l.domain(b).Ord), cmp.Compare(a, b))
	})
	return out
}

// domain returns the domain keyed dk.
func (l *layout) domain(dk string) Domain {
	if d, ok := l.in.Domains[dk]; ok {
		d.Key = dk
		return d
	}
	return Domain{Key: dk}
}

// rankCmp orders members by index, with members whose index is unknown
// last, an index's primary before an older member of the same index, and
// then by key.
func rankCmp(a, b *mstate) int {
	ai, bi := a.Index, b.Index
	if ai < 0 {
		ai = math.MaxInt
	}
	if bi < 0 {
		bi = math.MaxInt
	}
	pa, pb := 0, 0
	if !a.primary {
		pa = 1
	}
	if !b.primary {
		pb = 1
	}
	return cmp.Or(cmp.Compare(ai, bi), cmp.Compare(pa, pb), cmp.Compare(a.Key, b.Key))
}

// domainMembers returns the members of domain dk in rank order.
func (l *layout) domainMembers(dk string) []*mstate {
	var out []*mstate
	for _, m := range l.members {
		if m.Domain == dk {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, rankCmp)
	return out
}

// planDomain plans the rules of one domain and returns them, trimmed.
func (l *layout) planDomain(dk string) []candRule {
	dom := l.domain(dk)
	ms := l.domainMembers(dk)
	if dom.NodesKnown && dom.Nodes == 0 || dom.Nodes > 0 && dom.Nodes < 3 && !l.p.Unsafe {
		l.out.Small = append(l.out.Small, dk)
		return nil
	}
	var cands []candRule
	for ti, t := range l.p.Terms {
		term := ti + 1
		switch t.Kind {
		case KindBlock:
			cands = append(cands, l.planBlock(dom, ms, t, term)...)
		case KindStride:
			cands = append(cands, l.planStride(dom, ms, t, term)...)
		case KindWindow:
			cands = append(cands, l.planWindow(dom, ms, t, term)...)
		case KindSets:
			cands = append(cands, l.planSets(dom, ms, t, term)...)
		}
	}
	if !l.p.HasKind(KindBlock) {
		for _, m := range ms {
			if m.Index < 0 && m.joinable {
				l.out.Unindexed = append(l.out.Unindexed, m.Key)
			}
		}
	}
	kept, trimmed := l.trim(dom, ms, cands)
	kept = dropCoveredRuns(l.joinFixed(dom, kept))
	if trimmed {
		kept = l.keepLowestApart(dom, ms, cands, kept)
	}
	if !l.p.IsDefaultBlock() && lowestFree(ms, kept) && (dom.Nodes == 0 || dom.Nodes >= 3) {
		l.out.LowFree = append(l.out.LowFree, dk)
	}
	return kept
}

// blockCap returns a block term's largest rule size in dom: its size, or one
// fewer than the domain's nodes, whichever is smaller. A pattern that gave
// up drain safety may name a size up to the domain's nodes.
func blockCap(t Term, dom Domain, unsafe bool) int {
	limit := math.MaxInt
	if dom.Nodes > 0 {
		limit = dom.Nodes - 1
		if unsafe && t.Size > 0 {
			limit = dom.Nodes
		}
	}
	if t.Size > 0 {
		return min(t.Size, limit)
	}
	return limit
}

// joinRoom returns how many members a rule of cap members may grow to now.
func joinRoom(capacity int, dom Domain) int {
	if dom.PlaceableKnown || dom.Placeable > 0 {
		return min(capacity, max(dom.Placeable, 0))
	}
	return capacity
}

// planBlock fills the domain's block rules. The block rules of a group's
// domains take turns in their numbering, in domain order: with two domains,
// the first domain's places are rules 1, 3, and 5, and the second's are 2, 4,
// and 6. So a domain's n-th place is always the same rule, whatever the other
// domains hold.
func (l *layout) planBlock(dom Domain, ms []*mstate, t Term, term int) []candRule {
	spread, pos := l.spread(), max(dom.Ord-1, 0)
	f := filler{
		l: l, members: ms, existing: l.chunks,
		number:   func(slot int) int { return slot*spread + pos + 1 },
		name:     func(n int) string { return ChunkName(l.in.Base, n) },
		capacity: blockCap(t, dom, l.p.Unsafe),
		dom:      dom,
	}
	rules, singles := f.fill(term)
	if len(rules) > 0 {
		l.out.Alone = append(l.out.Alone, singles...)
	}
	return rules
}

// spread returns how many domains take turns in the block rule numbering:
// Input.Spread when set, and otherwise the highest domain Ord, or 1.
func (l *layout) spread() int {
	n := max(l.in.Spread, 1)
	for _, d := range l.in.Domains {
		n = max(n, d.Ord)
	}
	return n
}

// planStride fills the rules of each residue class of the domain.
func (l *layout) planStride(dom Domain, ms []*mstate, t Term, term int) []candRule {
	var out []candRule
	var singles []string
	for class := range t.K {
		var cms []*mstate
		for _, m := range ms {
			if m.Index >= 0 && m.Index%t.K == class {
				cms = append(cms, m)
			}
		}
		if len(cms) == 0 {
			continue
		}
		existing := map[int][]string{}
		for n, keys := range l.pats {
			d, tt, local := SplitPatternRuleNumber(n)
			c, chunk := SplitStrideLocal(local)
			if d == dom.Ord && tt == term && c == class && chunk >= 1 {
				existing[chunk] = keys
			}
		}
		f := filler{
			l: l, members: cms, existing: existing,
			number: func(slot int) int {
				if slot+1 > MaxChunk {
					return 0
				}
				return slot + 1
			},
			name: func(n int) string {
				return PatternRuleName(l.in.Base, PatternRuleNumber(dom.Ord, term, StrideLocal(class, n)))
			},
			capacity: blockCap(Term{}, dom, l.p.Unsafe),
			dom:      dom,
		}
		rules, alone := f.fill(term)
		out = append(out, rules...)
		singles = append(singles, alone...)
	}
	// A member alone in its class is unprotected once any class of the term
	// holds a rule.
	if len(out) > 0 {
		l.out.Alone = append(l.out.Alone, singles...)
	}
	return out
}

// filler lays out one sequence of rules, a block or one stride class, by
// position. The layout of the sequence is a function of its members and its
// holds alone, so it is the same whatever order the members joined in.
type filler struct {
	l       *layout
	members []*mstate
	// existing holds the rules of the sequence now, by number.
	existing map[int][]string
	// number returns the rule number of the sequence's place slot, counted
	// from 0, or 0 when the sequence has no number for it.
	number func(slot int) int
	// name returns the name of the rule numbered n.
	name     func(n int) string
	capacity int
	dom      Domain
}

// fill lays the members out. Each index's primary takes the next position
// in index order, held places included, and the n-th rule of the sequence
// holds the n-th run of capacity positions. Without split only the first run
// is written. A member keeps its place in its rule whether or not it may
// join, and a member that is not in its rule yet joins it while the rule has
// room and the join check lets it in. A member that may not join, or that the
// check refuses, still takes its position, so no other member shifts. The
// members with no position, an older member of an index and a member whose
// index is unknown, come after the positions are placed. Each keeps the rule
// it is in while the rule has room. A joinable member with no index that is
// in no rule then joins the lowest rule with members and room that lets it
// in, and when no rule with members has room, the lowest empty place. It
// returns the rules and the members left alone in a rule.
func (f *filler) fill(term int) ([]candRule, []string) {
	if f.capacity < 2 {
		return nil, nil
	}
	l := f.l
	room := joinRoom(f.capacity, f.dom)
	inRule := map[string]bool{}
	for _, keys := range f.existing {
		for _, k := range keys {
			inRule[k] = true
		}
	}
	var positions, extras []*mstate
	for _, m := range f.members {
		if m.primary && m.Index >= 0 {
			positions = append(positions, m)
		} else {
			extras = append(extras, m)
		}
	}
	var runs [][]*mstate
	for start := 0; start < len(positions); {
		end := min(start+f.capacity, len(positions))
		runs = append(runs, positions[start:end])
		start = end
	}
	slots := len(f.members) + 1
	if !l.in.Split {
		slots = 1
	}
	var numbers []int
	for s := range slots {
		n := f.number(s)
		if n == 0 {
			break
		}
		numbers = append(numbers, n)
	}
	desired := make([][]string, len(numbers))
	for slot, run := range runs {
		if slot >= len(numbers) {
			f.leaveOut(run, inRule)
			continue
		}
		desired[slot] = f.place(numbers[slot], run, room, inRule)
	}
	var joiners []*mstate
	for _, m := range extras {
		slot := slices.IndexFunc(numbers, func(n int) bool { return slices.Contains(f.existing[n], m.Key) })
		switch {
		case slot >= 0 && len(desired[slot]) < f.capacity:
			desired[slot] = append(desired[slot], m.Key)
		case m.primary && m.joinable:
			joiners = append(joiners, m)
		case inRule[m.Key]:
			l.out.Displaced = append(l.out.Displaced, m.Key)
		}
	}
	for _, m := range joiners {
		f.join(m, numbers, desired, room)
	}
	return f.rules(term, numbers, desired)
}

// join puts a joinable member with no index that is in no rule into the
// lowest rule with members and room that lets it in. When no rule with
// members has room, it takes the lowest empty place. A member that every rule
// with room refused stays out.
func (f *filler) join(m *mstate, numbers []int, desired [][]string, room int) {
	refused := false
	for slot, keys := range desired {
		if len(keys) == 0 || len(keys) >= room {
			continue
		}
		if !f.l.canJoin(f.name(numbers[slot]), keys, m.Key) {
			f.l.refuse(m.Key, f.name(numbers[slot]))
			refused = true
			continue
		}
		desired[slot] = append(keys, m.Key)
		return
	}
	if refused {
		return
	}
	for slot, keys := range desired {
		if len(keys) == 0 && room > 0 {
			desired[slot] = []string{m.Key}
			return
		}
	}
	f.l.out.Overflow = append(f.l.out.Overflow, m.Key)
}

// place settles who is in the rule numbered n, whose place holds run. A
// member the rule holds now stays. Every other member joins in rank order
// while the rule has room and the join check lets it in.
func (f *filler) place(n int, run []*mstate, room int, inRule map[string]bool) []string {
	l := f.l
	var keys []string
	for _, m := range run {
		if slices.Contains(f.existing[n], m.Key) {
			keys = append(keys, m.Key)
		}
	}
	for _, m := range run {
		switch {
		case slices.Contains(keys, m.Key):
		case !m.joinable:
			if inRule[m.Key] {
				l.out.Displaced = append(l.out.Displaced, m.Key)
			}
		case len(keys) >= room:
			l.out.Overflow = append(l.out.Overflow, m.Key)
		case len(keys) > 0 && !l.canJoin(f.name(n), keys, m.Key):
			l.refuse(m.Key, f.name(n))
		default:
			keys = append(keys, m.Key)
		}
	}
	return keys
}

// leaveOut records the members of a run that no rule is written for: those
// that may join as left out for want of room, and those that may not as
// displaced when they had a place.
func (f *filler) leaveOut(run []*mstate, inRule map[string]bool) {
	for _, m := range run {
		switch {
		case m.joinable:
			f.l.out.Overflow = append(f.l.out.Overflow, m.Key)
		case inRule[m.Key]:
			f.l.out.Displaced = append(f.l.out.Displaced, m.Key)
		}
	}
}

// rules turns a fill's desired members into candidate rules, and returns the
// members left alone in a rule, which the caller reports as unprotected when
// the term writes any rule.
func (f *filler) rules(term int, numbers []int, desired [][]string) ([]candRule, []string) {
	var out []candRule
	var singles []string
	for slot, keys := range desired {
		n := numbers[slot]
		if n == 0 {
			continue
		}
		if len(keys) == 1 {
			singles = append(singles, keys[0])
			if f.l.out.AloneIn == nil {
				f.l.out.AloneIn = map[string]string{}
			}
			f.l.out.AloneIn[keys[0]] = f.name(n)
		}
		if len(keys) < 2 {
			continue
		}
		c := candRule{name: f.name(n), term: term}
		for _, k := range keys {
			c.members = append(c.members, f.l.members[k])
		}
		c.low = lowIndex(c.members)
		out = append(out, c)
	}
	return out, singles
}

// lowIndex returns the lowest known index among ms, or math.MaxInt.
func lowIndex(ms []*mstate) int {
	low := math.MaxInt
	for _, m := range ms {
		if m.Index >= 0 && m.Index < low {
			low = m.Index
		}
	}
	return low
}

// fixedMembers returns the members of a window or a set: the index primaries
// of ms whose index is in want, that may join the rule or that the rule named
// name holds now.
func (l *layout) fixedMembers(ms []*mstate, want func(int) bool, n int) ([]*mstate, map[string]bool) {
	current := map[string]bool{}
	for _, k := range l.pats[n] {
		current[k] = true
	}
	var out []*mstate
	for _, m := range ms {
		if m.Index < 0 || m.Index > MaxIndex || !m.primary || !want(m.Index) {
			continue
		}
		if m.joinable || current[m.Key] {
			out = append(out, m)
		}
	}
	return out, current
}

// planWindow asks for one rule over every run of K consecutive indexes that
// holds two members or more. A run whose members another run holds stays a
// candidate, because trimming may drop the other run, and dropCoveredRuns
// leaves it out after the trim only when the other run is kept.
func (l *layout) planWindow(dom Domain, ms []*mstate, t Term, term int) []candRule {
	var starts []int
	for _, m := range ms {
		if m.Index >= 0 && m.Index <= MaxIndex && m.primary && !slices.Contains(starts, m.Index) {
			starts = append(starts, m.Index)
		}
	}
	slices.Sort(starts)
	var out []candRule
	for _, s := range starts {
		n := PatternRuleNumber(dom.Ord, term, s)
		members, current := l.fixedMembers(ms, func(i int) bool { return i >= s && i < s+t.K }, n)
		if len(members) < 2 {
			continue
		}
		out = append(out, candRule{name: PatternRuleName(l.in.Base, n), members: members, term: term,
			low: lowIndex(members), fixed: true, current: current, window: true})
	}
	return out
}

// dropCoveredRuns leaves out each window run whose members a kept run of the
// same term holds, because it adds nothing. It runs after the trim and the
// joins, so a run whose cover the trim dropped, or whose cover lost a member
// to a refused join, still protects its members.
func dropCoveredRuns(kept []candRule) []candRule {
	out := make([]candRule, 0, len(kept))
	for i, c := range kept {
		covered := false
		for j, o := range kept {
			if i == j || !c.window || !o.window || c.term != o.term || !subset(c.members, o.members) {
				continue
			}
			if len(c.members) < len(o.members) || j < i {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, c)
		}
	}
	return out
}

// planSets asks for one rule over each set that holds two members or more.
func (l *layout) planSets(dom Domain, ms []*mstate, t Term, term int) []candRule {
	var out []candRule
	for i, set := range t.Sets {
		n := PatternRuleNumber(dom.Ord, term, i+1)
		members, current := l.fixedMembers(ms, func(x int) bool { return slices.Contains(set, x) }, n)
		if len(members) < 2 {
			continue
		}
		out = append(out, candRule{name: PatternRuleName(l.in.Base, n), members: members, term: term,
			low: lowIndex(members), fixed: true, current: current})
	}
	return out
}

// subset reports whether every member of a is in b.
func subset(a, b []*mstate) bool {
	for _, m := range a {
		if !slices.Contains(b, m) {
			return false
		}
	}
	return true
}

// trim adds the candidate rules in order of their lowest index and then of
// their term, and leaves out each one whose addition would break the
// domain's bound. A drain-safe pattern allows each member at most N-2
// partners, and a pattern that gave up drain safety allows at most N members
// per rule and a layout that a greedy coloring fits on N nodes.
func (l *layout) trim(dom Domain, ms []*mstate, cands []candRule) ([]candRule, bool) {
	slices.SortStableFunc(cands, func(a, b candRule) int {
		return cmp.Or(cmp.Compare(a.low, b.low), cmp.Compare(a.term, b.term), cmp.Compare(a.name, b.name))
	})
	if dom.Nodes <= 0 {
		return cands, false
	}
	partners := map[string]map[string]bool{}
	var kept []candRule
	trimmed := false
	for _, c := range cands {
		if !l.fits(dom, ms, partners, c.members) {
			l.out.Trimmed = append(l.out.Trimmed, c.name)
			trimmed = true
			continue
		}
		addPartners(partners, c.members)
		kept = append(kept, c)
	}
	return kept, trimmed
}

// fits reports whether adding a rule over members keeps the domain within its
// bound.
func (l *layout) fits(dom Domain, ms []*mstate, partners map[string]map[string]bool, members []*mstate) bool {
	if dom.Nodes <= 0 {
		return true
	}
	if !l.p.Unsafe {
		for _, m := range members {
			n := len(partners[m.Key])
			for _, o := range members {
				if o != m && !partners[m.Key][o.Key] {
					n++
				}
			}
			if n > dom.Nodes-2 {
				return false
			}
		}
		return true
	}
	if len(members) > dom.Nodes {
		return false
	}
	trial := map[string]map[string]bool{}
	for k, v := range partners {
		trial[k] = map[string]bool{}
		for o := range v {
			trial[k][o] = true
		}
	}
	addPartners(trial, members)
	return colorable(ms, trial, dom.Nodes)
}

// addPartners records every pair of members as partners.
func addPartners(partners map[string]map[string]bool, members []*mstate) {
	for _, m := range members {
		for _, o := range members {
			if m == o {
				continue
			}
			if partners[m.Key] == nil {
				partners[m.Key] = map[string]bool{}
			}
			partners[m.Key][o.Key] = true
		}
	}
}

// colorable reports whether a greedy coloring in rank order places every
// member on one of n nodes with none of its partners.
func colorable(ms []*mstate, partners map[string]map[string]bool, n int) bool {
	color := map[string]int{}
	for _, m := range ms {
		taken := map[int]bool{}
		for o := range partners[m.Key] {
			if c, ok := color[o]; ok {
				taken[c] = true
			}
		}
		c := 0
		for taken[c] {
			c++
		}
		if c >= n {
			return false
		}
		color[m.Key] = c
	}
	return true
}

// joinFixed settles who is in each window and set rule that survived
// trimming. A member the rule holds now stays. Every other member joins in
// index order while the rule has room and the join brings on no conflict.
func (l *layout) joinFixed(dom Domain, kept []candRule) []candRule {
	capacity := math.MaxInt
	if dom.Nodes > 0 {
		capacity = dom.Nodes - 1
		if l.p.Unsafe {
			capacity = dom.Nodes
		}
	}
	room := joinRoom(capacity, dom)
	out := kept[:0]
	for _, c := range kept {
		if !c.fixed {
			out = append(out, c)
			continue
		}
		var final []*mstate
		for _, m := range c.members {
			if c.current[m.Key] {
				final = append(final, m)
			}
		}
		for _, m := range c.members {
			if c.current[m.Key] {
				continue
			}
			keys := memberKeys(final)
			switch {
			case len(final) >= room:
				l.out.Overflow = append(l.out.Overflow, m.Key)
			case len(final) > 0 && !l.canJoin(c.name, keys, m.Key):
				l.refuse(m.Key, c.name)
			default:
				final = append(final, m)
			}
		}
		if len(final) < 2 {
			continue
		}
		c.members = final
		out = append(out, c)
	}
	return out
}

// keepLowestApart adds a rule over the domain's two lowest indexes when the
// pattern kept them apart but trimming left them free to share a node, as
// long as the rule keeps the domain within its bound.
func (l *layout) keepLowestApart(dom Domain, ms []*mstate, cands, kept []candRule) []candRule {
	a, b := lowestTwo(ms)
	if a == nil || !partnered(cands, a, b) || partnered(kept, a, b) {
		return kept
	}
	partners := map[string]map[string]bool{}
	for _, c := range kept {
		addPartners(partners, c.members)
	}
	pair := []*mstate{a, b}
	if !l.fits(dom, ms, partners, pair) {
		return kept
	}
	n := PatternRuleNumber(dom.Ord, 0, 0)
	name := PatternRuleName(l.in.Base, n)
	inRule := map[string]bool{}
	for _, k := range l.pats[n] {
		inRule[k] = true
	}
	for _, m := range pair {
		if !inRule[m.Key] && !m.joinable {
			return kept
		}
	}
	// When neither is in the rule, a is its first member and b joins it.
	holder, joiner := a, b
	if inRule[b.Key] && !inRule[a.Key] {
		holder, joiner = b, a
	}
	if !inRule[joiner.Key] && !l.canJoin(name, []string{holder.Key}, joiner.Key) {
		l.refuse(joiner.Key, name)
		return kept
	}
	l.out.AddedFirst = append(l.out.AddedFirst, name)
	return append(kept, candRule{name: name, members: pair, term: 0, low: lowIndex(pair)})
}

// lowestTwo returns the two lowest-indexed primaries of ms, or nils.
func lowestTwo(ms []*mstate) (*mstate, *mstate) {
	var out []*mstate
	for _, m := range ms {
		if m.Index >= 0 && m.primary {
			out = append(out, m)
			if len(out) == 2 {
				return out[0], out[1]
			}
		}
	}
	return nil, nil
}

// partnered reports whether any rule of rules holds both a and b.
func partnered(rules []candRule, a, b *mstate) bool {
	for _, c := range rules {
		if slices.Contains(c.members, a) && slices.Contains(c.members, b) {
			return true
		}
	}
	return false
}

// lowestFree reports whether the domain has two indexed members and no rule
// of rules keeps its two lowest apart.
func lowestFree(ms []*mstate, rules []candRule) bool {
	a, b := lowestTwo(ms)
	return a != nil && !partnered(rules, a, b)
}

// capRules keeps at most MaxRules rules, the ones with the lowest indexes,
// and records the rest as capped.
func (l *layout) capRules(all []candRule) {
	limit := l.in.MaxRules
	if limit <= 0 {
		limit = DefaultMaxRules
	}
	slices.SortStableFunc(all, func(a, b candRule) int {
		return cmp.Or(cmp.Compare(a.low, b.low), cmp.Compare(a.term, b.term), cmp.Compare(a.name, b.name))
	})
	for i, c := range all {
		if i >= limit {
			l.out.Capped = append(l.out.Capped, c.name)
			continue
		}
		keys := memberKeys(c.members)
		slices.Sort(keys)
		l.out.Rules[c.name] = keys
	}
}

// finish sorts and dedupes the diagnostics.
func (o *Output) finish() {
	for _, list := range []*[]string{&o.Trimmed, &o.Capped, &o.Overflow, &o.Alone,
		&o.Displaced, &o.Unindexed, &o.LowFree, &o.AddedFirst, &o.Small} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	placed := map[string]bool{}
	for _, keys := range o.Rules {
		for _, k := range keys {
			placed[k] = true
		}
	}
	// A member some rule holds was not left out, whatever another term said.
	for _, list := range []*[]string{&o.Overflow, &o.Displaced, &o.Unindexed} {
		*list = slices.DeleteFunc(*list, func(k string) bool { return placed[k] })
	}
	for k, rules := range o.Refused {
		if placed[k] {
			delete(o.Refused, k)
			continue
		}
		slices.Sort(rules)
		o.Refused[k] = slices.Compact(rules)
	}
	o.Overflow = slices.DeleteFunc(o.Overflow, func(k string) bool { return o.Refused[k] != nil })
}

// canJoin asks the caller whether key may join the rule.
func (l *layout) canJoin(rule string, members []string, key string) bool {
	if l.in.CanJoin == nil || l.reserved(key) {
		return true
	}
	members = slices.DeleteFunc(slices.Clone(members), l.reserved)
	if len(members) == 0 {
		return true
	}
	return l.in.CanJoin(rule, members, key)
}

// reserved reports whether key is the reserved place of a member that left.
func (l *layout) reserved(key string) bool {
	m := l.members[key]
	return m != nil && m.Reserved
}

// refuse records that rule refused key's join.
func (l *layout) refuse(key, rule string) {
	l.out.Refused[key] = append(l.out.Refused[key], rule)
}

// memberKeys returns the keys of ms.
func memberKeys(ms []*mstate) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Key)
	}
	return out
}

// Partners returns, for every member any rule names, the members that share
// a rule with it.
func Partners(rules map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, keys := range rules {
		for _, k := range keys {
			for _, o := range keys {
				if k == o {
					continue
				}
				if out[k] == nil {
					out[k] = map[string]bool{}
				}
				out[k][o] = true
			}
		}
	}
	return out
}
