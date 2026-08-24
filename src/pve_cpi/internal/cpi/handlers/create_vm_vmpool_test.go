package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// withPoolsSvc wires ps as the pool service on deps.PVE. deps.PVE must be the
// *mockPVEClient constructed by buildVMDeps/buildVMDepsForTemplate (both
// return that concrete type through the handlers.Deps.PVE interface field);
// this file is in package handlers_test alongside testmocks_test.go, so the
// assertion is a same-package cast, not a cross-package type leak.
func withPoolsSvc(t *testing.T, deps handlers.Deps, ps pve.PoolService) {
	t.Helper()
	mc, ok := deps.PVE.(*mockPVEClient)
	if !ok {
		t.Fatalf("withPoolsSvc: deps.PVE is %T, want *mockPVEClient", deps.PVE)
	}
	mc.poolsSvc = ps
}

// poolCallRecorder is a pve.PoolService that records every CreatePool call
// (poolID + comment) and lets a test inject a CreatePool error — e.g. the
// live PVE "already exists" 500+text shape (isPoolAlreadyExists tolerates it)
// or an unrelated failure. AddVM/DeletePool/GetPoolComment are silent no-ops:
// none of these create_vm tests exercise pool membership or the delete_vm
// reaper.
type poolCallRecorder struct {
	createPoolErr   error
	createPoolCalls []poolCreateCall
}

type poolCreateCall struct {
	poolID  string
	comment string
}

func (p *poolCallRecorder) AddVM(_ context.Context, _ string, _ int64) error        { return nil }
func (p *poolCallRecorder) MoveVMToPool(_ context.Context, _ string, _ int64) error { return nil }

func (p *poolCallRecorder) CreatePool(_ context.Context, poolID, comment string) error {
	p.createPoolCalls = append(p.createPoolCalls, poolCreateCall{poolID: poolID, comment: comment})
	return p.createPoolErr
}

func (p *poolCallRecorder) DeletePool(_ context.Context, _ string) error { return nil }

func (p *poolCallRecorder) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// ---------------------------------------------------------------------------
// pve.vm_pool: global-only knob (no cloud_properties override), both create
// paths. Unset (default) means no "pool" key/field anywhere — byte-identical
// to every release before this property existed.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_VMPool_Unset_NoPoolKey(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-vmpool-unset", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vmpool-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if v, present := q.createCalls[0].params["pool"]; present {
		t.Errorf("createParams must not carry a \"pool\" key when vm_pool is unset; got %v", v)
	}
}

func TestCreateVM_ImportPath_VMPool_Set_CarriesPoolKey(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.VMPool = "bosh-vms"
	withPoolsSvc(t, deps, &noopPoolService{})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-vmpool-set", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vmpool-set")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if got, _ := q.createCalls[0].params["pool"].(string); got != "bosh-vms" {
		t.Errorf("createParams[\"pool\"] = %q; want %q", got, "bosh-vms")
	}
}

func TestCreateVM_ClonePath_VMPool_Unset_NilField(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000E001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-vmpool-clone-unset", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vmpool-clone-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
	}
	if p := n.createQemuCloneCalls[0].params.Pool; p != nil {
		t.Errorf("CreateQemuCloneParams.Pool = %v; want nil when vm_pool is unset", *p)
	}
}

func TestCreateVM_ClonePath_VMPool_Set_CarriesPoolField(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000E002:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.VMPool = "bosh-vms"
	withPoolsSvc(t, deps, &noopPoolService{})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-vmpool-clone-set", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("vmpool-clone-set")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
	}
	p := n.createQemuCloneCalls[0].params.Pool
	if p == nil || *p != "bosh-vms" {
		got := "<nil>"
		if p != nil {
			got = *p
		}
		t.Errorf("CreateQemuCloneParams.Pool = %v; want %q", got, "bosh-vms")
	}
}

