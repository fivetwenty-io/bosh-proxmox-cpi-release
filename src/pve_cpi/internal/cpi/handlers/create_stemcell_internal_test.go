package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-pve-cpi/internal/pve/stemcell_fetch"
)

// TestParseStemcellCloudProps_LightFields verifies all four new light-stemcell
// fields are populated correctly from cloud_properties input.
func TestParseStemcellCloudProps_LightFields(t *testing.T) {
	t.Parallel()

	type wantFields struct {
		imageID       string
		imageURL      string
		authNonEmpty  bool
		authBearerTok string // extracted from ImageURLAuth if non-empty
		node          string
		isLight       bool
		lightMode     string
	}

	cases := []struct {
		name string
		cp   map[string]any
		want wantFields
	}{
		{
			name: "image_id only — preuploaded",
			cp:   map[string]any{"image_id": "nfs:import/x.qcow2"},
			want: wantFields{
				imageID:   "nfs:import/x.qcow2",
				imageURL:  "",
				isLight:   true,
				lightMode: "preuploaded",
			},
		},
		{
			name: "image_url only — fetch",
			cp:   map[string]any{"image_url": "https://example.com/x.qcow2"},
			want: wantFields{
				imageID:   "",
				imageURL:  "https://example.com/x.qcow2",
				isLight:   true,
				lightMode: "fetch",
			},
		},
		{
			name: "image_url with image_url_auth bearer",
			cp: map[string]any{
				"image_url": "https://example.com/x.qcow2",
				"image_url_auth": map[string]any{
					"type":         "bearer",
					"bearer_token": "tok123",
				},
			},
			want: wantFields{
				imageURL:      "https://example.com/x.qcow2",
				authNonEmpty:  true,
				authBearerTok: "tok123",
				isLight:       true,
				lightMode:     "fetch",
			},
		},
		{
			name: "node only — no light fields",
			cp:   map[string]any{"node": "pve1"},
			want: wantFields{
				node:      "pve1",
				isLight:   false,
				lightMode: "",
			},
		},
		{
			name: "empty cp — all light fields zero",
			cp:   map[string]any{},
			want: wantFields{
				isLight:   false,
				lightMode: "",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := parseStemcellCloudProps(tc.cp)

			if p.ImageID != tc.want.imageID {
				t.Errorf("ImageID = %q; want %q", p.ImageID, tc.want.imageID)
			}
			if p.ImageURL != tc.want.imageURL {
				t.Errorf("ImageURL = %q; want %q", p.ImageURL, tc.want.imageURL)
			}
			if p.Node != tc.want.node {
				t.Errorf("Node = %q; want %q", p.Node, tc.want.node)
			}
			if p.IsLight() != tc.want.isLight {
				t.Errorf("IsLight() = %v; want %v", p.IsLight(), tc.want.isLight)
			}
			if p.LightMode() != tc.want.lightMode {
				t.Errorf("LightMode() = %q; want %q", p.LightMode(), tc.want.lightMode)
			}

			if tc.want.authNonEmpty {
				if len(p.ImageURLAuth) == 0 {
					t.Fatal("ImageURLAuth is empty; want non-empty JSON bytes")
				}
				// Re-parse to confirm bearer_token round-trips correctly.
				var auth map[string]string
				if err := json.Unmarshal(p.ImageURLAuth, &auth); err != nil {
					t.Fatalf("ImageURLAuth unmarshal: %v (raw: %s)", err, p.ImageURLAuth)
				}
				if auth["bearer_token"] != tc.want.authBearerTok {
					t.Errorf("bearer_token = %q; want %q", auth["bearer_token"], tc.want.authBearerTok)
				}
			} else {
				if len(p.ImageURLAuth) != 0 {
					t.Errorf("ImageURLAuth = %s; want empty", p.ImageURLAuth)
				}
			}
		})
	}
}

// ============================================================
// White-box tests: handleLightStemcellFetch via Deps.FetchResolver seam
// ============================================================

// mockSource is a test-only stemcellfetch.Source that returns a fixed body.
type mockSource struct {
	body          []byte
	contentLength int64
	fetchErr      error
}

