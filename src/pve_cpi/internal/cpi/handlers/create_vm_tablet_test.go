package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// tablet=0: unconditional on every VM, both create paths. No cloud_properties
// knob exists — the emulated USB tablet is pure overhead on a headless BOSH
// VM, so there is nothing to make configurable.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_Tablet_AlwaysDisabled(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-tablet-import", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("tablet-import")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	tabletVal, present := p["tablet"]
	if !present {
		t.Fatal("createParams must carry \"tablet\" key — the CPI writes it unconditionally on every VM")
	}
	if iv, ok := tabletVal.(int); !ok || iv != 0 {
		t.Errorf("createParams[\"tablet\"] = %v (%T); want int 0", tabletVal, tabletVal)
	}
}

func TestCreateVM_ClonePath_Tablet_AlwaysDisabled(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000B001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-tablet-clone", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("tablet-clone")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	// The resource-shape UpdateQemuConfig call is the one carrying a non-nil
	// Memory field (mirrors the cpu_type tests' approach of locating the
	// relevant call by a field only that call sets).
	var found bool
	for _, c := range n.updateConfigCalls {
		if c.params.Memory == nil {
			continue
		}
		found = true
		if c.params.Tablet == nil {
			t.Error("resource UpdateQemuConfig params.Tablet is nil; want explicit false (tablet must be disabled on every clone)")
			continue
		}
		if *c.params.Tablet != false {
			t.Errorf("params.Tablet = %v; want false", *c.params.Tablet)
		}
	}
	if !found {
		t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (resource-shape call not found)")
	}
}
