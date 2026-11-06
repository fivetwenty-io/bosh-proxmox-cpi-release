package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// Deleting an HA-managed VM stops it through PVE HA before the destroy. The
// HA resource stays configured through the stop, so the CRM never sees an
// unmanaged guest it could restart or move, and the destroy removes the
// resource with purge. These tests pin that order and the wait-and-retry
// recovery on the running-destroy failure.

func dlbEnabledConfig() *config.CPIConfig {
	cfg := *testConfig()
	cfg.HA = haDLBEnabled()
	return &cfg
}

func TestHandleDeleteVM_HAManagedVMStopsThroughHABeforeTheDestroy(t *testing.T) {
	t.Parallel()

	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	var purged bool
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			scene.Do(func(s *handlers.HADeleteScene) { s.Events = append(s.Events, "DeleteQemu") })
			purged = params != nil && params.Purge != nil && *params.Purge
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error { return nil },
	}

	deps := testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	deps.Config = dlbEnabledConfig()
	deps.PVE = &handlers.HAScenePermsClient{Scene: scene, Client: &mockPVEClient{
		qemuSvc:    &handlers.HASceneQEMU{Service: qemuSvc, Scene: scene},
		nodesSvc:   &handlers.HASceneNodes{Service: nodesSvc, Scene: scene},
		tasksSvc:   tasksSvc,
		storageSvc: &mockStorageService{},
		clusterSvc: &handlers.HASceneCluster{Service: defaultClusterSvc(101, "pve-node1"), Scene: scene},
		poolsSvc:   &noopPoolService{},
	}}

	h := handlers.HandleDeleteVM(deps)
	if _, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := scene.EventsSnapshot()
	stopIdx, destroyIdx := -1, -1
	for i, ev := range events {
		switch {
		case ev == "UpdateHaResources:vm:101=stopped" && stopIdx == -1:
			stopIdx = i
		case ev == "DeleteQemu" && destroyIdx == -1:
			destroyIdx = i
		}
	}
	if stopIdx == -1 || destroyIdx == -1 || stopIdx > destroyIdx {
		t.Fatalf("the HA-managed VM must be stopped through PVE HA before the destroy; events: %v", events)
	}
	for _, ev := range events[:destroyIdx] {
		if strings.HasPrefix(ev, "DeleteHaResources:") {
			t.Fatalf("the HA resource was removed before the destroy, so the CRM could act on an unmanaged guest; "+
				"events: %v", events)
		}
	}
	if !purged {
		t.Fatal("the destroy must purge, which removes the HA resource")
	}
}

// runningDestroyDeps builds deps where the first destroy fails with PVE's
// "is running" refusal, the VM reports the given status sequence, and the
// destroy succeeds afterwards.
func runningDestroyDeps(t *testing.T, statuses []string, deleteCalls *int, statusCalls *int) handlers.Deps {
	t.Helper()
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			idx := *statusCalls
			*statusCalls++
			if idx >= len(statuses) {
				idx = len(statuses) - 1
			}
			return map[string]any{"status": statuses[idx]}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			*deleteCalls++
			if *deleteCalls == 1 {
				return nil, errors.New("API request failed: VM 101 is running - destroy failed")
			}
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error { return nil },
	}
	return testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc)
}

func TestHandleDeleteVM_RunningDestroy_WaitsForStopThenRetries(t *testing.T) {
	defer handlers.SetDeleteStopPollInterval(time.Millisecond)()

	var deleteCalls, statusCalls int
	deps := runningDestroyDeps(t, []string{"running", "stopped"}, &deleteCalls, &statusCalls)

	h := handlers.HandleDeleteVM(deps)
	result, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if deleteCalls != 2 {
		t.Errorf("DeleteQemu: want 2 calls (running-refusal then retry), got %d", deleteCalls)
	}
	if statusCalls < 2 {
		t.Errorf("Status: want at least 2 polls (running then stopped), got %d", statusCalls)
	}
}

func TestHandleDeleteVM_RunningDestroy_WaitBudgetExhausted_Retriable(t *testing.T) {
	defer handlers.SetDeleteStopPollInterval(time.Millisecond)()
	defer handlers.SetDeleteStopWaitBudget(5 * time.Millisecond)()

	var deleteCalls, statusCalls int
	deps := runningDestroyDeps(t, []string{"running"}, &deleteCalls, &statusCalls)

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected an error when the VM never stops")
	}
	if !strings.Contains(err.Error(), "still") {
		t.Errorf("error should name the stuck state for the operator; got: %v", err)
	}
	if deleteCalls != 1 {
		t.Errorf("DeleteQemu: want exactly 1 call (no blind retry against a running VM), got %d", deleteCalls)
	}
}

// The happy path must not issue any status polls — the wait only runs on the
// running-destroy refusal. Guarded by the mock's panic-on-unconfigured Status
// in every pre-existing delete test, but pinned explicitly here.
func TestHandleDeleteVM_CleanDestroy_NoStatusPolls(t *testing.T) {
	t.Parallel()

	var statusCalls int
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
		statusFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			statusCalls++
			return map[string]any{"status": "stopped"}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{
		removeFn: func(_ context.Context, _ string, _ int) error { return nil },
	}

	h := handlers.HandleDeleteVM(testDepsFoundVM(101, qemuSvc, nodesSvc, tasksSvc, agentSvc))
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusCalls != 0 {
		t.Errorf("Status: want 0 polls on the clean-destroy path, got %d", statusCalls)
	}
}
