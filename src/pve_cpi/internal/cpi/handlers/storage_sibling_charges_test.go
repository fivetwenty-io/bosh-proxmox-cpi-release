package handlers

import (
	"encoding/json"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

const (
	siblingTestGiB     = uint64(1) << 30
	siblingTestUpdated = "2026-09-15T12:00:00Z"
)

func siblingTestStamp(t *testing.T) time.Time {
	t.Helper()
	stamp, err := time.Parse(time.RFC3339, siblingTestUpdated)
	if err != nil {
		t.Fatalf("parse stamp: %v", err)
	}
	return stamp
}

// siblingTestRecord builds a journal record around an encoded plan without
// touching a journal, which keeps these tests to the accounting itself.
func siblingTestRecord(t *testing.T, id string, state aj.State, plan StorageAllocationPlan, steps ...aj.Step) aj.Record {
	t.Helper()
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return siblingTestRawRecord(t, id, state, 1, payload, steps...)
}

func siblingTestRawRecord(t *testing.T, id string, state aj.State, version int, payload []byte, steps ...aj.Step) aj.Record {
	t.Helper()
	stamp := siblingTestStamp(t)
	return aj.Record{
		Version:   aj.Version,
		ID:        id,
		Namespace: "director",
		Kind:      "vm",
		State:     state,
		Steps:     steps,
		Intent:    aj.Intent{PlanVersion: version, Plan: json.RawMessage(payload)},
		CreatedAt: stamp,
		UpdatedAt: stamp,
	}
}

func siblingTestPlan(charges ...inv.ChargeRecord) StorageAllocationPlan {
	return StorageAllocationPlan{Version: 1, Namespace: "director", Charges: charges}
}

func siblingTestCharge(id, capacityKey, domainKey string, bytes uint64) inv.ChargeRecord {
	charge := inv.Charge{ID: id, Role: storageRoleRoot, Node: "pve1", StorageID: capacityKey, Bytes: bytes}
	return inv.ChargeRecord{Charge: charge, CapacityKey: capacityKey, DomainKey: domainKey}
}

func siblingTestGroupPlan(group, capacityKey string) StorageAllocationPlan {
	plan := siblingTestPlan(siblingTestCharge("root", capacityKey, "", siblingTestGiB))
	plan.Group = group
	plan.Targets = []StoragePlanTarget{{Role: storageRoleRoot, Node: "pve1", StorageID: capacityKey, CapacityKey: capacityKey}}
	return plan
}

func siblingTestCharges(t *testing.T, records []aj.Record, group string, start time.Time) (map[string]uint64, map[string]uint64, map[string]int) {
	t.Helper()
	member, domain, counts, err := siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeInstanceGroup, start)
	if err != nil {
		t.Fatalf("sibling charges: %v", err)
	}
	return member, domain, counts
}

func assertSiblingBytes(t *testing.T, label string, actual map[string]uint64, expected map[string]uint64) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("%s holds %v, want %v", label, actual, expected)
	}
	for key, want := range expected {
		if actual[key] != want {
			t.Fatalf("%s[%s] is %d, want %d", label, key, actual[key], want)
		}
	}
}

// A planned record has chosen its targets before it writes a step, so its plan
// claim is the only evidence of what it holds.
func TestSiblingChargesPlannedRecordUsesPlanClaim(t *testing.T) {
	plan := siblingTestPlan(siblingTestCharge("root", "a", "dom", 3*siblingTestGiB), siblingTestCharge("ephemeral", "b", "", siblingTestGiB))
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Planned, plan)}
	member, domain, counts := siblingTestCharges(t, records, "", siblingTestStamp(t))
	assertSiblingBytes(t, "member", member, map[string]uint64{"a": 3 * siblingTestGiB, "b": siblingTestGiB})
	assertSiblingBytes(t, "domain", domain, map[string]uint64{"dom": 3 * siblingTestGiB})
	if counts != nil {
		t.Fatalf("counts is %v, want nil for an empty group", counts)
	}
}

