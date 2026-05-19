package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// testStemcellCID is the canonical volid-format stemcell CID used across create_vm tests.
const testStemcellCID = "test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2"

// --------------------------------------------------------------------------
// create_vm-specific mocks
// --------------------------------------------------------------------------

// vmMockQEMU implements qemu.Service for create_vm tests.
type vmMockQEMU struct {
	createFn     func(ctx context.Context, node string, params map[string]interface{}) (string, error)
	startFn      func(ctx context.Context, node string, vmid int) (string, error)
	stopFn       func(ctx context.Context, node string, vmid int) (string, error)
	configFn     func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	attachDiskFn func(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error)

	createCalls []vmCreateCall
	startCalls  []int
}

type vmCreateCall struct {
	node   string
	params map[string]interface{}
}

func (m *vmMockQEMU) Create(ctx context.Context, node string, params map[string]interface{}) (string, error) {
	m.createCalls = append(m.createCalls, vmCreateCall{node, params})
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "UPID:pve:create:ok", nil
}

func (m *vmMockQEMU) Start(ctx context.Context, node string, vmid int) (string, error) {
	m.startCalls = append(m.startCalls, vmid)
	if m.startFn != nil {
		return m.startFn(ctx, node, vmid)
	}
	return "UPID:pve:start:ok", nil
}
func (m *vmMockQEMU) Stop(ctx context.Context, node string, vmid int) (string, error) {
	if m.stopFn != nil {
		return m.stopFn(ctx, node, vmid)
	}
	return "UPID:pve:stop:ok", nil
}
func (m *vmMockQEMU) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
}
func (m *vmMockQEMU) AttachDisk(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error) {
	if m.attachDiskFn != nil {
		return m.attachDiskFn(ctx, node, vmid, volid, bus, opts)
	}
	return "scsi1", nil
}

// Clone panics — create_vm no longer calls Clone; tests that accidentally trigger
// it reveal a regression in the handler.
func (m *vmMockQEMU) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("vmMockQEMU.Clone: create_vm must not call Clone (direct-import mode)")
}

// Unimplemented stubs — panic on unexpected calls.
func (m *vmMockQEMU) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("vmMockQEMU.Status: not expected")
}
func (m *vmMockQEMU) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("vmMockQEMU.Reset: not expected")
}
func (m *vmMockQEMU) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("vmMockQEMU.Template: not expected")
}
func (m *vmMockQEMU) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("vmMockQEMU.DetachDisk: not expected")
}
func (m *vmMockQEMU) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("vmMockQEMU.ResizeDisk: not expected")
}
func (m *vmMockQEMU) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("vmMockQEMU.Snapshot: not expected")
}
func (m *vmMockQEMU) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("vmMockQEMU.DeleteSnapshot: not expected")
}
func (m *vmMockQEMU) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("vmMockQEMU.ListSnapshots: not expected")
}
func (m *vmMockQEMU) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("vmMockQEMU.RollbackSnapshot: not expected")
}

// vmMockNodes embeds panicNodesStub and overrides the two methods create_vm uses.
type vmMockNodes struct {
	panicNodesStub

	updateConfigCalls []vmUpdateConfigCall
	deleteQemuCalls   []vmDeleteQemuCall

	updateConfigFn func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteQemuFn   func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
}

type vmUpdateConfigCall struct {
	node   string
	vmid   string
	params *sdknodes.UpdateQemuConfigParams
}

type vmDeleteQemuCall struct {
	node string
	vmid string
}

func (m *vmMockNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	m.updateConfigCalls = append(m.updateConfigCalls, vmUpdateConfigCall{node, vmid, params})
	if m.updateConfigFn != nil {
		return m.updateConfigFn(ctx, node, vmid, params)
	}
	return nil
}

func (m *vmMockNodes) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	m.deleteQemuCalls = append(m.deleteQemuCalls, vmDeleteQemuCall{node, vmid})
	if m.deleteQemuFn != nil {
		return m.deleteQemuFn(ctx, node, vmid, params)
	}
	raw := sdknodes.DeleteQemuResponse{}
	return &raw, nil
}

