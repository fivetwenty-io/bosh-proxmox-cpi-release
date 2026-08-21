// Retryability boundary tests for handler error paths. Each case asserts
// OkToRetry() on the *cpierrors.Error found in the returned error chain.
//
// Scope: this file verifies that errors from PVE SDK calls in create_stemcell
// are correctly typed when they reach the handler's return boundary, so the
// dispatcher can set ok_to_retry faithfully. The primary gap closed here is
// the pools API on the template-create path (EnsurePoolExists before the
// create loop; AssignVMToPool on a lost race): a transient 503 must produce
// TypeRetriableCloud; a 400 must stay non-retriable.
package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// makeSdkAPIErr builds a sentinel-carrying SDK APIError for the given HTTP status.
// Uses ParseAPIError so errors.Is(err, sdkerrors.ErrServer) returns true for 5xx.
func makeSdkAPIErr(httpCode int, msg string) error {
	body := []byte(`{"message":"` + msg + `","code":` + strconv.Itoa(httpCode) + `}`)
	return sdkerrors.ParseAPIError(httpCode, body)
}

// retryBoundaryPoolService is a pve.PoolService whose mutating methods
// (AddVM, MoveVMToPool, CreatePool) always return err. With the template
// pool passed at qemu-create time, the first pools-API call on the critical
// path is EnsurePoolExists' CreatePool — that is the boundary these tests
// classify.
type retryBoundaryPoolService struct {
	err error
}

func (s *retryBoundaryPoolService) AddVM(_ context.Context, _ string, _ int64) error {
	return s.err
}
func (s *retryBoundaryPoolService) MoveVMToPool(_ context.Context, _ string, _ int64) error {
	return s.err
}
func (s *retryBoundaryPoolService) CreatePool(_ context.Context, _, _ string) error { return s.err }
func (s *retryBoundaryPoolService) DeletePool(_ context.Context, _ string) error    { return nil }
func (s *retryBoundaryPoolService) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// assertHandlerRetriable calls t.Fatal unless err's chain contains a *cpierrors.Error
// with OkToRetry()==want.
func assertHandlerRetriable(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected non-nil error from handler")
	}
	var e *cpierrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("error chain has no *cpierrors.Error: %T %v", err, err)
	}
	if e.OkToRetry() != want {
		t.Errorf("OkToRetry()=%v; want %v. Error: %v", e.OkToRetry(), want, err)
	}
}

// buildPoolBoundaryDeps constructs Deps for create_stemcell pool-boundary tests.
// Pool is named "test-pool" so ensureTemplateVM reaches EnsurePoolExists
// before its create loop. QEMU.Create, MakeTemplate, and task-wait all
// succeed; the pools API uses poolSvc.
func buildPoolBoundaryDeps(t *testing.T, poolSvc pve.PoolService) handlers.Deps {
	t.Helper()

	nodesSvc := &stemcellMockNodes{
		// Empty template list → create path through ensureTemplateVM.
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
		// Synchronous template freeze (no UPID → no task wait).
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			raw := sdknodes.CreateQemuTemplateResponse(`""`)
			return &raw, nil
		},
	}

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          nodesSvc,
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        &stemcellMockCluster{},
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: lightStemcellClusterStorage("local", "nfs", true),
		poolsSvc:          poolSvc,
	}

	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                 vmNode,
			StemcellStorage:      "local",
			VMStorage:            "local",
			DiskStorage:          "local",
			StemcellTemplatePool: "test-pool",
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// create_stemcell → ensureTemplateVM → pools API boundary
// ---------------------------------------------------------------------------

