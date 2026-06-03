// Retryability boundary tests for AssignVMToPool and the error classification
// helpers it relies on. Each test asserts the OkToRetry() bit for a representative
// error shape — transient (5xx, connection, timeout, storage-lock) → true;
// permanent (400, 404, validation) → false.
package pve_test

import (
	"context"
	"errors"
	"testing"

	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

// retryPoolService implements pve.PoolService with a configurable AddVM func.
type retryPoolService struct {
	addVMFn func(ctx context.Context, poolID string, vmid int64) error
}

func (s *retryPoolService) AddVM(ctx context.Context, poolID string, vmid int64) error {
	if s.addVMFn != nil {
		return s.addVMFn(ctx, poolID, vmid)
	}
	return nil
}

// poolMockClient wires a retryPoolService into a minimal pve.Client.
// All services other than Pools panic so unexpected calls fail the test.
type poolMockClient struct {
	mockClient // re-use nil stubs from task_test.go for other methods
	poolsSvc   *retryPoolService
}

func (c *poolMockClient) Pools() pve.PoolService { return c.poolsSvc }

// assertRetriable is a helper that extracts the *cpierrors.Error from err's
// chain and calls t.Fatal if none is found or if OkToRetry() != want.
func assertRetriable(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	var e *cpierrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("error chain has no *cpierrors.Error: %T %v", err, err)
	}
	if e.OkToRetry() != want {
		t.Errorf("OkToRetry()=%v; want %v. Error: %v", e.OkToRetry(), want, err)
	}
}

// ---------------------------------------------------------------------------
// AssignVMToPool — boundary tests
// ---------------------------------------------------------------------------

func TestAssignVMToPool_Retriable_503(t *testing.T) {
	t.Parallel()
	// A PVE 503 response from the pools endpoint must become retriable because
	// pve.WrapError maps 5xx → TypeRetriableCloud.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				return makeAPIErr(503, "service unavailable")
			},
		},
	}
	err := pve.AssignVMToPool(context.Background(), client, "my-pool", 100)
	assertRetriable(t, err, true)
}

func TestAssignVMToPool_Retriable_ConnectionError(t *testing.T) {
	t.Parallel()
	// A connection-refused error from the pools endpoint must become retriable.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				return &sdkerrors.ConnectionError{
					Host:    "pve.test.local",
					Port:    8006,
					Message: "connection refused",
				}
			},
		},
	}
	err := pve.AssignVMToPool(context.Background(), client, "my-pool", 100)
	assertRetriable(t, err, true)
}

func TestAssignVMToPool_Retriable_TimeoutError(t *testing.T) {
	t.Parallel()
	// An SDK timeout from the pools endpoint must become retriable.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				return &sdkerrors.TimeoutError{
					Operation: "POST /api2/json/pools/my-pool",
					Duration:  "30s",
				}
			},
		},
	}
	err := pve.AssignVMToPool(context.Background(), client, "my-pool", 100)
	assertRetriable(t, err, true)
}

func TestAssignVMToPool_NotRetriable_400(t *testing.T) {
	t.Parallel()
	// A 400 client error from the pools endpoint (e.g. invalid pool name) is
	// a permanent misconfiguration and must NOT be retriable.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				return makeAPIErr(400, "invalid pool name")
			},
		},
	}
	err := pve.AssignVMToPool(context.Background(), client, "bad-pool", 100)
	assertRetriable(t, err, false)
}

func TestAssignVMToPool_NotRetriable_404(t *testing.T) {
	t.Parallel()
	// A 404 from the pools endpoint means the pool does not exist — permanent.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				return makeAPIErr(404, "pool not found")
			},
		},
	}
	err := pve.AssignVMToPool(context.Background(), client, "missing-pool", 100)
	assertRetriable(t, err, false)
}

