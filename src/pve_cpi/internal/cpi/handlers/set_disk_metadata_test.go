package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// Minimal mock for qemu.Service — only Config is used by set_disk_metadata.
// ---------------------------------------------------------------------------

type diskMetaQEMUMock struct {
	// configs maps "node:vmid" → config map.
	configs map[string]map[string]any
}

func (m *diskMetaQEMUMock) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	key := diskKey(node, vmid)
	if cfg, ok := m.configs[key]; ok {
		return cfg, nil
	}
	return map[string]any{}, nil
}

func (m *diskMetaQEMUMock) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("Create not expected")
}
func (m *diskMetaQEMUMock) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *diskMetaQEMUMock) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *diskMetaQEMUMock) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("Snapshot not expected")
}
func (m *diskMetaQEMUMock) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("DeleteSnapshot not expected")
}
func (m *diskMetaQEMUMock) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("ListSnapshots not expected")
}
func (m *diskMetaQEMUMock) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("RollbackSnapshot not expected")
}

var _ qemu.Service = (*diskMetaQEMUMock)(nil)

// ---------------------------------------------------------------------------
// Minimal nodes.Service mock — only UpdateQemuConfig used by set_disk_metadata.
// ListQemu is no longer called; findVMsHostingDisk uses ListResources directly.
// ---------------------------------------------------------------------------

// diskMetaNodesMock implements the nodes.Service interface. Only UpdateQemuConfig
// is implemented; all other methods panic.
type diskMetaNodesMock struct {
	sdknodes.Service // nil embed — panics on uncovered methods
	// capturedDesc captures the description value written by UpdateQemuConfig.
	capturedDesc *string
	// capturedTags captures the tags value written by UpdateQemuConfig.
	capturedTags *string
	// updateErr if set, UpdateQemuConfig returns this error.
	updateErr error
	// listQemuSrc, when set, backs the authoritative per-node listing
	// (pve.ListGuestsAuthoritative) with the suite's cluster fixture rows.
	// buildDiskMetaPVE wires it to the cluster service's ListResources.
	listQemuSrc func(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error)
}

// ListQemu serves the node's guests from the cluster fixture (empty when unwired).
func (m *diskMetaNodesMock) ListQemu(ctx context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	out := sdknodes.ListQemuResponse{}
	if m.listQemuSrc == nil {
		return &out, nil
	}
	rows, err := m.listQemuSrc(ctx, nil)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for _, raw := range *rows {
			var item struct {
				Node string `json:"node"`
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Node != node {
				continue
			}
			if item.Type != "" && item.Type != "qemu" {
				continue
			}
			out = append(out, raw)
		}
	}
	return &out, nil
}

func diskKey(node string, vmid int) string {
	return node + ":" + strconv.Itoa(vmid)
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

// compile-time interface check.
var _ sdknodes.Service = (*diskMetaNodesMock)(nil)

// ---------------------------------------------------------------------------
// diskMetaClientMock wires together the mock services.
// ---------------------------------------------------------------------------

type diskMetaClientMock struct {
	qemuSvc    *diskMetaQEMUMock
	nodesSvc   *diskMetaNodesMock
	clusterSvc sdkclusterapi.Service
}

func (c *diskMetaClientMock) QEMU() qemu.Service           { return c.qemuSvc }
func (c *diskMetaClientMock) Storage() storage.Service     { return nil }
func (c *diskMetaClientMock) CloudInit() cloudinit.Service { return nil }
func (c *diskMetaClientMock) Tasks() tasks.Service         { return nil }

// Nodes wires the nodes mock's authoritative listing source to the cluster
// fixture on first use (unless a test already set one), so every
// construction site of diskMetaClientMock gets pve.ListGuestsAuthoritative
// coverage without repeating the wiring.
func (c *diskMetaClientMock) Nodes() sdknodes.Service {
	if c.nodesSvc != nil && c.nodesSvc.listQemuSrc == nil && c.clusterSvc != nil {
		c.nodesSvc.listQemuSrc = c.clusterSvc.ListResources
	}
	return c.nodesSvc
}
func (c *diskMetaClientMock) Cluster() sdkclusterapi.Service         { return c.clusterSvc }
func (c *diskMetaClientMock) ClusterStorage() clusterstorage.Service { return nil }
func (c *diskMetaClientMock) Pools() pve.PoolService                 { return &noopPoolService{} }

// ---------------------------------------------------------------------------
// helper builders
// ---------------------------------------------------------------------------

const testVMID = int64(100)

// clusterResourcesWithVM builds a ListResourcesResponse with a single VM entry.
// This is used by findVMsHostingDisk (via Cluster().ListResources) after the B9 fix.
// node is always testNode in this suite; kept as parameter for call-site clarity.
//
//nolint:unparam // node kept for readability; call sites always pass testNode
func clusterResourcesWithVM(vmid int64, node string) *sdkclusterapi.ListResourcesResponse {
	type entry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
	}
	raw, _ := json.Marshal(entry{VMID: vmid, Node: node})
	resp := sdkclusterapi.ListResourcesResponse{raw}
	return &resp
}

// clusterResourcesWithVMs builds a ListResourcesResponse with multiple VM entries.
func clusterResourcesWithVMs(pairs ...struct {
	vmid int64
	node string
}) *sdkclusterapi.ListResourcesResponse {
	type entry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
	}
	resp := make(sdkclusterapi.ListResourcesResponse, 0, len(pairs))
	for _, p := range pairs {
		raw, _ := json.Marshal(entry{VMID: p.vmid, Node: p.node})
		resp = append(resp, raw)
	}
	return &resp
}

