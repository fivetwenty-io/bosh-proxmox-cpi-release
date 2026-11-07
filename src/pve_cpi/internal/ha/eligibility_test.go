package ha

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// eligCfg builds a config whose pve.ha block the options fill in.
func eligCfg(mode string, opts ...func(*config.HAConfig)) *config.CPIConfig {
	m := config.HAChoice(mode)
	h := &config.HAConfig{Mode: &m}
	for _, o := range opts {
		o(h)
	}
	return &config.CPIConfig{HA: h}
}

func withPolicy(v string) func(*config.HAConfig) {
	return func(h *config.HAConfig) { c := config.HAChoice(v); h.LocalPolicy = &c }
}

func withTopology(v string) func(*config.HAConfig) {
	return func(h *config.HAConfig) { c := config.HAChoice(v); h.StorageTopology = &c }
}

func withStrictAZ(strict bool) func(*config.HAConfig) {
	return func(h *config.HAConfig) { h.PinAZStrict = &strict }
}

func withDLB(enabled bool, az string) func(*config.HAConfig) {
	return func(h *config.HAConfig) { h.DLB = &config.HADLBConfig{Enabled: &enabled, AZName: &az} }
}

func withISOKey(cfg *config.CPIConfig) *config.CPIConfig {
	t := true
	cfg.RequireSharedISOForHA = &t
	return cfg
}

// eligProfile is a healthy full-HA profile over the named nodes. Every node is
// online and placeable, and the storages are the fixtures below.
func eligProfile(mode string, nodes ...string) *Profile {
	p := &Profile{
		Mode: mode, Effective: EffectiveFull, MinVersion: "9.2.4", HAVersionOK: true, DLBSupported: true,
		SysConsole: true, VMReplicate: true, Storages: eligStorages(),
	}
	for _, n := range nodes {
		p.Nodes = append(p.Nodes, NodeFacts{Name: n, Online: true})
	}
	return p
}

func eligStorages() []pve.StorageInfo {
	return []pve.StorageInfo{
		{Name: "nfs", Type: pve.StorageTypeNFS, Content: "images,iso"},
		{Name: "nfs-ab", Type: pve.StorageTypeNFS, Nodes: []string{"pve1", "pve2"}, Content: "images"},
		{Name: "nfs-ac", Type: pve.StorageTypeNFS, Nodes: []string{"pve1", "pve3"}, Content: "images"},
		{Name: "local", Type: pve.StorageTypeDir, Content: "iso,vztmpl"},
		{Name: "local-lvm", Type: pve.StorageTypeLVMThin, Content: "images,rootdir"},
		{Name: "tank", Type: pve.StorageTypeZFSPool, Content: "images,rootdir"},
		{Name: "tank-ab", Type: pve.StorageTypeZFSPool, Nodes: []string{"pve1", "pve2"}, Content: "images"},
		{Name: "rootonly", Type: pve.StorageTypeZFSPool, Content: "rootdir"},
	}
}

func vols(ids ...string) []VMVolume {
	out := make([]VMVolume, 0, len(ids))
	for _, id := range ids {
		out = append(out, VMVolume{Volume: id})
	}
	return out
}

func iso(id string) VMVolume { return VMVolume{Volume: id, Kind: VolumeISO} }

const eligHost = "host"

func baseInput(cfg *config.CPIConfig, volumes ...VMVolume) VMInput {
	return VMInput{Config: cfg, VMID: 101, Node: "pve1", Volumes: volumes, CPUType: "x86-64-v3"}
}

func mustDecide(t *testing.T, p *Profile, in VMInput) *VMDecision {
	t.Helper()
	d, err := p.DecideVM(in)
	if err != nil {
		t.Fatalf("DecideVM: %v", err)
	}
	return d
}

func warned(d *VMDecision, text string) bool {
	for _, w := range d.Warnings {
		if strings.Contains(w, text) {
			return true
		}
	}
	return false
}

func TestDecideVMRejectsCallerMistakes(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	cfg := eligCfg(config.HAModeAuto)
	var nilProfile *Profile
	cases := map[string]struct {
		p  *Profile
		in VMInput
	}{
		"nil profile": {nilProfile, baseInput(cfg)},
		"nil config":  {p, VMInput{VMID: 1, Node: "pve1"}},
		"zero vmid":   {p, VMInput{Config: cfg, VMID: 0, Node: "pve1"}},
		"blank node":  {p, VMInput{Config: cfg, VMID: 1, Node: " "}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if d, err := tc.p.DecideVM(tc.in); err == nil || d != nil {
				t.Fatalf("want an error, got %+v, %v", d, err)
			}
		})
	}
}

// The first rows of the decision table are the profile's: a cluster that fails
// a prerequisite gives observe-only in auto and an error in managed.
func TestDecideVMProfileGateRows(t *testing.T) {
	observe := func(mode string) *Profile {
		p := eligProfile(mode, "pve1", "pve2", "pve3")
		p.Effective = EffectiveObserveOnly
		p.Reason = "the entry node does not report the cluster quorate"
		p.Gaps = []string{p.Reason}
		return p
	}
	t.Run("mode off registers nothing", func(t *testing.T) {
		p := eligProfile(config.HAModeOff, "pve1", "pve2")
		p.Effective = EffectiveOff
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeOff), vols("nfs:100/vm-101-disk-0")...))
		if d.Outcome != OutcomeSkip || d.Registers() || d.Failure() != nil {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("mode off fails a sentinel AZ VM", func(t *testing.T) {
		p := eligProfile(config.HAModeOff, "pve1", "pve2")
		p.Effective = EffectiveOff
		in := baseInput(eligCfg(config.HAModeOff))
		in.InDLBAZ = true
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || d.Failure() == nil || !strings.Contains(d.Err.Error(), "pve.ha.mode") {
			t.Fatalf("decision = %+v", d)
		}
		if cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("the sentinel error must not be retriable: %v", d.Err)
		}
	})
	t.Run("mode off ignores the sentinel when it is disabled", func(t *testing.T) {
		p := eligProfile(config.HAModeOff, "pve1", "pve2")
		p.Effective = EffectiveOff
		in := baseInput(eligCfg(config.HAModeOff, withDLB(false, "")))
		in.InDLBAZ = true
		if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("auto observes only", func(t *testing.T) {
		d := mustDecide(t, observe(config.HAModeAuto), baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/vm-101-disk-0")...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "quorate") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("managed errors with the prerequisite", func(t *testing.T) {
		d := mustDecide(t, observe(config.HAModeManaged), baseInput(eligCfg(config.HAModeManaged), vols("nfs:100/vm-101-disk-0")...))
		if d.Outcome != OutcomeFail || d.Failure() == nil || !strings.Contains(d.Err.Error(), "quorate") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMAllSharedRegistersFullHA(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			d := mustDecide(t, p, baseInput(eligCfg(mode), vols("nfs:100/vm-101-disk-0", "nfs:100/vm-101-cloudinit")...))
			if !d.Registers() || d.Policy != PolicyShared {
				t.Fatalf("decision = %+v", d)
			}
			if d.Hard != nil || !reflect.DeepEqual(d.Admissible, []string{"pve1", "pve2", "pve3"}) {
				t.Fatalf("no hard constraint expected: hard=%v admissible=%v", d.Hard, d.Admissible)
			}
			if !reflect.DeepEqual(d.Priorities, map[string]int{"pve1": 2, "pve2": 0, "pve3": 0}) {
				t.Fatalf("priorities = %v", d.Priorities)
			}
			if d.SID != "vm:101" || d.Placed != "pve1" || d.CPUDomain != nil {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestDecideVMNoVolumesCountsAsShared(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto)))
	if !d.Registers() || d.Policy != PolicyShared {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideVMLocalVolumeRows(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1")
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode+" none", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			d := mustDecide(t, p, baseInput(eligCfg(mode), disks...))
			if d.Outcome != OutcomeSkip || d.Policy != PolicyNone || !strings.Contains(d.Reason, "local-lvm:vm-101-disk-1") {
				t.Fatalf("a none policy must skip in %s without failing: %+v", mode, d)
			}
		})
		t.Run(mode+" hold", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			d := mustDecide(t, p, baseInput(eligCfg(mode, withPolicy(config.HALocalPolicyHold)), disks...))
			if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) {
				t.Fatalf("decision = %+v", d)
			}
			rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
			if err != nil || rule == nil || !rule.Strict || !reflect.DeepEqual(rule.NodeNames(), []string{"pve1"}) {
				t.Fatalf("pin = %+v, %v", rule, err)
			}
		})
	}
}

func TestDecideVMSharedRootWithNodeLocalISO(t *testing.T) {
	disks := []VMVolume{{Volume: "nfs:100/vm-101-disk-0"}, iso("local:iso/vm-101-ci.iso")}
	t.Run("auto downgrades to some-local with a warning", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), disks...))
		if d.Outcome != OutcomeSkip || !warned(d, "node-local ISO") || !strings.Contains(d.Reason, "local:iso/vm-101-ci.iso") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("auto with hold pins the VM", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyHold)), disks...))
		if !d.Registers() || d.Policy != PolicyHold || !warned(d, "node-local ISO") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("require_shared_iso_for_ha fails the call in auto", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(withISOKey(eligCfg(config.HAModeAuto)), disks...))
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "pve.iso_storage") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("managed always fails", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeManaged), disks...))
		if d.Outcome != OutcomeFail {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a shared ISO keeps full HA", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeManaged),
			VMVolume{Volume: "nfs:100/vm-101-disk-0"}, iso("nfs:iso/vm-101-ci.iso")))
		if !d.Registers() || d.Policy != PolicyShared {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("the key does not fail a VM that was never eligible", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(withISOKey(eligCfg(config.HAModeAuto)),
			VMVolume{Volume: "local-lvm:vm-101-disk-0"}, iso("local:iso/vm-101-ci.iso")))
		if d.Outcome != OutcomeSkip {
			t.Fatalf("a VM with a local disk is not made to fail by its ISO: %+v", d)
		}
	})
}

