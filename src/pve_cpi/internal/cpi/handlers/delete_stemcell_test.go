package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ============================================================
// Tests: HandleDeleteStemcell (path-identity CID / director-UUID refs)
//
// Fixtures reuse the shared stemcellMock* mock infrastructure defined in
// create_stemcell_test.go (same handlers_test package).
// ============================================================

const testStemcellSHA8 = "ab12cd34"

// testHeavyStemcellFilename returns the canonical qcow2 filename produced by
// pve.BuildStemcellFilename for a stemcell whose content sha8 is testStemcellSHA8.
func testStemcellFilename() string {
	return "bosh-stemcell-ubuntu-jammy-1.0-" + testStemcellSHA8 + ".qcow2"
}

func testHeavyCID() string {
	return pve.BuildHeavyStemcellCID("local", testStemcellFilename())
}

func testLightCID() string {
	return pve.BuildLightStemcellCID("local", testStemcellFilename())
}

func testVolumePath() string {
	return "import/" + testStemcellFilename()
}

// deleteStemcellVolumeCall records one observed DeleteVolumeIfExists
// invocation, in call order, so tests can assert both content and ordering
// (e.g. primary-then-replica).
type deleteStemcellVolumeCall struct {
	node, storage, volume string
}

// deleteStemcellMockStorage implements sdkstorage.Service for delete_stemcell
// tests. Only DeleteVolumeIfExists is wired; all other methods panic on
// accidental call.
type deleteStemcellMockStorage struct {
	sdkstorage.Service        // nil embed — panics on unhandled calls
	deleteVolumeIfExistsFn    func(ctx context.Context, node, storage, volume string) (bool, error)
	deleteVolumeIfExistsCalls int
	calls                     []deleteStemcellVolumeCall
}

func (m *deleteStemcellMockStorage) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	m.deleteVolumeIfExistsCalls++
	m.calls = append(m.calls, deleteStemcellVolumeCall{node: node, storage: storage, volume: volume})
	if m.deleteVolumeIfExistsFn != nil {
		return m.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	// Default: volume exists and delete succeeds.
	return true, nil
}

// Upload is overridden to prevent nil-embed panic in test clients that wire
// this storage mock into a stemcellMockClient (which wires storageSvc).
func (m *deleteStemcellMockStorage) Upload(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
	panic("deleteStemcellMockStorage.Upload: not expected in delete_stemcell tests")
}

// deleteStemcellDepsOpts collects the pieces buildDeleteStemcellDeps needs;
// every field defaults to a harmless no-op mock/value when left zero.
type deleteStemcellDepsOpts struct {
	cfg        *config.CPIConfig
	qemuSvc    *stemcellMockQEMU
	nodesSvc   *stemcellMockNodes
	clusterSvc *stemcellMockCluster
	storageSvc *deleteStemcellMockStorage
	logger     *log.Logger
}

func buildDeleteStemcellDeps(o deleteStemcellDepsOpts) handlers.Deps {
	if o.cfg == nil {
		o.cfg = &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	}
	if o.qemuSvc == nil {
		o.qemuSvc = &stemcellMockQEMU{}
	}
	if o.nodesSvc == nil {
		o.nodesSvc = &stemcellMockNodes{}
	}
	if o.clusterSvc == nil {
		o.clusterSvc = &stemcellMockCluster{}
	}
	if o.storageSvc == nil {
		o.storageSvc = &deleteStemcellMockStorage{}
	}
	if o.logger == nil {
		o.logger = log.NewNopLogger()
	}
	client := &stemcellMockClient{
		qemuSvc:    o.qemuSvc,
		nodesSvc:   o.nodesSvc,
		tasksSvc:   &stemcellMockTasks{},
		clusterSvc: o.clusterSvc,
		storageSvc: o.storageSvc,
	}
	return handlers.Deps{Config: o.cfg, PVE: client, Logger: o.logger}
}

// directorRefsDescJSON builds a template description JSON string carrying the
// given sha8 and director_refs set, matching stemcellProvenance's shape.
func directorRefsDescJSON(sha8 string, refs ...string) string {
	if refs == nil {
		refs = []string{}
	}
	b, _ := json.Marshal(refs)
	return fmt.Sprintf(
		`{"name":"test","version":"1.0","sha8":%q,"created":"2026-08-03T00:00:00Z","kind":"heavy","director_refs":%s}`,
		sha8, string(b),
	)
}

// directorRefsDescMap wraps directorRefsDescJSON in the map[string]any shape
// QEMU().Config mocks return. Every call site in this file exercises the
// same fixture sha8 (testStemcellSHA8), so it is fixed here rather than
// threaded through as a parameter.
func directorRefsDescMap(refs ...string) map[string]any {
	return map[string]any{"description": directorRefsDescJSON(testStemcellSHA8, refs...)}
}