// diskMetaClusterSvc is a minimal cluster.Service that only implements ListResources.
type diskMetaClusterSvc struct {
	sdkclusterapi.Service // nil embed
	listFn                func(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error)
	listErr               error
	resp                  *sdkclusterapi.ListResourcesResponse
}

func (m *diskMetaClusterSvc) ListResources(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, params)
	}
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.resp != nil {
		return m.resp, nil
	}
	empty := sdkclusterapi.ListResourcesResponse{}
	return &empty, nil
}

// ListConfigNodes derives corosync membership from the same fixture rows
// (falling back to testNode), for pve.ListGuestsAuthoritative.
func (m *diskMetaClusterSvc) ListConfigNodes(ctx context.Context) (*sdkclusterapi.ListConfigNodesResponse, error) {
	rows, err := m.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	return authConfigNodesFromResources(rows, testNode), nil
}

var _ sdkclusterapi.Service = (*diskMetaClusterSvc)(nil)

// vmConfigWithDisk builds a QEMU config map that includes diskCID at scsi0.
//
//nolint:unparam // diskCID kept for call-site clarity; some callers vary it (cid local var)
func vmConfigWithDisk(diskCID, desc string) map[string]any {
	m := map[string]any{
		"scsi0": diskCID,
	}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func makeDiskMetaDeps(client pve.Client) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// makeDiskMetaDepsClient builds Deps from a generic pve.Client. Used by tests
// that wire custom mock implementations (e.g., diskMetaFullMock).
func makeDiskMetaDepsClient(client pve.Client) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// makeMetaArgs builds the two-element set_disk_metadata arg slice. The
// handler hard-rejects unenveloped disk_cid input, so a bare diskCID is
// wrapped in a pvd- envelope here; an already-encoded or empty diskCID is
// passed through unchanged so it is never double-wrapped and the handler's
// own empty-disk_cid rejection still hits that check directly.
func makeMetaArgs(t *testing.T, diskCID string, meta map[string]any) []json.RawMessage {
	t.Helper()
	if diskCID != "" && !strings.HasPrefix(diskCID, "pvd-") && !strings.HasPrefix(diskCID, "pvz-") {
		diskCID = mustEncodeDiskCID(t, diskCID, nil)
	}
	r0, _ := json.Marshal(diskCID)
	r1, _ := json.Marshal(meta)
	return []json.RawMessage{json.RawMessage(r0), json.RawMessage(r1)}
}

// buildDiskMetaPVE constructs a diskMetaClientMock with a cluster service that
// returns the given VM in its resource list and a QEMU mock that returns the
// given configs. The nodes mock is provided directly for UpdateQemuConfig capture.
func buildDiskMetaPVE(clusterSvc sdkclusterapi.Service, qemuCfgs map[string]map[string]any, nodesSvc *diskMetaNodesMock) *diskMetaClientMock {
	return &diskMetaClientMock{
		qemuSvc:    &diskMetaQEMUMock{configs: qemuCfgs},
		nodesSvc:   nodesSvc,
		clusterSvc: clusterSvc,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleSetDiskMetadata_AttachedSingleVM(t *testing.T) {
	t.Parallel()

	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, ""),
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	meta := map[string]any{"deployment": "cf", "instance_id": "vm-abc123"}
	result, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, meta), jsonrpc.Context{})
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

	nodesSvc := &diskMetaNodesMock{}
	// VM config has NO matching disk — disk is detached.
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): {"scsi0": "local-lvm:vm-100-disk-99"},
	}, nodesSvc)

	deps := handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    pve,
		Logger: logger,
	}

	h := handlers.HandleSetDiskMetadata(deps)
	result, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{}), jsonrpc.Context{})
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
	// Both vmids (100 and 200) have configs containing testDiskCID, triggering the
	// ambiguous-match error path in the handler.
	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithVMs(
			struct {
				vmid int64
				node string
			}{testVMID, testNode},
			struct {
				vmid int64
				node string
			}{vm2, testNode},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, ""),
		diskKey(testNode, int(vm2)):      vmConfigWithDisk(testDiskCID, ""),
	}, nodesSvc)

	hAmbig := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	ambigResult, ambigErr := hAmbig.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{}), jsonrpc.Context{})
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
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, existingDesc),
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
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

	pve := buildDiskMetaPVE(
		&diskMetaClusterSvc{},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
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

	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): {
			"scsi0": testDiskCID,
			"tags":  "env--prod;director--abc",
		},
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	meta := map[string]any{
		"deployment": "cf",
		"tags": map[string]any{
			"tier": "bronze",
		},
	}
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, meta), jsonrpc.Context{})
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

	pve := buildDiskMetaPVE(
		&diskMetaClusterSvc{},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
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

	pve := buildDiskMetaPVE(
		&diskMetaClusterSvc{},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	r0, _ := json.Marshal(mustEncodeDiskCID(t, testDiskCID, nil))
	r1 := json.RawMessage(`42`) // integer, not JSON object
	_, err := h.Handle(context.Background(), []json.RawMessage{r0, r1}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-object metadata, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("want CloudError, got %v", err)
	}
}

// TestHandleSetDiskMetadata_ListResourcesError — B9 fix: ListResources transport
// failure propagates as an error from the handler instead of silently succeeding.
func TestHandleSetDiskMetadata_ListResourcesError(t *testing.T) {
	t.Parallel()

	clusterErr := fmt.Errorf("cluster unreachable")
	pve := buildDiskMetaPVE(
		&diskMetaClusterSvc{listErr: clusterErr},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from ListResources failure, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) && !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("want Cloud or RetriableCloud error for cluster transport failure, got %T %v", err, err)
	}
}