// TestCreateStemcell_PoolAssign_503_IsRetriable verifies that a transient 503
// from the pools API (EnsurePoolExists' CreatePool, the first pools call on
// the template-create path) propagates as TypeRetriableCloud from the handler.
// Before the wrapping fix, the raw SDK error reached the dispatcher catch-all
// and became non-retriable (OkToRetry=false).
func TestCreateStemcell_PoolAssign_503_IsRetriable(t *testing.T) {
	t.Parallel()

	deps := buildPoolBoundaryDeps(t, &retryBoundaryPoolService{
		err: makeSdkAPIErr(503, "service unavailable"),
	})

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-pool-503"})

	assertHandlerRetriable(t, err, true)

	var e *cpierrors.Error
	if errors.As(err, &e) && e.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("error type = %q; want TypeRetriableCloud", e.Type())
	}
}

// TestCreateStemcell_PoolAssign_400_IsNotRetriable verifies that a permanent
// 400 from the pools API stays non-retriable. A misconfigured pool name must
// surface immediately.
func TestCreateStemcell_PoolAssign_400_IsNotRetriable(t *testing.T) {
	t.Parallel()

	deps := buildPoolBoundaryDeps(t, &retryBoundaryPoolService{
		err: makeSdkAPIErr(400, "invalid pool name"),
	})

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-pool-400"})

	assertHandlerRetriable(t, err, false)
}

// TestCreateStemcell_PoolAssign_ConnectionError_IsRetriable verifies that a
// connection-refused error from the pools endpoint becomes TypeRetriableCloud.
func TestCreateStemcell_PoolAssign_ConnectionError_IsRetriable(t *testing.T) {
	t.Parallel()

	deps := buildPoolBoundaryDeps(t, &retryBoundaryPoolService{
		err: &sdkerrors.ConnectionError{
			Host:    "pve.test.local",
			Port:    8006,
			Message: "connection refused",
		},
	})

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-pool-conn"})

	assertHandlerRetriable(t, err, true)
}

// ---------------------------------------------------------------------------
// No-pool path: pool assignment is skipped → success
// ---------------------------------------------------------------------------

// TestCreateStemcell_NoPool_HappyPath verifies that when StemcellTemplatePool
// is empty, pool assignment is skipped and create_stemcell succeeds.
func TestCreateStemcell_NoPool_HappyPath(t *testing.T) {
	t.Parallel()

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        &stemcellMockCluster{},
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: lightStemcellClusterStorage("local", "nfs", true),
		// poolsSvc nil → noopPoolService; with empty StemcellTemplatePool it is never called.
	}

	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:            vmNode,
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
			// StemcellTemplatePool empty → pool assignment skipped.
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-nopool"})
	if err != nil {
		t.Fatalf("expected success with no pool; got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// ---------------------------------------------------------------------------
// Cluster ListResources transient error → retriable (VMID allocation path)
// ---------------------------------------------------------------------------

// TestCreateStemcell_ClusterListResources_503_IsRetriable verifies that a
// transient 503 from ListResources (used by NextVMID / AllocateWithRetry)
// propagates as TypeRetriableCloud from create_stemcell.
func TestCreateStemcell_ClusterListResources_503_IsRetriable(t *testing.T) {
	t.Parallel()

	transientErr := makeSdkAPIErr(503, "service unavailable")

	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, transientErr
		},
	}

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        clusterSvc,
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: lightStemcellClusterStorage("local", "nfs", true),
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

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	// fastRetryCtx zeroes TransientBackoff so the 8-attempt loop completes in ms.
	ctx := fastRetryCtx(context.Background())
	_, err := h.Handle(ctx, args, jsonrpc.Context{RequestID: "req-cluster-503"})
	if err == nil {
		t.Fatal("expected error from transient cluster ListResources failure")
	}
	assertHandlerRetriable(t, err, true)
}

// ---------------------------------------------------------------------------
// create_stemcell → storage policy → ListStorage transient/permanent boundary
// ---------------------------------------------------------------------------

