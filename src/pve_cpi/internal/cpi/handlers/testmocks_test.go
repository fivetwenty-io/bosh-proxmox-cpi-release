// Package handlers_test provides shared mock implementations for handler unit tests.
package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// Package-level sentinel constants shared across handler test files.
const (
	testNode    = "pve1"
	vmNode      = "pve-node1"
	diskSlot    = "scsi2"
	storageName = "local-lvm"
	diskCID     = "local-lvm:vm-9001-disk-0"
	testDiskCID = "local-lvm:vm-100-disk-0"
	vmStatus    = "stopped"
)

// mockPVEClient implements pve.Client for tests.
type mockPVEClient struct {
	qemuSvc           qemu.Service
	nodesSvc          nodes.Service
	tasksSvc          tasks.Service
	storageSvc        storage.Service
	cloudInitSvc      cloudinit.Service
	clusterSvc        cluster.Service
	clusterStorageSvc clusterstorage.Service
}

func (m *mockPVEClient) QEMU() qemu.Service                     { return m.qemuSvc }
func (m *mockPVEClient) Nodes() nodes.Service                   { return m.nodesSvc }
func (m *mockPVEClient) Tasks() tasks.Service                   { return m.tasksSvc }
func (m *mockPVEClient) Storage() storage.Service               { return m.storageSvc }
func (m *mockPVEClient) CloudInit() cloudinit.Service           { return m.cloudInitSvc }
func (m *mockPVEClient) Cluster() cluster.Service               { return m.clusterSvc }
func (m *mockPVEClient) ClusterStorage() clusterstorage.Service { return m.clusterStorageSvc }
func (m *mockPVEClient) Pools() pve.PoolService                 { return nil }

// --------------------------------------------------------------------------
// mockQEMUService
// --------------------------------------------------------------------------

// compile-time interface check.
var _ qemu.Service = (*mockQEMUService)(nil)

// mockQEMUService partially implements qemu.Service for handler tests.
// Only methods used by the handlers are wired; others panic if called.
// CreateFn defaults to ("upid-mock-create", nil) when nil so tests not
// exercising Create pass through without configuration.
type mockQEMUService struct {
	createFn func(ctx context.Context, node string, params map[string]any) (string, error)
	stopFn   func(ctx context.Context, node string, vmid int) (string, error)
	resetFn  func(ctx context.Context, node string, vmid int) (string, error)
	startFn  func(ctx context.Context, node string, vmid int) (string, error)
	statusFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
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

func (m *mockQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	// Default: empty config. Handlers (e.g., set_vm_metadata) call Config()
	// to preserve operator-supplied tags; tests that don't exercise that
	// preservation logic should see a no-op empty config.
	return map[string]any{}, nil
}

func (m *mockQEMUService) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	// Default: succeed quietly so tests not exercising Create don't need configuration.
	return "upid-mock-create", nil
}

func (m *mockQEMUService) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.statusFn != nil {
		return m.statusFn(ctx, node, vmid)
	}
	panic("mockQEMUService.Status: not configured")
}

func (m *mockQEMUService) Start(ctx context.Context, node string, vmid int) (string, error) {
	if m.startFn != nil {
		return m.startFn(ctx, node, vmid)
	}
	panic("mockQEMUService.Start: not configured")
}

// Unimplemented qemu.Service methods — panic on accidental call.
func (m *mockQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *mockQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("mockQEMUService.Snapshot: not expected")
}
func (m *mockQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("mockQEMUService.DeleteSnapshot: not expected")
}
func (m *mockQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("mockQEMUService.ListSnapshots: not expected")
}
func (m *mockQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("mockQEMUService.RollbackSnapshot: not expected")
}

// --------------------------------------------------------------------------
// mockNodesService
// --------------------------------------------------------------------------

// mockNodesService embeds panicNodesStub and overrides the methods
// used by delete_vm, reboot_vm, set_vm_metadata, direct-qcow handlers,
// and calculate_vm_cloud_properties (listStorageFn).
// All Fn fields default to nil; nil means use the safe default behaviour
// documented on each method below.
type mockNodesService struct {
	panicNodesStub
	deleteQemuFn             func(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error)
	updateQemuConfigFn       func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error
	listStorageContentFn     func(ctx context.Context, node string, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error)
	createQemuStatusRebootFn func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error)
	listStorageFn            func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
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