// TestHandleSetDiskMetadata_ListResourcesTransient_IsRetriable pins the
// documented contract that transient transport errors from ListResources
// propagate as RETRIABLE: a pvedaemon worker recycle (HTTP 5xx) must reach
// the Director as ok_to_retry=true, not permanently fail the operation. This
// is the classification the plain-error test above deliberately does not pin.
func TestHandleSetDiskMetadata_ListResourcesTransient_IsRetriable(t *testing.T) {
	t.Parallel()

	pve := buildDiskMetaPVE(
		// ParseAPIError (not a hand-built struct) so the 5xx sentinel is set
		// and errors.Is(err, sdkerrors.ErrServer) classifies it as transient.
		&diskMetaClusterSvc{listErr: sdkerrors.ParseAPIError(500, []byte(`{"message":"pvedaemon worker exiting"}`))},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	_, err := h.Handle(fastRetryCtx(context.Background()), makeMetaArgs(t, testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from transient ListResources failure, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("5xx from ListResources must be retriable, got %T %v", err, err)
	}
}

// TestHandleSetDiskMetadata_UpdateConfigError verifies that an UpdateQemuConfig
// SDK failure propagates as an error after the disk is found on a VM.
func TestHandleSetDiskMetadata_UpdateConfigError(t *testing.T) {
	t.Parallel()

	updateErr := fmt.Errorf("write conflict: locked")
	nodesSvc := &diskMetaNodesMock{updateErr: updateErr}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, ""),
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from UpdateQemuConfig failure, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) && !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("want Cloud or RetriableCloud error for UpdateQemuConfig failure, got %T %v", err, err)
	}
}

// setDiskMetadataCallHandler is a helper that invokes HandleSetDiskMetadata and
// returns the captured description from the nodes mock after the call.
func setDiskMetadataCallHandler(t *testing.T, nodesSvc *diskMetaNodesMock, qemuSvc *diskMetaQEMUMock, clusterSvc sdkclusterapi.Service, diskCID string, meta map[string]any) string {
	t.Helper()
	pve := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, diskCID, meta), jsonrpc.Context{})
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
		configs: map[string]map[string]any{
			configKey: vmConfigWithDisk(testDiskCID, ""),
		},
	}
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}

	// First call: write deployment=cf.
	desc1 := setDiskMetadataCallHandler(t, nodesSvc, qemuSvc, clusterSvc, testDiskCID, map[string]any{"deployment": "cf"})
	if !strings.Contains(desc1, "cf") {
		t.Fatalf("first call: sentinel missing 'cf'; got: %s", desc1)
	}

	// Second call: write instance_id only — same diskCID → replaces first call's entry.
	qemuSvc.configs[configKey]["description"] = desc1
	nodesSvc.capturedDesc = nil

	clusterSvc2 := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve2 := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc2}
	h2 := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve2))
	_, err := h2.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"instance_id": "vm-xyz"}), jsonrpc.Context{})
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
func TestHandleSetDiskMetadata_CrossDiskMergePreserved(t *testing.T) {
	t.Parallel()

	const diskA = testDiskCID               // "local-lvm:vm-100-disk-0"
	const diskB = "local-lvm:vm-100-disk-1" // second disk on same VM
	configKey := diskKey(testNode, int(testVMID))

	qemuSvc := &diskMetaQEMUMock{
		configs: map[string]map[string]any{
			configKey: {
				"scsi0": diskA,
				"scsi1": diskB,
			},
		},
	}
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}

	// Call 1: set metadata for disk A.
	desc1 := setDiskMetadataCallHandler(t, nodesSvc, qemuSvc, clusterSvc, diskA, map[string]any{"deployment": "cf"})
	if !strings.Contains(desc1, diskA) {
		t.Fatalf("call 1: sentinel missing diskA CID; got: %s", desc1)
	}

	// Inject call 1's written description so call 2 sees it.
	qemuSvc.configs[configKey]["description"] = desc1
	nodesSvc.capturedDesc = nil

	// Call 2: set metadata for disk B (different CID, same VM).
	clusterSvc2 := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve2 := &diskMetaClientMock{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc2}
	h2 := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve2))
	_, err := h2.Handle(context.Background(), makeMetaArgs(t, diskB, map[string]any{"instance_id": "vm-xyz"}), jsonrpc.Context{})
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
		configs: map[string]map[string]any{
			configKey: vmConfigWithDisk(testDiskCID, corruptDesc),
		},
	}
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, qemuSvc.configs, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "reset-test"}), jsonrpc.Context{})
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
// (storage="local") and matched against the VM config.
func TestHandleSetDiskMetadata_Dir_CID(t *testing.T) {
	t.Parallel()

	const dirCID = "local:9001/vm-9001-disk-0.raw"
	const dirVMID = int64(9001)

	configKey := diskKey(testNode, int(dirVMID))
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(dirVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		configKey: {
			"scsi0": dirCID,
		},
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, dirCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
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

// TestHandleSetDiskMetadata_ExactVolidMatch — a diskCID "local-lvm:vm-100-disk-0"
// must NOT match a VM that holds "local-lvm:vm-100-disk-0-clone" (substring match
// would produce a false positive). Only exact volid equality or option-prefixed
// match "local-lvm:vm-100-disk-0,..." must match.
func TestHandleSetDiskMetadata_ExactVolidMatch(t *testing.T) {
	t.Parallel()

	const wantCID = "local-lvm:vm-100-disk-0"
	// VM config holds a different volid that contains wantCID as a substring.
	const similarButDifferentVolid = "local-lvm:vm-100-disk-0-clone"

	configKey := diskKey(testNode, int(testVMID))
	nodesSvc := &diskMetaNodesMock{}
	logger, _ := log.NewObservedLogger(log.LevelWarn)
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		configKey: {"scsi0": similarButDifferentVolid},
	}, nodesSvc)

	deps := handlers.Deps{
		Config: &config.CPIConfig{VMDiskFormat: "qcow2"},
		PVE:    pve,
		Logger: logger,
	}
	h := handlers.HandleSetDiskMetadata(deps)
	result, err := h.Handle(context.Background(), makeMetaArgs(t, wantCID, map[string]any{"k": "v"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("exact volid match: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("exact volid match: expected nil result (no-op), got %v", result)
	}
	// UpdateQemuConfig must NOT be called — the disk is not attached to any VM.
	if nodesSvc.capturedDesc != nil {
		t.Errorf("exact volid match: UpdateQemuConfig must not be called for a substring-only match; got desc: %s", *nodesSvc.capturedDesc)
	}
}

// TestHandleSetDiskMetadata_OptionStringVolidMatch — volid with option suffix
// "local-lvm:vm-100-disk-0,size=10G" must still match diskCID "local-lvm:vm-100-disk-0".
func TestHandleSetDiskMetadata_OptionStringVolidMatch(t *testing.T) {
	t.Parallel()

	const wantCID = "local-lvm:vm-100-disk-0"
	const volidWithOptions = "local-lvm:vm-100-disk-0,size=10G"

	configKey := diskKey(testNode, int(testVMID))
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		configKey: {"scsi0": volidWithOptions},
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, wantCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("option string volid match: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("option string volid match: UpdateQemuConfig not called — disk should have been found")
	}
}

// TestHandleSetDiskMetadata_TransportErrorPropagates — B9 fix: when ListResources
// returns a transport error, the handler must propagate it rather than treating it
// as "disk not attached" and returning nil.
func TestHandleSetDiskMetadata_TransportErrorPropagates(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("dial tcp: connection refused")
	pve := buildDiskMetaPVE(
		&diskMetaClusterSvc{listErr: transportErr},
		map[string]map[string]any{},
		&diskMetaNodesMock{},
	)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))

	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("transport error: expected error, got nil")
	}
	// Must NOT be "disk not attached" — that must only happen when the cluster scan
	// completes successfully and finds no match.
	if strings.Contains(err.Error(), "not attached") {
		t.Errorf("transport error must not be reported as 'not attached'; got: %v", err)
	}
}

