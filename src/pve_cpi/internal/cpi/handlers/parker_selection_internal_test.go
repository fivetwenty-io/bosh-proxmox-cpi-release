package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// registeredParkerRig is parker 90000 on pve01 holding volumes, registered
// with PVE HA by this Director, under mode.
func registeredParkerRig(mode string, volumes ...string) *phRig {
	r := newPHRig(mode, map[int]map[string]any{90000: phParker(volumes...)})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateStopped, comment: ourMarker()}
	return r
}

func admit(t *testing.T, r *phRig, volid string) (bool, error) {
	t.Helper()
	return admitDiskIntoParker(context.Background(), r.deps, phNode, 90000, volid)
}

func TestAdmitDiskIntoParker_UnregisteredTakesEveryDisk(t *testing.T) {
	r := newPHRig(config.HAModeOff, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	for _, volid := range []string{"ac:vm-700-disk-1", "local-lvm:vm-700-disk-1", "gone:vm-700-disk-1"} {
		ok, err := admit(t, r, volid)
		if err != nil || !ok {
			t.Fatalf("%s: an unregistered parker takes every disk; ok %v err %v", volid, ok, err)
		}
	}
}

func TestAdmitDiskIntoParker_StrictPinKeepsTheReach(t *testing.T) {
	// Off mode on purpose: a parker registered while HA was on keeps its
	// resource, and the rule reads it in every mode.
	r := registeredParkerRig(config.HAModeOff, "ab:vm-90000-disk-0")
	r.cluster.setPin(true, "pve01,pve02", ourMarker()+" bosh-vols=ab")
	for volid, want := range map[string]bool{
		"ab:vm-700-disk-1":        true,
		"ac:vm-700-disk-1":        false,
		"local-lvm:vm-700-disk-1": false,
	} {
		ok, err := admit(t, r, volid)
		if err != nil || ok != want {
			t.Errorf("%s: ok %v err %v, want %v", volid, ok, err, want)
		}
	}
}

func TestAdmitDiskIntoParker_NoStrictPinNeedsEveryNode(t *testing.T) {
	r := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
	r.cluster.setPin(false, "pve01", ourMarker()+" bosh-vols=nfs")
	for volid, want := range map[string]bool{"nfs:vm-700-disk-1": true, "ab:vm-700-disk-1": false} {
		ok, err := admit(t, r, volid)
		if err != nil || ok != want {
			t.Errorf("%s: ok %v err %v, want %v", volid, ok, err, want)
		}
	}
}

func TestAdmitDiskIntoParker_ADisabledPinDoesNotNarrowTheReach(t *testing.T) {
	r := registeredParkerRig(config.HAModeAuto, "ab:vm-90000-disk-0")
	r.cluster.setPin(true, "pve01,pve02", ourMarker())
	rule := r.cluster.rules[naRuleNameFor(90000)]
	disabled := true
	rule.Disable = &disabled
	r.cluster.rules[naRuleNameFor(90000)] = rule
	ok, err := admit(t, r, "ab:vm-700-disk-1")
	if err != nil || ok {
		t.Fatalf("a disabled pin confines nothing, so the disk must reach every node; ok %v err %v", ok, err)
	}
}

func TestAdmitDiskIntoParker_HeldVolumesWidenTheReach(t *testing.T) {
	// The parker's pin names pve01 and pve02, and the disk it holds reaches
	// pve03 too, which a narrower disk that left since allowed. The rule keeps
	// the parker's access set as it is, so a disk that would narrow it below
	// pve03 goes elsewhere.
	r := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
	r.cluster.setPin(true, "pve01,pve02", ourMarker())
	ok, err := admit(t, r, "ab:vm-700-disk-1")
	if err != nil || ok {
		t.Fatalf("ab does not reach pve03, which the held disk reaches; ok %v err %v", ok, err)
	}
}

func TestAdmitDiskIntoParker_StorageTopologyLocal(t *testing.T) {
	r := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
	r.deps.Config.HA.StorageTopology = haChoiceOf(config.HAStorageTopologyLocal)
	ok, err := admit(t, r, "nfs:vm-700-disk-1")
	if err != nil || ok {
		t.Fatalf("under storage_topology local every disk is node-local; ok %v err %v", ok, err)
	}
}

func TestAdmitDiskIntoParker_UnknownAnswersAreRetriable(t *testing.T) {
	cases := map[string]func(*phRig){
		"HA resource read": func(r *phRig) { r.cluster.getErr = errors.New("500 pve02 unreachable") },
		"HA rules read":    func(r *phRig) { r.cluster.rulesErr = errors.New("500 rules unavailable") },
		"parker config":    func(r *phRig) { r.client.configErr[90000] = errors.New("500 pmxcfs timeout") },
		"HA profile": func(r *phRig) {
			r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("boom") }
		},
		"storage reads": func(r *phRig) { r.profile.StoragesErr = "500 storage.cfg unreadable" },
		"no node":       func(r *phRig) { r.profile.Nodes = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
			mutate(r)
			ok, err := admit(t, r, "nfs:vm-700-disk-1")
			if ok || err == nil || !okToRetryCPIError(err) {
				t.Fatalf("want a retriable error and no admission; ok %v err %v", ok, err)
			}
		})
	}
	t.Run("a storage the profile does not list", func(t *testing.T) {
		r := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
		ok, err := admit(t, r, "gone:vm-700-disk-1")
		if ok || err == nil || !okToRetryCPIError(err) || !strings.Contains(err.Error(), "gone") {
			t.Fatalf("want a retriable error naming the storage; ok %v err %v", ok, err)
		}
	})
}

