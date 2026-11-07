package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// holdRig returns a staging rig for VM 100 whose root disk sits on node-local
// storage under local_policy hold, with the decision the eligibility rules
// reach for it.
func holdRig(t *testing.T, mode string) *stagingRig {
	t.Helper()
	r := newStagingRig(t, mode, true)
	r.deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	profile, err := r.deps.HAProfile(context.Background())
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	nodes, _ := r.deps.Config.AZCandidates("z1")
	d, err := profile.DecideVM(ha.VMInput{
		Config: r.deps.Config, VMID: 100, Node: "pve01", AZ: "z1", AZNodes: nodes,
		Volumes: []ha.VMVolume{{Volume: testPinStorage + ":vm-100-disk-0"}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !d.Registers() || d.Policy != ha.PolicyHold || !slices.Equal(d.Hard, []string{"pve01"}) {
		t.Fatalf("decision = %+v, want a hold registration hard-pinned to pve01", d)
	}
	r.decision = d
	return r
}

func TestHoldVMInAnAZIsPinnedToItsOwnNode(t *testing.T) {
	r := holdRig(t, config.HAModeAuto)
	r.stage(t)
	landing, err := r.confineAndActivate()
	if err != nil || landing != "pve01" || !r.h.haActivated() {
		t.Fatalf("landing %q err %v activated %v", landing, err, r.h.haActivated())
	}
	pin := r.rules.rules["bosh-na-100"]
	if pin.Nodes == nil || *pin.Nodes != "pve01" {
		t.Fatalf("pin nodes %v: the AZ must not widen a hold VM's pin", pin.Nodes)
	}
	if pin.Strict == nil || !*pin.Strict {
		t.Fatalf("pin strict %v, want a strict pin", pin.Strict)
	}
	want := []string{"stage:ignored", "pin:bosh-na-100", "request:started", "wait:pve01"}
	if !slices.Equal(r.log.events, want) {
		t.Fatalf("events %v, want %v", r.log.events, want)
	}
}

func TestHoldVMWhosePinFailsVerificationIsNeverActivated(t *testing.T) {
	widen := func(r *stagingRig) {
		p := r.rules.rules["bosh-na-100"]
		nodes := "pve01,pve02"
		p.Nodes = &nodes
		r.rules.rules["bosh-na-100"] = p
	}
	loosen := func(r *stagingRig) {
		p := r.rules.rules["bosh-na-100"]
		off := false
		p.Strict = &off
		r.rules.rules["bosh-na-100"] = p
	}
	for _, tc := range []struct {
		name   string
		tamper func(*stagingRig)
	}{
		{"pin disabled after the write", (*stagingRig).disablePin},
		{"pin widened past the node", widen},
		{"pin no longer strict", loosen},
		{"pin removed", func(r *stagingRig) { delete(r.rules.rules, "bosh-na-100") }},
	} {
		t.Run(tc.name+" in auto runs the VM without HA", func(t *testing.T) {
			r := holdRig(t, config.HAModeAuto)
			r.stage(t)
			ctx := context.Background()
			if err := r.h.confine(ctx, r.parsed, "pve01"); err != nil {
				t.Fatalf("confine: %v", err)
			}
			tc.tamper(r)
			if _, err := r.h.activate(ctx, "pve01", r.start); err != nil {
				t.Fatalf("auto must fall back to a direct start: %v", err)
			}
			if slices.Contains(r.log.events, "request:started") || r.h.haActivated() {
				t.Fatalf("events %v: an unconfined hold VM must never be activated", r.log.events)
			}
			want := []string{"stage:ignored", "pin:bosh-na-100", "unstage", "unpin:bosh-na-100", "start-direct"}
			if tc.name == "pin removed" {
				want = slices.Delete(want, 3, 4)
			}
			if !slices.Equal(r.log.events, want) {
				t.Fatalf("events %v, want %v", r.log.events, want)
			}
		})
		t.Run(tc.name+" in managed fails the call", func(t *testing.T) {
			r := holdRig(t, config.HAModeManaged)
			r.stage(t)
			ctx := context.Background()
			if err := r.h.confine(ctx, r.parsed, "pve01"); err != nil {
				t.Fatalf("confine: %v", err)
			}
			tc.tamper(r)
			_, err := r.h.activate(ctx, "pve01", r.start)
			if !errors.Is(err, ha.ErrConfinementUnverified) {
				t.Fatalf("err = %v, want ErrConfinementUnverified", err)
			}
			if slices.Contains(r.log.events, "request:started") || r.started != 0 || r.h.haActivated() {
				t.Fatalf("events %v: an unconfined hold VM must never be activated", r.log.events)
			}
		})
	}
}

func TestHoldVMWhosePinCannotBeWrittenIsNeverActivated(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			r := holdRig(t, mode)
			r.rules.ruleErr = errors.New("cfs lock timeout")
			r.stage(t)
			if _, err := r.confineAndActivate(); err == nil {
				t.Fatal("a failed pin write must fail the call")
			}
			if slices.Contains(r.log.events, "request:started") || r.started != 0 || r.h.haActivated() {
				t.Fatalf("events %v: nothing may start without its confinement", r.log.events)
			}
			if strings.Contains(strings.Join(r.log.events, ","), "wait:") {
				t.Fatalf("events %v: no state wait without a pin", r.log.events)
			}
		})
	}
}

