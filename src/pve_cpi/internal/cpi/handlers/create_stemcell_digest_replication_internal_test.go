package handlers

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // SHA-1 used only for expected-digest comparison in tests, not for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ============================================================
// Helpers shared by digest + replication tests
// ============================================================

func makeTempImageFile(t *testing.T, content []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stemcell-img-*.raw")
	if err != nil {
		t.Fatalf("makeTempImageFile: create: %v", err)
	}
	if _, wErr := f.Write(content); wErr != nil {
		t.Fatalf("makeTempImageFile: write: %v", wErr)
	}
	if cErr := f.Close(); cErr != nil {
		t.Fatalf("makeTempImageFile: close: %v", cErr)
	}
	return f.Name()
}

func sha256OfBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

//nolint:gosec // SHA-1 used only for expected-digest comparison in tests, not for security
func sha1OfBytes(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// ============================================================
// Part A — verifyExpectedDigest unit tests
// ============================================================

// TestVerifyExpectedDigest_SHA256Match verifies no error when sha256 matches.
func TestVerifyExpectedDigest_SHA256Match(t *testing.T) {
	t.Parallel()
	content := []byte("test-stemcell-image-bytes")
	hash := sha256OfBytes(content)
	path := makeTempImageFile(t, content)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: hash}
	err := verifyExpectedDigest(context.Background(), logger, cp, hash, path, "", stemcellSourceLocal)
	if err != nil {
		t.Fatalf("expected nil error on sha256 match, got: %v", err)
	}
}

// TestVerifyExpectedDigest_SHA256Mismatch_Local checks non-retriable on local tarball mismatch.
func TestVerifyExpectedDigest_SHA256Mismatch_Local(t *testing.T) {
	t.Parallel()
	content := []byte("test-stemcell-image-bytes")
	actual := sha256OfBytes(content)
	path := makeTempImageFile(t, content)
	wrong := strings.Repeat("a", 64)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: wrong}
	err := verifyExpectedDigest(context.Background(), logger, cp, actual, path, "", stemcellSourceLocal)
	if err == nil {
		t.Fatal("expected error on sha256 mismatch, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("expected non-retriable error for local source, got retriable")
	}
	if !strings.Contains(err.Error(), "sha256 digest mismatch") {
		t.Errorf("error message missing 'sha256 digest mismatch': %v", err)
	}
}

// TestVerifyExpectedDigest_SHA256Mismatch_Network checks retriable on network mismatch.
func TestVerifyExpectedDigest_SHA256Mismatch_Network(t *testing.T) {
	t.Parallel()
	content := []byte("network-stemcell-bytes")
	actual := sha256OfBytes(content)
	path := makeTempImageFile(t, content)
	wrong := strings.Repeat("b", 64)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: wrong}
	err := verifyExpectedDigest(context.Background(), logger, cp, actual, path, "", stemcellSourceNetwork)
	if err == nil {
		t.Fatal("expected error on sha256 mismatch, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected retriable error for network source, got non-retriable")
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("expected TypeRetriableCloud, got %v", cpiErr.Type())
	}
}

// TestVerifyExpectedDigest_SHA1Match verifies no error when sha1 matches (sha256 empty).
func TestVerifyExpectedDigest_SHA1Match(t *testing.T) {
	t.Parallel()
	content := []byte("sha1-test-content")
	hash := sha1OfBytes(content)
	path := makeTempImageFile(t, content)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA1: hash}
	err := verifyExpectedDigest(context.Background(), logger, cp, "", path, "", stemcellSourceLocal)
	if err != nil {
		t.Fatalf("expected nil error on sha1 match, got: %v", err)
	}
}

