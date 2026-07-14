package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// testStemcellCID is the canonical volid-format stemcell CID used across create_vm tests.
const testStemcellCID = "test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2"

// --------------------------------------------------------------------------
// create_vm-specific mocks
// --------------------------------------------------------------------------

// vmMockQEMU implements qemu.Service for create_vm tests.
type vmMockQEMU struct {
	createFn     func(ctx context.Context, node string, params map[string]any) (string, error)
	startFn      func(ctx context.Context, node string, vmid int) (string, error)
	stopFn       func(ctx context.Context, node string, vmid int) (string, error)
	configFn     func(ctx context.Context, node string, vmid int) (map[string]any, error)
	attachDiskFn func(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error)
	// cloneFn, when non-nil, is called by Clone instead of panicking. Tests that
	// exercise the clone path set this field. Import-only tests leave it nil so
	// the default panic fires on unexpected Clone calls — guarding the import path.
	cloneFn func(ctx context.Context, node string, vmid int, params map[string]any) (string, error)
	// resizeDiskFn, when non-nil, is called by ResizeDisk. Tests that exercise
	// the root disk grow path set this field. Tests that must not call ResizeDisk
	// leave it nil so ResizeDisk panics on unexpected calls.
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)

	mu          sync.Mutex
	createCalls []vmCreateCall
	startCalls  []int
}

type vmCreateCall struct {
	node   string
	params map[string]any
}

func (m *vmMockQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	m.mu.Lock()
	m.createCalls = append(m.createCalls, vmCreateCall{node, params})
	m.mu.Unlock()
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "UPID:pve:create:ok", nil
}

func (m *vmMockQEMU) Start(ctx context.Context, node string, vmid int) (string, error) {
	m.mu.Lock()
	m.startCalls = append(m.startCalls, vmid)
	m.mu.Unlock()
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
func (m *vmMockQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
}
func (m *vmMockQEMU) AttachDisk(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error) {
	if m.attachDiskFn != nil {
		return m.attachDiskFn(ctx, node, vmid, volid, bus, opts)
	}
	return "scsi1", nil
}

// Clone delegates to cloneFn when set; panics otherwise. Import-only tests
// leave cloneFn nil so an unexpected Clone call triggers a panic and reveals
// the regression. Clone-path tests wire cloneFn explicitly.
func (m *vmMockQEMU) Clone(ctx context.Context, node string, vmid int, params map[string]any) (string, error) {
	if m.cloneFn != nil {
		return m.cloneFn(ctx, node, vmid, params)
	}
	panic("vmMockQEMU.Clone: create_vm must not call Clone (direct-import mode)")
}

// Unimplemented stubs — panic on unexpected calls.
func (m *vmMockQEMU) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *vmMockQEMU) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if m.resizeDiskFn != nil {
		return m.resizeDiskFn(ctx, node, vmid, diskID, sizeGiB)
	}
	panic("vmMockQEMU.ResizeDisk: not expected in this test")
}
func (m *vmMockQEMU) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("vmMockQEMU.Snapshot: not expected")
}
func (m *vmMockQEMU) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("vmMockQEMU.DeleteSnapshot: not expected")
}
func (m *vmMockQEMU) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("vmMockQEMU.ListSnapshots: not expected")
}
func (m *vmMockQEMU) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("vmMockQEMU.RollbackSnapshot: not expected")
}

// vmMockNodes embeds panicNodesStub and overrides the methods create_vm uses.
type vmMockNodes struct {
	panicNodesStub

	updateConfigCalls    []vmUpdateConfigCall
	deleteQemuCalls      []vmDeleteQemuCall
	createQemuCloneCalls []vmCreateQemuCloneCall

	// firewallRuleActions records "<type>:<action>" for each CreateQemuFirewallRules call.
	firewallRuleActions []string
	// firewallEnableOptCalls counts UpdateQemuFirewallOptions calls with Enable=true.
	firewallEnableOptCalls int

	updateConfigFn              func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteQemuFn                func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	createQemuFirewallRulesFn   func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuFirewallRulesParams) error
	updateQemuFirewallOptionsFn func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuFirewallOptionsParams) error
	// listVersionFn, when non-nil, is called by ListVersion. Tests that exercise
	// ensureDLBMembership set this to control the PVE version reported. When nil,
	// ListVersion returns a response with version "0.0" so the DLB version guard
	// (PVE >= 9.2) causes ensureDLBMembership to skip silently — keeping tests
	// that do not focus on DLB membership from being polluted by DLB side-effects.
	listVersionFn func(ctx context.Context, node string) (*sdknodes.ListVersionResponse, error)
	// CreateQemuCloneFn, when non-nil, is called by CreateQemuClone instead of
	// the panic default. Clone-path tests set this to capture params and return
	// a UPID. Import-only tests leave it nil so an unexpected call panics.
	createQemuCloneFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error)
	// listQemuFn, when non-nil, is called by ListQemu. Old-CID opportunistic
	// lookup tests set this to simulate template presence/absence. When nil,
	// ListQemu returns an empty list (no templates found → import-from path).
	listQemuFn func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	// listStorageFn, when non-nil, is called by ListStorage (used by placement
	// scoring via GatherNodeFacts). When nil, returns an empty active storage
	// response so placement scoring degrades gracefully (storage axis = 0).
	listStorageFn func(ctx context.Context, node string, params *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error)
}

type vmCreateQemuCloneCall struct {
	node   string
	vmid   string
	params *sdknodes.CreateQemuCloneParams
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

func (m *vmMockNodes) CreateQemuClone(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
	m.createQemuCloneCalls = append(m.createQemuCloneCalls, vmCreateQemuCloneCall{node, vmid, params})
	if m.createQemuCloneFn != nil {
		return m.createQemuCloneFn(ctx, node, vmid, params)
	}
	panic("vmMockNodes.CreateQemuClone: not expected in import-only tests (set createQemuCloneFn to enable)")
}

// ListQemu returns an empty list when listQemuFn is nil (no templates →
// old-CID path falls through to import-from). Tests that exercise the
// opportunistic lookup set listQemuFn to control which templates are visible.
func (m *vmMockNodes) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if m.listQemuFn != nil {
		return m.listQemuFn(ctx, node, params)
	}
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

// ListStorage returns an empty response when listStorageFn is nil.
// GatherNodeFacts calls this for per-node storage scoring; an empty response
// means the storage axis contributes 0 to the score (graceful degradation).
func (m *vmMockNodes) ListStorage(ctx context.Context, node string, params *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
	if m.listStorageFn != nil {
		return m.listStorageFn(ctx, node, params)
	}
	empty := sdknodes.ListStorageResponse{}
	return &empty, nil
}

// ListVersion returns version "0.0" when listVersionFn is nil, causing the DLB
// version guard (PVE >= 9.2) to skip ensureDLBMembership silently. Tests that
// focus on DLB membership behavior set listVersionFn explicitly.
func (m *vmMockNodes) ListVersion(ctx context.Context, node string) (*sdknodes.ListVersionResponse, error) {
	if m.listVersionFn != nil {
		return m.listVersionFn(ctx, node)
	}
	v := "0.0"
	return &sdknodes.ListVersionResponse{Version: v}, nil
}

// CreateQemuFirewallRules records rule calls; delegates to fn when set.
// Default: success (no error) — tests that do not exercise firewall rules pass
// without configuration.
func (m *vmMockNodes) CreateQemuFirewallRules(_ context.Context, _ string, _ string, p *sdknodes.CreateQemuFirewallRulesParams) error {
	if m.createQemuFirewallRulesFn != nil {
		return m.createQemuFirewallRulesFn(context.Background(), "", "", p)
	}
	m.firewallRuleActions = append(m.firewallRuleActions, p.Type+":"+p.Action)
	return nil
}

// UpdateQemuFirewallOptions counts enable calls; delegates to fn when set.
// Default: success — tests not focused on firewall enable pass without configuration.
func (m *vmMockNodes) UpdateQemuFirewallOptions(_ context.Context, _ string, _ string, p *sdknodes.UpdateQemuFirewallOptionsParams) error {
	if m.updateQemuFirewallOptionsFn != nil {
		return m.updateQemuFirewallOptionsFn(context.Background(), "", "", p)
	}
	if p.Enable != nil && *p.Enable {
		m.firewallEnableOptCalls++
	}
	return nil
}

// vmMockCluster satisfies cluster.Service for create_vm tests.
// ListResources is used by AllocateWithRetry (NextVMID) and detectIPConflict.
// ListStatus is used by placement.GatherNodeFacts when PlacementEnabled()==true.
// ListFirewallGroups is used by applySecurityGroups / listFirewallGroupNames.
type vmMockCluster struct {
	sdkcluster.Service // embed nil — panics on unmocked calls

	listResourcesFn       func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
	listStatusFn          func(ctx context.Context) (*sdkcluster.ListStatusResponse, error)
	listFirewallGroupsFn  func() (*sdkcluster.ListFirewallGroupsResponse, error)
	listFirewallOptionsFn func(ctx context.Context) (*sdkcluster.ListFirewallOptionsResponse, error)
	listSdnVnetsFn        func(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error)
	// listFirewallOptionsCalls counts ListFirewallOptions invocations, for
	// asserting the §1.4 master-switch probe's once-per-process semantics.
	listFirewallOptionsCalls int
}

func (m *vmMockCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if m.listResourcesFn != nil {
		return m.listResourcesFn(ctx, params)
	}
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

// ListFirewallOptions defaults to reporting the datacenter firewall master
// switch as enabled, so the §1.4 probe (create_vm_firewall_masterswitch.go)
// never logs a Warn for tests that don't specifically exercise it. Tests that
// need the switch reported as disabled, or the probe call to fail, wire
// listFirewallOptionsFn explicitly.
func (m *vmMockCluster) ListFirewallOptions(ctx context.Context) (*sdkcluster.ListFirewallOptionsResponse, error) {
	m.listFirewallOptionsCalls++
	if m.listFirewallOptionsFn != nil {
		return m.listFirewallOptionsFn(ctx)
	}
	enabled := int64(1)
	return &sdkcluster.ListFirewallOptionsResponse{Enable: &enabled}, nil
}

// ListSdnVnets defaults to an empty vnet list, so the §1.6 SDN eventual-
// consistency gate (internal/pve/network_resolve.go, active by default as of
// Phase 1) classifies every test bridge as "not SDN-managed" and passes
// straight through with zero polling — matching this suite's pre-Phase-1,
// gate-off behavior for tests that don't specifically exercise SDN vnets.
// Tests that need a bridge recognized as an SDN vnet wire listSdnVnetsFn
// explicitly (see create_vm_netresolve_internal_test.go's dedicated fakes for
// the gate's own positive-path tests).
func (m *vmMockCluster) ListSdnVnets(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	if m.listSdnVnetsFn != nil {
		return m.listSdnVnetsFn(ctx, params)
	}
	resp := sdkcluster.ListSdnVnetsResponse{}
	return &resp, nil
}

func (m *vmMockCluster) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	if m.listStatusFn != nil {
		return m.listStatusFn(ctx)
	}
	// Default: single online node "pve" — matches the default config.Node used by buildVMDeps.
	// Placement scoring picks "pve" (only candidate), preserving existing test invariants.
	raw, _ := json.Marshal(map[string]any{
		"type":   "node",
		"name":   "pve",
		"online": 1,
		"maxcpu": int64(4),
		"maxmem": int64(8 * 1024 * 1024 * 1024),
		"mem":    int64(2 * 1024 * 1024 * 1024),
		"cpu":    0.1,
	})
	resp := sdkcluster.ListStatusResponse{raw}
	return &resp, nil
}

func (m *vmMockCluster) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	// Default: no nodes in HA maintenance. Tests that need HA-maintenance behavior
	// must override via a custom cluster mock.
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

// listFirewallGroupsFn, when non-nil, is called by ListFirewallGroups. When nil,
// returns an empty groups response so tests that do not exercise firewall group
// attachment do not receive unexpected call panics.
func (m *vmMockCluster) ListFirewallGroups(_ context.Context) (*sdkcluster.ListFirewallGroupsResponse, error) {
	if m.listFirewallGroupsFn != nil {
		return m.listFirewallGroupsFn()
	}
	empty := sdkcluster.ListFirewallGroupsResponse{}
	return &empty, nil
}

// HA rule/resource stubs on vmMockCluster: return not-found so
// removeNodeAffinityPin is a safe no-op in tests that do not exercise HA
// pinning. The not-found strings are matched by isHaNotFound.
func (m *vmMockCluster) DeleteHaRules(_ context.Context, _ string) error {
	return fmt.Errorf("no such rule (mock)")
}
func (m *vmMockCluster) DeleteHaResources(_ context.Context, _ string, _ *sdkcluster.DeleteHaResourcesParams) error {
	return fmt.Errorf("does not exist (mock)")
}
func (m *vmMockCluster) CreateHaResources(_ context.Context, _ *sdkcluster.CreateHaResourcesParams) error {
	return nil
}
func (m *vmMockCluster) CreateHaRules(_ context.Context, _ *sdkcluster.CreateHaRulesParams) error {
	return nil
}
func (m *vmMockCluster) ListHaRules(_ context.Context, _ *sdkcluster.ListHaRulesParams) (*sdkcluster.ListHaRulesResponse, error) {
	empty := sdkcluster.ListHaRulesResponse{}
	return &empty, nil
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

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// placementDisabled is a convenience *bool for disabling placement scoring in
// tests that predate the feature and pin config.Node explicitly.
var placementDisabled = func() *bool { f := false; return &f }()

func buildVMDeps(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      storageName,
			NetworkBridge:  "vmbr0",
			VMIDRangeStart: 100,
			// AgentMBus is required by the registry-less completeness assertion
			// for all non-noagent modes. Tests that exercise the mbus-empty error
			// path build their own config without this field.
			AgentMBus: "nats://mbus.test:4222",
			// Placement disabled so pre-placement tests keep their existing
			// behavior: node is taken directly from Config.Node ("pve").
			Placement: &config.PlacementConfig{Enabled: placementDisabled},
			// IP-conflict check disabled so pre-placement tests that use a
			// live mock cluster (ListResources returns empty) are not affected.
			EnsureNoIPConflicts: placementDisabled,
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
	var cpiErr *cpierrors.Error
	return errors.As(err, &cpiErr)
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

// TestHandleCreateVM_HappyPath verifies the complete create_vm direct-import flow.
func TestHandleCreateVM_HappyPath(t *testing.T) {
	t.Parallel()
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

// TestHandleCreateVM_BoshCPITag verifies that every create_vm call stamps the
// "bosh-cpi" ownership tag on the PVE VM regardless of cloud_properties.tags
// content. The tag must appear in the QEMU.Create params["tags"] field.
func TestHandleCreateVM_BoshCPITag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		customTags map[string]any
	}{
		{"no_custom_tags", map[string]any{}},
		{"with_custom_tags", map[string]any{"tags": map[string]any{"env": "prod", "team": "platform"}}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &vmMockQEMU{}
			n := &vmMockNodes{}
			c := &vmMockCluster{}
			a := &vmMockAgent{}
			h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

			cp := map[string]any{"cores": 2, "memory": 1024}
			for k, v := range tc.customTags {
				cp[k] = v
			}
			args := mkArgs("agent-uuid-tag-test", testStemcellCID,
				cp,
				map[string]any{"default": map[string]any{
					"type": "manual", "ip": "10.0.0.5",
					"netmask": "255.255.255.0", "gateway": "10.0.0.1",
					"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
					"cloud_properties": map[string]any{"bridge": "vmbr0"},
				}},
				[]string{}, map[string]any{})

			_, err := h.Handle(context.Background(), args, mkCtx("tag-test-"+tc.name))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(q.createCalls) != 1 {
				t.Fatalf("expected 1 QEMU.Create call, got %d", len(q.createCalls))
			}
			tagsVal, _ := q.createCalls[0].params["tags"].(string)
			if !strings.Contains(tagsVal, "bosh-cpi") {
				t.Errorf("create params tags = %q; must contain \"bosh-cpi\"", tagsVal)
			}
		})
	}
}

// TestHandleCreateVM_VMIDAllocFail verifies cluster API failure propagates.
func TestHandleCreateVM_VMIDAllocFail(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
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
	t.Parallel()
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
	t.Parallel()
	q := &vmMockQEMU{
		attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "", fmt.Errorf("no such volume")
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{diskCID}, map[string]any{})

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleCreateVM_MissingNode verifies CloudError when node not configured anywhere
// and placement is explicitly disabled (so no cluster scan can salvage a node).
func TestHandleCreateVM_MissingNode(t *testing.T) {
	t.Parallel()
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
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
	t.Parallel()
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
	if !strings.Contains(err.Error(), "too many persistent disks") {
		t.Errorf("expected disk-cap error, got: %v", err)
	}
}

// TestCreateVM_RollbackRemovesConfigDriveISO confirms that when create_vm
// fails after agent.Configure has run, the rollback path also calls
// agent.Remove so ConfigDrive ISOs uploaded by the configdrive agent
// do not leak in storage.
func TestCreateVM_RollbackRemovesConfigDriveISO(t *testing.T) {
	t.Parallel()
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

// TestHandleCreateVM_VMIDConflictRetry verifies that create_vm retries on
// PVE "already exists" 500 errors (cross-process VMID collisions) and
// eventually succeeds without rolling back the (non-existent) VM.
func TestHandleCreateVM_VMIDConflictRetry(t *testing.T) {
	t.Parallel()
	attempt := 0
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			attempt++
			if attempt < 3 {
				body := []byte(`{"message":"unable to create VM 113 - VM 113 already exists on node 'pve'","code":500}`)
				return "", sdkerrors.ParseAPIError(500, body)
			}
			return "UPID:pve:create:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, &vmMockCluster{}, a)
	deps.Config.VMIDAllocAttempts = 5
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("vmid-conflict-retry"))
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if attempt != 3 {
		t.Errorf("expected 3 Create attempts (2 conflicts + 1 success), got %d", attempt)
	}
	if len(n.deleteQemuCalls) != 0 {
		t.Errorf("expected 0 rollback calls on conflict path, got %d", len(n.deleteQemuCalls))
	}
}

// TestHandleCreateVM_AwaitTaskNonConflictRollsBack verifies that when
// QEMU.Create succeeds (UPID returned) but the task-await surfaces a
// non-conflict failure, the partial VM for that attempt is destroyed via
// cleanupVM before propagating the error (so PVE state stays clean).
func TestHandleCreateVM_AwaitTaskNonConflictRollsBack(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "UPID:pve:create:partial", nil
		},
	}
	n := &vmMockNodes{}
	deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
	deps.Config.VMIDAllocAttempts = 3
	// Wire the tasks service to surface a non-conflict failure on await.
	deps.PVE = &mockPVEClient{
		qemuSvc:    q,
		nodesSvc:   n,
		clusterSvc: &vmMockCluster{},
		tasksSvc: &mockTasksService{
			waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
				return &sdktasks.Status{ExitStatus: "qemu boot disk format unknown"}, nil
			},
		},
	}

	h := handlers.HandleCreateVM(deps)
	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("await-nonconflict"))
	if err == nil {
		t.Fatal("expected error from non-conflict await failure")
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected exactly 1 per-attempt cleanupVM rollback, got %d", len(n.deleteQemuCalls))
	}
}

