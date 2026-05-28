package pve

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// ---------------------------------------------------------------------------
// minimal pve.Client mock for backend tests (lives in-package).
// ---------------------------------------------------------------------------

type backendTestClient struct {
	storageSvc storage.Service
	clusterSvc sdkcluster.Service
}

func (b *backendTestClient) QEMU() qemu.Service                     { return nil }
func (b *backendTestClient) Storage() storage.Service               { return b.storageSvc }
func (b *backendTestClient) CloudInit() cloudinit.Service           { return nil }
func (b *backendTestClient) Tasks() tasks.Service                   { return nil }
func (b *backendTestClient) Nodes() nodes.Service                   { return nil }
func (b *backendTestClient) Cluster() sdkcluster.Service            { return b.clusterSvc }
func (b *backendTestClient) ClusterStorage() clusterstorage.Service { return nil }
func (b *backendTestClient) Pools() PoolService                     { return nil }

// ---------------------------------------------------------------------------
// fake cluster.Service
// ---------------------------------------------------------------------------

type fakeCluster struct {
	sdkcluster.Service
	listFn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (f *fakeCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	return f.listFn(ctx, params)
}

// clusterResp builds a /cluster/resources response from typed rows.
func clusterResp(rows ...map[string]any) *sdkcluster.ListResourcesResponse {
	out := make(sdkcluster.ListResourcesResponse, 0, len(rows))
	for _, r := range rows {
		b, _ := json.Marshal(r)
		out = append(out, b)
	}
	return &out
}

// ---------------------------------------------------------------------------
// fake storage.Service (Exists only)
// ---------------------------------------------------------------------------

type fakeStorage struct {
	storage.Service
	existsFn func(ctx context.Context, node, storage, volume string) (bool, error)
}

func (f *fakeStorage) Exists(ctx context.Context, node, storage, volume string) (bool, error) {
	return f.existsFn(ctx, node, storage, volume)
}

// ---------------------------------------------------------------------------
// NodeForCreate
// ---------------------------------------------------------------------------

func TestLocalBackend_NodeForCreate_CoLocatesWithVMHint(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(
					map[string]any{"vmid": 100, "node": "pve-02"},
					map[string]any{"vmid": 200, "node": "pve-03"},
				), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "100", "ignored-by-vmHint-priority")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-02" {
		t.Fatalf("got %q, want pve-02 (VM 100 lives there)", got)
	}
}

func TestLocalBackend_NodeForCreate_VMHintMiss_FallsBackToCloudProp(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(), nil // VM 100 doesn't exist
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "100", "pve-cloud")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-cloud" {
		t.Fatalf("got %q, want pve-cloud", got)
	}
}

func TestLocalBackend_NodeForCreate_EmptyVMHintAndNoCloudProp_UsesDefault(t *testing.T) {
	t.Parallel()
	b := newLocalBackend(nil, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "", "")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
}

func TestLocalBackend_NodeForCreate_NoResolution_Errors(t *testing.T) {
	t.Parallel()
	b := newLocalBackend(nil, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "")
	_, err := b.NodeForCreate(context.Background(), "", "")
	if err == nil {
		t.Fatalf("expected error when no node resolvable")
	}
}

// ---------------------------------------------------------------------------
// NodeForExisting (cluster-wide scan)
// ---------------------------------------------------------------------------

func TestLocalBackend_NodeForExisting_FindsOwnerViaExistsProbe(t *testing.T) {
	t.Parallel()
	// pve-01 says no, pve-02 says yes.
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, node, _, _ string) (bool, error) {
				return node == "pve-02", nil
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(
					map[string]any{"node": "pve-01"},
					map[string]any{"node": "pve-02"},
				), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "")
	got, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-02" {
		t.Fatalf("got %q, want pve-02", got)
	}
}

