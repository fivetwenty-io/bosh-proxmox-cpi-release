package handlers_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ============================================================
// Mock infrastructure shared across stemcell test files.
// ============================================================

// stemcellMockClient implements pve.Client. All services default to no-ops or
// embed the interface (panics on unknown calls).
type stemcellMockClient struct {
	qemuSvc    sdkqemu.Service
	nodesSvc   sdknodes.Service
	tasksSvc   sdktasks.Service
	clusterSvc sdkcluster.Service
	storageSvc sdkstorage.Service
}

func (m *stemcellMockClient) QEMU() sdkqemu.Service                     { return m.qemuSvc }
func (m *stemcellMockClient) Storage() sdkstorage.Service               { return m.storageSvc }
func (m *stemcellMockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (m *stemcellMockClient) Tasks() sdktasks.Service                   { return m.tasksSvc }
func (m *stemcellMockClient) Nodes() sdknodes.Service                   { return m.nodesSvc }
func (m *stemcellMockClient) Cluster() sdkcluster.Service               { return m.clusterSvc }
func (m *stemcellMockClient) ClusterStorage() sdkclusterstorage.Service { return nil }

// stemcellMockQEMU satisfies sdkqemu.Service. Only Create and Config are wired.
// templateFn removed — the new flow never calls QEMU().Template().
type stemcellMockQEMU struct {
	sdkqemu.Service // embed nil — panics on unmocked methods
	createFn        func(ctx context.Context, node string, params map[string]interface{}) (string, error)
	configFn        func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
}

func (m *stemcellMockQEMU) Create(ctx context.Context, node string, params map[string]interface{}) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "UPID:node1:create:ok", nil
}

func (m *stemcellMockQEMU) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]interface{}{}, nil
}

// stemcellMockNodes satisfies sdknodes.Service for create_stemcell tests.
// UpdateQemuConfig and DeleteQemu are wired for legacy compat;
// ListStorageContent is wired for the dedup path.
type stemcellMockNodes struct {
	sdknodes.Service // embed nil — panics on unmocked methods
	updateConfigFn   func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteQemuFn     func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	listStorageFn    func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
}

func (m *stemcellMockNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if m.updateConfigFn != nil {
		return m.updateConfigFn(ctx, node, vmid, params)
	}
	return nil
}

func (m *stemcellMockNodes) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if m.deleteQemuFn != nil {
		return m.deleteQemuFn(ctx, node, vmid, params)
	}
	raw := sdknodes.DeleteQemuResponse(`""`)
	return &raw, nil
}

func (m *stemcellMockNodes) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	if m.listStorageFn != nil {
		return m.listStorageFn(ctx, node, storage, params)
	}
	// Default: empty — no existing volumes.
	empty := sdknodes.ListStorageContentResponse{}
	return &empty, nil
}

// stemcellMockTasks satisfies sdktasks.Service. Always returns OK by default.
type stemcellMockTasks struct {
	waitFn func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
}

func (m *stemcellMockTasks) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if m.waitFn != nil {
		return m.waitFn(ctx, node, upid, opts)
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// stemcellMockCluster satisfies sdkcluster.Service for stemcell tests.
// ListResources and ListConfigNodes are wired; other methods panic.
type stemcellMockCluster struct {
	sdkcluster.Service // embed nil — panics on unmocked methods
	listResourcesFn    func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
	listConfigNodesFn  func(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error)
}

func (m *stemcellMockCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if m.listResourcesFn != nil {
		return m.listResourcesFn(ctx, params)
	}
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

func (m *stemcellMockCluster) ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	if m.listConfigNodesFn != nil {
		return m.listConfigNodesFn(ctx)
	}
	// Default: single-node cluster — local storage is acceptable.
	resp := sdkcluster.ListConfigNodesResponse{json.RawMessage(`{"node":"pve-node1"}`)}
	return &resp, nil
}

// stemcellMockStorage satisfies sdkstorage.Service for stemcell upload tests.
// Upload and DeleteVolumeIfExists are wired; other methods panic on accidental call.
type stemcellMockStorage struct {
	sdkstorage.Service     // embed nil — panics on unexpected calls
	uploadFn               func(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error)
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
}

func (m *stemcellMockStorage) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, node, storage, content, filename, body)
	}
	// Default: no-op success, no UPID (no async task).
	return "", nil
}

