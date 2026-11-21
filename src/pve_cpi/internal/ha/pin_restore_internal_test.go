package ha

import (
	"maps"
	"strings"
	"testing"
	"time"
)

const restoreTestMarker = "bosh-cpi:v1 owner=dir-1 deployment=- group=- vr=shared"

// restoreTestPin returns a pin of vm:1 over nodes, written the way the API
// takes them, with comment.
func restoreTestPin(t *testing.T, nodes, comment string, strict bool) *Rule {
	t.Helper()
	pin := &Rule{ID: "bosh-na-1", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Strict: strict,
		Comment: comment, Resources: []string{"vm:1"}, Nodes: map[string]int{}}
	got, err := parseWireNodes([]byte(`"` + nodes + `"`))
	if err != nil {
		t.Fatal(err)
	}
	pin.Nodes = got
	return pin
}

// TestPinRestoreRecordKeepsTheNodesAndPriorities shows that the record a
// flatten writes names every node of the pin with its priority, in the form
// the API takes, and reads back the same for node names with dashes and dots.
func TestPinRestoreRecordKeepsTheNodesAndPriorities(t *testing.T) {
	pin := restoreTestPin(t, "az2-0,az2-1,pve-3.lab.example:2", restoreTestMarker, false)
	comment := WithPinRestoreRecord(pin.Comment, pin)
	if want := restoreTestMarker + " " + PinRestoreKey + "=az2-0,az2-1,pve-3.lab.example:2"; comment != want {
		t.Fatalf("comment = %q, want %q", comment, want)
	}
	nodes, present, ok := PinRestoreRecord(comment)
	if !present || !ok || len(nodes) != 3 || nodes["pve-3.lab.example"] != 2 || nodes["az2-0"] != 0 {
		t.Fatalf("record = %v present %v ok %v", nodes, present, ok)
	}
	// The record stays one field, so the marker's other readers see it as
	// one more key they do not know.
	if fields, _, ok := MarkerFields(comment); !ok || len(fields) != 5 {
		t.Fatalf("marker fields = %v", fields)
	}
}

// TestPinRestoreRecordOriginalWins shows that a second flatten never replaces
// the record, because the pin it flattens is no longer the one the VM had
// before it joined.
func TestPinRestoreRecordOriginalWins(t *testing.T) {
	first := restoreTestPin(t, "a:2,b:1,c", restoreTestMarker, false)
	comment := WithPinRestoreRecord(first.Comment, first)
	second := restoreTestPin(t, "a,b,c:5", comment, false)
	if got := WithPinRestoreRecord(second.Comment, second); got != comment {
		t.Fatalf("second flatten rewrote the record: %q, want %q", got, comment)
	}
	// A record that does not parse is still a record, so a flatten does not
	// put a new one beside it.
	broken := restoreTestMarker + " " + PinRestoreKey + "=a:x"
	if got := WithPinRestoreRecord(broken, second); got != broken {
		t.Fatalf("flatten replaced a broken record: %q", got)
	}
}

// TestPinRestoreRecordSkipsWhatItCannotWrite shows that a flatten writes no
// record for a node name the record's separators would split, and none that
// would carry the comment past what PVE stores.
func TestPinRestoreRecordSkipsWhatItCannotWrite(t *testing.T) {
	for _, bad := range []string{"a,b", "a:b", "a=b", "a b", "a%3Ab"} {
		pin := &Rule{ID: "bosh-na-1", Comment: restoreTestMarker, Nodes: map[string]int{bad: 2, "c": 0}}
		if got := WithPinRestoreRecord(pin.Comment, pin); got != restoreTestMarker {
			t.Errorf("node %q: comment = %q", bad, got)
		}
	}
	long := restoreTestMarker + " x=" + strings.Repeat("y", MaxRuleCommentLength-len(restoreTestMarker)-10)
	pin := restoreTestPin(t, "a:2,b", long, false)
	if got := WithPinRestoreRecord(long, pin); got != long {
		t.Fatalf("record carried the comment to %d characters", len(got))
	}
}

// TestPinRestoreRecordRejectsMalformedValues shows that a record that does not
// name a node with an integer priority, or names one twice, reads as present
// but not usable.
func TestPinRestoreRecordRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "a:", "a:x", ",a", "a,,b", "a:1,a:2"} {
		if _, present, ok := PinRestoreRecord(restoreTestMarker + " " + PinRestoreKey + "=" + value); !present || ok {
			t.Errorf("value %q: present %v ok %v", value, present, ok)
		}
	}
	if _, present, _ := PinRestoreRecord(restoreTestMarker); present {
		t.Fatal("a comment with no record reads as one that carries a record")
	}
}