// ---------------------------------------------------------------------------
// Resolved-pool wiring (T4): create_vm assigns the RESOLVED pool name (call >
// vm_type > vm_pool_template > global vm_pool — resolvePoolName) and ensures
// it exists (create-if-missing, tolerating a concurrent/prior create) before
// either create path consumes it.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_ResolvedPoolAssigned(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.VMPool = "bosh"
	pools := &poolCallRecorder{}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-pool-resolved-import", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("pool-resolved-import")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if got, _ := q.createCalls[0].params["pool"].(string); got != "bosh" {
		t.Errorf("createParams[\"pool\"] = %q; want %q", got, "bosh")
	}
	if len(pools.createPoolCalls) != 1 {
		t.Fatalf("expected EnsurePoolExists (CreatePool) called once, got %d calls", len(pools.createPoolCalls))
	}
	if pools.createPoolCalls[0].poolID != "bosh" {
		t.Errorf("CreatePool poolID = %q; want %q", pools.createPoolCalls[0].poolID, "bosh")
	}
	if !strings.HasPrefix(pools.createPoolCalls[0].comment, pve.PoolProvenanceComment) {
		t.Errorf("CreatePool comment = %q; want prefix %q", pools.createPoolCalls[0].comment, pve.PoolProvenanceComment)
	}
}

func TestCreateVM_ClonePath_ResolvedPoolAssigned(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000E003:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.VMPool = "bosh"
	pools := &poolCallRecorder{}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-pool-resolved-clone", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("pool-resolved-clone")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
	}
	p := n.createQemuCloneCalls[0].params.Pool
	if p == nil || *p != "bosh" {
		got := "<nil>"
		if p != nil {
			got = *p
		}
		t.Errorf("CreateQemuCloneParams.Pool = %v; want %q", got, "bosh")
	}
	if len(pools.createPoolCalls) != 1 {
		t.Fatalf("expected EnsurePoolExists (CreatePool) called once, got %d calls", len(pools.createPoolCalls))
	}
	if pools.createPoolCalls[0].poolID != "bosh" {
		t.Errorf("CreatePool poolID = %q; want %q", pools.createPoolCalls[0].poolID, "bosh")
	}
	if !strings.HasPrefix(pools.createPoolCalls[0].comment, pve.PoolProvenanceComment) {
		t.Errorf("CreatePool comment = %q; want prefix %q", pools.createPoolCalls[0].comment, pve.PoolProvenanceComment)
	}
}

func TestCreateVM_CallLevelPoolOverridesGlobal(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.VMPool = "bosh"
	pools := &poolCallRecorder{}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-pool-call-override", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "pool": "team-a"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("pool-call-override")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := q.createCalls[0].params["pool"].(string); got != "team-a" {
		// call-level cloud_properties.pool must outrank pve.vm_pool.
		t.Errorf("createParams[\"pool\"] = %q; want %q", got, "team-a")
	}
	if len(pools.createPoolCalls) != 1 || pools.createPoolCalls[0].poolID != "team-a" {
		t.Errorf("expected EnsurePoolExists called once with poolID %q; got %+v", "team-a", pools.createPoolCalls)
	}
}

