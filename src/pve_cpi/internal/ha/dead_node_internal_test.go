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

func TestStorageMayHoldGuestVolume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info pve.StorageInfo
		want bool
	}{
		{"no content list", pve.StorageInfo{Name: "s"}, true},
		{"images", pve.StorageInfo{Name: "s", Content: "images"}, true},
		{"rootdir", pve.StorageInfo{Name: "s", Content: "rootdir"}, true},
		{"iso among others", pve.StorageInfo{Name: "s", Content: "backup, iso"}, true},
		{"snippets", pve.StorageInfo{Name: "s", Content: "snippets"}, true},
		{"backup and templates only", pve.StorageInfo{Name: "s", Content: "backup,vztmpl"}, false},
		{"node list names the source", pve.StorageInfo{Name: "s", Nodes: []string{"a", "b"}}, true},
		{"node list leaves out the source", pve.StorageInfo{Name: "s", Nodes: []string{"b", "c"}}, false},
		{"disabled still counts", pve.StorageInfo{Name: "s", Disabled: true, Content: "images"}, true},
	}
	for _, tc := range cases {
		if got := StorageMayHoldGuestVolume(tc.info, "a"); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDeletionDestinationsNamesOnlyTheSharedAccessSet is the three-node case:
// the VM's storage reaches nodes a and b, node a dies, and the destinations
// name b alone, so the CRM can never steal the config onto c.
func TestDeletionDestinationsNamesOnlyTheSharedAccessSet(t *testing.T) {
	t.Parallel()
	storages := []pve.StorageInfo{
		{Name: "nfs-ab", Type: "nfs", Shared: true, Nodes: []string{"a", "b"}, Content: "images,iso"},
		{Name: "backups", Type: "dir", Content: "backup"},
		{Name: "zfs-c", Type: "zfspool", Nodes: []string{"c"}, Content: "images"},
	}
	got, err := DeletionDestinations(context.Background(), "a", deadNodeMembers, storages, placeableExcept("a"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Destinations, []string{"b"}) || !slices.Equal(got.Reach, []string{"b"}) {
		t.Fatalf("destinations = %v reach = %v; want b alone", got.Destinations, got.Reach)
	}
	if len(got.Blocking) != 0 || len(got.Unplaceable) != 0 {
		t.Fatalf("nothing blocks or is unplaceable here: %+v", got)
	}
}

func TestDeletionDestinationsCases(t *testing.T) {
	t.Parallel()
	shared := pve.StorageInfo{Name: "ceph", Type: "rbd", Content: "images"}
	cases := []struct {
		name        string
		storages    []pve.StorageInfo
		down        []string
		dests       []string
		blocking    []string
		unplaceable []string
	}{
		{name: "shared everywhere", storages: []pve.StorageInfo{shared}, down: []string{"a"},
			dests: []string{"b", "c"}},
		{name: "the failed source's stale LRM never blocks", storages: []pve.StorageInfo{shared},
			down: []string{"a"}, dests: []string{"b", "c"}},
		{name: "node-local images on the dead node",
			storages: []pve.StorageInfo{shared, {Name: "local-lvm", Type: "lvmthin", Content: "images,rootdir"}},
			down:     []string{"a"}, blocking: []string{"local-lvm"}},
		{name: "node-local ISO storage on the dead node",
			storages: []pve.StorageInfo{shared, {Name: "local", Type: "dir", Content: "iso,vztmpl,backup"}},
			down:     []string{"a"}, blocking: []string{"local"}},
		{name: "disabled storage", storages: []pve.StorageInfo{shared,
			{Name: "nfs-off", Type: "nfs", Shared: true, Disabled: true, Content: "images"}},
			down: []string{"a"}, blocking: []string{"nfs-off"}},
		{name: "both access nodes dead",
			storages: []pve.StorageInfo{{Name: "nfs-ab", Type: "nfs", Shared: true, Nodes: []string{"a", "b"}}},
			down:     []string{"a", "b"}, unplaceable: []string{"b"}},
		{name: "the other access node has a stale LRM",
			storages: []pve.StorageInfo{{Name: "nfs-ab", Type: "nfs", Shared: true, Nodes: []string{"a", "b"}}},
			down:     []string{"a", "b"}, unplaceable: []string{"b"}},
		{name: "storage on other nodes only is skipped",
			storages: []pve.StorageInfo{shared, {Name: "zfs-b", Type: "zfspool", Nodes: []string{"b"}}},
			down:     []string{"a"}, dests: []string{"b", "c"}},
		{name: "a storage reaching only the dead node",
			storages: []pve.StorageInfo{{Name: "nfs-a", Type: "nfs", Shared: true, Nodes: []string{"a"}}},
			down:     []string{"a"}, blocking: []string{"nfs-a"}},
		{name: "no storage", down: []string{"a"}, dests: []string{"b", "c"}},
	}
	for _, tc := range cases {
		got, err := DeletionDestinations(context.Background(), "a", deadNodeMembers, tc.storages,
			placeableExcept(tc.down...))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got.Destinations, tc.dests) || !slices.Equal(got.Blocking, tc.blocking) ||
			!slices.Equal(got.Unplaceable, tc.unplaceable) {
			t.Errorf("%s: got dests %v blocking %v unplaceable %v; want %v %v %v", tc.name,
				got.Destinations, got.Blocking, got.Unplaceable, tc.dests, tc.blocking, tc.unplaceable)
		}
		if got.Source != "a" || slices.Contains(got.Reach, "a") {
			t.Errorf("%s: the dead node is never in the reach: %+v", tc.name, got)
		}
	}
}

func TestDeletionDestinationsRefusesBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ok := placeableExcept()
	cases := []struct {
		name     string
		source   string
		storages []pve.StorageInfo
		place    func(string) bool
		want     string
	}{
		{"empty source", " ", nil, ok, "not named"},
		{"source not a member", "z", nil, ok, "not a cluster member"},
		{"no health answer", "a", nil, nil, "no node health"},
		{"unnamed storage", "a", []pve.StorageInfo{{Type: "dir"}}, ok, "no name"},
		{"duplicate storage", "a", []pve.StorageInfo{{Name: "s"}, {Name: "s"}}, ok, "twice"},
	}
	for _, tc := range cases {
		_, err := DeletionDestinations(ctx, tc.source, deadNodeMembers, tc.storages, tc.place)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error naming %q", tc.name, err, tc.want)
		}
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
