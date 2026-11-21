package handlers

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// aaDoctorDrift returns every write the doctor's plan of the stub's state
// would send, the way pve-ops ha reads it, and every group it could not plan.
func aaDoctorDrift(t *testing.T, deps Deps) []string {
	t.Helper()
	in, err := readAntiAffinityCheckInput(context.Background(), deps)
	if err != nil {
		t.Fatalf("read the check input: %v", err)
	}
	var drift []string
	for _, c := range CheckAntiAffinityGroups(in) {
		if c.Problem != "" {
			drift = append(drift, "problem: "+c.Problem)
		}
		drift = append(drift, c.Drift...)
	}
	return drift
}

// aaHoldFleet is the "web" group's guests as the stub lists them, each with its
// index tag. A destroy takes a guest out of the list, and its HA resource
// stays until the delete's cleanup purges it, the order delete_vm runs in.
type aaHoldFleet struct {
	stub  *aaClusterStub
	index map[int]int
}

func newAAHoldFleet(stub *aaClusterStub, vmids ...int) *aaHoldFleet {
	f := &aaHoldFleet{stub: stub, index: map[int]int{}}
	for _, v := range vmids {
		f.index[v] = v % 100
	}
	markWebMembers(stub, vmids...)
	f.list()
	return f
}

func (f *aaHoldFleet) list() {
	rows := make([]json.RawMessage, 0, len(f.index))
	for _, v := range slices.Sorted(maps.Keys(f.index)) {
		rows = append(rows, aaQEMU(v, aaTag+";index--"+strconv.Itoa(f.index[v])))
	}
	f.stub.listResourcesFn = aaResourcesFn(rows...)
}

func (f *aaHoldFleet) destroy(vmid int) {
	delete(f.index, vmid)
	f.list()
}

// add lists a recreated member with index and stages its HA resource.
func (f *aaHoldFleet) add(vmid, index int) {
	f.index[vmid] = index
	markWebMembers(f.stub, vmid)
	f.list()
}

// TestRemoveAntiAffinity_ConcurrentDeletesHoldEachOthersPlaces pins that two
// deletes in one group whose cleanups overlap keep both places free for the
// recreates, in either order. On four nodes a rule holds three members, so
// the layout is {0,1,2} and {3,4}. The second delete runs while the first one
// purges its HA resource, after the first delete's removal and before its plan
// of the group, so each plan runs after both removals and must see the other
// delete's hold. Index 3 never moves into index 1's place, and after every
// write the doctor's plan of the state writes nothing. The recreates then take
// both places back, and each consumes its hold. It runs with the rules lock
// on and off. The first delete purges its HA resource after it releases the
// lock of its removal and before it takes the lock again for its plan, so the
// nested second delete takes the lock for both of its steps in that gap, the
// way a concurrent delete on another worker would.
func TestRemoveAntiAffinity_ConcurrentDeletesHoldEachOthersPlaces(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		first, second int
		lock          bool
	}{
		"index 1 first":         {first: 101, second: 104},
		"index 4 first":         {first: 104, second: 101},
		"index 1 first, locked": {first: 101, second: 104, lock: true},
		"index 4 first, locked": {first: 104, second: 101, lock: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stub := newAAStub()
			stub.purgeFromRules = true
			fleet := newAAHoldFleet(stub, 100, 101, 102, 103, 104)
			stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			stub.seedRule(aaRule+"-2", "vm:103,vm:104")
			var locks []string
			pools := newAALockPools(&locks)
			deps := aaReplanDepsWithLocks(stub, tc.lock, pools)
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift before the deletes = %v; want the group in its layout", d)
			}
			writes := 0
			check := func(what string) {
				writes++
				if strings.Contains(stub.rules[aaRule], "vm:103") {
					t.Errorf("write %d (%s) moved index 3 into the first rule: %v", writes, what, stub.rules)
				}
				if d := aaDoctorDrift(t, deps); len(d) != 0 {
					t.Errorf("after write %d (%s) the doctor would write %v; rules %v, comments %v", writes, what,
						d, stub.rules, stub.ruleComments)
				}
			}
			stub.onRuleWrite = func(rule string) { check("rule " + rule) }
			stub.onResourceWrite = func(sid string) { check("resource " + sid) }
			remove := func(vmid int) {
				if err := removeAntiAffinityMembership(ctx, deps, vmid, webTags(vmid), log.NewNopLogger()); err != nil {
					t.Errorf("remove %d: %v", vmid, err)
				}
			}
			nested := false
			stub.onDeleteResource = func(sid string) {
				if sid != haResourceSid(tc.first) || nested {
					return
				}
				nested = true
				if len(pools.pools) != 0 {
					t.Errorf("the first delete purges its resource while it holds the lock %v", pools.pools)
				}
				taken := len(locks)
				fleet.destroy(tc.second)
				remove(tc.second)
				if tc.lock && aaLocksTaken(locks[taken:]) < 2 {
					t.Errorf("the second delete took the rules lock %d times; want its removal and its plan "+
						"each under the lock", aaLocksTaken(locks[taken:]))
				}
			}
			fleet.destroy(tc.first)
			remove(tc.first)
			if !nested {
				t.Fatal("the second delete never ran")
			}
			if want := map[string]string{aaRule: "vm:100,vm:102"}; !maps.Equal(stub.rules, want) {
				t.Fatalf("rules = %v after both deletes; want %v", stub.rules, want)
			}
			holds, _ := parseHaMarker(stub.ruleComments[aaRule])
			if !strings.Contains(stub.ruleComments[aaRule], "hold=1.101@") ||
				!strings.Contains(stub.ruleComments[aaRule], "4.104@") {
				t.Fatalf("comment = %q (%+v); want the holds of both deleted indexes", stub.ruleComments[aaRule], holds)
			}

			for _, r := range []struct{ vmid, index int }{{105, 1}, {106, 4}} {
				fleet.add(r.vmid, r.index)
				joinAntiAffinityAtMetadata(ctx, deps, r.vmid, map[string]any{"index": float64(r.index)},
					log.NewNopLogger())
			}
			want := map[string]string{aaRule: "vm:100,vm:102,vm:105", aaRule + "-2": "vm:103,vm:106"}
			if !maps.Equal(stub.rules, want) {
				t.Fatalf("rules = %v after the recreates; want %v", stub.rules, want)
			}
			for name, c := range stub.ruleComments {
				if c != aaRuleMarker() {
					t.Errorf("comment of %s = %q; want the plain marker once both holds are consumed", name, c)
				}
			}
		})
	}
}