func (m *stemcellMockStorage) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	// Default: volume absent, no error.
	return false, nil
}

// buildStemcellClient constructs a stemcellMockClient wired with the provided mocks.
func buildStemcellClient(qemu *stemcellMockQEMU, nodes *stemcellMockNodes, tasks *stemcellMockTasks, cluster *stemcellMockCluster) *stemcellMockClient {
	return &stemcellMockClient{
		qemuSvc:    qemu,
		nodesSvc:   nodes,
		tasksSvc:   tasks,
		clusterSvc: cluster,
		storageSvc: &stemcellMockStorage{},
	}
}

// defaultStemcellClient returns a client with all services using their default no-op behaviors.
func defaultStemcellClient() *stemcellMockClient {
	return buildStemcellClient(
		&stemcellMockQEMU{},
		&stemcellMockNodes{},
		&stemcellMockTasks{},
		&stemcellMockCluster{},
	)
}

// makeDeps returns a Deps with a mock PVE client and a minimal config.
func makeDeps(client pve.Client) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// marshalArg marshals v to a json.RawMessage for use as a handler argument.
func marshalArg(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalArg: %v", err)
	}
	return raw
}

// tempImageFile creates a temporary file with fixed deterministic bytes and
// returns its path. The content is non-qcow2 (no magic header) so format
// detection returns "raw".
func tempImageFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.img")
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	_, _ = f.WriteString("FAKE STEMCELL IMAGE DATA")
	_ = f.Close()
	return f.Name()
}

// computeFileSHA hashes the contents of path and returns the hex digest.
// Mirrors the production sha256FilePath helper so tests can predict the CID.
func computeFileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// containsSubstr is a substring check helper for test readability.
func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// localBackendResolver is a test BackendResolver that always returns BackendLocal.
// Used to trigger the shared-storage check in validateStemcellStorageShared.
type localBackendResolver struct{}

func (r *localBackendResolver) Resolve(_ context.Context, storage string) (pve.Backend, error) {
	return &localBackend{}, nil
}

type localBackend struct{}

func (b *localBackend) Kind() pve.BackendKind { return pve.BackendLocal }
func (b *localBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return "pve-node1", nil
}
func (b *localBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return "pve-node1", nil
}

// Ensure pve.Client interface is satisfied by stemcellMockClient at compile time.
var _ pve.Client = (*stemcellMockClient)(nil)

// Ensure pveerr package is used (404 sentinel check in delete_stemcell_test.go).
var _ = pveerr.ErrNotFound

// ============================================================
// Tests: HandleCreateStemcell — new direct-qcow upload flow
// ============================================================

// TestCreateStemcell_HappyPath_NewFlow verifies the full success path for the
// direct-qcow upload flow: exactly one upload (the qcow2), dedup check, and
// returned CID in the correct format. Stemcell identity is carried entirely
// by the qcow2 filename — PVE's content APIs don't accept arbitrary metadata
// for import volumes, so no sidecar or volume annotation is written.
func TestCreateStemcell_HappyPath_NewFlow(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)
	wantSHA := computeFileSHA(t, imgPath)
	sha8 := wantSHA[:8]

	type uploadCall struct {
		content  string
		filename string
	}
	var uploads []uploadCall

	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, content, filename string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			uploads = append(uploads, uploadCall{content: content, filename: filename})
			return "", nil
		},
	}

	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-happy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cidStr, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}

	wantFilename := "bosh-stemcell-ubuntu-jammy-1.234-" + sha8 + ".qcow2"
	wantCID := "local:import/" + wantFilename
	if cidStr != wantCID {
		t.Errorf("CID = %q; want %q", cidStr, wantCID)
	}

	if len(uploads) != 1 {
		t.Fatalf("expected 1 Upload call, got %d: %v", len(uploads), uploads)
	}
	if uploads[0].content != "import" {
		t.Errorf("upload content = %q; want %q", uploads[0].content, "import")
	}
	if uploads[0].filename != wantFilename {
		t.Errorf("upload filename = %q; want %q", uploads[0].filename, wantFilename)
	}
}