func (m *mockSource) Fetch(_ context.Context, _ stemcellfetch.Reference, _ stemcellfetch.Credentials) (io.ReadCloser, int64, error) {
	if m.fetchErr != nil {
		return nil, 0, m.fetchErr
	}
	return io.NopCloser(bytes.NewReader(m.body)), m.contentLength, nil
}

// wbBuildFetchDeps constructs a Deps suitable for white-box fetch tests.
// Uses shared NFS storage on a single-node cluster so the policy check passes.
// nodeListFn controls ListStorageContent responses.
// QEMU and Tasks are wired with default no-op mocks so ensureTemplateVM can run.
func wbBuildFetchDeps(
	t *testing.T,
	nodeListFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error),
) Deps {
	t.Helper()

	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	cluster := &wbMockCluster{nodeCount: 1}
	nodesWithStorage := &wbTemplateNodes{
		wbMockNodes: wbMockNodes{listStorageFn: nodeListFn},
	}
	storage := &wbTemplateStorage{}
	qemu := &wbMockQEMU{}
	tasks := &wbMockTasks{}

	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          nodesWithStorage,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        cluster,
			storageSvc:        storage,
		},
		qemuSvc:  qemu,
		tasksSvc: tasks,
	}

	return Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "nfs",
			StemcellTemplateVMIDRangeStart: 6000,
			StemcellTemplateVMIDRangeEnd:   8999,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
}

// wbEmptyNodeListFn returns a ListStorageContent that reports no volumes.
func wbEmptyNodeListFn() func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		empty := sdknodes.ListStorageContentResponse{}
		return &empty, nil
	}
}

// wbExistingVolumeListFn returns a ListStorageContent reporting qcow2Filename present.
func wbExistingVolumeListFn(storage, qcow2Filename string) func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		volid := storage + ":import/" + qcow2Filename
		raw, _ := json.Marshal(map[string]string{"volid": volid})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}
}

// wbMockClient, wbMockClusterStorage, wbMockCluster, wbMockNodes, wbMockStorage
// are minimal PVE client mocks for white-box tests. They are separate from the
// _test package mocks to avoid import cycles (the wb file is package handlers).

// wbNoopPoolService is a PoolService no-op for white-box tests not exercising pool logic.
type wbNoopPoolService struct{}

func (n *wbNoopPoolService) AddVM(_ context.Context, _ string, _ int64) error { return nil }

type wbMockClient struct {
	nodesSvc          sdknodes.Service
	clusterStorageSvc sdkclusterstorage.Service
	clusterSvc        sdkcluster.Service
	storageSvc        sdkstorage.Service
	// poolsSvc is nil → uses wbNoopPoolService.
	poolsSvc pve.PoolService
}

func (c *wbMockClient) QEMU() sdkqemu.Service                     { return nil }
func (c *wbMockClient) Storage() sdkstorage.Service               { return c.storageSvc }
func (c *wbMockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *wbMockClient) Tasks() sdktasks.Service                   { return nil }
func (c *wbMockClient) Nodes() sdknodes.Service                   { return c.nodesSvc }
func (c *wbMockClient) Cluster() sdkcluster.Service               { return c.clusterSvc }
func (c *wbMockClient) ClusterStorage() sdkclusterstorage.Service { return c.clusterStorageSvc }
func (c *wbMockClient) Pools() pve.PoolService {
	if c.poolsSvc != nil {
		return c.poolsSvc
	}
	return &wbNoopPoolService{}
}

var _ pve.Client = (*wbMockClient)(nil)

type wbMockClusterStorage struct {
	sdkclusterstorage.Service
	storageName string
	storageType string
	isShared    bool
}

func (m *wbMockClusterStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	shared := 0
	if m.isShared {
		shared = 1
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": m.storageName,
		"type":    m.storageType,
		"shared":  shared,
		"nodes":   "",
	})
	resp := sdkclusterstorage.ListStorageResponse{raw}
	return &resp, nil
}

type wbMockCluster struct {
	sdkcluster.Service
	nodeCount int
}

func (c *wbMockCluster) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	var resp sdkcluster.ListConfigNodesResponse
	for i := 0; i < c.nodeCount; i++ {
		raw, _ := json.Marshal(map[string]string{"node": "pve-node1"})
		resp = append(resp, raw)
	}
	return &resp, nil
}

