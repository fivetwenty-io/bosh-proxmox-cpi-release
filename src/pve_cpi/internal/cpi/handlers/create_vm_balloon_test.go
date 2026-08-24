package handlers_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// balloon: written as 0 (device disabled) on every VM by default, both create
// paths. Operators enable ballooning with a MiB floor via pve.balloon or
// cloud_properties.balloon; the "pve-default" sentinel restores PVE's own
// default by writing no balloon key at all.
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_Balloon_DefaultDisabled(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-balloon-import", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-import")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	balloonVal, present := p["balloon"]
	if !present {
		t.Fatal("createParams must carry \"balloon\" key — the CPI disables ballooning by default on every VM")
	}
	if iv, ok := balloonVal.(int); !ok || iv != 0 {
		t.Errorf("createParams[\"balloon\"] = %v (%T); want int 0", balloonVal, balloonVal)
	}
}

func TestCreateVM_ImportPath_Balloon_CloudPropsValue_Written(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-balloon-import-cp", testStemcellCID,
		map[string]any{"cores": 1, "memory": 2048, "balloon": 1024},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-import-cp")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if iv, _ := p["balloon"].(int); iv != 1024 {
		t.Errorf("createParams[\"balloon\"] = %v; want 1024 (cloud_properties.balloon)", p["balloon"])
	}
}

func TestCreateVM_ImportPath_Balloon_Sentinel_OmitsKey(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.Balloon = "1024" // global; per-call sentinel must suppress the key
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-balloon-import-sentinel", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "balloon": config.BalloonPVEDefault},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-import-sentinel")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if v, present := p["balloon"]; present {
		t.Errorf("createParams must carry no \"balloon\" key for the pve-default sentinel (PVE keeps its own default); got %v", v)
	}
}

func TestCreateVM_ImportPath_Balloon_EqualsMemory_Allowed(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-balloon-import-eq", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "balloon": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-import-eq")); err != nil {
		t.Fatalf("unexpected error for balloon == memory (the inclusive boundary): %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	if iv, _ := q.createCalls[0].params["balloon"].(int); iv != 512 {
		t.Errorf("createParams[\"balloon\"] = %v; want 512", q.createCalls[0].params["balloon"])
	}
}

func TestCreateVM_ImportPath_Balloon_ExceedsMemory_FailsFast(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-balloon-import-oversize", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "balloon": 1024},
		defaultNetMap(), []string{}, map[string]any{})

	_, err := h.Handle(context.Background(), args, mkCtx("balloon-import-oversize"))
	if err == nil {
		t.Fatal("expected error for balloon > memory, got nil")
	}
	if !strings.Contains(err.Error(), "balloon") {
		t.Errorf("error %q does not name the balloon knob", err)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("expected no Create calls after fail-fast validation, got %d", len(q.createCalls))
	}
}

func TestCreateVM_ClonePath_Balloon_DefaultDisabled(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-balloon-clone", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-clone")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.updateConfigCalls) < 1 {
		t.Fatalf("expected >=1 UpdateQemuConfig calls, got %d", len(n.updateConfigCalls))
	}
	// The resource-shape UpdateQemuConfig call is the one carrying a non-nil
	// Memory field (mirrors the tablet tests' approach of locating the
	// relevant call by a field only that call sets).
	var found bool
	for _, c := range n.updateConfigCalls {
		if c.params.Memory == nil {
			continue
		}
		found = true
		if c.params.Balloon == nil {
			t.Error("resource UpdateQemuConfig params.Balloon is nil; want explicit 0 (ballooning disabled by default on every clone)")
			continue
		}
		if *c.params.Balloon != 0 {
			t.Errorf("params.Balloon = %v; want 0", *c.params.Balloon)
		}
	}
	if !found {
		t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (resource-shape call not found)")
	}
}

func TestCreateVM_ClonePath_Balloon_GlobalValue_Written(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C002:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.Balloon = "256"
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-balloon-clone-global", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-clone-global")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, c := range n.updateConfigCalls {
		if c.params.Memory == nil {
			continue
		}
		found = true
		if c.params.Balloon == nil || *c.params.Balloon != 256 {
			got := "nil"
			if c.params.Balloon != nil {
				got = strconv.FormatInt(*c.params.Balloon, 10)
			}
			t.Errorf("params.Balloon = %s; want 256 (global pve.balloon)", got)
		}
	}
	if !found {
		t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (resource-shape call not found)")
	}
}

func TestCreateVM_ClonePath_Balloon_Sentinel_OmitsField(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C003:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.Balloon = config.BalloonPVEDefault
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-balloon-clone-sentinel", testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("balloon-clone-sentinel")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, c := range n.updateConfigCalls {
		if c.params.Memory == nil {
			continue
		}
		found = true
		if c.params.Balloon != nil {
			t.Errorf("params.Balloon = %v; want nil for the pve-default sentinel (no balloon key written)", *c.params.Balloon)
		}
		// Writing nothing is not enough on the clone path: the stemcell
		// template carries balloon=0 and PVE's clone copies the full source
		// config, so the sentinel must actively DELETE the inherited key to
		// restore PVE's own default (device enabled, balloon = memory).
		if c.params.Delete == nil || !strings.Contains(*c.params.Delete, "balloon") {
			got := "nil"
			if c.params.Delete != nil {
				got = *c.params.Delete
			}
			t.Errorf("params.Delete = %s; want it to contain \"balloon\" (sentinel must clear the template-inherited balloon=0)", got)
		}
	}
	if !found {
		t.Fatal("no UpdateQemuConfig call carried a non-nil Memory field (resource-shape call not found)")
	}
}