func TestDecideVMSharedRootWithNodeLocalPersistentDisk(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1")
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	none := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), disks...))
	if none.Outcome != OutcomeSkip {
		t.Fatalf("none: %+v", none)
	}
	hold := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyHold)), disks...))
	if !hold.Registers() || !reflect.DeepEqual(hold.Admissible, []string{"pve1"}) {
		t.Fatalf("hold: %+v", hold)
	}
	// The local disk lives on a node other than the one the VM is placed on.
	away := []VMVolume{{Volume: "nfs:100/vm-101-disk-0"}, {Volume: "local-lvm:vm-101-disk-1", HomeNode: "pve2"}}
	d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyHold)), away...))
	if d.Outcome != OutcomeSkip || !warned(d, "cannot be pinned to node pve1") {
		t.Fatalf("a hold VM away from its disk must get no HA: %+v", d)
	}
	managed := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
	d = mustDecide(t, managed, baseInput(eligCfg(config.HAModeManaged, withPolicy(config.HALocalPolicyHold)), away...))
	if d.Outcome != OutcomeFail {
		t.Fatalf("managed must fail: %+v", d)
	}
}

func TestDecideVMStorageTopology(t *testing.T) {
	shared := vols("nfs:100/vm-101-disk-0")
	local := vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1")
	cases := []struct {
		name     string
		topology string
		volumes  []VMVolume
		policy   string
		want     Outcome
		wantPol  Policy
	}{
		{"auto shared volumes", config.HAStorageTopologyAuto, shared, "", OutcomeRegister, PolicyShared},
		{"auto local volume follows the policy", config.HAStorageTopologyAuto, local, "", OutcomeSkip, PolicyNone},
		{"mixed shared volumes", config.HAStorageTopologyMixed, shared, "", OutcomeRegister, PolicyShared},
		{"mixed local volume holds", config.HAStorageTopologyMixed, local, config.HALocalPolicyHold, OutcomeRegister, PolicyHold},
		{"shared accepts shared volumes", config.HAStorageTopologyShared, shared, "", OutcomeRegister, PolicyShared},
		{"shared refuses a local volume", config.HAStorageTopologyShared, local, config.HALocalPolicyHold, OutcomeFail, PolicyNone},
		{"local makes every VM follow the policy", config.HAStorageTopologyLocal, shared, "", OutcomeSkip, PolicyNone},
		{"local with hold pins even shared volumes", config.HAStorageTopologyLocal, shared, config.HALocalPolicyHold,
			OutcomeRegister, PolicyHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := []func(*config.HAConfig){withTopology(tc.topology)}
			if tc.policy != "" {
				opts = append(opts, withPolicy(tc.policy))
			}
			for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
				p := eligProfile(mode, "pve1", "pve2", "pve3")
				d := mustDecide(t, p, baseInput(eligCfg(mode, opts...), tc.volumes...))
				if d.Outcome != tc.want || d.Policy != tc.wantPol {
					t.Fatalf("%s: decision = %+v", mode, d)
				}
			}
		})
	}
	t.Run("shared topology refuses a VM with too small an access set", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withTopology(config.HAStorageTopologyShared),
			withPolicy(config.HALocalPolicyHold)), vols("nfs-ab:100/a", "nfs-ac:100/b")...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "storage_topology is shared") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMLegacyDisk(t *testing.T) {
	volumes := []VMVolume{{Volume: "nfs:100/vm-101-disk-0"}, {Volume: "nfs:100/vm-101-disk-1", Legacy: true},
		{Volume: "nfs:100/vm-101-disk-2", Legacy: true}}
	t.Run("auto warns once per disk and registers", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), volumes...))
		if !d.Registers() || len(d.Warnings) != 2 || !warned(d, "vm-101-disk-1") || !warned(d, "vm-101-disk-2") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("managed refuses to register", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeManaged), volumes...))
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "legacy") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a VM that gets no HA needs no warning", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeManaged),
			VMVolume{Volume: "local-lvm:vm-101-disk-0", Legacy: true}))
		if d.Outcome != OutcomeSkip || len(d.Warnings) != 0 {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an ISO is never a legacy disk", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeManaged),
			VMVolume{Volume: "nfs:iso/ci.iso", Kind: VolumeISO, Legacy: true}))
		if !d.Registers() {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMAccessSetIntersection(t *testing.T) {
	// nfs-ab reaches pve1 and pve2, and nfs-ac reaches pve1 and pve3.
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	t.Run("two storages that meet in one node leave one", func(t *testing.T) {
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), vols("nfs-ab:100/a", "nfs-ac:100/b")...))
		if d.Outcome != OutcomeSkip || !reflect.DeepEqual(d.Access, []string{"pve1"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("with hold the single node pins the VM", func(t *testing.T) {
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyHold)),
			vols("nfs-ab:100/a", "nfs-ac:100/b")...))
		if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a restricted storage that holds two nodes is eligible and strict", func(t *testing.T) {
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), vols("nfs-ab:100/a")...))
		if !d.Registers() || d.Policy != PolicyShared || !reflect.DeepEqual(d.Hard, []string{"pve1", "pve2"}) {
			t.Fatalf("decision = %+v", d)
		}
		rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
		if err != nil || !rule.Strict || len(rule.Nodes) != 2 {
			t.Fatalf("pin = %+v, %v", rule, err)
		}
	})
	t.Run("a placed node outside the access set gets no HA", func(t *testing.T) {
		in := baseInput(eligCfg(config.HAModeAuto), vols("nfs-ac:100/b", "nfs:100/c")...)
		in.Node = "pve2"
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an unknown storage fails closed", func(t *testing.T) {
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), vols("mystery:100/a")...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "mystery") {
			t.Fatalf("decision = %+v", d)
		}
		managed := eligProfile(config.HAModeManaged, "pve1", "pve2")
		if d = mustDecide(t, managed, baseInput(eligCfg(config.HAModeManaged), vols("mystery:100/a")...)); d.Outcome != OutcomeFail {
			t.Fatalf("managed: %+v", d)
		}
	})
	t.Run("a volume ID without a storage fails closed", func(t *testing.T) {
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto), vols("novolid")...))
		if d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("unreadable storage definitions fail closed", func(t *testing.T) {
		bad := eligProfile(config.HAModeAuto, "pve1", "pve2")
		bad.Storages, bad.StoragesErr = nil, "boom"
		d := mustDecide(t, bad, baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/a")...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "boom") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMBridgeOnOneNodeOfTheSharedAccessSet(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	in := baseInput(eligCfg(config.HAModeAuto), vols("nfs-ab:100/a")...)
	in.Constraints = []NodeConstraint{{Name: "bridge vmbr7", Nodes: []string{"pve1", "pve3"}}}
	if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip || !reflect.DeepEqual(d.Access, []string{"pve1"}) {
		t.Fatalf("a bridge on one node of the set must leave one node: %+v", d)
	}
	in.Config = eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyHold))
	if d := mustDecide(t, p, in); !d.Registers() || d.Policy != PolicyHold {
		t.Fatalf("hold: %+v", d)
	}
	// A bridge on both nodes of the set changes nothing.
	in.Constraints = []NodeConstraint{{Name: "bridge vmbr7", Nodes: []string{"pve1", "pve2"}}}
	in.Config = eligCfg(config.HAModeAuto)
	if d := mustDecide(t, p, in); !d.Registers() || d.Policy != PolicyShared {
		t.Fatalf("both nodes: %+v", d)
	}
	// A constraint that no node meets leaves nothing.
	in.Constraints = []NodeConstraint{{Name: "PCI device 0000:01:00.0"}}
	if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip || len(d.Access) != 0 {
		t.Fatalf("no node: %+v", d)
	}
}

