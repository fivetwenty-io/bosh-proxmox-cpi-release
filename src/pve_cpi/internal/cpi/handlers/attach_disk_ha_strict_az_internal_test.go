package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// strictAZRig is an attach to VM 100 on pve01, whose root disk is on nfs, in
// the AZ z1 over az, with pin_az on and the given strictness, under mode. Its
// profile adds nfs-ac, which pve01 and pve03 reach, and parsed holds the cloud
// properties create_vm saw for the VM.
type strictAZRig struct {
	deps    Deps
	stub    *attachHAStub
	profile *ha.Profile
	parsed  *createVMParsedArgs
}

func newStrictAZRig(mode string, strict bool, az ...string) *strictAZRig {
	deps, stub, profile := attachRig(mode)
	cfg := naPinConfig(map[string][]string{"z1": az})
	cfg.HA = haPinOn(new(strict))
	cfg.HA.Mode = haChoiceOf(mode)
	deps.Config = cfg
	profile.Storages = append(profile.Storages, pve.StorageInfo{Name: "nfs-ac", Type: pve.StorageTypeNFS,
		Nodes: []string{"pve01", "pve03"}, Content: "images"})
	return &strictAZRig{deps: deps, stub: stub, profile: profile,
		parsed: &createVMParsedArgs{cloudProps: createVMCloudProps{AvailabilityZone: "z1"}}}
}

// down marks node offline in the rig's profile.
func (r *strictAZRig) down(node string) {
	for i := range r.profile.Nodes {
		if r.profile.Nodes[i].Name == node {
			r.profile.Nodes[i].Online = false
		}
	}
}

// createDecision is the HA decision create_vm makes for VM 100 on pve01 with
// the root disk on nfs and the volumes extra.
func (r *strictAZRig) createDecision(t *testing.T, extra ...string) *ha.VMDecision {
	t.Helper()
	cfg := map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3"}
	for i, volid := range extra {
		cfg[fmt.Sprintf("scsi%d", i+1)] = volid + ",size=10G"
	}
	ctx := context.Background()
	in := createdVMHAInput(ctx, r.deps, r.parsed, &createVMShape{node: "pve01"}, 100, cfg,
		r.profile, r.deps.Log(ctx))
	d, err := r.profile.DecideVM(in)
	if err != nil {
		t.Fatalf("create decision: %v", err)
	}
	return d
}

// pinAsCreated stores the pin create_vm writes for VM 100 with its root disk
// alone, and returns it.
func (r *strictAZRig) pinAsCreated(t *testing.T) *ha.Rule {
	t.Helper()
	d := r.createDecision(t)
	pin, err := ha.DecisionPin(d, naRuleNameFor(100), haIdentity{Owner: aaDirector}.marker(), []string{"nfs"},
		profileNodeNames(r.profile), profileRebalancesOnStart(r.profile), false)
	if err != nil {
		t.Fatalf("create pin: %v (decision %+v)", err, d)
	}
	r.stub.pin(pin.Strict, strings.Join(pin.NodeNames(), ","), "nfs")
	return pin
}

// assertKeepsHA checks that the attach keeps the VM under HA as a shared VM,
// with a strict pin over nodes that records storages.
func assertKeepsHA(t *testing.T, h *attachHA, err error, nodes []string, storages ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHAGuarded || h.newPin == nil {
		t.Fatalf("h %+v, want a guarded attach that keeps the VM under HA", h)
	}
	if h.decision.Policy != ha.PolicyShared || !slices.Equal(h.decision.Admissible, nodes) {
		t.Fatalf("decision %+v, want shared on %v", h.decision, nodes)
	}
	if !h.newPin.Strict || !slices.Equal(h.newPin.NodeNames(), nodes) {
		t.Fatalf("new pin %v strict %v, want strict %v", h.newPin.NodeNames(), h.newPin.Strict, nodes)
	}
	if rec, ok := ha.VolumeRecord(h.newPin.Comment); !ok || !slices.Equal(rec, storages) {
		t.Errorf("record %v, want %v", rec, storages)
	}
}

// TestDecideAttachHA_OneNodeStrictAZKeepsHA attaches a shared disk to a VM in a
// strict AZ of one node. create_vm registered the VM with a strict pin to that
// node, and the attach keeps it under HA with the same pin, in auto and in
// managed mode, rather than read the pin as an access set of one node.
func TestDecideAttachHA_OneNodeStrictAZKeepsHA(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			r := newStrictAZRig(mode, true, "pve01")
			if pin := r.pinAsCreated(t); !pin.Strict || !slices.Equal(pin.NodeNames(), []string{"pve01"}) {
				t.Fatalf("create pin %v strict %v, want strict pve01", pin.NodeNames(), pin.Strict)
			}
			h, err := decideAttachHA(context.Background(), r.deps, "100", sharedDisk())
			assertKeepsHA(t, h, err, []string{"pve01"}, "nfs", "nfs2")
		})
	}
}