// aaLocksTaken counts the lock pools events record as created.
func aaLocksTaken(events []string) int {
	n := 0
	for _, ev := range events {
		if strings.HasPrefix(ev, "lock-create:") {
			n++
		}
	}
	return n
}

// aaHoldFixture is the check of a group whose rule holds indexes 0 and 2 on
// four nodes, with index 3 outside it and index 1 deleted. The rule's comment
// carries the given extra marker fields.
func aaHoldFixture(t *testing.T, fields string) AntiAffinityCheckInput {
	t.Helper()
	f := newAACheckFixture(t, 4, "")
	for _, m := range []struct{ vmid, index int }{{100, 0}, {102, 2}, {103, 3}} {
		f.in.Guests = append(f.in.Guests, pve.GuestRef{VMID: m.vmid, Tags: aaTag + ";index--" + strconv.Itoa(m.index)})
		f.in.Resources[haResourceSid(m.vmid)] = aaIdent("web").resourceMarker("", 1700000000)
	}
	f.in.Rules.Rules = append(f.in.Rules.Rules, ha.Rule{ID: aaRule, Type: haRuleType, Affinity: haRuleAffinity,
		Resources: []string{"vm:100", "vm:102"}, Comment: aaRuleMarker() + fields})
	return f.in
}

// TestCheckAntiAffinityGroups_ALiveHoldKeepsItsPlace pins that the doctor
// plans with a recorded hold, so the place a delete left for the recreate of
// its index reads as occupied and no drift is reported. Once the hold lapses,
// index 3 may take the place, and the doctor reports that write as drift.
func TestCheckAntiAffinityGroups_ALiveHoldKeepsItsPlace(t *testing.T) {
	t.Parallel()
	now := time.Unix(1800000000, 0)
	for name, tc := range map[string]struct {
		until int64
		drift bool
	}{
		"live":   {until: now.Unix() + 600},
		"lapsed": {until: now.Unix(), drift: true},
	} {
		in := aaHoldFixture(t, " hold=1.101@"+strconv.FormatInt(tc.until, 10))
		in.Now = now
		c := oneCheck(t, in)
		switch {
		case !tc.drift && len(c.Drift) != 0:
			t.Errorf("%s: drift = %v; want none while the hold keeps index 1's place", name, c.Drift)
		case tc.drift && (len(c.Drift) == 0 || !strings.Contains(c.Drift[0], "vm:100,vm:102,vm:103")):
			t.Errorf("%s: drift = %v; want index 3 moved into the place once the hold lapsed", name, c.Drift)
		}
	}
}