// TestSetDiskMetadata_UpdateTransient_Retriable verifies that when SDK calls in
// persistMetadata and applyCustomTagsToVM return a transient 5xx error, the
// WrapError wrapping inside those functions preserves the Retriable classification
// so the BOSH director re-issues the request rather than treating it as permanent.
//
// Three subtests, one per write site:
//   - "persist_metadata_update": UpdateQemuConfig in persistMetadata (disk metadata write).
//   - "apply_tags_config_fetch": Config() in applyCustomTagsToVM (tag-merge read).
//   - "apply_tags_update": UpdateQemuConfig in applyCustomTagsToVM (tag write).
func TestSetDiskMetadata_UpdateTransient_Retriable(t *testing.T) {
	t.Parallel()

	// ConnectionError is always classified as transient by WrapError → RetriableCloud.
	transientErr := &sdkerrors.ConnectionError{Host: "pve.test.local", Port: 8006, Message: "connection reset by peer"}

	const cid = testDiskCID
	configKey := diskKey(testNode, int(testVMID))
	baseConfigs := map[string]map[string]any{
		configKey: vmConfigWithDisk(cid, ""),
	}

	// subtest helper: builds and invokes the handler, then asserts Retriable error.
	runCase := func(t *testing.T, name string, qemuSvc qemu.Service, nodesSvc sdknodes.Service, meta map[string]any) {
		t.Helper()
		clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
		client := &diskMetaFullMock{
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			clusterSvc: clusterSvc,
		}
		h := handlers.HandleSetDiskMetadata(makeDiskMetaDepsClient(client))
		_, err := h.Handle(context.Background(), makeMetaArgs(t, cid, meta), jsonrpc.Context{})
		if err == nil {
			t.Fatalf("%s: expected error from transient SDK failure, got nil", name)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) {
			t.Fatalf("%s: expected *cpierrors.Error, got %T: %v", name, err, err)
		}
		if cpiErr.Type() != cpierrors.TypeRetriableCloud {
			t.Errorf("%s: error type = %q; want %q (Retriable)", name, cpiErr.Type(), cpierrors.TypeRetriableCloud)
		}
	}

	// Site 1: UpdateQemuConfig in persistMetadata fails with transient error.
	// No tags in metadata so applyCustomTagsToVM is not called.
	t.Run("persist_metadata_update", func(t *testing.T) {
		t.Parallel()
		qemuSvc := &diskMetaQEMUMock{configs: baseConfigs}
		nodesSvc := &diskMetaNodesMock{updateErr: transientErr}
		runCase(t, "persist_metadata_update", qemuSvc, nodesSvc, map[string]any{"deployment": "cf"})
	})

	// Site 2: Config() in applyCustomTagsToVM fails with transient error.
	// persistMetadata (first UpdateQemuConfig) succeeds; the second Config()
	// call — inside applyCustomTagsToVM — returns the transient error.
	// findVMsHostingDisk calls Config once; persistMetadata calls Config once;
	// applyCustomTagsToVM calls Config a third time — inject error on call ≥ 3.
	t.Run("apply_tags_config_fetch", func(t *testing.T) {
		t.Parallel()
		configCallCount := 0
		qemuSvc := &configFnQEMU{
			base: &diskMetaQEMUMock{configs: baseConfigs},
			fn: func(_ context.Context, node string, vmid int) (map[string]any, error) {
				configCallCount++
				// findVMsHostingDisk = call 1 (scan); persistMetadata = call 2; applyCustomTagsToVM = call 3.
				if configCallCount >= 3 {
					return nil, transientErr
				}
				key := diskKey(node, vmid)
				if cfg, ok := baseConfigs[key]; ok {
					return cfg, nil
				}
				return map[string]any{}, nil
			},
		}
		// UpdateQemuConfig succeeds for persistMetadata (call 1).
		nodesSvc := &diskMetaNodesMock{}
		meta := map[string]any{"deployment": "cf", "tags": map[string]any{"tier": "gold"}}
		runCase(t, "apply_tags_config_fetch", qemuSvc, nodesSvc, meta)
	})

	// Site 3: UpdateQemuConfig in applyCustomTagsToVM fails with transient error.
	// persistMetadata's UpdateQemuConfig (first call) succeeds; the second call
	// (tags write in applyCustomTagsToVM) returns the transient error.
	t.Run("apply_tags_update", func(t *testing.T) {
		t.Parallel()
		updateCallCount := 0
		captured := &diskMetaNodesMock{}
		nodesSvc := &updateCaptureMock{
			updateFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				updateCallCount++
				if updateCallCount >= 2 {
					return transientErr
				}
				// Capture first write (persistMetadata) so the test helper sees it.
				if params != nil && params.Description != nil {
					captured.capturedDesc = params.Description
				}
				return nil
			},
		}
		qemuSvc := &diskMetaQEMUMock{configs: baseConfigs}
		meta := map[string]any{"deployment": "cf", "tags": map[string]any{"tier": "gold"}}
		runCase(t, "apply_tags_update", qemuSvc, nodesSvc, meta)
	})
}

