package handlers

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// aaUnindexedGrace is how long a member with no readable instance index waits
// for its set_vm_metadata before the plan places it anyway, after every
// indexed member. A member whose birth is unknown has waited long enough.
const aaUnindexedGrace = 30 * time.Minute

// indexTagPrefix starts the tag set_vm_metadata writes with the VM's BOSH
// instance index, index--<n>.
const indexTagPrefix = "index--"

// aaPlanner plans a group's negative rules from the member scan and a read of
// the rules. It is called again after every write with a fresh read, so it
// returns no writes once the rules hold the plan.
//
// The layout itself is aapattern.Layout's. The group's pattern is the one the
// newest member's create named through ha_anti_affinity, or
// pve.ha.anti_affinity_pattern when no member named one, and the default is
// the block pattern. Each rule holds at most one member fewer than its
// domain's configured nodes, filled from the lowest instance index, so the
// two lowest indexes are always apart and a rolling drain always finds a node
// for every member. With pve.ha.anti_affinity_overflow split, the default, the
// members past the first rule spill into further rules. With skip only the
// first rule is written. A member joins a rule only while the rule holds fewer
// members than the domain's placeable nodes, so a node in maintenance never
// makes the CPI rewrite a rule, and a replacement member waits until it can be
// placed apart.
//
// A scanned sibling joins a rule only once it is an HA resource. PVE refuses
// a rule that names a resource it does not manage, and the Director creates an
// instance group's VMs in parallel, so a sibling whose create has tagged its
// guest but not yet staged its resource would make every write fail. Such a
// sibling keeps any place it has and joins no new rule. Every write carries the
// digest of the read it was planned from, so no write drops another's member
// unseen, and the group converges on every sibling. The scan is older than the
// read, so a plan that would drop a member whose guest the scan missed scans
// again first.
type aaPlanner struct {
	ident haIdentity
	// sid is the member the plan is for, or "". Outside a heal it joins
	// whether or not it is an HA resource yet, because its caller found or
	// staged its resource first.
	sid string
	// index is sid's instance index when the caller knows it, as
	// set_vm_metadata does, or -1, which reads it from the scan like any
	// other member's.
	index int
	// checkSelf runs the feasibility check on sid's own join too, so a join
	// that would make the rules conflict leaves sid out instead of failing the
	// write. set_vm_metadata sets it, because it never fails on a rule write.
	checkSelf bool
	// heal plans the group's rules again after a delete, for no create of its
	// own. sid then joins only while it is an HA resource, like every scanned
	// member, so a member that another removal purges while the heal runs
	// drops out of the plan on the next read of the resources.
	heal bool
	// exclude is the sid of a member that delete_vm is removing, or "". The
	// plan drops it from the rules and never adds it.
	exclude string
	// excludeIndex is exclude's instance index when the caller knows it, or
	// -1, which reads it from the scan. While the index is known, the plan
	// holds exclude's place as a reserved place, in the rule whose left=
	// record names it or else by the index, so no member moves into it and
	// the recreate of the index takes it back with one write.
	excludeIndex int
	// holdOut holds the members of the confinement pins that a repair took
	// out of the group's writes. A heal, or a create that plans again after
	// such a repair, keeps such a member where a rule still holds it and adds
	// it to no rule, because the write that adds it would put its pin out of
	// effect again. The plan finds a conflict that the CPI's own feasibility
	// checks can see by itself (see joinConflicts), so holdOut matters only
	// for one that PVE alone reports.
	holdOut map[string]bool
	scan    *groupScan
	logger  *log.Logger
	// clusterNodes is the configured node set, or nil when it is unknown.
	// The feasibility checks use it, and nil turns off the plan's own check
	// of a join. A member's soft pin is widened to it (see flattenMemberPins).
	clusterNodes []string
	// topo resolves each member's placement domain and the domain's node
	// counts.
	topo aaTopology
	// split is pve.ha.anti_affinity_overflow split.
	split bool
	// pattern is pve.ha.anti_affinity_pattern, the pattern of a group whose
	// members named none.
	pattern aapattern.Pattern
	// now reads the clock that ages a member with no index.
	now func() time.Time
	// managed lists the HA resources PVE manages, each with its comment. It
	// is read on every plan. Nil treats every scanned member as managed.
	managed func() (map[string]string, error)
	// rescan scans the group's members again. A plan calls it before it
	// drops a rule member whose guest the scan did not see, because the scan
	// is older than the rule read it plans from. Nil keeps the scan.
	rescan func() (*groupScan, error)

	// placedIn is the rule this VM is in after the last plan, or "". With a
	// pattern of several terms the VM can be in several rules, and placedIn
	// names the first of them by name.
	placedIn string
	// managedNow is the last plan's read of managed, or nil.
	managedNow map[string]string
	// leaving holds the members the group's rules record as leaving, which
	// a delete took out of a rule ahead of its purge.
	leaving map[string]bool
	// said holds the log lines already written, so each says its piece once
	// per planner however many rounds the writer runs.
	said map[string]bool
	// conflicts holds the conflicts each refused join brought on in the last
	// plan, keyed by the member.
	conflicts map[string]map[string][]string
	// lastIn and lastOut are the last plan's layout input and output, which
	// the doctor's check of a group reads.
	lastIn  aapattern.Input
	lastOut aapattern.Output
	// lastRecorded says the last plan's pattern came from a member's
	// create or from the record on the group's rules, not from
	// pve.ha.anti_affinity_pattern.
	lastRecorded bool
	// stamp is the canonical pattern the last plan records on each of the
	// group's rules, or "" when a member records the pattern. A group whose
	// members came from a CPI that recorded no pattern keeps the one it was
	// first planned with this way, so a later change of the job default does
	// not move its members.
	stamp string
	// stampOnly names the rules whose only write records the stamp.
	stampOnly map[string]bool
}