func (m *mockNodesService) CreateQemuStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
	if m.createQemuStatusRebootFn != nil {
		return m.createQemuStatusRebootFn(ctx, node, vmid, params)
	}
	panic("mockNodesService.CreateQemuStatusReboot: not configured")
}

func (m *mockNodesService) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	if m.listStorageFn != nil {
		return m.listStorageFn(ctx, node, params)
	}
	// Default: return active+images for whatever storage name was requested.
	// The handler passes &ListStorageParams{Storage: &effectiveStorage}; when
	// params.Storage is set, echo that name back as active+images so tests
	// that do not exercise storage filtering pass without extra configuration.
	storageName := "local-lvm" // safe fallback matching testConfig()
	if params != nil && params.Storage != nil && *params.Storage != "" {
		storageName = *params.Storage
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName,
		"type":    "dir",
		"active":  1,
		"enabled": 1,
		"content": "images,rootdir",
	})
	resp := nodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
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

func (m *mockTasksService) WaitForUPID(ctx context.Context, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
	panic("mockTasksService.WaitForUPID: not expected in handler tests")
}

// --------------------------------------------------------------------------
// mockStorageService
// --------------------------------------------------------------------------

// mockStorageService lets individual tests wire CreateVolume, DeleteVolume,
// Exists, DeleteVolumeIfExists, and Upload with function literals.
// Methods not set are no-ops or return zero values.
type mockStorageService struct {
	createVolumeFn         func(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error)
	deleteVolumeFn         func(ctx context.Context, node, storage, volume string) error
	existsFn               func(ctx context.Context, node, storage, volume string) (bool, error)
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
	uploadFn               func(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error)
}

func (m *mockStorageService) CreateVolume(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
	if m.createVolumeFn != nil {
		return m.createVolumeFn(ctx, node, storage, sizeGiB, format, vmid, name)
	}
	return fmt.Sprintf("%s/%s", storage, name), nil
}

func (m *mockStorageService) DeleteVolume(ctx context.Context, node, storage, volume string) error {
	if m.deleteVolumeFn != nil {
		return m.deleteVolumeFn(ctx, node, storage, volume)
	}
	return nil
}

func (m *mockStorageService) DeleteVolumeAsync(ctx context.Context, node, storage, volume string) (string, error) {
	if err := m.DeleteVolume(ctx, node, storage, volume); err != nil {
		return "", err
	}
	return "", nil
}

func (m *mockStorageService) Exists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.existsFn != nil {
		return m.existsFn(ctx, node, storage, volume)
	}
	return false, nil
}

func (m *mockStorageService) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	return false, nil
}

func (m *mockStorageService) DeleteVolumeIfExistsAsync(ctx context.Context, node, storage, volume string) (bool, string, error) {
	existed, err := m.DeleteVolumeIfExists(ctx, node, storage, volume)
	if err != nil {
		return false, "", err
	}
	return existed, "", nil
}

func (m *mockStorageService) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, node, storage, content, filename, body)
	}
	return "", nil
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

//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
func boolPtr(b bool) *bool { return &b }

// --------------------------------------------------------------------------
// testConfig
// --------------------------------------------------------------------------

// testConfigOption is a functional option that modifies a CPIConfig returned
// by testConfig or testConfigWith.
type testConfigOption func(*config.CPIConfig)

// WithStorageType returns a testConfigOption that sets VMStorage and DiskStorage
// to a pool name that reflects the given PVE storage type.
// Accepted values: "zfspool", "lvmthin", "dir".
// Default (when not applied): "local-lvm" (lvm block storage).
//
// This option also updates VMDiskFormat to "raw" for block storages (zfspool,
// lvmthin) because qcow2 is rejected by those storage types. For "dir" the
// format stays "qcow2" since dir-type storage supports image formats.
func WithStorageType(storageType string) testConfigOption {
	return func(c *config.CPIConfig) {
		switch storageType {
		case "zfspool":
			c.VMStorage = "local-zfs"
			c.DiskStorage = "local-zfs"
			c.VMDiskFormat = "raw"
		case "lvmthin":
			c.VMStorage = "local-lvm-thin"
			c.DiskStorage = "local-lvm-thin"
			c.VMDiskFormat = "raw"
		case "dir":
			c.VMStorage = "local"
			c.DiskStorage = "local"
			// dir-type storage accepts qcow2; keep the default format.
		default:
			panic("WithStorageType: unsupported storage type " + storageType + "; accepted: zfspool, lvmthin, dir")
		}
	}
}