// configFnQEMU wraps diskMetaQEMUMock, overriding Config with a custom function.
// Used by tests that need per-call error injection on Config.
type configFnQEMU struct {
	base *diskMetaQEMUMock
	fn   func(context.Context, string, int) (map[string]any, error)
}

func (c *configFnQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if c.fn != nil {
		return c.fn(ctx, node, vmid)
	}
	return c.base.Config(ctx, node, vmid)
}

func (c *configFnQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	return c.base.Create(ctx, node, params)
}
func (c *configFnQEMU) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	return c.base.Status(ctx, node, vmid)
}
func (c *configFnQEMU) Start(ctx context.Context, node string, vmid int) (string, error) {
	return c.base.Start(ctx, node, vmid)
}
func (c *configFnQEMU) Stop(ctx context.Context, node string, vmid int) (string, error) {
	return c.base.Stop(ctx, node, vmid)
}
func (c *configFnQEMU) Reset(ctx context.Context, node string, vmid int) (string, error) {
	return c.base.Reset(ctx, node, vmid)
}
func (c *configFnQEMU) Clone(ctx context.Context, node string, vmid int, params map[string]any) (string, error) {
	return c.base.Clone(ctx, node, vmid, params)
}
func (c *configFnQEMU) Template(ctx context.Context, node string, vmid int) (string, error) {
	return c.base.Template(ctx, node, vmid)
}
func (c *configFnQEMU) AttachDisk(ctx context.Context, node string, vmid int, slot, volid string, opts *qemu.AttachOpts) (string, error) {
	return c.base.AttachDisk(ctx, node, vmid, slot, volid, opts)
}
func (c *configFnQEMU) DetachDisk(ctx context.Context, node string, vmid int, slot string) error {
	return c.base.DetachDisk(ctx, node, vmid, slot)
}
func (c *configFnQEMU) ResizeDisk(ctx context.Context, node string, vmid int, slot string, sizeGB int) (string, error) {
	return c.base.ResizeDisk(ctx, node, vmid, slot, sizeGB)
}
func (c *configFnQEMU) Snapshot(ctx context.Context, node string, vmid int, name string, params map[string]any) (string, error) {
	return c.base.Snapshot(ctx, node, vmid, name, params)
}
func (c *configFnQEMU) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) error {
	return c.base.DeleteSnapshot(ctx, node, vmid, name)
}
func (c *configFnQEMU) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]any, error) {
	return c.base.ListSnapshots(ctx, node, vmid)
}
func (c *configFnQEMU) RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (string, error) {
	return c.base.RollbackSnapshot(ctx, node, vmid, name)
}

var _ qemu.Service = (*configFnQEMU)(nil)

// updateCaptureMock implements nodes.Service with only UpdateQemuConfig wired.
// All other methods delegate to the embedded panicNodesStub (panic on call).
type updateCaptureMock struct {
	panicNodesStub
	updateFn func(context.Context, string, string, *sdknodes.UpdateQemuConfigParams) error
}

// ListQemu returns an empty node: the authoritative enumeration reaches every
// wired nodes service, and this mock's suite scripts guests through the
// cluster fixture instead.
func (m *updateCaptureMock) ListQemu(context.Context, string, *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

func (m *updateCaptureMock) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, node, vmid, params)
	}
	return nil
}

var _ sdknodes.Service = (*updateCaptureMock)(nil)