// once reports whether the line keyed key has not been logged yet, and marks
// it logged.
func (p *aaPlanner) once(key string) bool {
	if p.said[key] {
		return false
	}
	if p.said == nil {
		p.said = map[string]bool{}
	}
	p.said[key] = true
	return true
}

func (p *aaPlanner) plan(set *ha.RuleSet) ([]ha.RuleOp, error) {
	p.placedIn = ""
	if p.managed != nil {
		managed, err := p.managed()
		if err != nil {
			return nil, fmt.Errorf("anti-affinity: list HA resources: %w", err)
		}
		p.managedNow = managed
	}
	existing, err := p.existingRules(set)
	if err != nil {
		return nil, err
	}
	p.leaving = map[string]bool{}
	for _, r := range existing {
		if m, ok := parseHaMarker(r.Comment); ok {
			for _, s := range m.Left {
				p.leaving[s] = true
			}
		}
	}
	if p.rescan != nil && p.wouldDropUnseen(existing) {
		scan, err := p.rescan()
		if err != nil {
			return nil, fmt.Errorf("anti-affinity: scan group members again: %w", err)
		}
		p.scan = scan
	}
	in := p.layoutInput(set, existing)
	p.conflicts = nil
	out := aapattern.Layout(in)
	p.lastIn, p.lastOut = in, out
	p.logLayout(in, out)
	return p.ruleOps(set, existing, out), nil
}

// existingRules returns the group's rules by name. A rule under one of the
// group's names that does not carry this Director's marker is not ours to
// rewrite, so it fails the plan.
func (p *aaPlanner) existingRules(set *ha.RuleSet) (map[string]*ha.Rule, error) {
	base := p.ident.ruleName()
	existing := map[string]*ha.Rule{}
	for i := range set.Rules {
		r := &set.Rules[i]
		if !aapattern.IsGroupRule(base, r.ID) {
			continue
		}
		if haOwnershipOf(r.Comment, p.ident.Owner) != haOwnedByUs || r.Type != haRuleType {
			return nil, fmt.Errorf("anti-affinity: rule %q exists without this Director's ownership marker; "+
				"leaving it alone", r.ID)
		}
		existing[r.ID] = r
	}
	return existing, nil
}

// wouldDropUnseen reports whether the plan would drop a member of an existing
// rule because the scan saw no live guest for it. The excluded member goes
// whatever the scan says, so it never calls for a rescan.
func (p *aaPlanner) wouldDropUnseen(existing map[string]*ha.Rule) bool {
	if !p.scan.liveComplete {
		return false
	}
	for _, r := range existing {
		for _, s := range r.Resources {
			if s == p.exclude {
				continue
			}
			if _, ok := p.scan.live[s]; !ok {
				return true
			}
		}
	}
	return false
}

