// Internal tests proving that TypeRetriableCloud errors from
// ensureAntiAffinityMembership and applyAZNodeAffinityPin propagate to the
// caller rather than being swallowed as best-effort warnings.
//
// These tests close a false-confidence gap: an earlier test asserted the error
// was returned by ensureAntiAffinityMembership itself but never proved that the
// create_vm call-site routing forwarded it.
package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// callSiteRoute mirrors the exact conditional the create_vm call sites use:
// propagate TypeRetriableCloud, swallow everything else as a warning.
// Extracting it here makes the contract explicit and verifiable.
func callSiteRoute(err error) error {
	if err == nil {
		return nil
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		return err // caller must return this to the director
	}
	return nil // generic HA blip: warn-and-continue
}

// --------------------------------------------------------------------------
// anti-affinity call-site routing
// --------------------------------------------------------------------------

// TestCallSiteRoute_PropagatesRetriable asserts the call-site routing function
// forwards TypeRetriableCloud errors.
func TestCallSiteRoute_PropagatesRetriable(t *testing.T) {
	t.Parallel()
	retriable := cpierrors.WrapAs(
		cpierrors.Cloud("lock timeout"),
		cpierrors.TypeRetriableCloud, "lock")
	if got := callSiteRoute(retriable); got == nil {
		t.Error("call-site must propagate TypeRetriableCloud")
	}
}

// TestCallSiteRoute_SwallowsGeneric asserts that a non-retriable HA error is
// swallowed (preserves §7.21 fail-open intent for generic HA API blips).
func TestCallSiteRoute_SwallowsGeneric(t *testing.T) {
	t.Parallel()
	generic := cpierrors.Cloud("HA not configured")
	if got := callSiteRoute(generic); got != nil {
		t.Errorf("call-site must swallow non-retriable HA errors; got %v", got)
	}
}

// TestAntiAffinityRetriable_LockTimeout_Propagated asserts that a lock-timeout
// retriable error from ensureAntiAffinityMembership is propagated by callSiteRoute.
func TestAntiAffinityRetriable_LockTimeout_Propagated(t *testing.T) {
	t.Parallel()
	events := []string{}
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	lockPools := newAALockPools(&events)
	// Pre-hold the lock with a far-future expiry; every CreatePool returns dup.
	lockPools.pools["bosh-lock-aa-web"] = encodeAALockComment("other-owner", 1<<40)
	lockPools.createErr = func(_ string) error { return fmt.Errorf("already exists") }

	cfg := aaLockConfig("pool", false, 1) // 1s timeout → fast
	deps := aaDepsLock(cfg, stub, lockPools)
	err := ensureAntiAffinityMembership(context.Background(), deps, "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected retriable error from lock-timeout")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("lock-timeout must be TypeRetriableCloud; got %v", err)
	}
	// Apply the call-site routing — must propagate.
	if callSiteRoute(err) == nil {
		t.Fatal("call-site routing must propagate the lock-timeout error to the director")
	}
}

// TestAntiAffinityRetriable_VerifyAbsent_Propagated asserts that a verify-absent
// retriable error propagates through the call-site routing.
func TestAntiAffinityRetriable_VerifyAbsent_Propagated(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	stub.dropMemberOnRecreate = "vm:101"

	cfg := aaLockConfig("off", true, 0)
	deps := aaDepsLock(cfg, stub, newAALockPools(nil))
	err := ensureAntiAffinityMembership(context.Background(), deps, "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected retriable error from verify-absent")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("verify-absent must be TypeRetriableCloud; got %v", err)
	}
	if callSiteRoute(err) == nil {
		t.Fatal("call-site routing must propagate the verify-absent error to the director")
	}
}

// TestAntiAffinityGeneric_Swallowed asserts that a generic HA list-rules failure
// is NOT propagated (preserves §7.21 best-effort / fail-open behaviour).
func TestAntiAffinityGeneric_Swallowed(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	stub.failListRules = true // generic HA blip

	cfg := aaLockConfig("off", false, 0)
	deps := aaDepsLock(cfg, stub, newAALockPools(nil))
	err := ensureAntiAffinityMembership(context.Background(), deps, "web", 101, log.NewNopLogger())
	if err == nil {
		t.Fatal("ensureAntiAffinityMembership should return the list error")
	}
	// Non-retriable error must be swallowed at the call site.
	if callSiteRoute(err) != nil {
		t.Fatal("generic HA error must be swallowed (fail-open); should not reach the director")
	}
}

// aaEnvForGroup builds a BOSH env whose instanceGroupName resolves to group:
// the full group ("d-<group>") plus a groups list from which the shortest
// "-<group>" suffix is selected. This drives the REAL applyAntiAffinityMembership
// through its group-derivation path rather than calling ensureAntiAffinityMembership
// with a pre-baked key.
func aaEnvForGroup(group string) map[string]any {
	return map[string]any{
		"bosh": map[string]any{
			"group":  "d-" + group,
			"groups": []any{"d", group, "d-" + group},
		},
	}
}

