package ha

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// wireNull is the JSON null a PVE field may hold in place of a value.
const wireNull = "null"

// The PVE 9 HA rule types and affinities the CPI reads and writes.
const (
	RuleTypeNodeAffinity     = "node-affinity"
	RuleTypeResourceAffinity = "resource-affinity"
	AffinityPositive         = "positive"
	AffinityNegative         = "negative"
)

// Rule is one HA rule from /cluster/ha/rules in a normalized form, so two reads
// of the same rule compare equal whatever shape PVE serialized them in.
type Rule struct {
	// ID is the rule's name.
	ID string
	// Type is node-affinity or resource-affinity. A type the CPI does not know
	// is kept as read, and the feasibility checks pass over it.
	Type string
	// Affinity is positive or negative. A node-affinity rule that names none
	// reads as positive, which is PVE's default.
	Affinity string
	// Strict is the node-affinity strict flag. A resource-affinity rule has
	// none, and PVE refuses one sent for it.
	Strict bool
	// Disabled is true when the rule carries disable.
	Disabled bool
	// Nodes maps each node a node-affinity rule names to its priority. A node
	// listed without a priority has priority 0, as PVE reads it.
	Nodes map[string]int
	// Resources holds the rule's service IDs, sorted and without duplicates.
	Resources []string
	// Comment is the rule's comment, which carries the CPI's ownership marker.
	Comment string
	// Errors holds the feasibility errors PVE reported for the rule, each as
	// "property: message", sorted. An empty list means PVE reported none.
	Errors []string
}

// RuleSet is one read of the whole rules list. Digest is the digest of the
// rules file, which every update must carry so that PVE refuses it when another
// writer changed the file since the read.
type RuleSet struct {
	Rules  []Rule
	Digest string
}

// Find returns the rule named id, or nil.
func (s *RuleSet) Find(id string) *Rule {
	if s == nil {
		return nil
	}
	for i := range s.Rules {
		if s.Rules[i].ID == id {
			return &s.Rules[i]
		}
	}
	return nil
}

// Clone returns a deep copy, so a candidate set can be edited without touching
// the read it came from.
func (s *RuleSet) Clone() *RuleSet {
	out := &RuleSet{Digest: s.Digest, Rules: make([]Rule, len(s.Rules))}
	for i := range s.Rules {
		out.Rules[i] = s.Rules[i].Clone()
	}
	return out
}

// Clone returns a deep copy of the rule.
func (r Rule) Clone() Rule {
	out := r
	out.Resources = slices.Clone(r.Resources)
	out.Errors = slices.Clone(r.Errors)
	if r.Nodes != nil {
		out.Nodes = make(map[string]int, len(r.Nodes))
		for n, p := range r.Nodes {
			out.Nodes[n] = p
		}
	}
	return out
}

// HasResource reports whether the rule names sid.
func (r *Rule) HasResource(sid string) bool {
	return r != nil && slices.Contains(r.Resources, sid)
}