func TestAssignVMToPool_NilPoolID_ReturnsNil(t *testing.T) {
	t.Parallel()
	// Empty pool ID is a no-op — no API call, no error.
	client := &poolMockClient{
		poolsSvc: &retryPoolService{
			addVMFn: func(_ context.Context, _ string, _ int64) error {
				t.Error("AddVM must not be called when poolID is empty")
				return nil
			},
		},
	}
	if err := pve.AssignVMToPool(context.Background(), client, "", 100); err != nil {
		t.Fatalf("empty pool ID should return nil; got: %v", err)
	}
}

func TestAssignVMToPool_Success_ReturnsNil(t *testing.T) {
	t.Parallel()
	client := &poolMockClient{
		poolsSvc: &retryPoolService{},
	}
	if err := pve.AssignVMToPool(context.Background(), client, "my-pool", 100); err != nil {
		t.Fatalf("expected nil on success; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher boundary — raw-error catch-all behavior
// ---------------------------------------------------------------------------

// TestDispatcherCatchAll_PlainErrorIsNonRetriable verifies the dispatcher's
// catch-all correctly marks a plain fmt.Errorf as non-retriable. This is the
// baseline that justifies routing SDK errors through WrapError before they
// escape to the dispatcher.
func TestDispatcherCatchAll_PlainErrorIsNonRetriable(t *testing.T) {
	t.Parallel()
	// A plain error with no *cpierrors.Error in the chain should hit the
	// catch-all and become a non-retriable CloudError via dispatchError.
	// We verify the classification directly through pve.WrapError since
	// dispatchError is internal; the contract is equivalent.
	raw := errors.New("some transient-looking text but not a cpierrors type")
	wrapped := pve.WrapError(raw) // catch-all: non-retriable
	assertRetriable(t, wrapped, false)
}

// TestWrapError_5xx_IsRetriable verifies that pve.WrapError correctly routes a
// 5xx SDK error to TypeRetriableCloud. Used as an anchor for the AssignVMToPool
// fix: the change ensures SDK errors go through WrapError before the fmt.Errorf
// wrapper so this classification propagates to the dispatcher chain.
func TestWrapError_5xxFromPool_IsRetriable(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(503, "service unavailable")
	wrapped := pve.WrapError(err)
	assertRetriable(t, wrapped, true)
}

// ---------------------------------------------------------------------------
// StorageInfo / storage policy retriability boundary
// ---------------------------------------------------------------------------

// stubPolicyDepsRetriable implements pve.PolicyDeps. storageErr is returned
// verbatim from StorageInfo so tests can inject typed or raw SDK errors and
// observe whether the policy functions preserve or discard the retriable flag.
type stubPolicyDepsRetriable struct {
	storageErr     error
	clusterSize    int
	clusterSizeErr error
}

func (s *stubPolicyDepsRetriable) StorageInfo(_ context.Context, _ string) (pve.StorageInfo, error) {
	if s.storageErr != nil {
		return pve.StorageInfo{}, s.storageErr
	}
	return pve.StorageInfo{Name: "local", Type: "dir"}, nil
}

func (s *stubPolicyDepsRetriable) ClusterNodeCount(_ context.Context) (int, error) {
	if s.clusterSizeErr != nil {
		return 0, s.clusterSizeErr
	}
	return s.clusterSize, nil
}

// TestValidateLightStemcellStorage_503_IsRetriable verifies that a transient
// 503 from StorageInfo (the ListStorage call) propagates as retriable from
// ValidateLightStemcellStorage. Before the fix, StorageInfo returned the raw
// SDK error and the policy function flattened it via cpierrors.Cloud(%s) →
// non-retriable.
func TestValidateLightStemcellStorage_503_IsRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		storageErr:  pve.WrapError(makeAPIErr(503, "service unavailable")),
		clusterSize: 1,
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateLightStemcellStorage_ConnectionError_IsRetriable verifies that a
// connection-refused from StorageInfo propagates as retriable.
func TestValidateLightStemcellStorage_ConnectionError_IsRetriable(t *testing.T) {
	t.Parallel()
	connErr := &sdkerrors.ConnectionError{Host: "pve.test.local", Port: 8006, Message: "connection refused"}
	deps := &stubPolicyDepsRetriable{
		storageErr:  pve.WrapError(connErr),
		clusterSize: 1,
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateLightStemcellStorage_400_IsNotRetriable verifies that a permanent
// 400 from StorageInfo stays non-retriable through the policy path.
func TestValidateLightStemcellStorage_400_IsNotRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		storageErr:  pve.WrapError(makeAPIErr(400, "bad request")),
		clusterSize: 1,
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, false)
}

// TestValidateTemplateCloneStorage_503_IsRetriable verifies retriability through
// ValidateTemplateCloneStorage (the other consumer of StorageInfo).
func TestValidateTemplateCloneStorage_503_IsRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		storageErr:  pve.WrapError(makeAPIErr(503, "service unavailable")),
		clusterSize: 1,
	}
	_, err := pve.ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateTemplateCloneStorage_400_IsNotRetriable verifies permanent 400
// stays non-retriable through ValidateTemplateCloneStorage.
func TestValidateTemplateCloneStorage_400_IsNotRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		storageErr:  pve.WrapError(makeAPIErr(400, "bad request")),
		clusterSize: 1,
	}
	_, err := pve.ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, false)
}