// layoutInput builds the layout's input from the scan, the last read of the
// HA resources, and the group's rules now.
func (p *aaPlanner) layoutInput(set *ha.RuleSet, existing map[string]*ha.Rule) aapattern.Input {
	rules := make(map[string][]string, len(existing))
	held := map[string]bool{}
	for name, r := range existing {
		rules[name] = slices.Clone(r.Resources)
		for _, s := range r.Resources {
			held[s] = true
		}
	}
	keys := map[string]bool{}
	for s := range p.scan.members {
		keys[s] = true
	}
	for s := range held {
		keys[s] = true
	}
	if p.sid != "" {
		keys[p.sid] = true
	}
	delete(keys, p.exclude)
	members := make([]aapattern.Member, 0, len(keys)+1)
	domainKeys := map[string]bool{}
	var newest aaPatternSource
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		m, ok := p.member(set, k, held[k])
		if !ok {
			continue
		}
		members = append(members, m)
		domainKeys[m.Domain] = true
		newest.consider(k, p.markerOf(k))
	}
	if m, ok := p.reservedPlace(set, existing, rules); ok {
		members = append(members, m)
		domainKeys[m.Domain] = true
	}
	return aapattern.Input{
		Base:    p.ident.ruleName(),
		Pattern: p.groupPattern(newest, existing),
		Split:   p.split,
		Domains: p.topo.domains(domainKeys),
		Members: members,
		Rules:   rules,
		CanJoin: func(rule string, ms []string, key string) bool {
			return p.canJoin(set, existing[rule], rule, ms, key)
		},
	}
}

// reservedPlace returns the reserved place of the member a delete removes,
// and false when there is none or its index is unknown. A rule whose left=
// record names the member holds the place in rules, so the plan keeps it
// where the member was.
func (p *aaPlanner) reservedPlace(
	set *ha.RuleSet, existing map[string]*ha.Rule, rules map[string][]string,
) (aapattern.Member, bool) {
	if p.exclude == "" {
		return aapattern.Member{}, false
	}
	index := p.excludeIndex
	if g, ok := p.scan.guests[p.exclude]; ok && index < 0 {
		index = guestIndex(g)
	}
	if index < 0 {
		return aapattern.Member{}, false
	}
	for name, r := range existing {
		if m, ok := parseHaMarker(r.Comment); ok && slices.Contains(m.Left, p.exclude) &&
			!slices.Contains(rules[name], p.exclude) {
			rules[name] = append(rules[name], p.exclude)
		}
	}
	return aapattern.Member{
		Key: p.exclude, Index: index, Domain: p.topo.domainOf(set, p.exclude, p.ident.Owner), Reserved: true,
	}, true
}

// member returns the layout's view of the member keyed sid, and false when
// the member drops out of the group's rules: a rule member whose guest is
// provably gone.
func (p *aaPlanner) member(set *ha.RuleSet, sid string, inRule bool) (aapattern.Member, bool) {
	_, tagged := p.scan.members[sid]
	self := sid == p.sid
	if !tagged && !self {
		if _, live := p.scan.live[sid]; !live && p.scan.liveComplete {
			if p.once("gone " + sid) {
				p.logger.Info("anti-affinity: dropping rule member with no live guest",
					log.String("rule", p.ident.ruleName()), log.String("sid", sid))
			}
			return aapattern.Member{}, false
		}
	}
	m := aapattern.Member{Key: sid, Index: -1, Domain: p.topo.domainOf(set, sid, p.ident.Owner)}
	if g, ok := p.scan.guests[sid]; ok {
		m.Index = guestIndex(g)
	}
	if self && p.index >= 0 {
		m.Index = p.index
	}
	marker := p.markerOf(sid)
	m.Born = marker.Born
	m.Joinable = p.joinable(sid, m, tagged || self, inRule)
	return m, true
}

// joinable reports whether the member sid may join a rule that does not hold
// it now. A member the scan does not tag keeps only the place it has, as does
// one that is held out or not an HA resource yet. A member with no index
// waits out aaUnindexedGrace from its birth.
func (p *aaPlanner) joinable(sid string, m aapattern.Member, tagged, inRule bool) bool {
	switch {
	case !tagged || p.holdOut[sid]:
		return false
	case sid != p.sid && p.leaving[sid]:
		if !inRule && p.once("leaving "+sid) {
			p.logger.Info("anti-affinity: member is leaving the group, because its delete took it out of its "+
				"rule ahead of the purge of its HA resource, so the plan does not put it back",
				log.String("sid", sid), log.String("rule", p.ident.ruleName()))
		}
		return false
	case (p.heal || sid != p.sid) && !p.isManaged(sid):
		if !inRule {
			p.logDeferred(sid)
		}
		return false
	case m.Index < 0 && m.Born > 0 && p.clock().Sub(time.Unix(m.Born, 0)) < aaUnindexedGrace:
		if !inRule && p.once("young "+sid) {
			p.logger.Info("anti-affinity: member has no instance index yet, so it joins no rule until its "+
				"set_vm_metadata names one or it is old enough to join after every indexed member",
				log.String("sid", sid), log.String("rule", p.ident.ruleName()))
		}
		return false
	}
	return true
}