func (c *wbMockCluster) ListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	// Default: no existing VMs → AllocateWithRetry/NextVMID starts at range start.
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

type wbMockNodes struct {
	sdknodes.Service
	listStorageFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
	listQemuFn    func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
}

func (n *wbMockNodes) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	if n.listStorageFn != nil {
		return n.listStorageFn(ctx, node, storage, params)
	}
	empty := sdknodes.ListStorageContentResponse{}
	return &empty, nil
}

func (n *wbMockNodes) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if n.listQemuFn != nil {
		return n.listQemuFn(ctx, node, params)
	}
	// Default: no existing templates → ensureTemplateVM proceeds to create.
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

type wbMockStorage struct {
	sdkstorage.Service
	uploadFn func(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error)
}

func (s *wbMockStorage) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	if s.uploadFn != nil {
		return s.uploadFn(ctx, node, storage, content, filename, body)
	}
	// Drain body so upload doesn't stall.
	_, _ = io.Copy(io.Discard, body)
	return "", nil
}

func (s *wbMockStorage) DeleteVolumeIfExists(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}

// TestHandleCreateStemcell_LightFetch_HappyPath verifies the full fetch success
// path: mock Source returns fixed body, dedup misses (empty storage), upload
// records the canonical filename, CID is "template:<vmid>" (D-11).
func TestHandleCreateStemcell_LightFetch_HappyPath(t *testing.T) {
	t.Parallel()

	// Fixed body — sha8 is deterministic from these bytes.
	body := []byte("FAKE STEMCELL QCOW2 BYTES")
	sum := sha256.Sum256(body)
	sha256hex := hex.EncodeToString(sum[:])
	sha8 := sha256hex[:8]
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.438-" + sha8 + ".qcow2"

	var uploadedFilename string
	deps := wbBuildFetchDeps(t, wbEmptyNodeListFn())
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	// Wire custom storage that records uploaded filename.
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, filename string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploadedFilename = filename
				return "", nil
			},
		},
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.438",
		"image_url": "https://example.com/ubuntu-jammy.qcow2",
	}
	args := []json.RawMessage{
		mustMarshal(t, "/dev/null"),
		mustMarshal(t, cp),
	}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	// D-11: result is always "template:<vmid>" now.
	if !pve.IsTemplateStemcellCID(cid) {
		t.Errorf("CID = %q; want template:<vmid> format", cid)
	}
	if uploadedFilename != wantFilename {
		t.Errorf("uploaded filename = %q; want %q", uploadedFilename, wantFilename)
	}
}

// TestHandleCreateStemcell_LightFetch_DedupBySHA verifies that when the exact
// SHA-matched filename already exists on storage, no upload occurs and the
// returned CID is "template:<vmid>" (template built from existing qcow2, D-11).
func TestHandleCreateStemcell_LightFetch_DedupBySHA(t *testing.T) {
	t.Parallel()

	body := []byte("FAKE STEMCELL QCOW2 BYTES FOR DEDUP")
	sum := sha256.Sum256(body)
	sha256hex := hex.EncodeToString(sum[:])
	sha8 := sha256hex[:8]
	existingFilename := "bosh-stemcell-ubuntu-jammy-1.999-" + sha8 + ".qcow2"

	var uploadCalls []struct{}
	deps := wbBuildFetchDeps(t, wbExistingVolumeListFn("nfs", existingFilename))
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploadCalls = append(uploadCalls, struct{}{})
				return "", nil
			},
		},
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.999",
		"image_url": "https://example.com/ubuntu-jammy.qcow2",
	}
	args := []json.RawMessage{
		mustMarshal(t, "/dev/null"),
		mustMarshal(t, cp),
	}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	// D-11: SHA-dedup hit → build template from existing qcow2 → "template:<vmid>".
	if !pve.IsTemplateStemcellCID(cid) {
		t.Errorf("CID = %q; want template:<vmid> format", cid)
	}
	if len(uploadCalls) != 0 {
		t.Error("Upload called despite SHA dedup hit; should be skipped")
	}
}

// mustMarshal marshals v to JSON and fatals on error. White-box test helper.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return raw
}

