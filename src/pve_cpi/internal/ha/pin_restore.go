package ha

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// PinRestoreKey is the key of the comment token on a VM's pin that records the
// nodes and priorities the pin had before an anti-affinity join flattened it.
// PVE refuses a pin with several priorities for a VM in a resource-affinity
// rule, so the join gives the pin one priority, and nothing else records the
// VM's AZ preference. When the VM leaves its last such rule, the CPI writes the
// recorded priorities back and drops the token. The value is the pin's nodes
// in the form the API takes them, as in "bosh-prio=az2-0,az2-1,az2-2:2", where
// a node with no priority has priority 0. Node names hold no ',', ':', or '=',
// so the value reads back the one way. Older releases ignore the token.
const PinRestoreKey = "bosh-prio"

// MaxRuleCommentLength is the longest comment the CPI writes on an HA rule,
// the limit PVE's API sets for the comment of an HA resource.
const MaxRuleCommentLength = 4096

// WithPinRestoreRecord returns comment, the comment of the flattened pin of
// orig, with a record of orig's nodes and priorities. A comment that already
// carries a record keeps it, so a second flatten never replaces the
// priorities the VM had before its first. No record is added when orig names
// no node, when a node's name holds a character the record cannot carry, or
// when the record would make the comment longer than MaxRuleCommentLength.
func WithPinRestoreRecord(comment string, orig *Rule) string {
	if _, present := pinRestoreValue(comment); present || orig == nil || len(orig.Nodes) == 0 {
		return comment
	}
	for n := range orig.Nodes {
		if !recordableNode(n) {
			return comment
		}
	}
	next := withCommentToken(comment, PinRestoreKey, orig.NodesParam())
	if len(next) > MaxRuleCommentLength {
		return comment
	}
	return next
}

// WithoutPinRestoreRecord returns comment with its record token removed. The
// rest of the comment keeps its order.
func WithoutPinRestoreRecord(comment string) string {
	return withCommentToken(comment, PinRestoreKey, "")
}

// PinRestoreRecord returns the nodes and priorities the record on comment
// names. present says the comment carries a record token at all, and ok says
// that the token names at least one node, each once, with an integer priority.
func PinRestoreRecord(comment string) (nodes map[string]int, present, ok bool) {
	raw, present := pinRestoreValue(comment)
	if !present {
		return nil, false, false
	}
	nodes = map[string]int{}
	for entry := range strings.SplitSeq(raw, ",") {
		name, prio, hasPrio := strings.Cut(entry, ":")
		if !recordableNode(name) {
			return nil, true, false
		}
		if _, dup := nodes[name]; dup {
			return nil, true, false
		}
		p := 0
		if hasPrio {
			v, err := strconv.Atoi(prio)
			if err != nil {
				return nil, true, false
			}
			p = v
		}
		nodes[name] = p
	}
	return nodes, true, true
}

// CarryPinRestore keeps the record of cur, the pin as the writer read it, on
// next, the pin the writer builds afresh in its place. It keeps the record
// while next has one priority on every node, which is how a VM in a
// resource-affinity rule's pin looks. A next with several priorities carries
// the CPI's own preference for a VM in no such rule already, so the record is
// dropped, because writing it back later would undo that preference.
func CarryPinRestore(cur, next *Rule) {
	if next == nil {
		return
	}
	next.Comment = WithoutPinRestoreRecord(next.Comment)
	if cur == nil || next.MultiplePriorities() {
		return
	}
	if raw, ok := pinRestoreValue(cur.Comment); ok && raw != "" {
		next.Comment = withCommentToken(next.Comment, PinRestoreKey, raw)
	}
}

// PinRestoreAction is what the restore of a pin's priorities writes.
type PinRestoreAction int

const (
	// PinRestoreNone writes nothing.
	PinRestoreNone PinRestoreAction = iota
	// PinRestoreWrite writes the recorded priorities back and drops the
	// record.
	PinRestoreWrite
	// PinRestoreDrop drops the record and keeps the pin's nodes and
	// priorities as they are.
	PinRestoreDrop
)

