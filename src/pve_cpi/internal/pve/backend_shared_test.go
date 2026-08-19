package pve

import (
	"context"
	"errors"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

func TestSharedBackend_NodeForCreate_PrefersCloudPropNode(t *testing.T) {
	t.Parallel()
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "100", "pve-explicit")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-explicit" {
		t.Fatalf("got %q, want pve-explicit", got)
	}
}

func TestSharedBackend_NodeForCreate_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "", "")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
}

func TestSharedBackend_NodeForCreate_ErrorsWhenNothingResolves(t *testing.T) {
	t.Parallel()
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "")
	_, err := b.NodeForCreate(context.Background(), "", "")
	if err == nil {
		t.Fatalf("expected error when no node hints available")
	}
}

func TestSharedBackend_NodeForExisting_UsesDefault(t *testing.T) {
	t.Parallel()
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
}

func TestSharedBackend_NodeForExisting_FallsBackToInfoNodes(t *testing.T) {
	t.Parallel()
	b := newSharedBackend(nil, StorageInfo{Name: "nfs", Type: "nfs", Nodes: []string{"pve-02", "pve-03"}}, "")
	got, err := b.NodeForExisting(context.Background(), "anything")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-02" {
		t.Fatalf("got %q, want pve-02", got)
	}
}

// TestSharedBackend_NodeForCreate_VMHintLookupErrorPropagates confirms that a
// transport failure from nodeFromCluster (the /cluster/resources lookup used
// to co-locate a new disk with its owning VM's node) is wrapped and returned,
// mirroring localBackend.NodeForCreate, instead of being silently swallowed
// and falling through to defaultNode as if the VM simply did not exist yet.
// Reuses backendTestClient/fakeCluster defined in backend_local_test.go (same
// package, same test binary).
func TestSharedBackend_NodeForCreate_VMHintLookupErrorPropagates(t *testing.T) {
	t.Parallel()
	rawErr := errors.New("api unreachable")
	c := &backendTestClient{
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return nil, rawErr
			},
		},
	}
	// No cloudPropNode and no defaultNode, so the only path that could
	// otherwise mask the lookup failure (falling through to defaultNode) is
	// unavailable — if the error were swallowed, NodeForCreate would instead
	// return formatNodeResolveError's generic "cannot resolve node" message.
	b := newSharedBackend(c, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "")
	_, err := b.NodeForCreate(context.Background(), "100", "")
	if err == nil {
		t.Fatalf("expected error when vmHint cluster lookup fails")
	}

	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error does not carry cpierrors classification (type=%T): %v", err, err)
	}
	if !errors.Is(err, rawErr) {
		t.Fatalf("wrapped error does not chain to the original transport error; err=%v", err)
	}
}
