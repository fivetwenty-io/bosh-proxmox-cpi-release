package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// Minimal mock for qemu.Service — only Config is used by set_disk_metadata.
// ---------------------------------------------------------------------------

type diskMetaQEMUMock struct {
	// configs maps "node:vmid" → config map.
	configs map[string]map[string]interface{}
	// updateDesc captures the description written via UpdateQemuConfig on
	// the main QEMU config path (used in nodes mock).
}

func (m *diskMetaQEMUMock) Config(_ context.Context, node string, vmid int) (map[string]interface{}, error) {
	key := diskKey(node, vmid)
	if cfg, ok := m.configs[key]; ok {
		return cfg, nil
	}
	return map[string]interface{}{}, nil
}

func (m *diskMetaQEMUMock) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("Create not expected")
}
func (m *diskMetaQEMUMock) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("Status not expected")
}
func (m *diskMetaQEMUMock) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("Start not expected")
}
func (m *diskMetaQEMUMock) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("Stop not expected")
}
func (m *diskMetaQEMUMock) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("Reset not expected")
}
func (m *diskMetaQEMUMock) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("Clone not expected")
}
func (m *diskMetaQEMUMock) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("Template not expected")
}
func (m *diskMetaQEMUMock) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("AttachDisk not expected")
}
func (m *diskMetaQEMUMock) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("DetachDisk not expected")
}
func (m *diskMetaQEMUMock) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("ResizeDisk not expected")
}
func (m *diskMetaQEMUMock) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("Snapshot not expected")
}
func (m *diskMetaQEMUMock) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("DeleteSnapshot not expected")
}
func (m *diskMetaQEMUMock) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("ListSnapshots not expected")
}
func (m *diskMetaQEMUMock) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("RollbackSnapshot not expected")
}

var _ qemu.Service = (*diskMetaQEMUMock)(nil)

// ---------------------------------------------------------------------------
// Minimal nodes.Service mock — only ListQemu and UpdateQemuConfig used.
// ---------------------------------------------------------------------------

// diskMetaNodesMock implements the nodes.Service interface. Only ListQemu and
// UpdateQemuConfig are implemented; all other methods panic.
type diskMetaNodesMock struct {
	sdknodes.Service // nil embed — panics on uncovered methods
	// vmsByNode maps node name → list of vmid int64.
	vmsByNode map[string][]int64
	// capturedDesc captures the description value written by UpdateQemuConfig.
	capturedDesc *string
	// capturedTags captures the tags value written by UpdateQemuConfig.
	capturedTags *string
	// updateErr if set, UpdateQemuConfig returns this error.
	updateErr error
}

func diskKey(node string, vmid int) string {
	return node + ":" + strconv.Itoa(vmid)
}

func vmidRaw(id int64) json.RawMessage {
	b, _ := json.Marshal(map[string]interface{}{"vmid": id})
	return json.RawMessage(b)
}

func (m *diskMetaNodesMock) ListQemu(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	ids, ok := m.vmsByNode[node]
	if !ok {
		resp := sdknodes.ListQemuResponse{}
		return &resp, nil
	}
	resp := make(sdknodes.ListQemuResponse, 0, len(ids))
	for _, id := range ids {
		resp = append(resp, vmidRaw(id))
	}
	return &resp, nil
}

func (m *diskMetaNodesMock) UpdateQemuConfig(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	if params != nil {
		if params.Description != nil {
			m.capturedDesc = params.Description
		}
		if params.Tags != nil {
			m.capturedTags = params.Tags
		}
	}
	return nil
}

// All remaining nodes.Service methods panic — they are never called by set_disk_metadata.
func (m *diskMetaNodesMock) ListNodes(_ context.Context) (*sdknodes.ListNodesResponse, error) {
	panic("ListNodes not expected")
}
func (m *diskMetaNodesMock) GetNodes(_ context.Context, _ string) (*sdknodes.GetNodesResponse, error) {
	panic("GetNodes not expected")
}

// compile-time interface check.
var _ sdknodes.Service = (*diskMetaNodesMock)(nil)

// ---------------------------------------------------------------------------
// diskMetaClientMock wires together the mock services.
// ---------------------------------------------------------------------------

