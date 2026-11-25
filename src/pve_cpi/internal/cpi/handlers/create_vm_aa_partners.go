package handlers

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// antiAffinityPartnersForCreate returns the partner nodes create_vm charges
// for a create whose effective ha_anti_affinity pattern has the canonical
// form pattern. A create whose pattern is off joins no rule of its group, so
// it gets no partner charge, even while the group still records a hold from
// its rules before the switch, and it keeps only the group spread (see
// antiAffinityScorerGroup). Every other create reads its partners through
// antiAffinityPartnerNodes.
func antiAffinityPartnersForCreate(
	ctx context.Context, deps Deps, env map[string]any, cp createVMCloudProps, pattern string,
) map[string]int {
	if pattern == aapattern.Off().Canonical() || cp.haOff {
		return nil
	}
	return antiAffinityPartnerNodes(ctx, deps, env, cp)
}

// antiAffinityPartnerNodes returns, by node, how many present members of the
// VM's anti-affinity group the group's rules will keep apart from the VM once
// its index is known, so placement can keep the VM off their nodes from the
// start. Without it, a recreate can land next to a member its rule then holds,
// and PVE migrates one of the two as soon as set_vm_metadata adds the VM to
// the rule.
//
// The Director names the index only at set_vm_metadata, so the read infers it
// from the group's live holds, each of which a delete left for the recreate of
// its index. A roll at a max_in_flight above 1 leaves several, and the create
// cannot tell which index is its own, so the partners of every live hold
// count. The partners come from the same plan the rule writer makes (see
// aapattern.HeldPartners), so a create avoids exactly the members its join
// will put it with.
//
// It returns nil, which leaves placement as it was, when anti-affinity rules
// or scored placement are off, when the request names target_node, when the
// group has no live hold, or when a read fails, which it logs at Warn. The
// reads take no lock and write nothing. A create that only scales out pays one
// read of the rules and one of the HA resources.
func antiAffinityPartnerNodes(
	ctx context.Context, deps Deps, env map[string]any, cp createVMCloudProps,
) map[string]int {
	cfg := deps.Config
	if cfg == nil || deps.PVE == nil || strings.TrimSpace(cp.TargetNode) != "" || !cfg.PlacementEnabled() ||
		!antiAffinityJoinEnabled(cfg) {
		return nil
	}
	ident := haIdentityFor(deps, env)
	if !ident.valid() || ident.Owner == "" {
		return nil
	}
	logger := deps.Log(ctx)
	fail := func(what string, err error) map[string]int {
		logger.Warn("create_vm: placement: could not read the anti-affinity rule partners of the VM's index, "+
			"so placement counts only the group's members", log.String("read", what),
			log.String("rule", ident.ruleName()), log.Err(err))
		return nil
	}
	svc := deps.PVE.Cluster()
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return fail("rules", err)
	}
	managed, err := listHAResources(ctx, svc)
	if err != nil {
		return fail("HA resources", err)
	}
	if !groupRecordsHolds(ident, set, managed) {
		return nil
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return fail("cluster profile", err)
	}
	scan, err := collectGroupMemberSids(ctx, deps, ident)
	if err != nil {
		return fail("group members", err)
	}
	p := newAAPlanner(cfg, profile, ident, scan, log.NewNopLogger())
	p.managed = func() (map[string]string, error) { return managed, nil }
	_, in, err := p.prepare(set)
	if err != nil {
		return fail("rules", err)
	}
	partners := map[string]bool{}
	var indexes []int
	for _, h := range p.sortedHolds() {
		if h.state != aaHoldLive || h.key == "" {
			continue
		}
		indexes = append(indexes, h.Index)
		for _, k := range aapattern.HeldPartners(in, h.key) {
			partners[k] = true
		}
	}
	out := map[string]int{}
	for _, sid := range slices.Sorted(maps.Keys(partners)) {
		if g, ok := scan.guests[sid]; ok && g.Node != "" {
			out[g.Node]++
		}
	}
	if len(out) == 0 {
		return nil
	}
	logger.Info("create_vm: placement: avoiding the nodes of the anti-affinity rule partners of the indexes "+
		"this create may recreate", log.String("rule", ident.ruleName()), log.Any("indexes", indexes),
		log.Any("partner_nodes", out))
	return out
}

// groupRecordsHolds reports whether a rule of ident's group, or the HA
// resource of one of its members, records a hold, which is what a delete
// leaves for the recreate of its index.
func groupRecordsHolds(ident haIdentity, set *ha.RuleSet, managed map[string]string) bool {
	base := ident.ruleName()
	for i := range set.Rules {
		r := &set.Rules[i]
		if aapattern.IsGroupRule(base, r.ID) && haOwnershipOf(r.Comment, ident.Owner) == haOwnedByUs &&
			len(holdsOf(r.Comment)) > 0 {
			return true
		}
	}
	p := &aaPlanner{ident: ident, managedNow: managed}
	for sid := range managed {
		if m, ok := p.groupMarkerOf(sid); ok && len(m.Holds) > 0 {
			return true
		}
	}
	return false
}

// withPartnerCounts returns facts with each node's PartnerCount set from
// partners, or facts as they are when partners is empty.
func withPartnerCounts(
	facts []placement.NodeFacts, partners map[string]int, logger *log.Logger,
) []placement.NodeFacts {
	if len(partners) == 0 {
		return facts
	}
	out := slices.Clone(facts)
	for i := range out {
		if n := partners[out[i].Node]; n > 0 {
			out[i].PartnerCount = n
			logger.Debug("create_vm: placement: counting anti-affinity rule partners",
				log.String("node", out[i].Node), log.Int("partners", n))
		}
	}
	return out
}
