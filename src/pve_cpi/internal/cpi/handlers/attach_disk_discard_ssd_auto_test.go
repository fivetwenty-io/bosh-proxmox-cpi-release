package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// attachDepsPerfWithStorageType is attachDepsPerf plus a wired
// clusterStorageSvc reporting storageType for storageName, so the
// discard/ssd TRIM-capability auto-resolution has a live type to resolve
// against.
func attachDepsPerfWithStorageType(qemuSvc qemu.Service, cfg *config.CPIConfig, storageType string) handlers.Deps {
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:           qemuSvc,
			clusterStorageSvc: &mockClusterStorage{storageName: storageName, storageType: storageType},
		},
		Logger: log.NewNopLogger(),
	}
}

// TestHandleAttachDisk_DiscardSSDAuto_BareCID_TrimCapableStorage verifies
// that a bare (no-meta) disk CID re-attached to a TRIM-capable pool picks up
// discard and ssd via auto-resolution, alongside the iothread default.
func TestHandleAttachDisk_DiscardSSDAuto_BareCID_TrimCapableStorage(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{Node: testNode, VMDiskFormat: "qcow2"}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "zfspool")
	h := handlers.HandleAttachDisk(deps)

	_, err := h.Handle(context.Background(), attachArgs("100", bareCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// buildDiskOptStr order: alphabetical — discard, iothread, ssd.
	wantVolid := bareCID + ",discard=on,iothread=1,ssd=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_DiscardSSDAuto_LegacyCID_SSDInvariantViolation
// verifies the interaction between the new ssd auto-resolution and the
// pre-existing disk-performance invariant guard: a disk CID recorded before
// ssd auto-resolution existed (only "cache" recorded, no "ssd" key at all)
// re-attached to a TRIM-capable pool now resolves ssd=1 as a fresh global
// default — a structural divergence from the disk's creation-time record,
// caught by disk_perf_invariant_mode exactly as any other global-default
// change would be (see the iothread/virtio_scsi_single default flip).
// discard is NOT invariant-tracked (see diskPerfInvariantKeys), so its own
// divergence must not appear in the error.
func TestHandleAttachDisk_DiscardSSDAuto_LegacyCID_SSDInvariantViolation(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9100-disk-0"
	// Legacy CID: only cache was ever recorded — no ssd (or discard) key,
	// simulating a disk created before this feature existed.
	encodedCID := perfDiskCID(bareCID, map[string]string{"cache": "writeback"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:         testNode,
		VMDiskFormat: "qcow2",
		// iothread also defaults true and is likewise unrecorded on this
		// legacy CID, which would independently diverge and confuse the
		// assertions below — disable it explicitly so this test isolates
		// the ssd-specific divergence story.
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "rbd") // TRIM-capable

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs("100", encodedCID), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected an invariant-divergence error for the unrecorded ssd auto-default, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud (non-retriable), got %v", err)
	}
	if !strings.Contains(err.Error(), "ssd") {
		t.Errorf("error should name the diverging option ssd, got: %v", err)
	}
	if strings.Contains(err.Error(), "discard") {
		t.Errorf("discard is not invariant-tracked and must not appear in the error, got: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must NOT be called on enforce reject; got volid %q", qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_DiscardSSDAuto_LegacyCID_WarnModeProceeds verifies
// that warn mode logs the same ssd divergence but still proceeds, applying
// the merged options (cache from the CID, ssd and discard from the fresh
// auto-resolution).
func TestHandleAttachDisk_DiscardSSDAuto_LegacyCID_WarnModeProceeds(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9101-disk-0"
	encodedCID := perfDiskCID(bareCID, map[string]string{"cache": "writeback"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:                  testNode,
		VMDiskFormat:          "qcow2",
		DiskPerfInvariantMode: "warn",
		DiskPerformance:       &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "rbd")

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs("100", encodedCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("warn mode must not error, got: %v", err)
	}

	wantVolid := bareCID + ",cache=writeback,discard=on,ssd=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q, got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}
