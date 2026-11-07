package handlers

import (
	"context"
	"slices"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

func localDisk() resolvedDisk {
	return resolvedDisk{volid: testPinStorage + ":vm-100-disk-1", stableID: "abcd1234"}
}

func TestDecideAttachHA_LocalDiskUnderHoldKeepsTheVMInHAPinnedToItsNode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
		nodes  string
	}{
		{"non-strict pin", false, "pve01,pve02,pve03"},
		{"strict AZ pin", true, "pve01,pve02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, stub, _ := attachRig(config.HAModeAuto)
			deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
			stub.pin(tc.strict, tc.nodes, "nfs")
			h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h == nil || h.action != attachHAGuarded || h.newPin == nil {
				t.Fatalf("h %+v, want a guarded attach with a new pin", h)
			}
			if !h.newPin.Strict || !slices.Equal(h.newPin.NodeNames(), []string{"pve01"}) {
				t.Fatalf("new pin %v strict %v, want a strict pin to pve01 only", h.newPin.NodeNames(),
					h.newPin.Strict)
			}
			rec, ok := ha.VolumeRecord(h.newPin.Comment)
			if !ok || !slices.Contains(rec, testPinStorage) || !slices.Contains(rec, "nfs") {
				t.Errorf("record %v, want nfs and %s", rec, testPinStorage)
			}
			if h.node != "pve01" {
				t.Errorf("node %q, want pve01", h.node)
			}
		})
	}
}

func TestDecideAttachHA_LocalDiskUnderHoldInManagedMode(t *testing.T) {
	// The disk is on the node the VM runs on, so managed mode holds the VM
	// there the same way auto mode does.
	deps, stub, _ := attachRig(config.HAModeManaged)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("h %+v err %v, want a guarded attach under managed mode", h, err)
	}
	if !h.newPin.Strict || !slices.Equal(h.newPin.NodeNames(), []string{"pve01"}) {
		t.Fatalf("new pin %v strict %v, want a strict pin to pve01 only", h.newPin.NodeNames(), h.newPin.Strict)
	}
}
