package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// attachQEMUService: QEMU mock for attach_disk tests.
// Wraps mockQEMUService from testmocks_test.go and adds AttachDisk control.
// ---------------------------------------------------------------------------

type attachQEMUService struct {
	// AttachDisk control.
	attachReturnDiskID string
	attachErr          error

	// Config control (used by ResolveDiskID after attach).
	configCfg map[string]interface{}
	configErr error
}

func (m *attachQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	return m.attachReturnDiskID, m.attachErr
}

func (m *attachQEMUService) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	return m.configCfg, m.configErr
}

// Remaining Service methods — panic on accidental call.
func (m *attachQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("attachQEMUService.Create: not expected")
}
func (m *attachQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("attachQEMUService.Status: not expected")
}
func (m *attachQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("attachQEMUService.Start: not expected")
}
func (m *attachQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("attachQEMUService.Stop: not expected")
}
func (m *attachQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("attachQEMUService.Reset: not expected")
}
func (m *attachQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("attachQEMUService.Clone: not expected")
}
func (m *attachQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("attachQEMUService.Template: not expected")
}
func (m *attachQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("attachQEMUService.DetachDisk: not expected")
}
func (m *attachQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("attachQEMUService.ResizeDisk: not expected")
}
func (m *attachQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("attachQEMUService.Snapshot: not expected")
}
func (m *attachQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("attachQEMUService.DeleteSnapshot: not expected")
}
func (m *attachQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("attachQEMUService.ListSnapshots: not expected")
}
func (m *attachQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("attachQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*attachQEMUService)(nil)

// ---------------------------------------------------------------------------
// captureAgent: records UpdateDiskHints calls for assertion.
// ---------------------------------------------------------------------------

type captureAgent struct {
	updateCalled bool
	updateVMID   int
	updateHints  []agent.DiskHint
	updateErr    error
}

func (a *captureAgent) Configure(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
	return nil
}
func (a *captureAgent) Remove(_ context.Context, _ string, _ int) error { return nil }
func (a *captureAgent) UpdateDiskHints(_ context.Context, vmid int, disks []agent.DiskHint) error {
	a.updateCalled = true
	a.updateVMID = vmid
	a.updateHints = disks
	return a.updateErr
}

var _ agent.Agent = (*captureAgent)(nil)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// attachDeps builds a Deps suitable for attach_disk tests.
func attachDeps(qemuSvc qemu.Service, ag agent.Agent) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         "pve1",
			VMDiskFormat: "qcow2",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}
}

// attachArgs builds a two-element arg slice for attach_disk.
func attachArgs(vmCID, diskCID string) []json.RawMessage {
	return marshalArgs(vmCID, diskCID)
}

// extractPath unmarshals result into a map and returns the "path" value.
func extractPath(t *testing.T, result any) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var hints map[string]string
	if e := json.Unmarshal(raw, &hints); e != nil {
		t.Fatalf("unmarshal disk_hints: %v", e)
	}
	return hints["path"]
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHandleAttachDisk_Happy verifies the full happy path:
//   - AttachDisk returns diskID "scsi2"
//   - Config returns config confirming scsi2 → volid
//   - disk_hints {"path": "/dev/sdc"} returned (scsi index 2 → 'a'+2 = 'c' → /dev/sdc)
//   - UpdateDiskHints called with correct vmid and device_path
func TestHandleAttachDisk_Happy(t *testing.T) {
	const (
		vmCID   = "100"
		diskCID = "local-lvm:vm-9001-disk-0"
		volid   = "vm-9001-disk-0"
	)

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi2",
		configCfg: map[string]interface{}{
			"scsi0": "local-lvm:vm-100-disk-0",
			"scsi1": "local-lvm:vm-100-disk-1",
			"scsi2": volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil; want disk_hints object")
	}

	path := extractPath(t, result)
	if path != "/dev/sdc" {
		t.Errorf("disk_hints.path: want /dev/sdc, got %q", path)
	}

	if !ag.updateCalled {
		t.Error("UpdateDiskHints not called")
	}
	if ag.updateVMID != 100 {
		t.Errorf("UpdateDiskHints vmid: want 100, got %d", ag.updateVMID)
	}
	if len(ag.updateHints) != 1 {
		t.Fatalf("UpdateDiskHints hints len: want 1, got %d", len(ag.updateHints))
	}
	if ag.updateHints[0].DevicePath != "/dev/sdc" {
		t.Errorf("UpdateDiskHints device_path: want /dev/sdc, got %q", ag.updateHints[0].DevicePath)
	}
	if ag.updateHints[0].DiskCID != diskCID {
		t.Errorf("UpdateDiskHints disk_cid: want %q, got %q", diskCID, ag.updateHints[0].DiskCID)
	}
}