func TestDecideVMOnlyPlaceableNodesCount(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
		{Name: "pve1", Placeable: true}, {Name: "pve2", Maintenance: true}, {Name: "pve3", Placeable: true},
	}}
	in := baseInput(eligCfg(config.HAModeAuto), vols("nfs-ab:100/a")...)
	d := mustDecide(t, p, in)
	if d.Outcome != OutcomeSkip || !reflect.DeepEqual(d.Access, []string{"pve1", "pve2"}) {
		t.Fatalf("a node in maintenance does not count toward the two: %+v", d)
	}
	in = baseInput(eligCfg(config.HAModeAuto), vols("nfs-ac:100/a")...)
	if d = mustDecide(t, p, in); !d.Registers() {
		t.Fatalf("two placeable nodes: %+v", d)
	}
}

// eligCPUs builds the CPU domains of a cluster from a node-to-model map.
func eligCPUs(p *Profile, models map[string]string) {
	cpus := map[string]CPUInfo{}
	for n, m := range models {
		cpus[n] = CPUInfo{Known: m != "", Model: m}
	}
	p.CPUDomains, p.UnknownCPUNodes = groupCPUDomains(cpus)
}

func TestDecideVMCPUMigrationDomain(t *testing.T) {
	models := map[string]string{"pve1": "Xeon A", "pve2": "Xeon A", "pve3": "Xeon B"}
	disks := vols("nfs:100/vm-101-disk-0")
	t.Run("a domain of two nodes keeps full HA inside it", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, models)
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = eligHost
		d := mustDecide(t, p, in)
		if !d.Registers() || d.Policy != PolicyShared ||
			!reflect.DeepEqual(d.CPUDomain, []string{"pve1", "pve2"}) || !reflect.DeepEqual(d.Hard, []string{"pve1", "pve2"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("max follows the same rule", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, models)
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = "max,flags=+aes"
		if d := mustDecide(t, p, in); !d.Registers() || !reflect.DeepEqual(d.Hard, []string{"pve1", "pve2"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	one := map[string]string{"pve1": "Xeon A", "pve2": "Xeon B", "pve3": "Xeon B"}
	t.Run("a domain of one gives no HA in auto and names pve.cpu_type", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, one)
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = eligHost
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "pve.cpu_type") || !warned(d, "pve.cpu_type") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a domain of one fails managed with the same text", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		eligCPUs(p, one)
		in := baseInput(eligCfg(config.HAModeManaged), disks...)
		in.CPUType = eligHost
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "pve.cpu_type") ||
			cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a portable CPU model keeps the full access set", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, one)
		for _, cpu := range []string{"x86-64-v2-AES", "Cascadelake-Server", "", "kvm64", "cputype=x86-64-v3,flags=+aes"} {
			in := baseInput(eligCfg(config.HAModeAuto), disks...)
			in.CPUType = cpu
			d := mustDecide(t, p, in)
			if !d.Registers() || d.Hard != nil || d.CPUDomain != nil || len(d.Access) != 3 {
				t.Fatalf("cpu %q: decision = %+v", cpu, d)
			}
		}
	})
	t.Run("a two node cluster with one model keeps HA for a host VM", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2")
		eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "Xeon A"})
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = eligHost
		if d := mustDecide(t, p, in); !d.Registers() || d.Hard != nil {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a two node cluster with two models gives a host VM no HA", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2")
		eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "Xeon B"})
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = eligHost
		if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an unknown CPU on the placed node fails closed", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, map[string]string{"pve1": "", "pve2": "Xeon A", "pve3": "Xeon A"})
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.CPUType = eligHost
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip || !d.Transient || !strings.Contains(d.Reason, "CPU of its node pve1 could not be read") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a domain with enough nodes where too few are placeable is retriable in managed", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		eligCPUs(p, models)
		p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
			{Name: "pve1", Placeable: true}, {Name: "pve2", Maintenance: true}, {Name: "pve3", Placeable: true},
		}}
		in := baseInput(eligCfg(config.HAModeManaged), disks...)
		in.CPUType = eligHost
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestEffectiveCPUTypeFollowsTheWriteOrder(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		vm   *string
		pass string
		want string
	}{
		{"nil config leaves the choice to PVE", nil, nil, "", ""},
		{"unset pve.cpu_type leaves the choice to PVE", &config.CPIConfig{}, nil, "", ""},
		{"defaulted pve.cpu_type is host", &config.CPIConfig{CPUType: config.DefaultCPUType}, nil, "", config.DefaultCPUType},
		{"pve.cpu_type", &config.CPIConfig{CPUType: " x86-64-v3 "}, nil, "", "x86-64-v3"},
		{"pve-default sentinel", &config.CPIConfig{CPUType: config.CPUTypePVEDefault}, nil, "", ""},
		{"cloud_properties.cpu_type wins over pve.cpu_type", &config.CPIConfig{CPUType: eligHost}, str("x86-64-v2-AES"), "",
			"x86-64-v2-AES"},
		{"cloud_properties sentinel", &config.CPIConfig{CPUType: eligHost}, str(config.CPUTypePVEDefault), "", ""},
		{"cloud_properties.cpu_type host", &config.CPIConfig{CPUType: "x86-64-v3"}, str(eligHost), "", eligHost},
		{"pve_config passthrough is the last write", &config.CPIConfig{CPUType: "x86-64-v3"}, str("kvm64"), "host", eligHost},
		{"passthrough over a nil config", nil, nil, "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCPUType(tc.cfg, tc.vm, tc.pass); got != tc.want {
				t.Fatalf("EffectiveCPUType = %q, want %q", got, tc.want)
			}
		})
	}
}

