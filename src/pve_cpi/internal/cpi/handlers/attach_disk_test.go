package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// attachQEMUService: QEMU mock for attach_disk tests.
// Wraps mockQEMUService from testmocks_test.go and adds AttachDisk control.
// ---------------------------------------------------------------------------

type attachQEMUService struct {
	// AttachDisk control.
	attachReturnDiskID string
	attachErr          error
	// attachLastVolid captures the volid argument passed to the most recent
	// AttachDisk call. Tests use this to assert that performance options are
	// baked into the volid string (not into AttachOpts.Extra).
	attachLastVolid string

	// Config control. Two modes:
	//   - configCfgs (when non-nil): staged responses; call N returns
	//     configCfgs[N-1], or the last entry if call N exceeds the slice.
	//   - configCfg + configErr: legacy single-shape mode used by older
	//     tests. configErrAfter, when > 0, returns configCfg/nil for the
	//     first N calls and configErr starting with call N+1.
	configCfgs     []map[string]any
	configCfg      map[string]any
	configErr      error
	configErrAfter int
	configCalls    int

	// DetachDisk control — records calls and may inject error.
	detachCalls []string
	detachErr   error

	// listSnapshotsFn controls ListSnapshots behavior for snapshot guard tests.
	// nil → return only the synthetic "current" entry (no real snapshots),
	// so existing tests are not affected by the guard.
	listSnapshotsFn func(ctx context.Context, node string, vmid int) ([]map[string]any, error)
}

func (m *attachQEMUService) AttachDisk(_ context.Context, _ string, _ int, volid string, _ string, _ *qemu.AttachOpts) (string, error) {
	m.attachLastVolid = volid
	return m.attachReturnDiskID, m.attachErr
}

func (m *attachQEMUService) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *attachQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("attachQEMUService.Create: not expected")
}
func (m *attachQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *attachQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *attachQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("attachQEMUService.Snapshot: not expected")
}
func (m *attachQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("attachQEMUService.DeleteSnapshot: not expected")
}
func (m *attachQEMUService) ListSnapshots(
	ctx context.Context, node string, vmid int,
) ([]map[string]any, error) {
	if m.listSnapshotsFn != nil {
		return m.listSnapshotsFn(ctx, node, vmid)
	}
	// Default: only the synthetic "current" entry — no real snapshots.
	// Existing tests do not set listSnapshotsFn; this keeps them unaffected by
	// the snapshot guard (guard sees len(names)==0 and proceeds normally).
	return []map[string]any{{"name": "current"}}, nil
}
func (m *attachQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("attachQEMUService.RollbackSnapshot: not expected")
}

var _ qemu.Service = (*attachQEMUService)(nil)

// ---------------------------------------------------------------------------
// captureAgent: a minimal agent.Agent stub for attach_disk tests.
// ---------------------------------------------------------------------------

type captureAgent struct{}

func (a *captureAgent) Configure(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
	return nil
}
func (a *captureAgent) Remove(_ context.Context, _ string, _ int) error { return nil }

var _ agent.Agent = (*captureAgent)(nil)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// attachDeps builds a Deps suitable for attach_disk tests.
func attachDeps(qemuSvc qemu.Service, ag agent.Agent) handlers.Deps {
	// The cluster listing places VM 100 on testNode so the handler's
	// authoritative VM lookup hits on the scan; an index miss would
	// otherwise trigger per-node config probes, consuming a scripted
	// Config response and shifting the call sequences these tests assert.
	targetRaw, _ := json.Marshal(map[string]any{
		"vmid": 100,
		"node": testNode,
		"type": "qemu",
	})
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{targetRaw}
			return &resp, nil
		},
	}
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: clusterSvc},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}
}

// attachDepsWithConfig builds a Deps with a caller-supplied config. Used by
// snapshot guard tests that need AllowDiskOpsWithSnapshots / RequireSnapshotCheckPass.
func attachDepsWithConfig(qemuSvc qemu.Service, ag agent.Agent, cfg *config.CPIConfig) handlers.Deps {
	return handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: attachClusterWithVM100()},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}
}

// attachClusterWithVM100 places VM 100 on testNode, for the same reason as
// the fixture inside attachDeps: the authoritative lookup must hit on the
// scan so no per-node config probe consumes a scripted Config response.
func attachClusterWithVM100() *mockClusterSvc {
	targetRaw, _ := json.Marshal(map[string]any{
		"vmid": 100,
		"node": testNode,
		"type": "qemu",
	})
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{targetRaw}
			return &resp, nil
		},
	}
}

// attachArgs builds a two-element arg slice for attach_disk. The handler
// hard-rejects unenveloped input, so a bare diskCID is wrapped in a pvd-
// envelope here — callers in this file pass either a bare PVE-volid-shaped
// string (including deliberately malformed ones used to exercise error
// paths) or an already-encoded CID built via mustEncodeDiskCID/perfDiskCID
// (e.g. when the test needs specific meta baked in). An already-encoded or
// empty diskCID is passed through unchanged so it is never double-wrapped
// and the handler's own empty-disk_cid rejection still hits that check
// directly.
func attachArgs(t *testing.T, vmCID, diskCID string) []json.RawMessage {
	t.Helper()
	if diskCID == "" || strings.HasPrefix(diskCID, "pvd-") || strings.HasPrefix(diskCID, "pvz-") {
		return marshalArgs(vmCID, diskCID)
	}
	return marshalArgs(vmCID, mustEncodeDiskCID(t, diskCID, nil))
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
func TestHandleAttachDisk_Happy(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "vm-9001-disk-0"
	)

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: diskSlot,
		configCfg: map[string]any{
			"scsi0":  testDiskCID,
			"scsi1":  "local-lvm:vm-100-disk-1",
			diskSlot: volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, vmCID, diskCID), jsonrpc.Context{})
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
}

