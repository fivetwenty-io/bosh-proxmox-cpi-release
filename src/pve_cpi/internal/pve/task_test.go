package pve_test

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---- mock task service ----

type mockTasksService struct {
	waitFn func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
}

func (m *mockTasksService) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if m.waitFn != nil {
		return m.waitFn(ctx, node, upid, opts)
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

func TestAwaitTask_EmptyExitStatusReturnsError(t *testing.T) {
	t.Parallel()
	// Empty exit status means PVE never wrote a terminal ExitStatus — the task
	// poller returned before the task completed or was killed without recording
	// an outcome. AwaitTask must treat this as an error, not as success.
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
		pve.WithPollInterval(interval))
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

func TestAwaitTask_WithPollInterval_Zero(t *testing.T) {
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
		pve.WithPollInterval(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// default is 2000 ms
	if capturedOpts.IntervalMillis != 2000 {
		t.Errorf("expected default IntervalMillis 2000 for zero opt, got %d", capturedOpts.IntervalMillis)
	}
}