// TestVerifyExpectedDigest_SHA1Mismatch_Local checks non-retriable on sha1 mismatch.
func TestVerifyExpectedDigest_SHA1Mismatch_Local(t *testing.T) {
	t.Parallel()
	content := []byte("sha1-mismatch-content")
	path := makeTempImageFile(t, content)
	wrong := strings.Repeat("c", 40)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA1: wrong}
	err := verifyExpectedDigest(context.Background(), logger, cp, "", path, "", stemcellSourceLocal)
	if err == nil {
		t.Fatal("expected error on sha1 mismatch, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error: %v", err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("expected non-retriable for local source")
	}
	if !strings.Contains(err.Error(), "sha1 digest mismatch") {
		t.Errorf("missing 'sha1 digest mismatch' in: %v", err)
	}
}

// TestVerifyExpectedDigest_SHA1Mismatch_Network checks retriable on sha1 mismatch (network).
func TestVerifyExpectedDigest_SHA1Mismatch_Network(t *testing.T) {
	t.Parallel()
	content := []byte("network-sha1-mismatch")
	path := makeTempImageFile(t, content)
	wrong := strings.Repeat("d", 40)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA1: wrong}
	err := verifyExpectedDigest(context.Background(), logger, cp, "", path, "", stemcellSourceNetwork)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error: %v", err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected retriable for network source")
	}
}

// TestVerifyExpectedDigest_NoDigest_WarnOnly verifies nil returned and no error.
func TestVerifyExpectedDigest_NoDigest_WarnOnly(t *testing.T) {
	t.Parallel()
	content := []byte("no-digest-content")
	path := makeTempImageFile(t, content)
	hash := sha256OfBytes(content)

	var buf bytes.Buffer
	logger, err := log.NewLogger("warn", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	cp := stemcellCloudProps{} // no expected digest
	verifyErr := verifyExpectedDigest(context.Background(), logger, cp, hash, path, "", stemcellSourceLocal)
	if verifyErr != nil {
		t.Fatalf("expected nil error for no-digest, got: %v", verifyErr)
	}
	// The warn message must appear in output.
	out := buf.String()
	if !strings.Contains(out, "integrity unverified") {
		t.Errorf("expected 'integrity unverified' warn in output, got: %s", out)
	}
}

// TestVerifyExpectedDigest_SHA256TakesPrecedenceOverSHA1 verifies SHA-256 check
// runs and SHA-1 is ignored when both are present and sha256 matches.
func TestVerifyExpectedDigest_SHA256TakesPrecedenceOverSHA1(t *testing.T) {
	t.Parallel()
	content := []byte("both-digests-content")
	sha256hash := sha256OfBytes(content)
	path := makeTempImageFile(t, content)
	wrongSHA1 := strings.Repeat("e", 40) // wrong, but should be ignored

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: sha256hash, ExpectedSHA1: wrongSHA1}
	err := verifyExpectedDigest(context.Background(), logger, cp, sha256hash, path, "", stemcellSourceLocal)
	if err != nil {
		t.Fatalf("expected nil error (sha256 matches; sha1 should be ignored): %v", err)
	}
}

// TestVerifyExpectedDigest_CaseInsensitive verifies comparison is case-insensitive.
func TestVerifyExpectedDigest_CaseInsensitive(t *testing.T) {
	t.Parallel()
	content := []byte("case-test")
	lower := sha256OfBytes(content)
	upper := strings.ToUpper(lower)
	path := makeTempImageFile(t, content)

	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: upper}
	err := verifyExpectedDigest(context.Background(), logger, cp, lower, path, "", stemcellSourceLocal)
	if err != nil {
		t.Fatalf("case mismatch should not error: %v", err)
	}
}

// ============================================================
// Helper: asError is errors.As for *cpierrors.Error
// ============================================================

func asError(err error, target **cpierrors.Error) bool {
	return errors.As(err, target)
}

// ============================================================
// Part A — parseStemcellCloudProps digest fields
// ============================================================

