// Package handlers internal tests for the §7.39 consume-side SDN
// eventual-consistency gate wired into configureNICs.
package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// nrIntPtr is a local *int helper for NetworkResolveRetries assignments —
// as of Phase 1 that field is *int so an unset property (nil) can be
// distinguished from an explicit 0 (see config.CPIConfig.NetworkResolveRetries).
func nrIntPtr(i int) *int { return &i }

// nrParsed builds a minimal parsed-args with one dynamic NIC on the named bridge.
func nrParsed(bridge string) *createVMParsedArgs {
	return &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: map[string]any{nicCPKeyBridge: bridge}},
		},
	}
}

// TestConfigureNICs_GateOff_NoResolveCalls verifies that disabling the
// consume-side bridge-resolve gate (NetworkResolveRetries=0) makes zero
// node-network polls. It does NOT disable the separate SDN vnet-membership
// listing that decides mtu=1 inheritance and vlan/tag membership — that
// listing runs whenever the VM has NICs, in every mode and regardless of
// this gate (see TestConfigureNICs_MTU_BridgeMode_* for the
// membership behavior itself), so exactly one ListSdnVnets call is expected
// here.
func TestConfigureNICs_GateOff_NoResolveCalls(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = nrIntPtr(0) // explicit 0: gate off (Phase 1 opt-out)
	cl := &fwClusterStub{sdnVnets: []string{"v1"}}
	nd := &fwNodesStub{nodeIfaces: []string{"v1"}}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("v1"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if cl.listSdnVnetsCall != 1 {
		t.Errorf("vnet-membership listing must still run once (mtu/vlan, decoupled from this gate); got %d calls",
			cl.listSdnVnetsCall)
	}
	if nd.listNetCalls != 0 {
		t.Errorf("gate off must make no node-network resolve calls; got %d", nd.listNetCalls)
	}
	if nd.lastNet == nil {
		t.Error("UpdateQemuConfig must still run when the gate is off")
	}
}

func TestConfigureNICs_GateOn_BridgePresent_Proceeds(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = nrIntPtr(3)
	cl := &fwClusterStub{sdnVnets: []string{"v1"}} // v1 is SDN-managed
	nd := &fwNodesStub{nodeIfaces: []string{"vmbr0", "v1"}}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("v1"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if nd.listNetCalls != 1 {
		t.Errorf("want 1 node-network poll (resolves first try), got %d", nd.listNetCalls)
	}
	if nd.lastNet == nil {
		t.Error("UpdateQemuConfig must run after the bridge resolves")
	}
}

func TestConfigureNICs_GateOn_NonSDNBridge_Skips(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = nrIntPtr(3)
	// vmbr0 is not in the SDN vnet set → external bridge → gate skips it.
	cl := &fwClusterStub{sdnVnets: []string{"v1"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("vmbr0"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if nd.listNetCalls != 0 {
		t.Errorf("external bridge must not poll node network; got %d polls", nd.listNetCalls)
	}
	if nd.lastNet == nil {
		t.Error("UpdateQemuConfig must run for an external bridge")
	}
}

// TestConfigureNICs_GateDefaultUnset_Active proves the Phase 1 acceptance
// criterion at the full configureNICs level: a config that leaves
// NetworkResolveRetries entirely unset (nil — the shape an empty-manifest
// property materializes as; deliberately NOT icMinConfig, which zeroes this
// field for the safety of unrelated tests elsewhere in this package) resolves
// the gate to ACTIVE by default. The bridge is present on the first poll so
// the test resolves immediately regardless of the retry budget (30) — the
// numeric default itself is unit-tested directly against the accessor in
// config_test.go's TestNetworkResolveAccessors; this test proves the gate
// actually RUNS (listNetCalls > 0) rather than silently passing through as it
// would pre-Phase-1.
func TestConfigureNICs_GateDefaultUnset_Active(t *testing.T) {
	cfg := &config.CPIConfig{
		Host:           "pve.test.local",
		Port:           8006,
		User:           "root",
		APIToken:       "test-token",
		Node:           "pve1",
		VMStorage:      "local-lvm",
		DiskStorage:    "local-lvm",
		NetworkBridge:  "vmbr0",
		AgentMode:      "noagent",
		VMDiskFormat:   "qcow2",
		VMIDRangeStart: 100,
		// NetworkResolveRetries intentionally left nil (unset) — this is the
		// property under test.
	}
	cl := &fwClusterStub{sdnVnets: []string{"v1"}} // v1 is SDN-managed
	nd := &fwNodesStub{nodeIfaces: []string{"vmbr0", "v1"}}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("v1"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if nd.listNetCalls == 0 {
		t.Error("default-unset config must leave the SDN resolve gate ACTIVE (Phase 1); got 0 node-network polls")
	}
	if nd.lastNet == nil {
		t.Error("UpdateQemuConfig must run after the bridge resolves")
	}
}

func TestConfigureNICs_GateOn_BridgeAbsent_RetriableNoWrite(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = nrIntPtr(1) // 1 retry → at most one ~1s sleep
	cl := &fwClusterStub{sdnVnets: []string{"v1"}}
	nd := &fwNodesStub{nodeIfaces: []string{"vmbr0"}} // v1 never appears
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("v1"), shape, 100)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("absent SDN bridge: want retriable-cloud, got %v", err)
	}
	if nd.lastNet != nil {
		t.Error("UpdateQemuConfig must NOT run when a bridge fails to resolve (no partial netN=)")
	}
}
