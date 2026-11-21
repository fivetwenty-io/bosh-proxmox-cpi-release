package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// aaReplanDeps returns deps whose settings let a delete plan its group's
// rules again on a cluster of four nodes, so a rule holds at most three
// members. With lock set, every rule write runs under the HA rules lock.
func aaReplanDeps(stub *aaClusterStub, lock bool) Deps {
	mode := "off"
	if lock {
		mode = "pool"
	}
	deps := aaDepsLock(aaMembershipConfig(mode, false, 30), stub, newAALockPools(nil))
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4")
	deps.HARuleSleep = func(context.Context, time.Duration) error { return nil }
	return deps
}

// indexedWebGuests lists each VMID as a guest of the "web" group whose index
// tag names the VMID's last two digits, so vm:100 has index 0.
func indexedWebGuests(stub *aaClusterStub, vmids ...int) {
	rows := make([]json.RawMessage, 0, len(vmids))
	for _, v := range vmids {
		rows = append(rows, aaQEMU(v, webTags(v)))
	}
	stub.tagged(rows...)
}

// webTags returns the tags of a "web" member whose index is the VMID's last
// two digits.
func webTags(vmid int) string {
	return aaTag + ";index--" + strconv.Itoa(vmid%100)
}

// TestRemoveAntiAffinity_PlansTheGroupAgainByIndex pins that a delete plans
// the rest of its group again with the deleted member's place held for the
// recreate of its index, with the rules lock on and off. On four nodes a rule
// holds three members. The rule holds indexes 0, 1, and 2, and index 3 waits
// outside it. Once index 1 is deleted, index 3 stays out of the free place,
// because the recreate of index 1 would take the place back and move index 3
// out again, which costs two writes and can migrate index 3 twice. So the
// plan writes nothing, and the recreate of index 1 joins the rule with one
// write, which also drops the record of the member that left, since its HA
// resource is purged.
func TestRemoveAntiAffinity_PlansTheGroupAgainByIndex(t *testing.T) {
	t.Parallel()
	for name, lock := range map[string]bool{"lock on": true, "lock off": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.purgeFromRules = true
			indexedWebGuests(stub, 100, 102, 103)
			markWebMembers(stub, 100, 101, 102, 103)
			stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			if err := removeAntiAffinityMembership(context.Background(), aaReplanDeps(stub, lock), 101, webTags(101),
				log.NewNopLogger()); err != nil {
				t.Fatal(err)
			}
			if got := stub.rules[aaRule]; got != "vm:100,vm:102" {
				t.Fatalf("rule = %q; want the place index 1 left held for its recreate", got)
			}
			if _, ok := stub.rules[aaRule+"-2"]; ok {
				t.Fatalf("rules = %v; want no second rule", stub.rules)
			}
			stub.tagged(aaQEMU(100, webTags(100)), aaQEMU(102, webTags(102)), aaQEMU(103, webTags(103)),
				aaQEMU(104, aaTag+";index--1"))
			markWebMembers(stub, 104)
			before := stub.createRuleCalls + stub.updateRuleCalls + stub.deleteRuleCalls
			joinAntiAffinityAtMetadata(context.Background(), aaReplanDeps(stub, lock), 104,
				map[string]any{"index": 1.0}, log.NewNopLogger())
			if got := stub.rules[aaRule]; got != "vm:100,vm:102,vm:104" {
				t.Fatalf("rule = %q after the recreate; want index 1 back in its place", got)
			}
			if n := stub.createRuleCalls + stub.updateRuleCalls + stub.deleteRuleCalls - before; n != 1 {
				t.Fatalf("the recreate wrote %d rules; want 1", n)
			}
			if got := stub.ruleComments[aaRule]; got != aaRuleMarker() {
				t.Fatalf("comment = %q; want the plain marker once the HA resource is purged", got)
			}
		})
	}
}

