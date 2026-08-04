package handlers_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// oldCIDSHA8 is the sha8 embedded in testStemcellCIDWithSHA's filename — the
// key the strategy=template cluster-cache lookup matches on. That file is also
// present in vmMockNodes' default storage listing, so an import fallback from
// this CID succeeds instead of failing on a missing qcow2.
const oldCIDSHA8 = "abc12345"

// cloneUPIDResponse is a canned CreateQemuClone response carrying a UPID.
func cloneUPIDResponse() *sdknodes.CreateQemuCloneResponse {
	raw := sdknodes.CreateQemuCloneResponse{}
	_ = json.Unmarshal([]byte(`"UPID:pve:00005555:00000001:clone:ok"`), &raw)
	return &raw
}

// nodeListWithTemplate returns a listQemuFn reporting one frozen cache
// template carrying the generation marker and the sha tag — the shape
// GET /nodes/<node>/qemu returns, which is authoritative and not served from
// the lagging cluster index.
func nodeListWithTemplate(vmid int64, sha8 string) func(context.Context, string, *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	return func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		raw, _ := json.Marshal(map[string]any{
			"vmid":     vmid,
			"name":     "bosh-stemcell-ubuntu-jammy-1-438",
			"tags":     "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8,
			"template": 1,
		})
		resp := sdknodes.ListQemuResponse{raw}
		return &resp, nil
	}
}

// TestCreateVM_TemplateCacheMiss_RecheckFindsTemplate_Clones is the
// template-visibility race: create_stemcell freezes a template, the director
// issues create_vm immediately, and PVE's /cluster/resources index has not
// caught up yet. The first lookup misses; the bounded re-check must find the
// template and clone rather than silently degrading to a full-copy import.
func TestCreateVM_TemplateCacheMiss_RecheckFindsTemplate_Clones(t *testing.T) {
	defer handlers.SetTemplateCacheRecheckDelay(0)()

	var cacheLookups atomic.Int32
	cloneCalled := false

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			return cloneUPIDResponse(), nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	// The cluster index lags: empty on the first cache lookup, populated after.
	c := &vmMockCluster{
		listResourcesFn: func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			if params != nil && params.Type != nil {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			if cacheLookups.Add(1) == 1 {
				empty := sdkcluster.ListResourcesResponse{}
				return &empty, nil
			}
			return clusterCacheListResourcesFn(6042, "pve", oldCIDSHA8)(ctx, params)
		},
	}

	deps := buildVMDepsForOldCIDLookup(q, n, c, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-recheck-hit", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("recheck-hit")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cloneCalled {
		t.Error("a template that became visible on re-check must be cloned, not imported")
	}
	if len(q.createCalls) != 0 {
		t.Errorf("QEMU.Create (import path) must not be called, got %d calls", len(q.createCalls))
	}
	if got := cacheLookups.Load(); got < 2 {
		t.Errorf("cluster cache lookup ran %d time(s); the re-check must retry after a miss", got)
	}
}