// TestHandleAttachDisk_AlreadyAttached verifies idempotency: if AttachDisk returns
// the existing diskID, the handler returns valid disk_hints without error.
func TestHandleAttachDisk_AlreadyAttached(t *testing.T) {
	t.Parallel()
	const (
		vmCID = "100"
		volid = "vm-9001-disk-0"
	)

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi0": testDiskCID,
			"scsi1": volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, vmCID, diskCID), jsonrpc.Context{})
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
	t.Parallel()
	qemuSvc := &attachQEMUService{
		attachErr: &sdkerrors.APIError{Code: 404, Message: "not found"},
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs(t, "999", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_AttachFail verifies a generic attach error propagates.
func TestHandleAttachDisk_AttachFail(t *testing.T) {
	t.Parallel()
	qemuSvc := &attachQEMUService{
		attachErr: errors.New("PVE connection refused"),
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", "local-lvm:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestHandleAttachDisk_InvalidVMCID verifies non-integer vm_cid returns VMNotFound.
func TestHandleAttachDisk_InvalidVMCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs(t, "not-a-number", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Errorf("error type: want VMNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_InvalidDiskCID verifies malformed disk_cid returns DiskNotFound.
func TestHandleAttachDisk_InvalidDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", "nodisk"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
	}
}

// TestHandleAttachDisk_MissingArgs verifies argument count validation.
func TestHandleAttachDisk_MissingArgs(t *testing.T) {
	t.Parallel()
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing args")
	}
}

// TestHandleAttachDisk_EmptyVMCID verifies empty vm_cid string is rejected.
func TestHandleAttachDisk_EmptyVMCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs(t, "", "local:vol"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty vm_cid")
	}
}

// TestHandleAttachDisk_EmptyDiskCID verifies empty disk_cid string is rejected.
func TestHandleAttachDisk_EmptyDiskCID(t *testing.T) {
	t.Parallel()
	h := handlers.HandleAttachDisk(attachDeps(&attachQEMUService{}, &captureAgent{}))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", ""), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

// TestHandleAttachDisk_ResolveFallback verifies that when ResolveDiskID's
// Config call fails after a successful AttachDisk, the handler falls back to
// the diskID returned by AttachDisk and still returns valid disk_hints
// (logs a warning).
//
// The first two Config calls (holder scan on the listed VM, then slot
// selection) must succeed; the third (resolve) must fail. configErrAfter=2
// implements that behavior.
func TestHandleAttachDisk_ResolveFallback(t *testing.T) {
	t.Parallel()
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi3",
		configCfg:          map[string]any{},
		configErr:          errors.New("transient config error"),
		configErrAfter:     2,
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", "local:vol"), jsonrpc.Context{})
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
	t.Parallel()
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: diskSlot,
		configCfg:          map[string]any{diskSlot: volid},
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", "local:"+volid), jsonrpc.Context{})
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
	t.Parallel()
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		// First Config call (slot selection): config has virtio0 root, no scsi.
		// Second Config call (resolve): config has the new scsi1 attachment.
		configCfg: map[string]any{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:" + volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", "data:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
	}
}

// TestHandleAttachDisk_RootDiskBusSCSI_StillSkipsSCSI0 verifies that when the
// root disk itself occupies scsi0 (pve.root_disk_bus=scsi), a new persistent
// disk still lands at scsi1 — nextFreeSCSIIndexAtLeast always starts its scan
// at 1 regardless of what (if anything) occupies scsi0, so root_disk_bus=scsi
// introduces no persistent-disk slot collision.
func TestHandleAttachDisk_RootDiskBusSCSI_StillSkipsSCSI0(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		// scsi0 holds the ROOT disk's own (unrelated) volid — root_disk_bus=scsi.
		// First Config call (slot selection): scsi0 occupied, no scsi1 yet.
		// Second Config call (resolve): the new attachment landed at scsi1.
		configCfg: map[string]any{
			"scsi0": "data:vm-100-disk-0",
			"scsi1": "data:" + volid,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", "data:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qemuSvc.detachCalls) != 0 {
		t.Errorf("scsi0 holds the root disk's own volid, not the attaching disk's — "+
			"no legacy-migration detach should fire, got %v", qemuSvc.detachCalls)
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
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfgs: []map[string]any{
			// Call 1, holder scan: VM 100 is listed by its node, and the
			// disk-holder identity scan reads its config to match the disk.
			{"virtio0": "data:vm-100-disk-0", "scsi0": diskCID},
			// Call 2, overlay read: the holder's recorded per-disk option
			// overrides (none here) are read from the same config.
			{"virtio0": "data:vm-100-disk-0", "scsi0": diskCID},
			// Call 3, slot selection: legacy scsi0 attachment present.
			{"virtio0": "data:vm-100-disk-0", "scsi0": diskCID},
			// Call 4, re-read after Detach: scsi0 gone.
			{"virtio0": "data:vm-100-disk-0"},
			// Call 5 and later, Resolve after AttachDisk: scsi1 present with volid.
			{"virtio0": "data:vm-100-disk-0", "scsi1": diskCID},
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
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
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi3",
		configCfg: map[string]any{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:other",
			"scsi3":   diskCID,
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
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
	t.Parallel()
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: diskSlot,
		configCfg: map[string]any{
			"virtio0": "data:vm-100-disk-0",
			"scsi1":   "data:other-a",
			"scsi3":   "data:other-c",
			diskSlot:  "data:" + volid, // post-attach resolve view
		},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", "data:"+volid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q (lowest free >= 1), got %q", wantPath, path)
	}
}

// ---------------------------------------------------------------------------
// Snapshot guard tests
//
// Each test uses a fresh attachQEMUService that wires listSnapshotsFn to
// control the guard outcome, and verifies:
//   a) whether AttachDisk was called (attachCalled tracks this via attachErr sentinel)
//   b) the error type / message content
//
// The "attachCalled" pattern: if attachErr is set to errAttachSentinel, the
// test can detect a call vs. no-call by checking whether the error returned by
// the handler contains "sentinel". When no error is expected the attach
// succeeds normally via attachReturnDiskID.
// ---------------------------------------------------------------------------

// snapshots builds a ListSnapshots response with the given real snapshot names
// plus the mandatory synthetic "current" entry.
func snapshots(names ...string) func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	return func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
		out := make([]map[string]any, 0, 1+len(names))
		out = append(out, map[string]any{"name": "current"})
		for _, n := range names {
			out = append(out, map[string]any{"name": n})
		}
		return out, nil
	}
}

// snapshotErr returns a listSnapshotsFn that fails with the given error.
func snapshotErr(err error) func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	return func(_ context.Context, _ string, _ int) ([]map[string]any, error) {
		return nil, err
	}
}

// guardCfg returns a CPIConfig with the given guard flags and the minimal
// fields the handler's local-backend path requires. Node is set to match
// the mock's resolved node ("pve1" matches attachDeps default; disk_cid
// storage "local-lvm" is shared; we use "data" storage which maps to shared
// backend so no vmNode lookup is triggered).
func guardCfg(allowDiskOps, requireCheckPass bool) *config.CPIConfig {
	return &config.CPIConfig{
		Node:                      testNode,
		VMDiskFormat:              "qcow2",
		AllowDiskOpsWithSnapshots: allowDiskOps,
		RequireSnapshotCheckPass:  requireCheckPass,
	}
}

// snapQEMUSvc builds an attachQEMUService with listSnapshotsFn set and a
// working attach+config path. diskCID must match the "storage:volid" form
// used in the test.
func snapQEMUSvc(
	listFn func(context.Context, string, int) ([]map[string]any, error),
) *attachQEMUService {
	const volid = "vm-9001-disk-0"
	return &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": "data:" + volid,
		},
		listSnapshotsFn: listFn,
	}
}

