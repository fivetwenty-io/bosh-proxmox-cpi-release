package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// fakeRuleAPI is an in-memory /cluster/ha/rules. It renders the rules the way
// PVE lists them, refuses a stale digest, and can inject faults per write.
type fakeRuleAPI struct {
	rules  []Rule
	digest int
	lists  int
	writes []string

	// faults, keyed by the write's index, return an error. land applies the
	// write before the error is returned, which is a held write that landed.
	faults map[int]ruleFault
	// listErr fails the n-th list (1-based).
	listErr map[int]error
	// onList runs before the n-th list (1-based) renders, and bumpAfter says
	// whether another writer changes the file right after the n-th list.
	onList    func(n int, f *fakeRuleAPI)
	bumpAfter func(n int) bool
	// evaluate runs after every write that lands, like PVE's own checks that
	// disable or flag a rule.
	evaluate func(f *fakeRuleAPI)
}

type ruleFault struct {
	err  error
	land bool
}

func (f *fakeRuleAPI) find(id string) *Rule {
	for i := range f.rules {
		if f.rules[i].ID == id {
			return &f.rules[i]
		}
	}
	return nil
}

func (f *fakeRuleAPI) ListHaRules(_ context.Context, _ *sdkcluster.ListHaRulesParams) (*sdkcluster.ListHaRulesResponse,
	error) {
	f.lists++
	if f.onList != nil {
		f.onList(f.lists, f)
	}
	if err := f.listErr[f.lists]; err != nil {
		return nil, err
	}
	resp := make(sdkcluster.ListHaRulesResponse, 0, len(f.rules))
	for i := range f.rules {
		resp = append(resp, renderRule(&f.rules[i], fmt.Sprintf("d%d", f.digest)))
	}
	if f.bumpAfter != nil && f.bumpAfter(f.lists) {
		f.digest++
	}
	return &resp, nil
}

func renderRule(r *Rule, digest string) json.RawMessage {
	row := map[string]any{"rule": r.ID, "type": r.Type, "resources": strings.Join(r.Resources, ","), "digest": digest}
	if r.Affinity != "" {
		row["affinity"] = r.Affinity
	}
	if r.Comment != "" {
		row["comment"] = r.Comment
	}
	if r.Disabled {
		row["disable"] = 1
	}
	if r.Type == RuleTypeNodeAffinity {
		row["nodes"] = r.NodesParam()
		if r.Strict {
			row["strict"] = 1
		}
	}
	if len(r.Errors) > 0 {
		errs := map[string][]string{}
		for _, e := range r.Errors {
			prop, msg, _ := strings.Cut(e, ": ")
			errs[prop] = append(errs[prop], msg)
		}
		row["errors"] = errs
	}
	raw, _ := json.Marshal(row)
	return raw
}

func (f *fakeRuleAPI) write(kind, id string, apply func() error) error {
	idx := len(f.writes)
	f.writes = append(f.writes, kind+":"+id)
	fault, faulty := f.faults[idx]
	if faulty && !fault.land {
		return fault.err
	}
	if err := apply(); err != nil {
		return err
	}
	f.digest++
	if f.evaluate != nil {
		f.evaluate(f)
	}
	if faulty {
		return fault.err
	}
	return nil
}

func parseNodesParam(s *string) map[string]int {
	if s == nil {
		return nil
	}
	raw, _ := json.Marshal(*s)
	nodes, _ := parseWireNodes(raw)
	return nodes
}

func splitResources(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeRuleAPI) CreateHaRules(_ context.Context, p *sdkcluster.CreateHaRulesParams) error {
	return f.write("create", p.Rule, func() error {
		if f.find(p.Rule) != nil {
			return fmt.Errorf("rule '%s' already defined", p.Rule)
		}
		r := Rule{ID: p.Rule, Type: p.Type, Resources: splitResources(p.Resources)}
		if p.Comment != nil {
			r.Comment = *p.Comment
		}
		if p.Disable != nil {
			r.Disabled = *p.Disable
		}
		if p.Affinity != nil {
			r.Affinity = *p.Affinity
		}
		if p.Type == RuleTypeNodeAffinity {
			r.Nodes = parseNodesParam(p.Nodes)
			if p.Strict != nil {
				r.Strict = *p.Strict
			}
			if r.Affinity == "" {
				r.Affinity = AffinityPositive
			}
		}
		f.rules = append(f.rules, r)
		return nil
	})
}

