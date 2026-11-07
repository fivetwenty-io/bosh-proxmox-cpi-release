package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedCloneStorage is the root storage of the managed clone fixtures, named
// in the HA profile as local or shared.
const managedCloneStorage = "source"

// managedCloneRequest returns a ranking request whose root is storage "source" with
// a stemcell template on it. A local fixture makes every storage a directory
// storage on both nodes, so the template clones linked when nothing forces a
// full clone, and a shared one keeps the NFS storages of planFixture.
func managedCloneRequest(t *testing.T, local bool, cloneMode string) StoragePlanRequest {
	t.Helper()
	request, _, _ := planFixture(t, func(source *planFixtureSource, cfg *config.CPIConfig) {
		// With no ephemeral set the root is the vm_storage pool, which may be
		// local, where a set member must be shared NFS. The fixture discovers
		// "source" as a companion storage, so the pool is that one.
		cfg.EphemeralStorageSet = ""
		cfg.VMStorage = managedCloneStorage
		if !local {
			return
		}
		source.defs = nil
		for _, id := range []string{"a", "b", "p", "source"} {
			source.defs = append(source.defs, planJSON(t, map[string]any{
				"storage": id, "type": "dir", "shared": 0, "path": "/" + id, "nodes": "n1,n2",
				"content": "images,iso,import",
			}))
		}
	})
	request.CloneMode = cloneMode
	request.Sources = []StorageRootSource{{
		Node: "n1", StorageID: managedCloneStorage, VolumeID: managedCloneStorage + ":100/base-100-disk-0.qcow2",
		TemplateVMID: 100, VirtualBytes: 5 << 30,
	}}
	return request
}

// managedCloneDeps returns deps for a Director whose HA block has the given
// mode and the hold local policy, over a profile that lists the root storage as
// local or shared.
func managedCloneDeps(haMode string, local bool) Deps {
	storage := pve.StorageInfo{Name: managedCloneStorage, Type: pve.StorageTypeNFS, Content: "images"}
	if local {
		storage.Type = pve.StorageTypeDir
	}
	profile := stagingProfile(haMode)
	profile.Storages = []pve.StorageInfo{storage}
	return Deps{
		Config: &config.CPIConfig{HA: &config.HAConfig{
			Mode: haChoiceOf(haMode), LocalPolicy: haChoiceOf(config.HALocalPolicyHold),
		}},
		Logger:              log.NewNopLogger(),
		RequestDirectorUUID: aaDirector,
		HAProfile:           func(context.Context) (*ha.Profile, error) { return profile, nil },
	}
}

// rankManagedClone ranks request the way the first placement does, with the
// plan's local-root question answered from deps.
func rankManagedClone(t *testing.T, request StoragePlanRequest, deps Deps) (*managedVMPlan, error) {
	t.Helper()
	request.HoldsLocalRoot = haLocalRootPredicate(t.Context(), deps)
	observed := &managedVMPlan{
		selection: request.Selection, inventory: request.Inventory, request: request,
		snapshotStart: request.Clock(),
	}
	return observed, rankManagedVMPlan(t.Context(), deps, &createVMParsedArgs{}, observed)
}

func managedCloneRoot(t *testing.T, observed *managedVMPlan) StoragePlanTarget {
	t.Helper()
	root, ok := managedVMRoleTarget(observed.plan, storageRoleRoot)
	if !ok || root.Source == nil || root.Source.TemplateVMID != 100 {
		t.Fatalf("plan froze no template root: %+v", observed.plan)
	}
	return root
}

func TestManagedPlanClonesALocalRootInFullUnderHold(t *testing.T) {
	t.Parallel()
	for _, haMode := range []string{config.HAModeManaged, config.HAModeAuto} {
		for _, cloneMode := range []string{"auto", "full"} {
			t.Run(haMode+"/"+cloneMode, func(t *testing.T) {
				t.Parallel()
				observed, err := rankManagedClone(t, managedCloneRequest(t, true, cloneMode),
					managedCloneDeps(haMode, true))
				if err != nil {
					t.Fatalf("a local root under hold was refused: %v", err)
				}
				if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismFullClone {
					t.Errorf("root mechanism = %q, want %q", got, storageMechanismFullClone)
				}
			})
		}
	}
}

// TestManagedPlanLocalRootCloneIsLinkedWithoutTheHAQuestion is the control for
// the forced full clone: the same fixture plans a linked clone when the plan is
// not told that HA would hold the root.
func TestManagedPlanLocalRootCloneIsLinkedWithoutTheHAQuestion(t *testing.T) {
	t.Parallel()
	request := managedCloneRequest(t, true, "auto")
	observed := &managedVMPlan{
		selection: request.Selection, inventory: request.Inventory, request: request,
		snapshotStart: request.Clock(),
	}
	if err := rankManagedVMPlan(t.Context(), managedCloneDeps(config.HAModeManaged, true),
		&createVMParsedArgs{}, observed); err != nil {
		t.Fatal(err)
	}
	if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismLinkedClone {
		t.Fatalf("fixture mechanism = %q, want %q", got, storageMechanismLinkedClone)
	}
}

func TestManagedPlanKeepsTheLinkedCloneOnASharedRoot(t *testing.T) {
	t.Parallel()
	observed, err := rankManagedClone(t, managedCloneRequest(t, false, "auto"),
		managedCloneDeps(config.HAModeManaged, false))
	if err != nil {
		t.Fatalf("a shared root was refused: %v", err)
	}
	if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismLinkedClone {
		t.Errorf("root mechanism = %q, want %q", got, storageMechanismLinkedClone)
	}
}

