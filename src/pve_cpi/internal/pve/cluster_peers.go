package pve

import (
	"context"
	"encoding/json"
	"sort"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// clusterStatusPeer is the minimal shape needed from GET /cluster/status rows
// for VXLAN peer derivation. The "online" field is an integer in the PVE API
// (1 = online, 0 = offline); "ip" is present only on type=node rows.
type clusterStatusPeer struct {
	Type   string `json:"type"`
	IP     string `json:"ip"`
	Online int64  `json:"online"`
}

// ClusterNodePeerIPs returns the sorted management IPs of all online cluster
// nodes from GET /cluster/status. It backs VXLAN peer derivation when the
// operator has not set sdn_vxlan_peers explicitly.
//
// An empty result is NOT an error — a caller that requires at least one peer
// (vxlan zone creation) must enforce that itself with an actionable message.
// Offline nodes are excluded deliberately: a vxlan zone created while a node
// is down simply omits that node's tunnel endpoint, and the operator can
// re-apply with explicit peers once the node returns.
//
// The status call is wrapped in RetryOnTransient to absorb the
// pvedaemon-worker-recycle window (see listClusterVMIDs).
func ClusterNodePeerIPs(ctx context.Context, c Client) ([]string, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ClusterNodePeerIPs: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("ClusterNodePeerIPs: client must not be nil")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("ClusterNodePeerIPs: cluster service unavailable")
	}

	var resp *sdkcluster.ListStatusResponse
	err := RetryOnTransient(ctx, nil, "peer_list_cluster_status", 0, func() error {
		var inner error
		resp, inner = svc.ListStatus(ctx)
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "peers: list cluster status")
	}
	if resp == nil {
		return nil, cpierrors.Cloud("peers: nil response from cluster status")
	}

	peers := make([]string, 0, len(*resp))
	for _, raw := range *resp {
		var item clusterStatusPeer
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Skip malformed entries; a single bad item should not abort derivation.
			continue
		}
		if item.Type != resourceTypeNode || item.Online != 1 || item.IP == "" {
			continue
		}
		peers = append(peers, item.IP)
	}
	sort.Strings(peers)
	return peers, nil
}
