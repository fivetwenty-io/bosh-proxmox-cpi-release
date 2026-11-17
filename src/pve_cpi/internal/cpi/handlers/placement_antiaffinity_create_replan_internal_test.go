package handlers

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// These tests cover how a create ends after it plans its group's rules again
// because a pin repair took a sibling out of its write.

// TestPlacedState_ChecksTheRuleHoldsTheVMEnabledAndWithoutErrors pins each
// condition placedInEffect checks on the rule the last plan placed the VM in.
func TestPlacedState_ChecksTheRuleHoldsTheVMEnabledAndWithoutErrors(t *testing.T) {
	t.Parallel()
	rule := func(mut func(*ha.Rule)) *ha.ApplyResult {
		r := ha.Rule{ID: aaRule, Type: haRuleType, Affinity: haRuleAffinity,
			Resources: []string{"vm:101", "vm:102", "vm:104"}}
		if mut != nil {
			mut(&r)
		}
		return &ha.ApplyResult{Set: &ha.RuleSet{Rules: []ha.Rule{r}}}
	}
	cases := []struct {
		name     string
		res      *ha.ApplyResult
		placedIn string
		want     string
	}{
		{name: "in effect", res: rule(nil), placedIn: aaRule},
		{name: "placed in no rule", res: rule(nil), want: "placed vm:104 in no rule"},
		{name: "no read", placedIn: aaRule, want: "could not be read"},
		{name: "gone", res: &ha.ApplyResult{Set: &ha.RuleSet{}}, placedIn: aaRule, want: "is gone"},
		{name: "without the VM", placedIn: aaRule, want: "does not hold vm:104",
			res: rule(func(r *ha.Rule) { r.Resources = []string{"vm:101", "vm:102"} })},
		{name: "disabled", placedIn: aaRule, want: "is disabled",
			res: rule(func(r *ha.Rule) { r.Disabled = true })},
		{name: "errors", placedIn: aaRule, want: "has errors: vm:104 conflicts",
			res: rule(func(r *ha.Rule) { r.Errors = []string{"vm:104 conflicts"} })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			planner := &aaPlanner{sid: "vm:104", placedIn: tc.placedIn}
			got := placedState(tc.res, planner)
			if tc.want == "" {
				if got != "" || !placedInEffect(tc.res, planner) {
					t.Fatalf("state = %q; want the rule in effect", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || placedInEffect(tc.res, planner) {
				t.Fatalf("state = %q; want it out of effect with %q", got, tc.want)
			}
		})
	}
}

// TestEnsureAntiAffinity_ManagedCreateFailsWhenItsPlacedRuleIsOutOfEffect pins
// that a managed create whose first write a repair reverted fails when its
// second plan ends without the VM in effect in its rule, even though that
// plan's write came back clean. Only PVE reports that vm:103's strict pin
// conflicts with the rule, so the first round's write is reverted and the
// second round holds vm:103 out. Then the second round's read finds the rule
// already holding vm:104 but disabled or in error, as a concurrent writer and
// PVE would leave it, so the plan has nothing to write. Or the verify read of
// the second round's write finds vm:104 gone from the rule. The error must
// say what the last read showed and still fail the call.
func TestEnsureAntiAffinity_ManagedCreateFailsWhenItsPlacedRuleIsOutOfEffect(t *testing.T) {
	t.Parallel()
	// The stub's rule reads in this case run in a fixed order. The sixth is
	// the second round's first read, and the eighth is the verify read after
	// that round's write.
	const secondRound, secondVerify = 6, 8
	cases := []struct {
		name string
		at   int
		set  func(stub *aaClusterStub, flagged *bool)
		want string
	}{
		{
			name: "errors",
			at:   secondRound,
			set: func(stub *aaClusterStub, flagged *bool) {
				stub.rules[aaRule] = "vm:101,vm:102,vm:104"
				*flagged = true
			},
			want: "has errors",
		},
		{
			name: "disabled",
			at:   secondRound,
			set: func(stub *aaClusterStub, _ *bool) {
				stub.rules[aaRule] = "vm:101,vm:102,vm:104"
				stub.ruleDisabled = map[string]bool{aaRule: true}
			},
			want: "is disabled",
		},
		{
			name: "without the VM",
			at:   secondVerify,
			set: func(stub *aaClusterStub, _ *bool) {
				stub.rules[aaRule] = dropSidFromCSV(stub.rules[aaRule], "vm:104")
			},
			want: "does not hold vm:104",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103, 104)
			stub.seedRule(aaRule, "vm:101,vm:102")
			pin := seedStrictPin(stub, 103, "pve1")
			pinOutWhileInRule(stub, pin, aaRule, "vm:103")
			flagged := false
			pinErrors := stub.ruleErrors
			stub.ruleErrors = func(name string) map[string][]string {
				if name == aaRule && flagged {
					return map[string][]string{"resources": {"vm:104 conflicts"}}
				}
				return pinErrors(name)
			}
			stub.beforeListRules = func(n int) {
				if n == tc.at {
					tc.set(stub, &flagged)
				}
			}
			deps := aaModeDeps(stub, config.HAModeManaged, "", "pve1", "pve2", "pve3", "pve4")
			logger, _ := log.NewObservedLogger(log.LevelDebug)
			_, err := ensureAntiAffinityMembershipFor(context.Background(), deps, aaIdent("web"), 104, nil, logger)
			if !errors.Is(err, ha.ErrRuleNotInEffect) {
				t.Fatalf("err = %v; want the managed create to fail with the rule not in effect", err)
			}
			if !strings.HasPrefix(err.Error(), "anti-affinity: after 2 plans") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want it to describe the second plan's read with %q", err, tc.want)
			}
		})
	}
}

