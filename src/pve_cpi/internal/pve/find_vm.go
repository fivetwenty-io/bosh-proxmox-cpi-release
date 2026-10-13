package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// VMLocation is FindVMAuthoritative's answer: where the guest lives, its raw
// (semicolon-delimited) tag string, and whether it exists at all. Found=false
// means absence was proven against every reachable member: each answered the
// config probe with a clean not-found, never a bare index miss. For
// FindVMAuthoritative, members the quorate cluster reports offline are
// excluded from that proof, and absence for their guests rests on the index
// rows that persist while a node is down (see the offline-member tolerance
// paragraph on FindVMAuthoritative). FindVMByConfigProbe never reads the
// index, so it excludes no member: its Found=false means every member
// answered a clean not-found, in two complete sweeps.
//
// Template and Type come from the same answer that located the guest, so the
// VM ownership guard can refuse a template or a container without another
// read. Type is the index row's resource type ("qemu" or "lxc"); a hit on the
// config-probe path is always "qemu", because the probe reads the QEMU
// config endpoint, and Template is then read from that config's "template"
// flag.
//
// FromConfig is true when the answer came from a config read on the node
// rather than from the index, which is the config-probe path. Tags and
// Template are then node-local truth, so a caller that would otherwise read
// the config to confirm them can judge the location as it stands.
type VMLocation struct {
	Node       string
	Tags       string
	Found      bool
	Template   bool
	Type       string
	FromConfig bool
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
	row, err := findVMRowViaCluster(ctx, c, vmid)
	if err != nil {
		return VMLocation{}, err
	}
	if row.Found && row.Node != "" {
		return row, nil
	}
	return probeVMConfigs(ctx, c, vmid, "FindVMAuthoritative", "absent from the cluster index", true)
}

// configProbeAbsenceSweeps is how many complete sweeps FindVMByConfigProbe
// runs before it answers absent. PVE moves a migrating guest's config with a
// single rename at the end of the migration, so one sequential sweep can
// probe the target before the rename and the source after it and see the
// guest nowhere. The rename happens once, so a second sweep that starts after
// the first one ends sees the config on the target.
const configProbeAbsenceSweeps = 2

// FindVMByConfigProbe locates a guest by probing every cluster member's QEMU
// config, and it never consults the /cluster/resources index. It is for a
// caller whose index row has just been contradicted, such as a config read on
// the row's node that answered not-found, because the index would only repeat
// that stale row.
//
// A hit returns the node with FromConfig set. Found=false means two complete
// sweeps each found every member answering a clean not-found (404, or
// pmxcfs's config-missing 500); the second sweep closes the window in which a
// migration's config rename lands between two probes of one sweep, and it
// runs only on the absence path. A hit in the second sweep returns that
// location.
//
// The failure rules are stricter than FindVMAuthoritative's. A member the
// quorate cluster reports offline is not excluded from the proof, because no
// index row stands in for its guests here, so an offline member makes the
// answer a retriable error with no hit elsewhere. So do an enumeration
// failure, an empty member list, and any member's probe error. A nil client,
// cluster service, or QEMU service is retriable too, because absence cannot
// be proven without probing the members.
func FindVMByConfigProbe(ctx context.Context, c Client, vmid int) (VMLocation, error) {
	if c == nil || c.Cluster() == nil || c.QEMU() == nil {
		return VMLocation{}, cpierrors.Retriable("FindVMByConfigProbe: VM %d: cluster service unavailable; cannot prove absence", vmid)
	}
	if vmid <= 0 {
		return VMLocation{}, nil
	}
	for range configProbeAbsenceSweeps {
		loc, err := probeVMConfigs(ctx, c, vmid, "FindVMByConfigProbe", "contradicted by its node", false)
		if err != nil {
			return VMLocation{}, err
		}
		if loc.Found {
			return loc, nil
		}
	}
	return VMLocation{}, nil
}

// probeVMConfigs runs one sweep of the config-probe fan-out behind
// FindVMAuthoritative's miss path and FindVMByConfigProbe. caller names the
// public function and why names the reason the index answer was set aside,
// for the log lines and error messages. tolerateOffline excludes a member the
// quorate cluster reports offline from the absence proof, which only
// FindVMAuthoritative may do, because its index fast path still covers that
// member's guests. Without it, such a member is not probed, since its config
// endpoint cannot answer while the node is down, and a sweep with no hit
// returns a retriable error naming it.
func probeVMConfigs(ctx context.Context, c Client, vmid int, caller, why string, tolerateOffline bool) (VMLocation, error) {
	nodes, enumErr := ListClusterMemberNames(ctx, c)
	if enumErr != nil {
		return VMLocation{}, cpierrors.WrapAs(enumErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: VM %d %s, and node enumeration failed; cannot prove absence", caller, vmid, why))
	}
	if len(nodes) == 0 {
		return VMLocation{}, cpierrors.Retriable(
			"%s: VM %d %s, and node enumeration returned zero nodes; cannot prove absence", caller, vmid, why)
	}

	offline := offlineClusterNodes(ctx, c, log.FromContext(ctx))
	var failedNodes []string
	var unprovenOffline []string
	var lastProbeErr error
	probedAny := false
	for _, n := range nodes {
		if offline[n] {
			if tolerateOffline {
				log.FromContext(ctx).Warn(caller+": excluding member the cluster reports offline; absence rests on the index for its guests",
					log.String("node", n), log.Int("vmid", vmid))
			} else {
				log.FromContext(ctx).Warn(caller+": member the cluster reports offline cannot answer the config probe; absence stays unproven",
					log.String("node", n), log.Int("vmid", vmid))
				unprovenOffline = append(unprovenOffline, n)
			}
			continue
		}
		probedAny = true
		var cfg map[string]any
		missing := false
		probeErr := RetryOnTransient(ctx, nil, "find_vm_authoritative_probe", 0, func() error {
			var inner error
			// The CPI owns retries for this read so it can distinguish a settled
			// missing-config 500 from transient failures before any backoff.
			cfg, inner = c.QEMU().Config(sdkclient.WithRetries(ctx, 0), n, vmid)
			missing = IsNotFound(inner) || IsPmxcfsConfigMissing(inner)
			if missing {
				return nil
			}
			return inner
		})
		if missing {
			continue
		}
		if probeErr != nil {
			failedNodes = append(failedNodes, n)
			lastProbeErr = probeErr
			continue
		}
		cfgTags, _ := ConfigString(cfg, "tags")
		return VMLocation{Node: n, Tags: cfgTags, Found: true, Template: ConfigFlag(cfg, "template"), Type: "qemu", FromConfig: true}, nil
	}
	if len(failedNodes) > 0 {
		return VMLocation{}, cpierrors.WrapAs(WrapError(lastProbeErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: VM %d %s, and the config probe failed on node(s) %s; cannot prove absence",
				caller, vmid, why, strings.Join(failedNodes, ",")))
	}
	if len(unprovenOffline) > 0 {
		return VMLocation{}, cpierrors.Retriable(
			"%s: VM %d %s, and the cluster reports node(s) %s offline, so their configs cannot be probed; cannot prove absence",
			caller, vmid, why, strings.Join(unprovenOffline, ","))
	}
	if !probedAny {
		return VMLocation{}, cpierrors.Retriable(
			"%s: VM %d %s, and every cluster member reports offline; cannot prove absence", caller, vmid, why)
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