// TestParseHaMarker_ReadsTheStandInFormat pins that the marker 0.11.1 writes
// on a rule and a resource, which records no pattern and no birth, still
// parses, plain and as PVE encoded it again.
func TestParseHaMarker_ReadsTheStandInFormat(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{
		"bosh-cpi:v1 owner=" + aaDirector + " deployment=" + aaDeployment + " group=web",
		"bosh-cpi%3Av1 owner=" + aaDirector + " deployment=" + aaDeployment + " group=web",
	} {
		m, ok := parseHaMarker(comment)
		if !ok || m.Owner != aaDirector || m.Deployment != aaDeployment || m.Group != "web" || m.AA != "" ||
			m.Born != 0 || len(m.Left) != 0 {
			t.Errorf("parse %q = %+v, %v; want the stand-in marker", comment, m, ok)
		}
	}
}

// TestRemoveAntiAffinity_TheLastRuleLeavesItsHoldOnAMember pins that a delete
// that leaves its group no rule records its hold on the HA resource of the
// member that is left, with the rules lock on and off. The recreate of the
// index then joins that member in a new rule, and the plan drops the hold
// from the resource once the rule holds the recreate.
func TestRemoveAntiAffinity_TheLastRuleLeavesItsHoldOnAMember(t *testing.T) {
	t.Parallel()
	for _, lock := range []bool{false, true} {
		t.Run("lock "+strconv.FormatBool(lock), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stub := newAAStub()
			stub.purgeFromRules = true
			fleet := newAAHoldFleet(stub, 100, 101)
			stub.seedRule(aaRule, "vm:100,vm:101")
			deps := aaReplanDeps(stub, lock)
			plain := stub.resComments["vm:100"]
			fleet.destroy(101)
			if err := removeAntiAffinityMembership(ctx, deps, 101, webTags(101), log.NewNopLogger()); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if len(stub.rules) != 0 {
				t.Fatalf("rules = %v; want none once one member is left", stub.rules)
			}
			if c := stub.resComments["vm:100"]; !strings.Contains(c, " hold=1.101@") {
				t.Fatalf("comment of vm:100 = %q; want the hold of index 1", c)
			}
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("drift after the delete = %v; want none", d)
			}
			fleet.add(102, 1)
			joinAntiAffinityAtMetadata(ctx, deps, 102, map[string]any{"index": float64(1)}, log.NewNopLogger())
			if want := map[string]string{aaRule: "vm:100,vm:102"}; !maps.Equal(stub.rules, want) {
				t.Fatalf("rules = %v after the recreate; want %v", stub.rules, want)
			}
			if c := stub.resComments["vm:100"]; c != plain {
				t.Errorf("comment of vm:100 = %q after the recreate; want %q", c, plain)
			}
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Errorf("drift after the recreate = %v; want none", d)
			}
		})
	}
}

// TestRemoveAntiAffinity_TheHoldLandsBeforeTheRuleGoes pins that a delete
// that leaves its rule with one member records its hold before it deletes the
// rule, so no state between two writes leaves the deleted member's place
// recorded nowhere. On three nodes a rule holds two members. With a second
// rule left in the group the hold goes on that rule, and with none left it
// goes on the HA resource of the member that stays. After every write the
// doctor's plan writes nothing, with the rules lock on and off.
func TestRemoveAntiAffinity_TheHoldLandsBeforeTheRuleGoes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		vmids   []int
		rules   map[string]string
		carrier string
	}{
		"another rule": {[]int{100, 101, 102, 103}, map[string]string{aaRule: "vm:100,vm:101", aaRule + "-2": "vm:102,vm:103"},
			"rule " + aaRule + "-2"},
		"a member's resource": {[]int{100, 101}, map[string]string{aaRule: "vm:100,vm:101"}, "resource vm:100"},
	} {
		for _, lock := range []bool{false, true} {
			t.Run(name+" lock "+strconv.FormatBool(lock), func(t *testing.T) {
				t.Parallel()
				stub := newAAStub()
				stub.purgeFromRules = true
				fleet := newAAHoldFleet(stub, tc.vmids...)
				for rule, members := range tc.rules {
					stub.seedRule(rule, members)
				}
				deps := aaReplanDeps(stub, lock)
				deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
				writes := 0
				var carriers []string
				check := func(what string) {
					writes++
					carriers = aaHoldCarriers(stub, 1)
					inRule := slices.ContainsFunc(slices.Collect(maps.Values(stub.rules)), func(csv string) bool {
						return slices.Contains(strings.Split(csv, ","), "vm:101")
					})
					if !inRule && len(carriers) == 0 {
						t.Errorf("after write %d (%s) vm:101 is out of its rule and no rule or resource records "+
							"its hold; rules %v, comments %v, resources %v", writes, what, stub.rules,
							stub.ruleComments, stub.resComments)
					}
					if d := aaDoctorDrift(t, deps); len(d) != 0 {
						t.Errorf("after write %d (%s) the doctor would write %v; rules %v", writes, what, d, stub.rules)
					}
				}
				stub.onRuleWrite = func(rule string) { check("rule " + rule) }
				stub.onResourceWrite = func(sid string) { check("resource " + sid) }
				fleet.destroy(101)
				if err := removeAntiAffinityMembership(context.Background(), deps, 101, webTags(101),
					log.NewNopLogger()); err != nil {
					t.Fatalf("remove: %v", err)
				}
				if _, ok := stub.rules[aaRule]; ok {
					t.Fatalf("rules = %v; want the rule of one member gone", stub.rules)
				}
				if !slices.Equal(carriers, []string{tc.carrier}) {
					t.Fatalf("the hold of index 1 is recorded on %v; want %s", carriers, tc.carrier)
				}
			})
		}
	}
}

