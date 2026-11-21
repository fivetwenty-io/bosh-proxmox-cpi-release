package handlers

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// sharedClusterOwner is the Director whose two cpi-config entries reach the
// same three-node cluster in the shared-cluster fixture.
const sharedClusterOwner = "62f6eacf-fcda-41fb-8e61-df242ed2cae4"

// sharedClusterNodes are the fixture cluster's nodes.
var sharedClusterNodes = []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-1", "lab-pve-cpi-az2-2"}

// sharedClusterMember is a member of a group in the shared-cluster fixture.
type sharedClusterMember struct {
	vmid  int
	group string
	index int
	// pin is the member's node-affinity pin, and strict says it is strict.
	pin    []string
	strict bool
}

// sharedClusterInput builds the cluster state an entry with no az_map left
// after it placed two groups of deployment rl-a and one of rl-n. The hold
// group's members each carry a strict pin to one node of their own, the sh
// group's six members carry soft pins over every node, and the rules hold
// the block layout of an entry whose az_map defines no AZ. cfg is the config
// of the entry that judges the state.
func sharedClusterInput(t *testing.T, cfg *config.CPIConfig) AntiAffinityCheckInput {
	t.Helper()
	profile, err := pve9ClusterProfile(sharedClusterNodes...)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ident := func(dep, group string) haIdentity {
		return haIdentity{Owner: sharedClusterOwner, Deployment: dep, Group: group}
	}
	hold, sh, cl := ident("rl-a", "hold"), ident("rl-a", "sh"), ident("rl-n", "cl")
	all := sharedClusterNodes
	members := []struct {
		ident haIdentity
		sharedClusterMember
	}{
		{hold, sharedClusterMember{210054, "hold", 0, all[1:2], true}},
		{hold, sharedClusterMember{210098, "hold", 1, all[0:1], true}},
		{hold, sharedClusterMember{210192, "hold", 2, all[2:3], true}},
		{sh, sharedClusterMember{210135, "sh", 0, all, false}},
		{sh, sharedClusterMember{210122, "sh", 1, all, false}},
		{sh, sharedClusterMember{210178, "sh", 2, all, false}},
		{sh, sharedClusterMember{210183, "sh", 3, all, false}},
		{sh, sharedClusterMember{210003, "sh", 4, all, false}},
		{sh, sharedClusterMember{210006, "sh", 5, all, false}},
		{cl, sharedClusterMember{210017, "cl", 0, all, false}},
	}
	in := AntiAffinityCheckInput{
		Config: cfg, Profile: profile, Owner: sharedClusterOwner, GuestsComplete: true,
		Rules: &ha.RuleSet{}, Resources: map[string]string{}, Now: time.Unix(1795283470, 0),
	}
	pinComment := "bosh-cpi:v1 owner=" + sharedClusterOwner + " deployment=- group=- bosh-vols=nfs-ephemeral-3"
	for _, m := range members {
		sid := haResourceSid(m.vmid)
		in.Guests = append(in.Guests, pve.GuestRef{VMID: m.vmid,
			Tags: m.ident.ruleName() + ";index--" + strconv.Itoa(m.index)})
		in.Resources[sid] = m.ident.resourceMarker("block", 1795282456)
		pin := ha.Rule{ID: naRuleNameFor(m.vmid), Type: ha.RuleTypeNodeAffinity, Strict: m.strict,
			Resources: []string{sid}, Nodes: map[string]int{}, Comment: pinComment}
		for _, n := range m.pin {
			pin.Nodes[n] = 1
		}
		in.Rules.Rules = append(in.Rules.Rules, pin)
	}
	in.Resources[haResourceSid(210017)] = cl.resourceMarker("block", 1795282626) +
		" hold=1.210009@1795285223,2.210028@1795285224"
	rule := func(id haIdentity, name string, sids ...string) {
		in.Rules.Rules = append(in.Rules.Rules, ha.Rule{ID: name, Type: haRuleType, Affinity: haRuleAffinity,
			Resources: sids, Comment: id.marker()})
	}
	rule(hold, hold.ruleName(), "vm:210054", "vm:210098")
	rule(sh, sh.ruleName(), "vm:210122", "vm:210135")
	rule(sh, sh.ruleName()+"-2", "vm:210178", "vm:210183")
	rule(sh, sh.ruleName()+"-3", "vm:210003", "vm:210006")
	return in
}

// sharedClusterConfig is the config of an entry with anti-affinity rules on,
// the hold local policy, and azMap as its placement.az_map.
func sharedClusterConfig(azMap map[string][]string) *config.CPIConfig {
	cfg := aaMembershipConfig("off", false, 0)
	cfg.Placement.AZMap = azMap
	return cfg
}