// clusterTemplateItem builds a raw JSON cluster resource entry for a stemcell
// template with the given vmid, node, and semicolon-separated tags.
func clusterTemplateItem(vmid int64, node, name, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type":     "qemu",
		"vmid":     vmid,
		"node":     node,
		"name":     name,
		"tags":     tags,
		"template": 1,
	})
	return raw
}

// cacheTemplateTags is the tag string of a cache template this CPI built: the
// generation marker plus the content sha tag. The marker is what makes a
// template eligible for the sha8-keyed lookup and sweep at all — a template
// carrying only the sha tag was built by a previous CPI generation and is
// deliberately invisible to both.
func cacheTemplateTags(sha8 string) string {
	return "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8
}

// ============================================================
// Tests: argument validation + CID grammar
// ============================================================

func TestDeleteStemcell_MissingArg(t *testing.T) {
	t.Parallel()

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{})
	h := handlers.HandleDeleteStemcell(deps)

	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when stemcell_cid is missing, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

func TestDeleteStemcell_NonStringArg(t *testing.T) {
	t.Parallel()

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{json.RawMessage(`5042`)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when stemcell_cid is a JSON number (not string), got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_LegacyAndMalformedCIDs_HardError verifies that every
// retired CID grammar — and every malformed path CID — is rejected as a hard,
// non-retriable cloud error with zero PVE API calls. Pre-release cutover: no
// backward compatibility, no legacy no-op arms.
func TestDeleteStemcell_LegacyAndMalformedCIDs_HardError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cid  string
	}{
		{"legacy template CID", "template:6042"},
		{"legacy integer CID", "5042"},
		{"legacy light CID (no leading colon)", "light:nfs:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"},
		{"legacy bare volume CID (no leading colon)", "nfs:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"},
		{"empty", ""},
		{"just colon", ":"},
		{"unknown kind", ":medium:local:import/x.qcow2"},
		{"doubled prefix", ":light::heavy:local:import/x.qcow2"},
		{"missing import prefix", ":heavy:local:volumes/x.qcow2"},
		{"no colon at all", "localbosh-stemcell-ubuntu.qcow2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			storageSvc := &deleteStemcellMockStorage{}
			var deleteQemuCalled bool
			nodesSvc := &stemcellMockNodes{
				deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
					deleteQemuCalled = true
					resp := sdknodes.DeleteQemuResponse(`""`)
					return &resp, nil
				},
			}
			deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{nodesSvc: nodesSvc, storageSvc: storageSvc})
			h := handlers.HandleDeleteStemcell(deps)

			args := []json.RawMessage{marshalArg(t, tc.cid)}
			_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
			if err == nil {
				t.Fatalf("expected error for CID %q, got nil", tc.cid)
			}
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) {
				t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
			}
			if storageSvc.deleteVolumeIfExistsCalls != 0 {
				t.Errorf("expected zero storage calls for rejected CID %q, got %d", tc.cid, storageSvc.deleteVolumeIfExistsCalls)
			}
			if deleteQemuCalled {
				t.Errorf("expected zero DeleteQemu calls for rejected CID %q", tc.cid)
			}
		})
	}
}

// ============================================================
// Tests: light stemcells — file NEVER deleted
// ============================================================

func TestDeleteStemcell_Light_LastRef_TemplateDestroyed_FileNeverDeleted(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(6042)
	var destroyedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyedVMIDs = append(destroyedVMIDs, vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testLightCID())}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if len(destroyedVMIDs) != 1 || destroyedVMIDs[0] != fmt.Sprintf("%d", primaryVMID) {
		t.Errorf("expected primary template %d destroyed, got %v", primaryVMID, destroyedVMIDs)
	}
	if storageSvc.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("light stemcell: DeleteVolumeIfExists must NEVER be called, got %d calls", storageSvc.deleteVolumeIfExistsCalls)
	}
}

func TestDeleteStemcell_Light_NoTemplates_NoOp(t *testing.T) {
	t.Parallel()

	storageSvc := &deleteStemcellMockStorage{}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testLightCID())}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if storageSvc.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("light stemcell no-template path must never touch storage, got %d calls", storageSvc.deleteVolumeIfExistsCalls)
	}
}