// TestHandleAttachDisk_GuardBlocksWhenSnapshotsPresent verifies that the
// handler returns a Cloud error and does NOT call AttachDisk when the VM has
// real snapshots and AllowDiskOpsWithSnapshots is false.
func TestHandleAttachDisk_GuardBlocksWhenSnapshotsPresent(t *testing.T) {
	t.Parallel()
	const diskCID = "data:vm-9001-disk-0"
	snapName1, snapName2 := "bosh-snap-a", "bosh-snap-b"

	qemuSvc := snapQEMUSvc(snapshots(snapName1, snapName2))
	// Sentinel: if AttachDisk is called unexpectedly it will return this error,
	// and the test will catch it via the error message.
	qemuSvc.attachErr = errors.New("AttachDisk must not be called")

	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDepsWithConfig(qemuSvc, ag, guardCfg(false, false)))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when snapshots present; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud, got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, snapName1) || !strings.Contains(msg, snapName2) {
		t.Errorf("error message must contain snapshot names %q and %q; got: %s", snapName1, snapName2, msg)
	}
	if !strings.Contains(msg, "allow_disk_ops_with_snapshots") {
		t.Errorf("error message must contain remediation hint 'allow_disk_ops_with_snapshots'; got: %s", msg)
	}
}

// TestHandleAttachDisk_GuardProceedsWhenNoSnapshots verifies the happy path
// when the VM has no real snapshots: AttachDisk is called and disk_hints returned.
func TestHandleAttachDisk_GuardProceedsWhenNoSnapshots(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := snapQEMUSvc(snapshots( /* none */ ))
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDepsWithConfig(qemuSvc, ag, guardCfg(false, false)))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error when no snapshots: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result; got nil")
	}
}

// TestHandleAttachDisk_GuardCheckErrorFailOpen verifies that when ListSnapshots
// returns an error and RequireSnapshotCheckPass is false, the handler WARNs and
// proceeds: AttachDisk is called and disk_hints are returned.
func TestHandleAttachDisk_GuardCheckErrorFailOpen(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	listErr := errors.New("PVE transient: connection reset")
	qemuSvc := snapQEMUSvc(snapshotErr(listErr))
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDepsWithConfig(qemuSvc, ag, guardCfg(false, false)))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected fail-open (proceed with warn) when RequireSnapshotCheckPass=false; got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result on fail-open; got nil")
	}
}

// TestHandleAttachDisk_GuardCheckErrorFailClosed verifies that when ListSnapshots
// returns an error and RequireSnapshotCheckPass is true, the handler returns an
// error and does NOT call AttachDisk.
func TestHandleAttachDisk_GuardCheckErrorFailClosed(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	listErr := errors.New("PVE transient: connection reset")
	qemuSvc := snapQEMUSvc(snapshotErr(listErr))
	qemuSvc.attachErr = errors.New("AttachDisk must not be called")

	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDepsWithConfig(qemuSvc, ag, guardCfg(false, true)))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when RequireSnapshotCheckPass=true and check fails; got nil")
	}
	if !strings.Contains(err.Error(), "require_snapshot_check_pass") {
		t.Errorf("error must mention require_snapshot_check_pass; got: %v", err)
	}
}

// TestHandleAttachDisk_GuardAllowOverrideProceeds verifies that when snapshots
// are present but AllowDiskOpsWithSnapshots is true, the handler WARNs and
// proceeds: AttachDisk is called and disk_hints are returned.
func TestHandleAttachDisk_GuardAllowOverrideProceeds(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	const diskCID = "data:" + volid

	qemuSvc := snapQEMUSvc(snapshots("snap-override-test"))
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDepsWithConfig(qemuSvc, ag, guardCfg(true, false)))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected proceed when AllowDiskOpsWithSnapshots=true; got error: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result with allow override; got nil")
	}
}

// ---------------------------------------------------------------------------
// Local-backend co-location test types (file-local, prefixed attachDisk).
//
// The production localBackend (in the pve package) locates existing volumes via
// Storage().Exists cluster scan. For handler-level unit tests, we inject a fake
// resolver/backend that returns a configurable node directly, avoiding all PVE
// API calls except Cluster().ListResources which FindVMNodeViaCluster needs.
// ---------------------------------------------------------------------------

// attachDiskLocalBackend is a pve.Backend that reports BackendLocal kind and
// returns a fixed node for NodeForExisting. Used to trigger the co-location
// check in attach_disk without wiring the production storage cluster scan.
type attachDiskLocalBackend struct {
	diskNode string // node that "holds" the disk
}

func (b *attachDiskLocalBackend) Kind() pve.BackendKind { return pve.BackendLocal }

func (b *attachDiskLocalBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return b.diskNode, nil
}

func (b *attachDiskLocalBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return b.diskNode, nil
}

// attachDiskLocalResolver is a pve.BackendResolver that always returns an
// attachDiskLocalBackend bound to diskNode.
type attachDiskLocalResolver struct {
	diskNode string
}

func (r *attachDiskLocalResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return &attachDiskLocalBackend{diskNode: r.diskNode}, nil
}

