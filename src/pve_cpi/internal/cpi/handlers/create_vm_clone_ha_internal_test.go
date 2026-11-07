package handlers

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cloneHARig is a cloneFromTemplate call for a VM whose root disk is on
// node-local LVM-thin storage, with the HA block, the local policy, and the
// clone mode of the test.
type cloneHARig struct {
	nodes   *cloneNodes
	deps    Deps
	shape   *createVMShape
	profile *ha.Profile
}

// newCloneHARig builds a rig whose vm_storage is storage of type storageType.
func newCloneHARig(t *testing.T, haMode, policy, cloneMode, storage, storageType string) *cloneHARig {
	t.Helper()
	nodes := &cloneNodes{}
	shared := storageType == pve.StorageTypeNFS
	deps := buildCloneDepsWithTopology(nodes, cloneMode, storage, storageType, 1, shared)
	deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(haMode), LocalPolicy: haChoiceOf(policy)}
	deps.RequestDirectorUUID = aaDirector
	r := &cloneHARig{
		nodes: nodes, deps: deps, profile: stagingProfile(haMode),
		shape: buildCloneShape(storage, storageType, "raw"),
	}
	r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return r.profile, nil }
	return r
}

func localCloneRig(t *testing.T, haMode, policy, cloneMode string) *cloneHARig {
	t.Helper()
	return newCloneHARig(t, haMode, policy, cloneMode, testPinStorage, pve.StorageTypeLVMThin)
}

func (r *cloneHARig) clone(logger *log.Logger) error {
	return cloneFromTemplate(context.Background(), r.deps, logger, r.shape, 300, "vm-300", "pve", 7300)
}

// wantFull fails the test unless the one clone call asked for a full clone, or
// for a linked one, and the shape says so.
func (r *cloneHARig) wantFull(t *testing.T, full bool) {
	t.Helper()
	if len(r.nodes.calls) != 1 {
		t.Fatalf("clone calls = %d, want 1", len(r.nodes.calls))
	}
	p := r.nodes.calls[0]
	if got := p.Full != nil && *p.Full; got != full {
		t.Errorf("full clone = %v, want %v", got, full)
	}
	if r.shape.linkedClone == full {
		t.Errorf("shape.linkedClone = %v after a clone with full = %v", r.shape.linkedClone, full)
	}
}

func TestCloneFromTemplate_HALocalVMGetsAFullCloneInEveryUnforcedMode(t *testing.T) {
	t.Parallel()
	for _, haMode := range []string{config.HAModeAuto, config.HAModeManaged} {
		for _, cloneMode := range []string{config.CloneModeAuto, config.CloneModeFull} {
			t.Run(haMode+"/"+cloneMode, func(t *testing.T) {
				t.Parallel()
				r := localCloneRig(t, haMode, config.HALocalPolicyHold, cloneMode)
				if err := r.clone(log.NewNopLogger()); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				r.wantFull(t, true)
				p := r.nodes.calls[0]
				if p.Storage == nil || *p.Storage != testPinStorage {
					t.Errorf("a full clone must name vm_storage %q, got %v", testPinStorage, p.Storage)
				}
			})
		}
	}
}

func TestCloneFromTemplate_HAForcedFullCloneIsLogged(t *testing.T) {
	t.Parallel()
	r := localCloneRig(t, config.HAModeAuto, config.HALocalPolicyHold, config.CloneModeAuto)
	var buf bytes.Buffer
	if err := r.clone(capturingLogger(t, &buf)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "forcing a full clone") {
		t.Errorf("log %q must say the clone was forced to full", buf.String())
	}
}