type diskMetaClientMock struct {
	qemuSvc    *diskMetaQEMUMock
	nodesSvc   *diskMetaNodesMock
	clusterSvc *mockClusterService
}

func (c *diskMetaClientMock) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *diskMetaClientMock) Storage() storage.Service               { return nil }
func (c *diskMetaClientMock) CloudInit() cloudinit.Service           { return nil }
func (c *diskMetaClientMock) Tasks() tasks.Service                   { return nil }
func (c *diskMetaClientMock) Nodes() sdknodes.Service                { return c.nodesSvc }
func (c *diskMetaClientMock) Cluster() cluster.Service               { return c.clusterSvc }
func (c *diskMetaClientMock) ClusterStorage() clusterstorage.Service { return nil }

// ---------------------------------------------------------------------------
// helper builders
// ---------------------------------------------------------------------------

const testDiskCID = "local-lvm:vm-100-disk-0"
const testNode = "pve1"
const testVMID = int64(100)

// clusterWithNode builds a ListStatusResponse with a single online node entry.
func clusterWithNode(name string) *cluster.ListStatusResponse {
	raw, _ := json.Marshal(map[string]interface{}{
		"type":   "node",
		"name":   name,
		"online": 1,
	})
	resp := cluster.ListStatusResponse{json.RawMessage(raw)}
	return &resp
}

// vmConfigWithDisk builds a QEMU config map that includes testDiskCID at scsi0.
func vmConfigWithDisk(diskCID, desc string) map[string]interface{} {
	m := map[string]interface{}{
		"scsi0": diskCID,
	}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func makeDiskMetaDeps(pve *diskMetaClientMock) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    pve,
		Logger: log.NewNopLogger(),
	}
}

func makeMetaArgs(diskCID string, meta map[string]any) []json.RawMessage {
	r0, _ := json.Marshal(diskCID)
	r1, _ := json.Marshal(meta)
	return []json.RawMessage{json.RawMessage(r0), json.RawMessage(r1)}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleSetDiskMetadata_AttachedSingleVM(t *testing.T) {
	t.Parallel()

	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			testNode + ":100": vmConfigWithDisk(testDiskCID, ""),
		},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	meta := map[string]any{"deployment": "cf", "instance_id": "vm-abc123"}
	result, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, meta), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}

	// Verify UpdateQemuConfig was called with a description containing the sentinel.
	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called — capturedDesc is nil")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, "<!--BOSH:") {
		t.Errorf("description missing sentinel block; got: %s", desc)
	}
	if !strings.Contains(desc, testDiskCID) {
		t.Errorf("description missing disk_cid in sentinel; got: %s", desc)
	}
}

func TestHandleSetDiskMetadata_Detached(t *testing.T) {
	t.Parallel()

	logger, logs := log.NewObservedLogger(log.LevelWarn)

	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}
	// VM config has NO matching disk.
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			testNode + ":100": {"scsi0": "local-lvm:vm-100-disk-99"},
		},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	deps := handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    pve,
		Logger: logger,
	}

	h := handlers.HandleSetDiskMetadata(deps)
	result, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("detached disk: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("detached disk: expected nil result, got %v", result)
	}

	// Verify warn was logged.
	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("detached disk: expected warn log, got none")
	}
}

func TestHandleSetDiskMetadata_Ambiguous(t *testing.T) {
	t.Parallel()

	const vm2 = int64(200)
	ambigNodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID, vm2}},
	}
	// Both vmids (100 and 200) have configs containing testDiskCID, triggering the
	// ambiguous-match error path in the handler.
	pveAmbig := &diskMetaClientMock{
		qemuSvc: &diskMetaQEMUMock{configs: map[string]map[string]interface{}{
			"pve1:100": vmConfigWithDisk(testDiskCID, ""),
			"pve1:200": vmConfigWithDisk(testDiskCID, ""),
		}},
		nodesSvc:   ambigNodesSvc,
		clusterSvc: &mockClusterService{statusResp: clusterWithNode(testNode)},
	}

	hAmbig := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pveAmbig))
	ambigResult, ambigErr := hAmbig.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{}), jsonrpc.Context{})
	if ambigErr == nil {
		t.Fatalf("ambiguous: expected error, got nil result=%v", ambigResult)
	}
	if !cpierrors.IsType(ambigErr, cpierrors.TypeCloud) {
		t.Errorf("ambiguous: want CloudError, got %v", ambigErr)
	}
	if !strings.Contains(ambigErr.Error(), "ambiguous") {
		t.Errorf("ambiguous: error should mention 'ambiguous'; got %q", ambigErr.Error())
	}
}

