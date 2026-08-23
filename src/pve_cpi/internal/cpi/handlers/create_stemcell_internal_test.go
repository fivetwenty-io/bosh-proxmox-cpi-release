package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
}

// captureTemplateTagsUpdate wires the deps' nodes mock to record the first
// Tags-bearing UpdateQemuConfig call into *dst — the post-create identity-tag
// write (attemptCreateTemplateVM applies tags after create, not at create
// time; see its pool-permission note). Later Tags-bearing updates (e.g.
// registerStemcellRef's director-ref add) are ignored.
func captureTemplateTagsUpdate(t *testing.T, deps Deps, dst *string) {
	t.Helper()
	nodes, ok := deps.PVE.(*wbTemplateMockClient).nodesSvc.(*wbTemplateNodes)
	if !ok {
		t.Fatalf("nodes mock is %T; want *wbTemplateNodes", deps.PVE.(*wbTemplateMockClient).nodesSvc)
	}
	prev := nodes.updateConfigFn
	nodes.updateConfigFn = func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
		if params != nil && params.Tags != nil && *dst == "" {
			*dst = *params.Tags
		}
		if prev != nil {
			return prev(ctx, node, vmid, params)
		}
		return nil
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

func (n *wbNoopPoolService) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (n *wbNoopPoolService) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }
func (n *wbNoopPoolService) CreatePool(_ context.Context, _, _ string) error         { return nil }
func (n *wbNoopPoolService) DeletePool(_ context.Context, _ string) error            { return nil }
func (n *wbNoopPoolService) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

type wbMockClient struct {
	nodesSvc          sdknodes.Service
	clusterStorageSvc sdkclusterstorage.Service
	clusterSvc        sdkcluster.Service
	storageSvc        sdkstorage.Service
	// poolsSvc is nil → uses wbNoopPoolService.
	poolsSvc pve.PoolService
}

func (c *wbMockClient) QEMU() sdkqemu.Service           { return nil }
func (c *wbMockClient) Storage() sdkstorage.Service     { return c.storageSvc }
func (c *wbMockClient) CloudInit() sdkcloudinit.Service { return nil }
func (c *wbMockClient) Tasks() sdktasks.Service         { return nil }

// Nodes wraps the wired nodes service so pve.ListGuestsAuthoritative sees
// the guests scripted through the cluster ListResources fixture (delegate
// rows win on vmid collisions; every other method delegates through).
func (c *wbMockClient) Nodes() sdknodes.Service {
	if c.clusterSvc == nil {
		return c.nodesSvc
	}
	return &icNodesService{Service: c.nodesSvc, listFn: c.clusterSvc.ListResources, fallbackNode: "pve-node1"}
}
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
	// listResourcesFn, when set, backs ListResources — used by cluster-scoped
	// cache-template lookup tests (pve.FindTemplatesBySHATagCluster /
	// pve.FindTemplateByNameCluster) to report existing template VMs. nil
	// (the default) reports an empty cluster-resources list, so
	// AllocateWithRetry/NextVMID starts at range start and no cache template
	// is ever "found" by the cluster-scoped lookups.
	listResourcesFn func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
	// listConfigNodesFn, when set, backs ListConfigNodes (listClusterNodes'
	// replication-target discovery) — used by tests that need distinct,
	// caller-named cluster nodes rather than the nodeCount-many identical
	// "pve-node1" default below.
	listConfigNodesFn func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error)
}

func (c *wbMockCluster) ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	if c.listConfigNodesFn != nil {
		return c.listConfigNodesFn(ctx)
	}
	var resp sdkcluster.ListConfigNodesResponse
	for i := 0; i < c.nodeCount; i++ {
		// "name" is the key production decoders read (PVE's real
		// /cluster/config/nodes rows carry it); the old "node" key made the
		// default rows invisible to every name-decoding consumer.
		raw, _ := json.Marshal(map[string]string{"name": "pve-node1"})
		resp = append(resp, raw)
	}
	return &resp, nil
}

func (c *wbMockCluster) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if c.listResourcesFn != nil {
		return c.listResourcesFn(ctx, params)
	}
	// Default: no existing VMs → AllocateWithRetry/NextVMID starts at range start.
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// clusterResourceQemuTemplate marshals one cluster/resources JSON row for a
// frozen QEMU template VM — the shape pve.FindTemplatesBySHATagCluster /
// pve.FindTemplateByNameCluster decode (type="qemu", vmid, node, name, tags,
// template=true). Used by stemcell cache-template lookup tests to populate
// wbMockCluster.listResourcesFn.
func clusterResourceQemuTemplate(vmid int64, node, name, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type":     "qemu",
		"vmid":     vmid,
		"node":     node,
		"name":     name,
		"tags":     tags,
		"template": true,
	})
	return raw
}

type wbMockNodes struct {
	sdknodes.Service
	listStorageFn  func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
	listQemuFn     func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	updateConfigFn func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
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

// UpdateQemuConfig is called by registerStemcellRef when ensureTemplateVM
// reuses an existing template. Default is a no-op success.
func (n *wbMockNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if n.updateConfigFn != nil {
		return n.updateConfigFn(ctx, node, vmid, params)
	}
	return nil
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
// records the canonical filename, CID is a well-formed :heavy: path-identity CID.
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
	// Fetch is always CPI-owned bytes → :heavy:, regardless of dedup outcome.
	wantCID := pve.BuildHeavyStemcellCID("nfs", wantFilename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
	if uploadedFilename != wantFilename {
		t.Errorf("uploaded filename = %q; want %q", uploadedFilename, wantFilename)
	}
}

// TestHandleCreateStemcell_LightFetch_DedupBySHA verifies that when the exact
// SHA-matched filename already exists on storage, no upload occurs and the
// returned CID is the same well-formed :heavy: CID (template built/reused
// from the existing qcow2).
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
	// SHA-dedup hit → build/reuse template from the existing qcow2; the
	// computed filename (name+version+sha8) matches existingFilename exactly.
	wantCID := pve.BuildHeavyStemcellCID("nfs", existingFilename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}
	if len(uploadCalls) != 0 {
		t.Error("Upload called despite SHA dedup hit; should be skipped")
	}
}

// TestHandleCreateStemcell_LightFetch_PrefixDedup_WritesSHATag verifies the
// the light-fetch prefix-dedup arm (Step 3, fetchFindByPrefix — hit
// before any network fetch, sha256hex genuinely unknown to THIS call) derives
// the sha8 from the matched existing filename and threads it through so the
// resulting cache template still carries "bosh-stemcell-sha-<sha8>". Without
// this, sha8Of("") produces no sha tag at all, and the template becomes
// unreachable by delete_stemcell's and create_vm's sha-tag lookups even
// though the filename it was built from plainly carries a known digest.
func TestHandleCreateStemcell_LightFetch_PrefixDedup_WritesSHATag(t *testing.T) {
	t.Parallel()

	const sha8 = "aabbccdd"
	existingFilename := "bosh-stemcell-ubuntu-jammy-1.998-" + sha8 + ".qcow2"

	var capturedTags string
	var createCalled bool
	deps := wbBuildFetchDeps(t, wbExistingVolumeListFn("nfs", existingFilename))
	// resolveFetchSource always runs (Step 1, before the prefix-dedup check),
	// so the resolver itself must succeed; a network fetch here would mean the
	// prefix-dedup short-circuit failed to fire, so Source.Fetch is wired to
	// fail loudly if it is ever actually called.
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{fetchErr: fmt.Errorf("network fetch must not be attempted on a prefix-dedup hit")},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL}, nil
	}
	deps.PVE.(*wbTemplateMockClient).qemuSvc = &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	captureTemplateTagsUpdate(t, deps, &capturedTags)

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.998",
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
	wantCID := pve.BuildHeavyStemcellCID("nfs", existingFilename)
	if cid, ok := result.(string); !ok || cid != wantCID {
		t.Errorf("CID = %v; want %q", result, wantCID)
	}
	// The cache template already exists (dedup hit) in this fixture's cluster
	// view (wbBuildFetchDeps' clusterSvc reports no templates by default), so
	// QEMU.Create IS expected here — this asserts the FRESH-BUILD tag set,
	// which is the only place the sha tag is written.
	if !createCalled {
		t.Fatal("expected QEMU.Create to build the cache template")
	}
	wantTag := stemcellSHATagPrefix + sha8
	if !strings.Contains(capturedTags, wantTag) {
		t.Errorf("template tags = %q; want to contain %q (derived from the matched filename's sha8)", capturedTags, wantTag)
	}
}