// TestPlanPinRestore pins what the restore writes for each pin it may find
// once the VM is in no resource-affinity rule.
func TestPlanPinRestore(t *testing.T) {
	configured := []string{"az1-0", "az1-1", "az2-0", "az2-1", "az2-2"}
	recorded := restoreTestMarker + " " + PinRestoreKey + "=az2-0,az2-1,az2-2:2"
	for name, tc := range map[string]struct {
		nodes      string
		comment    string
		strict     bool
		configured []string
		action     PinRestoreAction
		want       string
		wantStrict bool
	}{
		// The pin the lab found: the flatten widened a soft pin to every
		// configured node at one priority.
		"widened soft pin": {nodes: "az1-0,az1-1,az2-0,az2-1,az2-2", comment: recorded, configured: configured,
			action: PinRestoreWrite, want: "az2-0,az2-1,az2-2:2"},
		"flattened soft pin": {nodes: "az2-0,az2-1,az2-2", comment: recorded, configured: configured,
			action: PinRestoreWrite, want: "az2-0,az2-1,az2-2:2"},
		// A strict pin keeps the nodes it confines the VM to, which a CPI
		// writer such as a narrow may have changed, and gets the priorities
		// back on the nodes the record names.
		"strict pin": {nodes: "az2-0,az2-2,az2-9", comment: recorded, strict: true, configured: configured,
			action: PinRestoreWrite, want: "az2-0,az2-2:2,az2-9", wantStrict: true},
		"strict pin with unknown nodes": {nodes: "az2-0,az2-1,az2-2", comment: recorded, strict: true,
			action: PinRestoreWrite, want: "az2-0,az2-1,az2-2:2", wantStrict: true},
		// An operator who changed the pin after the flatten wins.
		"priorities set again": {nodes: "az1-0:3,az2-0,az2-1,az2-2", comment: recorded, configured: configured,
			action: PinRestoreDrop, want: "az1-0:3,az2-0,az2-1,az2-2"},
		"recorded node taken out": {nodes: "az2-0,az2-2", comment: recorded, configured: configured,
			action: PinRestoreDrop, want: "az2-0,az2-2"},
		"unconfigured node added": {nodes: "az2-0,az2-1,az2-2,elsewhere", comment: recorded,
			configured: configured, action: PinRestoreDrop, want: "az2-0,az2-1,az2-2,elsewhere"},
		"broken record": {nodes: "az2-0,az2-1", comment: restoreTestMarker + " " + PinRestoreKey + "=az2-0:x",
			configured: configured, action: PinRestoreDrop, want: "az2-0,az2-1"},
		// Without the configured nodes the restore cannot tell which nodes the
		// flatten added, so it waits for a plan that knows them.
		"configured unknown": {nodes: "az1-0,az2-0,az2-1,az2-2", comment: recorded, action: PinRestoreNone},
		"no record": {nodes: "az2-0,az2-1", comment: restoreTestMarker, configured: configured,
			action: PinRestoreNone},
	} {
		t.Run(name, func(t *testing.T) {
			cur := restoreTestPin(t, tc.nodes, tc.comment, tc.strict)
			before := cur.Clone()
			next, action, reason := PlanPinRestore(cur, tc.configured)
			if !SameRule(&before, cur) {
				t.Fatal("PlanPinRestore changed the pin it was given")
			}
			if action != tc.action {
				t.Fatalf("action = %v (%s), want %v", action, reason, tc.action)
			}
			if action == PinRestoreNone {
				if next != nil {
					t.Fatalf("no restore returned a pin %+v", next)
				}
				return
			}
			if action == PinRestoreDrop && reason == "" {
				t.Fatal("a drop says nothing about why")
			}
			if next.NodesParam() != tc.want || next.Strict != tc.wantStrict {
				t.Fatalf("restored nodes %q strict %v, want %q strict %v", next.NodesParam(), next.Strict, tc.want,
					tc.wantStrict)
			}
			if _, present, _ := PinRestoreRecord(next.Comment); present {
				t.Fatalf("restored pin kept the record: %q", next.Comment)
			}
			if !strings.HasPrefix(next.Comment, restoreTestMarker) {
				t.Fatalf("restored pin lost the rest of its comment: %q", next.Comment)
			}
		})
	}
}

