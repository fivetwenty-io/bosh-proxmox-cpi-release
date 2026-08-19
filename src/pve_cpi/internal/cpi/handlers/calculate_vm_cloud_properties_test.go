package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// mock cluster service shim
// ---------------------------------------------------------------------------

// mockClusterService is a thin shim over the canonical mockClusterSvc that
// preserves the previous statusResp/statusErr literal-construction ergonomics
// used by calculate_vm_cloud_properties tests. The shim returns a configured
// *mockClusterSvc whose listStatusFn closes over the provided response/error.
type mockClusterService struct {
	statusResp *cluster.ListStatusResponse
	statusErr  error
}

// asCanonical adapts the shim into a *mockClusterSvc by wiring listStatusFn.
func (m *mockClusterService) asCanonical() *mockClusterSvc {
	resp := m.statusResp
	err := m.statusErr
	return &mockClusterSvc{
		listStatusFn: func(_ context.Context) (*cluster.ListStatusResponse, error) {
			return resp, err
		},
	}
}

// ---------------------------------------------------------------------------
// storage response helpers (calc-test-only)
// ---------------------------------------------------------------------------

// storageActiveImagesJSON returns a single-entry ListStorageResponse with the
// named storage marked active=1 and content="images,rootdir".
func storageActiveImagesJSON(storageName string) *nodes.ListStorageResponse {
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName,
		"type":    "dir",
		"active":  1,
		"enabled": 1,
		"content": "images,rootdir",
	})
	resp := nodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp
}

// storageInactiveJSON returns a single-entry response where the storage is
// listed but active==0 (mount failed or backend offline).
func storageInactiveJSON(storageName string) *nodes.ListStorageResponse {
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName,
		"type":    "dir",
		"active":  0,
		"enabled": 1,
		"content": "images,rootdir",
	})
	resp := nodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp
}

// storageNoImagesJSON returns a response where storage is active but does not
// declare "images" content type (e.g., backup-only storage).
func storageNoImagesJSON(storageName string) *nodes.ListStorageResponse {
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName,
		"type":    "dir",
		"active":  1,
		"enabled": 1,
		"content": "backup,vztmpl",
	})
	resp := nodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp
}

// emptyStorageResponse returns an empty list (storage not present on node).
func emptyStorageResponse() *nodes.ListStorageResponse {
	resp := nodes.ListStorageResponse{}
	return &resp
}

// ---------------------------------------------------------------------------
// deps helpers
// ---------------------------------------------------------------------------

// nodeJSON builds a json.RawMessage representing a PVE cluster/status node entry.
func nodeJSON(name string, maxcpu, maxmem, mem int64, online int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type":   "node",
		"name":   name,
		"maxcpu": maxcpu,
		"maxmem": maxmem,
		"mem":    mem,
		"online": online,
	})
	return json.RawMessage(raw)
}

// makeCalcDeps builds Deps with a default mockNodesService (nil listStorageFn →
// returns active+images for any requested storage). All pre-existing tests that
// do not exercise storage filtering pass without modification.
func makeCalcDeps(svc *mockClusterService) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			VMDiskFormat: "qcow2",
			VMStorage:    storageName,
		},
		PVE: &mockPVEClient{
			clusterSvc: svc.asCanonical(),
			nodesSvc:   &mockNodesService{}, // nil listStorageFn → safe default
		},
		Logger: log.NewNopLogger(),
	}
}

// makeCalcDepsWithNodes constructs Deps with a custom nodes service,
// used by storage-first tests that need to control ListStorage behavior.
func makeCalcDepsWithNodes(svc *mockClusterService, nodesSvc nodes.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			VMDiskFormat: "qcow2",
			VMStorage:    storageName,
		},
		PVE: &mockPVEClient{
			clusterSvc: svc.asCanonical(),
			nodesSvc:   nodesSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

func makeCalcArgs(cpu, ram, disk int) []json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"cpu":                 cpu,
		"ram":                 ram,
		"ephemeral_disk_size": disk,
	})
	return []json.RawMessage{json.RawMessage(raw)}
}

// makeCalcArgsWithStorage builds args that include a per-request storage override.
func makeCalcArgsWithStorage(cpu, ram, disk int, stor string) []json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"cpu":                 cpu,
		"ram":                 ram,
		"ephemeral_disk_size": disk,
		"storage":             stor,
	})
	return []json.RawMessage{json.RawMessage(raw)}
}

