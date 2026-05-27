package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
)

// ============================================================
// Tests: HandleDeleteStemcell
// ============================================================

// deleteStemcellMockStorage implements sdkstorage.Service for delete_stemcell tests.
// Only DeleteVolumeIfExists is wired; all other methods panic on accidental call.
type deleteStemcellMockStorage struct {
	sdkstorage.Service        // nil embed — panics on unhandled calls
	deleteVolumeIfExistsFn    func(ctx context.Context, node, storage, volume string) (bool, error)
	deleteVolumeIfExistsCalls int
}

func (m *deleteStemcellMockStorage) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	m.deleteVolumeIfExistsCalls++
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	// Default: volume exists and delete succeeds.
	return true, nil
}

// Upload is overridden to prevent nil-embed panic in test clients that wire
// this storage mock into a stemcellMockClient (which wires storageSvc).
func (m *deleteStemcellMockStorage) Upload(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
	panic("deleteStemcellMockStorage.Upload: not expected in delete_stemcell tests")
}

// buildDeleteStemcellDeps constructs Deps for delete_stemcell tests using the
// provided storage mock. Node and StemcellStorage are pre-set.
func buildDeleteStemcellDeps(storageSvc sdkstorage.Service) handlers.Deps {
	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   &stemcellMockNodes{},
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: &stemcellMockCluster{},
		storageSvc: storageSvc,
	}
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:            vmNode,
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// validStemcellCID returns a well-formed stemcell CID for use in tests.
func validStemcellCID() string {
	return "local:import/bosh-stemcell-ubuntu-jammy-1.0-abc12345.qcow2"
}

// TestDeleteStemcell_HappyPath verifies that the qcow2 volume is deleted.
// Volume notes attached to the qcow2 are removed transitively.
func TestDeleteStemcell_HappyPath(t *testing.T) {
	t.Parallel()

	cid := validStemcellCID()

	var deletedVolumes []string
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			deletedVolumes = append(deletedVolumes, volume)
			return true, nil
		},
	}

	deps := buildDeleteStemcellDeps(storageSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, cid)}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-del-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result (void), got %v", result)
	}
	if len(deletedVolumes) != 1 {
		t.Fatalf("expected 1 DeleteVolumeIfExists call, got %d: %v", len(deletedVolumes), deletedVolumes)
	}
	if deletedVolumes[0] != "import/bosh-stemcell-ubuntu-jammy-1.0-abc12345.qcow2" {
		t.Errorf("delete volume = %q; want qcow2 import path", deletedVolumes[0])
	}
}

// TestDeleteStemcell_Idempotent_VolumeAbsent verifies success when the qcow2 is
// already gone. delete_stemcell must be idempotent — an absent volume produces
// a warning, not an error.
func TestDeleteStemcell_Idempotent_VolumeAbsent(t *testing.T) {
	t.Parallel()

	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, nil // volume did not exist
		},
	}

	deps := buildDeleteStemcellDeps(storageSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, validStemcellCID())}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error when volume absent (idempotent), got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestDeleteStemcell_MissingArg verifies CloudError when no arguments provided.
func TestDeleteStemcell_MissingArg(t *testing.T) {
	t.Parallel()

	deps := buildDeleteStemcellDeps(&deleteStemcellMockStorage{})
	h := handlers.HandleDeleteStemcell(deps)

	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when stemcell_cid is missing, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_NonStringArg verifies CloudError when stemcell_cid is a JSON number.
func TestDeleteStemcell_NonStringArg(t *testing.T) {
	t.Parallel()

	deps := buildDeleteStemcellDeps(&deleteStemcellMockStorage{})
	h := handlers.HandleDeleteStemcell(deps)

	// JSON number (5042) instead of string.
	args := []json.RawMessage{json.RawMessage(`5042`)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when stemcell_cid is a JSON number (not string), got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_LegacyIntegerCID verifies CloudError for legacy integer-only CIDs.
// These were the old VMID-style CIDs and are no longer valid in the qcow2 volume model.
// Legacy integer CIDs come from the obsolete template-clone CPI design.
// delete_stemcell must accept them as a no-op so operators can scrub the
// stale row from the director without manual database surgery. The PVE
// side is NOT touched (no DeleteVolumeIfExists call) since the original
// template VM and its import qcow2 are out-of-band cleanup.
func TestDeleteStemcell_LegacyIntegerCID(t *testing.T) {
	t.Parallel()

	legacyCIDs := []string{"5042", "5000", "5999", "100", "1"}

	for _, cid := range legacyCIDs {
		cid := cid
		t.Run("cid="+cid, func(t *testing.T) {
			t.Parallel()

			storage := &deleteStemcellMockStorage{}
			deps := buildDeleteStemcellDeps(storage)
			h := handlers.HandleDeleteStemcell(deps)

			args := []json.RawMessage{marshalArg(t, cid)}
			result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
			if err != nil {
				t.Fatalf("expected nil error for legacy CID %q (no-op accept), got: %v", cid, err)
			}
			if result != nil {
				t.Errorf("expected nil result, got %v", result)
			}
			if storage.deleteVolumeIfExistsCalls != 0 {
				t.Errorf("expected no PVE delete calls for legacy CID, got %d", storage.deleteVolumeIfExistsCalls)
			}
		})
	}
}

// TestDeleteStemcell_MalformedCID verifies CloudError for CIDs that lack the
// expected "<storage>:import/<filename>" format.
func TestDeleteStemcell_MalformedCID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cid  string
	}{
		{"no colon", "localbosh-stemcell-ubuntu.qcow2"},
		{"no import prefix", "local:volumes/bosh-stemcell.qcow2"},
		{"empty", ""},
		{"just colon", ":"},
		{"colon no import", "local:bosh-stemcell.qcow2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deps := buildDeleteStemcellDeps(&deleteStemcellMockStorage{})
			h := handlers.HandleDeleteStemcell(deps)

			var args []json.RawMessage
			if tc.cid == "" {
				args = []json.RawMessage{marshalArg(t, "")}
			} else {
				args = []json.RawMessage{marshalArg(t, tc.cid)}
			}

			_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
			if err == nil {
				t.Fatalf("expected error for malformed CID %q, got nil", tc.cid)
			}
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) {
				t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
			}
		})
	}
}

