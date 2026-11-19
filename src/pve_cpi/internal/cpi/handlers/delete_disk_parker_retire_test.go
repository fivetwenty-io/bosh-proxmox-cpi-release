package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// The tests in this file swap the process-wide emptied-parker retirement, so
// none of them may call t.Parallel.

// TestHandleDeleteDisk_RetiresTheParkerTheUnparkEmptied proves delete_disk
// hands the parker it unparked the disk from to the retirement, after the
// volume's delete.
func TestHandleDeleteDisk_RetiresTheParkerTheUnparkEmptied(t *testing.T) {
	calls, restore := handlers.SetRetireEmptiedParkerForTest()
	defer restore()

	deleted := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			if len(calls()) != 0 {
				t.Error("the parker must be retired after the volume's delete, not before it")
			}
			deleted = true
			return nil
		},
	}
	client, _ := parkerDeleteClient(t, storageSvc, 90000, testNode, nil)
	h := handlers.HandleDeleteDisk(parkerDepsForDelete(client))
	if _, err := h.Handle(context.Background(), []json.RawMessage{marshal(mustEncodeDiskCID(t, diskCID, nil))},
		jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleted {
		t.Fatal("the volume must be deleted")
	}
	want := []handlers.RetiredParker{{Node: testNode, VMID: 90000, Trigger: "delete_disk"}}
	if got := calls(); !slices.Equal(got, want) {
		t.Fatalf("retirements %v, want %v", got, want)
	}
}

// TestHandleDeleteDisk_RetiresTheParkerTheParkedOwnedDeleteEmptied proves the
// deallocation of a disk its parker owns hands that parker to the retirement.
func TestHandleDeleteDisk_RetiresTheParkerTheParkedOwnedDeleteEmptied(t *testing.T) {
	calls, restore := handlers.SetRetireEmptiedParkerForTest()
	defer restore()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != reparkParkerVMID {
				return map[string]any{}, nil
			}
			return map[string]any{
				"tags": "bosh-cpi;bosh-parker", "protection": true,
				"scsi0": reparkVolid + ",serial=" + reparkStableID + ",size=10G",
			}, nil
		},
		detachDiskFn: func(context.Context, string, int, string) error { return nil },
	}
	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(context.Context, string, string, string) (string, error) { return "", nil },
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	if _, err := handlers.HandleDeleteDisk(deps).Handle(context.Background(),
		[]json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []handlers.RetiredParker{{Node: "lab-ceph-2", VMID: reparkParkerVMID, Trigger: "delete_disk"}}
	if got := calls(); !slices.Equal(got, want) {
		t.Fatalf("retirements %v, want %v", got, want)
	}
}

// TestHandleDeleteDisk_RetiresNoParkerItDidNotEmpty proves a failed unpark,
// and a disk that was on no parker, retire nothing.
func TestHandleDeleteDisk_RetiresNoParkerItDidNotEmpty(t *testing.T) {
	calls, restore := handlers.SetRetireEmptiedParkerForTest()
	defer restore()

	storageSvc := &mockStorageService{deleteVolumeFn: func(context.Context, string, string, string) error { return nil }}
	client, _ := parkerDeleteClient(t, storageSvc, 90000, testNode, errors.New("PVE lock contention"))
	if _, err := handlers.HandleDeleteDisk(parkerDepsForDelete(client)).Handle(context.Background(),
		[]json.RawMessage{marshal(mustEncodeDiskCID(t, diskCID, nil))}, jsonrpc.Context{}); err == nil {
		t.Fatal("a failed unpark must fail the delete")
	}
	client, _ = parkerDeleteClient(t, storageSvc, 0, "", nil)
	if _, err := handlers.HandleDeleteDisk(parkerDepsForDelete(client)).Handle(context.Background(),
		[]json.RawMessage{marshal(mustEncodeDiskCID(t, diskCID, nil))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("no parker was emptied, so none is retired; retirements %v", got)
	}
}
