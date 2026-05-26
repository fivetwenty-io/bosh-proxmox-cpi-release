// Package pve: LocalBackend implementation.
//
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
	"encoding/json"
	"fmt"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

type localBackend struct {
	client      Client
	info        StorageInfo
	defaultNode string
}

func newLocalBackend(c Client, info StorageInfo, defaultNode string) Backend {
	return &localBackend{client: c, info: info, defaultNode: defaultNode}
}

func (l *localBackend) Kind() BackendKind { return BackendLocal }

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
	anyProbeSucceeded := false

	for _, node := range candidates {
		exists, err := l.client.Storage().Exists(ctx, node, storage, volume)
		if err != nil {
			// Probe failure on one node should not abort the cluster scan —
			// the volume may live on a different healthy node. Record the
			// error and continue so a healthy node can still return a hit.
			lastProbeErr = err
			continue
		}
		anyProbeSucceeded = true
		if exists {
			return node, nil
		}
	}

	// If every candidate errored (none returned a clean false), we cannot
	// distinguish "disk genuinely absent" from "all nodes unreachable". Treat
	// the latter as retriable so the director re-drives the action once PVE
	// recovers, rather than permanently losing track of the disk.
	if !anyProbeSucceeded && lastProbeErr != nil {
		return "", cpierrors.WrapAs(lastProbeErr, cpierrors.TypeRetriableCloud,
			"NodeForExisting: all-node probe failed")
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

	typ := "node"
	resp, err := l.client.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
	if err != nil {
		return nil, cpierrors.Wrap(err, "backend(local): list cluster nodes")
	}
	if resp == nil {
		return out, nil
	}
	for _, raw := range *resp {
		var entry struct {
			Node string `json:"node"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		// /cluster/resources?type=node populates "node" with the node name;
		// fall back to "name" if "node" is absent in older PVE responses.
		if entry.Node != "" {
			add(entry.Node)
		} else if entry.Name != "" {
			add(entry.Name)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("backend(local): cluster scan returned zero candidate nodes")
	}
	return out, nil
}