// NodeNames returns the rule's nodes, sorted.
func (r *Rule) NodeNames() []string {
	out := make([]string, 0, len(r.Nodes))
	for n := range r.Nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// MultiplePriorities reports whether the rule's nodes carry more than one
// priority, which PVE refuses for a resource that is also in a resource-affinity
// rule.
func (r *Rule) MultiplePriorities() bool {
	first, seen := 0, false
	for _, p := range r.Nodes {
		if !seen {
			first, seen = p, true
			continue
		}
		if p != first {
			return true
		}
	}
	return false
}

// NodesParam renders the nodes the way the API takes them, "node:prio,node",
// sorted by node, leaving out a priority of 0.
func (r *Rule) NodesParam() string {
	parts := make([]string, 0, len(r.Nodes))
	for _, n := range r.NodeNames() {
		if p := r.Nodes[n]; p != 0 {
			parts = append(parts, n+":"+strconv.Itoa(p))
			continue
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, ",")
}

// ResourcesParam renders the resources as the API takes them.
func (r *Rule) ResourcesParam() string {
	return strings.Join(r.Resources, ",")
}

// ruleWire is one row of GET /cluster/ha/rules.
type ruleWire struct {
	Rule      string          `json:"rule"`
	Type      string          `json:"type"`
	Affinity  string          `json:"affinity"`
	Strict    json.RawMessage `json:"strict"`
	Disable   json.RawMessage `json:"disable"`
	Nodes     json.RawMessage `json:"nodes"`
	Resources json.RawMessage `json:"resources"`
	Comment   string          `json:"comment"`
	Digest    string          `json:"digest"`
	Errors    json.RawMessage `json:"errors"`
}

// ParseRuleList normalizes a rules list. A row that does not parse fails the
// whole read, because a writer that cannot see every rule cannot check what its
// write does to the rest of the set.
func ParseRuleList(resp *sdkcluster.ListHaRulesResponse) (*RuleSet, error) {
	set := &RuleSet{}
	if resp == nil {
		return set, nil
	}
	for i, raw := range *resp {
		var w ruleWire
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("HA rules list row %d does not parse: %w", i, err)
		}
		if strings.TrimSpace(w.Rule) == "" {
			return nil, fmt.Errorf("HA rules list row %d names no rule", i)
		}
		r, err := w.normalize()
		if err != nil {
			return nil, fmt.Errorf("HA rule %q does not parse: %w", w.Rule, err)
		}
		if set.Digest == "" {
			set.Digest = w.Digest
		}
		set.Rules = append(set.Rules, r)
	}
	return set, nil
}

func (w *ruleWire) normalize() (Rule, error) {
	r := Rule{ID: w.Rule, Type: w.Type, Affinity: w.Affinity, Comment: w.Comment}
	var err error
	if r.Strict, err = wireTruthy(w.Strict); err != nil {
		return Rule{}, fmt.Errorf("strict: %w", err)
	}
	if r.Disabled, err = wireTruthy(w.Disable); err != nil {
		return Rule{}, fmt.Errorf("disable: %w", err)
	}
	if r.Resources, err = parseWireResources(w.Resources); err != nil {
		return Rule{}, fmt.Errorf("resources: %w", err)
	}
	if r.Nodes, err = parseWireNodes(w.Nodes); err != nil {
		return Rule{}, fmt.Errorf("nodes: %w", err)
	}
	r.Errors = parseWireErrors(w.Errors)
	if r.Type == RuleTypeNodeAffinity && r.Affinity == "" {
		r.Affinity = AffinityPositive
	}
	return r, nil
}

// wireTruthy reads a PVE boolean, which arrives as 0 or 1, a string of either,
// or a JSON boolean. Absent and null read as false.
func wireTruthy(raw json.RawMessage) (bool, error) {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", wireNull, "0", `"0"`, "false", `""`:
		return false, nil
	case "1", `"1"`, "true":
		return true, nil
	}
	return false, fmt.Errorf("value %s is not a boolean", s)
}

