package ha

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func withFailback(on bool) func(*config.HAConfig) {
	return func(h *config.HAConfig) { h.Failback = &on }
}

func TestDecideVMFailbackFollowsThePolicy(t *testing.T) {
	cases := map[string]struct {
		cfg    *config.CPIConfig
		vols   []VMVolume
		policy Policy
		want   bool
	}{
		"an all-shared VM gets failback by default": {
			eligCfg(config.HAModeAuto), vols("nfs:100/vm-101-disk-0", "nfs:100/vm-101-disk-1"), PolicyShared, true,
		},
		"an all-shared VM with a shared ISO gets failback": {
			eligCfg(config.HAModeManaged), append(vols("nfs:100/vm-101-disk-0"), iso("nfs:iso/ci.iso")),
			PolicyShared, true,
		},
		"an explicit failback true reads as the default": {
			eligCfg(config.HAModeAuto, withFailback(true)), vols("nfs:100/vm-101-disk-0"), PolicyShared, true,
		},
		"failback false keeps an all-shared VM at 0": {
			eligCfg(config.HAModeAuto, withFailback(false)), vols("nfs:100/vm-101-disk-0"), PolicyShared, false,
		},
		"a hold VM with a local disk keeps failback off": {
			holdCfg(config.HAModeAuto), vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1"), PolicyHold, false,
		},
		"a replicate VM keeps failback off": {
			replicateCfg(config.HAModeAuto), vols("tank:vm-101-disk-0", "nfs:100/vm-101-data"), PolicyReplicate,
			false,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := mustDecide(t, eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3"), baseInput(c.cfg, c.vols...))
			if !d.Registers() || d.Policy != c.policy {
				t.Fatalf("decision = %+v, want a %v registration", d, c.policy)
			}
			if d.Failback != c.want {
				t.Errorf("Failback = %v, want %v", d.Failback, c.want)
			}
		})
	}
}

func TestDecideVMSharedVMOnASingleReachingNodeHoldsWithoutFailback(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	p.Nodes[1].Online, p.Nodes[2].Online = false, false
	d := mustDecide(t, p, baseInput(holdCfg(config.HAModeAuto), vols("nfs:100/vm-101-disk-0")...))
	if d.Registers() && d.Failback {
		t.Fatalf("decision = %+v; a VM that cannot move to a second node must not get failback", d)
	}
}

// TestDecideVMSelfPinnedSharedVMKeepsFailbackOff decides an all-shared VM
// under its own strict one-node pin, the pin a hold VM keeps after its last
// node-local disk is gone. The admissible set holds that one node, so the
// decision registers the VM under the shared policy with nowhere to fail back
// to, and failback must stay off.
func TestDecideVMSelfPinnedSharedVMKeepsFailbackOff(t *testing.T) {
	in := baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/vm-101-disk-0")...)
	in.Constraints = append(in.Constraints, NodeConstraint{Name: "the VM's pin bosh-na-101", Nodes: []string{"pve1"},
		Pin: true})
	d := mustDecide(t, eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3"), in)
	if !d.Registers() {
		t.Fatalf("decision = %+v, want a registration", d)
	}
	if d.Failback {
		t.Fatalf("decision = %+v; a VM whose admissible set holds one node must not get failback", d)
	}
}

func TestProfileStorageSharedClassifiesLikeTheDecision(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	cases := map[string]struct {
		cfg           *config.CPIConfig
		storage       string
		shared, known bool
	}{
		"a shared storage":                      {eligCfg(config.HAModeAuto), "nfs", true, true},
		"a node-local storage":                  {eligCfg(config.HAModeAuto), "local-lvm", false, true},
		"a storage the definitions do not list": {eligCfg(config.HAModeAuto), "gone", false, false},
		"a shared storage under topology local": {
			eligCfg(config.HAModeAuto, withTopology(config.HAStorageTopologyLocal)), "nfs", false, true,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			shared, known := p.StorageShared(c.cfg, c.storage)
			if shared != c.shared || known != c.known {
				t.Fatalf("StorageShared = %v, %v, want %v, %v", shared, known, c.shared, c.known)
			}
		})
	}
	broken := eligProfile(config.HAModeAuto, "pve1")
	broken.StoragesErr = "storage read failed"
	if shared, known := broken.StorageShared(eligCfg(config.HAModeAuto), "nfs"); shared || known {
		t.Fatalf("StorageShared = %v, %v with unreadable definitions, want false, false", shared, known)
	}
}