// TestEnsureAntiAffinity_CreateStopsAfterFourPlans pins the bound on a
// create's plans. PVE flags a different sibling's strict pin in each round,
// so every repair confines a new sibling and the create could go on planning.
// It must stop after four plans. In managed mode it must fail with the fourth
// round's refusal, which names vm:107's pin. In auto mode it must return an
// error that runs the VM without HA, because the fourth repair took vm:104
// back out of the rule. Either way the heal after the create must leave the
// rule holding vm:104 and none of the flagged siblings.
func TestEnsureAntiAffinity_CreateStopsAfterFourPlans(t *testing.T) {
	t.Parallel()
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.enforceManaged = true
			managedGroup(stub, 101, 102, 103, 104, 105, 106, 107)
			stub.seedRule(aaRule, "vm:101,vm:102")
			plans := flagOneSiblingPerRound(stub, []int{103, 105, 106, 107})
			deps := aaModeDeps(stub, mode, "", "pve1", "pve2", "pve3", "pve4", "pve5", "pve6", "pve7", "pve8",
				"pve9")
			logger, _ := log.NewObservedLogger(log.LevelDebug)
			_, err := ensureAntiAffinityMembershipFor(context.Background(), deps, aaIdent("web"), 104, nil, logger)
			requireFourthPlanOutcome(t, deps, mode, err)
			if *plans != 4 {
				t.Fatalf("the create wrote %d plans that joined vm:104 with a flagged sibling; want 4", *plans)
			}
			if got := stub.rules[aaRule]; got != "vm:101,vm:102,vm:104" {
				t.Fatalf("rule = %q; want the heal to leave vm:104 in and the flagged siblings out", got)
			}
		})
	}
}

// flagOneSiblingPerRound pins each of vmids strictly to its own node and has
// PVE flag only the pin of the first of them that the rule holds, so each
// repair confines the next one. It returns the count of writes of the rule
// that joined vm:104 with a flagged sibling, which is one per plan.
func flagOneSiblingPerRound(stub *aaClusterStub, vmids []int) *int {
	flagged := make([]string, 0, len(vmids))
	pins := map[string]string{}
	for i, vmid := range vmids {
		flagged = append(flagged, haResourceSid(vmid))
		pins[seedStrictPin(stub, vmid, "pve"+strconv.Itoa(i+1))] = haResourceSid(vmid)
	}
	stub.ruleErrors = func(name string) map[string][]string {
		sid, ok := pins[name]
		if !ok {
			return nil
		}
		members := strings.Split(stub.rules[aaRule], ",")
		i := slices.IndexFunc(flagged, func(f string) bool { return slices.Contains(members, f) })
		if i < 0 || flagged[i] != sid {
			return nil
		}
		return map[string][]string{"resources": {sid + " conflicts"}}
	}
	plans := 0
	stub.onRuleWrite = func(rule string) {
		members := strings.Split(stub.rules[rule], ",")
		if rule == aaRule && slices.Contains(members, "vm:104") &&
			slices.ContainsFunc(flagged, func(f string) bool { return slices.Contains(members, f) }) {
			plans++
		}
	}
	return &plans
}

// requireFourthPlanOutcome checks the error of a create whose fourth plan was
// its last. Managed mode fails with the fourth round's refusal, which names
// vm:107's pin. Auto mode returns an error that runs the VM without HA.
func requireFourthPlanOutcome(t *testing.T, deps Deps, mode string, err error) {
	t.Helper()
	if mode == config.HAModeManaged {
		if !errors.Is(err, ha.ErrRuleNotInEffect) || !strings.Contains(err.Error(), naRuleNameFor(107)) {
			t.Fatalf("err = %v; want the fourth round's refusal, which names %s", err, naRuleNameFor(107))
		}
		return
	}
	if err == nil || propagateHARuleError(deps, err) {
		t.Fatalf("err = %v; want an auto error that runs the VM without HA", err)
	}
	if !strings.Contains(err.Error(), "after 4 plans") {
		t.Fatalf("err = %v; want it to name the fourth plan", err)
	}
}