// vmMockCluster satisfies cluster.Service; only ListResources is needed for NextVMID.
type vmMockCluster struct {
	sdkcluster.Service // embed nil — panics on unmocked calls

	listResourcesFn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (m *vmMockCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if m.listResourcesFn != nil {
		return m.listResourcesFn(ctx, params)
	}
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

// vmMockAgent implements agent.Agent for create_vm tests.
type vmMockAgent struct {
	configureFn    func(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error
	configureCalls []vmConfigureCall
	removeFn       func(ctx context.Context, node string, vmid int) error
	removeCalls    []vmRemoveCall
}

type vmConfigureCall struct {
	node string
	vmid int
	cfg  agent.AgentConfig
}

type vmRemoveCall struct {
	node string
	vmid int
}

func (m *vmMockAgent) Configure(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error {
	m.configureCalls = append(m.configureCalls, vmConfigureCall{node, vmid, cfg})
	if m.configureFn != nil {
		return m.configureFn(ctx, node, vmid, cfg)
	}
	return nil
}
func (m *vmMockAgent) Remove(ctx context.Context, node string, vmid int) error {
	m.removeCalls = append(m.removeCalls, vmRemoveCall{node, vmid})
	if m.removeFn != nil {
		return m.removeFn(ctx, node, vmid)
	}
	return nil
}
func (m *vmMockAgent) UpdateDiskHints(_ context.Context, _ int, _ []agent.DiskHint) error {
	return nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func buildVMDeps(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			NetworkBridge:  "vmbr0",
			VMIDRangeStart: 100,
		},
		PVE: &mockPVEClient{
			qemuSvc:    q,
			nodesSvc:   n,
			clusterSvc: c,
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
		},
		Agent:  a,
		Logger: log.NewNopLogger(),
	}
}

func mkCtx(requestID string) jsonrpc.Context {
	return jsonrpc.Context{RequestID: requestID}
}

func mkArgs(agentID, stemcellCID string, cloudProps, networks, diskCIDs, env any) []json.RawMessage {
	return marshalArgs(agentID, stemcellCID, cloudProps, networks, diskCIDs, env)
}

func isCloudError(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*cpierrors.Error)
	return ok
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

// TestHandleCreateVM_HappyPath verifies the complete create_vm direct-import flow.
func TestHandleCreateVM_HappyPath(t *testing.T) {
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-uuid-1", testStemcellCID,
		map[string]any{"cores": 2, "memory": 1024},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": "10.0.0.5",
			"netmask": "255.255.255.0", "gateway": "10.0.0.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("happy-1"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("expected []any{vmCID, networks} len 2, got %T", result)
	}

	vmCID, ok := tuple[0].(string)
	if !ok || vmCID == "" {
		t.Fatalf("vm_cid must be non-empty string, got %v", tuple[0])
	}
	vmidInt, parseErr := strconv.Atoi(vmCID)
	if parseErr != nil || vmidInt < 100 {
		t.Errorf("vm_cid %q must be numeric VMID >= 100", vmCID)
	}

	// Verify QEMU.Create was called (not Clone) with the import-from directive.
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 QEMU.Create call, got %d", len(q.createCalls))
	}
	createParams := q.createCalls[0].params
	virtio0, ok := createParams["virtio0"].(string)
	if !ok {
		t.Fatalf("create params missing virtio0 key or wrong type: %v", createParams["virtio0"])
	}
	wantImportFrom := "import-from=" + testStemcellCID
	if !strings.Contains(virtio0, wantImportFrom) {
		t.Errorf("virtio0 %q must contain %q", virtio0, wantImportFrom)
	}
	if !strings.Contains(virtio0, "format=qcow2") {
		t.Errorf("virtio0 %q must contain format=qcow2", virtio0)
	}
	if boot, _ := createParams["boot"].(string); boot != "order=virtio0" {
		t.Errorf("create params boot = %q; want %q", boot, "order=virtio0")
	}
	if _, scsi0Present := createParams["scsi0"]; scsi0Present {
		t.Errorf("create params should not set scsi0 (system disk lives on virtio0)")
	}

	if len(n.updateConfigCalls) < 1 {
		t.Errorf("expected >= 1 UpdateQemuConfig call (NIC config), got %d", len(n.updateConfigCalls))
	}

	if len(a.configureCalls) != 1 {
		t.Fatalf("expected 1 agent.Configure call, got %d", len(a.configureCalls))
	}
	cfg := a.configureCalls[0].cfg
	if cfg.AgentID != "agent-uuid-1" {
		t.Errorf("AgentID: want agent-uuid-1, got %q", cfg.AgentID)
	}
	if cfg.Disks.System != "/dev/sda" {
		t.Errorf("Disks.System: want /dev/sda (mapped to /dev/vda by agent), got %q", cfg.Disks.System)
	}
	// Ephemeral disk path must be empty: the agent's
	// CreatePartitionIfNoEphemeralDisk=true setting carves the ephemeral
	// partition from the root disk. A hard-coded /dev/sdb would cause the
	// agent's DevicePathResolver to poll forever for a disk that never appears.
	if cfg.Disks.Ephemeral != "" {
		t.Errorf("Disks.Ephemeral: want empty (carved from root), got %q", cfg.Disks.Ephemeral)
	}
	if len(q.startCalls) != 1 {
		t.Fatalf("expected 1 start call, got %d", len(q.startCalls))
	}

	// Response networks: MAC must come from VM config.
	raw, _ := json.Marshal(tuple[1])
	var respNets map[string]json.RawMessage
	if err := json.Unmarshal(raw, &respNets); err != nil {
		t.Fatalf("response networks not a JSON object: %v", err)
	}
	var defNet map[string]json.RawMessage
	if err := json.Unmarshal(respNets["default"], &defNet); err != nil {
		t.Fatalf("response 'default' network not a JSON object: %v", err)
	}
	var mac string
	if err := json.Unmarshal(defNet["mac"], &mac); err != nil || mac != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("network MAC: want aa:bb:cc:dd:ee:ff, got %q (err=%v)", mac, err)
	}
}

