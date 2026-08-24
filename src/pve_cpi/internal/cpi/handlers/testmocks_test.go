// Package handlers_test provides shared mock implementations for handler unit tests.
package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
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
	poolsSvc          pve.PoolService
}

func (m *mockPVEClient) QEMU() qemu.Service { return m.qemuSvc }

// Nodes serves the authoritative per-node listing surface
// (pve.ListGuestsAuthoritative) by deriving ListQemu rows from the wired
// cluster fixture, so suites scripted against the /cluster/resources index
// keep working after the production migration to per-node listings. A wired
// nodes service keeps handling every other method through delegation; with
// neither service wired, Nodes stays nil so production nil-guards behave as
// before.
func (m *mockPVEClient) Nodes() nodes.Service {
	if m.nodesSvc != nil {
		return &authNodesService{Service: m.nodesSvc, listFn: m.Cluster().ListResources, fallbackNode: testNode}
	}
	if m.clusterSvc != nil {
		return &authNodesService{listFn: m.clusterSvc.ListResources, fallbackNode: testNode}
	}
	return nil
}
func (m *mockPVEClient) Tasks() tasks.Service         { return m.tasksSvc }
func (m *mockPVEClient) Storage() storage.Service     { return m.storageSvc }
func (m *mockPVEClient) CloudInit() cloudinit.Service { return m.cloudInitSvc }

// Cluster falls back to an empty-listing stub when a test wires no cluster
// service. The parked detached-disk strategy is the default, so handlers scan
// cluster resources for parker VMs on paths that never touched the cluster
// before; an empty listing is what production sees when no parker exists.
func (m *mockPVEClient) Cluster() cluster.Service {
	if m.clusterSvc == nil {
		return &mockClusterSvc{}
	}
	return m.clusterSvc
}
func (m *mockPVEClient) ClusterStorage() clusterstorage.Service { return m.clusterStorageSvc }
func (m *mockPVEClient) Pools() pve.PoolService                 { return m.poolsSvc }

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
	createFn     func(ctx context.Context, node string, params map[string]any) (string, error)
	stopFn       func(ctx context.Context, node string, vmid int) (string, error)
	resetFn      func(ctx context.Context, node string, vmid int) (string, error)
	startFn      func(ctx context.Context, node string, vmid int) (string, error)
	statusFn     func(ctx context.Context, node string, vmid int) (map[string]any, error)
	configFn     func(ctx context.Context, node string, vmid int) (map[string]any, error)
	detachDiskFn func(ctx context.Context, node string, vmid int, slot string) error
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
func (m *mockQEMUService) DetachDisk(ctx context.Context, node string, vmid int, slot string) error {
	if m.detachDiskFn != nil {
		return m.detachDiskFn(ctx, node, vmid, slot)
	}
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
	updateQemuUnlinkFn       func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) error
	listStorageContentFn     func(ctx context.Context, node string, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error)
	createQemuStatusRebootFn func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error)
	listStorageFn            func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
	listHardwarePciFn        func(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error)
	listQemuFn               func(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error)
}

// ListQemu backs the authoritative per-node listing
// (pve.ListGuestsAuthoritative); nil listQemuFn means an empty node, which is
// what suites that only script the VM under test expect the rest of the
// cluster to look like. A scripted listQemuFn MUST honor its node argument
// (return each guest for exactly one node): the enumeration calls it once per
// derived cluster member, and a fn that ignores node duplicates the same
// guest under every node, so multi-node assertions pass or fail for reasons
// unrelated to production behavior.
func (m *mockNodesService) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	if m.listQemuFn != nil {
		return m.listQemuFn(ctx, node, params)
	}
	empty := nodes.ListQemuResponse{}
	return &empty, nil
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