// holdDecideRig is decideRig for a VM whose root disk is on node-local storage
// under local_policy hold.
func holdDecideRig(mode string) (Deps, *stagingNAStub, *createVMParsedArgs) {
	deps, rules, parsed := decideRig(mode, aaDirector, nil)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) {
			return map[string]any{"scsi0": testPinStorage + ":vm-100-disk-0,size=10G", "cpu": "x86-64-v3"}, nil
		},
	}
	return deps, rules, parsed
}

func TestDecideCreatedVMHA_HoldRegistersALocalVMWithAStrictSelfPin(t *testing.T) {
	deps, rules, parsed := holdDecideRig(config.HAModeAuto)
	h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100)
	if err != nil || h == nil {
		t.Fatalf("h %v err %v, want a staged hold VM", h, err)
	}
	d := h.decision
	if !d.Registers() || d.Policy != ha.PolicyHold || !slices.Equal(d.Hard, []string{"pve01"}) {
		t.Fatalf("decision = %+v, want a hold registration hard-pinned to pve01 although the AZ has pve02", d)
	}
	if !h.staged() || !rules.resources["vm:100"] {
		t.Fatalf("staged %v resources %v, want vm:100 staged", h.staged(), rules.resources)
	}
	if !slices.Equal(h.storages, []string{testPinStorage}) {
		t.Errorf("storages %v, want [%s]", h.storages, testPinStorage)
	}
}

func TestDecideCreatedVMHA_HoldWithALinkedCloneOnLocalStorage(t *testing.T) {
	t.Run("auto runs the VM without HA", func(t *testing.T) {
		deps, rules, parsed := holdDecideRig(config.HAModeAuto)
		h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
			&createVMShape{node: "pve01", linkedClone: true, rootDiskKey: "scsi0"}, 100)
		if err != nil || h != nil && h.staged() || len(rules.resources) != 0 {
			t.Fatalf("h %v err %v resources %v, want no HA and nothing staged", h, err, rules.resources)
		}
	})
	t.Run("managed fails the call and stages nothing", func(t *testing.T) {
		deps, rules, parsed := holdDecideRig(config.HAModeManaged)
		_, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
			&createVMShape{node: "pve01", linkedClone: true, rootDiskKey: "scsi0"}, 100)
		if err == nil || !strings.Contains(err.Error(), "clone_mode") || len(rules.resources) != 0 {
			t.Fatalf("err = %v resources %v, want a clone_mode failure", err, rules.resources)
		}
		if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err = %v: a linked clone on local storage is not worth a retry", err)
		}
	})
}
