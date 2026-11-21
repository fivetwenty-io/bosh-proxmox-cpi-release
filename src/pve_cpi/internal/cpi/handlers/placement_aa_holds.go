package handlers

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// aaHoldTTL is how long a hold keeps a deleted member's place free for the
// recreate of its index. BOSH recreates a VM with a delete followed by a
// create, so a hold normally lives for well under a minute, and one that a
// scale-in leaves lapses on its own.
const aaHoldTTL = 30 * time.Minute

// aaHold is the place a delete keeps free for the recreate of an index. The
// marker records it as hold=<index>.<vmid>@<unix-until>, with several holds
// separated by commas, on a rule of the group or, when the member's domain
// has no rule left, on the HA resource of a member of that domain. The VMID
// is the deleted member's, so a plan tells the deleted VM, whose HA resource
// can outlive its guest for a moment, from a recreate that got the same VMID
// back. The record holds no character PVE escapes in a comment.
type aaHold struct {
	Index int
	// VMID is the deleted member's VMID, or 0 for a hold recorded without
	// one.
	VMID int
	// Until is when the hold lapses, in Unix seconds.
	Until int64
}

// sid returns the HA resource ID of the deleted member, or "" without a
// VMID.
func (h aaHold) sid() string {
	if h.VMID <= 0 {
		return ""
	}
	return haResourceSid(h.VMID)
}

// made returns when the hold was written, in Unix seconds.
func (h aaHold) made() int64 {
	return h.Until - int64(aaHoldTTL/time.Second)
}

// String renders the hold as the marker records it.
func (h aaHold) String() string {
	s := strconv.Itoa(h.Index)
	if h.VMID > 0 {
		s += "." + strconv.Itoa(h.VMID)
	}
	return s + "@" + strconv.FormatInt(h.Until, 10)
}

// cmpAAHold orders holds by index, then VMID, then expiry.
func cmpAAHold(a, b aaHold) int {
	return cmp.Or(cmp.Compare(a.Index, b.Index), cmp.Compare(a.VMID, b.VMID), cmp.Compare(a.Until, b.Until))
}

// parseAAHolds reads the value of a hold= field. A part that does not parse
// is skipped, so a newer CPI's record never fails a plan.
func parseAAHolds(val string) []aaHold {
	var out []aaHold
	for _, part := range strings.Split(val, ",") {
		head, until, ok := strings.Cut(part, "@")
		if !ok {
			continue
		}
		idx, vmid, hasVMID := strings.Cut(head, ".")
		h := aaHold{}
		var good bool
		if h.Index, good = canonicalIndex(idx, 9); !good {
			continue
		}
		if hasVMID {
			n, err := strconv.Atoi(vmid)
			if err != nil || n <= 0 || strconv.Itoa(n) != vmid {
				continue
			}
			h.VMID = n
		}
		n, err := strconv.ParseInt(until, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != until {
			continue
		}
		h.Until = n
		out = append(out, h)
	}
	return out
}

// formatAAHolds renders holds as the value of a hold= field, in order and
// without repeats, or "" when there are none.
func formatAAHolds(holds []aaHold) string {
	sorted := slices.SortedFunc(slices.Values(holds), cmpAAHold)
	sorted = slices.Compact(sorted)
	parts := make([]string, 0, len(sorted))
	for _, h := range sorted {
		parts = append(parts, h.String())
	}
	return strings.Join(parts, ",")
}

// withHolds returns the comment with its holds replaced by holds, or with no
// hold= field when holds is empty. The field takes the place of the first one
// the comment had, and otherwise goes before the record of leaving members,
// or last when there is none. Every other field stays where it was, so a
// resource's comment keeps the tokens other writers own. A comment that holds
// no marker is returned as it is, and one PVE encoded again is written back
// plain.
func withHolds(comment string, holds []aaHold) string {
	fields, _, ok := ha.MarkerFields(comment)
	if !ok {
		return comment
	}
	val := formatAAHolds(holds)
	out := []string{haMarkerPrefix}
	placed := val == ""
	put := func() {
		if !placed {
			out = append(out, ha.AntiAffinityHoldKey+val)
			placed = true
		}
	}
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, ha.AntiAffinityHoldKey):
			put()
			continue
		case strings.HasPrefix(f, "left="):
			put()
		}
		out = append(out, f)
	}
	put()
	return strings.Join(out, " ")
}

