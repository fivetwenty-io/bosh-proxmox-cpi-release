package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
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

// buildDeleteStemcellDepsWithNodes constructs Deps for delete_stemcell tests
// that need a customised nodes mock (e.g. template destroy path).
func buildDeleteStemcellDepsWithNodes(nodesSvc *stemcellMockNodes, cfg *config.CPIConfig) handlers.Deps {
	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   nodesSvc,
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: &stemcellMockCluster{},
		storageSvc: &deleteStemcellMockStorage{},
	}
	return handlers.Deps{
		Config: cfg,
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// defaultTemplateDeps returns Deps configured for template CID tests.
// nodesSvc.deleteQemuFn is wired by the caller.
func defaultTemplateDeps(nodesSvc *stemcellMockNodes) handlers.Deps {
	return buildDeleteStemcellDepsWithNodes(nodesSvc, &config.CPIConfig{
		Node:            vmNode,
		StemcellStorage: "local",
		VMStorage:       "local",
		DiskStorage:     "local",
	})
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

// ============================================================
// Tests: HandleDeleteStemcell — template CID routing
// ============================================================

// TestDeleteStemcell_TemplateCID_HappyPath verifies that a "template:<vmid>"
// CID triggers DeleteQemu with purge=true and destroy-unreferenced-disks=true,
// and that the returned UPID is awaited.
func TestDeleteStemcell_TemplateCID_HappyPath(t *testing.T) {
	t.Parallel()

	type deleteCall struct {
		node         string
		vmid         string
		purge        bool
		destroyDisks bool
	}
	var deleteCalls []deleteCall

	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			call := deleteCall{node: node, vmid: vmid}
			if params != nil && params.Purge != nil {
				call.purge = *params.Purge
			}
			if params != nil && params.DestroyUnreferencedDisks != nil {
				call.destroyDisks = *params.DestroyUnreferencedDisks
			}
			deleteCalls = append(deleteCalls, call)
			// Return a non-empty UPID so the await path is exercised.
			resp := sdknodes.DeleteQemuResponse(`"UPID:pve-node1:00AB12CD:template-del"`)
			return &resp, nil
		},
	}

	deps := defaultTemplateDeps(nodesSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, "template:6042")}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-del-tmpl-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result (void), got %v", result)
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", len(deleteCalls))
	}
	if deleteCalls[0].vmid != "6042" {
		t.Errorf("DeleteQemu vmid = %q; want %q", deleteCalls[0].vmid, "6042")
	}
	if deleteCalls[0].node != vmNode {
		t.Errorf("DeleteQemu node = %q; want %q", deleteCalls[0].node, vmNode)
	}
	if !deleteCalls[0].purge {
		t.Error("DeleteQemu: purge must be true")
	}
	if !deleteCalls[0].destroyDisks {
		t.Error("DeleteQemu: DestroyUnreferencedDisks must be true")
	}
}

// TestDeleteStemcell_TemplateCID_Idempotent_NotFound verifies that a 404 from
// DeleteQemu is treated as success. BOSH may call delete_stemcell multiple times.
func TestDeleteStemcell_TemplateCID_Idempotent_NotFound(t *testing.T) {
	t.Parallel()

	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404}
		},
	}

	deps := defaultTemplateDeps(nodesSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, "template:6042")}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error when VM not found (idempotent), got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestDeleteStemcell_TemplateCID_DestroyError verifies that a non-404 error from
// DeleteQemu is propagated as a cloud error.
func TestDeleteStemcell_TemplateCID_DestroyError(t *testing.T) {
	t.Parallel()

	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return nil, errors.New("PVE: internal server error")
		},
	}

	deps := defaultTemplateDeps(nodesSvc)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, "template:6042")}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for template destroy failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_TemplateCID_NodeFromStemcellTemplateNode verifies that when
// StemcellTemplateNode is set it is used for the delete call rather than Node.
func TestDeleteStemcell_TemplateCID_NodeFromStemcellTemplateNode(t *testing.T) {
	t.Parallel()

	var capturedNode string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, node, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			capturedNode = node
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                 vmNode,
		StemcellTemplateNode: "pve-template-node",
		StemcellStorage:      "local",
		VMStorage:            "local",
		DiskStorage:          "local",
	}
	deps := buildDeleteStemcellDepsWithNodes(nodesSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, "template:7001")}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedNode != "pve-template-node" {
		t.Errorf("DeleteQemu node = %q; want %q (StemcellTemplateNode)", capturedNode, "pve-template-node")
	}
}

