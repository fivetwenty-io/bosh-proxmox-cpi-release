package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// setNodeVersions sets the pve-manager version of each named node.
func setNodeVersions(p *ha.Profile, byNode map[string]string) {
	for i := range p.Nodes {
		if v, ok := byNode[p.Nodes[i].Name]; ok {
			p.Nodes[i].Version = v
		}
	}
}

// assertAttachDeferred fails the test unless the attach decision deferred with
// a retriable error naming every part of want and changed nothing in HA.
func assertAttachDeferred(t *testing.T, h *attachHA, err error, stub *attachHAStub, want ...string) {
	t.Helper()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
		t.Fatalf("h %+v err %v, want a retriable deferral", h, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err %q does not name %q", err, w)
		}
	}
	if stub.resDelCalls+stub.deleteCalls+stub.createCalls+stub.updateCalls != 0 {
		t.Fatalf("a deferral must write nothing to HA: resource deletes %d, rule deletes %d, creates %d, updates %d",
			stub.resDelCalls, stub.deleteCalls, stub.createCalls, stub.updateCalls)
	}
}

// TestDecideAttachHA_DLBVMOnALowerVersionStaysInHA attaches a disk to a DLB VM
// on a cluster whose lowest node runs PVE 9.1. The VM keeps HA and its pin is
// guarded as for any other VM, and only the dynamic load balancer turns off.
func TestDecideAttachHA_DLBVMOnALowerVersionStaysInHA(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			deps, stub, profile := attachRig(mode)
			stub.rebalance = true
			stub.recordPin("nfs")
			setNodeVersions(profile, map[string]string{"pve02": "9.1.0"})
			profile.DLBSupported, profile.MinVersion = false, "9.1.0"
			h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h == nil || h.action != attachHAGuarded || h.newPin == nil {
				t.Fatalf("h %+v, want the VM to stay in HA with a guarded attach", h)
			}
			if h.decision.DLB {
				t.Fatal("the dynamic load balancer must be off below PVE 9.2")
			}
			if stub.resDelCalls != 0 || stub.deleteCalls != 0 {
				t.Fatalf("the VM must keep its HA resource and pin: resource deletes %d, rule deletes %d",
					stub.resDelCalls, stub.deleteCalls)
			}
		})
	}
}

// TestDecideAttachHA_NodeBelowPVE9KeepsTheRegistration attaches a disk to a VM
// under HA after a node below PVE 9 came online. The attach keeps the VM's HA
// registration as it is, writes nothing to HA, and names the node and its
// version, ahead of any other gap the profile reports.
func TestDecideAttachHA_NodeBelowPVE9KeepsTheRegistration(t *testing.T) {
	for name, rebalance := range map[string]bool{"a DLB VM": true, "a VM without DLB": false} {
		t.Run(name, func(t *testing.T) {
			deps, stub, profile := attachRig(config.HAModeAuto)
			stub.rebalance = rebalance
			stub.recordPin("nfs")
			setNodeVersions(profile, map[string]string{"pve03": "8.4.1"})
			profile.HAVersionOK, profile.DLBSupported, profile.MinVersion = false, false, "8.4.1"
			profile.Effective = ha.EffectiveObserveOnly
			profile.Gaps = []string{"missing Sys.Console on /", "PVE 8.4.1 is below the PVE 9 floor for HA writes"}
			profile.Reason = profile.Gaps[0]
			h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
			assertAttachDeferred(t, h, err, stub, "node pve03 (8.4.1)", "PVE 9", "writes nothing to HA")
		})
	}
	t.Run("an offline node of a known old version defers too", func(t *testing.T) {
		deps, stub, profile := attachRig(config.HAModeManaged)
		stub.recordPin("nfs")
		setNodeVersions(profile, map[string]string{"pve03": "8.3.0"})
		profile.Nodes[2].Online = false
		h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
		assertAttachDeferred(t, h, err, stub, "node pve03 (8.3.0)")
	})
}

// TestDecideAttachHA_UnknownCPUNamesTheOperatorStep attaches a disk to a host
// CPU VM whose node's CPU cannot be read. The attach defers, names the probe
// failure, and tells the operator to fix the node's CPU reporting or set
// pve.cpu_type to a named model.
func TestDecideAttachHA_UnknownCPUNamesTheOperatorStep(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			deps, stub, profile := attachRig(mode)
			stub.recordPin("nfs")
			hostCPUConfig(deps)
			profile.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"pve02", "pve03"}}}
			profile.UnknownCPUNodes = []string{"pve01"}
			profile.Nodes[0].CPUErr = "permission check failed (/nodes/pve01, Sys.Audit)"
			h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
			assertAttachDeferred(t, h, err, stub, "node pve01", "Sys.Audit", "Fix the CPU reporting of node pve01",
				"pve.cpu_type")
		})
	}
}

// TestDecideAttachHA_VMOutsideItsStrictPinDefers attaches a disk to a VM that a
// manual migration put on pve01, outside its strict pin over pve02 and pve03.
// The VM keeps HA, the attach writes nothing to HA, and the error names the VM,
// its node, and the pin's nodes, and both ways out.
func TestDecideAttachHA_VMOutsideItsStrictPinDefers(t *testing.T) {
	for name, disk := range map[string]resolvedDisk{
		"a shared disk": sharedDisk(),
		"a local disk":  {volid: testPinStorage + ":vm-100-disk-1", stableID: "abcd1234"},
	} {
		t.Run(name, func(t *testing.T) {
			deps, stub, _ := attachRig(config.HAModeAuto)
			stub.pin(true, "pve02,pve03", "nfs")
			h, err := decideAttachHA(context.Background(), deps, "100", disk)
			assertAttachDeferred(t, h, err, stub, "VM 100", "node pve01", "[pve02,pve03]", "Migrate the VM back",
				"widen the pin")
		})
	}
}

func TestAttachHAInput_MarksTheVMAndItsPin(t *testing.T) {
	cfg := icMinConfig()
	cfg.HA = haModeAuto()
	old := &ha.Rule{ID: "bosh-na-100", Nodes: map[string]int{"pve01": 0, "pve02": 0}, Strict: true}
	in := attachHAInput(Deps{Config: cfg}, 100, "pve01", map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G",
		"hostpci0": "0000:01:00.0"}, &cluster.GetHaResourcesResponse{}, old, resolvedDisk{volid: "nfs2:vm-100-disk-1"})
	if !in.HAManaged {
		t.Error("an attach decides for a VM that runs under HA")
	}
	for _, c := range in.Constraints {
		if want := c.Name == "the VM's pin bosh-na-100"; c.Pin != want {
			t.Errorf("constraint %q: pin %v, want %v", c.Name, c.Pin, want)
		}
	}
}
