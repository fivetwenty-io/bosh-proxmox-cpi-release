package handlers

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// cloneModeForHA adjusts the resolved clone_mode for a VM that PVE HA would
// manage on node-local storage. A linked clone's overlay depends on its base
// template's disk, and on node-local storage that base exists on one node only,
// so the clone cannot follow the VM to another node. In that case:
//
//   - clone_mode auto becomes full, so the VM gets a disk that is its own.
//   - clone_mode full stays full.
//   - clone_mode linked is the operator asking for two things that cannot both
//     hold. Managed mode fails the call before anything changes, and auto mode
//     keeps the linked clone and leaves the VM without HA, because the HA
//     decision refuses a hold pin for a linked clone on node-local storage.
//
// Any other VM keeps the mode it resolved.
func cloneModeForHA(
	ctx context.Context, deps Deps, logger *log.Logger, shape *createVMShape, mode string,
) (string, error) {
	if !haWouldManageLocalRoot(ctx, deps, shape) {
		return mode, nil
	}
	switch mode {
	case config.CloneModeLinked:
		if deps.Config.HAModeValue() == config.HAModeManaged {
			return "", linkedRootRefusal(deps, shape.vmStorage, false)
		}
		logger.Warn("create_vm: clone_mode=linked keeps a linked clone on node-local storage, so the VM runs without "+
			"HA; set clone_mode to auto or full to give the VM a disk HA can protect",
			log.String("vm_storage", shape.vmStorage), log.String("local_policy", deps.Config.HALocalPolicyValue()))
	case config.CloneModeFull:
	default:
		logger.Info("create_vm: forcing a full clone, because the VM would run under HA on node-local storage "+
			"and a linked clone's base template stays on one node",
			log.String("vm_storage", shape.vmStorage), log.String("clone_mode", mode))
		return config.CloneModeFull, nil
	}
	return mode, nil
}

// haWouldManageLocalRoot reports whether the VM being cloned would run under
// PVE HA with its root disk on node-local storage. The storage plan asks the
// same question through haLocalRootPredicate, so both clone paths agree.
func haWouldManageLocalRoot(ctx context.Context, deps Deps, shape *createVMShape) bool {
	return haLocalRootPredicate(ctx, deps)(shape.vmStorage)
}

// haLocalRootPredicate returns a function that reports whether a VM with its
// root disk on the named storage would run under PVE HA on node-local
// storage. That takes an HA mode that writes, a local policy other than none,
// a Director that can own the resource, a cluster profile that lets the CPI
// write HA state, and a storage that is node-local. Every fact it cannot read
// answers false, so a clone is forced to full only on facts the CPI has, and a
// managed call that lacks a prerequisite fails in the HA decision with the
// prerequisite's own error. The profile is read once, here, so the returned
// function does no I/O and the storage plan can call it under the journal
// lock.
func haLocalRootPredicate(ctx context.Context, deps Deps) func(storage string) bool {
	no := func(string) bool { return false }
	cfg := deps.Config
	if cfg == nil || cfg.HAModeValue() == config.HAModeOff || cfg.HALocalPolicyValue() == config.HALocalPolicyNone ||
		deps.RequestDirectorUUID == "" {
		return no
	}
	topology := cfg.HAStorageTopologyValue()
	if topology == config.HAStorageTopologyShared {
		// The HA decision refuses a node-local volume under this topology, so
		// no HA registration depends on the clone type.
		return no
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil || profile == nil || !profile.WritesAllowed() {
		return no
	}
	return func(storage string) bool {
		for i := range profile.Storages {
			if profile.Storages[i].Name == storage {
				return topology == config.HAStorageTopologyLocal || !profile.Storages[i].IsShared()
			}
		}
		return false
	}
}

// linkedRootRefusal is the error for a managed HA call whose root would be a
// linked clone on node-local storage. frozen is true when the plan that holds
// the clone was recorded earlier, so the clone mode can no longer change for
// this allocation.
func linkedRootRefusal(deps Deps, storage string, frozen bool) error {
	policy := deps.Config.HALocalPolicyValue()
	if frozen {
		return cpierrors.Cloud("create_vm: the recorded storage plan clones the root disk on storage %q as a linked "+
			"clone, and pve.ha.mode=managed with pve.ha.local_policy=%s cannot protect it: the storage is "+
			"node-local, and a linked clone's base template stays on one node. The plan is frozen, so clone_mode "+
			"cannot change it; set pve.ha.mode to auto or off, set pve.ha.local_policy to none, or use shared storage",
			storage, policy)
	}
	return cpierrors.Cloud("create_vm: clone_mode=linked conflicts with pve.ha.mode=managed and "+
		"pve.ha.local_policy=%s: storage %q is node-local, and a linked clone's base template stays on "+
		"one node, so HA cannot protect the VM; set clone_mode to auto or full, or use shared storage",
		policy, storage)
}

// managedLinkedRootRefusal returns the refusal for a storage plan whose root is
// a linked clone that managed HA could not protect, and nil for every other
// plan. It reads holdsLocalRoot, the predicate the plan was built with, so it
// does no I/O. A fresh plan reaches here only when clone_mode is linked,
// because the plan otherwise forces a full clone; a frozen plan reaches here
// when it was recorded before the settings that now refuse it.
func managedLinkedRootRefusal(
	deps Deps, holdsLocalRoot func(storage string) bool, root StoragePlanTarget, frozen bool,
) error {
	if deps.Config == nil || deps.Config.HAModeValue() != config.HAModeManaged ||
		root.Mechanism != storageMechanismLinkedClone || holdsLocalRoot == nil || !holdsLocalRoot(root.StorageID) {
		return nil
	}
	return linkedRootRefusal(deps, root.StorageID, frozen)
}
