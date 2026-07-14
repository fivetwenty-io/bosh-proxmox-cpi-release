// Package handlers internal tests for MTU inheritance on SDN-vnet NICs:
// configureNICs appends mtu=1 to virtio NICs attached to an SDN vnet so the
// guest inherits the (encapsulation-reduced) bridge MTU.
package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// mtuParsed builds a minimal parsed-args with one dynamic NIC on the named
// bridge and optional per-NIC model override.
func mtuParsed(bridge, model string) *createVMParsedArgs {
	cp := map[string]any{nicCPKeyBridge: bridge}
	if model != "" {
		cp[nicCPKeyModel] = model
	}
	return &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: cp},
		},
	}
}

func TestConfigureNICs_MTU_VnetVirtio_Inherits(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "sdn"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("boshvnet", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if nd.lastNet == nil {
		t.Fatal("UpdateQemuConfig did not run")
	}
	if !strings.Contains(nd.lastNet[0], ",mtu=1") {
		t.Errorf("vnet-attached virtio NIC must carry mtu=1; got %q", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_ExternalBridge_Absent(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "sdn"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("vmbr0", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(nd.lastNet[0], "mtu=") {
		t.Errorf("external-bridge NIC must not carry mtu; got %q", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_VnetE1000_Absent(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "sdn"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("boshvnet", "e1000"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(nd.lastNet[0], "mtu=") {
		t.Errorf("e1000 NIC must not carry mtu (PVE rejects it); got %q", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_ListError_FailOpen(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "sdn"
	cl := &fwClusterStub{sdnVnetsErr: errors.New("pvedaemon hiccup")}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("boshvnet", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs must fail open on vnet-list error: %v", err)
	}
	if nd.lastNet == nil {
		t.Fatal("UpdateQemuConfig did not run")
	}
	if strings.Contains(nd.lastNet[0], "mtu=") {
		t.Errorf("fail-open path must omit mtu; got %q", nd.lastNet[0])
	}
}

func TestConfigureNICs_MTU_BridgeMode_NoVnetListing(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "bridge"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("vmbr0", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if cl.listSdnVnetsCall != 0 {
		t.Errorf("bridge mode must not list SDN vnets; got %d calls", cl.listSdnVnetsCall)
	}
}