// diskMetaFullMock wires a qemu.Service and nodes.Service for transient tests.
// Both fields accept the interface to allow configFnQEMU / updateCaptureMock.
type diskMetaFullMock struct {
	qemuSvc    qemu.Service
	nodesSvc   sdknodes.Service
	clusterSvc sdkclusterapi.Service
}

func (c *diskMetaFullMock) QEMU() qemu.Service           { return c.qemuSvc }
func (c *diskMetaFullMock) Storage() storage.Service     { return nil }
func (c *diskMetaFullMock) CloudInit() cloudinit.Service { return nil }
func (c *diskMetaFullMock) Tasks() tasks.Service         { return nil }

// Nodes wraps the wired nodes service so pve.ListGuestsAuthoritative sees
// the guests scripted through the cluster fixture; every other method
// delegates to the suite's mock.
func (c *diskMetaFullMock) Nodes() sdknodes.Service {
	if c.clusterSvc == nil {
		return c.nodesSvc
	}
	return &authNodesService{Service: c.nodesSvc, listFn: c.clusterSvc.ListResources, fallbackNode: testNode}
}
func (c *diskMetaFullMock) Cluster() sdkclusterapi.Service         { return c.clusterSvc }
func (c *diskMetaFullMock) ClusterStorage() clusterstorage.Service { return nil }
func (c *diskMetaFullMock) Pools() pve.PoolService                 { return &noopPoolService{} }

// TestHandleSetDiskMetadata_TransientConfigErrorDuringScan_Retriable verifies
// that a transient per-VM Config fault during the attachment scan surfaces as
// a retriable error rather than being silently skipped — a skip could yield a
// false 0-match (metadata dropped) or a false 1-match (masked multi-attach).
func TestHandleSetDiskMetadata_TransientConfigErrorDuringScan_Retriable(t *testing.T) {
	t.Parallel()

	const faultyVM = int64(100)
	const hostingVM = int64(200)
	transientErr := &sdkerrors.ConnectionError{Host: "pve.test.local", Port: 8006, Message: "connection reset by peer"}

	baseConfigs := map[string]map[string]any{
		diskKey(testNode, int(hostingVM)): vmConfigWithDisk(testDiskCID, ""),
	}
	qemuSvc := &configFnQEMU{
		base: &diskMetaQEMUMock{configs: baseConfigs},
		fn: func(ctx context.Context, node string, vmid int) (map[string]any, error) {
			if vmid == int(faultyVM) {
				return nil, transientErr
			}
			return (&diskMetaQEMUMock{configs: baseConfigs}).Config(ctx, node, vmid)
		},
	}
	client := &diskMetaFullMock{
		qemuSvc:  qemuSvc,
		nodesSvc: &diskMetaNodesMock{},
		clusterSvc: &diskMetaClusterSvc{resp: clusterResourcesWithVMs(
			struct {
				vmid int64
				node string
			}{faultyVM, testNode},
			struct {
				vmid int64
				node string
			}{hostingVM, testNode},
		)},
	}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDepsClient(client))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("transient config error during scan: expected error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient config error during scan: want retriable-cloud, got %v", err)
	}
}

// TestHandleSetDiskMetadata_NotFoundSkippedDuringScan verifies that a 404 from
// one VM's Config (deleted concurrently) is skipped while the scan continues,
// and metadata is persisted on the VM that does host the disk.
func TestHandleSetDiskMetadata_NotFoundSkippedDuringScan(t *testing.T) {
	t.Parallel()

	const goneVM = int64(100)
	const hostingVM = int64(200)
	notFoundErr := &sdkerrors.APIError{HTTPCode: 404, Message: "VM not found"}

	baseConfigs := map[string]map[string]any{
		diskKey(testNode, int(hostingVM)): vmConfigWithDisk(testDiskCID, ""),
	}
	qemuSvc := &configFnQEMU{
		base: &diskMetaQEMUMock{configs: baseConfigs},
		fn: func(ctx context.Context, node string, vmid int) (map[string]any, error) {
			if vmid == int(goneVM) {
				return nil, notFoundErr
			}
			return (&diskMetaQEMUMock{configs: baseConfigs}).Config(ctx, node, vmid)
		},
	}
	nodesSvc := &diskMetaNodesMock{}
	client := &diskMetaFullMock{
		qemuSvc:  qemuSvc,
		nodesSvc: nodesSvc,
		clusterSvc: &diskMetaClusterSvc{resp: clusterResourcesWithVMs(
			struct {
				vmid int64
				node string
			}{goneVM, testNode},
			struct {
				vmid int64
				node string
			}{hostingVM, testNode},
		)},
	}

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDepsClient(client))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("404 during scan: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("404 during scan: metadata not persisted on the hosting VM")
	}
	if !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Errorf("404 during scan: persisted description missing disk_cid; got: %s", *nodesSvc.capturedDesc)
	}
}

// ---------------------------------------------------------------------------
// Parker exclusion tests
// ---------------------------------------------------------------------------

// clusterResourceEntry holds vmid, node, and optional tags for building
// cluster-resource responses in parker exclusion tests.
type clusterResourceEntry struct {
	vmid int64
	node string
	tags string
}

// clusterResourcesWithTaggedVMs builds a ListResourcesResponse that includes
// a "tags" field in each entry. Used to simulate cluster responses where
// some VMs carry the "bosh-parker" tag.
func clusterResourcesWithTaggedVMs(entries ...clusterResourceEntry) *sdkclusterapi.ListResourcesResponse {
	type entry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Tags string `json:"tags,omitempty"`
	}
	resp := make(sdkclusterapi.ListResourcesResponse, 0, len(entries))
	for _, e := range entries {
		raw, _ := json.Marshal(entry{VMID: e.vmid, Node: e.node, Tags: e.tags})
		resp = append(resp, raw)
	}
	return &resp
}

