package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
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
			// The volume is still on storage, which is the state this
			// refusal exists for: the parker went and the disk stayed.
			storageSvc: &mockStorageService{
				existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return true, nil },
			},
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
			parkerVMCfg,        // option-override read on the parker
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

// ---------------------------------------------------------------------------
// The anchor promise on file storage, where the point probe cannot answer and
// the storage content listing is what decides which refusal the operator gets.
// ---------------------------------------------------------------------------

// anchorNFSVolid is a promised-anchor volume on NFS storage, whose volid
// carries the owning VMID as a directory component the way file storage does.
const (
	anchorNFSStorage = "nfs-images"
	anchorNFSVolid   = "nfs-images:9001/vm-9001-disk-0.qcow2"
)

// anchorFileStorageDeps builds attach Deps for a promised-anchor disk on NFS
// storage with no holder anywhere. The point probe answers PVE's "no format"
// 500, and listing decides whether the volume is provably gone.
func anchorFileStorageDeps(
	qemuSvc *attachQEMUService,
	listing func() (*sdknodes.ListStorageContentResponse, error),
) handlers.Deps {
	client := &visiblePVEClient{
		mockPVEClient: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: emptyClusterSvc(), // no holder anywhere
			storageSvc: &mockStorageService{
				existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
					return false, nfsNoFormat(volume)
				},
			},
			nodesSvc: &mockNodesService{
				listStorageContentFn: func(
					_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams,
				) (*sdknodes.ListStorageContentResponse, error) {
					return listing()
				},
			},
			clusterStorageSvc: &mockClusterStorage{
				storageName: anchorNFSStorage,
				storageType: "nfs",
				shared:      true,
			},
		},
	}
	return handlers.Deps{
		Config: parkedCfg(),
		PVE:    client,
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}
}

// TestHandleAttachDisk_AnchorPromise_VolumeProvenGone_SaysDataIsGone covers
// the state where the parker and the disk it held went together. The strict
// mode escape hatch lets a caller proceed against a free-floating volume, and
// there is no volume left to proceed against, so the refusal has to say the
// data is gone and point at the Director's records instead.
func TestHandleAttachDisk_AnchorPromise_VolumeProvenGone_SaysDataIsGone(t *testing.T) {
	t.Parallel()
	diskCID := mustEncodeDiskCID(t, anchorNFSVolid, &pve.DiskCIDMeta{Anchor: true})

	qemuSvc := &attachQEMUService{}
	deps := anchorFileStorageDeps(qemuSvc, func() (*sdknodes.ListStorageContentResponse, error) {
		return storageContentListing(), nil
	})

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected the data-is-gone refusal, got nil")
	}
	if !strings.Contains(err.Error(), "the data is gone") {
		t.Errorf("the refusal must say the data is gone, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cck") {
		t.Errorf("the refusal must name the recovery, got: %v", err)
	}
	if strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("strict mode cannot recover a volume that is not there, so it must not be advised: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must not run after the refusal; attached %q", qemuSvc.attachLastVolid)
	}
}

// TestHandleAttachDisk_AnchorPromise_AbsenceUnproven_KeepsRefusal pins the
// fail-safe direction. An absence nobody could establish is not an absence,
// so the original refusal and its escape hatch stand.
func TestHandleAttachDisk_AnchorPromise_AbsenceUnproven_KeepsRefusal(t *testing.T) {
	t.Parallel()
	diskCID := mustEncodeDiskCID(t, anchorNFSVolid, &pve.DiskCIDMeta{Anchor: true})

	qemuSvc := &attachQEMUService{}
	deps := anchorFileStorageDeps(qemuSvc, func() (*sdknodes.ListStorageContentResponse, error) {
		return nil, errors.New("storage 'nfs-images' is not online")
	})

	h := handlers.HandleAttachDisk(deps)
	_, err := h.Handle(context.Background(), attachArgs(t, "100", diskCID), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected the anchor-missing refusal, got nil")
	}
	if !strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("an unproven absence keeps the original refusal and its escape hatch, got: %v", err)
	}
	if strings.Contains(err.Error(), "the data is gone") {
		t.Errorf("an unproven absence must not be reported as lost data, got: %v", err)
	}
	if qemuSvc.attachLastVolid != "" {
		t.Errorf("AttachDisk must not run after the refusal; attached %q", qemuSvc.attachLastVolid)
	}
}
