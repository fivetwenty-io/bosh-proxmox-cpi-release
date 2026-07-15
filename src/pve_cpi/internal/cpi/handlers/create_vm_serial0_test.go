package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// serial0=socket: default on every VM, both create paths. Stemcells log the
// BOSH agent's console output to the serial console, so a wedged agent is
// only debuggable via `qm terminal` when a serial device exists. Unlike
// tablet, this default is overridable via cloud_properties.pve_config.serial0
// (allowlisted) — see create_vm_pve_config_test.go for the override test.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_Serial0_DefaultsToSocket(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-serial0-import", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("serial0-import")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	serial0Val, present := p["serial0"]
	if !present {
		t.Fatal("createParams must carry \"serial0\" key by default — no serial device means a wedged agent has no console")
	}
	if sv, ok := serial0Val.(string); !ok || sv != "socket" {
		t.Errorf("createParams[\"serial0\"] = %v (%T); want string \"socket\"", serial0Val, serial0Val)
	}
}

func TestCreateVM_ClonePath_Serial0_DefaultsToSocket(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000D001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-serial0-clone", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("serial0-clone")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	// The resource-shape UpdateQemuConfig call is the one carrying a non-nil
	// Memory field (mirrors the tablet/cpu_type tests' approach of locating
	// the relevant call by a field only that call sets).
	var found bool
	for _, c := range n.updateConfigCalls {
		if c.params.Memory == nil {
			continue
		}
		found = true
		if c.params.Serial == nil {
			t.Error("resource UpdateQemuConfig params.Serial is nil; want index 0 set to \"socket\"")
			continue
		}
		if v := c.params.Serial[0]; v != "socket" {
			t.Errorf("params.Serial[0] = %q; want \"socket\"", v)
		}
	}
	if !found {
		t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (resource-shape call not found)")
	}
}