// TestDeleteStemcell_Light_NoTemplates_NoNodeConfigured_StillSucceeds verifies
// that a light no-op does not require config.node — there is no PVE call to
// make.
func TestDeleteStemcell_Light_NoTemplates_NoNodeConfigured_StillSucceeds(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{Node: "", StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testLightCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ============================================================
// Tests: heavy stemcells — destroy-then-delete ordering, replicas
// ============================================================

func TestDeleteStemcell_Heavy_LastRef_DestroysTemplateThenDeletesFile_OrderAsserted(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7001)
	const replicaVMID = int64(7002)

	var events []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			events = append(events, fmt.Sprintf("destroy:%s:%s", node, vmid))
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
				clusterTemplateItem(replicaVMID, "pve2", "stemcell-cache-replica", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, node, _, _ string) (bool, error) {
			events = append(events, "qcow2:"+node)
			return true, nil
		},
	}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		fmt.Sprintf("destroy:%s:%d", vmNode, primaryVMID),
		fmt.Sprintf("destroy:%s:%d", "pve2", replicaVMID),
		"qcow2:" + vmNode,
		"qcow2:pve2",
	}
	if len(events) != len(want) {
		t.Fatalf("event order = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q (full sequence: %v)", i, events[i], want[i], events)
		}
	}
	if storageSvc.deleteVolumeIfExistsCalls != 2 {
		t.Errorf("expected 2 qcow2 deletes (primary + 1 replica), got %d", storageSvc.deleteVolumeIfExistsCalls)
	}
	if storageSvc.calls[0].volume != testVolumePath() || storageSvc.calls[1].volume != testVolumePath() {
		t.Errorf("expected both qcow2 deletes to target %q, got %+v", testVolumePath(), storageSvc.calls)
	}
}

func TestDeleteStemcell_Heavy_ReplicaDestroyFailure_BestEffort_OverallSucceeds(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7101)
	const replicaVMID = int64(7102)

	var destroyCount int
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyCount++
			if vmid == fmt.Sprintf("%d", replicaVMID) {
				return nil, errors.New("PVE: transient error on replica")
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
				clusterTemplateItem(replicaVMID, "pve2", "stemcell-cache-replica", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed even when a replica template destroy fails; got: %v", err)
	}
	if destroyCount != 2 {
		t.Errorf("expected 2 DeleteQemu attempts (primary + replica), got %d", destroyCount)
	}
	// The template destroy overall still counts as "destroyed" (primary
	// succeeded); qcow2 cleanup on both nodes is still attempted.
	if storageSvc.deleteVolumeIfExistsCalls != 2 {
		t.Errorf("expected qcow2 delete attempted on both nodes despite replica destroy failure, got %d calls", storageSvc.deleteVolumeIfExistsCalls)
	}
}

// ============================================================
// Tests: anchor selection — replicas must never hijack the ref-anchor
// ============================================================

// TestDeleteStemcell_ReplicaLowerVMID_DoesNotHijackAnchor verifies that a
// per-node replica carrying a LOWER VMID than the primary never becomes the
// deregister anchor — anchor selection must skip every replica-tagged match
// and pick the lowest-VMID NON-replica match instead, regardless of the
// ascending-VMID order FindTemplatesBySHATagCluster returns.
func TestDeleteStemcell_ReplicaLowerVMID_DoesNotHijackAnchor(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(30500) // anchor: NOT tagged as a replica.
	const replicaVMID = int64(30100) // lower VMID, but IS tagged as a replica.
	const replicaNode = "pve2"

	var events []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			events = append(events, fmt.Sprintf("destroy:%s:%s", node, vmid))
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
			events = append(events, "config-read:"+node)
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			// Deliberately listed replica-first; FindTemplatesBySHATagCluster
			// sorts ascending by VMID, so the replica (lower VMID) sorts
			// first in refs regardless of this order — anchor selection must
			// still skip it.
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(replicaVMID, replicaNode, "stemcell-cache-replica",
					cacheTemplateTags(testStemcellSHA8)+";"+pve.ReplicaNodeTagForNode(replicaNode)),
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The deregister read-modify-write's config read must target the
	// primary — the anchor — never the lower-VMID replica.
	if len(events) == 0 || events[0] != "config-read:"+vmNode {
		t.Fatalf("anchor config read must target the primary node %q first, got events=%v", vmNode, events)
	}
	wantDestroyOrder := []string{
		"destroy:" + vmNode + ":" + fmt.Sprintf("%d", primaryVMID),
		"destroy:" + replicaNode + ":" + fmt.Sprintf("%d", replicaVMID),
	}
	var gotDestroys []string
	for _, e := range events {
		if strings.HasPrefix(e, "destroy:") {
			gotDestroys = append(gotDestroys, e)
		}
	}
	if len(gotDestroys) != len(wantDestroyOrder) {
		t.Fatalf("destroy events = %v, want %v", gotDestroys, wantDestroyOrder)
	}
	for i := range wantDestroyOrder {
		if gotDestroys[i] != wantDestroyOrder[i] {
			t.Errorf("destroy[%d] = %q, want %q (full events: %v)", i, gotDestroys[i], wantDestroyOrder[i], events)
		}
	}
}

