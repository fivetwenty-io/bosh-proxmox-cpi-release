package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// White-box tests: handleLightStemcellFetch via fetchResolverOverride seam
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
func wbBuildFetchDeps(
	t *testing.T,
	nodeListFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error),
) Deps {
	t.Helper()

	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	cluster := &wbMockCluster{nodeCount: 1}
	nodes := &wbMockNodes{listStorageFn: nodeListFn}
	storage := &wbMockStorage{}

	return Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "nfs",
			VMStorage:       "nfs",
		},
		PVE:    &wbMockClient{nodesSvc: nodes, clusterStorageSvc: clusterStorage, clusterSvc: cluster, storageSvc: storage},
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

type wbMockClient struct {
	nodesSvc          sdknodes.Service
	clusterStorageSvc sdkclusterstorage.Service
	clusterSvc        sdkcluster.Service
	storageSvc        sdkstorage.Service
}

func (c *wbMockClient) QEMU() sdkqemu.Service                     { return nil }
func (c *wbMockClient) Storage() sdkstorage.Service               { return c.storageSvc }
func (c *wbMockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *wbMockClient) Tasks() sdktasks.Service                   { return nil }
func (c *wbMockClient) Nodes() sdknodes.Service                   { return c.nodesSvc }
func (c *wbMockClient) Cluster() sdkcluster.Service               { return c.clusterSvc }
func (c *wbMockClient) ClusterStorage() sdkclusterstorage.Service { return c.clusterStorageSvc }

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

type wbMockNodes struct {
	sdknodes.Service
	listStorageFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
}

func (n *wbMockNodes) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	if n.listStorageFn != nil {
		return n.listStorageFn(ctx, node, storage, params)
	}
	empty := sdknodes.ListStorageContentResponse{}
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
// records the canonical filename, CID has "light:" prefix with sha8 from body.
//
// Not marked t.Parallel: sets the package-level fetchResolverOverride, which
// is not safe to share concurrently with other tests that also set it.
func TestHandleCreateStemcell_LightFetch_HappyPath(t *testing.T) {
	// Fixed body — sha8 is deterministic from these bytes.
	body := []byte("FAKE STEMCELL QCOW2 BYTES")
	sum := sha256.Sum256(body)
	sha256hex := hex.EncodeToString(sum[:])
	sha8 := sha256hex[:8]
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.438-" + sha8 + ".qcow2"
	wantCID := "light:nfs:import/" + wantFilename

	// Install source override; restore after test.
	fetchResolverOverride = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	t.Cleanup(func() { fetchResolverOverride = nil })

	var uploadedFilename string
	deps := wbBuildFetchDeps(t, wbEmptyNodeListFn())
	deps.PVE.(*wbMockClient).storageSvc = &wbMockStorage{
		uploadFn: func(_ context.Context, _, _, _, filename string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			uploadedFilename = filename
			return "", nil
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
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
	if uploadedFilename != wantFilename {
		t.Errorf("uploaded filename = %q; want %q", uploadedFilename, wantFilename)
	}
}

// TestHandleCreateStemcell_LightFetch_DedupBySHA verifies that when the exact
// SHA-matched filename already exists on storage, no upload occurs and the
// existing CID is returned. NOT parallel — mutates package-level
// fetchResolverOverride which is shared with sibling tests.
func TestHandleCreateStemcell_LightFetch_DedupBySHA(t *testing.T) {

	body := []byte("FAKE STEMCELL QCOW2 BYTES FOR DEDUP")
	sum := sha256.Sum256(body)
	sha256hex := hex.EncodeToString(sum[:])
	sha8 := sha256hex[:8]
	existingFilename := "bosh-stemcell-ubuntu-jammy-1.999-" + sha8 + ".qcow2"
	wantCID := "light:nfs:import/" + existingFilename

	fetchResolverOverride = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	t.Cleanup(func() { fetchResolverOverride = nil })

	var uploadCalled bool
	deps := wbBuildFetchDeps(t, wbExistingVolumeListFn("nfs", existingFilename))
	deps.PVE.(*wbMockClient).storageSvc = &wbMockStorage{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, body)
			uploadCalled = true
			return "", nil
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
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
	if uploadCalled {
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