func TestHandleSetDiskMetadata_DescriptionPreserved(t *testing.T) {
	t.Parallel()

	existingDesc := "VM managed by BOSH"
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			testNode + ":100": vmConfigWithDisk(testDiskCID, existingDesc),
		},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, existingDesc) {
		t.Errorf("existing description text not preserved; got: %s", desc)
	}
	if !strings.Contains(desc, "<!--BOSH:") {
		t.Errorf("sentinel block missing; got: %s", desc)
	}
}

func TestHandleSetDiskMetadata_MissingArgs(t *testing.T) {
	t.Parallel()

	pve := &diskMetaClientMock{
		clusterSvc: &mockClusterService{},
		nodesSvc:   &diskMetaNodesMock{},
		qemuSvc:    &diskMetaQEMUMock{},
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("want CloudError, got %v", err)
	}
}

// TestHandleSetDiskMetadata_AppliesDiskTags verifies that a "tags" sub-key in
// metadata causes the hosting VM's tags field to be updated with sanitized
// "<key>--<value>" entries while pre-existing non-conflicting tags survive.
func TestHandleSetDiskMetadata_AppliesDiskTags(t *testing.T) {
	t.Parallel()

	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			testNode + ":100": {
				"scsi0": testDiskCID,
				"tags":  "env--prod;director--abc",
			},
		},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	meta := map[string]any{
		"deployment": "cf",
		"tags": map[string]any{
			"tier": "bronze",
		},
	}
	_, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, meta), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, "bosh_disk_tags") {
		t.Errorf("description sentinel missing bosh_disk_tags; got: %s", desc)
	}
	if nodesSvc.capturedTags == nil {
		t.Fatal("UpdateQemuConfig did not write VM tags field")
	}
	gotTags := *nodesSvc.capturedTags
	for _, want := range []string{"env--prod", "director--abc", "tier--bronze"} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("VM tags missing %q; got: %q", want, gotTags)
		}
	}
}

func TestHandleSetDiskMetadata_InvalidDiskCID(t *testing.T) {
	t.Parallel()

	pve := &diskMetaClientMock{
		clusterSvc: &mockClusterService{},
		nodesSvc:   &diskMetaNodesMock{},
		qemuSvc:    &diskMetaQEMUMock{},
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	r0, _ := json.Marshal("nodisk") // no colon → invalid
	r1, _ := json.Marshal(map[string]any{})
	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(r0), json.RawMessage(r1)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for invalid disk_cid, got nil")
	}
}

// TestHandleSetDiskMetadata_MetadataNotObject verifies that supplying a non-object
// JSON value (integer 42) as the metadata argument returns a CloudError.
func TestHandleSetDiskMetadata_MetadataNotObject(t *testing.T) {
	t.Parallel()

	pve := &diskMetaClientMock{
		clusterSvc: &mockClusterService{},
		nodesSvc:   &diskMetaNodesMock{},
		qemuSvc:    &diskMetaQEMUMock{},
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	r0, _ := json.Marshal(testDiskCID)
	r1 := json.RawMessage(`42`) // integer, not JSON object
	_, err := h.Handle(context.Background(), []json.RawMessage{r0, r1}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-object metadata, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("want CloudError, got %v", err)
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error should mention 'metadata'; got %q", err.Error())
	}
}

// TestHandleSetDiskMetadata_ClusterStatusError verifies that a Cluster().ListStatus()
// failure propagates as an error from the handler.
func TestHandleSetDiskMetadata_ClusterStatusError(t *testing.T) {
	t.Parallel()

	clusterErr := fmt.Errorf("cluster unreachable")
	pve := &diskMetaClientMock{
		clusterSvc: &mockClusterService{statusErr: clusterErr},
		nodesSvc:   &diskMetaNodesMock{},
		qemuSvc:    &diskMetaQEMUMock{},
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	_, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from cluster status failure, got nil")
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("error should mention 'cluster'; got %q", err.Error())
	}
}

// TestHandleSetDiskMetadata_UpdateConfigError verifies that an UpdateQemuConfig
// SDK failure propagates as an error after the disk is found on a VM.
func TestHandleSetDiskMetadata_UpdateConfigError(t *testing.T) {
	t.Parallel()

	updateErr := fmt.Errorf("write conflict: locked")
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
		updateErr: updateErr,
	}
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, ""),
		},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from UpdateQemuConfig failure, got nil")
	}
	if !strings.Contains(err.Error(), "write conflict") {
		t.Errorf("error should propagate SDK message; got %q", err.Error())
	}
}