// TestCreateStemcell_Dedup_SameFilename verifies that when ListStorageContent
// returns a matching volid, Upload is not called and the existing CID is returned.
func TestCreateStemcell_Dedup_SameFilename(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)
	wantSHA := computeFileSHA(t, imgPath)
	sha8 := wantSHA[:8]
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.234-" + sha8 + ".qcow2"
	existingVolid := "local:import/" + wantFilename

	var uploadCalled bool
	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalled = true
			return "", nil
		},
	}

	// ListStorageContent returns the matching entry.
	nodesSvc := &stemcellMockNodes{
		listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			entry, _ := json.Marshal(map[string]string{"volid": existingVolid})
			resp := sdknodes.ListStorageContentResponse{entry}
			return &resp, nil
		},
	}

	client := buildStemcellClient(&stemcellMockQEMU{}, nodesSvc, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cidStr, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	if cidStr != existingVolid {
		t.Errorf("CID = %q; want existing volid %q", cidStr, existingVolid)
	}
	if uploadCalled {
		t.Error("Upload called despite dedup hit; should be skipped")
	}
}

// TestCreateStemcell_RejectLocalStemcellStorage verifies D-03: a local-only
// stemcell storage on a multi-node cluster causes the handler to return an error
// containing "shared" (the canonical D-03 message fragment).
func TestCreateStemcell_RejectLocalStemcellStorage(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	// Cluster reports 2 nodes.
	cluster := &stemcellMockCluster{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			resp := sdkcluster.ListConfigNodesResponse{
				json.RawMessage(`{"node":"pve-node1"}`),
				json.RawMessage(`{"node":"pve-node2"}`),
			}
			return &resp, nil
		},
	}

	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, cluster)
	client.storageSvc = &stemcellMockStorage{}

	deps := makeDeps(client)
	// Wire a resolver that classifies stemcell storage as local.
	deps.Resolver = &localBackendResolver{}

	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for local storage on multi-node cluster (D-03), got nil")
	}
	if !containsSubstr(err.Error(), "shared") {
		t.Errorf("error %q does not contain expected fragment %q", err.Error(), "shared")
	}
}

// TestCreateStemcell_MissingName_ReturnsError verifies that omitting
// cloud_properties.name produces a cloud error (name is required for CID).
func TestCreateStemcell_MissingName_ReturnsError(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	// version present, name absent.
	cp := map[string]any{"version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing cloud_properties.name, got nil")
	}
	if !containsSubstr(err.Error(), "name") {
		t.Errorf("error %q does not reference missing field %q", err.Error(), "name")
	}
}

// TestCreateStemcell_MissingVersion_ReturnsError verifies that omitting
// cloud_properties.version produces a cloud error (version is required for CID).
func TestCreateStemcell_MissingVersion_ReturnsError(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	// name present, version absent.
	cp := map[string]any{"name": "ubuntu-jammy", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing cloud_properties.version, got nil")
	}
	if !containsSubstr(err.Error(), "version") {
		t.Errorf("error %q does not reference missing field %q", err.Error(), "version")
	}
}

// ============================================================
// Tests: image-path validation (retained from original suite)
// ============================================================

// TestCreateStemcell_BadImagePath_MissingFile verifies CloudError when image_path
// does not exist.
func TestCreateStemcell_BadImagePath_MissingFile(t *testing.T) {
	t.Parallel()

	nonExistent := filepath.Join(t.TempDir(), "no-such-stemcell.img")

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	args := []json.RawMessage{marshalArg(t, nonExistent), marshalArg(t, map[string]any{})}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for non-existent image_path, got nil")
	}
}

// TestCreateStemcell_BadImagePath_Directory verifies CloudError when image_path
// is a directory rather than a regular file.
func TestCreateStemcell_BadImagePath_Directory(t *testing.T) {
	t.Parallel()

	dirPath := t.TempDir()

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	args := []json.RawMessage{marshalArg(t, dirPath), marshalArg(t, map[string]any{})}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when image_path is a directory, got nil")
	}
}

