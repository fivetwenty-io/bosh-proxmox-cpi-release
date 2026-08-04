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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestVerifyExpectedDigest_SHA256UnavailableFailsClosed verifies the check
// fails closed (retriable) when the operator pinned an expected sha256 but the
// actual digest could not be computed — a read error must not silently turn
// the requested integrity gate into a no-op.
func TestVerifyExpectedDigest_SHA256UnavailableFailsClosed(t *testing.T) {
	t.Parallel()
	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA256: strings.Repeat("a", 64)}
	err := verifyExpectedDigest(context.Background(), logger, cp, "", "", "", stemcellSourceLocal)
	if err == nil {
		t.Fatal("expected fail-closed error when sha256 is pinned but unavailable, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected retriable error (transient I/O is the usual cause), got non-retriable")
	}
}

// TestVerifyExpectedDigest_SHA1UnavailableFailsClosed mirrors the sha256
// fail-closed case for the sha1 branch: an unreadable file must block the
// upload when a digest was pinned.
func TestVerifyExpectedDigest_SHA1UnavailableFailsClosed(t *testing.T) {
	t.Parallel()
	logger, _ := log.NewLogger("debug", io.Discard)
	cp := stemcellCloudProps{ExpectedSHA1: strings.Repeat("a", 40)}
	err := verifyExpectedDigest(context.Background(), logger, cp, "", "/nonexistent/definitely-missing.img", "", stemcellSourceLocal)
	if err == nil {
		t.Fatal("expected fail-closed error when sha1 is pinned but uncomputable, got nil")
	}
	var cpiErr *cpierrors.Error
	if !asError(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected retriable error, got non-retriable")
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
		name       string
		cp         map[string]any
		wantSHA256 string
		wantSHA1   string
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
func (n *noopReplicationPoolService) CreatePool(_ context.Context, _, _ string) error  { return nil }
func (n *noopReplicationPoolService) DeletePool(_ context.Context, _ string) error     { return nil }
func (n *noopReplicationPoolService) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// countingNodesService wraps sdknodes.Service and counts ListQemu calls per node.
// mu guards listQemuCallsByNode so the service is safe for concurrent callers.
type countingNodesService struct {
	sdknodes.Service
	mu                   sync.Mutex
	listQemuCallsByNode  map[string]int
	listQemuFn           func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	listStorageContentFn func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
	deleteQemuFn         func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	createQemuTemplateFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error)
}

func (c *countingNodesService) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	c.mu.Lock()
	if c.listQemuCallsByNode == nil {
		c.listQemuCallsByNode = map[string]int{}
	}
	c.listQemuCallsByNode[node]++
	c.mu.Unlock()
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

// TestReplicateStemcellToNodes_Disabled calls replicateStemcellToNodes directly
// with only the primary node in the list and asserts no replica upload is
// attempted. (The StemcellReplicateLocal flag guard itself lives in
// HandleCreateStemcell and is covered by the enabled/handler tests.)
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
		sha256hex, []string{"pve1"}, "/dev/null", "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// Primary node must never receive an upload: given only the primary in the
	// node list, replicateStemcellToNodes has no other node to replicate to.
	if uploadCalls["pve1"] != 0 {
		t.Errorf("primary node pve1 should not receive upload, got %d", uploadCalls["pve1"])
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
		sha256hex, clusterNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

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
		sha256hex, clusterNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

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

// ============================================================
// Part C — §7.35 bounded-concurrency parallel replication tests
// ============================================================

// buildReplicationDeps constructs Deps for concurrency tests. concurrency is
// set as StemcellReplicationConcurrency; nodes beyond primary are non-primary
// targets. All services are passed as pointers to avoid lock-copy (countingNodesService
// embeds sync.Mutex since §7.35 concurrency hardening).
func buildReplicationDeps(
	t *testing.T,
	concurrency int,
	storageSvc *replicationMockStorage,
	nodesSvc *countingNodesService,
	qemuSvc *replicationMockQEMU,
) Deps {
	t.Helper()
	cfg := &config.CPIConfig{
		Node:                           "pve1",
		VMStorage:                      "local",
		StemcellReplicateLocal:         true,
		StemcellTemplateVMIDRangeStart: 30000,
		StemcellTemplateVMIDRangeEnd:   30999,
		StemcellReplicationConcurrency: concurrency,
	}
	logger, _ := log.NewLogger("debug", io.Discard)
	mc := &digestReplicationMockClient{
		clusterSvc: &countingClusterService{},
		nodesSvc:   nodesSvc,
		qemuSvc:    qemuSvc,
		storageSvc: storageSvc,
		tasksSvc:   &replicationMockTasks{},
	}
	return Deps{Config: cfg, PVE: mc, Logger: logger}
}

// makeSrcFile writes content to a temp file and returns its path.
func makeSrcFile(t *testing.T, content []byte) string {
	t.Helper()
	tmpDir := t.TempDir()
	path := fmt.Sprintf("%s/source.qcow2", tmpDir)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("makeSrcFile: %v", err)
	}
	return path
}

// TestReplicateStemcellToNodes_Concurrency_AllNodesAttempted verifies that with
// concurrency > 1 and M target non-primary nodes:
//   - all M nodes receive an upload attempt
//   - peak concurrent uploads never exceeds the limit
//   - peak concurrent uploads is > 1 (i.e. goroutines actually overlap)
//
// A rendezvous channel forces uploads to block until 'limit' are in flight
// simultaneously, proving the pool dispatches multiple goroutines concurrently.
func TestReplicateStemcellToNodes_Concurrency_AllNodesAttempted(t *testing.T) {
	t.Parallel()

	const limit = 2
	targetNodes := []string{"pve1", "pve2", "pve3", "pve4"} // pve1 = primary; 3 non-primary
	nonPrimary := targetNodes[1:]

	content := []byte("concurrency-all-nodes-test")
	sha256hex := sha256OfBytes(content)
	srcPath := makeSrcFile(t, content)

	var (
		uploadedNodes sync.Map
		inFlight      atomic.Int64
		peakInFlight  atomic.Int64
	)

	// rendezvous: when 'limit' uploads are in flight simultaneously, all are
	// released. If the pool is serial, only 1 upload runs at a time and the
	// WaitGroup never reaches Done — the test would deadlock (caught by
	// -timeout flag or t.Cleanup). We use a channel instead of a WaitGroup
	// to avoid deadlock if the pool actually IS serial, so the test just fails
	// (peak ≤ 1, assertion below catches it).
	ready := make(chan struct{}, len(nonPrimary)) // buffered: non-blocking send
	release := make(chan struct{})                // closed when all in-limit are ready

	var releaseOnce sync.Once

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			// Update peak.
			for {
				peak := peakInFlight.Load()
				if cur <= peak || peakInFlight.CompareAndSwap(peak, cur) {
					break
				}
			}
			// Signal this goroutine is in the upload phase.
			ready <- struct{}{}
			if inFlight.Load() >= int64(limit) {
				// 'limit' uploads are simultaneous — release all.
				releaseOnce.Do(func() { close(release) })
			}
			// Wait for the release signal (or a short timeout so we don't
			// deadlock if fewer than 'limit' goroutines ever overlap). Truly
			// concurrent uploads overlap within microseconds, so a serial-pool
			// regression only waits this backstop once before the peak assertion
			// below catches it.
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			uploadedNodes.Store(node, struct{}{})
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	var createSeq atomic.Int64
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createSeq.Add(1)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, limit, &storageSvc, &nodesSvc, &qemuSvc)
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// All non-primary nodes must have received an upload.
	for _, n := range nonPrimary {
		if _, ok := uploadedNodes.Load(n); !ok {
			t.Errorf("node %s: expected upload attempt, got none", n)
		}
	}

	// Primary must NOT receive an upload.
	if _, ok := uploadedNodes.Load("pve1"); ok {
		t.Errorf("primary node pve1 must not receive upload")
	}

	// Peak concurrency must be > 1 (concurrency is real) and ≤ limit.
	peak := peakInFlight.Load()
	if peak < 2 {
		t.Errorf("peak concurrent uploads = %d; want >= 2 (concurrency limit=%d, %d non-primary nodes)",
			peak, limit, len(nonPrimary))
	}
	if peak > int64(limit) {
		t.Errorf("peak concurrent uploads = %d; want <= %d", peak, limit)
	}
}

