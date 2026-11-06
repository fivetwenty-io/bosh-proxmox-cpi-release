package ha

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNoAdmissibleNode means no configured node satisfies a pin's hard
// constraints, so the VM has nowhere it may run and must not be left without a
// pin.
var ErrNoAdmissibleNode = errors.New("no configured node satisfies the hard constraints")

// AdmissibleHard returns the nodes of hard that the configured node set holds,
// or every node of hard when the configured set is unknown. An empty answer
// returns ErrNoAdmissibleNode.
func AdmissibleHard(hard, configured []string) ([]string, error) {
	known := toSet(trimmed(configured))
	var nodes []string
	for _, n := range trimmed(hard) {
		if len(known) == 0 || known[n] {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%w %s", ErrNoAdmissibleNode, strings.Join(trimmed(hard), ","))
	}
	return nodes, nil
}

// Pin priorities. PVE runs a resource in the highest-priority group of nodes
// that are available, so the placed node comes first, the other nodes of a
// non-strict AZ next, and every other admissible node last.
const (
	PinPriorityPlaced    = 2
	PinPriorityPreferred = 1
	PinPriorityRest      = 0
)

// PinSpec is everything that decides one VM's merged node-affinity pin.
type PinSpec struct {
	// ID and Comment are the rule's name and its ownership marker, and SID is
	// the VM's HA service ID.
	ID      string
	Comment string
	SID     string

	// Hard is the hard admissible set, which is the intersection of every hard
	// constraint (a strict AZ, a PCI device, a local disk, a bridge). nil means
	// the VM has no hard constraint. A non-nil empty set is an error, because
	// no node satisfies the constraints.
	Hard []string

	// Configured is the cluster's configured node set. When it is unknown the
	// pin is strict whenever it has a hard set, which is the safer reading.
	Configured []string

	// Placed is the node the CPI placed the VM on, and Preferred holds the
	// nodes of a non-strict AZ. Neither ever removes a node from the pin.
	Placed    string
	Preferred []string

	// RebalanceOnStart says the cluster has ha-rebalance-on-start on, so a VM
	// with no constraint gets a non-strict pin that names only the placed
	// node. DLB says the VM is in the DLB AZ, which lets PVE choose, so it gets
	// neither that pin nor a placed priority.
	RebalanceOnStart bool
	DLB              bool

	// SinglePriority flattens every priority to one value. PVE refuses a pin
	// with several priorities for a resource that is also in a
	// resource-affinity rule.
	SinglePriority bool
}

// BuildPin returns the merged node-affinity rule for spec, or nil when the VM
// needs no pin. The rule names every node of the hard admissible set and is
// strict when that set is smaller than the configured node set. Soft
// constraints only add priorities.
func BuildPin(spec PinSpec) (*Rule, error) {
	configured := toSet(trimmed(spec.Configured))
	var nodes []string
	strict := false
	switch {
	case spec.Hard != nil:
		var err error
		if nodes, err = AdmissibleHard(spec.Hard, spec.Configured); err != nil {
			return nil, fmt.Errorf("pin %s: %w", spec.ID, err)
		}
		strict = len(configured) == 0 || len(nodes) < len(configured)
	case len(trimmed(spec.Preferred)) > 0:
		if len(configured) > 0 {
			nodes = sortedKeys(configured)
		} else {
			nodes = trimmed(append(append([]string{}, spec.Preferred...), spec.Placed))
		}
	case spec.RebalanceOnStart && !spec.DLB && strings.TrimSpace(spec.Placed) != "":
		nodes = []string{strings.TrimSpace(spec.Placed)}
	default:
		return nil, nil
	}

	preferred := toSet(trimmed(spec.Preferred))
	rule := &Rule{
		ID:        spec.ID,
		Type:      RuleTypeNodeAffinity,
		Affinity:  AffinityPositive,
		Strict:    strict,
		Comment:   spec.Comment,
		Resources: []string{spec.SID},
		Nodes:     make(map[string]int, len(nodes)),
	}
	for _, n := range nodes {
		p := PinPriorityRest
		switch {
		case spec.SinglePriority:
		case n == spec.Placed && !spec.DLB:
			p = PinPriorityPlaced
		case preferred[n]:
			p = PinPriorityPreferred
		}
		rule.Nodes[n] = p
	}
	if rule.MultiplePriorities() {
		return rule, nil
	}
	// A rule whose nodes share one priority says the same thing with none.
	for n := range rule.Nodes {
		rule.Nodes[n] = PinPriorityRest
	}
	return rule, nil
}

// ConfinementOnly returns the pin's confinement part, which names the same
// nodes, is strict, and carries no priorities. Its node set never grows, so the
// rewrite needs no barrier.
func ConfinementOnly(pin *Rule) *Rule {
	out := pin.Clone()
	out.Strict = true
	for n := range out.Nodes {
		out.Nodes[n] = PinPriorityRest
	}
	return &out
}

// Flattened returns the pin with one priority on every node, keeping its nodes
// and its strictness.
func Flattened(pin *Rule) *Rule {
	out := pin.Clone()
	for n := range out.Nodes {
		out.Nodes[n] = PinPriorityRest
	}
	return &out
}

// Narrows reports whether moving a VM from the pin old to the pin next shrinks
// the set of nodes it may run on. A VM with no pin may run anywhere, and a
// non-strict pin confines nothing, so each allows every node.
func Narrows(old, next *Rule) bool {
	nextSet, nextAll := allowedNodes(next)
	if nextAll {
		return false
	}
	oldSet, oldAll := allowedNodes(old)
	if oldAll {
		return true
	}
	for n := range oldSet {
		if !nextSet[n] {
			return true
		}
	}
	return false
}

// allowedNodes returns the nodes a pin confines its VM to, or all=true when it
// confines it to none.
func allowedNodes(pin *Rule) (set map[string]bool, all bool) {
	if pin == nil || !pin.Strict || pin.Disabled {
		return nil, true
	}
	return toSet(pin.NodeNames()), false
}

func trimmed(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
