package handlers

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// numberingMember is a member of the group in a numbering fixture, by its
// VMID and its BOSH instance index.
type numberingMember struct {
	vmid, index int
}

// numberingRule is a rule of the group in a numbering fixture. suffix is ""
// for the base rule and "-<n>" otherwise, and holds is the value of the hold=
// field its comment records, or "".
type numberingRule struct {
	suffix string
	vmids  []int
	holds  string
}

// lab9Client is the placement.az_map of the az2-client entry, which maps
// each node of the shared cluster to an AZ of its own.
var lab9Client = map[string][]string{
	"p9c0": {"lab-pve-cpi-az2-0"}, "p9c1": {"lab-pve-cpi-az2-1"}, "p9c2": {"lab-pve-cpi-az2-2"},
}

// numberingConfig is sharedClusterConfig with the VM ID band both lab entries
// use, so every member of a numbering fixture lies in the judging entry's band.
func numberingConfig(azMap map[string][]string) *config.CPIConfig {
	cfg := sharedClusterConfig(azMap)
	cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 210000, 210199
	return cfg
}

// numberingInput builds the shared three-node cluster with one group of the
// Director, whose members carry soft pins over every node and whose rules are
// rules. cfg is the config of the entry that judges the state, and now is
// when the check runs.
func numberingInput(t *testing.T, cfg *config.CPIConfig, ident haIdentity, members []numberingMember,
	rules []numberingRule, now int64,
) AntiAffinityCheckInput {
	t.Helper()
	profile, err := pve9ClusterProfile(sharedClusterNodes...)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in := AntiAffinityCheckInput{
		Config: cfg, Profile: profile, Owner: sharedClusterOwner, GuestsComplete: true,
		Rules: &ha.RuleSet{}, Resources: map[string]string{}, Now: time.Unix(now, 0),
	}
	pinComment := "bosh-cpi:v1 owner=" + sharedClusterOwner + " deployment=- group=- bosh-vols=nfs-ephemeral-3"
	for i, m := range members {
		sid := haResourceSid(m.vmid)
		in.Guests = append(in.Guests, pve.GuestRef{VMID: m.vmid, Node: sharedClusterNodes[i%3],
			Tags: ident.ruleName() + ";index--" + strconv.Itoa(m.index)})
		in.Resources[sid] = ident.resourceMarker("block", now-3600+int64(i))
		pin := ha.Rule{ID: naRuleNameFor(m.vmid), Type: ha.RuleTypeNodeAffinity, Resources: []string{sid},
			Nodes: map[string]int{}, Comment: pinComment}
		for _, n := range sharedClusterNodes {
			pin.Nodes[n] = 1
		}
		in.Rules.Rules = append(in.Rules.Rules, pin)
	}
	for _, r := range rules {
		comment := withPatternStamp(ident.marker(), "block")
		if r.holds != "" {
			comment += " hold=" + r.holds
		}
		sids := make([]string, 0, len(r.vmids))
		for _, v := range r.vmids {
			sids = append(sids, haResourceSid(v))
		}
		in.Rules.Rules = append(in.Rules.Rules, ha.Rule{ID: ident.ruleName() + r.suffix, Type: haRuleType,
			Affinity: haRuleAffinity, Resources: sids, Comment: comment})
	}
	return in
}

// checkOne runs the check of in and returns its one group.
func checkOne(t *testing.T, in AntiAffinityCheckInput) AntiAffinityGroupCheck {
	t.Helper()
	checks := CheckAntiAffinityGroups(in)
	if len(checks) != 1 {
		t.Fatalf("checks = %+v; want one group", checks)
	}
	return checks[0]
}

// gpIdent is the group that the lab's az2-client entry placed for deployment
// gp.
var gpIdent = haIdentity{Owner: sharedClusterOwner, Deployment: "gp", Group: "g"}

// gpMembers are the members of gp after its scale-down took index 1, 210166,
// away and left a hold for it until 1795323299.
var gpMembers = []numberingMember{{210025, 0}, {210142, 2}, {210118, 3}}

// gpRules are gp's rules after the scale-down. The CPI removed the base rule,
// which held only 210025, and recorded the hold on the comment of the rule
// that az2-client's three AZs number 4.
var gpRules = []numberingRule{{"-4", []int{210118, 210142}, "1.210166@1795323299"}}

// TestCheckAntiAffinityGroups_AnEntryWithNoAZMapRenamesNoRuleOfAnAZMapEntry
// pins the lab's shape. The az2-client entry, whose az_map has three AZs,
// numbers the cluster's block rules 1, 4, and 7, so gp's second pair sits in
// rule -4. The az2 entry has no az_map and numbers them 1, 2, and 3, so a plan
// of gp under az2 alone would rename -4 to -2. Under az2 the check reports
// the group as not planned and names -4, so neither the doctor's drift nor
// the fix renames a rule that az2-client keeps, while az2-client finds the
// group in its layout.
func TestCheckAntiAffinityGroups_AnEntryWithNoAZMapRenamesNoRuleOfAnAZMapEntry(t *testing.T) {
	t.Parallel()
	const live = 1795323000
	client := checkOne(t, numberingInput(t, numberingConfig(lab9Client), gpIdent, gpMembers, gpRules, live))
	if client.Problem != "" || client.ForeignRule != "" || len(planDeletesOf(client)) != 0 {
		t.Fatalf("az2-client's check = %+v; want gp in its layout", client)
	}

	az2 := checkOne(t, numberingInput(t, numberingConfig(nil), gpIdent, gpMembers, gpRules, live))
	if az2.Problem != "" || !strings.Contains(az2.ForeignRule, gpIdent.ruleName()+"-4") {
		t.Fatalf("az2's check = %+v; want a foreign rule that names %s-4", az2, gpIdent.ruleName())
	}
}

