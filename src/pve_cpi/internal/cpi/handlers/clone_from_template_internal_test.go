package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// Local fakes for cloneFromTemplate tests.
//
// These live in package handlers (not handlers_test) so the unexported
// cloneFromTemplate function is directly callable. Only the methods exercised
// by cloneFromTemplate are implemented; all others panic to reveal accidental
// dependencies.
// ---------------------------------------------------------------------------

// cloneNodes is a minimal nodes.Service stub for clone tests. It captures the
// CreateQemuClone call and returns a configurable result.
type cloneNodes struct {
	sdknodes.Service
	cloneFn     func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error)
	calls       []*sdknodes.CreateQemuCloneParams
	updateCalls []*sdknodes.UpdateQemuConfigParams
}

// UpdateQemuConfig records the post-clone CPU/memory application so tests can
// assert the cloned VM is resized off the template's minimal defaults.
func (n *cloneNodes) UpdateQemuConfig(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	n.updateCalls = append(n.updateCalls, params)
	return nil
}

func (n *cloneNodes) CreateQemuClone(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
	n.calls = append(n.calls, params)
	if n.cloneFn != nil {
		return n.cloneFn(ctx, node, vmid, params)
	}
	// Default: return a UPID wrapped in a raw JSON string so AwaitTask is exercised.
	raw := sdknodes.CreateQemuCloneResponse{}
	if err := json.Unmarshal([]byte(`"UPID:pve:00001234:00000001:clone:ok"`), &raw); err != nil {
		panic("cloneNodes: unmarshal default UPID: " + err.Error())
	}
	return &raw, nil
}

// cloneClusterStorage is a minimal clusterstorage.Service stub that reports a
// single named storage entry with a configurable type and shared flag.
type cloneClusterStorage struct {
	storageName string
	storageType string
	shared      bool
}

func (s *cloneClusterStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	sharedInt := 0
	if s.shared {
		sharedInt = 1
	}
	raw, err := json.Marshal(map[string]any{
		"storage": s.storageName,
		"type":    s.storageType,
		"shared":  sharedInt,
	})
	if err != nil {
		panic("cloneClusterStorage: marshal: " + err.Error())
	}
	resp := sdkclusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}

func (s *cloneClusterStorage) CreateStorage(_ context.Context, _ *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("cloneClusterStorage.CreateStorage: not expected")
}
func (s *cloneClusterStorage) DeleteStorage(_ context.Context, _ string) error {
	panic("cloneClusterStorage.DeleteStorage: not expected")
}
func (s *cloneClusterStorage) GetStorage(_ context.Context, _ string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("cloneClusterStorage.GetStorage: not expected")
}
func (s *cloneClusterStorage) UpdateStorage(_ context.Context, _ string, _ *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("cloneClusterStorage.UpdateStorage: not expected")
}

var _ sdkclusterstorage.Service = (*cloneClusterStorage)(nil)

// cloneClusterSvc is a minimal cluster.Service stub that returns a fixed node
// count via ListConfigNodes.
type cloneClusterSvc struct {
	sdkcluster.Service
	nodeCount int
}

func (s *cloneClusterSvc) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	resp := make(sdkcluster.ListConfigNodesResponse, s.nodeCount)
	for i := 0; i < s.nodeCount; i++ {
		raw, err := json.Marshal(map[string]any{"node": fmt.Sprintf("pve%02d", i+1)})
		if err != nil {
			panic("cloneClusterSvc: marshal node: " + err.Error())
		}
		resp[i] = raw
	}
	return &resp, nil
}

// cloneClient wires up a pve.Client with a configurable nodes service and a
// tasks service that immediately reports OK status.
type cloneClient struct {
	etClient
	tasksFn           func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
	clusterStorageSvc sdkclusterstorage.Service
	clusterSvc        sdkcluster.Service
}

func (c *cloneClient) Tasks() sdktasks.Service {
	return &cloneTasksService{waitFn: c.tasksFn}
}

func (c *cloneClient) ClusterStorage() sdkclusterstorage.Service { return c.clusterStorageSvc }

func (c *cloneClient) Cluster() sdkcluster.Service { return c.clusterSvc }

// Pools panics — the clone path must not call it.
func (c *cloneClient) Pools() pve.PoolService {
	panic("cloneClient.Pools: not expected in clone tests")
}

type cloneTasksService struct {
	sdktasks.Service
	waitFn func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
}