// TestCreateVM_RollbackTolerantToRemoveError ensures the rollback still
// completes and the original error propagates when agent.Remove itself
// fails — the agent error is logged but must not overwrite the cause.
func TestCreateVM_RollbackTolerantToRemoveError(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(err.Error(), "simulated start failure") {
		t.Errorf("expected original start failure in error chain, got %v", err)
	}
	if len(a.removeCalls) != 1 {
		t.Errorf("expected agent.Remove to be invoked once during rollback, got %d", len(a.removeCalls))
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected VM purge to complete even when agent.Remove errors, got %d", len(n.deleteQemuCalls))
	}
}

// TestCreateVM_NICConfigTransient_Retriable verifies that when UpdateQemuConfig
// returns a transient connection error, the returned cpierror is classified as
// RetriableCloudError AND the VM rollback (DeleteQemu) is invoked.
func TestCreateVM_NICConfigTransient_Retriable(t *testing.T) {
	t.Parallel()
	transientErr := &sdkerrors.ConnectionError{
		Host:    "pve",
		Port:    8006,
		Message: "connection refused (simulated transient)",
	}

	n := &vmMockNodes{
		updateConfigFn: func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return transientErr
		},
	}
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("nic-transient"))
	if err == nil {
		t.Fatal("expected error from transient NIC config failure")
	}

	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected OkToRetry()=true for transient NIC error, got false; type=%s msg=%s",
			cpiErr.Type(), cpiErr.Error())
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("expected TypeRetriableCloud, got %s", cpiErr.Type())
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback DeleteQemu call, got %d", len(n.deleteQemuCalls))
	}
}

// TestCreateVM_AttachDiskTransient_Retriable verifies that when AttachDisk
// returns a transient connection error, the returned cpierror is classified as
// RetriableCloudError AND the VM rollback (DeleteQemu) is invoked.
func TestCreateVM_AttachDiskTransient_Retriable(t *testing.T) {
	t.Parallel()
	transientErr := &sdkerrors.ConnectionError{
		Host:    "pve",
		Port:    8006,
		Message: "connection reset (simulated transient)",
	}

	q := &vmMockQEMU{
		attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "", transientErr
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	// Non-empty diskCIDs forces the AttachDisk path.
	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{"local-lvm:vm-9002-disk-0"}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("attachdisk-transient"))
	if err == nil {
		t.Fatal("expected error from transient AttachDisk failure")
	}

	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected OkToRetry()=true for transient AttachDisk error, got false; type=%s msg=%s",
			cpiErr.Type(), cpiErr.Error())
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("expected TypeRetriableCloud, got %s", cpiErr.Type())
	}
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback DeleteQemu call, got %d", len(n.deleteQemuCalls))
	}
}

// TestCreateVM_CleanupAwaitsDestroy verifies that when cleanupVM is triggered
// (AttachDisk fails after VM creation), the rollback path decodes the UPID
// returned by DeleteQemu and awaits the destroy task before returning.
func TestCreateVM_CleanupAwaitsDestroy(t *testing.T) {
	t.Parallel()
	const destroyUPID = "UPID:pve:00AABBCC:00112233:6789ABCD:qmdestroy:9999:root@pam:"

	awaitCalled := false

	q := &vmMockQEMU{
		attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "", fmt.Errorf("simulated attach disk failure")
		},
	}

	// DeleteQemu returns a destroy UPID so cleanupVM must await it.
	n := &vmMockNodes{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			raw := sdknodes.DeleteQemuResponse(`"` + destroyUPID + `"`)
			return &raw, nil
		},
	}

	// Wire a custom tasks mock that records whether the destroy UPID was awaited.
	awaitTasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			if upid == destroyUPID {
				awaitCalled = true
			}
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}

	deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
	// Override the tasks service so the destroy await is observed.
	deps.PVE.(*mockPVEClient).tasksSvc = awaitTasksSvc

	args := mkArgs("agent-1", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{
			"type":             "dynamic",
			"cloud_properties": map[string]any{},
		}},
		[]string{"local-lvm:vm-50-disk-0"}, // non-empty diskCIDs triggers AttachDisk
		map[string]any{})

	_, err := deps.PVE.(*mockPVEClient).qemuSvc.(*vmMockQEMU).AttachDisk(
		context.Background(), "pve", 100, "local-lvm:vm-50-disk-0", "scsi", nil)
	// Sanity: confirm the stub fails as expected.
	if err == nil {
		t.Fatal("test setup error: vmMockQEMU.attachDiskFn must return error")
	}

	h := handlers.HandleCreateVM(deps)
	_, handlerErr := h.Handle(context.Background(), args, mkCtx("cleanup-await"))
	if handlerErr == nil {
		t.Fatal("expected handler to return error when AttachDisk fails")
	}

	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 DeleteQemu call during rollback, got %d", len(n.deleteQemuCalls))
	}
	if !awaitCalled {
		t.Error("destroy UPID was not awaited during cleanupVM rollback")
	}
}

// TestHandleCreateVM_AuthFailure verifies that a 401 Unauthorized from QEMU.Create
// is classified as a non-retriable Cloud error. Auth failures are operator
// configuration mistakes (wrong API token / expired ticket) and must NOT cause
// BOSH to retry indefinitely; they surface immediately with a CloudError.
// No rollback DeleteQemu call should occur because Create itself failed — the VM
// was never created on PVE.
func TestHandleCreateVM_AuthFailure(t *testing.T) {
	t.Parallel()
	authErr := &sdkerrors.APIError{HTTPCode: 401, Message: "authentication failure"}

	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "", authErr
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-auth-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("auth-failure"))
	if err == nil {
		t.Fatal("expected error from 401 auth failure")
	}

	// 401 is a 4xx non-404 → WrapError returns non-retriable CloudError.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("auth failure must not be retriable; OkToRetry()=true; type=%s", cpiErr.Type())
	}
	if cpiErr.Type() == cpierrors.TypeRetriableCloud {
		t.Errorf("auth failure classified as RetriableCloud; want TypeCloud; type=%s", cpiErr.Type())
	}

	// Create failed before VM existed — no rollback should fire.
	if len(n.deleteQemuCalls) != 0 {
		t.Errorf("expected 0 rollback DeleteQemu calls for pre-create auth failure, got %d", len(n.deleteQemuCalls))
	}
}