// ============================================================
// openStagedFile unit tests — empty stagingDir (byte-identical) and set stagingDir
// ============================================================

// TestOpenStagedFile_EmptyStagingDir_UsesDirectPath verifies that when stagingDir
// is empty, openStagedFile opens the file directly via os.Open (byte-identical
// behavior to prior releases). The test creates a real temp file so the open
// succeeds, then confirms the file contents are readable.
func TestOpenStagedFile_EmptyStagingDir_UsesDirectPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	f, werr := os.Create(filepath.Join(dir, "test.qcow2"))
	if werr != nil {
		t.Fatalf("create test file: %v", werr)
	}
	wantContent := []byte("QCOW2-MAGIC-BYTES")
	if _, werr = f.Write(wantContent); werr != nil {
		t.Fatalf("write test file: %v", werr)
	}
	_ = f.Close()

	// Empty stagingDir — direct open.
	opened, err := openStagedFile("", filepath.Join(dir, "test.qcow2"))
	if err != nil {
		t.Fatalf("openStagedFile with empty stagingDir: %v", err)
	}
	defer func() { _ = opened.Close() }()

	got, rerr := io.ReadAll(opened)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if !bytes.Equal(got, wantContent) {
		t.Errorf("content = %q; want %q", got, wantContent)
	}
}

// TestOpenStagedFile_StagingDir_ScopedAccess verifies that when stagingDir is set:
//  1. A path inside the staging dir is opened successfully.
//  2. A path outside the staging dir (escaping via "..") is rejected.
func TestOpenStagedFile_StagingDir_ScopedAccess(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create file inside staging dir.
	insidePath := filepath.Join(stagingDir, "root.img")
	if werr := os.WriteFile(insidePath, []byte("DISK-IMAGE"), 0600); werr != nil {
		t.Fatalf("write inside file: %v", werr)
	}

	// Create file outside staging dir.
	outsidePath := filepath.Join(outsideDir, "escape.img")
	if werr := os.WriteFile(outsidePath, []byte("OUTSIDE"), 0600); werr != nil {
		t.Fatalf("write outside file: %v", werr)
	}

	// Inside path — must succeed.
	in, err := openStagedFile(stagingDir, insidePath)
	if err != nil {
		t.Fatalf("openStagedFile inside stagingDir: %v", err)
	}
	_ = in.Close()

	// Outside path — must be rejected.
	_, outErr := openStagedFile(stagingDir, outsidePath)
	if outErr == nil {
		t.Fatal("openStagedFile outside stagingDir: expected error; got nil")
	}
	if !strings.Contains(outErr.Error(), "escapes") && !strings.Contains(outErr.Error(), "..") {
		t.Errorf("expected path-escape error; got %q", outErr.Error())
	}
}

// TestOpenStagedFile_StagingDir_NonExistentFile verifies that a non-existent
// file inside a valid staging dir returns a file-not-found error (not a
// path-escape error).
func TestOpenStagedFile_StagingDir_NonExistentFile(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	missingPath := filepath.Join(stagingDir, "nonexistent.qcow2")

	_, err := openStagedFile(stagingDir, missingPath)
	if err == nil {
		t.Fatal("expected error for nonexistent file; got nil")
	}
	if strings.Contains(err.Error(), "escapes") {
		t.Errorf("got path-escape error for file-not-found case: %q", err.Error())
	}
}

// ============================================================
// ensureTemplateVM unit tests (white-box; package handlers)
// ============================================================

// wbMockQEMU is a minimal sdkqemu.Service stub for ensureTemplateVM tests.
// createFn controls Create; all other methods panic on accidental call.
type wbMockQEMU struct {
	sdkqemu.Service
	createFn func(ctx context.Context, node string, params map[string]any) (string, error)
}

func (q *wbMockQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if q.createFn != nil {
		return q.createFn(ctx, node, params)
	}
	// Default: synchronous success, no UPID.
	return "", nil
}

// wbMockTasks is a minimal sdktasks.Service stub for ensureTemplateVM tests.
// waitFn controls Wait; all other methods panic on accidental call.
type wbMockTasks struct {
	sdktasks.Service
	waitFn func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
}

