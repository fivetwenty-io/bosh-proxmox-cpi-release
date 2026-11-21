package handlers

import (
	"context"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// aaPinRestore is one write the restore of a member's pin would send, as the
// last plan of the group sees it. A join that flattens a member's pin records
// the priorities the pin had on its comment (see ha.PinRestoreKey), and once
// the member is in no resource-affinity rule the restore writes them back, or
// drops a record that an operator's edit of the pin has outdated.
type aaPinRestore struct {
	// SID is the member's HA resource.
	SID string
	// Pin is the name of the member's node-affinity rule.
	Pin string
	// Action is what the restore writes.
	Action ha.PinRestoreAction
	// Nodes is the pin's nodes after the restore, in the form the API takes
	// them.
	Nodes string
	// Reason says why a drop drops the record.
	Reason string
	// Deferred says the group has a live hold, so the restore waits until a
	// plan without one. The deleted member's recreate may put the member back
	// into a rule, and the CPI does not move the VM's priorities twice for it.
	Deferred bool
	// rule is the pin to write.
	rule ha.Rule
}

// pinRestores returns the restores of the members of the last plan whose pin
// records priorities from a flatten while the member is in no
// resource-affinity rule, sorted by pin. It reads the plan's layout, so it
// runs after plan on the same set. A member the plan places in a rule, or
// keeps a place for, would be flattened again by the plan's write, and a
// member that is not an HA resource yet may be a create that joins next, so
// neither is restored. A member in a disabled rule is not restored either,
// because PVE could not enable that rule again while the pin has several
// priorities.
func (p *aaPlanner) pinRestores(set *ha.RuleSet) []aaPinRestore {
	placed := map[string]bool{}
	for _, keys := range p.lastOut.Rules {
		for _, k := range keys {
			placed[k] = true
		}
	}
	for k := range p.lastOut.Refused {
		placed[k] = true
	}
	deferred := len(p.liveIndexes()) > 0
	var out []aaPinRestore
	seen := map[string]bool{}
	for _, m := range p.lastIn.Members {
		sid := m.Key
		if m.Reserved || seen[sid] || sid == p.exclude || placed[sid] || p.holdOut[sid] || !p.isManaged(sid) {
			continue
		}
		seen[sid] = true
		if inAnyResourceAffinityRule(set, sid) {
			continue
		}
		vmid, ok := vmidFromSid(sid)
		if !ok {
			continue
		}
		pin := set.Find(naRuleNameFor(vmid))
		if pin == nil || pin.Type != ha.RuleTypeNodeAffinity ||
			haOwnershipOf(pin.Comment, p.ident.Owner) != haOwnedByUs {
			continue
		}
		next, action, reason := ha.PlanPinRestore(pin, p.clusterNodes)
		if action == ha.PinRestoreNone {
			continue
		}
		out = append(out, aaPinRestore{
			SID: sid, Pin: pin.ID, Action: action, Nodes: next.NodesParam(), Reason: reason,
			Deferred: deferred, rule: *next,
		})
	}
	slices.SortFunc(out, func(a, b aaPinRestore) int { return strings.Compare(a.Pin, b.Pin) })
	return out
}

// restoreOps returns the writes of the last plan's restores that do not wait
// for a live hold.
func (p *aaPlanner) restoreOps(set *ha.RuleSet) []ha.RuleOp {
	var ops []ha.RuleOp
	restores := p.pinRestores(set)
	for i := range restores {
		if !restores[i].Deferred {
			ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: restores[i].rule})
		}
	}
	return ops
}

// inAnyResourceAffinityRule reports whether sid is in a resource-affinity
// rule, enabled or not.
func inAnyResourceAffinityRule(set *ha.RuleSet, sid string) bool {
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type == ha.RuleTypeResourceAffinity && r.HasResource(sid) {
			return true
		}
	}
	return false
}

