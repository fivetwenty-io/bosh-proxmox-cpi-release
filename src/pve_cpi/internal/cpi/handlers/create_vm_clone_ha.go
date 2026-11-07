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
			return "", cpierrors.Cloud("create_vm: clone_mode=linked conflicts with pve.ha.mode=managed and "+
				"pve.ha.local_policy=%s: storage %q is node-local, and a linked clone's base template stays on "+
				"one node, so HA cannot protect the VM; set clone_mode to auto or full, or use shared storage",
				deps.Config.HALocalPolicyValue(), shape.vmStorage)
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
// PVE HA with its root disk on node-local storage. That takes an HA mode that
// writes, a local policy other than none, a Director that can own the
// resource, a cluster profile that lets the CPI write HA state, and a
// vm_storage that is node-local. Every fact it cannot read answers false, so a
// clone is forced to full only on facts the CPI has, and a managed call that
// lacks a prerequisite fails in the HA decision with the prerequisite's own
// error.
func haWouldManageLocalRoot(ctx context.Context, deps Deps, shape *createVMShape) bool {
	cfg := deps.Config
	if cfg == nil || cfg.HAModeValue() == config.HAModeOff || cfg.HALocalPolicyValue() == config.HALocalPolicyNone ||
		deps.RequestDirectorUUID == "" {
		return false
	}
	topology := cfg.HAStorageTopologyValue()
	if topology == config.HAStorageTopologyShared {
		// The HA decision refuses a node-local volume under this topology, so
		// no HA registration depends on the clone type.
		return false
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil || profile == nil || !profile.WritesAllowed() {
		return false
	}
	for i := range profile.Storages {
		if profile.Storages[i].Name == shape.vmStorage {
			return topology == config.HAStorageTopologyLocal || !profile.Storages[i].IsShared()
		}
	}
	return false
}
