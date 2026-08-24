// Light-stemcell storage policy validation. ValidateLightStemcellStorage
// enforces five placement rules that determine whether a given PVE storage is
// acceptable for light stemcell uploads and whether a specific cluster node
// must be pinned for that upload.
//
// Backing-identity note: both ValidateTemplateCloneStorage and
// ValidateLightStemcellStorage classify exactly ONE named storage's own
// topology (shared/local, block/file, cluster size) — neither compares two
// storage IDs against each other, so StorageInfo.BackingKey/SameBacking
// (storage_info.go) have no direct application inside this file. The
// backing-identity-aware "are these two storage IDs really the same
// storage" decisions live where an actual two-ID comparison exists: the
// clone-mode storageMismatch check and the clone Target-validation direction
// fix, both in create_vm_disk.go.
package pve

import (
	"context"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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
	StorageTypeLVM:     {},
	StorageTypeLVMThin: {},
	StorageTypeZFSPool: {},
	StorageTypeRBD:     {},
}

// IsBlockStorage reports whether the given PVE storage type cannot hold
// file content (qcow2). The check is case-insensitive. This is exported
// so handlers that perform pre-flight checks can call it directly without
// going through the full policy validation path.
func IsBlockStorage(storageType string) bool {
	_, ok := blockStorageTypes[strings.ToLower(storageType)]
	return ok
}

// IsLinkedCloneSupported reports whether the PVE storage type supports linked
// (copy-on-write) clones. Only thick LVM ("lvm") lacks snapshot-backed linked
// clone support; every other backend (dir, nfs, cifs, zfspool, lvmthin, rbd,
// cephfs, and unknown/empty types) is treated as linked-capable.
//
// The check is case-insensitive. An empty string or unrecognised type returns
// true (permissive default) so that new or custom storage backends do not
// silently fall back to slower full clones.
//
// Callers should prefer StorageTypeLVM and related constants rather than raw
// string literals to avoid silent mismatches on future PVE renames.
func IsLinkedCloneSupported(storageType string) bool {
	return !strings.EqualFold(storageType, StorageTypeLVM)
}

// ValidateTemplateCloneStorage enforces the clone placement policy:
// in a multi-node cluster, a template on LOCAL (non-shared) storage can only
// be cloned on the same node, so the operator must pin the node via
// cloud_properties.node or use shared storage. Returns the node the clone
// must run on, or a non-nil *cpierrors.Error when the configuration is
// incompatible.
//
// Unlike ValidateLightStemcellStorage, block storage backends (rbd, lvmthin,
// zfspool, lvm) are NOT rejected here — PVE linked clones work from block
// backends for QEMU disk cloning. The constraint is topology only.
//
// Rules evaluated in order:
//
//  1. Single-node cluster (clusterSize <= 1) — ACCEPT any backend.
//     With only one node, local vs. shared is irrelevant. Returns
//     cloudPropsNode as chosenNode (empty string if none provided).
//
//  2. Multi-node cluster + shared storage (IsShared or rbd) — ACCEPT.
//     Any node can host the clone. Returns cloudPropsNode unchanged.
//
//  3. Multi-node cluster + local storage + cloudPropsNode provided — ACCEPT.
//     Clone must run on the pinned node. Returns cloudPropsNode.
//
//  4. Multi-node cluster + local storage + no cloudPropsNode — REJECT.
//     CPI cannot guarantee clone and template land on the same node without
//     migration. Returns a human-readable actionable error.
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
//   - deps.ClusterNodeCount error → wrapped *cpierrors.Error.
//   - multi-node local + no pin → *cpierrors.Error with actionable message.
func ValidateTemplateCloneStorage(
	ctx context.Context,
	deps PolicyDeps,
	storage string,
	cloudPropsNode string,
) (chosenNode string, err error) {
	if storage == "" {
		return "", cpierrors.Cloud("validate_template_clone_storage: storage name required")
	}

	info, err := deps.StorageInfo(ctx, storage)
	if err != nil {
		return "", cpierrors.Wrap(err,
			"validate_template_clone_storage: lookup storage "+storage)
	}

	clusterSize, err := deps.ClusterNodeCount(ctx)
	if err != nil {
		return "", cpierrors.Wrap(err, "validate_template_clone_storage: lookup cluster node count")
	}

	// Rule 1: single-node cluster accepts any backend.
	if clusterSize <= 1 {
		return cloudPropsNode, nil
	}

	// Rule 2: multi-node + shared storage — any node works.
	if info.IsShared() {
		return cloudPropsNode, nil
	}

	// Rules 3/4: multi-node + local storage — node pin required.
	if cloudPropsNode == "" {
		return "", cpierrors.Cloud(
			"validate_template_clone_storage: storage %q is local on a multi-node cluster (%d nodes);"+
				" template on local storage can only be cloned on the node that hosts it;"+
				" set cloud_properties.node to pin the VM to that node,"+
				" or use shared storage; CPI does not auto-migrate clones across nodes",
			storage, clusterSize)
	}

	return cloudPropsNode, nil
}