// TestCreateStemcell_MissingImagePath verifies CloudError when args[0] is absent.
func TestCreateStemcell_MissingImagePath(t *testing.T) {
	t.Parallel()

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when image_path is missing, got nil")
	}
}

// TestCreateStemcell_EmptyImagePath verifies CloudError when image_path is "".
func TestCreateStemcell_EmptyImagePath(t *testing.T) {
	t.Parallel()

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	args := []json.RawMessage{marshalArg(t, ""), marshalArg(t, map[string]any{})}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for empty image_path, got nil")
	}
}

// TestCreateStemcell_BadCloudProperties verifies CloudError when cloud_properties
// is a JSON array rather than an object.
func TestCreateStemcell_BadCloudProperties(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	client := defaultStemcellClient()
	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	args := []json.RawMessage{marshalArg(t, imgPath), json.RawMessage(`[1, 2, 3]`)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when cloud_properties is not an object, got nil")
	}
}

// TestCreateStemcell_NoNodeConfigured verifies CloudError when config.Node is empty.
func TestCreateStemcell_NoNodeConfigured(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	client := defaultStemcellClient()
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
	h := handlers.HandleCreateStemcell(deps)

	// name + version required — supply them so validation reaches node check.
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when node is empty, got nil")
	}
}

// ============================================================
// Tests: disk format translation (retained — pveDiskFormat still used)
// ============================================================

// captureUploadsForFormat invokes create_stemcell with the given disk_format
// and returns the qcow2 upload filename captured by the spy.
func captureUploadsForFormat(t *testing.T, diskFormat string) string {
	t.Helper()

	imgPath := tempImageFile(t)

	var capturedFilename string
	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, filename string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			if capturedFilename == "" {
				// First call = qcow2 filename.
				capturedFilename = filename
			}
			return "", nil
		},
	}

	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "test-os", "version": "1.0", "disk_format": diskFormat}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}
	if _, err := h.Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error for disk_format=%q: %v", diskFormat, err)
	}
	return capturedFilename
}

// TestCreateStemcell_OpenstackFormatTranslatedToQcow2 confirms that a stemcell
// advertising disk_format=openstack-qcow2 uploads with the .qcow2 extension.
// The upload filename is always .qcow2 (BuildStemcellFilename always produces
// .qcow2); the format translation affects future VM create_vm import-from calls.
func TestCreateStemcell_OpenstackFormatTranslatedToQcow2(t *testing.T) {
	t.Parallel()
	filename := captureUploadsForFormat(t, "openstack-qcow2")
	if !containsSubstr(filename, ".qcow2") {
		t.Errorf("upload filename %q does not end in .qcow2", filename)
	}
}

// TestCreateStemcell_OpenstackRawTranslatedToRaw mirrors the qcow2 case
// for the raw variant. Upload filename still .qcow2 (canonical stemcell
// container format); the disk_format in the volume notes carries the raw hint.
func TestCreateStemcell_OpenstackRawTranslatedToRaw(t *testing.T) {
	t.Parallel()
	filename := captureUploadsForFormat(t, "openstack-raw")
	if !containsSubstr(filename, ".qcow2") {
		t.Errorf("upload filename %q does not end in .qcow2 (stemcell container always .qcow2)", filename)
	}
}

// TestCreateStemcell_CIDFormat verifies the returned CID matches the canonical
// "<storage>:import/<filename>" format and that the filename encodes name,
// version, and sha8 predictably.
func TestCreateStemcell_CIDFormat(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)
	wantSHA := computeFileSHA(t, imgPath)
	sha8 := wantSHA[:8]

	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			return "", nil
		},
	}
	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}

	wantPrefix := "local:import/bosh-stemcell-ubuntu-jammy-1.234-"
	if !containsSubstr(cid, wantPrefix) {
		t.Errorf("CID %q does not start with %q", cid, wantPrefix)
	}
	if !containsSubstr(cid, sha8) {
		t.Errorf("CID %q does not contain sha8 %q", cid, sha8)
	}
	if !containsSubstr(cid, ".qcow2") {
		t.Errorf("CID %q does not end with .qcow2", cid)
	}
	// CID must parse successfully via ParseStemcellCID.
	storage, volPath, parseErr := pve.ParseStemcellCID(cid)
	if parseErr != nil {
		t.Fatalf("ParseStemcellCID(%q) failed: %v", cid, parseErr)
	}
	if storage != "local" {
		t.Errorf("parsed storage = %q; want %q", storage, "local")
	}
	if !containsSubstr(volPath, "import/") {
		t.Errorf("parsed volumePath = %q; does not start with import/", volPath)
	}
}

