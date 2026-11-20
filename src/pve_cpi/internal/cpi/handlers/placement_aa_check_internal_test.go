package handlers

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// aaCheckFixture is a group of the "web" group's members, each with its
// index tag and an HA resource marked by the test Director.
type aaCheckFixture struct {
	in AntiAffinityCheckInput
}

func newAACheckFixture(t *testing.T, nodes int, pattern string, members ...int) *aaCheckFixture {
	t.Helper()
	names := make([]string, 0, nodes)
	for i := 1; i <= nodes; i++ {
		names = append(names, "pve"+strconv.Itoa(i))
	}
	profile, err := pve9ClusterProfile(names...)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &aaCheckFixture{in: AntiAffinityCheckInput{
		Config: aaMembershipConfig("off", false, 0), Profile: profile, Owner: aaDirector,
		GuestsComplete: true, Rules: &ha.RuleSet{}, Resources: map[string]string{},
		Now: time.Unix(1800000000, 0),
	}}
	for i, v := range members {
		f.in.Guests = append(f.in.Guests, pve.GuestRef{VMID: v, Tags: aaTag + ";index--" + strconv.Itoa(i)})
		f.in.Resources[haResourceSid(v)] = aaIdent("web").resourceMarker(pattern, 1700000000)
	}
	return f
}

func (f *aaCheckFixture) rule(name string, sids ...string) {
	f.in.Rules.Rules = append(f.in.Rules.Rules, ha.Rule{ID: name, Type: haRuleType, Affinity: haRuleAffinity,
		Resources: sids, Comment: aaIdent("web").marker()})
}

func oneCheck(t *testing.T, in AntiAffinityCheckInput) AntiAffinityGroupCheck {
	t.Helper()
	got := CheckAntiAffinityGroups(in)
	if len(got) != 1 {
		t.Fatalf("checks = %+v; want one group", got)
	}
	return got[0]
}

// TestCheckAntiAffinityGroups_ARuleOfNMembersIsFullAndDrifts pins that a
// rule an older CPI filled to the node count reads as full, and that the plan
// that takes it to N-1 members reads as drift.
func TestCheckAntiAffinityGroups_ARuleOfNMembersIsFullAndDrifts(t *testing.T) {
	t.Parallel()
	f := newAACheckFixture(t, 4, "", 100, 101, 102, 103)
	f.rule(aaRule, "vm:100", "vm:101", "vm:102", "vm:103")
	c := oneCheck(t, f.in)
	if c.Rule != aaRule || c.Group != "web" || c.Members != 4 || c.Pattern != "block" {
		t.Fatalf("check = %+v", c)
	}
	if len(c.Full) != 1 || c.Full[0].Rule != aaRule || c.Full[0].Nodes != 4 {
		t.Fatalf("full = %+v; want the rule of four members on four nodes", c.Full)
	}
	if len(c.Drift) == 0 || !strings.Contains(c.Drift[0], "update "+aaRule+" with vm:100,vm:101,vm:102") {
		t.Fatalf("drift = %v; want the rule cut to the three lowest indexes", c.Drift)
	}
}

// TestCheckAntiAffinityGroups_AGroupInItsLayoutReadsClean pins that a group
// whose rules hold the plan reports nothing.
func TestCheckAntiAffinityGroups_AGroupInItsLayoutReadsClean(t *testing.T) {
	t.Parallel()
	f := newAACheckFixture(t, 4, "", 100, 101, 102)
	f.rule(aaRule, "vm:100", "vm:101", "vm:102")
	c := oneCheck(t, f.in)
	if len(c.Drift) != 0 || len(c.Full) != 0 || len(c.Trimmed) != 0 || c.BlocksDrain || c.Problem != "" {
		t.Fatalf("check = %+v; want a clean group", c)
	}
	if got := c.Layout[aaRule]; !slices.Equal(got, []string{"vm:100", "vm:101", "vm:102"}) {
		t.Fatalf("layout = %v", c.Layout)
	}
}

// TestCheckAntiAffinityGroups_ReportsTrimmingAndUnsafePatterns pins the two
// pattern findings: a window of two over six members on three nodes is
// trimmed, and a pattern that gives up drain safety says so.
func TestCheckAntiAffinityGroups_ReportsTrimmingAndUnsafePatterns(t *testing.T) {
	t.Parallel()
	c := oneCheck(t, newAACheckFixture(t, 3, "window:2", 100, 101, 102, 103, 104, 105).in)
	if len(c.Trimmed) == 0 || c.BlocksDrain {
		t.Fatalf("check = %+v; want trimmed rules and drain safety kept", c)
	}
	c = oneCheck(t, newAACheckFixture(t, 3, "window:2!unsafe", 100, 101, 102, 103, 104, 105).in)
	if !c.BlocksDrain {
		t.Fatalf("check = %+v; want the pattern reported as blocking drains", c)
	}
}