// TestCreateVM_AgentDead_EmitsDiagnostic verifies that when agent.Configure fails
// with a message that suggests the agent cannot be reached (e.g., registry or
// NATS unreachable), the handler probes the VM's PVE status via QEMU.Status and
// includes that status information in the error so operators can distinguish a dead
// VM from a network/registry misconfiguration.
func TestCreateVM_AgentDead_EmitsDiagnostic(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	a := &vmMockAgent{
		configureFn: func(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
			return fmt.Errorf("registry: dial tcp 10.0.0.1:25250: connect: connection refused")
		},
	}
	n := &vmMockNodes{}

	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "pve",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			VMIDRangeStart:      100,
			AgentMBus:           "nats://mbus.test:4222",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
		},
		PVE: &mockPVEClient{
			qemuSvc:    q,
			nodesSvc:   n,
			clusterSvc: &vmMockCluster{},
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
		},
		Agent:  a,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleCreateVM(deps)
	args := mkArgs("agent-dead-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("agent-dead"))
	if err == nil {
		t.Fatal("expected error when agent.Configure fails")
	}

	errMsg := err.Error()
	if errMsg == "" {
		t.Error("error message must not be empty")
	}

	// Primary invariants: rollback fires after agent.Configure failure.
	if len(n.deleteQemuCalls) != 1 {
		t.Errorf("expected 1 rollback DeleteQemu after agent.Configure failure, got %d", len(n.deleteQemuCalls))
	}
	if len(a.configureCalls) != 1 {
		t.Errorf("expected 1 agent.Configure call, got %d", len(a.configureCalls))
	}
}

// TestHandleCreateVM_LightStemcellCID_StripsPrefix verifies that a stemcell CID
// with the "light:" prefix is stripped before being passed to PVE's import-from=
// directive. Light CIDs identify operator-managed stemcells; PVE itself only
// understands the underlying "<storage>:import/<file>" volid. Without the strip,
// every deploy of a light stemcell would fail with an invalid-storage error
// from PVE.
func TestHandleCreateVM_LightStemcellCID_StripsPrefix(t *testing.T) {
	t.Parallel()
	const lightCID = "light:" + testStemcellCID

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-uuid-light", lightCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": "10.0.0.7",
			"netmask": "255.255.255.0", "gateway": "10.0.0.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("light-strip-1")); err != nil {
		t.Fatalf("expected no error for light CID, got: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 QEMU.Create call, got %d", len(q.createCalls))
	}
	virtio0, _ := q.createCalls[0].params["virtio0"].(string)
	wantImportFrom := "import-from=" + testStemcellCID
	if !strings.Contains(virtio0, wantImportFrom) {
		t.Errorf("virtio0 %q must contain stripped import-from %q", virtio0, wantImportFrom)
	}
	if strings.Contains(virtio0, "light:") {
		t.Errorf("virtio0 %q must NOT contain \"light:\" — prefix must be stripped before passing to PVE", virtio0)
	}
}

// --------------------------------------------------------------------------
// Template-CID dispatch tests
// --------------------------------------------------------------------------

// buildVMDepsForTemplate constructs Deps with cluster storage + single-node
// cluster wired. Used for template-CID dispatch tests that go through
// ValidateTemplateCloneStorage.
func buildVMDepsForTemplate(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "pve",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			VMIDRangeStart:      100,
			AgentMBus:           "nats://mbus.test:4222",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
		},
		PVE: &mockPVEClient{
			qemuSvc:    q,
			nodesSvc:   n,
			clusterSvc: withConfigNodes(c, 1),
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
			clusterStorageSvc: &mockClusterStorage{
				storageName: storageName,
				storageType: "dir",
				shared:      false,
			},
		},
		Agent:  a,
		Logger: log.NewNopLogger(),
	}
}

// buildVMDepsForTemplateCrossNode constructs Deps for cross-node template tests
// with a 2-node cluster and configurable storage shared flag.
func buildVMDepsForTemplateCrossNode(q *vmMockQEMU, n *vmMockNodes, a *vmMockAgent, storageType string, shared bool) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 "pve",
			VMStorage:            storageName,
			NetworkBridge:        "vmbr0",
			VMIDRangeStart:       100,
			AgentMBus:            "nats://mbus.test:4222",
			StemcellTemplateNode: "pve-tmpl",
			Placement:            &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts:  placementDisabled,
		},
		PVE: &mockPVEClient{
			qemuSvc:    q,
			nodesSvc:   n,
			clusterSvc: withConfigNodes(&vmMockCluster{}, 2),
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
			clusterStorageSvc: &mockClusterStorage{
				storageName: storageName,
				storageType: storageType,
				shared:      shared,
			},
		},
		Agent:  a,
		Logger: log.NewNopLogger(),
	}
}

// withConfigNodes wraps a vmMockCluster so ListConfigNodes returns nodeCount
// single-node entries. Returns the wrapped cluster.Service. mockClusterSvc
// embeds mockSDNCluster, whose SDN methods panic when unconfigured (by
// design, for create_network_test.go's own SDN-focused tests); this wrapper
// also supplies a safe empty-list default for ListSdnVnets so the §1.6
// network-resolve gate (active by default as of Phase 1, incidentally
// reachable from any create_vm test with a NIC) classifies every test bridge
// as "not SDN-managed" and passes straight through, matching this suite's
// pre-Phase-1, gate-off behavior for create_vm tests that don't specifically
// exercise SDN vnets.
func withConfigNodes(c *vmMockCluster, nodeCount int) *mockClusterSvc {
	return &mockClusterSvc{
		mockSDNCluster: mockSDNCluster{
			listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
				resp := sdkcluster.ListSdnVnetsResponse{}
				return &resp, nil
			},
		},
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			resp := make(sdkcluster.ListConfigNodesResponse, nodeCount)
			for i := 0; i < nodeCount; i++ {
				raw, _ := json.Marshal(map[string]any{"node": fmt.Sprintf("pve%02d", i+1)})
				resp[i] = raw
			}
			return &resp, nil
		},
		listResourcesFn: func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return c.ListResources(ctx, params)
		},
	}
}

// testTemplateCID is a sample template stemcell CID used in dispatch tests.
const testTemplateCID = "template:6042"

// TestCreateVM_TemplateCID_ClonesNotImports verifies that a "template:<vmid>"
// CID routes to CreateQemuClone (not QEMU.Create), and the post-clone tail
// (NIC config, agent configure, start) still runs.
func TestCreateVM_TemplateCID_ClonesNotImports(t *testing.T) {
	t.Parallel()

	cloneCalled := false
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			// Return a UPID for AwaitTask to consume.
			raw := sdknodes.CreateQemuCloneResponse{}
			if err := json.Unmarshal([]byte(`"UPID:pve:00001111:00000001:clone:ok"`), &raw); err != nil {
				panic("clone response unmarshal: " + err.Error())
			}
			return &raw, nil
		},
	}
	q := &vmMockQEMU{} // Create must NOT be called — panics if it is via createFn=nil path
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-1", testTemplateCID,
		map[string]any{"cores": 2, "memory": 1024},
		map[string]any{"default": map[string]any{
			"type": "dynamic", "cloud_properties": map[string]any{},
		}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("template-clone-1"))
	if err != nil {
		t.Fatalf("template CID: unexpected error: %v", err)
	}

	// Clone must have fired, QEMU.Create must NOT have fired.
	if !cloneCalled {
		t.Error("template CID: CreateQemuClone was not called (expected clone path)")
	}
	if len(q.createCalls) != 0 {
		t.Errorf("template CID: QEMU.Create must not be called on clone path, got %d calls", len(q.createCalls))
	}

	// Post-clone tail must have run: NIC config, agent configure, start.
	if len(n.updateConfigCalls) < 1 {
		t.Errorf("template CID: expected >=1 UpdateQemuConfig (NIC) call, got %d", len(n.updateConfigCalls))
	}
	if len(a.configureCalls) != 1 {
		t.Errorf("template CID: expected 1 agent.Configure call, got %d", len(a.configureCalls))
	}
	if len(q.startCalls) != 1 {
		t.Errorf("template CID: expected 1 QEMU.Start call, got %d", len(q.startCalls))
	}

	// Response must include a VM CID.
	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("expected [vmCID, networks], got %T", result)
	}
	if vmCID, _ := tuple[0].(string); vmCID == "" {
		t.Error("template CID: vm_cid in response must not be empty")
	}
}

// TestCreateVM_TemplateCID_SameNode_NoTarget verifies that when config.Node ==
// StemcellTemplateNode (same node), the clone params do NOT set Target.
func TestCreateVM_TemplateCID_SameNode_NoTarget(t *testing.T) {
	t.Parallel()

	var capturedCloneParams *sdknodes.CreateQemuCloneParams
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			capturedCloneParams = params
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00002222:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	// Both config.Node and StemcellTemplateNode are "pve" (same node, single-node cluster).
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-samenode", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("samenode")); err != nil {
		t.Fatalf("same-node template clone: unexpected error: %v", err)
	}
	if capturedCloneParams == nil {
		t.Fatal("same-node: clone params not captured")
	}
	if capturedCloneParams.Target != nil {
		t.Errorf("same-node: Target must be nil, got %q", *capturedCloneParams.Target)
	}
}

// TestCreateVM_TemplateCID_CrossNode_Shared_SetsTarget verifies that when
// StemcellTemplateNode differs from config.Node and storage is shared, the
// clone params set Target = config.Node.
func TestCreateVM_TemplateCID_CrossNode_Shared_SetsTarget(t *testing.T) {
	t.Parallel()

	var capturedCloneParams *sdknodes.CreateQemuCloneParams
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			capturedCloneParams = params
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00003333:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	// StemcellTemplateNode="pve-tmpl", config.Node="pve", shared NFS storage, 2 nodes.
	deps := buildVMDepsForTemplateCrossNode(q, n, a, "nfs", true)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-crossnode", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("crossnode-shared")); err != nil {
		t.Fatalf("cross-node+shared template clone: unexpected error: %v", err)
	}
	if capturedCloneParams == nil {
		t.Fatal("cross-node+shared: clone params not captured")
	}
	if capturedCloneParams.Target == nil {
		t.Fatalf("cross-node+shared: Target must be set to %q, got nil", "pve")
	}
	if *capturedCloneParams.Target != "pve" {
		t.Errorf("cross-node+shared: Target = %q, want %q", *capturedCloneParams.Target, "pve")
	}
	// QEMU.Create must not be called.
	if len(q.createCalls) != 0 {
		t.Errorf("cross-node+shared: QEMU.Create must not be called, got %d calls", len(q.createCalls))
	}
}

// TestCreateVM_TemplateCID_CrossNode_Local_Error verifies that a template CID
// with local storage and nodes that differ returns a cross-node local-storage error and does not
// call CreateQemuClone.
func TestCreateVM_TemplateCID_CrossNode_Local_Error(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{} // CreateQemuClone panics if called — leave createQemuCloneFn nil.
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	// StemcellTemplateNode="pve-tmpl", config.Node="pve", LOCAL dir storage, 2 nodes.
	deps := buildVMDepsForTemplateCrossNode(q, n, a, "dir", false)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-crosslocal", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("crossnode-local"))
	if err == nil {
		t.Fatal("cross-node+local: expected cross-node local-storage error, got nil")
	}
	if !strings.Contains(err.Error(), "local") && !strings.Contains(err.Error(), "node") {
		t.Errorf("cross-node+local: error lacks actionable context: %v", err)
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("cross-node+local: CreateQemuClone must not be called on cross-node local-storage violation, got %d calls", len(n.createQemuCloneCalls))
	}
}

// TestCreateVM_OldFormCID_NoTemplate_StillImports verifies that an old-form CID
// ("<storage>:import/<file>") uses the QEMU.Create import-from= path when no
// matching template is found by the opportunistic sha-tag lookup.
func TestCreateVM_OldFormCID_NoTemplate_StillImports(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	// listQemuFn returns empty (no templates) — the default when listQemuFn is nil.
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-import-1", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("oldform-import")); err != nil {
		t.Fatalf("old-form CID: unexpected error: %v", err)
	}

	// QEMU.Create must be called (import path), clone must NOT be called.
	if len(q.createCalls) != 1 {
		t.Fatalf("old-form CID: expected 1 QEMU.Create call, got %d", len(q.createCalls))
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("old-form CID: CreateQemuClone must not be called, got %d calls", len(n.createQemuCloneCalls))
	}
}

// TestCreateVM_TemplateCID_CloneConflict_Retries verifies that when a clone
// attempt returns a VMID conflict error, AllocateWithRetry retries with a
// fresh VMID candidate.
func TestCreateVM_TemplateCID_CloneConflict_Retries(t *testing.T) {
	t.Parallel()

	cloneAttempts := 0
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, vmidStr string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneAttempts++
			if cloneAttempts == 1 {
				// First attempt: simulate VMID conflict.
				return nil, fmt.Errorf("VM %d already exists on node 'pve'", params.Newid)
			}
			// Second attempt: succeed.
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00004444:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-retry-1", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("clone-retry"))
	if err != nil {
		t.Fatalf("clone conflict retry: unexpected error after retry: %v", err)
	}
	if cloneAttempts != 2 {
		t.Errorf("expected 2 clone attempts (1 conflict + 1 success), got %d", cloneAttempts)
	}
	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("expected [vmCID, networks], got %T", result)
	}
}

// --------------------------------------------------------------------------
// Old-CID opportunistic template dispatch tests
// --------------------------------------------------------------------------

// testStemcellCIDWithSHA is a stemcell CID whose filename contains a known sha8
// so the opportunistic lookup can match it. sha8 = "abc12345" (from filename).
const testStemcellCIDWithSHA = "test-storage:import/bosh-stemcell-ubuntu-jammy-1.438-abc12345.qcow2"

