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

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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

// TestConfigureNICs_MTU_BridgeMode_ListsVnetsButNoMatch verifies the mode-gating
// fix: under network_mode=bridge, configureNICs still lists SDN vnets — vnet
// membership for mtu=1 inheritance is decided by the actual vnet list in
// every mode, not by network_mode — but a bridge that does not actually name
// a vnet (vmbr0, not in the vnet list) still gets no mtu.
func TestConfigureNICs_MTU_BridgeMode_ListsVnetsButNoMatch(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "bridge"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("vmbr0", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if cl.listSdnVnetsCall != 1 {
		t.Errorf("bridge mode must still list SDN vnets for membership; got %d calls", cl.listSdnVnetsCall)
	}
	if strings.Contains(nd.lastNet[0], "mtu=") {
		t.Errorf("vmbr0 is not a vnet; must not carry mtu even though the list ran; got %q", nd.lastNet[0])
	}
}

// TestConfigureNICs_MTU_BridgeMode_VnetBridge_Inherits is the direct
// regression test: a NIC attached to a bridge name that IS a real
// pre-existing SDN vnet must still get mtu=1 under network_mode=bridge — the
// mode governs create_network/delete_network routing, not this NIC
// attribute. Before the fix, sdnVnetNameSet skipped the listing entirely
// under bridge mode and this NIC silently lost MTU inheritance.
// TestConfigureNICs_MTU_PerNIC_Malformed_Rejected verifies that an mtu key
// PRESENT with a non-integer value (null, bool, array, unparseable string) is
// rejected with a Cloud error naming the network and key, rather than being
// silently treated as absent.
func TestConfigureNICs_MTU_PerNIC_Malformed_Rejected(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{"null", nil},
		{"bool", true},
		{"array", []any{9000}},
		{"unparseable string", "jumbo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := icMinConfig()
			deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
			shape := &createVMShape{node: "pve1"}
			parsed := &createVMParsedArgs{
				networks: map[string]createVMNetworkSpec{
					"default": {Type: "dynamic", CloudProperties: map[string]any{
						nicCPKeyBridge: "vmbr0",
						nicCPKeyMTU:    tc.val,
					}},
				},
			}

			_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
			if err == nil {
				t.Fatalf("mtu=%v (present, malformed) must be rejected, not silently dropped", tc.val)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
			}
			for _, want := range []string{"default", "mtu"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must name the network and key %q", err.Error(), want)
				}
			}
		})
	}
}

// TestConfigureNICs_MTU_NetworkDefaults_Malformed_Rejected is the
// network_defaults counterpart of TestConfigureNICs_MTU_PerNIC_Malformed_Rejected.
func TestConfigureNICs_MTU_NetworkDefaults_Malformed_Rejected(t *testing.T) {
	cfg := icMinConfig()
	parsed := mtuParsed("vmbr0", "")
	parsed.cloudProps.NetworkDefaults = map[string]any{nicCPKeyMTU: false}
	deps := fwDeps(&fwClusterStub{}, &fwNodesStub{}, cfg)
	shape := &createVMShape{node: "pve1"}

	_, err := configureNICs(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
	if err == nil {
		t.Fatal("network_defaults.mtu malformed must be rejected, not silently dropped")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected non-retriable CloudError, got %T: %v", err, err)
	}
	for _, want := range []string{"default", "mtu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name the network and key %q", err.Error(), want)
		}
	}
}

// --------------------------------------------------------------------------
// Explicit vlan tag on a bridge that is itself an SDN vnet: warn, don't fail.
// --------------------------------------------------------------------------

// TestConfigureNICs_VLANOnSDNVnet_Warns verifies that setting an explicit
// vlan on a bridge that is a known SDN vnet logs a Warn naming the network,
// bridge, and vlan — mixing Pattern A (trunk bridge + NIC tag) on top of
// Pattern B (SDN vnet-per-VLAN) — without failing the create.
func TestConfigureNICs_VLANOnSDNVnet_Warns(t *testing.T) {
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

	parsed := &createVMParsedArgs{
		networks: map[string]createVMNetworkSpec{
			"default": {Type: "dynamic", CloudProperties: map[string]any{
				nicCPKeyBridge: "boshvnet",
				nicCPKeyVLAN:   100,
			}},
		},
	}
	if _, err := configureNICs(context.Background(), deps, logger, parsed, shape, 100); err != nil {
		t.Fatalf("configureNICs: vlan-on-vnet must warn, not fail: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "explicit vlan tag on a bridge that is itself an SDN vnet") {
		t.Errorf("expected vlan-on-vnet warning, got log output: %s", out)
	}
	for _, want := range []string{"\"default\"", "\"boshvnet\"", "100"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing expected field %s; got: %s", want, out)
		}
	}
}

// TestConfigureNICs_VLANOnExternalBridge_NoWarn verifies that an explicit
// vlan on a plain (non-vnet) bridge — the ordinary Pattern A case — logs no
// vlan/vnet-mixing warning.
func TestConfigureNICs_VLANOnExternalBridge_NoWarn(t *testing.T) {
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

	if _, err := configureNICs(context.Background(), deps, logger, vlanParsed("vmbr0", 100, nil, ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if strings.Contains(buf.String(), "explicit vlan tag on a bridge that is itself an SDN vnet") {
		t.Errorf("vlan on a plain external bridge must not warn; got log output: %s", buf.String())
	}
}

func TestConfigureNICs_MTU_BridgeMode_VnetBridge_Inherits(t *testing.T) {
	cfg := icMinConfig()
	cfg.NetworkMode = "bridge"
	cl := &fwClusterStub{sdnVnets: []string{"boshvnet"}}
	nd := &fwNodesStub{}
	deps := fwDeps(cl, nd, cfg)
	shape := &createVMShape{node: "pve1"}

	if _, err := configureNICs(context.Background(), deps, log.NewNopLogger(), mtuParsed("boshvnet", ""), shape, 100); err != nil {
		t.Fatalf("configureNICs: %v", err)
	}
	if !strings.Contains(nd.lastNet[0], ",mtu=1") {
		t.Errorf("bridge-mode NIC on a real SDN vnet must still inherit mtu=1; got %q", nd.lastNet[0])
	}
}