// parseWireResources reads resources as a CSV string, an array, or an object
// keyed by service ID.
func parseWireResources(raw json.RawMessage) ([]string, error) {
	seen := map[string]bool{}
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			seen[v] = true
		}
	}
	if len(raw) > 0 && string(raw) != wireNull {
		var s string
		var arr []string
		var obj map[string]json.RawMessage
		switch {
		case json.Unmarshal(raw, &s) == nil:
			for _, p := range strings.Split(s, ",") {
				add(p)
			}
		case json.Unmarshal(raw, &arr) == nil:
			for _, p := range arr {
				add(p)
			}
		case json.Unmarshal(raw, &obj) == nil:
			for k := range obj {
				add(k)
			}
		default:
			return nil, fmt.Errorf("value %s has no known form", raw)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// parseWireNodes reads nodes as "node:prio,node", an array of such entries, or
// an object keyed by node whose values are a priority or {"priority": n}.
func parseWireNodes(raw json.RawMessage) (map[string]int, error) {
	if len(raw) == 0 || string(raw) == wireNull {
		return nil, nil
	}
	out := map[string]int{}
	addEntry := func(entry string) error {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil
		}
		name, prio, hasPrio := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("entry %q names no node", entry)
		}
		p := 0
		if hasPrio {
			v, err := strconv.Atoi(strings.TrimSpace(prio))
			if err != nil {
				return fmt.Errorf("entry %q has no integer priority", entry)
			}
			p = v
		}
		out[name] = p
		return nil
	}
	var s string
	var arr []string
	var obj map[string]json.RawMessage
	switch {
	case json.Unmarshal(raw, &s) == nil:
		for _, e := range strings.Split(s, ",") {
			if err := addEntry(e); err != nil {
				return nil, err
			}
		}
	case json.Unmarshal(raw, &arr) == nil:
		for _, e := range arr {
			if err := addEntry(e); err != nil {
				return nil, err
			}
		}
	case json.Unmarshal(raw, &obj) == nil:
		for name, v := range obj {
			p, err := parseWirePriority(v)
			if err != nil {
				return nil, fmt.Errorf("node %q: %w", name, err)
			}
			out[name] = p
		}
	default:
		return nil, fmt.Errorf("value %s has no known form", raw)
	}
	return out, nil
}

func parseWirePriority(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == wireNull || string(raw) == "{}" {
		return 0, nil
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		v, err := strconv.Atoi(n.String())
		return v, err
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return 0, nil
		}
		return strconv.Atoi(s)
	}
	var obj struct {
		Priority json.RawMessage `json:"priority"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return parseWirePriority(obj.Priority)
	}
	return 0, fmt.Errorf("priority %s has no known form", raw)
}

// parseWireErrors flattens PVE's {"property": ["message"]} errors. Any other
// non-empty shape still counts as an error, so a rule PVE flagged is never read
// as healthy.
func parseWireErrors(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == wireNull || s == "{}" || s == "[]" || s == `""` {
		return nil
	}
	var obj map[string][]string
	if json.Unmarshal(raw, &obj) != nil {
		return []string{"unreadable errors: " + s}
	}
	var out []string
	for prop, msgs := range obj {
		for _, m := range msgs {
			out = append(out, prop+": "+m)
		}
	}
	if len(out) == 0 && len(obj) > 0 {
		out = append(out, "unreadable errors: "+s)
	}
	sort.Strings(out)
	return out
}

// RuleDiff names every field in which got differs from want, compared the way
// the rule's type defines it. For a node-affinity rule that is its type, nodes
// and their priorities, strict, disable, resources, and comment. For a
// resource-affinity rule it is its type, affinity, disable, resources, and
// comment. An empty result means the rules match.
func RuleDiff(want, got *Rule) []string {
	switch {
	case want == nil && got == nil:
		return nil
	case want == nil || got == nil:
		return []string{"presence"}
	}
	var diff []string
	if want.Type != got.Type {
		diff = append(diff, "type")
	}
	if want.Disabled != got.Disabled {
		diff = append(diff, "disable")
	}
	if !slices.Equal(want.Resources, got.Resources) {
		diff = append(diff, "resources")
	}
	if want.Comment != got.Comment {
		diff = append(diff, "comment")
	}
	switch want.Type {
	case RuleTypeNodeAffinity:
		if want.Affinity != got.Affinity {
			diff = append(diff, "affinity")
		}
		if want.Strict != got.Strict {
			diff = append(diff, "strict")
		}
		if !sameNodes(want.Nodes, got.Nodes) {
			diff = append(diff, "nodes")
		}
	default:
		if want.Affinity != got.Affinity {
			diff = append(diff, "affinity")
		}
	}
	return diff
}

// SameRule reports whether got matches want in every field RuleDiff compares.
func SameRule(want, got *Rule) bool { return len(RuleDiff(want, got)) == 0 }

func sameNodes(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for n, p := range a {
		q, ok := b[n]
		if !ok || p != q {
			return false
		}
	}
	return true
}
