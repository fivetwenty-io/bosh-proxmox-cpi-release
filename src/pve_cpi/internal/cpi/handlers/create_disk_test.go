package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// The regression invariant is structural: after unwrapping the pvd
	// envelope, a single "data:" prefix and the canonical "vm-<vmid>-disk-0"
	// volid shape. A double-prefixed bare CID ("data:data:vm-...") fails the
	// Sscanf literal-prefix match.
	bareCID, _, perr := pve.ParseEncodedDiskCID(diskCID)
	if perr != nil {
		t.Fatalf("ParseEncodedDiskCID(%q): %v", diskCID, perr)
	}
	var vmid int
	if n, serr := fmt.Sscanf(bareCID, "data:vm-%d-disk-0", &vmid); serr != nil || n != 1 {
		t.Errorf("bare disk_cid = %q, want form data:vm-<vmid>-disk-0 (single prefix, no double-prefix bug)", bareCID)
	} else if vmid < 9000 || vmid > 29999 {
		t.Errorf("bare disk_cid = %q, vmid %d outside disk range [9000,29999]", bareCID, vmid)
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

// TestHandleCreateDisk_StorageLockBudget_DefaultFallback verifies that when no
// retry.storage_lock.max_attempts is set in config, the create_disk lock-retry
// loop uses pve.DefaultStorageLockMaxAttempts (10) as its budget. We simulate
// a storage-lock error on every attempt and confirm exactly 10 attempts are
// made before the handler returns an error.
func TestHandleCreateDisk_StorageLockBudget_DefaultFallback(t *testing.T) {
	t.Parallel()
	attempts := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			attempts++
			// Return a storage-lock timeout on every attempt.
			return "", errors.New("can't lock file 'storage' - got timeout")
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	// No retry.storage_lock config set; must fall back to DefaultStorageLockMaxAttempts=10.
	if deps.Config.Retry != nil {
		t.Fatal("baseDepsForCreate must not set a Retry block for this test")
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(fastRetryCtx(context.Background()), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error after all attempts exhausted, got nil")
	}
	if attempts != pve.DefaultStorageLockMaxAttempts {
		t.Errorf("expected %d attempts (DefaultStorageLockMaxAttempts), got %d",
			pve.DefaultStorageLockMaxAttempts, attempts)
	}
}

// TestHandleCreateDisk_StorageLockBudget_ConfigOverride verifies that setting
// retry.storage_lock.max_attempts in config overrides the default budget.
func TestHandleCreateDisk_StorageLockBudget_ConfigOverride(t *testing.T) {
	t.Parallel()
	attempts := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			attempts++
			return "", errors.New("can't lock file 'storage' - got timeout")
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.Retry = &config.RetryConfig{
		StorageLock: &config.RetryPolicy{MaxAttempts: 3},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(fastRetryCtx(context.Background()), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error after lock exhaustion, got nil")
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (config override), got %d", attempts)
	}
}

// TestHandleCreateDisk_StorageLockBudget_StorageImportFallback verifies that
// when retry.storage_lock is unset but retry.storage_import.max_attempts is
// set, the storage_import value is used as the lock-budget fallback (preserves
// pre-storage_lock deployments).
func TestHandleCreateDisk_StorageLockBudget_StorageImportFallback(t *testing.T) {
	t.Parallel()
	attempts := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			attempts++
			return "", errors.New("can't lock file 'storage' - got timeout")
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.Retry = &config.RetryConfig{
		// storage_lock unset (nil) — must fall back to storage_import.
		StorageImport: &config.RetryPolicy{MaxAttempts: 4},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(fastRetryCtx(context.Background()), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error after lock exhaustion, got nil")
	}
	if attempts != 4 {
		t.Errorf("expected 4 attempts (storage_import fallback), got %d", attempts)
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
// ---------------------------------------------------------------------------
// AvailabilityZone → disk CID metadata wiring tests
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_AZ_InCIDMeta exercises the full production path:
// HandleCreateDisk receives cloud_properties.availability_zone, passes it
// through attemptCreateVolume, and the returned disk CID decodes to meta.AZ
// equal to the supplied zone. Also asserts meta.Pool matches the resolved
// storage. Fails if any link in the chain is severed.
func TestHandleCreateDisk_AZ_InCIDMeta(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{
			"availability_zone": "zone-a",
		}),
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
		t.Fatal("meta is nil; AvailabilityZone not encoded into disk CID — wiring broken")
	}
	if meta.AZ != "zone-a" {
		t.Errorf("meta.AZ = %q; want %q — cloud_properties.availability_zone not propagated through HandleCreateDisk → attemptCreateVolume → EncodeDiskCID", meta.AZ, "zone-a")
	}
	if meta.Pool != storageName {
		t.Errorf("meta.Pool = %q; want %q", meta.Pool, storageName)
	}
}

// TestHandleCreateDisk_NoAZ_MetaAZEmpty verifies that when
// cloud_properties.availability_zone is absent, the returned disk CID has
// meta.AZ == "" (backward-compatible; no AZ constraint imposed on create_vm).
func TestHandleCreateDisk_NoAZ_MetaAZEmpty(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no availability_zone
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
	// meta may be nil (bare CID) or non-nil but AZ must be empty.
	if meta != nil && meta.AZ != "" {
		t.Errorf("meta.AZ = %q; want empty string when cloud_properties.availability_zone not set", meta.AZ)
	}
}

// ---------------------------------------------------------------------------
// Layered resolver handler-level tests
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_DiskTypeProfileSelectsPool verifies that a disk_type
// selector in cloud_properties resolves storage_pool from the named profile
// when no explicit storage_pool is given in the call.
func TestHandleCreateDisk_DiskTypeProfileSelectsPool(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	// Add a disk_type profile that supplies storage_pool.
	deps.Config.DiskTypes = map[string]config.TypeProfile{
		"fast-disk": {
			CloudProperties: map[string]any{
				"storage_pool": "ssd-pool",
			},
		},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_type": "fast-disk"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "ssd-pool" {
		t.Errorf("disk_type profile storage_pool: CreateVolume received storage=%q, want %q", capturedStorage, "ssd-pool")
	}
}

// TestHandleCreateDisk_VMTypeProfileSelectsPool verifies that a vm_type
// selector in cloud_properties resolves storage_pool from the named profile
// when no explicit storage_pool and no disk_type are given.
func TestHandleCreateDisk_VMTypeProfileSelectsPool(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.VMTypes = map[string]config.TypeProfile{
		"large": {
			CloudProperties: map[string]any{
				"storage_pool": "hdd-pool",
			},
		},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"vm_type": "large"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "hdd-pool" {
		t.Errorf("vm_type profile storage_pool: CreateVolume received storage=%q, want %q", capturedStorage, "hdd-pool")
	}
}

// TestHandleCreateDisk_ExplicitStoragePoolBeatsProfile verifies that an
// explicit storage_pool in cloud_properties beats any profile-supplied value.
func TestHandleCreateDisk_ExplicitStoragePoolBeatsProfile(t *testing.T) {
	t.Parallel()
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskTypes = map[string]config.TypeProfile{
		"fast-disk": {
			CloudProperties: map[string]any{
				"storage_pool": "ssd-pool",
			},
		},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		// explicit storage_pool must win over disk_type profile
		marshal(map[string]string{"storage_pool": "call-explicit", "disk_type": "fast-disk"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "call-explicit" {
		t.Errorf("explicit storage_pool beats profile: CreateVolume received storage=%q, want call-explicit", capturedStorage)
	}
}

// TestHandleCreateDisk_DiskFormatFromProfile verifies that disk_format supplied
// by a disk_type profile is forwarded to CreateVolume as formatArg.
func TestHandleCreateDisk_DiskFormatFromProfile(t *testing.T) {
	t.Parallel()
	var capturedFormat string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, _ string) (string, error) {
			capturedFormat = format
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskTypes = map[string]config.TypeProfile{
		"raw-disk": {
			CloudProperties: map[string]any{
				"storage_pool": storageName,
				"disk_format":  "raw",
			},
		},
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_type": "raw-disk"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFormat != "raw" {
		t.Errorf("disk_format from profile: CreateVolume received format=%q, want raw", capturedFormat)
	}
}

// TestHandleCreateDisk_UnknownDiskTypeSelector verifies that an unknown
// disk_type selector causes a non-retriable CloudError before any PVE call.
func TestHandleCreateDisk_UnknownDiskTypeSelector(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			t.Error("CreateVolume must not be called when selector resolution fails")
			return "", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	// DiskTypes map is empty; any disk_type selector is unknown.

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"disk_type": "nonexistent"}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected CloudError for unknown disk_type selector, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("unknown selector error must not be retriable")
	}
}

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

// ---------------------------------------------------------------------------
// Per-disk performance options → disk CID metadata (§7.8 layered resolver)
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_PerfOpts_InCIDMeta verifies that cloud_properties perf
// options are resolved and encoded into the returned disk CID metadata. The
// test round-trips through ParseEncodedDiskCID to assert meta.Opts content.
func TestHandleCreateDisk_PerfOpts_InCIDMeta(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{
			"storage_pool": storageName,
			"iothread":     true,
			"cache":        "writeback",
			"mbps_rd":      100,
		}),
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
		t.Fatal("meta is nil; perf opts not encoded into disk CID")
	}
	wantOpts := map[string]string{
		"iothread": "1",
		"cache":    "writeback",
		"mbps_rd":  "100",
	}
	for k, wantV := range wantOpts {
		if gotV := meta.Opts[k]; gotV != wantV {
			t.Errorf("meta.Opts[%q] = %q; want %q", k, gotV, wantV)
		}
	}
	// No extra keys beyond the three supplied.
	if len(meta.Opts) != len(wantOpts) {
		t.Errorf("meta.Opts has %d keys, want %d: %v", len(meta.Opts), len(wantOpts), meta.Opts)
	}
}

// TestHandleCreateDisk_NoPerfOpts_ByteIdenticalCID verifies that when no perf
// options are supplied, the returned disk CID is byte-identical to one produced
// without any Opts field — preserving backward compatibility.
// TestHandleCreateDisk_NoPerfOpts_CurrentDefaultBakesIothread verifies that a
// create_disk call with no explicit perf opts bakes the Phase 2 default
// (iothread=1) into the disk CID's recorded Opts — replacing the pre-Phase-2
// "byte-identical, no options" assertion.
func TestHandleCreateDisk_NoPerfOpts_CurrentDefaultBakesIothread(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}), // no perf opts
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
	if meta == nil || len(meta.Opts) != 1 || meta.Opts["iothread"] != "1" {
		got := any(nil)
		if meta != nil {
			got = meta.Opts
		}
		t.Errorf("meta.Opts = %v; want map[iothread:1] (Phase 2 default)", got)
	}
}

// TestHandleCreateDisk_ExplicitOptOut_ByteIdenticalCID verifies that an
// explicit cloud_properties.iothread:false restores the exact pre-Phase-2
// bare CID shape (no recorded Opts at all).
func TestHandleCreateDisk_ExplicitOptOut_ByteIdenticalCID(t *testing.T) {
	t.Parallel()
	var capturedVMID int
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			capturedVMID = vmid
			capturedStorage = storage
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{"iothread": false}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	diskCID, ok := result.(string)
	if !ok || diskCID == "" {
		t.Fatalf("expected non-empty string result, got %T %v", result, result)
	}

	// Construct the expected CID as EncodeDiskCID would without Opts.
	bareCID := fmt.Sprintf("%s:vm-%d-disk-0", capturedStorage, capturedVMID)
	expectedCID, encErr := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{
		Pool: capturedStorage,
		Node: deps.Config.Node,
	})
	if encErr != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bareCID, encErr)
	}
	if diskCID != expectedCID {
		t.Errorf("explicit-opt-out CID not byte-identical to pre-Phase-2 form:\n  got  = %q\n  want = %q", diskCID, expectedCID)
	}

	_, meta, parseErr := pve.ParseEncodedDiskCID(diskCID)
	if parseErr != nil {
		t.Fatalf("ParseEncodedDiskCID(%q): %v", diskCID, parseErr)
	}
	// meta.Opts must be nil or empty with the default explicitly disabled.
	if meta != nil && len(meta.Opts) != 0 {
		t.Errorf("meta.Opts = %v; want nil/empty with explicit iothread:false", meta.Opts)
	}
}

