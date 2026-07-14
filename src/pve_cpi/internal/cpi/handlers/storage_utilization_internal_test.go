package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// suWarnMode is the shared "warn" literal for log level and
// storage.max_utilization_mode across the storage-utilization gate test
// files in this package. A single named constant (rather than repeating the
// literal at every call site) keeps the package's goconst literal-repetition
// count low — the string itself is intentionally reused across many test
// cases, unlike goconst's usual signal of an accidental typo-prone repeat.
const suWarnMode = "warn"

// suNodesStub implements nodes.Service for storage-utilization gate tests.
// Embedding the nil nodes.Service interface makes any unconfigured method
// panic on call, matching the embedding idiom already used by naClusterStub
// in placement_nodeaffinity_internal_test.go.
type suNodesStub struct {
	nodes.Service
	listStorageFn func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
}

func (s *suNodesStub) ListStorage(
	ctx context.Context, node string, params *nodes.ListStorageParams,
) (*nodes.ListStorageResponse, error) {
	if s.listStorageFn != nil {
		return s.listStorageFn(ctx, node, params)
	}
	return nil, nil
}

// suPVEClient implements pve.Client, exposing only Nodes() for these tests.
type suPVEClient struct {
	pve.Client
	nodesSvc nodes.Service
}

func (c *suPVEClient) Nodes() nodes.Service { return c.nodesSvc }

// suStorageResp builds a *nodes.ListStorageResponse from field maps, mirroring
// the raw JSON shape GET /nodes/<node>/storage returns.
func suStorageResp(entries ...map[string]any) *nodes.ListStorageResponse {
	resp := make(nodes.ListStorageResponse, 0, len(entries))
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		resp = append(resp, raw)
	}
	return &resp
}

// suPoolName is the storage pool name used across all suEntry test fixtures.
const suPoolName = "local-lvm"

// suEntry is the standard "active, images-capable" storage entry shape used
// by most test cases for pool suPoolName.
func suEntry(avail, total int64) map[string]any {
	return map[string]any{
		"storage": suPoolName,
		"active":  1,
		"enabled": 1,
		"content": "images,rootdir",
		"avail":   avail,
		"total":   total,
	}
}

func suDeps(t *testing.T, cfg *config.CPIConfig, nodesSvc nodes.Service, buf *bytes.Buffer) Deps {
	t.Helper()
	logger, err := log.NewLogger(suWarnMode, buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	return Deps{
		Config: cfg,
		PVE:    &suPVEClient{nodesSvc: nodesSvc},
		Logger: logger,
	}
}

// ---------------------------------------------------------------------------
// projectedUtilizationPct
// ---------------------------------------------------------------------------

func TestProjectedUtilizationPct_Basic(t *testing.T) {
	t.Parallel()
	// 100GiB total, 20GiB avail -> 80GiB used; +10GiB add -> 90%.
	const gib = int64(1024 * 1024 * 1024)
	got := projectedUtilizationPct(20*gib, 100*gib, 10*gib)
	if got != 90 {
		t.Errorf("projectedUtilizationPct = %v; want 90", got)
	}
}

func TestProjectedUtilizationPct_ZeroAddIsPointInTime(t *testing.T) {
	t.Parallel()
	const gib = int64(1024 * 1024 * 1024)
	got := projectedUtilizationPct(50*gib, 100*gib, 0)
	if got != 50 {
		t.Errorf("projectedUtilizationPct(addBytes=0) = %v; want 50", got)
	}
}

// ---------------------------------------------------------------------------
// checkMaxUtilizationGate: disabled / missing-facts fail-open
// ---------------------------------------------------------------------------

func TestCheckMaxUtilizationGate_Disabled_NoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := &config.CPIConfig{} // Storage nil -> ceiling 0 -> disabled
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		t.Fatal("ListStorage must not be called when the gate is disabled")
		return nil, nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 1024, "create_disk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckMaxUtilizationGate_MissingFacts_FailsOpen(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 80
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 1024, "create_disk"); err != nil {
		t.Fatalf("expected fail-open (nil error) on missing facts, got %v", err)
	}
	if !strings.Contains(buf.String(), "gate fails open") {
		t.Errorf("expected a fail-open Warn to be logged, got %q", buf.String())
	}
}