func (m *mockNodesService) UpdateQemuUnlink(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) error {
	if m.updateQemuUnlinkFn != nil {
		return m.updateQemuUnlinkFn(ctx, node, vmid, params)
	}
	panic("mockNodesService.UpdateQemuUnlink: not configured")
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

func (m *mockNodesService) ListHardwarePci(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error) {
	if m.listHardwarePciFn != nil {
		return m.listHardwarePciFn(ctx, node, params)
	}
	// Default: empty PCI device list (no passthrough devices present).
	empty := nodes.ListHardwarePciResponse{}
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

func (m *mockTasksService) WaitForUPID(ctx context.Context, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
	panic("mockTasksService.WaitForUPID: not expected in handler tests")
}

func (m *mockTasksService) GetStatus(_ context.Context, _, upid string) (*tasks.Status, error) {
	// Default: report a completed task. Handler tests do not enable adaptive
	// polling, so this is only reached if a test opts in explicitly.
	return &tasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// --------------------------------------------------------------------------
// mockStorageService
// --------------------------------------------------------------------------

// mockStorageService lets individual tests wire CreateVolume, DeleteVolume,
// DeleteVolumeAsync, Exists, DeleteVolumeIfExists, and Upload with function
// literals. Methods not set are no-ops or return zero values.
type mockStorageService struct {
	createVolumeFn         func(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error)
	deleteVolumeFn         func(ctx context.Context, node, storage, volume string) error
	deleteVolumeAsyncFn    func(ctx context.Context, node, storage, volume string) (string, error)
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
	if m.deleteVolumeAsyncFn != nil {
		return m.deleteVolumeAsyncFn(ctx, node, storage, volume)
	}
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
// Without options it is equivalent to testConfig(). NetworkResolveRetries is
// explicitly disabled (0): as of Phase 1 that field defaults to 30 (enabled)
// when left nil, and the great majority of tests sharing this minimal
// baseline predate and are unrelated to the SDN eventual-consistency gate —
// tests that specifically exercise it override this field explicitly via a
// testConfigOption or by mutating the returned config directly.
func testConfigWith(opts ...testConfigOption) *config.CPIConfig {
	c := &config.CPIConfig{
		Host:                  "pve.test.local",
		Port:                  8006,
		User:                  "root",
		APIToken:              "test-token",
		Node:                  "pve-node1",
		VMStorage:             "local-lvm",
		DiskStorage:           "local-lvm",
		NetworkBridge:         "vmbr0",
		AgentMode:             "noagent",
		VMDiskFormat:          "qcow2",
		VerifySSL:             boolPtr(false),
		VMIDRangeStart:        100,
		NetworkResolveRetries: new(int),
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
// The pool service defaults to a no-op implementation so handler tests that do
// not exercise lock behaviour compile and pass unchanged.
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
			poolsSvc:   &noopPoolService{},
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
	listStatusFn            func(ctx context.Context) (*cluster.ListStatusResponse, error)
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

// ListSdnZones is the one mockSDNCluster method that does NOT panic when
// unconfigured (every other method here does, by design — see the comment
// above CreateSdnVnets). As of §1.7, pve.NextVNI unconditionally lists SDN
// zones to exclude zone-level control VNIs (e.g. an EVPN zone's vrf-vxlan)
// from allocation, so this call is now incidentally reachable from any
// create_network test that exercises auto-tag allocation (vxlan/evpn zones
// with no explicit cloud_properties.vnet_tag) -- most of which have nothing
// to do with zone listing and never configured listSdnZonesFn. Defaulting to
// an empty zone list here is safe and semantically correct: it means "no
// zone-level VNIs to exclude", which is exactly the pre-§1.7 behavior. Tests
// that specifically need to verify zone-level VNI exclusion wire
// listSdnZonesFn explicitly (see create_network_vxlan_test.go); the
// dedicated exclusion-logic tests live in internal/pve/vni_test.go.
func (m *mockSDNCluster) ListSdnZones(ctx context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error) {
	if m.listSdnZonesFn != nil {
		return m.listSdnZonesFn(ctx, params)
	}
	empty := cluster.ListSdnZonesResponse{}
	return &empty, nil
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

func (m *mockSDNCluster) ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error) {
	if m.listStatusFn != nil {
		return m.listStatusFn(ctx)
	}
	panic("mockSDNCluster.ListStatus called without configuration; opt in by setting listStatusFn")
}

// HA rule/resource stubs: return not-found so removeNodeAffinityPin is a
// safe no-op in tests that do not exercise HA pinning. Tests that exercise
// pinning use naClusterStub (internal package) or naStub (external package).
func (m *mockSDNCluster) DeleteHaRules(_ context.Context, _ string) error {
	return fmt.Errorf("no such rule (mock)")
}
func (m *mockSDNCluster) DeleteHaResources(_ context.Context, _ string, _ *cluster.DeleteHaResourcesParams) error {
	return fmt.Errorf("does not exist (mock)")
}
func (m *mockSDNCluster) CreateHaResources(_ context.Context, _ *cluster.CreateHaResourcesParams) error {
	return nil
}
func (m *mockSDNCluster) CreateHaRules(_ context.Context, _ *cluster.CreateHaRulesParams) error {
	return nil
}
func (m *mockSDNCluster) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	empty := cluster.ListHaRulesResponse{}
	return &empty, nil
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
	// Default: no offline members (fully online fixture cluster). The
	// authoritative enumeration consults this best-effort on many paths whose
	// tests never script it.
	empty := cluster.ListStatusResponse{}
	return &empty, nil
}

func (m *mockClusterSvc) ListConfigNodes(ctx context.Context) (*cluster.ListConfigNodesResponse, error) {
	if m.listConfigNodesFn != nil {
		return m.listConfigNodesFn(ctx)
	}
	// Default: derive membership from the ListResources fixture (the distinct
	// node names in the scripted rows), falling back to a single-node cluster.
	// A fixed default node here would make guests scripted on other nodes
	// invisible to pve.ListGuestsAuthoritative. Tests exercising a specific
	// topology set listConfigNodesFn explicitly.
	rows, err := m.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	return authConfigNodesFromResources(rows, "pve-node1"), nil
}

// authConfigNodesFromResources derives a /cluster/config/nodes response from
// a ListResources-shaped fixture: the distinct node names in the rows, or the
// fallback node when the fixture is empty or node-less, so an empty fixture
// reads as an empty cluster rather than a failed enumeration.
func authConfigNodesFromResources(rows *cluster.ListResourcesResponse, fallback string) *cluster.ListConfigNodesResponse {
	seen := map[string]bool{}
	names := make([]string, 0, 2)
	if rows != nil {
		for _, raw := range *rows {
			var item struct {
				Node string `json:"node"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Node == "" || seen[item.Node] {
				continue
			}
			seen[item.Node] = true
			names = append(names, item.Node)
		}
	}
	if len(names) == 0 {
		names = append(names, fallback)
	}
	out := make(cluster.ListConfigNodesResponse, 0, len(names))
	for _, n := range names {
		// "name" is what pve.ListClusterConfigNodes parses; "node" is kept
		// for callers keyed on the resource-index field name.
		b, _ := json.Marshal(map[string]any{"name": n, "node": n})
		out = append(out, b)
	}
	return &out
}

// authNodesService serves the authoritative per-node listing surface for
// pve.ListGuestsAuthoritative by deriving each node's qemu rows from the
// same ListResources fixture a suite scripts, so tests written against the
// /cluster/resources index keep working after the production migration to
// per-node listings. Rows with an explicit non-qemu type are filtered; rows
// without a node land on fallbackNode. Every other method delegates to the
// embedded Service (panicking when it is nil, same as the suites' own stubs).
type authNodesService struct {
	nodes.Service
	listFn       func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
	fallbackNode string
}

// ListStorageContent and UpdateQemuConfig delegate to the embedded Service
// when one is wired; without a delegate they fall back to what paths that
// previously saw a nil nodes service tolerated: an empty content listing and
// an accepted config write.
func (s *authNodesService) ListStorageContent(ctx context.Context, node, storageName string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if s.Service != nil {
		return s.Service.ListStorageContent(ctx, node, storageName, params)
	}
	empty := nodes.ListStorageContentResponse{}
	return &empty, nil
}

func (s *authNodesService) UpdateQemuConfig(ctx context.Context, node, vmid string, params *nodes.UpdateQemuConfigParams) error {
	if s.Service != nil {
		return s.Service.UpdateQemuConfig(ctx, node, vmid, params)
	}
	return nil
}

// ListQemu unions the delegate's own listing (suites that script per-node
// rows directly) with rows derived from the cluster fixture, deduplicated by
// vmid with the delegate winning, so neither scripting style is shadowed.
func (s *authNodesService) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	out := make(nodes.ListQemuResponse, 0, 4)
	seen := map[int64]bool{}
	vmidOf := func(raw json.RawMessage) int64 {
		var item struct {
			Vmid int64 `json:"vmid"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return 0
		}
		return item.Vmid
	}
	if s.Service != nil {
		resp, err := s.Service.ListQemu(ctx, node, params)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			for _, raw := range *resp {
				out = append(out, raw)
				if id := vmidOf(raw); id > 0 {
					seen[id] = true
				}
			}
		}
	}
	var rows *cluster.ListResourcesResponse
	if s.listFn != nil {
		// Type="vm" matches the VMID allocator's own query shape; suites use
		// Type-unset ListResources as the signature of the legacy cache
		// lookup, so the derive must not masquerade as one.
		vmType := "vm"
		var err error
		rows, err = s.listFn(ctx, &cluster.ListResourcesParams{Type: &vmType})
		if err != nil {
			return nil, err
		}
	}
	if rows != nil {
		for _, raw := range *rows {
			var item struct {
				Node string `json:"node"`
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			if item.Type != "" && item.Type != "qemu" {
				continue
			}
			rowNode := item.Node
			if rowNode == "" {
				rowNode = s.fallbackNode
			}
			if rowNode != node {
				continue
			}
			if id := vmidOf(raw); id > 0 && seen[id] {
				continue
			}
			out = append(out, raw)
		}
	}
	return &out, nil
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
		// Every VM the CPI creates is tagged, and handlers that classify a VM
		// from its cluster row treat an EMPTY tag string as "this PVE may not
		// populate the field" and fall back to a config read. An untagged
		// fixture would exercise that fallback rather than the path production
		// takes.
		"tags": "bosh-cpi",
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

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (m *mockNodesService) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	empty := nodes.ListNodesResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (s *authNodesService) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	empty := nodes.ListNodesResponse{}
	return &empty, nil
}