// A CPU type from cloud_properties.cpu_type and one from the pve_config
// passthrough both reach the migration-domain check.
func TestDecideVMCPUTypeFromEachSource(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "Xeon B", "pve3": "Xeon B"})
	host, portable := eligHost, "x86-64-v3"
	cfg := &config.CPIConfig{CPUType: "x86-64-v3"}
	cfg.HA = eligCfg(config.HAModeAuto).HA
	for name, tc := range map[string]struct {
		perVM *string
		pass  string
		want  Outcome
	}{
		"pve.cpu_type portable":             {nil, "", OutcomeRegister},
		"cloud_properties.cpu_type host":    {&host, "", OutcomeSkip},
		"pve_config passthrough host":       {nil, "host", OutcomeSkip},
		"passthrough portable beats a host": {&host, portable, OutcomeRegister},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput(cfg, vols("nfs:100/vm-101-disk-0")...)
			in.CPUType = EffectiveCPUType(cfg, tc.perVM, tc.pass)
			if d := mustDecide(t, p, in); d.Outcome != tc.want {
				t.Fatalf("cpu %q: decision = %+v", in.CPUType, d)
			}
		})
	}
}

func TestCPUTypeNeedsDomain(t *testing.T) {
	for cpu, want := range map[string]bool{
		"host": true, "HOST": true, "max": true, "host,flags=+aes": true, "cputype=host": true,
		"": false, "kvm64": false, "x86-64-v3": false, "Cascadelake-Server": false, "flags=+aes": false,
		"cputype=x86-64-v2-AES,flags=+pcid": false, "host-model": false,
	} {
		if got := CPUTypeNeedsDomain(cpu); got != want {
			t.Errorf("CPUTypeNeedsDomain(%q) = %v, want %v", cpu, got, want)
		}
	}
}

func TestDecideVMAvailabilityZone(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0")
	az := []string{"pve1", "pve2"}
	t.Run("a non-strict AZ raises priorities and removes no node", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeAuto, withStrictAZ(false)), disks...)
		in.AZ, in.AZNodes = "az1", az
		d := mustDecide(t, p, in)
		if !d.Registers() || d.Hard != nil || !reflect.DeepEqual(d.Admissible, []string{"pve1", "pve2", "pve3"}) ||
			!reflect.DeepEqual(d.Preferred, az) {
			t.Fatalf("decision = %+v", d)
		}
		if !reflect.DeepEqual(d.Priorities, map[string]int{"pve1": 2, "pve2": 1, "pve3": 0}) {
			t.Fatalf("priorities = %v", d.Priorities)
		}
		rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
		if err != nil || rule.Strict || len(rule.Nodes) != 3 {
			t.Fatalf("pin = %+v, %v", rule, err)
		}
	})
	t.Run("a strict AZ narrows the hard set", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.AZ, in.AZNodes = "az1", az
		d := mustDecide(t, p, in)
		if !d.Registers() || !reflect.DeepEqual(d.Hard, az) || !reflect.DeepEqual(d.Admissible, az) || d.Preferred != nil {
			t.Fatalf("decision = %+v", d)
		}
		rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
		if err != nil || !rule.Strict || len(rule.Nodes) != 2 {
			t.Fatalf("pin = %+v, %v", rule, err)
		}
	})
	t.Run("a non-strict AZ with a hold self-pin gives a strict rule over the self-pin alone", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeAuto, withStrictAZ(false), withPolicy(config.HALocalPolicyHold)),
			vols("nfs:100/vm-101-disk-0", "local-lvm:vm-101-disk-1")...)
		in.AZ, in.AZNodes = "az1", az
		d := mustDecide(t, p, in)
		if !d.Registers() || d.Policy != PolicyHold || !reflect.DeepEqual(d.Hard, []string{"pve1"}) {
			t.Fatalf("decision = %+v", d)
		}
		rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), false))
		if err != nil || !rule.Strict || !reflect.DeepEqual(rule.NodeNames(), []string{"pve1"}) {
			t.Fatalf("pin = %+v, %v", rule, err)
		}
	})
	t.Run("a strict AZ that excludes the placed node gives no HA", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.AZ, in.AZNodes = "az2", []string{"pve2", "pve3"}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "outside its hard admissible set") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a strict AZ that shares no node with the access set leaves an empty set", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeManaged), vols("nfs-ab:100/a")...)
		in.AZ, in.AZNodes = "az2", []string{"pve3"}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "empty hard admissible set") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("pin_az off ignores the AZ", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		cfg := eligCfg(config.HAModeAuto, func(h *config.HAConfig) { c := config.HAChoice(config.HARuleOff); h.PinAZ = &c })
		in := baseInput(cfg, disks...)
		in.AZ, in.AZNodes = "az1", az
		if d := mustDecide(t, p, in); !d.Registers() || d.Hard != nil || d.Preferred != nil {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMDLB(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0")
	t.Run("the sentinel AZ makes a DLB VM without a placed priority", func(t *testing.T) {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			in := baseInput(eligCfg(mode), disks...)
			in.InDLBAZ = true
			d := mustDecide(t, p, in)
			if !d.Registers() || !d.DLB || d.Priorities["pve1"] != PinPriorityRest {
				t.Fatalf("%s: decision = %+v", mode, d)
			}
			if rule, err := BuildPin(d.PinSpec("bosh-na-101", "m", p.nodeNames(), true)); err != nil || rule != nil {
				t.Fatalf("a DLB VM gets no placed-node pin: %+v, %v", rule, err)
			}
		}
	})
	t.Run("dlb.enabled makes every VM a DLB VM", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withDLB(true, "dlb")), disks...))
		if !d.Registers() || !d.DLB {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an empty az_name disables the sentinel", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := baseInput(eligCfg(config.HAModeAuto, withDLB(false, "")), disks...)
		in.InDLBAZ = true
		if d := mustDecide(t, p, in); !d.Registers() || d.DLB {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("old PVE gives a sentinel VM no HA in auto and an error in managed", func(t *testing.T) {
		auto := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		auto.DLBSupported, auto.MinVersion = false, "9.1.0"
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.InDLBAZ = true
		if d := mustDecide(t, auto, in); d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "9.2") {
			t.Fatalf("auto: %+v", d)
		}
		managed := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		managed.DLBSupported, managed.MinVersion = false, "9.1.0"
		in = baseInput(eligCfg(config.HAModeManaged), disks...)
		in.InDLBAZ = true
		if d := mustDecide(t, managed, in); d.Outcome != OutcomeFail {
			t.Fatalf("managed: %+v", d)
		}
	})
	t.Run("old PVE with dlb.enabled warns and registers in auto", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		p.DLBSupported, p.MinVersion = false, "9.1.0"
		d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withDLB(true, "dlb")), disks...))
		if !d.Registers() || d.DLB || !warned(d, "dlb.enabled") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func replicateCfg(mode string) *config.CPIConfig {
	return eligCfg(mode, withPolicy(config.HALocalPolicyReplicate))
}