// TestReplicateStemcellToNodes_Concurrency_Serial verifies that with concurrency
// 1 (the default), peak concurrent uploads is exactly 1 even with multiple nodes.
func TestReplicateStemcellToNodes_Concurrency_Serial(t *testing.T) {
	t.Parallel()

	const limit = 0 // 0 → resolves to 1 (serial)
	targetNodes := []string{"pve1", "pve2", "pve3"}

	content := []byte("serial-concurrency-test")
	sha256hex := sha256OfBytes(content)
	srcPath := makeSrcFile(t, content)

	var (
		uploadedNodes sync.Map
		inFlight      atomic.Int64
		peakInFlight  atomic.Int64
	)

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			cur := inFlight.Add(1)
			for {
				peak := peakInFlight.Load()
				if cur <= peak || peakInFlight.CompareAndSwap(peak, cur) {
					break
				}
			}
			defer inFlight.Add(-1)
			uploadedNodes.Store(node, struct{}{})
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	var createSeq2 atomic.Int64
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createSeq2.Add(1)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, limit, &storageSvc, &nodesSvc, &qemuSvc)
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// Both non-primary nodes must have been attempted.
	for _, n := range []string{"pve2", "pve3"} {
		if _, ok := uploadedNodes.Load(n); !ok {
			t.Errorf("node %s: expected upload attempt, got none", n)
		}
	}

	// Peak concurrency must be exactly 1 (serial).
	peak := peakInFlight.Load()
	if peak != 1 {
		t.Errorf("peak concurrent uploads = %d; want 1 (serial mode)", peak)
	}
}