func TestCloneFromTemplate_ExplicitLinkedKeepsTheCloneInAutoAndDropsHA(t *testing.T) {
	t.Parallel()
	r := localCloneRig(t, config.HAModeAuto, config.HALocalPolicyHold, config.CloneModeLinked)
	var buf bytes.Buffer
	if err := r.clone(capturingLogger(t, &buf)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.wantFull(t, false)
	if !strings.Contains(buf.String(), "runs without HA") {
		t.Errorf("log %q must say the VM runs without HA", buf.String())
	}
}

func TestCloneFromTemplate_ExplicitLinkedFailsInManagedBeforeAnyClone(t *testing.T) {
	t.Parallel()
	r := localCloneRig(t, config.HAModeManaged, config.HALocalPolicyHold, config.CloneModeLinked)
	err := r.clone(log.NewNopLogger())
	if err == nil || !strings.Contains(err.Error(), "clone_mode=linked conflicts with pve.ha.mode=managed") {
		t.Fatalf("err = %v, want the linked clone conflict", err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("err = %v; an operator conflict is permanent, not retriable", err)
	}
	if len(r.nodes.calls) != 0 {
		t.Errorf("clone calls = %d, want none before the conflict is raised", len(r.nodes.calls))
	}
}

func TestCloneFromTemplate_LinkedCloneStaysWhereHAIsNotInPlay(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		haMode, policy string
		change         func(*cloneHARig)
	}{
		"mode off":    {config.HAModeOff, config.HALocalPolicyHold, nil},
		"policy none": {config.HAModeAuto, config.HALocalPolicyNone, nil},
		"no director": {config.HAModeAuto, config.HALocalPolicyHold, func(r *cloneHARig) {
			r.deps.RequestDirectorUUID = ""
		}},
		"profile cannot write": {config.HAModeAuto, config.HALocalPolicyHold, func(r *cloneHARig) {
			r.profile.Effective = ha.EffectiveObserveOnly
		}},
		"profile unreadable": {config.HAModeAuto, config.HALocalPolicyHold, func(r *cloneHARig) {
			r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("boom") }
		}},
		"storage not listed": {config.HAModeAuto, config.HALocalPolicyHold, func(r *cloneHARig) {
			r.profile.Storages = nil
		}},
		"topology shared": {config.HAModeAuto, config.HALocalPolicyHold, func(r *cloneHARig) {
			r.deps.Config.HA.StorageTopology = haChoiceOf(config.HAStorageTopologyShared)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := localCloneRig(t, tc.haMode, tc.policy, config.CloneModeAuto)
			if tc.change != nil {
				tc.change(r)
			}
			if err := r.clone(log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			r.wantFull(t, false)
		})
	}
}

func TestCloneFromTemplate_SharedRootStorageKeepsTheLinkedClone(t *testing.T) {
	t.Parallel()
	r := newCloneHARig(t, config.HAModeAuto, config.HALocalPolicyHold, config.CloneModeAuto, "nfs", pve.StorageTypeNFS)
	if err := r.clone(log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.wantFull(t, false)
}

func TestCloneFromTemplate_StorageTopologyLocalForcesAFullCloneOnSharedStorage(t *testing.T) {
	t.Parallel()
	r := newCloneHARig(t, config.HAModeAuto, config.HALocalPolicyHold, config.CloneModeAuto, "nfs", pve.StorageTypeNFS)
	r.deps.Config.HA.StorageTopology = haChoiceOf(config.HAStorageTopologyLocal)
	if err := r.clone(log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.wantFull(t, true)
}

func TestMarkLinkedRootDisk(t *testing.T) {
	t.Parallel()
	volumes := func() []ha.VMVolume {
		return []ha.VMVolume{{Volume: "local-lvm:vm-1-disk-0"}, {Volume: "local-lvm:vm-1-disk-1"}}
	}
	cfg := map[string]any{"virtio0": "local-lvm:vm-1-disk-0,size=10G"}
	t.Run("a linked clone marks the root disk only", func(t *testing.T) {
		t.Parallel()
		v := volumes()
		markLinkedRootDisk(v, cfg, &createVMShape{rootDiskKey: "virtio0", linkedClone: true})
		if !v[0].LinkedClone || v[1].LinkedClone {
			t.Errorf("volumes %+v, want only the root disk marked", v)
		}
	})
	for name, shape := range map[string]*createVMShape{
		"a full clone":          {rootDiskKey: "virtio0"},
		"no shape":              nil,
		"a root key not in cfg": {rootDiskKey: "scsi0", linkedClone: true},
	} {
		t.Run(name+" marks nothing", func(t *testing.T) {
			t.Parallel()
			v := volumes()
			markLinkedRootDisk(v, cfg, shape)
			if v[0].LinkedClone || v[1].LinkedClone {
				t.Errorf("volumes %+v, want none marked", v)
			}
		})
	}
	t.Run("an ISO is never marked", func(t *testing.T) {
		t.Parallel()
		v := []ha.VMVolume{{Volume: "local-lvm:vm-1-disk-0", Kind: ha.VolumeISO}}
		markLinkedRootDisk(v, cfg, &createVMShape{rootDiskKey: "virtio0", linkedClone: true})
		if v[0].LinkedClone {
			t.Errorf("volumes %+v, want the ISO unmarked", v)
		}
	})
}

func TestVolumeIsLinkedClone(t *testing.T) {
	t.Parallel()
	for volid, want := range map[string]bool{
		"nfs:base-9000-disk-0/vm-100-disk-0.qcow2": true,
		"nfs:100/vm-100-disk-0.qcow2":              false,
		"local-lvm:vm-100-disk-0":                  false,
		"local-lvm:base-9000-disk-0":               false,
		"novolid":                                  false,
	} {
		if got := volumeIsLinkedClone(volid); got != want {
			t.Errorf("volumeIsLinkedClone(%q) = %v, want %v", volid, got, want)
		}
	}
}