func TestDecideVMReplicate(t *testing.T) {
	zfs := vols("tank:vm-101-disk-0", "nfs:100/vm-101-data")
	cfg := replicateCfg
	t.Run("local ZFS with a shared ISO replicates and pins the source", func(t *testing.T) {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			in := baseInput(cfg(mode), append(zfs, iso("nfs:iso/ci.iso"))...)
			d := mustDecide(t, p, in)
			if !d.Registers() || d.Policy != PolicyReplicate || !reflect.DeepEqual(d.Hard, []string{"pve1"}) ||
				d.Partner != "pve2" || !reflect.DeepEqual(d.Widened, []string{"pve1", "pve2"}) {
				t.Fatalf("%s: decision = %+v", mode, d)
			}
		}
	})
	t.Run("an existing job keeps its target as the partner", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		p.Replication = []ReplicationJob{{ID: "101-0", Guest: 101, Source: "pve1", Target: "pve3"}}
		if d := mustDecide(t, p, baseInput(cfg(config.HAModeAuto), zfs...)); d.Partner != "pve3" {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("the partner needs the same ZFS storage", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(cfg(config.HAModeAuto), vols("tank-ab:vm-101-disk-0")...))
		if !d.Registers() || d.Partner != "pve2" {
			t.Fatalf("decision = %+v", d)
		}
		onlyHome := eligProfile(config.HAModeAuto, "pve1", "pve3")
		d = mustDecide(t, onlyHome, baseInput(cfg(config.HAModeAuto), vols("tank-ab:vm-101-disk-0")...))
		if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "no placeable partner") {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMReplicatePrerequisites(t *testing.T) {
	cfg := replicateCfg
	t.Run("a non-ZFS local volume gives no HA in auto and an error in managed", func(t *testing.T) {
		in := func(mode string) VMInput { return baseInput(cfg(mode), vols("local-lvm:vm-101-disk-0")...) }
		d := mustDecide(t, eligProfile(config.HAModeAuto, "pve1", "pve2"), in(config.HAModeAuto))
		if d.Outcome != OutcomeSkip || !warned(d, "not a ZFS pool") {
			t.Fatalf("auto: %+v", d)
		}
		d = mustDecide(t, eligProfile(config.HAModeManaged, "pve1", "pve2"), in(config.HAModeManaged))
		if d.Outcome != OutcomeFail || !strings.Contains(d.Err.Error(), "not a ZFS pool") {
			t.Fatalf("managed: %+v", d)
		}
	})
	t.Run("a node-local ISO gives no HA in auto and an error in managed", func(t *testing.T) {
		in := func(mode string) VMInput {
			return baseInput(cfg(mode), VMVolume{Volume: "tank:vm-101-disk-0"}, iso("local:iso/ci.iso"))
		}
		d := mustDecide(t, eligProfile(config.HAModeAuto, "pve1", "pve2"), in(config.HAModeAuto))
		if d.Outcome != OutcomeSkip || !warned(d, "ISO") {
			t.Fatalf("auto: %+v", d)
		}
		d = mustDecide(t, eligProfile(config.HAModeManaged, "pve1", "pve2"), in(config.HAModeManaged))
		if d.Outcome != OutcomeFail {
			t.Fatalf("managed: %+v", d)
		}
		keyed := baseInput(withISOKey(cfg(config.HAModeAuto)), VMVolume{Volume: "tank:vm-101-disk-0"}, iso("local:iso/ci.iso"))
		if d = mustDecide(t, eligProfile(config.HAModeAuto, "pve1", "pve2"), keyed); d.Outcome != OutcomeFail {
			t.Fatalf("require_shared_iso_for_ha must fail a replicate VM: %+v", d)
		}
	})
	t.Run("each other prerequisite", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate func(*Profile)
			vol    VMVolume
			linked bool
			want   string
		}{
			"no VM.Replicate":    {mutate: func(p *Profile) { p.VMReplicate = false }, vol: VMVolume{Volume: "tank:v"}, want: "VM.Replicate"},
			"replicate flag":     {vol: VMVolume{Volume: "tank:v", NoReplicate: true}, want: "replicate flag"},
			"linked clone":       {vol: VMVolume{Volume: "tank:v"}, linked: true, want: "linked clones"},
			"storage not images": {vol: VMVolume{Volume: "rootonly:v"}, want: "does not hold images"},
			"disabled storage": {mutate: func(p *Profile) { p.Storages[5].Disabled = true }, vol: VMVolume{Volume: "tank:v"},
				want: "disabled"},
			"home not defined": {vol: VMVolume{Volume: "tank-ab:v", HomeNode: "pve3"}, want: "not defined on node pve3"},
		} {
			t.Run(name, func(t *testing.T) {
				p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
				if tc.mutate != nil {
					tc.mutate(p)
				}
				in := baseInput(cfg(config.HAModeAuto), tc.vol)
				in.LinkedClone = tc.linked
				if tc.vol.HomeNode != "" {
					in.Node = tc.vol.HomeNode
				}
				d := mustDecide(t, p, in)
				if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, tc.want) {
					t.Fatalf("decision = %+v", d)
				}
			})
		}
	})
}

func TestDecideVMReplicatePartnerChoice(t *testing.T) {
	zfs := vols("tank:vm-101-disk-0", "nfs:100/vm-101-data")
	cfg := replicateCfg
	t.Run("the partner is chosen inside the CPU domain, the AZ, and the hardware", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		eligCPUs(p, map[string]string{"pve1": "A", "pve2": "B", "pve3": "A"})
		in := baseInput(cfg(config.HAModeAuto), zfs...)
		in.CPUType = eligHost
		if d := mustDecide(t, p, in); !d.Registers() || d.Partner != "pve3" {
			t.Fatalf("cpu domain: %+v", d)
		}
		in.CPUType = "x86-64-v3"
		in.Constraints = []NodeConstraint{{Name: "bridge vmbr7", Nodes: []string{"pve1", "pve3"}}}
		if d := mustDecide(t, p, in); d.Partner != "pve3" {
			t.Fatalf("hardware: %+v", d)
		}
		in.Constraints = nil
		in.AZ, in.AZNodes = "az1", []string{"pve1", "pve3"}
		d := mustDecide(t, p, in)
		if d.Partner != "pve3" || !reflect.DeepEqual(d.Widened, []string{"pve1", "pve3"}) {
			t.Fatalf("az: %+v", d)
		}
	})
	t.Run("an unknown CPU gives no replica", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2")
		eligCPUs(p, map[string]string{"pve1": "", "pve2": "A"})
		in := baseInput(cfg(config.HAModeAuto), zfs...)
		in.CPUType = eligHost
		if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a shared volume the partner cannot reach leaves no partner", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		d := mustDecide(t, p, baseInput(cfg(config.HAModeAuto), vols("tank:v", "nfs-ac:100/b")...))
		if !d.Registers() || d.Partner != "pve3" {
			t.Fatalf("decision = %+v", d)
		}
		d = mustDecide(t, p, baseInput(cfg(config.HAModeAuto), vols("tank-ab:v", "nfs-ac:100/b")...))
		if d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("the partner must be placeable", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2")
		p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
			{Name: "pve1", Placeable: true}, {Name: "pve2", Maintenance: true},
		}}
		if d := mustDecide(t, p, baseInput(cfg(config.HAModeAuto), vols("tank:v")...)); d.Outcome != OutcomeSkip {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a legacy disk is refused in managed", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2")
		d := mustDecide(t, p, baseInput(cfg(config.HAModeManaged), VMVolume{Volume: "tank:v", Legacy: true}))
		if d.Outcome != OutcomeFail {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecideVMTooSmallAccessSetFollowsLocalPolicy(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	volumes := vols("nfs-ab:100/a", "nfs-ac:100/b")
	d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto, withPolicy(config.HALocalPolicyReplicate)), volumes...))
	if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "no node-local disk") {
		t.Fatalf("replicate over shared storage: %+v", d)
	}
}