// Once a step exists its charges restate the plan's claim, so the two sources
// combine by taking the larger per key and never by summing.
func TestSiblingChargesTakesLargerOfPlanAndSteps(t *testing.T) {
	plan := siblingTestPlan(siblingTestCharge("root", "a", "dom", 10*siblingTestGiB), siblingTestCharge("ephemeral", "b", "dom", siblingTestGiB))
	step := aj.Step{
		ID:    "attempt-0-step-0",
		Kind:  "clone",
		State: aj.Submitted,
		Charges: []aj.Charge{
			{Backing: "a", Domain: "dom", PlannedBytes: int64(4 * siblingTestGiB), OutstandingBytes: int64(4 * siblingTestGiB)},
			{Backing: "b", Domain: "dom", PlannedBytes: int64(6 * siblingTestGiB), OutstandingBytes: int64(6 * siblingTestGiB)},
		},
	}
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Submitted, plan, step)}
	member, domain, _ := siblingTestCharges(t, records, "", siblingTestStamp(t))
	// Member a keeps the plan's larger claim and member b takes the step's.
	assertSiblingBytes(t, "member", member, map[string]uint64{"a": 10 * siblingTestGiB, "b": 6 * siblingTestGiB})
	// The domain is one key, so the larger of the plan's 11 and the steps' 10.
	assertSiblingBytes(t, "domain", domain, map[string]uint64{"dom": 11 * siblingTestGiB})
}

// Charges are acquired one at a time, so a single step can hold one acquired
// charge beside one that is still outstanding and both have to count.
func TestSiblingChargesCountsPerChargeNotPerStep(t *testing.T) {
	step := aj.Step{
		ID:    "attempt-0-step-0",
		Kind:  "clone",
		State: aj.Observed,
		Charges: []aj.Charge{
			{Backing: "a", Domain: "dom", PlannedBytes: int64(2 * siblingTestGiB), AcquiredBytes: int64(2 * siblingTestGiB)},
			{Backing: "b", Domain: "dom", PlannedBytes: int64(5 * siblingTestGiB), OutstandingBytes: int64(5 * siblingTestGiB)},
		},
	}
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Observed, siblingTestPlan(), step)}
	member, domain, _ := siblingTestCharges(t, records, "", siblingTestStamp(t))
	assertSiblingBytes(t, "member", member, map[string]uint64{"a": 2 * siblingTestGiB, "b": 5 * siblingTestGiB})
	assertSiblingBytes(t, "domain", domain, map[string]uint64{"dom": 7 * siblingTestGiB})
}

// Only the in-flight states charge. In every resting state the volume is either
// already reported by PVE or gone, and our own record never charges us.
func TestSiblingChargesSkipsRestingStatesAndSelf(t *testing.T) {
	resting := []aj.State{aj.ReadyToReturn, aj.Adopted, aj.VMDeletedRetained, aj.Deleted, aj.Cleaned}
	plan := siblingTestPlan(siblingTestCharge("root", "a", "dom", 3*siblingTestGiB))
	step := aj.Step{ID: "attempt-0-step-0", Kind: "clone", State: aj.Observed, Charges: []aj.Charge{
		{Backing: "a", Domain: "dom", PlannedBytes: int64(siblingTestGiB), OutstandingBytes: int64(siblingTestGiB)},
	}}
	for _, state := range resting {
		records := []aj.Record{siblingTestRecord(t, "alloc-peer", state, plan, step)}
		member, domain, _ := siblingTestCharges(t, records, "", siblingTestStamp(t))
		assertSiblingBytes(t, string(state)+" member", member, map[string]uint64{})
		assertSiblingBytes(t, string(state)+" domain", domain, map[string]uint64{})
	}
	own := []aj.Record{siblingTestRecord(t, "alloc-self", aj.Planned, plan, step)}
	member, domain, _ := siblingTestCharges(t, own, "", siblingTestStamp(t))
	assertSiblingBytes(t, "self member", member, map[string]uint64{})
	assertSiblingBytes(t, "self domain", domain, map[string]uint64{})
}

// Acquired bytes count only while the record is newer than the snapshot start
// less the skew tolerance. The record clock has no seam, so the snapshot stamp
// is what moves here, exactly as it would across two directors.
func TestSiblingChargesAcquiredBytesFollowSnapshotStart(t *testing.T) {
	updated := siblingTestStamp(t)
	step := aj.Step{ID: "attempt-0-step-0", Kind: "clone", State: aj.Observed, Charges: []aj.Charge{
		{Backing: "a", Domain: "dom", PlannedBytes: int64(4 * siblingTestGiB), AcquiredBytes: int64(4 * siblingTestGiB)},
	}}
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Observed, siblingTestPlan(), step)}
	cases := []struct {
		name  string
		start time.Time
		want  uint64
	}{
		{name: "snapshot began after the record", start: updated.Add(-time.Minute), want: 4 * siblingTestGiB},
		{name: "snapshot began inside the tolerance", start: updated.Add(time.Minute), want: 4 * siblingTestGiB},
		{name: "snapshot began exactly one tolerance later", start: updated.Add(siblingAcquiredClockSkewTolerance), want: 0},
		{name: "snapshot began well after the record", start: updated.Add(time.Hour), want: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			member, domain, _ := siblingTestCharges(t, records, "", testCase.start)
			expected := map[string]uint64{}
			if testCase.want != 0 {
				expected["a"] = testCase.want
			}
			assertSiblingBytes(t, "member", member, expected)
			expectedDomain := map[string]uint64{}
			if testCase.want != 0 {
				expectedDomain["dom"] = testCase.want
			}
			assertSiblingBytes(t, "domain", domain, expectedDomain)
		})
	}
}

