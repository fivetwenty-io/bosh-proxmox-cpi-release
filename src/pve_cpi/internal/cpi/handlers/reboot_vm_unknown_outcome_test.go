package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// softRebootOutcome runs a soft reboot_vm of a running VM whose reboot call
// returns rebootResp and rebootErr and whose reboot task poll answers with
// waitFn. It returns how many hard resets were sent and the handler's error.
// The hard reset's own task always succeeds.
func softRebootOutcome(
	t *testing.T,
	rebootResp *nodes.CreateQemuStatusRebootResponse,
	rebootErr error,
	waitFn func(upid string) (*tasks.Status, error),
) (int32, error) {
	t.Helper()
	var resets atomic.Int32
	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resets.Add(1)
			return "UPID:pve-node1:reset-task", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(
			_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams,
		) (*nodes.CreateQemuStatusRebootResponse, error) {
			return rebootResp, rebootErr
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == "UPID:pve-node1:reset-task" {
				return &tasks.Status{ExitStatus: "OK"}, nil
			}
			if waitFn == nil {
				t.Errorf("unexpected poll of %q", upid)
				return &tasks.Status{ExitStatus: "OK"}, nil
			}
			return waitFn(upid)
		},
	}
	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft"))
	_, err := h.Handle(fastRetryCtx(context.Background()), marshalArgs("101"), jsonrpc.Context{})
	return resets.Load(), err
}

// requireUnknownOutcome checks that err is a retriable error that says the
// outcome is unknown, tells the operator to run the operation again, and
// contains every string in want. The Director does not retry reboot_vm, so
// the error must not leave that step to it.
func requireUnknownOutcome(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a retriable error for an unknown outcome, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud || !cpiErr.OkToRetry() {
		t.Errorf("error type = %q, ok_to_retry %v; want %q and true", cpiErr.Type(), cpiErr.OkToRetry(),
			cpierrors.TypeRetriableCloud)
	}
	msg := err.Error()
	for _, w := range append([]string{"is unknown", "did not hard-reset", "run the operation again"}, want...) {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q does not contain %q", msg, w)
		}
	}
}

// TestHandleRebootVM_UnresolvedRebootPoll_NoHardReset checks that a graceful
// reboot whose task poll ends without an exit status is never followed by a
// hard reset, because the reboot may already have finished or may still be
// running. That covers a poll that ran out of time, a poll 404 that outlasts
// the registration retries, a poll 403, and a poll fault that ends the poll.
// The handler returns a retriable error that names the task.
func TestHandleRebootVM_UnresolvedRebootPoll_NoHardReset(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve-node1:0002D9FF:24345A8C:6AF5ABD4:qmreboot:101:root@pam::"
	cases := map[string]func(string) (*tasks.Status, error){
		"poll ended with no exit status": func(string) (*tasks.Status, error) {
			return &tasks.Status{Status: "running", ExitStatus: ""}, nil
		},
		"poll window ran out": func(string) (*tasks.Status, error) {
			return nil, fmt.Errorf("task polling canceled: %w", context.DeadlineExceeded)
		},
		"poll 404": func(string) (*tasks.Status, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "no such task"}
		},
		"poll 403": func(string) (*tasks.Status, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"}
		},
		"poll fault": func(string) (*tasks.Status, error) {
			return nil, fmt.Errorf("failed to get task status: %w",
				errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"))
		},
	}
	for name, waitFn := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resets, err := softRebootOutcome(t, rebootRawUPID(upid), nil, waitFn)
			if resets != 0 {
				t.Errorf("hard reset sent %d times after an unresolved reboot poll; want 0", resets)
			}
			requireUnknownOutcome(t, err, upid)
		})
	}
}