// attachDiskClusterSvc implements cluster.Service. Only ListResources is
// overridden; all other methods panic on accidental call. The listFn field
// controls what ListResources returns so tests can place a VM on a specific node.
type attachDiskClusterSvc struct {
	sdkcluster.Service // nil — non-overridden methods panic

	listFn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (c *attachDiskClusterSvc) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if c.listFn != nil {
		return c.listFn(ctx, params)
	}
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// attachDiskVMOnNode builds a ListResourcesResponse that places vmid on node.
// FindVMNodeViaCluster parses {"vmid": N, "node": "name"} rows.
func attachDiskVMOnNode(vmid int, node string) *sdkcluster.ListResourcesResponse {
	raw, _ := json.Marshal(map[string]any{
		"vmid": vmid,
		"node": node,
		"type": "qemu",
	})
	resp := sdkcluster.ListResourcesResponse{raw}
	return &resp
}

// ---------------------------------------------------------------------------
// Co-location enforcement test
//
// CID-variant tests below (LVM_CID, ZFSPool_CID, Dir_CID, LVMThin_CID) use a
// static (shared) backend and exercise ParseDiskCID/identity through the handler.
// The co-location test is the real branch that exercises BackendLocal logic.
// ---------------------------------------------------------------------------

// TestHandleAttachDisk_LocalBackend_CoLocationEnforced verifies that when the
// storage backend is local, the disk lives on pve-node2, and the VM lives on
// pve-node1, attach_disk returns an error rather than issuing a cross-node
// config PUT that PVE would reject with an opaque storage error.
func TestHandleAttachDisk_LocalBackend_CoLocationEnforced(t *testing.T) {
	t.Parallel()
	const (
		vmCID    = "100"
		diskNode = "pve-node2" // where the local-storage disk lives
	)

	// attachQEMUService: slot selection needs Config (returns empty — picks scsi1).
	// AttachDisk must NOT be called; set attachErr to a sentinel so an accidental
	// call produces a recognizable test failure rather than a silent success.
	qemuSvc := &attachQEMUService{
		attachErr: errors.New("AttachDisk must not be called on co-location violation"),
		configCfg: map[string]any{},
	}

	// Cluster returns VM 100 on vmNode so FindVMNodeViaCluster resolves it.
	clusterSvc := &attachDiskClusterSvc{
		listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return attachDiskVMOnNode(100, vmNode), nil
		},
	}

	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
		},
		Agent:    &captureAgent{},
		Logger:   log.NewNopLogger(),
		Resolver: &attachDiskLocalResolver{diskNode: diskNode},
	}

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, vmCID, diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected co-location error; got nil")
	}
	// Error must be a Cloud-type error (not VMNotFound/DiskNotFound).
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud, got %T: %v", err, err)
	}
	msg := err.Error()
	// Message must name both nodes so the operator can diagnose the mismatch.
	if !strings.Contains(msg, diskNode) {
		t.Errorf("error must mention disk node %q; got: %s", diskNode, msg)
	}
	if !strings.Contains(msg, vmNode) {
		t.Errorf("error must mention VM node %q; got: %s", vmNode, msg)
	}
}

// ---------------------------------------------------------------------------
// CID-variant success tests (static/shared backend).
//
// These tests exercise ParseDiskCID with different volid formats and verify that
// the handler proceeds through AttachDisk for each active local storage type.
// Storage-type branching does not exist in attach_disk — the CID is passed
// opaquely to AttachDisk; these tests confirm CID parsing doesn't break for
// any of the four active local formats.
// ---------------------------------------------------------------------------

// TestHandleAttachDisk_LVM_CID verifies that a standard LVM CID
// ("local-lvm:vm-9001-disk-0") is parsed correctly and attach proceeds.
func TestHandleAttachDisk_LVM_CID(t *testing.T) {
	t.Parallel()
	const volid = "vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": diskCID},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVM CID: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("LVM CID: expected disk_hints; got nil")
	}
	_ = volid // CID exercises ParseDiskCID; volid is the opaque volume segment
}

// TestHandleAttachDisk_ZFSPool_CID verifies that a ZFS pool bare-volname CID
// ("local-zfs:vm-9001-disk-0") is parsed correctly and attach proceeds.
func TestHandleAttachDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local-zfs:vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": diskCID},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("ZFSPool CID: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("ZFSPool CID: expected disk_hints; got nil")
	}
}

// TestHandleAttachDisk_Dir_CID verifies that a dir-type subpath CID
// ("local:9001/vm-9001-disk-0.raw") is parsed correctly and attach proceeds.
// ParseDiskCID splits on the first colon; the volume segment ("9001/vm-9001-disk-0.raw")
// is treated as opaque by the handler.
func TestHandleAttachDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local:9001/vm-9001-disk-0.raw"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": diskCID},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("Dir CID: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("Dir CID: expected disk_hints; got nil")
	}
}

// TestHandleAttachDisk_LVMThin_CID verifies that an LVMThin bare-volname CID
// ("local-lvm-thin:vm-9001-disk-0") is parsed correctly and attach proceeds.
func TestHandleAttachDisk_LVMThin_CID(t *testing.T) {
	t.Parallel()
	const diskCID = "local-lvm-thin:vm-9001-disk-0"
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": diskCID},
	}
	ag := &captureAgent{}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, ag))

	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("LVMThin CID: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("LVMThin CID: expected disk_hints; got nil")
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleAttachDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleAttachDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleAttachDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleAttachDisk_CIFS_CID(t *testing.T) { ... }

// ---------------------------------------------------------------------------
// Auth-failure test
// ---------------------------------------------------------------------------

// TestHandleAttachDisk_AuthFailure verifies that a 401 Unauthorized from
// AttachDisk is classified as a non-retriable Cloud error. Auth failures
// (wrong API token, expired ticket) are operator configuration issues; BOSH
// must surface them immediately rather than retrying indefinitely.
func TestHandleAttachDisk_AuthFailure(t *testing.T) {
	t.Parallel()
	authErr := &sdkerrors.APIError{HTTPCode: 401, Message: "authentication failure"}

	qemuSvc := &attachQEMUService{
		attachErr: authErr,
	}
	h := handlers.HandleAttachDisk(attachDeps(qemuSvc, &captureAgent{}))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", "local-lvm:vm-9001-disk-0"), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from 401 auth failure")
	}

	// 401 is a 4xx non-404 → WrapError returns a non-retriable Cloud error.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("auth failure must not be retriable; OkToRetry()=true; type=%s", cpiErr.Type())
	}
	if cpiErr.Type() == cpierrors.TypeRetriableCloud {
		t.Errorf("auth failure classified as RetriableCloud; want non-retriable TypeCloud; type=%s", cpiErr.Type())
	}
}

