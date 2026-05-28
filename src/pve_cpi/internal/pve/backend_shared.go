// SharedBackend governs storages that are cluster-visible (rbd/cephfs/nfs/cifs/
// glusterfs/pbs, or any storage marked shared=1 in PVE). All cluster nodes can
// access the volume, so node selection is a pure routing decision driven by
// caller preference rather than physical location.
package pve

import (
	"context"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

type sharedBackend struct {
	client      Client
	info        StorageInfo
	defaultNode string
}

func newSharedBackend(c Client, info StorageInfo, defaultNode string) Backend {
	return &sharedBackend{client: c, info: info, defaultNode: defaultNode}
}

// Kind reports BackendShared — a cluster-visible storage pool where any node may host the VM.
func (s *sharedBackend) Kind() BackendKind { return BackendShared }

// NodeForCreate picks the node for a new volume on a shared storage. Order:
//
//  1. cloud_properties.node — operator's explicit choice.
//  2. vmHint → the VM's current node, when the VM exists.
//  3. defaultNode (config.node).
//
// Any cluster node is acceptable; we still apply the preference order so
// admin-driven placement (cloud_properties.node) wins over autoplacement, and
// so disks pinned to a particular VM share its node (helps locality on Ceph
// where reads/writes prefer the closest OSD).
func (s *sharedBackend) NodeForCreate(ctx context.Context, vmHint, cloudPropNode string) (string, error) {
	if cloudPropNode != "" {
		return cloudPropNode, nil
	}
	if vmid, ok := asInt(vmHint); ok {
		if node, found, err := nodeFromCluster(ctx, s.client, vmid); err == nil && found && node != "" {
			return node, nil
		}
	}
	if s.defaultNode != "" {
		return s.defaultNode, nil
	}
	return "", formatNodeResolveError(BackendShared, "create_disk", vmHint, cloudPropNode, s.defaultNode)
}

// NodeForExisting locates the node for an existing volume on shared storage.
// Storage is cluster-visible, so any node works — we prefer defaultNode and
// fall back to nodes restriction in StorageInfo.
func (s *sharedBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	if s.defaultNode != "" {
		return s.defaultNode, nil
	}
	if len(s.info.Nodes) > 0 {
		return s.info.Nodes[0], nil
	}
	return "", cpierrors.Cloud("backend(shared): no node available — set config.node or restrict storage to specific nodes")
}