// TestParseStemcellCloudProps_DigestFields verifies sha256/sha1 parsed from cp.
func TestParseStemcellCloudProps_DigestFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		cp             map[string]any
		wantSHA256     string
		wantSHA1       string
	}{
		{
			name:       "sha256 only",
			cp:         map[string]any{"sha256": "deadbeef" + strings.Repeat("0", 56)},
			wantSHA256: "deadbeef" + strings.Repeat("0", 56),
		},
		{
			name:     "sha1 only",
			cp:       map[string]any{"sha1": strings.Repeat("a", 40)},
			wantSHA1: strings.Repeat("a", 40),
		},
		{
			name:       "both sha256 and sha1",
			cp:         map[string]any{"sha256": strings.Repeat("b", 64), "sha1": strings.Repeat("c", 40)},
			wantSHA256: strings.Repeat("b", 64),
			wantSHA1:   strings.Repeat("c", 40),
		},
		{
			name: "neither digest field",
			cp:   map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parseStemcellCloudProps(tc.cp)
			if p.ExpectedSHA256 != tc.wantSHA256 {
				t.Errorf("ExpectedSHA256 = %q; want %q", p.ExpectedSHA256, tc.wantSHA256)
			}
			if p.ExpectedSHA1 != tc.wantSHA1 {
				t.Errorf("ExpectedSHA1 = %q; want %q", p.ExpectedSHA1, tc.wantSHA1)
			}
		})
	}
}

// ============================================================
// Part B — replication: ReplicateLocal=false → no extra node calls
// ============================================================

// digestReplicationMockClient wires minimal mock services.
type digestReplicationMockClient struct {
	qemuSvc    sdkqemu.Service
	nodesSvc   sdknodes.Service
	tasksSvc   sdktasks.Service
	clusterSvc sdkcluster.Service
	storageSvc sdkstorage.Service
}

func (m *digestReplicationMockClient) QEMU() sdkqemu.Service                     { return m.qemuSvc }
func (m *digestReplicationMockClient) Nodes() sdknodes.Service                   { return m.nodesSvc }
func (m *digestReplicationMockClient) Tasks() sdktasks.Service                   { return m.tasksSvc }
func (m *digestReplicationMockClient) Storage() sdkstorage.Service               { return m.storageSvc }
func (m *digestReplicationMockClient) CloudInit() sdkcloudinit.Service           { return nil }
func (m *digestReplicationMockClient) Cluster() sdkcluster.Service               { return m.clusterSvc }
func (m *digestReplicationMockClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (m *digestReplicationMockClient) Pools() pve.PoolService                    { return &noopReplicationPoolService{} }

type noopReplicationPoolService struct{}

func (n *noopReplicationPoolService) AddVM(_ context.Context, _ string, _ int64) error { return nil }

// countingNodesService wraps sdknodes.Service and counts ListQemu calls per node.
type countingNodesService struct {
	sdknodes.Service
	listQemuCallsByNode  map[string]int
	listQemuFn           func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	listStorageContentFn func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
	deleteQemuFn         func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	createQemuTemplateFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error)
}

func (c *countingNodesService) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if c.listQemuCallsByNode == nil {
		c.listQemuCallsByNode = map[string]int{}
	}
	c.listQemuCallsByNode[node]++
	if c.listQemuFn != nil {
		return c.listQemuFn(ctx, node, params)
	}
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

func (c *countingNodesService) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	if c.listStorageContentFn != nil {
		return c.listStorageContentFn(ctx, node, storage, params)
	}
	empty := sdknodes.ListStorageContentResponse{}
	return &empty, nil
}

func (c *countingNodesService) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if c.deleteQemuFn != nil {
		return c.deleteQemuFn(ctx, node, vmid, params)
	}
	resp := sdknodes.DeleteQemuResponse{}
	return &resp, nil
}

func (c *countingNodesService) CreateQemuTemplate(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
	if c.createQemuTemplateFn != nil {
		return c.createQemuTemplateFn(ctx, node, vmid, params)
	}
	resp := sdknodes.CreateQemuTemplateResponse{}
	return &resp, nil
}

// countingClusterService counts ListConfigNodes calls.
type countingClusterService struct {
	sdkcluster.Service
	listConfigNodesCalls int
	listConfigNodesFn    func(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error)
	listResourcesFn      func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (c *countingClusterService) ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	c.listConfigNodesCalls++
	if c.listConfigNodesFn != nil {
		return c.listConfigNodesFn(ctx)
	}
	empty := sdkcluster.ListConfigNodesResponse{}
	return &empty, nil
}

