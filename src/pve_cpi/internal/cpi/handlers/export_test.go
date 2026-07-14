package handlers

import "sync"

// ResetFirewallMasterSwitchProbeOnce resets the package-level sync.Once
// guarding probeFirewallMasterSwitch, for use by handlers_test package tests
// that need deterministic once-per-process probe behavior across separate
// test functions in the same test binary. Test-only; never called from
// production code.
func ResetFirewallMasterSwitchProbeOnce() {
	firewallMasterSwitchProbeOnce = sync.Once{}
}
