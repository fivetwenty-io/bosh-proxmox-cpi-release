package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// Shared mock infrastructure (handlers_test package scope)
// ---------------------------------------------------------------------------

// mockStorageService lets individual tests wire CreateVolume, DeleteVolume,
// Exists, DeleteVolumeIfExists, and Upload with function literals.
// Methods not set are no-ops or return zero values.
type mockStorageService struct {
	createVolumeFn         func(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error)
	deleteVolumeFn         func(ctx context.Context, node, storage, volume string) error
	existsFn               func(ctx context.Context, node, storage, volume string) (bool, error)
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
	uploadFn               func(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error)
}

func (m *mockStorageService) CreateVolume(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
	if m.createVolumeFn != nil {
		return m.createVolumeFn(ctx, node, storage, sizeGiB, format, vmid, name)
	}
	return fmt.Sprintf("%s/%s", storage, name), nil
}

func (m *mockStorageService) DeleteVolume(ctx context.Context, node, storage, volume string) error {
	if m.deleteVolumeFn != nil {
		return m.deleteVolumeFn(ctx, node, storage, volume)
	}
	return nil
}

func (m *mockStorageService) DeleteVolumeAsync(ctx context.Context, node, storage, volume string) (string, error) {
	if err := m.DeleteVolume(ctx, node, storage, volume); err != nil {
		return "", err
	}
	return "", nil
}

func (m *mockStorageService) Exists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.existsFn != nil {
		return m.existsFn(ctx, node, storage, volume)
	}
	return false, nil
}

func (m *mockStorageService) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	return false, nil
}

func (m *mockStorageService) DeleteVolumeIfExistsAsync(ctx context.Context, node, storage, volume string) (bool, string, error) {
	existed, err := m.DeleteVolumeIfExists(ctx, node, storage, volume)
	if err != nil {
		return false, "", err
	}
	return existed, "", nil
}

func (m *mockStorageService) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, node, storage, content, filename, body)
	}
	return "", nil
}

// mockClusterServiceForHandlers implements cluster.Service with a configurable ListResources.
type mockClusterServiceForHandlers struct {
	sdkclusterapi.Service
	listFn func(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error)
}

func (m *mockClusterServiceForHandlers) ListResources(ctx context.Context, params *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
	if m.listFn != nil {
		return m.listFn(ctx, params)
	}
	resp := sdkclusterapi.ListResourcesResponse{}
	return &resp, nil
}

// handlerMockClient wires storage and cluster services for handler tests.
type handlerMockClient struct {
	storageSvc sdkstorage.Service
	clusterSvc sdkcluster.Service
}

func (c *handlerMockClient) QEMU() sdkqemu.Service                     { return nil }
func (c *handlerMockClient) Storage() sdkstorage.Service               { return c.storageSvc }
func (c *handlerMockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *handlerMockClient) Tasks() sdktasks.Service                   { return nil }
func (c *handlerMockClient) Nodes() sdknodes.Service                   { return nil }
func (c *handlerMockClient) Cluster() sdkcluster.Service               { return c.clusterSvc }
func (c *handlerMockClient) ClusterStorage() sdkclusterstorage.Service { return nil }

// newHandlerMockClient builds a mock pve.Client with wired storage and cluster.
// clusterVMIDs is the set of VMIDs reported by the cluster (for NextDiskVMID).
func newHandlerMockClient(storageSvc *mockStorageService, clusterVMIDs []int) pve.Client {
	listFn := func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
		resp := make(sdkclusterapi.ListResourcesResponse, 0, len(clusterVMIDs))
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
	return &handlerMockClient{
		storageSvc: storageSvc,
		clusterSvc: &mockClusterServiceForHandlers{listFn: listFn},
	}
}

// baseDepsForCreate builds a Deps suitable for create_disk tests.
func baseDepsForCreate(t *testing.T, storageSvc *mockStorageService, clusterVMIDs []int) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:         "pve1",
			DiskStorage:  "local-lvm",
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
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
			if node != "pve1" {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local-lvm" {
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

func TestHandleCreateDisk_DefaultStorage(t *testing.T) {
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
	if capturedStorage != "local-lvm" {
		t.Errorf("expected default storage %q, got %q", "local-lvm", capturedStorage)
	}
}

func TestHandleCreateDisk_CustomFormat(t *testing.T) {
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
		t.Error("vm_cid leaked into naming VMID; persistent disks must use synthetic 9xxx VMIDs")
	}
	if capturedVMID < 9000 || capturedVMID > 9999 {
		t.Errorf("expected namingVMID in [9000,9999], got %d", capturedVMID)
	}
}

func TestHandleCreateDisk_SDKError(t *testing.T) {
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
	// The disk VMID is allocated from the synthetic disk range [9000,9999]
	// with a randomized scan start, so the exact ID is non-deterministic.
	// The regression invariant is structural: a single "data:" prefix and
	// the canonical "vm-<vmid>-disk-0" volid shape. A double-prefixed CID
	// ("data:data:vm-...") fails the Sscanf literal-prefix match.
	var vmid int
	if n, serr := fmt.Sscanf(diskCID, "data:vm-%d-disk-0", &vmid); serr != nil || n != 1 {
		t.Errorf("disk_cid = %q, want form data:vm-<vmid>-disk-0 (single prefix, no double-prefix bug)", diskCID)
	} else if vmid < 9000 || vmid > 9999 {
		t.Errorf("disk_cid = %q, vmid %d outside disk range [9000,9999]", diskCID, vmid)
	}
}

func TestHandleCreateDisk_EmptyVolidFallback(t *testing.T) {
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
	_, err := h.Handle(context.Background(), []json.RawMessage{
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
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "", // missing
			DiskStorage: "local-lvm",
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
