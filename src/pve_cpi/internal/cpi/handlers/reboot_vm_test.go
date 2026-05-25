package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// testDepsReboot builds Deps with the given qemu/nodes/tasks mocks and
// reboot_mode/reboot_timeout from the provided config overrides.
func testDepsReboot(
	qemuSvc *mockQEMUService,
	nodesSvc *mockNodesService,
	tasksSvc *mockTasksService,
	mode string,
	timeout int,
) handlers.Deps {
	cfg := testConfig()
	if mode != "" {
		cfg.RebootMode = mode
	}
	if timeout > 0 {
		cfg.RebootTimeout = timeout
	}
	var ns nodes.Service
	if nodesSvc != nil {
		ns = nodesSvc
	}
	var ts tasks.Service
	if tasksSvc != nil {
		ts = tasksSvc
	}
	if qemuSvc == nil {
		qemuSvc = &mockQEMUService{}
	}
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:  qemuSvc,
			nodesSvc: ns,
			tasksSvc: ts,
		},
		Agent:  &mockAgentService{},
		Logger: log.NewNopLogger(),
	}
}

// rebootRawUPID returns a *nodes.CreateQemuStatusRebootResponse carrying upid.
func rebootRawUPID(upid string) *nodes.CreateQemuStatusRebootResponse {
	b, err := json.Marshal(upid)
	if err != nil {
		panic("rebootRawUPID: " + err.Error())
	}
	raw := nodes.CreateQemuStatusRebootResponse(b)
	return &raw
}

// rebootRawEmpty returns a *nodes.CreateQemuStatusRebootResponse with empty JSON.
func rebootRawEmpty() *nodes.CreateQemuStatusRebootResponse {
	raw := nodes.CreateQemuStatusRebootResponse(json.RawMessage(`""`))
	return &raw
}

// --------------------------------------------------------------------------
// a. Soft happy path: status running; reboot returns UPID; await OK; reset NOT called.
// --------------------------------------------------------------------------

func TestHandleRebootVM_SoftHappy(t *testing.T) {
	t.Parallel()

	resetCalled := false
	rebootCalled := false
	awaitCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, node, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			if node != "pve-node1" || vmid != "101" {
				t.Errorf("CreateQemuStatusReboot: unexpected node=%q vmid=%q", node, vmid)
			}
			if params == nil || params.Timeout == nil || *params.Timeout != 60 {
				t.Errorf("CreateQemuStatusReboot: expected timeout=60, got %v", params)
			}
			rebootCalled = true
			return rebootRawUPID("UPID:node:reboot-task"), nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid != "UPID:node:reboot-task" {
				t.Errorf("Wait: unexpected upid=%q", upid)
			}
			awaitCalled = true
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !rebootCalled {
		t.Error("CreateQemuStatusReboot was not called")
	}
	if !awaitCalled {
		t.Error("Tasks.Wait was not called")
	}
	if resetCalled {
		t.Error("Reset should not have been called on soft happy path")
	}
}

// --------------------------------------------------------------------------
// b. Soft fallback on task failure: reboot UPID returned; await fails; reset called.
// --------------------------------------------------------------------------

func TestHandleRebootVM_SoftFallbackOnTaskFail(t *testing.T) {
	t.Parallel()

	resetCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			return "UPID:node:reset-task", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			return rebootRawUPID("UPID:node:reboot-fail"), nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			if upid == "UPID:node:reboot-fail" {
				return &tasks.Status{ExitStatus: "ERROR: acpi reboot timed out"}, nil
			}
			// reset task UPID
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !resetCalled {
		t.Error("Reset (fallback) was not called after task failure")
	}
}

// --------------------------------------------------------------------------
// c. Soft fallback on reboot-call error: status running; CreateQemuStatusReboot errors; reset called.
// --------------------------------------------------------------------------

func TestHandleRebootVM_SoftFallbackOnRebootCallError(t *testing.T) {
	t.Parallel()

	resetCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			return "UPID:node:reset-task", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			return nil, errors.New("pve: acpi not available")
		},
	}
	tasksSvc := &mockTasksService{}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !resetCalled {
		t.Error("Reset (fallback) was not called after reboot-call error")
	}
}

// --------------------------------------------------------------------------
// d. Hard mode: Config.RebootMode="hard"; status running; reset called; reboot NOT called.
// --------------------------------------------------------------------------

func TestHandleRebootVM_HardMode(t *testing.T) {
	t.Parallel()

	resetCalled := false
	rebootCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			return "UPID:node:reset-task", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			rebootCalled = true
			t.Error("CreateQemuStatusReboot must not be called in hard mode")
			return nil, nil
		},
	}
	tasksSvc := &mockTasksService{}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "hard", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !resetCalled {
		t.Error("Reset was not called in hard mode")
	}
	if rebootCalled {
		t.Error("CreateQemuStatusReboot was unexpectedly called in hard mode")
	}
}

