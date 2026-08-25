// delete_disk_parked_owned_test.go — the lab-ceph re-park deletion failure.
// After a reassignment the volume is named for the parker holding it, so
// delete_disk deallocates it through the parker's detach rather than through
// storage. These tests pin what must follow: no second delete of a volume
// that is already gone, an imgdel that races one anyway reported as success,
// and an absent volume never diagnosed as a parker deleted out-of-band.
package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	// reparkParkerVMID is the parker the disk ended up on after the last of
	// the report's seven park/unpark reassignments.
	reparkParkerVMID = 90842
	// reparkVolid is the parker-named volume that reassignment produced.
	reparkVolid = "rbd:vm-90842-disk-0"
	// reparkStableID is the disk identity riding the parker's drive entry.
	reparkStableID = "bpd-00112233aabbccdd"
)

// reparkDiskCID encodes the CID as create_disk emitted it under the parked
// strategy: a stable identity plus the parker anchor promise.
func reparkDiskCID(t *testing.T) string {
	t.Helper()
	return mustEncodeDiskCID(t, "rbd:vm-604-disk-0", &pve.DiskCIDMeta{ID: reparkStableID, Anchor: true})
}

// reparkDeps builds delete_disk Deps for a shared-RBD cluster whose parker
// sits on a different node than the configured one, as on lab-ceph.
func reparkDeps(client *mockPVEClient) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     "lab-ceph-0",
			DiskStorage:              "rbd",
			DetachedDiskStrategy:     "parked",
			DiskDeleteStateGuard:     "off",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// reparkClusterSvc places the parker on lab-ceph-2.
func reparkClusterSvc() *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"vmid": reparkParkerVMID, "node": "lab-ceph-2", "type": "qemu"})
			resp := sdkclusterapi.ListResourcesResponse{raw}
			return &resp, nil
		},
	}
}

// TestHandleDeleteDisk_ParkedOwned_SkipsRedundantStorageDelete is the primary
// defect. The parker's detach IS the deallocation for an owner-named volume,
// so issuing imgdel afterwards deletes nothing and can only fail.
func TestHandleDeleteDisk_ParkedOwned_SkipsRedundantStorageDelete(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	detached := false
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != reparkParkerVMID {
				return map[string]any{}, nil
			}
			cfg := map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}
			if !detached {
				cfg["scsi0"] = reparkVolid + ",serial=" + reparkStableID + ",size=10G"
			}
			return cfg, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, _ string) error {
			detached = true
			return nil
		},
	}

	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	if _, err := h.Handle(context.Background(),
		[]json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !detached {
		t.Fatal("the parked-owned deallocation must run")
	}
	if deleteCalls != 0 {
		t.Errorf("delete_disk must not issue imgdel for a volume the parker detach already deallocated, got %d calls", deleteCalls)
	}
}

// TestHandleDeleteDisk_ImgdelOnAbsentRbdImage_Idempotent is the defense in
// depth: whatever removed the image first, an imgdel that reports the image
// absent has nothing left to do, and must not surface as a delete failure.
func TestHandleDeleteDisk_ImgdelOnAbsentRbdImage_Idempotent(t *testing.T) {
	t.Parallel()

	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			return "", errors.New(`rbd: error opening image vm-90842-disk-0: (2) No such file or directory`)
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node: "lab-ceph-0", DiskStorage: "rbd",
			DetachedDiskStrategy: "free", DiskDeleteStateGuard: "off",
		},
		PVE:    &mockPVEClient{storageSvc: storageSvc, clusterSvc: &mockClusterSvc{}},
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleDeleteDisk(deps)
	if _, err := h.Handle(context.Background(),
		[]json.RawMessage{marshal(mustEncodeDiskCID(t, "rbd:vm-90842-disk-0", nil))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("an imgdel reporting the image already absent must be idempotent success, got %v", err)
	}
}

// TestHandleDeleteDisk_AnchorMissing_VolumeGone_Idempotent covers the
// follow-on the report calls the worse half. After the deallocation the
// parker carries no scsi0 and the volume is gone, so the holder scan finds
// nothing — which the anchor guard read as "the parker was deleted
// out-of-band" and answered with advice to relax pve.parked_anchor_strict.
// A volume that is not on storage has already been deleted; that is what
// delete_disk was asked for.
func TestHandleDeleteDisk_AnchorMissing_VolumeGone_Idempotent(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return false, nil },
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	// The parker survives but holds nothing; no VM in the cluster references
	// the volume.
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("a disk whose volume is already off storage must be idempotent success, got %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_VolumePresent_StillRefuses keeps the
// guard that the idempotency shortcut must not weaken: a volume that IS on
// storage with no VM referencing it is the state the anchor refusal exists
// for, and it must still be refused with its recovery advice.
func TestHandleDeleteDisk_AnchorMissing_VolumePresent_StillRefuses(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return true, nil },
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("a promised-anchor volume still on storage with no holder must be refused")
	}
	if !strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("the refusal must keep its recovery advice, got %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_ExistenceUnprovable_Refuses is the
// fail-safe direction: when the existence probe itself fails, absence is not
// established, and the refusal stands rather than a delete proceeding on a
// guess.
func TestHandleDeleteDisk_AnchorMissing_ExistenceUnprovable_Refuses(t *testing.T) {
	t.Parallel()

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, errors.New("ceph mon unreachable")
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("an unprovable absence must not be treated as a completed delete")
	}
}
