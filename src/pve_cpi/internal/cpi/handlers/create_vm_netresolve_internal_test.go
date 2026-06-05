// Package handlers internal tests for the §7.39 consume-side SDN
// eventual-consistency gate wired into configureNICs.
package handlers

import (
	"context"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// nrParsed builds a minimal parsed-args with one dynamic NIC on the named bridge.
func nrParsed(bridge string) *createVMParsedArgs {
	return &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: map[string]any{nicCPKeyBridge: bridge}},
		},
	}
}

func TestConfigureNICs_GateOff_NoResolveCalls(t *testing.T) {
	cfg := icMinConfig() // NetworkResolveRetries defaults to 0 → gate off
	cl := &fwClusterStub{sdnVnets: []string{"v1"}}
	nd := &fwNodesStub{nodeIfaces: []string{"v1"}}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), nrParsed("v1"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if cl.listSdnVnetsCall != 0 || nd.listNetCalls != 0 {
		t.Errorf("gate off must make no resolve calls; got listSdnVnets=%d listNetwork=%d",
			cl.listSdnVnetsCall, nd.listNetCalls)
	}
	if nd.lastNet == nil {
		t.Error("UpdateQemuConfig must still run when the gate is off")
	}
}

func TestConfigureNICs_GateOn_BridgePresent_Proceeds(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = 3
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
	cfg.NetworkResolveRetries = 3
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

func TestConfigureNICs_GateOn_BridgeAbsent_RetriableNoWrite(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkResolveRetries = 1 // 1 retry → at most one ~1s sleep
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
