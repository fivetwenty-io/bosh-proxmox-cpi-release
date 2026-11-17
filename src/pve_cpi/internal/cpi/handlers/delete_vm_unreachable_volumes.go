package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// warnUnreachableLocalVolumes logs a warning for every volume of the VM that
// sits on a node-local storage and that the node holding the VM does not hold.
// A VM whose config was moved off a dead node by hand is the usual case. The
// destroy runs from the node that holds the config, so it cannot free a
// volume that lives on another node's local storage. When the storage is
// defined for every node, it is active on this node too, and the destroy
// looks in this node's own copy of the storage, finds nothing, and skips the
// volume without a word. Neither PVE nor the destroy task says anything about
// it, and the volume stays behind on the node that holds it.
//
// A volume counts as out of reach when its storage is inactive or absent on
// the node, or when the storage is active there and the node's content
// listing for it does not show the volume. The check only logs. It never
// changes what the delete destroys and it never fails the delete, so any read
// that goes wrong ends the check for that volume quietly. It reuses the config
// the delete already read, asks the node for each storage's status, and reads
// each active storage's content once. It reads the cluster storage index once,
// and only when a volume looks out of reach, to leave shared storage alone.
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
	// The listing is limited to this VM when every volume on the storage is
	// named for it, because a volume another VM owns would not appear in it.
	ownPrefix := "vm-" + strconv.Itoa(vmid) + "-"
	onlyOwn := make(map[string]bool)
	for _, v := range volumes {
		if name, vol, ok := strings.Cut(v.Volume, ":"); ok {
			own := strings.Contains(vol, ownPrefix)
			if prev, known := onlyOwn[name]; !known {
				onlyOwn[name] = own
			} else {
				onlyOwn[name] = prev && own
			}
		}
	}
	type listing struct {
		held  map[string]bool
		known bool
		why   string
	}
	listings := make(map[string]listing)
	var byName map[string]pve.StorageInfo
	indexRead := false
	for _, v := range volumes {
		name, _, ok := strings.Cut(v.Volume, ":")
		if !ok || name == "" {
			continue
		}
		l, asked := listings[name]
		if !asked {
			l.held, l.why, l.known = storageVolumesOnNode(ctx, deps, node, name, vmid, onlyOwn[name])
			listings[name] = l
		}
		if !l.known || l.held[v.Volume] {
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
		warnUnreachableVolume(logger, vmCID, vmid, node, v.Volume, name, l.why, byName)
	}
}

// warnUnreachableVolume logs the warning for one volume that its node does not
// hold, unless the storage index shows the storage is shared or does not carry
// it, in which case the node's view says nothing about where the volume lives.
// The message covers both causes, a storage the node cannot use and a volume
// the node's copy of the storage does not hold, without guessing between them.
func warnUnreachableVolume(
	logger *log.Logger, vmCID string, vmid int, node, volid, storage, why string, index map[string]pve.StorageInfo,
) {
	info, known := index[storage]
	if !known || info.IsShared() {
		return
	}
	attrs := []log.Field{
		log.String("vm_cid", vmCID), log.Int("vmid", vmid), log.String("volid", volid),
		log.String("storage", storage), log.String("node", node), log.String("reason", why),
	}
	msg := "delete_vm: the VM's volume " + volid + " is on node-local storage " + storage +
		" and node " + node + " does not hold it, either because the storage is not usable on this node or because" +
		" this node's copy of the storage has no such volume, so the destroy cannot free it. It will stay behind on" +
		" the node that holds it, and needs removing there once that node is back"
	if len(info.Nodes) > 0 {
		attrs = append(attrs, log.String("likely_node", strings.Join(info.Nodes, ",")))
		msg += " (most likely " + strings.Join(info.Nodes, " or ") + ")"
	}
	logger.Warn(msg, attrs...)
}

// storageVolumesOnNode returns the volume IDs node's own content listing
// shows for the storage. A storage the node reports inactive, or does not
// report at all, holds none, and the reason says which. known is false when
// the storage is shared, or the node says it is active and its listing could
// not be read, so the check has nothing to judge by. Only this VM's volumes are listed when
// onlyOwn is set.
func storageVolumesOnNode(
	ctx context.Context, deps Deps, node, storage string, vmid int, onlyOwn bool,
) (held map[string]bool, reason string, known bool) {
	status, err := deps.PVE.Nodes().ListStorageStatus(ctx, node, storage)
	if err != nil || status == nil {
		return nil, "the storage is not reported on this node", true
	}
	// The node's status carries the storage's type and shared flag, so shared
	// storage ends the check here without a listing.
	if (pve.StorageInfo{Type: status.Type, Shared: status.Shared != nil && status.Shared.Bool()}).IsShared() {
		return nil, "", false
	}
	if status.Active == nil || !status.Active.Bool() {
		return nil, "the storage is not active on this node", true
	}
	params := &nodes.ListStorageContentParams{}
	if onlyOwn {
		id := int64(vmid)
		params.Vmid = &id
	}
	resp, err := deps.PVE.Nodes().ListStorageContent(ctx, node, storage, params)
	if err != nil || resp == nil {
		return nil, "", false
	}
	held = make(map[string]bool, len(*resp))
	for _, raw := range *resp {
		var item struct {
			VolID string `json:"volid"`
		}
		if json.Unmarshal(raw, &item) == nil && item.VolID != "" {
			held[item.VolID] = true
		}
	}
	return held, "the volume is not in this node's content for the storage", true
}
