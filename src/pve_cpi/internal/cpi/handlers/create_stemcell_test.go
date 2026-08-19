package handlers_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	pveerr "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ============================================================
// Mock infrastructure shared across stemcell test files.
// ============================================================

// noopPoolService is a PoolService whose AddVM is a silent no-op.
// Used by mocks when pool assignment is not under test.
type noopPoolService struct{}

func (n *noopPoolService) AddVM(_ context.Context, _ string, _ int64) error { return nil }
func (n *noopPoolService) CreatePool(_ context.Context, _, _ string) error  { return nil }
func (n *noopPoolService) DeletePool(_ context.Context, _ string) error     { return nil }
func (n *noopPoolService) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// stemcellMockClient implements pve.Client. All services default to no-ops or
// embed the interface (panics on unknown calls).
type stemcellMockClient struct {
	qemuSvc           sdkqemu.Service
	nodesSvc          sdknodes.Service
	tasksSvc          sdktasks.Service
	clusterSvc        sdkcluster.Service
	storageSvc        sdkstorage.Service
	clusterStorageSvc sdkclusterstorage.Service
	// poolsSvc controls pool assignment; nil uses noopPoolService (silent no-op).
	poolsSvc pve.PoolService
}

func (m *stemcellMockClient) QEMU() sdkqemu.Service           { return m.qemuSvc }
func (m *stemcellMockClient) Storage() sdkstorage.Service     { return m.storageSvc }
func (m *stemcellMockClient) CloudInit() sdkcloudinit.Service { return nil }
func (m *stemcellMockClient) Tasks() sdktasks.Service         { return m.tasksSvc }
func (m *stemcellMockClient) Nodes() sdknodes.Service         { return m.nodesSvc }
func (m *stemcellMockClient) Cluster() sdkcluster.Service     { return m.clusterSvc }
func (m *stemcellMockClient) ClusterStorage() sdkclusterstorage.Service {
	return m.clusterStorageSvc
}
func (m *stemcellMockClient) Pools() pve.PoolService {
	if m.poolsSvc != nil {
		return m.poolsSvc
	}
	return &noopPoolService{}
}

// stemcellMockClusterStorage satisfies sdkclusterstorage.Service.
// Only ListStorage is wired; other methods panic on accidental call.
type stemcellMockClusterStorage struct {
	sdkclusterstorage.Service // embed nil — panics on unexpected calls
	listStorageFn             func(ctx context.Context, params *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error)
}

func (m *stemcellMockClusterStorage) ListStorage(ctx context.Context, params *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	if m.listStorageFn != nil {
		return m.listStorageFn(ctx, params)
	}
	// Default: empty list — no storages visible. Tests that exercise
	// storage policy must supply listStorageFn.
	empty := sdkclusterstorage.ListStorageResponse{}
	return &empty, nil
}

// lightStemcellClusterStorage builds a stemcellMockClusterStorage whose
// ListStorage returns a single entry with the given storage name and type.
// shared=1 when isShared is true, 0 otherwise. nodes is always empty (""),
// meaning no node restriction.
func lightStemcellClusterStorage(storageName, storageType string, isShared bool) *stemcellMockClusterStorage {
	return &stemcellMockClusterStorage{
		listStorageFn: func(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
			shared := 0
			if isShared {
				shared = 1
			}
			raw, _ := json.Marshal(map[string]any{
				"storage": storageName,
				"type":    storageType,
				"shared":  shared,
				"nodes":   "",
			})
			resp := sdkclusterstorage.ListStorageResponse{raw}
			return &resp, nil
		},
	}
}

