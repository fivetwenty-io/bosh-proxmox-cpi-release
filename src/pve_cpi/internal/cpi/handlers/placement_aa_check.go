package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// AntiAffinityCheckInput is what a check of the anti-affinity groups reads.
// The doctor fills it from its own reads, and FixAntiAffinityGroups from the
// cluster, so both judge a group the way a plan of the CPI would.
type AntiAffinityCheckInput struct {
	Config  *config.CPIConfig
	Profile *ha.Profile
	// Owner limits the check to the groups of one Director, or "" checks
	// every group whose rules or HA resources carry a CPI marker.
	Owner string
	// Guests lists every live guest, and GuestsComplete says the list
	// covered every node.
	Guests         []pve.GuestRef
	GuestsComplete bool
	Rules          *ha.RuleSet
	// Resources holds the comment of every HA resource, keyed by its sid.
	Resources map[string]string
	Now       time.Time
	// GroupPatterns holds a default pattern for some groups, keyed by the
	// group's base rule name, which a plan of that group uses in place of
	// Config's pve.ha.anti_affinity_pattern. The doctor sets it under
	// --cpi-config to the default of the entry that writes the group's rules.
	// A pattern a member's create named still wins over it.
	GroupPatterns map[string]aapattern.Pattern
}

// AntiAffinityGroupCheck is the check of one anti-affinity group.
type AntiAffinityGroupCheck struct {
	// Rule is the group's base rule name, bosh-aa-<hash12>, which is also
	// its member tag.
	Rule       string `json:"rule"`
	Owner      string `json:"owner"`
	Deployment string `json:"deployment"`
	Group      string `json:"group"`
	// Pattern is the canonical form of the pattern the plan used, and
	// PatternRecorded says a member's create named it through
	// ha_anti_affinity, so no default pattern played a part.
	Pattern         string `json:"pattern"`
	PatternRecorded bool   `json:"pattern_recorded,omitempty"`
	Members         int    `json:"members"`
	// Trimmed names the rules the pattern asked for that the drain-safety
	// check, or the fit check of a pattern that gave up drain safety, left
	// out.
	Trimmed []string `json:"trimmed,omitempty"`
	// BlocksDrain says the pattern gave up drain safety with drain_safe
	// false.
	BlocksDrain bool `json:"blocks_drain,omitempty"`
	// Full lists the group's rules that hold as many members as their
	// domain has nodes, so a drain of one node finds no free node for one of
	// them.
	Full []AntiAffinityFullRule `json:"full,omitempty"`
	// Drift describes each write the plan would send, in order, and is
	// empty when the rules hold the planned layout.
	Drift []string `json:"drift,omitempty"`
	// Layout holds the planned rules by name, each with its members.
	Layout map[string][]string `json:"layout,omitempty"`
	// Holds lists the places the group's deletes keep free for the
	// recreates of their indexes, which the plan treats as occupied, and
	// LapsedHolds the holds still recorded that keep no place any more,
	// because they expired or a member with their index took the place. The
	// next write of the group drops a lapsed hold, and so does pve-ops ha
	// --fix-anti-affinity --apply.
	Holds       []AntiAffinityHold `json:"holds,omitempty"`
	LapsedHolds []AntiAffinityHold `json:"lapsed_holds,omitempty"`
	// PinRestores lists the members whose pin still records the priorities
	// a join flattened while the member is in no resource-affinity rule. The
	// group's next write restores them, and so does pve-ops ha
	// --fix-anti-affinity --apply, unless a live hold defers it.
	PinRestores []AntiAffinityPinRestore `json:"pin_restores,omitempty"`
	// Problem says why the group could not be planned, or "".
	Problem string `json:"problem,omitempty"`
}