// testConfig returns a minimal CPIConfig for handler tests.
func testConfig() *config.CPIConfig {
	return testConfigWith()
}

// testConfigWith returns a minimal CPIConfig with the given options applied.
// Without options it is equivalent to testConfig().
func testConfigWith(opts ...testConfigOption) *config.CPIConfig {
	c := &config.CPIConfig{
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
	for _, o := range opts {
		o(c)
	}
	return c
}

// --------------------------------------------------------------------------
// testDeps
// --------------------------------------------------------------------------

// testDeps builds a Deps struct wiring the provided mock services.
//
//nolint:unparam // qemuSvc kept for future tests that need a non-nil QEMU mock
func testDeps(qemuSvc qemu.Service, nodesSvc nodes.Service, tasksSvc tasks.Service, agentSvc agent.Agent) handlers.Deps {
	return testDepsWithStorage(qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{})
}

// testDepsWithStorage is testDeps with an explicit storage service.
// The default cluster mock returns an empty resource list, so
// FindVMNodeViaCluster reports VM-not-found. Tests that need the cluster scan
// to resolve a specific node must use testDepsFoundVM or testDepsFoundVMWithStorage.
func testDepsWithStorage(qemuSvc qemu.Service, nodesSvc nodes.Service, tasksSvc tasks.Service, agentSvc agent.Agent, storageSvc storage.Service) handlers.Deps {
	if qemuSvc == nil {
		qemuSvc = &mockQEMUService{}
	}
	return handlers.Deps{
		Config: testConfig(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   tasksSvc,
			storageSvc: storageSvc,
			clusterSvc: &mockClusterSvc{}, // empty list: VM not found
		},
		Agent:  agentSvc,
		Logger: log.NewNopLogger(),
	}
}

// testDepsFoundVM builds Deps where the cluster scan resolves vmid to "pve-node1".
// Use for tests that must reach handler logic past the cluster-scan step (e.g.
// Stop, Config, UpdateQemuConfig, DeleteSnapshot). Storage defaults to a no-op mock.
func testDepsFoundVM(vmid int, qemuSvc qemu.Service, nodesSvc nodes.Service, tasksSvc tasks.Service, agentSvc agent.Agent) handlers.Deps {
	return testDepsFoundVMWithStorage(vmid, qemuSvc, nodesSvc, tasksSvc, agentSvc, &mockStorageService{})
}

// testDepsFoundVMWithStorage is testDepsFoundVM with an explicit storage service.
func testDepsFoundVMWithStorage(vmid int, qemuSvc qemu.Service, nodesSvc nodes.Service, tasksSvc tasks.Service, agentSvc agent.Agent, storageSvc storage.Service) handlers.Deps {
	if qemuSvc == nil {
		qemuSvc = &mockQEMUService{}
	}
	return handlers.Deps{
		Config: testConfig(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   tasksSvc,
			storageSvc: storageSvc,
			clusterSvc: defaultClusterSvc(vmid, "pve-node1"),
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

// --------------------------------------------------------------------------
// mockSDNCluster
// --------------------------------------------------------------------------

// mockSDNCluster embeds cluster.Service (nil — panics on any non-overridden
// method). Override only the SDN methods handlers call; set the corresponding
// Fn field to supply behaviour; leave nil for a zero-value safe default.
type mockSDNCluster struct {
	cluster.Service // nil — non-overridden methods panic

	createSdnVnetsFn        func(ctx context.Context, params *cluster.CreateSdnVnetsParams) error
	deleteSdnVnetsFn        func(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) error
	getSdnVnetsFn           func(ctx context.Context, vnet string, params *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error)
	listSdnVnetsFn          func(ctx context.Context, params *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error)
	createSdnZonesFn        func(ctx context.Context, params *cluster.CreateSdnZonesParams) error
	deleteSdnZonesFn        func(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) error
	getSdnZonesFn           func(ctx context.Context, zone string, params *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error)
	listSdnZonesFn          func(ctx context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error)
	createSdnVnetsSubnetsFn func(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) error
	deleteSdnVnetsSubnetsFn func(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) error
	listSdnVnetsSubnetsFn   func(ctx context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error)
	updateSdnFn             func(ctx context.Context, params *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error)
}

// compile-time interface check.
var _ cluster.Service = (*mockSDNCluster)(nil)

// SDN mock defaults panic on unconfigured methods. Tests that exercise the
// SDN code path MUST explicitly opt into each method by setting the
// corresponding Fn field. Panic-on-unconfigured surfaces missing mock
// configuration immediately rather than silently succeeding.

func (m *mockSDNCluster) CreateSdnVnets(ctx context.Context, params *cluster.CreateSdnVnetsParams) error {
	if m.createSdnVnetsFn != nil {
		return m.createSdnVnetsFn(ctx, params)
	}
	panic("mockSDNCluster.CreateSdnVnets called without configuration; opt in by setting createSdnVnetsFn")
}

func (m *mockSDNCluster) DeleteSdnVnets(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) error {
	if m.deleteSdnVnetsFn != nil {
		return m.deleteSdnVnetsFn(ctx, vnet, params)
	}
	panic("mockSDNCluster.DeleteSdnVnets called without configuration; opt in by setting deleteSdnVnetsFn")
}

func (m *mockSDNCluster) GetSdnVnets(ctx context.Context, vnet string, params *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error) {
	if m.getSdnVnetsFn != nil {
		return m.getSdnVnetsFn(ctx, vnet, params)
	}
	panic("mockSDNCluster.GetSdnVnets called without configuration; opt in by setting getSdnVnetsFn")
}

func (m *mockSDNCluster) ListSdnVnets(ctx context.Context, params *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	if m.listSdnVnetsFn != nil {
		return m.listSdnVnetsFn(ctx, params)
	}
	panic("mockSDNCluster.ListSdnVnets called without configuration; opt in by setting listSdnVnetsFn")
}

func (m *mockSDNCluster) CreateSdnZones(ctx context.Context, params *cluster.CreateSdnZonesParams) error {
	if m.createSdnZonesFn != nil {
		return m.createSdnZonesFn(ctx, params)
	}
	panic("mockSDNCluster.CreateSdnZones called without configuration; opt in by setting createSdnZonesFn")
}

func (m *mockSDNCluster) DeleteSdnZones(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) error {
	if m.deleteSdnZonesFn != nil {
		return m.deleteSdnZonesFn(ctx, zone, params)
	}
	panic("mockSDNCluster.DeleteSdnZones called without configuration; opt in by setting deleteSdnZonesFn")
}

func (m *mockSDNCluster) GetSdnZones(ctx context.Context, zone string, params *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error) {
	if m.getSdnZonesFn != nil {
		return m.getSdnZonesFn(ctx, zone, params)
	}
	panic("mockSDNCluster.GetSdnZones called without configuration; opt in by setting getSdnZonesFn")
}

func (m *mockSDNCluster) ListSdnZones(ctx context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error) {
	if m.listSdnZonesFn != nil {
		return m.listSdnZonesFn(ctx, params)
	}
	panic("mockSDNCluster.ListSdnZones called without configuration; opt in by setting listSdnZonesFn")
}

func (m *mockSDNCluster) CreateSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) error {
	if m.createSdnVnetsSubnetsFn != nil {
		return m.createSdnVnetsSubnetsFn(ctx, vnet, params)
	}
	panic("mockSDNCluster.CreateSdnVnetsSubnets called without configuration; opt in by setting createSdnVnetsSubnetsFn")
}

func (m *mockSDNCluster) DeleteSdnVnetsSubnets(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) error {
	if m.deleteSdnVnetsSubnetsFn != nil {
		return m.deleteSdnVnetsSubnetsFn(ctx, vnet, subnet, params)
	}
	panic("mockSDNCluster.DeleteSdnVnetsSubnets called without configuration; opt in by setting deleteSdnVnetsSubnetsFn")
}

func (m *mockSDNCluster) ListSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
	if m.listSdnVnetsSubnetsFn != nil {
		return m.listSdnVnetsSubnetsFn(ctx, vnet, params)
	}
	panic("mockSDNCluster.ListSdnVnetsSubnets called without configuration; opt in by setting listSdnVnetsSubnetsFn")
}

func (m *mockSDNCluster) UpdateSdn(ctx context.Context, params *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
	if m.updateSdnFn != nil {
		return m.updateSdnFn(ctx, params)
	}
	panic("mockSDNCluster.UpdateSdn called without configuration; opt in by setting updateSdnFn")
}

// --------------------------------------------------------------------------
// mockBridgeNodes
// --------------------------------------------------------------------------

// mockBridgeNodes embeds panicNodesStub (nil — panics on non-overridden methods)
// and overrides the three node-bridge methods the bridge fallback path calls.
// Set the corresponding Fn field to supply behaviour; nil gives a safe default.
// Additive: does not replace the existing mockNodesService used by other tests.
type mockBridgeNodes struct {
	panicNodesStub

	createNetworkFn  func(ctx context.Context, node string, params *nodes.CreateNetworkParams) error
	deleteNetwork2Fn func(ctx context.Context, node string, iface string) error
	updateNetworkFn  func(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error)
}

// compile-time interface check.
var _ nodes.Service = (*mockBridgeNodes)(nil)

func (m *mockBridgeNodes) CreateNetwork(ctx context.Context, node string, params *nodes.CreateNetworkParams) error {
	if m.createNetworkFn != nil {
		return m.createNetworkFn(ctx, node, params)
	}
	return nil
}

func (m *mockBridgeNodes) DeleteNetwork2(ctx context.Context, node string, iface string) error {
	if m.deleteNetwork2Fn != nil {
		return m.deleteNetwork2Fn(ctx, node, iface)
	}
	return nil
}

func (m *mockBridgeNodes) UpdateNetwork(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error) {
	if m.updateNetworkFn != nil {
		return m.updateNetworkFn(ctx, node, params)
	}
	return nil, nil
}

// --------------------------------------------------------------------------
// mockClusterSvc
// --------------------------------------------------------------------------

// mockClusterSvc implements cluster.Service with a configurable ListResources.
// All other methods are provided by the embedded mockSDNCluster (which panics
// on accidental calls to SDN methods). The default ListResources places any
// queried VM on "pve-node1" (matching testConfig().Node) so existing tests that
// do not exercise HA-failover node resolution pass without modification.
//
// Set listResourcesFn / listStatusFn to override behavior for specific tests.
type mockClusterSvc struct {
	mockSDNCluster
	listResourcesFn   func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
	listStatusFn      func(ctx context.Context) (*cluster.ListStatusResponse, error)
	listConfigNodesFn func(ctx context.Context) (*cluster.ListConfigNodesResponse, error)
}

var _ cluster.Service = (*mockClusterSvc)(nil)

func (m *mockClusterSvc) ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	if m.listResourcesFn != nil {
		return m.listResourcesFn(ctx, params)
	}
	// Default: return an empty resource list. Handlers treat empty-list as
	// "VM not found". Tests that need a VM to be found must supply listResourcesFn
	// or use clusterVMOnNode to build a response.
	empty := cluster.ListResourcesResponse{}
	return &empty, nil
}

func (m *mockClusterSvc) ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error) {
	if m.listStatusFn != nil {
		return m.listStatusFn(ctx)
	}
	panic("mockClusterSvc.ListStatus: not configured")
}