func (t *wbMockTasks) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if t.waitFn != nil {
		return t.waitFn(ctx, node, upid, opts)
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// wbTemplateNodes extends wbMockNodes with ListQemu and CreateQemuTemplate support.
type wbTemplateNodes struct {
	wbMockNodes
	listQemuFn           func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	createQemuTemplateFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error)
}

func (n *wbTemplateNodes) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if n.listQemuFn != nil {
		return n.listQemuFn(ctx, node, params)
	}
	// Default: no existing templates.
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

func (n *wbTemplateNodes) CreateQemuTemplate(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
	if n.createQemuTemplateFn != nil {
		return n.createQemuTemplateFn(ctx, node, vmid, params)
	}
	// Default: synchronous freeze success, no UPID.
	raw := sdknodes.CreateQemuTemplateResponse(`""`)
	return &raw, nil
}

// wbTemplateMockClient wires QEMU + Tasks + Nodes for ensureTemplateVM tests.
type wbTemplateMockClient struct {
	wbMockClient
	qemuSvc  sdkqemu.Service
	tasksSvc sdktasks.Service
}

func (c *wbTemplateMockClient) QEMU() sdkqemu.Service   { return c.qemuSvc }
func (c *wbTemplateMockClient) Tasks() sdktasks.Service { return c.tasksSvc }
func (c *wbTemplateMockClient) Pools() pve.PoolService {
	if c.poolsSvc != nil {
		return c.poolsSvc
	}
	return &wbNoopPoolService{}
}

// wbTemplateStorage extends wbMockStorage with controllable DeleteVolumeIfExists.
type wbTemplateStorage struct {
	wbMockStorage
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
}

func (s *wbTemplateStorage) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	if s.deleteVolumeIfExistsFn != nil {
		return s.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	// Default: volume absent, no error.
	return false, nil
}

// buildEnsureTemplateDeps constructs a Deps suitable for ensureTemplateVM tests.
// All fields default to success no-ops unless overridden by the caller.
func buildEnsureTemplateDeps(
	qemu *wbMockQEMU,
	nodes *wbTemplateNodes,
	tasks *wbMockTasks,
	storage *wbTemplateStorage,
) Deps {
	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:   nodes,
			storageSvc: storage,
		},
		qemuSvc:  qemu,
		tasksSvc: tasks,
	}
	return Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "nfs",
			VMStorage:       "nfs",
			// Template VMID range: adaptive default (set by ApplyDefaults in production;
			// hand-set here for deterministic tests).
			StemcellTemplateVMIDRangeStart: 6000,
			StemcellTemplateVMIDRangeEnd:   8999,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
}

// listQemuOneTemplate returns a ListQemu stub reporting a single frozen template
// with the given name and vmid. Used for idempotency tests.
func listQemuOneTemplate(name string, vmid int64) func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	return func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		isTemplate := true
		raw, _ := json.Marshal(map[string]any{
			"vmid":     vmid,
			"name":     name,
			"template": isTemplate,
		})
		resp := sdknodes.ListQemuResponse{raw}
		return &resp, nil
	}
}

// listQemuEmpty returns a ListQemu stub reporting no VMs.
func listQemuEmpty() func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	return func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		empty := sdknodes.ListQemuResponse{}
		return &empty, nil
	}
}

// listClusterResourcesEmpty returns a ListResources stub reporting no VMs.
// Used to satisfy AllocateWithRetry's NextVMID call.
func listClusterResourcesEmpty() func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	return func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		empty := sdkcluster.ListResourcesResponse{}
		return &empty, nil
	}
}