// sameAAHolds reports whether a and b hold the same holds, in any order.
func sameAAHolds(a, b []aaHold) bool {
	return formatAAHolds(a) == formatAAHolds(b)
}

// aaHoldState says what a hold means to a plan.
type aaHoldState int

const (
	// aaHoldLive keeps its place free.
	aaHoldLive aaHoldState = iota
	// aaHoldConsumed has a member with its index that holds a place or may
	// join one, normally the recreate, so it keeps nothing.
	aaHoldConsumed
	// aaHoldLapsed is past its expiry and keeps nothing.
	aaHoldLapsed
)

// aaGroupHold is one hold as a plan of the group sees it.
type aaGroupHold struct {
	aaHold
	// Domain keys the domain of the deleted member, which is the domain of
	// the rule or the resource that records the hold.
	Domain string
	// recorded says a rule or a resource of the group records the hold, as
	// opposed to a hold the plan carries from an earlier round or from its
	// caller.
	recorded bool
	state    aaHoldState
	// key is the layout's key of the hold's reserved place.
	key string
}

// betterHold reports whether a should stand for its index over b: a live
// hold before a lapsed one, a recorded one before one only the plan carries,
// and then the later expiry and the lower VMID.
func betterHold(a, b aaGroupHold, now int64) bool {
	al, bl := a.Until > now, b.Until > now
	switch {
	case al != bl:
		return al
	case a.recorded != b.recorded:
		return a.recorded
	case a.Until != b.Until:
		return a.Until > b.Until
	}
	return a.VMID < b.VMID
}

// groupMarkerOf returns the marker of sid's HA resource when it marks a
// member of the planner's group, and false otherwise.
func (p *aaPlanner) groupMarkerOf(sid string) (haMarker, bool) {
	comment, ok := p.managedNow[sid]
	if !ok || haOwnershipOf(comment, p.ident.Owner) != haOwnedByUs {
		return haMarker{}, false
	}
	m, ok := parseHaMarker(comment)
	if !ok || m.Deployment != markerField(p.ident.Deployment) || m.Group != markerField(p.ident.Group) {
		return haMarker{}, false
	}
	return m, true
}

// markerField returns s as parseHaMarker reads it back from a marker.
func markerField(s string) string {
	if v := markerValue(s); v != haUnsetField {
		return v
	}
	return ""
}

// ruleDomain returns the domain of a rule's members, read from its first
// member, as every member of a group rule shares one domain.
func (p *aaPlanner) ruleDomain(set *ha.RuleSet, members []string) string {
	if len(members) == 0 {
		return ""
	}
	return p.topo.domainOf(set, members[0], p.ident.Owner)
}

