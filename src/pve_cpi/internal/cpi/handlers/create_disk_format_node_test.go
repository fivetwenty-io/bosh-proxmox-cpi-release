package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The CID envelope's recorded Format must describe the volume PVE actually
// created. Block-native storages (lvm/lvmthin/zfspool/rbd) have no file
// format — PVE always allocates raw — so recording the built-in qcow2
// fallback there writes a lie into the CID. These tests pin the recording
// matrix: an operator-expressed format (per-call, profile, or the global
// vm_disk_format) wins verbatim; with no preference anywhere the recorded
// format follows the storage's backing type; an unknown type keeps the
// default. Node recording is pinned alongside: shared backends carry no node
// pin (DiskCIDMeta.Node's documented contract), local backends do.

// depsForCreateDiskFormatRecord wires the production BackendResolver over a
// StorageInfoCache fed by mockClusterStorage, the same shape main.go wires,
// so both the storage-type classification and the shared/local backend split
// resolve from one fixture. vmDiskFormat parameterizes the global
// vm_disk_format config knob.
func depsForCreateDiskFormatRecord(storageSvc *mockStorageService, storageType, vmDiskFormat string) handlers.Deps {
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
			VMDiskFormat: vmDiskFormat,
			// Opt out of the parked default; parker paths have dedicated tests.
			DetachedDiskStrategy: "free",
		},
		PVE:      client,
		Resolver: pve.NewBackendResolver(client, cache, testNode),
	}
}

// formatRecordStorageSvc answers CreateVolume the way PVE does per plugin
// family: path-form volids for file-style names (extension present), bare
// volids otherwise.
func formatRecordStorageSvc() *mockStorageService {
	return &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, name string) (string, error) {
			if strings.Contains(name, ".") {
				return fmt.Sprintf("%s:%d/%s", storage, vmid, name), nil
			}
			return fmt.Sprintf("%s:%s", storage, name), nil
		},
	}
}

// createDiskMetaWith runs create_disk against deps and returns the decoded
// CID metadata.
func createDiskMetaWith(t *testing.T, deps handlers.Deps, cloudProps map[string]any) *pve.DiskCIDMeta {
	t.Helper()
	if cloudProps == nil {
		cloudProps = map[string]any{}
	}
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
		t.Fatal("expected CID metadata")
	}
	return meta
}

func TestHandleCreateDisk_RecordedFormat_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		storageType  string
		vmDiskFormat string
		cloudProps   map[string]any
		wantFormat   string
	}{
		// No preference at any layer: the recorded format follows the
		// storage's backing type.
		{"lvmthin no preference records raw", "lvmthin", "", nil, "raw"},
		{"rbd no preference records raw", "rbd", "", nil, "raw"},
		{"lvm no preference records raw", "lvm", "", nil, "raw"},
		{"zfspool no preference records raw", "zfspool", "", nil, "raw"},
		{"dir no preference records qcow2", "dir", "", nil, "qcow2"},
		{"nfs no preference records qcow2", "nfs", "", nil, "qcow2"},
		// On file-based storages an expressed format is what PVE creates
		// and is recorded verbatim.
		{"explicit raw wins on nfs", "nfs", "", map[string]any{"disk_format": "raw"}, "raw"},
		{"config raw honored on dir", "dir", "raw", nil, "raw"},
		// Block-native storages allocate raw no matter what any layer
		// expressed; recording the expressed qcow2 would describe a volume
		// that does not exist.
		{"explicit qcow2 still records raw on rbd", "rbd", "", map[string]any{"disk_format": "qcow2"}, "raw"},
		{"config qcow2 still records raw on rbd", "rbd", "qcow2", nil, "raw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := depsForCreateDiskFormatRecord(formatRecordStorageSvc(), tc.storageType, tc.vmDiskFormat)
			meta := createDiskMetaWith(t, deps, tc.cloudProps)
			if meta.Format != tc.wantFormat {
				t.Errorf("meta.Format = %q; want %q", meta.Format, tc.wantFormat)
			}
		})
	}
}

