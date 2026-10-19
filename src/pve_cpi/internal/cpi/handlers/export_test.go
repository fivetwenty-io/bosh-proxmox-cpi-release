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