// stemcellMockQEMU satisfies sdkqemu.Service. Only Create and Config are wired.
// templateFn removed — the new flow never calls QEMU().Template().
type stemcellMockQEMU struct {
	sdkqemu.Service // embed nil — panics on unmocked methods
	createFn        func(ctx context.Context, node string, params map[string]any) (string, error)
	configFn        func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (m *stemcellMockQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "UPID:node1:create:ok", nil
}

func (m *stemcellMockQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if m.configFn != nil {
		return m.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}

// stemcellMockNodes satisfies sdknodes.Service for create_stemcell tests.
// UpdateQemuConfig and DeleteQemu are wired for legacy compat;
// ListStorageContent is wired for the dedup path;
// ListQemu is wired for FindTemplateByName (ensureTemplateVM idempotency).
type stemcellMockNodes struct {
	sdknodes.Service // embed nil — panics on unmocked methods
	updateConfigFn   func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteQemuFn     func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	listStorageFn    func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
	// listQemuFn is called by FindTemplateByName inside ensureTemplateVM.
	// Default (nil): returns an empty list (no templates found).
	listQemuFn func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	// createQemuTemplateFn is called by MakeTemplate inside ensureTemplateVM.
	// Default (nil): returns synchronous success with no UPID.
	createQemuTemplateFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error)
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

func (m *stemcellMockNodes) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if m.listQemuFn != nil {
		return m.listQemuFn(ctx, node, params)
	}
	// Default: empty list — no templates found; ensureTemplateVM proceeds to create.
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

// createQemuTemplateFn is the optional fn backing CreateQemuTemplate in stemcellMockNodes.
// createQemuTemplateFn field is added below inside stemcellMockNodes; this
// method routes through it so MakeTemplate (pve.MakeTemplate) can be tested.
func (m *stemcellMockNodes) CreateQemuTemplate(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
	if m.createQemuTemplateFn != nil {
		return m.createQemuTemplateFn(ctx, node, vmid, params)
	}
	// Default: synchronous success, no UPID (freeze completed immediately).
	raw := sdknodes.CreateQemuTemplateResponse(`""`)
	return &raw, nil
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

func (m *stemcellMockTasks) WaitForUPID(ctx context.Context, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	panic("stemcellMockTasks.WaitForUPID: not expected in stemcell tests")
}

func (m *stemcellMockTasks) GetStatus(_ context.Context, _, upid string) (*sdktasks.Status, error) {
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
	nodeEntry, _ := json.Marshal(map[string]string{"node": vmNode})
	resp := sdkcluster.ListConfigNodesResponse{nodeEntry}
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

// buildLightStemcellClient constructs a stemcellMockClient wired for light
// stemcell tests. clusterStorage drives handlerPolicyDeps.StorageInfo;
// clusterNodes drives clusterNodeCount via cluster.ListConfigNodes.
func buildLightStemcellClient(
	clusterStorage *stemcellMockClusterStorage,
	nodes *stemcellMockNodes,
	cluster *stemcellMockCluster,
) *stemcellMockClient {
	if nodes == nil {
		nodes = &stemcellMockNodes{}
	}
	if cluster == nil {
		cluster = &stemcellMockCluster{}
	}
	return &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          nodes,
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        cluster,
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: clusterStorage,
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
			Node:            vmNode,
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

// localBackendResolver is a test BackendResolver that always returns BackendLocal.
// Used to trigger the shared-storage check in validateStemcellStorageShared.
type localBackendResolver struct{}

func (r *localBackendResolver) Resolve(_ context.Context, storage string) (pve.Backend, error) {
	return &localBackend{}, nil
}

type localBackend struct{}

func (b *localBackend) Kind() pve.BackendKind { return pve.BackendLocal }
func (b *localBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return vmNode, nil
}
func (b *localBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return vmNode, nil
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
	var (
		uploadsMu sync.Mutex
		uploads   []uploadCall
	)

	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, content, filename string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			uploadsMu.Lock()
			uploads = append(uploads, uploadCall{content: content, filename: filename})
			uploadsMu.Unlock()
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

	// New flow: the tarball-upload path is always CPI-owned bytes → :heavy:.
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.234-" + sha8 + ".qcow2"
	wantCID := pve.BuildHeavyStemcellCID("local", wantFilename)
	if cidStr != wantCID {
		t.Errorf("CID = %q; want %q", cidStr, wantCID)
	}

	// Upload still happens: qcow2 is staged to storage before template creation.
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

// TestCreateStemcell_BoshCPITag verifies that the "bosh-cpi" ownership tag is
// present in the QEMU.Create params["tags"] field on every successful
// create_stemcell call, regardless of other stemcell cloud_properties.
func TestCreateStemcell_BoshCPITag(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)

	var (
		mu         sync.Mutex
		createTags string
	)

	q := &stemcellMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			mu.Lock()
			if v, ok := params["tags"].(string); ok {
				createTags = v
			}
			mu.Unlock()
			return "UPID:node1:create:ok", nil
		},
	}

	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			return "", nil
		},
	}

	client := buildStemcellClient(q, &stemcellMockNodes{}, &stemcellMockTasks{}, &stemcellMockCluster{})
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-bosh-cpi-tag"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	tags := createTags
	mu.Unlock()

	if !strings.Contains(tags, "bosh-cpi") {
		t.Errorf("create_stemcell QEMU.Create tags = %q; must contain \"bosh-cpi\"", tags)
	}
}

// TestCreateStemcell_Dedup_SameFilename verifies that when the qcow2 volume
// already exists on storage AND a matching cache template already exists
// (cluster-scoped sha-tag lookup), Upload is not called and the handler
// returns the same :heavy: CID the qcow2 filename determines — CID identity
// no longer depends on which VMID backs the cache template.
func TestCreateStemcell_Dedup_SameFilename(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)
	wantSHA := computeFileSHA(t, imgPath)
	sha8 := wantSHA[:8]
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.234-" + sha8 + ".qcow2"
	existingVolid := "local:import/" + wantFilename

	// existingVMID is the VMID of the pre-existing cache template, discovered
	// via the cluster-scoped sha-tag lookup (not ListQemu).
	const existingVMID = int64(6042)
	templateName := pve.BuildTemplateNameWithSHA("ubuntu-jammy", "1.234", sha8)

	var uploadCalls []struct{}
	storageSvc := &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalls = append(uploadCalls, struct{}{})
			return "", nil
		},
	}

	// ListStorageContent returns the matching qcow2 volume (dedup hit).
	nodesSvc := &stemcellMockNodes{
		listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			entry, _ := json.Marshal(map[string]string{"volid": existingVolid})
			resp := sdknodes.ListStorageContentResponse{entry}
			return &resp, nil
		},
	}
	// Cluster().ListResources returns the matching cache template
	// (pve.FindTemplatesBySHATagCluster hit).
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(existingVMID, vmNode, templateName, cacheTemplateTags(sha8)),
			}
			return &items, nil
		},
	}

	client := buildStemcellClient(&stemcellMockQEMU{}, nodesSvc, &stemcellMockTasks{}, clusterSvc)
	client.storageSvc = storageSvc

	deps := makeDeps(client)
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cidStr, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	// Dedup returns the deterministic :heavy: CID (storage+filename), not a
	// VMID-bearing identifier.
	wantCID := pve.BuildHeavyStemcellCID("local", wantFilename)
	if cidStr != wantCID {
		t.Errorf("CID = %q; want %q", cidStr, wantCID)
	}
	if len(uploadCalls) != 0 {
		t.Error("Upload called despite dedup hit; should be skipped")
	}
}