// String names the action the way the doctor reports it.
func (a PinRestoreAction) String() string {
	switch a {
	case PinRestoreWrite:
		return "restore"
	case PinRestoreDrop:
		return "drop"
	}
	return "none"
}

// PlanPinRestore decides what the restore writes for cur, a pin of this
// Director's whose VM is in no resource-affinity rule. configured is the
// cluster's configured node set, or nil when it is unknown. It returns the
// pin to write, which is a copy, and why a drop drops the record.
//
// The record goes back only while the pin still looks the way a flatten
// leaves it, which means one priority on every node. A strict pin keeps its
// nodes, because they confine the VM and a CPI writer such as a narrow may
// have changed them since, and each node gets the priority the record gives
// it, or 0. A pin that is not strict gets the recorded nodes back too, since
// the flatten added every configured node to it, as long as it still names
// every recorded node and every other node it names is a configured one.
//
// Anything else means someone changed the pin after the flatten, and their
// pin wins. That covers a pin with several priorities again, a pin that is not
// strict and lost a recorded node or names a node outside the cluster, and a
// record that does not parse. The restore then drops the record and keeps
// the pin's nodes and priorities. With the configured nodes unknown, a pin
// that is not strict and names nodes beyond the record cannot be judged, so
// nothing is written until a plan that knows them.
func PlanPinRestore(cur *Rule, configured []string) (*Rule, PinRestoreAction, string) {
	if cur == nil {
		return nil, PinRestoreNone, ""
	}
	rec, present, ok := PinRestoreRecord(cur.Comment)
	switch {
	case !present:
		return nil, PinRestoreNone, ""
	case !ok:
		return withoutRecord(cur), PinRestoreDrop, "the record does not name each node once with an integer priority"
	case cur.MultiplePriorities():
		return withoutRecord(cur), PinRestoreDrop, "the pin has several priorities again"
	}
	next := withoutRecord(cur)
	if cur.Strict {
		next.Nodes = make(map[string]int, len(cur.Nodes))
		for n := range cur.Nodes {
			next.Nodes[n] = rec[n]
		}
		return next, PinRestoreWrite, ""
	}
	for _, n := range slices.Sorted(maps.Keys(rec)) {
		if _, ok := cur.Nodes[n]; !ok {
			return next, PinRestoreDrop, fmt.Sprintf("the pin no longer names the recorded node %s", n)
		}
	}
	known := toSet(trimmed(configured))
	for _, n := range cur.NodeNames() {
		if _, ok := rec[n]; ok {
			continue
		}
		if len(known) == 0 {
			return nil, PinRestoreNone, "the configured nodes are unknown"
		}
		if !known[n] {
			return next, PinRestoreDrop, fmt.Sprintf("the pin names %s, which is not a configured node", n)
		}
	}
	next.Nodes = maps.Clone(rec)
	return next, PinRestoreWrite, ""
}

// withoutRecord returns a copy of pin without its record token and without
// the errors PVE reported on it.
func withoutRecord(pin *Rule) *Rule {
	out := pin.Clone()
	out.Comment = WithoutPinRestoreRecord(pin.Comment)
	out.Errors = nil
	return &out
}

// pinRestoreValue returns the value of the first record token on comment, and
// whether the comment carries one.
func pinRestoreValue(comment string) (string, bool) {
	for _, f := range strings.Fields(comment) {
		if k, v, ok := strings.Cut(f, "="); ok && k == PinRestoreKey {
			return v, true
		}
	}
	return "", false
}

// recordableNode reports whether a node name can stand in a record: it is not
// empty and holds none of the record's separators, no '=', no '%', which
// PVE's comment encoding writes, and no whitespace.
func recordableNode(n string) bool {
	return n != "" && !strings.ContainsAny(n, ",:=%") && !strings.ContainsFunc(n, unicode.IsSpace)
}
