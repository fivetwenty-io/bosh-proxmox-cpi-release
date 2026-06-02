package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// mockStorageService is defined in testmocks_test.go (canonical taxonomy).

// newHandlerMockClient builds a mock pve.Client with wired storage and cluster
// using the canonical mockPVEClient / mockClusterSvc from testmocks_test.go.
// clusterVMIDs is the set of VMIDs reported by the cluster (for NextDiskVMID).
func newHandlerMockClient(storageSvc *mockStorageService, clusterVMIDs []int) pve.Client {
	listFn := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		resp := make(sdkcluster.ListResourcesResponse, 0, len(clusterVMIDs))
		for _, id := range clusterVMIDs {
			id64 := int64(id)
			entry := struct {
				Vmid int64 `json:"vmid"`
			}{Vmid: id64}
			raw, _ := json.Marshal(entry)
			resp = append(resp, raw)
		}
		return &resp, nil
	}
	return &mockPVEClient{
		storageSvc: storageSvc,
		clusterSvc: &mockClusterSvc{listResourcesFn: listFn},
	}
}

// baseDepsForCreate builds a Deps suitable for create_disk tests.
func baseDepsForCreate(t *testing.T, storageSvc *mockStorageService, clusterVMIDs []int) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			DiskStorage:  storageName,
			VMDiskFormat: "qcow2",
		},
		PVE:    newHandlerMockClient(storageSvc, clusterVMIDs),
		Logger: log.NewNopLogger(),
	}
}

// marshal produces a json.RawMessage from v; panics on error (test helper only).
func marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// create_disk tests
// ---------------------------------------------------------------------------

func TestHandleCreateDisk_Happy(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
			if node != testNode {
				t.Errorf("unexpected node %q", node)
			}
			if storage != storageName {
				t.Errorf("unexpected storage %q", storage)
			}
			if sizeGiB != 1 {
				t.Errorf("expected sizeGiB=1, got %d", sizeGiB)
			}
			// Format is intentionally empty when caller did not pass an
			// explicit cloud_properties.disk_format — PVE auto-picks per
			// storage type. (lvm/lvmthin/zfspool reject qcow2.)
			if format != "" {
				t.Errorf("expected empty format (PVE auto-pick), got %q", format)
			}
			// Return PVE-style volid.
			return fmt.Sprintf("local-lvm:vm-%d-disk-0", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, []int{9000}) // NextDiskVMID → 9001

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(512),                 // 512 MiB → 1 GiB
		marshal(map[string]string{}), // empty cloud_properties
		json.RawMessage(`null`),      // no vm_cid
	}, jsonrpc.Context{})

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
	t.Logf("disk_cid = %s", diskCID)
}

func TestHandleCreateDisk_CustomStorage(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + "/bosh-disk-9000", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil) // NextDiskVMID → 9000

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage": "ceph-pool"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "ceph-pool" {
		t.Errorf("expected storage %q, got %q", "ceph-pool", capturedStorage)
	}
}

// TestHandleCreateDisk_StoragePoolCloudProp verifies that cloud_properties.storage_pool
// is the highest-precedence storage selector and reaches CreateVolume.
func TestHandleCreateDisk_StoragePoolCloudProp(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	// config.DiskStorage is "local-lvm" (storageName from baseDepsForCreate);
	// storage_pool must override it.
	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_pool": "ceph-rbd", "storage": "local-lvm"}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "ceph-rbd" {
		t.Errorf("storage_pool precedence: CreateVolume received storage=%q, want %q", capturedStorage, "ceph-rbd")
	}
}

// TestHandleCreateDisk_StoragePoolAliasFallback verifies that cloud_properties.storage
// (alias) is used when storage_pool is absent, and config.DiskStorage is the final fallback.
func TestHandleCreateDisk_StoragePoolAliasFallback(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	// Only storage (alias) set, no storage_pool.
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage": "nfs-store"}),
	}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "nfs-store" {
		t.Errorf("storage alias fallback: CreateVolume received storage=%q, want %q", capturedStorage, "nfs-store")
	}
}

func TestHandleCreateDisk_DefaultStorage(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + "/bosh-disk-9000", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(2048),
		marshal(map[string]string{}), // no storage override → use config default
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != storageName {
		t.Errorf("expected default storage %q, got %q", storageName, capturedStorage)
	}
}

func TestHandleCreateDisk_CustomFormat(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, _ int, _ string) (string, error) {
			capturedFormat = format
			return "local-lvm/vol", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_format": "raw"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "raw" {
		t.Errorf("expected format %q, got %q", "raw", capturedFormat)
	}
}

func TestHandleCreateDisk_SizeCeiling(t *testing.T) {
	t.Parallel()
	// 1025 MiB → ceil → 2 GiB
	var capturedSizeGiB int
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, sizeGiB int, _ string, _ int, _ string) (string, error) {
			capturedSizeGiB = sizeGiB
			return "local-lvm/vol", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1025),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedSizeGiB != 2 {
		t.Errorf("expected sizeGiB=2 for 1025 MiB, got %d", capturedSizeGiB)
	}
}