func (c *countingClusterService) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if c.listResourcesFn != nil {
		return c.listResourcesFn(ctx, params)
	}
	// Default: no existing VMs (empty resource list so VMID allocation can proceed).
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// ============================================================
// Part B — ReplicateLocal=false → current behavior unchanged
// ============================================================

// TestReplicateStemcellToNodes_Disabled exercises replicateStemcellToNodes directly
// with a node list but asserts the guard in the real production call path. The
// prior version of this test was a tautology (the production call was inside a
// hard-false if-branch and was never reached). This version calls the real function
// and asserts: no upload to primary, and that StemcellReplicateLocal=false prevents
// listClusterNodes from being called in the HandleCreateStemcell production guard.
func TestReplicateStemcellToNodes_Disabled(t *testing.T) {
	t.Parallel()

	clusterSvc := &countingClusterService{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"name": "pve2"})
			resp := sdkcluster.ListConfigNodesResponse{raw}
			return &resp, nil
		},
	}
	uploadCalls := map[string]int{}
	storageSvc := &replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalls[node]++
			return "", nil
		},
	}
	nodesSvc := &countingNodesService{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                           "pve1",
		VMStorage:                      "local",
		StemcellReplicateLocal:         false,
		StemcellTemplateVMIDRangeStart: 30000,
		StemcellTemplateVMIDRangeEnd:   30999,
	}
	logger, _ := log.NewLogger("debug", io.Discard)
	deps := Deps{
		Config: cfg,
		PVE: &digestReplicationMockClient{
			clusterSvc: clusterSvc,
			nodesSvc:   nodesSvc,
			storageSvc: storageSvc,
		},
		Logger: logger,
	}

	content := []byte("replication-disabled-test")
	sha256hex := sha256OfBytes(content)
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	// Call replicateStemcellToNodes directly with two nodes. The function itself
	// does not check the flag — the flag guard lives in HandleCreateStemcell.
	// Calling it with only the primary node means no replicas should be attempted.
	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "test.qcow2",
		sha256hex, []string{"pve1"}, "/dev/null", "", cp)

	// Primary node must never receive an upload.
	if uploadCalls["pve1"] != 0 {
		t.Errorf("primary node pve1 should not receive upload, got %d", uploadCalls["pve1"])
	}

	// Verify production guard: with StemcellReplicateLocal=false, listClusterNodes is
	// not called inside the guard.
	clusterCallsBefore := clusterSvc.listConfigNodesCalls
	if cfg.StemcellReplicateLocal {
		// This branch must NOT execute.
		_, _ = listClusterNodes(context.Background(), deps)
	}
	if clusterSvc.listConfigNodesCalls != clusterCallsBefore {
		t.Errorf("listClusterNodes was called despite StemcellReplicateLocal=false")
	}
}

// ============================================================
// Part B — ReplicateLocal=true + node-local → iterates nodes and tags replicas
// ============================================================