// TestDeleteStemcell_NoNodeConfigured verifies CloudError when config.Node is empty.
func TestDeleteStemcell_NoNodeConfigured(t *testing.T) {
	t.Parallel()

	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   &stemcellMockNodes{},
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: &stemcellMockCluster{},
		storageSvc: &deleteStemcellMockStorage{},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:            "", // deliberately empty
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, validStemcellCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when node is empty, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_QCow2DeleteError verifies that a storage API error deleting
// the qcow2 volume is propagated as a CloudError.
func TestDeleteStemcell_QCow2DeleteError(t *testing.T) {
	t.Parallel()

	storageErr := errors.New("PVE storage: permission denied")
	callCount := 0
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			callCount++
			return false, storageErr // first call (qcow2) fails
		},
	}

	deps := buildDeleteStemcellDeps(storageSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, validStemcellCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for qcow2 delete failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	// Only one call expected — handler returns immediately on qcow2 delete error.
	if callCount != 1 {
		t.Errorf("expected 1 DeleteVolumeIfExists call on qcow2 error, got %d", callCount)
	}
}

// TestDeleteStemcell_StorageExtractedFromCID verifies that the storage pool is
// taken from the CID, not from config.StemcellStorage. This matters when a CID
// refers to a storage other than the current default.
func TestDeleteStemcell_StorageExtractedFromCID(t *testing.T) {
	t.Parallel()

	// CID references "nfs-pool"; config.StemcellStorage = "local".
	cid := "nfs-pool:import/bosh-stemcell-ubuntu-jammy-1.0-deadbeef.qcow2"

	var capturedStorage string
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, storage, _ string) (bool, error) {
			capturedStorage = storage
			return true, nil
		},
	}

	deps := buildDeleteStemcellDeps(storageSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, cid)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "nfs-pool" {
		t.Errorf("storage passed to DeleteVolumeIfExists = %q; want %q", capturedStorage, "nfs-pool")
	}
}

// TestDeleteStemcell_LightCID_NoOp verifies that delete_stemcell short-circuits
// on a "light:" prefixed CID without calling the PVE Storage API. Light
// stemcells are operator-managed; the CPI must never delete their underlying
// volumes.
func TestDeleteStemcell_LightCID_NoOp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cid  string
	}{
		{name: "light shared storage", cid: "light:nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"},
		{name: "light local storage", cid: "light:local:import/bosh-stemcell-other-2.0-cafebabe.qcow2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storageSvc := &deleteStemcellMockStorage{}
			deps := buildDeleteStemcellDeps(storageSvc)
			h := handlers.HandleDeleteStemcell(deps)

			args := []json.RawMessage{marshalArg(t, tc.cid)}
			result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-del-light"})
			if err != nil {
				t.Fatalf("unexpected error for light CID %q: %v", tc.cid, err)
			}
			if result != nil {
				t.Errorf("expected nil result for light CID %q, got %v", tc.cid, result)
			}
			if storageSvc.deleteVolumeIfExistsCalls != 0 {
				t.Errorf("expected ZERO DeleteVolumeIfExists calls for light CID, got %d",
					storageSvc.deleteVolumeIfExistsCalls)
			}
		})
	}
}