// aaMembershipConfig extends aaLockConfig by enabling anti_affinity + use_ha_rules
// so the REAL applyAntiAffinityMembership entry gate opens (aaLockConfig alone
// leaves it disabled, which is why the ensure-level tests bypass the gate).
func aaMembershipConfig(mode string, verify bool, timeoutSec int) *config.CPIConfig {
	c := aaLockConfig(mode, verify, timeoutSec)
	on := true
	if c.Placement == nil {
		c.Placement = &config.PlacementConfig{}
	}
	c.Placement.AntiAffinity = &config.AntiAffinityConfig{Enabled: &on, UseHaRules: &on}
	return c
}

// TestApplyAntiAffinityMembership_RetriableLockTimeout_Propagates drives the REAL
// create_vm routing function end-to-end: a held cluster lock that never frees
// must surface as a TypeRetriableCloud the director re-drives — proving the
// propagation at the actual call-site function, not a test-local mirror.
func TestApplyAntiAffinityMembership_RetriableLockTimeout_Propagates(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	events := []string{}
	lockPools := newAALockPools(&events)
	lockPools.pools["bosh-lock-aa-web"] = encodeAALockComment("other-owner", 1<<40)
	lockPools.createErr = func(_ string) error { return fmt.Errorf("already exists") }

	cfg := aaMembershipConfig("pool", false, 1)
	deps := aaDepsLock(cfg, stub, lockPools)
	err := applyAntiAffinityMembership(context.Background(), deps, 101, aaEnvForGroup("web"), log.NewNopLogger())
	if err == nil {
		t.Fatal("applyAntiAffinityMembership must propagate the lock-timeout retriable to the director")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("must be TypeRetriableCloud; got %v", err)
	}
}

// TestApplyAntiAffinityMembership_GenericFail_Swallowed proves the same real
// function preserves §7.21 fail-open intent: a non-retriable HA blip is logged
// and NOT returned, so create_vm still succeeds.
func TestApplyAntiAffinityMembership_GenericFail_Swallowed(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	stub.failListRules = true // generic HA blip, non-retriable

	cfg := aaMembershipConfig("off", false, 0)
	deps := aaDepsLock(cfg, stub, newAALockPools(nil))
	err := applyAntiAffinityMembership(context.Background(), deps, 101, aaEnvForGroup("web"), log.NewNopLogger())
	if err != nil {
		t.Fatalf("generic HA failure must be swallowed by applyAntiAffinityMembership; got %v", err)
	}
}

// --------------------------------------------------------------------------
// node-affinity call-site propagation via applyAZNodeAffinityPin
// --------------------------------------------------------------------------

// naVerifyDeps builds Deps for a node-affinity verify test: HA pin enabled,
// verify on, single-AZ map containing pve-node1.
func naVerifyDeps(stub *aaClusterStub, verifyOn bool) Deps {
	v := verifyOn
	pinEnabled := true
	cfg := icMinConfig()
	cfg.AntiAffinityVerify = &v
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: &pinEnabled,
		AZMap: map[string][]string{
			"z1": {"pve-node1"},
		},
	}
	return aaDepsLock(cfg, stub, newAALockPools(nil))
}

// TestApplyAZNodeAffinityPin_RetriableVerify_Propagates asserts that when
// verifyAntiAffinityMember detects a member-absent (concurrent drop), the
// returned TypeRetriableCloud propagates out of applyAZNodeAffinityPin.
// The stub's dropMemberOnRecreate triggers the concurrent-drop simulation
// (same mechanism as the anti-affinity verify test).
func TestApplyAZNodeAffinityPin_RetriableVerify_Propagates(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	// node-affinity: CreateHaRules for bosh-na-101 creates the rule, then
	// dropMemberOnRecreate drops the sid so verify sees it absent.
	stub.dropMemberOnRecreate = "vm:101"
	deps := naVerifyDeps(stub, true)
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	err := applyAZNodeAffinityPin(context.Background(), deps, 101, cp, "pve-node1", log.NewNopLogger())
	if err == nil {
		t.Fatal("expected retriable error when verify sees member absent in node-affinity rule")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("node-affinity verify-absent must be TypeRetriableCloud; got %v", err)
	}
}

// TestApplyAZNodeAffinityPin_GenericFail_Swallowed asserts that a generic HA
// failure (non-retriable) from ensureNodeAffinityPin is logged as a warning by
// applyAZNodeAffinityPin and not returned — preserving §7.21 fail-open intent.
func TestApplyAZNodeAffinityPin_GenericFail_Swallowed(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.failListRules = true // causes ensureNodeAffinityPin to return a generic error
	deps := naVerifyDeps(stub, false) // verify off so only the list-rules error fires
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	err := applyAZNodeAffinityPin(context.Background(), deps, 101, cp, "pve-node1", log.NewNopLogger())
	if err != nil {
		t.Fatalf("generic HA failure must be swallowed by applyAZNodeAffinityPin; got %v", err)
	}
}