// clock returns the planner's time now.
func (p *aaPlanner) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// markerOf returns the marker of sid's HA resource when this Director owns it,
// or a zero marker.
func (p *aaPlanner) markerOf(sid string) haMarker {
	comment, ok := p.managedNow[sid]
	if !ok || haOwnershipOf(comment, p.ident.Owner) != haOwnedByUs {
		return haMarker{}
	}
	m, _ := parseHaMarker(comment)
	return m
}

// aaPatternSource tracks the newest member whose create named a pattern, and
// whether the members that named one disagree.
type aaPatternSource struct {
	sid      string
	marker   haMarker
	patterns map[string]bool
}

// consider adds sid's marker to the tally.
func (a *aaPatternSource) consider(sid string, m haMarker) {
	if m.AA == "" {
		return
	}
	if a.patterns == nil {
		a.patterns = map[string]bool{}
	}
	a.patterns[m.AA] = true
	if a.sid == "" || m.Born > a.marker.Born || m.Born == a.marker.Born && sid > a.sid {
		a.sid, a.marker = sid, m
	}
}

// groupPattern returns the group's pattern: the newest member's, when a
// member's create named one, then the pattern recorded on the group's rules,
// and otherwise pve.ha.anti_affinity_pattern. When no member records a
// pattern, the plan records the one it uses on every rule of the group, so a
// group whose members a CPI that recorded no pattern created keeps its
// pattern across a change of the job default. A recorded pattern that no
// longer parses, and members that disagree, are logged once.
func (p *aaPlanner) groupPattern(src aaPatternSource, existing map[string]*ha.Rule) aapattern.Pattern {
	p.lastRecorded, p.stamp = false, ""
	if src.sid == "" {
		return p.rulePattern(existing)
	}
	if len(src.patterns) > 1 && p.once("disagree") {
		p.logger.Warn("anti-affinity: the group's members were created with different ha_anti_affinity patterns; "+
			"the plan uses the newest member's",
			log.String("rule", p.ident.ruleName()), log.String("pattern", src.marker.AA), log.String("from", src.sid),
			log.String("patterns", strings.Join(slices.Sorted(maps.Keys(src.patterns)), ",")))
	}
	pat, err := aapattern.Parse(src.marker.AA)
	if err != nil {
		if p.once("unparsed") {
			p.logger.Warn("anti-affinity: the pattern recorded on a member's HA resource does not parse; the plan "+
				"uses pve.ha.anti_affinity_pattern",
				log.String("rule", p.ident.ruleName()), log.String("from", src.sid), log.Err(err))
		}
		return p.pattern
	}
	p.lastRecorded = true
	return pat
}

// rulePattern returns the pattern recorded on the group's rules, reading the
// base rule first and then the others by name, or pve.ha.anti_affinity_pattern
// when no rule records one. It sets the stamp the plan records on the rules.
// A recorded pattern that does not parse, which a newer CPI may have written,
// is logged once and left in place, and the plan stamps nothing.
func (p *aaPlanner) rulePattern(existing map[string]*ha.Rule) aapattern.Pattern {
	base := p.ident.ruleName()
	names := slices.Sorted(maps.Keys(existing))
	if i := slices.Index(names, base); i > 0 {
		names = append([]string{base}, slices.Delete(names, i, i+1)...)
	}
	for _, name := range names {
		m, ok := parseHaMarker(existing[name].Comment)
		if !ok || m.AA == "" {
			continue
		}
		pat, err := aapattern.Parse(m.AA)
		if err != nil {
			if p.once("unparsed rule") {
				p.logger.Warn("anti-affinity: the pattern recorded on the group's rule does not parse; the plan "+
					"uses pve.ha.anti_affinity_pattern",
					log.String("rule", name), log.Err(err))
			}
			return p.pattern
		}
		p.lastRecorded, p.stamp = true, pat.Canonical()
		return pat
	}
	p.stamp = p.pattern.Canonical()
	return p.pattern
}

// canJoin reports whether key may join the rule named rule, which the layout
// has given members so far. The plan's own member joins unchecked outside a
// heal, unless checkSelf asks for the check, so a create whose own pin
// conflicts still fails or logs the refusal the way its mode says.
func (p *aaPlanner) canJoin(set *ha.RuleSet, cur *ha.Rule, rule string, members []string, key string) bool {
	if key == p.sid && !p.heal && !p.checkSelf {
		return true
	}
	bad := p.joinConflicts(set, cur, rule, members, key)
	if len(bad) == 0 {
		return true
	}
	if p.conflicts == nil {
		p.conflicts = map[string]map[string][]string{}
	}
	if p.conflicts[key] == nil {
		p.conflicts[key] = map[string][]string{}
	}
	maps.Copy(p.conflicts[key], bad)
	return false
}

