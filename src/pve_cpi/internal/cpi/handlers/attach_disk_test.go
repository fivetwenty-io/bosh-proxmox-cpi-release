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

	// Config control. Two modes:
	//   - configCfgs (when non-nil): staged responses; call N returns
	//     configCfgs[N-1], or the last entry if call N exceeds the slice.
	//   - configCfg + configErr: legacy single-shape mode used by older
	//     tests. configErrAfter, when > 0, returns configCfg/nil for the
	//     first N calls and configErr starting with call N+1.
	configCfgs     []map[string]interface{}
	configCfg      map[string]interface{}
	configErr      error
	configErrAfter int
	configCalls    int

	// DetachDisk control — records calls and may inject error.
	detachCalls []string
	detachErr   error
}

func (m *attachQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	return m.attachReturnDiskID, m.attachErr
}

func (m *attachQEMUService) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	m.configCalls++
	if len(m.configCfgs) > 0 {
		idx := m.configCalls - 1
		if idx >= len(m.configCfgs) {
			idx = len(m.configCfgs) - 1
		}
		return m.configCfgs[idx], nil
	}
	if m.configErrAfter > 0 && m.configCalls <= m.configErrAfter {
		return m.configCfg, nil
	}
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
func (m *attachQEMUService) DetachDisk(_ context.Context, _ string, _ int, diskID string) error {
	m.detachCalls = append(m.detachCalls, diskID)
	return m.detachErr
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
//   - disk_hints {"path": "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"} returned
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

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
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
	if ag.updateHints[0].DevicePath != wantPath {
		t.Errorf("UpdateDiskHints device_path: want %q, got %q", wantPath, ag.updateHints[0].DevicePath)
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

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
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

// TestHandleAttachDisk_ResolveFallback verifies that when ResolveDiskID's
// Config call fails after a successful AttachDisk, the handler falls back to
// the diskID returned by AttachDisk and still returns valid disk_hints
// (logs a warning).
//
// The first Config call (slot selection) must succeed; the second (resolve)
// must fail. configErrAfter=1 implements that two-phase behavior.
func TestHandleAttachDisk_ResolveFallback(t *testing.T) {
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi3",
		configCfg:          map[string]interface{}{}, // empty config → slot-selection picks scsi1
		configErr:          errors.New("transient config error"),
		configErrAfter:     1,
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", "local:vol"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error in fallback path: %v", err)
	}

	// Resolve's Config call fails; the handler falls back to the diskID
	// returned by AttachDisk ("scsi3"). devicePathByID is a pure function
	// of the diskID so the final by-id path is still valid even though
	// the resolve step degraded.
	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi3"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
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

// TestHandleAttachDisk_FreshVMSkipsSCSI0 verifies that a fresh attach on a VM
// with no scsi disks lands at scsi1 (not scsi0). scsi0 is reserved because the
// BOSH agent's mappedDevicePathResolver resolves /dev/sda hints to the virtio
// root disk /dev/vda when one exists, shadowing the persistent disk.
func TestHandleAttachDisk_FreshVMSkipsSCSI0(t *testing.T) {
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		// First Config call (slot selection): config has virtio0 root, no scsi.
		// Second Config call (resolve): config has the new scsi1 attachment.
		configCfg: map[string]interface{}{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:" + volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", "data:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
	}
}

// TestHandleAttachDisk_LegacySCSI0Migration verifies the migration path:
// when a disk is already attached at scsi0 (from a prior CPI version that
// allowed it), attach_disk detaches scsi0 and reattaches at scsi1+.
func TestHandleAttachDisk_LegacySCSI0Migration(t *testing.T) {
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfgs: []map[string]interface{}{
			// Call 1 — slot selection: legacy scsi0 attachment present.
			{"virtio0": "data:vm-100-disk-0", "scsi0": diskCID},
			// Call 2 — re-read after Detach: scsi0 gone.
			{"virtio0": "data:vm-100-disk-0"},
			// Call 3 — Resolve after AttachDisk: scsi1 present with volid.
			{"virtio0": "data:vm-100-disk-0", "scsi1": diskCID},
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(qemuSvc.detachCalls) != 1 || qemuSvc.detachCalls[0] != "scsi0" {
		t.Errorf("expected DetachDisk(\"scsi0\") to be called exactly once, got %v", qemuSvc.detachCalls)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q (post-migration), got %q", wantPath, path)
	}
}

// TestHandleAttachDisk_PreservesExistingNonZeroSlot verifies that an existing
// attachment at scsi >= 1 is preserved (idempotent reattach with no migration).
func TestHandleAttachDisk_PreservesExistingNonZeroSlot(t *testing.T) {
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi3",
		configCfg: map[string]interface{}{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:other",
			"scsi3":   diskCID,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(qemuSvc.detachCalls) != 0 {
		t.Errorf("expected no DetachDisk calls when existing slot is non-zero; got %v", qemuSvc.detachCalls)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi3"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q (scsi3 preserved), got %q", wantPath, path)
	}
}

// TestHandleAttachDisk_PicksLowestFreeAtOrAboveOne verifies the slot allocator
// picks the lowest free index >= 1, skipping occupied slots.
func TestHandleAttachDisk_PicksLowestFreeAtOrAboveOne(t *testing.T) {
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi2",
		configCfg: map[string]interface{}{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:other-a",
			"scsi3":   "data:other-c",
			"scsi2":   "data:" + volid, // post-attach resolve view
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs("100", "data:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q (lowest free >= 1), got %q", wantPath, path)
	}
}