// buildStoragePolicyBoundaryDeps constructs Deps for storage-policy boundary tests.
// clusterStorageSvc drives handlerPolicyDeps.StorageInfo (via ClusterStorage().ListStorage).
// The cluster is single-node so storage policy does not reject local storage.
func buildStoragePolicyBoundaryDeps(t *testing.T, clusterStorageSvc *stemcellMockClusterStorage) handlers.Deps {
	t.Helper()

	// Single-node cluster so ListConfigNodes returns 1 node — local storage is acceptable.
	cluster := &stemcellMockCluster{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			raw, _ := json.Marshal(map[string]string{"node": vmNode})
			resp := sdkcluster.ListConfigNodesResponse{raw}
			return &resp, nil
		},
	}

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        cluster,
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: clusterStorageSvc,
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

// lightStoragePolicyCPArgs builds a minimal cloud_properties argument list for
// the light-preuploaded stemcell path. The image_id must be a valid volid of
// the form "<storage>:import/<file>" so the parser accepts it. The storage name
// must match the StemcellStorage config so the policy lookup fires against the
// right entry in the ClusterStorage list. name + version are required for
// deterministic CID construction.
func lightStoragePolicyCPArgs(t *testing.T, imgPath string) []json.RawMessage {
	t.Helper()
	// image_id must match StemcellStorage ("local") so the handler queries "local"
	// from ClusterStorage().ListStorage — the call this test stubs to inject errors.
	cp := map[string]any{
		"name":     "ubuntu-jammy",
		"version":  "1.234",
		"image_id": "local:import/bosh-stemcell-ubuntu-jammy-1.234-abcdef12.qcow2",
		// sha256 is required for preuploaded stemcells; its value is
		// irrelevant to these boundary tests, which exercise storage-policy and
		// cluster-node-count API failures upstream of any content verification.
		"sha256": "ef0c5d8d1d8ba6e1a8620b2cba931c76e3bc9049395c3e7a5d5733cc3df2983f",
	}
	return []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}
}

// TestCreateStemcell_StoragePolicy_ListStorage_503_IsRetriable verifies that a
// transient 503 from ClusterStorage().ListStorage during storage-policy validation
// propagates as TypeRetriableCloud from the create_stemcell handler boundary.
// Before the fix, StorageInfo returned the raw SDK error and the policy function
// flattened it via cpierrors.Cloud(%s) → OkToRetry()==false.
func TestCreateStemcell_StoragePolicy_ListStorage_503_IsRetriable(t *testing.T) {
	t.Parallel()

	clusterStorage := &stemcellMockClusterStorage{
		listStorageFn: func(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
			return nil, makeSdkAPIErr(503, "service unavailable")
		},
	}
	deps := buildStoragePolicyBoundaryDeps(t, clusterStorage)

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-storagepolicy-503"})

	assertHandlerRetriable(t, err, true)

	var e *cpierrors.Error
	if errors.As(err, &e) && e.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("error type = %q; want TypeRetriableCloud", e.Type())
	}
}

// TestCreateStemcell_StoragePolicy_ListStorage_ConnectionErr_IsRetriable verifies
// that a connection-refused error from ListStorage during policy validation
// propagates as TypeRetriableCloud from the handler.
func TestCreateStemcell_StoragePolicy_ListStorage_ConnectionErr_IsRetriable(t *testing.T) {
	t.Parallel()

	clusterStorage := &stemcellMockClusterStorage{
		listStorageFn: func(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
			return nil, &sdkerrors.ConnectionError{
				Host:    "pve.test.local",
				Port:    8006,
				Message: "connection refused",
			}
		},
	}
	deps := buildStoragePolicyBoundaryDeps(t, clusterStorage)

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-storagepolicy-conn"})

	assertHandlerRetriable(t, err, true)
}

// TestCreateStemcell_StoragePolicy_ListStorage_400_IsNotRetriable verifies that
// a permanent 400 from ListStorage during policy validation stays non-retriable.
func TestCreateStemcell_StoragePolicy_ListStorage_400_IsNotRetriable(t *testing.T) {
	t.Parallel()

	clusterStorage := &stemcellMockClusterStorage{
		listStorageFn: func(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
			return nil, makeSdkAPIErr(400, "bad request")
		},
	}
	deps := buildStoragePolicyBoundaryDeps(t, clusterStorage)

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-storagepolicy-400"})

	assertHandlerRetriable(t, err, false)
}

