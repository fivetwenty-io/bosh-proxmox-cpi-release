package pve_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---- mock task service ----

type mockTasksService struct {
	waitFn      func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
	getStatusFn func(ctx context.Context, node, upid string) (*sdktasks.Status, error)
}

func (m *mockTasksService) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if m.waitFn != nil {
		return m.waitFn(ctx, node, upid, opts)
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

func (m *mockTasksService) WaitForUPID(ctx context.Context, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	panic("mockTasksService.WaitForUPID: not expected in pve tests")
}

func (m *mockTasksService) GetStatus(ctx context.Context, node, upid string) (*sdktasks.Status, error) {
	if m.getStatusFn != nil {
		return m.getStatusFn(ctx, node, upid)
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// ---- mock client ----

// mockClient is shared across task_test.go and vmid_test.go (both in package pve_test).
type mockClient struct {
	tasksSvc   sdktasks.Service
	clusterSvc sdkcluster.Service
	nodesSvc   sdknodes.Service
}

func (m *mockClient) QEMU() sdkqemu.Service                     { return nil }
func (m *mockClient) Storage() sdkstorage.Service               { return nil }
func (m *mockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (m *mockClient) Tasks() sdktasks.Service                   { return m.tasksSvc }
func (m *mockClient) Nodes() sdknodes.Service                   { return m.nodesSvc }
func (m *mockClient) Cluster() sdkcluster.Service               { return m.clusterSvc }
func (m *mockClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (m *mockClient) Pools() pve.PoolService                    { return nil }

func newMockClient(tasksSvc sdktasks.Service) *mockClient {
	return &mockClient{tasksSvc: tasksSvc}
}

// ---- tests ----

// TestAwaitTask_Adaptive_PollsThenSucceeds verifies that with adaptive polling
// enabled, AwaitTask drives its own loop via GetStatus (not the SDK Wait),
// tolerates a running task with progress, and succeeds on the terminal read.
func TestAwaitTask_Adaptive_PollsThenSucceeds(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	calls := 0
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			t.Error("SDK Wait must NOT be called when adaptive polling is enabled")
			return nil, nil
		},
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			calls++
			if calls < 3 {
				return &sdktasks.Status{Status: "running", UpID: upid, Progress: 0.4 * float64(calls)}, nil
			}
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	// Instant polling via the test backoff seam.
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:clone"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected adaptive loop to poll until terminal, got %d GetStatus calls", calls)
	}
}

// TestAwaitTask_Adaptive_TerminalFailure verifies a non-OK terminal exit is a
// non-retriable failure in the adaptive loop.
func TestAwaitTask_Adaptive_TerminalFailure(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "clone failed", UpID: upid}, nil
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:clone")
	if err == nil {
		t.Fatal("expected error for non-OK terminal exit")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("terminal task failure must be a non-retriable CloudError, got: %v", err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("terminal task failure must NOT be retriable, got: %v", err)
	}
}

// TestAwaitTask_Adaptive_DisabledUsesWait confirms the default (disabled) path
// uses the SDK Wait and never calls GetStatus — byte-identical routing.
func TestAwaitTask_Adaptive_DisabledUsesWait(t *testing.T) {
	t.Parallel() // adaptive flag defaults false; no global mutation here

	waitCalled := false
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			waitCalled = true
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
		getStatusFn: func(_ context.Context, _, _ string) (*sdktasks.Status, error) {
			t.Error("GetStatus must NOT be called when adaptive polling is disabled")
			return nil, nil
		},
	}
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waitCalled {
		t.Error("disabled path must use SDK Wait")
	}
}

// TestAwaitTask_Adaptive_TransientErrorThenSucceeds verifies the adaptive loop
// tolerates transient GetStatus errors (falls through and retries) rather than
// failing, then succeeds on the terminal read.
func TestAwaitTask_Adaptive_TransientErrorThenSucceeds(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	calls := 0
	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			calls++
			if calls < 3 {
				// Use a genuine transient shape so the loop retries it correctly.
				// ConnectionError is matched by IsTransientTransport.
				return nil, &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "transient read blip"}
			}
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:x"); err != nil {
		t.Fatalf("transient errors must be retried, got: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected retries past transient errors, got %d calls", calls)
	}
}