// TestCarryPinRestore shows that a writer that builds a pin afresh keeps the
// record while the pin it writes has one priority, and drops it once the pin
// carries priorities of the CPI's own planning.
func TestCarryPinRestore(t *testing.T) {
	recorded := restoreTestMarker + " " + PinRestoreKey + "=a:2,b"
	cur := restoreTestPin(t, "a,b,c", recorded, false)

	flat := restoreTestPin(t, "a,b,c", restoreTestMarker, false)
	CarryPinRestore(cur, flat)
	if flat.Comment != recorded {
		t.Fatalf("flat pin comment = %q, want %q", flat.Comment, recorded)
	}
	planned := restoreTestPin(t, "a:2,b:1,c", recorded, false)
	CarryPinRestore(cur, planned)
	if planned.Comment != restoreTestMarker {
		t.Fatalf("planned pin comment = %q, want %q", planned.Comment, restoreTestMarker)
	}
	fresh := restoreTestMarker + " " + PinRestoreKey + "=c:2,a,b"
	own := restoreTestPin(t, "a,b,c", fresh, false)
	CarryPinRestore(cur, own)
	if own.Comment != fresh {
		t.Fatalf("pin with its own record: comment = %q, want %q", own.Comment, fresh)
	}
	none := restoreTestPin(t, "a,b,c", restoreTestMarker, false)
	CarryPinRestore(nil, none)
	CarryPinRestore(cur, nil)
	if none.Comment != restoreTestMarker {
		t.Fatalf("no current pin: comment = %q", none.Comment)
	}
}

// TestPinWritersKeepTheRestoreRecord shows that every helper that derives a
// pin or its comment from the current pin keeps the record.
func TestPinWritersKeepTheRestoreRecord(t *testing.T) {
	recorded := restoreTestMarker + " " + PinRestoreKey + "=a:2,b"
	cur := restoreTestPin(t, "a,b", recorded, true)
	owned := func(*Rule) bool { return true }
	for name, comment := range map[string]string{
		"Flattened":          Flattened(cur).Comment,
		"WithEveryNode":      WithEveryNode(cur, []string{"a", "b", "c"}).Comment,
		"ConfinementOnly":    ConfinementOnly(cur).Comment,
		"WithVolumeRecord":   WithVolumeRecord(cur.Comment, []string{"other"}),
		"WithReplicaPartner": WithReplicaPartner(cur.Comment, "b"),
		"WithReplicaChanged": WithReplicaChanged(cur.Comment, time.Unix(1_700_000_000, 0)),
		"WithReplicaRebuild": WithReplicaRebuild(cur.Comment, "b"),
		"WithDeadNodeSource": WithDeadNodeSource(cur.Comment, "a"),
		"DeadNodePinComment": DeadNodePinComment(cur, owned, "bosh-cpi:v1 owner=dir-1", "a"),
		"Clone":              cur.Clone().Comment,
	} {
		nodes, present, ok := PinRestoreRecord(comment)
		if !present || !ok || nodes["a"] != 2 || len(nodes) != 2 {
			t.Errorf("%s dropped the record: %q", name, comment)
		}
	}
}

// TestPinRestoreRecordLeavesOtherReadersAlone stands in for a downgrade. An
// older release reads a pin's comment through these same readers, and the
// record must change nothing they read.
func TestPinRestoreRecordLeavesOtherReadersAlone(t *testing.T) {
	base := WithReplicaPartner(WithVolumeRecord(restoreTestMarker, []string{"nfs", "shared"}), "b")
	pin := restoreTestPin(t, "a:2,b", base, false)
	recorded := WithPinRestoreRecord(base, pin)
	if recorded == base {
		t.Fatal("no record was written")
	}
	for _, comment := range []string{base, recorded} {
		if vols, ok := VolumeRecord(comment); !ok || strings.Join(vols, ",") != "nfs,shared" {
			t.Errorf("%q: volume record %v %v", comment, vols, ok)
		}
		if partner, ok := ReplicaPartner(comment); !ok || partner != "b" {
			t.Errorf("%q: partner %q %v", comment, partner, ok)
		}
		if _, ok := DeadNodeSource(comment); ok {
			t.Errorf("%q: reads a dead-node token", comment)
		}
	}
	if DecodedMarkerComment(recorded) != recorded {
		t.Fatalf("the marker decoder changed the record: %q", DecodedMarkerComment(recorded))
	}
}

