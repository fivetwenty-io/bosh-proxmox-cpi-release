package handlers

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// restoreLabPin is the pin the lab saw on a member before its join: soft, on
// three of the four nodes, with a higher priority on the third.
const restoreLabPin = "pve1,pve2,pve3:2"

// restoreFixture is a four-node cluster whose rule holds three members, with
// the members 101, 102, and 103 of the "web" group tagged by index, and 103's
// pin seeded as restoreLabPin.
func restoreFixture(t *testing.T) (*aaClusterStub, Deps, string) {
	t.Helper()
	stub := newAAStub()
	stub.tagged(aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"), aaQEMU(103, aaTag+";index--3"))
	markWebMembers(stub, 101, 102, 103)
	pin := naRuleNameFor(103)
	stub.seedRuleWithComment(pin, "vm:103", haIdentity{Owner: aaDirector}.marker())
	stub.ruleTypes[pin] = ha.RuleTypeNodeAffinity
	stub.ruleNodes[pin] = restoreLabPin
	return stub, aaMetadataDeps(stub), pin
}

// joinWeb runs the set_vm_metadata join of vmid at index.
func joinWeb(t *testing.T, deps Deps, vmid, index int, logger *log.Logger) {
	t.Helper()
	if _, err := applyAntiAffinityMembership(context.Background(), deps, aaIdent("web"), vmid,
		aaJoinOpts{index: index, checkSelf: true}, logger); err != nil {
		t.Fatalf("join of %d: %v", vmid, err)
	}
}

// joinIndexZero tags the member 100 with index 0 and joins it, which takes
// 103's place in the rule, so 103 leaves its last rule.
func joinIndexZero(t *testing.T, stub *aaClusterStub, deps Deps, logger *log.Logger) {
	t.Helper()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"),
		aaQEMU(103, aaTag+";index--3"))
	markWebMembers(stub, 100)
	joinWeb(t, deps, 100, 0, logger)
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want the three lowest indexes", got)
	}
}

// pinRecord returns the record on the stub's pin comment.
func pinRecord(stub *aaClusterStub, pin string) (string, bool) {
	for f := range strings.FieldsSeq(stub.ruleComments[pin]) {
		if v, ok := strings.CutPrefix(f, ha.PinRestoreKey+"="); ok {
			return v, true
		}
	}
	return "", false
}

// TestPinRestore_JoinFlattenLeaveRestore walks the sequence the lab saw. The
// join of 103 flattens its pin and records the priorities it had, and once a
// member with a lower index takes 103's place, the same join writes them back
// and drops the record.
func TestPinRestore_JoinFlattenLeaveRestore(t *testing.T) {
	t.Parallel()
	stub, deps, pin := restoreFixture(t)
	joinWeb(t, deps, 103, 3, log.NewNopLogger())
	if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:103" {
		t.Fatalf("rule = %q", got)
	}
	if got := stub.ruleNodes[pin]; strings.Contains(got, ":") {
		t.Fatalf("pin of a member in the rule = %q; want one priority", got)
	}
	if rec, ok := pinRecord(stub, pin); !ok || rec != restoreLabPin {
		t.Fatalf("record = %q, %v; want %q", rec, ok, restoreLabPin)
	}

	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	joinIndexZero(t, stub, deps, logger)
	if got := stub.ruleNodes[pin]; got != restoreLabPin {
		t.Fatalf("pin after the member left = %q; want %q", got, restoreLabPin)
	}
	if rec, ok := pinRecord(stub, pin); ok {
		t.Fatalf("record %q stayed after the restore", rec)
	}
	if !strings.HasPrefix(stub.ruleComments[pin], haIdentity{Owner: aaDirector}.marker()) {
		t.Fatalf("restore lost the pin's marker: %q", stub.ruleComments[pin])
	}
	for _, e := range obs.All() {
		if e.Level == slog.LevelInfo && strings.Contains(e.Message, "restored the priorities") {
			return
		}
	}
	t.Fatalf("no line says the priorities came back in %+v", obs.All())
}