// TestDeleteStemcell_AllReplicas_FallsThroughToNoTemplateSemantics verifies
// The other anchor edge: when every cluster-scoped match for this content sha8 is
// a per-node replica, there is no anchor to deregister against — the call
// must fall through to the no-cache-template semantics (idempotent qcow2
// delete convergence) rather than deregistering against, and possibly
// destroying, a replica whose ref set is a fossil of its own creation.
func TestDeleteStemcell_AllReplicas_FallsThroughToNoTemplateSemantics(t *testing.T) {
	t.Parallel()

	const replicaVMID = int64(30200)
	const replicaNode = "pve3"

	var deleteQemuCalled bool
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deleteQemuCalled = true
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(replicaVMID, replicaNode, "stemcell-cache-replica",
					cacheTemplateTags(testStemcellSHA8)+";"+pve.ReplicaNodeTagForNode(replicaNode)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if deleteQemuCalled {
		t.Error("an all-replica match set must never be deregistered/destroyed directly — there is no anchor")
	}
	// No-template semantics: qcow2 delete still attempted for retry
	// convergence, on config.Node (there is no template to read a node from).
	if storageSvc.deleteVolumeIfExistsCalls == 0 {
		t.Error("expected the no-template heavy qcow2 delete to still run")
	}
	if len(storageSvc.calls) == 0 || storageSvc.calls[0].node != vmNode {
		t.Errorf("expected the first qcow2 delete on config.Node %q, got %+v", vmNode, storageSvc.calls)
	}
}

// ============================================================
// Tests: refs remain — nothing destroyed
// ============================================================

func TestDeleteStemcell_Heavy_NonLastRef_NothingDestroyed_RefsWrittenMinusCaller(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7201)

	var deleteQemuCalled bool
	var capturedDesc string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deleteQemuCalled = true
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
		updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a", "dir-b"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleteQemuCalled {
		t.Error("template must NOT be destroyed while another director's ref remains")
	}
	if storageSvc.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("qcow2 must NOT be touched while refs remain, got %d calls", storageSvc.deleteVolumeIfExistsCalls)
	}
	if capturedDesc == "" {
		t.Fatal("expected the ref set to be persisted (RMW write)")
	}
	if strings.Contains(capturedDesc, `"dir-a"`) {
		t.Errorf("caller's ref must be removed from the persisted set: %s", capturedDesc)
	}
	if !strings.Contains(capturedDesc, `"dir-b"`) {
		t.Errorf("remaining director's ref must survive: %s", capturedDesc)
	}
}

// TestDeleteStemcell_SecondDirectorDeleteAfterFirst_Destroys simulates two
// sequential deletes from different directors sharing one cache template: the
// first drops its ref (template survives), the second is the last ref and
// destroys.
func TestDeleteStemcell_SecondDirectorDeleteAfterFirst_Destroys(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7301)

	// --- First director's delete: refs=[dir-a,dir-b], caller dir-a. ---
	var firstDestroyCalled bool
	firstNodes := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			firstDestroyCalled = true
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	firstQemu := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a", "dir-b"), nil
		},
	}
	firstCluster := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	firstDeps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: firstQemu, nodesSvc: firstNodes, clusterSvc: firstCluster})
	h1 := handlers.HandleDeleteStemcell(firstDeps)
	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	if _, err := h1.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"}); err != nil {
		t.Fatalf("first delete: unexpected error: %v", err)
	}
	if firstDestroyCalled {
		t.Fatal("first delete must NOT destroy — dir-b's ref remains")
	}

	// --- Second director's delete: refs now [dir-b] (persisted state), caller dir-b. ---
	var secondDestroyCalled bool
	secondNodes := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			secondDestroyCalled = true
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	secondQemu := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-b"), nil
		},
	}
	secondCluster := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	secondDeps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: secondQemu, nodesSvc: secondNodes, clusterSvc: secondCluster})
	h2 := handlers.HandleDeleteStemcell(secondDeps)
	if _, err := h2.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-b"}); err != nil {
		t.Fatalf("second delete: unexpected error: %v", err)
	}
	if !secondDestroyCalled {
		t.Error("second delete must destroy — dir-b was the last ref")
	}
}

// ============================================================
// Tests: no cache template found — idempotent retry convergence
// ============================================================