func TestHandleCreateDisk_VMCIDIgnoredForNaming(t *testing.T) {
	t.Parallel()
	// vm_cid is NOT used for naming — each persistent disk gets its own
	// synthetic VMID from the 9xxx pool so it cannot collide with the
	// owning VM's system disk (vm-{vmcid}-disk-0).
	var capturedVMID int
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedVMID = vmid
			return fmt.Sprintf("local-lvm:vm-%d-disk-0", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil) // NextDiskVMID → 9000

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
		marshal("200"), // vm_cid = "200" — must NOT bleed into naming
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedVMID == 200 {
		t.Error("vm_cid leaked into naming VMID; persistent disks must use synthetic disk-range VMIDs")
	}
	if capturedVMID < 9000 || capturedVMID > 29999 {
		t.Errorf("expected namingVMID in [9000,29999], got %d", capturedVMID)
	}
}

func TestHandleCreateDisk_SDKError(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return "", errors.New("PVE storage unavailable")
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from CreateVolume failure, got nil")
	}
}

// When CreateVolume returns an error AND PVE partially committed the
// volume (Exists=true), create_disk must DeleteVolume so the orphan does
// not linger in storage. Verifies the best-effort cleanup branch.
func TestHandleCreateDisk_OrphanCleanupAfterCreateVolumeError(t *testing.T) {
	t.Parallel()
	var deletedVolID string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return "", errors.New("network drop after lvcreate")
		},
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
		deleteVolumeFn: func(_ context.Context, _, _, volume string) error {
			deletedVolID = volume
			return nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from CreateVolume failure, got nil")
	}
	if deletedVolID == "" {
		t.Fatal("expected DeleteVolume to be called for orphan cleanup, but it was not")
	}
}

// On a successful create_disk call, the deferred rollback must NOT fire.
// Verifies the success-flag pattern by asserting DeleteVolume is never
// invoked when CreateVolume succeeds.
func TestHandleCreateDisk_NoRollbackOnSuccess(t *testing.T) {
	t.Parallel()
	deleteCalls := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalls++
			return nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("rollback fired on success path: DeleteVolume called %d times", deleteCalls)
	}
}

func TestHandleCreateDisk_ZeroSizeMB(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(0),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for size_mb=0")
	}
}

func TestHandleCreateDisk_NegativeSizeMB(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(-512),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for negative size_mb")
	}
}

func TestHandleCreateDisk_TooFewArgs(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		// missing cloud_properties
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for too few arguments")
	}
}

// Regression: PVE returns volids in canonical "storage:name" form. The
// returned disk_cid must match that volid verbatim — re-prefixing with
// storage produces malformed CIDs like "data:data:vm-9003-disk-0" that
// break every subsequent ParseDiskCID consumer.
func TestHandleCreateDisk_DiskCIDNotDoublePrefixed(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	// Override default storage to "data" to surface the exact bug pattern.
	deps.Config.DiskStorage = "data"

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diskCID, ok := result.(string)
	if !ok {
		t.Fatalf("expected string result, got %T", result)
	}
	// The disk VMID is allocated from the synthetic disk range [9000,29999]
	// with a randomized scan start, so the exact ID is non-deterministic.
	// The regression invariant is structural: a single "data:" prefix and
	// the canonical "vm-<vmid>-disk-0" volid shape. A double-prefixed CID
	// ("data:data:vm-...") fails the Sscanf literal-prefix match.
	var vmid int
	if n, serr := fmt.Sscanf(diskCID, "data:vm-%d-disk-0", &vmid); serr != nil || n != 1 {
		t.Errorf("disk_cid = %q, want form data:vm-<vmid>-disk-0 (single prefix, no double-prefix bug)", diskCID)
	} else if vmid < 9000 || vmid > 29999 {
		t.Errorf("disk_cid = %q, vmid %d outside disk range [9000,29999]", diskCID, vmid)
	}
}

func TestHandleCreateDisk_EmptyVolidFallback(t *testing.T) {
	t.Parallel()
	// When SDK returns an empty volid, CPI constructs one from storage+name.
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, name string) (string, error) {
			return "", nil // empty volid
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

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
}

// TestHandleCreateDisk_VMIDConflictRetry verifies that CreateVolume returning
// an "already exists" error causes AllocateDiskWithRetry to pick a fresh VMID
// and re-attempt without firing the orphan-cleanup branch (a pure conflict
// means PVE rejected the volume; no partial commit can exist).
func TestHandleCreateDisk_VMIDConflictRetry(t *testing.T) {
	t.Parallel()
	attempt := 0
	existsCalls := 0
	deleteCalls := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			attempt++
			if attempt < 3 {
				body := []byte(`{"message":"volume vm-9000-disk-0 already exists","code":500}`)
				return "", sdkerrors.ParseAPIError(500, body)
			}
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			existsCalls++
			return false, nil
		},
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalls++
			return nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.VMIDAllocAttempts = 5

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(fastRetryCtx(context.Background()), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if attempt != 3 {
		t.Errorf("expected 3 CreateVolume attempts (2 conflicts + 1 success), got %d", attempt)
	}
	if existsCalls != 0 {
		t.Errorf("expected 0 Exists calls on pure conflict path, got %d", existsCalls)
	}
	if deleteCalls != 0 {
		t.Errorf("expected 0 DeleteVolume calls on pure conflict path, got %d", deleteCalls)
	}
}