// TestPinRestore_SecondFlattenKeepsTheOriginal pins that a flatten of a pin
// that records priorities already keeps that record, so the priorities the VM
// had before its first join are the ones that come back.
func TestPinRestore_SecondFlattenKeepsTheOriginal(t *testing.T) {
	t.Parallel()
	marker := haIdentity{Owner: aaDirector}.marker()
	set := &ha.RuleSet{Rules: []ha.Rule{{
		ID: naRuleNameFor(103), Type: ha.RuleTypeNodeAffinity, Affinity: ha.AffinityPositive,
		Comment:   marker + " " + ha.PinRestoreKey + "=" + restoreLabPin,
		Resources: []string{"vm:103"}, Nodes: map[string]int{"pve1": 5, "pve2": 0, "pve3": 0, "pve4": 0},
	}}}
	ops := flattenMemberPins(set, []string{"vm:103"}, aaDirector, []string{"pve1", "pve2", "pve3", "pve4"})
	if len(ops) != 1 || ops[0].Rule.MultiplePriorities() {
		t.Fatalf("ops = %+v; want one flatten", ops)
	}
	if nodes, _, ok := ha.PinRestoreRecord(ops[0].Rule.Comment); !ok || nodes["pve3"] != 2 || nodes["pve1"] != 0 {
		t.Fatalf("second flatten recorded %q; want the original %q", ops[0].Rule.Comment, restoreLabPin)
	}

	// A pin another Director owns, or one with no marker, records nothing.
	set.Rules[0].Comment = "bosh-cpi:v1 owner=dir-2"
	if ops := flattenMemberPins(set, []string{"vm:103"}, aaDirector, nil); len(ops) != 0 {
		t.Fatalf("another Director's pin was rewritten: %+v", ops)
	}
	set.Rules[0].Comment = ""
	ops = flattenMemberPins(set, []string{"vm:103"}, aaDirector, nil)
	if len(ops) != 1 || strings.Contains(ops[0].Rule.Comment, ha.PinRestoreKey) {
		t.Fatalf("unmarked pin flatten = %+v; want a flatten with no record", ops)
	}
}

// TestPinRestore_HandEditedPinKeepsItsPriorities pins that a pin an operator
// changed after the flatten keeps the operator's nodes and priorities when
// its member leaves, and only loses the record.
func TestPinRestore_HandEditedPinKeepsItsPriorities(t *testing.T) {
	t.Parallel()
	stub, deps, pin := restoreFixture(t)
	joinWeb(t, deps, 103, 3, log.NewNopLogger())
	stub.ruleNodes[pin] = "pve1,pve2:4,pve3,pve4"
	joinIndexZero(t, stub, deps, log.NewNopLogger())
	if got := stub.ruleNodes[pin]; got != "pve1,pve2:4,pve3,pve4" {
		t.Fatalf("hand-edited pin = %q; want the operator's priorities", got)
	}
	if rec, ok := pinRecord(stub, pin); ok {
		t.Fatalf("record %q stayed on a hand-edited pin", rec)
	}
}

// TestPinRestore_FailedRestoreWarnsAndTheFixRestores pins that a restore
// write PVE refuses only logs a warning, that the doctor's check then reports
// the pin, and that the fix writes the priorities back.
func TestPinRestore_FailedRestoreWarnsAndTheFixRestores(t *testing.T) {
	t.Parallel()
	stub, deps, pin := restoreFixture(t)
	joinWeb(t, deps, 103, 3, log.NewNopLogger())
	stub.failUpdateOf = func(rule string) error {
		if rule == pin {
			return errors.New("permission denied")
		}
		return nil
	}
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	joinIndexZero(t, stub, deps, logger)
	if rec, ok := pinRecord(stub, pin); !ok || rec != restoreLabPin {
		t.Fatalf("record after the failed restore = %q, %v", rec, ok)
	}
	warned := false
	for _, e := range obs.All() {
		warned = warned || e.Level == slog.LevelWarn && strings.Contains(e.Message, "could not restore")
	}
	if !warned {
		t.Fatalf("no warning for the failed restore in %+v", obs.All())
	}

	in, err := readAntiAffinityCheckInput(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	checks := CheckAntiAffinityGroups(in)
	if len(checks) != 1 || len(checks[0].PinRestores) != 1 {
		t.Fatalf("checks = %+v; want one pin to restore", checks)
	}
	want := AntiAffinityPinRestore{SID: "vm:103", Pin: pin, Action: "restore", Nodes: restoreLabPin}
	if got := checks[0].PinRestores[0]; got != want {
		t.Fatalf("pin restore = %+v; want %+v", got, want)
	}
	if len(checks[0].Drift) != 0 {
		t.Fatalf("drift = %v; the rules themselves are in their layout", checks[0].Drift)
	}

	stub.failUpdateOf = nil
	got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{DryRun: true})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityWouldFix ||
		len(got[0].Drift) != 1 || got[0].Drift[0] != "restore the priorities "+restoreLabPin+" on "+pin {
		t.Fatalf("dry run = %+v, %v", got, err)
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
		t.Fatalf("fix = %+v, %v", got, err)
	}
	if stub.ruleNodes[pin] != restoreLabPin {
		t.Fatalf("pin after the fix = %q; want %q", stub.ruleNodes[pin], restoreLabPin)
	}
	if rec, ok := pinRecord(stub, pin); ok {
		t.Fatalf("record %q stayed after the fix", rec)
	}
}