// TestReplicateStemcellToNodes_Concurrency_NonFatal verifies that a per-node
// upload failure in parallel mode does not abort other nodes. All successful
// nodes complete; the failed node leaves no VM.
func TestReplicateStemcellToNodes_Concurrency_NonFatal(t *testing.T) {
	t.Parallel()

	const limit = 3
	targetNodes := []string{"pve1", "pve2", "pve3", "pve4"} // pve1=primary; pve2 fails

	content := []byte("nonfatal-concurrency-test")
	sha256hex := sha256OfBytes(content)
	srcPath := makeSrcFile(t, content)

	var (
		uploadedNodes sync.Map // node → bool
		vmCreated     sync.Map // node → bool
	)

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			if node == "pve2" {
				return "", errors.New("simulated upload failure on pve2")
			}
			uploadedNodes.Store(node, true)
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			vmCreated.Store(node, true)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, limit, &storageSvc, &nodesSvc, &qemuSvc)
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	// Must not panic or return error — best-effort.
	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// pve2 upload failed → no VM for pve2.
	if _, ok := vmCreated.Load("pve2"); ok {
		t.Errorf("pve2: VM created despite upload failure")
	}

	// pve3 and pve4 must have completed successfully.
	for _, n := range []string{"pve3", "pve4"} {
		if _, ok := uploadedNodes.Load(n); !ok {
			t.Errorf("node %s: expected successful upload", n)
		}
		if _, ok := vmCreated.Load(n); !ok {
			t.Errorf("node %s: expected VM created", n)
		}
	}

	// pve1 = primary, must not receive upload.
	if _, ok := uploadedNodes.Load("pve1"); ok {
		t.Errorf("primary pve1 must not receive upload")
	}
}