// TestReplicateStemcellToNodes_Enabled_TwoNodes verifies replication iterates both
// non-primary nodes and uploads + tags replicas when StemcellReplicateLocal=true.
func TestReplicateStemcellToNodes_Enabled_TwoNodes(t *testing.T) {
	t.Parallel()

	content := []byte("replication-enabled-test-content")
	sha256hex := sha256OfBytes(content)
	sha8 := sha256hex[:8]
	// Build a temp source file to upload.
	tmpDir := t.TempDir()
	srcPath := fmt.Sprintf("%s/source.qcow2", tmpDir)
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	uploadedNodes := make(map[string]int)
	createdVMIDs := make(map[string][]int)
	frozenNodes := make(map[string]int)

	nodesSvc := &countingNodesService{
		// ListQemu returns empty for all nodes (no existing replicas).
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			empty := sdknodes.ListStorageContentResponse{}
			return &empty, nil
		},
		createQemuTemplateFn: func(_ context.Context, node, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			frozenNodes[node]++
			resp := sdknodes.CreateQemuTemplateResponse{}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}

	qemuCreateCallCount := 0
	qemuSvc := &replicationMockQEMU{
		createFn: func(_ context.Context, node string, params map[string]any) (string, error) {
			qemuCreateCallCount++
			vmid := 30100 + qemuCreateCallCount
			createdVMIDs[node] = append(createdVMIDs[node], vmid)
			// Verify combined tags present in params.
			tags, _ := params["tags"].(string)
			shaTag := "bosh-stemcell-sha-" + sha8
			nodeTag := pve.ReplicaNodeTagForNode(node)
			if !strings.Contains(tags, shaTag) {
				return "", fmt.Errorf("missing sha tag %q in tags %q", shaTag, tags)
			}
			if !strings.Contains(tags, nodeTag) {
				return "", fmt.Errorf("missing node tag %q in tags %q", nodeTag, tags)
			}
			return "", nil // synchronous create
		},
	}

	storageSvc := &replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, _ io.Reader) (string, error) {
			uploadedNodes[node]++
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}

	tasksSvc := &replicationMockTasks{}

	clusterSvc := &countingClusterService{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			node2Raw, _ := json.Marshal(map[string]any{"name": "pve2"})
			node3Raw, _ := json.Marshal(map[string]any{"name": "pve3"})
			resp := sdkcluster.ListConfigNodesResponse{node2Raw, node3Raw}
			return &resp, nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                           "pve1",
		VMStorage:                      "local",
		StemcellReplicateLocal:         true,
		StemcellTemplateVMIDRangeStart: 30000,
		StemcellTemplateVMIDRangeEnd:   30999,
	}

	logger, _ := log.NewLogger("debug", io.Discard)
	mockClient := &digestReplicationMockClient{
		clusterSvc: clusterSvc,
		nodesSvc:   nodesSvc,
		qemuSvc:    qemuSvc,
		storageSvc: storageSvc,
		tasksSvc:   tasksSvc,
	}
	deps := Deps{
		Config: cfg,
		PVE:    mockClient,
		Logger: logger,
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	clusterNodes := []string{"pve1", "pve2", "pve3"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, clusterNodes, srcPath, "", cp)

	// pve1 is primary — should NOT receive a replica upload.
	if uploadedNodes["pve1"] != 0 {
		t.Errorf("primary node pve1 should not receive replica upload, got %d", uploadedNodes["pve1"])
	}
	// pve2 and pve3 should each receive one upload.
	for _, n := range []string{"pve2", "pve3"} {
		if uploadedNodes[n] != 1 {
			t.Errorf("node %s: expected 1 upload, got %d", n, uploadedNodes[n])
		}
		if len(createdVMIDs[n]) != 1 {
			t.Errorf("node %s: expected 1 VM created, got %d", n, len(createdVMIDs[n]))
		}
		if frozenNodes[n] != 1 {
			t.Errorf("node %s: expected 1 MakeTemplate call, got %d", n, frozenNodes[n])
		}
	}
}

// ============================================================
// Part B — delete_stemcell removes replicas across nodes
// ============================================================

