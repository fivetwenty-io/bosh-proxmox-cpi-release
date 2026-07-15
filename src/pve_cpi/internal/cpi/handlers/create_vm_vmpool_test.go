package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

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