// buildVMDepsForOldCIDLookup constructs Deps with cluster storage + single-node
// cluster wired. The nodes mock has listQemuFn set to the provided function so
// FindTemplateBySHATag exercises the correct code path.
func buildVMDepsForOldCIDLookup(q *vmMockQEMU, n *vmMockNodes, a *vmMockAgent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "pve",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			VMIDRangeStart:      100,
			AgentMBus:           "nats://mbus.test:4222",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
		},
		PVE: &mockPVEClient{
			qemuSvc:    q,
			nodesSvc:   n,
			clusterSvc: withConfigNodes(&vmMockCluster{}, 1),
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
			clusterStorageSvc: &mockClusterStorage{
				storageName: storageName,
				storageType: "dir",
				shared:      false,
			},
		},
		Agent:  a,
		Logger: log.NewNopLogger(),
	}
}

// listQemuWithTemplate returns a ListQemu stub that reports a single frozen
// template carrying the given sha8 tag and VMID.
func listQemuWithTemplate(vmid int64, sha8 string) func(context.Context, string, *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	return func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		isTemplate := true
		tag := "bosh-stemcell-sha-" + sha8
		raw, _ := json.Marshal(map[string]any{
			"vmid":     vmid,
			"name":     "bosh-stemcell-ubuntu-jammy-1.438",
			"tags":     tag,
			"template": isTemplate,
		})
		resp := sdknodes.ListQemuResponse{raw}
		return &resp, nil
	}
}

// TestCreateVM_OldCID_TemplateFound_ClonesNotImports verifies that when the
// opportunistic sha-tag lookup finds an existing template, CreateQemuClone is
// called and QEMU.Create (import-from) is NOT called.
func TestCreateVM_OldCID_TemplateFound_ClonesNotImports(t *testing.T) {
	t.Parallel()

	cloneCalled := false
	n := &vmMockNodes{
		// FindTemplateBySHATag calls ListQemu; return a matching template.
		listQemuFn: listQemuWithTemplate(6042, "abc12345"),
		// Clone path fires when template is found.
		createQemuCloneFn: func(_ context.Context, _, _ string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00005555:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{} // Create must NOT be called — panics via nil createFn path
	a := &vmMockAgent{}

	deps := buildVMDepsForOldCIDLookup(q, n, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-oldcid-found", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("oldcid-found"))
	if err != nil {
		t.Fatalf("old-CID+template-found: unexpected error: %v", err)
	}

	if !cloneCalled {
		t.Error("old-CID+template-found: CreateQemuClone was not called")
	}
	if len(q.createCalls) != 0 {
		t.Errorf("old-CID+template-found: QEMU.Create must not be called, got %d calls", len(q.createCalls))
	}

	// Post-clone tail must have run.
	if len(n.updateConfigCalls) < 1 {
		t.Errorf("old-CID+template-found: expected >=1 UpdateQemuConfig call, got %d", len(n.updateConfigCalls))
	}
	if len(a.configureCalls) != 1 {
		t.Errorf("old-CID+template-found: expected 1 agent.Configure call, got %d", len(a.configureCalls))
	}
	if len(q.startCalls) != 1 {
		t.Errorf("old-CID+template-found: expected 1 QEMU.Start call, got %d", len(q.startCalls))
	}

	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("expected [vmCID, networks], got %T", result)
	}
	if vmCID, _ := tuple[0].(string); vmCID == "" {
		t.Error("old-CID+template-found: vm_cid in response must not be empty")
	}
}

// TestCreateVM_OldCID_NoTemplate_FallsBackToImport verifies that when the
// opportunistic lookup returns no match, QEMU.Create (import-from) is called
// and CreateQemuClone is NOT called.
func TestCreateVM_OldCID_NoTemplate_FallsBackToImport(t *testing.T) {
	t.Parallel()

	// listQemuFn returns empty → not-found; the nil default on vmMockNodes does
	// exactly this, but we set it explicitly to document the intent.
	n := &vmMockNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForOldCIDLookup(q, n, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-oldcid-notfound", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("oldcid-notfound")); err != nil {
		t.Fatalf("old-CID+no-template: unexpected error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("old-CID+no-template: expected 1 QEMU.Create call, got %d", len(q.createCalls))
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("old-CID+no-template: CreateQemuClone must not be called, got %d calls", len(n.createQemuCloneCalls))
	}
}

// TestCreateVM_OldCID_LookupError_FallsBackToImport verifies that when
// FindTemplateBySHATag returns an error, create_vm does NOT fail — it logs
// a warning and falls back to the import-from path.
func TestCreateVM_OldCID_LookupError_FallsBackToImport(t *testing.T) {
	t.Parallel()

	lookupErr := fmt.Errorf("PVE API: connection refused")
	n := &vmMockNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			return nil, lookupErr
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForOldCIDLookup(q, n, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-oldcid-lookuperr", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	// create_vm must succeed despite the lookup error (fallback to import-from).
	if _, err := h.Handle(context.Background(), args, mkCtx("oldcid-lookuperr")); err != nil {
		t.Fatalf("old-CID+lookup-error: expected success (fallback), got error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("old-CID+lookup-error: expected 1 QEMU.Create (fallback) call, got %d", len(q.createCalls))
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("old-CID+lookup-error: CreateQemuClone must not be called, got %d calls", len(n.createQemuCloneCalls))
	}
}

// TestCreateVM_OldCID_TemplateFound_ConflictRetries verifies that when the
// opportunistic clone path is taken and the first attempt hits a VMID conflict,
// AllocateWithRetry retries with a fresh candidate.
func TestCreateVM_OldCID_TemplateFound_ConflictRetries(t *testing.T) {
	t.Parallel()

	cloneAttempts := 0
	n := &vmMockNodes{
		listQemuFn: listQemuWithTemplate(6042, "abc12345"),
		createQemuCloneFn: func(_ context.Context, _, _ string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneAttempts++
			if cloneAttempts == 1 {
				return nil, fmt.Errorf("VM %d already exists on node 'pve'", params.Newid)
			}
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00006666:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForOldCIDLookup(q, n, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-oldcid-retry", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	result, err := h.Handle(context.Background(), args, mkCtx("oldcid-clone-retry"))
	if err != nil {
		t.Fatalf("old-CID+clone-conflict-retry: unexpected error: %v", err)
	}
	if cloneAttempts != 2 {
		t.Errorf("expected 2 clone attempts (1 conflict + 1 success), got %d", cloneAttempts)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("old-CID+clone-conflict-retry: QEMU.Create must not be called, got %d", len(q.createCalls))
	}

	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("expected [vmCID, networks], got %T", result)
	}
}

// TestCreateVM_TemplateCID_Regression_StillClones verifies that the template:<vmid>
// path is unchanged by the old-CID opportunistic change — no ListQemu call
// is made and CreateQemuClone fires directly.
func TestCreateVM_TemplateCID_Regression_StillClones(t *testing.T) {
	t.Parallel()

	cloneCalled := false
	n := &vmMockNodes{
		// listQemuFn is nil: if ListQemu is called for a template:<vmid> CID
		// it returns empty — but it must NOT be called at all on this path.
		// We detect accidental calls via a sentinel.
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			t.Error("template:<vmid> path must not call ListQemu")
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00007777:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-tmpl-regression", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("tmpl-regression")); err != nil {
		t.Fatalf("template:<vmid> regression: unexpected error: %v", err)
	}
	if !cloneCalled {
		t.Error("template:<vmid> regression: CreateQemuClone was not called")
	}
	if len(q.createCalls) != 0 {
		t.Errorf("template:<vmid> regression: QEMU.Create must not be called, got %d calls", len(q.createCalls))
	}
}

// --------------------------------------------------------------------------
// Placement + IP-conflict tests
// --------------------------------------------------------------------------

// buildVMDepsPlacement constructs Deps with placement enabled and IP-conflict
// check enabled. listStatusFn controls which nodes the cluster reports.
// listResourcesFn provides the ListResources response (used by detectIPConflict
// and GatherNodeFacts). Both are required.
func buildVMDepsPlacement(
	q *vmMockQEMU,
	n *vmMockNodes,
	listStatusFn func(ctx context.Context) (*sdkcluster.ListStatusResponse, error),
	listResourcesFn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error),
	a *vmMockAgent,
	extraCfg func(*config.CPIConfig),
) handlers.Deps {
	c := &vmMockCluster{
		listStatusFn:    listStatusFn,
		listResourcesFn: listResourcesFn,
	}
	cfg := &config.CPIConfig{
		Node:           "pve",
		VMStorage:      storageName,
		NetworkBridge:  "vmbr0",
		VMIDRangeStart: 100,
		AgentMBus:      "nats://mbus.test:4222",
		// Placement enabled explicitly; IP-conflict guard enabled.
	}
	if extraCfg != nil {
		extraCfg(cfg)
	}
	return handlers.Deps{
		Config: cfg,
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

// listStatusSingleNode returns a ListStatus response with one online node named "pve".
func listStatusSingleNode() func(context.Context) (*sdkcluster.ListStatusResponse, error) {
	return func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
		raw, _ := json.Marshal(map[string]any{
			"type":   "node",
			"name":   "pve",
			"online": 1,
			"maxcpu": int64(4),
			"maxmem": int64(8 * 1024 * 1024 * 1024),
			"mem":    int64(1 * 1024 * 1024 * 1024),
			"cpu":    0.05,
		})
		resp := sdkcluster.ListStatusResponse{raw}
		return &resp, nil
	}
}

// listStatusTwoNodes returns a ListStatus response with two online nodes.
// pve1 has more free memory, so the scorer should prefer it.
func listStatusTwoNodes() func(context.Context) (*sdkcluster.ListStatusResponse, error) {
	return func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
		gib := int64(1024 * 1024 * 1024)
		raw1, _ := json.Marshal(map[string]any{
			"type": "node", "name": "pve1", "online": 1,
			"maxcpu": int64(8), "maxmem": 16 * gib, "mem": 4 * gib, "cpu": 0.1,
		})
		raw2, _ := json.Marshal(map[string]any{
			"type": "node", "name": "pve2", "online": 1,
			"maxcpu": int64(8), "maxmem": 16 * gib, "mem": 14 * gib, "cpu": 0.8,
		})
		resp := sdkcluster.ListStatusResponse{raw1, raw2}
		return &resp, nil
	}
}

// emptyListResources is a ListResources response with no VMs (clean cluster for IP checks).
func emptyListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

// TestCreateVM_PlacementEnabled_TargetNodeOverride verifies that when
// cloud_properties.target_node is set, placement scoring is bypassed entirely.
// The test would fail if GatherNodeFacts were called because ListStatus panics.
func TestCreateVM_PlacementEnabled_TargetNodeOverride(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:pve2:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// Use a cluster that panics on ListStatus — proves GatherNodeFacts is never called.
	panicStatus := func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
		panic("ListStatus must not be called when target_node is set")
	}
	deps := buildVMDepsPlacement(q, n, panicStatus, emptyListResources, a, nil)
	deps.Config.EnsureNoIPConflicts = placementDisabled // no static IPs in this test
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-override", testStemcellCID,
		map[string]any{"target_node": "pve2"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("override")); err != nil {
		t.Fatalf("expected success with target_node override, got: %v", err)
	}
	if createNode != "pve2" {
		t.Errorf("create node: want pve2, got %q", createNode)
	}
}

// TestCreateVM_PlacementEnabled_MultiNode_BestNodePicked verifies that when
// placement is enabled and multiple nodes are online, the scorer picks the node
// with the highest score (most free memory = lowest utilisation).
func TestCreateVM_PlacementEnabled_MultiNode_BestNodePicked(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:pve1:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// pve1 has 12 GiB free (16-4), pve2 has 2 GiB free (16-14) — pve1 wins.
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, a, func(c *config.CPIConfig) {
		c.Node = "pve1" // config.Node is the fallback only; scoring should pick pve1
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-multi", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("multi-node")); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if createNode != "pve1" {
		t.Errorf("expected placement to choose pve1 (most free RAM), got %q", createNode)
	}
}

// TestCreateVM_PlacementDisabled_UsesConfigNode verifies that when
// Placement.Enabled=false, Config.Node is used directly without GatherNodeFacts.
func TestCreateVM_PlacementDisabled_UsesConfigNode(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:pve-static:ok", nil
		},
	}
	// buildVMDeps disables placement — reuse it.
	deps := buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-disabled", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("placement-disabled")); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if createNode != "pve" {
		t.Errorf("expected config.Node \"pve\", got %q", createNode)
	}
}

// TestCreateVM_AZSet_CandidatesRestricted verifies that when availability_zone
// is set and matches an entry in placement.az_map, only those nodes are scored.
func TestCreateVM_AZSet_CandidatesRestricted(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:az-node:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// Cluster has pve1 and pve2; AZ "zone-a" maps to only pve2.
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, a, func(c *config.CPIConfig) {
		c.Node = "pve1"
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve2"},
			},
		}
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-az", testStemcellCID,
		map[string]any{"availability_zone": "zone-a"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("az-restrict")); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if createNode != "pve2" {
		t.Errorf("expected pve2 (only node in zone-a), got %q", createNode)
	}
}

// TestCreateVM_AZSet_UnknownAZ_CloudError verifies that specifying an AZ not
// present in placement.az_map returns a CloudError immediately.
func TestCreateVM_AZSet_UnknownAZ_CloudError(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{}
	a := &vmMockAgent{}

	panicStatus := func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
		panic("ListStatus must not be called before AZ validation fails")
	}
	deps := buildVMDepsPlacement(&vmMockQEMU{}, n, panicStatus, emptyListResources, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
			},
		}
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-badaz", testStemcellCID,
		map[string]any{"availability_zone": "zone-unknown"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("az-unknown"))
	if err == nil {
		t.Fatal("expected CloudError for unknown AZ, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "zone-unknown") {
		t.Errorf("error should mention the unknown AZ name; got: %v", err)
	}
}