// aaHoldCarriers names each rule and resource of the stub that records a hold
// of index, as "rule <id>" or "resource <sid>", in order.
func aaHoldCarriers(stub *aaClusterStub, index int) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(stub.ruleComments)) {
		if recordsIndex(holdsOf(stub.ruleComments[id]), index) {
			out = append(out, "rule "+id)
		}
	}
	for _, sid := range slices.Sorted(maps.Keys(stub.resComments)) {
		if stub.resources[sid] && recordsIndex(holdsOf(stub.resComments[sid]), index) {
			out = append(out, "resource "+sid)
		}
	}
	return out
}

// TestCheckAntiAffinityGroups_ListsTheHolds pins what the check reports of
// each recorded hold: a live one with the rule that records it, and as lapsed
// one that expired and one whose index a member holds.
func TestCheckAntiAffinityGroups_ListsTheHolds(t *testing.T) {
	t.Parallel()
	now := time.Unix(1800000000, 0)
	in := aaHoldFixture(t, " hold=1.101@1800000600,2.199@1800000600,5.105@1700000000")
	in.Now = now
	c := oneCheck(t, in)
	if len(c.Drift) != 0 || c.Members != 3 {
		t.Fatalf("drift = %v, members = %d; want no drift and three members", c.Drift, c.Members)
	}
	want := []AntiAffinityHold{{Index: 1, VMID: 101, Until: time.Unix(1800000600, 0).UTC(), Carriers: []string{aaRule}}}
	if !slices.EqualFunc(c.Holds, want, sameCheckHold) {
		t.Errorf("holds = %+v; want %+v", c.Holds, want)
	}
	lapsed := []AntiAffinityHold{
		{Index: 2, VMID: 199, Until: time.Unix(1800000600, 0).UTC(), Consumed: true, Carriers: []string{aaRule}},
		{Index: 5, VMID: 105, Until: time.Unix(1700000000, 0).UTC(), Carriers: []string{aaRule}},
	}
	if !slices.EqualFunc(c.LapsedHolds, lapsed, sameCheckHold) {
		t.Errorf("lapsed holds = %+v; want %+v", c.LapsedHolds, lapsed)
	}
}

func sameCheckHold(a, b AntiAffinityHold) bool {
	return a.Index == b.Index && a.VMID == b.VMID && a.Until.Equal(b.Until) && a.Consumed == b.Consumed &&
		slices.Equal(a.Carriers, b.Carriers)
}

// TestCheckAntiAffinityGroups_ResourceHoldKeepsItsPlace pins that the doctor
// reads a hold that a member's HA resource records the way it reads one on a
// rule.
func TestCheckAntiAffinityGroups_ResourceHoldKeepsItsPlace(t *testing.T) {
	t.Parallel()
	in := aaHoldFixture(t, "")
	in.Now = time.Unix(1800000000, 0)
	in.Resources["vm:100"] = withHolds(in.Resources["vm:100"], []aaHold{{Index: 1, VMID: 101, Until: 1800000600}})
	if c := oneCheck(t, in); len(c.Drift) != 0 || len(c.Holds) != 1 || !slices.Equal(c.Holds[0].Carriers,
		[]string{"vm:100"}) {
		t.Errorf("check = drift %v, holds %+v; want no drift and the hold on vm:100", c.Drift, c.Holds)
	}
}