func TestParkerWriteConfigFor_CarriesTheSelectionRule(t *testing.T) {
	deps := idTestDeps(newIDFakeClient(nil))
	if parkerWriteConfigFor(deps).Admit == nil {
		t.Fatal("every park must carry the parker selection rule")
	}
	if parkerReadConfigFor(deps).Admit != nil {
		t.Fatal("a read never parks, so the read config carries no rule")
	}
}

func TestVolumeStorageAndNodesOutside(t *testing.T) {
	if got := volumeStorage(" ab:vm-1-disk-0 "); got != "ab" {
		t.Errorf("volumeStorage = %q", got)
	}
	if got := volumeStorage("bare"); got != "bare" {
		t.Errorf("volumeStorage of an unqualified ID = %q", got)
	}
	if got := nodesOutside([]string{"c", "a", "b"}, []string{"b"}); strings.Join(got, ",") != "a,c" {
		t.Errorf("nodesOutside = %v", got)
	}
}

// detachABAC runs the AB then AC case on the detach path. Parker 90000 holds
// a disk on storage ab, and, when registered is true, PVE HA manages it with
// a strict pin on pve1 and pve2. Parker 90001 is empty. The detach parks a
// disk on storage ac, which pve2 cannot reach.
func detachABAC(t *testing.T, registered bool) (*idFakeClient, *phCluster) {
	t.Helper()
	const birth = "ac:vm-9001-disk-0"
	const attachedVolid = "ac:vm-700-disk-1"
	c := newIDFakeClient(map[int]map[string]any{
		700:   {"scsi1": attachedVolid + ",serial=" + idTestToken + ",size=10G"},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, "scsi0": "ab:vm-90000-disk-0"},
		90001: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	hac := newPHCluster("pve1", "pve2", "pve3")
	if registered {
		hac.res[phSID] = &phResource{state: parkerHAStateStopped, comment: ourMarker()}
		hac.setPin(true, "pve1,pve2", ourMarker()+" bosh-vols=ab")
	}
	c.ha = hac
	deps := idTestDeps(c)
	deps.Config.DiskStorage = "ac"
	deps.RequestDirectorUUID = aaDirector
	profile := &ha.Profile{
		Effective: ha.EffectiveFull,
		Storages: []pve.StorageInfo{
			{Name: "ab", Type: pve.StorageTypeNFS, Content: "images", Nodes: []string{"pve1", "pve2"}},
			{Name: "ac", Type: pve.StorageTypeNFS, Content: "images", Nodes: []string{"pve1", "pve3"}},
		},
	}
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		profile.Nodes = append(profile.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
	}
	deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }

	ctx := context.Background()
	diskCID := overlayCID(t, birth, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	bare, meta, err := decodeDiskCID(ctx, deps, "detach_disk", diskCID)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rd, err := resolveDiskForOp(ctx, deps, "detach_disk", diskCID, bare, meta)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := handleDetachStableID(ctx, deps, "700", 700, rd); err != nil {
		t.Fatalf("detach: %v", err)
	}
	return c, hac
}

// holdsStorage reports whether a parker config has a bus slot on storage.
func holdsStorage(cfg map[string]any, storage string) bool {
	for k, v := range cfg {
		if s, ok := v.(string); ok && strings.HasPrefix(k, "scsi") && strings.HasPrefix(s, storage+":") {
			return true
		}
	}
	return false
}

// TestHandleDetachStableID_KeepsARegisteredParkersReach proves the detached
// ac disk lands in parker 90001 when 90000 is registered, and that the pin and
// parker 90000 stay as they were. The unregistered control shows the same
// detach lands in 90000, so the registration is what moved the disk.
func TestHandleDetachStableID_KeepsARegisteredParkersReach(t *testing.T) {
	t.Parallel()
	t.Run("registered", func(t *testing.T) {
		t.Parallel()
		c, hac := detachABAC(t, true)
		if !holdsStorage(c.configs[90001], "ac") || holdsStorage(c.configs[90000], "ac") {
			t.Fatalf("the ac disk must land in parker 90001; 90001 %v, 90000 %v", c.configs[90001], c.configs[90000])
		}
		pin := hac.rules[naRuleNameFor(90000)]
		if pin.Nodes == nil || *pin.Nodes != "pve1,pve2" || len(hac.log.events) != 0 {
			t.Fatalf("the pin must stay as it was; pin %+v events %v", pin, hac.log.events)
		}
	})
	t.Run("unregistered control", func(t *testing.T) {
		t.Parallel()
		c, _ := detachABAC(t, false)
		if !holdsStorage(c.configs[90000], "ac") {
			t.Fatalf("without a registration the ac disk lands in the first parker; 90000 %v", c.configs[90000])
		}
	})
}
