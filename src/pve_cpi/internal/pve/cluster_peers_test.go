package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// statusStubCluster satisfies cluster.Service via embedding; only ListStatus
// is overridden. All other methods panic if called.
type statusStubCluster struct {
	sdkcluster.Service
	listStatusFn func(ctx context.Context) (*sdkcluster.ListStatusResponse, error)
}

func (s *statusStubCluster) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	return s.listStatusFn(ctx)
}

// statusRow marshals one /cluster/status row. online uses the PVE integer
// boolean encoding.
func statusRow(typ, name, ip string, online int) json.RawMessage {
	entry := map[string]any{"type": typ, "name": name, "online": online}
	if ip != "" {
		entry["ip"] = ip
	}
	raw, _ := json.Marshal(entry)
	return raw
}

func newPeersClient(rows ...json.RawMessage) *mockClient {
	resp := sdkcluster.ListStatusResponse(rows)
	return &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
				return &resp, nil
			},
		},
	}
}

func TestClusterNodePeerIPs_FiltersAndSorts(t *testing.T) {
	t.Parallel()
	c := newPeersClient(
		statusRow("cluster", "lab", "", 1),           // non-node row excluded
		statusRow("node", "pve3", "192.168.1.30", 1), // included
		statusRow("node", "pve1", "192.168.1.10", 1), // included; sorts first
		statusRow("node", "pve2", "192.168.1.20", 0), // offline excluded
		statusRow("node", "pve4", "", 1),             // missing ip excluded
	)

	peers, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"192.168.1.10", "192.168.1.30"}
	if !reflect.DeepEqual(peers, want) {
		t.Errorf("peers = %v, want %v", peers, want)
	}
}

func TestClusterNodePeerIPs_EmptyStatus_NotAnError(t *testing.T) {
	t.Parallel()
	c := newPeersClient()

	peers, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("peers = %v, want empty", peers)
	}
}

func TestClusterNodePeerIPs_SingleNode(t *testing.T) {
	t.Parallel()
	c := newPeersClient(statusRow("node", "pve1", "10.0.0.1", 1))

	peers, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(peers) != 1 || peers[0] != "10.0.0.1" {
		t.Errorf("peers = %v, want [10.0.0.1]", peers)
	}
}

func TestClusterNodePeerIPs_ListError_Wrapped(t *testing.T) {
	t.Parallel()
	c := &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
				return nil, errors.New("boom")
			},
		},
	}

	_, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClusterNodePeerIPs_MalformedRowSkipped(t *testing.T) {
	t.Parallel()
	c := newPeersClient(
		json.RawMessage(`"not an object"`),
		statusRow("node", "pve1", "10.0.0.1", 1),
	)

	peers, err := pve.ClusterNodePeerIPs(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(peers) != 1 || peers[0] != "10.0.0.1" {
		t.Errorf("peers = %v, want [10.0.0.1]", peers)
	}
}

func TestClusterNodePeerIPs_NilArgs(t *testing.T) {
	t.Parallel()
	if _, err := pve.ClusterNodePeerIPs(context.Background(), nil); err == nil {
		t.Error("nil client: expected error")
	}
	//lint:ignore SA1012 deliberate nil-ctx contract check
	//nolint:staticcheck // deliberate nil-ctx contract check
	if _, err := pve.ClusterNodePeerIPs(nil, newPeersClient()); err == nil {
		t.Error("nil ctx: expected error")
	}
}