// A field a later release adds to the plan must not break an older binary's
// creates, so the sibling decode allows fields it does not know.
func TestSiblingChargesToleratesUnknownPlanFields(t *testing.T) {
	payload, err := json.Marshal(siblingTestPlan(siblingTestCharge("root", "a", "dom", 2*siblingTestGiB)))
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	object["FutureField"] = map[string]any{"nested": 1}
	extended, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal extended plan: %v", err)
	}
	records := []aj.Record{siblingTestRawRecord(t, "alloc-peer", aj.Planned, 1, extended)}
	member, domain, _ := siblingTestCharges(t, records, "", siblingTestStamp(t))
	assertSiblingBytes(t, "member", member, map[string]uint64{"a": 2 * siblingTestGiB})
	assertSiblingBytes(t, "domain", domain, map[string]uint64{"dom": 2 * siblingTestGiB})
}

// Evidence we cannot read is an error rather than a silently dropped claim,
// because a namespace we cannot read in full cannot be accounted for in full.
func TestSiblingChargesRejectsUnreadablePlanEvidence(t *testing.T) {
	valid, err := json.Marshal(siblingTestPlan(siblingTestCharge("root", "a", "dom", siblingTestGiB)))
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	cases := []struct {
		name    string
		version int
		payload []byte
	}{
		{name: "invalid json", version: 1, payload: []byte(`{"Version":1`)},
		{name: "trailing bytes", version: 1, payload: append(append([]byte{}, valid...), []byte(`{"Version":1}`)...)},
		{name: "unknown plan version", version: 2, payload: valid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			records := []aj.Record{siblingTestRawRecord(t, "alloc-peer", aj.Planned, testCase.version, testCase.payload)}
			if _, _, _, err := siblingCharges(records, "alloc-self", "", config.StorageAntiAffinityScopeInstanceGroup, siblingTestStamp(t)); err == nil {
				t.Fatal("unreadable plan evidence was accepted")
			}
		})
	}
}

// A resting record feeds only the count, so an unreadable one is skipped
// rather than failing every create, and the readable siblings still count.
func TestSiblingChargesSkipsUnreadableRestingPlanEvidence(t *testing.T) {
	group := "deployment--cf/instance-group--diego-cell"
	records := []aj.Record{
		siblingTestRawRecord(t, "alloc-broken", aj.ReadyToReturn, 2, []byte(`{"Version":2}`)),
		siblingTestRecord(t, "alloc-peer", aj.Adopted, siblingTestGroupPlan(group, "a")),
	}
	_, _, counts, err := siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeInstanceGroup, siblingTestStamp(t))
	if err != nil {
		t.Fatalf("unreadable resting evidence failed the read: %v", err)
	}
	if len(counts) != 1 || counts["a"] != 1 {
		t.Fatalf("counts is %v, want one on member a from the readable sibling", counts)
	}
}

// A terminal record feeds neither result, so we never decode it and its plan
// can no longer fail a create years after the allocation ended.
func TestSiblingChargesIgnoresTerminalPlanEvidence(t *testing.T) {
	for _, state := range []aj.State{aj.Deleted, aj.Cleaned} {
		records := []aj.Record{siblingTestRawRecord(t, "alloc-peer", state, 7, []byte(`{"Version":1`))}
		member, domain, counts := siblingTestCharges(t, records, "deployment--d/instance-group--g", siblingTestStamp(t))
		assertSiblingBytes(t, "member", member, map[string]uint64{})
		assertSiblingBytes(t, "domain", domain, map[string]uint64{})
		if len(counts) != 0 {
			t.Fatalf("counts is %v, want empty for a %s record", counts, state)
		}
	}
}