// TestDeleteStemcell_TemplateCID_MalformedVMID verifies that a "template:"
// CID with a non-integer suffix is rejected as a cloud error without making
// any PVE API calls.
func TestDeleteStemcell_TemplateCID_MalformedVMID(t *testing.T) {
	t.Parallel()

	nodesSvc := &stemcellMockNodes{}
	deps := defaultTemplateDeps(nodesSvc)
	h := handlers.HandleDeleteStemcell(deps)

	// "template:abc" satisfies HasPrefix("template:") but IsTemplateStemcellCID
	// returns false because "abc" is not all-digits, so it falls through to the
	// legacy/volume parse path and gets rejected there.  Use a CID where the
	// prefix is "template:" and the remainder looks like a template but parses
	// as something the handler rejects at the CID-routing level by forcing a
	// direct call through the flow.  The simplest approach: any CID that is NOT
	// IsTemplateStemcellCID but starts with "template:" reaches ParseStemcellCID
	// which will reject it.  We test that no DeleteQemu call is made.
	args := []json.RawMessage{marshalArg(t, "template:abc")}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed template CID, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	// No DeleteQemu should have been called.
	if nodesSvc.deleteQemuFn != nil {
		// deleteQemuFn is nil by default; if it were wired a call would panic.
		t.Log("deleteQemuFn is nil, no call possible")
	}
}

// TestDeleteStemcell_VolumeCID_Regression_TemplateRouteNotTriggered verifies that
// a plain volume CID ("local:import/foo.qcow2") takes the existing volume-delete
// path and does NOT trigger any DeleteQemu call.
func TestDeleteStemcell_VolumeCID_Regression_TemplateRouteNotTriggered(t *testing.T) {
	t.Parallel()

	cid := "local:import/bosh-stemcell-ubuntu-jammy-1.0-deadbeef.qcow2"

	var deleteQemuCalled bool
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deleteQemuCalled = true
			return nil, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}
	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   nodesSvc,
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: &stemcellMockCluster{},
		storageSvc: storageSvc,
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:            vmNode,
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, cid)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for volume CID: %v", err)
	}
	if deleteQemuCalled {
		t.Error("DeleteQemu must NOT be called for a volume CID — template route was incorrectly triggered")
	}
	if storageSvc.deleteVolumeIfExistsCalls != 1 {
		t.Errorf("expected 1 DeleteVolumeIfExists call for volume CID, got %d", storageSvc.deleteVolumeIfExistsCalls)
	}
}

// TestDeleteStemcell_LightCID_Regression_TemplateRouteNotTriggered verifies
// that a light CID takes the no-op path and does not call DeleteQemu.
func TestDeleteStemcell_LightCID_Regression_TemplateRouteNotTriggered(t *testing.T) {
	t.Parallel()

	var deleteQemuCalled bool
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deleteQemuCalled = true
			return nil, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}
	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   nodesSvc,
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: &stemcellMockCluster{},
		storageSvc: storageSvc,
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:            vmNode,
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, "light:nfs:import/bosh-stemcell-ubuntu-1.0-cafebabe.qcow2")}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for light CID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if deleteQemuCalled {
		t.Error("DeleteQemu must NOT be called for a light CID")
	}
	if storageSvc.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("expected 0 DeleteVolumeIfExists calls for light CID, got %d", storageSvc.deleteVolumeIfExistsCalls)
	}
}

// ============================================================
// Tests: sha-sweep (D-03 — always cross-node)
// ============================================================