// joinConflicts returns the feasibility conflicts that adding s to the rule
// named name, which the plan has given members so far, would leave in the
// rules, or nil when there are none. It checks the rules the way the writer's
// check before a send does, with the rule written as ruleOps writes it and
// with the members' pins flattened. Like that check, it counts every conflict
// on the rule itself, even one the rule had before the join, because the
// writer refuses to write a rule that conflicts at all. On every other rule it
// counts only a conflict that the rule set without s lacks. With the
// configured nodes unknown it checks nothing, as the writer does.
func (p *aaPlanner) joinConflicts(
	set *ha.RuleSet, cur *ha.Rule, name string, members []string, s string,
) map[string][]string {
	if len(p.clusterNodes) == 0 || len(members) == 0 || set == nil {
		return nil
	}
	with := append(slices.Clone(members), s)
	return ha.IntroducedConflicts(p.ruleCandidate(set, cur, name, members), p.ruleCandidate(set, cur, name, with),
		[]string{name}, p.clusterNodes)
}

// ruleCandidate returns set's rules with the rule name holding members, the
// way ruleOps writes it, and with the members' pins flattened. A rule left
// with fewer than two members is left out, because ruleOps deletes it.
func (p *aaPlanner) ruleCandidate(set *ha.RuleSet, cur *ha.Rule, name string, members []string) []ha.Rule {
	members = slices.Sorted(slices.Values(members))
	flat := map[string]ha.Rule{}
	ops := flattenMemberPins(set, members, p.ident.Owner, p.clusterNodes)
	for i := range ops {
		flat[ops[i].Rule.ID] = ops[i].Rule
	}
	out := make([]ha.Rule, 0, len(set.Rules)+1)
	for i := range set.Rules {
		r := set.Rules[i]
		if r.ID == name {
			continue
		}
		if f, ok := flat[r.ID]; ok {
			r = f
		}
		out = append(out, r)
	}
	if len(members) < 2 {
		return out
	}
	return append(out, p.wantRule(cur, name, members))
}

// wantRule returns the rule name holding members as ruleOps writes it: cur
// with its members replaced, or a new rule with the group's marker. Either
// records the plan's stamp, when it has one.
func (p *aaPlanner) wantRule(cur *ha.Rule, name string, members []string) ha.Rule {
	if cur == nil {
		return ha.Rule{ID: name, Type: haRuleType, Affinity: haRuleAffinity, Resources: members,
			Comment: withPatternStamp(p.ident.marker(), p.stamp)}
	}
	next := cur.Clone()
	next.Resources, next.Affinity, next.Errors = members, haRuleAffinity, nil
	if m, ok := parseHaMarker(cur.Comment); ok && len(m.Left) > 0 {
		// A record stays while its member is still an HA resource that the
		// rule leaves out, so the purge has yet to land. This VM's own join
		// clears its VMID's record, which an earlier VM left.
		left := slices.DeleteFunc(slices.Clone(m.Left), func(s string) bool {
			return s == p.sid || !p.isManaged(s) || slices.Contains(members, s)
		})
		next.Comment = withLeavingMembers(cur.Comment, left)
	}
	next.Comment = withPatternStamp(next.Comment, p.stamp)
	return next
}