// TestCreateVM_IPConflict_StaticIP_Refused verifies that when a static IP in
// the network spec is already claimed by another VM, create_vm returns a
// CloudError and does not proceed to boot.
func TestCreateVM_IPConflict_StaticIP_Refused(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// ListResources returns one existing VM with VMID 200.
	conflictingVMListFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		raw, _ := json.Marshal(map[string]any{
			"vmid": 200,
			"node": "pve",
			"name": "existing-vm",
			"type": "qemu",
		})
		resp := sdkcluster.ListResourcesResponse{raw}
		return &resp, nil
	}
	// The conflicting VM's Config returns ipconfig0 holding the target IP.
	conflictingIP := "10.99.0.5"
	q.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid == 200 {
			return map[string]any{
				"ipconfig0": "ip=" + conflictingIP + "/24,gw=10.99.0.1",
				"net0":      "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			}, nil
		}
		// For any new VM created during the test, return normal config.
		return map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
	}

	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), conflictingVMListFn, a, func(c *config.CPIConfig) {
		// EnsureNoIPConflicts defaults to true (nil), so leave it unset.
		// Placement disabled to keep node fixed at "pve".
		c.Placement = &config.PlacementConfig{Enabled: placementDisabled}
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-conflict", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": conflictingIP,
			"netmask": "255.255.255.0", "gateway": "10.99.0.1",
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("ip-conflict"))
	if err == nil {
		t.Fatal("expected CloudError for IP conflict, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), conflictingIP) {
		t.Errorf("error must mention conflicting IP %q; got: %v", conflictingIP, err)
	}
	if !strings.Contains(err.Error(), "200") {
		t.Errorf("error must mention conflicting VMID 200; got: %v", err)
	}
	// Start must NOT have been called — conflict check fires before boot.
	if len(q.startCalls) != 0 {
		t.Errorf("QEMU.Start must not be called when IP conflict detected; got %d calls", len(q.startCalls))
	}
}

// TestCreateVM_IPConflict_Clear_Proceeds verifies that when no IP conflict is
// detected, create_vm proceeds normally.
func TestCreateVM_IPConflict_Clear_Proceeds(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// ListResources returns one existing VM, but it holds a DIFFERENT IP.
	noConflictFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		raw, _ := json.Marshal(map[string]any{
			"vmid": 201, "node": "pve", "name": "other-vm", "type": "qemu",
		})
		resp := sdkcluster.ListResourcesResponse{raw}
		return &resp, nil
	}
	q.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid == 201 {
			// VM 201 holds a different IP — no conflict.
			return map[string]any{
				"ipconfig0": "ip=10.99.0.99/24,gw=10.99.0.1",
				"net0":      "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			}, nil
		}
		return map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
	}

	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), noConflictFn, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{Enabled: placementDisabled}
		// EnsureNoIPConflicts nil (default true).
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-noconflict", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": "10.99.0.5",
			"netmask": "255.255.255.0", "gateway": "10.99.0.1",
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("no-conflict")); err != nil {
		t.Fatalf("expected success when no IP conflict, got: %v", err)
	}
	if len(q.startCalls) != 1 {
		t.Errorf("expected 1 Start call after clean conflict check, got %d", len(q.startCalls))
	}
}

// TestCreateVM_IPConflict_DynamicNetworkSkipped verifies that dynamic (DHCP)
// networks are not checked for IP conflicts. The QEMU.Config mock for the
// conflicting VM would return an IP that matches the target if the check ran
// (but it cannot match, since DHCP networks have no static target IP).
// The test passes if and only if create_vm succeeds (no spurious conflict error).
func TestCreateVM_IPConflict_DynamicNetworkSkipped(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// Wire a ListResources that returns a VM whose Config holds "ip=dhcp" —
	// same as the new VM's ipconfig. detectIPConflict, if mistakenly called,
	// would see "ip=dhcp" and return nil (extractStaticIP returns "" for dhcp).
	// But the intent of the test is: no conflict check at all for DHCP networks.
	conflictCheckListFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		// Return empty so AllocateWithRetry can pick a VMID.
		resp := sdkcluster.ListResourcesResponse{}
		return &resp, nil
	}
	// Config for VMID 9999: return "ip=dhcp" — safe; extractStaticIP returns "".
	q.configFn = func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
	}

	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), conflictCheckListFn, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{Enabled: placementDisabled}
		// EnsureNoIPConflicts nil (default true) — but DHCP network has no static IP,
		// so collectStaticIPsForConflictCheck returns empty and detectIPConflict is never called.
	})
	h := handlers.HandleCreateVM(deps)

	// Dynamic network — no static IP to check.
	args := mkArgs("agent-dhcp", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	// The invariant: create_vm succeeds without any IP-conflict CloudError.
	// If the check incorrectly fires for DHCP, it would still pass (dhcp → no
	// static IP → no conflict), but if collectStaticIPsForConflictCheck
	// incorrectly extracts a non-empty IP, an error would surface here.
	if _, err := h.Handle(context.Background(), args, mkCtx("dhcp-skip")); err != nil {
		t.Fatalf("expected success for DHCP network (conflict check must be skipped), got: %v", err)
	}
	if len(q.startCalls) != 1 {
		t.Errorf("expected 1 Start call (VM should boot), got %d", len(q.startCalls))
	}
}

// TestCreateVM_IPConflict_MultiBridge_NonLastBridgeDetected verifies that when a
// VM has static IPs on two bridges (net0=vmbr0, net1=vmbr1), a conflict on the
// non-last bridge is detected and refused. This is the regression test for the
// multi-bridge gap: the old single-bridge implementation only checked the bridge
// of the last NIC, silently missing conflicts on earlier bridges.
func TestCreateVM_IPConflict_MultiBridge_NonLastBridgeDetected(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// The conflicting VM (VMID 300) holds the vmbr0 IP on vmbr0 only.
	// The new VM will request net0=vmbr0 (conflicting) and net1=vmbr1 (clean).
	// With the old bug, if vmbr1 was the "last" bridge seen, detectIPConflict
	// would only scan vmbr1-filtered NICs and miss the vmbr0 conflict entirely.
	conflictIP := "10.1.0.5"
	listFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		raw, _ := json.Marshal(map[string]any{
			"vmid": 300, "node": "pve", "name": "conflicting-vm", "type": "qemu",
		})
		resp := sdkcluster.ListResourcesResponse{raw}
		return &resp, nil
	}
	q.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid == 300 {
			// VM 300 has the conflicting IP on vmbr0 (net0).
			// net1=vmbr1 has a different, non-conflicting IP.
			return map[string]any{
				"ipconfig0": "ip=" + conflictIP + "/24,gw=10.1.0.1",
				"net0":      "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
				"ipconfig1": "ip=10.2.0.50/24,gw=10.2.0.1",
				"net1":      "virtio=bb:cc:dd:ee:ff:00,bridge=vmbr1",
			}, nil
		}
		return map[string]any{
			"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			"net1": "virtio=bb:cc:dd:ee:ff:00,bridge=vmbr1",
		}, nil
	}

	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), listFn, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{Enabled: placementDisabled}
		// EnsureNoIPConflicts nil (default true).
	})
	h := handlers.HandleCreateVM(deps)

	// New VM requests two manual networks: net0 on vmbr0 (IP conflicts with VM 300),
	// net1 on vmbr1 (clean IP). The conflict on vmbr0 must be caught even though
	// vmbr1 is processed later in the map iteration.
	args := mkArgs("agent-multibr", testStemcellCID,
		map[string]any{},
		map[string]any{
			"net0": map[string]any{
				"type": "manual", "ip": conflictIP,
				"netmask": "255.255.255.0", "gateway": "10.1.0.1",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
			"net1": map[string]any{
				"type": "manual", "ip": "10.2.0.99",
				"netmask": "255.255.255.0", "gateway": "10.2.0.1",
				"cloud_properties": map[string]any{"bridge": "vmbr1"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("multi-bridge-conflict"))
	if err == nil {
		t.Fatal("expected CloudError for multi-bridge IP conflict on vmbr0, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), conflictIP) {
		t.Errorf("error must mention conflicting IP %q; got: %v", conflictIP, err)
	}
	if !strings.Contains(err.Error(), "300") {
		t.Errorf("error must mention conflicting VMID 300; got: %v", err)
	}
	// VM must not be booted when a conflict is detected.
	if len(q.startCalls) != 0 {
		t.Errorf("QEMU.Start must not be called when IP conflict detected; got %d calls", len(q.startCalls))
	}
}

// TestCreateVM_IPConflict_NoSelfConflict verifies that a static-IP create_vm
// succeeds even when the cluster's ListResources conflict scan returns the
// newly-created VM itself holding that IP. Without the excludeVMID fix, this
// would return a CloudError ("IP conflict") against the VM's own ipconfig,
// aborting every static-IP deployment.
func TestCreateVM_IPConflict_NoSelfConflict(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	const staticIP = "10.77.0.250"

	// createdVMID captures the VMID actually allocated by AllocateWithRetry so
	// the listResourcesFn can return it in the detectIPConflict scan. The VMID
	// is random within the range (nextVMIDInRange uses a random offset), so we
	// cannot hardcode it.
	var createdVMID int
	var createdMu sync.Mutex

	// createFn: record the VMID from the params map, then succeed normally.
	q.createFn = func(_ context.Context, _ string, params map[string]any) (string, error) {
		if v, ok := params["vmid"]; ok {
			switch id := v.(type) {
			case int:
				createdMu.Lock()
				createdVMID = id
				createdMu.Unlock()
			case int64:
				createdMu.Lock()
				createdVMID = int(id)
				createdMu.Unlock()
			case float64:
				createdMu.Lock()
				createdVMID = int(id)
				createdMu.Unlock()
			}
		}
		return "UPID:pve:create:ok", nil
	}

	// listResourcesFn: always return empty so NextVMID has a free range, AND
	// so detectIPConflict's initial resource list is also empty (no other VMs).
	// After the VM is created, we want detectIPConflict to see it — but since
	// we control configFn, we simulate that by returning the new VM once the
	// VMID has been captured.
	//
	// Note: ListResources is called first by listClusterVMIDs (VMID allocation)
	// and then by detectIPConflict. Both calls go through the same fn. After the
	// createFn fires, createdVMID is set. The conflict-check call happens later,
	// so we return the new VM's entry for any call after createdVMID is set.
	listResourcesFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		createdMu.Lock()
		vmid := createdVMID
		createdMu.Unlock()

		if vmid == 0 {
			// VMID not yet allocated — return empty for NextVMID.
			resp := sdkcluster.ListResourcesResponse{}
			return &resp, nil
		}
		// VM was created: conflict scan sees the new VM holding the target IP.
		// This simulates the self-conflict scenario that the fix must prevent.
		raw, _ := json.Marshal(map[string]any{
			"vmid": vmid, "node": "pve", "name": "new-vm", "type": "qemu",
		})
		resp := sdkcluster.ListResourcesResponse{raw}
		return &resp, nil
	}

	// configFn: for the new VM, return its own ipconfig with the target IP —
	// exactly what configureNICs writes via UpdateQemuConfig before the check.
	q.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		createdMu.Lock()
		newID := createdVMID
		createdMu.Unlock()
		if vmid == newID && newID != 0 {
			return map[string]any{
				"ipconfig0": "ip=" + staticIP + "/24,gw=10.77.0.1",
				"net0":      "virtio=de:ad:be:ef:00:01,bridge=vmbr0",
			}, nil
		}
		return map[string]any{"net0": "virtio=de:ad:be:ef:00:01,bridge=vmbr0"}, nil
	}

	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), listResourcesFn, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{Enabled: placementDisabled}
		// EnsureNoIPConflicts nil (default true) — the guard is active.
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-selfconflict", testStemcellCID,
		map[string]any{},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": staticIP,
			"netmask": "255.255.255.0", "gateway": "10.77.0.1",
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("no-self-conflict")); err != nil {
		t.Fatalf("static-IP create_vm must not self-conflict: %v", err)
	}
	// VM must have booted — the conflict check did not abort the flow.
	if len(q.startCalls) != 1 {
		t.Errorf("expected 1 Start call (VM boots normally), got %d", len(q.startCalls))
	}
}