// AntiAffinityPinRestore is a member's pin whose recorded priorities the next
// write of its group gives back.
type AntiAffinityPinRestore struct {
	SID string `json:"sid"`
	Pin string `json:"pin"`
	// Action is "restore" when the recorded priorities go back, or "drop"
	// when the pin changed after the join flattened it, so the record goes
	// and the pin keeps its nodes and priorities.
	Action string `json:"action"`
	// Nodes is the pin's nodes after the write, in the form the API takes
	// them.
	Nodes string `json:"nodes"`
	// Reason says why a drop drops the record.
	Reason string `json:"reason,omitempty"`
	// Deferred says a live hold of the group defers the write until the
	// hold lapses or its recreate joins.
	Deferred bool `json:"deferred,omitempty"`
}

// describe returns what the write does, in the words of the fix's drift.
func (r AntiAffinityPinRestore) describe() string {
	if r.Action == ha.PinRestoreDrop.String() {
		return fmt.Sprintf("drop the priority record from %s, because %s", r.Pin, r.Reason)
	}
	return fmt.Sprintf("restore the priorities %s on %s", r.Nodes, r.Pin)
}

// AntiAffinityHold is a place a delete keeps free for the recreate of its
// index.
type AntiAffinityHold struct {
	Index int `json:"index"`
	// VMID is the deleted member's VMID, or 0 when the record names none.
	VMID int `json:"vmid,omitempty"`
	// Until is when the hold lapses.
	Until time.Time `json:"until"`
	// Consumed says a member with the hold's index holds a place or may
	// join one, so the hold keeps nothing.
	Consumed bool `json:"consumed,omitempty"`
	// Carriers names the rules and the HA resources that record the hold.
	Carriers []string `json:"carriers,omitempty"`
}

// AntiAffinityFullRule is a rule that holds as many members as its domain has
// nodes.
type AntiAffinityFullRule struct {
	Rule    string `json:"rule"`
	Members int    `json:"members"`
	Nodes   int    `json:"nodes"`
}

// CheckAntiAffinityGroups plans every anti-affinity group in the input the
// way a heal of the group would, and reports what the plan finds, without any
// write. A group is found through the rules and the HA resources that carry a
// CPI marker naming it, so a group from an older CPI with no marker is not
// checked here.
func CheckAntiAffinityGroups(in AntiAffinityCheckInput) []AntiAffinityGroupCheck {
	set := in.Rules
	if set == nil {
		set = &ha.RuleSet{}
	}
	idents := antiAffinityGroupsOf(in.Owner, set, in.Resources)
	out := make([]AntiAffinityGroupCheck, 0, len(idents))
	for _, ident := range idents {
		out = append(out, checkAntiAffinityGroup(in, set, ident))
	}
	return out
}

// antiAffinityGroupsOf returns the identities of the groups that the rules
// and HA resources name in a CPI marker, sorted by rule name. With owner set,
// only that Director's groups are returned. A rule counts only when its name
// is one of the group's names.
func antiAffinityGroupsOf(owner string, set *ha.RuleSet, resources map[string]string) []haIdentity {
	found := map[string]haIdentity{}
	add := func(comment string, ruleID string) {
		m, ok := parseHaMarker(comment)
		if !ok || m.Owner == "" || m.Group == "" || owner != "" && m.Owner != owner {
			return
		}
		ident := haIdentity{Owner: m.Owner, Deployment: m.Deployment, Group: m.Group}
		if ruleID != "" && !aapattern.IsGroupRule(ident.ruleName(), ruleID) {
			return
		}
		found[ident.ruleName()] = ident
	}
	for i := range set.Rules {
		if r := &set.Rules[i]; r.Type == haRuleType && strings.HasPrefix(r.ID, haRuleNamePrefix) {
			add(r.Comment, r.ID)
		}
	}
	for _, comment := range resources {
		add(comment, "")
	}
	out := make([]haIdentity, 0, len(found))
	for _, name := range slices.Sorted(maps.Keys(found)) {
		out = append(out, found[name])
	}
	return out
}

