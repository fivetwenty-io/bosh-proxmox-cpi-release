package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// TestDecisionPinOfAnAntiAffinityMemberNamesEveryNode shows that a registered
// VM with no node constraint in an anti-affinity rule gets a soft pin over
// every configured node at one priority under ha-rebalance-on-start, where a
// VM outside such a rule gets the placed-node pin. A pin that names only the
// placed node would leave the CRM nowhere to move the member once a sibling
// runs there. A pin the CPI wrote earlier with the placed node alone is
// rewritten the same way.
func TestDecisionPinOfAnAntiAffinityMemberNamesEveryNode(t *testing.T) {
	profileFn := func(context.Context) (*ha.Profile, error) {
		p, _ := pve9ClusterProfile("pve01", "pve02", "pve03")(context.Background())
		p.CRS = ha.CRS{Known: true, RebalanceOnStart: true}
		return p, nil
	}
	for name, earlier := range map[string]bool{"fresh": false, "earlier placed-node pin": true} {
		t.Run(name, func(t *testing.T) {
			stub := newNAStub()
			stub.resources["vm:100"] = true
			negative := ha.AffinityNegative
			stub.rules["bosh-aa-x"] = cluster.CreateHaRulesParams{Rule: "bosh-aa-x", Type: ha.RuleTypeResourceAffinity,
				Affinity: &negative, Resources: "vm:100,vm:101"}
			if earlier {
				nodes, strict := "pve02", false
				comment := ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), []string{testPinStorage})
				stub.rules[naRuleNameFor(100)] = cluster.CreateHaRulesParams{Rule: naRuleNameFor(100),
					Type: ha.RuleTypeNodeAffinity, Resources: "vm:100", Nodes: &nodes, Strict: &strict, Comment: &comment}
			}
			deps := naDeps(stub)
			deps.HAProfile = profileFn
			profile, err := deps.HAProfile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			d := &ha.VMDecision{VMID: 100, SID: "vm:100", Node: "pve02", Outcome: ha.OutcomeRegister,
				Placed: "pve02", Admissible: []string{"pve01", "pve02", "pve03"}}
			if _, err := writeMergedPinRule(context.Background(), deps, profile, 100,
				pinRequest{Decision: d, Storages: []string{testPinStorage}}, log.NewNopLogger()); err != nil {
				t.Fatal(err)
			}
			rule := stub.rules[naRuleNameFor(100)]
			if got := ptrStr(rule.Nodes); got != "pve01,pve02,pve03" || (rule.Strict != nil && *rule.Strict) {
				t.Fatalf("anti-affinity member pin nodes %q strict %v", got, rule.Strict)
			}
		})
	}
}