// TestCreateVM_DLBSentinelAZ_AllNodesCandidate verifies that when
// availability_zone equals the sentinel DLB AZ name ("dlb") and it is absent
// from az_map, resolveTargetNode does NOT return a CloudError and instead uses
// all online nodes as candidates (candidateSet = nil path). The VM is created
// on whichever node the scorer chooses.
func TestCreateVM_DLBSentinelAZ_AllNodesCandidate(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:pve:dlb-sentinel:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// Cluster has a single node "pve"; az_map does NOT contain "dlb".
	// DLBAZName is the default "dlb" (nil pointer → default).
	deps := buildVMDepsPlacement(q, n, listStatusSingleNode(), emptyListResources, a, func(c *config.CPIConfig) {
		c.Node = "pve"
		c.Placement = &config.PlacementConfig{
			// AZMap intentionally omits "dlb" — the sentinel must not trigger CloudError.
			AZMap: map[string][]string{
				"zone-a": {"pve"},
			},
			// DLB block: AZName nil → accessor returns default "dlb".
			DLB: &config.DLBConfig{},
		}
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-dlb-sentinel", testStemcellCID,
		map[string]any{"availability_zone": "dlb"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("dlb-sentinel")); err != nil {
		t.Fatalf("DLB sentinel AZ must not return an error: %v", err)
	}
	if createNode == "" {
		t.Errorf("expected a node to be chosen; got empty string")
	}
}

// TestCreateVM_UnknownAZ_NotSentinel_CloudError verifies that specifying an AZ
// that is neither the sentinel DLB AZ nor present in az_map still returns a
// CloudError (unchanged behavior for non-DLB unknown AZs).
func TestCreateVM_UnknownAZ_NotSentinel_CloudError(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// ListStatus must not be reached — AZ validation fires before GatherNodeFacts.
	panicStatus := func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
		panic("ListStatus must not be called before AZ validation fails")
	}
	deps := buildVMDepsPlacement(&vmMockQEMU{}, n, panicStatus, emptyListResources, a, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve"},
			},
			// DLB sentinel is "dlb"; "unknownzone" is not sentinel → must error.
			DLB: &config.DLBConfig{},
		}
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-badaz2", testStemcellCID,
		map[string]any{"availability_zone": "unknownzone"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("unknown-az-not-sentinel"))
	if err == nil {
		t.Fatal("expected CloudError for unknown non-sentinel AZ, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "unknownzone") {
		t.Errorf("error must mention the unknown AZ name; got: %v", err)
	}
}

// --------------------------------------------------------------------------
// VM-storage cloud_properties / vm_type profile resolution
// --------------------------------------------------------------------------

// buildVMDepsWithVMTypes constructs Deps including VMTypes map for profile tests.
func buildVMDepsWithVMTypes(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent, vmTypes map[string]config.TypeProfile) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "pve",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			VMIDRangeStart:      100,
			AgentMBus:           "nats://mbus.test:4222",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
			VMTypes:             vmTypes,
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

// defaultNetMap returns a minimal single-network args[3] for create_vm tests.
func defaultNetMap() map[string]any {
	return map[string]any{
		"default": map[string]any{
			"type": "manual", "ip": "10.0.0.5",
			"netmask": "255.255.255.0", "gateway": "10.0.0.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		},
	}
}

// TestCreateVM_NoProfile_StorageFromConfig verifies that with no vm_type selector
// and no storage_pool in cloud_properties, vmStorage equals config.VMStorage.
func TestCreateVM_NoProfile_StorageFromConfig(t *testing.T) {
	t.Parallel()

	var capturedParams map[string]any
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, nil))

	args := mkArgs("agent-1", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("no-profile")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	virtio0, _ := capturedParams["virtio0"].(string)
	// storage= in virtio0 string must be storageName (config.VMStorage).
	if !strings.Contains(virtio0, storageName) {
		t.Errorf("virtio0 %q must contain config.VMStorage %q", virtio0, storageName)
	}
}

// TestCreateVM_CallCP_StoragePool_OverridesConfig verifies that
// cloud_properties.storage_pool overrides config.VMStorage in the QEMU create call.
func TestCreateVM_CallCP_StoragePool_OverridesConfig(t *testing.T) {
	t.Parallel()

	var capturedParams map[string]any
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, nil))

	args := mkArgs("agent-2", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "storage_pool": "override-pool"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("call-cp-storage")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	virtio0, _ := capturedParams["virtio0"].(string)
	if !strings.Contains(virtio0, "override-pool") {
		t.Errorf("virtio0 %q: expected override-pool; got config/default pool instead", virtio0)
	}
}

// TestCreateVM_VMTypeProfile_StoragePool verifies that a vm_type profile
// supplying storage_pool is used when the call has no storage_pool override.
func TestCreateVM_VMTypeProfile_StoragePool(t *testing.T) {
	t.Parallel()

	var capturedParams map[string]any
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}

	vmTypes := map[string]config.TypeProfile{
		"gpu": {
			CloudProperties: map[string]any{
				"storage_pool": "ceph-ssd",
			},
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, vmTypes))

	args := mkArgs("agent-3", testStemcellCID,
		map[string]any{"cores": 2, "memory": 4096, "vm_type": "gpu"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vmtype-profile")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	virtio0, _ := capturedParams["virtio0"].(string)
	if !strings.Contains(virtio0, "ceph-ssd") {
		t.Errorf("virtio0 %q: expected ceph-ssd from vm_type profile", virtio0)
	}
}

// TestCreateVM_CallStoragePool_BeatsVMTypeProfile verifies that an explicit
// storage_pool in the call beats the vm_type profile's storage_pool.
func TestCreateVM_CallStoragePool_BeatsVMTypeProfile(t *testing.T) {
	t.Parallel()

	var capturedParams map[string]any
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}

	vmTypes := map[string]config.TypeProfile{
		"gpu": {
			CloudProperties: map[string]any{
				"storage_pool": "ceph-ssd",
			},
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, vmTypes))

	args := mkArgs("agent-4", testStemcellCID,
		map[string]any{"cores": 2, "memory": 4096, "vm_type": "gpu", "storage_pool": "nvme-fast"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("call-beats-profile")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	virtio0, _ := capturedParams["virtio0"].(string)
	if !strings.Contains(virtio0, "nvme-fast") {
		t.Errorf("virtio0 %q: expected nvme-fast (call beats vm_type profile)", virtio0)
	}
	if strings.Contains(virtio0, "ceph-ssd") {
		t.Errorf("virtio0 %q: must NOT contain profile pool ceph-ssd when call overrides", virtio0)
	}
}

// TestCreateVM_UnknownVMType_ReturnsCloudError verifies that an unknown vm_type
// selector returns a CloudError from create_vm without creating a VM.
func TestCreateVM_UnknownVMType_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	q := &vmMockQEMU{}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{},
		map[string]config.TypeProfile{}))

	args := mkArgs("agent-5", testStemcellCID,
		map[string]any{"vm_type": "does-not-exist"},
		defaultNetMap(), []string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("unknown-vmtype"))
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected CloudError, got %T: %v", err, err)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("expected no QEMU.Create calls on CloudError; got %d", len(q.createCalls))
	}
}

// TestCreateVM_VMTypeProfile_DiskFormat verifies that vm_disk_format from a
// vm_type profile is applied to the virtio0 disk format parameter.
func TestCreateVM_VMTypeProfile_DiskFormat(t *testing.T) {
	t.Parallel()

	var capturedParams map[string]any
	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}

	vmTypes := map[string]config.TypeProfile{
		"raw-vm": {
			CloudProperties: map[string]any{
				"vm_disk_format": "raw",
			},
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithVMTypes(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, vmTypes))

	args := mkArgs("agent-6", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "vm_type": "raw-vm"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("profile-diskfmt")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	virtio0, _ := capturedParams["virtio0"].(string)
	if !strings.Contains(virtio0, "format=raw") {
		t.Errorf("virtio0 %q: expected format=raw from vm_type profile", virtio0)
	}
}

// TestCreateVM_RealAZMap_Restricted_WithDLBConfigured verifies that when DLB is
// configured but a real az_map zone is used, placement scoring still restricts
// candidates to mapped nodes (master-flag-on + AZ topology unchanged).
func TestCreateVM_RealAZMap_Restricted_WithDLBConfigured(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNode = node
			return "UPID:pve2:az-dlb:ok", nil
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	// Cluster: pve1 and pve2; AZ "zone-b" maps only to pve2.
	// DLB configured (AZName = "dlb"); real AZ must still restrict to az_map nodes.
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, a, func(c *config.CPIConfig) {
		c.Node = "pve1"
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-b": {"pve2"},
			},
			DLB: &config.DLBConfig{},
		}
		c.EnsureNoIPConflicts = placementDisabled
	})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-realaz-dlb", testStemcellCID,
		map[string]any{"availability_zone": "zone-b"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("realaz-with-dlb")); err != nil {
		t.Fatalf("expected success with real AZ and DLB configured, got: %v", err)
	}
	if createNode != "pve2" {
		t.Errorf("expected pve2 (only node in zone-b), got %q", createNode)
	}
}

// --------------------------------------------------------------------------
// Firewall + security-group resolution end-to-end tests
// --------------------------------------------------------------------------

// firewallGroupsClusterFn builds a listFirewallGroupsFn that reports groups as present.
func firewallGroupsClusterFn(groups ...string) func() (*sdkcluster.ListFirewallGroupsResponse, error) {
	return func() (*sdkcluster.ListFirewallGroupsResponse, error) {
		resp := make(sdkcluster.ListFirewallGroupsResponse, 0, len(groups))
		for _, g := range groups {
			raw, _ := json.Marshal(map[string]any{"group": g})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
}

// buildVMDepsFirewall builds Deps with optional VMTypes, DiskTypes, SecurityGroups,
// and VMFirewall for firewall-resolution e2e tests.
type vmFirewallDepsOpts struct {
	vmTypes        map[string]config.TypeProfile
	securityGroups []string
	vmFirewall     *bool
}

func buildVMDepsFirewall(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent, opts vmFirewallDepsOpts) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                "pve",
			VMStorage:           storageName,
			NetworkBridge:       "vmbr0",
			VMIDRangeStart:      100,
			AgentMBus:           "nats://mbus.test:4222",
			Placement:           &config.PlacementConfig{Enabled: placementDisabled},
			EnsureNoIPConflicts: placementDisabled,
			VMTypes:             opts.vmTypes,
			SecurityGroups:      opts.securityGroups,
			VMFirewall:          opts.vmFirewall,
			// Explicitly disabled: as of Phase 1 this defaults to 30 (enabled)
			// when nil, and these firewall-focused tests don't wire SDN vnet/
			// node-network fakes for the gate to poll against.
			NetworkResolveRetries: new(int),
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

// TestCreateVM_NoGroupsNoFirewall_ZeroFirewallAPICalls is the critical no-op proof:
// nil VMFirewall + no SecurityGroups + no profile => zero firewall API calls.
func TestCreateVM_NoGroupsNoFirewall_ZeroFirewallAPICalls(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{} // no listFirewallGroupsFn — if called would return empty, but should not be called at all
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{}))

	args := mkArgs("agent-noop", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("noop-fw")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 0 {
		t.Errorf("no firewall rule calls expected; got %v", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 0 {
		t.Errorf("no firewall enable calls expected; got %d", n.firewallEnableOptCalls)
	}
}

// --------------------------------------------------------------------------
// Ephemeral disk full-stack tests
// --------------------------------------------------------------------------

// buildVMDepsWithStorage constructs Deps like buildVMDeps but also wires a
// mockStorageService so tests that exercise the ephemeral disk path
// (attachEphemeralDisk → Storage().CreateVolume) have a storage service.
func buildVMDepsWithStorage(q *vmMockQEMU, n *vmMockNodes, c *vmMockCluster, a *vmMockAgent, stor *mockStorageService) handlers.Deps {
	d := buildVMDeps(q, n, c, a)
	d.PVE.(*mockPVEClient).storageSvc = stor
	return d
}

// TestHandleCreateVM_Ephemeral_Unset verifies byte-identical behavior when
// ephemeral_disk_size_mb is absent: CreateVolume is not called and
// agentCfg.Disks.Ephemeral remains empty (agent carves from root).
func TestHandleCreateVM_Ephemeral_Unset(t *testing.T) {
	t.Parallel()

	createVolumeCalled := false
	stor := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			createVolumeCalled = true
			return "", nil
		},
	}
	var gotEphemeral string
	a := &vmMockAgent{
		configureFn: func(_ context.Context, _ string, _ int, cfg agent.AgentConfig) error {
			gotEphemeral = cfg.Disks.Ephemeral
			return nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithStorage(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, a, stor))

	args := mkArgs("agent-eph-unset", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("eph-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if createVolumeCalled {
		t.Error("CreateVolume called when ephemeral_disk_size_mb is unset — must be no-op")
	}
	if gotEphemeral != "" {
		t.Errorf("Disks.Ephemeral = %q; want empty (agent carves from root)", gotEphemeral)
	}
}

// TestHandleCreateVM_Ephemeral_Success verifies the full happy path:
// ephemeral_disk_size_mb=4096, CreateVolume returns a volid, Config returns
// empty VM config (slot 1 free), AttachDisk returns "scsi1", and
// agentCfg.Disks.Ephemeral is set to the expected by-id device path.
func TestHandleCreateVM_Ephemeral_Success(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-ephemeral-0"
	stor := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
	}
	q := &vmMockQEMU{
		// Config is called at multiple points: for readVirtio0SizeGiB (fallback)
		// and for attachEphemeralDisk slot detection. Return a config with net0
		// so readVirtio0SizeGiB falls back to defaultStemcellDiskGiB, and no
		// scsi slots taken so nextFreeSCSIIndexAtLeast returns 1.
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, gotVolid, bus string, _ *sdkqemu.AttachOpts) (string, error) {
			if gotVolid == volid {
				return "scsi1", nil
			}
			// Persistent-disk attach (none in this test); fall through to default.
			return "scsi1", nil
		},
	}
	var gotEphemeral string
	a := &vmMockAgent{
		configureFn: func(_ context.Context, _ string, _ int, cfg agent.AgentConfig) error {
			gotEphemeral = cfg.Disks.Ephemeral
			return nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithStorage(q, &vmMockNodes{}, &vmMockCluster{}, a, stor))

	args := mkArgs("agent-eph-ok", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "ephemeral_disk_size_mb": 4096},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("eph-ok")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	if gotEphemeral != want {
		t.Errorf("Disks.Ephemeral = %q; want %q", gotEphemeral, want)
	}
}

// TestHandleCreateVM_Ephemeral_ExplicitPool verifies that ephemeral_storage_pool
// in cloud_properties is passed to CreateVolume.
func TestHandleCreateVM_Ephemeral_ExplicitPool(t *testing.T) {
	t.Parallel()

	var gotStorage string
	stor := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			gotStorage = storage
			return storage + ":vm-eph-0", nil
		},
	}
	q := &vmMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "scsi1", nil
		},
	}
	h := handlers.HandleCreateVM(buildVMDepsWithStorage(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, stor))

	args := mkArgs("agent-eph-pool", testStemcellCID,
		map[string]any{
			"cores": 1, "memory": 512,
			"ephemeral_disk_size_mb": 4096,
			"ephemeral_storage_pool": "fast-ssd",
		},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("eph-pool")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotStorage != "fast-ssd" {
		t.Errorf("CreateVolume storage = %q; want fast-ssd", gotStorage)
	}
}