func TestCheckMaxUtilizationGate_PoolInactive_FailsOpen(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 80
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		entry := suEntry(0, 100)
		entry["active"] = 0
		return suStorageResp(entry), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 1, "create_disk"); err != nil {
		t.Fatalf("expected fail-open on inactive pool, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// checkMaxUtilizationGate: enforce mode — the gate matrix
// ---------------------------------------------------------------------------

const suGiB = int64(1024 * 1024 * 1024)

func TestCheckMaxUtilizationGate_Enforce_BelowCeiling_Proceeds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	// used=50GiB of 100GiB; +10GiB add -> 60% < 90%.
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(50*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 10*suGiB, "create_disk"); err != nil {
		t.Fatalf("unexpected error below ceiling: %v", err)
	}
}

func TestCheckMaxUtilizationGate_Enforce_AtCeiling_Proceeds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	// used=80GiB of 100GiB; +10GiB add -> exactly 90% (boundary: not > ceiling).
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(20*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 10*suGiB, "create_disk"); err != nil {
		t.Fatalf("unexpected error exactly at ceiling: %v", err)
	}
}

func TestCheckMaxUtilizationGate_Enforce_AboveCeiling_RetriableError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	// used=85GiB of 100GiB; +10GiB add -> 95% > 90%.
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(15*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 10*suGiB, "create_disk")
	if err == nil {
		t.Fatal("expected an error above the ceiling in enforce mode")
	}
	if !cpierrIsRetriable(err) {
		t.Errorf("expected a RETRIABLE error, got %v", err)
	}
	if !strings.Contains(err.Error(), "local-lvm") || !strings.Contains(err.Error(), "pve01") {
		t.Errorf("error must name pool and node, got: %v", err)
	}
	if !strings.Contains(err.Error(), "90") {
		t.Errorf("error must name the ceiling, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// checkMaxUtilizationGate: warn mode
// ---------------------------------------------------------------------------

func TestCheckMaxUtilizationGate_Warn_AboveCeiling_LogsAndProceeds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suWarnMode}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(15*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 10*suGiB, "create_disk"); err != nil {
		t.Fatalf("warn mode must never error, got %v", err)
	}
	if !strings.Contains(buf.String(), "warn mode; proceeding") {
		t.Errorf("expected a warn-mode Warn to be logged, got %q", buf.String())
	}
}

func TestCheckMaxUtilizationGate_Warn_BelowCeiling_NoLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suWarnMode}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(50*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	if err := checkMaxUtilizationGate(context.Background(), deps, "pve01", "local-lvm", 10*suGiB, "create_disk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("expected no log below ceiling, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// warnIfStorageAboveCeiling (snapshot_disk): always Warn-only
// ---------------------------------------------------------------------------

func TestWarnIfStorageAboveCeiling_AboveCeiling_WarnsRegardlessOfMode(t *testing.T) {
	t.Parallel()
	// Configured mode is "enforce", yet this evaluation point must only warn,
	// never error — snapshot_disk has no error return path from this call.
	var buf bytes.Buffer
	pct := 80
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: "enforce"}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		// Already at 85%, above the 80% ceiling, with addBytes=0 (point-in-time).
		return suStorageResp(suEntry(15*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	warnIfStorageAboveCeiling(context.Background(), deps, "pve01", "local-lvm")
	if !strings.Contains(buf.String(), "already above the utilization ceiling") {
		t.Errorf("expected an above-ceiling Warn, got %q", buf.String())
	}
}

func TestWarnIfStorageAboveCeiling_BelowCeiling_NoWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pct := 80
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		return suStorageResp(suEntry(50*suGiB, 100*suGiB)), nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	warnIfStorageAboveCeiling(context.Background(), deps, "pve01", "local-lvm")
	if buf.String() != "" {
		t.Errorf("expected no warn below ceiling, got %q", buf.String())
	}
}

func TestWarnIfStorageAboveCeiling_Disabled_NoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := &config.CPIConfig{} // gate disabled
	nodesSvc := &suNodesStub{listStorageFn: func(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
		t.Fatal("ListStorage must not be called when the gate is disabled")
		return nil, nil
	}}
	deps := suDeps(t, cfg, nodesSvc, &buf)
	warnIfStorageAboveCeiling(context.Background(), deps, "pve01", "local-lvm")
}

// cpierrIsRetriable reports whether err is a RETRIABLE CloudError, using the
// same errors package the production gate returns.
func cpierrIsRetriable(err error) bool {
	return cpierrors.IsType(err, cpierrors.TypeRetriableCloud)
}