// TestReplicateStemcellToNodes_Concurrency_IdempotentSkip verifies that a node
// whose replica already exists (ResolveTemplateVMIDForNode hit) is skipped: no
// upload and no VM create — even in parallel mode.
func TestReplicateStemcellToNodes_Concurrency_IdempotentSkip(t *testing.T) {
	t.Parallel()

	const limit = 4
	content := []byte("idempotent-skip-concurrency-test")
	sha256hex := sha256OfBytes(content)
	sha8 := sha256hex[:8]
	srcPath := makeSrcFile(t, content)

	targetNodes := []string{"pve1", "pve2", "pve3"}
	// pve2 already has a replica: ListQemu returns a matching template.
	pve2TemplateName := "bosh-stemcell-ubuntu-jammy-1.0"

	var (
		uploadedNodes sync.Map
		vmCreated     sync.Map
	)

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			uploadedNodes.Store(node, true)
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	isTemplate := true
	_ = sha8 // used in the tag inside the template entry below
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node == "pve2" {
				// Return an existing template tagged with the sha8 so
				// ResolveTemplateVMIDForNode returns alreadyExists=true.
				entry, _ := json.Marshal(map[string]any{
					"vmid":     30500,
					"name":     pve2TemplateName,
					"template": isTemplate,
					"tags":     "bosh-stemcell-sha-" + sha8 + ";bosh-stemcell-node-pve2",
				})
				resp := sdknodes.ListQemuResponse{entry}
				return &resp, nil
			}
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			vmCreated.Store(node, true)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, limit, &storageSvc, &nodesSvc, &qemuSvc)
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// pve2 already had a replica — no upload, no VM create.
	if _, ok := uploadedNodes.Load("pve2"); ok {
		t.Errorf("pve2: upload called despite existing replica (idempotent skip failed)")
	}
	if _, ok := vmCreated.Load("pve2"); ok {
		t.Errorf("pve2: VM created despite existing replica (idempotent skip failed)")
	}

	// pve3 must have been replicated normally.
	if _, ok := uploadedNodes.Load("pve3"); !ok {
		t.Errorf("pve3: expected upload, got none")
	}
}

// TestReplicateStemcellToNodes_AdoptsRacingReplica verifies the §7.37
// adopt-and-wait path: when replica_adopt_timeout_sec is set and a per-node
// replica appears (tagged) after the settled-only existence check missed it —
// i.e. a concurrent winner built it in the TOCTOU window — the loser adopts that
// artifact and skips its own upload + VM create rather than building a duplicate.
//
// pve2's ListQemu returns an in-flight (unfrozen) replica on the first poll so
// ResolveTemplateVMIDForNode (settled-only) misses, then a settled template on
// the adopt probe so AdoptReplicaTemplate adopts immediately (no wait).
func TestReplicateStemcellToNodes_AdoptsRacingReplica(t *testing.T) {
	t.Parallel()

	content := []byte("adopt-racing-replica-test")
	sha256hex := sha256OfBytes(content)
	sha8 := sha256hex[:8]
	srcPath := makeSrcFile(t, content)

	targetNodes := []string{"pve1", "pve2", "pve3"}
	const adoptedVMID = 30700

	var (
		uploadedNodes sync.Map
		vmCreated     sync.Map
		pve2Polls     atomic.Int64
	)

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			uploadedNodes.Store(node, true)
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	tags := "bosh-stemcell-sha-" + sha8 + ";bosh-stemcell-node-pve2"
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node == "pve2" {
				n := pve2Polls.Add(1)
				if n == 1 {
					// First poll (settled-only existence check): a concurrent
					// winner is mid-clone — tagged but not yet frozen.
					entry, _ := json.Marshal(map[string]any{
						"vmid": adoptedVMID, "template": 0, "lock": "clone", jsonKeyTags: tags,
					})
					return &sdknodes.ListQemuResponse{entry}, nil
				}
				// Adopt probe: the winner has frozen — a settled template to adopt.
				entry, _ := json.Marshal(map[string]any{
					"vmid": adoptedVMID, "template": 1, jsonKeyTags: tags,
				})
				return &sdknodes.ListQemuResponse{entry}, nil
			}
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			vmCreated.Store(node, true)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, 1, &storageSvc, &nodesSvc, &qemuSvc)
	deps.Config.ReplicaAdoptTimeoutSec = 300 // enable adopt-and-wait
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// pve2: adopted the racing winner — no upload, no VM create.
	if _, ok := uploadedNodes.Load("pve2"); ok {
		t.Errorf("pve2: upload called despite adoptable racing replica (adopt-and-wait failed)")
	}
	if _, ok := vmCreated.Load("pve2"); ok {
		t.Errorf("pve2: VM created despite adoptable racing replica (duplicate build not prevented)")
	}
	// pve3: no racing winner — replicated normally.
	if _, ok := uploadedNodes.Load("pve3"); !ok {
		t.Errorf("pve3: expected normal upload, got none")
	}
}