// TestRemoveAntiAffinity_FindsTheGroupFromTheTagsAfterThePurge pins that a
// delete whose destroy already purged the VM's HA resource, which took the VM
// out of every rule, still plans the group again. The rules no longer name
// the VM, so the group comes from the member tag on the tags the delete read
// before the destroy. Without that tag the delete finds no group. The rule an
// older release filled to the four nodes shows the plan: it keeps index 1's
// place free and moves indexes 3 and 4 to a second rule.
func TestRemoveAntiAffinity_FindsTheGroupFromTheTagsAfterThePurge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tags string
		want string
	}{
		"member tag":    {tags: aaTag + ";index--1", want: "vm:100,vm:102"},
		"no member tag": {tags: "index--1", want: "vm:100,vm:102,vm:103,vm:104"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			indexedWebGuests(stub, 100, 102, 103, 104)
			markWebMembers(stub, 100, 102, 103, 104)
			stub.seedRule(aaRule, "vm:100,vm:102,vm:103,vm:104")
			if err := removeAntiAffinityMembership(context.Background(), aaReplanDeps(stub, true), 101, tc.tags,
				log.NewNopLogger()); err != nil {
				t.Fatal(err)
			}
			if got := stub.rules[aaRule]; got != tc.want {
				t.Fatalf("rule = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestRemoveAntiAffinity_RewritesAFullRuleFromAnOlderRelease pins that the
// first delete in a group whose rule an older release filled to the number
// of nodes rewrites the group into the index layout. The rule holds indexes 0
// through 3 on four nodes, and the deleted index 4 was in no rule. The plan
// keeps indexes 0, 1, and 2 and writes no rule for index 3 alone.
func TestRemoveAntiAffinity_RewritesAFullRuleFromAnOlderRelease(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.purgeFromRules = true
	indexedWebGuests(stub, 100, 101, 102, 103)
	markWebMembers(stub, 100, 101, 102, 103, 104)
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102,vm:103")
	if err := removeAntiAffinityMembership(context.Background(), aaReplanDeps(stub, true), 104, webTags(104),
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want the full rule cut to indexes 0, 1, and 2", got)
	}
	if _, ok := stub.rules[aaRule+"-2"]; ok {
		t.Fatalf("rules = %v; want no rule for index 3 alone", stub.rules)
	}
}

// TestRemoveAntiAffinity_LeavesAnotherDirectorsGroupAlone pins that the
// member tag finds only this Director's group, so a delete never plans the
// rules of a group whose rules and HA resources name another Director.
func TestRemoveAntiAffinity_LeavesAnotherDirectorsGroupAlone(t *testing.T) {
	t.Parallel()
	other := haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}
	tag := other.ruleName()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, tag+";index--0"), aaQEMU(102, tag+";index--2"), aaQEMU(103, tag+";index--3"))
	for _, v := range []int{100, 102, 103} {
		stub.resComments[haResourceSid(v)] = other.resourceMarker("", 1700000000)
	}
	stub.seedRuleWithComment(tag, "vm:100,vm:102", other.marker())
	if err := removeAntiAffinityMembership(context.Background(), aaReplanDeps(stub, true), 101, tag+";index--1",
		log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if got := stub.rules[tag]; got != "vm:100,vm:102" || stub.createRuleCalls+stub.updateRuleCalls != 0 {
		t.Fatalf("rule = %q after %d writes; want the other Director's rule untouched", got,
			stub.createRuleCalls+stub.updateRuleCalls)
	}
}

// TestCleanupHAMembership_AFailedReplanOnlyWarns pins that a delete whose
// plan of the group fails to write still succeeds, and that the failure is
// logged as a warning. The plan cuts a rule an older release filled to the
// four nodes, so it has a rule to write.
func TestCleanupHAMembership_AFailedReplanOnlyWarns(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	indexedWebGuests(stub, 100, 102, 103, 104)
	markWebMembers(stub, 100, 102, 103, 104)
	stub.seedRule(aaRule, "vm:100,vm:102,vm:103,vm:104")
	stub.failUpdateRule = true
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	st := &pve.HAServiceState{SID: "vm:101", VMID: 101, Known: true, Configured: true, Raw: "stopped", Node: "pve1"}
	if err := cleanupHAMembership(context.Background(), aaReplanDeps(stub, true), st, 101, webTags(101),
		logger); err != nil {
		t.Fatalf("cleanup = %v; want success", err)
	}
	if stub.updateRuleCalls == 0 {
		t.Fatal("the delete never tried to write the group's rules")
	}
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "cleanup incomplete") &&
			strings.Contains(fmt.Sprint(e.Attrs["error"]), "again after the delete") {
			return
		}
	}
	t.Fatalf("no warning about the failed plan in %+v", obs.All())
}

