package ha

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func holdCfg(mode string) *config.CPIConfig {
	return eligCfg(mode, withPolicy(config.HALocalPolicyHold))
}

func linked(id string) VMVolume { return VMVolume{Volume: id, LinkedClone: true} }

func TestHoldVMInAZGetsAStrictSelfPin(t *testing.T) {
	az := []string{"pve1", "pve2"}
	disks := vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1")
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode+" strict AZ", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			in := baseInput(holdCfg(mode), disks...)
			in.AZ, in.AZNodes = "az1", az
			d := mustDecide(t, p, in)
			if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) {
				t.Fatalf("decision = %+v", d)
			}
			rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
			if err != nil || !rule.Strict || !reflect.DeepEqual(rule.NodeNames(), []string{"pve1"}) {
				t.Fatalf("pin = %+v, %v; the AZ must not widen a hold pin", rule, err)
			}
		})
	}
	t.Run("a strict AZ that excludes the placed node is a permanent refusal", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(holdCfg(config.HAModeAuto), disks...)
		in.AZ, in.AZNodes = "az2", []string{"pve2", "pve3"}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip || d.Transient || !strings.Contains(d.Reason, "empty hard admissible set") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestHoldVMWithLinkedCloneOnLocalStorage(t *testing.T) {
	disks := []VMVolume{linked("local-lvm:vm-101-disk-0"), {Volume: "local-lvm:vm-101-disk-1"}}
	t.Run("auto drops HA with a warning", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(holdCfg(config.HAModeAuto), disks...))
		if d.Outcome != OutcomeSkip || d.Registers() || d.Transient {
			t.Fatalf("decision = %+v, want a permanent skip", d)
		}
		if !warned(d, "linked clone disks local-lvm:vm-101-disk-0 have a base template on node-local storage") {
			t.Fatalf("reason %q warnings %v must name only the linked disk", d.Reason, d.Warnings)
		}
	})
	t.Run("managed fails the call without a retry", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(holdCfg(config.HAModeManaged), disks...))
		if d.Outcome != OutcomeFail || d.Transient || d.Failure() == nil {
			t.Fatalf("decision = %+v, want a permanent failure", d)
		}
		if cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) || !strings.Contains(d.Err.Error(), "clone_mode") {
			t.Fatalf("err = %v, want a non-retriable error that names clone_mode", d.Err)
		}
	})
	t.Run("a linked clone on shared storage with a local persistent disk still holds", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(holdCfg(config.HAModeAuto),
			linked("nfs:base-9000-disk-0/vm-101-disk-0.qcow2"), VMVolume{Volume: "local-lvm:vm-101-disk-1"}))
		if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a linked clone on shared storage keeps full HA", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(holdCfg(config.HAModeManaged),
			linked("nfs:base-9000-disk-0/vm-101-disk-0.qcow2")))
		if !d.Registers() || d.Policy != PolicyShared {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an ISO is never a linked clone disk", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(holdCfg(config.HAModeAuto),
			VMVolume{Volume: "local-lvm:vm-101-disk-0"}, VMVolume{Volume: "local:iso/ci.iso", Kind: VolumeISO, LinkedClone: true}))
		if !d.Registers() || d.Policy != PolicyHold {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("the policy none does not look at the clone", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), disks...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "local_policy is none") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

// A VM that runs under HA on shared storage and receives a node-local
// persistent disk follows the hold policy: it stays registered, confined to
// its own node.
func TestHoldVMWhenALocalDiskArrivesOnAHAManagedVM(t *testing.T) {
	shared := VMVolume{Volume: "nfs:100/vm-101-disk-0"}
	local := VMVolume{Volume: "local-lvm:vm-101-disk-1"}
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			in := managedInput(mode, shared, local)
			in.Config = holdCfg(mode)
			d := mustDecide(t, p, in)
			if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) ||
				!reflect.DeepEqual(d.Admissible, []string{"pve1"}) {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
	t.Run("a strict pin that already holds the node is kept inside the hold set", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, shared, local)
		in.Config = holdCfg(config.HAModeAuto)
		in.Constraints = []NodeConstraint{{Name: "pin", Nodes: []string{"pve1", "pve2"}, Pin: true}}
		d := mustDecide(t, p, in)
		if !d.Registers() || !reflect.DeepEqual(d.Admissible, []string{"pve1"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a VM outside its strict pin still defers and is never unregistered", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, shared, local)
		in.Config = holdCfg(config.HAModeAuto)
		in.Constraints = []NodeConstraint{{Name: "pin", Nodes: []string{"pve2", "pve3"}, Pin: true}}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !d.Transient || !cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("decision = %+v, want a retriable deferral", d)
		}
	})
	t.Run("a node below the PVE 9 floor still defers", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		versions(p, map[string]string{"pve3": "8.4.1"})
		in := managedInput(config.HAModeAuto, shared, local)
		in.Config = holdCfg(config.HAModeAuto)
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !d.Transient || !cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("decision = %+v, want a retriable deferral", d)
		}
	})
	t.Run("a disk on a node other than the VM's node leaves no hold pin", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, shared, VMVolume{Volume: "local-lvm:vm-101-disk-1", HomeNode: "pve2"})
		in.Config = holdCfg(config.HAModeAuto)
		d := mustDecide(t, p, in)
		if d.Registers() || d.Transient {
			t.Fatalf("decision = %+v, want a permanent refusal", d)
		}
	})
}