// TestAwaitTask_Adaptive_NotFoundIsNonRetriable verifies a not-found task UPID is
// classified non-retriable (preserves IsNotFound) in the adaptive loop.
func TestAwaitTask_Adaptive_NotFoundIsNonRetriable(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, _ string) (*sdktasks.Status, error) {
			return nil, sdkerrors.ErrNotFound
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:gone")
	if err == nil {
		t.Fatal("expected error for not-found task")
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("not-found must be non-retriable, got: %v", err)
	}
	if !pve.IsNotFound(err) {
		t.Errorf("not-found classification must be preserved, got: %v", err)
	}
}

// TestAwaitTask_Adaptive_ContextCancelledIsRetriable verifies parent-context
// cancellation surfaces as a retriable error.
func TestAwaitTask_Adaptive_ContextCancelledIsRetriable(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "running", UpID: upid, Progress: 0.5}, nil
		},
	}
	ctx, cancel := context.WithCancel(pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 }))
	cancel() // cancel before the call so the running-task select takes the ctx.Done arm
	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:slow")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("context cancellation must be retriable, got: %v", err)
	}
}

// TestAwaitTask_Adaptive_PermanentErrorIsNonRetriable verifies that a permanent
// 4xx-style error from GetStatus is returned immediately as non-retriable, not
// silently retried until the adaptive deadline fires a retriable timeout.
func TestAwaitTask_Adaptive_PermanentErrorIsNonRetriable(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	calls := 0
	// A 403-style permanent error: not transient-transport, not not-found.
	permErr := &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied", Code: 403}
	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, _ string) (*sdktasks.Status, error) {
			calls++
			return nil, permErr
		},
	}
	// WithMaxWait(1s) bounds the test even before the fix: the bug causes the loop
	// to spin until actx fires (returning retriable timeout); the fix exits on the
	// first call. Either way the test completes in under 2 seconds.
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:perm",
		pve.WithMaxWait(1*time.Second))
	if err == nil {
		t.Fatal("expected error for permanent GetStatus failure, got nil")
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("permanent 403 must be non-retriable; got retriable error: %v", err)
	}
	if calls > 2 {
		t.Errorf("permanent error must exit immediately, not loop; got %d GetStatus calls", calls)
	}
}

// TestAwaitTask_Adaptive_TransientErrorStillRetries verifies that a transient
// transport error (5xx / connection) keeps the current retry-until-deadline
// behaviour and does not short-circuit to an early return.
func TestAwaitTask_Adaptive_TransientErrorStillRetries(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	calls := 0
	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			calls++
			if calls < 4 {
				// Transient shape: ConnectionError is detected by IsTransientTransport.
				return nil, &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "EOF"}
			}
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:transient"); err != nil {
		t.Fatalf("transient 5xx must be retried until success; got: %v", err)
	}
	if calls < 4 {
		t.Errorf("expected at least 4 calls (3 transient + 1 success); got %d", calls)
	}
}