// TestHandleAttachDisk_AlreadyAttached verifies idempotency: if AttachDisk returns
// the existing diskID, the handler returns valid disk_hints without error.
func TestHandleAttachDisk_AlreadyAttached(t *testing.T) {
	const (
		vmCID   = "100"
		diskCID = "local-lvm:vm-9001-disk-0"
		volid   = "vm-9001-disk-0"
	)

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]interface{}{
			"scsi0": "local-lvm:vm-100-disk-0",
			"scsi1": volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(vmCID, diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error on already-attached: %v", err)
	}

	// scsi1 → index 1 → 'a'+1 = 'b' → /dev/sdb
	path := extractPath(t, result)
	if path != "/dev/sdb" {
		t.Errorf("disk_hints.path: want /dev/sdb, got %q", path)
	}
}

// TestHandleAttachDisk_VMNotFound verifies that a 404 from AttachDisk maps to VMNotFound.
func TestHandleAttachDisk_VMNotFound(t *testing.T) {
	qemuSvc := &attachQEMUService{
		attachErr: &sdkerrors.APIError{Code: 404, Message: "not found"},
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs("999", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_AttachFail verifies a generic attach error propagates.
func TestHandleAttachDisk_AttachFail(t *testing.T) {
	qemuSvc := &attachQEMUService{
		attachErr: errors.New("PVE connection refused"),
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs("100", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestHandleAttachDisk_UpdateDiskHintsFail verifies that UpdateDiskHints failure
// propagates (not silently dropped).
func TestHandleAttachDisk_UpdateDiskHintsFail(t *testing.T) {
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi2",
		configCfg:          map[string]interface{}{"scsi2": volid},
	}
	ag := &captureAgent{updateErr: errors.New("registry unavailable")}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	_, err := h.Handle(context.Background(), attachArgs("100", "local-lvm:"+volid), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when UpdateDiskHints fails")
	}
}

// TestHandleAttachDisk_InvalidVMCID verifies non-integer vm_cid returns VMNotFound.
func TestHandleAttachDisk_InvalidVMCID(t *testing.T) {
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs("not-a-number", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_InvalidDiskCID verifies malformed disk_cid returns DiskNotFound.
func TestHandleAttachDisk_InvalidDiskCID(t *testing.T) {
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs("100", "nodisk"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_MissingArgs verifies argument count validation.
func TestHandleAttachDisk_MissingArgs(t *testing.T) {
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

// TestHandleAttachDisk_EmptyVMCID verifies empty vm_cid string is rejected.
func TestHandleAttachDisk_EmptyVMCID(t *testing.T) {
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs("", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

// TestHandleAttachDisk_EmptyDiskCID verifies empty disk_cid string is rejected.
func TestHandleAttachDisk_EmptyDiskCID(t *testing.T) {
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs("100", ""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

// TestHandleAttachDisk_ResolveFallback verifies that when ResolveDiskID fails after
// a successful AttachDisk, the handler falls back to the diskID returned by
// AttachDisk and still returns valid disk_hints (logs a warning).
func TestHandleAttachDisk_ResolveFallback(t *testing.T) {
	// Config returns error on the ResolveDiskID call, but AttachDisk succeeds.
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi3",
		configErr:          errors.New("transient config error"),
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", "local:vol"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error in fallback path: %v", err)
	}

	// scsi3 → index 3 → 'a'+3 = 'd' → /dev/sdd
	path := extractPath(t, result)
	if path != "/dev/sdd" {
		t.Errorf("disk_hints.path: want /dev/sdd (fallback from scsi3), got %q", path)
	}
}

// TestHandleAttachDisk_DiskHintsShape verifies the returned object has exactly
// the "path" key required by the BOSH CPI v2 spec.
func TestHandleAttachDisk_DiskHintsShape(t *testing.T) {
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi2",
		configCfg:          map[string]interface{}{"scsi2": volid},
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	result, err := h.Handle(context.Background(), attachArgs("100", "local:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, _ := json.Marshal(result)
	var m map[string]json.RawMessage
	if e := json.Unmarshal(raw, &m); e != nil {
		t.Fatalf("disk_hints not an object: %v", e)
	}
	if _, ok := m["path"]; !ok {
		t.Errorf("disk_hints missing required key 'path'; got keys: %v", keysOf(m))
	}
}

// keysOf returns the keys of a map for readable error messages.
func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