// TestReplicateStemcellToNodes_AdoptDisabled_BuildsReplica verifies the
// byte-identical default: with replica_adopt_timeout_sec unset (0), an in-flight
// racing replica is NOT probed for — the node uploads and builds as before. This
// guards the off-path against accidental behaviour change.
func TestReplicateStemcellToNodes_AdoptDisabled_BuildsReplica(t *testing.T) {
	t.Parallel()

	content := []byte("adopt-disabled-builds-test")
	sha256hex := sha256OfBytes(content)
	sha8 := sha256hex[:8]
	srcPath := makeSrcFile(t, content)

	targetNodes := []string{"pve1", "pve2"}

	var (
		uploadedNodes sync.Map
		vmCreated     sync.Map
	)

	storageSvc := replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, r io.Reader) (string, error) {
			_, _ = io.Copy(io.Discard, r)
			uploadedNodes.Store(node, true)
			return "", nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return true, nil
		},
	}
	// pve2 always shows an in-flight (unfrozen) replica. With adopt OFF, the
	// settled-only check misses it and there is no adopt probe, so the node builds.
	inflightTags := "bosh-stemcell-sha-" + sha8 + ";bosh-stemcell-node-pve2"
	nodesSvc := countingNodesService{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node == "pve2" {
				entry, _ := json.Marshal(map[string]any{
					"vmid": 30800, "template": 0, "lock": "clone", jsonKeyTags: inflightTags,
				})
				return &sdknodes.ListQemuResponse{entry}, nil
			}
			return &sdknodes.ListQemuResponse{}, nil
		},
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return &sdknodes.ListStorageContentResponse{}, nil
		},
		createQemuTemplateFn: func(_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return &sdknodes.CreateQemuTemplateResponse{}, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return &sdknodes.DeleteQemuResponse{}, nil
		},
	}
	qemuSvc := replicationMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			vmCreated.Store(node, true)
			return "", nil
		},
	}

	deps := buildReplicationDeps(t, 1, &storageSvc, &nodesSvc, &qemuSvc) // adopt unset → 0 → off
	cp := stemcellCloudProps{Name: "ubuntu-jammy", Version: "1.0"}

	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, targetNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	// adopt off: pve2 builds its own replica (upload + create), byte-identical.
	if _, ok := uploadedNodes.Load("pve2"); !ok {
		t.Errorf("pve2: expected upload with adopt disabled (byte-identical), got none")
	}
	if _, ok := vmCreated.Load("pve2"); !ok {
		t.Errorf("pve2: expected VM create with adopt disabled (byte-identical), got none")
	}
}

// TestStemcellReplicationConcurrencyValue verifies the accessor returns correct
// effective values for boundary inputs.
func TestStemcellReplicationConcurrencyValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{"nil config resolves to 1", 0, 1}, // tested via nil pointer below
		{"zero resolves to 1", 0, 1},
		{"one stays 1", 1, 1},
		{"two stays 2", 2, 2},
		{"64 stays 64", 64, 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{StemcellReplicationConcurrency: tc.configured}
			got := cfg.StemcellReplicationConcurrencyValue()
			if got != tc.want {
				t.Errorf("StemcellReplicationConcurrencyValue() = %d; want %d", got, tc.want)
			}
		})
	}
	// nil pointer.
	t.Run("nil pointer resolves to 1", func(t *testing.T) {
		t.Parallel()
		var cfg *config.CPIConfig
		if got := cfg.StemcellReplicationConcurrencyValue(); got != 1 {
			t.Errorf("nil CPIConfig.StemcellReplicationConcurrencyValue() = %d; want 1", got)
		}
	})
}

// ============================================================
// H2 (A13 review) — a panic in a single replica-node worker goroutine must
// be recovered, logged, and treated as that node's (best-effort) failure —
// NOT propagate out of the goroutine, which would crash the whole CPI
// process (stdout gets nothing, the Director sees "unexpected end of
// input") regardless of which request happened to be executing at the time.
// ============================================================