// decodeCloudProps decodes the handler result into a map for assertion.
func decodeCloudProps(t *testing.T, result any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Pre-existing tests (assertions unchanged; mock now backed by mockNodesService
// with nil listStorageFn that returns active+images by default).
// ---------------------------------------------------------------------------

func TestHandleCalculateVMCloudProperties_SingleNodeFit(t *testing.T) {
	t.Parallel()

	// Node has 8 cores, 8 GiB RAM, 2 GiB used → 6 GiB free.
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 8*1024*1024*1024, 2*1024*1024*1024, 1),
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	// Request 2 CPUs, 2048 MiB RAM.
	result, err := h.Handle(context.Background(), makeCalcArgs(2, 2048, 10240), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)

	var cores int
	if e := json.Unmarshal(m["cores"], &cores); e != nil || cores != 2 {
		t.Errorf("cores = %d; want 2", cores)
	}
	var sockets int
	if e := json.Unmarshal(m["sockets"], &sockets); e != nil || sockets != 1 {
		t.Errorf("sockets = %d; want 1", sockets)
	}
	var memory int
	if e := json.Unmarshal(m["memory"], &memory); e != nil || memory != 2048 {
		t.Errorf("memory = %d; want 2048", memory)
	}
	var vmDiskFormat string
	if e := json.Unmarshal(m["vm_disk_format"], &vmDiskFormat); e != nil || vmDiskFormat != "qcow2" {
		t.Errorf("vm_disk_format = %q; want qcow2", vmDiskFormat)
	}
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != testNode {
		t.Errorf("target_node = %q; want pve1", targetNode)
	}
	var targetStorage string
	if e := json.Unmarshal(m["target_storage"], &targetStorage); e != nil || targetStorage != storageName {
		t.Errorf("target_storage = %q; want local-lvm", targetStorage)
	}
}

func TestHandleCalculateVMCloudProperties_MultiNodePicksBest(t *testing.T) {
	t.Parallel()

	// pve1: 16 cores, 4 GiB free. pve2: 16 cores, 12 GiB free → pick pve2.
	gib := int64(1024 * 1024 * 1024)
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 16, 16*gib, 12*gib, 1), // 4 GiB free
		nodeJSON("pve2", 16, 16*gib, 4*gib, 1),    // 12 GiB free
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(4, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (most free memory)", targetNode)
	}
}

func TestHandleCalculateVMCloudProperties_NoFit(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// Node only has 1 GiB free; request 4 GiB.
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 4*gib, 3*gib, 1),
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(2, 4096, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for no-fit scenario, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type = %T %v; want NotSupported", err, err)
	}
}

func TestHandleCalculateVMCloudProperties_OfflineNodeSkipped(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 8*gib, 1*gib, 0), // offline
		nodeJSON("pve2", 8, 8*gib, 1*gib, 1),   // online, 7 GiB free
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (online)", targetNode)
	}
}

func TestHandleCalculateVMCloudProperties_CPUShortfall(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// Node has only 4 cores; request 8.
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 4, 16*gib, 1*gib, 1),
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(8, 1024, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for CPU shortfall, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type want NotSupported, got %v", err)
	}
}

func TestHandleCalculateVMCloudProperties_MissingArgs(t *testing.T) {
	t.Parallel()

	deps := makeCalcDeps(&mockClusterService{})
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type want CloudError, got %v", err)
	}
}

func TestHandleCalculateVMCloudProperties_InvalidJSON(t *testing.T) {
	t.Parallel()

	deps := makeCalcDeps(&mockClusterService{})
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`{invalid}`)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestHandleCalculateVMCloudProperties_ClusterAPIError(t *testing.T) {
	t.Parallel()

	svc := &mockClusterService{statusErr: fmt.Errorf("connection refused")}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from cluster API failure, got nil")
	}
}