// --------------------------------------------------------------------------
// e. Stopped→start: status stopped; startFn returns UPID; await OK; reboot/reset NOT called.
// --------------------------------------------------------------------------

func TestHandleRebootVM_StoppedStart(t *testing.T) {
	t.Parallel()

	startCalled := false
	resetCalled := false
	rebootCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "stopped"}, nil
		},
		startFn: func(_ context.Context, node string, vmid int) (string, error) {
			if node != "pve-node1" || vmid != 101 {
				t.Errorf("Start: unexpected node=%q vmid=%d", node, vmid)
			}
			startCalled = true
			return "UPID:node:start-task", nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			t.Error("Reset must not be called for stopped VM")
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			rebootCalled = true
			t.Error("CreateQemuStatusReboot must not be called for stopped VM")
			return nil, nil
		},
	}
	tasksSvc := &mockTasksService{}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if !startCalled {
		t.Error("Start was not called for stopped VM")
	}
	if resetCalled {
		t.Error("Reset was unexpectedly called for stopped VM")
	}
	if rebootCalled {
		t.Error("CreateQemuStatusReboot was unexpectedly called for stopped VM")
	}
}

// --------------------------------------------------------------------------
// f. Stopped→start error: status stopped; startFn returns error → CloudError.
// --------------------------------------------------------------------------

func TestHandleRebootVM_StoppedStartError(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "stopped"}, nil
		},
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("pve: start failed: disk error")
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nil, nil, "soft", 60))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from start failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// g. Status 404 → VMNotFound.
// --------------------------------------------------------------------------

func TestHandleRebootVM_StatusNotFound(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, notFoundAPIErr()
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nil, nil, "soft", 60))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected VMNotFound error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("error type = %q; want %q", cpiErr.Type(), cpierrors.TypeVMNotFound)
	}
}

// --------------------------------------------------------------------------
// h. Status generic error → CloudError.
// --------------------------------------------------------------------------

func TestHandleRebootVM_StatusGenericError(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return nil, errors.New("pve: connection refused")
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nil, nil, "soft", 60))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from status failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() == cpierrors.TypeVMNotFound {
		t.Error("generic status error should not be VMNotFound")
	}
}

// --------------------------------------------------------------------------
// i. Reboot 404 → VMNotFound (no fallback to hardReset).
// --------------------------------------------------------------------------

func TestHandleRebootVM_RebootNotFound(t *testing.T) {
	t.Parallel()

	resetCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			t.Error("Reset must not be called when reboot returns 404")
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			return nil, notFoundAPIErr()
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, nil, "soft", 60))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected VMNotFound error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("error type = %q; want %q", cpiErr.Type(), cpierrors.TypeVMNotFound)
	}
	if resetCalled {
		t.Error("Reset was unexpectedly called after 404 reboot (should be VMNotFound, not fallback)")
	}
}

// --------------------------------------------------------------------------
// j. Missing vm_cid (empty args) → CloudError.
// --------------------------------------------------------------------------

func TestHandleRebootVM_MissingVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleRebootVM(testDepsReboot(nil, nil, nil, "soft", 60))
	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing vm_cid")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// k. Invalid vm_cid ("abc") → CloudError.
// --------------------------------------------------------------------------

func TestHandleRebootVM_InvalidVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleRebootVM(testDepsReboot(nil, nil, nil, "soft", 60))
	_, err := h.Handle(context.Background(), marshalArgs("not-a-vmid"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for non-integer vm_cid")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// l. Soft empty UPID: reboot returns empty raw → UPIDFromRaw ""→ no await → success.
// --------------------------------------------------------------------------

func TestHandleRebootVM_SoftEmptyUPID(t *testing.T) {
	t.Parallel()

	awaitCalled := false
	resetCalled := false

	qemuSvc := &mockQEMUService{
		statusFn: func(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
			return map[string]interface{}{"status": "running"}, nil
		},
		resetFn: func(_ context.Context, _ string, _ int) (string, error) {
			resetCalled = true
			t.Error("Reset must not be called when soft reboot returns empty UPID (synchronous success)")
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		createQemuStatusRebootFn: func(_ context.Context, _, _ string, _ *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			return rebootRawEmpty(), nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			awaitCalled = true
			t.Error("Tasks.Wait must not be called when UPID is empty")
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	h := handlers.HandleRebootVM(testDepsReboot(qemuSvc, nodesSvc, tasksSvc, "soft", 60))
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error for empty UPID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if awaitCalled {
		t.Error("Tasks.Wait was unexpectedly called for empty UPID")
	}
	if resetCalled {
		t.Error("Reset was unexpectedly called for empty UPID synchronous reboot")
	}
}