// TestBuildPinRecordsThePinOutsideTheRule shows that the pin BuildPin makes
// for a VM in a resource-affinity rule records the pin the same spec gives
// the VM outside the rule, so the restore can give that pin back, and that
// a spec for a VM in no such rule records nothing.
func TestBuildPinRecordsThePinOutsideTheRule(t *testing.T) {
	spec := PinSpec{
		ID: "bosh-na-1", Comment: restoreTestMarker, SID: "vm:1", Configured: []string{"a", "b", "c"},
		Preferred: []string{"a"}, Placed: "b",
	}
	free, err := BuildPin(spec)
	if err != nil || free == nil || !free.MultiplePriorities() {
		t.Fatalf("free pin = %+v, %v", free, err)
	}
	if _, present, _ := PinRestoreRecord(free.Comment); present {
		t.Fatalf("a pin outside the rule records %q", free.Comment)
	}
	spec.SinglePriority = true
	flat, err := BuildPin(spec)
	if err != nil || flat == nil || flat.MultiplePriorities() {
		t.Fatalf("flat pin = %+v, %v", flat, err)
	}
	nodes, present, ok := PinRestoreRecord(flat.Comment)
	if !present || !ok || !maps.Equal(nodes, free.Nodes) {
		t.Fatalf("flat pin records %v (present %v, ok %v), want %v", nodes, present, ok, free.Nodes)
	}

	// A spec with no preference gets no pin, so there is nothing to record.
	spec.Preferred, spec.Placed = nil, ""
	if none, err := BuildPin(spec); err != nil || none != nil {
		t.Fatalf("pin with no preference = %+v, %v", none, err)
	}
}

// TestAFreshPinWithNoPreferenceDropsAnOutdatedRecord shows that a writer
// that builds the pin of a VM in a resource-affinity rule afresh, when it
// knows the VM's preference and the pin the VM gets outside the rule has none
// of its own, does not carry the record of the pin it replaces. That record
// names priorities the CPI no longer asks for, so the restore would write
// them back and failback would move the VM. A writer that doesn't know the
// preference keeps the record.
func TestAFreshPinWithNoPreferenceDropsAnOutdatedRecord(t *testing.T) {
	configured := []string{"a", "b", "c"}
	cur := &Rule{
		ID: "bosh-na-101", Type: RuleTypeNodeAffinity, Affinity: AffinityPositive, Resources: []string{"vm:101"},
		Nodes:   map[string]int{"a": PinPriorityRest, "b": PinPriorityRest, "c": PinPriorityRest},
		Comment: WithPinRestoreRecord(restoreTestMarker, &Rule{Nodes: map[string]int{"a": 2, "b": 2, "c": 0}}),
	}

	t.Run("no preference at all", func(t *testing.T) {
		d := &VMDecision{VMID: 101, SID: "vm:101", Node: "a", Outcome: OutcomeRegister, Policy: PolicyShared,
			Admissible: configured, PreferenceKnown: true}
		next, err := DecisionPin(d, "bosh-na-101", restoreTestMarker, []string{"nfs"}, configured, false, true)
		if err != nil || next == nil {
			t.Fatalf("pin = %+v, %v", next, err)
		}
		CarryPinRestore(cur, next)
		if raw, present := pinRestoreValue(next.Comment); present && raw == "a:2,b:2,c" {
			t.Fatalf("the fresh pin carries the outdated record: %q", next.Comment)
		}
		if got, act, why := PlanPinRestore(next, configured); act == PinRestoreWrite && got.MultiplePriorities() {
			t.Fatalf("the restore writes priorities the CPI no longer asks for: %+v (%s)", got.Nodes, why)
		}
	})

	t.Run("a preference for every node", func(t *testing.T) {
		next, err := BuildPin(PinSpec{
			ID: "bosh-na-101", Comment: restoreTestMarker, SID: "vm:101", Configured: configured,
			Preferred: configured, SinglePriority: true, PreferenceKnown: true,
		})
		if err != nil || next == nil {
			t.Fatalf("pin = %+v, %v", next, err)
		}
		CarryPinRestore(cur, next)
		if got, act, why := PlanPinRestore(next, configured); act == PinRestoreWrite && got.MultiplePriorities() {
			t.Fatalf("the restore writes priorities the CPI no longer asks for: %+v (%s)", got.Nodes, why)
		}
	})

	t.Run("a preference the writer doesn't know", func(t *testing.T) {
		d := &VMDecision{VMID: 101, SID: "vm:101", Node: "a", Outcome: OutcomeRegister, Policy: PolicyShared,
			Admissible: configured}
		next, err := DecisionPin(d, "bosh-na-101", restoreTestMarker, []string{"nfs"}, configured, false, true)
		if err != nil || next == nil {
			t.Fatalf("pin = %+v, %v", next, err)
		}
		CarryPinRestore(cur, next)
		if raw, _ := pinRestoreValue(next.Comment); raw != "a:2,b:2,c" {
			t.Fatalf("a writer that doesn't know the preference must keep the record: %q", next.Comment)
		}
	})
}
