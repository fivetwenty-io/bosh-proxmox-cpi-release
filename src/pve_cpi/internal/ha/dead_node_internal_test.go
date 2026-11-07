package ha

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

var deadNodeMembers = []string{"a", "b", "c"}

// placeableExcept answers placeable for every node but the ones named.
func placeableExcept(down ...string) func(string) bool {
	return func(n string) bool { return !slices.Contains(down, n) }
}

// deadNodePin is this Director's pin of vm:101 naming nodes, strict when
// strict is true.
func deadNodePin(strict bool, nodes map[string]int) Rule {
	return Rule{ID: "bosh-na-101", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: strict,
		Nodes: nodes, Resources: []string{"vm:101"}, Comment: "owner=us bosh-vols=nfs"}
}

// deadNodeStorages indexes a shared storage every member reaches, one that
// reaches a and b alone, and a node-local one.
func deadNodeStorages(t *testing.T) StorageIndex {
	t.Helper()
	idx, err := NewStorageIndex([]pve.StorageInfo{
		{Name: "nfs", Type: "nfs", Shared: true},
		{Name: "ab", Type: "nfs", Shared: true, Nodes: []string{"a", "b"}},
		{Name: "local-lvm", Type: "lvmthin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func deletionDestinations(t *testing.T, set *RuleSet, down ...string) (DeadNodeReach, error) {
	t.Helper()
	return DeletionDestinations("a", deadNodeMembers, set, "vm:101", "bosh-na-101", ownedByUs,
		placeableExcept(down...), deadNodeStorages(t))
}

// TestDeletionDestinationsNamesOnlyTheSharedAccessSet is the three-node case:
// the VM's storage reaches nodes a and b, so its strict pin names them, node a
// dies, and the destinations name b alone, so the CRM can never steal the
// config onto c.
func TestDeletionDestinationsNamesOnlyTheSharedAccessSet(t *testing.T) {
	t.Parallel()
	got, err := deletionDestinations(t, deadNodeRules(deadNodePin(true, map[string]int{"a": 2, "b": 0})), "a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Destinations, []string{"b"}) || !slices.Equal(got.Reach, []string{"b"}) ||
		!slices.Equal(got.Admissible, []string{"a", "b"}) || !got.Pinned || got.PinnedToSource() {
		t.Fatalf("got %+v; want b alone, bounded by the pin", got)
	}
	if len(got.Unplaceable) != 0 {
		t.Fatalf("nothing is unplaceable here: %+v", got)
	}
}

func TestDeletionDestinationsCases(t *testing.T) {
	t.Parallel()
	off := deadNodePin(true, map[string]int{"a": 0, "c": 0})
	off.Disabled = true
	errored := deadNodePin(true, map[string]int{"a": 0, "c": 0})
	errored.Errors = []string{"resources: conflict"}
	narrowed := deadNodePin(true, map[string]int{"c": 0})
	narrowed.Comment = "owner=us bosh-vols=nfs dead-node-from=a"
	partial := deadNodePin(false, map[string]int{"a": 0, "b": 0, "c": 0})
	partial.Comment = "owner=us bosh-vols=ab"
	local := deadNodePin(false, map[string]int{"a": 0, "b": 0, "c": 0})
	local.Comment = "owner=us bosh-vols=local-lvm,nfs"
	cases := []struct {
		name        string
		set         *RuleSet
		down        []string
		dests       []string
		unplaceable []string
		pinned      bool
		toSource    bool
	}{
		{name: "a non-strict pin over every node admits every member", set: deadNodeRules(deadNodePin(false,
			map[string]int{"a": 0, "b": 0, "c": 0})), down: []string{"a"}, dests: []string{"b", "c"}},
		{name: "the failed source's stale LRM never blocks", set: deadNodeRules(deadNodePin(false,
			map[string]int{"b": 0})), down: []string{"a"}, dests: []string{"b", "c"}},
		{name: "the recorded storage bounds a non-strict pin", set: deadNodeRules(partial), down: []string{"a"},
			dests: []string{"b"}},
		{name: "a recorded node-local storage keeps the VM on its source", set: deadNodeRules(local),
			down: []string{"a"}},
		{name: "a non-strict pin only orders the nodes", set: deadNodeRules(deadNodePin(false,
			map[string]int{"a": 2})), down: []string{"a"}, dests: []string{"b", "c"}},
		{name: "a pin to the dead node alone", set: deadNodeRules(deadNodePin(true, map[string]int{"a": 0})),
			down: []string{"a"}, pinned: true, toSource: true},
		{name: "a disabled pin still records the set", set: deadNodeRules(off), down: []string{"a"},
			dests: []string{"c"}, pinned: true},
		{name: "a pin in error still records the set", set: deadNodeRules(errored), down: []string{"a"},
			dests: []string{"c"}, pinned: true},
		{name: "a pin an earlier attempt narrowed", set: deadNodeRules(narrowed), down: []string{"a"},
			dests: []string{"c"}, pinned: true},
		{name: "both admitted nodes dead", set: deadNodeRules(deadNodePin(true, map[string]int{"a": 0, "b": 0})),
			down: []string{"a", "b"}, unplaceable: []string{"b"}, pinned: true},
		{name: "a pin node that is no member is ignored", set: deadNodeRules(deadNodePin(true,
			map[string]int{"a": 0, "b": 0, "gone": 0})), down: []string{"a"}, dests: []string{"b"}, pinned: true},
		{name: "other VMs' pins do not count", set: deadNodeRules(deadNodePin(false, map[string]int{"a": 0}),
			Rule{ID: "bosh-na-102", Type: RuleTypeNodeAffinity, Strict: true, Nodes: map[string]int{"c": 0},
				Resources: []string{"vm:102"}}), down: []string{"a"}, dests: []string{"b", "c"}},
	}
	for _, tc := range cases {
		got, err := deletionDestinations(t, tc.set, tc.down...)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got.Destinations, tc.dests) || !slices.Equal(got.Unplaceable, tc.unplaceable) ||
			got.Pinned != tc.pinned || got.PinnedToSource() != tc.toSource {
			t.Errorf("%s: got %+v; want dests %v unplaceable %v pinned %v to source %v", tc.name, got,
				tc.dests, tc.unplaceable, tc.pinned, tc.toSource)
		}
		if got.Source != "a" || slices.Contains(got.Reach, "a") {
			t.Errorf("%s: the dead node is never in the reach: %+v", tc.name, got)
		}
	}
}

func TestDeletionDestinationsRefusesBadInput(t *testing.T) {
	t.Parallel()
	ok := placeableExcept()
	rules := deadNodeRules()
	foreign := deadNodePin(true, map[string]int{"a": 0})
	foreign.Comment = "owner=them"
	shared := deadNodePin(true, map[string]int{"a": 0})
	shared.Resources = []string{"vm:101", "vm:9"}
	empty := deadNodePin(true, map[string]int{})
	elsewhere := deadNodePin(true, map[string]int{"b": 0})
	otherToken := deadNodePin(true, map[string]int{"b": 0})
	otherToken.Comment = "owner=us bosh-vols=nfs dead-node-from=c"
	unrecorded := deadNodePin(true, map[string]int{"a": 0})
	unrecorded.Comment = "owner=us"
	emptyRecord := deadNodePin(true, map[string]int{"a": 0})
	emptyRecord.Comment = "owner=us bosh-vols="
	unknownStorage := deadNodePin(true, map[string]int{"a": 0})
	unknownStorage.Comment = "owner=us bosh-vols=gone"
	idx := deadNodeStorages(t)
	cases := []struct {
		name   string
		source string
		set    *RuleSet
		place  func(string) bool
		owned  func(*Rule) bool
		index  pve.StorageInfoGetter
		want   string
	}{
		{"empty source", " ", rules, ok, ownedByUs, idx, "not named"},
		{"source not a member", "z", rules, ok, ownedByUs, idx, "not a cluster member"},
		{"no health answer", "a", rules, nil, ownedByUs, idx, "no node health"},
		{"no rules read", "a", nil, ok, ownedByUs, idx, "rules were not read"},
		{"no storage index", "a", rules, ok, ownedByUs, nil, "storage index was not read"},
		{"no pin", "a", rules, ok, ownedByUs, idx, "no record of where its volumes are"},
		{"pin is a resource-affinity rule", "a", deadNodeRules(Rule{ID: "bosh-na-101",
			Type: RuleTypeResourceAffinity, Resources: []string{"vm:101", "vm:9"}}), ok, ownedByUs, idx,
			"not a node-affinity pin"},
		{"foreign pin", "a", deadNodeRules(foreign), ok, ownedByUs, idx, "ownership marker"},
		{"no ownership answer", "a", deadNodeRules(deadNodePin(true, map[string]int{"a": 0})), ok, nil, idx,
			"ownership marker"},
		{"pin holds another resource", "a", deadNodeRules(shared), ok, ownedByUs, idx, "not vm:101 alone"},
		{"strict pin with no node", "a", deadNodeRules(empty), ok, ownedByUs, idx, "names no node"},
		{"pin without a volume record", "a", deadNodeRules(unrecorded), ok, ownedByUs, idx,
			"carries no bosh-vols record"},
		{"pin with an empty volume record", "a", deadNodeRules(emptyRecord), ok, ownedByUs, idx,
			"carries no bosh-vols record"},
		{"recorded storage not indexed", "a", deadNodeRules(unknownStorage), ok, ownedByUs, idx,
			"reach of the recorded storages gone"},
		{"strict pin without the dead node", "a", deadNodeRules(elsewhere), ok, ownedByUs, idx,
			"does not name dead node"},
		{"token naming another node", "a", deadNodeRules(otherToken), ok, ownedByUs, idx, "does not name dead node"},
	}
	for _, tc := range cases {
		_, err := DeletionDestinations(tc.source, deadNodeMembers, tc.set, "vm:101", "bosh-na-101", tc.owned,
			tc.place, tc.index)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
	if _, err := DeletionDestinations("a", deadNodeMembers, rules, "", "bosh-na-101", ownedByUs, ok,
		idx); err == nil {
		t.Error("an unnamed resource must be an error")
	}
}

func TestDeadNodePinComment(t *testing.T) {
	t.Parallel()
	marker := "owner=us deployment=- group=-"
	cur := deadNodePin(true, map[string]int{"a": 0})
	cur.Comment = "owner=us deployment=cf group=router"
	if got := DeadNodePinComment(&cur, ownedByUs, marker, "a"); got != cur.Comment+" dead-node-from=a" {
		t.Fatalf("an owned pin's comment is kept: %q", got)
	}
	foreign := cur.Clone()
	foreign.Comment = "owner=them deployment=x"
	for _, c := range []*Rule{nil, &foreign} {
		if got := DeadNodePinComment(c, ownedByUs, marker, "a"); got != marker+" dead-node-from=a" {
			t.Fatalf("a missing or foreign pin starts from the marker: %q", got)
		}
	}
	if got := DeadNodePinComment(&cur, nil, marker, "a"); got != marker+" dead-node-from=a" {
		t.Fatalf("with no ownership answer the marker is used: %q", got)
	}
}

func TestStorageIndexGet(t *testing.T) {
	t.Parallel()
	idx, err := NewStorageIndex([]pve.StorageInfo{{Name: "s", Type: "dir"}})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := idx.Get(context.Background(), "s"); err != nil || info.Type != "dir" {
		t.Fatalf("Get(s) = %+v, %v", info, err)
	}
	if _, err := idx.Get(context.Background(), "missing"); err == nil {
		t.Fatal("an unknown storage must be an error")
	}
}

func TestDeadNodeSourceToken(t *testing.T) {
	t.Parallel()
	marker := "bosh-cpi:v1 owner=u deployment=- group=-"
	tokened := WithDeadNodeSource(marker, "a")
	if tokened != marker+" dead-node-from=a" {
		t.Fatalf("tokened = %q", tokened)
	}
	if again := WithDeadNodeSource(tokened, "b"); again != marker+" dead-node-from=b" {
		t.Fatalf("a second token replaces the first: %q", again)
	}
	if src, ok := DeadNodeSource(tokened); !ok || src != "a" {
		t.Fatalf("DeadNodeSource = %q, %v", src, ok)
	}
	for _, c := range []string{"", marker, marker + " dead-node-from="} {
		if _, ok := DeadNodeSource(c); ok {
			t.Fatalf("comment %q carries no token", c)
		}
	}
	if got := WithDeadNodeSource("", "a"); got != "dead-node-from=a" {
		t.Fatalf("empty comment = %q", got)
	}
}

func ownedByUs(r *Rule) bool { return strings.Contains(r.Comment, "owner=us") }

func deadNodeRules(rules ...Rule) *RuleSet { return &RuleSet{Rules: rules, Digest: "d1"} }

func TestDeadNodeRuleBlockers(t *testing.T) {
	t.Parallel()
	pin := Rule{ID: "bosh-na-101", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: true,
		Nodes: map[string]int{"a": 2}, Resources: []string{"vm:101"}, Comment: "owner=us"}
	ourAA := Rule{ID: "bosh-aa-x", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative,
		Resources: []string{"vm:101", "vm:102"}, Comment: "owner=us"}
	cases := []struct {
		name string
		set  *RuleSet
		want string
	}{
		{"no read", nil, "not read"},
		{"our pin and our anti-affinity", deadNodeRules(pin, ourAA), ""},
		{"no rules", deadNodeRules(), ""},
		{"pin is a resource-affinity rule", deadNodeRules(Rule{ID: "bosh-na-101", Type: RuleTypeResourceAffinity,
			Affinity: AffinityNegative, Resources: []string{"vm:101", "vm:9"}}), "not the VM's node-affinity pin"},
		{"pin is foreign", deadNodeRules(func() Rule { r := pin.Clone(); r.Comment = "owner=them"; return r }()),
			"ownership marker"},
		{"pin holds another resource", deadNodeRules(func() Rule {
			r := pin.Clone()
			r.Resources = []string{"vm:101", "vm:9"}
			return r
		}()), "not vm:101 alone"},
		{"second node-affinity rule", deadNodeRules(pin, Rule{ID: "op-pin", Type: RuleTypeNodeAffinity,
			Resources: []string{"vm:101"}}), "also holds"},
		{"positive resource affinity", deadNodeRules(Rule{ID: "together", Type: RuleTypeResourceAffinity,
			Affinity: AffinityPositive, Resources: []string{"vm:101", "vm:9"}}), "positive resource-affinity"},
		{"foreign anti-affinity", deadNodeRules(Rule{ID: "op-aa", Type: RuleTypeResourceAffinity,
			Affinity: AffinityNegative, Resources: []string{"vm:101", "vm:9"}}), "anti-affinity rule \"op-aa\""},
		{"unknown rule type", deadNodeRules(Rule{ID: "odd", Type: "future", Resources: []string{"vm:101"}}),
			"of type future"},
		{"other VMs' rules", deadNodeRules(Rule{ID: "op-aa", Type: RuleTypeResourceAffinity,
			Affinity: AffinityNegative, Resources: []string{"vm:8", "vm:9"}}), ""},
	}
	for _, tc := range cases {
		got := strings.Join(DeadNodeRuleBlockers(tc.set, "vm:101", "bosh-na-101", ownedByUs), "; ")
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: blockers %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := DeadNodeRuleBlockers(deadNodeRules(pin), "vm:101", "bosh-na-101", nil); len(got) == 0 {
		t.Error("with no ownership answer every rule is foreign")
	}
}

func TestDeadNodePinPlanning(t *testing.T) {
	t.Parallel()
	want := DeadNodePin("bosh-na-101", "vm:101", "owner=us dead-node-from=a", []string{"b", "c"})
	if !want.Strict || want.Nodes["b"] != 0 || want.Nodes["c"] != 0 || len(want.Nodes) != 2 ||
		want.NodesParam() != "b,c" {
		t.Fatalf("the pin is strict and carries no priorities: %+v", want)
	}
	plan := PlanDeadNodePin(want, ownedByUs)

	ops, err := plan(deadNodeRules())
	if err != nil || len(ops) != 1 || ops[0].Kind != OpCreate {
		t.Fatalf("no pin creates one: %+v %v", ops, err)
	}
	old := Rule{ID: "bosh-na-101", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: true,
		Nodes: map[string]int{"a": 2, "b": 1}, Resources: []string{"vm:101"}, Comment: "owner=us"}
	ops, err = plan(deadNodeRules(old))
	if err != nil || len(ops) != 1 || ops[0].Kind != OpUpdate || ops[0].Rule.NodesParam() != "b,c" {
		t.Fatalf("an old pin is replaced in place: %+v %v", ops, err)
	}
	ops, err = plan(deadNodeRules(want))
	if err != nil || len(ops) != 0 {
		t.Fatalf("a matching pin needs no write: %+v %v", ops, err)
	}
	foreign := old.Clone()
	foreign.Comment = "owner=them"
	if _, err := plan(deadNodeRules(foreign)); err == nil {
		t.Fatal("a foreign pin must refuse the plan")
	}
	if _, err := PlanDeadNodePin(DeadNodePin("p", "vm:101", "", nil), ownedByUs)(deadNodeRules()); err == nil {
		t.Fatal("a pin without nodes must refuse the plan")
	}
	bad := want.Clone()
	bad.Resources = nil
	if _, err := PlanDeadNodePin(bad, ownedByUs)(deadNodeRules()); err == nil {
		t.Fatal("a pin without its resource must refuse the plan")
	}
}

func TestDeadNodePinInEffect(t *testing.T) {
	t.Parallel()
	want := DeadNodePin("bosh-na-101", "vm:101", "owner=us", []string{"b"})
	disabled := want.Clone()
	disabled.Disabled = true
	errored := want.Clone()
	errored.Errors = []string{"resources: conflict"}
	wider := want.Clone()
	wider.Nodes = map[string]int{"b": 0, "c": 0}
	cases := []struct {
		name string
		set  *RuleSet
		ok   bool
		why  string
	}{
		{"in effect", deadNodeRules(want), true, ""},
		{"no read", nil, false, "not read"},
		{"missing", deadNodeRules(), false, "missing"},
		{"disabled", deadNodeRules(disabled), false, "disabled"},
		{"errors", deadNodeRules(errored), false, "conflict"},
		{"differs", deadNodeRules(wider), false, "nodes"},
	}
	for _, tc := range cases {
		ok, why := DeadNodePinInEffect(tc.set, want)
		if ok != tc.ok || !strings.Contains(why, tc.why) {
			t.Errorf("%s: got %v %q", tc.name, ok, why)
		}
	}
}

func TestPlanAntiAffinityRelease(t *testing.T) {
	t.Parallel()
	three := Rule{ID: "bosh-aa-3", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative,
		Resources: []string{"vm:101", "vm:102", "vm:103"}, Comment: "owner=us", Errors: []string{"x: y"}}
	two := Rule{ID: "bosh-aa-2", Type: RuleTypeResourceAffinity, Affinity: AffinityNegative,
		Resources: []string{"vm:101", "vm:104"}, Comment: "owner=us"}
	plan := PlanAntiAffinityRelease("vm:101", "bosh-na-101", ownedByUs)
	ops, err := plan(deadNodeRules(three, two))
	if err != nil || len(ops) != 2 {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
	if ops[0].Kind != OpUpdate || !slices.Equal(ops[0].Rule.Resources, []string{"vm:102", "vm:103"}) ||
		len(ops[0].Rule.Errors) != 0 {
		t.Fatalf("a rule of three keeps the other two: %+v", ops[0])
	}
	if ops[1].Kind != OpDelete || ops[1].Rule.ID != "bosh-aa-2" {
		t.Fatalf("a rule of two is deleted: %+v", ops[1])
	}
	if !slices.Equal(three.Resources, []string{"vm:101", "vm:102", "vm:103"}) {
		t.Fatal("planning must not change the read")
	}
	if ops, err := plan(deadNodeRules()); err != nil || len(ops) != 0 {
		t.Fatalf("nothing to release: %+v %v", ops, err)
	}
	foreign := two.Clone()
	foreign.Comment = "owner=them"
	if _, err := plan(deadNodeRules(foreign)); err == nil {
		t.Fatal("a foreign anti-affinity rule must refuse the plan")
	}
}

func TestAllowedNodes(t *testing.T) {
	t.Parallel()
	pin := Rule{ID: "p", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: true,
		Nodes: map[string]int{"a": 1, "b": 0}, Resources: []string{"vm:101"}}
	loose := pin.Clone()
	loose.Strict = false
	off := pin.Clone()
	off.Disabled = true
	broken := pin.Clone()
	broken.Errors = []string{"x: y"}
	cases := []struct {
		name string
		set  *RuleSet
		want []string
	}{
		{"no read", nil, deadNodeMembers},
		{"no pin", deadNodeRules(), deadNodeMembers},
		{"strict pin", deadNodeRules(pin), []string{"a", "b"}},
		{"non-strict pin", deadNodeRules(loose), deadNodeMembers},
		{"disabled pin", deadNodeRules(off), deadNodeMembers},
		{"pin with errors", deadNodeRules(broken), deadNodeMembers},
	}
	for _, tc := range cases {
		if got := AllowedNodes(tc.set, "vm:101", deadNodeMembers); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOrDash(t *testing.T) {
	t.Parallel()
	if orDash("") != "-" || orDash("x") != "x" {
		t.Fatal("orDash")
	}
}
