package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// nlCluster answers cluster status from rows and reports no HA view.
type nlCluster struct {
	sdkcluster.Service
	rows []json.RawMessage
	err  error
}

func (c *nlCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	resp := sdkcluster.ListStatusResponse(c.rows)
	return &resp, nil
}

func (c *nlCluster) ListHaStatusManagerStatus(context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	empty := sdkcluster.ListHaStatusManagerStatusResponse{}
	return &empty, nil
}

func (c *nlCluster) ListHaStatusCurrent(context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

type nlNodes struct{ sdknodes.Service }

func (nlNodes) ListTime(context.Context, string) (*sdknodes.ListTimeResponse, error) {
	return nil, errors.New("clock not served")
}

type nlClient struct {
	Client
	cluster sdkcluster.Service
	nodes   sdknodes.Service
}

func (c *nlClient) Cluster() sdkcluster.Service { return c.cluster }
func (c *nlClient) Nodes() sdknodes.Service     { return c.nodes }

// nlRow is a node row of cluster status; online false is a node PVE reports
// offline.
func nlRow(name string, online bool) json.RawMessage {
	o := 0
	if online {
		o = 1
	}
	raw, _ := json.Marshal(map[string]any{"type": "node", "name": name, "online": o, "id": "node/" + name})
	return raw
}

func nlQuorum() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "cluster", "name": "lab", "quorate": 1, "nodes": 3})
	return raw
}

// nlClientFor builds a client whose cluster holds the given rows.
func nlClientFor(rows ...json.RawMessage) *nlClient {
	return &nlClient{cluster: &nlCluster{rows: append([]json.RawMessage{nlQuorum()}, rows...)}, nodes: nlNodes{}}
}

func TestSharedBackend_NodeForExisting_DeadConfigNodeMovesToStorageNode(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", true), nlRow("pve3", true))
	b := newSharedBackend(c, StorageInfo{Name: "nfs1", Type: "nfs", Nodes: []string{"pve1", "pve3"}}, "pve1")
	got, err := b.NodeForExisting(context.Background(), "nfs1:100/vm-100-disk-0.qcow2")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve3" {
		t.Fatalf("got %q, want pve3: config.node is dead and pve2 is outside the storage's node list", got)
	}
}

func TestSharedBackend_NodeForExisting_PrefersLiveConfigNode(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", true), nlRow("pve2", true))
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve2")
	got, err := b.NodeForExisting(context.Background(), "ceph:vm-100-disk-0")
	if err != nil || got != "pve2" {
		t.Fatalf("got %q err=%v, want pve2", got, err)
	}
}

func TestSharedBackend_NodeForExisting_UnrestrictedStorageUsesAnyOnlineNode(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", false), nlRow("pve3", true))
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve1")
	got, err := b.NodeForExisting(context.Background(), "ceph:vm-100-disk-0")
	if err != nil || got != "pve3" {
		t.Fatalf("got %q err=%v, want the only online node pve3", got, err)
	}
}

func TestSharedBackend_NodeForExisting_AllNodesRefusedIsRetriableError(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", false))
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve1")
	got, err := b.NodeForExisting(context.Background(), "ceph:vm-100-disk-0")
	if err == nil {
		t.Fatalf("got node %q, want an error because no node answers", got)
	}
	var ce *cpierrors.Error
	if !errors.As(err, &ce) || !ce.OkToRetry() {
		t.Fatalf("want a retriable error, got %v", err)
	}
	if !strings.Contains(err.Error(), "pve1") {
		t.Errorf("the error should name the refused config.node: %v", err)
	}
}

func TestSharedBackend_NodeForExisting_RestrictedStorageNeverFallsOutsideItsNodes(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", true))
	b := newSharedBackend(c, StorageInfo{Name: "nfs1", Type: "nfs", Nodes: []string{"pve1"}}, "pve1")
	if got, err := b.NodeForExisting(context.Background(), "nfs1:100/x"); err == nil {
		t.Fatalf("got %q: pve2 is online but the storage is not mounted there", got)
	}
}

func TestSharedBackend_NodeForExisting_HealthReadFailureIsRetriable(t *testing.T) {
	t.Parallel()
	c := &nlClient{cluster: &nlCluster{err: errors.New("connection refused")}, nodes: nlNodes{}}
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve1")
	if _, err := b.NodeForExisting(context.Background(), "ceph:vm-100-disk-0"); err == nil {
		t.Fatal("want an error when node health cannot be read")
	}
}

func TestSharedBackend_NodeForCreate_SkipsDeadConfigNode(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", true))
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve1")
	got, err := b.NodeForCreate(context.Background(), "", "")
	if err != nil || got != "pve2" {
		t.Fatalf("got %q err=%v, want pve2", got, err)
	}
}

func TestSharedBackend_NodeForCreate_CloudPropNodeIsTheOperatorsChoice(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false))
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd"}, "pve1")
	got, err := b.NodeForCreate(context.Background(), "", "pve7")
	if err != nil || got != "pve7" {
		t.Fatalf("got %q err=%v, want the explicit pve7", got, err)
	}
}

func TestLocalBackend_NodeForCreate_DeadConfigNodeFallbackFails(t *testing.T) {
	t.Parallel()
	c := nlClientFor(nlRow("pve1", false), nlRow("pve2", true))
	b := newLocalBackend(c, StorageInfo{Name: "lvm1", Type: "lvmthin"}, "pve1", nil)
	if got, err := b.NodeForCreate(context.Background(), "", ""); err == nil {
		t.Fatalf("got %q, want an error because config.node is dead", got)
	}
	live := nlClientFor(nlRow("pve1", true))
	b = newLocalBackend(live, StorageInfo{Name: "lvm1", Type: "lvmthin"}, "pve1", nil)
	if got, err := b.NodeForCreate(context.Background(), "", ""); err != nil || got != "pve1" {
		t.Fatalf("got %q err=%v, want pve1", got, err)
	}
}