// ---------------------------------------------------------------------------
// ClusterNodeCount path — retriability boundary
// ---------------------------------------------------------------------------
// The stub injects errors directly into ClusterNodeCount (simulating what
// clusterNodeCount returns after pve.WrapError types the SDK error).
// StorageInfo succeeds so the test isolates the cluster-count call.

// TestValidateLightStemcellStorage_ClusterNodeCount_503_IsRetriable verifies
// that a transient 503 from ClusterNodeCount propagates as retriable from
// ValidateLightStemcellStorage. Before the fix, the policy flattened the error
// via cpierrors.Cloud(%s), dropping the retriable flag.
func TestValidateLightStemcellStorage_ClusterNodeCount_503_IsRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		clusterSizeErr: pve.WrapError(makeAPIErr(503, "service unavailable")),
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateLightStemcellStorage_ClusterNodeCount_ConnErr_IsRetriable verifies
// that a connection-refused error from ClusterNodeCount propagates as retriable.
func TestValidateLightStemcellStorage_ClusterNodeCount_ConnErr_IsRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		clusterSizeErr: pve.WrapError(&sdkerrors.ConnectionError{
			Host:    "pve.test.local",
			Port:    8006,
			Message: "connection refused",
		}),
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateLightStemcellStorage_ClusterNodeCount_400_IsNotRetriable verifies
// that a permanent 400 from ClusterNodeCount stays non-retriable.
func TestValidateLightStemcellStorage_ClusterNodeCount_400_IsNotRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		clusterSizeErr: pve.WrapError(makeAPIErr(400, "bad request")),
	}
	_, err := pve.ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, false)
}

// TestValidateTemplateCloneStorage_ClusterNodeCount_503_IsRetriable verifies
// that a transient 503 from ClusterNodeCount propagates as retriable from
// ValidateTemplateCloneStorage.
func TestValidateTemplateCloneStorage_ClusterNodeCount_503_IsRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		clusterSizeErr: pve.WrapError(makeAPIErr(503, "service unavailable")),
	}
	_, err := pve.ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, true)
}

// TestValidateTemplateCloneStorage_ClusterNodeCount_400_IsNotRetriable verifies
// that a permanent 400 from ClusterNodeCount stays non-retriable.
func TestValidateTemplateCloneStorage_ClusterNodeCount_400_IsNotRetriable(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDepsRetriable{
		clusterSizeErr: pve.WrapError(makeAPIErr(400, "bad request")),
	}
	_, err := pve.ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	assertRetriable(t, err, false)
}