// TestCreateStemcell_ClusterListResources_400_IsNotRetriable verifies that a
// permanent 400 from ListResources stays non-retriable.
func TestCreateStemcell_ClusterListResources_400_IsNotRetriable(t *testing.T) {
	t.Parallel()

	permanentErr := makeSdkAPIErr(400, "bad request")

	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, permanentErr
		},
	}

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        clusterSvc,
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: lightStemcellClusterStorage("local", "nfs", true),
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

	imgPath := tempImageFile(t)
	cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
	args := []json.RawMessage{marshalArg(t, imgPath), marshalArg(t, cp)}

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-cluster-400"})
	if err == nil {
		t.Fatal("expected error from permanent cluster ListResources failure")
	}
	assertHandlerRetriable(t, err, false)
}

// ---------------------------------------------------------------------------
// create_stemcell → storage policy → ListConfigNodes transient/permanent boundary
// ---------------------------------------------------------------------------
// buildListConfigNodesBoundaryDeps constructs Deps where ListStorage succeeds
// (shared NFS storage) but ListConfigNodes returns configuredErr. This isolates
// the ClusterNodeCount path through the storage policy.
func buildListConfigNodesBoundaryDeps(t *testing.T, listConfigNodesErr error) handlers.Deps {
	t.Helper()

	clusterSvc := &stemcellMockCluster{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			return nil, listConfigNodesErr
		},
	}

	// Shared storage so the block-storage guard passes; the cluster-size check
	// runs next and is the one we want to exercise.
	clusterStorage := lightStemcellClusterStorage("local", "nfs", true)

	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        clusterSvc,
		storageSvc:        &stemcellMockStorage{},
		clusterStorageSvc: clusterStorage,
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

// TestCreateStemcell_ListConfigNodes_503_IsRetriable verifies that a transient
// 503 from ListConfigNodes (cluster node count) propagates as TypeRetriableCloud
// from the create_stemcell handler. Before the fix, clusterNodeCount wrapped the
// raw SDK error with cpierrors.Wrap which defaults to non-retriable; the policy
// then re-flattened it with cpierrors.Cloud(%s) — two layers of retriability loss.
func TestCreateStemcell_ListConfigNodes_503_IsRetriable(t *testing.T) {
	t.Parallel()

	deps := buildListConfigNodesBoundaryDeps(t, makeSdkAPIErr(503, "service unavailable"))

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-listnodes-503"})

	assertHandlerRetriable(t, err, true)

	var e *cpierrors.Error
	if errors.As(err, &e) && e.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("error type = %q; want TypeRetriableCloud", e.Type())
	}
}

// TestCreateStemcell_ListConfigNodes_ConnectionErr_IsRetriable verifies that a
// connection-refused error from ListConfigNodes propagates as TypeRetriableCloud.
func TestCreateStemcell_ListConfigNodes_ConnectionErr_IsRetriable(t *testing.T) {
	t.Parallel()

	deps := buildListConfigNodesBoundaryDeps(t, &sdkerrors.ConnectionError{
		Host:    "pve.test.local",
		Port:    8006,
		Message: "connection refused",
	})

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-listnodes-conn"})

	assertHandlerRetriable(t, err, true)
}

// TestCreateStemcell_ListConfigNodes_400_IsNotRetriable verifies that a permanent
// 400 from ListConfigNodes stays non-retriable through the handler.
func TestCreateStemcell_ListConfigNodes_400_IsNotRetriable(t *testing.T) {
	t.Parallel()

	deps := buildListConfigNodesBoundaryDeps(t, makeSdkAPIErr(400, "bad request"))

	imgPath := tempImageFile(t)
	args := lightStoragePolicyCPArgs(t, imgPath)

	h := handlers.HandleCreateStemcell(deps)
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{RequestID: "req-listnodes-400"})

	assertHandlerRetriable(t, err, false)
}
