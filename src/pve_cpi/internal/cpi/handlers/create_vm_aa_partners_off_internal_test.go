package handlers

import (
	"context"
	"maps"
	"testing"
)

// TestAntiAffinityPartnersForCreate_OffChargesNoPartner pins that a create
// whose pattern is off reads no partner nodes, even in a group that still
// records a live hold from its block rules, because no rule will pair it
// with a partner. The group still counts for the spread, and a create that
// keeps the block pattern still reads the hold's partners.
func TestAntiAffinityPartnersForCreate_OffChargesNoPartner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps, _, fleet := aaPartnerFleet(t)
	aaDeleteMember(t, deps, fleet, 101)
	var cp createVMCloudProps
	if got := antiAffinityPartnersForCreate(ctx, deps, aaEnvForWebGroup(), cp, "off"); got != nil {
		t.Fatalf("off: partner nodes = %v; want none", got)
	}
	if !antiAffinityScorerGroup(deps, aaEnvForWebGroup()).enabled() {
		t.Fatal("off: the scorer lost the group; want it kept for the spread")
	}
	want := map[string]int{"pve1": 1}
	for _, pattern := range []string{"block", ""} {
		if got := antiAffinityPartnersForCreate(ctx, deps, aaEnvForWebGroup(), cp, pattern); !maps.Equal(got, want) {
			t.Fatalf("pattern %q: partner nodes = %v; want %v", pattern, got, want)
		}
	}
}