// TestCheckAntiAffinityGroups_AnEntryWithNoAZMapLeavesALapsedAZMapGroupAlone
// pins gp once its hold lapsed. az2-client's own plan then regroups the
// group, which is drift for the entry that placed it. A plan under az2 would
// delete rule -4 too, but -4 holds indexes 2 and 3, and az2 gives that name
// only to a rule of indexes 6 and up, so az2 still reports the group as not
// planned.
func TestCheckAntiAffinityGroups_AnEntryWithNoAZMapLeavesALapsedAZMapGroupAlone(t *testing.T) {
	t.Parallel()
	const lapsed = 1795323400
	client := checkOne(t, numberingInput(t, numberingConfig(lab9Client), gpIdent, gpMembers, gpRules, lapsed))
	if client.Problem != "" || client.ForeignRule != "" || len(client.Drift) == 0 {
		t.Fatalf("az2-client's check = %+v; want drift it fixes itself", client)
	}

	az2 := checkOne(t, numberingInput(t, numberingConfig(nil), gpIdent, gpMembers, gpRules, lapsed))
	if !strings.Contains(az2.ForeignRule, gpIdent.ruleName()+"-4") || !strings.Contains(az2.ForeignRule, "index 2") {
		t.Fatalf("az2's check = %+v; want a foreign rule that names -4 and index 2", az2)
	}
}

// TestCheckAntiAffinityGroups_AnAZMapEntryRenamesNoRuleOfAnEntryWithNone pins
// the reverse direction, which the remainder check misses. The az2 entry
// placed the group over eight positions, of which a hold keeps 2 to 5, so its
// rules are the base rule and -4. az2-client numbers the fourth pair 10, and
// 4 is a number its three AZs give the cluster's domain, but its plan would
// rename -4 to -10 with the same members, which az2's numbering explains.
func TestCheckAntiAffinityGroups_AnAZMapEntryRenamesNoRuleOfAnEntryWithNone(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: sharedClusterOwner, Deployment: "rv", Group: "r"}
	members := []numberingMember{{210010, 0}, {210011, 1}, {210016, 6}, {210017, 7}}
	rules := []numberingRule{
		{"", []int{210010, 210011}, "2.210012@1795330000,3.210013@1795330000,4.210014@1795330000," +
			"5.210015@1795330000"},
		{"-4", []int{210016, 210017}, ""},
	}
	const now = 1795320000
	az2 := checkOne(t, numberingInput(t, numberingConfig(nil), ident, members, rules, now))
	if az2.Problem != "" || az2.ForeignRule != "" || len(planDeletesOf(az2)) != 0 {
		t.Fatalf("az2's check = %+v; want the group in its layout", az2)
	}

	client := checkOne(t, numberingInput(t, numberingConfig(lab9Client), ident, members, rules, now))
	if !strings.Contains(client.ForeignRule, ident.ruleName()+"-4") {
		t.Fatalf("az2-client's check = %+v; want a foreign rule that names %s-4", client, ident.ruleName())
	}
}

// TestCheckAntiAffinityGroups_AnEntryStillFixesItsOwnDrift pins that real
// drift of a group the entry placed keeps its writes. A member missing from
// its rule, a regroup after a hold lapsed below a renamed rule, and a regroup
// that renames two rules by a shift no other numbering gives all stay drift,
// with no foreign rule, in an entry with no az_map.
func TestCheckAntiAffinityGroups_AnEntryStillFixesItsOwnDrift(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: sharedClusterOwner, Deployment: "own", Group: "o"}
	const now = 1795320000
	cases := map[string]struct {
		members []numberingMember
		rules   []numberingRule
		want    string
	}{
		"a member missing from its rule": {
			members: []numberingMember{{210020, 0}, {210021, 1}, {210022, 2}, {210023, 3}},
			rules:   []numberingRule{{"", []int{210020, 210021}, ""}, {"-2", []int{210022}, ""}},
			want:    "update " + ident.ruleName() + "-2",
		},
		"a regroup after a lapsed hold": {
			members: []numberingMember{{210020, 0}, {210021, 1}, {210024, 4}, {210025, 5}},
			rules: []numberingRule{
				{"", []int{210020, 210021}, "2.210022@1795310000,3.210023@1795310000"},
				{"-3", []int{210024, 210025}, ""},
			},
			want: "delete " + ident.ruleName() + "-3",
		},
		"a regroup no other numbering explains": {
			members: []numberingMember{{210020, 0}, {210021, 1}, {210024, 4}, {210025, 5}, {210026, 6},
				{210027, 7}},
			rules: []numberingRule{
				{"", []int{210020, 210021}, ""}, {"-3", []int{210024, 210025}, ""},
				{"-4", []int{210026, 210027}, ""},
			},
			want: "delete " + ident.ruleName() + "-4",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := checkOne(t, numberingInput(t, numberingConfig(nil), ident, tc.members, tc.rules, now))
			if c.Problem != "" || c.ForeignRule != "" {
				t.Fatalf("check = %+v; want drift this entry fixes", c)
			}
			if !strings.Contains(strings.Join(c.Drift, "; "), tc.want) {
				t.Fatalf("drift = %q; want %q", c.Drift, tc.want)
			}
		})
	}
}