func (m *mockClusterSvc) ListConfigNodes(ctx context.Context) (*cluster.ListConfigNodesResponse, error) {
	if m.listConfigNodesFn != nil {
		return m.listConfigNodesFn(ctx)
	}
	// Default: single-node cluster (one entry). Tests exercising multi-node
	// topology must set listConfigNodesFn explicitly.
	raw, _ := json.Marshal(map[string]any{"node": "pve-node1"})
	resp := cluster.ListConfigNodesResponse{raw}
	return &resp, nil
}

// mockClusterStorage is a minimal clusterstorage.Service stub for create_vm
// template dispatch tests. It reports a single named storage entry.
type mockClusterStorage struct {
	storageName string
	storageType string
	shared      bool
}

func (s *mockClusterStorage) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	sharedInt := 0
	if s.shared {
		sharedInt = 1
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": s.storageName,
		"type":    s.storageType,
		"shared":  sharedInt,
	})
	resp := clusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}

func (s *mockClusterStorage) CreateStorage(_ context.Context, _ *clusterstorage.CreateStorageParams) (*clusterstorage.CreateStorageResponse, error) {
	panic("mockClusterStorage.CreateStorage: not expected")
}
func (s *mockClusterStorage) DeleteStorage(_ context.Context, _ string) error {
	panic("mockClusterStorage.DeleteStorage: not expected")
}
func (s *mockClusterStorage) GetStorage(_ context.Context, _ string) (*clusterstorage.GetStorageResponse, error) {
	panic("mockClusterStorage.GetStorage: not expected")
}
func (s *mockClusterStorage) UpdateStorage(_ context.Context, _ string, _ *clusterstorage.UpdateStorageParams) (*clusterstorage.UpdateStorageResponse, error) {
	panic("mockClusterStorage.UpdateStorage: not expected")
}

