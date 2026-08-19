package handlers

// White-box tests for the server-side download path (source_url cloud_property).
// Lives in package handlers (not handlers_test) to access handleStemcellDownloadURL
// and the supporting internal types directly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ============================================================
// Helpers shared across server-download tests
// ============================================================

// wbDownloadNodes wraps wbTemplateNodes with a controllable
// CreateStorageDownloadUrl method.
type wbDownloadNodes struct {
	wbTemplateNodes
	createStorageDownloadURLFn func(ctx context.Context, node, storage string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error)
}

// CreateStorageDownloadUrl dispatches to createStorageDownloadURLFn when set;
// returns a valid UPID raw response by default.
//
//nolint:revive // method name must match the SDK interface (SDK uses Url not URL)
func (n *wbDownloadNodes) CreateStorageDownloadUrl(ctx context.Context, node, storage string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
	if n.createStorageDownloadURLFn != nil {
		return n.createStorageDownloadURLFn(ctx, node, storage, params)
	}
	// Default: return a valid-looking UPID string so AwaitTask can proceed.
	raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
	return &raw, nil
}

// buildDownloadDeps returns a Deps suitable for server-download handler tests.
// listStorageFn controls ListStorageContent (dedup/volume-find); download nodesFn
// controls CreateStorageDownloadUrl. The cluster is single-node with NFS shared storage.
func buildDownloadDeps(
	t *testing.T,
	listStorageFn func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error),
	dlNodesFn func(ctx context.Context, node, storage string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error),
) Deps {
	t.Helper()

	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	cluster := &wbMockCluster{nodeCount: 1}

	templateNodes := wbTemplateNodes{
		wbMockNodes: wbMockNodes{listStorageFn: listStorageFn},
	}
	downloadNodes := &wbDownloadNodes{
		wbTemplateNodes:            templateNodes,
		createStorageDownloadURLFn: dlNodesFn,
	}

	storage := &wbTemplateStorage{}
	qemu := &wbMockQEMU{}
	tasks := &wbMockTasks{}

	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          downloadNodes,
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

// wbDownloadListFn returns a ListStorageContent that reports no volumes on the
// first call (pre-dedup miss) then reports the given volume on subsequent calls
// (post-download volume-find). This models the common server-download sequence.
// Every caller in this file targets the "nfs" storage, so that segment of the
// volid is fixed rather than threaded through as a parameter.
func wbDownloadListFn(filename string) func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	var callCount int
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		callCount++
		if callCount == 1 {
			// Pre-dedup: nothing found.
			empty := sdknodes.ListStorageContentResponse{}
			return &empty, nil
		}
		// Post-download: volume present.
		volid := "nfs:import/" + filename
		raw, _ := json.Marshal(map[string]string{"volid": volid})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_Dispatches
// Verifies that when source_url is set, HandleCreateStemcell dispatches to the
// server-download path (CreateStorageDownloadUrl is called, not Upload).
// ============================================================

func TestCreateStemcell_SourceURL_Dispatches(t *testing.T) {
	t.Parallel()

	const (
		stemcellName    = "ubuntu-jammy"
		stemcellVersion = "1.500"
		sourceURL       = "https://example.com/ubuntu-jammy-1.500.qcow2"
		sha256hex       = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	)

	// The canonical filename with the sha8 from sha256hex.
	wantFilename := pve.BuildStemcellFilename(stemcellName, stemcellVersion, sha256hex)

	var dlCallParams *sdknodes.CreateStorageDownloadUrlParams
	var uploadCalled bool

	listFn := wbDownloadListFn(wantFilename)
	dlFn := func(_ context.Context, _, _ string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCallParams = params
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	deps := buildDownloadDeps(t, listFn, dlFn)
	// Override storage to detect unexpected Upload calls.
	deps.PVE.(*wbTemplateMockClient).storageSvc = &wbTemplateStorage{
		wbMockStorage: wbMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
				uploadCalled = true
				return "", nil
			},
		},
	}

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       stemcellName,
		"version":    stemcellVersion,
		"source_url": sourceURL,
		"sha256":     sha256hex,
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// CreateStorageDownloadUrl must have been called.
	if dlCallParams == nil {
		t.Fatal("CreateStorageDownloadUrl was not called; expected server-download dispatch")
	}
	if uploadCalled {
		t.Error("Upload was called; expected server-download path to never call Upload")
	}

	// Returned CID must be the deterministic :heavy: CID — PVE (not the CPI)
	// streamed the bytes, but the CPI owns the resulting import volume.
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	wantCID := pve.BuildHeavyStemcellCID("nfs", wantFilename)
	if cid != wantCID {
		t.Errorf("CID = %q; want %q", cid, wantCID)
	}

	// source_url must be forwarded to the SDK call.
	if dlCallParams.Url != sourceURL {
		t.Errorf("CreateStorageDownloadUrl.Url = %q; want %q", dlCallParams.Url, sourceURL)
	}
	// Content must be "import".
	if dlCallParams.Content != "import" {
		t.Errorf("CreateStorageDownloadUrl.Content = %q; want \"import\"", dlCallParams.Content)
	}
}