// TestCreateStemcell_QCow2FileBarePassthrough verifies that a bare qcow2 file
// (with QCOW2 magic bytes) is passed through without extraction and produces
// a valid CID.
func TestCreateStemcell_QCow2FileBarePassthrough(t *testing.T) {
	t.Parallel()

	// Construct a file with QCOW2 magic header: 'Q','F','I',0xFB.
	qcow2Magic := []byte{'Q', 'F', 'I', 0xFB, 0x00, 0x00, 0x00, 0x03}
	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.qcow2")
	if err != nil {
		t.Fatalf("create temp qcow2 file: %v", err)
	}
	_, _ = f.Write(qcow2Magic)
	_, _ = f.Write(bytes.Repeat([]byte{0x00}, 512))
	_ = f.Close()
	imgPath := f.Name()

	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			return "", nil
		},
	}
	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-focal", "version": "99.1", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for bare qcow2 passthrough: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil; expected stemcell_cid string")
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	if !containsSubstr(cid, "local:import/bosh-stemcell-ubuntu-focal-99.1-") {
		t.Errorf("CID %q does not match expected prefix", cid)
	}
}

// ============================================================
// Tests: tar extraction — B10 selection and magic-byte checks
// ============================================================

// makeStemcellTar builds an in-memory gzip+tar archive containing the given
// files and writes it to a temp file. Each entry in files maps filename to
// content bytes.
func makeStemcellTar(t *testing.T, files map[string][]byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.tgz")
	if err != nil {
		t.Fatalf("makeStemcellTar: create: %v", err)
	}

	gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		t.Fatalf("makeStemcellTar: gzip writer: %v", err)
	}
	tw := tar.NewWriter(gw)

	for name, data := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(data)),
			Mode:     0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("makeStemcellTar: write header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("makeStemcellTar: write data %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close file: %v", err)
	}
	return f.Name()
}

// qcow2Bytes returns a minimal valid-magic qcow2 header padded to size bytes.
func qcow2Bytes(size int) []byte {
	b := make([]byte, size)
	b[0] = 'Q'
	b[1] = 'F'
	b[2] = 'I'
	b[3] = 0xFB
	return b
}

// TestResolveStemcell_PrefersImgOverLargerNonImg verifies that when a tarball
// contains a smaller root.img and a larger binary blob, the .img file wins.
// This is the B10 regression test: the old code picked the largest file by
// byte count regardless of suffix, causing a non-image blob to be uploaded.
func TestResolveStemcell_PrefersImgOverLargerNonImg(t *testing.T) {
	t.Parallel()

	// root.img: 2 MiB with qcow2 magic — correct disk image.
	// extras.bin: 5 MiB with arbitrary bytes — should be ignored.
	imgData := qcow2Bytes(2 * 1024 * 1024)
	binData := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, (5*1024*1024)/4)

	tgzPath := makeStemcellTar(t, map[string][]byte{
		"root.img":   imgData,
		"extras.bin": binData,
	})

	var uploadedBytes []byte
	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			data, err := io.ReadAll(body)
			if err != nil {
				return "", err
			}
			uploadedBytes = data
			return "", nil
		},
	}
	client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	if _, err := h.Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(uploadedBytes) < 4 {
		t.Fatalf("upload too short (%d bytes); expected qcow2 content", len(uploadedBytes))
	}
	// Verify the uploaded bytes are from root.img (qcow2 magic), not extras.bin.
	if uploadedBytes[0] != 'Q' || uploadedBytes[1] != 'F' || uploadedBytes[2] != 'I' || uploadedBytes[3] != 0xFB {
		t.Errorf("upload does not start with qcow2 magic; first 4 bytes: %#x %#x %#x %#x",
			uploadedBytes[0], uploadedBytes[1], uploadedBytes[2], uploadedBytes[3])
	}
}

