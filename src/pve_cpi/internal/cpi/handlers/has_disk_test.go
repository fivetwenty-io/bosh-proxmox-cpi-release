package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// ---------------------------------------------------------------------------
// hasDisk-local backend doubles (file-local, prefixed hasDisk).
// These are separate from the localBackend in create_stemcell_test.go to avoid
// coupling unrelated test files. Each type satisfies pve.Backend and
// pve.BackendResolver.
// ---------------------------------------------------------------------------

// hasDiskBackend is a configurable pve.Backend double for has_disk tests.
// nodeForExistingFn controls the response to NodeForExisting calls so tests
// can inject DiskNotFound, generic errors, or a happy-path node name.
type hasDiskBackend struct {
	nodeForExistingFn func(ctx context.Context, volume string) (string, error)
}

func (b *hasDiskBackend) Kind() pve.BackendKind { return pve.BackendLocal }

func (b *hasDiskBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return testNode, nil
}

func (b *hasDiskBackend) NodeForExisting(ctx context.Context, volume string) (string, error) {
	if b.nodeForExistingFn != nil {
		return b.nodeForExistingFn(ctx, volume)
	}
	return testNode, nil
}

// hasDiskResolver wraps a fixed hasDiskBackend and satisfies pve.BackendResolver.
type hasDiskResolver struct {
	backend pve.Backend
}

func (r *hasDiskResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return r.backend, nil
}

// hasDiskErrorResolver always returns an error from Resolve, exercising the
// backend-resolution-failure path in HandleHasDisk.
type hasDiskErrorResolver struct {
	err error
}

func (r *hasDiskErrorResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return nil, r.err
}

// Compile-time interface checks.
var _ pve.Backend = (*hasDiskBackend)(nil)
var _ pve.BackendResolver = (*hasDiskResolver)(nil)
var _ pve.BackendResolver = (*hasDiskErrorResolver)(nil)

// baseDepsForHas builds Deps for has_disk tests.
func baseDepsForHas(t *testing.T, storageSvc *mockStorageService) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// has_disk tests
// ---------------------------------------------------------------------------

func TestHandleHasDisk_Exists(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, node, storage, volume string) (bool, error) {
			if node != testNode {
				t.Errorf("unexpected node %q", node)
			}
			if storage != storageName {
				t.Errorf("unexpected storage %q", storage)
			}
			if volume != "local-lvm:vm-9001-disk-0" {
				t.Errorf("unexpected volume %q", volume)
			}
			return true, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestHandleHasDisk_NotExists(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9999-disk-0", nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if exists {
		t.Error("expected exists=false")
	}
}

func TestHandleHasDisk_SDKNotFoundError_ReturnsFalse(t *testing.T) {
	t.Parallel()
	// If SDK Exists returns a not-found error (unusual but defensive), CPI returns false.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, &sdkerrors.APIError{
				Message:  "volume not found",
				HTTPCode: 404,
			}
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for not-found, got: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if exists {
		t.Error("expected exists=false for not-found error")
	}
}

func TestHandleHasDisk_SDKError_Propagated(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, errors.New("storage backend unavailable")
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error to be propagated from Exists failure")
	}
}

// TestHandleHasDisk_TransientSDKError_IsRetriable pins the retriability
// contract for has_disk: a pvedaemon worker recycle (HTTP 5xx/596) during
// the Exists check must reach the Director as a retriable error, not a
// permanent CloudError — has_disk previously had no transient absorption at
// all on this path.
func TestHandleHasDisk_TransientSDKError_IsRetriable(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			// ParseAPIError sets the 5xx sentinel so errors.Is(err,
			// sdkerrors.ErrServer) classifies it as transient.
			return false, sdkerrors.ParseAPIError(596, []byte(`{"message":"connection close"}`))
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(fastRetryCtx(context.Background()), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from persistent transient failure")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("5xx from Exists must be retriable, got %T %v", err, err)
	}
}

func TestHandleHasDisk_MalformedCID_NoColon(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("malformed-disk-cid"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for malformed disk_cid with no colon")
	}
}

func TestHandleHasDisk_EmptyCID(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(""),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

func TestHandleHasDisk_TooFewArgs(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing disk_cid argument")
	}
}

func TestHandleHasDisk_MissingNode(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "",
			DiskStorage: storageName,
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestHandleHasDisk_EmptyStoragePart(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(":volume"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty storage part")
	}
}

func TestHandleHasDisk_EmptyVolumePart(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("storage:"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty volume part")
	}
}

// ---------------------------------------------------------------------------
// New tests: backend-resolution error paths (gaps #4, #5, #6 from R1).
// ---------------------------------------------------------------------------

// TestHandleHasDisk_NodeForExistingDiskNotFound verifies that when the backend's
// NodeForExisting returns a DiskNotFound error (the sentinel produced by
// LocalBackend when no node holds the volume), the handler returns false, nil
// rather than propagating the error. This mirrors the impl branch at has_disk.go:67-71.
func TestHandleHasDisk_NodeForExistingDiskNotFound(t *testing.T) {
	t.Parallel()
	diskNotFound := cpierrors.DiskNotFound("local-lvm:vm-9001-disk-0")

	backend := &hasDiskBackend{
		nodeForExistingFn: func(_ context.Context, _ string) (string, error) {
			return "", diskNotFound
		},
	}
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			t.Error("Storage.Exists must not be called when NodeForExisting returns DiskNotFound")
			return false, nil
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:      newHandlerMockClient(storageSvc, nil),
		Logger:   log.NewNopLogger(),
		Resolver: &hasDiskResolver{backend: backend},
	}

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error when NodeForExisting returns DiskNotFound, got: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if exists {
		t.Error("expected exists=false when NodeForExisting returns DiskNotFound")
	}
}