// TestHandleCreateDisk_BadCacheMode_CloudError verifies that an invalid cache
// mode in cloud_properties causes a non-retriable CloudError before any PVE
// call is made. resolveDiskPerfOptions validates cache values.
func TestHandleCreateDisk_BadCacheMode_CloudError(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			t.Error("CreateVolume must not be called when perf option validation fails")
			return "", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{
			"cache": "bogus",
		}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected CloudError for invalid cache mode, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("bad cache mode error must not be retriable")
	}
}

// TestHandleCreateDisk_Aio_InCIDMeta verifies that cloud_properties.aio is
// resolved and baked into the returned disk CID's recorded Opts, alongside
// the existing per-disk performance options.
func TestHandleCreateDisk_Aio_InCIDMeta(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{
			"storage_pool": storageName,
			"aio":          "native",
			"cache":        "none",
		}),
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
		t.Fatal("meta is nil; aio option not encoded into disk CID")
	}
	if got := meta.Opts["aio"]; got != "native" {
		t.Errorf("meta.Opts[\"aio\"] = %q; want %q", got, "native")
	}
}

// TestHandleCreateDisk_BadAioMode_CloudError verifies that an invalid aio
// value in cloud_properties causes a non-retriable CloudError before any PVE
// call is made, matching the existing cache validation behavior.
func TestHandleCreateDisk_BadAioMode_CloudError(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			t.Error("CreateVolume must not be called when perf option validation fails")
			return "", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{
			"aio": "bogus",
		}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected CloudError for invalid aio mode, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("bad aio mode error must not be retriable")
	}
}