func TestDeleteStemcell_Heavy_NoTemplates_FileDeleteStillAttempted(t *testing.T) {
	t.Parallel()

	storageSvc := &deleteStemcellMockStorage{}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storageSvc.deleteVolumeIfExistsCalls != 1 {
		t.Fatalf("expected 1 DeleteVolumeIfExists call, got %d", storageSvc.deleteVolumeIfExistsCalls)
	}
	got := storageSvc.calls[0]
	if got.node != vmNode || got.storage != "local" || got.volume != testVolumePath() {
		t.Errorf("qcow2 delete call = %+v, want node=%q storage=local volume=%q", got, vmNode, testVolumePath())
	}
}

func TestDeleteStemcell_Heavy_NoTemplates_VolumeAlreadyAbsent_Idempotent(t *testing.T) {
	t.Parallel()

	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, nil // already gone
		},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	result, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("expected nil error when volume already absent (idempotent), got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

func TestDeleteStemcell_Heavy_NoTemplates_QCow2DeleteError_Propagates(t *testing.T) {
	t.Parallel()

	storageErr := errors.New("PVE storage: permission denied")
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, storageErr
		},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error for qcow2 delete failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

func TestDeleteStemcell_Heavy_NoTemplates_NoNodeConfigured_Errors(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{Node: "", StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error when node is empty, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestDeleteStemcell_Heavy_NoTemplates_UsesStemcellTemplateNode verifies the
// node fallback order (StemcellTemplateNode over Node) when there is no
// template left to read a node from.
func TestDeleteStemcell_Heavy_NoTemplates_UsesStemcellTemplateNode(t *testing.T) {
	t.Parallel()

	storageSvc := &deleteStemcellMockStorage{}
	cfg := &config.CPIConfig{
		Node:                 vmNode,
		StemcellTemplateNode: "pve-template-node",
		StemcellStorage:      "local",
		VMStorage:            "local",
		DiskStorage:          "local",
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storageSvc.calls) != 1 || storageSvc.calls[0].node != "pve-template-node" {
		t.Errorf("expected qcow2 delete on StemcellTemplateNode, got %+v", storageSvc.calls)
	}
}

// TestDeleteStemcell_Heavy_NoTemplates_ReplicatedStorage_DeletesEveryNode
// verifies that with no cache template left to read a replica node list
// from, the no-template branch still best-effort deletes the qcow2 on every
// OTHER cluster node when storage is not positively known to be shared (the
// default here — no ClusterStorage service wired, so classification is
// unknown, which the branch treats as "not positively shared").
func TestDeleteStemcell_Heavy_NoTemplates_ReplicatedStorage_DeletesEveryNode(t *testing.T) {
	t.Parallel()

	var deletedNodes []string
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, node, _, _ string) (bool, error) {
			deletedNodes = append(deletedNodes, node)
			return true, nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listConfigNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			n1, _ := json.Marshal(map[string]string{"name": vmNode})
			n2, _ := json.Marshal(map[string]string{"name": "pve2"})
			n3, _ := json.Marshal(map[string]string{"name": "pve3"})
			resp := sdkcluster.ListConfigNodesResponse{n1, n2, n3}
			return &resp, nil
		},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]bool{vmNode: true, "pve2": true, "pve3": true}
	if len(deletedNodes) != len(want) {
		t.Fatalf("expected qcow2 delete attempted on all %d cluster nodes, got %v", len(want), deletedNodes)
	}
	for _, n := range deletedNodes {
		if !want[n] {
			t.Errorf("unexpected node in delete attempts: %q (all attempts: %v)", n, deletedNodes)
		}
	}
}

// TestDeleteStemcell_Heavy_NoTemplates_SharedStorage_SkipsOtherNodes verifies
// The positive-shared exemption: when storage IS positively known to be
// shared, the primary delete already removed the cluster's only copy, so no
// per-other-node cleanup should be attempted.
func TestDeleteStemcell_Heavy_NoTemplates_SharedStorage_SkipsOtherNodes(t *testing.T) {
	t.Parallel()

	var deletedNodes []string
	storageSvc := &deleteStemcellMockStorage{
		deleteVolumeIfExistsFn: func(_ context.Context, node, _, _ string) (bool, error) {
			deletedNodes = append(deletedNodes, node)
			return true, nil
		},
	}
	clusterStorageSvc := lightStemcellClusterStorage("local", "nfs", true) // shared=1
	client := &stemcellMockClient{
		qemuSvc:           &stemcellMockQEMU{},
		nodesSvc:          &stemcellMockNodes{},
		tasksSvc:          &stemcellMockTasks{},
		clusterSvc:        &stemcellMockCluster{},
		storageSvc:        storageSvc,
		clusterStorageSvc: clusterStorageSvc,
	}
	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := handlers.Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deletedNodes) != 1 || deletedNodes[0] != vmNode {
		t.Errorf("expected exactly 1 qcow2 delete (primary, positively-shared storage), got %v", deletedNodes)
	}
}

