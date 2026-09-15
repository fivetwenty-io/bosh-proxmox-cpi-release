// Backend resolves the PVE node that every disk operation must target.
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
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// BackendKind distinguishes shared from local storage backends.
type BackendKind string

// BackendKind values returned by Backend.Kind().
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

// StorageInfoProvider is an optional capability of a Backend: implementations
// built from a StorageInfoCache lookup (shared/local production backends)
// expose the classified StorageInfo they were constructed with. Kept out of
// the Backend interface so test fakes and the static fallback need not
// implement it.
type StorageInfoProvider interface {
	StorageInfo() StorageInfo
}

// BackendStorageInfo returns the StorageInfo a backend was classified with,
// when the backend carries one. The static test/fallback backend and fakes
// return (zero, false); callers must treat that as "type unknown", not as a
// classification. This gives handlers cached access to the storage type
// (e.g. create_disk's file-vs-block volume-naming decision) without a second
// live /storage lookup.
func BackendStorageInfo(b Backend) (StorageInfo, bool) {
	if p, ok := b.(StorageInfoProvider); ok {
		return p.StorageInfo(), true
	}
	return StorageInfo{}, false
}

// CorroboratedNodeSweeper is an optional capability of a Backend: a sweep that
// takes extra empty-listing corroborators for one call. The local backend
// implements it, because its cluster sweep is the one Backend method that
// proves a volume absent, and a caller that already read the cluster's configs
// holds evidence the backend itself has no way to obtain.
//
// It is kept out of the Backend interface so the fakes and the static fallback
// need not implement it; NodeForExistingCorroborated is the free function that
// uses it when a backend has it and falls back to NodeForExisting when it does
// not.
type CorroboratedNodeSweeper interface {
	NodeForExistingCorroborated(ctx context.Context, volume string, extra ...EmptyListingCorroborator) (string, error)
}

// NodeForExistingCorroborated locates the node holding volume, adding extra
// corroborators to any empty content listing the sweep has to weigh. A backend
// that cannot take them (the shared backend, which never proves an absence, and
// every test fake) answers the ordinary NodeForExisting question, so the extra
// evidence is an improvement where it applies rather than a new requirement.
func NodeForExistingCorroborated(
	ctx context.Context, b Backend, volume string, extra ...EmptyListingCorroborator,
) (string, error) {
	if b == nil {
		return "", cpierrors.Cloud("backend: NodeForExistingCorroborated needs a backend")
	}
	if sweeper, ok := b.(CorroboratedNodeSweeper); ok && len(extra) > 0 {
		return sweeper.NodeForExistingCorroborated(ctx, volume, extra...)
	}
	return b.NodeForExisting(ctx, volume)
}

// resolver is the production BackendResolver. It consults StorageInfoCache to
// classify the storage, then constructs either a SharedBackend or LocalBackend.
type resolver struct {
	client      Client
	cache       *StorageInfoCache
	defaultNode string
	// corroborate supplies the empty-listing corroborators the backends this
	// resolver builds hand to their absence proofs. It is a function rather
	// than a slice because the sources it names (the CPI's allocation journal,
	// PVE's storage status) are read at proof time, and because the handlers
	// package owns the journal while this package owns the proof. Nil means no
	// corroboration, which is what every caller had before the option existed.
	corroborate func() []EmptyListingCorroborator
}

// BackendResolverOption configures the production resolver at construction.
type BackendResolverOption func(*resolver)

// WithEmptyListingCorroborators gives the resolver's backends a source of
// second opinions on an empty storage content listing. The local backend's
// cluster sweep reads a listing on every node it probes, and an nfs or cifs
// export that mounts but serves the wrong tree lists nothing on all of them,
// so without corroboration that sweep reports a volume absent from every node
// in the cluster.
//
// supply is called once per sweep, before the first node is probed, and the
// sources it returns are shared by every probe in that sweep. It must therefore
// be cheap to call and must not read anything itself; a source that needs a
// file or an API call reads it lazily, when a probe actually asks. A nil
// supply, or one that returns no corroborators, leaves the pre-corroboration
// behavior in place.
func WithEmptyListingCorroborators(supply func() []EmptyListingCorroborator) BackendResolverOption {
	return func(r *resolver) { r.corroborate = supply }
}

// NewBackendResolver builds the production resolver. The cache may be nil — in
// which case every Resolve falls back to BackendLocal on defaultNode (matching
// the "treat unknown as local, require explicit node" safety default).
func NewBackendResolver(
	client Client, cache *StorageInfoCache, defaultNode string, opts ...BackendResolverOption,
) BackendResolver {
	r := &resolver{client: client, cache: cache, defaultNode: defaultNode}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
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
				// The shared backend never proves an absence: it routes to a
				// node rather than sweeping for one, so it has no empty
				// listing to weigh and takes no corroborators.
				return newSharedBackend(r.client, info, r.defaultNode), nil
			}
			return newLocalBackend(r.client, info, r.defaultNode, r.corroborate), nil
		}
		// StorageInfoCache.Get returns a non-nil error for two very different
		// conditions: the storage genuinely absent from the PVE index (a plain
		// error, not classified through cpierrors), or a transient lister
		// failure that refresh() has already wrapped through WrapError and
		// classified as retriable. Only the former is safe to silently mask as
		// "storage: local, unclassified" — masking the latter would silently
		// flip a shared storage's placement algorithm to local's vmHint-first
		// ordering on a condition the Director could clear by retrying.
		if isRetriableCPIError(err) {
			return nil, cpierrors.Wrap(err, "backend: storage classification lookup")
		}
		// Non-retriable lookup failure (storage genuinely absent from the
		// index): fall through to default local backend with a fabricated
		// StorageInfo. The local backend's NodeForCreate refuses to make
		// decisions without one of (vmHint, cloudPropNode, defaultNode),
		// which keeps the safe-default behavior described in the plan.
		// High-frequency path; resolver has no logger field so no Debug log
		// is emitted here (see NewBackendResolver).
	}

	// No cache configured or storage not found: treat as local. Tests that
	// don't wire a resolver use NewStaticBackendResolver instead; this branch
	// is for production lookups that miss.
	return newLocalBackend(r.client, StorageInfo{Name: storage}, r.defaultNode, r.corroborate), nil
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

// Kind reports BackendShared because the static fallback is treated as cluster-visible (no node pinning is enforced).
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

// asInt parses s as a positive int VMID. Returns (0, false) on any failure,
// including trailing garbage after the digits (e.g. "100abc", "100 200") —
// strconv.Atoi requires the entire string to be consumed, unlike fmt.Sscanf's
// %d verb which silently ignores anything after the matched digits.
// Used by backends that accept a vmHint string from BOSH disk_cid arguments.
func asInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