func TestDecideVMNoNodesInTheProfile(t *testing.T) {
	p := eligProfile(config.HAModeAuto)
	d := mustDecide(t, p, baseInput(eligCfg(config.HAModeAuto)))
	if d.Outcome != OutcomeSkip || !strings.Contains(d.Reason, "no cluster node") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecisionCarriesTheUIDAndStagedFlagToTheRepair(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	in := baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/a")...)
	in.State = &pve.HAServiceState{SID: "vm:101", Known: true, InCRM: true, UID: "abc123"}
	d := mustDecide(t, p, in)
	if d.DecisionUID != "abc123" || d.StagedIgnored {
		t.Fatalf("decision = %+v", d)
	}
	deps := d.RepairDeps(nil, func(context.Context, string) error { return nil })
	if uid, ok := deps.DecisionUID("vm:101"); !ok || uid != "abc123" {
		t.Fatalf("DecisionUID = %q, %v", uid, ok)
	}
	if _, ok := deps.DecisionUID("vm:102"); ok {
		t.Fatal("the decision must answer only for its own VM")
	}
	if deps.StagedIgnored("vm:101") {
		t.Fatal("nothing has been staged yet")
	}
	d.MarkStaged()
	if !deps.StagedIgnored("vm:101") || deps.StagedIgnored("vm:102") {
		t.Fatal("a staged resource is removable, and only its own")
	}
	d.MarkActivated()
	if deps.StagedIgnored("vm:101") {
		t.Fatal("an activated resource is no longer removable on the staged path")
	}
	if deps.RemoveResource == nil {
		t.Fatal("the removal path must be kept")
	}
}

func TestDecisionUIDIgnoresStatesThatProveNothing(t *testing.T) {
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	for name, st := range map[string]*pve.HAServiceState{
		"nil":           nil,
		"other VM":      {SID: "vm:102", Known: true, InCRM: true, UID: "x"},
		"unknown":       {SID: "vm:101", Known: false, InCRM: true, UID: "x"},
		"not in CRM":    {SID: "vm:101", Known: true, InCRM: false, UID: "x"},
		"no uid at all": {SID: "vm:101", Known: true, InCRM: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/a")...)
			in.State = st
			d := mustDecide(t, p, in)
			if d.DecisionUID != "" {
				t.Fatalf("DecisionUID = %q", d.DecisionUID)
			}
			deps := d.RepairDeps(nil, nil)
			if _, ok := deps.DecisionUID("vm:101"); ok {
				t.Fatal("an empty uid must not count as a decision read")
			}
		})
	}
}

func TestRepairDepsOfANilDecision(t *testing.T) {
	var d *VMDecision
	deps := d.RepairDeps(nil, func(context.Context, string) error { return errors.New("x") })
	if deps.StagedIgnored != nil || deps.DecisionUID != nil || deps.RemoveResource == nil {
		t.Fatalf("deps = %+v", deps)
	}
	if d.Registers() || d.Failure() != nil {
		t.Fatal("a nil decision registers nothing")
	}
}

// repairDecision decides VM 101 from a state read that carries uid, and
// returns the decision with the repair deps it feeds the rule writer.
func repairDecision(
	t *testing.T, st *pve.HAServiceState, remove func(context.Context, string) error,
	read func(context.Context, int) (*pve.HAServiceState, error),
) (*VMDecision, RepairDeps) {
	t.Helper()
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	in := baseInput(eligCfg(config.HAModeAuto), vols("nfs:100/a")...)
	in.State = st
	d := mustDecide(t, p, in)
	deps := d.RepairDeps(read, remove)
	return d, deps
}

// TestDecisionFieldsReachTheRemovalRepair drives a decision through the rule
// writer's removal repair, the way the writer reaches it when a pin cannot be
// repaired. A decision that carries the uid of the episode it read, and a
// resource that is still the CRM's same episode, lets the repair remove the
// resource, where a repair without the decision fails closed. A staged
// resource is removed directly, and an activated one goes back to the
// proof-by-uid path.
func TestDecisionFieldsReachTheRemovalRepair(t *testing.T) {
	var removed []string
	remove := func(_ context.Context, sid string) error {
		if sid != "vm:101" {
			return fmt.Errorf("the repair asked to remove %s, which is not the decided VM", sid)
		}
		removed = append(removed, sid)
		return nil
	}
	same := func(context.Context, int) (*pve.HAServiceState, error) { return goodState("vm:101", "u1"), nil }

	t.Run("fails closed without the decision", func(t *testing.T) {
		removed = nil
		err := applyUnrepairable(t, RepairDeps{RemoveResource: remove, ReadState: same})
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(removed) != 0 {
			t.Fatalf("err = %v, removed %v; want a retriable error and no removal", err, removed)
		}
	})
	t.Run("a decision that read no episode fails closed", func(t *testing.T) {
		removed = nil
		_, deps := repairDecision(t, nil, remove, same)
		err := applyUnrepairable(t, deps)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(removed) != 0 {
			t.Fatalf("err = %v, removed %v; want a retriable error and no removal", err, removed)
		}
	})
	t.Run("the decision's uid proves the same episode", func(t *testing.T) {
		removed = nil
		d, deps := repairDecision(t, goodState("vm:101", "u1"), remove, same)
		if d.DecisionUID != "u1" {
			t.Fatalf("DecisionUID = %q, want the uid of the state it read", d.DecisionUID)
		}
		if err := applyUnrepairable(t, deps); err != nil || !reflect.DeepEqual(removed, []string{"vm:101"}) {
			t.Fatalf("err = %v, removed %v; want the repair to remove vm:101", err, removed)
		}
	})
	t.Run("a changed episode keeps the resource", func(t *testing.T) {
		removed = nil
		_, deps := repairDecision(t, goodState("vm:101", "u1"), remove,
			func(context.Context, int) (*pve.HAServiceState, error) { return goodState("vm:101", "u2"), nil })
		err := applyUnrepairable(t, deps)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(removed) != 0 {
			t.Fatalf("err = %v, removed %v; want a retriable error and no removal", err, removed)
		}
	})
	t.Run("a staged resource is removed without a state read", func(t *testing.T) {
		removed = nil
		d, deps := repairDecision(t, nil, remove, nil)
		d.MarkStaged()
		if err := applyUnrepairable(t, deps); err != nil || !reflect.DeepEqual(removed, []string{"vm:101"}) {
			t.Fatalf("err = %v, removed %v; want the staged resource removed", err, removed)
		}
	})
	t.Run("an activated resource is no longer removed as staged", func(t *testing.T) {
		removed = nil
		d, deps := repairDecision(t, nil, remove, nil)
		d.MarkStaged()
		d.MarkActivated()
		err := applyUnrepairable(t, deps)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(removed) != 0 {
			t.Fatalf("err = %v, removed %v; want a retriable error and no removal", err, removed)
		}
	})
}