// TestHandleRebootVM_FailedRebootTask_HardResets checks that a reboot task
// that resolved with a failing exit status still falls back to a hard reset.
func TestHandleRebootVM_FailedRebootTask_HardResets(t *testing.T) {
	t.Parallel()

	resets, err := softRebootOutcome(t, rebootRawUPID("UPID:pve-node1:reboot"), nil,
		func(string) (*tasks.Status, error) {
			return &tasks.Status{Status: "stopped", ExitStatus: "VM quit/powerdown failed"}, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resets != 1 {
		t.Errorf("hard reset sent %d times after a failed reboot task; want 1", resets)
	}
}

// TestHandleRebootVM_RebootTaskLockTimeout_NoHardReset checks that a reboot
// task that failed because it timed out waiting for the VM's lock gives no
// hard reset. Another task held the lock, such as an earlier reboot that is
// still shutting the guest down, so a reset could power-cycle the guest twice.
func TestHandleRebootVM_RebootTaskLockTimeout_NoHardReset(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve-node1:0002DA00:24345A8D:6AF5ABD5:qmreboot:101:root@pam::"
	resets, err := softRebootOutcome(t, rebootRawUPID(upid), nil, func(string) (*tasks.Status, error) {
		return &tasks.Status{Status: "stopped",
			ExitStatus: "can't lock file '/var/lock/qemu-server/lock-101.conf' - got timeout"}, nil
	})
	if resets != 0 {
		t.Errorf("hard reset sent %d times after a lock timeout; want 0", resets)
	}
	requireUnknownOutcome(t, err, upid, "lock, which another task held")
}

// TestHandleRebootVM_RebootCallOutcome checks the reboot call's own error. An
// error that may have reached PVE gives no hard reset and a retriable error,
// and an error that PVE answered, or that proves nothing was sent, still
// falls back to a hard reset.
func TestHandleRebootVM_RebootCallOutcome(t *testing.T) {
	t.Parallel()

	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	cases := []struct {
		name      string
		err       error
		wantReset bool
		want      string
	}{
		{
			name: "maybe sent after the connection",
			err:  &endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w", read)},
		},
		{
			name: "untagged transport fault",
			err:  fmt.Errorf("Post: %w", read),
		},
		{
			name: "deadline while waiting for the answer",
			err:  fmt.Errorf("Post: %w", context.DeadlineExceeded),
		},
		{
			name: "relay 596 after the request may have reached the node",
			err:  &sdkerrors.APIError{HTTPCode: 596, Message: "broken pipe"},
		},
		{
			name: "answered lock timeout",
			err: &sdkerrors.APIError{HTTPCode: 500,
				Message: "can't lock file '/var/lock/qemu-server/lock-101.conf' - got timeout"},
			want: "lock, which another task held",
		},
		{
			name:      "dial failed without the pool, never sent",
			err:       dial,
			wantReset: true,
		},
		{
			name:      "answered refusal",
			err:       &sdkerrors.APIError{HTTPCode: 500, Message: "acpi not available"},
			wantReset: true,
		},
		{
			name:      "answered 595 from the entry node",
			err:       &sdkerrors.APIError{HTTPCode: 595, Message: "Connection refused"},
			wantReset: true,
		},
		{
			name: "connect phase, never sent",
			err: fmt.Errorf("Post: %w", &endpointpool.ConnectPhaseError{
				Phase: endpointpool.PhaseDial, Endpoint: "pve1:8006", Err: dial,
			}),
			wantReset: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resets, err := softRebootOutcome(t, nil, tc.err, nil)
			if !tc.wantReset {
				if resets != 0 {
					t.Errorf("hard reset sent %d times; want 0", resets)
				}
				want := tc.want
				if want == "" {
					want = "may have reached PVE"
				}
				requireUnknownOutcome(t, err, want)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resets != 1 {
				t.Errorf("hard reset sent %d times; want 1", resets)
			}
		})
	}
}

// TestHandleRebootVM_UnreadableRebootUPID_NoHardReset checks that an accepted
// reboot whose task ID cannot be read gives no hard reset, because PVE may
// have started the reboot task.
func TestHandleRebootVM_UnreadableRebootUPID_NoHardReset(t *testing.T) {
	t.Parallel()

	raw := nodes.CreateQemuStatusRebootResponse(`42`)
	resets, err := softRebootOutcome(t, &raw, nil, nil)
	if resets != 0 {
		t.Errorf("hard reset sent %d times after an unreadable UPID; want 0", resets)
	}
	requireUnknownOutcome(t, err, "task ID could not be read")
}

// TestHandleRebootVM_HardResetMaybeSent_NotResent checks that a hard reset
// whose request may have reached PVE is never sent a second time, because a
// reset that landed and was sent again power-cycles the guest twice. The
// handler returns a retriable error that says the outcome is unknown.
func TestHandleRebootVM_HardResetMaybeSent_NotResent(t *testing.T) {
	t.Parallel()

	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	cases := map[string]error{
		"maybe sent after the connection": &endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w", read)},
		"relay 596":                       &sdkerrors.APIError{HTTPCode: 596, Message: "broken pipe"},
		"sdk timeout":                     &sdkerrors.TimeoutError{Operation: "POST", Duration: "30s"},
	}
	for name, resetErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var resets atomic.Int32
			qemuSvc := &mockQEMUService{
				statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
					return map[string]any{"status": "running"}, nil
				},
				resetFn: func(_ context.Context, _ string, _ int) (string, error) {
					resets.Add(1)
					return "", resetErr
				},
			}
			h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nil, nil, "hard"))
			_, err := h.Handle(fastRetryCtx(context.Background()), marshalArgs("101"), jsonrpc.Context{})
			if n := resets.Load(); n != 1 {
				t.Errorf("hard reset sent %d times; want 1", n)
			}
			if err == nil {
				t.Fatal("expected a retriable error for an unknown reset outcome, got nil")
			}
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) || cpiErr.Type() != cpierrors.TypeRetriableCloud {
				t.Errorf("error %T %v; want a %q error", err, err, cpierrors.TypeRetriableCloud)
			}
			for _, w := range []string{"is unknown", "did not send it again", "run the operation again"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err.Error(), w)
				}
			}
		})
	}
}