// TestDeleteStemcell_StorageExtractedFromCID verifies the storage pool comes
// from the CID, not config.StemcellStorage.
func TestDeleteStemcell_StorageExtractedFromCID(t *testing.T) {
	t.Parallel()

	cid := pve.BuildHeavyStemcellCID("nfs-pool", testStemcellFilename())

	storageSvc := &deleteStemcellMockStorage{}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{storageSvc: storageSvc}) // config.StemcellStorage = "local"
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, cid)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(storageSvc.calls) != 1 || storageSvc.calls[0].storage != "nfs-pool" {
		t.Errorf("storage passed to DeleteVolumeIfExists = %+v; want storage=nfs-pool", storageSvc.calls)
	}
}

// ============================================================
// Tests: base-volume-in-use → actionable, non-retriable error
// ============================================================

func TestDeleteStemcell_IsBaseVolumeInUse_ActionableMessage(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7401)

	var updateCalls []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "volume 'local:base-7401-disk-0' is still in use by 'linked-clone-9999'"}
		},
		updateConfigFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Tags != nil {
				updateCalls = append(updateCalls, *params.Tags)
			}
			return nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err == nil {
		t.Fatal("expected error when the template's base volume is still in use")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("base-volume-in-use error must be non-retriable (operator action required)")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf("%d", primaryVMID)) {
		t.Errorf("error message must name the template VMID: %s", msg)
	}
	if !strings.Contains(msg, vmNode) {
		t.Errorf("error message must name the node: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "linked-clone") && !strings.Contains(strings.ToLower(msg), "linked clone") {
		t.Errorf("error message must instruct deleting/migrating the dependent VM(s): %s", msg)
	}
	// A pending-destroy marker must have been stamped so a retry resumes.
	foundPending := false
	for _, tags := range updateCalls {
		if strings.Contains(tags, "bosh-destroy-pending") {
			foundPending = true
		}
	}
	if !foundPending {
		t.Errorf("expected a bosh-destroy-pending tag stamp, got tag writes: %v", updateCalls)
	}
}

// ============================================================
// Tests: empty director UUID collapses to the shared sentinel
// ============================================================

func TestDeleteStemcell_EmptyDirectorUUID_UnknownDirectorFlow(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7501)

	var buf bytes.Buffer
	logger, logErr := log.NewLogger("warn", &buf)
	if logErr != nil {
		t.Fatalf("log.NewLogger: %v", logErr)
	}

	var destroyed bool
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyed = true
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("unknown-director"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, logger: logger})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroy — empty UUID collapses to the shared unknown-director ref, matching the only ref present")
	}
	if !strings.Contains(buf.String(), "director UUID") {
		t.Errorf("expected a Warn log about the missing director UUID, got: %s", buf.String())
	}
}

// ============================================================
// Tests: sha8 unextractable from the CID filename
// ============================================================

func TestDeleteStemcell_SHA8Unextractable_WarnsAndSkipsClusterLookup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, logErr := log.NewLogger("warn", &buf)
	if logErr != nil {
		t.Fatalf("log.NewLogger: %v", logErr)
	}

	var listResourcesCalled bool
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listResourcesCalled = true
			return &sdkcluster.ListResourcesResponse{}, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{clusterSvc: clusterSvc, storageSvc: storageSvc, logger: logger})
	h := handlers.HandleDeleteStemcell(deps)

	// "badname.qcow2" has no "-<8hex>" tail — extractSHA8FromFilename fails.
	cid := ":heavy:local:import/badname.qcow2"
	args := []json.RawMessage{marshalArg(t, cid)}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listResourcesCalled {
		t.Error("cluster template lookup must be skipped when sha8 is unextractable")
	}
	if !strings.Contains(buf.String(), "sha8 unextractable") {
		t.Errorf("expected a Warn log naming sha8 unextractable, got: %s", buf.String())
	}
	// Falls through to the no-template heavy path: qcow2 delete still attempted.
	if storageSvc.deleteVolumeIfExistsCalls != 1 {
		t.Errorf("expected the no-template heavy qcow2 delete to still run, got %d calls", storageSvc.deleteVolumeIfExistsCalls)
	}
}

// ============================================================
// Tests: orphan prune (opt-in, director-scoped from the REQUEST context)
// ============================================================