func (s *cloneTasksService) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if s.waitFn != nil {
		return s.waitFn(ctx, node, upid, opts)
	}
	return &sdktasks.Status{ExitStatus: "OK"}, nil
}

// buildCloneDeps constructs a Deps with the given nodes service and a task
// service that reports immediate OK. cloneMode defaults to "" → "auto" in
// cloneFromTemplate.
//
// nodeCount controls the simulated cluster size (1 = single-node; ≥2 = multi-node).
// storageShared controls whether the storage is reported as shared.
func buildCloneDeps(n *cloneNodes, cloneMode, vmStorage, vmStorageType string) Deps {
	return buildCloneDepsWithTopology(n, cloneMode, vmStorage, vmStorageType, 1, false)
}

// buildCloneDepsWithTopology extends buildCloneDeps with explicit cluster
// node count and storage shared flag. Used by cross-node Target= tests.
func buildCloneDepsWithTopology(
	n *cloneNodes,
	cloneMode, vmStorage, vmStorageType string,
	nodeCount int,
	storageShared bool,
) Deps {
	cfg := &config.CPIConfig{
		Node:      "pve",
		CloneMode: cloneMode,
	}
	cfg.ApplyDefaults()
	// Restore CloneMode to exactly what the test requested (ApplyDefaults sets
	// CloneMode to "auto" when empty; tests that want "" explicitly should pass
	// "auto" to keep behavior consistent).
	cfg.CloneMode = cloneMode

	return Deps{
		Config: cfg,
		PVE: &cloneClient{
			etClient: etClient{nodes: n},
			tasksFn:  nil, // default: return OK immediately
			clusterStorageSvc: &cloneClusterStorage{
				storageName: vmStorage,
				storageType: vmStorageType,
				shared:      storageShared,
			},
			clusterSvc: &cloneClusterSvc{nodeCount: nodeCount},
		},
		Logger: log.NewNopLogger(),
	}
}

// buildCloneShape returns a createVMShape with the storage fields populated.
func buildCloneShape(vmStorage, vmStorageType, vmDiskFormat string) *createVMShape {
	return buildCloneShapeWithNode(vmStorage, vmStorageType, vmDiskFormat, "pve")
}

// buildCloneShapeWithNode returns a createVMShape with a configurable target node.
func buildCloneShapeWithNode(vmStorage, vmStorageType, vmDiskFormat, targetNode string) *createVMShape {
	if vmDiskFormat == "" {
		vmDiskFormat = diskFormatQCOW2
	}
	return &createVMShape{
		node:          targetNode,
		vmStorage:     vmStorage,
		vmStorageType: vmStorageType,
		vmDiskFormat:  vmDiskFormat,
		rootDiskGiB:   5,
		cores:         1,
		sockets:       1,
		memMiB:        512,
		maxAttempts:   3,
	}
}

// ---------------------------------------------------------------------------
// Clone-mode × storage-type matrix
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_AutoLinkedCapable verifies that auto mode on a
// linked-capable storage (dir) produces Full=nil (linked clone) and does NOT
// set Storage or Format on the params.
func TestCloneFromTemplate_AutoLinkedCapable(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "auto", "local", "dir")
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 200, "vm-200", "pve", 6042)
	if err != nil {
		t.Fatalf("auto+dir: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full != nil {
		t.Errorf("auto+dir: Full must be nil (linked clone), got %v", *p.Full)
	}
	if p.Storage != nil {
		t.Errorf("auto+dir: Storage must be nil for linked clone, got %q", *p.Storage)
	}
	if p.Format != nil {
		t.Errorf("auto+dir: Format must be nil for linked clone, got %q", *p.Format)
	}
	if p.Newid != 200 {
		t.Errorf("Newid: want 200, got %d", p.Newid)
	}
	wantName := "vm-200"
	if p.Name == nil || *p.Name != wantName {
		t.Errorf("Name: want %q, got %v", wantName, p.Name)
	}
}

