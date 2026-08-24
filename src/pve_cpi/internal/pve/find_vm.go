package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// VMLocation is FindVMAuthoritative's answer: where the guest lives, its raw
// (semicolon-delimited) tag string, and whether it exists at all. Found=false
// means absence was proven against every reachable member: each answered the
// config probe with a clean not-found, never a bare index miss. Members the
// quorate cluster reports offline are excluded from that proof, and absence
// for their guests rests on the index rows that persist while a node is down
// (see the offline-member tolerance paragraph on FindVMAuthoritative).
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
// Offline-member tolerance: a member the quorate cluster itself reports
// offline is excluded from the probe fan-out (its config endpoint can never
// answer while the node is down) rather than blocking every miss-path lookup
// cluster-wide, which would wedge exactly the has_vm/delete_vm re-drives that
// resurrection depends on when a node dies. The absence conclusion stays
// sound because the index fast path above still covers the excluded member:
// its guests' rows persist in /cluster/resources while the node is down, so
// an index MISS plus a clean probe of every online member proves absence for
// everything except a guest created on that member inside the index lag
// window immediately before it went dark, a corner logged via the Warn on
// exclusion. Without quorum, or with every member offline, the tolerance is
// withheld and the fail-loud rule stands.
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

	nodes, enumErr := ListClusterMemberNames(ctx, c)
	if enumErr != nil {
		return VMLocation{}, cpierrors.WrapAs(enumErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("FindVMAuthoritative: VM %d absent from the cluster index, and node enumeration failed; cannot prove absence", vmid))
	}
	if len(nodes) == 0 {
		return VMLocation{}, cpierrors.Retriable(
			"FindVMAuthoritative: VM %d absent from the cluster index, and node enumeration returned zero nodes; cannot prove absence", vmid)
	}

	offline := offlineClusterNodes(ctx, c, log.FromContext(ctx))
	var failedNodes []string
	var lastProbeErr error
	probedAny := false
	for _, n := range nodes {
		if offline[n] {
			log.FromContext(ctx).Warn("FindVMAuthoritative: excluding member the cluster reports offline; absence rests on the index for its guests",
				log.String("node", n), log.Int("vmid", vmid))
			continue
		}
		probedAny = true
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
	if !probedAny {
		return VMLocation{}, cpierrors.Retriable(
			"FindVMAuthoritative: VM %d absent from the cluster index, and every cluster member reports offline; cannot prove absence", vmid)
	}
	return VMLocation{}, nil
}

// ListClusterMemberNames returns the authoritative membership for
// cluster-wide fan-outs. The primary source is corosync-backed
// GET /cluster/config/nodes (ListClusterConfigNodes). A host that was never
// joined to a cluster has no corosync configuration, and that endpoint then
// answers with an empty list on some PVE versions and a resolved API error
// on others; both shapes fall back to GET /nodes, which the host serves from
// its own member state and which on a standalone host names exactly itself,
// so the documented single-node topology keeps working. Two failure shapes
// never fall back: a transport failure, because membership is then unknown,
// not empty; and an auth or permission verdict (401/403), because that never
// means "no corosync configuration", and under an asymmetric ACL the
// permission-filtered GET /nodes answer could name a subset of a real
// cluster. Returning either as membership would repeat the partial-fleet bug
// this package exists to prevent, so both stay fail-loud.
func ListClusterMemberNames(ctx context.Context, c Client) ([]string, error) {
	names, err := ListClusterConfigNodes(ctx, c)
	if err == nil && len(names) > 0 {
		return names, nil
	}
	if err != nil {
		code, isAPIVerdict := apiHTTPCode(err)
		if !isAPIVerdict {
			// Unclassifiable transport fault: the corosync answer is
			// unknown, not "no cluster"; no fallback.
			return nil, err
		}
		if code == 401 || code == 403 {
			// An auth/permission verdict is a settled answer about this
			// caller's grants, not about cluster membership; no fallback.
			return nil, err
		}
	}
	fbNames, fbErr := listNodesMembership(ctx, c)
	if fbErr == nil && len(fbNames) > 0 {
		return fbNames, nil
	}
	if err != nil {
		return nil, err
	}
	if fbErr != nil {
		return nil, cpierrors.Wrap(WrapError(fbErr), "ListClusterMemberNames: GET /nodes fallback")
	}
	return nil, cpierrors.Retriable(
		"ListClusterMemberNames: both /cluster/config/nodes and /nodes returned no members; cannot enumerate the cluster")
}

// listNodesMembership reads GET /nodes as the standalone-host membership
// fallback; see ListClusterMemberNames.
func listNodesMembership(ctx context.Context, c Client) ([]string, error) {
	if c == nil || c.Nodes() == nil {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := RetryOnTransient(ctx, nil, "list_nodes_membership", 0, func() error {
		resp, inner := c.Nodes().ListNodes(ctx)
		if inner != nil {
			return inner
		}
		raws = nil
		if resp != nil {
			raws = *resp
		}
		return nil
	}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raws))
	for _, raw := range raws {
		var item struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Node == "" {
			continue
		}
		names = append(names, item.Node)
	}
	return names, nil
}

// ListClusterConfigNodes returns every cluster member's node name from
// GET /cluster/config/nodes — corosync membership, which unlike the
// /cluster/resources index does not lag and includes nodes the index has not
// caught up with yet (recently joined, or briefly unindexed). The listing is
// wrapped in RetryOnTransient; a persistent failure returns the classified
// error for the caller to surface. Fan-out callers should prefer
// ListClusterMemberNames, which adds the standalone-host fallback.
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
