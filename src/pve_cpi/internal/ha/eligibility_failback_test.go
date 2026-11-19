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