func TestHandleCreateDisk_MissingNode(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "", // missing
			DiskStorage: storageName,
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

// ---------------------------------------------------------------------------
// Storage-type × formatArg tests
//
// Block storages (lvm, lvmthin, zfspool) reject qcow2; file storages (dir,
// nfs, cifs) accept it but PVE also auto-picks per storage type. When the
// caller does NOT set cloud_properties.disk_format, create_disk must pass
// format="" to CreateVolume so PVE selects the correct default for each
// storage type. When disk_format IS set, the value must be forwarded verbatim.
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_LVM_NoFormatArg — lvm block storage, no disk_format in
// cloud_properties → CreateVolume must receive format="" (PVE auto-picks raw).
func TestHandleCreateDisk_LVM_NoFormatArg(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("local-lvm:vm-%d-disk-0", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = storageName

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no disk_format
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "" {
		t.Errorf("lvm storage: expected CreateVolume format=%q (PVE auto-pick), got %q", "", capturedFormat)
	}
}

// TestHandleCreateDisk_LVMThin_NoFormatArg — lvmthin block storage, no
// disk_format → format="" forwarded so PVE picks the correct raw default.
func TestHandleCreateDisk_LVMThin_NoFormatArg(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("local-lvm-thin:vm-%d-disk-0", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = "local-lvm-thin"

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no disk_format
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "" {
		t.Errorf("lvmthin storage: expected CreateVolume format=%q (PVE auto-pick), got %q", "", capturedFormat)
	}
}

// TestHandleCreateDisk_ZFSPool_NoFormatArg — zfspool block storage, no
// disk_format → format="" forwarded so PVE picks the correct default.
func TestHandleCreateDisk_ZFSPool_NoFormatArg(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("local-zfs:vm-%d-disk-0", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = "local-zfs"

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no disk_format
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "" {
		t.Errorf("zfspool storage: expected CreateVolume format=%q (PVE auto-pick), got %q", "", capturedFormat)
	}
}

// TestHandleCreateDisk_Dir_NoFormatArg — dir (file) storage, no disk_format →
// format="" forwarded; PVE auto-picks raw for dir-type without an explicit hint.
func TestHandleCreateDisk_Dir_NoFormatArg(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			// Dir storage returns subpath volid form: storage:vmid/volname.ext
			return fmt.Sprintf("local:9001/vm-%d-disk-0.raw", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = "local"

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no disk_format
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "" {
		t.Errorf("dir storage: expected CreateVolume format=%q (PVE auto-pick), got %q", "", capturedFormat)
	}
}

// TestHandleCreateDisk_Dir_ExplicitFormat_Forwarded — dir storage with
// cloud_properties.disk_format=qcow2 → format="qcow2" forwarded verbatim to
// CreateVolume. Verifies the explicit-format forwarding path for file storages.
func TestHandleCreateDisk_Dir_ExplicitFormat_Forwarded(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("local:9001/vm-%d-disk-0.qcow2", vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = "local"

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_format": "qcow2"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "qcow2" {
		t.Errorf("dir storage with explicit disk_format: expected CreateVolume format=%q, got %q", "qcow2", capturedFormat)
	}
}

// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a nfs pool via env.
//
// func TestHandleCreateDisk_NFS_NoFormatArg(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a rbd pool via env.
//
// func TestHandleCreateDisk_RBD_NoFormatArg(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0. Re-enable when
// integration-test harness provides a cephfs pool via env.
//
// func TestHandleCreateDisk_CephFS_NoFormatArg(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2. Re-enable when
// integration-test harness provides a cifs pool via env.
//
// func TestHandleCreateDisk_CIFS_NoFormatArg(t *testing.T) { ... }

// ---------------------------------------------------------------------------
// Auth-failure test
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_AuthFailure verifies that a 401 Unauthorized error from
// the storage CreateVolume call is classified as a non-retriable Cloud error.
// Auth failures are operator configuration issues (wrong API token or expired
// ticket) and must surface immediately — BOSH must not retry indefinitely.
func TestHandleCreateDisk_AuthFailure(t *testing.T) {
	t.Parallel()
	authErr := &sdkerrors.APIError{HTTPCode: 401, Message: "authentication failure"}

	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return "", authErr
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from 401 auth failure")
	}

	// 401 is a 4xx non-404 → WrapError returns a non-retriable Cloud error.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		// The error may not be wrapped as a CPI error if the handler does not
		// call WrapError on storage errors; surface the raw error for diagnosis.
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("auth failure must not be retriable; OkToRetry()=true; type=%s", cpiErr.Type())
	}
	if cpiErr.Type() == cpierrors.TypeRetriableCloud {
		t.Errorf("auth failure classified as RetriableCloud; want non-retriable TypeCloud; type=%s", cpiErr.Type())
	}
}