// collectHolds reads every hold of the group, from the group's rules and the
// HA resources of its members, and adds the holds the planner carries: the
// one for the member a delete removes, when its index is known, and those of
// earlier rounds, whose record a write of this planner may have taken away
// with a rule it deleted. Each index keeps one hold, chosen by betterHold.
func (p *aaPlanner) collectHolds(set *ha.RuleSet, existing map[string]*ha.Rule) {
	now := p.clock().Unix()
	best := map[int]aaGroupHold{}
	add := func(h aaGroupHold) {
		if cur, ok := best[h.Index]; !ok || betterHold(h, cur, now) {
			best[h.Index] = h
		}
	}
	for _, name := range slices.Sorted(maps.Keys(existing)) {
		r := existing[name]
		m, ok := parseHaMarker(r.Comment)
		if !ok {
			continue
		}
		dom := p.ruleDomain(set, r.Resources)
		for _, h := range m.Holds {
			add(aaGroupHold{aaHold: h, Domain: dom, recorded: true})
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(p.managedNow)) {
		m, ok := p.groupMarkerOf(sid)
		if !ok || len(m.Holds) == 0 {
			continue
		}
		dom := p.topo.domainOf(set, sid, p.ident.Owner)
		for _, h := range m.Holds {
			add(aaGroupHold{aaHold: h, Domain: dom, recorded: true})
		}
	}
	for _, h := range p.holdMemo {
		h.recorded = false
		add(h)
	}
	if h, ok := p.excludeHold(set, now); ok {
		add(h)
	}
	p.holds = make(map[int]*aaGroupHold, len(best))
	if p.holdMemo == nil {
		p.holdMemo = map[int]aaGroupHold{}
	}
	for idx, h := range best {
		h.state = aaHoldLive
		if h.Until <= now {
			h.state = aaHoldLapsed
		}
		p.holds[idx] = &h
		p.holdMemo[idx] = h
	}
}

// excludeHold returns the hold of the member a delete removes, when its
// index is known from the caller or from its guest, which keeps its place
// free from now on.
func (p *aaPlanner) excludeHold(set *ha.RuleSet, now int64) (aaGroupHold, bool) {
	vmid, ok := vmidFromSid(p.exclude)
	if !ok {
		return aaGroupHold{}, false
	}
	index := p.excludeIndex
	if g, ok := p.scan.guests[p.exclude]; ok && index < 0 {
		index = guestIndex(g)
	}
	if index < 0 {
		return aaGroupHold{}, false
	}
	return aaGroupHold{
		aaHold: aaHold{Index: index, VMID: vmid, Until: now + int64(aaHoldTTL/time.Second)},
		Domain: p.topo.domainOf(set, p.exclude, p.ident.Owner),
	}, true
}

// sortedHolds returns the plan's holds by index.
func (p *aaPlanner) sortedHolds() []*aaGroupHold {
	out := make([]*aaGroupHold, 0, len(p.holds))
	for _, idx := range slices.Sorted(maps.Keys(p.holds)) {
		out = append(out, p.holds[idx])
	}
	return out
}

// dropDeletedHolders takes out of keys the deleted member of each live hold,
// whose HA resource or rule place can outlive its guest for a moment, and
// records each one it takes out in deletedHolders. A member with that VMID
// that was born after the hold was written is a new VM that got the VMID
// back, and it stays, as does the plan's own member.
func (p *aaPlanner) dropDeletedHolders(keys map[string]bool) {
	p.deletedHolders = map[string]bool{}
	for _, h := range p.sortedHolds() {
		sid := h.sid()
		if h.state != aaHoldLive || sid == "" || sid == p.sid || !keys[sid] {
			continue
		}
		if born := p.markerOf(sid).Born; born == 0 || born < h.made() {
			delete(keys, sid)
			p.deletedHolders[sid] = true
		}
	}
}

// placeHolds adds a reserved place to members for every live hold that no
// member has consumed, and returns the members. A member with the hold's
// index consumes it once it holds a place or may join a rule. One that may do
// neither yet, such as a recreate whose HA resource is not staged, is left
// out of the layout, which could not place it anyway, so it does not take its
// index's place from the hold. A rule whose left= record names the deleted
// member holds the reserved place in rules, so the plan keeps the place where
// the member was.
func (p *aaPlanner) placeHolds(
	members []aapattern.Member, held, keys map[string]bool, existing map[string]*ha.Rule,
	rules map[string][]string, domainKeys map[string]bool,
) []aapattern.Member {
	for _, h := range p.sortedHolds() {
		if h.state != aaHoldLive {
			continue
		}
		var waiting []string
		for _, m := range members {
			if m.Reserved || m.Index != h.Index {
				continue
			}
			if m.Joinable || held[m.Key] {
				h.state = aaHoldConsumed
				break
			}
			waiting = append(waiting, m.Key)
		}
		if h.state != aaHoldLive {
			continue
		}
		members = slices.DeleteFunc(members, func(m aapattern.Member) bool { return slices.Contains(waiting, m.Key) })
		h.key = h.sid()
		if h.key == "" || keys[h.key] {
			h.key = "hold/" + strconv.Itoa(h.Index)
		} else {
			for name, r := range existing {
				if mk, ok := parseHaMarker(r.Comment); ok && slices.Contains(mk.Left, h.key) &&
					!slices.Contains(rules[name], h.key) {
					rules[name] = append(rules[name], h.key)
				}
			}
		}
		members = append(members, aapattern.Member{Key: h.key, Index: h.Index, Domain: h.Domain, Reserved: true})
		domainKeys[h.Domain] = true
	}
	return members
}

// holdsOf returns the holds a comment records.
func holdsOf(comment string) []aaHold {
	m, _ := parseHaMarker(comment)
	return m.Holds
}

// recordsIndex reports whether holds has a hold of index.
func recordsIndex(holds []aaHold, index int) bool {
	return slices.ContainsFunc(holds, func(h aaHold) bool { return h.Index == index })
}

// aaRuleRank orders a group's rule names the way their numbers do: the block
// rules first, by number, then the pattern rules, by number, and any other
// name last.
func aaRuleRank(base, name string) (int, int) {
	if n, ok := aapattern.ChunkIndex(base, name); ok {
		return 0, n
	}
	if n, ok := aapattern.PatternRuleIndex(base, name); ok {
		return 1, n
	}
	return 2, 0
}

// sortedRuleNames returns names in aaRuleRank order, then by name.
func (p *aaPlanner) sortedRuleNames(names []string) []string {
	base := p.ident.ruleName()
	return slices.SortedFunc(slices.Values(names), func(a, b string) int {
		ak, an := aaRuleRank(base, a)
		bk, bn := aaRuleRank(base, b)
		return cmp.Or(cmp.Compare(ak, bk), cmp.Compare(an, bn), strings.Compare(a, b))
	})
}

// assignHolds decides where every live hold is recorded once the plan's
// rules are written. A hold stays on each rule of the plan in its domain, and
// each member resource of its domain, that records its index already.
// Otherwise it goes on the rule its deleted member leaves, when the plan
// keeps that rule, so the write that takes the member out records the hold
// and the recreate's join drops it again, with no write of its own for
// either. A hold whose member's rule goes goes on the lowest-numbered rule
// of the plan in its domain that exists now. When the domain has no such
// rule, it goes on the HA resource of the domain's lowest-indexed member,
// because the plan writes the deletes of its rules before the creates, so a
// rule it creates could record the hold only after the delete of the rule
// that records it now. Only a domain with neither puts the hold on a rule the
// plan creates. ruleHolds and resHolds then hold what each carrier should
// record, and a consumed or lapsed hold is recorded nowhere.
func (p *aaPlanner) assignHolds(in aapattern.Input, existing map[string]*ha.Rule, out aapattern.Output) {
	p.ruleHolds, p.resHolds = map[string][]aaHold{}, map[string][]aaHold{}
	domainOf := map[string]string{}
	var present []aapattern.Member
	for _, m := range in.Members {
		domainOf[m.Key] = m.Domain
		if !m.Reserved {
			present = append(present, m)
		}
	}
	ruleDom := map[string]string{}
	var names []string
	for name, keys := range out.Rules {
		for _, k := range keys {
			if d, ok := domainOf[k]; ok {
				ruleDom[name] = d
				names = append(names, name)
				break
			}
		}
	}
	names = p.sortedRuleNames(names)
	carriers := p.resourceCarriers(present)
	for _, h := range p.sortedHolds() {
		if h.state != aaHoldLive {
			continue
		}
		placed := false
		for _, name := range names {
			if ruleDom[name] == h.Domain && existing[name] != nil && recordsIndex(holdsOf(existing[name].Comment), h.Index) {
				p.ruleHolds[name] = append(p.ruleHolds[name], h.aaHold)
				placed = true
			}
		}
		for _, c := range carriers {
			if c.Domain == h.Domain && recordsIndex(holdsOf(p.managedNow[c.Key]), h.Index) {
				p.resHolds[c.Key] = append(p.resHolds[c.Key], h.aaHold)
				placed = true
			}
		}
		if placed {
			continue
		}
		if name, ok := p.leftRule(h, names, ruleDom, existing); ok {
			p.ruleHolds[name] = append(p.ruleHolds[name], h.aaHold)
			continue
		}
		if i := slices.IndexFunc(names, func(n string) bool {
			return ruleDom[n] == h.Domain && existing[n] != nil
		}); i >= 0 {
			p.ruleHolds[names[i]] = append(p.ruleHolds[names[i]], h.aaHold)
			continue
		}
		if i := slices.IndexFunc(carriers, func(m aapattern.Member) bool { return m.Domain == h.Domain }); i >= 0 {
			p.resHolds[carriers[i].Key] = append(p.resHolds[carriers[i].Key], h.aaHold)
			continue
		}
		if i := slices.IndexFunc(names, func(n string) bool { return ruleDom[n] == h.Domain }); i >= 0 {
			p.ruleHolds[names[i]] = append(p.ruleHolds[names[i]], h.aaHold)
			continue
		}
		if p.once("unrecorded hold " + h.String()) {
			p.logger.Warn("anti-affinity: no rule or member of the group is left in the deleted member's domain to "+
				"record its hold, so only this plan keeps its place free",
				log.String("rule", p.ident.ruleName()), log.Int("index", h.Index), log.String("sid", h.sid()),
				log.String("domain", aaDomainLabel(h.Domain)))
		}
	}
}

// leftRule returns the lowest-numbered rule among names, in the hold's
// domain, that holds the hold's deleted member now or records it as leaving,
// and false when the plan keeps no such rule or the hold names no VMID.
func (p *aaPlanner) leftRule(
	h *aaGroupHold, names []string, ruleDom map[string]string, existing map[string]*ha.Rule,
) (string, bool) {
	sid := h.sid()
	if sid == "" {
		return "", false
	}
	for _, name := range names {
		r := existing[name]
		if ruleDom[name] != h.Domain || r == nil {
			continue
		}
		if r.HasResource(sid) {
			return name, true
		}
		if m, ok := parseHaMarker(r.Comment); ok && slices.Contains(m.Left, sid) {
			return name, true
		}
	}
	return "", false
}

// resourceCarriers returns the members whose HA resources can record a hold:
// present members that are HA resources marked as members of the group, and
// no hold's deleted member, ordered by index, with no index last, and then
// by key.
func (p *aaPlanner) resourceCarriers(present []aapattern.Member) []aapattern.Member {
	deleted := map[string]bool{}
	for _, h := range p.holds {
		deleted[h.sid()] = true
	}
	var out []aapattern.Member
	for _, m := range present {
		if _, ok := p.groupMarkerOf(m.Key); ok && !deleted[m.Key] {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b aapattern.Member) int {
		ai, bi := a.Index, b.Index
		if ai < 0 {
			ai = int(^uint(0) >> 1)
		}
		if bi < 0 {
			bi = int(^uint(0) >> 1)
		}
		return cmp.Or(cmp.Compare(ai, bi), strings.Compare(a.Key, b.Key))
	})
	return out
}

// liveIndexes returns the indexes of the plan's live holds.
func (p *aaPlanner) liveIndexes() map[int]bool {
	out := map[int]bool{}
	for idx, h := range p.holds {
		if h.state == aaHoldLive {
			out[idx] = true
		}
	}
	return out
}

// carriesLive reports whether a rule records a live hold of the plan.
func (p *aaPlanner) carriesLive(r *ha.Rule) bool {
	live := p.liveIndexes()
	return slices.ContainsFunc(holdsOf(r.Comment), func(h aaHold) bool { return live[h.Index] })
}

// holdChange returns the holds want adds to what a comment records, and
// whether the comment records a hold that want does not keep.
func holdChange(comment string, want []aaHold) (adds []aaHold, stale bool) {
	have := holdsOf(comment)
	for _, h := range want {
		if !slices.Contains(have, h) {
			adds = append(adds, h)
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			stale = true
		}
	}
	return adds, stale
}

// aaResourceHoldEdit is a change to the holds a member's HA resource
// records.
type aaResourceHoldEdit struct {
	sid  string
	add  []aaHold
	drop []aaHold
}

// apply returns comment with the edit's holds dropped and added, keeping any
// hold another writer recorded meanwhile.
func (e aaResourceHoldEdit) apply(comment string) string {
	holds := slices.DeleteFunc(holdsOf(comment), func(h aaHold) bool { return slices.Contains(e.drop, h) })
	for _, h := range e.add {
		if !slices.Contains(holds, h) {
			holds = append(holds, h)
		}
	}
	if sameAAHolds(holds, holdsOf(comment)) {
		return comment
	}
	return withHolds(comment, holds)
}

// resourceHoldEdits returns the changes that make each member resource of
// the group record what assignHolds gave it: the ones that add a hold, and
// the ones that only drop holds that no longer keep a place.
func (p *aaPlanner) resourceHoldEdits() (adds, cleanups []aaResourceHoldEdit) {
	sids := map[string]bool{}
	for sid := range p.resHolds {
		sids[sid] = true
	}
	for sid := range p.managedNow {
		if m, ok := p.groupMarkerOf(sid); ok && len(m.Holds) > 0 {
			sids[sid] = true
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(sids)) {
		want := p.resHolds[sid]
		have := holdsOf(p.managedNow[sid])
		e := aaResourceHoldEdit{sid: sid}
		for _, h := range want {
			if !slices.Contains(have, h) {
				e.add = append(e.add, h)
			}
		}
		for _, h := range have {
			if !slices.Contains(want, h) {
				e.drop = append(e.drop, h)
			}
		}
		switch {
		case len(e.add) > 0:
			adds = append(adds, e)
		case len(e.drop) > 0:
			cleanups = append(cleanups, e)
		}
	}
	return adds, cleanups
}

// writeResourceHolds sends edits through recordHolds. A planner without it,
// such as the doctor's, records each edit that adds a hold as pending. A
// write that fails is logged once at Warn and not tried again by this
// planner, because a hold that a resource fails to record only loses its
// place to the next plan once this one is done.
func (p *aaPlanner) writeResourceHolds(edits []aaResourceHoldEdit) {
	for _, e := range edits {
		if p.recordHolds == nil {
			if len(e.add) > 0 {
				p.pendingHolds = append(p.pendingHolds, e)
			}
			continue
		}
		if p.holdWriteFailed[e.sid] {
			continue
		}
		if err := p.recordHolds(e); err != nil {
			if p.holdWriteFailed == nil {
				p.holdWriteFailed = map[string]bool{}
			}
			p.holdWriteFailed[e.sid] = true
			p.logger.Warn("anti-affinity: the hold could not be recorded on the member's HA resource (non-fatal)",
				log.String("rule", p.ident.ruleName()), log.String("sid", e.sid),
				log.String("add", formatAAHolds(e.add)), log.String("drop", formatAAHolds(e.drop)), log.Err(err))
		}
	}
}

// holdReport returns the plan's live holds, and the recorded holds that keep
// no place any more, each with the rules and resources that record it.
func (p *aaPlanner) holdReport(set *ha.RuleSet) (live, lapsed []AntiAffinityHold) {
	carriers := map[aaHold][]string{}
	base := p.ident.ruleName()
	for i := range set.Rules {
		r := &set.Rules[i]
		if !aapattern.IsGroupRule(base, r.ID) || haOwnershipOf(r.Comment, p.ident.Owner) != haOwnedByUs {
			continue
		}
		for _, h := range holdsOf(r.Comment) {
			carriers[h] = append(carriers[h], r.ID)
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(p.managedNow)) {
		if m, ok := p.groupMarkerOf(sid); ok {
			for _, h := range m.Holds {
				carriers[h] = append(carriers[h], sid)
			}
		}
	}
	report := func(h aaHold, consumed bool) AntiAffinityHold {
		return AntiAffinityHold{Index: h.Index, VMID: h.VMID, Until: time.Unix(h.Until, 0).UTC(), Consumed: consumed,
			Carriers: carriers[h]}
	}
	for _, h := range p.sortedHolds() {
		if h.state == aaHoldLive {
			live = append(live, report(h.aaHold, false))
		}
	}
	for _, h := range slices.SortedFunc(maps.Keys(carriers), cmpAAHold) {
		if g := p.holds[h.Index]; g != nil && g.state == aaHoldLive && g.aaHold == h {
			continue
		}
		g := p.holds[h.Index]
		lapsed = append(lapsed, report(h, g != nil && g.state == aaHoldConsumed && h.Until > p.clock().Unix()))
	}
	return live, lapsed
}

// carryHolds keeps recorded the live holds of the group's rules in deleting,
// for a write that deletes those rules without a plan of the group, such as a
// prune or the repair of a confinement pin. Each such hold moves to the
// lowest-numbered rule of the group in its domain that the write keeps with
// two members or more, through the update carryHolds returns, which the
// caller sends before the delete. A hold whose domain keeps no such rule goes
// on the HA resource of a member of that domain, first a member of the rule
// that goes and then any member of the group, by sid. carryHolds writes that
// edit itself through recordHolds before it returns. A hold that has lapsed,
// or whose index a rule outside deleting or a member resource records
// already, needs neither. avoid names the resources that must not record a
// hold, such as that of the member a delete or a repair is taking out.
func (p *aaPlanner) carryHolds(set *ha.RuleSet, deleting map[string]bool, avoid ...string) []ha.RuleOp {
	now := p.clock().Unix()
	base := p.ident.ruleName()
	rules := map[string]*ha.Rule{}
	for i := range set.Rules {
		r := &set.Rules[i]
		if aapattern.IsGroupRule(base, r.ID) && r.Type == haRuleType &&
			haOwnershipOf(r.Comment, p.ident.Owner) == haOwnedByUs {
			rules[r.ID] = r
		}
	}
	type carried struct {
		aaGroupHold
		from *ha.Rule
	}
	var pending []carried
	for _, name := range p.sortedRuleNames(slices.Collect(maps.Keys(rules))) {
		r := rules[name]
		if !deleting[name] {
			continue
		}
		for _, h := range holdsOf(r.Comment) {
			if h.Until <= now || slices.ContainsFunc(pending, func(c carried) bool { return c.Index == h.Index }) {
				continue
			}
			dom := p.ruleDomain(set, r.Resources)
			if len(r.Resources) == 0 {
				dom = p.topo.domainOf(set, h.sid(), p.ident.Owner)
			}
			pending = append(pending, carried{aaGroupHold{aaHold: h, Domain: dom}, r})
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if p.managed != nil {
		managed, err := p.managed()
		if err != nil {
			p.logger.Warn("anti-affinity: the HA resources could not be listed to keep the holds of the rules a "+
				"write deletes (non-fatal)", log.String("rule", base), log.Err(err))
		}
		p.managedNow = managed
	}
	recorded := func(index int) bool {
		for name, r := range rules {
			if !deleting[name] && recordsIndex(holdsOf(r.Comment), index) {
				return true
			}
		}
		for sid := range p.managedNow {
			if m, ok := p.groupMarkerOf(sid); ok && recordsIndex(m.Holds, index) {
				return true
			}
		}
		return false
	}
	var keptNames []string
	for name, r := range rules {
		if !deleting[name] && len(r.Resources) >= 2 {
			keptNames = append(keptNames, name)
		}
	}
	keptNames = p.sortedRuleNames(keptNames)
	adds := map[string][]aaHold{}
	var edits []aaResourceHoldEdit
	for _, c := range pending {
		if recorded(c.Index) {
			continue
		}
		if i := slices.IndexFunc(keptNames, func(n string) bool {
			return p.ruleDomain(set, rules[n].Resources) == c.Domain
		}); i >= 0 {
			adds[keptNames[i]] = append(adds[keptNames[i]], c.aaHold)
			continue
		}
		if sid, ok := p.holdResource(set, c.Domain, c.from.Resources, append([]string{c.sid()}, avoid...)); ok {
			edits = append(edits, aaResourceHoldEdit{sid: sid, add: []aaHold{c.aaHold}})
			continue
		}
		if p.once("uncarried hold " + c.String()) {
			p.logger.Warn("anti-affinity: no rule or member of the group is left in the deleted member's domain to "+
				"take over the hold of a rule a write deletes, so the hold lapses with the rule",
				log.String("rule", c.from.ID), log.Int("index", c.Index), log.String("sid", c.sid()),
				log.String("domain", aaDomainLabel(c.Domain)))
		}
	}
	p.writeResourceHolds(edits)
	var ops []ha.RuleOp
	for _, name := range keptNames {
		if len(adds[name]) == 0 {
			continue
		}
		next := rules[name].Clone()
		next.Errors = nil
		next.Comment = withHolds(next.Comment, append(holdsOf(next.Comment), adds[name]...))
		ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
	}
	return ops
}

// holdResource returns the HA resource, in domain, that takes over a hold
// for carryHolds: a member resource of the group among first, then any
// member resource of the group by sid, and never one in avoid.
func (p *aaPlanner) holdResource(set *ha.RuleSet, domain string, first, avoid []string) (string, bool) {
	fits := func(sid string) bool {
		if slices.Contains(avoid, sid) {
			return false
		}
		_, ok := p.groupMarkerOf(sid)
		return ok && p.topo.domainOf(set, sid, p.ident.Owner) == domain
	}
	for _, sid := range slices.Sorted(slices.Values(first)) {
		if fits(sid) {
			return sid, true
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(p.managedNow)) {
		if fits(sid) {
			return sid, true
		}
	}
	return "", false
}

// carryRepairHolds returns the rule writer's CarryHolds for the repair of a
// confinement pin. When the repair deletes an anti-affinity rule of a group
// this Director owns, the rule's live holds move to another rule of the
// group or to a member's HA resource first (see carryHolds), and never to
// the resource of a VM the repair takes out of the rule.
func carryRepairHolds(
	deps Deps, logger *log.Logger,
) func(ctx context.Context, set *ha.RuleSet, gone *ha.Rule, leaving []string) []ha.RuleOp {
	owner := deps.RequestDirectorUUID
	return func(ctx context.Context, set *ha.RuleSet, gone *ha.Rule, leaving []string) []ha.RuleOp {
		m, ok := parseHaMarker(gone.Comment)
		if !ok || len(m.Holds) == 0 || haOwnershipOf(gone.Comment, owner) != haOwnedByUs {
			return nil
		}
		ident := haIdentity{Owner: owner, Deployment: m.Deployment, Group: m.Group}
		if !ident.valid() || !aapattern.IsGroupRule(ident.ruleName(), gone.ID) {
			return nil
		}
		p := newGroupPlanner(ctx, deps, nil, ident, &groupScan{}, logger)
		return p.carryHolds(set, map[string]bool{gone.ID: true}, leaving...)
	}
}
