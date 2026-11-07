package handlers

import (
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
