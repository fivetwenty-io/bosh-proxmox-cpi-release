package handlers

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// ResetFirewallMasterSwitchProbeOnce drains the per-cluster probe state
// guarding probeFirewallMasterSwitch, for use by handlers_test package tests
// that need deterministic probe behavior across separate test functions in
// the same test binary. Test-only; never called from production code.
func ResetFirewallMasterSwitchProbeOnce() {
	firewallMasterSwitchProbedClusters.Range(func(k, _ any) bool {
		firewallMasterSwitchProbedClusters.Delete(k)
		return true
	})
}

// TagRetainEphemeral re-exports the unexported tagRetainEphemeral constant
// for handlers_test package tests that assert on the tag string without
// duplicating its literal value. Test-only; never referenced from production
// code.
const TagRetainEphemeral = tagRetainEphemeral

// SelfSignedCAPEMForTest re-exports the internal test helper that generates a
// fresh self-signed CA certificate as PEM, for handlers_test package tests
// that need two distinct, valid pve.ca_cert values. Test-only.
func SelfSignedCAPEMForTest(t *testing.T) string {
	t.Helper()
	return selfSignedCAPEMForCacheKeyTest(t)
}

// SetUntrackedCloneRescanTimeoutForTest shortens the deadline of the
// untracked destroy's linked-clone rescan and returns a func that restores
// it. A test that calls it must not run in parallel. Test-only.
func SetUntrackedCloneRescanTimeoutForTest(d time.Duration) func() {
	old := untrackedCloneRescanTimeout
	untrackedCloneRescanTimeout = d
	return func() { untrackedCloneRescanTimeout = old }
}

// VMIDLockTTLFor, VMIDLockTTLMargin, and PoolSentinelBudget re-export what the
// VMID lock budget is built from, for handlers_test package tests that check
// the time a handler spends under the lock against the TTL it claims with.
// Test-only.
var VMIDLockTTLFor = vmidLockTTLFor

const (
	VMIDLockTTLMargin  = vmidLockTTLMargin
	PoolSentinelBudget = poolSentinelBudget
)

// HAOwnerMarkerForTest returns the HA resource comment that marks a resource
// as owned by the Director with the given UUID, for handlers_test package tests
// that build an HA scene this Director owns. Test-only.
func HAOwnerMarkerForTest(owner string) string {
	return haIdentity{Owner: owner}.marker()
}

// GroupRuleForTest returns the hashed anti-affinity rule name and the rule
// comment marker of the instance group group in deployment, owned by the
// Director with the given UUID, for handlers_test package tests that seed a
// group's rule the heal can find. Test-only.
func GroupRuleForTest(owner, deployment, group string) (rule, marker string) {
	id := haIdentity{Owner: owner, Deployment: deployment, Group: group}
	return id.ruleName(), id.marker()
}

// SpreadClaimKey re-exports the group name the create of a VM with env claims
// its node under, for handlers_test package tests that read the claim record.
// Test-only.
func SpreadClaimKey(deps Deps, env map[string]any) string {
	return antiAffinityScorerGroup(deps, env).claimKey()
}

// RetiredParker is one call of the emptied-parker retirement a test captured
// with SetRetireEmptiedParkerForTest. Test-only.
type RetiredParker struct {
	Node    string
	VMID    int
	Trigger string
}

// SetRetireEmptiedParkerForTest replaces the retirement every emptying path
// calls with a recorder and returns the calls it saw and a func that restores
// the production retirement. A test that calls it must not run in parallel.
// Test-only.
func SetRetireEmptiedParkerForTest() (calls func() []RetiredParker, restore func()) {
	var mu sync.Mutex
	var seen []RetiredParker
	restore = setRetireEmptiedParkerForTest(func(_ context.Context, _ Deps, node string, vmid int, trigger string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, RetiredParker{Node: node, VMID: vmid, Trigger: trigger})
	})
	return func() []RetiredParker {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}, restore
}