// TestParseHaMarker_ReadsTheHolds pins the hold= field on a rule and on a
// resource, plain and as PVE encoded it again, with the form that names no
// VMID, and that a part that does not parse is skipped.
func TestParseHaMarker_ReadsTheHolds(t *testing.T) {
	t.Parallel()
	base := "bosh-cpi:v1 owner=" + aaDirector + " deployment=" + aaDeployment + " group=web"
	want := []aaHold{{Index: 1, VMID: 101, Until: 2000}, {Index: 4, Until: 3000}}
	for _, comment := range []string{
		base + " aa=block hold=1.101@2000,4@3000 left=101",
		strings.Replace(base, ":", "%3A", 1) + " aa=block hold=1.101@2000,4@3000",
		base + " born=1700000000 hold=1.101@2000,4@3000 bosh-failback-hold=9",
		base + " hold=1.101@2000,x@1,1.0101@5,01@5,1.-3@5,2.7@,2.7@-1,4@3000,3.8",
	} {
		m, ok := parseHaMarker(comment)
		if !ok || !slices.Equal(m.Holds, want) || m.Group != "web" {
			t.Errorf("parse %q = %+v, %v; want holds %+v", comment, m.Holds, ok, want)
		}
	}
	if m, _ := parseHaMarker(base + " aa=block"); len(m.Holds) != 0 {
		t.Errorf("holds of a marker without the field = %+v; want none", m.Holds)
	}
}

// TestWithHolds_KeepsEveryOtherField pins where withHolds writes the field:
// in the place of the first one, before left= when there is none, and last
// when there is neither, with no field when no hold is left.
func TestWithHolds_KeepsEveryOtherField(t *testing.T) {
	t.Parallel()
	m := "bosh-cpi:v1 owner=o deployment=d group=web"
	holds := []aaHold{{Index: 4, VMID: 104, Until: 9}, {Index: 1, VMID: 101, Until: 8}}
	for _, tc := range []struct{ in, want string }{
		{m + " aa=block", m + " aa=block hold=1.101@8,4.104@9"},
		{m + " aa=block left=101", m + " aa=block hold=1.101@8,4.104@9 left=101"},
		{m + " hold=2.1@1 aa=block hold=3.1@1", m + " hold=1.101@8,4.104@9 aa=block"},
		{m + " born=1 bosh-dlb=1", m + " born=1 bosh-dlb=1 hold=1.101@8,4.104@9"},
		{"bosh-cpi%3Av1 owner=o deployment=d group=web", m + " hold=1.101@8,4.104@9"},
		{"someone else's comment", "someone else's comment"},
	} {
		if got := withHolds(tc.in, holds); got != tc.want {
			t.Errorf("withHolds(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := withHolds(m+" aa=block hold=1.101@8 left=101", nil); got != m+" aa=block left=101" {
		t.Errorf("withHolds with none = %q; want the field gone", got)
	}
	if got := withPatternStamp(m+" hold=1.101@8 left=101 aa=window", "block"); got !=
		m+" aa=block hold=1.101@8 left=101" {
		t.Errorf("withPatternStamp = %q; want the pattern, then the holds, then left=", got)
	}
}

// TestFixAntiAffinityGroups_DropsALapsedHold pins that the fix treats a
// lapsed hold the rules still record as work: a dry run names it, and the
// applied fix writes the rule without it and changes nothing else.
func TestFixAntiAffinityGroups_DropsALapsedHold(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"))
	markWebMembers(stub, 100, 101)
	stub.seedRuleWithComment(aaRule, "vm:100,vm:101", aaRuleMarker()+" hold=4.104@1700000000")
	deps := aaMetadataDeps(stub)
	got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{DryRun: true})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityWouldFix ||
		!slices.Contains(got[0].Drift, "drop the lapsed hold of index 4 from "+aaRule) {
		t.Fatalf("dry run = %+v, %v; want the lapsed hold named", got, err)
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
		t.Fatalf("fix = %+v, %v; want the group fixed", got, err)
	}
	if r, c := stub.rules[aaRule], stub.ruleComments[aaRule]; r != "vm:100,vm:101" || c != aaRuleMarker() {
		t.Fatalf("rule = %q with %q; want the members kept and the hold gone", r, c)
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityInLayout {
		t.Fatalf("second fix = %+v, %v; want the group in its layout", got, err)
	}
}