// makeParkedDeps builds Deps with the parked strategy active and the given
// parker VMID range. DirectorID is left empty.
func makeParkedDeps(client pve.Client, rangeStart, rangeEnd int) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			VMDiskFormat:             "qcow2",
			DetachedDiskStrategy:     "parked",
			ParkedDiskVMIDRangeStart: rangeStart,
			ParkedDiskVMIDRangeEnd:   rangeEnd,
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// TestHandleSetDiskMetadata_ParkerSkipped_RealVMMatches verifies that when the
// cluster resource list includes both a parker VM and a real VM, the parker VM
// is skipped and metadata is persisted on the real VM only.
func TestHandleSetDiskMetadata_ParkerSkipped_RealVMMatches(t *testing.T) {
	t.Parallel()

	const parkerVMID = int64(90001)
	const realVMID = int64(200)
	const parkerRangeStart = 90000
	const parkerRangeEnd = 90999

	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			clusterResourceEntry{vmid: parkerVMID, node: testNode, tags: "bosh-parker"},
			clusterResourceEntry{vmid: realVMID, node: testNode, tags: ""},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	// Both VMs "hold" the disk in their QEMU config. The parker must be excluded
	// so only the real VM is counted — producing exactly 1 match and triggering
	// the metadata-persist path rather than the ambiguous-attachment error.
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(parkerVMID)): vmConfigWithDisk(testDiskCID, ""),
		diskKey(testNode, int(realVMID)):   vmConfigWithDisk(testDiskCID, ""),
	}
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeParkedDeps(client, parkerRangeStart, parkerRangeEnd))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("parker skip + real VM: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("parker skip + real VM: UpdateQemuConfig not called — metadata not persisted on real VM")
	}
	if !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Errorf("parker skip + real VM: persisted description missing disk_cid; got: %s", *nodesSvc.capturedDesc)
	}
}

// TestHandleSetDiskMetadata_ParkerWithEmptyRowTags_StillSkipped covers the PVE
// that does not populate "tags" on a cluster-resources row. An empty field
// cannot tell an untagged VM from an unpopulated one, so deciding on it alone
// would treat the parker as a real holder and merge deployment metadata into the
// description that carries its provenance sentinel. The config read the scan
// already performs carries the tags, so the second test costs nothing.
func TestHandleSetDiskMetadata_ParkerWithEmptyRowTags_StillSkipped(t *testing.T) {
	t.Parallel()

	const parkerVMID = int64(90001)

	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			clusterResourceEntry{vmid: parkerVMID, node: testNode, tags: ""},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	parkerCfg := vmConfigWithDisk(testDiskCID, "")
	parkerCfg["tags"] = "bosh-cpi;bosh-parker"
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(parkerVMID)): parkerCfg,
	}
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeParkedDeps(client, 90000, 90999))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("a parked disk is the warn-and-return-nil path, not an error: %v", err)
	}
	if nodesSvc.capturedDesc != nil {
		t.Errorf("metadata was written into the parker description: %s", *nodesSvc.capturedDesc)
	}
}

// TestHandleSetDiskMetadata_ParkedDiskOnly_WarnAndNil verifies that when the
// only VM holding the disk is a parker VM, findVMsHostingDisk returns 0
// matches, and the handler logs a warn and returns nil (the existing
// "not attached" path fires unchanged).
func TestHandleSetDiskMetadata_ParkedDiskOnly_WarnAndNil(t *testing.T) {
	t.Parallel()

	const parkerVMID = int64(90001)
	const parkerRangeStart = 90000
	const parkerRangeEnd = 90999

	logger, logs := log.NewObservedLogger(log.LevelWarn)
	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			clusterResourceEntry{vmid: parkerVMID, node: testNode, tags: "bosh-parker"},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(parkerVMID)): vmConfigWithDisk(testDiskCID, ""),
	}
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)

	deps := handlers.Deps{
		Config: &config.CPIConfig{
			VMDiskFormat:             "qcow2",
			DetachedDiskStrategy:     "parked",
			ParkedDiskVMIDRangeStart: parkerRangeStart,
			ParkedDiskVMIDRangeEnd:   parkerRangeEnd,
		},
		PVE:    client,
		Logger: logger,
	}

	h := handlers.HandleSetDiskMetadata(deps)
	result, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("parked-only: unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("parked-only: expected nil result, got %v", result)
	}
	// UpdateQemuConfig must NOT be called.
	if nodesSvc.capturedDesc != nil {
		t.Errorf("parked-only: UpdateQemuConfig must not be called; got desc: %s", *nodesSvc.capturedDesc)
	}
	// Warn must be logged (existing not-attached path).
	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("parked-only: expected warn log, got none")
	}
}

// TestHandleSetDiskMetadata_ZeroConfig_ParkerSkipped verifies that a VM in the
// default parker band carrying the "bosh-parker" tag is skipped even when the
// manifest names neither the strategy nor the band. Both come from the parked
// default, so the exclusion that keeps deployment metadata off a parker VM
// applies to a config that sets nothing.
func TestHandleSetDiskMetadata_ZeroConfig_ParkerSkipped(t *testing.T) {
	t.Parallel()

	const parkerTaggedVMID = int64(90001)

	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			clusterResourceEntry{vmid: parkerTaggedVMID, node: testNode, tags: "bosh-parker"},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(parkerTaggedVMID)): vmConfigWithDisk(testDiskCID, ""),
	}
	// Zero-config Deps: no DetachedDiskStrategy, no VMID range.
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("zero-config: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc != nil {
		t.Errorf("zero-config: parker VM must be skipped under the parked default; got desc: %s", *nodesSvc.capturedDesc)
	}
}