// The count keys on the root target's capacity key and covers every state but
// the terminal ones, because a sibling that finished still occupies its share.
func TestSiblingChargesGroupCounts(t *testing.T) {
	group := "deployment--cf/instance-group--diego-cell"
	records := []aj.Record{
		siblingTestRecord(t, "alloc-1", aj.ReadyToReturn, siblingTestGroupPlan(group, "a")),
		siblingTestRecord(t, "alloc-2", aj.Planned, siblingTestGroupPlan(group, "b")),
		siblingTestRecord(t, "alloc-3", aj.Adopted, siblingTestGroupPlan(group, "a")),
		siblingTestRecord(t, "alloc-4", aj.Planned, siblingTestGroupPlan("deployment--cf/instance-group--router", "a")),
		siblingTestRecord(t, "alloc-5", aj.Deleted, siblingTestGroupPlan(group, "a")),
		siblingTestRecord(t, "alloc-6", aj.Planned, siblingTestGroupPlan("", "a")),
		siblingTestRecord(t, "alloc-self", aj.Planned, siblingTestGroupPlan(group, "a")),
	}
	_, _, counts := siblingTestCharges(t, records, group, siblingTestStamp(t))
	if len(counts) != 2 || counts["a"] != 2 || counts["b"] != 1 {
		t.Fatalf("counts is %v, want two on member a and one on member b", counts)
	}
	_, _, empty := siblingTestCharges(t, records, "", siblingTestStamp(t))
	if empty != nil {
		t.Fatalf("counts is %v, want nil when the request carries no group", empty)
	}
}

// A record with no root target names no share, so it steers nothing.
func TestSiblingChargesSkipsRecordWithoutRootTarget(t *testing.T) {
	group := "deployment--cf/instance-group--diego-cell"
	plan := siblingTestGroupPlan(group, "a")
	plan.Targets = []StoragePlanTarget{{Role: "persistent", Node: "pve1", StorageID: "a", CapacityKey: "a"}}
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Planned, plan)}
	_, _, counts := siblingTestCharges(t, records, group, siblingTestStamp(t))
	if len(counts) != 0 {
		t.Fatalf("counts is %v, want empty without a root target", counts)
	}
}

// The predicate is the single definition of which states charge, and the
// journal audit command reads the same one.
func TestStorageAllocationCharging(t *testing.T) {
	charging := map[aj.State]bool{
		aj.Planned:                true,
		aj.Submitted:              true,
		aj.Observed:               true,
		aj.ReconciliationRequired: true,
		aj.ReadyToReturn:          false,
		aj.Adopted:                false,
		aj.VMDeletedRetained:      false,
		aj.Deleted:                false,
		aj.Cleaned:                false,
	}
	for state, want := range charging {
		if got := StorageAllocationCharging(state); got != want {
			t.Fatalf("state %s reports charging %t, want %t", state, got, want)
		}
	}
	if StorageAllocationCharging(aj.State("unknown")) {
		t.Fatal("an unknown state reports charging")
	}
}

// A negative charge is corrupt evidence rather than a credit against a share.
func TestSiblingChargesRejectsNegativeCharge(t *testing.T) {
	step := aj.Step{ID: "attempt-0-step-0", Kind: "clone", State: aj.Observed, Charges: []aj.Charge{
		{Backing: "a", PlannedBytes: -1, OutstandingBytes: -1},
	}}
	records := []aj.Record{siblingTestRecord(t, "alloc-peer", aj.Observed, siblingTestPlan(), step)}
	if _, _, _, err := siblingCharges(records, "alloc-self", "", config.StorageAntiAffinityScopeInstanceGroup, siblingTestStamp(t)); err == nil {
		t.Fatal("a negative charge was accepted")
	}
}

// The deployment scope cuts both sides of the comparison to the deployment
// half, so a sibling from another instance group of the same deployment
// counts, and the none scope counts nothing at all.
func TestSiblingChargesGroupCountsFollowScope(t *testing.T) {
	group := "deployment--cf/instance-group--diego-cell"
	records := []aj.Record{
		siblingTestRecord(t, "alloc-1", aj.Planned, siblingTestGroupPlan(group, "a")),
		siblingTestRecord(t, "alloc-2", aj.Planned, siblingTestGroupPlan("deployment--cf/instance-group--router", "a")),
		siblingTestRecord(t, "alloc-3", aj.Planned, siblingTestGroupPlan("deployment--other/instance-group--diego-cell", "b")),
		siblingTestRecord(t, "alloc-4", aj.Planned, siblingTestGroupPlan("", "b")),
	}
	_, _, _, err := siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeDeployment, siblingTestStamp(t))
	if err != nil {
		t.Fatalf("sibling charges: %v", err)
	}
	_, _, counts, _ := siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeDeployment, siblingTestStamp(t))
	if len(counts) != 1 || counts["a"] != 2 {
		t.Fatalf("deployment scope counts is %v, want two on member a", counts)
	}
	_, _, counts, _ = siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeInstanceGroup, siblingTestStamp(t))
	if len(counts) != 1 || counts["a"] != 1 {
		t.Fatalf("instance group scope counts is %v, want one on member a", counts)
	}
	_, _, counts, _ = siblingCharges(records, "alloc-self", group, config.StorageAntiAffinityScopeNone, siblingTestStamp(t))
	if counts != nil {
		t.Fatalf("none scope counts is %v, want nil", counts)
	}
}
