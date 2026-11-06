package ha

import (
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// CheckFeasibility is the CPI's copy of PVE's rule feasibility checks
// (PVE::HA::Rules::check_feasibility in pve-ha-manager). It returns, for every
// rule the checks reject, the reasons, keyed by rule ID. clusterNodes is the
// configured node set, which PVE takes from manager status. Disabled rules are
// left out of every check, as PVE leaves them out.
//
// The checks are:
//
//   - a resource in more than one node-affinity rule
//   - a negative node-affinity rule that leaves no node, or that sets priorities
//   - a resource-affinity rule with fewer than two resources
//   - a negative resource-affinity rule with more resources than configured nodes
//   - a positive and a negative resource-affinity rule that share two or more
//     resources, after overlapping positive rules are merged
//   - a resource in a resource-affinity rule that is also in a node-affinity
//     rule with more than one priority
//   - a merged positive resource-affinity set paired with more than one
//     node-affinity rule
//   - a negative resource-affinity rule whose members' node-affinity rules allow
//     fewer distinct nodes than the number of members those rules hold
func CheckFeasibility(rules []Rule, clusterNodes []string) map[string][]string {
	errs := map[string][]string{}
	add := func(id, msg string) { errs[id] = append(errs[id], msg) }

	var nodeAff, resAff, positive, negative []*Rule
	for i := range rules {
		r := &rules[i]
		if r.Disabled {
			continue
		}
		switch r.Type {
		case RuleTypeNodeAffinity:
			nodeAff = append(nodeAff, r)
		case RuleTypeResourceAffinity:
			resAff = append(resAff, r)
			switch r.Affinity {
			case AffinityPositive:
				positive = append(positive, r)
			case AffinityNegative:
				negative = append(negative, r)
			}
		}
	}
	clusterSet := toSet(clusterNodes)
	groups := disjointResourceSets(positive)

	checkNodeAffinityRules(nodeAff, clusterSet, add)
	checkResourceAffinityRules(resAff, negative, groups, len(clusterNodes), add)
	checkMultiplePriorities(resAff, nodeAff, add)
	checkPositiveSetPins(groups, nodeAff, add)
	checkNegativeRulePins(negative, nodeAff, clusterSet, add)

	for id := range errs {
		sort.Strings(errs[id])
		errs[id] = dedupSorted(errs[id])
	}
	return errs
}

// feasibilityAdd records one reason against one rule.
type feasibilityAdd func(id, msg string)

// checkNodeAffinityRules runs the node-affinity checks: a resource in more than
// one node-affinity rule, and a negative rule that leaves no node or sets
// priorities.
func checkNodeAffinityRules(nodeAff []*Rule, clusterSet map[string]bool, add feasibilityAdd) {
	refs := map[string][]string{}
	for _, r := range nodeAff {
		for _, sid := range r.Resources {
			refs[sid] = append(refs[sid], r.ID)
		}
	}
	for _, ids := range refs {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			add(id, "resources: at least one resource is already used in another node affinity rule")
		}
	}
	for _, r := range nodeAff {
		if r.Affinity != AffinityNegative {
			continue
		}
		left := 0
		for n := range clusterSet {
			if _, ok := r.Nodes[n]; !ok {
				left++
			}
		}
		if left == 0 {
			add(r.ID, "nodes: negative node affinity rule must not specify all nodes")
		}
		for _, p := range r.Nodes {
			if p != 0 {
				add(r.ID, "nodes: negative node affinity rule must not specify node priorities")
				break
			}
		}
	}
}

// checkResourceAffinityRules runs the resource-affinity checks: fewer than two
// resources, a negative rule with more resources than configured nodes, and a
// positive and a negative rule that share two or more resources.
func checkResourceAffinityRules(resAff, negative []*Rule, groups []resourceGroup, nodes int, add feasibilityAdd) {
	for _, r := range resAff {
		if len(r.Resources) < 2 {
			add(r.ID, "resources: rule must specify at least two resources")
		}
	}
	for _, r := range negative {
		if len(r.Resources) > nodes {
			add(r.ID, "resources: rule defines more resources than available nodes")
		}
	}
	for _, g := range groups {
		for _, neg := range negative {
			if countCommon(g.resources, neg.Resources) < 2 {
				continue
			}
			add(neg.ID, "resources: rule shares two or more resources with a positive resource affinity rule")
			for _, id := range g.ids {
				add(id, "resources: rule shares two or more resources with a negative resource affinity rule")
			}
		}
	}
}

// checkMultiplePriorities rejects a resource-affinity rule whose resources are
// in a node-affinity rule with more than one priority, and those node-affinity
// rules.
func checkMultiplePriorities(resAff, nodeAff []*Rule, add feasibilityAdd) {
	for _, ra := range resAff {
		var paired []string
		conflict := false
		for _, na := range nodeAff {
			if countCommon(toSet(na.Resources), ra.Resources) == 0 {
				continue
			}
			paired = append(paired, na.ID)
			if na.MultiplePriorities() {
				conflict = true
			}
		}
		if !conflict {
			continue
		}
		add(ra.ID, "resources: resources are in node affinity rules with multiple priorities")
		for _, id := range paired {
			add(id, "resources: resources are in a resource affinity rule and cannot be in a node affinity rule "+
				"with multiple priorities")
		}
	}
}

