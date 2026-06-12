package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ============================================================
// Minimal test doubles for stemcell_refs unit tests
// ============================================================

// refsPoolSvc is a PoolService that always succeeds — allows lock acquisition.
type refsPoolSvc struct{}

func (p *refsPoolSvc) AddVM(_ context.Context, _ string, _ int64) error { return nil }
func (p *refsPoolSvc) CreatePool(_ context.Context, _, _ string) error  { return nil }
func (p *refsPoolSvc) DeletePool(_ context.Context, _ string) error     { return nil }
func (p *refsPoolSvc) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// refsQEMUSvc satisfies sdkqemu.Service for stemcell_refs tests.
// Only Config is overridden; all other methods panic via the nil embed.
type refsQEMUSvc struct {
	sdkqemu.Service // nil embed — panics on unneeded methods
	configFn        func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (q *refsQEMUSvc) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.configFn != nil {
		return q.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}

// refsNodesSvc satisfies sdknodes.Service for stemcell_refs tests.
// Only UpdateQemuConfig and DeleteQemu are overridden.
type refsNodesSvc struct {
	sdknodes.Service // nil embed
	updateFn         func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteCalled     bool
}

func (n *refsNodesSvc) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if n.updateFn != nil {
		return n.updateFn(ctx, node, vmid, params)
	}
	return nil
}

// DeleteQemu records the destroy call; a nil response means the destroy
// completed synchronously (no UPID to await).
func (n *refsNodesSvc) DeleteQemu(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	n.deleteCalled = true
	return nil, nil
}

// refsClient satisfies pve.Client using the test doubles above.
// Unneeded methods panic via the nil embed.
type refsClient struct {
	pve.Client // nil embed
	q          *refsQEMUSvc
	n          *refsNodesSvc
	p          *refsPoolSvc
}

func (c *refsClient) QEMU() sdkqemu.Service           { return c.q }
func (c *refsClient) Nodes() sdknodes.Service         { return c.n }
func (c *refsClient) Pools() pve.PoolService          { return c.p }
func (c *refsClient) Storage() sdkstorage.Service     { return nil }
func (c *refsClient) CloudInit() sdkcloudinit.Service  { return nil }
func (c *refsClient) Tasks() sdktasks.Service         { return nil }
func (c *refsClient) Cluster() sdkcluster.Service     { return nil }
func (c *refsClient) ClusterStorage() sdkclusterstorage.Service { return nil }

// buildRefsTestDeps constructs Deps for stemcell_refs unit tests.
func buildRefsTestDeps(
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error),
	updateFn func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error,
) Deps {
	return Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE: &refsClient{
			q: &refsQEMUSvc{configFn: configFn},
			n: &refsNodesSvc{updateFn: updateFn},
			p: &refsPoolSvc{},
		},
		Logger: log.NewNopLogger(),
	}
}

// ============================================================
// Tests: registerStemcellRef
// ============================================================

// TestRegisterStemcellRef_AppendsNewCID verifies that registerStemcellRef
// appends the stemcell CID when it is not already in refs.
func TestRegisterStemcellRef_AppendsNewCID(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	expectedCID := pve.BuildTemplateStemcellCID(templateVMID)

	// Start with a description that has no stemcell_refs.
	initialDesc := `{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z"}`

	var capturedDesc string
	var updateCalled bool

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": initialDesc}, nil
		},
		func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			if params != nil && params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	)

	registerStemcellRef(context.Background(), deps, deps.Logger, templateNode, templateVMID)

	if !updateCalled {
		t.Fatal("expected UpdateQemuConfig to be called; was not")
	}

	prov, ok := parseStemcellProvenanceFromDescription(capturedDesc)
	if !ok {
		t.Fatalf("written description is not valid JSON: %q", capturedDesc)
	}
	refs := ParseStemcellRefs(prov.StemcellRefs)
	found := false
	for _, r := range refs {
		if r == expectedCID {
			found = true
		}
	}
	if !found {
		t.Errorf("refs %v does not contain expected CID %q", refs, expectedCID)
	}
}

// TestRegisterStemcellRef_Idempotent_CIDAlreadyPresent verifies that
// registerStemcellRef does not add a duplicate when the CID is already in refs.
func TestRegisterStemcellRef_Idempotent_CIDAlreadyPresent(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	existingCID := pve.BuildTemplateStemcellCID(templateVMID)

	initialDesc := fmt.Sprintf(
		`{"name":"bosh-ubuntu-noble","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","stemcell_refs":%q}`,
		existingCID,
	)

	var capturedDesc string

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": initialDesc}, nil
		},
		func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	)

	registerStemcellRef(context.Background(), deps, deps.Logger, templateNode, templateVMID)

	// Whether or not UpdateQemuConfig was called, the resulting refs must not
	// contain duplicates. If no update was made, capturedDesc is empty — that is
	// acceptable as the idempotent skip path.
	if capturedDesc == "" {
		// No update made — idempotent skip is correct.
		return
	}

	prov, ok := parseStemcellProvenanceFromDescription(capturedDesc)
	if !ok {
		t.Fatalf("written description is not valid JSON: %q", capturedDesc)
	}
	refs := ParseStemcellRefs(prov.StemcellRefs)
	count := 0
	for _, r := range refs {
		if r == existingCID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of %q in refs %v, got %d", existingCID, refs, count)
	}
}