// checkAntiAffinityGroup plans one group and reports what the plan finds.
func checkAntiAffinityGroup(in AntiAffinityCheckInput, set *ha.RuleSet, ident haIdentity) AntiAffinityGroupCheck {
	c := AntiAffinityGroupCheck{
		Rule: ident.ruleName(), Owner: ident.Owner, Deployment: ident.Deployment, Group: ident.Group,
	}
	scan := scanGroupMembers(in.Guests, in.GuestsComplete, ident)
	p := newAAPlanner(in.Config, in.Profile, ident, scan, log.NewNopLogger())
	if pat, ok := in.GroupPatterns[ident.ruleName()]; ok {
		p.pattern = pat.Clone().OrDefault()
	}
	p.heal, p.keepStale, p.keepInFlight = true, true, true
	p.managed = func() (map[string]string, error) { return in.Resources, nil }
	if !in.Now.IsZero() {
		p.now = func() time.Time { return in.Now }
	}
	ops, err := p.plan(set)
	if err != nil {
		c.Problem = err.Error()
		return c
	}
	c.Pattern, c.PatternRecorded = p.lastIn.Pattern.Canonical(), p.lastRecorded
	for _, m := range p.lastIn.Members {
		if !m.Reserved {
			c.Members++
		}
	}
	c.Holds, c.LapsedHolds = p.holdReport(set)
	c.Trimmed = slices.Clone(p.lastOut.Trimmed)
	c.BlocksDrain = p.lastOut.Unsafe
	c.Layout = p.lastOut.Rules
	if reason := confinedLayoutRule(set, ident.Owner, p.lastIn, p.lastOut); reason != "" {
		c.Problem = reason
		return c
	}
	for i := range ops {
		switch id := ops[i].Rule.ID; {
		case p.stampOnly[id]:
			c.Drift = append(c.Drift, fmt.Sprintf("record the pattern %s on %s", p.stamp, id))
		case p.holdOnly[id]:
			c.Drift = append(c.Drift, fmt.Sprintf("record the holds %s on %s", formatAAHolds(holdsOf(ops[i].Rule.Comment)),
				id))
		default:
			c.Drift = append(c.Drift, describeRuleOp(&ops[i]))
		}
	}
	for _, e := range p.pendingHolds {
		c.Drift = append(c.Drift, fmt.Sprintf("record the holds %s on the HA resource %s", formatAAHolds(e.add),
			e.sid))
	}
	restores := p.pinRestores(set)
	for i := range restores {
		r := &restores[i]
		c.PinRestores = append(c.PinRestores, AntiAffinityPinRestore{
			SID: r.SID, Pin: r.Pin, Action: r.Action.String(), Nodes: r.Nodes, Reason: r.Reason, Deferred: r.Deferred,
		})
	}
	c.Full = fullGroupRules(set, ident, p.lastIn)
	return c
}

// confinedLayoutRule returns why a plan's layout cannot be judged under the
// config it was planned with, or "". That is so when a rule of the cluster's
// domain names only members whose strict pins of owner leave them, between
// them, no more nodes than the rule has members. No AZ of placement.az_map
// holds those pins, or the members would be in that AZ's domain, so the plan
// counts the cluster's nodes where the members may run on fewer, and the rule
// would leave no node free while one of them drains. Entries of a cpi-config
// that share a cluster can map AZs differently, and such a group was most
// likely placed by an entry whose az_map holds the pins, which plans it over
// that AZ's nodes. The check reports the group as not planned, so neither the
// doctor's drift nor the fix acts on a layout that entry leaves out on purpose.
func confinedLayoutRule(set *ha.RuleSet, owner string, in aapattern.Input, out aapattern.Output) string {
	domainOf := make(map[string]string, len(in.Members))
	for _, m := range in.Members {
		domainOf[m.Key] = m.Domain
	}
	for _, rule := range slices.Sorted(maps.Keys(out.Rules)) {
		sids := out.Rules[rule]
		if len(sids) < 2 {
			continue
		}
		nodes := map[string]bool{}
		confined := true
		for _, sid := range sids {
			pinned := strictPinNodes(set, sid, owner)
			if d, ok := domainOf[sid]; !ok || d != "" || len(pinned) == 0 {
				confined = false
				break
			}
			for _, n := range pinned {
				nodes[n] = true
			}
		}
		if !confined || len(nodes) > len(sids) {
			continue
		}
		return fmt.Sprintf("the plan puts %s in rule %s, but their strict pins leave them only the nodes %s, so the "+
			"rule would leave no node free while one of them drains. No AZ of placement.az_map holds those nodes, so "+
			"the group was most likely placed by another cpis entry whose az_map does; check and fix the group with "+
			"that entry's request or under --cpi-config", strings.Join(sids, ", "), rule,
			strings.Join(slices.Sorted(maps.Keys(nodes)), ", "))
	}
	return ""
}

