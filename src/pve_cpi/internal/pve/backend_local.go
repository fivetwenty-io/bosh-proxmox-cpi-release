// LocalBackend governs storages that live on a single node (lvm/lvmthin/zfspool/
// dir/btrfs not flagged shared=1). Disk operations and the VM that attaches the
// disk must target the same node.
//
// Node selection:
//   - new disks co-locate with their owner VM (vmHint), then fall back to
//     cloud_properties.node, then config.node.
//   - existing volumes are located via a cluster-wide scan. Every node is
//     probed through ProveVolumeAbsent, and the first node that does not prove
//     the volume absent wins.
package pve

import (
	"context"
	"strings"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// resourceTypeNode is the PVE cluster-resource type tag for compute nodes,
// used as the `type=` filter for ListResources calls.
const resourceTypeNode = "node"

// statusRowTypeCluster is the type discriminator of the /cluster/status row
// that carries cluster-wide fields such as quorate.
const statusRowTypeCluster = "cluster"

type localBackend struct {
	client      Client
	info        StorageInfo
	defaultNode string

	// corroborate supplies the second opinions the sweep hands to an empty
	// content listing. The resolver sets it (see WithEmptyListingCorroborators);
	// nil is a backend built without one, which sweeps exactly as it did before
	// corroboration existed.
	corroborate func() []EmptyListingCorroborator

	// liveOnce guards the single live classification this backend is willing
	// to pay for. The read happens on the first classifier call, which the
	// absence proof only makes after a point probe has already failed, so a
	// sweep that never reaches a content listing never issues it, and a sweep
	// that reaches several issues it once.
	liveOnce sync.Once
	liveInfo StorageInfo
	liveOK   bool
}

func newLocalBackend(
	c Client, info StorageInfo, defaultNode string, corroborate func() []EmptyListingCorroborator,
) Backend {
	return &localBackend{client: c, info: info, defaultNode: defaultNode, corroborate: corroborate}
}

// Kind reports BackendLocal — node-pinned storage where the volume's host node determines VM placement.
func (l *localBackend) Kind() BackendKind { return BackendLocal }

// StorageInfo exposes the classification this backend was built from
// (StorageInfoProvider capability — see BackendStorageInfo). A cache-miss
// fallback backend carries a fabricated StorageInfo with an empty Type;
// callers treat that as "type unknown".
func (l *localBackend) StorageInfo() StorageInfo { return l.info }

// NodeForCreate picks the node for a new local-storage volume. Order:
//
//  1. vmHint → owning VM's current node (co-location is mandatory for local).
//  2. cloud_properties.node — operator override.
//  3. defaultNode (config.node).
//
// vmHint comes first because a local-storage disk MUST live on the same node
// as the VM that will attach it; any other choice produces an unattachable
// disk. The fallback chain still lets disk creation succeed when the VM does
// not yet exist (BOSH may create the disk before the VM).
func (l *localBackend) NodeForCreate(ctx context.Context, vmHint, cloudPropNode string) (string, error) {
	if vmid, ok := asInt(vmHint); ok {
		if node, found, err := nodeFromCluster(ctx, l.client, vmid); err != nil {
			return "", cpierrors.Wrap(err, "backend(local): lookup vmHint node")
		} else if found && node != "" {
			return node, nil
		}
	}
	if cloudPropNode != "" {
		return cloudPropNode, nil
	}
	if l.defaultNode != "" {
		return l.defaultNode, nil
	}
	return "", formatNodeResolveError(BackendLocal, "create_disk", vmHint, cloudPropNode, l.defaultNode)
}

// NodeForExisting scans the cluster to find the node currently hosting volume
// on l.info.Name. The scan candidates are the cluster's node set, optionally
// constrained by the storage's "nodes" restriction.
//
// Returns cpierrors.DiskNotFound when no node holds the volume.
func (l *localBackend) NodeForExisting(ctx context.Context, volume string) (string, error) {
	return l.nodeForExisting(ctx, volume)
}

// NodeForExistingCorroborated is NodeForExisting with extra corroborators for
// this one sweep (the CorroboratedNodeSweeper capability). The caller's extras
// go first, ahead of whatever the resolver supplied, because the evidence a
// caller carries is evidence it already paid for: the cluster's VM configs,
// read by a holder scan that ran before the sweep. The resolver's own sources
// follow in their configured order, which ends with the one that spends an API
// call.
func (l *localBackend) NodeForExistingCorroborated(
	ctx context.Context, volume string, extra ...EmptyListingCorroborator,
) (string, error) {
	return l.nodeForExisting(ctx, volume, extra...)
}

func (l *localBackend) nodeForExisting(
	ctx context.Context, volume string, extra ...EmptyListingCorroborator,
) (string, error) {
	if volume == "" {
		return "", cpierrors.Cloud("backend(local): volume must not be empty")
	}
	storage := l.info.Name
	if storage == "" {
		return "", cpierrors.Cloud("backend(local): storage name is empty in StorageInfo")
	}

	candidates, err := l.candidateNodes(ctx)
	if err != nil {
		return "", err
	}

	// The second opinions are built once for the whole sweep. Every candidate
	// node asks the same sources the same question, and a source such as the
	// allocation journal reads a file on local disk to answer it, so building
	// them per node would read that file once per node in the cluster.
	corroborators := l.emptyListingCorroborators(extra)

	var lastProbeErr error

	for _, node := range candidates {
		// ProveVolumeAbsent folds the lvmthin/zfspool "Failed to find logical
		// volume" / "dataset does not exist" 500 errors into a clean absence
		// so the cluster scan reports a miss instead of a retriable error when
		// the volume is genuinely gone (e.g. just-deleted disks being
		// re-probed by has_disk / delete_disk idempotency paths). A miss on
		// file storage never reaches us as a 404 at all, because PVE answers
		// the volume GET with a 500 naming volume_size_info, which says
		// nothing either way. The proof settles that question from a storage
		// content listing, so the scan gets its clean miss there too.
		absent, err := ProveVolumeAbsent(
			ctx, l.client, node, storage, volume, l.classifyStorage, corroborators...)
		if err != nil {
			// Probe failure on one node should not abort the cluster scan —
			// the volume may live on a different healthy node. Record the
			// error and continue so a healthy node can still return a hit.
			lastProbeErr = err
			continue
		}
		if !absent {
			return node, nil
		}
	}

	// DiskNotFound may only be concluded from a COMPLETE sweep: every
	// candidate answered a clean false. A single erroring node with the rest
	// clean-absent is exactly the case where the volume lives on the node we
	// could not ask — the erroring node is the likeliest holder, since a
	// local volume's node going unhealthy takes its probe down with it.
	// Concluding absence there turns delete_disk into false success,
	// has_disk into a false no, and detach_disk into a phantom
	// already-detached. Any probe error with no hit is therefore retriable so
	// the director re-drives the action once PVE recovers.
	if lastProbeErr != nil {
		return "", cpierrors.WrapAs(lastProbeErr, cpierrors.TypeRetriableCloud,
			"NodeForExisting: node probe(s) failed with no hit on the reachable nodes; cannot prove the volume absent")
	}

	return "", cpierrors.DiskNotFound(FormatDiskCID(storage, volume))
}

// emptyListingCorroborators orders the second opinions one sweep hands to an
// empty content listing: the caller's own first, then the resolver's. The
// resolver's supplier is called once per sweep rather than once per backend, so
// a source such as the allocation journal is read against the cluster as it
// stands when the sweep starts rather than as a concurrent CPI process leaves
// it partway through.
func (l *localBackend) emptyListingCorroborators(extra []EmptyListingCorroborator) []EmptyListingCorroborator {
	var supplied []EmptyListingCorroborator
	if l.corroborate != nil {
		supplied = l.corroborate()
	}
	if len(extra) == 0 {
		return supplied
	}
	out := make([]EmptyListingCorroborator, 0, len(extra)+len(supplied))
	out = append(out, extra...)
	return append(out, supplied...)
}

// classifyStorage answers the absence proof's classifier question for this
// backend's storage, preferring a live read of the PVE storage index over the
// StorageInfo this backend was constructed from.
//
// The captured info is right often enough, but two shapes make it the wrong
// thing to answer with. A cache miss that is not retriable leaves the resolver
// fabricating a StorageInfo carrying only the name, so its Type is empty and
// its is_mountpoint is false on a storage that may well carry the flag. And one
// CPI process can outlive an operator's storage.cfg edit, because the
// multi-request stdin loop keeps the process alive across calls while the
// classification cache holds its entries for a TTL.
//
// The live read is made at most once per backend and only from here, which the
// proof reaches only after a point probe failed, so the cost lands on the paths
// that are about to read a content listing anyway.
//
// When the live read cannot be made, the answer falls back to the captured
// info, and how much that proves depends on what it carries. A captured info
// with a Type is a real classification, so it stays a successful one, which is
// what keeps this working on a client that wires no cluster storage service. A
// captured info with no Type is the fabricated one, and reporting that as a
// successful classification told the proof it was looking at an unclassified
// storage when the truth is that we never classified it at all. It now answers
// false, which the proof reads as unproven.
func (l *localBackend) classifyStorage(ctx context.Context) (StorageInfo, bool) {
	l.liveOnce.Do(func() {
		info, err := LiveStorageInfo(ctx, l.client, l.info.Name)
		if err != nil {
			// Not fatal on its own: the fallback below still answers, and the
			// caller's own failure message is the one that matters if it does
			// not. Logged at Debug so an operator chasing an unproven absence
			// can see that the live classification is what went missing.
			log.FromContext(ctx).Debug("backend(local): live storage classification unavailable, using captured info",
				log.String("storage", l.info.Name),
				log.Err(err),
			)
			return
		}
		l.liveInfo, l.liveOK = info, true
	})
	if l.liveOK {
		return l.liveInfo, true
	}
	if strings.TrimSpace(l.info.Type) == "" {
		return l.info, false
	}
	return l.info, true
}

// candidateNodes builds the ordered list of node names to probe. Preference:
//
//  1. defaultNode first (cheap hit on single-node deployments).
//  2. Storage's "nodes" restriction (PVE storage.cfg).
//  3. All cluster nodes via /cluster/resources?type=node.
//
// Deduplicated; defaultNode is only emitted once.
func (l *localBackend) candidateNodes(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var out []string

	add := func(n string) {
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}

	add(l.defaultNode)
	for _, n := range l.info.Nodes {
		add(n)
	}

	if len(l.info.Nodes) > 0 {
		// Storage has explicit nodes restriction; no need to enumerate all
		// cluster nodes — the volume cannot live anywhere else.
		return out, nil
	}

	if l.client == nil {
		return out, nil
	}

	// Enumerate from /cluster/config/nodes (corosync membership) rather than
	// the /cluster/resources index: the index lags cluster state, so a
	// recently joined or briefly unindexed node would be silently skipped by
	// the probe sweep — and an unswept node is a node whose volume this scan
	// would wrongly conclude absent. ListClusterMemberNames wraps the listing
	// in RetryOnTransient, classifies its errors (retriable transport faults
	// propagate up as RetriableCloud once retries are exhausted), and falls
	// back to GET /nodes on a never-clustered standalone host.
	nodes, listErr := ListClusterMemberNames(ctx, l.client)
	if listErr != nil {
		return nil, cpierrors.Wrap(listErr, "backend(local): list cluster nodes")
	}
	for _, n := range nodes {
		add(n)
	}

	if len(out) == 0 {
		// The membership listing came back empty (or every row failed to
		// parse) — an invisible-cluster condition, not a permanent
		// misconfiguration. Retriable, matching the classification convention
		// every other error return in this file follows, so the Director
		// re-drives the action once cluster visibility recovers instead of
		// treating an unparseable snapshot as a hard failure.
		return nil, cpierrors.Retriable("backend(local): cluster scan returned zero candidate nodes")
	}
	return out, nil
}