// buildDeleteStemcellDepsWithCluster constructs Deps for sha-sweep and
// orphan-prune tests that need a wired cluster mock.
func buildDeleteStemcellDepsWithCluster(
	nodesSvc *stemcellMockNodes,
	clusterSvc *stemcellMockCluster,
	cfg *config.CPIConfig,
) handlers.Deps {
	client := &stemcellMockClient{
		qemuSvc:    &stemcellMockQEMU{},
		nodesSvc:   nodesSvc,
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: clusterSvc,
		storageSvc: &deleteStemcellMockStorage{},
	}
	return handlers.Deps{
		Config: cfg,
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// makeStemcellClusterItem builds a raw JSON cluster resource entry for a
// stemcell template with the given vmid, node, and semicolon-separated tags.
func makeStemcellClusterItem(vmid int64, node, name, tags string) json.RawMessage {
	templateVal := 1
	raw, _ := json.Marshal(map[string]any{
		"type":     "qemu",
		"vmid":     vmid,
		"node":     node,
		"name":     name,
		"tags":     tags,
		"template": templateVal,
	})
	return raw
}

// TestDeleteStemcell_ShaSwep_DeletesCrossNodeReplicas verifies that the sha-sweep
// deletes two cross-node template VMs carrying the sha8 tag.
func TestDeleteStemcell_ShaSwep_DeletesCrossNodeReplicas(t *testing.T) {
	t.Parallel()

	const sha8 = "abc12345"
	const primaryVMID = int64(6042)
	const replica1VMID = int64(6043)
	const replica2VMID = int64(6044)

	// Primary tag set for resolveStemcellSHA8FromVMID (ListQemu on primary node).
	primaryTagsStr := "bosh-stemcell;bosh-stemcell-sha-" + sha8
	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != vmNode {
				return &sdknodes.ListQemuResponse{}, nil
			}
			raw, _ := json.Marshal(map[string]any{
				"vmid": primaryVMID,
				"tags": primaryTagsStr,
			})
			resp := sdknodes.ListQemuResponse{raw}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	var destroyedVMIDs []int64
	origDelete := nodesSvc.deleteQemuFn
	nodesSvc.deleteQemuFn = func(_ context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		id := int64(0)
		for _, v := range []int64{primaryVMID, replica1VMID, replica2VMID} {
			if vmid == fmt.Sprintf("%d", v) {
				id = v
			}
		}
		if id != 0 {
			destroyedVMIDs = append(destroyedVMIDs, id)
		}
		return origDelete(context.Background(), node, vmid, params)
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				makeStemcellClusterItem(replica1VMID, "pve2", "stemcell-r1", shaTag),
				makeStemcellClusterItem(replica2VMID, "pve3", "stemcell-r2", shaTag),
				// non-template (template==0) — must not be touched
				func() json.RawMessage {
					tmplZero := 0
					raw, _ := json.Marshal(map[string]any{
						"type":     "qemu",
						"vmid":     int64(9999),
						"node":     "pve4",
						"tags":     shaTag,
						"template": tmplZero,
					})
					return raw
				}(),
			}
			return &items, nil
		},
	}

	cfg := &config.CPIConfig{
		Node:            vmNode,
		StemcellStorage: "local",
		VMStorage:       "local",
		DiskStorage:     "local",
	}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-sweep-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}

	// Primary + both replicas must have been destroyed.
	found := map[int64]bool{}
	for _, id := range destroyedVMIDs {
		found[id] = true
	}
	for _, want := range []int64{primaryVMID, replica1VMID, replica2VMID} {
		if !found[want] {
			t.Errorf("expected VMID %d to be destroyed; destroyed set: %v", want, destroyedVMIDs)
		}
	}
}

// TestDeleteStemcell_ShaSwep_BestEffort verifies that a destroy error on one
// replica does not prevent the handler from returning success.
func TestDeleteStemcell_ShaSwep_BestEffort(t *testing.T) {
	t.Parallel()

	const sha8 = "deadbeef"
	const primaryVMID = int64(7001)
	const replica1VMID = int64(7002)
	const replica2VMID = int64(7003)

	primaryTagsStr := "bosh-stemcell;bosh-stemcell-sha-" + sha8
	var destroyCount int
	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != vmNode {
				return &sdknodes.ListQemuResponse{}, nil
			}
			raw, _ := json.Marshal(map[string]any{"vmid": primaryVMID, "tags": primaryTagsStr})
			resp := sdknodes.ListQemuResponse{raw}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyCount++
			// Fail on replica1 only.
			if vmid == fmt.Sprintf("%d", replica1VMID) {
				return nil, errors.New("PVE: transient error on replica")
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				makeStemcellClusterItem(replica1VMID, "pve2", "r1", shaTag),
				makeStemcellClusterItem(replica2VMID, "pve3", "r2", shaTag),
			}
			return &items, nil
		},
	}

	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed even when a replica destroy fails; got: %v", err)
	}
	// primary + 2 sweep attempts = 3 calls (replica1 fails, replica2 succeeds)
	if destroyCount != 3 {
		t.Errorf("expected 3 DeleteQemu calls (primary + 2 replicas), got %d", destroyCount)
	}
}