func (f *fakeRuleAPI) UpdateHaRules(_ context.Context, id string, p *sdkcluster.UpdateHaRulesParams) error {
	if p.Digest != nil && *p.Digest != fmt.Sprintf("d%d", f.digest) {
		f.writes = append(f.writes, "conflict:"+id)
		return errors.New("detected modified configuration - file changed by other user")
	}
	return f.write("update", id, func() error {
		r := f.find(id)
		if r == nil {
			return fmt.Errorf("no such rule '%s'", id)
		}
		del := map[string]bool{}
		if p.Delete != nil {
			for _, d := range strings.Split(*p.Delete, ",") {
				del[d] = true
			}
		}
		r.Type = p.Type
		if p.Resources != nil {
			r.Resources = splitResources(*p.Resources)
		}
		switch {
		case p.Comment != nil:
			r.Comment = *p.Comment
		case del["comment"]:
			r.Comment = ""
		}
		switch {
		case p.Disable != nil:
			r.Disabled = *p.Disable
		case del["disable"]:
			r.Disabled = false
		}
		if p.Affinity != nil {
			r.Affinity = *p.Affinity
		}
		if p.Type == RuleTypeNodeAffinity {
			r.Nodes = parseNodesParam(p.Nodes)
			if p.Strict != nil {
				r.Strict = *p.Strict
			}
		}
		r.Errors = nil
		return nil
	})
}

func (f *fakeRuleAPI) DeleteHaRules(_ context.Context, id string) error {
	return f.write("delete", id, func() error {
		if f.find(id) == nil {
			return fmt.Errorf("no such rule '%s'", id)
		}
		f.rules = slices.DeleteFunc(f.rules, func(r Rule) bool { return r.ID == id })
		return nil
	})
}

// pveFlagsPriorityPins is PVE's check that flags a node-affinity rule with
// several priorities whose resources are in an enabled resource-affinity rule.
// PVE reports the error on every read and leaves the rule out of effect.
func pveFlagsPriorityPins(f *fakeRuleAPI) {
	for i := range f.rules {
		na := &f.rules[i]
		if na.Type != RuleTypeNodeAffinity {
			continue
		}
		na.Errors = nil
		if !na.MultiplePriorities() {
			continue
		}
		for j := range f.rules {
			ra := &f.rules[j]
			if ra.Type != RuleTypeResourceAffinity || ra.Disabled {
				continue
			}
			if slices.ContainsFunc(na.Resources, ra.HasResource) {
				na.Errors = []string{"resources: resources are in a resource affinity rule and cannot be in a " +
					"node affinity rule with multiple priorities"}
			}
		}
	}
}

func rowsOf(rows ...string) *sdkcluster.ListHaRulesResponse {
	resp := make(sdkcluster.ListHaRulesResponse, 0, len(rows))
	for _, r := range rows {
		resp = append(resp, json.RawMessage(r))
	}
	return &resp
}