// ---------------------------------------------------------------------------
// Emitted CID length warning
// ---------------------------------------------------------------------------

// TestHandleCreateDisk_HardErrorsWhenCIDExceeds255WithCompressionOff verifies
// that create_disk returns a hard, non-retriable error — not a warning — when
// the emitted pvd envelope CID would exceed 255 characters and
// disk_cid_compression is off. MySQL-backed Directors store disk_cid in a
// VARCHAR(255) column; silently emitting an overflowing CID would truncate or
// be rejected on a later write and orphan the volume, so the volume that was
// just created must also be rolled back (DeleteVolumeAsync called) rather than
// leaked.
func TestHandleCreateDisk_HardErrorsWhenCIDExceeds255WithCompressionOff(t *testing.T) {
	t.Parallel()
	longStorage := strings.Repeat("s", 220)
	var deletedVolume string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, volume string) (string, error) {
			deletedVolume = volume
			return "", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = longStorage

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected a hard error for an over-255-character disk CID with compression off")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud (non-retriable config problem), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("error should mention the 255-character limit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "disk_cid_compression") {
		t.Errorf("error should name the disk_cid_compression remediation, got: %v", err)
	}
	if deletedVolume == "" {
		t.Error("expected the just-created volume to be rolled back (DeleteVolumeAsync called), got none")
	}
}

