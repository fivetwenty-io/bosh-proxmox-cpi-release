package pve_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// captureWaitOpts runs one AwaitTask against a mock that records the
// WaitOptions the SDK received, and returns them.
//
// NOTE: these tests mutate process-wide poll defaults and therefore must NOT
// run in parallel with each other or with other AwaitTask tests; they always
// restore the previous values via the SetTaskPollingForTest restore func.
func captureWaitOpts(t *testing.T) *sdktasks.WaitOptions {
	t.Helper()
	var captured *sdktasks.WaitOptions
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			captured = opts
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil {
		t.Fatal("WaitOptions was nil — opts not passed to SDK")
	}
	return captured
}

// TestTaskPollDefaults_PreserveShippedConstants verifies that without any
// configuration the poll cadence matches the constants the CPI shipped with:
// 2000ms interval, 10000ms max interval, 10% jitter.
func TestTaskPollDefaults_PreserveShippedConstants(t *testing.T) {
	// Establish the known-good baseline and restore it on exit, matching the
	// pattern used by the three sibling tests.  Without this a prior test that
	// mutates the process-wide atomics and fails to restore them (e.g. due to
	// a panic) would leave stale values that cause this assertion to fail
	// spuriously — even though the shipped defaults are correct.
	restore := pve.SetTaskPollingForTest(2000, 10000, 10)
	defer restore()

	opts := captureWaitOpts(t)
	if opts.IntervalMillis != 2000 {
		t.Errorf("default IntervalMillis = %d, want 2000", opts.IntervalMillis)
	}
	if opts.MaxIntervalMillis != 10000 {
		t.Errorf("default MaxIntervalMillis = %d, want 10000", opts.MaxIntervalMillis)
	}
	if opts.JitterPct != 10 {
		t.Errorf("default JitterPct = %d, want 10", opts.JitterPct)
	}
}

// TestConfigureTaskPolling_AppliesOverrides verifies operator values reach the
// SDK WaitOptions and that the previous values are restored afterward.
func TestConfigureTaskPolling_AppliesOverrides(t *testing.T) {
	restore := pve.SetTaskPollingForTest(2000, 10000, 10) // known baseline + restore
	defer restore()

	pve.ConfigureTaskPolling(5000, 60000, 25)
	opts := captureWaitOpts(t)
	if opts.IntervalMillis != 5000 {
		t.Errorf("IntervalMillis = %d, want 5000", opts.IntervalMillis)
	}
	if opts.MaxIntervalMillis != 60000 {
		t.Errorf("MaxIntervalMillis = %d, want 60000", opts.MaxIntervalMillis)
	}
	if opts.JitterPct != 25 {
		t.Errorf("JitterPct = %d, want 25", opts.JitterPct)
	}
}

// TestConfigureTaskPolling_PartialKeepsDefaults verifies that a zero argument
// leaves that field at its existing value (only > 0 values are applied).
func TestConfigureTaskPolling_PartialKeepsDefaults(t *testing.T) {
	restore := pve.SetTaskPollingForTest(2000, 10000, 10)
	defer restore()

	pve.ConfigureTaskPolling(0, 0, 30) // only jitter set
	opts := captureWaitOpts(t)
	if opts.IntervalMillis != 2000 {
		t.Errorf("IntervalMillis = %d, want unchanged 2000", opts.IntervalMillis)
	}
	if opts.JitterPct != 30 {
		t.Errorf("JitterPct = %d, want 30", opts.JitterPct)
	}
}

// TestConfigureAdaptiveTaskPoll_TogglesAwaitTaskRouting verifies that
// ConfigureAdaptiveTaskPoll(true) routes AwaitTask through the adaptive
// GetStatus loop (not the SDK's fixed-interval Wait), and that
// ConfigureAdaptiveTaskPoll(false) restores the prior fixed-interval routing.
// Process-wide state, so restore via SetAdaptiveTaskPollForTest like every
// other test in this file that touches package atomics.
func TestConfigureAdaptiveTaskPoll_TogglesAwaitTaskRouting(t *testing.T) {
	restore := pve.SetAdaptiveTaskPollForTest(false) // known baseline
	defer restore()

	pve.ConfigureAdaptiveTaskPoll(true)
	waitCalled := false
	getStatusCalled := false
	svc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			waitCalled = true
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
		getStatusFn: func(_ context.Context, _, upid string) (*sdktasks.Status, error) {
			getStatusCalled = true
			return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
		},
	}
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if waitCalled {
		t.Error("ConfigureAdaptiveTaskPoll(true): SDK Wait must NOT be called")
	}
	if !getStatusCalled {
		t.Error("ConfigureAdaptiveTaskPoll(true): GetStatus must be called (adaptive loop)")
	}

	pve.ConfigureAdaptiveTaskPoll(false)
	waitCalled, getStatusCalled = false, false
	if err := pve.AwaitTask(context.Background(), newMockClient(svc), "node1", "UPID:node1:abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !waitCalled {
		t.Error("ConfigureAdaptiveTaskPoll(false): SDK Wait must be called (fixed-interval routing restored)")
	}
	if getStatusCalled {
		t.Error("ConfigureAdaptiveTaskPoll(false): GetStatus must NOT be called")
	}
}

// TestConfigureTaskPolling_MaxClampedToInterval verifies the max interval is
// clamped up to the base interval so the SDK never sees max < base.
func TestConfigureTaskPolling_MaxClampedToInterval(t *testing.T) {
	restore := pve.SetTaskPollingForTest(2000, 10000, 10)
	defer restore()

	pve.ConfigureTaskPolling(8000, 1000, 0) // max (1000) < interval (8000)
	opts := captureWaitOpts(t)
	if opts.IntervalMillis != 8000 {
		t.Errorf("IntervalMillis = %d, want 8000", opts.IntervalMillis)
	}
	if opts.MaxIntervalMillis < opts.IntervalMillis {
		t.Errorf("MaxIntervalMillis %d must be >= IntervalMillis %d",
			opts.MaxIntervalMillis, opts.IntervalMillis)
	}
}