// TestCreateStemcell_RejectLocalStemcellStorage verifies that a local-only
// stemcell storage on a multi-node cluster causes the handler to return an error
// containing "shared" (the canonical rejection message fragment).
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
		t.Fatal("expected error for local storage on multi-node cluster, got nil")
	}
	if !strings.Contains(err.Error(), "shared") {
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
	if !strings.Contains(err.Error(), "name") {
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
	if !strings.Contains(err.Error(), "version") {
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
// Tests: disk_format aliases accepted end-to-end (canonical .qcow2 upload name)
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
	if !strings.Contains(filename, ".qcow2") {
		t.Errorf("upload filename %q does not end in .qcow2", filename)
	}
}

// TestCreateStemcell_OpenstackRawTranslatedToRaw mirrors the qcow2 case
// for the raw variant. Upload filename still .qcow2 (canonical stemcell
// container format); the disk_format in the volume notes carries the raw hint.
func TestCreateStemcell_OpenstackRawTranslatedToRaw(t *testing.T) {
	t.Parallel()
	filename := captureUploadsForFormat(t, "openstack-raw")
	if !strings.Contains(filename, ".qcow2") {
		t.Errorf("upload filename %q does not end in .qcow2 (stemcell container always .qcow2)", filename)
	}
}

// TestCreateStemcell_CIDFormat verifies the returned CID is a valid template CID
// ("template:<vmid>") with a positive VMID. The qcow2 filename (encoding name,
// version, and sha8) is an internal staging detail used by ensureTemplateVM but
// is not exposed in the returned CID — the CID now identifies the template VM
// rather than the raw upload volume.
func TestCreateStemcell_CIDFormat(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t)
	_ = computeFileSHA(t, imgPath) // ensure file is readable; sha is internal only

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

	// CID must be a well-formed :heavy: path-identity CID (the tarball-upload
	// path is always CPI-owned bytes).
	kind, storage, volumePath, parseErr := pve.ParseStemcellPathCID(cid)
	if parseErr != nil {
		t.Fatalf("ParseStemcellPathCID(%q) failed: %v", cid, parseErr)
	}
	if kind != pve.StemcellKindHeavy {
		t.Errorf("CID %q kind = %q; want %q", cid, kind, pve.StemcellKindHeavy)
	}
	if storage != "local" {
		t.Errorf("CID %q storage = %q; want %q", cid, storage, "local")
	}
	if !strings.HasPrefix(volumePath, "import/") {
		t.Errorf("CID %q volume path = %q; want \"import/...\"", cid, volumePath)
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
	// Bare qcow2 passthrough still produces a well-formed :heavy: CID — the
	// handler builds/reuses a PVE cache template from the uploaded image
	// regardless of input format.
	kind, _, _, parseErr := pve.ParseStemcellPathCID(cid)
	if parseErr != nil {
		t.Fatalf("ParseStemcellPathCID(%q) failed: %v", cid, parseErr)
	}
	if kind != pve.StemcellKindHeavy {
		t.Errorf("CID %q kind = %q; want %q", cid, kind, pve.StemcellKindHeavy)
	}
}

// ============================================================
// Tests: tar extraction — B10 selection and magic-byte checks
// ============================================================

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
	if !cpierrors.IsType(err, cpierrors.TypeStemcellMagicMismatch) {
		t.Errorf("error type: want StemcellMagicMismatch for unknown magic bytes, got %T %v", err, err)
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
	if !cpierrors.IsType(err, cpierrors.TypeStemcellExtractCap) {
		t.Errorf("error type: want StemcellExtractCap for tar-bomb, got %T %v", err, err)
	}
}

// TestCreateStemcell_RejectsNegativeHdrSize verifies that a tarball whose
// regular-file entry declares a negative Size (via the GNU base-256 sign
// extension) is rejected with a Cloud error before any disk I/O.
//
// Two-layered defense: archive/tar.Reader's handleRegularFile rejects
// negative Size with ErrHeader at parse time, and our resolveStemcellImage
// wraps that as a Cloud error before returning. The production code also
// carries an explicit hdr.Size < 0 guard as belt-and-suspenders for the
// case where a future archive/tar release loosens its check, or where a
// caller bypasses archive/tar entirely; that guard returns a more specific
// "malformed tar header (negative size N for NAME)" message.
//
// This test asserts the outer behavior: a negative-size tar is rejected
// with a Cloud error and DetachDisk-equivalent side effects do not occur.
// It accepts either the archive/tar reader's message ("invalid tar header")
// or the explicit guard message ("negative size") so the test stays valid
// across Go stdlib revisions.
func TestCreateStemcell_RejectsNegativeHdrSize(t *testing.T) {
	t.Parallel()

	tgzPath := makeNegativeSizeTar(t, "root.img")

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for negative tar header size; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeStemcellInvalidTar) {
		t.Errorf("error type: want StemcellInvalidTar for negative tar header, got %T %v", err, err)
	}
}

// TestCreateStemcell_RejectsDuplicateEntryBasenames verifies a tarball with
// two entries whose basenames collide after path flattening is rejected as
// invalid. Extraction flattens entry paths to basenames (path-traversal
// defense), so "a/root.img" and "b/root.img" would write to one destination
// with the second truncating the first while both candidate records keep
// their own size and SHA — decoupling the recorded digest (which flows into
// the stemcell filename, CID, and sha-tag dedup) from the file's actual
// bytes. A legitimate BOSH stemcell never carries duplicate basenames.
func TestCreateStemcell_RejectsDuplicateEntryBasenames(t *testing.T) {
	t.Parallel()

	tgzPath := makeStemcellTar(t, map[string][]byte{
		"a/root.img": qcow2Bytes(2 * 1024 * 1024),
		"b/root.img": qcow2Bytes(3 * 1024 * 1024),
	})

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for duplicate entry basenames; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeStemcellInvalidTar) {
		t.Errorf("error type: want StemcellInvalidTar for duplicate basenames, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "duplicate entry basename") {
		t.Errorf("error should name the duplicate-basename guard: %v", err)
	}
}

// TestCreateStemcell_NoImgCandidateError verifies that a tarball whose
// only candidate is a zero-byte .img file lands in the imgPath=="" branch
// of the candidate-selection logic and is rejected with a Cloud error that
// names the source tarball. Without this guard the upload path would
// receive an empty source path and attempt to read zero bytes — a silent
// failure mode that produces an unusable stemcell volume in PVE.
//
// Selection arithmetic: a 0-byte .img file bypasses the small-skip filter
// (which only drops non-.img files below 1 MiB), so it survives as a
// tarCandidate with size=0. The selection loop compares c.size > imgSize
// (imgSize starts at 0), so the comparison is false and imgPath stays "".
// The non-.img fallback branch also stays empty because the only candidate
// is an .img. After the fallback step imgPath is still "" — the new guard
// then surfaces a specific error rather than letting the empty path leak
// into the magic-byte stat and upload steps.
func TestCreateStemcell_NoImgCandidateError(t *testing.T) {
	t.Parallel()

	// Single zero-byte non-root .img file: isImg=true (so it survives the
	// small-file skip filter), size=0 (so c.size > imgSize == 0 is false),
	// and name != "root.img" (so the equal-size tiebreaker does not fire
	// either). The fallback non-.img branch is also empty because the only
	// candidate IS an .img. The result is imgPath == "" after selection —
	// exactly the branch the new guard protects.
	tgzPath := makeStemcellTar(t, map[string][]byte{
		"empty.img": nil, // zero-byte declared and written; not "root.img"
	})

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, tgzPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when tarball has no usable disk image candidate; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeStemcellNoCandidate) {
		t.Errorf("error type: want StemcellNoCandidate for empty tarball, got %T %v", err, err)
	}
}