// wbClusterForAlloc satisfies the cluster service for AllocateWithRetry.
type wbClusterForAlloc struct {
	sdkcluster.Service
	listResourcesFn func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (c *wbClusterForAlloc) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if c.listResourcesFn != nil {
		return c.listResourcesFn(ctx, params)
	}
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// TestEnsureTemplateVM_CreatePath_CpiOwnsSource verifies the create path
// (no existing template): QEMU.Create called with import-from and sha tag,
// MakeTemplate called (freeze), source qcow2 deleted (cpiOwnsSource=true),
// and the returned VMID is in the template range.
func TestEnsureTemplateVM_CreatePath_CpiOwnsSource(t *testing.T) {
	t.Parallel()

	const sha256hex = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	const sha8 = "abcdef12"
	const storage = "nfs"
	const qcow2Filename = "bosh-stemcell-ubuntu-jammy-1.0-" + sha8 + ".qcow2"

	var createParams map[string]any
	var createCalled bool
	var freezeCalled bool
	var deletedVolume string
	var deletedStorage string

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			createCalled = true
			createParams = params
			return "", nil // synchronous success
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			freezeCalled = true
			raw := sdknodes.CreateQemuTemplateResponse(`""`)
			return &raw, nil
		},
	}
	tasks := &wbMockTasks{}
	stor := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, storageName, volume string) (bool, error) {
			deletedStorage = storageName
			deletedVolume = volume
			return true, nil
		},
	}

	deps := buildEnsureTemplateDeps(qemu, nodes, tasks, stor)
	// Wire cluster service for AllocateWithRetry → NextVMID.
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", storage, qcow2Filename, sha256hex, true, cp)
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid < 6000 || vmid > 8999 {
		t.Errorf("vmid %d outside expected template range [6000,8999]", vmid)
	}
	if !createCalled {
		t.Error("QEMU.Create was not called")
	}
	if !freezeCalled {
		t.Error("Nodes.CreateQemuTemplate (MakeTemplate) was not called")
	}
	// Verify sha tag in create params.
	wantTag := "bosh-stemcell-sha-" + sha8
	if tag, _ := createParams["tags"].(string); tag != wantTag {
		t.Errorf("tags = %q; want %q", tag, wantTag)
	}
	// Verify import-from in virtio0.
	wantImportFrom := storage + ":import/" + qcow2Filename
	virtio0, _ := createParams["virtio0"].(string)
	if !strings.Contains(virtio0, wantImportFrom) {
		t.Errorf("virtio0 %q does not contain import-from %q", virtio0, wantImportFrom)
	}
	// Verify onboot=0 (no auto-start on template).
	if onboot, _ := createParams["onboot"].(int); onboot != 0 {
		t.Errorf("onboot = %d; want 0", onboot)
	}
	// Verify source qcow2 deleted (cpiOwnsSource=true). DeleteVolumeIfExists
	// takes the storage pool and the volume PATH ("import/<file>") as SEPARATE
	// args — same contract as delete_stemcell. The volume arg must NOT carry a
	// "<storage>:" prefix, or the pool is double-prefixed and the delete no-ops.
	wantDeleted := "import/" + qcow2Filename
	if deletedVolume != wantDeleted {
		t.Errorf("deleted volume = %q; want %q", deletedVolume, wantDeleted)
	}
	if deletedStorage != storage {
		t.Errorf("delete storage arg = %q; want %q", deletedStorage, storage)
	}
}

// TestEnsureTemplateVM_Idempotent_ExistingTemplate verifies that when
// FindTemplateByName finds an existing template, the existing VMID is returned
// immediately without creating a new VM or deleting the source qcow2.
func TestEnsureTemplateVM_Idempotent_ExistingTemplate(t *testing.T) {
	t.Parallel()

	const existingVMID = int64(6042)
	var createCalled bool
	var deleteCalled bool

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	// BuildTemplateName("ubuntu-jammy","1.0") = "bosh-stemcell-ubuntu-jammy-1.0"
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuOneTemplate("bosh-stemcell-ubuntu-jammy-1.0", existingVMID),
	}
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalled = true
			return true, nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, storage)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "stem.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", true, cp)
	if err != nil {
		t.Fatalf("expected nil error on idempotent reuse, got: %v", err)
	}
	if vmid != existingVMID {
		t.Errorf("vmid = %d; want %d", vmid, existingVMID)
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called on idempotent reuse path")
	}
	if deleteCalled {
		t.Error("DeleteVolumeIfExists must NOT be called on idempotent reuse path")
	}
}