// TestHandleCalculateVMCloudProperties_ClusterAPIConnErrorIsRetriable verifies
// that a PVE connection error on the ListStatus call produces a retriable error.
// This is the retryability fix from Tier 1 (pve.WrapError route for cluster status).
// A *sdkerrors.ConnectionError is used because it has exported fields and WrapError
// maps it to TypeRetriableCloud unconditionally.
func TestHandleCalculateVMCloudProperties_ClusterAPIConnErrorIsRetriable(t *testing.T) {
	t.Parallel()

	// *sdkerrors.ConnectionError → pve.WrapError → TypeRetriableCloud.
	connErr := &sdkerrors.ConnectionError{
		Host:    "pve.test.local",
		Port:    8006,
		Message: "connection refused",
	}

	svc := &mockClusterService{statusErr: connErr}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from connection failure, got nil")
	}

	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("connection error on ListStatus must be TypeRetriableCloud; got type in: %v", err)
	}

	// OkToRetry must be true.
	var cpiErr *cpierrors.Error
	if errs, ok := extractCPIError(err); ok {
		cpiErr = errs
	}
	if cpiErr == nil {
		t.Fatalf("expected *cpierrors.Error in chain; got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("connection error on ListStatus must have OkToRetry=true; got false. Error: %v", err)
	}
}

// extractCPIError walks the error chain to find the outermost *cpierrors.Error.
func extractCPIError(err error) (*cpierrors.Error, bool) {
	var target *cpierrors.Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Storage-first tests
// ---------------------------------------------------------------------------

// TestHandleCalculateVMCloudProperties_StorageFirst_AllHaveStorage verifies that
// when all nodes have storage, the node with the most free RAM wins.
func TestHandleCalculateVMCloudProperties_StorageFirst_AllHaveStorage(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// pve1: 4 GiB free. pve2: 10 GiB free. Both have local-lvm active.
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 12*gib, 1), // 4 GiB free
		nodeJSON("pve2", 8, 16*gib, 6*gib, 1),    // 10 GiB free
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}
	nodesSvc := &mockNodesService{} // nil listStorageFn → all nodes have storage active+images

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (most free RAM)", targetNode)
	}
	var targetStorage string
	if e := json.Unmarshal(m["target_storage"], &targetStorage); e != nil || targetStorage != storageName {
		t.Errorf("target_storage = %q; want local-lvm", targetStorage)
	}
}

// TestHandleCalculateVMCloudProperties_StorageFirst_BestRAMLacksStorage verifies
// that the node with most free RAM is skipped when its storage is not active,
// and the next-best node is picked.
func TestHandleCalculateVMCloudProperties_StorageFirst_BestRAMLacksStorage(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// pve1: 10 GiB free but storage inactive. pve2: 6 GiB free, storage active.
	// Expected winner: pve2.
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 6*gib, 1), // 10 GiB free, but storage bad
		nodeJSON("pve2", 8, 16*gib, 10*gib, 1),  // 6 GiB free, storage good
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			switch node {
			case testNode:
				return storageInactiveJSON(storageName), nil
			case "pve2":
				return storageActiveImagesJSON(storageName), nil
			default:
				return emptyStorageResponse(), nil
			}
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (storage active)", targetNode)
	}
}

// TestHandleCalculateVMCloudProperties_StorageFirst_AllLackStorage verifies that
// when all nodes lack storage, NotSupported is returned and the error message
// names the CPU/RAM-qualifying nodes that failed the storage check.
func TestHandleCalculateVMCloudProperties_StorageFirst_AllLackStorage(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1), // 12 GiB free, storage bad
		nodeJSON("pve2", 8, 16*gib, 4*gib, 1),   // 12 GiB free, storage bad
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			return storageInactiveJSON(storageName), nil
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected NotSupported error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type want NotSupported, got %v", err)
	}
	// Both pve1 and pve2 qualify on CPU+RAM but fail storage — must appear in message.
	errMsg := err.Error()
	if !strings.Contains(errMsg, testNode) || !strings.Contains(errMsg, "pve2") {
		t.Errorf("error message should name pve1 and pve2 as storage-failed nodes; got: %s", errMsg)
	}
	if !strings.Contains(errMsg, storageName) {
		t.Errorf("error message should name effective storage %q; got: %s", storageName, errMsg)
	}
}