func TestAwaitTask_Success(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestAwaitTask_PollsNodeEmbeddedInUPID(t *testing.T) {
	t.Parallel()
	// pveproxy may run a proxied request on the node handling the API
	// connection instead of the node addressed in the URL (seen with storage
	// uploads to a shared storage in a multi-node cluster), so the UPID's
	// embedded node — not the caller's — must be used for status polling:
	// the status endpoint rejects a UPID whose node differs from the URL's.
	var polledNode string
	svc := &mockTasksService{
		waitFn: func(_ context.Context, node, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			polledNode = node
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	upid := "UPID:node0:000671CC:00AA0359:6A5C1330:imgcopy::user@pve!t:"
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node2", upid); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if polledNode != "node0" {
		t.Fatalf("expected poll against UPID-embedded node %q, got %q", "node0", polledNode)
	}
}

func TestAwaitTask_KeepsCallerNodeWhenUPIDUnparseable(t *testing.T) {
	t.Parallel()
	var polledNode string
	svc := &mockTasksService{
		waitFn: func(_ context.Context, node, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			polledNode = node
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node2", "not-a-upid"); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if polledNode != "node2" {
		t.Fatalf("expected caller node %q preserved, got %q", "node2", polledNode)
	}
}

func TestAwaitTask_EmptyExitStatusReturnsRetriableError(t *testing.T) {
	t.Parallel()
	// Empty exit status means the SDK's TimeoutSeconds elapsed before PVE wrote
	// a terminal ExitStatus. This is a transient timeout — the PVE task is still
	// running. Return a retriable error so the director re-issues the action
	// rather than holding a queue slot forever.
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "", UpID: upid}, nil
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for empty exit status, got nil")
	}
	if !strings.Contains(err.Error(), "empty exit status") {
		t.Errorf("error message should mention empty exit status, got: %v", err)
	}
	// The empty-exit-status case must be retriable: it is a polling timeout,
	// not a permanent task failure. The director must re-issue the action.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("empty exit status error must be retriable (OkToRetry=true), got false; err=%v", err)
	}
}

func TestAwaitTask_Failure(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			// SDK returns the status AND the error for non-OK exit.
			return &sdktasks.Status{Status: "stopped", ExitStatus: "ERROR: disk full", UpID: upid},
				fmt.Errorf("task failed: ERROR: disk full")
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for failed task, got nil")
	}
}

func TestAwaitTask_NonOKExitStatusNoSDKError(t *testing.T) {
	t.Parallel()
	// Edge: SDK returns non-OK exit but no error (defensive path in AwaitTask).
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "FAILED", UpID: upid}, nil
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for FAILED exit status, got nil")
	}
}

func TestAwaitTask_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	svc := &mockTasksService{
		waitFn: func(ctx context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			// Simulate SDK respecting ctx cancellation.
			cancel()
			<-ctx.Done()
			return nil, context.DeadlineExceeded
		},
	}

	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	// DEC-4: ctx-cancel must be retriable so the director re-issues the action.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("ctx-cancel error must be retriable (OkToRetry=true), got false; err=%v", err)
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("ctx-cancel error type = %q, want %q", cpiErr.Type(), cpierrors.TypeRetriableCloud)
	}
}

// TestAwaitTask_ContextDeadline checks that a context.DeadlineExceeded
// (distinct from Canceled) also surfaces as retriable.
func TestAwaitTask_ContextDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	svc := &mockTasksService{
		waitFn: func(ctx context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			<-ctx.Done()
			return nil, context.DeadlineExceeded
		},
	}

	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for deadline-exceeded context, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("deadline-exceeded error must be retriable, got OkToRetry=false; err=%v", err)
	}
}

// TestAwaitTask_PermanentTaskFailure verifies that a non-OK exit status is NOT retriable.
func TestAwaitTask_PermanentTaskFailure(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "ERROR: disk not found", UpID: upid},
				fmt.Errorf("task failed: ERROR: disk not found")
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for permanent task failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	// Permanent task failure must NOT be retriable — retrying with the same
	// args won't fix a disk-not-found or similar terminal condition.
	if cpiErr.OkToRetry() {
		t.Errorf("permanent task failure must NOT be retriable, got OkToRetry=true; err=%v", err)
	}
}

// fakeNetError is a net.Error whose Timeout() returns true.
type fakeNetTimeoutError struct{ msg string }

func (e *fakeNetTimeoutError) Error() string   { return e.msg }
func (e *fakeNetTimeoutError) Timeout() bool   { return true }
func (e *fakeNetTimeoutError) Temporary() bool { return true }

// Verify fakeNetTimeoutError satisfies net.Error.
var _ net.Error = (*fakeNetTimeoutError)(nil)

