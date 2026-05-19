// Package handlers_test provides shared mock implementations for handler unit tests.
package handlers_test

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// mockPVEClient implements pve.Client for tests.
type mockPVEClient struct {
	qemuSvc      qemu.Service
	nodesSvc     nodes.Service
	tasksSvc     tasks.Service
	storageSvc   storage.Service
	cloudInitSvc cloudinit.Service
	clusterSvc   cluster.Service
}

func (m *mockPVEClient) QEMU() qemu.Service                     { return m.qemuSvc }
func (m *mockPVEClient) Nodes() nodes.Service                   { return m.nodesSvc }
func (m *mockPVEClient) Tasks() tasks.Service                   { return m.tasksSvc }
func (m *mockPVEClient) Storage() storage.Service               { return m.storageSvc }
func (m *mockPVEClient) CloudInit() cloudinit.Service           { return m.cloudInitSvc }
func (m *mockPVEClient) Cluster() cluster.Service               { return m.clusterSvc }
func (m *mockPVEClient) ClusterStorage() clusterstorage.Service { return nil }

// --------------------------------------------------------------------------
// mockQEMUService
// --------------------------------------------------------------------------

// mockQEMUService partially implements qemu.Service for handler tests.
// Only methods used by the handlers are wired; others panic if called.
// CreateFn defaults to ("upid-mock-create", nil) when nil so tests not
// exercising Create pass through without configuration.
type mockQEMUService struct {
	createFn func(ctx context.Context, node string, params map[string]interface{}) (string, error)
	stopFn   func(ctx context.Context, node string, vmid int) (string, error)
	resetFn  func(ctx context.Context, node string, vmid int) (string, error)
	configFn func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
}

func (m *mockQEMUService) Stop(ctx context.Context, node string, vmid int) (string, error) {
	if m.stopFn != nil {
		return m.stopFn(ctx, node, vmid)
	}
	panic("mockQEMUService.Stop: not configured")
}

func (m *mockQEMUService) Reset(ctx context.Context, node string, vmid int) (string, error) {
	if m.resetFn != nil {
		return m.resetFn(ctx, node, vmid)
	}
	panic("mockQEMUService.Reset: not configured")
}

func (m *mockQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	panic("mockQEMUService.Config: not configured")
}

func (m *mockQEMUService) Create(ctx context.Context, node string, params map[string]interface{}) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	// Default: succeed quietly so tests not exercising Create don't need configuration.
	return "upid-mock-create", nil
}

// Unimplemented qemu.Service methods — panic on accidental call.
func (m *mockQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("mockQEMUService.Status: not expected")
}
func (m *mockQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("mockQEMUService.Start: not expected")
}
func (m *mockQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("mockQEMUService.Clone: not expected")
}
func (m *mockQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("mockQEMUService.Template: not expected")
}
func (m *mockQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("mockQEMUService.AttachDisk: not expected")
}
func (m *mockQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("mockQEMUService.DetachDisk: not expected")
}
func (m *mockQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("mockQEMUService.ResizeDisk: not expected")
}
func (m *mockQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("mockQEMUService.Snapshot: not expected")
}
func (m *mockQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("mockQEMUService.DeleteSnapshot: not expected")
}
func (m *mockQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("mockQEMUService.ListSnapshots: not expected")
}
func (m *mockQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("mockQEMUService.RollbackSnapshot: not expected")
}

// --------------------------------------------------------------------------
// mockNodesService
// --------------------------------------------------------------------------

// mockNodesService embeds panicNodesStub and overrides the methods
// used by delete_vm, set_vm_metadata, and direct-qcow handlers.
// ListStorageContentFn defaults to returning an empty response when nil.
type mockNodesService struct {
	panicNodesStub
	deleteQemuFn         func(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error)
	updateQemuConfigFn   func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error
	listStorageContentFn func(ctx context.Context, node string, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error)
}

func (m *mockNodesService) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	if m.deleteQemuFn != nil {
		return m.deleteQemuFn(ctx, node, vmid, params)
	}
	panic("mockNodesService.DeleteQemu: not configured")
}

func (m *mockNodesService) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error {
	if m.updateQemuConfigFn != nil {
		return m.updateQemuConfigFn(ctx, node, vmid, params)
	}
	panic("mockNodesService.UpdateQemuConfig: not configured")
}

func (m *mockNodesService) ListStorageContent(ctx context.Context, node string, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if m.listStorageContentFn != nil {
		return m.listStorageContentFn(ctx, node, storage, params)
	}
	// Default: empty storage content — no volumes present.
	empty := nodes.ListStorageContentResponse{}
	return &empty, nil
}

// --------------------------------------------------------------------------
// mockTasksService
// --------------------------------------------------------------------------

// mockTasksService implements tasks.Service for handler tests.
type mockTasksService struct {
	waitFn func(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error)
}

func (m *mockTasksService) Wait(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
	if m.waitFn != nil {
		return m.waitFn(ctx, node, upid, opts)
	}
	// Default: succeed immediately.
	return &tasks.Status{ExitStatus: "OK"}, nil
}

// --------------------------------------------------------------------------
// mockAgentService
// --------------------------------------------------------------------------

// mockAgentService implements agent.Agent for handler tests.
type mockAgentService struct {
	removeFn func(ctx context.Context, node string, vmid int) error
}

func (m *mockAgentService) Configure(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
	panic("mockAgentService.Configure: not expected in these tests")
}

func (m *mockAgentService) Remove(ctx context.Context, node string, vmid int) error {
	if m.removeFn != nil {
		return m.removeFn(ctx, node, vmid)
	}
	return nil // default: no-op success
}

func (m *mockAgentService) UpdateDiskHints(_ context.Context, _ int, _ []agent.DiskHint) error {
	panic("mockAgentService.UpdateDiskHints: not expected in these tests")
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// --------------------------------------------------------------------------
// testConfig
// --------------------------------------------------------------------------

// testConfig returns a minimal CPIConfig for handler tests.
func testConfig() *config.CPIConfig {
	return &config.CPIConfig{
		Host:           "pve.test.local",
		Port:           8006,
		User:           "root",
		APIToken:       "test-token",
		Node:           "pve-node1",
		VMStorage:      "local-lvm",
		DiskStorage:    "local-lvm",
		NetworkBridge:  "vmbr0",
		AgentMode:      "noagent",
		VMDiskFormat:   "qcow2",
		VerifySSL:      boolPtr(false),
		VMIDRangeStart: 100,
	}
}

// --------------------------------------------------------------------------
// testDeps
// --------------------------------------------------------------------------

// testDeps builds a Deps struct wiring the provided mock services.
func testDeps(qemuSvc qemu.Service, nodesSvc nodes.Service, tasksSvc tasks.Service, agentSvc agent.Agent) handlers.Deps {
	return handlers.Deps{
		Config: testConfig(),
		PVE: &mockPVEClient{
			qemuSvc:  qemuSvc,
			nodesSvc: nodesSvc,
			tasksSvc: tasksSvc,
		},
		Agent:  agentSvc,
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// marshalArgs
// --------------------------------------------------------------------------

// marshalArgs JSON-encodes each value and returns a []json.RawMessage suitable
// for handler invocation.
func marshalArgs(vals ...any) []json.RawMessage {
	out := make([]json.RawMessage, len(vals))
	for i, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			panic("marshalArgs: " + err.Error())
		}
		out[i] = b
	}
	return out
}