// TestCloneFromTemplate_AppliesShapeCPUMemory verifies the clone path resizes
// the VM off the template's minimal defaults. Templates are created with PVE
// defaults (512 MiB / 1 core); without an explicit post-clone UpdateQemuConfig
// a cloned director boots undersized and never reaches "running".
func TestCloneFromTemplate_AppliesShapeCPUMemory(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "auto", "local", "dir")
	shape := buildCloneShape("local", "dir", "qcow2")
	shape.cores = 8
	shape.sockets = 1
	shape.memMiB = 16384

	if err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 207, "vm-207", "pve", 6049); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateQemuConfig call to apply CPU/memory, got %d", len(n.updateCalls))
	}
	u := n.updateCalls[0]
	if u.Memory == nil || *u.Memory != "16384" {
		t.Errorf("memory: want %q, got %v", "16384", u.Memory)
	}
	if u.Cores == nil || *u.Cores != 8 {
		t.Errorf("cores: want 8, got %v", u.Cores)
	}
	if u.Sockets == nil || *u.Sockets != 1 {
		t.Errorf("sockets: want 1, got %v", u.Sockets)
	}
	// The stemcell template carries agent=enabled=0; the clone must re-enable
	// the QEMU guest agent channel so QGA can reach the guest out-of-band.
	if u.Agent == nil || *u.Agent != "enabled=1" {
		t.Errorf("agent: want %q, got %v", "enabled=1", u.Agent)
	}
}

// TestCloneFromTemplate_AutoLVMFullClone verifies that auto mode on lvm
// (linked not supported) produces Full=&true and sets Storage + Format.
func TestCloneFromTemplate_AutoLVMFullClone(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "auto", "local-lvm", "lvm")
	shape := buildCloneShape("local-lvm", "lvm", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 201, "vm-201", "pve", 6043)
	if err != nil {
		t.Fatalf("auto+lvm: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Errorf("auto+lvm: Full must be &true (full clone required for lvm)")
	}
	if p.Storage == nil || *p.Storage != "local-lvm" {
		t.Errorf("auto+lvm: Storage must be set for full clone, got %v", p.Storage)
	}
	if p.Format == nil || *p.Format != "qcow2" {
		t.Errorf("auto+lvm: Format must be set for full clone, got %v", p.Format)
	}
}

// TestCloneFromTemplate_LinkedDirSuccess verifies that linked mode on a
// linked-capable storage (dir) succeeds with Full=nil.
func TestCloneFromTemplate_LinkedDirSuccess(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "linked", "local", "dir")
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 202, "vm-202", "pve", 6044)
	if err != nil {
		t.Fatalf("linked+dir: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full != nil {
		t.Errorf("linked+dir: Full must be nil, got %v", *p.Full)
	}
	if p.Storage != nil {
		t.Errorf("linked+dir: Storage must be nil for linked clone, got %q", *p.Storage)
	}
}

// TestCloneFromTemplate_LinkedLVMError verifies that clone_mode=linked on lvm
// returns an actionable error and does NOT call CreateQemuClone.
func TestCloneFromTemplate_LinkedLVMError(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "linked", "local-lvm", "lvm")
	shape := buildCloneShape("local-lvm", "lvm", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 203, "vm-203", "pve", 6045)
	if err == nil {
		t.Fatal("linked+lvm: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "clone_mode=linked") {
		t.Errorf("linked+lvm: error missing clone_mode=linked context: %v", err)
	}
	if !strings.Contains(err.Error(), "local-lvm") {
		t.Errorf("linked+lvm: error missing storage name: %v", err)
	}
	if !strings.Contains(err.Error(), "lvm") {
		t.Errorf("linked+lvm: error missing storage type: %v", err)
	}
	if len(n.calls) != 0 {
		t.Errorf("linked+lvm: CreateQemuClone must not be called on validation error, got %d calls", len(n.calls))
	}
}

// TestCloneFromTemplate_FullDirSuccess verifies that clone_mode=full on a
// dir storage sets Full=&true and Storage/Format.
func TestCloneFromTemplate_FullDirSuccess(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "full", "local", "dir")
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 204, "vm-204", "pve", 6046)
	if err != nil {
		t.Fatalf("full+dir: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Errorf("full+dir: Full must be &true")
	}
	if p.Storage == nil || *p.Storage != "local" {
		t.Errorf("full+dir: Storage must be set, got %v", p.Storage)
	}
	if p.Format == nil || *p.Format != "qcow2" {
		t.Errorf("full+dir: Format must be set, got %v", p.Format)
	}
}

// TestCloneFromTemplate_CloneQemuVMError verifies that a CloneQemuVM error
// propagates wrapped with context naming both VMIDs.
func TestCloneFromTemplate_CloneQemuVMError(t *testing.T) {
	t.Parallel()
	cloneErr := errors.New("PVE internal error: disk locked")
	n := &cloneNodes{
		cloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return nil, cloneErr
		},
	}
	deps := buildCloneDeps(n, "auto", "local", "dir")
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 205, "vm-205", "pve", 6047)
	if err == nil {
		t.Fatal("expected error from CloneQemuVM failure, got nil")
	}
	if !errors.Is(err, cloneErr) {
		t.Errorf("original error not in chain: %v", err)
	}
	if !strings.Contains(err.Error(), "6047") {
		t.Errorf("error should reference template vmid 6047: %v", err)
	}
	if !strings.Contains(err.Error(), "205") {
		t.Errorf("error should reference new vmid 205: %v", err)
	}
}