// ---------------------------------------------------------------------------
// Concurrent attach_disk test
// ---------------------------------------------------------------------------

// TestAttachDisk_ConcurrentSameVM spawns two goroutines each calling
// HandleAttachDisk against the same VMID with the same disk CID. The test
// asserts the invariant required by the BOSH CPI spec for attach_disk:
//
//   - Both calls succeed (idempotent: second call sees disk already attached).
//   - OR one call fails with a storage-lock retriable error (transient contention).
//
// The test does NOT assert that BOTH succeed simultaneously; PVE serialises
// QEMU config updates behind a per-VM lock, so one call may lose the race.
// What must NOT happen: a non-retriable Cloud error (which would cause BOSH
// to treat the attach as permanently failed and orphan the disk).
//
// Each goroutine gets its own independent mock service to avoid data races on
// the configCalls counter inside attachQEMUService; the VMID and disk CID are
// shared via immutable constants.
func TestAttachDisk_ConcurrentSameVM(t *testing.T) {
	t.Parallel()

	const vmCID = "100"

	// newQEMU returns a fresh, unshared attachQEMUService for each goroutine.
	// Sharing a single instance would cause a data race on configCalls.
	newQEMU := func() *attachQEMUService {
		return &attachQEMUService{
			attachReturnDiskID: "scsi1",
			configCfg: map[string]any{
				"scsi1": diskCID,
			},
		}
	}

	type callResult struct {
		err error
	}

	// workers = 2: minimum concurrent load to prove the per-goroutine mock
	// isolation prevents data races. Higher values would increase test runtime
	// without exercising additional code paths; the race detector catches
	// shared-state bugs regardless of goroutine count.
	const workers = 2
	results := make(chan callResult, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			// Independent QEMU service + captureAgent per goroutine — no shared mutable state.
			ag := &captureAgent{}
			h := handlers.HandleAttachDisk(attachDeps(newQEMU(), ag))
			_, err := h.Handle(context.Background(), attachArgs(t, vmCID, diskCID), jsonrpc.Context{})
			results <- callResult{err: err}
		}()
	}

	wg.Wait()
	close(results)

	var nonRetriableFailures []error
	for r := range results {
		if r.err == nil {
			continue // success: idempotent attach
		}
		var cpiErr *cpierrors.Error
		if !errors.As(r.err, &cpiErr) {
			// Non-CPI error — unexpected; surface it.
			nonRetriableFailures = append(nonRetriableFailures, r.err)
			continue
		}
		if !cpiErr.OkToRetry() {
			// Non-retriable error from concurrent attach is not acceptable.
			nonRetriableFailures = append(nonRetriableFailures, r.err)
		}
		// Retriable error (storage lock contention) is acceptable per BOSH spec.
	}

	if len(nonRetriableFailures) > 0 {
		t.Errorf("concurrent attach_disk produced %d non-retriable failure(s): %v",
			len(nonRetriableFailures), nonRetriableFailures)
	}
}

// ---------------------------------------------------------------------------
// Per-disk performance option tests.
//
// These tests verify that attach_disk bakes performance options into the volid
// argument passed to AttachDisk. The volid string format is:
//
//	"<bareDiskCID>[,key=value,...]"   (sorted alphabetically after "size")
//
// Options come from two sources, merged with per-disk CID metadata winning:
//   - Global config (deps.Config.DiskPerformance)
//   - Per-disk metadata encoded in the CID suffix (meta.Opts)
//
// All assertions use attachLastVolid captured by the mock's AttachDisk method.
// ---------------------------------------------------------------------------

// perfDiskCID encodes a bare disk CID with the given option map in its
// metadata suffix. Mirrors what create_disk produces at CID-encode time.
func perfDiskCID(t *testing.T, bare string, opts map[string]string) string {
	t.Helper()
	got, err := pve.EncodeDiskCID(bare, &pve.DiskCIDMeta{Opts: opts})
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bare, err)
	}
	return got
}

// attachDepsPerf builds Deps with a caller-supplied CPIConfig for perf tests.
func attachDepsPerf(qemuSvc qemu.Service, ag agent.Agent, cfg *config.CPIConfig) handlers.Deps {
	return handlers.Deps{
		Config: cfg,
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: attachClusterWithVM100()},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}
}

