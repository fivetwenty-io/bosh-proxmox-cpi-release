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
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// Deleting an HA/DLB-registered VM must deregister its HA state BEFORE the
// stop: while a VM is HA-managed, status/stop is redirected to a CRM request
// whose task completes on acceptance — not when the guest halts — so a
// stop-then-deregister sequence races the LRM and the destroy fails with
// "VM <id> is running - destroy failed". Observed live on a DLB-registered
// compilation VM. These tests pin the deregister-first order and the
// wait-and-retry recovery on the running-destroy failure.

// orderRecordingCluster embeds mockClusterSvc and records HA deregistration
// calls into a shared event log.
type orderRecordingCluster struct {
	mockClusterSvc
	events *[]string
}

func (m *orderRecordingCluster) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	*m.events = append(*m.events, "DeleteHaResources:"+sid)
	return nil
}

func (m *orderRecordingCluster) DeleteHaRules(_ context.Context, rule string) error {
	*m.events = append(*m.events, "DeleteHaRules:"+rule)
	return nil
}

func dlbEnabledConfig() *config.CPIConfig {
	cfg := *testConfig()
	enabled := true
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	return &cfg
}

func TestHandleDeleteVM_DLB_DeregistersHABeforeStop(t *testing.T) {
	t.Parallel()

	var events []string

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			events = append(events, "Stop")
			return "UPID:node:stop-task", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			events = append(events, "DeleteQemu")
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
	deps.PVE = &mockPVEClient{
		qemuSvc:    qemuSvc,
		nodesSvc:   nodesSvc,
		tasksSvc:   tasksSvc,
		storageSvc: &mockStorageService{},
		clusterSvc: &orderRecordingCluster{
			mockClusterSvc: *defaultClusterSvc(101, "pve-node1"),
			events:         &events,
		},
		poolsSvc: &noopPoolService{},
	}

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stopIdx, haIdx := -1, -1
	for i, ev := range events {
		if ev == "Stop" && stopIdx == -1 {
			stopIdx = i
		}
		if strings.HasPrefix(ev, "DeleteHaResources:") && haIdx == -1 {
			haIdx = i
		}
	}
	if haIdx == -1 {
		t.Fatalf("HA resource was never deregistered; events: %v", events)
	}
	if stopIdx == -1 {
		t.Fatalf("VM was never stopped; events: %v", events)
	}
	if haIdx > stopIdx {
		t.Errorf("HA deregistration must happen BEFORE the stop (CRM redirects stop of an HA-managed VM); events: %v", events)
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
