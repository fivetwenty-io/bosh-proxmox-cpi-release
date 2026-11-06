package pve

import (
	"context"
	"fmt"
	"sort"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// StorageInfoGetter is the slice of StorageInfoCache that VolumeAccessSet
// reads: one storage's classification by name.
type StorageInfoGetter interface {
	Get(ctx context.Context, name string) (StorageInfo, error)
}

// AccessVolume is one volume a VM holds, as VolumeAccessSet needs it.
type AccessVolume struct {
	// Volume is the storage-qualified volume ID, such as "ceph:vm-100-disk-0".
	Volume string

	// HomeNode is the node a volume on node-local storage lives on. It is
	// ignored for shared storage and required for local storage, because a
	// local volume is reachable from that one node alone.
	HomeNode string
}

// VolumeAccessSet returns the nodes from which every one of volumes is
// reachable, sorted by name. members is the cluster's node set.
//
// Each volume contributes the nodes of its storage: the storage's `nodes`
// list when it has one, otherwise every member. A storage that PVE disables
// contributes none. A volume on node-local storage narrows that to its home
// node, since only that node holds the bytes. The answer is the intersection
// of those sets, so a root disk on storage for nodes A and B with a persistent
// disk on storage for nodes A and C gives A alone, even though each storage
// reaches two nodes. The caller lists every volume the VM references,
// including its snapshot and VM-state volumes (see ConfigAccessVolumes and
// VMAccessVolumes).
//
// An empty slice means no node reaches them all. A volume that names no
// storage, or a storage the index does not carry, is an error, because
// guessing it reaches every node could register a VM that HA cannot recover.
// No volumes at all gives every member.
func VolumeAccessSet(
	ctx context.Context, storages StorageInfoGetter, members []string, volumes []AccessVolume,
) ([]string, error) {
	if storages == nil {
		return nil, cpierrors.Cloud("VolumeAccessSet: storage info source must not be nil")
	}
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m != "" {
			set[m] = struct{}{}
		}
	}
	infos := make(map[string]StorageInfo)
	for _, v := range volumes {
		storage, name, ok := strings.Cut(v.Volume, ":")
		if !ok || storage == "" || name == "" {
			return nil, cpierrors.Cloud("VolumeAccessSet: %q is not a storage-qualified volume ID", v.Volume)
		}
		info, seen := infos[storage]
		if !seen {
			var err error
			if info, err = storages.Get(ctx, storage); err != nil {
				return nil, cpierrors.Wrap(err, fmt.Sprintf("VolumeAccessSet: classify storage %s of %s", storage, v.Volume))
			}
			infos[storage] = info
		}
		reach, known := storageNodeSet(info, v.HomeNode)
		if !known {
			return nil, cpierrors.Cloud("VolumeAccessSet: %s is on node-local storage %s and no home node is known",
				v.Volume, storage)
		}
		if reach == nil {
			continue
		}
		for n := range set {
			if _, in := reach[n]; !in {
				delete(set, n)
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// storageNodeSet returns the nodes info reaches for a volume with the given
// home node. A nil set with known true means the storage reaches every member.
// known is false when the storage is node-local and homeNode is empty, so the
// reach cannot be stated. An empty non-nil set means no node reaches it.
func storageNodeSet(info StorageInfo, homeNode string) (nodes map[string]struct{}, known bool) {
	if info.Disabled {
		return map[string]struct{}{}, true
	}
	if !info.IsShared() {
		if homeNode == "" {
			return nil, false
		}
		if len(info.Nodes) > 0 && !containsString(info.Nodes, homeNode) {
			return map[string]struct{}{}, true
		}
		return map[string]struct{}{homeNode: {}}, true
	}
	if len(info.Nodes) == 0 {
		return nil, true
	}
	out := make(map[string]struct{}, len(info.Nodes))
	for _, n := range info.Nodes {
		out[n] = struct{}{}
	}
	return out, true
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ConfigAccessVolumes lists the volumes a QEMU config (or one snapshot's
// config) references: every bus slot, efidisk, tpmstate, and unused entry, the
// cloud-init and ISO drives among them, and the vmstate key a snapshot or a
// hibernated VM carries. Each is stamped with homeNode, the node that holds
// node-local volumes. Entries that name no volume, which are empty values,
// "none", and a bare "cdrom", are skipped, and a duplicate volume ID appears
// once. The result is sorted by volume ID.
func ConfigAccessVolumes(config map[string]any, homeNode string) []AccessVolume {
	seen := make(map[string]struct{})
	var out []AccessVolume
	for key, raw := range config {
		if !isQemuDiskKey(key) && key != "vmstate" {
			continue
		}
		text, ok := ConfigStringValue(raw)
		if !ok {
			continue
		}
		volid := bareDriveVolid(text)
		if volid == "" || volid == "none" || volid == "cdrom" {
			continue
		}
		if _, dup := seen[volid]; dup {
			continue
		}
		seen[volid] = struct{}{}
		out = append(out, AccessVolume{Volume: volid, HomeNode: homeNode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out
}

// VMAccessVolumes reads every snapshot of the VM and returns the volumes of
// config together with those the snapshots and their VM-state files hold, so
// VolumeAccessSet sees every volume a recovery must reach. A failed snapshot
// listing or read is an error: an access set computed without a snapshot's
// volumes could name a node that cannot roll the snapshot back.
func VMAccessVolumes(
	ctx context.Context, c Client, node string, vmid int, config map[string]any,
) ([]AccessVolume, error) {
	out := ConfigAccessVolumes(config, node)
	seen := make(map[string]struct{}, len(out))
	for _, v := range out {
		seen[v.Volume] = struct{}{}
	}
	names, err := HasSnapshots(ctx, c, node, vmid)
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "VMAccessVolumes: list snapshots")
	}
	sort.Strings(names)
	for _, name := range names {
		cfg, cfgErr := SnapshotConfig(ctx, c, node, vmid, name)
		if cfgErr != nil {
			return nil, cpierrors.Wrap(WrapError(cfgErr), "VMAccessVolumes: read snapshot config")
		}
		for _, v := range ConfigAccessVolumes(cfg, node) {
			if _, dup := seen[v.Volume]; dup {
				continue
			}
			seen[v.Volume] = struct{}{}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out, nil
}