func TestParseRuleListShapes(t *testing.T) {
	set, err := ParseRuleList(rowsOf(
		`{"rule":"na","type":"node-affinity","strict":"1","disable":true,"nodes":{"b":{"priority":"2"},"a":null},`+
			`"resources":["vm:2","vm:1","vm:1"],"comment":"c","digest":"abc"}`,
		`{"rule":"ra","type":"resource-affinity","affinity":"negative","resources":{"vm:3":{},"vm:4":{}},`+
			`"errors":{"resources":["too many"]},"digest":"abc"}`,
		`{"rule":"nb","type":"node-affinity","nodes":["a:1","b"],"resources":"vm:5, vm:6","strict":0,"disable":null}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if set.Digest != "abc" || len(set.Rules) != 3 {
		t.Fatalf("digest %q rules %d", set.Digest, len(set.Rules))
	}
	na := set.Find("na")
	if !na.Strict || !na.Disabled || na.Affinity != AffinityPositive || na.Nodes["b"] != 2 || na.Nodes["a"] != 0 ||
		!slices.Equal(na.Resources, []string{"vm:1", "vm:2"}) || na.Comment != "c" {
		t.Fatalf("na = %+v", na)
	}
	ra := set.Find("ra")
	if ra.Strict || !slices.Equal(ra.Resources, []string{"vm:3", "vm:4"}) ||
		!slices.Equal(ra.Errors, []string{"resources: too many"}) {
		t.Fatalf("ra = %+v", ra)
	}
	nb := set.Find("nb")
	if nb.Strict || nb.Disabled || nb.Nodes["a"] != 1 || !slices.Equal(nb.Resources, []string{"vm:5", "vm:6"}) {
		t.Fatalf("nb = %+v", nb)
	}
	if set.Find("missing") != nil {
		t.Fatal("found a rule that is not there")
	}
	if got, err := ParseRuleList(nil); err != nil || len(got.Rules) != 0 {
		t.Fatalf("nil list = %+v, %v", got, err)
	}
}

func TestParseRuleListRefusesBadRows(t *testing.T) {
	for name, row := range map[string]string{
		"not json":      `{`,
		"no name":       `{"type":"node-affinity"}`,
		"bad strict":    `{"rule":"x","strict":"maybe"}`,
		"bad disable":   `{"rule":"x","disable":7}`,
		"bad nodes":     `{"rule":"x","nodes":"a:high"}`,
		"nameless node": `{"rule":"x","nodes":":1"}`,
		"bad prio":      `{"rule":"x","nodes":{"a":true}}`,
		"bad resources": `{"rule":"x","resources":7}`,
	} {
		if _, err := ParseRuleList(rowsOf(row)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	set, err := ParseRuleList(rowsOf(`{"rule":"x","errors":"odd"}`, `{"rule":"y","errors":{"p":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Rules[0].Errors) != 1 || len(set.Rules[1].Errors) != 1 {
		t.Fatalf("an unreadable errors shape must still count as an error: %+v", set.Rules)
	}
}

func TestRuleDiffNamesEachField(t *testing.T) {
	base := Rule{ID: "p", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: true,
		Nodes: map[string]int{"a": 2, "b": 0}, Resources: []string{"vm:1"}, Comment: "c"}
	if d := RuleDiff(&base, &base); len(d) != 0 {
		t.Fatalf("same rule differs: %v", d)
	}
	other := base.Clone()
	other.Strict, other.Disabled, other.Comment = false, true, "d"
	other.Nodes["b"] = 1
	other.Resources = []string{"vm:2"}
	other.Affinity = AffinityNegative
	got := RuleDiff(&base, &other)
	want := []string{"disable", "resources", "comment", "affinity", "strict", "nodes"}
	if !slices.Equal(got, want) {
		t.Fatalf("diff = %v, want %v", got, want)
	}
	if d := RuleDiff(nil, &base); !slices.Equal(d, []string{"presence"}) {
		t.Fatalf("presence diff = %v", d)
	}
	if RuleDiff(nil, nil) != nil {
		t.Fatal("two absent rules differ")
	}
	ra := Rule{ID: "r", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative, Resources: []string{"vm:1", "vm:2"}}
	rb := ra.Clone()
	rb.Affinity, rb.Type = AffinityPositive, RuleTypeNodeAffinity
	if d := RuleDiff(&ra, &rb); !slices.Equal(d, []string{"type", "affinity"}) {
		t.Fatalf("resource-affinity diff = %v", d)
	}
	if base.NodesParam() != "a:2,b" || !base.MultiplePriorities() {
		t.Fatalf("nodes param %q", base.NodesParam())
	}
}

func naRule(id string, strict bool, nodes map[string]int, sids ...string) Rule {
	return Rule{ID: id, Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: strict, Nodes: nodes,
		Resources: sids}
}

func raRule(id, affinity string, sids ...string) Rule {
	return Rule{ID: id, Type: RuleTypeResourceAffinity, Affinity: affinity, Resources: sids}
}

func hasReason(errs map[string][]string, id, fragment string) bool {
	for _, m := range errs[id] {
		if strings.Contains(m, fragment) {
			return true
		}
	}
	return false
}

func TestCheckFeasibility(t *testing.T) {
	nodes := []string{"a", "b", "c"}
	cases := []struct {
		name     string
		rules    []Rule
		id, frag string
	}{
		{"resource in two pins", []Rule{naRule("p1", true, map[string]int{"a": 0}, "vm:1"),
			naRule("p2", true, map[string]int{"b": 0}, "vm:1")}, "p2", "already used in another node affinity"},
		{"negative pin on all nodes", []Rule{{ID: "n", Type: RuleTypeNodeAffinity, Affinity: AffinityNegative,
			Nodes: map[string]int{"a": 0, "b": 0, "c": 0}, Resources: []string{"vm:1"}}}, "n", "must not specify all"},
		{"negative pin with priorities", []Rule{{ID: "n", Type: RuleTypeNodeAffinity, Affinity: AffinityNegative,
			Nodes: map[string]int{"a": 1}, Resources: []string{"vm:1"}}}, "n", "node priorities"},
		{"one-member rule", []Rule{raRule("r", AffinityNegative, "vm:1")}, "r", "at least two resources"},
		{"oversized negative", []Rule{raRule("r", AffinityNegative, "vm:1", "vm:2", "vm:3", "vm:4")}, "r",
			"more resources than available nodes"},
		{"positive and negative share", []Rule{raRule("pos", AffinityPositive, "vm:1", "vm:2"),
			raRule("pos2", AffinityPositive, "vm:2", "vm:3"), raRule("neg", AffinityNegative, "vm:1", "vm:3")},
			"pos2", "shares two or more resources with a negative"},
		{"priorities with resource affinity", []Rule{raRule("r", AffinityNegative, "vm:1", "vm:2"),
			naRule("p", false, map[string]int{"a": 2, "b": 0}, "vm:1")}, "p", "multiple priorities"},
		{"positive set in two pins", []Rule{raRule("pos", AffinityPositive, "vm:1", "vm:2"),
			naRule("p1", true, map[string]int{"a": 0}, "vm:1"), naRule("p2", true, map[string]int{"b": 0}, "vm:2")},
			"pos", "multiple node affinity rules"},
		{"negative restricted to too few nodes", []Rule{raRule("neg", AffinityNegative, "vm:1", "vm:2"),
			naRule("p1", true, map[string]int{"a": 0}, "vm:1"), naRule("p2", true, map[string]int{"a": 0}, "vm:2")},
			"neg", "restricted to less nodes"},
	}
	for _, tc := range cases {
		if errs := CheckFeasibility(tc.rules, nodes); !hasReason(errs, tc.id, tc.frag) {
			t.Errorf("%s: %s lacks %q: %v", tc.name, tc.id, tc.frag, errs)
		}
	}

	// A negative pin leaves nodes to its members, and a disabled rule is out
	// of every check.
	ok := []Rule{raRule("neg", AffinityNegative, "vm:1", "vm:2"),
		{ID: "n1", Type: RuleTypeNodeAffinity, Affinity: AffinityNegative, Nodes: map[string]int{"a": 0},
			Resources: []string{"vm:1"}},
		naRule("p2", true, map[string]int{"c": 0}, "vm:2"),
		{ID: "off", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative, Disabled: true,
			Resources: []string{"vm:9"}}}
	if errs := CheckFeasibility(ok, nodes); len(errs) != 0 {
		t.Fatalf("feasible set flagged: %v", errs)
	}
}

func TestIntroducedConflictsBlamesOnlyTheWrite(t *testing.T) {
	nodes := []string{"a", "b"}
	before := []Rule{raRule("old", AffinityNegative, "vm:1", "vm:2", "vm:3")}
	candidate := append(slices.Clone(before), raRule("new", AffinityNegative, "vm:4"))
	got := IntroducedConflicts(before, candidate, []string{"new"}, nodes)
	if _, ok := got["old"]; ok {
		t.Fatalf("an already infeasible rule was blamed on the write: %v", got)
	}
	if _, ok := got["new"]; !ok {
		t.Fatalf("the written rule's conflict was missed: %v", got)
	}
	got = IntroducedConflicts(nil, []Rule{raRule("x", AffinityNegative, "vm:1")}, nil, nodes)
	if _, ok := got["x"]; !ok {
		t.Fatalf("a newly infeasible rule was missed: %v", got)
	}
}

func TestBuildPin(t *testing.T) {
	cfg := []string{"a", "b", "c"}
	pin, err := BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: []string{"b", "a"}, Configured: cfg, Placed: "a"})
	if err != nil || !pin.Strict || pin.NodesParam() != "a:2,b" {
		t.Fatalf("hard subset pin = %+v, %v", pin, err)
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: cfg, Configured: cfg})
	if pin.Strict {
		t.Fatal("a pin naming every configured node must not be strict")
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: []string{"a", "b", "c"}, Placed: "a"})
	if !pin.Strict {
		t.Fatal("a hard pin with unknown configured nodes must be strict")
	}
	if _, err := BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: []string{"z"}, Configured: cfg}); err == nil {
		t.Fatal("a hard set outside the configured nodes was accepted")
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Preferred: []string{"b"}, Configured: cfg, Placed: "a"})
	if pin.Strict || pin.NodesParam() != "a:2,b:1,c" {
		t.Fatalf("preferred pin = %s strict %v", pin.NodesParam(), pin.Strict)
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Preferred: []string{"b"}, Placed: "a"})
	if pin.NodesParam() != "a:2,b:1" {
		t.Fatalf("preferred pin without configured nodes = %s", pin.NodesParam())
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Preferred: []string{"b"}, Configured: cfg, Placed: "a",
		SinglePriority: true})
	if pin.MultiplePriorities() || pin.NodesParam() != "a,b,c" {
		t.Fatalf("flattened pin = %s", pin.NodesParam())
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", RebalanceOnStart: true, Placed: "b", Configured: cfg})
	if pin == nil || pin.Strict || pin.NodesParam() != "b" {
		t.Fatalf("rebalance pin = %+v", pin)
	}
	for name, spec := range map[string]PinSpec{
		"dlb":           {ID: "p", SID: "vm:1", RebalanceOnStart: true, DLB: true, Placed: "b"},
		"no constraint": {ID: "p", SID: "vm:1", Placed: "b"},
	} {
		if pin, err := BuildPin(spec); pin != nil || err != nil {
			t.Errorf("%s: pin %+v, %v", name, pin, err)
		}
	}
	pin, _ = BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: []string{"a", "b"}, Configured: cfg, Placed: "a", DLB: true})
	if pin.MultiplePriorities() {
		t.Fatal("a DLB VM got a placed priority")
	}
}