// TestCloneFromTemplate_AwaitTaskError verifies that a task-await error after a
// successful clone submit is wrapped and propagated.
func TestCloneFromTemplate_AwaitTaskError(t *testing.T) {
	t.Parallel()
	awaitErr := fmt.Errorf("clone task failed: storage error")
	n := &cloneNodes{} // default: returns a UPID
	deps := buildCloneDeps(n, "auto", "local", "dir")
	// Override task service to fail on await.
	deps.PVE = &cloneClient{
		etClient: etClient{nodes: n},
		tasksFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, awaitErr
		},
		clusterStorageSvc: &cloneClusterStorage{storageName: "local", storageType: "dir", shared: false},
		clusterSvc:        &cloneClusterSvc{nodeCount: 1},
	}
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 206, "vm-206", "pve", 6048)
	if err == nil {
		t.Fatal("expected error from task await failure, got nil")
	}
	if !errors.Is(err, awaitErr) {
		t.Errorf("await error not in chain: %v", err)
	}
}

// TestCloneFromTemplate_EmptyUPIDNoAwait verifies that when CloneQemuVM returns
// an empty UPID (synchronous PVE completion), no task await is attempted and the
// function returns nil error.
func TestCloneFromTemplate_EmptyUPIDNoAwait(t *testing.T) {
	t.Parallel()
	awaitCalled := false
	n := &cloneNodes{
		cloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			// Return nil response → CloneQemuVM returns upid="" (empty).
			return nil, nil
		},
	}
	deps := buildCloneDeps(n, "auto", "local", "dir")
	deps.PVE = &cloneClient{
		etClient: etClient{nodes: n},
		tasksFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			awaitCalled = true
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
		clusterStorageSvc: &cloneClusterStorage{storageName: "local", storageType: "dir", shared: false},
		clusterSvc:        &cloneClusterSvc{nodeCount: 1},
	}
	shape := buildCloneShape("local", "dir", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 207, "vm-207", "pve", 6049)
	if err != nil {
		t.Fatalf("empty UPID path: unexpected error: %v", err)
	}
	if awaitCalled {
		t.Error("task.Wait must not be called when UPID is empty (synchronous completion)")
	}
}

// TestCloneFromTemplate_AutoUnknownStorageType verifies that auto mode with an
// empty (unknown) storage type treats it as linked-capable (permissive default)
// and produces Full=nil.
func TestCloneFromTemplate_AutoUnknownStorageType(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "auto", "some-store", "")
	shape := buildCloneShape("some-store", "", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 208, "vm-208", "pve", 6050)
	if err != nil {
		t.Fatalf("auto+unknown-type: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full != nil {
		t.Errorf("auto+unknown-type: Full must be nil (permissive default → linked), got %v", *p.Full)
	}
}

// TestCloneFromTemplate_FullLVMSuccess verifies that clone_mode=full on lvm
// also sets Full=&true (consistent with auto+lvm, but driven by explicit full mode).
func TestCloneFromTemplate_FullLVMSuccess(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDeps(n, "full", "local-lvm", "lvm")
	shape := buildCloneShape("local-lvm", "lvm", "raw")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 209, "vm-209", "pve", 6051)
	if err != nil {
		t.Fatalf("full+lvm: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Errorf("full+lvm: Full must be &true")
	}
	if p.Storage == nil || *p.Storage != "local-lvm" {
		t.Errorf("full+lvm: Storage must be set, got %v", p.Storage)
	}
	if p.Format == nil || *p.Format != "raw" {
		t.Errorf("full+lvm: Format must be raw, got %v", p.Format)
	}
}

// ---------------------------------------------------------------------------
// Cross-node Target= enforcement
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_SameNode_NoTarget verifies that when templateNode ==
// shape.node, params.Target is NOT set regardless of storage type.
func TestCloneFromTemplate_SameNode_NoTarget(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	// Single-node cluster, dir storage (non-shared) — same node for both template and VM.
	deps := buildCloneDepsWithTopology(n, "auto", "local", "dir", 1, false)
	shape := buildCloneShapeWithNode("local", "dir", "qcow2", "pve")

	// templateNode == shape.node == "pve" — no Target expected.
	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 300, "vm-300", "pve", 6100)
	if err != nil {
		t.Fatalf("same-node: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Target != nil {
		t.Errorf("same-node: Target must be nil when templateNode==shape.node, got %q", *p.Target)
	}
}

