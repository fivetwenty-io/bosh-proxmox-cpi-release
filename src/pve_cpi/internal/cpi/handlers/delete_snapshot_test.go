package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// delSnapQEMUService: QEMU mock for delete_snapshot tests.
// ---------------------------------------------------------------------------

type delSnapQEMUService struct {
	deleteSnapshotFn func(ctx context.Context, node string, vmid int, name string) error
	listSnapshotsFn  func(ctx context.Context, node string, vmid int) ([]map[string]any, error)
}

func (m *delSnapQEMUService) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) error {
	if m.deleteSnapshotFn != nil {
		return m.deleteSnapshotFn(ctx, node, vmid, name)
	}
	return nil
}

// Unimplemented methods.
func (m *delSnapQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("delSnapQEMUService.Create: not expected")
}
func (m *delSnapQEMUService) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("delSnapQEMUService.Config: not expected")
}
func (m *delSnapQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("delSnapQEMUService.Status: not expected")
}
func (m *delSnapQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("delSnapQEMUService.Start: not expected")
}
func (m *delSnapQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("delSnapQEMUService.Stop: not expected")
}
func (m *delSnapQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("delSnapQEMUService.Reset: not expected")
}
func (m *delSnapQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("delSnapQEMUService.Clone: not expected")
}
func (m *delSnapQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("delSnapQEMUService.Template: not expected")
}
func (m *delSnapQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("delSnapQEMUService.AttachDisk: not expected")
}
func (m *delSnapQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("delSnapQEMUService.DetachDisk: not expected")
}
func (m *delSnapQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("delSnapQEMUService.ResizeDisk: not expected")
}
func (m *delSnapQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("delSnapQEMUService.Snapshot: not expected")
}
func (m *delSnapQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]any, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
	// Default: snapshot already gone, so WaitForSnapshotAbsent returns immediately.
	return []map[string]any{{"name": "current"}}, nil
}
func (m *delSnapQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("delSnapQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*delSnapQEMUService)(nil)

// ---------------------------------------------------------------------------
// delSnapDeps builds Deps for delete_snapshot tests.
// ---------------------------------------------------------------------------

func delSnapDeps(qemuSvc qemu.Service) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node: "pve1",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: defaultClusterSvc(100, "pve1"),
		},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleDeleteSnapshot_Happy(t *testing.T) {
	var deleteCalled bool
	var deletedVMID int
	var deletedSnap string

	qemuSvc := &delSnapQEMUService{
		deleteSnapshotFn: func(_ context.Context, _ string, vmid int, name string) error {
			deleteCalled = true
			deletedVMID = vmid
			deletedSnap = name
			return nil
		},
	}

	h := handlers.HandleDeleteSnapshot(delSnapDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100:bosh-1234567890-abcd1234"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil (void), got %v", result)
	}
	if !deleteCalled {
		t.Error("DeleteSnapshot was not called")
	}
	if deletedVMID != 100 {
		t.Errorf("vmid: want 100, got %d", deletedVMID)
	}
	if deletedSnap != "bosh-1234567890-abcd1234" {
		t.Errorf("snap_name: want bosh-1234567890-abcd1234, got %q", deletedSnap)
	}
}

func TestHandleDeleteSnapshot_NotFound_Idempotent(t *testing.T) {
	qemuSvc := &delSnapQEMUService{
		deleteSnapshotFn: func(_ context.Context, _ string, _ int, _ string) error {
			return &sdkerrors.APIError{HTTPCode: 404, Message: "snapshot not found"}
		},
	}

	h := handlers.HandleDeleteSnapshot(delSnapDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100:bosh-snap"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil for 404, got: %v", err)
	}
	if result != nil {
		t.Errorf("result: want nil, got %v", result)
	}
}

func TestHandleDeleteSnapshot_SDKError(t *testing.T) {
	qemuSvc := &delSnapQEMUService{
		deleteSnapshotFn: func(_ context.Context, _ string, _ int, _ string) error {
			return errors.New("PVE internal error during snapshot delete")
		},
	}

	h := handlers.HandleDeleteSnapshot(delSnapDeps(qemuSvc))
	_, err := h.Handle(context.Background(), marshalArgs("100:bosh-snap"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from SDK delete failure")
	}
}

func TestHandleDeleteSnapshot_MalformedCID_NoColon(t *testing.T) {
	h := handlers.HandleDeleteSnapshot(delSnapDeps(&delSnapQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("no-colon-snapshot-cid"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed snapshot_cid with no colon")
	}
}

func TestHandleDeleteSnapshot_MalformedCID_InvalidVMID(t *testing.T) {
	// Snapshot CID with non-integer vm part.
	h := handlers.HandleDeleteSnapshot(delSnapDeps(&delSnapQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("not-a-vmid:snap"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-integer vmid in snapshot_cid")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

func TestHandleDeleteSnapshot_EmptyCID(t *testing.T) {
	h := handlers.HandleDeleteSnapshot(delSnapDeps(&delSnapQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs(""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty snapshot_cid")
	}
}

func TestHandleDeleteSnapshot_TooFewArgs(t *testing.T) {
	h := handlers.HandleDeleteSnapshot(delSnapDeps(&delSnapQEMUService{}))
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing snapshot_cid argument")
	}
}

func TestHandleDeleteSnapshot_MissingNode(t *testing.T) {
	// With the cluster-scan flow, the handler no longer validates Config.Node
	// before calling FindVMNodeViaCluster. A cluster service returning a
	// transport error is the new equivalent: the handler returns a cloud error.
	clusterErr := errors.New("cluster unavailable: connection refused")
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return nil, clusterErr
		},
	}
	qemuSvc := &delSnapQEMUService{}
	h := handlers.HandleDeleteSnapshot(handlers.Deps{
		Config: &config.CPIConfig{Node: ""},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: clusterSvc},
		Logger: log.NewNopLogger(),
	})
	_, err := h.Handle(context.Background(), marshalArgs("100:bosh-snap"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cluster service fails")
	}
}

func TestHandleDeleteSnapshot_ZeroVMID(t *testing.T) {
	// VMID "0" should be rejected as invalid.
	h := handlers.HandleDeleteSnapshot(delSnapDeps(&delSnapQEMUService{}))
	_, err := h.Handle(context.Background(), marshalArgs("0:bosh-snap"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for vmid=0")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}