// TestAwaitTask_TransientPollError_SDKConnection verifies that a ConnectionError
// during polling surfaces as retriable.
func TestAwaitTask_TransientPollError_SDKConnection(t *testing.T) {
	t.Parallel()
	connErr := &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection refused"}
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, connErr
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("SDK ConnectionError poll fault must be retriable, got OkToRetry=false; err=%v", err)
	}
}

// TestAwaitTask_TransientPollError_NetTimeout verifies that a net timeout
// during polling surfaces as retriable.
func TestAwaitTask_TransientPollError_NetTimeout(t *testing.T) {
	t.Parallel()
	netErr := &fakeNetTimeoutError{msg: "i/o timeout"}
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, netErr
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("net timeout poll fault must be retriable, got OkToRetry=false; err=%v", err)
	}
}

// TestAwaitTask_SDKNetworkError_NonRetriable verifies that a plain (non-timeout,
// non-connection) SDK error during polling is NOT retriable.
func TestAwaitTask_SDKNetworkError_NonRetriable(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, errors.New("unexpected internal error")
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("plain SDK error must NOT be retriable, got OkToRetry=true; err=%v", err)
	}
}

func TestAwaitTask_PollIntervalOption(t *testing.T) {
	t.Parallel()
	var capturedOpts *sdktasks.WaitOptions
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			capturedOpts = opts
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}

	interval := 500 * time.Millisecond
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc",
		pve.WithPollIntervalForTest(interval))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedOpts == nil {
		t.Fatal("WaitOptions was nil — opts not passed to SDK")
	}
	if capturedOpts.IntervalMillis != 500 {
		t.Errorf("expected IntervalMillis 500, got %d", capturedOpts.IntervalMillis)
	}
}

func TestAwaitTask_MaxWaitOption(t *testing.T) {
	t.Parallel()
	var capturedOpts *sdktasks.WaitOptions
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			capturedOpts = opts
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}

	maxWait := 60 * time.Second
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc",
		pve.WithMaxWait(maxWait))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedOpts.TimeoutSeconds != 60 {
		t.Errorf("expected TimeoutSeconds 60, got %d", capturedOpts.TimeoutSeconds)
	}
}

func TestAwaitTask_EmptyUPID(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "")
	if err == nil {
		t.Fatal("expected error for empty UPID, got nil")
	}
}

func TestAwaitTask_EmptyNode(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for empty node, got nil")
	}
}

func TestAwaitTask_NilContext(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{}
	//nolint:staticcheck // intentional nil ctx for validation test
	//lint:ignore SA1012 intentional nil ctx for validation test
	err := pve.AwaitTask(nil, newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

func TestAwaitTask_NilClient(t *testing.T) {
	t.Parallel()
	err := pve.AwaitTask(context.Background(), nil, "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

func TestAwaitTask_SDKNetworkError(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, errors.New("connection refused")
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc")
	if err == nil {
		t.Fatal("expected error for SDK network failure, got nil")
	}
}

func TestAwaitTaskWithLogger_Success(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	// nil logger path
	err := pve.AwaitTaskWithLogger(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwaitTaskWithLogger_SuccessWithLogger(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	err := pve.AwaitTaskWithLogger(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc", logger(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwaitTaskWithLogger_Failure(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "FAILED", UpID: upid},
				fmt.Errorf("task failed: FAILED")
		},
	}
	// nil logger path on failure
	err := pve.AwaitTaskWithLogger(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestAwaitTaskWithLogger_FailureWithLogger(t *testing.T) {
	t.Parallel()
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: "FAILED", UpID: upid},
				fmt.Errorf("task failed: FAILED")
		},
	}
	err := pve.AwaitTaskWithLogger(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc", logger(t))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestAwaitTask_WithPollIntervalForTest_Zero(t *testing.T) {
	t.Parallel()
	// Zero duration opt is ignored; default should be used.
	var capturedOpts *sdktasks.WaitOptions
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			capturedOpts = opts
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc",
		pve.WithPollIntervalForTest(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// default is 2000 ms
	if capturedOpts.IntervalMillis != 2000 {
		t.Errorf("expected default IntervalMillis 2000 for zero opt, got %d", capturedOpts.IntervalMillis)
	}
}