// LightStemcellPolicyOption adjusts ValidateLightStemcellStorage's rule
// evaluation. Options are handler-supplied context the policy cannot derive
// from the storage alone.
type LightStemcellPolicyOption func(*lightStemcellPolicyOptions)

type lightStemcellPolicyOptions struct {
	unpinnedLocalAccepted bool
}

// WithUnpinnedLocalAccepted relaxes rule 5: multi-node local storage without
// a cloud_properties.node pin is accepted instead of rejected. Callers pass
// it only after establishing that VM placement does not depend on the upload
// node — the single-shared-template topology (strategy=template, shared
// vm_storage).
func WithUnpinnedLocalAccepted() LightStemcellPolicyOption {
	return func(o *lightStemcellPolicyOptions) {
		o.unpinnedLocalAccepted = true
	}
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
//  5. Multi-node cluster + local storage + no cloudPropsNode — REJECT,
//     unless WithUnpinnedLocalAccepted was passed. Without a node pin, the
//     CPI cannot guarantee upload and VM placement land on the same node —
//     unless the caller has established that placement no longer depends on
//     the upload node (single-shared-template topology: strategy=template
//     with a shared vm_storage pool, where the one cache template serves
//     every node via cross-node clone). With the option, ACCEPT and return
//     "" so the caller falls back to its configured node. Returns a
//     human-readable operator message otherwise.
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
	opts ...LightStemcellPolicyOption,
) (chosenNode string, err error) {
	var policyOpts lightStemcellPolicyOptions
	for _, opt := range opts {
		opt(&policyOpts)
	}
	if storage == "" {
		return "", cpierrors.Cloud("validate_light_stemcell_storage: storage name required")
	}

	info, err := deps.StorageInfo(ctx, storage)
	if err != nil {
		return "", cpierrors.Wrap(err,
			"validate_light_stemcell_storage: lookup storage "+storage)
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
		return "", cpierrors.Wrap(err, "validate_light_stemcell_storage: lookup cluster node count")
	}

	// Rule 2: single-node cluster accepts any file-capable backend.
	if clusterSize <= 1 {
		return cloudPropsNode, nil
	}

	// Rule 3: multi-node + shared file storage — any node works.
	if info.IsShared() {
		return cloudPropsNode, nil
	}

	// Rules 4/5: multi-node + local storage — node pin required, unless the
	// caller established that placement no longer depends on the upload node.
	if cloudPropsNode == "" {
		if policyOpts.unpinnedLocalAccepted {
			return "", nil
		}
		return "", cpierrors.Cloud(
			"validate_light_stemcell_storage: storage %q is local on a multi-node cluster (%d nodes);"+
				" set cloud_properties.node to pin the stemcell upload to a specific node",
			storage, clusterSize)
	}

	return cloudPropsNode, nil
}