// TestCloneFromTemplate_CrossNode_SharedStorage_SetsTarget verifies that when
// templateNode != shape.node and storage is shared, params.Target is set to
// shape.node so PVE lands the clone on the correct node.
func TestCloneFromTemplate_CrossNode_SharedStorage_SetsTarget(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	// Multi-node cluster, NFS storage (shared) — template on "pve01", VM wanted on "pve02".
	deps := buildCloneDepsWithTopology(n, "auto", "nfs-store", "nfs", 2, true)
	shape := buildCloneShapeWithNode("nfs-store", "nfs", "qcow2", "pve02")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 301, "vm-301", "pve01", 6101)
	if err != nil {
		t.Fatalf("cross-node+shared: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Target == nil {
		t.Fatalf("cross-node+shared: Target must be set (want %q), got nil", "pve02")
	}
	if *p.Target != "pve02" {
		t.Errorf("cross-node+shared: Target = %q, want %q", *p.Target, "pve02")
	}
}

// TestCloneFromTemplate_CrossNode_LocalStorage_Error verifies that when
// templateNode != shape.node and storage is local (non-shared) in a multi-node
// cluster, cloneFromTemplate returns a cross-node local-storage violation error and does NOT call
// CreateQemuClone.
func TestCloneFromTemplate_CrossNode_LocalStorage_Error(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	// Multi-node cluster, dir storage (local, non-shared) — template on "pve01",
	// VM wanted on "pve02". This violates the cross-node local-storage constraint.
	// shape.node = "pve02" = cloudPropsNode passed to ValidateTemplateCloneStorage.
	// ValidateTemplateCloneStorage sees: multi-node + local + cloudPropsNode != "" → ACCEPT.
	// Note: multi-node + local + pinned → ACCEPT, returns cloudPropsNode.
	// But then templateNode ("pve01") != shape.node ("pve02") → we would try to set Target
	// on local storage — PVE rejects this.
	//
	// The real violation is: local storage, template on node A, VM wanted on node B,
	// they cannot match. ValidateTemplateCloneStorage with cloudPropsNode="pve02" returns
	// success (rule 3), but templateNode != shape.node AND storage is local.
	// In that case we must return an error — PVE cannot cross-node clone local storage.
	//
	// The correct enforcement: after ValidateTemplateCloneStorage, if templateNode !=
	// shape.node AND storage IsShared() == false → return error (cannot cross-node clone).
	// This is stricter than what ValidateTemplateCloneStorage alone checks.
	//
	// Note: the shape.node IS set (pve02) so ValidateTemplateCloneStorage rule 3 accepts.
	// The Target= branch then checks: shared? If not → error. This enforces the constraint.
	deps := buildCloneDepsWithTopology(n, "auto", "local", "dir", 2, false)
	shape := buildCloneShapeWithNode("local", "dir", "qcow2", "pve02")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 302, "vm-302", "pve01", 6102)
	if err == nil {
		t.Fatal("cross-node+local: expected cross-node local-storage error, got nil")
	}
	// Error must be actionable: mention local/cross-node constraint.
	if !strings.Contains(err.Error(), "local") && !strings.Contains(err.Error(), "node") {
		t.Errorf("cross-node+local: error lacks actionable context: %v", err)
	}
	if len(n.calls) != 0 {
		t.Errorf("cross-node+local: CreateQemuClone must not be called on cross-node local-storage violation, got %d calls", len(n.calls))
	}
}

// TestCloneFromTemplate_SingleNode_NoTarget verifies that a single-node cluster
// with local storage (e.g. default PVE install) always succeeds and never sets
// Target, even when templateNode is the same as shape.node.
func TestCloneFromTemplate_SingleNode_NoTarget(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	deps := buildCloneDepsWithTopology(n, "auto", "local-lvm", "lvm", 1, false)
	shape := buildCloneShapeWithNode("local-lvm", "lvm", "raw", "pve")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 303, "vm-303", "pve", 6103)
	if err != nil {
		t.Fatalf("single-node+lvm: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Target != nil {
		t.Errorf("single-node: Target must be nil, got %q", *p.Target)
	}
}