// TestHandleHasDisk_NodeForExistingOtherError verifies that when NodeForExisting
// returns a non-not-found error (e.g., a cluster API failure), the handler
// propagates the error to the caller rather than swallowing it.
func TestHandleHasDisk_NodeForExistingOtherError(t *testing.T) {
	t.Parallel()
	clusterErr := errors.New("cluster API unavailable")

	backend := &hasDiskBackend{
		nodeForExistingFn: func(_ context.Context, _ string) (string, error) {
			return "", clusterErr
		},
	}
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			t.Error("Storage.Exists must not be called when NodeForExisting returns an error")
			return false, nil
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:      newHandlerMockClient(storageSvc, nil),
		Logger:   log.NewNopLogger(),
		Resolver: &hasDiskResolver{backend: backend},
	}

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error to be propagated from NodeForExisting failure")
	}
}

// TestHandleHasDisk_BackendResolveError verifies that when deps.Resolver.Resolve
// returns an error, the handler propagates it. This exercises the resolution
// failure path at has_disk.go:61-64.
func TestHandleHasDisk_BackendResolveError(t *testing.T) {
	t.Parallel()
	resolveErr := errors.New("storage info cache miss")

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			t.Error("Storage.Exists must not be called when Resolve returns an error")
			return false, nil
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        testNode,
			DiskStorage: storageName,
		},
		PVE:      newHandlerMockClient(storageSvc, nil),
		Logger:   log.NewNopLogger(),
		Resolver: &hasDiskErrorResolver{err: resolveErr},
	}

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, "local-lvm:vm-9001-disk-0", nil)),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error to be propagated from Resolve failure")
	}
}

// ---------------------------------------------------------------------------
// New tests: local storage-type CID variants (gap #16 from R1).
//
// has_disk has no storage-type branching — the handler is CID-format-agnostic
// after ParseDiskCID splits on the first colon. These tests confirm ParseDiskCID
// handles each volume-string shape and that the rest of the handler path
// completes successfully. All use the default static resolver (Resolver: nil)
// so NodeForExisting returns testNode unconditionally and Storage.Exists is
// the only controlled outcome.
// ---------------------------------------------------------------------------

// TestHandleHasDisk_Dir_CID exercises the dir-storage volume format
// (storage:vmid/volname.ext). ParseDiskCID must split on the first colon only,
// yielding storage="local" and volume="9001/vm-9001-disk-0.raw".
func TestHandleHasDisk_Dir_CID(t *testing.T) {
	t.Parallel()
	const cid = "local:9001/vm-9001-disk-0.raw"

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, node, storage, volume string) (bool, error) {
			if node != testNode {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local" {
				t.Errorf("expected storage \"local\", got %q", storage)
			}
			if volume != cid {
				t.Errorf("expected volume %q, got %q", cid, volume)
			}
			return true, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, cid, nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !exists {
		t.Error("expected exists=true for dir-storage CID")
	}
}

// TestHandleHasDisk_ZFSPool_CID exercises the ZFS pool volume format
// (storage:volname — bare name, no subpath). ParseDiskCID yields
// storage="local-zfs", volume="vm-9001-disk-0".
func TestHandleHasDisk_ZFSPool_CID(t *testing.T) {
	t.Parallel()
	const cid = "local-zfs:vm-9001-disk-0"

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, node, storage, volume string) (bool, error) {
			if node != testNode {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local-zfs" {
				t.Errorf("expected storage \"local-zfs\", got %q", storage)
			}
			if volume != cid {
				t.Errorf("expected volume %q, got %q", cid, volume)
			}
			return true, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, cid, nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !exists {
		t.Error("expected exists=true for ZFS pool CID")
	}
}

// TestHandleHasDisk_LVMThin_CID exercises the LVM-thin volume format
// (storage:volname — same bare-name shape as lvm and zfspool). ParseDiskCID
// yields storage="local-lvm-thin", volume="vm-9001-disk-0".
func TestHandleHasDisk_LVMThin_CID(t *testing.T) {
	t.Parallel()
	const cid = "local-lvm-thin:vm-9001-disk-0"

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, node, storage, volume string) (bool, error) {
			if node != testNode {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local-lvm-thin" {
				t.Errorf("expected storage \"local-lvm-thin\", got %q", storage)
			}
			if volume != cid {
				t.Errorf("expected volume %q, got %q", cid, volume)
			}
			return true, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(mustEncodeDiskCID(t, cid, nil)),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !exists {
		t.Error("expected exists=true for LVM-thin CID")
	}
}

// ---------------------------------------------------------------------------
// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2.
// Re-enable when integration-test harness provides a nfs pool via env.
//
// func TestHandleHasDisk_NFS_CID(t *testing.T) { ... }

// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0.
// Re-enable when integration-test harness provides a rbd pool via env.
//
// func TestHandleHasDisk_RBD_CID(t *testing.T) { ... }

// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0.
// Re-enable when integration-test harness provides a cephfs pool via env.
//
// func TestHandleHasDisk_CephFS_CID(t *testing.T) { ... }

// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2.
// Re-enable when integration-test harness provides a cifs pool via env.
//
// func TestHandleHasDisk_CIFS_CID(t *testing.T) { ... }
