package ha

import "testing"

// TestBuildPinGivesAnAntiAffinityMemberEveryNode shows that a VM in a
// resource-affinity rule with no hard constraint gets no preference in its
// pin. PVE gives every node a soft pin leaves out a lower priority and applies
// resource affinity only to the highest, so a placed-node pin would act as a
// strict one, and PVE refuses several priorities for such a VM anyway.
func TestBuildPinGivesAnAntiAffinityMemberEveryNode(t *testing.T) {
	cfg := []string{"a", "b", "c"}
	for name, spec := range map[string]PinSpec{
		"rebalance on start": {RebalanceOnStart: true, Placed: "b"},
		"preferred AZ":       {Preferred: []string{"b"}, Placed: "b"},
		"both":               {RebalanceOnStart: true, Preferred: []string{"a", "b"}, Placed: "b"},
	} {
		spec.ID, spec.SID, spec.Configured, spec.SinglePriority = "p", "vm:1", cfg, true
		pin, err := BuildPin(spec)
		if err != nil || pin == nil || pin.Strict || pin.NodesParam() != "a,b,c" {
			t.Errorf("%s: pin %+v, %v", name, pin, err)
		}
		if SoftPinNamesSomeNodes(pin, cfg) {
			t.Errorf("%s: the pin still leaves out a node", name)
		}
	}

	// A member with nothing to prefer, or whose configured nodes are unknown,
	// gets no pin here and the record pin from DecisionPin.
	for name, spec := range map[string]PinSpec{
		"nothing to prefer": {Placed: "b", Configured: cfg},
		"dlb":               {RebalanceOnStart: true, DLB: true, Placed: "b", Configured: cfg},
		"unknown nodes":     {RebalanceOnStart: true, Preferred: []string{"b"}, Placed: "b"},
	} {
		spec.ID, spec.SID, spec.SinglePriority = "p", "vm:1", true
		if pin, err := BuildPin(spec); pin != nil || err != nil {
			t.Errorf("%s: pin %+v, %v", name, pin, err)
		}
	}

	// A hard constraint keeps its strict pin over its own nodes.
	pin, err := BuildPin(PinSpec{ID: "p", SID: "vm:1", Hard: []string{"b"}, Configured: cfg, Placed: "b",
		RebalanceOnStart: true, SinglePriority: true})
	if err != nil || !pin.Strict || pin.NodesParam() != "b" {
		t.Fatalf("hard pin = %+v, %v", pin, err)
	}
}

func TestDecisionPinOfAnAntiAffinityMemberWithUnknownNodesIsTheRecordPin(t *testing.T) {
	d := &VMDecision{VMID: 1, SID: "vm:1", Node: "b", Outcome: OutcomeRegister, Placed: "b",
		Admissible: []string{"a", "b", "c"}}
	pin, err := DecisionPin(d, "p", "m", []string{"s"}, nil, true, true)
	if err != nil || pin.Strict || pin.NodesParam() != "a,b,c" {
		t.Fatalf("pin = %+v, %v", pin, err)
	}
}

func TestSoftPinNamesSomeNodes(t *testing.T) {
	cfg := []string{"a", "b", "c"}
	off := naRule("p", false, map[string]int{"a": 0}, "vm:1")
	off.Disabled = true
	negative := naRule("p", false, map[string]int{"a": 0}, "vm:1")
	negative.Affinity = AffinityNegative
	for _, tc := range []struct {
		name string
		pin  *Rule
		cfg  []string
		want bool
	}{
		{"soft single node", ptr(naRule("p", false, map[string]int{"a": 0}, "vm:1")), cfg, true},
		{"soft with priorities", ptr(naRule("p", false, map[string]int{"a": 2, "b": 0}, "vm:1")), cfg, true},
		{"soft every node", ptr(naRule("p", false, map[string]int{"a": 0, "b": 0, "c": 0}, "vm:1")), cfg, false},
		{"strict single node", ptr(naRule("p", true, map[string]int{"a": 0}, "vm:1")), cfg, false},
		{"disabled", &off, cfg, false},
		{"negative", &negative, cfg, false},
		{"unknown nodes", ptr(naRule("p", false, map[string]int{"a": 0}, "vm:1")), nil, false},
		{"no pin", nil, cfg, false},
	} {
		if got := SoftPinNamesSomeNodes(tc.pin, tc.cfg); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithEveryNode(t *testing.T) {
	pin := naRule("p", false, map[string]int{"b": 2}, "vm:1")
	pin.Comment = "c"
	got := WithEveryNode(&pin, []string{"a", "b", "c"})
	if got.Strict || got.NodesParam() != "a,b,c" || got.Comment != "c" || pin.NodesParam() != "b:2" {
		t.Fatalf("widened = %+v, source = %s", got, pin.NodesParam())
	}
	empty := Rule{ID: "p", Type: RuleTypeNodeAffinity}
	if got := WithEveryNode(&empty, []string{"a"}); got.NodesParam() != "a" {
		t.Fatalf("widened empty pin = %s", got.NodesParam())
	}
}

func ptr(r Rule) *Rule { return &r }