// TestCreateStemcell_RejectsPathOutsideStagingRoot verifies the path-containment
// guard. The handler must refuse to open an image_path that
// resolves outside the permitted staging root (os.TempDir()). The error message
// names the path and the "outside permitted staging root" phrase so operators
// see immediately that the request was rejected by policy, not by I/O failure.
//
// Two paths are exercised:
//   - /etc/passwd — a classic probe target that exists on every Linux host.
//   - A constructed path under /var that does not exist; the containment guard
//     must fire BEFORE the stat call so a non-existent escape still surfaces
//     the policy error (not a "file not found" leak).
func TestCreateStemcell_RejectsPathOutsideStagingRoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		imagePath string
	}{
		{name: "etc-passwd", imagePath: "/etc/passwd"},
		{name: "var-nonexistent", imagePath: "/var/no-such-stemcell-staging/img.tgz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deps := makeDeps(defaultStemcellClient())
			h := handlers.HandleCreateStemcell(deps)

			cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
			args := []json.RawMessage{marshalArg(t, tc.imagePath), marshalArg(t, cp)}

			_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
			if err == nil {
				t.Fatalf("expected containment rejection for %q; got nil", tc.imagePath)
			}
			if !cpierrors.IsType(err, cpierrors.TypeStemcellEscapedRoot) {
				t.Errorf("error type: want StemcellEscapedRoot for escaped path, got %T %v", err, err)
			}
		})
	}
}

