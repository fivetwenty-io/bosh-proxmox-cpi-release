package handlers

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