// ruleOps returns the writes that make the group's rules hold the layout's
// plan, or nil when the rules and the pins already hold. The pin rewrites the
// members need lead, then the writes that take a member out of a rule, and
// then the ones that only add. The writer sends one write per read, so a
// member that moves leaves its old rule before it joins its new one, and no
// read between two writes shows it in both.
func (p *aaPlanner) ruleOps(set *ha.RuleSet, existing map[string]*ha.Rule, out aapattern.Output) []ha.RuleOp {
	var pins, shrink, grow []ha.RuleOp
	pinned := map[string]bool{}
	p.stampOnly = nil
	for _, name := range slices.Sorted(maps.Keys(out.Rules)) {
		members := out.Rules[name]
		if p.placedIn == "" && slices.Contains(members, p.sid) {
			p.placedIn = name
		}
		flat := flattenMemberPins(set, members, p.ident.Owner, p.clusterNodes)
		for i := range flat {
			if !pinned[flat[i].Rule.ID] {
				pinned[flat[i].Rule.ID] = true
				pins = append(pins, flat[i])
			}
		}
		cur := existing[name]
		switch {
		case cur == nil:
			grow = append(grow, ha.RuleOp{Kind: ha.OpCreate, Rule: p.wantRule(nil, name, members)})
		case !slices.Equal(cur.Resources, members) || cur.Affinity != haRuleAffinity:
			op := ha.RuleOp{Kind: ha.OpUpdate, Rule: p.wantRule(cur, name, members)}
			if subset(cur.Resources, members) {
				grow = append(grow, op)
			} else {
				shrink = append(shrink, op)
			}
		case p.stamp != "" && ruleStamp(cur) != p.stamp:
			if p.stampOnly == nil {
				p.stampOnly = map[string]bool{}
			}
			p.stampOnly[name] = true
			grow = append(grow, ha.RuleOp{Kind: ha.OpUpdate, Rule: p.wantRule(cur, name, members)})
		}
	}
	var deletes []ha.RuleOp
	for _, name := range slices.Sorted(maps.Keys(existing)) {
		if _, ok := out.Rules[name]; !ok {
			deletes = append(deletes, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: name}})
		}
	}
	total := len(pins) + len(deletes) + len(shrink) + len(grow)
	if total == 0 {
		return nil
	}
	ops := make([]ha.RuleOp, 0, total)
	ops = append(ops, pins...)
	ops = append(ops, deletes...)
	ops = append(ops, shrink...)
	return append(ops, grow...)
}

// ruleStamp returns the pattern recorded on a rule's comment, or "".
func ruleStamp(r *ha.Rule) string {
	m, _ := parseHaMarker(r.Comment)
	return m.AA
}

// subset reports whether every sid of have is in want.
func subset(have, want []string) bool {
	for _, s := range have {
		if !slices.Contains(want, s) {
			return false
		}
	}
	return true
}

// isManaged reports whether the last plan's read lists sid as an HA resource.
// Without a read every member counts as managed.
func (p *aaPlanner) isManaged(sid string) bool {
	if p.managedNow == nil {
		return true
	}
	_, ok := p.managedNow[sid]
	return ok
}

// logDeferred says once per sibling that it stays out of the rules until it
// is an HA resource.
func (p *aaPlanner) logDeferred(sid string) {
	if !p.once("deferred " + sid) {
		return
	}
	p.logger.Info("anti-affinity: sibling is not an HA resource yet, so a later plan of the group adds it to "+
		"the rules", log.String("sid", sid), log.String("rule", p.ident.ruleName()), log.String("joining", p.sid))
}

// logHeldOut says once per member, at Warn, that a pin repair took it out of
// this create's write and that the create plans the group's rules again
// without it. The member is then in no rule of the group, and under the
// rules lock no heal follows to tell the operator, so the line names the pin
// that PVE put out of effect.
func (p *aaPlanner) logHeldOut(siblings []string) {
	for _, sid := range siblings {
		if !p.once("conflicted " + sid) {
			continue
		}
		pin := ""
		if vmid, ok := vmidFromSid(sid); ok {
			pin = naRuleNameFor(vmid)
		}
		p.logger.Warn("anti-affinity: member left out of the group's rule, because PVE put its strict pin out "+
			"of effect when this create added it and a repair took it back out; a later write of the group adds "+
			"it once its pin goes away or allows more nodes",
			log.String("sid", sid), log.String("rule", p.ident.ruleName()), log.String("pin", pin),
			log.String("joining", p.sid))
	}
}

// logLayout says once per planner what the layout left out or changed, and
// why. Each line names the group's base rule and the member the plan is for.
func (p *aaPlanner) logLayout(in aapattern.Input, out aapattern.Output) {
	rule := log.String("rule", p.ident.ruleName())
	joining := log.String("joining", p.sid)
	pattern := log.String("pattern", in.Pattern.OrDefault().Canonical())
	if out.Unsafe && p.once("unsafe") {
		p.logger.Warn("anti-affinity: the group's pattern sets drain_safe false, so a rolling drain can find no "+
			"node for a member of a full rule; pve-ops ha reports it", rule, pattern)
	}
	if len(out.Trimmed) > 0 && p.once("trimmed "+strings.Join(out.Trimmed, ",")) {
		p.logger.Warn("anti-affinity: the plan left out rules the pattern asks for, because they would fill their "+
			"domain's nodes and block a rolling drain", rule, pattern, log.String("trimmed", strings.Join(out.Trimmed, ",")))
	}
	if len(out.AddedFirst) > 0 && p.once("first "+strings.Join(out.AddedFirst, ",")) {
		p.logger.Info("anti-affinity: the plan added a rule that keeps the two lowest indexes apart, because "+
			"trimming the pattern separated them", rule, pattern, log.String("added", strings.Join(out.AddedFirst, ",")))
	}
	if len(out.Capped) > 0 && p.once("capped") {
		p.logger.Warn("anti-affinity: the group reached the most rules the CPI writes for one group, so the rules "+
			"of the highest indexes are left out", rule, log.Int("left_out", len(out.Capped)))
	}
	for _, dk := range out.Small {
		if in.Domains[dk].Nodes == 0 && p.once("small "+dk) {
			p.logger.Warn("anti-affinity: none of the domain's nodes is in the cluster, so the group gets no rule "+
				"there; check placement.az_map against the cluster's nodes", rule, log.String("domain", aaDomainLabel(dk)))
			continue
		}
		if p.once("small " + dk) {
			p.logger.Warn("anti-affinity: the domain has too few nodes for a rule that leaves a node free for a "+
				"rolling drain, so the group gets no rule there; the placement scorer still spreads it",
				rule, log.String("domain", aaDomainLabel(dk)))
		}
	}
	for _, dk := range out.LowFree {
		if p.once("lowfree " + dk) {
			p.logger.Warn("anti-affinity: the group's pattern lets the two lowest indexes share a node",
				rule, pattern, log.String("domain", aaDomainLabel(dk)))
		}
	}
	p.logMembers(out, rule, joining)
}