// TestCreateVM_TemplateCacheMiss_NodeListingFindsTemplate_Clones covers the
// other half of the re-check: the cluster index never catches up within the
// budget, but the authoritative per-node listing (GET /nodes/<node>/qemu) sees
// the just-frozen template immediately. This is the single-node case, and it
// must resolve without waiting out any retry.
func TestCreateVM_TemplateCacheMiss_NodeListingFindsTemplate_Clones(t *testing.T) {
	defer handlers.SetTemplateCacheRecheckDelay(0)()

	cloneCalled := false
	var cloneSourceVMID string

	n := &vmMockNodes{
		listQemuFn: nodeListWithTemplate(6042, oldCIDSHA8),
		createQemuCloneFn: func(_ context.Context, _, vmid string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			cloneSourceVMID = vmid
			return cloneUPIDResponse(), nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	c := &vmMockCluster{listResourcesFn: emptyListResources}

	deps := buildVMDepsForOldCIDLookup(q, n, c, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-recheck-nodelist", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("recheck-nodelist")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cloneCalled {
		t.Fatal("the authoritative per-node listing found the template; create_vm must clone from it")
	}
	if cloneSourceVMID != "6042" {
		t.Errorf("clone source vmid = %q; want %q", cloneSourceVMID, "6042")
	}
	if len(q.createCalls) != 0 {
		t.Errorf("QEMU.Create (import path) must not be called, got %d calls", len(q.createCalls))
	}
}

// TestCreateVM_TemplateGenuinelyAbsent_FallsBackToImport verifies the re-check
// does not turn a genuine cache miss into a failure: after both mechanisms
// miss for the whole budget, create_vm still falls back to import-from.
func TestCreateVM_TemplateGenuinelyAbsent_FallsBackToImport(t *testing.T) {
	defer handlers.SetTemplateCacheRecheckDelay(0)()

	var cacheLookups atomic.Int32

	n := &vmMockNodes{} // nil createQemuCloneFn: any clone attempt panics
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	c := &vmMockCluster{
		listResourcesFn: func(_ context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			if params == nil || params.Type == nil {
				cacheLookups.Add(1)
			}
			empty := sdkcluster.ListResourcesResponse{}
			return &empty, nil
		},
	}

	deps := buildVMDepsForOldCIDLookup(q, n, c, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-recheck-absent", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("recheck-absent")); err != nil {
		t.Fatalf("genuine cache miss must still succeed via import: %v", err)
	}
	if len(q.createCalls) == 0 {
		t.Error("QEMU.Create (import path) was not called; the import fallback did not run")
	}
	if got := cacheLookups.Load(); got < 2 {
		t.Errorf("cluster cache lookup ran %d time(s); the re-check budget was not spent before conceding", got)
	}
}

// resourceShapeParams returns the post-clone UpdateQemuConfig call that
// carries the VM's resource shape — located by the non-nil Memory field only
// that call sets, matching the convention the balloon/tablet tests use.
func resourceShapeParams(t *testing.T, n *vmMockNodes) *sdknodes.UpdateQemuConfigParams {
	t.Helper()
	for _, c := range n.updateConfigCalls {
		if c.params.Memory != nil {
			return c.params
		}
	}
	t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (post-clone resource call not found)")
	return nil
}

// TestCreateVM_ClonePath_ReplacesInheritedStemcellIdentity is defect D-3: PVE
// copies tags and description from the clone source, so a workload VM cloned
// from a cache template came up advertising the template's identity —
// bosh-stemcell-cache, the content sha tag, the template's director-- ref tag,
// and the template's provenance JSON. Those are the exact keys the stemcell
// lookups and delete_stemcell's sweep match on. The post-clone config PUT must
// replace them with the workload VM's own tag set and clear the description.
func TestCreateVM_ClonePath_ReplacesInheritedStemcellIdentity(t *testing.T) {
	t.Parallel()

	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return cloneUPIDResponse(), nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-clone-identity", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("clone-identity")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	params := resourceShapeParams(t, n)
	if params.Tags == nil {
		t.Fatal("post-clone params.Tags is nil; the clone keeps the template's inherited stemcell tags")
	}
	if *params.Tags != "bosh-cpi" {
		t.Errorf("post-clone params.Tags = %q; want the workload tag set %q", *params.Tags, "bosh-cpi")
	}
	if params.Description == nil {
		t.Fatal("post-clone params.Description is nil; the clone keeps the template's provenance JSON")
	}
	if *params.Description != "" {
		t.Errorf("post-clone params.Description = %q; want empty (matching the import path)", *params.Description)
	}
}

// TestCreateVM_CloneAndImportPaths_AgreeOnVMIdentity pins the two create paths
// to the same VM identity: whatever tag set the import path writes at create
// time, the clone path must write in its post-clone config PUT. Without this
// the two paths drift and a VM's tags depend on which path happened to run.
func TestCreateVM_CloneAndImportPaths_AgreeOnVMIdentity(t *testing.T) {
	t.Parallel()

	// Clone path.
	cloneNodes := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return cloneUPIDResponse(), nil
		},
	}
	cloneDeps := buildVMDepsForTemplate(&vmMockQEMU{}, cloneNodes, &vmMockCluster{}, &vmMockAgent{})
	cloneArgs := mkArgs("agent-identity-clone", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	if _, err := handlers.HandleCreateVM(cloneDeps).Handle(context.Background(), cloneArgs, mkCtx("identity-clone")); err != nil {
		t.Fatalf("clone path: unexpected error: %v", err)
	}

	// Import path: no cache template anywhere, so create_vm imports.
	defer handlers.SetTemplateCacheRecheckDelay(0)()
	importQEMU := &vmMockQEMU{}
	importDeps := buildVMDepsForOldCIDLookup(importQEMU, &vmMockNodes{}, &vmMockCluster{listResourcesFn: emptyListResources}, &vmMockAgent{})
	importArgs := mkArgs("agent-identity-import", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	if _, err := handlers.HandleCreateVM(importDeps).Handle(context.Background(), importArgs, mkCtx("identity-import")); err != nil {
		t.Fatalf("import path: unexpected error: %v", err)
	}
	if len(importQEMU.createCalls) == 0 {
		t.Fatal("import path: QEMU.Create was not called")
	}

	importTags, _ := importQEMU.createCalls[0].params["tags"].(string)
	cloneParams := resourceShapeParams(t, cloneNodes)
	if cloneParams.Tags == nil || *cloneParams.Tags != importTags {
		got := "nil"
		if cloneParams.Tags != nil {
			got = *cloneParams.Tags
		}
		t.Errorf("clone-path tags = %q; want the import path's %q — both paths must produce one VM identity", got, importTags)
	}
	// The import path never writes a description, so the clone path must clear
	// the one it inherited rather than leaving the template's provenance JSON.
	if _, ok := importQEMU.createCalls[0].params["description"]; ok {
		t.Fatal("import path unexpectedly set a description; the clone-path expectation below needs updating")
	}
	if cloneParams.Description == nil || *cloneParams.Description != "" {
		t.Error("clone path must clear the inherited description to match the import path's empty one")
	}
}
