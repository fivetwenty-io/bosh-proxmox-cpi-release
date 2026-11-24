package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestAAOff_MovesTheMembersOutOfAnOlderCPIsRule pins that the join of a
// group whose newest member records aa=off takes this Director's members out
// of the group's unmarked rule from an older CPI, as it deletes the group's
// own rules, and leaves another Director's members in it. A rule left with
// fewer than two members is deleted.
func TestAAOff_MovesTheMembersOutOfAnOlderCPIsRule(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rule, want string
	}{
		"one foreign member left":  {rule: "vm:100,vm:101,vm:200"},
		"two foreign members left": {rule: "vm:100,vm:101,vm:200,vm:201", want: "vm:200,vm:201"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.seedRuleWithComment("bosh-aa-web", tc.rule, aaLegacyRuleComment)
			stub.tagged(aaQEMU(100, legacyTags(aaDirector)), aaQEMU(101, legacyTags(aaDirector)),
				aaQEMU(200, legacyTags(aaOtherDirector)), aaQEMU(201, legacyTags(aaOtherDirector)))
			markWebPattern(stub, "off", 102)
			if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102,
				log.NewNopLogger()); err != nil {
				t.Fatalf("join: %v", err)
			}
			if got := aaGroupRules(stub); len(got) != 0 {
				t.Fatalf("rules = %v; want none for an opted-out group", got)
			}
			got, ok := stub.rules["bosh-aa-web"]
			switch {
			case tc.want == "" && ok:
				t.Fatalf("the older CPI's rule = %q; want it deleted", got)
			case tc.want != "" && got != tc.want:
				t.Fatalf("the older CPI's rule = %q; want %q", got, tc.want)
			}
		})
	}
}