// checkPositiveSetPins rejects a merged positive resource-affinity set whose
// resources are in more than one node-affinity rule.
func checkPositiveSetPins(groups []resourceGroup, nodeAff []*Rule, add feasibilityAdd) {
	for _, g := range groups {
		var paired []string
		for _, na := range nodeAff {
			if countCommon(g.resources, na.Resources) > 0 {
				paired = append(paired, na.ID)
			}
		}
		if len(paired) < 2 {
			continue
		}
		for _, id := range g.ids {
			add(id, "resources: resources are in multiple node affinity rules")
		}
		for _, id := range paired {
			add(id, "resources: at least one resource is in a positive resource affinity rule and there are other "+
				"resources in at least one other node affinity rule already")
		}
	}
}

// checkNegativeRulePins rejects a negative resource-affinity rule whose
// members' node-affinity rules allow fewer distinct nodes than the number of
// members those rules hold.
func checkNegativeRulePins(negative, nodeAff []*Rule, clusterSet map[string]bool, add feasibilityAdd) {
	for _, neg := range negative {
		allowed := map[string]bool{}
		located := map[string]bool{}
		var paired []string
		for _, na := range nodeAff {
			common := 0
			for _, sid := range na.Resources {
				if neg.HasResource(sid) {
					located[sid] = true
					common++
				}
			}
			if common == 0 {
				continue
			}
			paired = append(paired, na.ID)
			for n := range nodeAffinityAllowed(na, clusterSet) {
				allowed[n] = true
			}
		}
		if len(allowed) >= len(located) {
			continue
		}
		add(neg.ID, "resources: two or more resources are restricted to less nodes than available to the resources")
		for _, id := range paired {
			add(id, "resources: at least one resource is in a negative resource affinity rule and this rule would "+
				"restrict these to less nodes than available to the resources")
		}
	}
}

// nodeAffinityAllowed returns the nodes a node-affinity rule lets its resources
// run on: its nodes when positive, and the configured nodes it leaves out when
// negative.
func nodeAffinityAllowed(na *Rule, clusterSet map[string]bool) map[string]bool {
	out := map[string]bool{}
	if na.Affinity != AffinityNegative {
		for n := range na.Nodes {
			out[n] = true
		}
		return out
	}
	for n := range clusterSet {
		if _, ok := na.Nodes[n]; !ok {
			out[n] = true
		}
	}
	return out
}

// resourceGroup is one connected set of positive resource-affinity rules.
type resourceGroup struct {
	ids       []string
	resources map[string]bool
}

// disjointResourceSets merges positive rules whose resources overlap, the way
// PVE's find_disjoint_rules_resource_sets does.
func disjointResourceSets(rules []*Rule) []resourceGroup {
	var groups []resourceGroup
	for _, r := range rules {
		merged := resourceGroup{ids: []string{r.ID}, resources: toSet(r.Resources)}
		var rest []resourceGroup
		for _, g := range groups {
			if countCommon(g.resources, r.Resources) > 0 || overlaps(g.resources, merged.resources) {
				merged.ids = append(merged.ids, g.ids...)
				for k := range g.resources {
					merged.resources[k] = true
				}
				continue
			}
			rest = append(rest, g)
		}
		rest = append(rest, merged)
		groups = rest
	}
	return groups
}

func overlaps(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

func countCommon(set map[string]bool, list []string) int {
	n := 0
	for _, v := range list {
		if set[v] {
			n++
		}
	}
	return n
}

func toSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out
}

func dedupSorted(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i > 0 && in[i-1] == v {
			continue
		}
		out = append(out, v)
	}
	return out
}

// IntroducedConflicts returns the feasibility errors of candidate that a write
// would be responsible for. Those are every error on a rule in touched, and
// every error on any other rule that before did not have. A rule that was
// already infeasible before the write is left to the operator, because the
// write did not cause it.
func IntroducedConflicts(before, candidate []Rule, touched []string, clusterNodes []string) map[string][]string {
	was := CheckFeasibility(before, clusterNodes)
	now := CheckFeasibility(candidate, clusterNodes)
	touch := toSet(touched)
	out := map[string][]string{}
	for id, msgs := range now {
		if touch[id] {
			out[id] = msgs
			continue
		}
		if _, had := was[id]; !had {
			out[id] = msgs
		}
	}
	return out
}

// pveFeasibilityRefusals are fragments of the errors PVE's check_feasibility
// reports, which its rules API returns when it refuses a rule. They match the
// messages CheckFeasibility copies.
var pveFeasibilityRefusals = []string{
	"infeasible",
	"is already used in another node affinity rule",
	"negative node affinity rule must not specify",
	"rule must specify at least two resources",
	"rule defines more resources than available nodes",
	"shares two or more resources with a",
	"are in node affinity rules with multiple priorities",
	"cannot be in a node affinity rule with multiple priorities",
	"resources are in multiple node affinity rules",
	"there are other resources in at least one other node affinity rule",
	"restricted to less nodes than available",
	"would restrict these to less nodes than available",
}

// IsFeasibilityRefusal reports whether err is PVE's refusal of a rule that its
// feasibility checks reject. Any other answer, such as a missing privilege, a
// missing rule, or a bad parameter, is not.
func IsFeasibilityRefusal(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := pve.AnsweredStatus(err); !ok || status < 400 || status >= 500 {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, f := range pveFeasibilityRefusals {
		if strings.Contains(msg, f) {
			return true
		}
	}
	return false
}