// setDiskMetadataCallHandler is a helper that invokes HandleSetDiskMetadata and
// returns the captured description from the nodes mock after the call.
func setDiskMetadataCallHandler(t *testing.T, nodesSvc *diskMetaNodesMock, qemuSvc *diskMetaQEMUMock, diskCID string, meta map[string]any) string {
	t.Helper()
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(diskCID, meta), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called — capturedDesc is nil")
	}
	return *nodesSvc.capturedDesc
}

// TestHandleSetDiskMetadata_SameCIDReplaces verifies that calling the handler twice
// with the same disk CID replaces that CID's metadata object wholesale. The second
// call's metadata wins; fields from the first call are NOT preserved (no field-level
// merge within a single CID entry).
func TestHandleSetDiskMetadata_SameCIDReplaces(t *testing.T) {
	t.Parallel()

	configKey := diskKey(testNode, int(testVMID))
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			configKey: vmConfigWithDisk(testDiskCID, ""),
		},
	}
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}

	// First call: write deployment=cf.
	desc1 := setDiskMetadataCallHandler(t, nodesSvc, qemuSvc, testDiskCID, map[string]any{"deployment": "cf"})
	if !strings.Contains(desc1, "cf") {
		t.Fatalf("first call: sentinel missing 'cf'; got: %s", desc1)
	}

	// Second call: write instance_id only — same diskCID → replaces first call's entry.
	qemuSvc.configs[configKey]["description"] = desc1
	nodesSvc.capturedDesc = nil

	clusterSvc2 := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve2 := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc2}
	h2 := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve2))
	_, err := h2.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{"instance_id": "vm-xyz"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("second call: UpdateQemuConfig not called")
	}
	desc2 := *nodesSvc.capturedDesc

	// Second call's value present.
	if !strings.Contains(desc2, "vm-xyz") {
		t.Errorf("second call: sentinel missing 'vm-xyz'; got: %s", desc2)
	}
	// First call's value replaced — "cf" must NOT appear as the deployment value.
	// The sentinel encodes the full metadata object, so "cf" should be absent.
	if strings.Contains(desc2, `"cf"`) {
		t.Errorf("second call: 'cf' should have been replaced, still present; got: %s", desc2)
	}
	// Exactly one sentinel block.
	if count := strings.Count(desc2, "<!--BOSH:"); count != 1 {
		t.Errorf("expected exactly 1 sentinel block, got %d; desc: %s", count, desc2)
	}
}