// TestRemoveAntiAffinity_RollingUpdateWritesEachMembersRulesOnly pins what a
// rolling update of a group costs. BOSH deletes each instance and creates it
// again under a new VMID with the same index, which joins at
// set_vm_metadata. Each instance may cost one write that changes a rule's
// members when it leaves and one when it comes back, no other member may
// change rules, and the group ends in the layout it started in.
//
// The writes that change only the holds a rule records are counted apart,
// against an exact number. On four nodes the deleted member's rule keeps two
// members, so the shrink that takes the member out records its hold and the
// recreate's join drops it, and no write changes the holds alone. On three
// nodes the delete leaves its rule with one member, and a rule delete carries
// no comment, so each member costs two such writes. The first records the
// hold on the group's other rule before the delete, so the place is never
// recorded nowhere, and the second drops it from that rule once the recreate
// has joined.
func TestRemoveAntiAffinity_RollingUpdateWritesEachMembersRulesOnly(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		nodes    []string
		rules    []string
		holdOnly int
	}{
		"three nodes": {[]string{"pve1", "pve2", "pve3"}, []string{"vm:100,vm:101", "vm:102,vm:103", "vm:104,vm:105"},
			12},
		"four nodes": {[]string{"pve1", "pve2", "pve3", "pve4"}, []string{"vm:100,vm:101,vm:102", "vm:103,vm:104,vm:105"},
			0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.purgeFromRules = true
			deps := aaReplanDeps(stub, true)
			deps.HAProfile = pve9ClusterProfile(tc.nodes...)
			vmids := []int{100, 101, 102, 103, 104, 105}
			list := func() { listIndexedMembers(stub, vmids) }
			list()
			markWebMembers(stub, vmids...)
			seedRuleChain(stub, tc.rules)
			start := aaRuleOfMembers(stub.rules)
			members, holdOnly := countRuleWrites(t, stub)
			for i, old := range slices.Clone(vmids) {
				tags := aaTag + ";index--" + strconv.Itoa(i)
				prev := aaRuleOfMembers(stub.rules)
				// The destroy lands before the delete's cleanup, and the
				// recreate's guest appears only after it.
				vmids[i] = 0
				list()
				if err := removeAntiAffinityMembership(context.Background(), deps, old, tags,
					log.NewNopLogger()); err != nil {
					t.Fatal(err)
				}
				vmids[i] = 200 + i
				list()
				markWebMembers(stub, vmids[i])
				joinAntiAffinityAtMetadata(context.Background(), deps, vmids[i], map[string]any{"index": float64(i)},
					log.NewNopLogger())
				for sid, rule := range aaRuleOfMembers(stub.rules) {
					if sid != haResourceSid(vmids[i]) && prev[sid] != rule {
						t.Errorf("replacing index %d moved %s from %q to %q", i, sid, prev[sid], rule)
					}
				}
			}
			assertEachIndexBackInItsRule(t, start, aaRuleOfMembers(stub.rules), vmids)
			if n := *members; n > 2*len(vmids) {
				t.Errorf("the update changed rule members in %d writes for %d members; want at most %d", n,
					len(vmids), 2*len(vmids))
			}
			if n := *holdOnly; n != tc.holdOnly {
				t.Errorf("the update changed only the holds of a rule in %d writes; want %d", n, tc.holdOnly)
			}
			for name, c := range stub.ruleComments {
				if len(holdsOf(c)) != 0 {
					t.Errorf("comment of %s = %q after the update; want every hold consumed and dropped", name, c)
				}
			}
		})
	}
}

// countRuleWrites counts the stub's rule writes from now on, as the writes
// that change a rule's members, which every create and delete does, and the
// writes that change only the holds a rule records. A write that keeps the
// members and changes any other part of the comment fails the test.
func countRuleWrites(t *testing.T, stub *aaClusterStub) (members, holdOnly *int) {
	t.Helper()
	members, holdOnly = new(int), new(int)
	rules, comments := maps.Clone(stub.rules), maps.Clone(stub.ruleComments)
	stub.onRuleWrite = func(rule string) {
		was, had := rules[rule]
		now, has := stub.rules[rule]
		switch {
		case !had || !has || was != now:
			*members++
		case withHolds(comments[rule], holdsOf(stub.ruleComments[rule])) == stub.ruleComments[rule]:
			*holdOnly++
		default:
			t.Errorf("write of %s kept its members and changed its comment from %q to %q", rule, comments[rule],
				stub.ruleComments[rule])
		}
		rules, comments = maps.Clone(stub.rules), maps.Clone(stub.ruleComments)
	}
	return members, holdOnly
}

// listIndexedMembers lists each nonzero VMID of vmids as the group member
// whose index is its position.
func listIndexedMembers(stub *aaClusterStub, vmids []int) {
	rows := make([]json.RawMessage, 0, len(vmids))
	for i, v := range vmids {
		if v != 0 {
			rows = append(rows, aaQEMU(v, aaTag+";index--"+strconv.Itoa(i)))
		}
	}
	stub.tagged(rows...)
}

// seedRuleChain seeds the group's rules in order, the first under the base
// name and each later one under the base name with its suffix.
func seedRuleChain(stub *aaClusterStub, rules []string) {
	for i, r := range rules {
		name := aaRule
		if i > 0 {
			name = aaRule + "-" + strconv.Itoa(i+1)
		}
		stub.seedRule(name, r)
	}
}

// assertEachIndexBackInItsRule checks that the VM at each index of vmids
// ends in the rule that the original VM of that index, 100 plus the index,
// started in.
func assertEachIndexBackInItsRule(t *testing.T, start, end map[string]string, vmids []int) {
	t.Helper()
	for i, v := range vmids {
		if got, want := end[haResourceSid(v)], start[haResourceSid(100+i)]; got != want {
			t.Errorf("index %d ends in %q; started in %q", i, got, want)
		}
	}
}

// aaRuleOfMembers maps each sid the stub's rules hold to its rule.
func aaRuleOfMembers(rules map[string]string) map[string]string {
	out := map[string]string{}
	for name, members := range rules {
		for _, sid := range strings.Split(members, ",") {
			if sid != "" {
				out[sid] = name
			}
		}
	}
	return out
}
