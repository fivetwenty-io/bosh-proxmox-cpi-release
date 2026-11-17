package handlers

import (
	"context"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// warnUnreachableLocalVolumes logs a warning for every volume of the VM that
// sits on a node-local storage the node holding the VM cannot reach. A VM
// whose config was moved off a dead node by hand is the usual case. The
// destroy runs from the node that holds the config, so it cannot free a
// volume on another node's local storage, and neither PVE nor the destroy
// task says anything about it. The volume stays behind on the node that
// holds it, and an operator has to remove it once that node is back.
//
// The check only logs. It never changes what the delete destroys and it never
// fails the delete, so any read that goes wrong ends the check quietly. It
// reuses the config the delete already read. It asks the node for each
// storage's status, and it reads the storage index only for a storage the node
// reports as inactive.
func warnUnreachableLocalVolumes(
	ctx context.Context, deps Deps, vmCID string, vmid int, node string, configs []map[string]any, logger *log.Logger,
) {
	var volumes []pve.AccessVolume
	seen := make(map[string]bool)
	for _, cfg := range configs {
		for _, v := range pve.ConfigAccessVolumes(cfg, node) {
			if !seen[v.Volume] {
				seen[v.Volume] = true
				volumes = append(volumes, v)
			}
		}
	}
	if len(volumes) == 0 || deps.PVE == nil {
		return
	}
	// Only a storage that its node reports as inactive needs classifying, so a
	// delete whose volumes are all reachable reads no storage index.
	var byName map[string]pve.StorageInfo
	indexRead := false
	active := make(map[string]bool)
	for _, v := range volumes {
		name, _, ok := strings.Cut(v.Volume, ":")
		if !ok || name == "" {
			continue
		}
		isActive, asked := active[name]
		if !asked {
			isActive = storageActiveOnNode(ctx, deps, node, name)
			active[name] = isActive
		}
		if isActive {
			continue
		}
		if !indexRead {
			indexRead = true
			storages, err := readStorageIndex(ctx, deps)
			if err != nil {
				logger.Debug("delete_vm: could not read the storage index to check for volumes this node cannot reach",
					log.String("node", node), log.Err(err))
			} else {
				byName = make(map[string]pve.StorageInfo, len(storages))
				for i := range storages {
					byName[storages[i].Name] = storages[i]
				}
			}
		}
		warnUnreachableVolume(logger, vmCID, vmid, node, v.Volume, name, byName)
	}
}

// warnUnreachableVolume logs the warning for one volume on a storage its node
// reports as inactive, unless the storage index shows the storage is shared
// or does not carry it, in which case the node's status says nothing about
// where the volume lives.
func warnUnreachableVolume(
	logger *log.Logger, vmCID string, vmid int, node, volid, storage string, index map[string]pve.StorageInfo,
) {
	info, known := index[storage]
	if !known || info.IsShared() {
		return
	}
	reason := "the storage is not active on this node"
	if len(info.Nodes) > 0 && !slices.Contains(info.Nodes, node) {
		reason = "the storage is not configured for this node"
	}
	attrs := []log.Field{
		log.String("vm_cid", vmCID), log.Int("vmid", vmid), log.String("volid", volid),
		log.String("storage", storage), log.String("node", node), log.String("reason", reason),
	}
	msg := "delete_vm: the VM's volume " + volid + " is on node-local storage " + storage +
		" that this node cannot reach, so the destroy cannot free it. It will stay behind on the node that" +
		" holds it, and needs removing there once that node is back"
	if len(info.Nodes) > 0 {
		attrs = append(attrs, log.String("likely_node", strings.Join(info.Nodes, ",")))
		msg += " (most likely " + strings.Join(info.Nodes, " or ") + ")"
	}
	logger.Warn(msg, attrs...)
}

// storageActiveOnNode reports whether node's own status for the storage says
// it is active. A status read that fails counts as not active, because the
// destroy would not reach the storage either.
func storageActiveOnNode(ctx context.Context, deps Deps, node, storage string) bool {
	status, err := deps.PVE.Nodes().ListStorageStatus(ctx, node, storage)
	if err != nil || status == nil || status.Active == nil {
		return false
	}
	return status.Active.Bool()
}
