package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// TestDeleteVM_AMidDeleteMemberNeverLooksLikeAGap models a roll of two
// members of one group at max_in_flight 2. On three nodes a rule holds two
// members, so the layout is {0,1}, {2,3}, {4,5}, and {6,7}. The deletes of
// index 3 and index 5 overlap: the first delete's first hold write meets a
// digest conflict because the second delete runs its whole flow, destroy,
// purge, removal, and plan of the group, in between. No plan of either
// delete may see index 3 as a gap that no hold keeps, so the rule of indexes
// 6 and 7 is never deleted or rewritten, and both places end up held.
func TestDeleteVM_AMidDeleteMemberNeverLooksLikeAGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stub := newAAStub()
	stub.purgeFromRules = true
	fleet := newAAHoldFleet(stub, 100, 101, 102, 103, 104, 105, 106, 107)
	stub.seedRule(aaRule, "vm:100,vm:101")
	stub.seedRule(aaRule+"-2", "vm:102,vm:103")
	stub.seedRule(aaRule+"-3", "vm:104,vm:105")
	stub.seedRule(aaRule+"-4", "vm:106,vm:107")
	deps := aaReplanDeps(stub, false)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	if d := aaDoctorDrift(t, deps); len(d) != 0 {
		t.Fatalf("drift before the deletes = %v; want the group in its layout", d)
	}
	stub.onRuleWrite = func(rule string) {
		if rule == aaRule+"-4" {
			t.Errorf("a delete wrote %s; rules %v", rule, stub.rules)
		}
		if got := stub.rules[aaRule+"-4"]; got != "vm:106,vm:107" {
			t.Errorf("after the write of %s, %s = %q; want indexes 6 and 7 kept", rule, aaRule+"-4", got)
		}
	}
	purge := true
	del := func(vmid int) {
		logger := log.NewNopLogger()
		if _, err := holdAntiAffinityPlacesBeforeDestroy(ctx, deps, vmid, webTags(vmid), logger); err != nil {
			t.Errorf("hold the places of %d before its destroy: %v", vmid, err)
		}
		fleet.destroy(vmid)
		if err := stub.DeleteHaResources(ctx, haResourceSid(vmid),
			&cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
			t.Fatal(err)
		}
		if err := removeAntiAffinityMembership(ctx, deps, vmid, webTags(vmid), logger); err != nil {
			t.Errorf("remove %d: %v", vmid, err)
		}
	}
	armed, nested := false, false
	stub.failUpdateOf = func(string) error {
		if !armed || nested {
			return nil
		}
		nested = true
		del(105)
		return errors.New("detected modified configuration - file changed by other user")
	}
	armed = true
	del(103)
	if !nested {
		t.Fatal("the second delete never ran")
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Errorf("rule %s = %q; want indexes 0 and 1 kept", aaRule, got)
	}
	var comments strings.Builder
	for _, c := range stub.ruleComments {
		comments.WriteString(c + " ")
	}
	for _, c := range stub.resComments {
		comments.WriteString(c + " ")
	}
	for _, h := range []string{"3.103@", "5.105@"} {
		if !strings.Contains(comments.String(), h) {
			t.Errorf("no record holds %s; rule comments %v, resource comments %v", h, stub.ruleComments,
				stub.resComments)
		}
	}
	if d := aaDoctorDrift(t, deps); len(d) != 0 {
		t.Errorf("drift after both deletes = %v; rules %v", d, stub.rules)
	}
}