// ============================================================
// TestCreateStemcell_NoSourceURL_ExistingFlowUnchanged
// Verifies that when source_url is absent, CreateStorageDownloadUrl is never
// called. The heavy tarball upload path is not exercised (image_path validation
// would fail without a real file); instead we use image_id (pre-uploaded) which
// is the simplest byte-identical dispatch to check the absence of download.
// ============================================================

func TestCreateStemcell_NoSourceURL_ExistingFlowUnchanged(t *testing.T) {
	t.Parallel()

	var dlCalled bool

	// Build deps where CreateStorageDownloadUrl panics if called.
	listFn := func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		// Pre-existing volume for image_id path.
		raw, _ := json.Marshal(map[string]string{"volid": "nfs:import/bosh-stemcell-ubuntu-jammy-1.0-00000000.qcow2"})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}
	dlFn := func(_ context.Context, _, _ string, _ *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCalled = true
		return nil, errors.New("CreateStorageDownloadUrl must not be called")
	}

	deps := buildDownloadDeps(t, listFn, dlFn)

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.0",
		"image_id": "nfs:import/bosh-stemcell-ubuntu-jammy-1.0-00000000.qcow2",
		// sha256 is required for preuploaded stemcells.
		"sha256": "ef0c5d8d1d8ba6e1a8620b2cba931c76e3bc9049395c3e7a5d5733cc3df2983f",
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dlCalled {
		t.Error("CreateStorageDownloadUrl was called; must not be called when source_url is absent")
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_ChecksumParams
// Verifies that when sha256 is set alongside source_url, Checksum and
// ChecksumAlgorithm params are forwarded to CreateStorageDownloadUrl.
// ============================================================

func TestCreateStemcell_SourceURL_ChecksumParams(t *testing.T) {
	t.Parallel()

	const sha256hex = "deadbeef12345678deadbeef12345678deadbeef12345678deadbeef12345678"
	wantFilename := pve.BuildStemcellFilename("ubuntu-jammy", "1.501", sha256hex)

	var dlCallParams *sdknodes.CreateStorageDownloadUrlParams
	listFn := wbDownloadListFn(wantFilename)
	dlFn := func(_ context.Context, _, _ string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCallParams = params
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	deps := buildDownloadDeps(t, listFn, dlFn)
	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.501",
		"source_url": "https://example.com/stemcell.qcow2",
		"sha256":     sha256hex,
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dlCallParams == nil {
		t.Fatal("CreateStorageDownloadUrl was not called")
	}
	if dlCallParams.Checksum == nil {
		t.Fatal("Checksum param is nil; want sha256hex forwarded")
	}
	if *dlCallParams.Checksum != sha256hex {
		t.Errorf("Checksum = %q; want %q", *dlCallParams.Checksum, sha256hex)
	}
	if dlCallParams.ChecksumAlgorithm == nil {
		t.Fatal("ChecksumAlgorithm param is nil; want \"sha256\"")
	}
	if *dlCallParams.ChecksumAlgorithm != "sha256" {
		t.Errorf("ChecksumAlgorithm = %q; want \"sha256\"", *dlCallParams.ChecksumAlgorithm)
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_NoSHA256_Rejected
// Verifies that server-download (source_url) now requires cloud_properties.sha256:
// a placeholder sha8 ("00000000") shared by every digest-less source_url
// stemcell would let sha256MatchesTemplateProvenance treat unrelated
// stemcells as a sha-tag match (neither side records a full digest), so the
// CPI rejects the call outright instead of emitting a colliding identity.
// ============================================================

func TestCreateStemcell_SourceURL_NoSHA256_Rejected(t *testing.T) {
	t.Parallel()

	var dlCalled bool
	listFn := func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		empty := sdknodes.ListStorageContentResponse{}
		return &empty, nil
	}
	dlFn := func(_ context.Context, _, _ string, _ *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCalled = true
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	deps := buildDownloadDeps(t, listFn, dlFn)
	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.502",
		"source_url": "https://example.com/stemcell.qcow2",
		// sha256 intentionally absent.
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for missing sha256; got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error %q does not mention sha256", err.Error())
	}
	if dlCalled {
		t.Error("CreateStorageDownloadUrl must not be called when sha256 validation fails")
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_ChecksumParamsAlwaysSent
// Verifies that, now that sha256 is mandatory for server-download,
// Checksum/ChecksumAlgorithm params are always forwarded to
// CreateStorageDownloadUrl.
// ============================================================

func TestCreateStemcell_SourceURL_ChecksumParamsAlwaysSent(t *testing.T) {
	t.Parallel()

	const sha256hex = "ef0c5d8d1d8ba6e1a8620b2cba931c76e3bc9049395c3e7a5d5733cc3df2983f"
	wantFilename := pve.BuildStemcellFilename("ubuntu-jammy", "1.502", sha256hex)

	var dlCallParams *sdknodes.CreateStorageDownloadUrlParams
	listFn := wbDownloadListFn(wantFilename)
	dlFn := func(_ context.Context, _, _ string, params *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCallParams = params
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	deps := buildDownloadDeps(t, listFn, dlFn)
	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.502",
		"source_url": "https://example.com/stemcell.qcow2",
		"sha256":     sha256hex,
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dlCallParams == nil {
		t.Fatal("CreateStorageDownloadUrl was not called")
	}
	if dlCallParams.Checksum == nil || *dlCallParams.Checksum != sha256hex {
		t.Errorf("Checksum = %v; want %q", dlCallParams.Checksum, sha256hex)
	}
	if dlCallParams.ChecksumAlgorithm == nil || *dlCallParams.ChecksumAlgorithm != "sha256" {
		t.Errorf("ChecksumAlgorithm = %v; want \"sha256\"", dlCallParams.ChecksumAlgorithm)
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_TaskFailure_NonRetriable
// Verifies that a task failure (e.g. checksum mismatch reported by PVE) is
// returned as a typed non-retriable cloud error so the Director does not retry,
// and that a best-effort cleanup of the partial import volume is attempted.
// ============================================================

func TestCreateStemcell_SourceURL_TaskFailure_NonRetriable(t *testing.T) {
	t.Parallel()

	// Tasks mock that returns a failed status.
	tasks := &wbMockTasks{
		waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
			return nil, fmt.Errorf("task failed: checksum mismatch for volume")
		},
	}

	listFn := func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		empty := sdknodes.ListStorageContentResponse{}
		return &empty, nil
	}
	dlFn := func(_ context.Context, _, _ string, _ *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	var cleanupVolume string
	clusterStorage := &wbMockClusterStorage{storageName: "nfs", storageType: "nfs", isShared: true}
	cluster := &wbMockCluster{nodeCount: 1}
	templateNodes := wbTemplateNodes{
		wbMockNodes: wbMockNodes{listStorageFn: listFn},
	}
	downloadNodes := &wbDownloadNodes{
		wbTemplateNodes:            templateNodes,
		createStorageDownloadURLFn: dlFn,
	}
	// Storage mock captures the cleanup attempt.
	storage := &wbTemplateStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			cleanupVolume = volume
			return false, nil
		},
	}
	qemu := &wbMockQEMU{}

	pveClient := &wbTemplateMockClient{
		wbMockClient: wbMockClient{
			nodesSvc:          downloadNodes,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        cluster,
			storageSvc:        storage,
		},
		qemuSvc:  qemu,
		tasksSvc: tasks,
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

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.503",
		"source_url": "https://example.com/stemcell.qcow2",
		// sha256 is required for server-download.
		"sha256": "ef0c5d8d1d8ba6e1a8620b2cba931c76e3bc9049395c3e7a5d5733cc3df2983f",
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error on task failure; got nil")
	}

	// Must be a typed non-retriable cloud error.
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("error is %T; want *cpierrors.Error", err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("OkToRetry() = true; want false (task failure is non-retriable)")
	}
	if !strings.Contains(err.Error(), "task failed") {
		t.Errorf("error = %q; want to contain \"task failed\"", err.Error())
	}

	// Best-effort cleanup must have been attempted on the partial volume.
	if cleanupVolume == "" {
		t.Error("DeleteVolumeIfExists not called; best-effort cleanup of partial download volume expected on task failure")
	}
	wantPrefix := "import/bosh-stemcell-ubuntu-jammy-1.503-"
	if !strings.HasPrefix(cleanupVolume, wantPrefix) {
		t.Errorf("cleanup volume = %q; want prefix %q", cleanupVolume, wantPrefix)
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_Dedup_SkipsDownload
// Verifies that when the target volume already exists in PVE storage
// (pre-dedup hit), CreateStorageDownloadUrl is not called.
// ============================================================

func TestCreateStemcell_SourceURL_Dedup_SkipsDownload(t *testing.T) {
	t.Parallel()

	const sha256hex = "cafebabe12345678cafebabe12345678cafebabe12345678cafebabe12345678"
	wantFilename := pve.BuildStemcellFilename("ubuntu-jammy", "1.504", sha256hex)

	var dlCalled bool

	// Volume already exists from the first call.
	listFn := func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		volid := "nfs:import/" + wantFilename
		raw, _ := json.Marshal(map[string]string{"volid": volid})
		resp := sdknodes.ListStorageContentResponse{raw}
		return &resp, nil
	}
	dlFn := func(_ context.Context, _, _ string, _ *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlCalled = true
		return nil, errors.New("download must not be called on dedup hit")
	}

	deps := buildDownloadDeps(t, listFn, dlFn)
	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.504",
		"source_url": "https://example.com/stemcell.qcow2",
		"sha256":     sha256hex,
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dlCalled {
		t.Error("CreateStorageDownloadUrl was called; dedup hit should prevent download")
	}
	cid, ok := result.(string)
	wantCID := pve.BuildHeavyStemcellCID("nfs", wantFilename)
	if !ok || cid != wantCID {
		t.Errorf("CID = %v (%T); want %q", result, result, wantCID)
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_MutualExclusionWithImageURL
// Verifies that setting both source_url and image_url is rejected.
// ============================================================

func TestCreateStemcell_SourceURL_MutualExclusionWithImageURL(t *testing.T) {
	t.Parallel()

	deps := buildDownloadDeps(t, wbEmptyNodeListFn(), nil)
	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.0",
		"source_url": "https://example.com/stemcell.qcow2",
		"image_url":  "https://example.com/stemcell2.qcow2",
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for mutual-exclusion violation; got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q; want mention of \"mutually exclusive\"", err.Error())
	}
}

// ============================================================
// TestParseStemcellCloudProps_SourceURL
// Verifies source_url is parsed from cloud_properties and that LightMode
// returns "server-download" while IsLight returns true.
// ============================================================

func TestParseStemcellCloudProps_SourceURL(t *testing.T) {
	t.Parallel()

	cp := map[string]any{
		"source_url": "https://example.com/stemcell.qcow2",
	}
	p := parseStemcellCloudProps(cp)

	if p.SourceURL != "https://example.com/stemcell.qcow2" {
		t.Errorf("SourceURL = %q; want \"https://example.com/stemcell.qcow2\"", p.SourceURL)
	}
	if !p.IsLight() {
		t.Error("IsLight() = false; want true when source_url is set")
	}
	if p.LightMode() != "server-download" {
		t.Errorf("LightMode() = %q; want \"server-download\"", p.LightMode())
	}
}

// ============================================================
// TestParseStemcellCloudProps_SourceURL_Absent
// Verifies that when source_url is absent, SourceURL is empty and LightMode
// returns "" (byte-identical to the pre-feature baseline).
// ============================================================

func TestParseStemcellCloudProps_SourceURL_Absent(t *testing.T) {
	t.Parallel()

	cp := map[string]any{
		"name":    "ubuntu-jammy",
		"version": "1.0",
	}
	p := parseStemcellCloudProps(cp)

	if p.SourceURL != "" {
		t.Errorf("SourceURL = %q; want empty when source_url absent", p.SourceURL)
	}
	if p.IsLight() {
		t.Error("IsLight() = true; want false when no light fields set")
	}
	if p.LightMode() != "" {
		t.Errorf("LightMode() = %q; want empty", p.LightMode())
	}
}

// ============================================================
// TestCreateStemcell_SourceURL_ReplicateLocal_DownloadsOnOtherNode
// Verifies that handleStemcellDownloadURL now replicates a
// server-side download to every other cluster node by re-issuing
// CreateStorageDownloadUrl there — previously source_url stemcells on
// node-local storage were stranded on the single node PVE happened to
// download to.
// ============================================================

func TestCreateStemcell_SourceURL_ReplicateLocal_DownloadsOnOtherNode(t *testing.T) {
	t.Parallel()

	const sha256hex = "d0d0cafed0d0cafed0d0cafed0d0cafed0d0cafed0d0cafed0d0cafed0d0cafe"
	wantFilename := pve.BuildStemcellFilename("ubuntu-jammy", "1.996", sha256hex)

	var dlNodes []string
	var createNodes []string
	var replicaTags string

	// Every node reports the volume already present (models both the primary's
	// pre-dedup hit and, after each replica's own download, its post-download
	// volume-find) so this test does not have to also fake task-await plumbing
	// per node.
	listFn := wbDownloadListFn(wantFilename)
	dlFn := func(_ context.Context, node, _ string, _ *sdknodes.CreateStorageDownloadUrlParams) (*sdknodes.CreateStorageDownloadUrlResponse, error) {
		dlNodes = append(dlNodes, node)
		raw := sdknodes.CreateStorageDownloadUrlResponse(`"UPID:pve-node1:00001234:00000001:00000001:download:0:root@pam:"`)
		return &raw, nil
	}

	templateNodes := wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: listFn,
		},
	}
	downloadNodes := &wbDownloadNodes{
		wbTemplateNodes:            templateNodes,
		createStorageDownloadURLFn: dlFn,
	}
	qemuSvc := &wbMockQEMU{
		createFn: func(_ context.Context, node string, params map[string]any) (string, error) {
			createNodes = append(createNodes, node)
			if node == "pve-node2" {
				replicaTags, _ = params["tags"].(string)
			}
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
			nodesSvc:          downloadNodes,
			clusterStorageSvc: clusterStorage,
			clusterSvc:        cluster,
			storageSvc:        &wbTemplateStorage{},
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

	h := HandleCreateStemcell(deps)
	cp := map[string]any{
		"name":       "ubuntu-jammy",
		"version":    "1.996",
		"source_url": "https://example.com/stemcell.qcow2",
		"sha256":     sha256hex,
	}
	args := []json.RawMessage{
		mustMarshalStr(t, "/dev/null"),
		mustMarshalMap(t, cp),
	}

	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundReplicaDownload := false
	for _, n := range dlNodes {
		if n == "pve-node2" {
			foundReplicaDownload = true
		}
	}
	if !foundReplicaDownload {
		t.Errorf("CreateStorageDownloadUrl node calls = %v; want a call for replica node pve-node2", dlNodes)
	}
	wantNodeTag := pve.ReplicaNodeTagForNode("pve-node2")
	if !strings.Contains(replicaTags, wantNodeTag) {
		t.Errorf("replica template tags = %q; want to contain %q", replicaTags, wantNodeTag)
	}
}

// ============================================================
// Helpers
// ============================================================

//nolint:unparam // s is always "/dev/null" in this file; kept as parameter for readability
func mustMarshalStr(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("mustMarshalStr: %v", err)
	}
	return b
}

func mustMarshalMap(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("mustMarshalMap: %v", err)
	}
	return b
}
