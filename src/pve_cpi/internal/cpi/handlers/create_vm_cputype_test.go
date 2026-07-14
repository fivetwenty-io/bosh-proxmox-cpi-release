package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// pve.cpu_type / cloud_properties.cpu_type: import path (createParams["cpu"])
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_CPUType_Unset_NoCPUKey(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-cputype-unset", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("cputype-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if v, present := p["cpu"]; present {
		t.Errorf("createParams must carry no \"cpu\" key when unset everywhere (byte-identical); got %v", v)
	}
}

func TestCreateVM_ImportPath_CPUType_GlobalConfig_SetsCPUKey(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.CPUType = "x86-64-v2-AES"
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-cputype-global", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("cputype-global")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if cpu, _ := p["cpu"].(string); cpu != "x86-64-v2-AES" {
		t.Errorf("createParams[\"cpu\"] = %q; want x86-64-v2-AES (global default)", cpu)
	}
}

func TestCreateVM_ImportPath_CPUType_CloudProperties_OverridesGlobal(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.CPUType = "x86-64-v2-AES" // global default; must lose to per-call value
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-cputype-override", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "cpu_type": "host"},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("cputype-override")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if cpu, _ := p["cpu"].(string); cpu != "host" {
		t.Errorf("createParams[\"cpu\"] = %q; want host (cloud_properties.cpu_type overrides global)", cpu)
	}
}

// ---------------------------------------------------------------------------
// pve.cpu_type / cloud_properties.cpu_type: clone path (UpdateQemuConfig.Cpu)
// ---------------------------------------------------------------------------

func cloneCPUTypeArgs(agentID string, cloudProps map[string]any) []json.RawMessage {
	return mkArgs(agentID, testTemplateCID, cloudProps,
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
}

func TestCreateVM_ClonePath_CPUType_Unset_NilCpu(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000A001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := cloneCPUTypeArgs("agent-clone-cputype-unset", map[string]any{"cores": 1, "memory": 512})
	if _, err := h.Handle(context.Background(), args, mkCtx("clone-cputype-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]
	if resourceCall.params.Cpu != nil {
		t.Errorf("Cpu must be nil in the resource UpdateQemuConfig params when unset everywhere (byte-identical); got %q", *resourceCall.params.Cpu)
	}
}

func TestCreateVM_ClonePath_CPUType_GlobalConfig_SetsCpuField(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000A002:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.CPUType = "x86-64-v2-AES"
	h := handlers.HandleCreateVM(deps)

	args := cloneCPUTypeArgs("agent-clone-cputype-global", map[string]any{"cores": 1, "memory": 512})
	if _, err := h.Handle(context.Background(), args, mkCtx("clone-cputype-global")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]
	if resourceCall.params.Cpu == nil || *resourceCall.params.Cpu != "x86-64-v2-AES" {
		got := "<nil>"
		if resourceCall.params.Cpu != nil {
			got = *resourceCall.params.Cpu
		}
		t.Errorf("Cpu = %q; want x86-64-v2-AES (global default)", got)
	}
}

func TestCreateVM_ClonePath_CPUType_CloudProperties_OverridesGlobal(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000A003:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.CPUType = "x86-64-v2-AES" // global; must lose to per-call value
	h := handlers.HandleCreateVM(deps)

	args := cloneCPUTypeArgs("agent-clone-cputype-override",
		map[string]any{"cores": 1, "memory": 512, "cpu_type": "host"})
	if _, err := h.Handle(context.Background(), args, mkCtx("clone-cputype-override")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	resourceCall := n.updateConfigCalls[0]
	if resourceCall.params.Cpu == nil || *resourceCall.params.Cpu != "host" {
		got := "<nil>"
		if resourceCall.params.Cpu != nil {
			got = *resourceCall.params.Cpu
		}
		t.Errorf("Cpu = %q; want host (cloud_properties.cpu_type overrides global)", got)
	}
}

// ---------------------------------------------------------------------------
// pve_config.cpu coexistence: the raw escape hatch still wins as the final
// write, and setting it logs a pointer at the first-class cpu_type knob.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_PVEConfigCPU_WinsAsRawOverride_AndLogsPointer(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)

	var buf bytes.Buffer
	logger, lerr := log.NewLogger("info", &buf)
	if lerr != nil {
		t.Fatalf("NewLogger: %v", lerr)
	}
	deps.Logger = logger

	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-cputype-pveconfig", testStemcellCID,
		map[string]any{
			"cores":    1,
			"memory":   512,
			"cpu_type": "x86-64-v2-AES",
			"pve_config": map[string]any{
				"cpu": "host",
			},
		},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("cputype-pveconfig")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The initial create carries cpu_type's resolved value ("x86-64-v2-AES").
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if cpu, _ := q.createCalls[0].params["cpu"].(string); cpu != "x86-64-v2-AES" {
		t.Errorf("createParams[\"cpu\"] = %q; want x86-64-v2-AES (cpu_type applied at create time)", cpu)
	}

	// The pve_config passthrough issues a separate UpdateQemuConfig call
	// carrying the raw "host" value — the effective final write, since PVE's
	// config PUT is a partial merge: later unrelated UpdateQemuConfig calls
	// (e.g. NIC configuration) do not touch the "cpu" key and so cannot reset
	// it. Exactly one UpdateQemuConfig call should carry a non-nil Cpu field;
	// find it and assert it carries the raw pve_config value, not cpu_type's.
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls (pve_config passthrough), got %d", len(n.updateConfigCalls))
	}
	var sawCPUWrite bool
	for _, c := range n.updateConfigCalls {
		if c.params.Cpu == nil {
			continue
		}
		sawCPUWrite = true
		if *c.params.Cpu != "host" {
			t.Errorf("UpdateQemuConfig Cpu write = %q; want host (raw pve_config escape hatch wins)", *c.params.Cpu)
		}
	}
	if !sawCPUWrite {
		t.Fatal("expected exactly one UpdateQemuConfig call to carry a non-nil Cpu field (the pve_config passthrough)")
	}

	if !strings.Contains(buf.String(), "pve_config.cpu is set") || !strings.Contains(buf.String(), "cpu_type") {
		t.Errorf("expected an Info pointer log naming cpu_type when pve_config.cpu is set, got %q", buf.String())
	}
}