// restoreGroupPins writes back the priorities of the pins of planner's group
// members that left their last resource-affinity rule, once planner's plan of
// the group's rules has been written. set is the rules as that write left
// them. The restore plans the group again on every round of its own write,
// under the caller's rules lock and the rules digest, so a member that a
// concurrent writer put back into a rule keeps its flat pin. It goes through
// its own write so that its failure never fails the call that planned the
// group. It logs a warning instead, and the group's next plan, or the
// doctor's fix, restores the pin.
func restoreGroupPins(
	ctx context.Context, deps Deps, profile *ha.Profile, planner *aaPlanner, set *ha.RuleSet, logger *log.Logger,
) {
	if planner == nil || set == nil || len(planner.restoreOps(set)) == 0 {
		return
	}
	// The restore plans on a copy, so the caller's planner keeps what its
	// own plan found, and the copy writes no holds to the HA resources.
	rp := *planner
	rp.recordHolds = nil
	planned := map[string]aaPinRestore{}
	writer := newHARuleWriterFor(deps, profile, logger, nil)
	res, err := writer.Apply(ctx, "anti-affinity pin restore", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
		if _, err := rp.plan(set); err != nil {
			return nil, err
		}
		restores := rp.pinRestores(set)
		for i := range restores {
			planned[restores[i].Pin] = restores[i]
		}
		return rp.restoreOps(set), nil
	})
	if res != nil {
		for i := range res.Wrote {
			r, ok := planned[res.Wrote[i].Rule.ID]
			if !ok {
				continue
			}
			if r.Action == ha.PinRestoreDrop {
				logger.Info("anti-affinity: dropped the priority record of a member's pin, which changed after the "+
					"join flattened it, so the pin keeps its nodes and priorities", log.String("pin", r.Pin),
					log.String("sid", r.SID), log.String("nodes", r.Nodes), log.String("reason", r.Reason))
				continue
			}
			logger.Info("anti-affinity: restored the priorities a join took from the pin of a member that is in "+
				"no anti-affinity rule now", log.String("pin", r.Pin), log.String("sid", r.SID),
				log.String("nodes", r.Nodes))
		}
	}
	if err != nil {
		logger.Warn("anti-affinity: could not restore the priorities of a member's pin, so the pin keeps one "+
			"priority on every node until the group's next plan or a fix of the group restores them",
			log.String("rule", planner.ident.ruleName()), log.Err(err))
	}
}

// restoreGroupPinsAfterPrune restores the pins of ident's group members once a
// prune deleted rules of the group, which can leave a member in no rule. The
// prune plans nothing of its own, so the restore scans the group and plans it
// as a heal without removed, the VM whose rollback ran the prune. It reads the
// cluster's nodes and scans the group only when a pin of this Director's
// records priorities for a VM in no resource-affinity rule.
func restoreGroupPinsAfterPrune(
	ctx context.Context, deps Deps, ident haIdentity, removed string, set *ha.RuleSet, logger *log.Logger,
) {
	if !pinRestorePending(set, ident.Owner) {
		return
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		logger.Warn("anti-affinity: could not read the cluster's nodes to restore the priorities of a member's pin",
			log.String("rule", ident.ruleName()), log.Err(err))
		return
	}
	scan, err := collectGroupMemberSids(ctx, deps, ident)
	if err != nil {
		logger.Warn("anti-affinity: could not scan the group's members to restore the priorities of a member's pin",
			log.String("rule", ident.ruleName()), log.Err(err))
		return
	}
	planner := newGroupPlanner(ctx, deps, profile, ident, scan, logger)
	planner.heal, planner.exclude, planner.recordHolds = true, removed, nil
	if _, err := planner.plan(set); err != nil {
		logger.Warn("anti-affinity: could not plan the group to restore the priorities of a member's pin",
			log.String("rule", ident.ruleName()), log.Err(err))
		return
	}
	restoreGroupPins(ctx, deps, profile, planner, set, logger)
}

// pinRestorePending reports whether set holds a pin of owner's that records
// priorities from a flatten while its VM is in no resource-affinity rule.
func pinRestorePending(set *ha.RuleSet, owner string) bool {
	if set == nil {
		return false
	}
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || !strings.HasPrefix(r.ID, naRuleNamePrefix) {
			continue
		}
		if _, present, _ := ha.PinRestoreRecord(r.Comment); !present ||
			haOwnershipOf(r.Comment, owner) != haOwnedByUs {
			continue
		}
		for _, sid := range r.Resources {
			if !inAnyResourceAffinityRule(set, sid) {
				return true
			}
		}
	}
	return false
}