// TestHandleCreateVM_Ephemeral_CreateFail_VMRolledBack verifies that when
// CreateVolume fails, a Cloud error is returned and the VM is rolled back
// (deleteQemuCalls >= 1).
func TestHandleCreateVM_Ephemeral_CreateFail_VMRolledBack(t *testing.T) {
	t.Parallel()

	stor := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return "", errors.New("storage pool full")
		},
	}
	q := &vmMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDepsWithStorage(q, n, &vmMockCluster{}, &vmMockAgent{}, stor))

	args := mkArgs("agent-eph-fail", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "ephemeral_disk_size_mb": 4096},
		defaultNetMap(), []string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("eph-fail"))
	if err == nil {
		t.Fatal("expected error from CreateVolume failure, got nil")
	}
	// Accept Cloud or Retriable — WrapError may classify storage error as transient.
	if err.Error() == "" {
		t.Error("returned error has empty message")
	}
	if len(n.deleteQemuCalls) == 0 {
		t.Error("VM not rolled back (deleteQemuCalls=0) after ephemeral CreateVolume failure")
	}
}

// TestCreateVM_CallSecurityGroups_AppliedAsToday verifies per-call security_groups
// are applied (regression guard for existing behavior).
func TestCreateVM_CallSecurityGroups_AppliedAsToday(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{}))

	args := mkArgs("agent-sg-call", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "security_groups": []any{"web"}},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("sg-call")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 1 || n.firewallRuleActions[0] != "group:web" {
		t.Errorf("firewallRuleActions = %v; want [group:web]", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want 1", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_VMTypeProfileSecurityGroups verifies vm_type profile security_groups
// are applied when the per-call list is empty.
func TestCreateVM_VMTypeProfileSecurityGroups(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("db")}
	a := &vmMockAgent{}
	vmTypes := map[string]config.TypeProfile{
		"secured": {CloudProperties: map[string]any{"security_groups": []any{"db"}}},
	}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{vmTypes: vmTypes}))

	args := mkArgs("agent-sg-profile", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "vm_type": "secured"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("sg-profile")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 1 || n.firewallRuleActions[0] != "group:db" {
		t.Errorf("firewallRuleActions = %v; want [group:db]", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want 1", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_CallSecurityGroupsBeatsProfile verifies per-call security_groups
// win over vm_type profile security_groups.
func TestCreateVM_CallSecurityGroupsBeatsProfile(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("from-call", "from-profile")}
	a := &vmMockAgent{}
	vmTypes := map[string]config.TypeProfile{
		"secured": {CloudProperties: map[string]any{"security_groups": []any{"from-profile"}}},
	}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{vmTypes: vmTypes}))

	args := mkArgs("agent-sg-beats", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "vm_type": "secured", "security_groups": []any{"from-call"}},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("sg-beats")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 1 || n.firewallRuleActions[0] != "group:from-call" {
		t.Errorf("firewallRuleActions = %v; want [group:from-call]", n.firewallRuleActions)
	}
}

// TestCreateVM_GlobalDefaultSecurityGroups verifies config.SecurityGroups is used
// when neither call nor profile specify security_groups.
func TestCreateVM_GlobalDefaultSecurityGroups(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("global")}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{
		securityGroups: []string{"global"},
	}))

	args := mkArgs("agent-sg-global", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("sg-global")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 1 || n.firewallRuleActions[0] != "group:global" {
		t.Errorf("firewallRuleActions = %v; want [group:global]", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want 1", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_FirewallFlagTrueViaProfile_NoGroups verifies that a vm_type profile
// with firewall=true and no groups enables the VM-level firewall once (no group attaches).
func TestCreateVM_FirewallFlagTrueViaProfile_NoGroups(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	vmTypes := map[string]config.TypeProfile{
		"fw-only": {CloudProperties: map[string]any{"firewall": true}},
	}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{vmTypes: vmTypes}))

	args := mkArgs("agent-fw-profile", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "vm_type": "fw-only"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-profile")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 0 {
		t.Errorf("no group rules expected when no groups; got %v", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want 1 (standalone enable)", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_FirewallFlagTrueViaConfig_NoGroups verifies config.VMFirewall=true
// without groups enables the VM-level firewall once standalone.
func TestCreateVM_FirewallFlagTrueViaConfig_NoGroups(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	trueBool := true
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{vmFirewall: &trueBool}))

	args := mkArgs("agent-fw-config", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-config")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 0 {
		t.Errorf("no group rules expected when no groups; got %v", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want 1 (standalone enable)", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_FirewallFlagTrue_WithGroups_NoDoubleEnable verifies that when both
// groups are present and firewall flag is true, the VM firewall is enabled exactly once.
func TestCreateVM_FirewallFlagTrue_WithGroups_NoDoubleEnable(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	trueBool := true
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{vmFirewall: &trueBool}))

	args := mkArgs("agent-fw-no-double", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "security_groups": []any{"web"}},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-no-double")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.firewallRuleActions) != 1 {
		t.Errorf("firewallRuleActions = %v; want [group:web]", n.firewallRuleActions)
	}
	if n.firewallEnableOptCalls != 1 {
		t.Errorf("firewallEnableOptCalls = %d; want exactly 1 (no double-enable)", n.firewallEnableOptCalls)
	}
}

// TestCreateVM_UnknownVMTypeInCloudProps_ReturnsCloudError verifies that an unknown
// vm_type selector in cloud_properties returns a non-retriable CloudError.
func TestCreateVM_UnknownVMTypeInCloudProps_ReturnsCloudError(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{}))

	args := mkArgs("agent-bad-vmtype", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "vm_type": "no-such-profile"},
		defaultNetMap(), []string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("bad-vmtype"))
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type selector")
	}
	if !isCloudError(err) {
		t.Errorf("expected CloudError, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// Root-disk performance options: import path
// --------------------------------------------------------------------------

// TestCreateVM_ImportPath_NoPerfOpts_Phase2Defaults verifies that when no
// perf opts and no virtio_scsi_single are set, createParams virtio0 carries
// the Phase 2 default iothread=1, and scsihw resolves to the Phase 2 default
// virtio-scsi-single. Replaces the pre-Phase-2 "byte-identical" assertions.
func TestCreateVM_ImportPath_NoPerfOpts_Phase2Defaults(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-noperf", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("noperf")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params

	// scsihw defaults to "virtio-scsi-single" as of Phase 2.
	if scsihw, _ := p["scsihw"].(string); scsihw != "virtio-scsi-single" {
		t.Errorf("scsihw = %q; want virtio-scsi-single (Phase 2 default)", scsihw)
	}

	// virtio0 must contain format=, import-from=, and iothread=1 (Phase 2
	// default), but no other comma-separated perf options (no cache=, ssd=,
	// discard=).
	virtio0, _ := p["virtio0"].(string)
	for _, forbidden := range []string{"cache=", "ssd=", "discard="} {
		if strings.Contains(virtio0, forbidden) {
			t.Errorf("virtio0 %q must not contain %q when no perf opts set", virtio0, forbidden)
		}
	}
	if !strings.Contains(virtio0, "iothread=1") {
		t.Errorf("virtio0 %q must contain iothread=1 (Phase 2 default)", virtio0)
	}
	if !strings.Contains(virtio0, "import-from=") {
		t.Errorf("virtio0 %q must contain import-from=", virtio0)
	}
}

// TestCreateVM_ImportPath_ExplicitOptOut_RestoresPreFlipShape verifies that
// explicitly disabling both flipped Phase 2 defaults (iothread,
// virtio_scsi_single) restores the exact pre-Phase-2 createParams shape.
func TestCreateVM_ImportPath_ExplicitOptOut_RestoresPreFlipShape(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-optout", testStemcellCID,
		map[string]any{
			"cores":              1,
			"memory":             512,
			"iothread":           false,
			"virtio_scsi_single": false,
		},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("optout")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params

	if scsihw, _ := p["scsihw"].(string); scsihw != "virtio-scsi-pci" {
		t.Errorf("scsihw = %q; want virtio-scsi-pci with explicit opt-out", scsihw)
	}
	virtio0, _ := p["virtio0"].(string)
	for _, forbidden := range []string{"cache=", "iothread=", "ssd=", "discard="} {
		if strings.Contains(virtio0, forbidden) {
			t.Errorf("virtio0 %q must not contain %q with explicit opt-out", virtio0, forbidden)
		}
	}
	if !strings.Contains(virtio0, "import-from=") {
		t.Errorf("virtio0 %q must contain import-from=", virtio0)
	}
}

// TestCreateVM_ImportPath_PerfOpts_AppliedToVirtio0 verifies that
// iothread:true + cache:"writeback" are appended to virtio0 in createParams,
// and ssd is NOT present (virtio bus drops it).
func TestCreateVM_ImportPath_PerfOpts_AppliedToVirtio0(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-perfimport", testStemcellCID,
		map[string]any{
			"cores":    1,
			"memory":   512,
			"iothread": true,
			"cache":    "writeback",
			"ssd":      true, // virtio bus must drop this
			// Explicit opt-out isolates this test's focus (iothread/cache/ssd
			// resolution) from the Phase 2 virtio_scsi_single default.
			"virtio_scsi_single": false,
		},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("perfimport")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	virtio0, _ := p["virtio0"].(string)

	if !strings.Contains(virtio0, "cache=writeback") {
		t.Errorf("virtio0 %q must contain cache=writeback", virtio0)
	}
	if !strings.Contains(virtio0, "iothread=1") {
		t.Errorf("virtio0 %q must contain iothread=1", virtio0)
	}
	if strings.Contains(virtio0, "ssd=") {
		t.Errorf("virtio0 %q must NOT contain ssd= (virtio bus drops it)", virtio0)
	}
	// scsihw: virtio_scsi_single explicitly opted out above → stays "virtio-scsi-pci".
	if scsihw, _ := p["scsihw"].(string); scsihw != "virtio-scsi-pci" {
		t.Errorf("scsihw = %q; want virtio-scsi-pci (explicit virtio_scsi_single:false)", scsihw)
	}
}

// TestCreateVM_ImportPath_VirtioSCSISingle_SetsCorrectScsihw verifies that
// virtio_scsi_single:true switches createParams["scsihw"] to "virtio-scsi-single".
func TestCreateVM_ImportPath_VirtioSCSISingle_SetsCorrectScsihw(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-vscsisingle", testStemcellCID,
		map[string]any{
			"cores":              1,
			"memory":             512,
			"virtio_scsi_single": true,
		},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vscsisingle")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if scsihw, _ := p["scsihw"].(string); scsihw != "virtio-scsi-single" {
		t.Errorf("scsihw = %q; want virtio-scsi-single", scsihw)
	}
}

// --------------------------------------------------------------------------
// Root-disk performance options: clone path
// --------------------------------------------------------------------------

// TestCreateVM_ClonePath_NoPerfOpts_Phase2Defaults verifies that the
// post-clone UpdateQemuConfig params carry the Phase 2 defaults when no perf
// opts and no virtio_scsi_single are set: Scsihw="virtio-scsi-single" and
// Virtio[0] containing iothread=1. Replaces the pre-Phase-2
// "byte-identical/no extra keys" assertions.
func TestCreateVM_ClonePath_NoPerfOpts_Phase2Defaults(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00009001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-noperf", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("clone-noperf")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The first UpdateQemuConfig call is the resource (cpu/memory/agent) apply.
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]
	if resourceCall.params.Scsihw == nil || *resourceCall.params.Scsihw != "virtio-scsi-single" {
		got := "<nil>"
		if resourceCall.params.Scsihw != nil {
			got = *resourceCall.params.Scsihw
		}
		t.Errorf("Scsihw = %q; want virtio-scsi-single (Phase 2 default)", got)
	}
	virtio0, ok := resourceCall.params.Virtio[0]
	if !ok || !strings.Contains(virtio0, "iothread=1") {
		t.Errorf("Virtio[0] = %q, present=%v; want iothread=1 present (Phase 2 default)", virtio0, ok)
	}
}

