package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// mock cluster service
// ---------------------------------------------------------------------------

// mockClusterService embeds cluster.Service to satisfy the full interface.
// Only ListStatus is overridden; all other methods panic at runtime if called.
// The embedded nil interface satisfies the compiler without listing every method.
type mockClusterService struct {
	cluster.Service // nil — panics if non-overridden methods are called
	statusResp      *cluster.ListStatusResponse
	statusErr       error
}

func (m *mockClusterService) ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error) {
	return m.statusResp, m.statusErr
}

// compile-time interface check.
var _ cluster.Service = (*mockClusterService)(nil)

// ---------------------------------------------------------------------------
// calcMockClient wires the mock cluster service into a pve.Client.
// ---------------------------------------------------------------------------

type calcMockClient struct {
	clusterSvc cluster.Service
}

func (c *calcMockClient) QEMU() qemu.Service                     { return nil }
func (c *calcMockClient) Storage() storage.Service               { return nil }
func (c *calcMockClient) CloudInit() cloudinit.Service           { return nil }
func (c *calcMockClient) Tasks() tasks.Service                   { return nil }
func (c *calcMockClient) Nodes() nodes.Service                   { return nil }
func (c *calcMockClient) Cluster() cluster.Service               { return c.clusterSvc }
func (c *calcMockClient) ClusterStorage() clusterstorage.Service { return nil }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// nodeJSON builds a json.RawMessage representing a PVE cluster/status node entry.
func nodeJSON(name string, maxcpu, maxmem, mem int64, online int) json.RawMessage {
	raw, _ := json.Marshal(map[string]interface{}{
		"type":   "node",
		"name":   name,
		"maxcpu": maxcpu,
		"maxmem": maxmem,
		"mem":    mem,
		"online": online,
	})
	return json.RawMessage(raw)
}

func makeCalcDeps(svc *mockClusterService) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			VMDiskFormat: "qcow2",
			VMStorage:    "local-lvm",
		},
		PVE:    &calcMockClient{clusterSvc: svc},
		Logger: log.NewNopLogger(),
	}
}

func makeCalcArgs(cpu, ram, disk int) []json.RawMessage {
	raw, _ := json.Marshal(map[string]interface{}{
		"cpu":                 cpu,
		"ram":                 ram,
		"ephemeral_disk_size": disk,
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
// Tests
// ---------------------------------------------------------------------------

func TestHandleCalculateVMCloudProperties_SingleNodeFit(t *testing.T) {
	t.Parallel()

	// Node has 8 cores, 8 GiB RAM, 2 GiB used → 6 GiB free.
	resp := cluster.ListStatusResponse{
		nodeJSON("pve1", 8, 8*1024*1024*1024, 2*1024*1024*1024, 1),
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
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != "pve1" {
		t.Errorf("target_node = %q; want pve1", targetNode)
	}
	var targetStorage string
	if e := json.Unmarshal(m["target_storage"], &targetStorage); e != nil || targetStorage != "local-lvm" {
		t.Errorf("target_storage = %q; want local-lvm", targetStorage)
	}
}

func TestHandleCalculateVMCloudProperties_MultiNodePicksBest(t *testing.T) {
	t.Parallel()

	// pve1: 16 cores, 4 GiB free. pve2: 16 cores, 12 GiB free → pick pve2.
	gib := int64(1024 * 1024 * 1024)
	resp := cluster.ListStatusResponse{
		nodeJSON("pve1", 16, 16*gib, 12*gib, 1), // 4 GiB free
		nodeJSON("pve2", 16, 16*gib, 4*gib, 1),  // 12 GiB free
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
		nodeJSON("pve1", 8, 4*gib, 3*gib, 1),
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
		nodeJSON("pve1", 8, 8*gib, 1*gib, 0), // offline
		nodeJSON("pve2", 8, 8*gib, 1*gib, 1), // online, 7 GiB free
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
		nodeJSON("pve1", 4, 16*gib, 1*gib, 1),
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