// ============================================================
// Tests: gatedDeregisterAndDestroyRef
// ============================================================

// TestGatedDeregisterStemcellRef_LastRef_ShouldDestroy verifies that when the
// deleted CID is the only ref, shouldDestroy=true is returned.
func TestGatedDeregisterStemcellRef_LastRef_ShouldDestroy(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	cid := pve.BuildTemplateStemcellCID(templateVMID)

	initialDesc := fmt.Sprintf(
		`{"name":"test","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","stemcell_refs":%q}`,
		cid,
	)

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": initialDesc}, nil
		},
		func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return nil
		},
	)

	shouldDestroy, err := gatedDeregisterAndDestroyRef(context.Background(), deps, deps.Logger, templateNode, templateVMID, cid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !shouldDestroy {
		t.Error("expected shouldDestroy=true when last ref is removed")
	}
}

// TestGatedDeregisterStemcellRef_RefsRemain_ShouldNotDestroy verifies that when
// other CIDs remain after removal, shouldDestroy=false.
func TestGatedDeregisterStemcellRef_RefsRemain_ShouldNotDestroy(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	cid := pve.BuildTemplateStemcellCID(templateVMID)
	otherCID := "template:6043"

	initialDesc := fmt.Sprintf(
		`{"name":"test","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","stemcell_refs":%q}`,
		cid+","+otherCID,
	)

	var capturedDesc string
	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": initialDesc}, nil
		},
		func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	)

	shouldDestroy, err := gatedDeregisterAndDestroyRef(context.Background(), deps, deps.Logger, templateNode, templateVMID, cid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldDestroy {
		t.Error("expected shouldDestroy=false when refs remain after removal")
	}
	if !strings.Contains(capturedDesc, otherCID) {
		t.Errorf("written description %q must contain remaining ref %q", capturedDesc, otherCID)
	}
}

// TestGatedDeregisterStemcellRef_MissingRefs_Conservative verifies that when
// the description has no parseable provenance, shouldDestroy=false.
func TestGatedDeregisterStemcellRef_MissingRefs_Conservative(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	cid := pve.BuildTemplateStemcellCID(templateVMID)

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Non-JSON description (pre-7.48 template or operator-written notes).
			return map[string]any{"description": "director: cf\ndeployment: prod\n"}, nil
		},
		func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return nil
		},
	)

	shouldDestroy, err := gatedDeregisterAndDestroyRef(context.Background(), deps, deps.Logger, templateNode, templateVMID, cid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldDestroy {
		t.Error("expected shouldDestroy=false when description is non-JSON (conservative rule)")
	}
}

// TestGatedDeregisterStemcellRef_EmptyRefs_Conservative verifies that when
// stemcell_refs exists but is empty, shouldDestroy=false.
func TestGatedDeregisterStemcellRef_EmptyRefs_Conservative(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	cid := pve.BuildTemplateStemcellCID(templateVMID)

	initialDesc := `{"name":"test","version":"1.0","sha8":"ab12ef34","created":"2026-06-12T00:00:00Z","stemcell_refs":""}`

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": initialDesc}, nil
		},
		func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return nil
		},
	)

	shouldDestroy, err := gatedDeregisterAndDestroyRef(context.Background(), deps, deps.Logger, templateNode, templateVMID, cid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldDestroy {
		t.Error("expected shouldDestroy=false when stemcell_refs is empty (conservative rule)")
	}
}

// TestGatedDeregisterStemcellRef_EmptyDescription_Conservative verifies that
// when the description field is absent entirely, shouldDestroy=false.
func TestGatedDeregisterStemcellRef_EmptyDescription_Conservative(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(6042)
	const templateNode = "pve-node1"
	cid := pve.BuildTemplateStemcellCID(templateVMID)

	deps := buildRefsTestDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// No description key in the returned map.
			return map[string]any{}, nil
		},
		func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return nil
		},
	)

	shouldDestroy, err := gatedDeregisterAndDestroyRef(context.Background(), deps, deps.Logger, templateNode, templateVMID, cid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shouldDestroy {
		t.Error("expected shouldDestroy=false when description key is absent (conservative rule)")
	}
}