// TestHandleCreateDisk_NoLengthWarnForTypicalCID proves the warning stays
// silent for a common-case CID.
func TestHandleCreateDisk_NoLengthWarnForTypicalCID(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}
	deps.Logger = logger

	h := handlers.HandleCreateDisk(deps)
	if _, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "disk CID exceeds 255 characters") {
		t.Errorf("unexpected length warning for typical CID: %s", buf.String())
	}
}

// TestHandleCreateDisk_CompressionEmitsPvzOver255: with disk_cid_compression
// enabled, a CID whose pvd- form overflows 255 characters is emitted as a
// pvz- envelope that fits the varchar(255) column, round-trips its metadata,
// and does not trigger the length warning.
func TestHandleCreateDisk_CompressionEmitsPvzOver255(t *testing.T) {
	t.Parallel()
	longStorage := strings.Repeat("s", 220)
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = longStorage
	deps.Config.DiskCIDCompression = true

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}
	deps.Logger = logger

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
	if !strings.HasPrefix(diskCID, "pvz-") {
		t.Fatalf("expected pvz- CID, got %q", diskCID)
	}
	if len(diskCID) > 255 {
		t.Errorf("compressed CID length %d exceeds 255", len(diskCID))
	}
	bare, meta, perr := pve.ParseEncodedDiskCID(diskCID)
	if perr != nil {
		t.Fatalf("emitted CID does not decode: %v", perr)
	}
	if !strings.HasPrefix(bare, longStorage+":") {
		t.Errorf("bare volid %q does not start with the storage prefix", bare)
	}
	if meta == nil || meta.Pool != longStorage {
		t.Errorf("meta.Pool: want the resolved storage, got %+v", meta)
	}
	if strings.Contains(buf.String(), "disk CID exceeds 255 characters") {
		t.Errorf("unexpected length warning for a CID that fits: %s", buf.String())
	}
}

