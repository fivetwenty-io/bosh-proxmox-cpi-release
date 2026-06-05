// Internal tests proving that the create_vm call-site routing functions —
// applyAntiAffinityMembership and applyAZNodeAffinityPin — forward
// TypeRetriableCloud errors to the director while swallowing generic HA blips as
// best-effort warnings. The tests drive the REAL routing functions end-to-end
// rather than a test-local copy of the conditional, so a drift between the test
// and the production routing cannot pass unnoticed.
package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

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

// TestApplyAntiAffinityMembership_RetriableLockLost_Propagates drives the REAL
// create_vm routing function end-to-end: a retriable error from the cluster-lock
// acquire must surface as a TypeRetriableCloud the director re-drives — proving
// the propagation at the actual call-site function, not a test-local mirror.
//
// The acquire fails fast here with a transport-class error (which the lock maps
// to retriable without entering its poll loop), so the test stays deterministic
// with no real sleep. The held-lock → wait → timeout → retriable mechanic itself
// is proven against a fake clock in the internal/pve cluster-lock tests.
func TestApplyAntiAffinityMembership_RetriableLockLost_Propagates(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	events := []string{}
	lockPools := newAALockPools(&events)
	// A non-duplicate create failure (transport/pmxcfs fault) is classified
	// retriable immediately, no poll loop.
	lockPools.createErr = func(_ string) error { return fmt.Errorf("pmxcfs unavailable") }

	cfg := aaMembershipConfig("pool", false, 1)
	deps := aaDepsLock(cfg, stub, lockPools)
	err := applyAntiAffinityMembership(context.Background(), deps, 101, aaEnvForGroup("web"), log.NewNopLogger())
	if err == nil {
		t.Fatal("applyAntiAffinityMembership must propagate the lock-acquire retriable to the director")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("must be TypeRetriableCloud; got %v", err)
	}
}

// TestApplyAntiAffinityMembership_RetriableVerify_Propagates proves the same real
// routing function forwards a verify-absent retriable: when a concurrent writer
// drops the new member from the recreated rule, the TypeRetriableCloud from the
// read-after-write verify must reach the director.
func TestApplyAntiAffinityMembership_RetriableVerify_Propagates(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, "job--web"))
	stub.dropMemberOnRecreate = "vm:101" // concurrent drop → verify sees member absent

	cfg := aaMembershipConfig("off", true, 0) // verify on, lock off
	deps := aaDepsLock(cfg, stub, newAALockPools(nil))
	err := applyAntiAffinityMembership(context.Background(), deps, 101, aaEnvForGroup("web"), log.NewNopLogger())
	if err == nil {
		t.Fatal("applyAntiAffinityMembership must propagate the verify-absent retriable to the director")
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
	stub.failListRules = true         // causes ensureNodeAffinityPin to return a generic error
	deps := naVerifyDeps(stub, false) // verify off so only the list-rules error fires
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	err := applyAZNodeAffinityPin(context.Background(), deps, 101, cp, "pve-node1", log.NewNopLogger())
	if err != nil {
		t.Fatalf("generic HA failure must be swallowed by applyAZNodeAffinityPin; got %v", err)
	}
}