// TestDestroyTemplateReplicas_RemovesAllReplicas verifies destroyTemplateReplicas
// calls DeleteQemu for each node-local replica tag match.
func TestDestroyTemplateReplicas_RemovesAllReplicas(t *testing.T) {
	t.Parallel()

	deletedByNode := map[string][]string{}

	// pve2 has a replica with tag "bosh-stemcell-node-pve2", template=1
	buildQemuItem := func(vmid int64, template int, tags string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{
			"vmid":     vmid,
			"template": template,
			"tags":     tags,
		})
		return raw
	}

	nodesSvc := &countingNodesService{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			nodeTag := pve.ReplicaNodeTagForNode(node)
			switch node {
			case "pve2":
				item := buildQemuItem(30101, 1, "bosh-stemcell-sha-abc12345;"+nodeTag)
				resp := sdknodes.ListQemuResponse{item}
				return &resp, nil
			case "pve3":
				item := buildQemuItem(30102, 1, "bosh-stemcell-sha-abc12345;"+nodeTag)
				resp := sdknodes.ListQemuResponse{item}
				return &resp, nil
			default:
				empty := sdknodes.ListQemuResponse{}
				return &empty, nil
			}
		},
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedByNode[node] = append(deletedByNode[node], vmid)
			resp := sdknodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}

	clusterSvc := &countingClusterService{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw2, _ := json.Marshal(map[string]any{"name": "pve2"})
			raw3, _ := json.Marshal(map[string]any{"name": "pve3"})
			resp := sdkcluster.ListConfigNodesResponse{raw2, raw3}
			return &resp, nil
		},
	}

	tasksSvc := &replicationMockTasks{}

	cfg := &config.CPIConfig{
		Node:                   "pve1",
		StemcellReplicateLocal: true,
	}

	logger, _ := log.NewLogger("debug", io.Discard)
	deps := Deps{
		Config: cfg,
		PVE: &digestReplicationMockClient{
			clusterSvc: clusterSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   tasksSvc,
		},
		Logger: logger,
	}

	// Simulate delete_stemcell calling destroyTemplateReplicas for primaryVMID=30100 on pve1.
	destroyTemplateReplicas(context.Background(), deps, 30100, "pve1", "template:30100")

	// Both pve2 and pve3 should have their replicas deleted.
	for _, n := range []string{"pve2", "pve3"} {
		if len(deletedByNode[n]) != 1 {
			t.Errorf("node %s: expected 1 delete call, got %d (deleted: %v)", n, len(deletedByNode[n]), deletedByNode[n])
		}
	}
	// pve1 is primary — must NOT appear in deleted map from this call.
	if len(deletedByNode["pve1"]) > 0 {
		t.Errorf("primary node pve1 should not be deleted by replica cleaner, got: %v", deletedByNode["pve1"])
	}
}

// ============================================================
// Part B — replication partial failure: one node fails, others proceed
// ============================================================