func TestDeleteStemcell_OrphanPrune_Disabled_NoSecondListResourcesCall(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7601)

	var listResourcesCalls int
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listResourcesCalls++
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	// Prune off: nil Stemcell block = default.
	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listResourcesCalls != 1 {
		t.Errorf("expected exactly 1 ListResources call (template lookup only, no orphan scan), got %d", listResourcesCalls)
	}
}

func TestDeleteStemcell_OrphanPrune_EmptyDirectorUUID_Skipped(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7602)

	var listResourcesCalls int
	var buf bytes.Buffer
	logger, logErr := log.NewLogger("warn", &buf)
	if logErr != nil {
		t.Fatalf("log.NewLogger: %v", logErr)
	}

	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("unknown-director"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listResourcesCalls++
			items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
			return &items, nil
		},
	}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, logger: logger})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if listResourcesCalls != 1 {
		t.Errorf("expected exactly 1 ListResources call (prune must be skipped before any orphan scan), got %d", listResourcesCalls)
	}
	if !strings.Contains(buf.String(), "carried no director UUID") {
		t.Errorf("expected a Warn log about orphan prune skipping due to missing director UUID, got: %s", buf.String())
	}
}

func TestDeleteStemcell_OrphanPrune_DryRun_NoDeletion(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7701)
	const orphanVMID = int64(7702)
	const dirUUID = "dir-prod"

	var deletedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap(dirUUID), nil
		},
	}
	dirTag := "director--" + dirUUID
	var listCall int
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCall++
			if listCall == 1 {
				// Template-cache lookup (sha-scoped).
				items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
				return &items, nil
			}
			// Orphan sweep (marker+director-scoped).
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(orphanVMID, "pve2", "orphan", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr, PruneDryRun: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: dirUUID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range deletedVMIDs {
		if id == fmt.Sprintf("%d", orphanVMID) {
			t.Errorf("dry-run must not delete the orphan candidate %d", orphanVMID)
		}
	}
}

// TestDeleteStemcell_OrphanPrune_CandidateWithForeignRef_NotPruned verifies
// The marker+director tag pair alone is not sufficient to prune — a
// candidate whose OWN provenance director_refs still names a director OTHER
// than (or in addition to) this request's director UUID must be skipped,
// because nothing ever removes the "director--<uuid>" tag on deregistration
// and the candidate may still be a live cache for a DIFFERENT stemcell this
// director (or another) actively references.
func TestDeleteStemcell_OrphanPrune_CandidateWithForeignRef_NotPruned(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7901)
	const foreignRefVMID = int64(7902)
	const dirUUID = "dir-scope"

	var deletedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == int(foreignRefVMID) {
				// Live-referenced by dirUUID AND a different director — must
				// not be pruned out from under that other director.
				return directorRefsDescMap(dirUUID, "some-other-director"), nil
			}
			return directorRefsDescMap(dirUUID), nil
		},
	}
	dirTag := "director--" + dirUUID
	var listCall int
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCall++
			if listCall == 1 {
				items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
				return &items, nil
			}
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(foreignRefVMID, "pve2", "still-live", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: dirUUID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range deletedVMIDs {
		if id == fmt.Sprintf("%d", foreignRefVMID) {
			t.Errorf("candidate carrying a live foreign director ref must NOT be pruned, destroys=%v", deletedVMIDs)
		}
	}
}

// TestDeleteStemcell_OrphanPrune_ExcludesJustHandledTemplate verifies the
// excludeVMIDs guard: the orphan sweep must never re-evaluate (and
// re-destroy) a VMID this SAME delete_stemcell call already handled above,
// even if a stale cluster-resource snapshot re-lists it.
func TestDeleteStemcell_OrphanPrune_ExcludesJustHandledTemplate(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7910)
	const dirUUID = "dir-exclude"

	var destroyCount int
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyCount++
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap(dirUUID), nil
		},
	}
	dirTag := "director--" + dirUUID
	var listCall int
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCall++
			if listCall == 1 {
				// Template-cache lookup: sha-tag match only is enough here.
				items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
				return &items, nil
			}
			// Orphan sweep re-lists the SAME vmid this call already
			// destroyed above (stale cluster-resource snapshot), now also
			// carrying the marker+director tags the sweep's filter needs —
			// excludeVMIDs must keep it from being evaluated a second time.
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", "bosh-stemcell;"+dirTag+";"+cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		},
	}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: dirUUID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if destroyCount != 1 {
		t.Errorf("expected exactly 1 destroy (the anchor, via deregister); excludeVMIDs must keep the prune "+
			"sweep from re-destroying the same VMID, got %d", destroyCount)
	}
}