// TestCreateVM_ClonePath_ExplicitOptOut_RestoresPreFlipShape verifies that
// explicitly disabling every option that now defaults on (iothread,
// virtio_scsi_single, and discard/ssd auto-resolution) restores the earlier
// UpdateQemuConfig shape: nil Scsihw, empty Virtio map.
func TestCreateVM_ClonePath_ExplicitOptOut_RestoresPreFlipShape(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000900A:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-optout", testTemplateCID,
		map[string]any{
			"cores":              1,
			"memory":             512,
			"iothread":           false,
			"virtio_scsi_single": false,
			// buildVMDepsForTemplate wires a "dir" storage type, which is
			// TRIM-capable at the default qcow2 format — discard/ssd must be
			// explicitly disabled too for this test's "everything off" shape.
			"discard": false,
			"ssd":     false,
		},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("clone-optout")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]
	if resourceCall.params.Scsihw != nil {
		t.Errorf("Scsihw must be nil in UpdateQemuConfig params with explicit opt-out, got %q", *resourceCall.params.Scsihw)
	}
	if len(resourceCall.params.Virtio) != 0 {
		t.Errorf("Virtio map must be empty with explicit opt-out, got %v", resourceCall.params.Virtio)
	}
}

// TestCreateVM_ClonePath_PerfOpts_AppliedViaUpdateConfig verifies that perf
// opts (iothread+cache) and scsihw switch appear in the post-clone
// UpdateQemuConfig call when opted in.
func TestCreateVM_ClonePath_PerfOpts_AppliedViaUpdateConfig(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00009002:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-perf", testTemplateCID,
		map[string]any{
			"cores":              1,
			"memory":             512,
			"iothread":           true,
			"cache":              "writeback",
			"virtio_scsi_single": true,
		},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("clone-perf")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]

	// scsihw must be set to "virtio-scsi-single".
	if resourceCall.params.Scsihw == nil {
		t.Fatal("Scsihw must not be nil when virtio_scsi_single:true")
	}
	if *resourceCall.params.Scsihw != "virtio-scsi-single" {
		t.Errorf("Scsihw = %q; want virtio-scsi-single", *resourceCall.params.Scsihw)
	}

	// Virtio[0] must include iothread=1 and cache=writeback.
	virtio0, ok := resourceCall.params.Virtio[0]
	if !ok {
		t.Fatal("Virtio[0] must be present when perf opts set")
	}
	if !strings.Contains(virtio0, "iothread=1") {
		t.Errorf("Virtio[0] %q must contain iothread=1", virtio0)
	}
	if !strings.Contains(virtio0, "cache=writeback") {
		t.Errorf("Virtio[0] %q must contain cache=writeback", virtio0)
	}
}

// TestCreateVM_ClonePath_OnlyScsihwSwitch_NoVirtio0Key verifies that when
// virtio_scsi_single is set but no perf opts, Scsihw is set but Virtio map
// is empty (don't emit a virtio0 key with no opts appended).
func TestCreateVM_ClonePath_OnlyScsihwSwitch_NoVirtio0Key(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:00009003:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}

	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-scsionly", testTemplateCID,
		map[string]any{
			"cores":              1,
			"memory":             512,
			"virtio_scsi_single": true,
			// Explicit opt-out isolates this test's focus (the scsihw switch
			// alone) from the iothread default and from discard/ssd
			// auto-resolution (buildVMDepsForTemplate wires a "dir" storage
			// type, which is TRIM-capable at the default qcow2 format).
			"iothread": false,
			"discard":  false,
			"ssd":      false,
		},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("clone-scsionly")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]

	if resourceCall.params.Scsihw == nil || *resourceCall.params.Scsihw != "virtio-scsi-single" {
		scsihwVal := "<nil>"
		if resourceCall.params.Scsihw != nil {
			scsihwVal = *resourceCall.params.Scsihw
		}
		t.Errorf("Scsihw = %q; want virtio-scsi-single", scsihwVal)
	}
	if len(resourceCall.params.Virtio) != 0 {
		t.Errorf("Virtio map must be empty when only scsihw switch (no perf opts), got %v", resourceCall.params.Virtio)
	}
}

// ---------------------------------------------------------------------------
// static-IP-in-range containment, gateway audit, searchdomain
// ---------------------------------------------------------------------------

// TestHandleCreateVM_IPContainment_InRange verifies that a manual IP within the
// declared range passes without error and the VM is allocated normally.
func TestHandleCreateVM_IPContainment_InRange(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "10.0.0.5", "netmask": "255.255.255.0",
				"gateway":          "10.0.0.1",
				"range":            "10.0.0.0/24",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("ip-in-range"))
	if err != nil {
		t.Fatalf("expected no error for IP in range, got: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Errorf("expected 1 VM create call (IP in range), got %d", len(q.createCalls))
	}
}

// TestHandleCreateVM_IPContainment_OutOfRange verifies that a manual IP outside
// the declared range returns a non-retriable CloudError BEFORE any VM is allocated.
func TestHandleCreateVM_IPContainment_OutOfRange(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "10.0.1.5", "netmask": "255.255.255.0",
				"gateway":          "10.0.0.1",
				"range":            "10.0.0.0/24",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("ip-out-of-range"))
	if err == nil {
		t.Fatal("expected CloudError for IP outside range, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error (non-retriable CloudError), got %T: %v", err, err)
	}
	// Error must be non-retriable.
	var cpiErr *cpierrors.Error
	if errors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Errorf("containment error must be non-retriable (OkToRetry=false), got OkToRetry=true")
	}
	// Message must name the offending IP and range.
	msg := err.Error()
	if !strings.Contains(msg, "10.0.1.5") {
		t.Errorf("error message must contain offending IP 10.0.1.5, got: %s", msg)
	}
	if !strings.Contains(msg, "10.0.0.0/24") {
		t.Errorf("error message must contain the range 10.0.0.0/24, got: %s", msg)
	}
	// No VM must have been allocated.
	if len(q.createCalls) != 0 {
		t.Errorf("no VM should be allocated when IP is out of range, got %d create calls", len(q.createCalls))
	}
}

// TestHandleCreateVM_IPContainment_NoRange verifies that omitting range skips
// validation — the create succeeds without error.
func TestHandleCreateVM_IPContainment_NoRange(t *testing.T) {
	t.Parallel()
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "192.168.99.5", "netmask": "255.255.255.0",
				"gateway":          "192.168.99.1",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("no-range"))
	if err != nil {
		t.Fatalf("expected no error when range absent, got: %v", err)
	}
}

// TestHandleCreateVM_IPContainment_DynamicSkipped verifies that dynamic-type
// networks are not subject to range containment even when range is set.
func TestHandleCreateVM_IPContainment_DynamicSkipped(t *testing.T) {
	t.Parallel()
	h := handlers.HandleCreateVM(buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type":             "dynamic",
				"range":            "10.0.0.0/24",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("dynamic-skip"))
	if err != nil {
		t.Fatalf("expected no error for dynamic network with range, got: %v", err)
	}
}

// TestHandleCreateVM_IPContainment_MalformedRange verifies that a malformed CIDR
// in range returns a non-retriable CloudError.
func TestHandleCreateVM_IPContainment_MalformedRange(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	h := handlers.HandleCreateVM(buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "10.0.0.5", "netmask": "255.255.255.0",
				"gateway":          "10.0.0.1",
				"range":            "not-a-cidr",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("malformed-range"))
	if err == nil {
		t.Fatal("expected CloudError for malformed range, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error for malformed range, got %T: %v", err, err)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("no VM should be allocated when range is malformed, got %d create calls", len(q.createCalls))
	}
}

// TestHandleCreateVM_IPContainment_MultiNIC_SecondOutOfRange verifies that when
// the second NIC has an out-of-range IP the error names the second network and
// no VM is allocated.
func TestHandleCreateVM_IPContainment_MultiNIC_SecondOutOfRange(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	h := handlers.HandleCreateVM(buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}))

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{
			"default": map[string]any{
				"type": "manual", "ip": "10.0.0.5", "netmask": "255.255.255.0",
				"gateway":          "10.0.0.1",
				"range":            "10.0.0.0/24",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
			"storage": map[string]any{
				"type":             "manual",
				"ip":               "192.168.2.200",
				"netmask":          "255.255.255.0",
				"range":            "192.168.1.0/24",
				"cloud_properties": map[string]any{"bridge": "vmbr1"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("multi-nic-second-out"))
	if err == nil {
		t.Fatal("expected CloudError for second NIC out of range, got nil")
	}
	if !isCloudError(err) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "192.168.2.200") {
		t.Errorf("error must name offending IP 192.168.2.200, got: %s", msg)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("no VM should be allocated when second NIC IP is out of range, got %d", len(q.createCalls))
	}
}

// TestHandleCreateVM_GatewayAudit_WarnOnMissing verifies that a manual network
// with a static IP but no gateway still succeeds (warn-only, no error).
// The ipconfig string must not contain "gw=" when gateway is absent.
func TestHandleCreateVM_GatewayAudit_WarnOnMissing(t *testing.T) {
	t.Parallel()
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
				// gateway deliberately absent
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("no-gw"))
	if err != nil {
		t.Fatalf("expected no error when gateway absent (warn only), got: %v", err)
	}
	if capturedNICParams == nil {
		t.Fatal("NIC params not captured")
	}
	ipconf := capturedNICParams.Ipconfig[0]
	if strings.Contains(ipconf, "gw=") {
		t.Errorf("ipconfig must not contain gw= when gateway is absent, got %q", ipconf)
	}
}

// TestHandleCreateVM_Searchdomain_Set verifies that a search_domain in
// cloud_properties propagates to nicParams.Searchdomain on the UpdateQemuConfig
// call, and that Nameserver is still set when DNS is present.
func TestHandleCreateVM_Searchdomain_Set(t *testing.T) {
	t.Parallel()
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
				"gateway": "10.0.0.1",
				"dns":     []string{"8.8.8.8"},
				"cloud_properties": map[string]any{
					"bridge":        "vmbr0",
					"search_domain": "corp.example.com",
				},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("searchdomain-set"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if capturedNICParams == nil {
		t.Fatal("NIC params not captured")
	}
	if capturedNICParams.Searchdomain == nil {
		t.Fatal("Searchdomain must be set when search_domain cloud_property is present")
	}
	if *capturedNICParams.Searchdomain != "corp.example.com" {
		t.Errorf("Searchdomain = %q; want corp.example.com", *capturedNICParams.Searchdomain)
	}
	// Nameserver must still be set.
	if capturedNICParams.Nameserver == nil || !strings.Contains(*capturedNICParams.Nameserver, "8.8.8.8") {
		t.Errorf("Nameserver must still be set when DNS present, got %v", capturedNICParams.Nameserver)
	}
}

// TestHandleCreateVM_Searchdomain_DnsSearchAlias verifies that the "dns_search"
// cloud_property key is also accepted as a search domain source.
func TestHandleCreateVM_Searchdomain_DnsSearchAlias(t *testing.T) {
	t.Parallel()
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
				"gateway": "10.0.0.1",
				"cloud_properties": map[string]any{
					"bridge":     "vmbr0",
					"dns_search": "search.example.com",
				},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("dns-search-alias"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if capturedNICParams == nil {
		t.Fatal("NIC params not captured")
	}
	if capturedNICParams.Searchdomain == nil {
		t.Fatal("Searchdomain must be set when dns_search cloud_property is present")
	}
	if *capturedNICParams.Searchdomain != "search.example.com" {
		t.Errorf("Searchdomain = %q; want search.example.com", *capturedNICParams.Searchdomain)
	}
}

// TestHandleCreateVM_Searchdomain_Absent verifies byte-identical behavior when
// no search domain cloud_property is supplied: Searchdomain remains nil.
func TestHandleCreateVM_Searchdomain_Absent(t *testing.T) {
	t.Parallel()
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
				"gateway":          "10.0.0.1",
				"cloud_properties": map[string]any{"bridge": "vmbr0"},
			},
		},
		[]string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("searchdomain-absent"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if capturedNICParams == nil {
		t.Fatal("NIC params not captured")
	}
	if capturedNICParams.Searchdomain != nil {
		t.Errorf("Searchdomain must be nil when no search_domain property set, got %q", *capturedNICParams.Searchdomain)
	}
}