func TestPinTransforms(t *testing.T) {
	pin := naRule("p", false, map[string]int{"a": 2, "b": 1}, "vm:1")
	c := ConfinementOnly(&pin)
	if !c.Strict || c.MultiplePriorities() || pin.Strict || !pin.MultiplePriorities() {
		t.Fatalf("confinement = %+v, source changed = %+v", c, pin)
	}
	f := Flattened(&pin)
	if f.Strict || f.MultiplePriorities() || len(f.Nodes) != 2 {
		t.Fatalf("flattened = %+v", f)
	}

	ab := naRule("p", true, map[string]int{"a": 0, "b": 0}, "vm:1")
	a := naRule("p", true, map[string]int{"a": 0}, "vm:1")
	soft := naRule("p", false, map[string]int{"a": 0}, "vm:1")
	off := a.Clone()
	off.Disabled = true
	for _, tc := range []struct {
		name      string
		old, next *Rule
		want      bool
	}{
		{"none to strict", nil, &a, true},
		{"wider to narrower", &ab, &a, true},
		{"narrower to wider", &a, &ab, false},
		{"strict to none", &a, nil, false},
		{"soft to strict", &soft, &a, true},
		{"strict to soft", &a, &soft, false},
		{"disabled to strict", &off, &a, true},
		{"same", &a, &a, false},
	} {
		if got := Narrows(tc.old, tc.next); got != tc.want {
			t.Errorf("%s: Narrows = %v", tc.name, got)
		}
	}
}