// TestHandleCreateDisk_CompressionSmallCIDStaysPvd: the flag must not touch
// CIDs that already fit — the common case stays pvd- and byte-identical to
// the flag-off encoding.
func TestHandleCreateDisk_CompressionSmallCIDStaysPvd(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskCIDCompression = true

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
	if !strings.HasPrefix(diskCID, "pvd-") {
		t.Errorf("small CID must stay pvd-, got %q", diskCID)
	}
}

// TestHandleCreateDisk_CompressionHardErrorsWhenStillOver255: gzip gains
// little on a high-entropy payload, so even the compressed form still
// overflows 255 characters. This must be a hard error — same as the
// compression-off case — with rollback of the just-created volume;
// compression is best-effort, not a guarantee, and a still-overflowing CID is
// exactly as unsafe as the uncompressed case.
func TestHandleCreateDisk_CompressionHardErrorsWhenStillOver255(t *testing.T) {
	t.Parallel()
	entropy := "Zq3xK9mWp2Lr8vTn5cYd1Bf7Hs4Ej6Ug0QaXwOiNkMzRlPyAoJhFbCtDeSvGxIu" +
		"VrEw2nT8mK5pL3qZ9dX7cB1fY4hS6jE0gU5aQ8wO2iN4kM6zR1lP3yA7oJ9hF2bC" +
		"Wm8sD4vN0xQ6tR2yU9aE5cI1oP7kL3gZjHfXbSnTdMwVrCqYeAiOuKgJhBzNvDx"
	var deletedVolume string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, volume string) (string, error) {
			deletedVolume = volume
			return "", nil
		},
	}
	deps := baseDepsForCreate(t, storageSvc, nil)
	deps.Config.DiskStorage = entropy
	deps.Config.DiskCIDCompression = true

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected a hard error for a still-over-255 CID even with compression on")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("error type: want TypeCloud (non-retriable config problem), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("error should mention the 255-character limit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "compression") {
		t.Errorf("error should acknowledge compression was already applied, got: %v", err)
	}
	if deletedVolume == "" {
		t.Error("expected the just-created volume to be rolled back (DeleteVolumeAsync called), got none")
	}
}
