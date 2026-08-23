// LocalBackend governs storages that live on a single node (lvm/lvmthin/zfspool/
// dir/btrfs not flagged shared=1). Disk operations and the VM that attaches the
// disk must target the same node.
//
// Node selection:
//   - new disks co-locate with their owner VM (vmHint), then fall back to
//     cloud_properties.node, then config.node.
//   - existing volumes are located via a cluster-wide scan: every node is
//     probed via Storage().Exists(); the first hit wins.
package pve

import (
	"context"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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
}

func newLocalBackend(c Client, info StorageInfo, defaultNode string) Backend {
	return &localBackend{client: c, info: info, defaultNode: defaultNode}
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

	var lastProbeErr error

	for _, node := range candidates {
		// ExistsTolerant folds the lvmthin/zfspool "Failed to find logical
		// volume" / "dataset does not exist" 500 errors into (false, nil)
		// so the cluster scan reports a clean miss instead of a retriable
		// error when the volume is genuinely gone (e.g. just-deleted disks
		// being re-probed by has_disk / delete_disk idempotency paths).
		exists, err := ExistsTolerant(ctx, l.client, node, storage, volume)
		if err != nil {
			// Probe failure on one node should not abort the cluster scan —
			// the volume may live on a different healthy node. Record the
			// error and continue so a healthy node can still return a hit.
			lastProbeErr = err
			continue
		}
		if exists {
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
	// would wrongly conclude absent. ListClusterConfigNodes wraps the listing
	// in RetryOnTransient and classifies its errors, so retriable transport
	// faults propagate up as RetriableCloud once retries are exhausted.
	nodes, listErr := ListClusterConfigNodes(ctx, l.client)
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