// TestDecideAttachHA_MultiNodeStrictAZ attaches to a VM in a strict AZ of two
// nodes. A disk that only one node of the AZ reaches narrows the pin to that
// node and keeps the VM under HA, and a node of the AZ that is down leaves the
// pin as it is, because create_vm would register the same VM there.
func TestDecideAttachHA_MultiNodeStrictAZ(t *testing.T) {
	t.Run("a disk that one node of the AZ reaches narrows the pin to it", func(t *testing.T) {
		r := newStrictAZRig(config.HAModeAuto, true, "pve01", "pve02")
		r.pinAsCreated(t)
		h, err := decideAttachHA(context.Background(), r.deps, "100",
			resolvedDisk{volid: "nfs-ac:vm-100-disk-1", stableID: "abcd1234"})
		assertKeepsHA(t, h, err, []string{"pve01"}, "nfs", "nfs-ac")
	})
	t.Run("a node of the AZ that is down leaves the pin as it is", func(t *testing.T) {
		r := newStrictAZRig(config.HAModeAuto, true, "pve01", "pve02")
		r.pinAsCreated(t)
		r.down("pve02")
		h, err := decideAttachHA(context.Background(), r.deps, "100", sharedDisk())
		assertKeepsHA(t, h, err, []string{"pve01", "pve02"}, "nfs", "nfs2")
	})
}

// TestAttachAndCreateDecideStrictAZAlike checks that create_vm and attach_disk
// reach the same HA decision for the same VM, storage, and settings. The
// create decision covers the root disk and the new disk together. The attach
// decision starts from the pin create_vm wrote for the root disk alone, and it
// adds the new disk. A registration must give the same policy and admissible
// set, a decision of no HA must take the VM out of HA, and a failure must fail
// the attach.
func TestAttachAndCreateDecideStrictAZAlike(t *testing.T) {
	local := testPinStorage + ":vm-100-disk-1"
	for _, tc := range []struct {
		name   string
		strict bool
		az     []string
		disk   string
		down   string
		want   ha.Outcome
	}{
		{name: "a strict AZ of one node and a shared disk", strict: true, az: []string{"pve01"},
			disk: "nfs2:vm-100-disk-1", want: ha.OutcomeRegister},
		{name: "a strict AZ of one node and a local disk", strict: true, az: []string{"pve01"}, disk: local,
			want: ha.OutcomeSkip},
		{name: "a strict AZ of two nodes and a shared disk", strict: true, az: []string{"pve01", "pve02"},
			disk: "nfs2:vm-100-disk-1", want: ha.OutcomeRegister},
		{name: "a strict AZ of two nodes and a disk both reach", strict: true, az: []string{"pve01", "pve02"},
			disk: "nfs-ab:vm-100-disk-1", want: ha.OutcomeRegister},
		{name: "a strict AZ of two nodes and a disk one reaches", strict: true, az: []string{"pve01", "pve02"},
			disk: "nfs-ac:vm-100-disk-1", want: ha.OutcomeRegister},
		{name: "a strict AZ of two nodes with one down", strict: true, az: []string{"pve01", "pve02"},
			disk: "nfs2:vm-100-disk-1", down: "pve02", want: ha.OutcomeRegister},
		{name: "a preferred AZ of one node and a shared disk", strict: false, az: []string{"pve01"},
			disk: "nfs2:vm-100-disk-1", want: ha.OutcomeRegister},
	} {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			t.Run(mode+": "+tc.name, func(t *testing.T) {
				r := newStrictAZRig(mode, tc.strict, tc.az...)
				if tc.down != "" {
					r.down(tc.down)
				}
				r.pinAsCreated(t)
				created := r.createDecision(t, tc.disk)
				if created.Outcome != tc.want {
					t.Fatalf("create decision %+v, want %s", created, tc.want)
				}
				h, err := decideAttachHA(context.Background(), r.deps, "100",
					resolvedDisk{volid: tc.disk, stableID: "abcd1234"})
				switch created.Outcome {
				case ha.OutcomeRegister:
					if err != nil || h == nil || h.action != attachHAGuarded {
						t.Fatalf("create registers %+v; attach h %+v err %v, want a guarded attach", created, h, err)
					}
					if h.decision.Policy != created.Policy || !slices.Equal(h.decision.Admissible, created.Admissible) {
						t.Fatalf("create %s on %v; attach %s on %v", created.Policy, created.Admissible,
							h.decision.Policy, h.decision.Admissible)
					}
				case ha.OutcomeSkip:
					if err != nil || h == nil || h.action != attachHALeave {
						t.Fatalf("create gives no HA (%s); attach h %+v err %v, want the VM to leave HA",
							created.Reason, h, err)
					}
				default:
					if err == nil {
						t.Fatalf("create fails (%s); attach h %+v, want an error", created.Reason, h)
					}
				}
			})
		}
	}
}

// TestReaddDecisionInput_OneNodeStrictPinStaysShared decides the re-registration
// of a VM in a strict AZ of one node, whose record keeps the strict pin
// create_vm wrote to that node. The re-registration states the pin the way the
// attach does, so it registers the VM as shared on that node rather than read
// the pin as an access set of one node and give the VM no HA.
func TestReaddDecisionInput_OneNodeStrictPinStaysShared(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			r := newStrictAZRig(mode, true, "pve01")
			in := readdDecisionInput(r.deps.Config, 100, "pve01", map[string]any{
				"scsi0": "nfs:vm-100-disk-0,size=10G", "scsi1": "nfs2:vm-100-disk-1,size=10G", "cpu": "x86-64-v3",
			}, nil, ha.ReaddRecord{PinNodes: []string{"pve01"}, PinStrict: true})
			d, err := r.profile.DecideVM(in)
			if err != nil {
				t.Fatalf("DecideVM: %v", err)
			}
			if !d.Registers() || d.Policy != ha.PolicyShared || !slices.Equal(d.Admissible, []string{"pve01"}) {
				t.Fatalf("decision %+v, want shared on pve01", d)
			}
		})
	}
}
