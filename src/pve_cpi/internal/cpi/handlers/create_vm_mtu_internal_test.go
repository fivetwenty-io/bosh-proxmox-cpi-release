// Package handlers internal tests for MTU inheritance on SDN-vnet NICs:
// configureNICs appends mtu=1 to virtio NICs attached to an SDN vnet so the
// guest inherits the (encapsulation-reduced) bridge MTU.
package handlers

import (
	"bytes"
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
	cfg.NetworkMode = networkModeSDN
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
	cfg.NetworkMode = networkModeSDN
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
	cfg.NetworkMode = networkModeSDN
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

// TestConfigureNICs_MTU_VnetE1000_WarnsAboutMTU verifies that a non-virtio
// NIC model on an SDN vnet logs a Warn naming the NIC (network name), model,
// and vnet — the guest will not auto-track the vnet's MTU, the root cause of
// the "small packets pass, large packets hang" trap.
func TestConfigureNICs_MTU_VnetE1000_WarnsAboutMTU(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = networkModeSDN
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}

	if _, err := configureNICs(context.Background(), deps, logger, mtuParsed("boshvnet", "e1000"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "non-virtio NIC model on an SDN vnet") {
		t.Errorf("expected MTU-mismatch warning, got log output: %s", out)
	}
	for _, want := range []string{"\"default\"", "\"e1000\"", "\"boshvnet\""} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing expected field %s; got: %s", want, out)
		}
	}
}

// TestConfigureNICs_MTU_VnetVirtio_NoWarn verifies the default virtio model
// on an SDN vnet — the normal, MTU-inheriting case — logs no MTU warning.
func TestConfigureNICs_MTU_VnetVirtio_NoWarn(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = networkModeSDN
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}

	if _, err := configureNICs(context.Background(), deps, logger, mtuParsed("boshvnet", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(buf.String(), "non-virtio NIC model on an SDN vnet") {
		t.Errorf("virtio-on-vnet must not warn; got log output: %s", buf.String())
	}
}

// TestConfigureNICs_MTU_ExternalBridge_E1000_NoWarn verifies that a
// non-virtio model on a plain (non-SDN) bridge — where MTU inheritance was
// never applicable in the first place — logs no MTU warning either.
func TestConfigureNICs_MTU_ExternalBridge_E1000_NoWarn(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = networkModeSDN
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}

	if _, err := configureNICs(context.Background(), deps, logger, mtuParsed("vmbr0", "e1000"), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(buf.String(), "non-virtio NIC model on an SDN vnet") {
		t.Errorf("e1000-on-plain-bridge must not warn; got log output: %s", buf.String())
	}
}

func TestConfigureNICs_MTU_ListError_FailOpen(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = networkModeSDN
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