// AntiAffinityPlacementFit reports how well an AZ map explains the strict
// pins of a group's members, the way a plan resolves each member's domain.
// misses counts the members whose strict pin of owner names nodes that no AZ
// of azMap holds, and width adds up the nodes of the smallest AZ that holds
// each other pinned member's pin. A member with no such pin counts in
// neither. The doctor reads it under --cpi-config to find the entry whose
// placement a group belongs to.
func AntiAffinityPlacementFit(azMap map[string][]string, set *ha.RuleSet, owner string, sids []string,
) (misses, width int) {
	topo := newAATopology(&config.CPIConfig{Placement: &config.PlacementConfig{AZMap: azMap}}, nil)
	for _, sid := range sids {
		if len(strictPinNodes(set, sid, owner)) == 0 {
			continue
		}
		dk := topo.domainOf(set, sid, owner)
		if dk == "" {
			misses++
			continue
		}
		width += len(azMap[dk])
	}
	return misses, width
}

// fullGroupRules returns the group's rules that hold as many members as the
// domain of their members has configured nodes.
func fullGroupRules(set *ha.RuleSet, ident haIdentity, in aapattern.Input) []AntiAffinityFullRule {
	domainOf := make(map[string]string, len(in.Members))
	for _, m := range in.Members {
		domainOf[m.Key] = m.Domain
	}
	var out []AntiAffinityFullRule
	base := ident.ruleName()
	for i := range set.Rules {
		r := &set.Rules[i]
		if !aapattern.IsGroupRule(base, r.ID) || r.Type != haRuleType {
			continue
		}
		for _, s := range r.Resources {
			dk, ok := domainOf[s]
			if !ok {
				continue
			}
			if d := in.Domains[dk]; d.Nodes > 0 && len(r.Resources) >= d.Nodes {
				out = append(out, AntiAffinityFullRule{Rule: r.ID, Members: len(r.Resources), Nodes: d.Nodes})
			}
			break
		}
	}
	return out
}

// describeRuleOp renders a planned write for a person to read.
func describeRuleOp(op *ha.RuleOp) string {
	if op.Kind == ha.OpDelete {
		return fmt.Sprintf("delete %s", op.Rule.ID)
	}
	return fmt.Sprintf("%s %s with %s", op.Kind, op.Rule.ID, strings.Join(op.Rule.Resources, ","))
}

// AntiAffinityFixOptions tunes FixAntiAffinityGroups.
type AntiAffinityFixOptions struct {
	// DryRun reports what the fix would write and writes nothing.
	DryRun bool
	// Rule limits the fix to the group with this base rule name, or "" fixes
	// every group of the Director.
	Rule string
}

// Outcomes of the fix of one group.
const (
	AntiAffinityInLayout = "in_layout"
	AntiAffinityWouldFix = "would_fix"
	AntiAffinityFixed    = "fixed"
	AntiAffinityFailed   = "failed"
	AntiAffinityProblem  = "not_planned"
)