var _ clusterstorage.Service = (*mockClusterStorage)(nil)

// clusterVMOnNode builds a ListResourcesResponse placing vmid on node.
// Used to feed FindVMNodeViaCluster in tests that need the cluster scan to
// resolve a specific node for a VM.
func clusterVMOnNode(vmid int, node string) *cluster.ListResourcesResponse {
	raw, _ := json.Marshal(map[string]any{
		"vmid": vmid,
		"node": node,
		"type": "qemu",
	})
	resp := cluster.ListResourcesResponse{raw}
	return &resp
}

// defaultClusterSvc returns a mockClusterSvc whose ListResources places vmid
// on the given node. Used in testDepsWithCluster to make cluster-scan-using
// handlers find the VM on the expected node.
func defaultClusterSvc(vmid int, node string) *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return clusterVMOnNode(vmid, node), nil
		},
	}
}

// defaultMultiNodeClusterSvc returns a mockClusterSvc that places vmid on
// node (which may be any of the three simulated cluster members: pve01, pve02,
// pve03). The cluster response also includes two other VMs on the remaining
// nodes to simulate a heterogeneous cluster — handlers must forward the
// resolved node correctly regardless of which member hosts the target VM.
func defaultMultiNodeClusterSvc(vmid int, node string) *mockClusterSvc {
	otherEntries := []json.RawMessage{}
	for _, n := range []struct {
		id   int
		node string
	}{
		{1000, "pve01"},
		{1001, "pve02"},
		{1002, "pve03"},
	} {
		if n.id == vmid {
			continue
		}
		b, _ := json.Marshal(map[string]any{"vmid": n.id, "node": n.node, "type": "qemu"})
		otherEntries = append(otherEntries, b)
	}
	target, _ := json.Marshal(map[string]any{"vmid": vmid, "node": node, "type": "qemu"})
	all := append(otherEntries, target) //nolint:gocritic // append to named slice intentional
	resp := cluster.ListResourcesResponse(all)

	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &resp, nil
		},
	}
}

// testDepsWithCluster is testDepsWithStorage plus an explicit cluster service.
// Use this for handlers that call FindVMNodeViaCluster when you need to control
// which node is returned.
func testDepsWithCluster(
	qemuSvc qemu.Service,
	nodesSvc nodes.Service,
	tasksSvc tasks.Service,
	agentSvc agent.Agent,
	storageSvc storage.Service,
	clusterSvc cluster.Service,
) handlers.Deps {
	if qemuSvc == nil {
		qemuSvc = &mockQEMUService{}
	}
	return handlers.Deps{
		Config: testConfig(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   tasksSvc,
			storageSvc: storageSvc,
			clusterSvc: clusterSvc,
		},
		Agent:  agentSvc,
		Logger: log.NewNopLogger(),
	}
}
