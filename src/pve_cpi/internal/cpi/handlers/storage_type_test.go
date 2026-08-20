package handlers_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// storageTypeCase holds the per-type inputs and expected behaviours shared
// across storage-type parametrized subtests.
type storageTypeCase struct {
	// storageType is the WithStorageType argument: "zfspool", "lvmthin", "dir".
	storageType string
	// storageName is the pool name assigned by WithStorageType.
	storageName string
	// expectedFormat is the format arg create_disk must forward to CreateVolume.
	// Empty string means the handler passes no explicit format (PVE auto-picks).
	expectedFormat string
	// diskCIDPrefix is the storage label before the colon in the returned CID.
	diskCIDPrefix string
}

var storageTypeCases = []storageTypeCase{
	{
		storageType:    "zfspool",
		storageName:    "local-zfs",
		expectedFormat: "", // zfspool rejects qcow2; PVE auto-picks raw
		diskCIDPrefix:  "local-zfs",
	},
	{
		storageType:    "lvmthin",
		storageName:    "local-lvm-thin",
		expectedFormat: "", // lvmthin rejects qcow2; PVE auto-picks raw
		diskCIDPrefix:  "local-lvm-thin",
	},
	{
		storageType:    "dir",
		storageName:    "local",
		expectedFormat: "", // dir with no explicit disk_format -> pass "" to PVE
		diskCIDPrefix:  "local",
	},
}

// TestCreateDisk_StorageTypes verifies that create_disk routes to the correct
// storage pool and forwards an empty format string when no disk_format is set
// in cloud_properties. Block storages (zfspool, lvmthin) and dir storage all
// require format="" so PVE selects the appropriate default per storage type.
func TestCreateDisk_StorageTypes(t *testing.T) {
	for _, tc := range storageTypeCases {
		t.Run(tc.storageType, func(t *testing.T) {
			t.Parallel()

			var capturedStorage string
			var capturedFormat string

			storageSvc := &mockStorageService{
				createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, _ string) (string, error) {
					capturedStorage = storage
					capturedFormat = format
					return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
				},
			}

			cfg := testConfigWith(WithStorageType(tc.storageType))
			// Opt out of the parked default; parker paths have dedicated tests.
			cfg.DetachedDiskStrategy = "free"
			deps := handlers.Deps{
				Config: cfg,
				PVE:    newHandlerMockClient(storageSvc, nil),
				Logger: log.NewNopLogger(),
			}

			h := handlers.HandleCreateDisk(deps)
			result, err := h.Handle(context.Background(), marshalArgs(1024, map[string]string{}), jsonrpc.Context{})

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			diskCID, ok := result.(string)
			if !ok {
				t.Fatalf("expected string result, got %T", result)
			}
			if diskCID == "" {
				t.Fatal("expected non-empty disk_cid")
			}

			if capturedStorage != tc.storageName {
				t.Errorf("storage: got %q, want %q", capturedStorage, tc.storageName)
			}
			if capturedFormat != tc.expectedFormat {
				t.Errorf("format: got %q, want %q (PVE auto-pick for %s)", capturedFormat, tc.expectedFormat, tc.storageType)
			}
		})
	}
}

// TestResizeDisk_StorageTypes verifies that resize_disk succeeds for each
// supported storage type. The storage type affects the CID format, not the
// resize logic; these subtests confirm ParseDiskCID handles each CID shape
// and that ResizeDisk is called with the correct positive delta.
func TestResizeDisk_StorageTypes(t *testing.T) {
	t.Parallel()
	type resizeCase struct {
		storageType string
		diskCID     string
		diskSlot    string
		currentGiB  int
		newMiB      int
		wantDelta   int
	}

	cases := []resizeCase{
		{
			storageType: "zfspool",
			diskCID:     "local-zfs:vm-9001-disk-0",
			diskSlot:    "scsi0",
			currentGiB:  10,
			newMiB:      20480, // 20 GiB
			wantDelta:   10,
		},
		{
			storageType: "lvmthin",
			diskCID:     "local-lvm-thin:vm-9001-disk-0",
			diskSlot:    "scsi1",
			currentGiB:  15,
			newMiB:      25600, // 25 GiB
			wantDelta:   10,
		},
		{
			storageType: "dir",
			diskCID:     "local:9001/vm-9001-disk-0.raw",
			diskSlot:    "scsi2",
			currentGiB:  20,
			newMiB:      30720, // 30 GiB
			wantDelta:   10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.storageType, func(t *testing.T) {
			t.Parallel()

			var capturedDelta int
			diskOptStr := fmt.Sprintf("%s,size=%dG", tc.diskCID, tc.currentGiB)

			qemuSvc := resizeQEMUWithDisk(tc.diskSlot, diskOptStr,
				func(_ context.Context, _ string, _ int, _ string, deltaGiB int) (string, error) {
					capturedDelta = deltaGiB
					return "", nil
				},
			)

			h := handlers.HandleResizeDisk(resizeDeps(qemuSvc, resizeClusterWith(9001), nil))
			result, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, tc.diskCID, nil), tc.newMiB), jsonrpc.Context{})
			if err != nil {
				t.Fatalf("unexpected error for %s CID: %v", tc.storageType, err)
			}
			if result != nil {
				t.Errorf("result: want nil (void), got %v", result)
			}
			if capturedDelta != tc.wantDelta {
				t.Errorf("resize delta: want %d GiB, got %d GiB", tc.wantDelta, capturedDelta)
			}
		})
	}
}