// TestEnsureTemplateVM_MakeTemplateFails_ErrorReturned verifies that when
// MakeTemplate (freeze) fails, the error is returned and the source qcow2
// is NOT deleted (template is not safe to use).
func TestEnsureTemplateVM_MakeTemplateFails_ErrorReturned(t *testing.T) {
	t.Parallel()

	var deleteCalled bool
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return nil, errors.New("PVE: cannot freeze: disk locked")
		},
	}
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalled = true
			return true, nil
		},
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, storage)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "2.0"}
	_, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", true, cp)
	if err == nil {
		t.Fatal("expected error when MakeTemplate fails; got nil")
	}
	if !strings.Contains(err.Error(), "freeze") {
		t.Errorf("error %q does not mention freeze", err.Error())
	}
	if deleteCalled {
		t.Error("source qcow2 must NOT be deleted when freeze fails")
	}
}

// TestEnsureTemplateVM_CpiOwnsSourceFalse_SourceNotDeleted verifies that when
// cpiOwnsSource=false (light-preuploaded path), the source qcow2 is never deleted
// even after successful template creation and freeze.
func TestEnsureTemplateVM_CpiOwnsSourceFalse_SourceNotDeleted(t *testing.T) {
	t.Parallel()

	var deleteCalled bool
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalled = true
			return true, nil
		},
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, storage)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "3.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		false, // cpiOwnsSource = false (operator pre-uploaded)
		cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 {
		t.Error("expected non-zero vmid")
	}
	if deleteCalled {
		t.Error("DeleteVolumeIfExists must NOT be called when cpiOwnsSource=false")
	}
}

// TestEnsureTemplateVM_DeleteFailsBestEffort_VmidStillReturned verifies the D-09
// best-effort deletion contract: when Storage().DeleteVolumeIfExists fails (e.g.
// network timeout), the function logs a warning but returns the vmid without error.
func TestEnsureTemplateVM_DeleteFailsBestEffort_VmidStillReturned(t *testing.T) {
	t.Parallel()

	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, errors.New("storage: timeout deleting volume")
		},
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, storage)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-bionic", Version: "4.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "bionic.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		true, // cpiOwnsSource: delete attempted but fails
		cp)
	if err != nil {
		t.Fatalf("delete failure must not surface as error (best-effort); got: %v", err)
	}
	if vmid < 6000 || vmid > 8999 {
		t.Errorf("vmid %d outside expected template range [6000,8999]", vmid)
	}
}

// TestEnsureTemplateVM_SHATagFormat verifies that the tag set on the template VM
// has the exact format "bosh-stemcell-sha-<sha8>" where sha8 = first 8 hex chars.
func TestEnsureTemplateVM_SHATagFormat(t *testing.T) {
	t.Parallel()

	const fullSHA = "deadbeef11223344deadbeef11223344deadbeef11223344deadbeef11223344"
	const wantTag = "bosh-stemcell-sha-deadbeef"

	var capturedTag string
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedTag, _ = params["tags"].(string)
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "5.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2", fullSHA, false, cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 {
		t.Error("expected non-zero vmid")
	}
	if capturedTag != wantTag {
		t.Errorf("tag = %q; want %q", capturedTag, wantTag)
	}
}

// TestEnsureTemplateVM_FindTemplateByNameAPIError verifies that an API error
// from FindTemplateByName (ListQemu) propagates as an error, no VM is created.
func TestEnsureTemplateVM_FindTemplateByNameAPIError(t *testing.T) {
	t.Parallel()

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			return nil, errors.New("PVE: connection refused")
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "6.0"}
	_, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "x.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", true, cp)
	if err == nil {
		t.Fatal("expected error when FindTemplateByName fails; got nil")
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called when FindTemplateByName fails")
	}
}

// TestStemcellCloudProps_validateLightMutex_Direct exercises the method
// directly on constructed struct values. Complements the black-box handler test.
func TestStemcellCloudProps_validateLightMutex_Direct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		p       stemcellCloudProps
		wantErr bool
	}{
		{
			name:    "both set — error",
			p:       stemcellCloudProps{ImageID: "local:import/a.qcow2", ImageURL: "https://example.com/b.qcow2"},
			wantErr: true,
		},
		{
			name:    "image_id only — nil",
			p:       stemcellCloudProps{ImageID: "local:import/a.qcow2"},
			wantErr: false,
		},
		{
			name:    "image_url only — nil",
			p:       stemcellCloudProps{ImageURL: "https://example.com/b.qcow2"},
			wantErr: false,
		},
		{
			name:    "neither — nil",
			p:       stemcellCloudProps{},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.p.validateLightMutex()
			if tc.wantErr && err == nil {
				t.Fatal("expected error; got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil {
				const frag = "mutually exclusive"
				found := false
				msg := err.Error()
				for i := 0; i <= len(msg)-len(frag); i++ {
					if msg[i:i+len(frag)] == frag {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("error %q does not contain %q", msg, frag)
				}
			}
		})
	}
}