// TestHandleSetDiskMetadata_CrossDiskMergePreserved verifies that calling the handler
// for two different disk CIDs on the same VM preserves both CID entries in the sentinel.
// Call 1 writes diskCID A; call 2 (seeing call 1's description in config) writes diskCID B.
// The final sentinel must contain entries for both A and B.
func TestHandleSetDiskMetadata_CrossDiskMergePreserved(t *testing.T) {
	t.Parallel()

	const diskA = testDiskCID               // "local-lvm:vm-100-disk-0"
	const diskB = "local-lvm:vm-100-disk-1" // second disk on same VM
	configKey := diskKey(testNode, int(testVMID))

	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			configKey: {
				"scsi0": diskA,
				"scsi1": diskB,
			},
		},
	}
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}

	// Call 1: set metadata for disk A.
	desc1 := setDiskMetadataCallHandler(t, nodesSvc, qemuSvc, diskA, map[string]any{"deployment": "cf"})
	if !strings.Contains(desc1, diskA) {
		t.Fatalf("call 1: sentinel missing diskA CID; got: %s", desc1)
	}

	// Inject call 1's written description so call 2 sees it.
	qemuSvc.configs[configKey]["description"] = desc1
	nodesSvc.capturedDesc = nil

	// Call 2: set metadata for disk B (different CID, same VM).
	clusterSvc2 := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve2 := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc2}
	h2 := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve2))
	_, err := h2.Handle(context.Background(), makeMetaArgs(diskB, map[string]any{"instance_id": "vm-xyz"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("call 2: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("call 2: UpdateQemuConfig not called")
	}
	desc2 := *nodesSvc.capturedDesc

	// Both disk CIDs must be present in the merged sentinel.
	if !strings.Contains(desc2, diskA) {
		t.Errorf("call 2: sentinel lost diskA entry; got: %s", desc2)
	}
	if !strings.Contains(desc2, diskB) {
		t.Errorf("call 2: sentinel missing diskB entry; got: %s", desc2)
	}
	// Exactly one sentinel block.
	if count := strings.Count(desc2, "<!--BOSH:"); count != 1 {
		t.Errorf("expected exactly 1 sentinel block, got %d; desc: %s", count, desc2)
	}
}

// TestHandleSetDiskMetadata_CorruptedSentinel verifies that when the existing VM
// description contains a sentinel block with invalid JSON, the handler resets the
// sentinel and writes fresh metadata rather than failing or propagating the
// parse error.
func TestHandleSetDiskMetadata_CorruptedSentinel(t *testing.T) {
	t.Parallel()

	corruptDesc := "operator note\n<!--BOSH:{not valid json at all-->rest"
	configKey := diskKey(testNode, int(testVMID))
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			configKey: vmConfigWithDisk(testDiskCID, corruptDesc),
		},
	}
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {testVMID}},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(testDiskCID, map[string]any{"deployment": "reset-test"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("corrupted sentinel: unexpected error (should reset+rewrite): %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("corrupted sentinel: UpdateQemuConfig not called")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, "<!--BOSH:") {
		t.Errorf("corrupted sentinel: rewritten description missing sentinel block; got: %s", desc)
	}
	if !strings.Contains(desc, "reset-test") {
		t.Errorf("corrupted sentinel: rewritten description missing new metadata value; got: %s", desc)
	}
}

// TestHandleSetDiskMetadata_Dir_CID verifies that a dir-storage disk CID in the
// form "local:9001/vm-9001-disk-0.raw" is correctly parsed by ParseDiskCID
// (storage="local") and matched against the VM config. The handler treats CID
// formats as opaque after the initial colon-split; this test exercises that path.
func TestHandleSetDiskMetadata_Dir_CID(t *testing.T) {
	t.Parallel()

	const dirCID = "local:9001/vm-9001-disk-0.raw"
	const dirVMID = int64(9001)

	configKey := diskKey(testNode, int(dirVMID))
	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]interface{}{
			configKey: {
				"scsi0": dirCID,
			},
		},
	}
	nodesSvc := &diskMetaNodesMock{
		vmsByNode: map[string][]int64{testNode: {dirVMID}},
	}
	clusterSvc := &mockClusterService{statusResp: clusterWithNode(testNode)}
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(dirCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("dir CID: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("dir CID: UpdateQemuConfig not called — capturedDesc is nil")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, "<!--BOSH:") {
		t.Errorf("dir CID: description missing sentinel block; got: %s", desc)
	}
	if !strings.Contains(desc, dirCID) {
		t.Errorf("dir CID: sentinel missing disk CID %q; got: %s", dirCID, desc)
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2.
// Re-enable when integration-test harness provides a nfs pool via env.
//
// func TestHandleSetDiskMetadata_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0.
// Re-enable when integration-test harness provides a rbd pool via env.
//
// func TestHandleSetDiskMetadata_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0.
// Re-enable when integration-test harness provides a cephfs pool via env.
//
// func TestHandleSetDiskMetadata_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2.
// Re-enable when integration-test harness provides a cifs pool via env.
//
// func TestHandleSetDiskMetadata_CIFS_CID(t *testing.T) { ... }