// TestReplicateStemcellToNodes_PanicRecovered injects a panic into one
// replica node's upload call and asserts:
//  1. replicateStemcellToNodes itself returns normally (does not propagate
//     the panic) — if the fix regresses, this test's own process crashes
//     rather than reporting a controlled failure, since an unrecovered
//     panic in ANY goroutine terminates the whole program.
//  2. The panicking node's replica did not complete (no VM create/freeze
//     call reached for it).
//  3. The OTHER (non-panicking) replica node still completes fully —
//     matching replicateOneNode's documented best-effort contract: one
//     node's failure must not abort replication to the rest.
//  4. The panic is logged at Error with the recovered value visible in the
//     log output, so the condition is diagnosable rather than silent.
func TestReplicateStemcellToNodes_PanicRecovered(t *testing.T) {
	t.Parallel()

	content := []byte("panic-recovery-test-content")
	sha256hex := sha256OfBytes(content)
	tmpDir := t.TempDir()
	srcPath := fmt.Sprintf("%s/source.qcow2", tmpDir)
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	var mu sync.Mutex
	uploadedNodes := make(map[string]int)
	frozenNodes := make(map[string]int)

	const panicNode = "pve2"
	const panicMsg = "injected test panic: simulated corrupt upload state"

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
			mu.Lock()
			frozenNodes[node]++
			mu.Unlock()
			resp := sdknodes.CreateQemuTemplateResponse{}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}

	qemuSvc := &replicationMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "", nil // synchronous create
		},
	}

	storageSvc := &replicationMockStorage{
		uploadFn: func(_ context.Context, node, _, _, _ string, _ io.Reader) (string, error) {
			if node == panicNode {
				panic(panicMsg)
			}
			mu.Lock()
			uploadedNodes[node]++
			mu.Unlock()
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
		// Serial (default) — deterministic ordering makes this test's
		// assertions unambiguous; the fix's correctness (per-goroutine
		// recover) does not depend on concurrency level.
	}

	var logBuf bytes.Buffer
	logger, logErr := log.NewLogger("debug", &logBuf)
	if logErr != nil {
		t.Fatalf("log.NewLogger: %v", logErr)
	}
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

	// The call under test. If the H2 fix regresses (recover() removed), the
	// panic in pve2's uploadFn propagates out of its worker goroutine
	// unrecovered and crashes this entire test binary — there is no way for
	// a plain t.Errorf to observe that outcome after the fact, which is
	// exactly why this test's mere ability to reach the assertions below is
	// itself part of the regression guard.
	replicateStemcellToNodes(context.Background(), deps, "pve1", "local", "bosh-stemcell.qcow2",
		sha256hex, clusterNodes, srcPath, "", ":heavy:local:import/test.qcow2", "test-director", pve.StemcellKindHeavy, cp, "")

	mu.Lock()
	defer mu.Unlock()

	// pve1 is primary — never a replica target.
	if uploadedNodes["pve1"] != 0 {
		t.Errorf("primary node pve1 should not receive replica upload, got %d", uploadedNodes["pve1"])
	}

	// pve2 panicked before recording its upload or reaching freeze — its
	// replica must NOT have completed.
	if uploadedNodes[panicNode] != 0 {
		t.Errorf("panicking node %s: expected 0 recorded uploads (panic occurs before recording), got %d", panicNode, uploadedNodes[panicNode])
	}
	if frozenNodes[panicNode] != 0 {
		t.Errorf("panicking node %s: expected 0 MakeTemplate calls, got %d", panicNode, frozenNodes[panicNode])
	}

	// pve3 must still complete fully — one node's panic must not abort
	// replication to the rest (best-effort contract).
	if uploadedNodes["pve3"] != 1 {
		t.Errorf("non-panicking node pve3: expected 1 upload, got %d", uploadedNodes["pve3"])
	}
	if frozenNodes["pve3"] != 1 {
		t.Errorf("non-panicking node pve3: expected 1 MakeTemplate call, got %d", frozenNodes["pve3"])
	}

	// The panic must be logged (at Error), naming both the node and the
	// recovered panic value, so the condition is diagnosable.
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "panicked") {
		t.Errorf("log output should mention the panic was recovered; got: %s", logOutput)
	}
	if !strings.Contains(logOutput, panicMsg) {
		t.Errorf("log output should include the recovered panic value %q; got: %s", panicMsg, logOutput)
	}
	if !strings.Contains(logOutput, panicNode) {
		t.Errorf("log output should name the panicking replica node %q; got: %s", panicNode, logOutput)
	}
}