// TestCreateStemcell_AcceptsPathUnderTempDir is the positive companion to
// TestCreateStemcell_RejectsPathOutsideStagingRoot: a path under os.TempDir()
// (via t.TempDir()) must pass the containment check and proceed to the next
// validation stage. The test asserts the handler does NOT short-circuit with
// the staging-root rejection error for a well-located path; it may still fail
// at later stages (magic-byte detection, missing cluster mocks, etc.), which
// is fine — the assertion is specifically that the policy guard is not the
// failure mode.
func TestCreateStemcell_AcceptsPathUnderTempDir(t *testing.T) {
	t.Parallel()

	imgPath := tempImageFile(t) // lives under os.TempDir() via t.TempDir()

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	// Any later-stage error is acceptable; this test only guards against the
	// staging-root rejection misfiring on a legitimate path.
	if err != nil && strings.Contains(err.Error(), "outside permitted staging root") {
		t.Errorf("staging-root guard rejected a legitimate temp path %q: %v", imgPath, err)
	}
}

// ============================================================
// Tests: parseStemcellCloudProps — light-stemcell fields
// ============================================================

// parseStemcellCloudPropsExported is a thin shim that exercises the package-internal
// parseStemcellCloudProps via the exported HandleCreateStemcell handler by extracting
// the parsed result from observable side-effects. Because stemcellCloudProps is
// package-internal, direct instantiation is not available from _test packages.
// Instead we use a dedicated test helper that invokes the exported constructor and
// reads back the fields via the exported helper methods IsLight and LightMode
// (also tested here), plus a targeted validateLightMutex error case via the handler.
//
// For field-level assertions that require access to unexported struct fields
// (ImageURLAuth, Node), we use a white-box test file (create_stemcell_internal_test.go)
// in the handlers package (package handlers). The table-driven tests below cover
// observable behaviour accessible from the _test package.

// TestParseStemcellCloudProps_LightFields_BlackBox exercises the light-field
// parsing through observable handler behaviour. IsLight and LightMode are
// verified indirectly: for preuploaded/fetch modes the handler currently still
// falls through to name/version validation (light path not yet routed), so we
// inspect the error message to confirm parsing proceeded past the mutex check.
//
// Direct field access (ImageURLAuth, Node) is validated in the white-box test
// file create_stemcell_wb_test.go (package handlers).
func TestParseStemcellCloudProps_LightFields_BlackBox(t *testing.T) {
	t.Parallel()

	// light cases: image_id only, image_url only — handler reaches name validation,
	// not a mutex error. This confirms parsing did not short-circuit on these inputs.
	cases := []struct {
		name          string
		cp            map[string]any
		wantMutexErr  bool
		wantMutexFrag string
	}{
		{
			name:         "image_id only",
			cp:           map[string]any{"image_id": "nfs:import/x.qcow2"},
			wantMutexErr: false,
		},
		{
			name:         "image_url only",
			cp:           map[string]any{"image_url": "https://example.com/x.qcow2"},
			wantMutexErr: false,
		},
		{
			name:          "both image_id and image_url",
			cp:            map[string]any{"image_id": "nfs:import/x.qcow2", "image_url": "https://example.com/x.qcow2"},
			wantMutexErr:  true,
			wantMutexFrag: "mutually exclusive",
		},
		{
			name:         "neither set",
			cp:           map[string]any{},
			wantMutexErr: false,
		},
		{
			name:         "node only",
			cp:           map[string]any{"node": "pve1"},
			wantMutexErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			imgPath := tempImageFile(t)
			deps := makeDeps(defaultStemcellClient())
			h := handlers.HandleCreateStemcell(deps)

			args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, tc.cp)}
			_, err := h.Handle(context.Background(), args, jsonrpc.Context{})

			if tc.wantMutexErr {
				if err == nil {
					t.Fatal("expected mutex error; got nil")
				}
				if !strings.Contains(err.Error(), tc.wantMutexFrag) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantMutexFrag)
				}
				return
			}
			// Non-mutex cases: error must NOT be the mutex error. Later-stage
			// errors (missing name/version) are acceptable.
			if err != nil && strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("unexpected mutex error for case %q: %v", tc.name, err)
			}
		})
	}
}