// TestHandleCreateVM_VMIDAllocFail verifies cluster API failure propagates.
func TestHandleCreateVM_VMIDAllocFail(t *testing.T) {
	c := &vmMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, fmt.Errorf("cluster API unreachable")
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, c, &vmMockAgent{}))
	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("vmid-fail"))
	if err == nil {
		t.Fatal("expected error from VMID alloc failure")
	}
}

// TestHandleCreateVM_CreateFail verifies error returned with NO rollback
// (QEMU.Create itself failed — the VM was never created).
func TestHandleCreateVM_CreateFail(t *testing.T) {
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
			return "", fmt.Errorf("storage full")
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("create-fail"))
	if err == nil {
		t.Fatal("expected error from Create failure")
	}
	if len(n.deleteQemuCalls) != 0 {
		t.Errorf("expected 0 rollback calls (Create failed before VM existed), got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_ConfigUpdateFail verifies rollback after VM config update failure.
func TestHandleCreateVM_ConfigUpdateFail(t *testing.T) {
	callCount := 0
	n := &vmMockNodes{
		updateConfigFn: func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			callCount++
			if callCount == 1 {
				return fmt.Errorf("permission denied")
			}
			return nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("cfg-fail"))
	if err == nil {
		t.Fatal("expected error from config update failure")
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback delete, got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_DiskAttachFail verifies rollback after disk attach failure.
func TestHandleCreateVM_DiskAttachFail(t *testing.T) {
	q := &vmMockQEMU{
		attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "", fmt.Errorf("no such volume")
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{"local-lvm:vm-9001-disk-0"}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("disk-fail"))
	if err == nil {
		t.Fatal("expected error from disk attach failure")
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback delete, got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_AgentConfigureFail verifies rollback after agent.Configure failure.
func TestHandleCreateVM_AgentConfigureFail(t *testing.T) {
	a := &vmMockAgent{
		configureFn: func(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
			return fmt.Errorf("registry unreachable")
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, n, &vmMockCluster{}, a))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("agent-fail"))
	if err == nil {
		t.Fatal("expected error from agent.Configure failure")
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback delete, got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_StartFail verifies rollback after VM start failure.
func TestHandleCreateVM_StartFail(t *testing.T) {
	q := &vmMockQEMU{
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", fmt.Errorf("no free KVM slots")
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("start-fail"))
	if err == nil {
		t.Fatal("expected error from start failure")
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback delete, got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_MultipleNICs verifies multiple networks generate multiple net=/ipconfig= entries.
func TestHandleCreateVM_MultipleNICs(t *testing.T) {
	var capturedNICParams *sdknodes.UpdateQemuConfigParams
	callCount := 0
	n := &vmMockNodes{
		updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			callCount++
			if callCount == 1 {
				capturedNICParams = params
			}
			return nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "10.0.0.5", "netmask": "255.255.255.0",
				"gateway": "10.0.0.1", "cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
			"storage": map[string]any{
				"type": "manual", "ip": "192.168.1.10", "netmask": "255.255.255.0",
				"cloud_properties": map[string]any{"bridge": "vmbr1"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("multi-nic"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if capturedNICParams == nil {
		t.Fatal("NIC config params not captured from UpdateQemuConfig call")
	}
	if len(capturedNICParams.Net) != 2 {
		t.Errorf("Net map: want 2 entries, got %d: %v", len(capturedNICParams.Net), capturedNICParams.Net)
	}
	if len(capturedNICParams.Ipconfig) != 2 {
		t.Errorf("Ipconfig map: want 2 entries, got %d: %v", len(capturedNICParams.Ipconfig), capturedNICParams.Ipconfig)
	}
}

// TestHandleCreateVM_InvalidArgs verifies CloudErrors on bad arguments.
func TestHandleCreateVM_InvalidArgs(t *testing.T) {
	deps := buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	h := handlers.HandleCreateVM(deps)
	ctx := context.Background()

	// Too few args.
	_, err := h.Handle(ctx, marshalArgs("a"), mkCtx("few"))
	if err == nil {
		t.Fatal("expected error for too few args")
	}

	// Empty agent_id.
	_, err = h.Handle(ctx, mkArgs("", testStemcellCID, map[string]any{}, map[string]any{}, []string{}, map[string]any{}), mkCtx("empty-agent"))
	if err == nil {
		t.Fatal("expected error for empty agent_id")
	}

	// Integer-only stemcell_cid — legacy format rejected by ParseStemcellCID.
	_, err = h.Handle(ctx, mkArgs("a", "5042", map[string]any{}, map[string]any{}, []string{}, map[string]any{}), mkCtx("integer-stemcell"))
	if err == nil {
		t.Fatal("expected error for integer stemcell_cid")
	}

	// CID missing colon separator.
	_, err = h.Handle(ctx, mkArgs("a", "notavolid", map[string]any{}, map[string]any{}, []string{}, map[string]any{}), mkCtx("no-colon-stemcell"))
	if err == nil {
		t.Fatal("expected error for stemcell_cid without ':' separator")
	}

	// CID with wrong path prefix (no "import/").
	_, err = h.Handle(ctx, mkArgs("a", "local:images/foo.qcow2", map[string]any{}, map[string]any{}, []string{}, map[string]any{}), mkCtx("wrong-prefix-stemcell"))
	if err == nil {
		t.Fatal("expected error for stemcell_cid with non-import path prefix")
	}
}

// TestCreateVM_InvalidStemcellCID_Integer verifies that passing a bare integer
// stemcell CID (legacy clone-based format, e.g. "5042") is rejected by
// ParseStemcellCID and produces a CloudError — never an attempt to contact PVE.
func TestCreateVM_InvalidStemcellCID_Integer(t *testing.T) {
	deps := buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	h := handlers.HandleCreateVM(deps)

	_, err := h.Handle(
		context.Background(),
		mkArgs("agent-1", "5042", map[string]any{},
			map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
			[]string{}, map[string]any{}),
		mkCtx("integer-cid"),
	)
	if err == nil {
		t.Fatal("expected error for integer stemcell_cid")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error (CloudError), got %T: %v", err, err)
	}
}

// TestHandleCreateVM_MissingNode verifies CloudError when node not configured anywhere.
func TestHandleCreateVM_MissingNode(t *testing.T) {
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE: &mockPVEClient{
			qemuSvc:    &vmMockQEMU{},
			nodesSvc:   &vmMockNodes{},
			clusterSvc: &vmMockCluster{},
			tasksSvc:   &mockTasksService{},
		},
		Agent:  &vmMockAgent{},
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-1", testStemcellCID,
		map[string]any{}, // no target_node
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("no-node"))
	if err == nil {
		t.Fatal("expected error when node not configured")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestHandleCreateVM_DynamicNetwork verifies DHCP ipconfig for dynamic network.
func TestHandleCreateVM_DynamicNetwork(t *testing.T) {
	var capturedNICParams *sdknodes.UpdateQemuConfigParams
	callCount := 0
	n := &vmMockNodes{
		updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			callCount++
			if callCount == 1 {
				capturedNICParams = params
			}
			return nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("dynamic-net"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if capturedNICParams == nil {
		t.Fatal("NIC params not captured")
	}
	ipconf, ok := capturedNICParams.Ipconfig[0]
	if !ok {
		t.Fatal("expected ipconfig[0] for dynamic network")
	}
	if ipconf != "ip=dhcp" {
		t.Errorf("dynamic ipconfig[0]: want ip=dhcp, got %q", ipconf)
	}
}

// TestHandleCreateVM_EnvMBusBlobstore verifies mbus and blobstore propagate to agent config.
func TestHandleCreateVM_EnvMBusBlobstore(t *testing.T) {
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, a))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{},
		map[string]any{
			"mbus": "nats://user:pass@10.0.0.1:4222",
			"blobstore": map[string]any{
				"provider": "dav",
				"options":  map[string]any{"endpoint": "http://10.0.0.1:25250"},
			},
		})

	_, err := h.Handle(context.Background(), args, mkCtx("mbus-test"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(a.configureCalls) != 1 {
		t.Fatal("expected agent.Configure call")
	}
	cfg := a.configureCalls[0].cfg
	if cfg.MBus != "nats://user:pass@10.0.0.1:4222" {
		t.Errorf("MBus: want nats://user:pass@10.0.0.1:4222, got %q", cfg.MBus)
	}
	if cfg.Blobstore.Provider != "dav" {
		t.Errorf("Blobstore.Provider: want dav, got %q", cfg.Blobstore.Provider)
	}
}

// TestHandleCreateVM_TargetNodeFromCloudProperties verifies cloud_properties.target_node
// takes precedence over config.node.
func TestHandleCreateVM_TargetNodeFromCloudProperties(t *testing.T) {
	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]interface{}) (string, error) {
			createNode = node
			return "UPID:pve2:create:ok", nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID,
		map[string]any{"target_node": "pve2"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("target-node"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createNode != "pve2" {
		t.Errorf("create node: want pve2, got %q", createNode)
	}
}

// TestCreateVM_RejectsTooManyPersistentDisks confirms create_vm refuses
// a deployment carrying more than 28 persistent disks at creation time —
// CPI reserves scsi29 and scsi30 for headroom and the cloud-init drive.
func TestCreateVM_RejectsTooManyPersistentDisks(t *testing.T) {
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	tooMany := make([]string, 29)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("disk-%02d", i)
	}

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		tooMany, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("too-many-disks"))
	if err == nil {
		t.Fatal("expected create_vm to reject 29 persistent disks")
	}
	if !containsSubstr(err.Error(), "too many persistent disks") {
		t.Errorf("expected disk-cap error, got: %v", err)
	}
}

// TestCreateVM_RollbackRemovesConfigDriveISO confirms that when create_vm
// fails after agent.Configure has run, the rollback path also calls
// agent.Remove so ConfigDrive ISOs uploaded by the configdrive agent
// do not leak in storage.
func TestCreateVM_RollbackRemovesConfigDriveISO(t *testing.T) {
	q := &vmMockQEMU{
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", fmt.Errorf("simulated start failure")
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, a))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("rollback-remove"))
	if err == nil {
		t.Fatal("expected error from start failure")
	}
	if len(a.configureCalls) != 1 {
		t.Fatalf("expected agent.Configure to have run before failure, got %d calls", len(a.configureCalls))
	}
	if len(a.removeCalls) != 1 {
		t.Fatalf("expected exactly one agent.Remove during rollback, got %d", len(a.removeCalls))
	}
	configured := a.configureCalls[0]
	removed := a.removeCalls[0]
	if removed.vmid != configured.vmid || removed.node != configured.node {
		t.Errorf("agent.Remove(node=%q,vmid=%d) does not match Configure(node=%q,vmid=%d)",
			removed.node, removed.vmid, configured.node, configured.vmid)
	}
}

// TestCreateVM_RollbackTolerantToRemoveError ensures the rollback still
// completes and the original error propagates when agent.Remove itself
// fails — the agent error is logged but must not overwrite the cause.
func TestCreateVM_RollbackTolerantToRemoveError(t *testing.T) {
	startErr := fmt.Errorf("simulated start failure")
	q := &vmMockQEMU{
		startFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", startErr
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{
		removeFn: func(_ context.Context, _ string, _ int) error {
			return fmt.Errorf("simulated remove failure")
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, a))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("rollback-remove-err"))
	if err == nil {
		t.Fatal("expected original start error to propagate")
	}
	if !containsSubstr(err.Error(), "simulated start failure") {
		t.Errorf("expected original start failure in error chain, got %v", err)
	}
	if len(a.removeCalls) != 1 {
		t.Errorf("expected agent.Remove to be invoked once during rollback, got %d", len(a.removeCalls))
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected VM purge to complete even when agent.Remove errors, got %d", len(n.deleteQemuCalls))
	}
}