// TestDeleteStemcell_OrphanPrune_NoTemplateBranch_StillRuns verifies that
// the opt-in orphan prune must also run at the end of the no-cache-template
// branch (handleDeleteStemcellNoTemplate), not only after a
// templates-found-and-destroyed call.
func TestDeleteStemcell_OrphanPrune_NoTemplateBranch_StillRuns(t *testing.T) {
	t.Parallel()

	const orphanVMID = int64(7920)
	const dirUUID = "dir-notmpl"

	var deletedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap(dirUUID), nil
		},
	}
	dirTag := "director--" + dirUUID
	var listCall int
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCall++
			if listCall == 1 {
				// Template-cache lookup for THIS stemcell's sha8: zero
				// matches (already destroyed / never existed) — the
				// no-template branch.
				return &sdkcluster.ListResourcesResponse{}, nil
			}
			// Orphan sweep for a DIFFERENT stemcell's abandoned template.
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(orphanVMID, "pve2", "orphan", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: dirUUID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, id := range deletedVMIDs {
		if id == fmt.Sprintf("%d", orphanVMID) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected orphan prune to run on the no-template branch and destroy orphan VMID %d, got destroys=%v", orphanVMID, deletedVMIDs)
	}
}

func TestDeleteStemcell_OrphanPrune_Live_DestroysOrphan_SkipsBaseInUse(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(7801)
	const orphanVMID = int64(7802)
	const baseInUseVMID = int64(7803)
	const dirUUID = "dir-prod2"

	var deletedVMIDs []string
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmid)
			if vmid == fmt.Sprintf("%d", baseInUseVMID) {
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "base volume still in use"}
			}
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap(dirUUID), nil
		},
	}
	dirTag := "director--" + dirUUID
	var listCall int
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			listCall++
			if listCall == 1 {
				items := sdkcluster.ListResourcesResponse{clusterTemplateItem(primaryVMID, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8))}
				return &items, nil
			}
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(orphanVMID, "pve2", "orphan", "bosh-stemcell;"+dirTag),
				clusterTemplateItem(baseInUseVMID, "pve3", "base-in-use", "bosh-stemcell;"+dirTag),
			}
			return &items, nil
		},
	}
	tr := true
	cfg := &config.CPIConfig{
		Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local",
		Stemcell: &config.StemcellProvenanceConfig{PruneOrphans: &tr},
	}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{cfg: cfg, qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: dirUUID})
	if err != nil {
		t.Fatalf("delete_stemcell must succeed even with a base-in-use orphan skip; got: %v", err)
	}
	foundOrphan := false
	for _, id := range deletedVMIDs {
		if id == fmt.Sprintf("%d", orphanVMID) {
			foundOrphan = true
		}
	}
	if !foundOrphan {
		t.Errorf("expected orphan VMID %d to be attempted, got %v", orphanVMID, deletedVMIDs)
	}
}

// TestDeleteStemcell_PreGenerationTemplate_NotSwept is the cross-generation
// safety guard on the destroy side. A template built by a PREVIOUS CPI
// generation carries the content sha tag but neither this generation's cache
// marker nor any director-- ref tag, so its provenance records no refs. If the
// sweep could see it, the very first delete_stemcell for that content would
// find a zero ref count and destroy a template a live older director still
// clones from. It must be invisible: nothing destroyed, and the call converges
// on the no-cache-template path.
func TestDeleteStemcell_PreGenerationTemplate_NotSwept(t *testing.T) {
	t.Parallel()

	const preGenVMID = int64(30169)
	var destroyedVMIDs []string
	var configReads []int
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, _, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyedVMIDs = append(destroyedVMIDs, vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	// A config read against the pre-generation template means the sweep
	// selected it as a ref anchor — the deregister step is the first thing to
	// touch it, and that is already too far.
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			configReads = append(configReads, vmid)
			return directorRefsDescMap(), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				// Previous-generation shape: bosh-cpi + sha tag only.
				clusterTemplateItem(preGenVMID, vmNode, "bosh-stemcell-ubuntu-noble-1-383",
					"bosh-cpi;bosh-stemcell-sha-"+testStemcellSHA8),
			}
			return &items, nil
		},
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{
		qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc,
	})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testLightCID())}
	if _, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(destroyedVMIDs) != 0 {
		t.Errorf("a previous-generation template must never be destroyed, got destroys of %v", destroyedVMIDs)
	}
	if len(configReads) != 0 {
		t.Errorf("a previous-generation template must never be selected as a ref anchor, got config reads of %v", configReads)
	}
}