// aloneRule names the rule the layout put sid in alone, or falls back to the
// group's base name.
func aloneRule(out aapattern.Output, sid string, fallback log.Field) log.Field {
	if name, ok := out.AloneIn[sid]; ok {
		return log.String("rule", name)
	}
	return fallback
}

// logMembers says once per member why the plan left it out of the rules.
func (p *aaPlanner) logMembers(out aapattern.Output, rule, joining log.Field) {
	for _, sid := range out.Overflow {
		if p.once("overflow " + sid) {
			p.logger.Warn("anti-affinity: member left out of the group's rules, because no rule has room for it; "+
				"a rule fills to one member fewer than its domain's nodes, and to no more than its placeable nodes",
				log.String("sid", sid), rule, joining, log.Bool("split", p.split))
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(out.Refused)) {
		if !p.once("conflicted " + sid) {
			continue
		}
		p.logger.Warn("anti-affinity: member left out of the group's rule, because adding it would make the rules "+
			"conflict, which would refuse the write or put a strict pin out of effect; a later write of the group "+
			"adds it once the conflict is gone",
			log.String("sid", sid), log.String("rules", strings.Join(out.Refused[sid], ",")),
			log.String("conflicting", strings.Join(slices.Sorted(maps.Keys(p.conflicts[sid])), ",")), joining)
	}
	for _, sid := range out.Alone {
		if p.once("alone " + sid) {
			p.logger.Warn("anti-affinity: member is in none of the group's rules, because the plan put it alone in a "+
				"rule of its own and a negative rule needs two members; the next member of the group that the plan "+
				"puts in that rule writes it with both", log.String("sid", sid), aloneRule(out, sid, rule), joining)
		}
	}
	for _, sid := range out.Displaced {
		if p.once("displaced " + sid) {
			p.logger.Info("anti-affinity: member left its rule for a lower index and waits for a place it may "+
				"join", log.String("sid", sid), rule, joining)
		}
	}
	for _, sid := range out.Unindexed {
		if p.once("unindexed " + sid) {
			p.logger.Warn("anti-affinity: member has no instance index, so the group's pattern cannot place it",
				log.String("sid", sid), rule, joining)
		}
	}
}

// aaDomainLabel names a domain key in a log line.
func aaDomainLabel(dk string) string {
	if dk == "" {
		return "cluster"
	}
	return dk
}

// guestIndex reads a guest's BOSH instance index from its index--<n> tag, and
// otherwise from a name that ends in -<n>, which set_vm_metadata gives every
// VM it names. It returns -1 when neither holds one. The name is read only
// for an index of at most five digits, so a UUID's last group never reads as
// one.
func guestIndex(g pve.GuestRef) int {
	for _, t := range g.SplitTags() {
		if rest, ok := strings.CutPrefix(strings.ToLower(t), indexTagPrefix); ok {
			if n, ok := canonicalIndex(rest, 9); ok {
				return n
			}
		}
	}
	if i := strings.LastIndexByte(g.Name, '-'); i >= 0 {
		if n, ok := canonicalIndex(g.Name[i+1:], 5); ok {
			return n
		}
	}
	return -1
}

// canonicalIndex parses s as an index of at most digits digits written with no
// sign and no leading zero.
func canonicalIndex(s string, digits int) (int, bool) {
	if s == "" || len(s) > digits {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

// aaTopology resolves a member's placement domain and each domain's node
// counts. A member whose strict pin of this Director names nodes that one AZ
// of placement.az_map holds belongs to the smallest such AZ, and every other
// member belongs to the whole cluster, keyed "".
type aaTopology struct {
	// configured is the cluster's configured node set, or nil when unknown.
	configured []string
	// azs holds the AZs of placement.az_map, sorted by name.
	azs []aaAZ
	// placeable counts the nodes of a set that can take a VM now.
	placeable func(nodes []string) int
	// clusterPlaceable is the cluster's placeable node count.
	clusterPlaceable int
	// placeableKnown says the counts were read from a profile that lists the
	// cluster's nodes or carries a health report, so a count of 0 means that
	// no node can take a VM rather than that nobody looked.
	placeableKnown bool
}

// aaAZ is one AZ of placement.az_map.
type aaAZ struct {
	name  string
	nodes []string
}

// newAATopology returns the topology of the cluster profile describes under
// cfg's AZ map.
func newAATopology(cfg *config.CPIConfig, profile *ha.Profile) aaTopology {
	t := aaTopology{
		configured:       profileNodeNames(profile),
		clusterPlaceable: placeableNodeCount(profile),
		placeable:        func(nodes []string) int { return placeableAmong(profile, nodes) },
		placeableKnown:   profile != nil && (profile.Health != nil || len(profile.Nodes) > 0),
	}
	if cfg != nil && cfg.Placement != nil {
		for _, name := range slices.Sorted(maps.Keys(cfg.Placement.AZMap)) {
			t.azs = append(t.azs, aaAZ{name: name, nodes: cfg.Placement.AZMap[name]})
		}
	}
	return t
}

// placeableAmong counts the nodes of nodes that can take a VM now, the way
// placeableNodeCount counts the cluster's.
func placeableAmong(p *ha.Profile, nodes []string) int {
	if p == nil {
		return 0
	}
	n := 0
	if p.Health != nil {
		for i := range p.Health.Nodes {
			if p.Health.Nodes[i].Placeable && slices.Contains(nodes, p.Health.Nodes[i].Name) {
				n++
			}
		}
		return n
	}
	for _, name := range p.OnlineNodes() {
		if slices.Contains(nodes, name) {
			n++
		}
	}
	return n
}

// domainOf returns the domain of the member sid: the smallest AZ that holds
// every node of its strict pin, when this Director owns that pin, with ties
// going to the AZ whose name sorts first. Every other member is in the
// cluster's domain, "".
func (t aaTopology) domainOf(set *ha.RuleSet, sid, owner string) string {
	vmid, ok := vmidFromSid(sid)
	if !ok || set == nil || len(t.azs) == 0 {
		return ""
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin == nil || pin.Type != ha.RuleTypeNodeAffinity || !pin.Strict || pin.Disabled ||
		haOwnershipOf(pin.Comment, owner) != haOwnedByUs {
		return ""
	}
	nodes := pin.NodeNames()
	if len(nodes) == 0 {
		return ""
	}
	best := -1
	for i, az := range t.azs {
		if !subset(nodes, az.nodes) {
			continue
		}
		if best < 0 || len(az.nodes) < len(t.azs[best].nodes) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return t.azs[best].name
}

// domains returns the layout's view of each domain keyed in keys. N is the
// AZ's nodes that the cluster configures, or every node the AZ names when
// the configured nodes are unknown, and the cluster's configured node count
// for the cluster's domain, which is 0 when unknown.
func (t aaTopology) domains(keys map[string]bool) map[string]aapattern.Domain {
	out := make(map[string]aapattern.Domain, len(keys))
	for dk := range keys {
		if dk == "" {
			out[dk] = aapattern.Domain{
				Key: dk, Nodes: len(t.configured), NodesKnown: len(t.configured) > 0,
				Placeable: t.clusterPlaceable, PlaceableKnown: t.placeableKnown,
			}
			continue
		}
		i := slices.IndexFunc(t.azs, func(az aaAZ) bool { return az.name == dk })
		if i < 0 {
			continue
		}
		nodes := t.azs[i].nodes
		if len(t.configured) > 0 {
			nodes = slices.DeleteFunc(slices.Clone(nodes), func(n string) bool {
				return !slices.Contains(t.configured, n)
			})
		}
		sort.Strings(nodes)
		out[dk] = aapattern.Domain{
			Key: dk, Ord: i + 1, Nodes: len(nodes), NodesKnown: len(t.configured) > 0,
			Placeable: t.placeable(nodes), PlaceableKnown: t.placeableKnown,
		}
	}
	return out
}