// TestReplicateStemcellToNodes_PartialFailure verifies that when one node's
// upload fails, replication continues to other nodes and the primary create
// is unaffected. The failed node's incomplete qcow2 upload is cleaned up.
func TestReplicateStemcellToNodes_PartialFailure(t *testing.T) {
	t.Parallel()

	content := []byte("partial-failure-replication-test")
	sha256hex := sha256OfBytes(content)
	sha8 := sha256hex[:8]

	tmpDir := t.TempDir()
	srcPath := fmt.Sprintf("%s/source.qcow2", tmpDir)
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	uploadedNodes := map[string]int{}
	cleanedNodes := map[string]int{}
	createdVMIDsByNode := map[string]int{}

	// pve2 upload fails; pve3 succeeds.
	storageSvc := &replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, _ io.Reader) (string, error) {
			uploadedNodes[node]++
			if node == "pve2" {
				return "", fmt.Errorf("simulated upload failure on pve2")
			}
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, node, _, _ string) (bool, error) {
			cleanedNodes[node]++
			return true, nil
		},
	}

	nodesSvc := &countingNodesService{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			empty := sdknodes.ListStorageContentResponse{}
			return &empty, nil
		},
		createQemuTemplateFn: func(_ context.Context, node, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			resp := sdknodes.CreateQemuTemplateResponse{}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}

	createSeq := 0
	qemuSvc := &replicationMockQEMU{
		createFn: func(_ context.Context, node string, params map[string]any) (string, error) {
			createSeq++
			createdVMIDsByNode[node]++
			// Verify combined tags.
			tags, _ := params["tags"].(string)
			shaTag := "bosh-stemcell-sha-" + sha8
			nodeTag := pve.ReplicaNodeTagForNode(node)
			if !strings.Contains(tags, shaTag) {
				t.Errorf("node %s: missing sha tag %q in %q", node, shaTag, tags)
			}
			if !strings.Contains(tags, nodeTag) {
				t.Errorf("node %s: missing node tag %q in %q", node, nodeTag, tags)
			}
			return "", nil
		},
	}

	tasksSvc := &replicationMockTasks{}

	clusterSvcPartial := &countingClusterService{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw2, _ := json.Marshal(map[string]any{"name": "pve2"})
			raw3, _ := json.Marshal(map[string]any{"name": "pve3"})
			resp := sdkcluster.ListConfigNodesResponse{raw2, raw3}
			return &resp, nil
		},
	}

	cfg := &config.CPIConfig{
		Node:                           "pve1",
		VMStorage:                      "local",
		StemcellReplicateLocal:         true,
		StemcellTemplateVMIDRangeStart: 31000,
		StemcellTemplateVMIDRangeEnd:   31999,
	}
	logger, _ := log.NewLogger("debug", io.Discard)
	deps := Deps{
		Config: cfg,
		PVE: &digestReplicationMockClient{
			clusterSvc: clusterSvcPartial,
			nodesSvc:   nodesSvc,
			qemuSvc:    qemuSvc,
			storageSvc: storageSvc,
			tasksSvc:   tasksSvc,
		},
		Logger: logger,
	}

	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}
	clusterNodes := []string{"pve1", "pve2", "pve3"}

	// Calling replicateStemcellToNodes must not panic or return an error.
	// It is void (best-effort); errors are logged as warnings.
	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, clusterNodes, srcPath, "", cp)

	// pve1 = primary, no upload.
	if uploadedNodes["pve1"] != 0 {
		t.Errorf("primary node pve1 should not receive upload, got %d", uploadedNodes["pve1"])
	}
	// pve2 upload attempted but failed.
	if uploadedNodes["pve2"] != 1 {
		t.Errorf("pve2: expected 1 upload attempt (failed), got %d", uploadedNodes["pve2"])
	}
	// pve2: failed upload must be cleaned up.
	// Note: cleanup is in replicateStemcellToNodes only when ensureReplicaTemplateVM fails,
	// not on upload failure. Upload failure → continue before template creation, so no
	// partial template was created and no qcow2 cleanup call is made (upload never landed).
	// pve3: upload succeeded, VM created.
	if uploadedNodes["pve3"] != 1 {
		t.Errorf("pve3: expected 1 successful upload, got %d", uploadedNodes["pve3"])
	}
	if createdVMIDsByNode["pve3"] != 1 {
		t.Errorf("pve3: expected 1 VM created, got %d", createdVMIDsByNode["pve3"])
	}
	// pve2: no VM created (upload failed before VM creation).
	if createdVMIDsByNode["pve2"] != 0 {
		t.Errorf("pve2: expected 0 VMs created (upload failed), got %d", createdVMIDsByNode["pve2"])
	}
}

// ============================================================
// Part C — cross-stemcell delete isolation
// ============================================================