// TestCheckAntiAffinityGroups_AnEntryWithOtherAZsDeletesNoRuleOfThePlacingEntry
// pins that an entry whose az_map maps each node of the cluster to an AZ of
// its own, checked alone, deletes and renames no rule that an entry with no
// az_map placed on the same cluster. The hold group's rule names members
// that the one-node AZs put in different domains, and the sh group's block
// rules carry the names the placing entry numbers them by, which this entry's
// three AZs never give the cluster's domain. Both groups are reported as not
// planned, so the fix writes nothing to them, while the placing entry still
// finds every group in its layout.
func TestCheckAntiAffinityGroups_AnEntryWithOtherAZsDeletesNoRuleOfThePlacingEntry(t *testing.T) {
	t.Parallel()
	placing := CheckAntiAffinityGroups(sharedClusterInput(t, sharedClusterConfig(nil)))
	if len(placing) != 3 {
		t.Fatalf("checks = %+v; want the hold, sh, and cl groups", placing)
	}
	for i := range placing {
		if c := &placing[i]; c.Problem != "" || len(c.Drift) != 0 {
			t.Fatalf("placing entry's check of %s = %+v; want it in its layout", c.Group, c)
		}
	}

	client := sharedClusterConfig(map[string][]string{
		"p9c0": {"lab-pve-cpi-az2-0"}, "p9c1": {"lab-pve-cpi-az2-1"}, "p9c2": {"lab-pve-cpi-az2-2"},
	})
	checks := CheckAntiAffinityGroups(sharedClusterInput(t, client))
	if len(checks) != 3 {
		t.Fatalf("checks = %+v; want the hold, sh, and cl groups", checks)
	}
	for i := range checks {
		c := &checks[i]
		if c.ForeignRule != "" {
			continue
		}
		for _, d := range c.Drift {
			if strings.HasPrefix(d, "delete ") || strings.HasPrefix(d, "update ") {
				t.Errorf("check of %s plans %q; want no write to a rule the placing entry keeps", c.Group, d)
			}
		}
		switch c.Group {
		case "hold":
			if c.Problem == "" {
				t.Errorf("check of hold = %+v; want it reported as not planned", c)
			}
		case "sh":
			if c.ForeignRule == "" {
				t.Errorf("check of %s = %+v; want it reported as not planned", c.Group, c)
			}
		case "cl":
			if c.Problem != "" || len(c.Drift) != 0 {
				t.Errorf("check of cl = %+v; want the one-member group in its layout", c)
			}
		}
	}
	sh := slices.IndexFunc(checks, func(c AntiAffinityGroupCheck) bool { return c.Group == "sh" })
	if sh >= 0 && !strings.Contains(checks[sh].ForeignRule, "bosh-aa-f48916583105-2") {
		t.Errorf("sh foreign rule = %q; want it to name the rule this entry would never write",
			checks[sh].ForeignRule)
	}
}

// TestFixAntiAffinityGroups_LeavesARuleNamedForAnotherAZMapAlone pins that
// the fix writes nothing to a group whose plan would delete a rule this
// entry's az_map never names that way. With three AZs in the map, the whole
// cluster's block rules are numbered 1, 4, and 7, so a rule named base-2 was
// written by an entry with another map, and neither the dry run nor --apply
// deletes it. The same rules under an entry with no map are its own, and the
// fix rewrites them.
func TestFixAntiAffinityGroups_LeavesARuleNamedForAnotherAZMapAlone(t *testing.T) {
	t.Parallel()
	seed := func() *aaClusterStub {
		stub := newAAStub()
		stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"),
			aaQEMU(102, aaTag+";index--2"), aaQEMU(103, aaTag+";index--3"))
		markWebMembers(stub, 100, 101, 102, 103)
		stub.seedRule(aaRule, "vm:100,vm:101")
		stub.seedRule(aaRule+"-2", "vm:102,vm:103")
		return stub
	}
	stub := seed()
	deps := aaMetadataDeps(stub)
	deps.Config.Placement.AZMap = map[string][]string{"a": {"pve1"}, "b": {"pve2"}, "c": {"pve3", "pve4"}}
	for _, dry := range []bool{true, false} {
		got, err := FixAntiAffinityGroups(t.Context(), deps, AntiAffinityFixOptions{DryRun: dry})
		if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityProblem {
			t.Fatalf("fix (dry run %v) = %+v, %v; want the group not planned", dry, got, err)
		}
		if !strings.Contains(got[0].Problem, aaRule+"-2") || len(got[0].Drift) != 0 {
			t.Fatalf("fix (dry run %v) = %+v; want a problem that names %s-2 and no drift", dry, got[0], aaRule)
		}
	}
	if stub.updateRuleCalls+stub.createRuleCalls+stub.deleteRuleCalls != 0 {
		t.Fatalf("rules = %v; want no rule written", stub.rules)
	}

	stub = seed()
	got, err := FixAntiAffinityGroups(t.Context(), aaMetadataDeps(stub), AntiAffinityFixOptions{})
	if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
		t.Fatalf("fix with no az_map = %+v, %v; want the group fixed", got, err)
	}
	if _, ok := stub.rules[aaRule+"-2"]; ok || stub.rules[aaRule] != "vm:100,vm:101,vm:102" {
		t.Fatalf("rules = %v; want the three lowest indexes in the base rule alone", stub.rules)
	}
}