// TestResolveStemcell_RejectsUnknownMagic verifies that a tarball containing
// only a root.img with unrecognised magic bytes (no qcow2/gzip/lz4 header and
// below the raw-size threshold) returns an error that names the file and the
// magic bytes. This ensures accidental upload of manifest or config files is
// blocked at extraction time.
func TestResolveStemcell_RejectsUnknownMagic(t *testing.T) {
	t.Parallel()

	// root.img with arbitrary non-disk header bytes and size below 1 MiB.
	// The four bytes 0xCA 0xFE 0xBA 0xBE are the Java class magic — clearly
	// not a disk image.
	smallJunk := make([]byte, 512*1024) // 512 KiB — below the raw-size floor
	smallJunk[0] = 0xCA
	smallJunk[1] = 0xFE
	smallJunk[2] = 0xBA
	smallJunk[3] = 0xBE

	tgzPath := makeStemcellTar(t, map[string][]byte{
		"root.img": smallJunk,
	})

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "raw"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for unknown magic bytes; got nil")
	}
	if !containsSubstr(err.Error(), "unknown magic bytes") {
		t.Errorf("error %q does not mention 'unknown magic bytes'", err.Error())
	}
}

// TestResolveStemcell_TarballExceedsMaxSize_Errors verifies that a tarball
// whose candidate entries declare a cumulative size above MaxStemcellTotalExtract
// is rejected before any disk I/O, returning an error that mentions the limit.
//
// The test constructs a minimal gzip+tar where a single .img entry declares a
// 33 GiB header size but provides only a 2 MiB payload. The extraction guard
// fires on the declared size, so the test completes instantly without allocating
// 33 GiB on disk.
func TestResolveStemcell_TarballExceedsMaxSize_Errors(t *testing.T) {
	t.Parallel()

	// Build a gzip+tar with one .img entry whose declared size exceeds 32 GiB.
	// The actual body written is only 2 MiB (qcow2 magic + padding) so the test
	// is fast; the guard triggers on hdr.Size, not on bytes written to disk.
	const declaredSize = 33 * 1024 * 1024 * 1024 // 33 GiB — above the 32 GiB cap

	f, err := os.CreateTemp(t.TempDir(), "stemcell-tarbomb-*.tgz")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		_ = f.Close()
		t.Fatalf("gzip writer: %v", err)
	}
	tw := tar.NewWriter(gw)

	body := qcow2Bytes(2 * 1024 * 1024) // 2 MiB with qcow2 magic
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "root.img",
		Size:     declaredSize, // declares 33 GiB — triggers the guard
		Mode:     0o644,
	}
	if werr := tw.WriteHeader(hdr); werr != nil {
		t.Fatalf("write tar header: %v", werr)
	}
	if _, werr := tw.Write(body); werr != nil {
		t.Fatalf("write tar body: %v", werr)
	}
	// Do NOT close tw/gw normally — the declared size exceeds the body, which
	// makes a well-formed tar impossible. We flush what we have and close
	// the underlying gzip writer so the file is readable up to the guard check.
	_ = gw.Flush()
	_ = gw.Close()
	_ = f.Close()
	tgzPath := f.Name()

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	_, err = h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for tar-bomb (declared size > 32GB); got nil")
	}
	if !containsSubstr(err.Error(), "exceed") {
		t.Errorf("error %q does not mention 'exceed'; expected the MaxStemcellTotalExtract message", err.Error())
	}
}

// ============================================================
// Compile-time interface checks
// ============================================================

// Verify stemcellMockClient fully implements pve.Client.
var _ pve.Client = (*stemcellMockClient)(nil)

// Verify that pve.Backend is fully implemented by localBackend.
var _ pve.Backend = (*localBackend)(nil)

// Verify that pve.BackendResolver is fully implemented by localBackendResolver.
var _ pve.BackendResolver = (*localBackendResolver)(nil)
