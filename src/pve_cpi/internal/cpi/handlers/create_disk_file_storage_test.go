package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// File-based storages (dir/nfs/cifs/glusterfs/btrfs) require the volume name
// passed to the content-allocation API to carry a format extension
// ("vm-<vmid>-disk-0.qcow2"); PVE rejects bare names on those plugins with
// "unable to parse volume filename". Block storages take the opposite
// convention (bare name, no extension). These tests pin the naming and
// format-pinning behavior per storage type.
//
// The file-storage cases wire the production BackendResolver over a
// StorageInfoCache (fed by mockClusterStorage) because create_disk reads the
// storage type from the backend's cached classification — the same path
// main.go wires — rather than issuing a second live /storage lookup.

func depsForCreateDiskWithResolver(storageSvc *mockStorageService, storageType string) handlers.Deps {
	client := newHandlerMockClient(storageSvc, nil).(*mockPVEClient)
	client.clusterStorageSvc = &mockClusterStorage{
		storageName: storageName,
		storageType: storageType,
	}
	cache := pve.NewStorageInfoCache(pve.ClusterStorageAsLister(client.ClusterStorage()), time.Minute)
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			DiskStorage:  storageName,
			VMDiskFormat: "qcow2",
			// Opt out of the parked default; parker paths have dedicated tests.
			DetachedDiskStrategy: "free",
		},
		PVE:      client,
		Resolver: pve.NewBackendResolver(client, cache, testNode),
	}
}

func TestHandleCreateDisk_FileStorage_AppendsFormatExtension(t *testing.T) {
	t.Parallel()
	var capturedName, capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			capturedName = name
			capturedFormat = format
			// PVE dir-style plugins return the volid in path form.
			return fmt.Sprintf("%s:%d/%s", storage, vmid, name), nil
		},
	}
	deps := depsForCreateDiskWithResolver(storageSvc, "nfs")

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(131072),
		marshal(map[string]string{"disk_format": "qcow2"}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(capturedName, ".qcow2") {
		t.Errorf("CreateVolume name = %q; want .qcow2 suffix on nfs storage", capturedName)
	}
	if capturedFormat != "qcow2" {
		t.Errorf("CreateVolume format = %q; want qcow2", capturedFormat)
	}

	diskCID, ok := result.(string)
	if !ok || diskCID == "" {
		t.Fatalf("expected non-empty string result, got %T %v", result, result)
	}
	bare, _, parseErr := pve.ParseEncodedDiskCID(diskCID)
	if parseErr != nil {
		t.Fatalf("ParseEncodedDiskCID(%q): %v", diskCID, parseErr)
	}
	// The path-form volid PVE returned must survive as the disk CID so
	// attach/detach/delete address the volume PVE actually allocated.
	if !strings.Contains(bare, "/vm-") || !strings.HasSuffix(bare, ".qcow2") {
		t.Errorf("disk CID bare volid = %q; want path-form storage:<vmid>/vm-<vmid>-disk-0.qcow2", bare)
	}
}

func TestHandleCreateDisk_FileStorage_NoExplicitFormat_PinsResolvedDefault(t *testing.T) {
	t.Parallel()
	var capturedName, capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			capturedName = name
			capturedFormat = format
			return fmt.Sprintf("%s:%d/%s", storage, vmid, name), nil
		},
	}
	deps := depsForCreateDiskWithResolver(storageSvc, "nfs")

	h := handlers.HandleCreateDisk(deps)
	// No disk_format at any layer: block storages let PVE auto-pick (empty
	// format arg), but a file storage needs a concrete extension, so the
	// resolved default (config VMDiskFormat=qcow2) must be pinned instead.
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(capturedName, ".qcow2") {
		t.Errorf("CreateVolume name = %q; want .qcow2 suffix (resolved default) on nfs storage", capturedName)
	}
	if capturedFormat != "qcow2" {
		t.Errorf("CreateVolume format = %q; want qcow2 pinned to match the extension", capturedFormat)
	}
}

func TestHandleCreateDisk_FileStorage_ExplicitRawFormat(t *testing.T) {
	t.Parallel()
	var capturedName, capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			capturedName = name
			capturedFormat = format
			return fmt.Sprintf("%s:%d/%s", storage, vmid, name), nil
		},
	}
	deps := depsForCreateDiskWithResolver(storageSvc, "dir")

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_format": "raw"}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(capturedName, ".raw") {
		t.Errorf("CreateVolume name = %q; want .raw suffix on dir storage", capturedName)
	}
	if capturedFormat != "raw" {
		t.Errorf("CreateVolume format = %q; want raw", capturedFormat)
	}
}

func TestHandleCreateDisk_BlockStorage_KeepsBareName(t *testing.T) {
	t.Parallel()
	var capturedName, capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			capturedName = name
			capturedFormat = format
			return fmt.Sprintf("%s:%s", storage, name), nil
		},
	}
	deps := depsForCreateDiskWithStorageType(storageSvc, "lvmthin")

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(capturedName, ".") {
		t.Errorf("CreateVolume name = %q; block storage must get a bare name without extension", capturedName)
	}
	if capturedFormat != "" {
		t.Errorf("CreateVolume format = %q; want empty (PVE auto-pick) when no layer set one", capturedFormat)
	}
}

func TestHandleCreateDisk_UnknownStorageType_KeepsBareName(t *testing.T) {
	t.Parallel()
	var capturedName string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, name string) (string, error) {
			capturedName = name
			return fmt.Sprintf("%s:%s", storage, name), nil
		},
	}
	// No cluster-storage service wired → type lookup returns "" → the
	// pre-existing bare-name behavior must be preserved.
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(capturedName, ".") {
		t.Errorf("CreateVolume name = %q; want bare name when storage type is unknown", capturedName)
	}
}