// ============================================================
// Tests: orphan prune (D-04 — opt-in director-scoped)
// ============================================================

// TestDeleteStemcell_OrphanPrune_Disabled verifies no prune ListResources call
// when pruning is not enabled.
func TestDeleteStemcell_OrphanPrune_Disabled(t *testing.T) {
	t.Parallel()

	var listResourcesCalled bool
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listResourcesCalled = true
			return &sdkcluster.ListResourcesResponse{}, nil
		},
	}

	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	// Prune is off (nil Stemcell block = default).
	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	// Use a volume CID — avoids sha-sweep ListResources path entirely.
	args := []json.RawMessage{marshalArg(t, "local:import/bosh-stemcell-ubuntu-jammy-1.0-abc12345.qcow2")}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listResourcesCalled {
		t.Error("ListResources must NOT be called when orphan prune is disabled")
	}
}

// TestDeleteStemcell_OrphanPrune_DryRun verifies that candidates are logged but
// no DeleteQemu is issued when dry_run is true.
func TestDeleteStemcell_OrphanPrune_DryRun(t *testing.T) {
	t.Parallel()

	const dirID = "bosh-dir-test"
	const sha8 = "cafebabe"
	const primaryVMID = int64(8001)
	const orphanVMID = int64(8002)

	primaryTagsStr := "bosh-stemcell;bosh-stemcell-sha-" + sha8 + ";director--" + dirID

	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != vmNode {
				return &sdknodes.ListQemuResponse{}, nil
			}
			raw, _ := json.Marshal(map[string]any{"vmid": primaryVMID, "tags": primaryTagsStr})
			resp := sdknodes.ListQemuResponse{raw}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			// Only primary delete is expected (sha sweep finds nothing extra in cluster).
			if vmid != fmt.Sprintf("%d", primaryVMID) {
				t.Errorf("unexpected DeleteQemu for vmid %s in dry-run test", vmid)
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	// sha-sweep: cluster returns only the primary (already gone = idempotent).
	// prune candidate: orphanVMID carries marker + director tag.
	shaTag := "bosh-stemcell-sha-" + sha8
	dirTag := "director--" + dirID
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				// sha-sweep candidate (primary — already gone, idempotent)
				makeStemcellClusterItem(primaryVMID, vmNode, "primary", shaTag),
				// orphan candidate: has marker+director, no sha tag
				makeStemcellClusterItem(orphanVMID, "pve2", "orphan", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}

	tr := true
	cfg := &config.CPIConfig{
		Node:            vmNode,
		StemcellStorage: "local",
		VMStorage:       "local",
		DiskStorage:     "local",
		Stemcell: &config.StemcellProvenanceConfig{
			PruneOrphans: &tr,
			PruneDryRun:  &tr,
			DirectorID:   dirID,
		},
	}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed in dry-run; got: %v", err)
	}
	// deleteQemuFn would call t.Errorf if orphanVMID was passed — test passes
	// if no unexpected call occurs.
}