// TestHandleAttachDisk_PerfOpts_MetaOptsApplied verifies that when the disk CID
// encodes meta.Opts {iothread:1, cache:writeback}, AttachDisk receives a volid
// with those options baked in ("bareDiskCID,cache=writeback,iothread=1" in
// alphabetical order per buildDiskOptStr), at a scsi slot >= 1.
func TestHandleAttachDisk_PerfOpts_MetaOptsApplied(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	metaOpts := map[string]string{"iothread": "1", "cache": "writeback"}
	encodedCID := perfDiskCID(t, bareCID, metaOpts)

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	cfg := &config.CPIConfig{
		Node:         testNode,
		VMDiskFormat: "qcow2",
	}
	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// buildDiskOptStr: "size" first, then remaining keys alphabetically.
	// meta opts: cache=writeback, iothread=1 → "local-lvm:vm-9001-disk-0,cache=writeback,iothread=1"
	wantVolid := bareCID + ",cache=writeback,iothread=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_PerfOpts_BareNoOptions verifies byte-identical behavior:
// when the CID has no meta opts and config has nil DiskPerformance, AttachDisk
// receives the bareDiskCID with no option suffix.
// TestHandleAttachDisk_PerfOpts_NoConfigCurrentDefaultApplied verifies that
// with a bare no-opts CID and no global DiskPerformance block, the volid
// passed to AttachDisk still picks up the Phase 2 iothread=1 default —
// replacing the pre-Phase-2 "byte-identical to bareCID" assertion.
func TestHandleAttachDisk_PerfOpts_NoConfigCurrentDefaultApplied(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	cfg := &config.CPIConfig{
		Node:            testNode,
		VMDiskFormat:    "qcow2",
		DiskPerformance: nil, // no global block at all — the built-in default still applies
	}
	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantVolid := bareCID + ",iothread=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q (Phase 2 default), got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_PerfOpts_ExplicitOptOut_BareNoOptions verifies that an
// explicit global Iothread=false restores the pre-Phase-2 byte-identical
// bare-CID attach shape.
func TestHandleAttachDisk_PerfOpts_ExplicitOptOut_BareNoOptions(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	cfg := &config.CPIConfig{
		Node:            testNode,
		VMDiskFormat:    "qcow2",
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if qemuSvc.attachLastVolid != bareCID {
		t.Errorf("AttachDisk volid: want bare %q with explicit opt-out, got %q", bareCID, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_PerfOpts_GlobalDefaultApplied verifies that when the CID
// has no meta opts but deps.Config.DiskPerformance.Iothread is true, the volid
// passed to AttachDisk includes "iothread=1".
func TestHandleAttachDisk_PerfOpts_GlobalDefaultApplied(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	ioTrue := true
	cfg := &config.CPIConfig{
		Node:         testNode,
		VMDiskFormat: "qcow2",
		DiskPerformance: &config.DiskPerformanceDefaults{
			Iothread: &ioTrue,
		},
	}
	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantVolid := bareCID + ",iothread=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q (global iothread=1), got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_PerfOpts_PerDiskWinsOverGlobal verifies that per-disk
// meta opts override global config defaults: config sets cache=none but the
// disk CID encodes cache=writeback — the volid must carry cache=writeback.
func TestHandleAttachDisk_PerfOpts_PerDiskWinsOverGlobal(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	// Per-disk meta overrides cache with writeback.
	encodedCID := perfDiskCID(t, bareCID, map[string]string{"cache": "writeback"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	cfg := &config.CPIConfig{
		Node:         testNode,
		VMDiskFormat: "qcow2",
		DiskPerformance: &config.DiskPerformanceDefaults{
			Cache: "none", // global default — must be overridden by meta
			// Explicit opt-out isolates this test's focus (cache precedence)
			// from the Phase 2 iothread default, which would otherwise be
			// absent from the disk's recorded (pre-flip-style) meta and trip
			// the disk-perf-invariant guard as an unrelated divergence.
			Iothread: boolPtr(false),
		},
	}
	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))

	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantVolid := bareCID + ",cache=writeback"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q (per-disk cache overrides global), got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// invariantDivergenceSetup builds the canonical §7.26 divergence scenario: the
// disk CID records cache=writeback (and nothing else), but global config
// introduces iothread=true. The merge pins cache=writeback (per-disk wins) yet
// adds iothread=1, which the disk did not have at creation — a structural
// invariant divergence. mode is the disk_perf_invariant_mode config value.
func invariantDivergenceSetup(t *testing.T, mode string) (*attachQEMUService, *config.CPIConfig, string) {
	t.Helper()
	const bareCID = "local-lvm:vm-9100-disk-0"
	encodedCID := perfDiskCID(t, bareCID, map[string]string{"cache": "writeback"})
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	ioTrue := true
	cfg := &config.CPIConfig{
		Node:                  testNode,
		VMDiskFormat:          "qcow2",
		DiskPerfInvariantMode: mode,
		DiskPerformance:       &config.DiskPerformanceDefaults{Iothread: &ioTrue},
		// Invariant checks are independent of the disk's detached lifecycle;
		// opt out of the parked default so no parker scan runs here.
		DetachedDiskStrategy: "free",
	}
	return qemuSvc, cfg, encodedCID
}

// TestHandleAttachDisk_Invariant_EnforceRejects verifies that in the default
// (enforce) mode a structural invariant divergence is rejected with a
// non-retriable CloudError BEFORE any AttachDisk call (no orphan).
func TestHandleAttachDisk_Invariant_EnforceRejects(t *testing.T) {
	t.Parallel()
	qemuSvc, cfg, encodedCID := invariantDivergenceSetup(t, "") // empty → enforce

	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected enforce-mode invariant divergence to error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud (non-retriable), got %v", err)
	}
	if !strings.Contains(err.Error(), "iothread") {
		t.Errorf("error should name the diverging option iothread, got: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must NOT be called on enforce reject; got volid %q", qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_Invariant_WarnProceeds verifies that warn mode logs and
// proceeds, applying the merged options (cache=writeback,iothread=1).
func TestHandleAttachDisk_Invariant_WarnProceeds(t *testing.T) {
	t.Parallel()
	qemuSvc, cfg, encodedCID := invariantDivergenceSetup(t, "warn")

	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("warn mode must not error, got: %v", err)
	}

	wantVolid := "local-lvm:vm-9100-disk-0,cache=writeback,iothread=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_Invariant_OffSkips verifies that off mode skips the check
// entirely and proceeds with the merged options.
func TestHandleAttachDisk_Invariant_OffSkips(t *testing.T) {
	t.Parallel()
	qemuSvc, cfg, encodedCID := invariantDivergenceSetup(t, "off")

	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("off mode must not error, got: %v", err)
	}

	wantVolid := "local-lvm:vm-9100-disk-0,cache=writeback,iothread=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_Invariant_EnforceNoDivergence verifies that enforce mode
// does NOT reject when the effective options match the creation-time CID — the
// per-disk-wins merge keeps cache=writeback and global cache=none is overridden,
// so there is no divergence. Regression guard for the common case.
func TestHandleAttachDisk_Invariant_EnforceNoDivergence(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	encodedCID := perfDiskCID(t, bareCID, map[string]string{"cache": "writeback"})
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:                  testNode,
		VMDiskFormat:          "qcow2",
		DiskPerfInvariantMode: "enforce",
		DiskPerformance: &config.DiskPerformanceDefaults{
			Cache: "writethrough", // overridden by meta
			// Explicit opt-out keeps this a true no-divergence regression
			// guard: the disk's recorded meta has no iothread key, so the
			// Phase 2 default (true) would otherwise introduce a real
			// divergence unrelated to the cache-precedence case under test.
			Iothread: boolPtr(false),
		},
	}

	h := handlers.HandleAttachDisk(attachDepsPerf(qemuSvc, &captureAgent{}, cfg))
	_, err := h.Handle(context.Background(), attachArgs(t, "100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("enforce mode must not error when effective matches creation, got: %v", err)
	}
	wantVolid := bareCID + ",cache=writeback"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// ---------------------------------------------------------------------------
// Parker (parked detached-disk strategy) tests.
//
// UnparkDisk runs between attachDiskResolveNode and the snapshot guard. The
// tests use a configurable cluster scan to control whether the disk appears
// parked on a parker VM.
//
// Test matrix:
//   - strategy=parked, disk parked          → UnparkDisk called, normal attach proceeds
//   - strategy=parked, disk free-floating   → UnparkDisk no-op (fast nil), attach proceeds
//   - strategy=free                         → holder scan still runs, no unpark mutations
//   - strategy=parked, unpark fails         → retriable error, AttachDisk not called
// ---------------------------------------------------------------------------

const (
	parkerVMID  = 90000
	parkerNode  = testNode // parker lives on the same node in these tests
	parkerSlot  = "scsi0"
	parkedVolid = "local-lvm:vm-9001-disk-0"
)

// parkerClusterSvc returns a mockClusterSvc whose ListResources reports
// parkerVMID on parkerNode (for FindVMByDiskVolidOrNone) when the holderVMID
// matches parkerVMID. Used to simulate a parked disk.
//
// FindVMByDiskVolid scans all VMs, fetches each VM's config looking for the
// volid. The cluster scan lists VMs by VMID. We return one entry: the parker
// VMID. The QEMU.Config call for that VMID must then return a config with the
// disk and the bosh-parker tag.
func parkerClusterSvc() *mockClusterSvc {
	raw, _ := json.Marshal(map[string]any{
		"vmid": parkerVMID,
		"node": parkerNode,
		"type": "qemu",
	})
	// The target VM (100) is listed too: the handler's authoritative VM
	// lookup would otherwise treat the index miss as unproven and probe
	// per-node configs, consuming a scripted Config response and shifting
	// the call sequences these tests assert.
	targetRaw, _ := json.Marshal(map[string]any{
		"vmid": 100,
		"node": testNode,
		"type": "qemu",
	})
	resp := sdkcluster.ListResourcesResponse{raw, targetRaw}
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return &resp, nil
		},
	}
}

// emptyClusterSvc returns a mockClusterSvc that lists no VMs — simulates a
// free-floating disk (no holder found in cluster scan).
func emptyClusterSvc() *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			empty := sdkcluster.ListResourcesResponse{}
			return &empty, nil
		},
	}
}

// parkedCfg returns a CPIConfig with detached_disk_strategy=parked and the
// default parker VMID range applied (90000–90999).
func parkedCfg() *config.CPIConfig {
	return &config.CPIConfig{
		Node:                     testNode,
		VMDiskFormat:             "qcow2",
		DetachedDiskStrategy:     "parked",
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
		// Explicit opt-out keeps this test group's focus on parker mechanics
		// (detach-then-attach flow) rather than the Phase 2 iothread default,
		// which several of these tests would otherwise pick up as an
		// unrelated volid suffix on every bare-CID assertion.
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
}

// TestHandleAttachDisk_Parked_UnparksBeforeAttach verifies the primary parker
// path: disk is parked on a parker VM → UnparkDisk detaches it from the parker
// → normal AttachDisk call follows with disk_hints returned.
//
// Config call sequence driven by configCfgs:
//
//	Call 1: FindVMByDiskVolid, the identity-scan config read for the parker VMID
//	        (disk present → match)
//	Call 2: resolveDiskHolder, the in-band holder read for tags + slot
//	Call 3: option-override read; the parker's provenance entry is where a
//	        parked disk's recorded overrides live, read before the unpark can
//	        remove it
//	Call 4: unpark, which re-resolves the slot under the protection lock; a
//	        slot name resolved before the lock is a blind write by the time the
//	        detach runs
//	Call 5: removeParkerProvenance, which re-reads the parker description under the
//	        same lock
//	Call 6: chooseSCSISlotSkippingZero, the target VM config (empty, so pick scsi1)
//	Call 7+: ResolveDiskID and the CID stamp on the target VM after attach
func TestHandleAttachDisk_Parked_UnparksBeforeAttach(t *testing.T) {
	t.Parallel()

	const bareCID = parkedVolid // "local-lvm:vm-9001-disk-0"

	// Parker VM config: holds the disk at parkerSlot with bosh-parker tag.
	parkerVMCfg := map[string]any{
		parkerSlot: bareCID,
		"tags":     "bosh-parker",
		"name":     "bosh-parker-90000",
	}
	// Target VM config (slot selection): empty VM, pick scsi1.
	targetVMCfgEmpty := map[string]any{}
	// Target VM config (resolve): scsi1 holds the disk after attach.
	targetVMCfgAttached := map[string]any{
		"scsi1": bareCID,
	}

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfgs: []map[string]any{
			parkerVMCfg,         // call 1: FindVMByDiskVolid config scan on parker VMID
			parkerVMCfg,         // call 2: resolveDiskHolder in-band tags + slot read
			parkerVMCfg,         // call 3: option-override read on the parker
			parkerVMCfg,         // call 4: unpark re-resolves the slot under the lock
			parkerVMCfg,         // call 5: removeParkerProvenance description read
			targetVMCfgEmpty,    // call 6: chooseSCSISlotSkippingZero on target VM
			targetVMCfgAttached, // call 7+: ResolveDiskID / CID stamp after attach
		},
	}
	ag := &captureAgent{}
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: parkerClusterSvc(),
		},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	result, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("parked disk attach: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("parked disk attach: expected disk_hints result; got nil")
	}

	// UnparkDisk calls DetachDisk on the parker VM at parkerSlot.
	if len(qemuSvc.detachCalls) != 1 || qemuSvc.detachCalls[0] != parkerSlot {
		t.Errorf("expected DetachDisk(%q) from unpark; got %v", parkerSlot, qemuSvc.detachCalls)
	}
	// Normal attach must still proceed.
	if qemuSvc.attachLastVolid != bareCID {
		t.Errorf("AttachDisk volid: want %q, got %q", bareCID, qemuSvc.attachLastVolid)
	}
	const wantPath = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	path := extractPath(t, result)
	if path != wantPath {
		t.Errorf("disk_hints.path: want %q, got %q", wantPath, path)
	}
}

// TestHandleAttachDisk_Parked_FreeFloatingProceedsNormally verifies that when
// strategy=parked but the disk is free-floating (no holder in cluster scan),
// UnparkDisk returns nil immediately (no DetachDisk calls) and the normal
// attach path proceeds.
func TestHandleAttachDisk_Parked_FreeFloatingProceedsNormally(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	ag := &captureAgent{}
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: emptyClusterSvc(), // no holder → free-floating
		},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	result, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("free-floating under parked strategy: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("free-floating under parked strategy: expected disk_hints; got nil")
	}

	// No unpark operation — disk was not parked.
	if len(qemuSvc.detachCalls) != 0 {
		t.Errorf("expected no DetachDisk calls for free-floating disk; got %v", qemuSvc.detachCalls)
	}
}

// TestHandleAttachDisk_Parked_UnparkFailRetriable verifies that when UnparkDisk
// returns an error (e.g. PVE transient failure), attach_disk returns a retriable
// error and AttachDisk is not called — leaving the disk safely parked for retry.
func TestHandleAttachDisk_Parked_UnparkFailRetriable(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	// Parker VM config returned for the IsDiskParked check; DetachDisk will fail.
	parkerVMCfg := map[string]any{
		parkerSlot: bareCID,
		"tags":     "bosh-parker",
	}

	qemuSvc := &attachQEMUService{
		attachErr: errors.New("AttachDisk must not be called on unpark failure"),
		configCfg: parkerVMCfg,
		// DetachDisk returns a transient error to simulate unpark failure.
		detachErr: errors.New("PVE 500: internal error during detach"),
	}
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: parkerClusterSvc(),
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected retriable error when unpark fails; got nil")
	}

	// Must be retriable so BOSH retries rather than orphaning the disk.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("unpark failure must be retriable; OkToRetry()=false, type=%s", cpiErr.Type())
	}

	// AttachDisk must not have been called.
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must not be called when unpark fails; got volid %q", qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_StrategyFree_NoParkerCalls verifies parker-free behavior
// when the operator opts out with strategy=free: no parker is touched, no
// unpark DetachDisk is issued, and the attach proceeds.
//
// Two ListResources calls are expected and required: attachDiskResolveNode
// locating the VM's owning node, and the holder scan that refuses a volume
// already attached elsewhere. The second one is deliberately not gated on the
// strategy — opting out of parking is exactly the configuration that can leave
// disks stranded on a parker, and a silent second attachment is the outcome
// worth spending a scan to prevent.
func TestHandleAttachDisk_StrategyFree_NoParkerCalls(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	ag := &captureAgent{}
	listCalls := 0
	countingCluster := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			entry, _ := json.Marshal(map[string]any{"vmid": 100, "node": testNode})
			resp := sdkcluster.ListResourcesResponse{entry}
			return &resp, nil
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			VMDiskFormat: "qcow2",
			// strategy=free opts out of the parked default; no range set →
			// ParkedStrategyActive()=false
			DetachedDiskStrategy: "free",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: countingCluster,
		},
		Agent:  ag,
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	result, err := h.Handle(context.Background(), attachArgs(t, "100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("strategy=free: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("strategy=free: expected disk_hints; got nil")
	}
	if len(qemuSvc.detachCalls) != 0 {
		t.Errorf("strategy=free: expected no DetachDisk calls; got %v", qemuSvc.detachCalls)
	}
	// Three fixture reads: the VM-node lookup reads the index once, and the
	// holder guard's authoritative enumeration derives cluster membership and
	// the per-node listing from the same fixture (one read each). The point
	// of the assertion is unchanged: no parker scan beyond the guard.
	if listCalls != 3 {
		t.Errorf("strategy=free: expected 3 fixture reads (VM-node lookup + holder-guard membership + node listing); got %d", listCalls)
	}
}

// TestHandleAttachDisk_HolderIsTarget_NotRefused pins the clause that keeps a
// re-driven attach_disk idempotent. The Director retries attach_disk routinely,
// and on a retry the target VM is already the holder -- refusing there would
// turn every retry into a permanent deployment failure. Deleting
// "&& holder.VMID != targetVMID" from the guard must fail this test.
func TestHandleAttachDisk_HolderIsTarget_NotRefused(t *testing.T) {
	t.Parallel()

	const targetVMID = 101
	const volid = "local-lvm:vm-9001-disk-0"

	// The target already holds the disk, which is what a retry looks like.
	qemuSvc := &attachQEMUService{
		configCfg: map[string]any{"scsi2": volid, "tags": "bosh-cpi"},
	}
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			targetRaw, _ := json.Marshal(map[string]any{
				"vmid": targetVMID, "node": "pve-node1", "type": "qemu", "tags": "bosh-cpi",
			})
			resp := sdkcluster.ListResourcesResponse{targetRaw}
			return &resp, nil
		},
	}

	deps := attachDeps(qemuSvc, &mockAgentService{})
	if mc, ok := deps.PVE.(*mockPVEClient); ok {
		mc.clusterSvc = clusterSvc
	}

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(),
		marshalArgs("101", mustEncodeDiskCID(t, volid, nil)), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("a retry whose target already holds the disk must succeed; got: %v", err)
	}
}

// TestHandleAttachDisk_ForeignHolder_Refused pins the guard that made the extra
// scan worth paying for: a volume another VM already holds must not be attached
// a second time. PVE permits two configs referencing one volume and nothing
// downstream notices, so the refusal has to happen here.
func TestHandleAttachDisk_ForeignHolder_Refused(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	// VM 101 holds the disk; the request asks to attach it to VM 100.
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
			"name":  "some-workload",
		},
	}
	clusterSvc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			a, _ := json.Marshal(map[string]any{"vmid": 100, "node": testNode})
			b, _ := json.Marshal(map[string]any{"vmid": 101, "node": testNode})
			resp := sdkcluster.ListResourcesResponse{a, b}
			return &resp, nil
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 testNode,
			VMDiskFormat:         "qcow2",
			DetachedDiskStrategy: "free",
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, "101", bareCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected a refusal when the volume is already attached to another VM")
	}
	if !strings.Contains(err.Error(), "already attached to VM 100") {
		t.Errorf("the message must name the VM to look at, got: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("no attach may be issued after the refusal; got %q", qemuSvc.attachLastVolid)
	}
}
