package pve_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cfsLockExit is a real PVE task exit status for storage-lock contention: the
// task failed because the lock did not come free in time, which is pure
// contention and must classify retriable on every poll path.
const cfsLockExit = "cfs-lock 'storage-vmdata' error: got lock request timeout"

// permanentExit is a real PVE task exit status for a genuinely permanent
// failure that no retry can change.
const permanentExit = "can't open file '/etc/pve/nodes/pve1/qemu-server/900.conf' - No such file or directory"

// TestAwaitTask_Adaptive_LockContentionExitIsRetriable is the classification
// parity test for the adaptive poll path: a terminal task exit whose text is
// storage-lock contention must classify retriable, exactly as the default
// SDK-poller path classifies the same exit text. Before the fix,
// classifyTaskExit hard-coded every non-OK exit to a permanent CloudError, so
// enabling pve.task_poll.adaptive silently flipped every lock-contention task
// failure to non-retriable; this test fails on that code.
func TestAwaitTask_Adaptive_LockContentionExitIsRetriable(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: cfsLockExit, UpID: upid}, nil
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })

	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:clone")
	if err == nil {
		t.Fatal("expected an error for a failed task exit")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("adaptive path classified lock-contention exit as %v, want retriable (parity with the default path)", err)
	}
}

// TestAwaitTask_Default_LockContentionExitIsRetriable pins the default
// SDK-poller path's classification of the same exit text, using the exact
// error shape the SDK's Wait returns for a failed task ("task failed:
// <exitstatus>" alongside the terminal status object). The two paths must
// agree.
func TestAwaitTask_Default_LockContentionExitIsRetriable(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(false)()

	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			status := &sdktasks.Status{Status: "stopped", ExitStatus: cfsLockExit, UpID: upid}
			return status, fmt.Errorf("task failed: %s", cfsLockExit)
		},
	}

	err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:clone")
	if err == nil {
		t.Fatal("expected an error for a failed task exit")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("default path classified lock-contention exit as %v, want retriable", err)
	}
}

// TestAwaitTask_Adaptive_PermanentExitStaysPermanent guards against
// over-correction: a genuinely permanent task exit must stay a non-retriable
// CloudError on the adaptive path after the parity fix.
func TestAwaitTask_Adaptive_PermanentExitStaysPermanent(t *testing.T) {
	defer pve.SetAdaptiveTaskPollForTest(true)()

	svc := &mockTasksService{
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			return &sdktasks.Status{Status: "stopped", ExitStatus: permanentExit, UpID: upid}, nil
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })

	err := pve.AwaitTask(ctx, newMockClient(svc), "node1", "UPID:node1:destroy")
	if err == nil {
		t.Fatal("expected an error for a failed task exit")
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("adaptive path classified a permanent exit as retriable: %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("adaptive path = %v, want TypeCloud", err)
	}
}
