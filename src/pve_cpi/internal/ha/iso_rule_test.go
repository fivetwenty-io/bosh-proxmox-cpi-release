package ha

import (
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// isoRuleRow is one combination of the inputs the rule reads.
type isoRuleRow struct {
	mode, policy, topology string
	setting, disksShared   bool
}

func (r isoRuleRow) String() string {
	return fmt.Sprintf("mode=%s setting=%v policy=%s topology=%s shared-disks=%v",
		r.mode, r.setting, r.policy, r.topology, r.disksShared)
}

// check decides the row's VM twice, with a node-local ISO and with a shared
// one, and compares the first decision with the rule. It reports whether the
// row reached the ISO check. A row where the VM fails with a shared ISO too
// fails for a reason the ISO does not decide, so it only checks that a
// node-local ISO never rescues the call.
func (r isoRuleRow) check(t *testing.T) bool {
	t.Helper()
	disk := VMVolume{Volume: "tank:vm-101-disk-0"}
	if r.disksShared {
		disk = VMVolume{Volume: "nfs:100/vm-101-disk-0"}
	}
	cfg := eligCfg(r.mode, withPolicy(r.policy), withTopology(r.topology))
	if r.setting {
		cfg = withISOKey(cfg)
	}
	p := eligProfile(r.mode, "pve1", "pve2", "pve3")
	local := mustDecide(t, p, baseInput(cfg, disk, iso("local:iso/vm-101-ci.iso")))
	shared := mustDecide(t, p, baseInput(cfg, disk, iso("nfs:iso/vm-101-ci.iso")))
	failsLocal := local.Outcome == OutcomeFail
	if shared.Outcome == OutcomeFail {
		if !failsLocal {
			t.Errorf("%s: a node-local ISO rescued a call a shared ISO fails: %+v", r, local)
		}
		return false
	}
	if want := LocalISORuleFor(cfg, r.mode).Fails(r.disksShared); failsLocal != want {
		t.Errorf("%s: DecideVM fails with a node-local ISO = %v (%s); LocalISORule says %v",
			r, failsLocal, local.Reason, want)
	}
	return true
}

// TestLocalISORuleMatchesDecideVM pins LocalISORuleFor to the decision it
// summarizes. For every mode, setting, local policy, storage topology, and
// disk layout, DecideVM fails a VM with a node-local ISO exactly when the rule
// says it does.
func TestLocalISORuleMatchesDecideVM(t *testing.T) {
	t.Parallel()
	modes := []string{config.HAModeOff, config.HAModeAuto, config.HAModeManaged}
	policies := []string{config.HALocalPolicyNone, config.HALocalPolicyHold, config.HALocalPolicyReplicate}
	topologies := []string{config.HAStorageTopologyAuto, config.HAStorageTopologyMixed,
		config.HAStorageTopologyShared, config.HAStorageTopologyLocal}
	checked := 0
	for _, mode := range modes {
		for _, policy := range policies {
			for _, topology := range topologies {
				for _, setting := range []bool{false, true} {
					for _, disksShared := range []bool{false, true} {
						row := isoRuleRow{mode, policy, topology, setting, disksShared}
						if row.check(t) {
							checked++
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no row reached the ISO check")
	}
}

// TestLocalISORuleOffAndNil pins that mode off and a nil config refuse no ISO.
func TestLocalISORuleOffAndNil(t *testing.T) {
	t.Parallel()
	on := withISOKey(eligCfg(config.HAModeOff, withTopology(config.HAStorageTopologyShared),
		withPolicy(config.HALocalPolicyReplicate)))
	if r := LocalISORuleFor(on, config.HAModeOff); r.Active() {
		t.Fatalf("mode off rule = %+v; want none", r)
	}
	if r := LocalISORuleFor(nil, config.HAModeManaged); r.Active() {
		t.Fatalf("nil config rule = %+v; want none", r)
	}
}
