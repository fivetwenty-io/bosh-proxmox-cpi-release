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

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

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
func wbDownloadListFn(storage, filename string) func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	var callCount int
	return func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		callCount++
		if callCount == 1 {
			// Pre-dedup: nothing found.
			empty := sdknodes.ListStorageContentResponse{}
			return &empty, nil
		}
		// Post-download: volume present.
		volid := storage + ":import/" + filename
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

	listFn := wbDownloadListFn("nfs", wantFilename)
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

	// Returned CID must be template:<vmid>.
	cid, ok := result.(string)
	if !ok {
		t.Fatalf("result is %T; want string", result)
	}
	if !pve.IsTemplateStemcellCID(cid) {
		t.Errorf("CID = %q; want template:<vmid> form", cid)
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
	listFn := wbDownloadListFn("nfs", wantFilename)
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
// TestCreateStemcell_SourceURL_NoSHA256_NoChecksumParams
// Verifies that when sha256 is absent, no Checksum/ChecksumAlgorithm params
// are sent to CreateStorageDownloadUrl.
// ============================================================

func TestCreateStemcell_SourceURL_NoSHA256_NoChecksumParams(t *testing.T) {
	t.Parallel()

	// Without sha256, filename uses placeholder sha8 "00000000".
	wantFilename := pve.BuildStemcellFilename("ubuntu-jammy", "1.502", "")

	var dlCallParams *sdknodes.CreateStorageDownloadUrlParams
	listFn := wbDownloadListFn("nfs", wantFilename)
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
		// sha256 intentionally absent
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
	if dlCallParams.Checksum != nil {
		t.Errorf("Checksum = %q; want nil (no sha256 supplied)", *dlCallParams.Checksum)
	}
	if dlCallParams.ChecksumAlgorithm != nil {
		t.Errorf("ChecksumAlgorithm = %q; want nil (no sha256 supplied)", *dlCallParams.ChecksumAlgorithm)
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
	if !ok || !pve.IsTemplateStemcellCID(cid) {
		t.Errorf("CID = %v (%T); want template:<vmid> string", result, result)
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
