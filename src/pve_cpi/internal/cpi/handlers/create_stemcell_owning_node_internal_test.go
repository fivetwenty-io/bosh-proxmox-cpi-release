package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// onMockClusterStorage serves one storage entry with a configurable node
// restriction, for stemcellStorageOwningNode tests.
type onMockClusterStorage struct {
	sdkclusterstorage.Service
	storageName string
	nodesCSV    string
	fail        bool
}

func (m *onMockClusterStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	if m.fail {
		return nil, context.DeadlineExceeded
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": m.storageName,
		"type":    "lvmthin",
		"shared":  0,
		"nodes":   m.nodesCSV,
	})
	resp := sdkclusterstorage.ListStorageResponse{raw}
	return &resp, nil
}

func onDeps(cs sdkclusterstorage.Service) Deps {
	return Deps{
		PVE:    &wbMockClient{clusterStorageSvc: cs},
		Logger: log.NewNopLogger(),
	}
}

// TestStemcellStorageOwningNodeKeepsUnrestricted: an empty nodes restriction
// means the storage is available everywhere; the configured node stays.
func TestStemcellStorageOwningNodeKeepsUnrestricted(t *testing.T) {
	t.Parallel()
	deps := onDeps(&onMockClusterStorage{storageName: "data", nodesCSV: ""})
	if got := stemcellStorageOwningNode(context.Background(), deps, "pve1", "data"); got != "pve1" {
		t.Fatalf("expected configured node pve1 kept, got %q", got)
	}
}

// TestStemcellStorageOwningNodeKeepsOwner: the configured node is inside the
// restriction set; no retarget happens.
func TestStemcellStorageOwningNodeKeepsOwner(t *testing.T) {
	t.Parallel()
	deps := onDeps(&onMockClusterStorage{storageName: "data", nodesCSV: "pve2,pve1"})
	if got := stemcellStorageOwningNode(context.Background(), deps, "pve1", "data"); got != "pve1" {
		t.Fatalf("expected configured owner pve1 kept, got %q", got)
	}
}

// TestStemcellStorageOwningNodeRetargets: the configured node is outside the
// restriction set; the call retargets to the lexicographically first owner.
func TestStemcellStorageOwningNodeRetargets(t *testing.T) {
	t.Parallel()
	deps := onDeps(&onMockClusterStorage{storageName: "data", nodesCSV: "pve3,pve2"})
	if got := stemcellStorageOwningNode(context.Background(), deps, "pve1", "data"); got != "pve2" {
		t.Fatalf("expected retarget to first owner pve2, got %q", got)
	}
}

// TestStemcellStorageOwningNodeFailsOpen: a storage-listing failure keeps the
// configured node, matching every other liveStorageInfo consumer.
func TestStemcellStorageOwningNodeFailsOpen(t *testing.T) {
	t.Parallel()
	deps := onDeps(&onMockClusterStorage{storageName: "data", fail: true})
	if got := stemcellStorageOwningNode(context.Background(), deps, "pve1", "data"); got != "pve1" {
		t.Fatalf("expected fail-open to configured node pve1, got %q", got)
	}
}

// TestStemcellStorageOwningNodeUnknownStorage: a storage the listing does not
// carry cannot be classified; the configured node stays.
func TestStemcellStorageOwningNodeUnknownStorage(t *testing.T) {
	t.Parallel()
	deps := onDeps(&onMockClusterStorage{storageName: "other", nodesCSV: "pve3"})
	if got := stemcellStorageOwningNode(context.Background(), deps, "pve1", "data"); got != "pve1" {
		t.Fatalf("expected unknown storage to keep pve1, got %q", got)
	}
}