func TestManagedPlanKeepsTheLinkedCloneWhereHAIsNotInPlay(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*Deps){
		"mode off":    func(d *Deps) { d.Config.HA.Mode = haChoiceOf(config.HAModeOff) },
		"policy none": func(d *Deps) { d.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyNone) },
		"no director": func(d *Deps) { d.RequestDirectorUUID = "" },
		"profile unreadable": func(d *Deps) {
			d.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("boom") }
		},
		"storage not in the profile": func(d *Deps) {
			d.HAProfile = func(context.Context) (*ha.Profile, error) {
				profile := stagingProfile(config.HAModeManaged)
				profile.Storages = nil
				return profile, nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := managedCloneDeps(config.HAModeManaged, true)
			change(&deps)
			observed, err := rankManagedClone(t, managedCloneRequest(t, true, "auto"), deps)
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismLinkedClone {
				t.Errorf("root mechanism = %q, want the unchanged %q", got, storageMechanismLinkedClone)
			}
		})
	}
}

func TestManagedPlanExplicitLinkedCloneOnALocalRoot(t *testing.T) {
	t.Parallel()
	t.Run("managed refuses before anything is frozen", func(t *testing.T) {
		t.Parallel()
		observed, err := rankManagedClone(t, managedCloneRequest(t, true, "linked"),
			managedCloneDeps(config.HAModeManaged, true))
		if err == nil || !strings.Contains(err.Error(), "clone_mode=linked conflicts with pve.ha.mode=managed") {
			t.Fatalf("err = %v, want the linked clone conflict", err)
		}
		if observed.plan != nil {
			t.Errorf("a plan was frozen despite the refusal: %+v", observed.plan)
		}
		if cpierrors.IsType(managedVMPlanCPIError(err), cpierrors.TypeRetriableCloud) {
			t.Errorf("err = %v; an operator conflict is permanent, not retriable", err)
		}
	})
	t.Run("auto keeps the linked clone and the VM runs without HA", func(t *testing.T) {
		t.Parallel()
		observed, err := rankManagedClone(t, managedCloneRequest(t, true, "linked"),
			managedCloneDeps(config.HAModeAuto, true))
		if err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
		if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismLinkedClone {
			t.Errorf("root mechanism = %q, want %q", got, storageMechanismLinkedClone)
		}
	})
}

// linkedLocalPlan freezes a linked clone on local storage by ranking without
// the HA question, which is the plan an earlier configuration left behind.
func linkedLocalPlan(t *testing.T) (StoragePlanRequest, *StorageAllocationPlan) {
	t.Helper()
	request := managedCloneRequest(t, true, "auto")
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return request, plan
}

func TestManagedLinkedRootRefusal(t *testing.T) {
	t.Parallel()
	local := func(string) bool { return true }
	linked := StoragePlanTarget{
		Role: storageRoleRoot, StorageID: managedCloneStorage, Mechanism: storageMechanismLinkedClone,
	}
	full := linked
	full.Mechanism = storageMechanismFullClone
	managed := managedCloneDeps(config.HAModeManaged, true)
	auto := managedCloneDeps(config.HAModeAuto, true)

	err := managedLinkedRootRefusal(managed, local, linked, true)
	if err == nil {
		t.Fatal("a frozen linked root on local storage under managed hold was admitted")
	}
	for _, want := range []string{
		`storage "source"`, "pve.ha.local_policy=hold", "pve.ha.mode=managed", "frozen",
		"pve.ha.mode to auto or off", "pve.ha.local_policy to none", "shared storage",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must mention %q", err, want)
		}
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("err = %v; an operator conflict is permanent, not retriable", err)
	}

	for name, got := range map[string]error{
		"a full clone":     managedLinkedRootRefusal(managed, local, full, true),
		"auto mode":        managedLinkedRootRefusal(auto, local, linked, true),
		"a shared root":    managedLinkedRootRefusal(managed, func(string) bool { return false }, linked, true),
		"no predicate":     managedLinkedRootRefusal(managed, nil, linked, true),
		"no configuration": managedLinkedRootRefusal(Deps{}, local, linked, true),
	} {
		if got != nil {
			t.Errorf("%s was refused: %v", name, got)
		}
	}
}

// TestManagedResumeRefusesAFrozenLinkedLocalRootBeforeAnyClone resumes a
// generation whose frozen plan clones the root linked on local storage. The
// deps carry no PVE client, so any read or write before the refusal would
// fail the test with a nil dereference.
func TestManagedResumeRefusesAFrozenLinkedLocalRootBeforeAnyClone(t *testing.T) {
	request, plan := linkedLocalPlan(t)
	if got := managedCloneRoot(t, &managedVMPlan{plan: plan}).Mechanism; got != storageMechanismLinkedClone {
		t.Fatalf("fixture mechanism = %q, want %q", got, storageMechanismLinkedClone)
	}
	shape := &createVMShape{node: plan.Node, vmStorage: managedCloneStorage, rootDiskKey: "virtio0", rootDiskGiB: 5,
		vmDiskFormat: "qcow2"}
	execution, err := freezeManagedVMExecution(request.Selection.Policy, shape)
	if err != nil {
		t.Fatal(err)
	}
	plan.VMExecution = execution
	journal := siblingPlacementJournal(t, request.Namespace)
	intent, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{}`)},
		request.Selection, request.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(t.Context(), "agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})

	deps := managedCloneDeps(config.HAModeManaged, true)
	_, err = continueManagedVM(t.Context(), deps, &createVMParsedArgs{agentID: "agent"}, request.Selection, journal,
		handle, plan, nil)
	if err == nil || !strings.Contains(err.Error(), "recorded storage plan") ||
		!strings.Contains(err.Error(), `storage "source"`) {
		t.Fatalf("err = %v, want the frozen linked clone refusal naming storage source", err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("err = %v; the refusal must be a permanent CPI error", err)
	}
}