// AntiAffinityFixResult is the fix of one group.
type AntiAffinityFixResult struct {
	AntiAffinityGroupCheck
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// FixAntiAffinityGroups plans every anti-affinity group of the requesting
// Director again and, unless opts.DryRun is set, writes each group whose
// rules differ from the plan, still record a lapsed hold, or leave a member's
// pin with the priorities its join flattened, under the HA rules lock,
// through the same heal a delete runs. It refuses to run when the settings
// write no anti-affinity rule, or when the request names no Director.
func FixAntiAffinityGroups(ctx context.Context, deps Deps, opts AntiAffinityFixOptions) ([]AntiAffinityFixResult, error) {
	if deps.RequestDirectorUUID == "" {
		return nil, errors.New("anti-affinity fix: no Director UUID names the groups to fix")
	}
	if !antiAffinityJoinEnabled(deps.Config) {
		return nil, errors.New("anti-affinity fix: the settings write no anti-affinity rule, because pve.ha.mode " +
			"or pve.ha.anti_affinity is off, or scheduler-soft anti-affinity is disabled")
	}
	deps = withRequestHAProfile(deps)
	logger := deps.Log(ctx)
	in, err := readAntiAffinityCheckInput(ctx, deps)
	if err != nil {
		return nil, err
	}
	var out []AntiAffinityFixResult
	checks := CheckAntiAffinityGroups(in)
	for i := range checks {
		c := &checks[i]
		if opts.Rule != "" && c.Rule != opts.Rule {
			continue
		}
		r := AntiAffinityFixResult{AntiAffinityGroupCheck: *c}
		r.Drift = slices.Clone(c.Drift)
		for _, h := range c.LapsedHolds {
			r.Drift = append(r.Drift, fmt.Sprintf("drop the lapsed hold of index %d from %s", h.Index,
				strings.Join(h.Carriers, ",")))
		}
		for _, pr := range c.PinRestores {
			if !pr.Deferred {
				r.Drift = append(r.Drift, pr.describe())
			}
		}
		switch {
		case c.Problem != "":
			r.Outcome = AntiAffinityProblem
		case len(r.Drift) == 0:
			r.Outcome = AntiAffinityInLayout
		case opts.DryRun:
			r.Outcome = AntiAffinityWouldFix
		default:
			ident := haIdentity{Owner: c.Owner, Deployment: c.Deployment, Group: c.Group}
			err := withHARulesLock(ctx, deps, 0, logger, func(ctx context.Context) error {
				return healAntiAffinityGroup(ctx, deps, ident, "", "", nil, logger)
			})
			r.Outcome = AntiAffinityFixed
			if err != nil {
				r.Outcome, r.Error = AntiAffinityFailed, err.Error()
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// readAntiAffinityCheckInput reads what a check of the requesting Director's
// groups needs from the cluster.
func readAntiAffinityCheckInput(ctx context.Context, deps Deps) (AntiAffinityCheckInput, error) {
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return AntiAffinityCheckInput{}, fmt.Errorf("anti-affinity fix: %w", err)
	}
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return AntiAffinityCheckInput{}, fmt.Errorf("anti-affinity fix: read the HA rules: %w", err)
	}
	resources, err := listHAResources(ctx, deps.PVE.Cluster())
	if err != nil {
		return AntiAffinityCheckInput{}, fmt.Errorf("anti-affinity fix: read the HA resources: %w", err)
	}
	guests, excluded, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, nil)
	if err != nil {
		return AntiAffinityCheckInput{}, fmt.Errorf("anti-affinity fix: list the guests: %w", err)
	}
	return AntiAffinityCheckInput{
		Config: deps.Config, Profile: profile, Owner: deps.RequestDirectorUUID,
		Guests: guests, GuestsComplete: len(excluded) == 0, Rules: set, Resources: resources, Now: time.Now(),
	}, nil
}
