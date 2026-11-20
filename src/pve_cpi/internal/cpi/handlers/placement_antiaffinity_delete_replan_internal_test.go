package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
// the rest of its group into the index layout, with the rules lock on and
// off. On four nodes a rule holds three members. The rule holds indexes 0, 1,
// and 2, and index 3 waits outside it. Once index 1 is deleted, index 3 takes
// the free place, and the record of the leaving member is gone with its HA
// resource.
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
			if got := stub.rules[aaRule]; got != "vm:100,vm:102,vm:103" {
				t.Fatalf("rule = %q; want index 3 in the place index 1 left", got)
			}
			if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
				t.Fatalf("comment = %q; want the plain marker once the HA resource is purged", got)
			}
			if _, ok := stub.rules[aaRule+"-2"]; ok {
				t.Fatalf("rules = %v; want no second rule", stub.rules)
			}
		})
	}
}

// TestRemoveAntiAffinity_FindsTheGroupFromTheTagsAfterThePurge pins that a
// delete whose destroy already purged the VM's HA resource, which took the VM
// out of every rule, still plans the group again. The rules no longer name
// the VM, so the group comes from the member tag on the tags the delete read
// before the destroy. Without that tag the delete finds no group.
func TestRemoveAntiAffinity_FindsTheGroupFromTheTagsAfterThePurge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tags string
		want string
	}{
		"member tag":    {tags: webTags(101), want: "vm:100,vm:102,vm:103"},
		"no member tag": {tags: "index--1", want: "vm:100,vm:102"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			indexedWebGuests(stub, 100, 102, 103)
			markWebMembers(stub, 100, 102, 103)
			stub.seedRule(aaRule, "vm:100,vm:102")
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
// logged as a warning.
func TestCleanupHAMembership_AFailedReplanOnlyWarns(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	indexedWebGuests(stub, 100, 102, 103)
	markWebMembers(stub, 100, 102, 103)
	stub.seedRule(aaRule, "vm:100,vm:102")
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
