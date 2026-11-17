package ha

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// strictPin is the strict pin of VM 101 over nodes, as a call that decides a
// VM under HA again states it.
func strictPin(nodes ...string) NodeConstraint {
	return NodeConstraint{Name: "the VM's pin bosh-na-101", Nodes: nodes, Pin: true}
}

// TestDecideVMStrictPinBoundsAfterTheSharedCheck checks that the VM's own strict
// pin narrows the admissible set after the shared check, the way a strict AZ
// does, and never the access set the shared check counts. A VM under HA whose
// pin names one node, which is what create_vm writes for a one-node strict AZ,
// stays eligible as shared.
func TestDecideVMStrictPinBoundsAfterTheSharedCheck(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0", "nfs:100/vm-101-disk-1")
	t.Run("a one-node pin keeps a shared VM registered on that node", func(t *testing.T) {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			in := managedInput(mode, disks...)
			in.Constraints = []NodeConstraint{strictPin("pve1")}
			d := mustDecide(t, p, in)
			if !d.Registers() || d.Policy != PolicyShared || !reflect.DeepEqual(d.Admissible, []string{"pve1"}) ||
				!reflect.DeepEqual(d.Hard, []string{"pve1"}) {
				t.Fatalf("%s: decision = %+v, want shared on pve1", mode, d)
			}
			if !reflect.DeepEqual(d.Access, []string{"pve1", "pve2", "pve3"}) {
				t.Errorf("%s: access %v; the pin must not narrow the access set", mode, d.Access)
			}
		}
	})
	t.Run("a two-node pin with one node down keeps both nodes", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		p.Nodes[1].Online = false
		in := managedInput(config.HAModeAuto, disks...)
		in.Constraints = []NodeConstraint{strictPin("pve1", "pve2")}
		d := mustDecide(t, p, in)
		if !d.Registers() || d.PlaceableShortfall || !reflect.DeepEqual(d.Admissible, []string{"pve1", "pve2"}) {
			t.Fatalf("decision = %+v, want shared on pve1 and pve2", d)
		}
	})
	t.Run("a pin decides as the strict AZ it carries", func(t *testing.T) {
		for _, az := range [][]string{{"pve1"}, {"pve1", "pve2"}} {
			for _, volumes := range [][]VMVolume{disks, vols("nfs:100/vm-101-disk-0", "nfs-ac:100/vm-101-disk-1")} {
				p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
				created := baseInput(eligCfg(config.HAModeAuto), volumes...)
				created.AZ, created.AZNodes = "az1", az
				fromAZ := mustDecide(t, p, created)
				again := managedInput(config.HAModeAuto, volumes...)
				again.Constraints = []NodeConstraint{strictPin(az...)}
				fromPin := mustDecide(t, p, again)
				if fromAZ.Outcome != fromPin.Outcome || fromAZ.Policy != fromPin.Policy ||
					!reflect.DeepEqual(fromAZ.Admissible, fromPin.Admissible) {
					t.Fatalf("AZ %v volumes %v: from the AZ %+v, from the pin %+v", az, volumes, fromAZ, fromPin)
				}
			}
		}
	})
	t.Run("a pin that the access set misses names the pin", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		p.Storages = append(p.Storages, pve.StorageInfo{Name: "nfs-bc", Type: pve.StorageTypeNFS,
			Nodes: []string{"pve2", "pve3"}, Content: "images"})
		in := managedInput(config.HAModeManaged, vols("nfs-bc:100/vm-101-disk-0")...)
		in.Constraints = []NodeConstraint{strictPin("pve1")}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "empty hard admissible set") ||
			!strings.Contains(d.Err.Error(), "the VM's pin bosh-na-101") {
			t.Fatalf("decision = %+v, want an empty set that names the pin", d)
		}
	})
}
