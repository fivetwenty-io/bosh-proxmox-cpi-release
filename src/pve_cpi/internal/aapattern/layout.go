package aapattern

import (
	"cmp"
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
}

// Domain is a set of nodes a group's members may run on. Rules only ever
// name members of one domain.
type Domain struct {
	Key string
	// Ord numbers the domain in pattern rule names: 0 for the whole
	// cluster, and an AZ's position plus one otherwise.
	Ord int
	// Nodes is the domain's configured node count, N, or 0 when it is
	// unknown, which turns off the cap and the drain-safety check.
	Nodes int
	// Placeable is how many of the domain's nodes can take a VM now, or 0
	// when that is unknown. A member joins a rule only while the rule holds
	// fewer members than this.
	Placeable int
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
	// Refused maps each joinable member that every rule with room refused
	// to the rules that refused it.
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
	used    map[int]bool
	out     *Output
}

// Layout plans the rules of one group.
func Layout(in Input) Output {
	l := &layout{
		in:      in,
		p:       in.Pattern.OrDefault(),
		members: map[string]*mstate{},
		chunks:  map[int][]string{},
		pats:    map[int][]string{},
		used:    map[int]bool{},
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
	l.finish()
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
		s := &mstate{Member: m, primary: true, joinable: m.Joinable}
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
		if prev.Born > s.Born || prev.Born == s.Born && prev.Key > s.Key {
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
	// Every block rule is claimed by the domain of its best-ranked member,
	// and its number stays in use while it is claimed.
	for n, keys := range l.chunks {
		if len(keys) > 0 {
			l.used[n] = true
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

// best returns the best-ranked present member of keys, or nil.
func (l *layout) best(keys []string) *mstate {
	var b *mstate
	for _, k := range keys {
		m := l.members[k]
		if m != nil && (b == nil || rankCmp(m, b) < 0) {
			b = m
		}
	}
	return b
}

// planDomain plans the rules of one domain and returns them, trimmed.
func (l *layout) planDomain(dk string) []candRule {
	dom := l.domain(dk)
	ms := l.domainMembers(dk)
	if dom.Nodes > 0 && dom.Nodes < 3 && !l.p.Unsafe {
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
	if dom.Placeable > 0 {
		return min(capacity, dom.Placeable)
	}
	return capacity
}

// planBlock fills the domain's block rules.
func (l *layout) planBlock(dom Domain, ms []*mstate, t Term, term int) []candRule {
	// The domain's rules are the block rules whose best-ranked member is in
	// it.
	existing := map[int][]string{}
	for n, keys := range l.chunks {
		if b := l.best(keys); b != nil && b.Domain == dom.Key {
			existing[n] = keys
		}
	}
	f := filler{
		l: l, members: ms, existing: existing,
		name: func(n int) string { return ChunkName(l.in.Base, n) },
		next: func() int {
			for n := 1; ; n++ {
				if !l.used[n] {
					l.used[n] = true
					return n
				}
			}
		},
		peek: func() int {
			n := 1
			for l.used[n] {
				n++
			}
			return n
		},
		release:  func(n int) { delete(l.used, n) },
		capacity: blockCap(t, dom, l.p.Unsafe),
		dom:      dom,
	}
	rules, singles := f.fill(term)
	if len(rules) > 0 {
		l.out.Alone = append(l.out.Alone, singles...)
	}
	return rules
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
		existing := map[int][]string{}
		used := map[int]bool{}
		for n, keys := range l.pats {
			d, tt, local := SplitPatternRuleNumber(n)
			c, chunk := SplitStrideLocal(local)
			if d == dom.Ord && tt == term && c == class && chunk >= 1 && len(keys) > 0 {
				existing[chunk] = keys
				used[chunk] = true
			}
		}
		if len(cms) == 0 {
			continue
		}
		f := filler{
			l: l, members: cms, existing: existing,
			name: func(n int) string {
				return PatternRuleName(l.in.Base, PatternRuleNumber(dom.Ord, term, StrideLocal(class, n)))
			},
			next: func() int {
				for n := 1; n <= MaxChunk; n++ {
					if !used[n] {
						used[n] = true
						return n
					}
				}
				return 0
			},
			peek: func() int {
				for n := 1; n <= MaxChunk; n++ {
					if !used[n] {
						return n
					}
				}
				return 0
			},
			release:  func(n int) { delete(used, n) },
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

// filler fills one sequence of rules, a block or one stride class, from the
// lowest index.
type filler struct {
	l        *layout
	members  []*mstate
	existing map[int][]string
	name     func(int) string
	next     func() int
	peek     func() int
	release  func(int)
	capacity int
	dom      Domain
}

// fillRun carries one fill's working state.
type fillRun struct {
	room    int
	inChunk map[string]int
	order   []int
	ranked  []*mstate
	desired map[int][]string
	placed  map[string]bool
	opened  map[int]bool
	first   int
	rest    []int
}

// fill lays the members out. The first rule holds the lowest-ranked members
// up to the cap. Every other member keeps its rule while the rule is within
// the cap, and the rest join the lowest rule with room, in index order, or
// open a new one when split allows it. It returns the rules and the members
// left alone in a rule.
func (f *filler) fill(term int) ([]candRule, []string) {
	if f.capacity < 2 {
		return nil, nil
	}
	r := f.start()
	f.fillFirst(r)
	r.rest = r.order
	if len(r.rest) > 0 && r.rest[0] == r.first {
		r.rest = r.rest[1:]
	}
	if f.l.in.Split {
		f.keepRest(r)
	}
	f.placeRest(r)
	all := append([]int{}, r.rest...)
	if r.first != 0 {
		all = append([]int{r.first}, all...)
	}
	// A number this fill opened for a rule that ends below two members is
	// handed back, so the next domain's rules number on from the last rule
	// written.
	for n := range r.opened {
		if len(r.desired[n]) < 2 {
			f.release(n)
		}
	}
	return f.rules(term, all, r.desired)
}

// start records which rule holds each member now and ranks the members the
// fill places: those in a rule and those that may join one.
func (f *filler) start() *fillRun {
	r := &fillRun{
		room:    joinRoom(f.capacity, f.dom),
		inChunk: map[string]int{},
		desired: map[int][]string{},
		placed:  map[string]bool{},
		opened:  map[int]bool{},
	}
	for n := range f.existing {
		r.order = append(r.order, n)
	}
	slices.Sort(r.order)
	for _, n := range r.order {
		for _, k := range f.existing[n] {
			if _, ok := r.inChunk[k]; !ok {
				r.inChunk[k] = n
			}
		}
	}
	for _, m := range f.members {
		if _, in := r.inChunk[m.Key]; in || m.joinable {
			r.ranked = append(r.ranked, m)
		}
	}
	// The first rule is the lowest-numbered rule the fill holds, unless a
	// lower number is free, so the base name always leads and skip keeps
	// only it.
	if len(r.order) > 0 {
		if p := f.peek(); p == 0 || p > r.order[0] {
			r.first = r.order[0]
		}
	}
	return r
}

// fillFirst fills the first rule with the lowest-ranked members up to the
// cap. A member already in it stays, and a new one joins while there is room
// and the rule takes it. A new member whose index is unknown only fills the
// room the members ranked by index leave, so it never displaces a member
// already in the rule, and one that a later rule holds keeps that rule,
// because the key order it ranks by is no reason to move it.
func (f *filler) fillFirst(r *fillRun) {
	l := f.l
	order := make([]*mstate, 0, len(r.ranked))
	var unindexed []*mstate
	for _, m := range r.ranked {
		n, held := r.inChunk[m.Key]
		switch {
		case m.Index >= 0 || r.first != 0 && n == r.first:
			order = append(order, m)
		case !held:
			unindexed = append(unindexed, m)
		}
	}
	order = append(order, unindexed...)
	for _, m := range order {
		if r.first != 0 && len(r.desired[r.first]) >= f.capacity {
			return
		}
		switch {
		case r.first != 0 && r.inChunk[m.Key] == r.first:
		case m.joinable && (r.first == 0 || len(r.desired[r.first]) < r.room):
			if r.first != 0 && len(r.desired[r.first]) > 0 &&
				!l.canJoin(f.name(r.first), r.desired[r.first], m.Key) {
				l.refuse(m.Key, f.name(r.first))
				continue
			}
			if r.first == 0 {
				r.first = f.next()
				if r.first == 0 {
					continue
				}
				r.opened[r.first] = true
				r.order = append([]int{r.first}, r.order...)
			}
		default:
			continue
		}
		r.desired[r.first] = append(r.desired[r.first], m.Key)
		r.placed[m.Key] = true
	}
}

// keepRest keeps every member of a later rule in it while the rule is within
// the cap, in rank order.
func (f *filler) keepRest(r *fillRun) {
	for _, n := range r.rest {
		for _, m := range r.ranked {
			if !r.placed[m.Key] && r.inChunk[m.Key] == n && len(r.desired[n]) < f.capacity {
				r.desired[n] = append(r.desired[n], m.Key)
				r.placed[m.Key] = true
			}
		}
	}
}

// placeRest places the members still pending. Under split each one fits the
// lowest rule that takes it, and otherwise it is left out and recorded.
func (f *filler) placeRest(r *fillRun) {
	l := f.l
	for _, m := range r.ranked {
		if r.placed[m.Key] {
			continue
		}
		if !m.joinable {
			l.out.Displaced = append(l.out.Displaced, m.Key)
			continue
		}
		if !l.in.Split {
			l.out.Overflow = append(l.out.Overflow, m.Key)
			continue
		}
		n, isNew := f.fit(m, r.rest, r.desired, r.room)
		if n == 0 {
			continue
		}
		if isNew {
			r.opened[n] = true
			r.rest = append(r.rest, n)
		}
		r.desired[n] = append(r.desired[n], m.Key)
		r.placed[m.Key] = true
	}
}

// fit finds the rule a pending member joins: the lowest rule after the first
// that holds members and has room and that takes it, then a rule a delete
// left with no members when no rule refused it, and then a new rule when no
// rule had room. It returns 0 when the member stays out, and records why.
func (f *filler) fit(m *mstate, rest []int, desired map[int][]string, room int) (int, bool) {
	l := f.l
	roomy := false
	var refused []string
	for _, n := range rest {
		if len(desired[n]) == 0 || len(desired[n]) >= room {
			continue
		}
		roomy = true
		if !l.canJoin(f.name(n), desired[n], m.Key) {
			refused = append(refused, f.name(n))
			continue
		}
		return n, false
	}
	for _, n := range rest {
		if len(desired[n]) != 0 || room <= 0 {
			continue
		}
		roomy = true
		if len(refused) == 0 {
			return n, false
		}
	}
	if !roomy && room > 0 {
		if n := f.next(); n != 0 {
			return n, true
		}
	}
	if len(refused) > 0 {
		for _, r := range refused {
			l.refuse(m.Key, r)
		}
	} else {
		l.out.Overflow = append(l.out.Overflow, m.Key)
	}
	return 0, false
}

// rules turns a fill's desired members into candidate rules, and returns the
// members left alone in a rule, which the caller reports as unprotected when
// the term writes any rule.
func (f *filler) rules(term int, order []int, desired map[int][]string) ([]candRule, []string) {
	var out []candRule
	var singles []string
	for _, n := range order {
		keys := desired[n]
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
func (l *layout) finish() {
	for _, list := range []*[]string{&l.out.Trimmed, &l.out.Capped, &l.out.Overflow, &l.out.Alone,
		&l.out.Displaced, &l.out.Unindexed, &l.out.LowFree, &l.out.AddedFirst, &l.out.Small} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	placed := map[string]bool{}
	for _, keys := range l.out.Rules {
		for _, k := range keys {
			placed[k] = true
		}
	}
	// A member some rule holds was not left out, whatever another term said.
	for _, list := range []*[]string{&l.out.Overflow, &l.out.Displaced, &l.out.Unindexed} {
		*list = slices.DeleteFunc(*list, func(k string) bool { return placed[k] })
	}
	for k, rules := range l.out.Refused {
		if placed[k] {
			delete(l.out.Refused, k)
			continue
		}
		slices.Sort(rules)
		l.out.Refused[k] = slices.Compact(rules)
	}
	l.out.Overflow = slices.DeleteFunc(l.out.Overflow, func(k string) bool { return l.out.Refused[k] != nil })
}

// canJoin asks the caller whether key may join the rule.
func (l *layout) canJoin(rule string, members []string, key string) bool {
	if l.in.CanJoin == nil || len(members) == 0 {
		return true
	}
	return l.in.CanJoin(rule, slices.Clone(members), key)
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
