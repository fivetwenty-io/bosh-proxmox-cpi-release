package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// DiskCIDMeta.Format: recorded at create_disk, preferred at attach_disk over
// the config-derived format guess.
// ---------------------------------------------------------------------------

// createDiskMetaFor runs create_disk with the given cloud_properties against a
// thick-lvm pool (no discard/ssd noise; block-native, so the recorded format
// is always raw) and returns the decoded CID metadata.
func createDiskMetaFor(t *testing.T, cloudProps map[string]any) *pve.DiskCIDMeta {
	t.Helper()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := depsForCreateDiskWithStorageType(storageSvc, "lvm")

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(cloudProps),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diskCID, ok := result.(string)
	if !ok || diskCID == "" {
		t.Fatalf("expected non-empty string result, got %T %v", result, result)
	}
	_, meta, parseErr := pve.ParseEncodedDiskCID(diskCID)
	if parseErr != nil {
		t.Fatalf("ParseEncodedDiskCID(%q): %v", diskCID, parseErr)
	}
	if meta == nil {
		t.Fatal("expected CID metadata (format is always recorded)")
	}
	return meta
}

// TestHandleCreateDisk_RecordsResolvedFormatInCID verifies the CID envelope
// carries the format the disk was actually created under: on a block-native
// pool that is raw regardless of the config default, and per-call raw is
// recorded verbatim.
func TestHandleCreateDisk_RecordsResolvedFormatInCID(t *testing.T) {
	t.Parallel()

	if got := createDiskMetaFor(t, map[string]any{}).Format; got != "raw" {
		t.Errorf("meta.Format = %q; want raw on a block-native pool", got)
	}
	if got := createDiskMetaFor(t, map[string]any{"disk_format": "raw"}).Format; got != "raw" {
		t.Errorf("meta.Format = %q; want the per-call raw", got)
	}
}

// TestHandleAttachDisk_CIDFormat_PreferredOverConfig verifies the attach-time
// discard/ssd auto-resolution uses the format recorded in the CID envelope,
// not the current vm_disk_format. On file-backed storage TRIM capability is
// format-dependent (qcow2 yes, raw no), so a raw-recorded disk must stay bare
// even when config has since flipped to qcow2.
func TestHandleAttachDisk_CIDFormat_PreferredOverConfig(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Format: "raw"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:         testNode,
		VMDiskFormat: "qcow2", // changed since create — must NOT win
		// Isolate the format story from the iothread global default.
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "nfs")

	h := handlers.HandleAttachDisk(deps)
	if _, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qemuSvc.attachLastVolid != bareCID {
		t.Errorf("AttachDisk volid: want bare %q (raw on nfs is not TRIM-capable), got %q", bareCID, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_CIDFormat_EnablesTrimDespiteConfig verifies the other
// direction: a qcow2-recorded disk keeps its discard/ssd auto-resolution even
// after vm_disk_format changed to raw.
func TestHandleAttachDisk_CIDFormat_EnablesTrimDespiteConfig(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9002-disk-0"
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Format: "qcow2"})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:            testNode,
		VMDiskFormat:    "raw", // changed since create — must NOT win
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "nfs")

	h := handlers.HandleAttachDisk(deps)
	if _, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantVolid := bareCID + ",discard=on,ssd=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q (qcow2 on nfs is TRIM-capable), got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_CIDFormat_LegacyCIDFallsBackToConfig verifies a legacy
// envelope with no recorded format keeps the pre-existing behavior: the
// config-derived guess decides, exactly as before the format field existed.
func TestHandleAttachDisk_CIDFormat_LegacyCIDFallsBackToConfig(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9003-disk-0"
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Pool: "local-lvm"}) // no Format

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{
		Node:            testNode,
		VMDiskFormat:    "qcow2",
		DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)},
	}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "nfs")

	h := handlers.HandleAttachDisk(deps)
	if _, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantVolid := bareCID + ",discard=on,ssd=1"
	if qemuSvc.attachLastVolid != wantVolid {
		t.Errorf("AttachDisk volid: want %q (config qcow2 fallback), got %q", wantVolid, qemuSvc.attachLastVolid)
	}
}