// ============================================================
// Tests: stemcellCloudProps.validateLightMutex
// ============================================================

// TestStemcellCloudProps_validateLightMutex exercises mutual-exclusion logic
// through HandleCreateStemcell since stemcellCloudProps is package-internal.
func TestStemcellCloudProps_validateLightMutex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cp      map[string]any
		wantErr bool
		errFrag string
	}{
		{
			name:    "both set — error",
			cp:      map[string]any{"image_id": "local:import/a.qcow2", "image_url": "https://example.com/b.qcow2"},
			wantErr: true,
			errFrag: "mutually exclusive",
		},
		{
			name:    "image_id only — ok",
			cp:      map[string]any{"image_id": "local:import/a.qcow2"},
			wantErr: false,
		},
		{
			name:    "image_url only — ok",
			cp:      map[string]any{"image_url": "https://example.com/b.qcow2"},
			wantErr: false,
		},
		{
			name:    "neither — ok",
			cp:      map[string]any{},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			imgPath := tempImageFile(t)
			deps := makeDeps(defaultStemcellClient())
			h := handlers.HandleCreateStemcell(deps)

			args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, tc.cp)}
			_, err := h.Handle(context.Background(), args, jsonrpc.Context{})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q; got nil", tc.errFrag)
				}
				if !strings.Contains(err.Error(), tc.errFrag) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errFrag)
				}
				return
			}
			// No mutex error expected. Later-stage errors (name/version missing) are fine.
			if err != nil && strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("unexpected mutual-exclusion error: %v", err)
			}
		})
	}
}

// ============================================================
// Tests: HandleCreateStemcell — light stemcell (pre-uploaded)
// ============================================================

// lightStemcellDeps builds a Deps suitable for light stemcell tests.
// clusterStorage drives handlerPolicyDeps.StorageInfo;
// nodeListFn drives ListStorageContent (existence check);
// configNodeCount controls how many nodes the cluster mock reports.
func lightStemcellDeps(
	t *testing.T,
	clusterStorage *stemcellMockClusterStorage,
	nodeListFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error),
	configNodeCount int,
) handlers.Deps {
	t.Helper()
	var clusterNodes sdkcluster.ListConfigNodesResponse
	for i := 0; i < configNodeCount; i++ {
		raw, _ := json.Marshal(map[string]string{"node": "pve-node1"})
		clusterNodes = append(clusterNodes, raw)
	}
	cluster := &stemcellMockCluster{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			return &clusterNodes, nil
		},
	}
	nodes := &stemcellMockNodes{listStorageFn: nodeListFn}
	client := buildLightStemcellClient(clusterStorage, nodes, cluster)
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:            vmNode,
			StemcellStorage: "nfs",
			VMStorage:       "nfs",
			DiskStorage:     "nfs",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// existingVolumeListFn returns a ListStorageContent that reports qcow2Filename as present.
//
//nolint:unparam // qcow2Filename kept for readability; future tests may vary it
func existingVolumeListFn(storage, qcow2Filename string) func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		volid := storage + ":import/" + qcow2Filename
		raw, _ := json.Marshal(map[string]string{"volid": volid})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}
}

// emptyVolumeListFn returns a ListStorageContent that reports no volumes.
func emptyVolumeListFn() func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		empty := sdknodes.ListStorageContentResponse{}
		return &empty, nil
	}
}

// preuploadedSHA256 is a fixed, well-formed 64-hex-character digest used by
// the preuploaded (light) stemcell test fixtures below. Its actual byte
// content is irrelevant to these tests — only its shape (valid hex, 64
// chars) matters, since preuploaded mode now requires cloud_properties.sha256
// but does not verify it against the (non-existent, /dev/null) local
// file.
const preuploadedSHA256 = "ef0c5d8d1d8ba6e1a8620b2cba931c76e3bc9049395c3e7a5d5733cc3df2983f"