// TestHandleCreateDisk_RecordedFormat_UnknownTypeKeepsDefault pins the
// fail-open branch: with no cluster-storage service wired the type lookup
// answers "", and the recorded format stays the built-in qcow2 default
// exactly as before.
func TestHandleCreateDisk_RecordedFormat_UnknownTypeKeepsDefault(t *testing.T) {
	t.Parallel()
	client := newHandlerMockClient(formatRecordStorageSvc(), nil).(*mockPVEClient)
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 testNode,
			DiskStorage:          storageName,
			VMDiskFormat:         "",
			DetachedDiskStrategy: "free",
		},
		PVE: client,
	}
	meta := createDiskMetaWith(t, deps, nil)
	if meta.Format != "qcow2" {
		t.Errorf("meta.Format = %q; want the qcow2 default when the storage type is unknown", meta.Format)
	}
}

// TestHandleCreateDisk_RecordedFormat_DoesNotChangeFormatArg verifies the
// derivation is recording-only: with no preference at any layer the format
// sent to CreateVolume stays empty (PVE auto-picks) even though the CID now
// records raw for a block-native pool.
func TestHandleCreateDisk_RecordedFormat_DoesNotChangeFormatArg(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("%s:%s", storage, name), nil
		},
	}
	deps := depsForCreateDiskFormatRecord(storageSvc, "rbd", "")
	meta := createDiskMetaWith(t, deps, nil)
	if capturedFormat != "" {
		t.Errorf("CreateVolume format = %q; want empty (PVE auto-pick) — the derivation must not leak into formatArg", capturedFormat)
	}
	if meta.Format != "raw" {
		t.Errorf("meta.Format = %q; want raw recorded for rbd", meta.Format)
	}
}

// TestHandleCreateDisk_NodePin_SharedVsLocal pins DiskCIDMeta.Node's
// documented contract in both directions: a shared-backend disk (rbd is
// shared by type) records no node, a local-backend disk (lvmthin) records
// the node the volume was created on.
func TestHandleCreateDisk_NodePin_SharedVsLocal(t *testing.T) {
	t.Parallel()

	t.Run("shared rbd records no node pin", func(t *testing.T) {
		t.Parallel()
		deps := depsForCreateDiskFormatRecord(formatRecordStorageSvc(), "rbd", "")
		meta := createDiskMetaWith(t, deps, nil)
		if meta.Node != "" {
			t.Errorf("meta.Node = %q; want empty on a shared backend (contract: node pins are for node-local backends)", meta.Node)
		}
		if meta.Pool != storageName {
			t.Errorf("meta.Pool = %q; want %q (pool must still be recorded)", meta.Pool, storageName)
		}
	})

	t.Run("local lvmthin records the owning node", func(t *testing.T) {
		t.Parallel()
		deps := depsForCreateDiskFormatRecord(formatRecordStorageSvc(), "lvmthin", "")
		meta := createDiskMetaWith(t, deps, nil)
		if meta.Node != testNode {
			t.Errorf("meta.Node = %q; want %q on a local backend", meta.Node, testNode)
		}
	})
}

// TestIsBlockNativeStorage pins the classification helper the recording
// derivation rests on, including its relationship to the TRIM set (thick lvm
// is block-native but not TRIM-capable).
func TestIsBlockNativeStorage(t *testing.T) {
	t.Parallel()
	blockNative := []string{"lvm", "lvmthin", "zfspool", "rbd", "RBD", " lvmthin "}
	for _, st := range blockNative {
		if !pve.IsBlockNativeStorage(st) {
			t.Errorf("IsBlockNativeStorage(%q) = false; want true", st)
		}
	}
	notBlockNative := []string{"dir", "nfs", "cifs", "cephfs", "glusterfs", "btrfs", "pbs", "", "unknown"}
	for _, st := range notBlockNative {
		if pve.IsBlockNativeStorage(st) {
			t.Errorf("IsBlockNativeStorage(%q) = true; want false", st)
		}
	}
}