// TestHandleSetDiskMetadata_StrategyFree_StrandedParkerSkipped is the opt-out
// counterpart. With strategy=free and no band nothing classifies a VM by its
// VMID, but a "bosh-parker" tag still says what the VM is, and that is the
// configuration an operator lands in by dropping the band while parkers still
// stand. Writing there merges deployment metadata into the parker's
// description — the field carrying the provenance sentinel for every disk it
// holds. Skipping instead costs a warn and an unpersisted annotation for a disk
// that is detached anyway.
func TestHandleSetDiskMetadata_StrategyFree_StrandedParkerSkipped(t *testing.T) {
	t.Parallel()

	const parkerTaggedVMID = int64(90001)

	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			clusterResourceEntry{vmid: parkerTaggedVMID, node: testNode, tags: "bosh-parker"},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(parkerTaggedVMID)): vmConfigWithDisk(testDiskCID, ""),
	}
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)
	deps := makeDiskMetaDeps(client)
	deps.Config.DetachedDiskStrategy = "free"
	h := handlers.HandleSetDiskMetadata(deps)

	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("strategy=free: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc != nil {
		t.Errorf("strategy=free: a parker-tagged VM must be skipped whatever the band says; got desc: %s",
			*nodesSvc.capturedDesc)
	}
}

// TestHandleSetDiskMetadata_RangeOnlyNoTag_NotSkipped verifies that a VM
// whose VMID falls in the parker range but does NOT carry the "bosh-parker"
// tag is not skipped. IsParkerVM requires both range AND tag.
func TestHandleSetDiskMetadata_RangeOnlyNoTag_NotSkipped(t *testing.T) {
	t.Parallel()

	const vmid = int64(90001) // in parker range
	const parkerRangeStart = 90000
	const parkerRangeEnd = 90999

	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithTaggedVMs(
			// No "bosh-parker" tag — range match alone must not skip.
			clusterResourceEntry{vmid: vmid, node: testNode, tags: "some-other-tag"},
		),
	}
	nodesSvc := &diskMetaNodesMock{}
	qemuCfgs := map[string]map[string]any{
		diskKey(testNode, int(vmid)): vmConfigWithDisk(testDiskCID, ""),
	}
	client := buildDiskMetaPVE(clusterSvc, qemuCfgs, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeParkedDeps(client, parkerRangeStart, parkerRangeEnd))
	_, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("range-only no tag: unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil {
		t.Fatal("range-only no tag: UpdateQemuConfig not called — VM wrongly skipped despite missing bosh-parker tag")
	}
}

// ---------------------------------------------------------------------------
// Foreign sentinel key preservation: the description sentinel is shared with
// other codecs (bosh_attached_disks from attach_disk, bosh_parked_disks).
// Both description writers in this handler must pass unknown top-level keys
// through untouched — dropping bosh_attached_disks makes a later get_disks
// fall back to bare volids and every envelope-CID disk scans as missing.
// ---------------------------------------------------------------------------

func TestHandleSetDiskMetadata_PreservesForeignSentinelKeys(t *testing.T) {
	t.Parallel()

	existingDesc := `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-100-disk-0":"pvd-recordedCID"}}-->`
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): vmConfigWithDisk(testDiskCID, existingDesc),
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	meta := map[string]any{"deployment": "cf", "instance_id": "vm-abc123"}
	if _, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, meta), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called — capturedDesc is nil")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, `"bosh_attached_disks"`) || !strings.Contains(desc, "pvd-recordedCID") {
		t.Errorf("metadata write dropped foreign sentinel key bosh_attached_disks; got: %s", desc)
	}
	if !strings.Contains(desc, `"bosh_disk_metadata"`) {
		t.Errorf("description missing bosh_disk_metadata after write; got: %s", desc)
	}
}

func TestHandleSetDiskMetadata_TagsPathPreservesForeignSentinelKeys(t *testing.T) {
	t.Parallel()

	existingDesc := `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-100-disk-0":"pvd-recordedCID"}}-->`
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	cfg := vmConfigWithDisk(testDiskCID, existingDesc)
	cfg["tags"] = "env--prod"
	pve := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): cfg,
	}, nodesSvc)

	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(pve))
	meta := map[string]any{
		"deployment": "cf",
		"tags":       map[string]any{"tier": "bronze"},
	}
	if _, err := h.Handle(context.Background(), makeMetaArgs(t, testDiskCID, meta), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if nodesSvc.capturedDesc == nil {
		t.Fatal("UpdateQemuConfig not called — capturedDesc is nil")
	}
	desc := *nodesSvc.capturedDesc
	if !strings.Contains(desc, `"bosh_attached_disks"`) || !strings.Contains(desc, "pvd-recordedCID") {
		t.Errorf("tags write dropped foreign sentinel key bosh_attached_disks; got: %s", desc)
	}
	if !strings.Contains(desc, `"bosh_disk_tags"`) {
		t.Errorf("description missing bosh_disk_tags after write; got: %s", desc)
	}
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (m *diskMetaClusterSvc) ListStatus(context.Context) (*sdkclusterapi.ListStatusResponse, error) {
	empty := sdkclusterapi.ListStatusResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (m *diskMetaNodesMock) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (m *updateCaptureMock) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}