// TestDecideVMMarksTransientRefusals checks that a refusal whose cause may pass
// is marked transient, so a caller whose VM runs under HA defers on it, and
// that a refusal on proven, permanent facts is not.
func TestDecideVMMarksTransientRefusals(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0")
	host := func(in VMInput) VMInput { in.CPUType = eligHost; return in }
	for name, tc := range map[string]struct {
		profile   func(*Profile)
		in        func(VMInput) VMInput
		transient bool
	}{
		"the placed node's CPU is unreadable": {
			profile: func(p *Profile) {
				eligCPUs(p, map[string]string{"pve1": "", "pve2": "Xeon A", "pve3": "Xeon A"})
			},
			in: host, transient: true,
		},
		"a domain of one beside a node whose CPU is unreadable": {
			profile: func(p *Profile) {
				eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "", "pve3": "Xeon B"})
			},
			in: host, transient: true,
		},
		"a domain of two with one node in maintenance": {
			profile: func(p *Profile) {
				eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "Xeon A", "pve3": "Xeon B"})
				p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
					{Name: "pve1", Placeable: true}, {Name: "pve2", Maintenance: true}, {Name: "pve3", Placeable: true},
				}}
			},
			in: host, transient: true,
		},
		"a domain of one with every CPU known": {
			profile: func(p *Profile) {
				eligCPUs(p, map[string]string{"pve1": "Xeon A", "pve2": "Xeon B", "pve3": "Xeon B"})
			},
			in: host,
		},
		"storage definitions that could not be read": {
			profile:   func(p *Profile) { p.StoragesErr = "connection reset" },
			transient: true,
		},
		"a storage the definitions do not list": {
			in:        func(in VMInput) VMInput { in.Volumes = vols("gone:100/vm-101-disk-0"); return in },
			transient: true,
		},
		"a volume ID with no storage": {
			in: func(in VMInput) VMInput { in.Volumes = vols("vm-101-disk-0"); return in },
		},
		"a node-local disk under local_policy none": {
			in: func(in VMInput) VMInput { in.Volumes = vols("local-lvm:vm-101-disk-0"); return in },
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
			if tc.profile != nil {
				tc.profile(p)
			}
			in := baseInput(eligCfg(config.HAModeAuto), disks...)
			if tc.in != nil {
				in = tc.in(in)
			}
			d := mustDecide(t, p, in)
			if d.Outcome != OutcomeSkip || d.Transient != tc.transient {
				t.Fatalf("outcome %s transient %v, want skip with transient %v: %s", d.Outcome, d.Transient,
					tc.transient, d.Reason)
			}
		})
	}
}

// TestDecideVMReplicateRefusalIsTransientOnlyWithoutAProvenGap marks a
// replicate refusal transient when every gap may pass, such as a partner that
// is only in maintenance, and permanent when any gap is proven.
func TestDecideVMReplicateRefusalIsTransientOnlyWithoutAProvenGap(t *testing.T) {
	maintenance := func(p *Profile) {
		p.Health = &pve.NodeHealthReport{Nodes: []pve.NodeHealthEntry{
			{Name: "pve1", Placeable: true}, {Name: "pve2", Maintenance: true}, {Name: "pve3", Maintenance: true},
		}}
	}
	p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	maintenance(p)
	d := mustDecide(t, p, baseInput(replicateCfg(config.HAModeAuto), vols("tank:vm-101-disk-0")...))
	if d.Outcome != OutcomeSkip || !d.Transient || !strings.Contains(d.Reason, "no placeable partner") {
		t.Fatalf("a partner in maintenance: decision = %+v", d)
	}

	p = eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	p.VMReplicate, p.PermissionsErr = false, "permissions unreadable"
	d = mustDecide(t, p, baseInput(replicateCfg(config.HAModeAuto), vols("tank:vm-101-disk-0")...))
	if d.Outcome != OutcomeSkip || !d.Transient {
		t.Fatalf("an unreadable VM.Replicate grant: decision = %+v", d)
	}

	p = eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	maintenance(p)
	in := baseInput(replicateCfg(config.HAModeAuto), vols("tank:vm-101-disk-0")...)
	in.LinkedClone = true
	if d = mustDecide(t, p, in); d.Outcome != OutcomeSkip || d.Transient {
		t.Fatalf("a linked clone beside a partner in maintenance: decision = %+v", d)
	}

	p = eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
	p.VMReplicate = false
	if d = mustDecide(t, p, baseInput(replicateCfg(config.HAModeAuto), vols("tank:v")...)); d.Transient {
		t.Fatalf("a proven missing VM.Replicate grant: decision = %+v", d)
	}
}

// managedInput is the input of a VM that already runs under HA, as attach_disk
// gives it.
func managedInput(mode string, volumes ...VMVolume) VMInput {
	in := baseInput(eligCfg(mode), volumes...)
	in.HAManaged = true
	return in
}

// versions sets the pve-manager version of each named node.
func versions(p *Profile, byNode map[string]string) {
	for i := range p.Nodes {
		if v, ok := byNode[p.Nodes[i].Name]; ok {
			p.Nodes[i].Version = v
		}
	}
}

// deferredRetriable fails the test unless d ends the call with a transient,
// retriable error whose text holds every part of want.
func deferredRetriable(t *testing.T, d *VMDecision, want ...string) {
	t.Helper()
	if d.Outcome != OutcomeFail || !d.Transient || !cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a transient retriable failure, got outcome %s transient %v err %v", d.Outcome, d.Transient,
			d.Err)
	}
	for _, w := range want {
		if !strings.Contains(d.Err.Error(), w) {
			t.Errorf("error %q does not name %q", d.Err, w)
		}
	}
}

