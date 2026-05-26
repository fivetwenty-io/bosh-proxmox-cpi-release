// Package pve: light-stemcell storage policy validation.
//
// ValidateLightStemcellStorage enforces five placement rules that determine
// whether a given PVE storage is acceptable for light stemcell uploads and
// whether a specific cluster node must be pinned for that upload.
package pve

import (
	"context"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// PolicyDeps is the narrow dependency interface ValidateLightStemcellStorage
// requires from its caller. Production code wires this from handlers.Deps;
// tests provide a stub. Only two methods keep the interface minimal and
// mockable without pulling the full Deps surface.
type PolicyDeps interface {
	// StorageInfo returns classification data for the named PVE storage.
	StorageInfo(ctx context.Context, storage string) (StorageInfo, error)

	// ClusterNodeCount returns the number of nodes currently registered in
	// the PVE cluster. Returns 1 for standalone (non-clustered) nodes.
	ClusterNodeCount(ctx context.Context) (int, error)
}

// blockStorageTypes lists PVE storage backends that store raw block volumes
// only and cannot accept qcow2 file uploads. Light stemcells (pre-uploaded or
// CPI-assisted-fetch) require file-content storage access, so these types are
// rejected regardless of shared/local classification.
//
// Note: rbd is PVE-classified as shared, but it stores raw RBD images, not
// qcow2 files. Rule 1 (block-type rejection) is evaluated before rule 3
// (shared acceptance) so rbd is always rejected.
var blockStorageTypes = map[string]struct{}{
	"lvm":     {},
	"lvmthin": {},
	"zfspool": {},
	"rbd":     {},
}

// IsBlockStorage reports whether the given PVE storage type cannot hold
// file content (qcow2). The check is case-insensitive. This is exported
// so handlers that perform pre-flight checks can call it directly without
// going through the full policy validation path.
func IsBlockStorage(storageType string) bool {
	_, ok := blockStorageTypes[strings.ToLower(storageType)]
	return ok
}

// ValidateLightStemcellStorage applies the light-stemcell storage policy.
// It returns the node the caller should target for the stemcell operation,
// or a non-nil *cpierrors.Error when the storage configuration is incompatible.
//
// Rules evaluated in order:
//
//  1. Block storage (lvm/lvmthin/zfspool/rbd) — REJECT unconditionally.
//     These backends have no file-content support; qcow2 uploads cannot land here.
//
//  2. Single-node cluster (clusterSize <= 1) — ACCEPT any file-capable backend.
//     With only one node, local vs. shared distinction is irrelevant. Returns
//     cloudPropsNode as chosenNode (empty string if caller provided none; the
//     caller falls back to config.Node or the storage's restricted-node list).
//
//  3. Multi-node cluster + shared file-based storage — ACCEPT.
//     Any node can serve the upload. Returns cloudPropsNode unchanged (empty
//     is fine; caller resolves via backend.NodeForExisting or config.Node).
//
//  4. Multi-node cluster + local storage + cloudPropsNode provided — ACCEPT.
//     The upload must land on the pinned node. Returns cloudPropsNode.
//
//  5. Multi-node cluster + local storage + no cloudPropsNode — REJECT.
//     Without a node pin, the CPI cannot guarantee upload and VM placement
//     land on the same node. Returns a human-readable operator message.
//
// Inputs:
//   - ctx: standard context; passed to deps methods unchanged.
//   - deps: storage info and cluster-size provider; must not be nil.
//   - storage: PVE storage name; empty string returns immediate error.
//   - cloudPropsNode: value of cloud_properties.node from the CPI request;
//     empty string means "no pin provided".
//
// Failure modes:
//   - storage == "" → *cpierrors.Error before any deps call.
//   - deps.StorageInfo error → wrapped *cpierrors.Error.
//   - IsBlockStorage true → *cpierrors.Error naming the type.
//   - deps.ClusterNodeCount error → wrapped *cpierrors.Error.
//   - multi-node local + no pin → *cpierrors.Error with actionable message.
func ValidateLightStemcellStorage(
	ctx context.Context,
	deps PolicyDeps,
	storage string,
	cloudPropsNode string,
) (chosenNode string, err error) {
	if storage == "" {
		return "", cpierrors.Cloud("validate_light_stemcell_storage: storage name required")
	}

	info, err := deps.StorageInfo(ctx, storage)
	if err != nil {
		return "", cpierrors.Cloud(
			"validate_light_stemcell_storage: lookup storage %q: %s", storage, err.Error())
	}

	// Rule 1: block storage rejected regardless of cluster topology.
	if IsBlockStorage(info.Type) {
		return "", cpierrors.Cloud(
			"validate_light_stemcell_storage: storage %q (type=%q) is block-only;"+
				" light stemcells require a file-content storage (dir/nfs/cifs/cephfs/glusterfs/btrfs)",
			storage, info.Type)
	}

	clusterSize, err := deps.ClusterNodeCount(ctx)
	if err != nil {
		return "", cpierrors.Cloud(
			"validate_light_stemcell_storage: lookup cluster node count: %s", err.Error())
	}

	// Rule 2: single-node cluster accepts any file-capable backend.
	if clusterSize <= 1 {
		return cloudPropsNode, nil
	}

	// Rule 3: multi-node + shared file storage — any node works.
	if info.IsShared() {
		return cloudPropsNode, nil
	}

	// Rules 4/5: multi-node + local storage — node pin required.
	if cloudPropsNode == "" {
		return "", cpierrors.Cloud(
			"validate_light_stemcell_storage: storage %q is local on a multi-node cluster (%d nodes);"+
				" set cloud_properties.node to pin the stemcell upload to a specific node",
			storage, clusterSize)
	}

	return cloudPropsNode, nil
}