// TestHandleCalculateVMCloudProperties_StorageFirst_OneNodeListStorageError verifies
// that a ListStorage API error on one node excludes it (fail-safe), while other
// nodes are still considered.
func TestHandleCalculateVMCloudProperties_StorageFirst_OneNodeListStorageError(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// pve1: ListStorage fails (network error). pve2: storage active.
	// Expected: pve1 excluded; pve2 wins.
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1), // 12 GiB free, ListStorage fails
		nodeJSON("pve2", 8, 16*gib, 8*gib, 1),   // 8 GiB free, storage good
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			if node == testNode {
				return nil, fmt.Errorf("connection refused")
			}
			return storageActiveImagesJSON(storageName), nil
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (pve1 excluded due to ListStorage error)", targetNode)
	}
}

// TestHandleCalculateVMCloudProperties_StorageFirst_AllNodesListStorageError verifies
// that when every CPU/RAM-fitting node errors on ListStorage, NotSupported is
// returned and the unreachable-storage nodes are named in the message (they are
// tracked the same as inactive-storage nodes).
func TestHandleCalculateVMCloudProperties_StorageFirst_AllNodesListStorageError(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1), // fits CPU/RAM, ListStorage fails
		nodeJSON("pve2", 8, 16*gib, 8*gib, 1),   // fits CPU/RAM, ListStorage fails
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	_, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected NotSupported error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeNotSupported) {
		t.Errorf("error type want NotSupported, got %v", err)
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, testNode) || !strings.Contains(errMsg, "pve2") {
		t.Errorf("error message must name the unreachable-storage nodes pve1 and pve2; got: %s", errMsg)
	}
}

// TestHandleCalculateVMCloudProperties_StorageOverride_Used verifies that a
// non-empty storage field in vmResources overrides deps.Config.VMStorage, and
// the returned TargetStorage reflects the override.
func TestHandleCalculateVMCloudProperties_StorageOverride_Used(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1),
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	// Only return active+images for "ceph-vm" (the override); return inactive for storageName.
	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			if params != nil && params.Storage != nil && *params.Storage == "ceph-vm" {
				return storageActiveImagesJSON("ceph-vm"), nil
			}
			return storageInactiveJSON(storageName), nil
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgsWithStorage(2, 1024, 0, "ceph-vm"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetStorage string
	if e := json.Unmarshal(m["target_storage"], &targetStorage); e != nil || targetStorage != "ceph-vm" {
		t.Errorf("target_storage = %q; want ceph-vm (per-request override)", targetStorage)
	}
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != testNode {
		t.Errorf("target_node = %q; want pve1", targetNode)
	}
}

// TestHandleCalculateVMCloudProperties_StorageOverride_Absent verifies that when
// no storage override is present in vmResources, TargetStorage equals the
// config VMStorage value.
func TestHandleCalculateVMCloudProperties_StorageOverride_Absent(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1),
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}
	nodesSvc := &mockNodesService{} // default: all nodes have storage

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	// No "storage" key in args.
	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetStorage string
	if e := json.Unmarshal(m["target_storage"], &targetStorage); e != nil || targetStorage != storageName {
		t.Errorf("target_storage = %q; want local-lvm (config default)", targetStorage)
	}
}

// ---------------------------------------------------------------------------
// §7.43 ephemeral_disk_size echo tests
// ---------------------------------------------------------------------------

// TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_EchoedInResponse verifies
// that a non-zero ephemeral_disk_size in vm_resources is present in the returned
// cloud_properties under the key "ephemeral_disk_size_mb" — the key consumed by
// create_vm's createVMCloudProps.EphemeralDiskSizeMB field and passed to
// resolveEphemeralShape. Unit: MiB (BOSH vm_resources.ephemeral_disk_size is MB/MiB).
func TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_EchoedInResponse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		ephemeralDiskSize int
		wantPresent       bool
	}{
		{name: "zero_omitted", ephemeralDiskSize: 0, wantPresent: false},
		{name: "small_1024_MiB", ephemeralDiskSize: 1024, wantPresent: true},
		{name: "large_102400_MiB", ephemeralDiskSize: 102400, wantPresent: true},
	}

	gib := int64(1024 * 1024 * 1024)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := cluster.ListStatusResponse{
				nodeJSON(testNode, 8, 16*gib, 4*gib, 1),
			}
			svc := &mockClusterService{statusResp: &resp}
			deps := makeCalcDeps(svc)
			h := handlers.HandleCalculateVMCloudProperties(deps)

			result, err := h.Handle(
				context.Background(),
				makeCalcArgs(2, 1024, tc.ephemeralDiskSize),
				jsonrpc.Context{},
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			m := decodeCloudProps(t, result)
			raw, present := m["ephemeral_disk_size_mb"]

			if tc.wantPresent {
				if !present {
					t.Fatalf("ephemeral_disk_size_mb missing from cloud_properties; want %d", tc.ephemeralDiskSize)
				}
				var got int
				if e := json.Unmarshal(raw, &got); e != nil {
					t.Fatalf("unmarshal ephemeral_disk_size_mb: %v", e)
				}
				if got != tc.ephemeralDiskSize {
					t.Errorf("ephemeral_disk_size_mb = %d; want %d", got, tc.ephemeralDiskSize)
				}
			} else {
				if present {
					t.Errorf("ephemeral_disk_size_mb present in response with zero input; want omitted (got raw %s)", raw)
				}
			}
		})
	}
}

// TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_ZeroOmitted verifies
// that when ephemeral_disk_size is 0 (not provided), the field is absent from
// the JSON response, preserving byte-identical behavior for pre-existing callers.
func TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_ZeroOmitted(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1),
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	if _, present := m["ephemeral_disk_size_mb"]; present {
		t.Errorf("ephemeral_disk_size_mb must be absent when input is 0; key was present in response")
	}
}

// TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_WireThrough verifies
// that the JSON emitted by calculate_vm_cloud_properties is consumable by
// create_vm's cloud_properties decode path: marshaling the response and
// re-decoding it into the same key map that create_vm uses must yield a
// non-zero ephemeral_disk_size_mb equal to the requested value.
// This catches key-name mismatches between the two handlers at the wire level.
func TestHandleCalculateVMCloudProperties_EphemeralDiskSizeMB_WireThrough(t *testing.T) {
	t.Parallel()

	const requestedMB = 10240

	gib := int64(1024 * 1024 * 1024)
	resp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1),
	}
	svc := &mockClusterService{statusResp: &resp}
	deps := makeCalcDeps(svc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(
		context.Background(),
		makeCalcArgs(2, 1024, requestedMB),
		jsonrpc.Context{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Marshal the handler result to JSON — this is what the Director serializes
	// and passes back as cloud_properties in the subsequent create_vm call.
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal handler result: %v", err)
	}

	// Decode into the key map create_vm uses. The struct field consuming
	// ephemeral_disk_size_mb is createVMCloudProps.EphemeralDiskSizeMB
	// (unexported type). Decode into an anonymous struct mirroring just that
	// field to verify the key name aligns at the wire level.
	var createVMProps struct {
		EphemeralDiskSizeMB int `json:"ephemeral_disk_size_mb"`
	}
	if e := json.Unmarshal(wire, &createVMProps); e != nil {
		t.Fatalf("unmarshal into create_vm cloud_properties shape: %v", e)
	}
	if createVMProps.EphemeralDiskSizeMB != requestedMB {
		t.Errorf(
			"wire-through: create_vm would see EphemeralDiskSizeMB=%d; want %d — key mismatch between calculate and create handlers",
			createVMProps.EphemeralDiskSizeMB, requestedMB,
		)
	}
}

// TestHandleCalculateVMCloudProperties_StorageFirst_StorageNoImages verifies
// that a node whose storage is active but lacks the "images" content type
// is excluded from selection.
func TestHandleCalculateVMCloudProperties_StorageFirst_StorageNoImages(t *testing.T) {
	t.Parallel()

	gib := int64(1024 * 1024 * 1024)
	// pve1: storage active but content=backup,vztmpl (no images). pve2: storage active+images.
	clusterResp := cluster.ListStatusResponse{
		nodeJSON(testNode, 8, 16*gib, 4*gib, 1), // 12 GiB free, no images
		nodeJSON("pve2", 8, 16*gib, 10*gib, 1),  // 6 GiB free, has images
	}
	clusterSvc := &mockClusterService{statusResp: &clusterResp}

	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			switch node {
			case testNode:
				return storageNoImagesJSON(storageName), nil
			case "pve2":
				return storageActiveImagesJSON(storageName), nil
			default:
				return emptyStorageResponse(), nil
			}
		},
	}

	deps := makeCalcDepsWithNodes(clusterSvc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 1024, 0), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve2" {
		t.Errorf("target_node = %q; want pve2 (pve1 lacks images content type)", targetNode)
	}
}