// TestPinRestore_LiveHoldDefersTheRestore pins that a group with a live hold
// leaves a member's pin flat, because the recreate the hold waits for may put
// the member back into a rule, and that the doctor reports the restore as
// deferred.
func TestPinRestore_LiveHoldDefersTheRestore(t *testing.T) {
	t.Parallel()
	stub, deps, pin := restoreFixture(t)
	joinWeb(t, deps, 103, 3, log.NewNopLogger())
	joinIndexZero(t, stub, deps, log.NewNopLogger())

	// Flatten the pin again by hand, as a failed restore would leave it.
	stub.ruleNodes[pin] = "pve1,pve2,pve3,pve4"
	stub.ruleComments[pin] = haIdentity{Owner: aaDirector}.marker() + " " + ha.PinRestoreKey + "=" + restoreLabPin

	in, err := readAntiAffinityCheckInput(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	set, err := readHARuleSet(context.Background(), deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	ident := haIdentity{Owner: aaDirector, Deployment: aaIdent("web").Deployment, Group: "web"}
	p := newAAPlanner(in.Config, in.Profile, ident, scanGroupMembers(in.Guests, in.GuestsComplete, ident),
		log.NewNopLogger())
	p.heal, p.managed = true, func() (map[string]string, error) { return in.Resources, nil }
	if _, err := p.plan(set); err != nil {
		t.Fatal(err)
	}
	if ops := p.restoreOps(set); len(ops) != 1 {
		t.Fatalf("restore ops without a hold = %+v; want one", ops)
	}
	p.holds = map[int]*aaGroupHold{1: {aaHold: aaHold{Index: 1, VMID: 99}, state: aaHoldLive}}
	if ops := p.restoreOps(set); len(ops) != 0 {
		t.Fatalf("restore ops under a live hold = %+v; want none", ops)
	}
	restores := p.pinRestores(set)
	if len(restores) != 1 || !restores[0].Deferred {
		t.Fatalf("pin restores under a live hold = %+v; want one deferred", restores)
	}
	before := stub.updateRuleCalls
	restoreGroupPins(context.Background(), deps, in.Profile, p, set, log.NewNopLogger())
	if stub.updateRuleCalls != before {
		t.Fatal("a restore was written under a live hold")
	}
}

// TestPinRestore_SettledJoinTakesTheLockToRestore pins that the lock-free
// fast path of set_vm_metadata does not skip a pin the locked join would
// restore, and that the locked join restores it.
func TestPinRestore_SettledJoinTakesTheLockToRestore(t *testing.T) {
	t.Parallel()
	stub, _, pin := restoreFixture(t)
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"),
		aaQEMU(103, aaTag+";index--3"))
	markWebMembers(stub, 100)
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	stub.ruleNodes[pin] = "pve1,pve2,pve3,pve4"
	stub.ruleComments[pin] = haIdentity{Owner: aaDirector}.marker() + " " + ha.PinRestoreKey + "=" + restoreLabPin
	events := []string{}
	joinAntiAffinityAtMetadata(context.Background(), lockedMetadataDeps(stub, &events), 101,
		map[string]any{"index": "1"}, log.NewNopLogger())
	if len(events) == 0 {
		t.Fatal("the join skipped the lock while a pin waited for its restore")
	}
	if stub.ruleNodes[pin] != restoreLabPin {
		t.Fatalf("pin = %q; want %q", stub.ruleNodes[pin], restoreLabPin)
	}
}

// TestPinRestore_MergedPinRecordsAndKeepsTheRecord pins the pin writer that
// builds a pin afresh. For a VM in a resource-affinity rule it records the
// priorities it would write outside the rule, and when that pin has nothing
// to record it keeps the record the pin had.
func TestPinRestore_MergedPinRecordsAndKeepsTheRecord(t *testing.T) {
	t.Parallel()
	negative := ha.AffinityNegative
	inRule := func() *naClusterStub {
		stub := newNAStub()
		stub.rules["bosh-aa-x"] = cluster.CreateHaRulesParams{Rule: "bosh-aa-x", Type: ha.RuleTypeResourceAffinity,
			Affinity: &negative, Resources: "vm:100,vm:101"}
		return stub
	}
	pin := naRuleNameFor(100)

	stub := inRule()
	req := pinRequest{Preferred: []string{"pve01", "pve02"}, Placed: "pve01", Storages: []string{testPinStorage}}
	if err := ensureMergedPin(context.Background(), naClusterDeps(stub, "pve01", "pve02", "pve03"), 100, req,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	r := stub.rules[pin]
	if got := ptrStr(r.Nodes); got != "pve01,pve02,pve03" {
		t.Fatalf("pin nodes = %q; want one priority", got)
	}
	if nodes, _, ok := ha.PinRestoreRecord(ptrStr(r.Comment)); !ok || nodes["pve01"] != 2 || nodes["pve02"] != 1 {
		t.Fatalf("pin comment = %q; want a record of pve01:2,pve02:1,pve03", ptrStr(r.Comment))
	}

	stub = inRule()
	nodes, strict := "pve01,pve02", true
	comment := haIdentity{Owner: aaDirector}.marker() + " " + ha.PinRestoreKey + "=pve01:2,pve02"
	stub.rules[pin] = cluster.CreateHaRulesParams{Rule: pin, Type: ha.RuleTypeNodeAffinity, Resources: "vm:100",
		Nodes: &nodes, Strict: &strict, Comment: &comment}
	req = pinRequest{Hard: []string{"pve01", "pve02"}, Storages: []string{testPinStorage}}
	if err := ensureMergedPin(context.Background(), naClusterDeps(stub, "pve01", "pve02", "pve03"), 100, req,
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	r = stub.rules[pin]
	if !strings.Contains(ptrStr(r.Comment), ha.PinRestoreKey+"=pve01:2,pve02") {
		t.Fatalf("rewritten pin comment = %q; want the record kept", ptrStr(r.Comment))
	}
}

// TestPinRestore_RebalanceComparesWithoutTheRecord pins that a rebalance does
// not count a pin's record as a change. A flat pin keeps the record it has,
// and a pin with the CPI's priorities is compared with the current pin
// without its record.
func TestPinRestore_RebalanceComparesWithoutTheRecord(t *testing.T) {
	t.Parallel()
	marker := haIdentity{Owner: aaDirector}.marker()
	recorded := marker + " " + ha.PinRestoreKey + "=pve1:2,pve2"
	cur := &ha.Rule{ID: "bosh-na-1", Type: ha.RuleTypeNodeAffinity, Comment: recorded, Resources: []string{"vm:1"},
		Nodes: map[string]int{"pve1": 0, "pve2": 0}}

	flat := cur.Clone()
	flat.Comment = marker + " " + ha.PinRestoreKey + "=pve2:2,pve1"
	if cmp := pinRestoreCompared(cur, &flat); len(ha.RuleDiff(&flat, cmp)) != 0 {
		t.Fatalf("flat pin diff = %v", ha.RuleDiff(&flat, cmp))
	}
	moved := cur.Clone()
	moved.Comment, moved.Nodes = marker, map[string]int{"pve1": 0, "pve2": 2}
	if diff := ha.RuleDiff(&moved, pinRestoreCompared(cur, &moved)); len(diff) != 1 || diff[0] != "nodes" {
		t.Fatalf("moved pin diff = %v; want only the nodes", diff)
	}
}

// TestPinRestore_OlderReadersIgnoreTheRecord stands in for a downgrade. The
// marker and ownership reads of a release that knows no record read a
// comment with one the same as without it.
func TestPinRestore_OlderReadersIgnoreTheRecord(t *testing.T) {
	t.Parallel()
	marker := haIdentity{Owner: aaDirector}.marker()
	recorded := marker + " " + ha.PinRestoreKey + "=" + restoreLabPin
	if haOwnershipOf(recorded, aaDirector) != haOwnershipOf(marker, aaDirector) {
		t.Fatal("the record changed the pin's ownership")
	}
	want, okWant := parseHaMarker(marker)
	got, okGot := parseHaMarker(recorded)
	if okGot != okWant || got.Owner != want.Owner || got.Deployment != want.Deployment || got.Group != want.Group {
		t.Fatalf("marker with a record = %+v, %v; want %+v, %v", got, okGot, want, okWant)
	}
}