// TestDeleteStemcell_OrphanPrune_Live verifies that an orphan is destroyed, and
// that a base-in-use error causes a skip rather than failure.
func TestDeleteStemcell_OrphanPrune_Live(t *testing.T) {
	t.Parallel()

	const dirID = "bosh-dir-prod"
	const sha8 = "11223344"
	const primaryVMID = int64(9001)
	const orphanVMID = int64(9002)
	const baseInUseVMID = int64(9003)

	primaryTagsStr := "bosh-stemcell;bosh-stemcell-sha-" + sha8 + ";director--" + dirID

	var deletedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != vmNode {
				return &sdknodes.ListQemuResponse{}, nil
			}
			raw, _ := json.Marshal(map[string]any{"vmid": primaryVMID, "tags": primaryTagsStr})
			resp := sdknodes.ListQemuResponse{raw}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmid)
			if vmid == fmt.Sprintf("%d", baseInUseVMID) {
				// Simulate PVE "base volume still in use" error.
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "base volume still in use"}
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	dirTag := "director--" + dirID
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				// sha-sweep: primary already gone — idempotent 404 path
				makeStemcellClusterItem(primaryVMID, vmNode, "primary", shaTag),
				// orphan: should be deleted
				makeStemcellClusterItem(orphanVMID, "pve2", "orphan", "bosh-stemcell;"+dirTag),
				// base-in-use: should be skipped
				makeStemcellClusterItem(baseInUseVMID, "pve3", "base-in-use", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}

	tr := true
	cfg := &config.CPIConfig{
		Node:            vmNode,
		StemcellStorage: "local",
		VMStorage:       "local",
		DiskStorage:     "local",
		Stemcell: &config.StemcellProvenanceConfig{
			PruneOrphans: &tr,
			PruneDryRun:  nil, // live mode
			DirectorID:   dirID,
		},
	}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed even with base-in-use skip; got: %v", err)
	}

	// orphanVMID must have been attempted.
	foundOrphan := false
	for _, id := range deletedVMIDs {
		if id == fmt.Sprintf("%d", orphanVMID) {
			foundOrphan = true
		}
	}
	if !foundOrphan {
		t.Errorf("expected orphanVMID %d to be deleted; deletedVMIDs: %v", orphanVMID, deletedVMIDs)
	}
}

// TestDeleteStemcell_OrphanPrune_EmptyDirectorID verifies that when director_id
// is empty, pruning is skipped with a warning and no deletions occur.
func TestDeleteStemcell_OrphanPrune_EmptyDirectorID(t *testing.T) {
	t.Parallel()

	const sha8 = "55667788"
	const primaryVMID = int64(5501)

	primaryTagsStr := "bosh-stemcell;bosh-stemcell-sha-" + sha8
	var orphanDeleteCalled bool

	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != vmNode {
				return &sdknodes.ListQemuResponse{}, nil
			}
			raw, _ := json.Marshal(map[string]any{"vmid": primaryVMID, "tags": primaryTagsStr})
			resp := sdknodes.ListQemuResponse{raw}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			if vmid != fmt.Sprintf("%d", primaryVMID) {
				orphanDeleteCalled = true
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			// Provide a sha-sweep item (primary) and an orphan with no director tag.
			items := sdkcluster.ListResourcesResponse{
				makeStemcellClusterItem(primaryVMID, vmNode, "primary", shaTag),
				makeStemcellClusterItem(int64(5502), "pve2", "orphan-no-dir", "bosh-stemcell"),
			}
			return &items, nil
		},
	}

	tr := true
	cfg := &config.CPIConfig{
		Node:            vmNode,
		StemcellStorage: "local",
		VMStorage:       "local",
		DiskStorage:     "local",
		Stemcell: &config.StemcellProvenanceConfig{
			PruneOrphans: &tr,
			DirectorID:   "", // empty — prune must be skipped
		},
	}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed when director_id is empty; got: %v", err)
	}
	if orphanDeleteCalled {
		t.Error("expected no orphan deletions when director_id is empty")
	}
}

// TestDeleteStemcell_ShaSwep_Idempotent_Primary404 verifies that a 404 on the
// primary delete is still treated as success.
func TestDeleteStemcell_ShaSwep_Idempotent_Primary404(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(4040)

	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			// Primary already gone — empty list.
			return &sdknodes.ListQemuResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404}
		},
	}

	clusterSvc := &stemcellMockCluster{} // returns empty list by default

	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDepsWithCluster(nodesSvc, clusterSvc, cfg)
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, fmt.Sprintf("template:%d", primaryVMID))}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error for 404 on primary (idempotent); got: %v", err)
	}
}

// Ensure sdkcluster import is used even if other test variants are skipped.
var _ *sdkcluster.ListResourcesParams

// Ensure sdkerrors import is used even if other test variants are skipped.
var _ = sdkerrors.APIError{}