func TestCreateVM_ExplicitEmptyVMPool_NoPoolKey(t *testing.T) {
	t.Parallel()

	t.Run("import", func(t *testing.T) {
		t.Parallel()
		q := &vmMockQEMU{}
		n := &vmMockNodes{}
		c := &vmMockCluster{}
		a := &vmMockAgent{}
		deps := buildVMDeps(q, n, c, a)
		deps.Config.VMPool = "" // explicit opt-out; no template, vm_type, or call override set
		pools := &poolCallRecorder{}
		withPoolsSvc(t, deps, pools)
		h := handlers.HandleCreateVM(deps)

		args := mkArgs("agent-pool-empty-import", testStemcellCID,
			map[string]any{"cores": 1, "memory": 512},
			defaultNetMap(), []string{}, map[string]any{})

		if _, err := h.Handle(context.Background(), args, mkCtx("pool-empty-import")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(q.createCalls) != 1 {
			t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
		}
		if v, present := q.createCalls[0].params["pool"]; present {
			t.Errorf("createParams must not carry a \"pool\" key when no pool resolves; got %v", v)
		}
		if len(pools.createPoolCalls) != 0 {
			t.Errorf("EnsurePoolExists must not be called when no pool resolves; got %d calls", len(pools.createPoolCalls))
		}
	})

	t.Run("clone", func(t *testing.T) {
		t.Parallel()
		n := &vmMockNodes{
			createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
				raw := sdknodes.CreateQemuCloneResponse{}
				_ = json.Unmarshal([]byte(`"UPID:pve:0000E011:00000001:clone:ok"`), &raw)
				return &raw, nil
			},
		}
		q := &vmMockQEMU{}
		a := &vmMockAgent{}
		deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
		deps.Config.VMPool = ""
		pools := &poolCallRecorder{}
		withPoolsSvc(t, deps, pools)
		h := handlers.HandleCreateVM(deps)

		args := mkArgs("agent-pool-empty-clone", testTemplateCID,
			map[string]any{"cores": 1, "memory": 512},
			map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
			[]string{}, map[string]any{})

		if _, err := h.Handle(context.Background(), args, mkCtx("pool-empty-clone")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(n.createQemuCloneCalls) != 1 {
			t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
		}
		if p := n.createQemuCloneCalls[0].params.Pool; p != nil {
			t.Errorf("CreateQemuCloneParams.Pool = %v; want nil when no pool resolves", *p)
		}
		if len(pools.createPoolCalls) != 0 {
			t.Errorf("EnsurePoolExists must not be called when no pool resolves; got %d calls", len(pools.createPoolCalls))
		}
	})
}

func TestCreateVM_TemplatePool_Rendered(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.VMPoolTemplate = "{prefix}-{deployment}"
	deps.Config.VMPrefix = "cpi"
	pools := &poolCallRecorder{}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	env := map[string]any{
		"bosh": map[string]any{
			"group":  "dir1-dep1-web",
			"groups": []any{"dir1", "dep1", "web"},
		},
	}
	args := mkArgs("agent-pool-template", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, env)

	if _, err := h.Handle(context.Background(), args, mkCtx("pool-template")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := q.createCalls[0].params["pool"].(string); got != "cpi-dep1" {
		t.Errorf("createParams[\"pool\"] = %q; want %q (rendered from vm_pool_template)", got, "cpi-dep1")
	}
	if len(pools.createPoolCalls) != 1 || pools.createPoolCalls[0].poolID != "cpi-dep1" {
		t.Errorf("expected EnsurePoolExists called once with poolID %q; got %+v", "cpi-dep1", pools.createPoolCalls)
	}
}

func TestCreateVM_EnsurePoolDuplicateTolerated(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.VMPool = "bosh"
	pools := &poolCallRecorder{
		createPoolErr: errors.New("create pool failed: pool 'bosh' already exists"),
	}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-pool-duplicate", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("pool-duplicate")); err != nil {
		t.Fatalf("expected create_vm to tolerate a live duplicate-pool CreatePool error, got: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if got, _ := q.createCalls[0].params["pool"].(string); got != "bosh" {
		t.Errorf("createParams[\"pool\"] = %q; want %q", got, "bosh")
	}
}

func TestCreateVM_InvalidPoolName_FailsPreCreate(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	pools := &poolCallRecorder{}
	withPoolsSvc(t, deps, pools)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-pool-invalid", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "pool": "a/b"},
		defaultNetMap(), []string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("pool-invalid"))
	if err == nil {
		t.Fatal("expected an error for a call-level pool name containing '/'")
	}
	if !isCloudError(err) {
		t.Errorf("expected a CloudError, got %T: %v", err, err)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("expected no QEMU.Create attempt (pre-create validation failure); got %d calls", len(q.createCalls))
	}
	if len(pools.createPoolCalls) != 0 {
		t.Errorf("expected no CreatePool attempt (pre-create validation failure); got %d calls", len(pools.createPoolCalls))
	}
}

// PoolHasVM reports no membership; tests that exercise the
// disambiguation supply their own fake.
func (p *poolCallRecorder) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}