// TestCheckAntiAffinityGroups_LeavesAGroupOutsideTheEntrysBandAlone pins that
// an entry leaves a group to another entry when its plan would rewrite a rule
// and no member lies in the entry's VM ID band, because the entry created
// none of them. The same drift is the entry's to fix once a member lies in
// its band.
func TestCheckAntiAffinityGroups_LeavesAGroupOutsideTheEntrysBandAlone(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: sharedClusterOwner, Deployment: "own", Group: "o"}
	members := []numberingMember{{210020, 0}, {210021, 1}, {210022, 2}, {210023, 3}}
	rules := []numberingRule{{"", []int{210020, 210021}, ""}, {"-2", []int{210022}, ""}}
	cfg := numberingConfig(nil)
	cfg.VMIDRangeStart, cfg.VMIDRangeEnd = 220000, 220199
	c := checkOne(t, numberingInput(t, cfg, ident, members, rules, 1795320000))
	if !strings.Contains(c.ForeignRule, "VM ID band 220000-220199") ||
		!strings.Contains(c.ForeignRule, ident.ruleName()+"-2") {
		t.Fatalf("check = %+v; want a foreign rule that names the band and %s-2", c, ident.ruleName())
	}

	cfg.VMIDRangeStart = 210023
	c = checkOne(t, numberingInput(t, cfg, ident, members, rules, 1795320000))
	if c.ForeignRule != "" || len(c.Drift) == 0 {
		t.Fatalf("check with a member in the band = %+v; want drift this entry fixes", c)
	}
}

// TestFixAntiAffinityGroups_AnEntryWithNoAZMapLeavesAnAZMapRuleAlone pins the
// fix for the lab's shape on three nodes. Rule -4 holds indexes 2 and 3, which
// an az_map of three AZs gives that name, so neither the dry run nor --apply
// of an entry with no az_map renames it. The entry with the az_map finds the
// group in its layout.
func TestFixAntiAffinityGroups_AnEntryWithNoAZMapLeavesAnAZMapRuleAlone(t *testing.T) {
	t.Parallel()
	seed := func() *aaClusterStub {
		stub := newAAStub()
		stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"),
			aaQEMU(102, aaTag+";index--2"), aaQEMU(103, aaTag+";index--3"))
		markWebMembers(stub, 100, 101, 102, 103)
		stub.seedRule(aaRule, "vm:100,vm:101")
		stub.seedRule(aaRule+"-4", "vm:102,vm:103")
		return stub
	}
	deps := func(stub *aaClusterStub, azMap map[string][]string) Deps {
		d := aaMetadataDeps(stub)
		d.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
		d.Config.Placement.AZMap = azMap
		return d
	}
	stub := seed()
	for _, dry := range []bool{true, false} {
		got, err := FixAntiAffinityGroups(t.Context(), deps(stub, nil), AntiAffinityFixOptions{DryRun: dry})
		if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityProblem {
			t.Fatalf("fix (dry run %v) = %+v, %v; want the group not planned", dry, got, err)
		}
		if !strings.Contains(got[0].Problem, aaRule+"-4") || len(got[0].Drift) != 0 {
			t.Fatalf("fix (dry run %v) = %+v; want a problem that names %s-4 and no drift", dry, got[0], aaRule)
		}
	}
	if stub.updateRuleCalls+stub.createRuleCalls+stub.deleteRuleCalls != 0 {
		t.Fatalf("rules = %v; want no rule written", stub.rules)
	}

	stub = seed()
	threeAZs := map[string][]string{"a": {"pve1"}, "b": {"pve2"}, "c": {"pve3"}}
	got, err := FixAntiAffinityGroups(t.Context(), deps(stub, threeAZs), AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityInLayout {
		t.Fatalf("fix with three AZs = %+v, %v; want the group in its layout", got, err)
	}
}

// planDeletesOf returns the rules a check's drift deletes or updates.
func planDeletesOf(c AntiAffinityGroupCheck) []string {
	var out []string
	for _, d := range c.Drift {
		if strings.HasPrefix(d, "delete ") || strings.HasPrefix(d, "update ") {
			out = append(out, d)
		}
	}
	return out
}
