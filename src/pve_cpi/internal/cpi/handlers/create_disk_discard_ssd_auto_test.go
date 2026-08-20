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

// depsForCreateDiskWithStorageType builds create_disk Deps wired with a
// mockClusterStorage reporting storageType for the disk's storage pool, so
// the discard/ssd TRIM-capability auto-resolution has a live type to resolve
// against.
func depsForCreateDiskWithStorageType(storageSvc *mockStorageService, storageType string) handlers.Deps {
	client := newHandlerMockClient(storageSvc, nil).(*mockPVEClient)
	client.clusterStorageSvc = &mockClusterStorage{
		storageName: storageName,
		storageType: storageType,
	}
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			DiskStorage:  storageName,
			VMDiskFormat: "qcow2",
			// Opt out of the parked default; parker paths have dedicated tests.
			DetachedDiskStrategy: "free",
		},
		PVE: client,
	}
}

func TestHandleCreateDisk_DiscardSSDAuto_TrimCapableStorage_BothBaked(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := depsForCreateDiskWithStorageType(storageSvc, "lvmthin")

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no perf opts — auto-resolution decides
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
		t.Fatal("expected CID metadata with baked options")
	}
	if meta.Opts["discard"] != "on" {
		t.Errorf("meta.Opts[discard] = %q; want on (lvmthin is TRIM-capable)", meta.Opts["discard"])
	}
	if meta.Opts["ssd"] != "1" {
		t.Errorf("meta.Opts[ssd] = %q; want 1 (lvmthin is TRIM-capable, scsi bus)", meta.Opts["ssd"])
	}
}

func TestHandleCreateDisk_DiscardSSDAuto_NonTrimStorage_NothingBaked(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := depsForCreateDiskWithStorageType(storageSvc, "lvm") // thick LVM

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
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
	if meta != nil {
		if _, present := meta.Opts["discard"]; present {
			t.Errorf("meta.Opts must not contain discard on thick lvm, got %v", meta.Opts)
		}
		if _, present := meta.Opts["ssd"]; present {
			t.Errorf("meta.Opts must not contain ssd on thick lvm, got %v", meta.Opts)
		}
	}
}

func TestHandleCreateDisk_DiscardSSDAuto_ExplicitFalse_OverridesTrimCapable(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := depsForCreateDiskWithStorageType(storageSvc, "lvmthin")

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{"discard": false, "ssd": false}),
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
	if meta != nil {
		if _, present := meta.Opts["discard"]; present {
			t.Errorf("meta.Opts must not contain discard with explicit false, got %v", meta.Opts)
		}
		if _, present := meta.Opts["ssd"]; present {
			t.Errorf("meta.Opts must not contain ssd with explicit false, got %v", meta.Opts)
		}
	}
}