// TestCheckAntiAffinityGroups_PlansAGroupWithItsOwnDefault pins that a
// default named for a group in GroupPatterns takes the place of the config's
// pve.ha.anti_affinity_pattern, and that a pattern a member's create named
// still wins over it. The check says which of the two the plan used.
func TestCheckAntiAffinityGroups_PlansAGroupWithItsOwnDefault(t *testing.T) {
	t.Parallel()
	window, err := aapattern.Parse("window:2")
	if err != nil {
		t.Fatal(err)
	}
	f := newAACheckFixture(t, 3, "", 100, 101, 102, 103, 104, 105)
	c := oneCheck(t, f.in)
	if c.Pattern != "block" || c.PatternRecorded {
		t.Fatalf("check = %+v; want the config's block pattern", c)
	}
	f.in.GroupPatterns = map[string]aapattern.Pattern{aaRule: window}
	c = oneCheck(t, f.in)
	if c.Pattern != "window:2" || c.PatternRecorded || len(c.Trimmed) == 0 {
		t.Fatalf("check = %+v; want the group's own window:2 default, trimmed", c)
	}
	f = newAACheckFixture(t, 3, "block", 100, 101, 102, 103, 104, 105)
	f.in.GroupPatterns = map[string]aapattern.Pattern{aaRule: window}
	c = oneCheck(t, f.in)
	if c.Pattern != "block" || !c.PatternRecorded {
		t.Fatalf("check = %+v; want the block pattern the members' creates named", c)
	}
}

// TestCheckAntiAffinityGroups_FindsGroupsByTheirMarkers pins that a group is
// found through its members' HA resources before it has a rule, that the
// owner filter leaves another Director's group out, and that a rule whose
// marker names a group under another group's name is not read as that group.
func TestCheckAntiAffinityGroups_FindsGroupsByTheirMarkers(t *testing.T) {
	t.Parallel()
	f := newAACheckFixture(t, 4, "", 100, 101)
	c := oneCheck(t, f.in)
	if len(c.Drift) != 1 || !strings.HasPrefix(c.Drift[0], "create "+aaRule) {
		t.Fatalf("drift = %v; want the group's rule created", c.Drift)
	}
	other := haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "api"}
	f.in.Rules.Rules = append(f.in.Rules.Rules,
		ha.Rule{ID: other.ruleName(), Type: haRuleType, Comment: other.marker(), Resources: []string{"vm:300", "vm:301"}},
		ha.Rule{ID: "bosh-aa-0123456789ab", Type: haRuleType, Comment: aaIdent("db").marker(),
			Resources: []string{"vm:400", "vm:401"}})
	if got := CheckAntiAffinityGroups(f.in); len(got) != 1 || got[0].Rule != aaRule {
		t.Fatalf("checks = %+v; want only the web group", got)
	}
	f.in.Owner = ""
	if got := CheckAntiAffinityGroups(f.in); len(got) != 2 {
		t.Fatalf("checks = %+v; want both marked groups with no owner filter", got)
	}
}

// TestFixAntiAffinityGroups_DryRunWritesNothingAndApplyWrites pins the fix:
// a dry run reports the group it would write, and the applied fix writes the
// planned layout.
func TestFixAntiAffinityGroups_DryRunWritesNothingAndApplyWrites(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"), aaQEMU(102, aaTag+";index--2"),
		aaQEMU(103, aaTag+";index--3"))
	markWebMembers(stub, 100, 101, 102, 103)
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102,vm:103")
	deps := aaMetadataDeps(stub)
	got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{DryRun: true})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityWouldFix {
		t.Fatalf("dry run = %+v, %v; want one group that would be fixed", got, err)
	}
	if stub.updateRuleCalls+stub.createRuleCalls+stub.deleteRuleCalls != 0 {
		t.Fatal("a dry run wrote a rule")
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
		t.Fatalf("fix = %+v, %v; want the group fixed", got, err)
	}
	if r := stub.rules[aaRule]; r != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want the three lowest indexes", r)
	}
	got, err = FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{Rule: aaRule})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityInLayout {
		t.Fatalf("second fix = %+v, %v; want the group in its layout", got, err)
	}
}

func TestFixAntiAffinityGroups_RefusesWithoutADirectorOrWithRulesOff(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	deps := aaMetadataDeps(stub)
	deps.RequestDirectorUUID = ""
	if _, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{}); err == nil {
		t.Fatal("a fix with no Director ran")
	}
	deps = aaMetadataDeps(stub)
	deps.Config.HA = haModeOff()
	if _, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{}); err == nil {
		t.Fatal("a fix with mode off ran")
	}
}
