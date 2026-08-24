package pve_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// transient503 builds the real SDK shape for a cycling pvedaemon worker.
func transient503() error {
	return sdkerrors.ParseAPIError(503, []byte(`{"message":"service unavailable"}`))
}

// TestAllocateWithRetry_ExhaustionKeepsConflictCause verifies that exhausting
// every attempt on conflicts surfaces the last conflict as the wrapped cause
// with its retriability derived from it, instead of the context-free permanent
// "exhausted VMID allocation" error that discarded both. A conflict storm is
// contention: the Director re-drives with fresh VMIDs and succeeds.
func TestAllocateWithRetry_ExhaustionKeepsConflictCause(t *testing.T) {
	t.Parallel()
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflict := sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to create VM: config file already exists"}`))
	_, err := pve.AllocateWithRetry(context.Background(), c,
		func(int) error { return conflict },
		func(err error) bool { return strings.Contains(err.Error(), "already exists") },
		3,
		pve.WithNoBackoff(),
	)
	if err == nil {
		t.Fatal("expected an exhaustion error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("exhaustion error lost its cause: %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("exhaustion on a 5xx conflict = %v, want retriable", err)
	}
}

// TestAllocateDiskWithRetry_NonConflictTransientStaysRetriable verifies the
// disk twin of AllocateWithRetry's WrapError treatment: a transient 503 from
// the create closure must classify retriable through the terminal wrap, which
// previously used a bare cpierrors.Wrap and flattened it to permanent.
func TestAllocateDiskWithRetry_NonConflictTransientStaysRetriable(t *testing.T) {
	t.Parallel()
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	_, err := pve.AllocateDiskWithRetry(context.Background(), c, "", "",
		func(int) error { return transient503() },
		func(error) bool { return false },
		3,
		pve.WithNoBackoff(),
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("disk allocation on a transient 503 = %v, want retriable", err)
	}
}

// TestCloneQemuVM_TransientStaysRetriable and the MakeTemplate case below
// verify the two template mutations classify raw SDK errors instead of
// flattening them with a bare cpierrors.Wrap.
func TestCloneQemuVM_TransientStaysRetriable(t *testing.T) {
	t.Parallel()
	nodesSvc := &templateNodesService{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return nil, transient503()
		},
	}
	_, err := pve.CloneQemuVM(context.Background(), newTemplateClient(nodesSvc), "pve1", 6042,
		&sdknodes.CreateQemuCloneParams{Newid: 100})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("CloneQemuVM on a 503 = %v, want retriable", err)
	}
}

func TestMakeTemplate_TransientStaysRetriable(t *testing.T) {
	t.Parallel()
	nodesSvc := &templateNodesService{
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return nil, transient503()
		},
	}
	_, err := pve.MakeTemplate(context.Background(), newTemplateClient(nodesSvc), "pve1", 6042)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("MakeTemplate on a 503 = %v, want retriable", err)
	}
}

// TestFindTemplatesBySHATagCluster_TransientRetriesInPlace verifies the
// cluster template listing rides RetryOnTransient: one 503 then success must
// produce the refs (two list calls), where the unwrapped code failed on the
// first error.
func TestFindTemplatesBySHATagCluster_TransientRetriesInPlace(t *testing.T) {
	t.Parallel()
	const sha8 = "ab12cd34"
	var calls int32
	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 6042, Node: "pve1", Name: "bosh-stemcell-ubuntu-jammy-1.0-" + sha8,
			Tags: "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8, Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, transient503()
		}
		return makeClusterResourcesResponse(items), nil
	})

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	refs, err := pve.FindTemplatesBySHATagCluster(ctx, c, sha8)
	if err != nil {
		t.Fatalf("expected in-place retry to absorb the 503, got: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	// Three fixture reads: the membership listing fails once and retries in
	// place (two), then the per-node qemu listing succeeds (three).
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("fixture reads = %d, want 3 (failed membership, retried membership, node listing)", got)
	}
}

// TestFindTemplatesBySHATagCluster_ExhaustedTransientStaysRetriable verifies
// the persistent-failure classification: the exhausted 503 surfaces retriable.
func TestFindTemplatesBySHATagCluster_ExhaustedTransientStaysRetriable(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return nil, transient503()
	})

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	_, err := pve.FindTemplatesBySHATagCluster(ctx, c, "ab12cd34")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("cluster listing on persistent 503 = %v, want retriable", err)
	}
}

// TestWaitForSnapshotAbsent_TimeoutIsRetriable pins the reclassified timeout:
// still-present after the wait budget means the asynchronous removal is still
// in progress, which is a retriable condition, not a permanent failure.
func TestWaitForSnapshotAbsent_TimeoutIsRetriable(t *testing.T) {
	t.Parallel()
	client := snapClient(func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
		return snapEntries("current", "snap1"), nil // never clears
	})
	err := pve.WaitForSnapshotAbsent(context.Background(), client, "pve1", 9001, "snap1",
		pve.WithPollIntervalForTest(1*time.Millisecond), pve.WithMaxWait(1*time.Second))
	if err == nil {
		t.Fatal("expected timeout error when snapshot never clears")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("snapshot-absent timeout = %v, want retriable (removal still in progress)", err)
	}
}

// TestWrapErrorKeepingClass_PreservesExistingClass verifies the helper's
// contract directly: an already-classified error passes through unchanged in
// both directions, and a raw error still gets classified.
func TestWrapErrorKeepingClass_PreservesExistingClass(t *testing.T) {
	t.Parallel()

	retriable := cpierrors.WrapAs(errors.New("poll timeout, task still running"),
		cpierrors.TypeRetriableCloud, "await")
	if got := pve.WrapErrorKeepingClass(retriable); !cpierrors.IsType(got, cpierrors.TypeRetriableCloud) {
		t.Errorf("retriable class flattened: %v", got)
	}

	permanent := cpierrors.Cloud("a verdict")
	if got := pve.WrapErrorKeepingClass(permanent); cpierrors.IsType(got, cpierrors.TypeRetriableCloud) {
		t.Errorf("permanent class flipped: %v", got)
	}

	if got := pve.WrapErrorKeepingClass(transient503()); !cpierrors.IsType(got, cpierrors.TypeRetriableCloud) {
		t.Errorf("raw 503 not classified: %v", got)
	}
}