// TestHandleCreateStemcell_LightFetch_SkipsVMIDOwnedByStorageContent verifies
// ensureTemplateVM's pve.WithStorageScan wiring: a template VMID with no
// entry in ListQemu (simulating a template owned by a DIFFERENT PVE cluster
// sharing the same VM/images storage) must still be skipped because a volume
// named "base-<vmid>-disk-0" for it (PVE's frozen-template naming) is visible
// on the resolved templateNode + deps.Config.VMStorage. Without the
// storage-scan wiring at ensureTemplateVM's AllocateWithRetry call this test
// would flake toward allocating the peer-owned VMID.
func TestHandleCreateStemcell_LightFetch_SkipsVMIDOwnedByStorageContent(t *testing.T) {
	t.Parallel()

	body := []byte("FAKE STEMCELL QCOW2 BYTES FOR STORAGE SCAN")

	// Within wbBuildFetchDeps' StemcellTemplateVMIDRangeStart/End [30000,30999],
	// exclusively occupied on shared storage — invisible to ListQemu (this
	// cluster's own view) but present as a base- template volume.
	peerOwnedVMID := 30500

	nodeListFn := func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		if storage != "nfs" {
			t.Errorf("unexpected storage-scan storage: %q", storage)
		}
		// No ":import/<prefix>" match here — fetchFindByPrefix's dedup check
		// ignores this entry and proceeds to upload+create, same as
		// wbEmptyNodeListFn. The peer-owned base- volume is what the new
		// storage scan (not the dedup check) must react to.
		raw, _ := json.Marshal(map[string]string{
			"volid": fmt.Sprintf("nfs:base-%d-disk-0.qcow2", peerOwnedVMID),
		})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}

	var uploadedFilename string
	var allocatedVMID int
	deps := wbBuildFetchDeps(t, nodeListFn)
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, filename string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploadedFilename = filename
				return "", nil
			},
		},
	}
	// Capture the VMID QEMU.Create actually allocated — the returned CID no
	// longer carries it (path-identity CIDs are storage+filename only), so
	// the collision assertion below must observe it via the create params.
	deps.PVE.(*wbTemplateMockClient).qemuSvc = &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			if v, ok := params[metadataKeyVMID].(int); ok {
				allocatedVMID = v
			}
			return "", nil
		},
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.500",
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
	if kind, _, _, parseErr := pve.ParseStemcellPathCID(cid); parseErr != nil || kind != pve.StemcellKindHeavy {
		t.Fatalf("CID = %q (kind=%q, err=%v); want a well-formed :heavy: path-identity CID", cid, kind, parseErr)
	}
	if uploadedFilename == "" {
		t.Fatal("expected upload to occur (no SHA/import dedup hit configured)")
	}
	if allocatedVMID == 0 {
		t.Fatal("expected QEMU.Create to be called with a non-zero vmid")
	}
	if allocatedVMID == peerOwnedVMID {
		t.Errorf("allocated template VMID %d collides with peer-cluster volume base-%d-disk-0 on shared storage — "+
			"storage-scan wiring at ensureTemplateVM's AllocateWithRetry call did not take effect",
			allocatedVMID, peerOwnedVMID)
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
// createFn controls Create; configFn controls Config (used by registerStemcellRef
// when ensureTemplateVM reuses an existing template). All other methods panic.
type wbMockQEMU struct {
	sdkqemu.Service
	createFn func(ctx context.Context, node string, params map[string]any) (string, error)
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (q *wbMockQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if q.createFn != nil {
		return q.createFn(ctx, node, params)
	}
	// Default: synchronous success, no UPID.
	return "", nil
}

// Config returns an empty map by default so registerStemcellRef (best-effort)
// can proceed without panicking. Tests that want specific provenance data can
// set configFn.
func (q *wbMockQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.configFn != nil {
		return q.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
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
	deleteQemuFn         func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
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
//
// clusterSvc defaults to a wbMockCluster reporting a single node and an empty
// cluster-resources list: ensureTemplateVM's cluster-scoped dedup
// (pve.FindTemplatesBySHATagCluster / pve.FindTemplateByNameCluster) and
// pve.AllocateWithRetry's NextVMID both call Client.Cluster().ListResources
// unconditionally, so a nil Cluster() panics on ANY fresh-build test case
// (not just cluster-lookup-specific tests) — this default keeps every
// existing call site of buildEnsureTemplateDeps working without change.
// Tests that need a populated cluster-resources view wire nodes.listQemuFn
// or override deps.PVE after construction (see wbTemplateMockClient.clusterSvc).
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
			clusterSvc: &wbMockCluster{nodeCount: 1},
		},
		qemuSvc:  qemu,
		tasksSvc: tasks,
	}
	return Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "nfs",
			VMStorage:       "nfs",
			// Template VMID range: fixed band default (set by ApplyDefaults in production;
			// hand-set here for deterministic tests).
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
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

// ListConfigNodes derives corosync membership from the listResourcesFn
// fixture (the distinct node names in the scripted rows), falling back to
// "pve-node1", so pve.ListGuestsAuthoritative lists the nodes that actually
// hold the scripted templates.
func (c *wbClusterForAlloc) ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	rows, err := c.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	resp := sdkcluster.ListConfigNodesResponse{}
	if rows != nil {
		for _, raw := range *rows {
			var item struct {
				Node string `json:"node"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Node == "" || seen[item.Node] {
				continue
			}
			seen[item.Node] = true
			b, _ := json.Marshal(map[string]any{"name": item.Node})
			resp = append(resp, b)
		}
	}
	if len(resp) == 0 {
		resp = append(resp, json.RawMessage(`{"name": "pve-node1"}`))
	}
	return &resp, nil
}

// TestEnsureTemplateVM_CreatePath_NoSourceDeletion verifies the create path
// (no existing template): QEMU.Create called with import-from and sha tag,
// MakeTemplate called (freeze), the returned VMID is in the template range,
// and — per D10 — the source qcow2 is NEVER deleted by ensureTemplateVM
// regardless of the CID kind (:heavy: here). Spy asserts zero
// DeleteVolumeIfExists calls.
func TestEnsureTemplateVM_CreatePath_NoSourceDeletion(t *testing.T) {
	t.Parallel()

	const sha256hex = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	const sha8 = "abcdef12"
	const storage = "nfs"
	const qcow2Filename = "bosh-stemcell-ubuntu-jammy-1.0-" + sha8 + ".qcow2"

	var createParams map[string]any
	var createCalled bool
	var freezeCalled bool
	var deleteCalls int

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
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalls++
			return true, nil
		},
	}

	deps := buildEnsureTemplateDeps(qemu, nodes, tasks, stor)
	// Template disk target storage (images-capable) is intentionally distinct
	// from the import-from source storage (StemcellStorage = "nfs"); they need
	// not be the same PVE storage (local has "import" but not "images").
	deps.Config.VMStorage = "images-pool"
	var capturedTags string
	captureTemplateTagsUpdate(t, deps, &capturedTags)
	// Wire cluster service for AllocateWithRetry → NextVMID AND the new
	// cluster-scoped cache-template lookups (both consult Cluster().ListResources).
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	stemcellCID := pve.BuildHeavyStemcellCID(storage, qcow2Filename)
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", storage, qcow2Filename, sha256hex,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "/tmp/test.qcow2")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid < 30000 || vmid > 30999 {
		t.Errorf("vmid %d outside expected template range [30000,30999]", vmid)
	}
	if node != "pve-node1" {
		t.Errorf("node = %q; want %q", node, "pve-node1")
	}
	if !createCalled {
		t.Error("QEMU.Create was not called")
	}
	if !freezeCalled {
		t.Error("Nodes.CreateQemuTemplate (MakeTemplate) was not called")
	}
	// Verify ownership + cache + sha + provenance marker/name/version tags in
	// the post-create tag write (provenance tags are unconditional; tags are
	// applied after create, not at create time — see attemptCreateTemplateVM).
	wantTag := ownershipTag + ";" + stemcellCacheTag + ";bosh-stemcell-sha-" + sha8 +
		";" + stemcellMarkerTag + ";" + stemcellNameTagPrefix + "ubuntu-jammy" + ";" + stemcellVersionTagPrefix + "1-0"
	if capturedTags != wantTag {
		t.Errorf("tags = %q; want %q", capturedTags, wantTag)
	}
	// Verify import-from in virtio0.
	wantImportFrom := storage + ":import/" + qcow2Filename
	virtio0, _ := createParams["virtio0"].(string)
	if !strings.Contains(virtio0, wantImportFrom) {
		t.Errorf("virtio0 %q does not contain import-from %q", virtio0, wantImportFrom)
	}
	// Verify the disk is allocated on <storage> via the "<storage>:0," prefix.
	// A bare "0" prefix is parsed by PVE as a volume ID and rejected with
	// "unable to parse volume ID '0'", breaking stemcell template creation.
	wantPrefix := deps.Config.VMStorage + ":0,"
	if !strings.HasPrefix(virtio0, wantPrefix) {
		t.Errorf("virtio0 %q must start with storage-prefixed allocation %q", virtio0, wantPrefix)
	}
	// Verify onboot=0 (no auto-start on template).
	if onboot, _ := createParams["onboot"].(int); onboot != 0 {
		t.Errorf("onboot = %d; want 0", onboot)
	}
	// Verify tablet=0 on the template so hand-made clones from CPI templates
	// inherit tablet-off (CPI-created clones are also patched in create_vm).
	if tablet, present := createParams["tablet"].(int); !present || tablet != 0 {
		t.Errorf("tablet = %v (present=%v); want explicit 0", createParams["tablet"], present)
	}
	// Verify balloon=0 on the template so hand-made clones from CPI templates
	// inherit ballooning-off (CPI-created clones are also patched in create_vm).
	if balloon, present := createParams["balloon"].(int); !present || balloon != 0 {
		t.Errorf("balloon = %v (present=%v); want explicit 0", createParams["balloon"], present)
	}
	// Verify the template carries no "cpu" key. The clone-path cpu sentinel
	// (create_vm.go, resourceParams.Cpu written only when non-empty) relies on
	// templates never setting an explicit cpu model, so that cpu_type:
	// "pve-default" restores PVE's kvm64 default on the clone instead of
	// silently inheriting whatever the template last had. If this assertion
	// ever fails, the clone-path sentinel needs a matching explicit override.
	if _, present := createParams["cpu"]; present {
		t.Errorf("cpu = %v present on template; want no cpu key (clone-path pve-default sentinel depends on its absence)", createParams["cpu"])
	}
	// D10: the source qcow2 is never reclaimed by create_stemcell/ensureTemplateVM
	// — it IS the :heavy: stemcell identity. delete_stemcell owns last-ref deletion.
	if deleteCalls != 0 {
		t.Errorf("DeleteVolumeIfExists called %d times; want 0 (no post-freeze reclaim, D10)", deleteCalls)
	}
}

// TestEnsureTemplateVM_RootDiskBusSCSI_UsesScsi0 verifies that
// pve.root_disk_bus=scsi is honored at template-creation time: the template's
// root disk lands on scsi0 (not virtio0) and the boot order matches, so a
// subsequent create_vm clone of this template passes create_vm's
// root_disk_bus=scsi bus-match guard instead of failing fast on a mismatch.
func TestEnsureTemplateVM_RootDiskBusSCSI_UsesScsi0(t *testing.T) {
	t.Parallel()

	const sha256hex = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	const sha8 = "abcdef12"
	const storage = "nfs"
	const qcow2Filename = "bosh-stemcell-ubuntu-jammy-1.0-" + sha8 + ".qcow2"

	var createParams map[string]any

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			createParams = params
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			raw := sdknodes.CreateQemuTemplateResponse(`""`)
			return &raw, nil
		},
	}
	tasks := &wbMockTasks{}
	stor := &wbTemplateStorage{}

	deps := buildEnsureTemplateDeps(qemu, nodes, tasks, stor)
	deps.Config.VMStorage = "images-pool"
	deps.Config.RootDiskBus = "scsi"
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	stemcellCID := pve.BuildHeavyStemcellCID(storage, qcow2Filename)
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", storage, qcow2Filename, sha256hex,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "/tmp/test.qcow2")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}

	if _, present := createParams["scsi0"]; !present {
		t.Error("createParams must carry a \"scsi0\" key when root_disk_bus=scsi")
	}
	if _, present := createParams["virtio0"]; present {
		t.Error("createParams must not carry a \"virtio0\" key when root_disk_bus=scsi")
	}
	if boot, _ := createParams["boot"].(string); boot != "order=scsi0" {
		t.Errorf("createParams[\"boot\"] = %q; want \"order=scsi0\"", boot)
	}
}

// TestEnsureTemplateVM_Idempotent_ExistingTemplate verifies that when the
// cluster-scoped sha-tag lookup (pve.FindTemplatesBySHATagCluster) finds an
// existing cache template, the existing (VMID, node) is returned immediately
// without creating a new VM or touching the source qcow2.
func TestEnsureTemplateVM_Idempotent_ExistingTemplate(t *testing.T) {
	t.Parallel()

	const existingVMID = int64(10042)
	const existingNode = "pve-node2"
	const sha256hex = "aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233"
	const sha8 = "aabbccdd"
	var createCalled bool
	var deleteCalled bool

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		// Config backs sha256MatchesTemplateProvenance's read of the candidate's
		// description; an empty map (no "description" key) means "no recorded
		// SHA256 in provenance" — treated as a legacy/unknown match, reused.
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			// Backs stemcellBackingQCow2Exists: the dedup hit is only
			// accepted once the sha-tag match is also confirmed to still have a
			// live backing qcow2 on storage.
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/stem.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalled = true
			return true, nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, storage)
	// Cluster-scoped sha-tag lookup: report one existing template on a
	// DIFFERENT node than the caller's templateNode, verifying ensureTemplateVM
	// returns the template's ACTUAL node rather than assuming templateNode.
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(existingVMID, existingNode, "bosh-stemcell-ubuntu-jammy-1-0-"+sha8,
					ownershipTag+";"+stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "stem.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "stem.qcow2",
		sha256hex, "", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("expected nil error on idempotent reuse, got: %v", err)
	}
	if vmid != existingVMID {
		t.Errorf("vmid = %d; want %d", vmid, existingVMID)
	}
	if node != existingNode {
		t.Errorf("node = %q; want %q (the template's actual node, not templateNode)", node, existingNode)
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
	var deletedVMID string
	var allocatedVMID int64
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			if v, ok := params["vmid"].(int); ok {
				allocatedVMID = int64(v)
			}
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return nil, errors.New("PVE: cannot freeze: disk locked")
		},
	}
	// deleteQemuFn backs the leaked-VM cleanup: on freeze failure,
	// ensureTemplateVM must best-effort delete the VM it just created —
	// otherwise it is invisible to every discovery scan (template != true)
	// and permanently occupies a VMID and a disk.
	nodes.deleteQemuFn = func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deletedVMID = vmid
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
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

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "2.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "focal.qcow2")
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err == nil {
		t.Fatal("expected error when MakeTemplate fails; got nil")
	}
	if !strings.Contains(err.Error(), "freeze") {
		t.Errorf("error %q does not mention freeze", err.Error())
	}
	if deleteCalled {
		t.Error("source qcow2 must NOT be deleted when freeze fails")
	}
	wantDeleted := strconv.FormatInt(allocatedVMID, 10)
	if deletedVMID != wantDeleted {
		t.Errorf("deleted VMID = %q; want the unfrozen VM %q to be cleaned up after freeze failure", deletedVMID, wantDeleted)
	}
}

// TestEnsureTemplateVM_LightKind_SourceNeverDeleted verifies that a
// :light: cache template build never touches the source qcow2 — same D10
// no-reclaim policy as :heavy:, but exercised with StemcellKindLight to
// confirm the kind itself has no bearing on the (already-absent) delete step.
func TestEnsureTemplateVM_LightKind_SourceNeverDeleted(t *testing.T) {
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
	stemcellCID := pve.BuildLightStemcellCID("nfs", "jammy.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindLight, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 {
		t.Error("expected non-zero vmid")
	}
	if deleteCalled {
		t.Error("DeleteVolumeIfExists must NOT be called for a :light: cache template build")
	}
}

// TestEnsureTemplateVM_SHATagFormat verifies that the tag set on the template VM
// has the exact format "bosh-stemcell-sha-<sha8>" where sha8 = first 8 hex chars.
func TestEnsureTemplateVM_SHATagFormat(t *testing.T) {
	t.Parallel()

	const fullSHA = "deadbeef11223344deadbeef11223344deadbeef11223344deadbeef11223344"
	// ownershipTag ("bosh-cpi") and stemcellCacheTag ("bosh-stemcell-cache")
	// are always prepended; shaTag and provenance marker/name/version tags follow
	// (provenance tags are unconditional).
	const wantTag = ownershipTag + ";" + stemcellCacheTag + ";bosh-stemcell-sha-deadbeef;" +
		stemcellMarkerTag + ";" + stemcellNameTagPrefix + "ubuntu-focal;" + stemcellVersionTagPrefix + "5-0"

	var capturedTag string
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	// Identity tags arrive via the post-create config update, not the create
	// call; capture the first Tags-bearing update (later ones are
	// registerStemcellRef's director-ref adds).
	nodes.updateConfigFn = func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
		if params != nil && params.Tags != nil && capturedTag == "" {
			capturedTag = *params.Tags
		}
		return nil
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "5.0"}
	stemcellCID := pve.BuildLightStemcellCID("nfs", "focal.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2", fullSHA,
		"",
		pve.StemcellKindLight, stemcellCID, "test-director", cp, "")
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

// TestEnsureTemplateVM_KnownSHA8Fallback_WritesTagWithoutFullDigest verifies
// the knownSHA8 fallback directly: when sha256hex is empty but the caller
// supplies a genuinely content-derived sha8 (e.g. recovered from an existing
// qcow2's filename — the light-fetch prefix-dedup case), the built template
// carries the correct "bosh-stemcell-sha-<sha8>" tag (letting the sha-tag
// cluster scan and delete_stemcell's lookup find it), but its provenance
// notes record NEITHER a sha8 NOR a full sha256 — buildStemcellProvenanceNotesPath
// derives its own SHA8 field from the full digest alone, so a template built
// from only a recovered sha8 records nothing there rather than an unverified
// value it cannot independently confirm. Downstream code never reads
// stemcellProvenance.SHA8 (it exists purely as descriptive metadata), so this
// asymmetry between the tag and the notes JSON is intentional and harmless.
func TestEnsureTemplateVM_KnownSHA8Fallback_WritesTagWithoutFullDigest(t *testing.T) {
	t.Parallel()

	const knownSHA8 = "deadbeef"
	wantTag := ownershipTag + ";" + stemcellCacheTag + ";bosh-stemcell-sha-" + knownSHA8 + ";" +
		stemcellMarkerTag + ";" + stemcellNameTagPrefix + "ubuntu-focal;" + stemcellVersionTagPrefix + "9-0"

	var capturedTag string
	var capturedDescription string
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedDescription, _ = params[pveConfigKeyDescription].(string)
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	nodes.updateConfigFn = func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
		if params != nil && params.Tags != nil && capturedTag == "" {
			capturedTag = *params.Tags
		}
		return nil
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "9.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "focal.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2",
		"" /* sha256hex unknown */, knownSHA8,
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 {
		t.Error("expected non-zero vmid")
	}
	if capturedTag != wantTag {
		t.Errorf("tag = %q; want %q", capturedTag, wantTag)
	}

	var prov stemcellProvenance
	if jsonErr := json.Unmarshal([]byte(capturedDescription), &prov); jsonErr != nil {
		t.Fatalf("description not valid JSON: %v — raw: %q", jsonErr, capturedDescription)
	}
	if prov.SHA8 != "" {
		t.Errorf("provenance SHA8 = %q; want empty — recorded only alongside a verified full sha256", prov.SHA8)
	}
	if prov.SHA256 != "" {
		t.Errorf("provenance SHA256 = %q; want empty — the caller only knows sha8, not the full digest", prov.SHA256)
	}
}

// TestEnsureTemplateVM_FindTemplateByNameClusterAPIError verifies that an API
// error from the cluster-scoped name lookup (pve.FindTemplateByNameCluster,
// Cluster().ListResources) propagates as an error and no VM is created. Only
// reachable when sha8 is unknown (empty sha256hex) — a known sha8 never
// consults the name-keyed fallback.
func TestEnsureTemplateVM_FindTemplateByNameClusterAPIError(t *testing.T) {
	t.Parallel()

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, errors.New("PVE: connection refused")
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-focal", Version: "6.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "x.qcow2")
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "x.qcow2",
		"", // sha8 unknown → name-fallback branch, which hits the failing cluster lookup
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err == nil {
		t.Fatal("expected error when FindTemplateByNameCluster fails; got nil")
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called when FindTemplateByNameCluster fails")
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

// wbRecordingPoolService records AddVM and CreatePool calls for assertion.
// Replaces wbNoopPoolService in tests that exercise pool assignment.
type wbRecordingPoolService struct {
	calls  []wbPoolCall
	addErr error // when non-nil, returned from every AddVM call

	createCalls []wbPoolCreateCall
	createErr   error // when non-nil, returned from every CreatePool call

	// callOrder records "create" / "assign" in the order the pool service
	// methods were invoked, so ordering (EnsurePoolExists before
	// AssignVMToPool) can be asserted without relying on call counts alone.
	callOrder []string
}

type wbPoolCall struct {
	poolID string
	vmid   int64
}

type wbPoolCreateCall struct {
	poolID  string
	comment string
}

func (p *wbRecordingPoolService) AddVM(_ context.Context, poolID string, vmid int64) error {
	p.calls = append(p.calls, wbPoolCall{poolID: poolID, vmid: vmid})
	p.callOrder = append(p.callOrder, "assign")
	return p.addErr
}
func (p *wbRecordingPoolService) CreatePool(_ context.Context, poolID, comment string) error {
	p.createCalls = append(p.createCalls, wbPoolCreateCall{poolID: poolID, comment: comment})
	p.callOrder = append(p.callOrder, "create")
	return p.createErr
}
func (p *wbRecordingPoolService) DeletePool(_ context.Context, _ string) error { return nil }
func (p *wbRecordingPoolService) MoveVMToPool(_ context.Context, _ string, _ int64) error {
	return nil
}
func (p *wbRecordingPoolService) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
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
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
			StemcellTemplatePool:           poolID,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
}

// TestEnsureTemplateVM_PoolAssignment_Called verifies that when
// StemcellTemplatePool is set, the pool is passed as the qemu-create "pool"
// param — so a token whose only VM.Allocate grant lives on the pool path can
// create the template — and AddVM is NOT called afterward (the template is
// born a pool member; re-adding would be rejected by PVE).
func TestEnsureTemplateVM_PoolAssignment_Called(t *testing.T) {
	t.Parallel()

	var createPoolParam any
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			createPoolParam = params["pool"]
			return "", nil
		},
	}
	pool := &wbRecordingPoolService{}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-stemcells",
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "7.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid < 30000 || vmid > 30999 {
		t.Errorf("vmid %d outside template range", vmid)
	}
	if createPoolParam != "bosh-stemcells" {
		t.Errorf("qemu-create pool param = %v; want %q", createPoolParam, "bosh-stemcells")
	}
	if len(pool.calls) != 0 {
		t.Errorf("AddVM called %d times; want 0 (pool set at create time)", len(pool.calls))
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
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "focal.qcow2")
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "focal.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pool.calls) != 0 {
		t.Errorf("AddVM called %d times; want 0 when StemcellTemplatePool is empty", len(pool.calls))
	}
}

// TestEnsureTemplateVM_PoolAssignmentError_ReturnsError verifies the fatal
// assign contract on the lost-race arm: when a lower-VMID twin survives and
// AddVM (the survivor's pool assignment) fails, ensureTemplateVM returns an
// error mentioning the pool name. Our own create carries the pool at
// qemu-create time, so this arm is the only one that still assigns.
func TestEnsureTemplateVM_PoolAssignmentError_ReturnsError(t *testing.T) {
	t.Parallel()

	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"

	pool := &wbRecordingPoolService{
		addErr: errors.New("PVE: resource pool 'bosh-stemcells' not found"),
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/bionic.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-stemcells",
	)
	var listCalls int
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			// Calls 1-2: pre-create dedup lookup and NextVMID scan — empty.
			// Later calls: post-freeze reconcile reports a lower-VMID twin.
			if listCalls <= 2 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(1, "pve-node1", "bosh-stemcell-x", stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-bionic", Version: "9.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "bionic.qcow2")
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "bionic.qcow2",
		fullSHA,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err == nil {
		t.Fatal("expected error when survivor pool AddVM fails; got nil")
	}
	if !strings.Contains(err.Error(), "bosh-stemcells") {
		t.Errorf("error %q does not mention pool name", err.Error())
	}
}

// TestEnsureTemplateVM_PoolCreatedIfMissing verifies that when
// StemcellTemplatePool is set, ensureTemplateVM creates the pool
// (pve.EnsurePoolExists → CreatePool) BEFORE the qemu-create that names it as
// the "pool" param — PVE rejects a create referencing a non-existent pool.
func TestEnsureTemplateVM_PoolCreatedIfMissing(t *testing.T) {
	t.Parallel()

	pool := &wbRecordingPoolService{}
	var poolCreatedBeforeQemuCreate bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			poolCreatedBeforeQemuCreate = len(pool.createCalls) == 1
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-templates",
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "10.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy2.qcow2")
	_, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy2.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pool.createCalls) != 1 {
		t.Fatalf("CreatePool called %d times; want 1", len(pool.createCalls))
	}
	if pool.createCalls[0].poolID != "bosh-templates" {
		t.Errorf("CreatePool poolID = %q; want %q", pool.createCalls[0].poolID, "bosh-templates")
	}
	if pool.createCalls[0].comment != pve.PoolProvenanceComment {
		t.Errorf("CreatePool comment = %q; want %q", pool.createCalls[0].comment, pve.PoolProvenanceComment)
	}
	if !poolCreatedBeforeQemuCreate {
		t.Error("CreatePool must run before QEMU.Create (create names the pool as a param)")
	}
	if len(pool.calls) != 0 {
		t.Errorf("AddVM called %d times; want 0 (pool set at create time)", len(pool.calls))
	}
}

// TestEnsureTemplateVM_PoolDuplicateTolerated verifies that a live PVE
// "already exists" 500+text response from CreatePool is tolerated (the pool
// existing is the desired end state) and ensureTemplateVM proceeds to the
// qemu-create without error.
func TestEnsureTemplateVM_PoolDuplicateTolerated(t *testing.T) {
	t.Parallel()

	pool := &wbRecordingPoolService{
		createErr: errors.New("create pool failed: pool 'bosh-templates' already exists\n"), //nolint:revive // verbatim live PVE error text incl. trailing newline
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-templates",
	)
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "11.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy3.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy3.qcow2",
		"aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233",
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error (duplicate-pool CreatePool error must be tolerated): %v", err)
	}
	if vmid < 30000 || vmid > 30999 {
		t.Errorf("vmid %d outside template range", vmid)
	}
	if len(pool.createCalls) != 1 {
		t.Fatalf("CreatePool called %d times; want 1", len(pool.createCalls))
	}
	if len(pool.calls) != 0 {
		t.Fatalf("AddVM call = %+v; want none (pool set at create time)", pool.calls)
	}
}

// TestEnsureTemplateVM_AssignStillFatal verifies the lost-race arm still
// assigns the SURVIVOR to the configured pool via AddVM: the survivor's own
// create may have configured no pool (or a different one), so this caller's
// pool preference must apply to whichever template survives.
func TestEnsureTemplateVM_AssignStillFatal(t *testing.T) {
	t.Parallel()

	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	const survivorVMID = int64(1)

	pool := &wbRecordingPoolService{}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/jammy4.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}
	deps := buildEnsureTemplateDepsWithPool(
		&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{},
		pool, "bosh-templates",
	)
	var listCalls int
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			if listCalls <= 2 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(survivorVMID, "pve-node1", "bosh-stemcell-x", stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "12.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy4.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "jammy4.qcow2",
		fullSHA,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid != survivorVMID {
		t.Fatalf("vmid = %d; want survivor %d", vmid, survivorVMID)
	}
	if len(pool.calls) != 1 {
		t.Fatalf("AddVM called %d times; want 1 (survivor assigned to this caller's pool)", len(pool.calls))
	}
	if pool.calls[0].poolID != "bosh-templates" || pool.calls[0].vmid != survivorVMID {
		t.Errorf("AddVM call = %+v; want poolID=bosh-templates vmid=%d", pool.calls[0], survivorVMID)
	}
}

// TestEnsureTemplateVM_DedupBySHATag_AcrossNameSchemeChange verifies that an
// existing template is reused when its sha tag matches, even though its NAME
// differs from the freshly-derived BuildTemplateNameWithSHA output. This is
// the dot-vs-dash naming-scheme change (commit 2b01653): keying dedup solely
// on the mutable display name orphaned identical-disk templates and created
// duplicates. Cluster-scoped: the match comes from Cluster().ListResources,
// not a node-scoped ListQemu scan.
func TestEnsureTemplateVM_DedupBySHATag_AcrossNameSchemeChange(t *testing.T) {
	t.Parallel()

	const existingVMID = int64(30203)
	const existingNode = "pve-node1"
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	// Existing template carries the OLD dot-form name; the new derivation yields
	// the dash form, so a name-only lookup would miss and create a duplicate.
	const oldName = "bosh-stemcell-ubuntu-noble-1.364"

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			// Backs stemcellBackingQCow2Exists.
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/noble.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(existingVMID, existingNode, oldName, stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	// cp produces the NEW dash-form name "bosh-stemcell-ubuntu-noble-1-364-891b3b74".
	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid != existingVMID {
		t.Errorf("vmid = %d; want %d (reuse by sha tag across name-scheme change)", vmid, existingVMID)
	}
	if node != existingNode {
		t.Errorf("node = %q; want %q", node, existingNode)
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called: existing template matched by sha tag")
	}
}

// deleteQemuFn lets wbTemplateNodes record/destroy a VM in race-reconcile tests.
func (n *wbTemplateNodes) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if n.deleteQemuFn != nil {
		return n.deleteQemuFn(ctx, node, vmid, params)
	}
	raw := sdknodes.DeleteQemuResponse(`""`)
	return &raw, nil
}

// TestEnsureTemplateVM_LostRace_DeletesDuplicateAndReusesSurvivor verifies the
// TOCTOU reconcile: when a concurrent create_stemcell froze a lower-VMID twin
// in the window between our lookup and our freeze, ensureTemplateVM deletes the
// template it just created and returns the survivor's VMID (and node).
//
// Cluster-scoped: both the pre-create sha-tag dedup lookup and the
// post-freeze reconcile scan go through Cluster().ListResources, as does
// pve.AllocateWithRetry's NextVMID call in between — so the fixture's call
// counter now instruments wbClusterForAlloc.listResourcesFn (calls 1-2 empty,
// call 3+ reports the survivor) rather than nodes.listQemuFn.
func TestEnsureTemplateVM_LostRace_DeletesDuplicateAndReusesSurvivor(t *testing.T) {
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	const survivorVMID = int64(1) // impossibly low → guaranteed < our random allocation
	const survivorNode = "pve-node1"

	var listCalls int
	var allocatedVMID int64
	var deletedVMID string

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			if v, ok := params["vmid"].(int); ok {
				allocatedVMID = int64(v)
			}
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			// Backs stemcellBackingQCow2Exists: the survivor's
			// backing qcow2 must still be confirmed present before reconcile
			// adopts it as the race winner.
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/noble.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	nodes.deleteQemuFn = func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deletedVMID = vmid
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}

	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			// Calls 1-2 are the pre-create sha-tag dedup lookup and
			// AllocateWithRetry's NextVMID cluster scan: empty. Any later call
			// is the post-freeze reconcile: report the lower-VMID twin.
			if listCalls <= 2 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(survivorVMID, survivorNode, "bosh-stemcell-x", stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if node != survivorNode {
		t.Errorf("node = %q; want survivor node %q", node, survivorNode)
	}
	if vmid != survivorVMID {
		t.Errorf("vmid = %d; want survivor %d", vmid, survivorVMID)
	}
	wantDeleted := strconv.FormatInt(allocatedVMID, 10)
	if deletedVMID != wantDeleted {
		t.Errorf("deleted VMID = %q; want our just-created allocation %q", deletedVMID, wantDeleted)
	}
}

// TestEnsureTemplateVM_RaceReconcile_SkipsSHA256CollidingTwin verifies the
// post-freeze reconcile applies the full-sha256 provenance guard: a lower-VMID
// template carrying the same sha8 tag but a DIFFERENT full sha256 (a 32-bit
// tag collision, not a genuine twin) must NOT be adopted — our freshly-frozen
// template survives and nothing is deleted.
func TestEnsureTemplateVM_RaceReconcile_SkipsSHA256CollidingTwin(t *testing.T) {
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	const collidingSHA = sha8 + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	const twinVMID = int64(1) // impossibly low → always below our random allocation
	const twinNode = "pve-node1"

	collidingDescription, marshalErr := json.Marshal(stemcellProvenance{
		Name: "ubuntu-noble", Version: "1.364", SHA8: sha8, SHA256: collidingSHA,
		Kind: string(pve.StemcellKindHeavy), CreatedBy: "dir-a", DirectorRefs: []string{"dir-a"},
	})
	if marshalErr != nil {
		t.Fatalf("marshal fixture description: %v", marshalErr)
	}

	var listCalls int
	var allocatedVMID int64
	var deleteCalled bool

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			if v, ok := params["vmid"].(int); ok {
				allocatedVMID = int64(v)
			}
			return "", nil
		},
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if int64(vmid) == twinVMID {
				return map[string]any{"description": string(collidingDescription)}, nil
			}
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}

	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			// Calls 1-2: pre-create dedup lookup + NextVMID scan (empty).
			// Later calls: the reconcile scan sees only the colliding twin.
			if listCalls <= 2 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(twinVMID, twinNode, "bosh-stemcell-x", stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"",
		pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid != allocatedVMID {
		t.Errorf("vmid = %d; want our own allocation %d (colliding twin must not be adopted)", vmid, allocatedVMID)
	}
	if deleteCalled {
		t.Error("our template was deleted; the sha256-colliding twin must not win the reconcile")
	}
}

// TestDeleteTemplateVM_DestroyDisksFollowsConfig verifies that deleteTemplateVM
// routes DestroyUnreferencedDisks through deps.Config.DestroyUnreferencedDisks
// instead of hardcoding true — same rationale as destroyTemplateVM in
// delete_stemcell.go: on storage shared by a second cluster with an
// overlapping VMID band, an unconditional true would free the OTHER
// cluster's VMID-matching volumes.
func TestDeleteTemplateVM_DestroyDisksFollowsConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		configFlag       bool
		wantDestroyValue bool
	}{
		{name: "config false propagates false", configFlag: false, wantDestroyValue: false},
		{name: "config true propagates true", configFlag: true, wantDestroyValue: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var capturedDestroy *bool
			nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
			nodes.deleteQemuFn = func(_ context.Context, _, _ string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
				capturedDestroy = params.DestroyUnreferencedDisks
				raw := sdknodes.DeleteQemuResponse(`""`)
				return &raw, nil
			}
			deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})
			deps.Config.DestroyUnreferencedDisks = tc.configFlag

			if err := deleteTemplateVM(context.Background(), deps, "pve-node1", 30500, deps.Logger); err != nil {
				t.Fatalf("deleteTemplateVM returned error: %v", err)
			}
			if capturedDestroy == nil {
				t.Fatal("DeleteQemuParams.DestroyUnreferencedDisks was not set")
			}
			if *capturedDestroy != tc.wantDestroyValue {
				t.Errorf("DestroyUnreferencedDisks = %v; want %v (config.DestroyUnreferencedDisks=%v)",
					*capturedDestroy, tc.wantDestroyValue, tc.configFlag)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stemcell provenance tests
// ---------------------------------------------------------------------------

// wbProvCapture records the map[string]any passed to QEMU.Create. A pointer
// to a map avoids the ptrToRefParam lint warning (gocritic) while still
// allowing callers to receive the captured params by reference.
type wbProvCapture struct {
	params map[string]any
}

// buildProvDeps returns a Deps wired for attemptCreateTemplateVM tests.
// qemu.createFn captures createParams into the returned wbProvCapture; the
// identity tags are written by the post-create config update (not the create
// call — see attemptCreateTemplateVM's pool-permission note), so the first
// Tags-bearing UpdateQemuConfig is folded into the same capture under "tags".
func buildProvDeps(t *testing.T) (Deps, *wbProvCapture) {
	t.Helper()
	captured := &wbProvCapture{params: make(map[string]any)}
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			for k, v := range params {
				captured.params[k] = v
			}
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	nodes.updateConfigFn = func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
		if params != nil && params.Tags != nil {
			if _, seen := captured.params["tags"]; !seen {
				captured.params["tags"] = *params.Tags
			}
		}
		return nil
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}
	return deps, captured
}

// TestAttemptCreateTemplateVM_ProvenanceAlwaysWritten verifies that provenance
// tags (ownership/cache/sha/marker/name/version) and the full notes JSON
// (name, version, sha8, sha256, kind, cid, created_by, created, director_refs
// seeded with the creating director) are unconditionally written — there is
// no config gate. attemptCreateTemplateVM itself does NOT stamp a
// "director--<uuid>" tag (that is registerStemcellDirectorRef's job, applied
// per registration, not per template build).
func TestAttemptCreateTemplateVM_ProvenanceAlwaysWritten(t *testing.T) {
	t.Parallel()

	const sha8 = "abcdef12"
	const shaTag = stemcellSHATagPrefix + sha8
	const sha256hex = sha8 + "34567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	const creatingDirectorUUID = "prod-director-uuid"
	const stemcellName = "ubuntu-jammy"
	const stemcellVersion = "1.438"

	deps, got := buildProvDeps(t)

	cp := stemcellCloudProps{Name: stemcellName, Version: stemcellVersion}
	source := "https://s3.example.com/ubuntu-jammy.qcow2"
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy.qcow2")
	spec := templateBuildSpec{
		TemplateName:         "bosh-stemcell-ubuntu-jammy-1-438-" + sha8,
		ImportVolid:          "nfs:import/jammy.qcow2",
		ShaTag:               shaTag,
		SHA256Hex:            sha256hex,
		TargetStorage:        "nfs",
		Kind:                 pve.StemcellKindHeavy,
		CID:                  stemcellCID,
		CreatingDirectorUUID: creatingDirectorUUID,
	}
	err := attemptCreateTemplateVM(
		context.Background(), deps, deps.Logger,
		"pve-node1", 30001, spec,
		cp, source,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// tags must contain ownership + cache markers + shaTag + stemcell marker +
	// name + version tokens. No "director--" token here (per-registration, not
	// per-build).
	gotTags, _ := got.params["tags"].(string)
	for _, wantToken := range []string{
		ownershipTag,
		stemcellCacheTag,
		shaTag,
		stemcellMarkerTag,
		stemcellNameTagPrefix + sanitizeTagValue(stemcellName),
		stemcellVersionTagPrefix + sanitizeTagValue(stemcellVersion),
	} {
		if !strings.Contains(gotTags, wantToken) {
			t.Errorf("tags %q missing expected token %q", gotTags, wantToken)
		}
	}
	if strings.Contains(gotTags, "director--") {
		t.Errorf("tags %q must NOT contain a director--<uuid> token (attemptCreateTemplateVM does not stamp it)", gotTags)
	}

	// description must be valid JSON with the full path-identity provenance fields.
	descRaw, hasDesc := got.params["description"].(string)
	if !hasDesc || descRaw == "" {
		t.Fatalf("description key missing or empty")
	}
	var prov stemcellProvenance
	if err := json.Unmarshal([]byte(descRaw), &prov); err != nil {
		t.Fatalf("description is not valid JSON: %v — raw: %q", err, descRaw)
	}
	if prov.Name != stemcellName {
		t.Errorf("provenance.name = %q; want %q", prov.Name, stemcellName)
	}
	if prov.Version != stemcellVersion {
		t.Errorf("provenance.version = %q; want %q", prov.Version, stemcellVersion)
	}
	if prov.SHA8 != sha8 {
		t.Errorf("provenance.sha8 = %q; want %q", prov.SHA8, sha8)
	}
	if prov.SHA256 != sha256hex {
		t.Errorf("provenance.sha256 = %q; want %q", prov.SHA256, sha256hex)
	}
	if prov.Kind != string(pve.StemcellKindHeavy) {
		t.Errorf("provenance.kind = %q; want %q", prov.Kind, pve.StemcellKindHeavy)
	}
	if prov.CID != stemcellCID {
		t.Errorf("provenance.cid = %q; want %q", prov.CID, stemcellCID)
	}
	if prov.CreatedBy != creatingDirectorUUID {
		t.Errorf("provenance.created_by = %q; want %q", prov.CreatedBy, creatingDirectorUUID)
	}
	if len(prov.DirectorRefs) != 1 || prov.DirectorRefs[0] != creatingDirectorUUID {
		t.Errorf("provenance.director_refs = %v; want [%q]", prov.DirectorRefs, creatingDirectorUUID)
	}
	if prov.Source != source {
		t.Errorf("provenance.source = %q; want %q", prov.Source, source)
	}
	if prov.Created == "" {
		t.Error("provenance.created must not be empty")
	}
}

// TestAttemptCreateTemplateVM_Replica verifies that with
// spec.ExtraBaseTags=[nodeTag], the tags field carries ownership, cache, sha,
// and node markers together, and the full provenance notes JSON is written
// exactly as the primary path's.
func TestAttemptCreateTemplateVM_Replica(t *testing.T) {
	t.Parallel()

	const sha8 = "abcdef12"
	const shaTag = stemcellSHATagPrefix + sha8
	const sha256hex = sha8 + "34567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	const replicaNode = "pve-node2"
	const stemcellName = "ubuntu-jammy"
	const stemcellVersion = "1.438"
	const creatingDirectorUUID = "lab-director-uuid"
	nodeTag := pve.ReplicaNodeTagForNode(replicaNode)

	deps, got := buildProvDeps(t)
	cp := stemcellCloudProps{Name: stemcellName, Version: stemcellVersion}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy.qcow2")
	spec := templateBuildSpec{
		TemplateName:         "bosh-stemcell-ubuntu-jammy-1-438-" + sha8,
		ImportVolid:          "nfs:import/jammy.qcow2",
		ShaTag:               shaTag,
		SHA256Hex:            sha256hex,
		TargetStorage:        "nfs",
		Kind:                 pve.StemcellKindHeavy,
		CID:                  stemcellCID,
		CreatingDirectorUUID: creatingDirectorUUID,
		ExtraBaseTags:        []string{nodeTag},
	}
	err := attemptCreateTemplateVM(
		context.Background(), deps, deps.Logger,
		replicaNode, 30002, spec,
		cp, "/tmp/jammy.qcow2",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotTags, _ := got.params["tags"].(string)
	// ownership, cache, sha, node, marker, name, version all present.
	for _, wantToken := range []string{
		ownershipTag,
		stemcellCacheTag,
		shaTag,
		nodeTag,
		stemcellMarkerTag,
		stemcellNameTagPrefix + sanitizeTagValue(stemcellName),
		stemcellVersionTagPrefix + sanitizeTagValue(stemcellVersion),
	} {
		if !strings.Contains(gotTags, wantToken) {
			t.Errorf("replica tags %q missing expected token %q", gotTags, wantToken)
		}
	}

	descRaw, hasDesc := got.params["description"].(string)
	if !hasDesc || descRaw == "" {
		t.Fatalf("description key missing or empty (replica path)")
	}
	var prov stemcellProvenance
	if err := json.Unmarshal([]byte(descRaw), &prov); err != nil {
		t.Fatalf("description not valid JSON: %v — raw: %q", err, descRaw)
	}
	if prov.SHA8 != sha8 {
		t.Errorf("provenance.sha8 = %q; want %q", prov.SHA8, sha8)
	}
	if prov.CreatedBy != creatingDirectorUUID {
		t.Errorf("provenance.created_by = %q; want %q", prov.CreatedBy, creatingDirectorUUID)
	}
}

// ---------------------------------------------------------------------------
// buildAndDeduplicateStemcellCID result tests
// ---------------------------------------------------------------------------

// TestBuildAndDeduplicateStemcellCID_ReturnsHashWithoutReread verifies that
// buildAndDeduplicateStemcellCID returns the sha256hex it computes internally
// so that callers (the WithHash wrapper and tests) do not need a second file
// read. The test writes a known-content file, calls the function, and checks
// the returned hash matches the pre-computed digest of that content.
func TestBuildAndDeduplicateStemcellCID_ReturnsHashWithoutReread(t *testing.T) {
	t.Parallel()

	content := []byte("BARE-QCOW2-MAGIC-BYTES-FOR-HASH-TEST")
	// Write a raw file that looks like a bare image so the function does not
	// attempt tarball extraction — resolveStemcellImage falls through to
	// passthrough mode when no .tgz signature is detected.
	imgPath := makeTempImageFile(t, content)

	wantHash := sha256OfBytes(content)
	wantSHA8 := wantHash[:8]
	// BuildStemcellFilename keeps dots in the version; "1.0" stays "1.0", not "1-0".
	wantFilename := "bosh-stemcell-ubuntu-jammy-1.0-" + wantSHA8 + ".qcow2"

	// buildAndDeduplicateStemcellCID calls FindStemcellByFilename which needs
	// ListStorageContent. Return empty so the dedup check misses (no-upload path).
	listFn := func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		empty := sdknodes.ListStorageContentResponse{}
		return &empty, nil
	}
	deps := wbBuildFetchDeps(t, listFn)

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	dedup, err := buildAndDeduplicateStemcellCID(
		context.Background(), deps, "pve-node1", "nfs", imgPath, cp, deps.Logger)
	if dedup.Cleanup != nil {
		defer dedup.Cleanup()
	}

	if err != nil {
		t.Fatalf("buildAndDeduplicateStemcellCID returned error: %v", err)
	}
	if dedup.Found {
		t.Error("found must be false when storage is empty (no dedup hit)")
	}
	if dedup.QCow2Filename != wantFilename {
		t.Errorf("qcow2 filename = %q; want %q", dedup.QCow2Filename, wantFilename)
	}
	if dedup.SHA256Hex != wantHash {
		t.Errorf("returned sha256hex = %q; want %q", dedup.SHA256Hex, wantHash)
	}
}

// TestBuildAndDeduplicateStemcellCID_DedupHit_HashStillReturned verifies that
// when the dedup fast-path fires (volume already present in storage), the
// sha256hex is still populated in the return so the caller never has to
// re-read the file.
func TestBuildAndDeduplicateStemcellCID_DedupHit_HashStillReturned(t *testing.T) {
	t.Parallel()

	content := []byte("DEDUP-HIT-IMAGE-BYTES")
	imgPath := makeTempImageFile(t, content)

	wantHash := sha256OfBytes(content)
	wantSHA8 := wantHash[:8]
	// BuildStemcellFilename keeps dots in the version; "2.0" stays "2.0", not "2-0".
	existingFilename := "bosh-stemcell-ubuntu-jammy-2.0-" + wantSHA8 + ".qcow2"

	listFn := wbExistingVolumeListFn("nfs", existingFilename)
	deps := wbBuildFetchDeps(t, listFn)

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "2.0"}
	dedup, err := buildAndDeduplicateStemcellCID(
		context.Background(), deps, "pve-node1", "nfs", imgPath, cp, deps.Logger)
	if dedup.Cleanup != nil {
		defer dedup.Cleanup()
	}

	if err != nil {
		t.Fatalf("buildAndDeduplicateStemcellCID returned error: %v", err)
	}
	if !dedup.Found {
		t.Error("found must be true when storage already has the volume (dedup hit)")
	}
	// Hash must be populated even on the dedup fast path — no second read needed.
	if dedup.SHA256Hex != wantHash {
		t.Errorf("dedup-hit sha256hex = %q; want %q (hash must be returned without re-read)", dedup.SHA256Hex, wantHash)
	}
}

// ---------------------------------------------------------------------------
// CPI v3 env.tags tests
// ---------------------------------------------------------------------------

// wbDirectorTagsSpec builds a minimal templateBuildSpec for the DirectorTags
// tests below, keeping each test focused on cp.DirectorTags behavior.
func wbDirectorTagsSpec(shaTag string) templateBuildSpec {
	return templateBuildSpec{
		TemplateName:  "bosh-stemcell-ubuntu-jammy-1-0",
		ImportVolid:   "nfs:import/test.qcow2",
		ShaTag:        shaTag,
		TargetStorage: "nfs",
		Kind:          pve.StemcellKindHeavy,
		CID:           pve.BuildHeavyStemcellCID("nfs", "test.qcow2"),
	}
}

// TestAttemptCreateTemplateVM_DirectorTags_MergedIntoTags verifies that when
// cp.DirectorTags is non-empty, the sanitized "key-value" tokens appear in the
// createParams["tags"] field alongside the base tags (ownershipTag + shaTag).
func TestAttemptCreateTemplateVM_DirectorTags_MergedIntoTags(t *testing.T) {
	t.Parallel()

	const sha8 = "deadbeef"
	const shaTag = stemcellSHATagPrefix + sha8

	deps, got := buildProvDeps(t)
	cp := stemcellCloudProps{
		Name:    "ubuntu-jammy",
		Version: "1.0",
		DirectorTags: map[string]string{
			"env":  "production",
			"team": "platform",
		},
	}

	err := attemptCreateTemplateVM(
		context.Background(), deps, deps.Logger,
		"pve-node1", 30001, wbDirectorTagsSpec(shaTag),
		cp, "/tmp/test.qcow2",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotTags, _ := got.params["tags"].(string)

	// Base tokens must be present.
	for _, want := range []string{ownershipTag, shaTag} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("tags %q missing base token %q", gotTags, want)
		}
	}

	// Director tag tokens must be present. Token format is "key-value"
	// (sanitized key + "-" + sanitized value).
	for k, v := range cp.DirectorTags {
		token := sanitizeTagValue(k) + "-" + sanitizeTagValue(v)
		if !strings.Contains(gotTags, token) {
			t.Errorf("tags %q missing director tag token %q (from key=%q val=%q)", gotTags, token, k, v)
		}
	}
}

// TestAttemptCreateTemplateVM_DirectorTags_Absent_ByteIdentical verifies that
// when cp.DirectorTags is nil, the tags field carries only the base tokens
// (no director-tag tokens appended).
func TestAttemptCreateTemplateVM_DirectorTags_Absent_ByteIdentical(t *testing.T) {
	t.Parallel()

	const sha8 = "deadbeef"
	const shaTag = stemcellSHATagPrefix + sha8
	wantTags := ownershipTag + ";" + stemcellCacheTag + ";" + shaTag + ";" +
		stemcellMarkerTag + ";" + stemcellNameTagPrefix + "ubuntu-jammy" + ";" + stemcellVersionTagPrefix + "1-0"

	deps, got := buildProvDeps(t)
	cp := stemcellCloudProps{
		Name:         "ubuntu-jammy",
		Version:      "1.0",
		DirectorTags: nil, // absent — no director-tag tokens appended
	}

	err := attemptCreateTemplateVM(
		context.Background(), deps, deps.Logger,
		"pve-node1", 30001, wbDirectorTagsSpec(shaTag),
		cp, "/tmp/test.qcow2",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotTags, _ := got.params["tags"].(string)
	if gotTags != wantTags {
		t.Errorf("tags = %q; want %q (no director-tag tokens when DirectorTags nil)", gotTags, wantTags)
	}
}

// TestAttemptCreateTemplateVM_DirectorTags_InvalidValue_Skipped verifies that
// a director tag whose key or value sanitizes to "" is silently dropped
// (neither a token nor an error is produced).
func TestAttemptCreateTemplateVM_DirectorTags_InvalidValue_Skipped(t *testing.T) {
	t.Parallel()

	const sha8 = "deadbeef"
	const shaTag = stemcellSHATagPrefix + sha8

	deps, got := buildProvDeps(t)
	cp := stemcellCloudProps{
		Name:    "ubuntu-jammy",
		Version: "1.0",
		DirectorTags: map[string]string{
			"env":   "prod",        // valid — must appear
			"...":   "invalid-key", // key sanitizes to "" → drop
			"valid": "...",         // value sanitizes to "" → drop
		},
	}

	err := attemptCreateTemplateVM(
		context.Background(), deps, deps.Logger,
		"pve-node1", 30001, wbDirectorTagsSpec(shaTag),
		cp, "/tmp/test.qcow2",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotTags, _ := got.params["tags"].(string)

	// The valid pair "env=prod" must appear.
	if !strings.Contains(gotTags, "env-prod") {
		t.Errorf("tags %q missing valid director tag token %q", gotTags, "env-prod")
	}

	// The invalid pairs must NOT produce tokens. Checking that "invalid-key" and
	// "---" are not present as tag tokens (they would appear as "---invalid-key"
	// or "valid---" if the drop logic is missing).
	if strings.Contains(gotTags, "invalid-key") {
		t.Errorf("tags %q must not contain token for all-punctuation key", gotTags)
	}
}

// TestHandleCreateStemcell_EnvTags_MergedIntoTemplate verifies the end-to-end
// 3-arg create_stemcell path: env.tags passed as args[2] reach the PVE
// QEMU.Create call's tags param. Uses the fetch-path via FetchResolver seam
// (no real file needed) with a single-node NFS storage mock.
func TestHandleCreateStemcell_EnvTags_MergedIntoTemplate(t *testing.T) {
	t.Parallel()

	body := []byte("FAKE STEMCELL ENV TAGS TEST")
	sum := sha256.Sum256(body)
	sha256hex := hex.EncodeToString(sum[:])
	sha8 := sha256hex[:8]

	var capturedTags string
	deps := wbBuildFetchDeps(t, wbEmptyNodeListFn())
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	deps.PVE.(*wbTemplateMockClient).qemuSvc = &wbMockQEMU{}
	captureTemplateTagsUpdate(t, deps, &capturedTags)
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
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
	env := map[string]any{
		"tags": map[string]any{
			"env":  "staging",
			"team": "ops",
		},
	}
	args := []json.RawMessage{
		mustMarshal(t, "/dev/null"),
		mustMarshal(t, cp),
		mustMarshal(t, env),
	}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	if kind, _, _, parseErr := pve.ParseStemcellPathCID(cid); parseErr != nil || kind != pve.StemcellKindHeavy {
		t.Errorf("CID = %q (kind=%q, err=%v); want a well-formed :heavy: path-identity CID", cid, kind, parseErr)
	}

	// sha8 token from the sha tag must be present (base identity).
	wantSHAToken := "bosh-stemcell-sha-" + sha8
	if !strings.Contains(capturedTags, wantSHAToken) {
		t.Errorf("tags %q missing sha token %q", capturedTags, wantSHAToken)
	}

	// Director tag tokens: "env-staging" and "team-ops".
	for _, token := range []string{"env-staging", "team-ops"} {
		if !strings.Contains(capturedTags, token) {
			t.Errorf("tags %q missing director tag token %q from env.tags", capturedTags, token)
		}
	}
}

// TestHandleCreateStemcell_NoEnvArg_ByteIdentical verifies the 2-arg call
// (no env) produces the same tags as before (byte-identical base path).
func TestHandleCreateStemcell_NoEnvArg_ByteIdentical(t *testing.T) {
	t.Parallel()

	body := []byte("FAKE STEMCELL NO ENV ARG")
	var capturedTags string

	deps := wbBuildFetchDeps(t, wbEmptyNodeListFn())
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL},
			nil
	}
	deps.PVE.(*wbTemplateMockClient).qemuSvc = &wbMockQEMU{}
	captureTemplateTagsUpdate(t, deps, &capturedTags)
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
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
	// Only 2 args — no env.
	args := []json.RawMessage{
		mustMarshal(t, "/dev/null"),
		mustMarshal(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Tags must NOT contain any director-tag tokens (no "env-" or "team-" prefix).
	for _, unexpected := range []string{"env-", "team-"} {
		if strings.Contains(capturedTags, unexpected) {
			t.Errorf("2-arg call: tags %q must not contain director-tag token %q", capturedTags, unexpected)
		}
	}
	// Base tokens must be present.
	if !strings.Contains(capturedTags, ownershipTag) {
		t.Errorf("tags %q missing ownershipTag %q", capturedTags, ownershipTag)
	}
}

// ---------------------------------------------------------------------------
// ensureTemplateAndRegisterRef / director-ref registration tests
// ---------------------------------------------------------------------------

// TestEnsureTemplateAndRegisterRef_FreshBuild_RegistersDirectorUUID verifies
// that a fresh cache-template build ends with the creating director's UUID
// registered: the initial provenance notes (written at QEMU.Create) seed
// DirectorRefs with it, and the follow-up registerStemcellDirectorRef call
// stamps the corresponding "director--<uuid>" tag via UpdateQemuConfig.
func TestEnsureTemplateAndRegisterRef_FreshBuild_RegistersDirectorUUID(t *testing.T) {
	t.Parallel()

	const directorUUID = "dir-uuid-fresh-build"
	var createDescription string
	var updatedTags string
	var updateCalls int

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			createDescription, _ = params["description"].(string)
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				updateCalls++
				if params.Tags != nil {
					updatedTags = *params.Tags
				}
				return nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	sha256hex := "aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233"
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy.qcow2")
	vmid, node, err := ensureTemplateAndRegisterRef(context.Background(), deps, deps.Logger,
		"pve-node1", "nfs", "jammy.qcow2", sha256hex,
		"",
		pve.StemcellKindHeavy, stemcellCID, directorUUID, cp, "/tmp/jammy.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 || node == "" {
		t.Fatalf("expected non-zero vmid/node, got vmid=%d node=%q", vmid, node)
	}

	// Initial notes (written at create time) must seed director_refs with the
	// creating director's UUID.
	var prov stemcellProvenance
	if jsonErr := json.Unmarshal([]byte(createDescription), &prov); jsonErr != nil {
		t.Fatalf("create-time description not valid JSON: %v — raw: %q", jsonErr, createDescription)
	}
	if len(prov.DirectorRefs) != 1 || prov.DirectorRefs[0] != directorUUID {
		t.Errorf("create-time director_refs = %v; want [%q]", prov.DirectorRefs, directorUUID)
	}

	// registerStemcellDirectorRef's follow-up call stamps the director tag
	// (idempotent no-op on the ref itself, since it was already seeded).
	if updateCalls == 0 {
		t.Fatal("expected registerStemcellDirectorRef to issue at least one UpdateQemuConfig call")
	}
	wantDirectorTag := "director--" + sanitizeTagValue(directorUUID)
	if !strings.Contains(updatedTags, wantDirectorTag) {
		t.Errorf("updated tags = %q; want to contain %q", updatedTags, wantDirectorTag)
	}
}

// TestEnsureTemplateAndRegisterRef_DedupHit_RegistersDirectorUUID verifies
// that a cluster-scoped dedup hit (cache template already exists, built by a
// DIFFERENT director) still registers the calling director's UUID as a live
// reference — refs must accumulate across directors sharing one cache
// template, not just at the template's original creation.
func TestEnsureTemplateAndRegisterRef_DedupHit_RegistersDirectorUUID(t *testing.T) {
	t.Parallel()

	const existingVMID = int64(30777)
	const existingNode = "pve-node1"
	const sha8 = "aabbccdd"
	const sha256hex = sha8 + "ee112233aabbccddee112233aabbccddee112233aabbccddee112233"
	const creatorUUID = "dir-uuid-original-creator"
	const callingUUID = "dir-uuid-second-caller"

	// existingDescription models a template already built by creatorUUID;
	// registerStemcellDirectorRef must merge callingUUID into DirectorRefs.
	existingDescription, marshalErr := json.Marshal(stemcellProvenance{
		Name: "ubuntu-jammy", Version: "1.0", SHA8: sha8, SHA256: sha256hex,
		Kind: string(pve.StemcellKindHeavy), CreatedBy: creatorUUID,
		DirectorRefs: []string{creatorUUID},
	})
	if marshalErr != nil {
		t.Fatalf("marshal fixture description: %v", marshalErr)
	}

	var updatedDescription string
	qemu := &wbMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": string(existingDescription)}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				if params.Description != nil {
					updatedDescription = *params.Description
				}
				return nil
			},
			// Backs stemcellBackingQCow2Exists.
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/bosh-stemcell-ubuntu-jammy-1.0-" + sha8 + ".qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(existingVMID, existingNode, "bosh-stemcell-ubuntu-jammy-1-0-"+sha8, stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "bosh-stemcell-ubuntu-jammy-1.0-"+sha8+".qcow2")
	vmid, node, err := ensureTemplateAndRegisterRef(context.Background(), deps, deps.Logger,
		"pve-node1", "nfs", "bosh-stemcell-ubuntu-jammy-1.0-"+sha8+".qcow2", sha256hex,
		"",
		pve.StemcellKindHeavy, stemcellCID, callingUUID, cp, "/tmp/jammy.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid != existingVMID || node != existingNode {
		t.Fatalf("vmid/node = %d/%q; want %d/%q (dedup hit)", vmid, node, existingVMID, existingNode)
	}

	if updatedDescription == "" {
		t.Fatal("expected registerStemcellDirectorRef to write an updated description (new ref)")
	}
	var prov stemcellProvenance
	if jsonErr := json.Unmarshal([]byte(updatedDescription), &prov); jsonErr != nil {
		t.Fatalf("updated description not valid JSON: %v — raw: %q", jsonErr, updatedDescription)
	}
	foundCreator, foundCaller := false, false
	for _, r := range prov.DirectorRefs {
		if r == creatorUUID {
			foundCreator = true
		}
		if r == callingUUID {
			foundCaller = true
		}
	}
	if !foundCreator {
		t.Errorf("director_refs = %v; must still contain original creator %q", prov.DirectorRefs, creatorUUID)
	}
	if !foundCaller {
		t.Errorf("director_refs = %v; must now also contain calling director %q", prov.DirectorRefs, callingUUID)
	}
}

// TestEnsureTemplateAndRegisterRef_TemplateGone_RebuildsAndRegisters verifies
// the ErrStemcellTemplateGone retry contract: when registerStemcellDirectorRef
// discovers the cache template vanished (a concurrent last-ref delete raced
// this lookup — modeled here by QEMU.Config returning not-found on the FIRST
// call), ensureTemplateAndRegisterRef rebuilds the template once and retries
// registration, succeeding on the second attempt.
func TestEnsureTemplateAndRegisterRef_TemplateGone_RebuildsAndRegisters(t *testing.T) {
	t.Parallel()

	const directorUUID = "dir-uuid-gone-retry"
	var createCalls int
	var configCalls int

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalls++
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			configCalls++
			if configCalls == 1 {
				// First registration attempt: template already gone.
				return nil, pveerr.ErrNotFound
			}
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	// Cluster-scoped lookups: always empty (no existing template) — both the
	// initial build and the post-gone rebuild must construct fresh.
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: listClusterResourcesEmpty(),
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	sha256hex := "aabbccddee112233aabbccddee112233aabbccddee112233aabbccddee112233"
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "jammy.qcow2")
	vmid, node, err := ensureTemplateAndRegisterRef(context.Background(), deps, deps.Logger,
		"pve-node1", "nfs", "jammy.qcow2", sha256hex,
		"",
		pve.StemcellKindHeavy, stemcellCID, directorUUID, cp, "/tmp/jammy.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == 0 || node == "" {
		t.Fatalf("expected non-zero vmid/node after rebuild, got vmid=%d node=%q", vmid, node)
	}
	if createCalls != 2 {
		t.Errorf("QEMU.Create called %d times; want 2 (initial build + rebuild after template-gone)", createCalls)
	}
	if configCalls != 2 {
		t.Errorf("QEMU.Config called %d times; want 2 (gone on first registration attempt, success on retry)", configCalls)
	}
}

// TestEnsureTemplateVM_SHA256Mismatch_BuildsFreshTemplate verifies the sha8
// (32-bit) collision guard: when a cluster-scoped sha-tag match's recorded
// full sha256 (in its provenance notes) differs from the caller's sha256,
// ensureTemplateVM does NOT reuse it — it logs a warning and builds a new
// template instead.
func TestEnsureTemplateVM_SHA256Mismatch_BuildsFreshTemplate(t *testing.T) {
	t.Parallel()

	const collidingVMID = int64(30888)
	const collidingNode = "pve-node1"
	const sha8 = "aabbccdd"
	const wantSHA256 = sha8 + "34567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	const collidingSHA256 = sha8 + "00000000000000000000000000000000000000000000000000000000ffff"

	collidingDescription, marshalErr := json.Marshal(stemcellProvenance{
		Name: "ubuntu-jammy", Version: "1.0", SHA8: sha8, SHA256: collidingSHA256,
		Kind: string(pve.StemcellKindHeavy), CreatedBy: "dir-a", DirectorRefs: []string{"dir-a"},
	})
	if marshalErr != nil {
		t.Fatalf("marshal fixture description: %v", marshalErr)
	}

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": string(collidingDescription)}, nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(collidingVMID, collidingNode, "bosh-stemcell-ubuntu-jammy-1-0-"+sha8, stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "bosh-stemcell-ubuntu-jammy-1.0-"+sha8+".qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-"+sha8+".qcow2", wantSHA256,
		"",
		pve.StemcellKindHeavy, stemcellCID, "dir-b", cp, "/tmp/jammy.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid == collidingVMID {
		t.Errorf("vmid = %d; must NOT reuse the sha8-colliding template (full sha256 mismatch)", vmid)
	}
	if !createCalled {
		t.Error("expected QEMU.Create to build a fresh template after the collision guard rejected the sha-tag match")
	}
}

// TestEnsureTemplateVM_SHATagDedup_SkipsReplicaAnchor verifies that a per-node
// replica carrying a LOWER VMID than the primary is never selected as the
// dedup-hit anchor: replicas never hold their own director references (their
// ref set is a fossil of their creator), so registering against one would
// consult the wrong ref set. Both entries carry no recorded provenance
// SHA256, so both would pass sha256MatchesTemplateProvenance's legacy-match
// fallback — IsReplica() must be the deciding factor, not sha256 verification.
func TestEnsureTemplateVM_SHATagDedup_SkipsReplicaAnchor(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(30500)
	const primaryNode = "pve-node1"
	const replicaVMID = int64(30100) // lower than primary
	const replicaNode = "pve-node2"
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/noble.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				// Replica sorts first (lower VMID) but must be skipped.
				clusterResourceQemuTemplate(replicaVMID, replicaNode, "bosh-stemcell-ubuntu-noble-1-364-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8+";"+pve.ReplicaNodeTagForNode(replicaNode)),
				clusterResourceQemuTemplate(primaryVMID, primaryNode, "bosh-stemcell-ubuntu-noble-1-364-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid != primaryVMID || node != primaryNode {
		t.Errorf("vmid/node = %d/%q; want primary %d/%q — replica must never anchor the dedup hit",
			vmid, node, primaryVMID, primaryNode)
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called: the (non-replica) primary already satisfies the dedup lookup")
	}
}

// TestEnsureTemplateVM_RaceReconcile_SkipsReplicaWinner verifies that a
// replica surfaced by the post-freeze reconcile scan (reconcileTemplateRace)
// is never adopted as the race winner, even when its VMID is lower than the
// template this call just froze: adopting a replica would discard our own
// (correctly anchored) template in favor of one that carries no live
// director-ref set of its own.
func TestEnsureTemplateVM_RaceReconcile_SkipsReplicaWinner(t *testing.T) {
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	const replicaVMID = int64(1) // impossibly low → always below our random allocation
	const replicaNode = "pve-node2"

	var listCalls int
	var allocatedVMID int64
	var deleteCalled bool

	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			if v, ok := params["vmid"].(int); ok {
				allocatedVMID = int64(v)
			}
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	nodes.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		raw := sdknodes.DeleteQemuResponse(`""`)
		return &raw, nil
	}

	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCalls++
			// Calls 1-2: pre-create dedup lookup + NextVMID scan (empty).
			// Later calls: the reconcile scan sees only the lower-VMID replica.
			if listCalls <= 2 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(replicaVMID, replicaNode, "bosh-stemcell-x",
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8+";"+pve.ReplicaNodeTagForNode(replicaNode)),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid != allocatedVMID || node != "pve-node1" {
		t.Errorf("vmid/node = %d/%q; want our own allocation %d/\"pve-node1\" (replica must not win the reconcile)",
			vmid, node, allocatedVMID)
	}
	if deleteCalled {
		t.Error("our template was deleted; a replica must never win the race reconcile")
	}
}

// TestEnsureTemplateVM_SHATagDedup_MissingBackingQCow2_BuildsFresh verifies
// a sha-tag+provenance dedup hit is rejected, and a fresh
// template built instead, when the storage content listing no longer
// contains the candidate's backing qcow2 — the signature of a
// partially-failed delete_stemcell run that destroyed the qcow2 but left the
// tagged template behind. Without this check the caller would receive a CID
// pointing at a file that no longer exists.
func TestEnsureTemplateVM_SHATagDedup_MissingBackingQCow2_BuildsFresh(t *testing.T) {
	t.Parallel()

	const staleVMID = int64(30500)
	const staleNode = "pve-node1"
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			// Storage content listing has NO entry for noble.qcow2 — the
			// backing file was already removed (e.g. a partial delete_stemcell).
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				empty := sdknodes.ListStorageContentResponse{}
				return &empty, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(staleVMID, staleNode, "bosh-stemcell-ubuntu-noble-1-364-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid == staleVMID {
		t.Errorf("vmid = %d; must NOT reuse the stale template whose backing qcow2 is gone", vmid)
	}
	if !createCalled {
		t.Error("expected QEMU.Create to build a fresh template after the missing-qcow2 guard rejected the stale dedup hit")
	}
}

// ---------------------------------------------------------------------------
// Replication shared-storage gate + fetchFindByPrefix anchoring
// ---------------------------------------------------------------------------

// TestHandleCreateStemcell_ReplicateLocal_SharedStorage_SkipsReplication
// verifies the replication gate: when stemcell_replicate_local is true but
// the resolved storage classifies as SHARED, create_stemcell never attempts
// to replicate to the cluster's other nodes — the single cache template is
// already reachable from every node. Asserted by counting Upload calls: a
// non-gated (local-storage) replication path would upload once per
// non-primary node; a correctly-gated shared-storage path uploads exactly
// once (the primary qcow2 only).
func TestHandleCreateStemcell_ReplicateLocal_SharedStorage_SkipsReplication(t *testing.T) {
	t.Parallel()

	imgPath := makeTempImageFile(t, []byte("REPLICATION-GATE-TEST-IMAGE-BYTES"))

	var uploadCount int
	nodesSvc := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storageSvc := &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploadCount++
				return "", nil
			},
		},
	}
	// Two-node cluster with SHARED nfs storage — replication must be a no-op.
	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	cluster := &wbMockCluster{
		nodeCount: 2,
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			empty := sdkcluster.ListResourcesResponse{}
			return &empty, nil
		},
	}
	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          nodesSvc,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        cluster,
			storageSvc:        storageSvc,
		},
		qemuSvc:  &wbMockQEMU{},
		tasksSvc: &wbMockTasks{},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "nfs",
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
			StemcellReplicateLocal:         true,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.0", "disk_format": "qcow2"}
	args := []json.RawMessage{mustMarshal(t, imgPath), mustMarshal(t, cp)}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uploadCount != 1 {
		t.Errorf("Upload called %d times; want 1 (shared storage must skip replication entirely)", uploadCount)
	}
}

// TestHandleCreateStemcell_LightFetch_ReplicateLocal_UploadsToOtherNode
// verifies that handleLightStemcellFetch's fresh-fetch arm now
// replicates the fetched image to every other cluster node, same as the
// tarball mainline — previously image_url stemcells on node-local storage
// were stranded on the single node that happened to fetch them.
func TestHandleCreateStemcell_LightFetch_ReplicateLocal_UploadsToOtherNode(t *testing.T) {
	t.Parallel()

	body := []byte("REPLICATE-LIGHT-FETCH-TEST-IMAGE-BYTES")

	var uploadNodes []string
	var createNodes []string
	var replicaTags string

	nodesSvc := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	// Identity tags land via the post-create config update (not the create
	// call); the replica's tag write is the pve-node2 one.
	nodesSvc.updateConfigFn = func(_ context.Context, node, _ string, params *sdknodes.UpdateQemuConfigParams) error {
		if node == "pve-node2" && params != nil && params.Tags != nil && replicaTags == "" {
			replicaTags = *params.Tags
		}
		return nil
	}
	storageSvc := &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, node, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploadNodes = append(uploadNodes, node)
				return "", nil
			},
		},
	}
	qemuSvc := &wbMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			createNodes = append(createNodes, node)
			return "", nil
		},
	}
	// Local (non-shared) storage on a two-node cluster — replication must fire.
	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "dir", isShared: false}
	cluster := &wbMockCluster{
		nodeCount: 2,
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			empty := sdkcluster.ListResourcesResponse{}
			return &empty, nil
		},
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw1, _ := json.Marshal(map[string]string{"name": "pve-node1"})
			raw2, _ := json.Marshal(map[string]string{"name": "pve-node2"})
			resp := sdkcluster.ListConfigNodesResponse{raw1, raw2}
			return &resp, nil
		},
	}
	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          nodesSvc,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        cluster,
			storageSvc:        storageSvc,
		},
		qemuSvc:  qemuSvc,
		tasksSvc: &wbMockTasks{},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "nfs",
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
			StemcellReplicateLocal:         true,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
	deps.FetchResolver = func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
		return &mockSource{body: body, contentLength: int64(len(body))},
			stemcellfetch.Reference{Scheme: "https", URL: rawURL}, nil
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":      "ubuntu-jammy",
		"version":   "1.997",
		"image_url": "https://example.com/ubuntu-jammy.qcow2",
		// Local storage on a multi-node cluster requires pinning the
		// PRIMARY fetch to one node (pve.ValidateLightStemcellStorage);
		// replication then fans the image out to the rest.
		"node": "pve-node1",
	}
	args := []json.RawMessage{
		mustMarshal(t, "/dev/null"),
		mustMarshal(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(uploadNodes) != 2 {
		t.Fatalf("Upload called for nodes %v; want exactly 2 calls (primary pve-node1 + replica pve-node2)", uploadNodes)
	}
	foundPrimary, foundReplica := false, false
	for _, n := range uploadNodes {
		if n == "pve-node1" {
			foundPrimary = true
		}
		if n == "pve-node2" {
			foundReplica = true
		}
	}
	if !foundPrimary || !foundReplica {
		t.Errorf("uploadNodes = %v; want both pve-node1 (primary) and pve-node2 (replica)", uploadNodes)
	}
	wantNodeTag := pve.ReplicaNodeTagForNode("pve-node2")
	if !strings.Contains(replicaTags, wantNodeTag) {
		t.Errorf("replica template tags = %q; want to contain %q", replicaTags, wantNodeTag)
	}
}

// buildReplicateOneNodeFailureDeps constructs Deps for replicateOneNode
// cleanup-guard tests: upload succeeds, then QEMU.Create fails with a
// non-retriable error so ensureReplicaTemplateVM fails and replicateOneNode's
// cleanup path runs. clusterStorage backs the shared/local classification
// stemcellStorageIsShared consults.
func buildReplicateOneNodeFailureDeps(clusterStorage *wbMockClusterStorage) (Deps, *bool) {
	deleteCalled := false
	nodesSvc := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storageSvc := &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				return "", nil
			},
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			deleteCalled = true
			return true, nil
		},
	}
	qemuSvc := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "", errors.New("PVE: 500 internal error")
		},
	}
	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          nodesSvc,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        &wbClusterForAlloc{listResourcesFn: listClusterResourcesEmpty()},
			storageSvc:        storageSvc,
		},
		qemuSvc:  qemuSvc,
		tasksSvc: &wbMockTasks{},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "nfs",
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
		},
		PVE:    pveClient,
		Logger: log.NewNopLogger(),
	}
	return deps, &deleteCalled
}

// TestReplicateOneNode_CleanupSkipped_UnclassifiableStorage verifies the
// fix: when a replica build fails and the storage's shared/local
// classification cannot be determined (no matching ClusterStorage entry —
// mirrors deps.PVE.ClusterStorage() returning nothing usable), the
// best-effort upload cleanup is SKIPPED rather than deleting
// "import/<file>" — on shared storage that path can be the exact file the
// caller's already-returned CID names.
func TestReplicateOneNode_CleanupSkipped_UnclassifiableStorage(t *testing.T) {
	t.Parallel()

	// storageName mismatches "nfs" so liveStorageInfo finds no entry — known=false.
	unclassifiable := &wbMockClusterStorage{storageName: "other-storage", storageType: "dir", isShared: false}
	deps, deleteCalled := buildReplicateOneNodeFailureDeps(unclassifiable)

	srcPath := makeTempImageFile(t, []byte("replicate-one-node-cleanup-guard-test"))
	nodeLogger := log.NewNopLogger()
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	replicateOneNode(context.Background(), deps, nodeLogger, "pve-node2", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-aabbccdd.qcow2", "aabbccdd00000000000000000000000000000000000000000000000000000000",
		"aabbccdd", srcPath, "", ":heavy:nfs:import/x.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	if *deleteCalled {
		t.Error("DeleteVolumeIfExists must NOT be called when storage classification is unknown — " +
			"the just-uploaded file may be the same one the returned CID names on shared storage")
	}
}

// TestReplicateOneNode_CleanupSkipped_SharedStorage mirrors the unclassifiable
// case but with storage POSITIVELY classified shared: cleanup must still be
// skipped, since "import/<file>" resolves to the same file on every node.
func TestReplicateOneNode_CleanupSkipped_SharedStorage(t *testing.T) {
	t.Parallel()

	shared := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	deps, deleteCalled := buildReplicateOneNodeFailureDeps(shared)

	srcPath := makeTempImageFile(t, []byte("replicate-one-node-cleanup-guard-test"))
	nodeLogger := log.NewNopLogger()
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	replicateOneNode(context.Background(), deps, nodeLogger, "pve-node2", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-aabbccdd.qcow2", "aabbccdd00000000000000000000000000000000000000000000000000000000",
		"aabbccdd", srcPath, "", ":heavy:nfs:import/x.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	if *deleteCalled {
		t.Error("DeleteVolumeIfExists must NOT be called on shared storage — the uploaded file is the one the returned CID names")
	}
}

// TestReplicateOneNode_CleanupOccurs_LocalStorage verifies cleanup DOES fire
// when storage is positively classified node-local: the just-uploaded
// "import/<file>" is genuinely this node's own copy, safe to reclaim on a
// failed replica build.
func TestReplicateOneNode_CleanupOccurs_LocalStorage(t *testing.T) {
	t.Parallel()

	local := &wbMockClusterStorage{storageName: "nfs", storageType: "dir", isShared: false}
	deps, deleteCalled := buildReplicateOneNodeFailureDeps(local)

	srcPath := makeTempImageFile(t, []byte("replicate-one-node-cleanup-guard-test"))
	nodeLogger := log.NewNopLogger()
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	replicateOneNode(context.Background(), deps, nodeLogger, "pve-node2", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-aabbccdd.qcow2", "aabbccdd00000000000000000000000000000000000000000000000000000000",
		"aabbccdd", srcPath, "", ":heavy:nfs:import/x.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	if !*deleteCalled {
		t.Error("DeleteVolumeIfExists must be called to reclaim this node's own local copy after a failed replica build")
	}
}

// TestTemplateReplicasNeeded verifies the replication gate keys on the
// TEMPLATE-DISK pool (config vm_storage) — not the stemcell (qcow2) pool.
// The split configuration (shared qcow2 pool + node-local vm_storage) is the
// one the old stemcell-pool gate got wrong: templates clone from vm_storage,
// so replicas are needed there even though the qcow2 is visible everywhere.
func TestTemplateReplicasNeeded(t *testing.T) {
	t.Parallel()

	entries := map[string]dlbStorageEntry{
		"local-lvm": {storageType: "lvmthin", shared: false},
		"nfs-imgs":  {storageType: "nfs", shared: true},
	}
	cases := []struct {
		name      string
		replicate bool
		vmStorage string
		want      bool
	}{
		{"flag off", false, "local-lvm", false},
		{"local vm_storage needs replicas (split-pool regression)", true, "local-lvm", true},
		{"shared vm_storage never needs replicas", true, "nfs-imgs", false},
		{"unclassifiable vm_storage fails open like create_vm's guard", true, "mystery", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			deps := storageLookupDeps(entries)
			deps.Config.StemcellReplicateLocal = c.replicate
			deps.Config.VMStorage = c.vmStorage
			// The stemcell pool is deliberately the SHARED one in every case:
			// it must not influence the verdict.
			deps.Config.StemcellStorage = "nfs-imgs"
			if got := templateReplicasNeeded(context.Background(), deps); got != c.want {
				t.Errorf("templateReplicasNeeded (vm_storage=%q replicate=%v) = %v, want %v",
					c.vmStorage, c.replicate, got, c.want)
			}
		})
	}
}

// TestReplicateOneNode_SharedPool_SkipsPerNodeUpload verifies the split-pool
// replica flow: when the stemcell pool classifies as shared, the per-node
// qcow2 upload is skipped ("import/<file>" already resolves on every node)
// and the replica template build proceeds directly.
func TestReplicateOneNode_SharedPool_SkipsPerNodeUpload(t *testing.T) {
	t.Parallel()

	uploadCalls := 0
	createCalls := 0
	nodesSvc := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storageSvc := &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				uploadCalls++
				_, _ = io.Copy(io.Discard, body)
				return "", nil
			},
		},
	}
	qemuSvc := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalls++
			return "", errors.New("PVE: 500 internal error") // stop after proving the build was attempted
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "local-lvm",
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
		},
		PVE: &wbTemplateMockClient{
			wbMockClient: wbMockClient{
				nodesSvc:          nodesSvc,
				clusterStorageSvc: &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true},
				clusterSvc:        &wbClusterForAlloc{listResourcesFn: listClusterResourcesEmpty()},
				storageSvc:        storageSvc,
			},
			qemuSvc:  qemuSvc,
			tasksSvc: &wbMockTasks{},
		},
		Logger: log.NewNopLogger(),
	}

	nodeLogger := log.NewNopLogger()
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	// No upload source at all — the light-preuploaded shape.
	replicateOneNode(context.Background(), deps, nodeLogger, "pve-node2", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-aabbccdd.qcow2", "aabbccdd00000000000000000000000000000000000000000000000000000000",
		"aabbccdd", "", "", ":light:nfs:import/x.qcow2", "test-director", pve.StemcellKindLight, cp, "")

	if uploadCalls != 0 {
		t.Errorf("upload called %d times; want 0 — shared pool needs no per-node copy", uploadCalls)
	}
	if createCalls == 0 {
		t.Error("replica template build was never attempted — shared-pool path must go straight to the build")
	}
}

// TestReplicateOneNode_NoSourceUnclassifiablePool_SkipsNode verifies the
// defensive arm: with no local upload source AND a pool that cannot be
// classified shared, the node is skipped outright — no upload, no build.
func TestReplicateOneNode_NoSourceUnclassifiablePool_SkipsNode(t *testing.T) {
	t.Parallel()

	uploadCalls := 0
	createCalls := 0
	nodesSvc := &wbTemplateNodes{listQemuFn: listQemuEmpty()}
	storageSvc := &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				uploadCalls++
				_, _ = io.Copy(io.Discard, body)
				return "", nil
			},
		},
	}
	qemuSvc := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalls++
			return "", errors.New("unexpected")
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                           "pve-node1",
			StemcellStorage:                "nfs",
			VMStorage:                      "local-lvm",
			StemcellTemplateVMIDRangeStart: 30000,
			StemcellTemplateVMIDRangeEnd:   30999,
		},
		PVE: &wbTemplateMockClient{
			wbMockClient: wbMockClient{
				nodesSvc:          nodesSvc,
				clusterStorageSvc: &wbMockClusterStorage{storageName: "other", storageType: "dir", isShared: false},
				clusterSvc:        &wbClusterForAlloc{listResourcesFn: listClusterResourcesEmpty()},
				storageSvc:        storageSvc,
			},
			qemuSvc:  qemuSvc,
			tasksSvc: &wbMockTasks{},
		},
		Logger: log.NewNopLogger(),
	}

	nodeLogger := log.NewNopLogger()
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	replicateOneNode(context.Background(), deps, nodeLogger, "pve-node2", "nfs",
		"bosh-stemcell-ubuntu-jammy-1.0-aabbccdd.qcow2", "aabbccdd00000000000000000000000000000000000000000000000000000000",
		"aabbccdd", "", "", ":light:nfs:import/x.qcow2", "test-director", pve.StemcellKindLight, cp, "")

	if uploadCalls != 0 || createCalls != 0 {
		t.Errorf("uploads=%d creates=%d; want 0/0 — node must be skipped when no source exists and the pool is unclassifiable",
			uploadCalls, createCalls)
	}
}

// TestFetchFindByPrefix_AnchoredMatch_RejectsNearMiss verifies the anchored-match fix:
// fetchFindByPrefix must NOT match a stored filename that merely contains the
// wanted "bosh-stemcell-<name>-<version>-" prefix as a substring — e.g. a
// DIFFERENT, longer-named stemcell ("ubuntu-jammy-go_agent") must not
// false-positive against a scan for "ubuntu-jammy".
func TestFetchFindByPrefix_AnchoredMatch_RejectsNearMiss(t *testing.T) {
	t.Parallel()

	const storage = "nfs"
	// Near-miss: "ubuntu-jammy-go-agent" contains "ubuntu-jammy-" as a
	// substring immediately after "import/", but is a DIFFERENT stemcell name
	// with extra characters before the sha8+".qcow2" tail — must NOT match.
	nearMissVolid := storage + ":import/bosh-stemcell-ubuntu-jammy-go-agent-1.0-deadbeef.qcow2"
	// Exact match: the real target, listed alongside the near-miss so a
	// non-anchored scan finding the near-miss FIRST would still be a bug even
	// if this one is present later.
	exactVolid := storage + ":import/bosh-stemcell-ubuntu-jammy-1.0-cafebabe.qcow2"

	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				near, _ := json.Marshal(map[string]string{"volid": nearMissVolid})
				exact, _ := json.Marshal(map[string]string{"volid": exactVolid})
				resp := sdknodes.ListStorageContentResponse{near, exact}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})

	prefix := stemcellfetch.FilenamePrefixForDedup("ubuntu-jammy", "1.0")
	got, err := fetchFindByPrefix(context.Background(), deps, "pve-node1", storage, prefix)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != exactVolid {
		t.Errorf("fetchFindByPrefix = %q; want the anchored exact match %q (near-miss %q must be rejected)",
			got, exactVolid, nearMissVolid)
	}
}

// TestFetchFindByPrefix_NoMatch_ReturnsEmpty verifies that when storage
// contains ONLY a near-miss (no exact anchored match at all), fetchFindByPrefix
// returns ("", nil) rather than falsely matching the near-miss.
func TestFetchFindByPrefix_NoMatch_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	const storage = "nfs"
	nearMissVolid := storage + ":import/bosh-stemcell-ubuntu-jammy-go-agent-1.0-deadbeef.qcow2"

	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				near, _ := json.Marshal(map[string]string{"volid": nearMissVolid})
				resp := sdknodes.ListStorageContentResponse{near}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(&wbMockQEMU{}, nodes, &wbMockTasks{}, &wbTemplateStorage{})

	prefix := stemcellfetch.FilenamePrefixForDedup("ubuntu-jammy", "1.0")
	got, err := fetchFindByPrefix(context.Background(), deps, "pve-node1", storage, prefix)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("fetchFindByPrefix = %q; want \"\" (near-miss must never match)", got)
	}
}

// TestEnsureTemplateVM_PreGenerationTemplate_NotAdopted is the cross-generation
// safety guard on the create side. A cluster upgraded from a previous CPI
// generation still holds that generation's cache templates: they carry the
// content sha tag (same stemcell, same digest) but neither this generation's
// cache marker nor any director-- ref tag. Adopting one would register a
// reference against a template whose provenance records none, so the first
// last-ref delete_stemcell would destroy a template the older director is
// still cloning from. ensureTemplateVM must ignore it and build its own.
func TestEnsureTemplateVM_PreGenerationTemplate_NotAdopted(t *testing.T) {
	t.Parallel()

	const preGenVMID = int64(30169)
	const sha8 = "cbc4cf34"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/noble.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(preGenVMID, "pve-node1", "bosh-stemcell-ubuntu-noble-1-383",
					ownershipTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.383"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, _, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid == preGenVMID {
		t.Errorf("adopted the previous generation's template %d; it must be invisible", preGenVMID)
	}
	if !createCalled {
		t.Error("QEMU.Create was not called: the CPI must build its own cache template alongside the older one")
	}
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (c *wbMockCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (c *wbClusterForAlloc) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (n *wbNoopPoolService) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (p *wbRecordingPoolService) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (n *wbMockNodes) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (n *wbTemplateNodes) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}
