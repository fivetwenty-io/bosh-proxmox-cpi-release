package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// VMLocation is FindVMAuthoritative's answer: where the guest lives, its raw
// (semicolon-delimited) tag string, and whether it exists at all. Found=false
// is only ever returned when absence was PROVEN — every cluster node answered
// the config probe with a clean not-found — never inferred from an index miss.
type VMLocation struct {
	Node  string
	Tags  string
	Found bool
}

// FindVMAuthoritative locates a guest without trusting the /cluster/resources
// index for absence. The index lags node-local state by minutes on loaded
// clusters, so an index miss cannot distinguish "deleted" from "created
// moments ago"; concluding absence from it has caused delete_vm to report
// false success (leaking a live VM and its IP) and has_vm to answer false for
// a VM the Director just created.
//
// Fast path: the cluster scan. A hit there is authoritative (a row exists
// only for a real guest) and costs one listing. On a miss, the fallback
// proves the answer: enumerate nodes via ListConfigNodes (corosync-backed
// membership, no lag) and probe GET /nodes/<n>/qemu/<vmid>/config on each.
// A clean not-found (404, or pmxcfs's config-missing 500) counts as proven
// absence for that node; a config answer is a hit and returns that node with
// the config's own tag string. The probes only run on the miss path, so the
// steady-state cost of this function equals the old cluster-scan-only lookup.
//
// Any failure that leaves absence unproven — node enumeration failing, an
// empty node list, or any single node's probe erroring with no hit elsewhere —
// returns a retriable error, never Found=false: the caller's not-found branch
// is typically destructive (report deleted, skip a detach) and must not run
// on partial evidence.
//
// A nil client/cluster service or non-positive vmid reports not-found with no
// error, matching FindVMViaCluster's contract for unit-test mocks.
func FindVMAuthoritative(ctx context.Context, c Client, vmid int) (VMLocation, error) {
	if c == nil || c.Cluster() == nil || vmid <= 0 {
		return VMLocation{}, nil
	}
	node, tags, found, err := FindVMViaCluster(ctx, c, vmid)
	if err != nil {
		return VMLocation{}, err
	}
	if found && node != "" {
		return VMLocation{Node: node, Tags: tags, Found: true}, nil
	}

	nodes, enumErr := ListClusterConfigNodes(ctx, c)
	if enumErr != nil {
		return VMLocation{}, cpierrors.WrapAs(enumErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("FindVMAuthoritative: VM %d absent from the cluster index, and node enumeration failed; cannot prove absence", vmid))
	}
	if len(nodes) == 0 {
		return VMLocation{}, cpierrors.Retriable(
			"FindVMAuthoritative: VM %d absent from the cluster index, and node enumeration returned zero nodes; cannot prove absence", vmid)
	}

	var failedNodes []string
	var lastProbeErr error
	for _, n := range nodes {
		var cfg map[string]any
		probeErr := RetryOnTransient(ctx, nil, "find_vm_authoritative_probe", 0, func() error {
			var inner error
			cfg, inner = c.QEMU().Config(ctx, n, vmid)
			return inner
		})
		if probeErr != nil {
			if IsNotFound(probeErr) || IsPmxcfsConfigMissing(probeErr) {
				continue // proven absent on this node
			}
			failedNodes = append(failedNodes, n)
			lastProbeErr = probeErr
			continue
		}
		cfgTags, _ := cfg["tags"].(string)
		return VMLocation{Node: n, Tags: cfgTags, Found: true}, nil
	}
	if len(failedNodes) > 0 {
		return VMLocation{}, cpierrors.WrapAs(WrapError(lastProbeErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("FindVMAuthoritative: VM %d absent from the cluster index, and the config probe failed on node(s) %s; cannot prove absence",
				vmid, strings.Join(failedNodes, ",")))
	}
	return VMLocation{}, nil
}

// ListClusterConfigNodes returns every cluster member's node name from
// GET /cluster/config/nodes — corosync membership, which unlike the
// /cluster/resources index does not lag and includes nodes the index has not
// caught up with yet (recently joined, or briefly unindexed). The listing is
// wrapped in RetryOnTransient; a persistent failure returns the classified
// error for the caller to surface.
func ListClusterConfigNodes(ctx context.Context, c Client) ([]string, error) {
	if c == nil || c.Cluster() == nil {
		return nil, cpierrors.Cloud("ListClusterConfigNodes: cluster service unavailable")
	}
	var raws []json.RawMessage
	if err := RetryOnTransient(ctx, nil, "list_cluster_config_nodes", 0, func() error {
		resp, inner := c.Cluster().ListConfigNodes(ctx)
		if inner != nil {
			return inner
		}
		if resp != nil {
			raws = *resp
		}
		return nil
	}); err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "ListClusterConfigNodes")
	}
	nodes := make([]string, 0, len(raws))
	for _, raw := range raws {
		var item struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Name == "" {
			continue
		}
		nodes = append(nodes, item.Name)
	}
	return nodes, nil
}
