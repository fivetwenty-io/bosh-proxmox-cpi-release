package placement_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestGatherNodeFacts_NotQuorateNamesTheReason gathers facts through an entry
// node that does not report the cluster quorate. Every node stays excluded,
// and the rejection names the lost quorum instead of maintenance, which is
// what placement used to log for every node of a cluster without quorum.
func TestGatherNodeFacts_NotQuorateNamesTheReason(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		quorateRow(0),
		localStatusNode(),
		statusNode("pve2", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{now: 1}, nopLogger(),
		placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, rejections := placement.Filter(facts, placement.Request{})
	for _, f := range facts {
		if !f.InMaintenance {
			t.Errorf("%s: a non-quorate entry node must leave no node placeable", f.Node)
		}
		want := "node not placeable (entry node does not report the cluster quorate)"
		if got := rejections[f.Node]; got != want {
			t.Errorf("%s: rejection = %q, want %q", f.Node, got, want)
		}
		if !placement.IsUnplaceableRejection(rejections[f.Node]) {
			t.Errorf("%s: rejection %q is not recognized as an unplaceable rejection", f.Node, rejections[f.Node])
		}
	}
}

// TestGatherNodeFacts_MaintenanceKeepsItsRejection checks that a node in
// maintenance still carries no other reason and keeps its old rejection.
func TestGatherNodeFacts_MaintenanceKeepsItsRejection(t *testing.T) {
	t.Parallel()
	statusRaw, _ := json.Marshal(map[string]any{
		"type": "node", "name": "pve1", "maxcpu": 8, "maxmem": 8 * gib, "mem": 4 * gib,
		"online": 1, "cpu": 0.0, "tags": "maintenance",
	})
	resp := cluster.ListStatusResponse{json.RawMessage(statusRaw)}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{}, nopLogger(),
		placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || !facts[0].InMaintenance || facts[0].UnplaceableReason != "" {
		t.Fatalf("facts = %+v, want one node in maintenance with no other reason", facts)
	}
	_, rejections := placement.Filter(facts, placement.Request{})
	if got := rejections["pve1"]; got != "node in maintenance" {
		t.Errorf("rejection = %q, want %q", got, "node in maintenance")
	}
	if placement.IsUnplaceableRejection(rejections["pve1"]) {
		t.Error("a maintenance rejection must not read as an unplaceable rejection")
	}
}

// TestGatherNodeFacts_SkipsStorageOfOfflineNodes checks that the per-node
// storage listing is not sent to a node that cluster status lists offline,
// since a member that is gone can take tens of seconds to fail each listing.
func TestGatherNodeFacts_SkipsStorageOfOfflineNodes(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
		statusNode("pve2", 8, 8*gib, 4*gib, 0, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	var mu sync.Mutex
	var asked []string
	base := &stubNodes{}
	ns := &stubNodes{
		storageFn: func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			mu.Lock()
			asked = append(asked, node)
			mu.Unlock()
			return base.ListStorage(ctx, node, params)
		},
	}
	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(),
		placement.GatherOptions{StorageName: "local-lvm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(asked) != 1 || asked[0] != "pve1" {
		t.Errorf("storage was listed on %v, want only pve1", asked)
	}
	for _, f := range facts {
		if f.Node == "pve1" && f.FreeStorageBytes != 10*gib {
			t.Errorf("pve1 FreeStorageBytes = %d, want %d", f.FreeStorageBytes, 10*gib)
		}
	}
}
