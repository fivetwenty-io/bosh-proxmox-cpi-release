// Package pve: persistent-disk backend abstraction.
//
// A Backend resolves the PVE node that every disk operation must target.
// Two flavours: SharedBackend (cluster-visible storages — any node works,
// preference order: cloud_props.node → vmHint → default) and LocalBackend
// (single-node storages — vmHint co-locates with the owning VM; existing
// volumes are located via a cluster scan).
//
// BackendResolver inspects a storage's classification via StorageInfoCache and
// hands back the right Backend. Handlers depend only on the Resolver; the
// concrete Backend type is an implementation detail.
package pve

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// BackendKind distinguishes shared from local storage backends.
type BackendKind string

const (
	BackendShared BackendKind = "shared"
	BackendLocal  BackendKind = "local"
)

// Backend resolves PVE nodes for disk operations against one PVE storage.
type Backend interface {
	// Kind reports whether this backend is shared (cluster-visible) or local
	// (node-pinned). Handlers branch on Kind() for backend-specific rules
	// (e.g., attach_disk verifies VM/disk co-location only for local).
	Kind() BackendKind

	// NodeForCreate picks the node where a NEW volume on this storage should
	// be created.
	//
	//   vmHint        — optional vm_cid passed to create_disk; empty when the
	//                   disk is created before its owner VM exists.
	//   cloudPropNode — optional cloud_properties.node override.
	//
	// Returns the target node, or an error if no node can be resolved.
	NodeForCreate(ctx context.Context, vmHint, cloudPropNode string) (string, error)

	// NodeForExisting locates the node currently holding an EXISTING volume on
	// this storage. For shared backends this is "any node" (defaults to the
	// configured default); for local backends this scans the cluster to find
	// the owner.
	NodeForExisting(ctx context.Context, volume string) (string, error)
}

// BackendResolver maps a storage name to its Backend implementation. Handlers
// hold a BackendResolver via handlers.Deps so the resolver can be substituted
// in tests (typically with NewStaticBackendResolver).
type BackendResolver interface {
	Resolve(ctx context.Context, storage string) (Backend, error)
}

// resolver is the production BackendResolver. It consults StorageInfoCache to
// classify the storage, then constructs either a SharedBackend or LocalBackend.
type resolver struct {
	client      Client
	cache       *StorageInfoCache
	defaultNode string
}

// NewBackendResolver builds the production resolver. The cache may be nil — in
// which case every Resolve falls back to BackendShared on defaultNode (matching
// the "treat unknown as shared on the configured node" safety default).
func NewBackendResolver(client Client, cache *StorageInfoCache, defaultNode string) BackendResolver {
	return &resolver{client: client, cache: cache, defaultNode: defaultNode}
}

// Resolve classifies storage and returns the appropriate Backend.
//
// Classification rule (mirrored in StorageInfo.IsShared):
//   - rbd / cephfs / nfs / cifs / glusterfs / pbs → shared
//   - any storage flagged shared=1 in PVE → shared
//   - everything else → local
//   - lookup failure → local on defaultNode (safe default: forces explicit
//     node selection via vmHint or cloud_properties.node).
func (r *resolver) Resolve(ctx context.Context, storage string) (Backend, error) {
	if storage == "" {
		return nil, cpierrors.Cloud("backend: storage name must not be empty")
	}

	if r.cache != nil {
		info, err := r.cache.Get(ctx, storage)
		if err == nil {
			if info.IsShared() {
				return newSharedBackend(r.client, info, r.defaultNode), nil
			}
			return newLocalBackend(r.client, info, r.defaultNode), nil
		}
		// Lookup failure: fall through to default local backend with a
		// fabricated StorageInfo. The local backend's NodeForCreate refuses
		// to make decisions without one of (vmHint, cloudPropNode, defaultNode),
		// which keeps the safe-default behavior described in the plan.
		_ = err
	}

	// No cache configured or storage not found: treat as local. Tests that
	// don't wire a resolver use NewStaticBackendResolver instead; this branch
	// is for production lookups that miss.
	return newLocalBackend(r.client, StorageInfo{Name: storage}, r.defaultNode), nil
}

// staticResolver is a deterministic resolver used by tests that don't exercise
// the classification path. It returns a staticBackend (shared, never touches
// the cluster API) bound to defaultNode.
type staticResolver struct {
	defaultNode string
}

// NewStaticBackendResolver returns a resolver that classifies every storage as
// shared and routes every operation to defaultNode. This matches the CPI's
// pre-abstraction behavior and is the safe default for any handler test that
// doesn't otherwise configure a Resolver on Deps.
//
// Unlike NewBackendResolver, this variant never calls the cluster API — it is
// safe to use with test mocks that don't wire a Cluster service.
func NewStaticBackendResolver(_ Client, defaultNode string) BackendResolver {
	return &staticResolver{defaultNode: defaultNode}
}

func (s *staticResolver) Resolve(_ context.Context, _ string) (Backend, error) {
	return &staticBackend{defaultNode: s.defaultNode}, nil
}

// staticBackend mirrors SharedBackend's intent but never consults the cluster.
// Used in tests and as the safe default when no Resolver is wired.
type staticBackend struct{ defaultNode string }

func (s *staticBackend) Kind() BackendKind { return BackendShared }

func (s *staticBackend) NodeForCreate(_ context.Context, _ string, cloudPropNode string) (string, error) {
	if cloudPropNode != "" {
		return cloudPropNode, nil
	}
	if s.defaultNode != "" {
		return s.defaultNode, nil
	}
	return "", cpierrors.Cloud("backend(static): cannot resolve node for create_disk — set config.node or cloud_properties.node")
}

func (s *staticBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	if s.defaultNode != "" {
		return s.defaultNode, nil
	}
	return "", cpierrors.Cloud("backend(static): cannot resolve node — set config.node")
}

// nodeFromCluster looks up the current PVE node hosting a given VMID via the
// /cluster/resources endpoint. Returns ("", false, nil) when the VM is not
// found in cluster resources (e.g., during create when the VM doesn't yet
// exist). Returns a non-nil error only on transport failures.
//
// Exported as a package-private helper because both LocalBackend and the
// promoted FindVMByDiskVolid call it.
func nodeFromCluster(ctx context.Context, c Client, vmid int) (string, bool, error) {
	if c == nil || vmid <= 0 {
		return "", false, nil
	}
	return FindVMNodeViaCluster(ctx, c, vmid)
}

// formatNodeResolveError formats a uniform error when a backend cannot find a
// target node from any of its inputs. The message names every input so the
// operator knows which knob to set.
func formatNodeResolveError(kind BackendKind, op string, vmHint, cloudPropNode, defaultNode string) error {
	missing := []string{}
	if vmHint == "" {
		missing = append(missing, "vm_cid (co-location hint)")
	}
	if cloudPropNode == "" {
		missing = append(missing, "cloud_properties.node")
	}
	if defaultNode == "" {
		missing = append(missing, "config.node")
	}
	return cpierrors.Cloud(
		"backend(%s): cannot resolve node for %s — provide one of: %s",
		kind, op, strings.Join(missing, ", "),
	)
}

// asInt parses s as a positive int VMID. Returns (0, false) on any failure.
// Used by backends that accept a vmHint string from BOSH disk_cid arguments.
func asInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