func TestLocalBackend_NodeForExisting_PrefersDefaultNodeFirst(t *testing.T) {
	t.Parallel()
	probedNodes := []string{}
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, node, _, _ string) (bool, error) {
				probedNodes = append(probedNodes, node)
				return node == "pve-default", nil
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(
					map[string]any{"node": "pve-01"},
					map[string]any{"node": "pve-default"},
				), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "pve-default")
	got, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
	if len(probedNodes) == 0 || probedNodes[0] != "pve-default" {
		t.Fatalf("probedNodes[0]=%v, want pve-default first (cheap-hit ordering)", probedNodes)
	}
}

func TestLocalBackend_NodeForExisting_NoOwner_DiskNotFound(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
				return false, nil
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(map[string]any{"node": "pve-01"}), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "pve-default")
	_, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err == nil {
		t.Fatalf("expected DiskNotFound error")
	}
}

func TestLocalBackend_NodeForExisting_ClusterListErrorPropagates(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
				return false, nil
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return nil, errors.New("api unreachable")
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "")
	_, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err == nil {
		t.Fatalf("expected error when cluster list fails")
	}
}

func TestNodeForExisting_AllNodesError_ReturnsRetriable(t *testing.T) {
	t.Parallel()
	// All candidate Exists() probes fail → must return retriable error, NOT DiskNotFound.
	probeErr := errors.New("connection refused")
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
				return false, probeErr
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(
					map[string]any{"node": "pve-01"},
					map[string]any{"node": "pve-02"},
				), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "")
	_, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err == nil {
		t.Fatalf("expected error when all probes fail")
	}

	// Must NOT be DiskNotFound — that would silently hide the cluster outage.
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got DiskNotFound but expected retriable error; err=%v", err)
	}

	// Must be retriable (ok_to_retry=true).
	type retriableChecker interface {
		OkToRetry() bool
	}
	rc, ok := err.(retriableChecker)
	if !ok {
		t.Fatalf("error does not implement OkToRetry(); type=%T err=%v", err, err)
	}
	if !rc.OkToRetry() {
		t.Fatalf("expected OkToRetry()=true, got false; err=%v", err)
	}
}

// TestCandidateNodes_RetriesOnTransient confirms candidateNodes wraps the
// /cluster/resources call in RetryOnTransient so a transient SDK error (e.g.
// pvedaemon worker recycle) does not cascade into a backend resolve failure.
// The fake cluster service returns a transient ConnectionError on the first
// call and a healthy response on the second; the test asserts the eventual
// success path runs (NodeForExisting returns the expected node) and that
// ListResources was invoked at least twice.
func TestCandidateNodes_RetriesOnTransient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, node, _, _ string) (bool, error) {
				return node == "pve-02", nil
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				n := calls.Add(1)
				if n == 1 {
					// Transient ConnectionError: IsTransientTransport returns true.
					return nil, &sdkerrors.ConnectionError{
						Host:    "pve.example",
						Port:    8006,
						Message: "transient blip",
					}
				}
				return clusterResp(
					map[string]any{"node": "pve-01"},
					map[string]any{"node": "pve-02"},
				), nil
			},
		},
	}
	b := newLocalBackend(c, StorageInfo{Name: "local-zfs", Type: "zfspool"}, "")
	got, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err != nil {
		t.Fatalf("NodeForExisting after transient retry: %v", err)
	}
	if got != "pve-02" {
		t.Errorf("got %q, want pve-02", got)
	}
	if n := calls.Load(); n < 2 {
		t.Errorf("ListResources call count = %d, want ≥2 (retry should have re-invoked)", n)
	}
}

func TestLocalBackend_NodeForExisting_RestrictedNodes_SkipsClusterScan(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, node, _, _ string) (bool, error) {
				return node == "pve-03", nil
			},
		},
		// clusterSvc deliberately nil — restricted-nodes path must not call it.
	}
	b := newLocalBackend(c, StorageInfo{Name: "shared-lvm", Type: "lvm", Nodes: []string{"pve-03", "pve-04"}}, "")
	got, err := b.NodeForExisting(context.Background(), "anything")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-03" {
		t.Fatalf("got %q, want pve-03", got)
	}
}