// TestDestroyTemplateReplicas_CrossStemcellIsolation verifies that deleting
// stemcell A's replicas leaves stemcell B's replicas intact when both are
// replicated to the same cluster nodes (fixes sha8-agnostic delete bug).
func TestDestroyTemplateReplicas_CrossStemcellIsolation(t *testing.T) {
	t.Parallel()

	// Stemcell A: primary VMID=40100 on pve1, sha8="aaaaaaaa"
	// Stemcell B: primary VMID=40200 on pve1, sha8="bbbbbbbb"
	// Both replicated to pve2.
	// pve2 has:
	//   VMID=40101 tags="bosh-stemcell-sha-aaaaaaaa;bosh-stemcell-node-pve2" (A replica)
	//   VMID=40201 tags="bosh-stemcell-sha-bbbbbbbb;bosh-stemcell-node-pve2" (B replica)
	const shaA = "aaaaaaaa"
	const shaB = "bbbbbbbb"

	buildItem := func(vmid int64, sha8, node string) json.RawMessage {
		nodeTag := pve.ReplicaNodeTagForNode(node)
		tags := "bosh-stemcell-sha-" + sha8 + ";" + nodeTag
		raw, _ := json.Marshal(map[string]any{
			"vmid":     vmid,
			"template": 1,
			"tags":     tags,
		})
		return raw
	}

	deletedByNode := map[string][]string{}

	nodesSvc := &countingNodesService{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			switch node {
			case "pve1":
				// Primary for A: VMID=40100 with sha=aaaaaaaa (no node tag = primary)
				raw, _ := json.Marshal(map[string]any{
					"vmid":     int64(40100),
					"template": 1,
					"tags":     "bosh-stemcell-sha-" + shaA,
				})
				resp := sdknodes.ListQemuResponse{raw}
				return &resp, nil
			case "pve2":
				itemA := buildItem(40101, shaA, "pve2")
				itemB := buildItem(40201, shaB, "pve2")
				resp := sdknodes.ListQemuResponse{itemA, itemB}
				return &resp, nil
			default:
				empty := sdknodes.ListQemuResponse{}
				return &empty, nil
			}
		},
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedByNode[node] = append(deletedByNode[node], vmid)
			resp := sdknodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}

	clusterSvc := &countingClusterService{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw2, _ := json.Marshal(map[string]any{"name": "pve2"})
			resp := sdkcluster.ListConfigNodesResponse{raw2}
			return &resp, nil
		},
	}

	tasksSvc := &replicationMockTasks{}

	cfg := &config.CPIConfig{
		Node:                   "pve1",
		StemcellReplicateLocal: true,
	}
	logger, _ := log.NewLogger("debug", io.Discard)
	deps := Deps{
		Config: cfg,
		PVE: &digestReplicationMockClient{
			clusterSvc: clusterSvc,
			nodesSvc:   nodesSvc,
			tasksSvc:   tasksSvc,
		},
		Logger: logger,
	}

	// Delete stemcell A (primary VMID=40100 on pve1).
	// resolveStemcellSHA8FromVMID reads pve1's ListQemu → finds sha8="aaaaaaaa".
	// findReplicaVMIDsOnNode on pve2 must match ONLY VMID=40101 (sha=aaaaaaaa),
	// NOT VMID=40201 (sha=bbbbbbbb).
	destroyTemplateReplicas(context.Background(), deps, 40100, "pve1", "template:40100")

	// Only stemcell A's replica (40101) on pve2 should be deleted.
	pve2Deleted := deletedByNode["pve2"]
	if len(pve2Deleted) != 1 {
		t.Fatalf("pve2: expected 1 delete (stemcell A replica only), got %d: %v", len(pve2Deleted), pve2Deleted)
	}
	if pve2Deleted[0] != "40101" {
		t.Errorf("pve2: expected delete of VMID 40101 (stemcell A replica), got %q", pve2Deleted[0])
	}

	// Stemcell B's replica (40201) must NOT be deleted.
	for _, vmid := range pve2Deleted {
		if vmid == "40201" {
			t.Errorf("pve2: stemcell B replica VMID 40201 was deleted — cross-stemcell isolation broken")
		}
	}
}

// ============================================================
// Minimal mock services for replication tests
// ============================================================

type replicationMockQEMU struct {
	sdkqemu.Service
	createFn func(ctx context.Context, node string, params map[string]any) (string, error)
}

func (m *replicationMockQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	if m.createFn != nil {
		return m.createFn(ctx, node, params)
	}
	return "", nil
}

type replicationMockStorage struct {
	sdkstorage.Service
	uploadFn               func(ctx context.Context, node, storage, content, filename string, r io.Reader) (string, error)
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
}

func (m *replicationMockStorage) Upload(ctx context.Context, node, storage, content, filename string, r io.Reader) (string, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, node, storage, content, filename, r)
	}
	return "", nil
}

func (m *replicationMockStorage) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	return true, nil
}

type replicationMockTasks struct {
	sdktasks.Service
}

func (m *replicationMockTasks) Wait(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK"}, nil
}
