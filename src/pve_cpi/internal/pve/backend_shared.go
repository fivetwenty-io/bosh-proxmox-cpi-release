// SharedBackend governs storages that are cluster-visible (rbd/cephfs/nfs/cifs/
// glusterfs/pbs, or any storage marked shared=1 in PVE). All cluster nodes can
// access the volume, so node selection is a pure routing decision driven by
// caller preference rather than physical location.
package pve

import (
	"context"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

type sharedBackend struct {
	client      Client
	info        StorageInfo
	defaultNode string
}

func newSharedBackend(c Client, info StorageInfo, defaultNode string) Backend {
	return &sharedBackend{client: c, info: info, defaultNode: defaultNode}
}

// StorageInfo exposes the classification this backend was built from
// (StorageInfoProvider capability — see BackendStorageInfo).
func (s *sharedBackend) StorageInfo() StorageInfo { return s.info }

// Kind reports BackendShared — a cluster-visible storage pool where any node may host the VM.
func (s *sharedBackend) Kind() BackendKind { return BackendShared }

// NodeForCreate picks the node for a new volume on a shared storage. Order:
//
//  1. cloud_properties.node — operator's explicit choice, used as given.
//  2. vmHint → the VM's current node, when the VM exists and answers.
//  3. defaultNode (config.node), when it answers.
//  4. A node from the storage's own node list, then any other node that
//     answers when the storage has no list.
//
// A node counts as answering only when NodeHealth calls it a proxy target, so
// a dead config.node never receives the call. Any cluster node is acceptable
// for shared storage; we still apply the preference order so admin-driven
// placement (cloud_properties.node) wins over autoplacement, and so disks
// pinned to a particular VM share its node (helps locality on Ceph where
// reads/writes prefer the closest OSD). A backend built without a cluster
// client skips the liveness check and keeps steps 1 to 3 as written.
func (s *sharedBackend) NodeForCreate(ctx context.Context, vmHint, cloudPropNode string) (string, error) {
	if cloudPropNode != "" {
		return cloudPropNode, nil
	}
	var prefer []string
	if vmid, ok := asInt(vmHint); ok {
		if node, found, err := nodeFromCluster(ctx, s.client, vmid); err != nil {
			return "", cpierrors.Wrap(err, "backend(shared): lookup vmHint node")
		} else if found && node != "" {
			if !canAskHealth(s.client) {
				return node, nil
			}
			prefer = append(prefer, node)
		}
	}
	if !canAskHealth(s.client) {
		if s.defaultNode != "" {
			return s.defaultNode, nil
		}
		return "", formatNodeResolveError(BackendShared, "create_disk", vmHint, cloudPropNode, s.defaultNode)
	}
	if s.defaultNode != "" {
		prefer = append(prefer, s.defaultNode)
	}
	return storageReadNode(ctx, s.client, "create_disk on shared storage "+s.info.Name, s.info.Nodes, prefer)
}

// NodeForExisting locates the node for an existing volume on shared storage.
// Storage is cluster-visible, so any node that reaches it works. The order is
// defaultNode, then the storage's own node list, then any other node, and a
// node qualifies only when NodeHealth calls it a proxy target. The last step
// is for storage reads alone: it never answers whether a VM exists or who
// holds a disk. An empty answer is a retriable error, not a dead node. A
// backend built without a cluster client skips the check and returns
// defaultNode or the first listed node as before. volume is intentionally
// unused: unlike localBackend, no cluster scan is needed since a shared volume
// is reachable from every node.
func (s *sharedBackend) NodeForExisting(ctx context.Context, _ string) (string, error) {
	if !canAskHealth(s.client) {
		if s.defaultNode != "" {
			return s.defaultNode, nil
		}
		if len(s.info.Nodes) > 0 {
			return s.info.Nodes[0], nil
		}
		return "", cpierrors.Cloud("backend(shared): no node available — set config.node or restrict storage to specific nodes")
	}
	var prefer []string
	if s.defaultNode != "" {
		prefer = append(prefer, s.defaultNode)
	}
	return storageReadNode(ctx, s.client, "locate a volume on shared storage "+s.info.Name, s.info.Nodes, prefer)
}
