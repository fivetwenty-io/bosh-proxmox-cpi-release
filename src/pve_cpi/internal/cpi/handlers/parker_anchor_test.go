package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Anchor-missing invariant, end to end through HandleAttachDisk. The CID
// envelope promises a parker anchor (created under the parked strategy); the
// cluster scan finds no holder at all.
// ---------------------------------------------------------------------------

// TestHandleAttachDisk_AnchorPromise_NoHolder_Refused verifies the strict
// default: a promised disk with no holder is refused before any mutating PVE
// call, with the escape hatch named in the error.
func TestHandleAttachDisk_AnchorPromise_NoHolder_Refused(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Anchor: true})

	qemuSvc := &attachQEMUService{}
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: emptyClusterSvc(), // no holder anywhere
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected the anchor-missing refusal, got nil")
	}
	if !strings.Contains(err.Error(), "parker anchor") || !strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("refusal must explain the promise and name the escape hatch, got: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must not run after the refusal; attached %q", qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_AnchorPromise_NoHolder_StrictOff_Proceeds verifies the
// escape hatch: pve.parked_anchor_strict=false restores the permissive
// treat-as-free-floating behavior and the attach completes.
func TestHandleAttachDisk_AnchorPromise_NoHolder_StrictOff_Proceeds(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Anchor: true})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg: map[string]any{
			"scsi1": bareCID,
		},
	}
	cfg := parkedCfg()
	cfg.ParkedAnchorStrict = boolPtr(false)
	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: emptyClusterSvc(),
		},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleAttachDisk(deps)
	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("strict off must proceed, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result, got nil")
	}
}

// TestHandleAttachDisk_AnchorPromise_HolderPresent_Unparks verifies the
// promise is satisfied by a live parker: the disk unparks and attaches
// exactly as an unpromised parked disk would.
func TestHandleAttachDisk_AnchorPromise_HolderPresent_Unparks(t *testing.T) {
	t.Parallel()
	const bareCID = parkedVolid
	diskCID := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{Anchor: true})

	parkerVMCfg := map[string]any{
		parkerSlot: bareCID,
		"tags":     "bosh-parker",
		"name":     "bosh-parker-90000",
	}
	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfgs: []map[string]any{
			parkerVMCfg,        // FindVMByDiskVolid scan
			parkerVMCfg,        // holder resolution (tags + slot)
			parkerVMCfg,        // unpark re-resolve under the lock
			{},                 // chooseSCSISlotSkippingZero on target
			{"scsi1": bareCID}, // ResolveDiskID after attach
		},
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
	result, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("promised disk with a live parker must attach, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected disk_hints result, got nil")
	}
	if len(qemuSvc.detachCalls) != 1 || qemuSvc.detachCalls[0] != parkerSlot {
		t.Errorf("expected one unpark DetachDisk(%q), got %v", parkerSlot, qemuSvc.detachCalls)
	}
}