// TestHandleCreateStemcell_LightPreUploaded_HappyPath verifies the end-to-end
// success path: valid image_id + sha256, shared NFS storage on a single-node
// cluster, file found on PVE — handler returns a well-formed :light: CID, no
// upload occurs.
func TestHandleCreateStemcell_LightPreUploaded_HappyPath(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	var uploadCalls []struct{}
	deps.PVE.(*stemcellMockClient).storageSvc = &stemcellMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalls = append(uploadCalls, struct{}{})
			return "", nil
		},
	}

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	// BOSH passes /dev/null for light stemcells — not a real file.
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	// Light-preuploaded returns the deterministic :light: CID.
	wantCID := pve.BuildLightStemcellCID(storageName, filename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
	if len(uploadCalls) != 0 {
		t.Error("Upload called for light stemcell; must be skipped")
	}
}

// TestHandleCreateStemcell_LightPreUploaded_MissingSHA256 verifies that a
// preuploaded stemcell without cloud_properties.sha256 is rejected with a
// clear validation error (sha256 is required for content identity
// and dedup).
func TestHandleCreateStemcell_LightPreUploaded_MissingSHA256(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		// sha256 intentionally omitted.
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing sha256; got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error %q does not mention sha256", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_MalformedSHA256 verifies that a
// preuploaded stemcell with a malformed cloud_properties.sha256 (wrong
// length / non-hex) is rejected with a clear validation error.
func TestHandleCreateStemcell_LightPreUploaded_MalformedSHA256(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   "not-hex-and-too-short",
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed sha256; got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error %q does not mention sha256", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_MalformedImageID verifies that a
// cloud_properties.image_id that cannot be parsed as a volid returns a Cloud
// error naming the bad value.
func TestHandleCreateStemcell_LightPreUploaded_MalformedImageID(t *testing.T) {
	t.Parallel()

	clusterStorage := lightStemcellClusterStorage("nfs", "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, emptyVolumeListFn(), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": "not-a-valid-volid",
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for malformed image_id; got nil")
	}
	if !strings.Contains(err.Error(), "not-a-valid-volid") {
		t.Errorf("error %q does not echo the bad image_id", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_VolumeNotFound verifies that when
// the existence check finds no matching volume, the error mentions "not found".
func TestHandleCreateStemcell_LightPreUploaded_VolumeNotFound(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, emptyVolumeListFn(), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for volume not found; got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention 'not found'", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_BlockStorageRejected verifies that
// an image_id referencing block-only storage (lvm type) is rejected by the
// storage policy with an error mentioning the block-only constraint.
func TestHandleCreateStemcell_LightPreUploaded_BlockStorageRejected(t *testing.T) {
	t.Parallel()

	const (
		storageName = "local-lvm"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	// lvm type → block-only → must be rejected.
	clusterStorage := lightStemcellClusterStorage(storageName, "lvm", false)
	deps := lightStemcellDeps(t, clusterStorage, emptyVolumeListFn(), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for block storage; got nil")
	}
	if !strings.Contains(err.Error(), "block") {
		t.Errorf("error %q does not mention 'block'", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_LocalMultiNodeNoPin verifies that
// local storage on a multi-node cluster without a cloud_properties.node pin is
// rejected with an error mentioning node pinning.
func TestHandleCreateStemcell_LightPreUploaded_LocalMultiNodeNoPin(t *testing.T) {
	t.Parallel()

	const (
		storageName = "local-dir"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	// dir type, not shared → local storage.
	clusterStorage := lightStemcellClusterStorage(storageName, "dir", false)
	// 2 nodes → must require pin.
	deps := lightStemcellDeps(t, clusterStorage, emptyVolumeListFn(), 2)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
		// node intentionally omitted.
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for local storage multi-node without node pin; got nil")
	}
	if !strings.Contains(err.Error(), "node") {
		t.Errorf("error %q does not mention 'node'", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_LocalSingleNodeAccepted verifies
// that local storage on a single-node cluster is accepted without a node pin.
func TestHandleCreateStemcell_LightPreUploaded_LocalSingleNodeAccepted(t *testing.T) {
	t.Parallel()

	const (
		storageName = "local-dir"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "dir", false)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for single-node local storage: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	wantCID := pve.BuildLightStemcellCID(storageName, filename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
}

// TestHandleCreateStemcell_LightPreUploaded_AnyStorageAccepted verifies that
// when image_id references any shared-file storage, the handler accepts it
// (there is no config-level restriction on which storage a pre-uploaded
// light stemcell may use — policy is applied by ValidateLightStemcellStorage).
func TestHandleCreateStemcell_LightPreUploaded_AnyStorageAccepted(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs-other"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error for shared-file storage: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	// The storage-policy acceptance (any shared-file storage is accepted) is
	// still validated — the CID reflects the deterministic :light: grammar.
	wantCID := pve.BuildLightStemcellCID(storageName, filename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
}

// TestHandleCreateStemcell_LightPreUploaded_StorageMatchSuccess verifies that
// shared-file storage is accepted and produces a well-formed :light: CID.
func TestHandleCreateStemcell_LightPreUploaded_StorageMatchSuccess(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs-light"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
		imageID     = storageName + ":import/" + filename
	)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error when storage matches config: %v", err)
	}
	cid, _ := result.(string)
	wantCID := pve.BuildLightStemcellCID(storageName, filename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
}

// TestHandleCreateStemcell_LightFetch_BadScheme verifies that an image_url with
// an unsupported scheme (e.g. "ftp://") is rejected before any network I/O with
// a Cloud error that names the URL. No mocking required: ResolveSource rejects
// the scheme synchronously.
func TestHandleCreateStemcell_LightFetch_BadScheme(t *testing.T) {
	t.Parallel()

	deps := makeDeps(defaultStemcellClient())
	h := handlers.HandleCreateStemcell(deps)

	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.438",
		"image_url": "ftp://example.com/ubuntu-jammy.qcow2",
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for unsupported scheme; got nil")
	}
	if !strings.Contains(err.Error(), "ftp") && !strings.Contains(err.Error(), "scheme") && !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error %q does not mention unsupported scheme", err.Error())
	}
}

// TestHandleCreateStemcell_LightFetch_BlockStorageRejected verifies that block-only
// storage (lvm type) is rejected by the storage policy before any network I/O.
// Uses the cluster-storage mock so handlerPolicyDeps.StorageInfo can classify the
// storage. No source mocking required: the policy check fires before Fetch is called.
func TestHandleCreateStemcell_LightFetch_BlockStorageRejected(t *testing.T) {
	t.Parallel()

	const storageName = "local-lvm"
	clusterStorage := lightStemcellClusterStorage(storageName, "lvm", false)
	deps := lightStemcellDeps(t, clusterStorage, emptyVolumeListFn(), 1)
	// Point StemcellStorage at the lvm pool so the fetch path resolves to it.
	deps.Config.StemcellStorage = storageName

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.438",
		"image_url": "https://example.com/ubuntu-jammy.qcow2",
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for block storage in fetch mode; got nil")
	}
	if !strings.Contains(err.Error(), "block") {
		t.Errorf("error %q does not mention 'block'", err.Error())
	}
}

// TestHandleCreateStemcell_LightPreUploaded_PathCIDAccepted verifies that
// cloud_properties.image_id may itself be a full ":light:" path-identity CID
// (not just a bare volid) — an operator re-supplying a previously-returned
// CID as image_id must work.
func TestHandleCreateStemcell_LightPreUploaded_PathCIDAccepted(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
	)
	imageID := pve.BuildLightStemcellCID(storageName, filename)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error when image_id is a :light: path CID: %v", err)
	}
	cid, _ := result.(string)
	wantCID := pve.BuildLightStemcellCID(storageName, filename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
}

// TestHandleCreateStemcell_LightPreUploaded_HeavyImageIDRejected verifies that
// a ":heavy:"-prefixed image_id is rejected: that kind asserts CPI ownership,
// which an operator-supplied preuploaded image contradicts by definition.
func TestHandleCreateStemcell_LightPreUploaded_HeavyImageIDRejected(t *testing.T) {
	t.Parallel()

	const (
		storageName = "nfs"
		filename    = "ubuntu-jammy-1.438-abc12345.qcow2"
	)
	imageID := pve.BuildHeavyStemcellCID(storageName, filename)

	clusterStorage := lightStemcellClusterStorage(storageName, "nfs", true)
	deps := lightStemcellDeps(t, clusterStorage, existingVolumeListFn(storageName, filename), 1)

	h := handlers.HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.438",
		"image_id": imageID,
		"sha256":   preuploadedSHA256,
	}
	args := []json.RawMessage{marshalArg(t, "/dev/null"), marshalArg(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for :heavy: image_id; got nil")
	}
	if !strings.Contains(err.Error(), "heavy") {
		t.Errorf("error %q does not mention the rejected :heavy: kind", err.Error())
	}
}

// ============================================================
// Compile-time interface checks
// ============================================================

// Verify stemcellMockClient fully implements pve.Client.
var _ pve.Client = (*stemcellMockClient)(nil)

// Verify stemcellMockClusterStorage fully implements sdkclusterstorage.Service
// at compile time.
var _ sdkclusterstorage.Service = (*stemcellMockClusterStorage)(nil)

// Verify that pve.Backend is fully implemented by localBackend.
var _ pve.Backend = (*localBackend)(nil)

// Verify that pve.BackendResolver is fully implemented by localBackendResolver.
var _ pve.BackendResolver = (*localBackendResolver)(nil)