// ============================================================
// ensureTemplateVM pool-assignment tests
// ============================================================

// wbRecordingPoolService records AddVM calls for assertion.
// Replaces wbNoopPoolService in tests that exercise pool assignment.
type wbRecordingPoolService struct {
	calls  []wbPoolCall
	addErr error // when non-nil, returned from every AddVM call
}

type wbPoolCall struct {
	poolID string
	vmid   int64
}

func (p *wbRecordingPoolService) AddVM(_ context.Context, poolID string, vmid int64) error {
	p.calls = append(p.calls, wbPoolCall{poolID: poolID, vmid: vmid})
	return p.addErr
}

// buildEnsureTemplateDepsWithPool returns a Deps wired with a recording pool service.
func buildEnsureTemplateDepsWithPool(
	qemu *wbMockQEMU,
	nodes *wbTemplateNodes,
	tasks *wbMockTasks,
	storage *wbTemplateStorage,
	pool *wbRecordingPoolService,
	poolID string,
) Deps {
	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:   nodes,
			storageSvc: storage,
			poolsSvc:   pool,
		},
		qemuSvc:  qemu,
		tasksSvc: tasks,
	}
	return Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "nfs",
			StemcellTemplateVMIDRangeStart: 6000,
			StemcellTemplateVMIDRangeEnd:   8999,
			StemcellTemplatePool:           poolID,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
}

// TestEnsureTemplateVM_PoolAssignment_Called verifies that when
// StemcellTemplatePool is set, AssignVMToPool is called with the correct
// poolID and the allocated VMID after the template is frozen.
func TestEnsureTemplateVM_PoolAssignment_Called(t *testing.T) {
	t.Parallel()

	pool := &wbRecordingPoolService{}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-stemcells",
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "7.0"}
	vmid, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", false, cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid < 6000 || vmid > 8999 {
		t.Errorf("vmid %d outside template range", vmid)
	}
	if len(pool.calls) != 1 {
		t.Fatalf("AddVM called %d times; want 1", len(pool.calls))
	}
	if pool.calls[0].poolID != "bosh-stemcells" {
		t.Errorf("AddVM poolID = %q; want %q", pool.calls[0].poolID, "bosh-stemcells")
	}
	if pool.calls[0].vmid != vmid {
		t.Errorf("AddVM vmid = %d; want %d", pool.calls[0].vmid, vmid)
	}
}

// TestEnsureTemplateVM_NoPool_AddVMNotCalled verifies that when
// StemcellTemplatePool is empty, AddVM is never called.
func TestEnsureTemplateVM_NoPool_AddVMNotCalled(t *testing.T) {
	t.Parallel()

	pool := &wbRecordingPoolService{}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "", // empty pool → AddVM must not fire
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "8.0"}
	_, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", false, cp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pool.calls) != 0 {
		t.Errorf("AddVM called %d times; want 0 when StemcellTemplatePool is empty", len(pool.calls))
	}
}

// TestEnsureTemplateVM_PoolAssignmentError_ReturnsError verifies that an AddVM
// error from the pool service causes ensureTemplateVM to return an error (fatal
// misconfiguration contract). The returned error must mention the pool name.
func TestEnsureTemplateVM_PoolAssignmentError_ReturnsError(t *testing.T) {
	t.Parallel()

	pool := &wbRecordingPoolService{
		addErr: errors.New("PVE: resource pool 'bosh-stemcells' not found"),
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-stemcells",
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-bionic", Version: "9.0"}
	_, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "bionic.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233", false, cp)
	if err == nil {
		t.Fatal("expected error when pool AddVM fails; got nil")
	}
	if !strings.Contains(err.Error(), "bosh-stemcells") {
		t.Errorf("error %q does not mention pool name", err.Error())
	}
}