// TestDecideVMKeepsAnHAManagedDLBVMOnALowerVersion checks that a node of a
// lower version never takes a VM under HA out of HA. With every node at PVE 9
// or later only the dynamic load balancer turns off, and a node below PVE 9
// defers with a retriable error that names the node and its version.
func TestDecideVMKeepsAnHAManagedDLBVMOnALowerVersion(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0")
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode+": every node at PVE 9 turns only the dynamic load balancer off", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			versions(p, map[string]string{"pve1": "9.2.4", "pve2": "9.1.0", "pve3": "9.2.4"})
			p.DLBSupported, p.MinVersion = false, "9.1.0"
			in := managedInput(mode, disks...)
			in.InDLBAZ = true
			d := mustDecide(t, p, in)
			if !d.Registers() || d.DLB || d.Transient || d.Policy != PolicyShared {
				t.Fatalf("decision = %+v", d)
			}
			if !warned(d, "keeps its HA registration and pin") || !warned(d, "9.1.0") {
				t.Fatalf("warnings %v, want the dynamic load balancer turned off by name", d.Warnings)
			}
		})
		t.Run(mode+": dlb.enabled on PVE 9.1 keeps the VM registered", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			p.DLBSupported, p.MinVersion = false, "9.1.0"
			in := managedInput(mode, disks...)
			in.Config = eligCfg(mode, withDLB(true, "dlb"))
			if d := mustDecide(t, p, in); !d.Registers() || d.DLB {
				t.Fatalf("decision = %+v", d)
			}
		})
		t.Run(mode+": a node below PVE 9 defers and names the node", func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			versions(p, map[string]string{"pve1": "9.2.4", "pve2": "9.2.4", "pve3": "8.4.1"})
			p.DLBSupported = false
			in := managedInput(mode, disks...)
			in.InDLBAZ = true
			d := mustDecide(t, p, in)
			deferredRetriable(t, d, "VM 101", "node pve3 (8.4.1) runs", "PVE 9", "writes nothing to HA")
			if d.Registers() || d.Hard != nil || d.Admissible != nil {
				t.Fatalf("a deferral must give no pin: %+v", d)
			}
		})
	}
	t.Run("every node below PVE 9 is named", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		versions(p, map[string]string{"pve1": "9.2.4", "pve2": "8.4.1", "pve3": "8.3.0"})
		d := mustDecide(t, p, managedInput(config.HAModeAuto, disks...))
		deferredRetriable(t, d, "nodes pve2 (8.4.1), pve3 (8.3.0) run")
	})
	t.Run("an observe-only profile defers a VM under HA in both modes", func(t *testing.T) {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			p.Effective, p.Reason = EffectiveObserveOnly, "the entry node does not report the cluster quorate"
			p.Gaps = []string{p.Reason}
			deferredRetriable(t, mustDecide(t, p, managedInput(mode, disks...)), "quorate", "writes nothing to HA")
			p.Reason = ""
			deferredRetriable(t, mustDecide(t, p, managedInput(mode, disks...)), "quorate")
			p.Gaps = nil
			deferredRetriable(t, mustDecide(t, p, managedInput(mode, disks...)), string(EffectiveObserveOnly))
		}
	})
	t.Run("a create on PVE 9.1 still gives a sentinel VM no HA", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		p.DLBSupported, p.MinVersion = false, "9.1.0"
		in := baseInput(eligCfg(config.HAModeAuto), disks...)
		in.InDLBAZ = true
		if d := mustDecide(t, p, in); d.Outcome != OutcomeSkip || d.Transient {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestHAFloorError(t *testing.T) {
	var nilProfile *Profile
	if err := nilProfile.HAFloorError(101); err != nil {
		t.Fatalf("a nil profile knows no node below the floor: %v", err)
	}
	p := eligProfile(config.HAModeAuto, "pve1", "pve2")
	versions(p, map[string]string{"pve1": "9.0.3", "pve2": "garbage"})
	if err := p.HAFloorError(101); err != nil {
		t.Fatalf("PVE 9.0 and an unparsable version are not below the floor: %v", err)
	}
	p.Nodes[1].Online, p.Nodes[1].Version = false, "7.4.2"
	err := p.HAFloorError(101)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "node pve2 (7.4.2) runs") {
		t.Fatalf("an offline node of a known old version is below the floor too: %v", err)
	}
	if gate := p.RuleWriteGate(); gate == nil || !strings.Contains(gate.Error(), "pve2 (7.4.2)") {
		t.Fatalf("the rule gate names the same node: %v", gate)
	}
}

// TestDecideVMUnknownPlacedCPUNamesTheOperatorStep checks that a placed node
// whose CPU cannot be read defers in both modes, never leaves HA, and names the
// node, the probe failure, and the two steps that end the wait.
func TestDecideVMUnknownPlacedCPUNamesTheOperatorStep(t *testing.T) {
	disks := vols("nfs:100/vm-101-disk-0")
	steps := []string{"node pve1", "Fix the CPU reporting of node pve1", "pve.cpu_type", "x86-64-v3"}
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			p := eligProfile(mode, "pve1", "pve2", "pve3")
			eligCPUs(p, map[string]string{"pve1": "", "pve2": "Xeon A", "pve3": "Xeon A"})
			p.Nodes[0].CPUErr = "permission check failed (/nodes/pve1, Sys.Audit)"
			in := managedInput(mode, disks...)
			in.CPUType = eligHost
			d := mustDecide(t, p, in)
			if !d.Transient || d.Registers() {
				t.Fatalf("decision = %+v", d)
			}
			text := d.Reason
			if mode == config.HAModeManaged {
				if d.Outcome != OutcomeFail || !cpierrors.IsType(d.Err, cpierrors.TypeRetriableCloud) {
					t.Fatalf("managed must fail retriable: %+v", d)
				}
				text = d.Err.Error()
			} else if d.Outcome != OutcomeSkip || !warned(d, "Fix the CPU reporting") {
				t.Fatalf("auto must skip transient and warn: %+v", d)
			}
			for _, w := range append(steps, "Sys.Audit") {
				if !strings.Contains(text, w) {
					t.Errorf("%q does not name %q", text, w)
				}
			}
		})
	}
	t.Run("no recorded probe failure still names the steps", func(t *testing.T) {
		p := eligProfile(config.HAModeManaged, "pve1", "pve2", "pve3")
		eligCPUs(p, map[string]string{"pve1": "", "pve2": "Xeon A", "pve3": "Xeon A"})
		in := managedInput(config.HAModeManaged, disks...)
		in.CPUType = eligHost
		deferredRetriable(t, mustDecide(t, p, in), append(steps, "the probe recorded no reason")...)
	})
	t.Run("a node the profile does not list has no recorded failure", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1")
		if got := p.cpuErrOf("pve9"); got != "" {
			t.Fatalf("cpuErrOf = %q", got)
		}
	})
}

// TestDecideVMOutsideItsStrictPinDefers checks that a VM under HA that a
// migration put outside its strict pin never leaves HA. The decision defers
// with a retriable error that names the VM, its node, and the pin's nodes, in
// every policy that would otherwise refuse it, and writes no pin.
func TestDecideVMOutsideItsStrictPinDefers(t *testing.T) {
	pin := NodeConstraint{Name: "the VM's pin bosh-na-101", Nodes: []string{"pve3", "pve2"}, Pin: true}
	want := []string{"VM 101", "node pve1", "the VM's pin bosh-na-101", "[pve2,pve3]", "Migrate the VM back",
		"widen the pin", "writes nothing to HA"}
	for name, tc := range map[string]struct {
		mode   string
		policy string
		volume string
	}{
		"shared volumes in auto":          {config.HAModeAuto, config.HALocalPolicyNone, "nfs:100/vm-101-disk-0"},
		"shared volumes in managed":       {config.HAModeManaged, config.HALocalPolicyNone, "nfs:100/vm-101-disk-0"},
		"a local disk under policy none":  {config.HAModeAuto, config.HALocalPolicyNone, "local-lvm:vm-101-disk-0"},
		"a local disk under policy hold":  {config.HAModeAuto, config.HALocalPolicyHold, "local-lvm:vm-101-disk-0"},
		"a ZFS disk under policy replica": {config.HAModeAuto, config.HALocalPolicyReplicate, "tank:vm-101-disk-0"},
	} {
		t.Run(name, func(t *testing.T) {
			p := eligProfile(tc.mode, "pve1", "pve2", "pve3")
			in := managedInput(tc.mode, vols(tc.volume)...)
			in.Config = eligCfg(tc.mode, withPolicy(tc.policy))
			in.Constraints = []NodeConstraint{pin}
			d := mustDecide(t, p, in)
			deferredRetriable(t, d, want...)
			if d.Hard != nil || d.Admissible != nil || d.Priorities != nil {
				t.Fatalf("a deferral must give no pin: %+v", d)
			}
		})
	}
	t.Run("a VM inside its pin is decided as before", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, vols("nfs:100/vm-101-disk-0")...)
		in.Constraints = []NodeConstraint{{Name: pin.Name, Nodes: []string{"pve1", "pve2"}, Pin: true}}
		d := mustDecide(t, p, in)
		if !d.Registers() || !reflect.DeepEqual(d.Admissible, []string{"pve1", "pve2"}) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("a hardware constraint that excludes the node is no pin", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, vols("nfs:100/vm-101-disk-0")...)
		in.Constraints = []NodeConstraint{{Name: "bridge vmbr7", Nodes: []string{"pve2", "pve3"}}}
		d := mustDecide(t, p, in)
		if d.Outcome != OutcomeSkip || d.Transient || !strings.Contains(d.Reason, "outside its hard admissible set") {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("an empty pin defers too", func(t *testing.T) {
		p := eligProfile(config.HAModeAuto, "pve1", "pve2", "pve3")
		in := managedInput(config.HAModeAuto, vols("nfs:100/vm-101-disk-0")...)
		in.Constraints = []NodeConstraint{{Name: pin.Name, Pin: true}}
		deferredRetriable(t, mustDecide(t, p, in), "(none)")
	})
}
