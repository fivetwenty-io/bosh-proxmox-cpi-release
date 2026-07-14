package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// --------------------------------------------------------------------------
// health-gate nodes mock
//
// Embeds panicNodesStub so every non-overridden method panics immediately,
// revealing accidental dependencies. Tests override CreateQemuAgentPing,
// ListQemuStatusCurrent, DeleteQemu, and UpdateQemuConfig as needed.
// --------------------------------------------------------------------------

type healthNodes struct {
	panicNodesStub

	pingFn            func(ctx context.Context, node, vmid string) (*sdknodes.CreateQemuAgentPingResponse, error)
	statusFn          func(ctx context.Context, node, vmid string) (*sdknodes.ListQemuStatusCurrentResponse, error)
	deleteQemuFn      func(ctx context.Context, node, vmid string, p *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
	updateConfigFn    func(ctx context.Context, node, vmid string, p *sdknodes.UpdateQemuConfigParams) error
	listQemuFn        func(ctx context.Context, node string, p *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
	listStorageFn     func(ctx context.Context, node string, p *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error)
	agentExecFn       func(ctx context.Context, node, vmid string, p *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error)
	agentExecStatusFn func(ctx context.Context, node, vmid string, p *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error)

	pingCalls int
	execCalls int
}

func (h *healthNodes) CreateQemuAgentExec(ctx context.Context, node, vmid string, p *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
	h.execCalls++
	if h.agentExecFn != nil {
		return h.agentExecFn(ctx, node, vmid, p)
	}
	panic("healthNodes.CreateQemuAgentExec: not configured")
}

func (h *healthNodes) ListQemuAgentExecStatus(ctx context.Context, node, vmid string, p *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error) {
	if h.agentExecStatusFn != nil {
		return h.agentExecStatusFn(ctx, node, vmid, p)
	}
	panic("healthNodes.ListQemuAgentExecStatus: not configured")
}

func (h *healthNodes) CreateQemuAgentPing(ctx context.Context, node, vmid string) (*sdknodes.CreateQemuAgentPingResponse, error) {
	h.pingCalls++
	if h.pingFn != nil {
		return h.pingFn(ctx, node, vmid)
	}
	panic("healthNodes.CreateQemuAgentPing: not configured")
}

func (h *healthNodes) ListQemuStatusCurrent(ctx context.Context, node, vmid string) (*sdknodes.ListQemuStatusCurrentResponse, error) {
	if h.statusFn != nil {
		return h.statusFn(ctx, node, vmid)
	}
	vmid64 := int64(100)
	return &sdknodes.ListQemuStatusCurrentResponse{Status: "running", Vmid: vmid64}, nil
}

func (h *healthNodes) DeleteQemu(ctx context.Context, node, vmid string, p *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if h.deleteQemuFn != nil {
		return h.deleteQemuFn(ctx, node, vmid, p)
	}
	return &sdknodes.DeleteQemuResponse{}, nil
}

func (h *healthNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *sdknodes.UpdateQemuConfigParams) error {
	if h.updateConfigFn != nil {
		return h.updateConfigFn(ctx, node, vmid, p)
	}
	return nil
}

func (h *healthNodes) ListQemu(ctx context.Context, node string, p *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if h.listQemuFn != nil {
		return h.listQemuFn(ctx, node, p)
	}
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

func (h *healthNodes) ListStorage(ctx context.Context, node string, p *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
	if h.listStorageFn != nil {
		return h.listStorageFn(ctx, node, p)
	}
	empty := sdknodes.ListStorageResponse{}
	return &empty, nil
}

func (h *healthNodes) ListVersion(_ context.Context, _ string) (*sdknodes.ListVersionResponse, error) {
	v := "0.0"
	return &sdknodes.ListVersionResponse{Version: v}, nil
}

// --------------------------------------------------------------------------
// helper: buildHealthDeps
//
// Builds a Deps wired to the given healthNodes mock and health_check config.
// Mirrors buildVMDeps but for health-gate scenarios.
// --------------------------------------------------------------------------

func buildHealthDeps(n *healthNodes, hcfg *config.HealthCheckConfig) handlers.Deps {
	clusterSvc := &vmMockCluster{}
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      storageName,
			NetworkBridge:  "vmbr0",
			VMIDRangeStart: 100,
			AgentMBus:      "nats://mbus.test:4222",
			// Placement disabled — tests focus on health-gate behavior, not scoring.
			Placement: &config.PlacementConfig{Enabled: placementDisabled},
			// IP-conflict check disabled (cluster ListResources returns empty).
			EnsureNoIPConflicts: placementDisabled,
			HealthCheck:         hcfg,
			// Explicitly disabled: as of Phase 1 this defaults to 30 (enabled)
			// when nil, and these health-gate tests don't wire SDN vnet/
			// node-network fakes for the gate to poll against.
			NetworkResolveRetries: new(int),
		},
		PVE: &mockPVEClient{
			qemuSvc:    &vmMockQEMU{},
			nodesSvc:   n,
			clusterSvc: clusterSvc,
			tasksSvc: &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return &sdktasks.Status{ExitStatus: "OK"}, nil
				},
			},
		},
		Agent:  &vmMockAgent{},
		Logger: log.NewNopLogger(),
	}
}

// healthCheckCfg builds a HealthCheckConfig with the given timeout and interval.
func healthCheckCfg(enabled bool, timeoutSec, intervalSec int) *config.HealthCheckConfig {
	e := enabled
	return &config.HealthCheckConfig{
		Enabled:     &e,
		TimeoutSec:  timeoutSec,
		IntervalSec: intervalSec,
	}
}

// standardNetworks returns a minimal networks map suitable for create_vm tests.
func standardNetworks() map[string]any {
	return map[string]any{
		"default": map[string]any{
			"type":    "manual",
			"ip":      "10.0.1.5",
			"netmask": "255.255.255.0",
			"gateway": "10.0.1.1",
			"dns":     []string{"8.8.8.8"},
			"default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{
				"bridge": "vmbr0",
			},
		},
	}
}

// --------------------------------------------------------------------------
// Scenario 1: disabled (default) — zero ping calls, behavior unchanged
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_Disabled_NoPingCalls(t *testing.T) {
	t.Parallel()

	n := &healthNodes{}
	// nil HealthCheck == disabled (default OFF).
	deps := buildHealthDeps(n, nil)

	args := mkArgs("agent-health-disabled", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("health-disabled"))
	if err != nil {
		t.Fatalf("create_vm with health gate disabled: unexpected error: %v", err)
	}
	if n.pingCalls != 0 {
		t.Errorf("expected 0 ping calls when health gate disabled, got %d", n.pingCalls)
	}
}

// --------------------------------------------------------------------------
// Scenario 2: enabled + agent ready on first ping → success, no rollback
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_Enabled_AgentReadyFirstPing(t *testing.T) {
	t.Parallel()

	n := &healthNodes{
		pingFn: func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			resp := sdknodes.CreateQemuAgentPingResponse(`{}`)
			return &resp, nil
		},
	}
	// 10s timeout, 1s interval — more than enough for an immediate success.
	deps := buildHealthDeps(n, healthCheckCfg(true, 10, 1))
	deleteQemuCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteQemuCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}

	args := mkArgs("agent-health-ready", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	result, err := h.Handle(context.Background(), args, mkCtx("health-ready"))
	if err != nil {
		t.Fatalf("create_vm with health gate, agent ready: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if n.pingCalls < 1 {
		t.Errorf("expected at least 1 ping call, got %d", n.pingCalls)
	}
	if deleteQemuCalled {
		t.Error("rollback (DeleteQemu) must NOT run when health gate passes")
	}
}

// --------------------------------------------------------------------------
// Scenario 3: enabled + transient ping failure then ready → retries, succeeds
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_Enabled_TransientFaultThenReady(t *testing.T) {
	t.Parallel()
	// Disable production floor so the test does not pay real-time waits.
	defer setHealthPollMinInterval(0)()

	callCount := 0
	n := &healthNodes{
		pingFn: func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			callCount++
			if callCount == 1 {
				// Use a transient-transport shape so the loop retries instead of failing fast.
				return nil, &sdkerrors.ConnectionError{Cause: errors.New("connection refused")}
			}
			resp := sdknodes.CreateQemuAgentPingResponse(`{}`)
			return &resp, nil
		},
	}
	// 30s timeout, 0s interval for fast test (interval applied only between retries).
	deps := buildHealthDeps(n, healthCheckCfg(true, 30, 0))

	args := mkArgs("agent-health-transient", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	result, err := h.Handle(context.Background(), args, mkCtx("health-transient"))
	if err != nil {
		t.Fatalf("create_vm with health gate, transient-then-ready: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 ping calls (transient+retry), got %d", callCount)
	}
}

// --------------------------------------------------------------------------
// Scenario 4: enabled + timeout → enriched error + rollback
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_Enabled_Timeout_EnrichedErrorAndRollback(t *testing.T) {
	t.Parallel()
	// Disable production floor so the 1s timeout expires quickly without the
	// floor adding extra latency between retries.
	defer setHealthPollMinInterval(0)()

	n := &healthNodes{
		pingFn: func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			// Use a transient shape so the loop retries until deadline rather than
			// failing fast on a permanent error.
			return nil, &sdkerrors.ConnectionError{Cause: errors.New("agent not ready")}
		},
		statusFn: func(_ context.Context, _, _ string) (*sdknodes.ListQemuStatusCurrentResponse, error) {
			return &sdknodes.ListQemuStatusCurrentResponse{Status: "running", Vmid: 200}, nil
		},
	}
	deleteQemuCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteQemuCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}

	// 1-second timeout → expires quickly; interval=0 for fast polling.
	deps := buildHealthDeps(n, healthCheckCfg(true, 1, 0))

	args := mkArgs("agent-health-timeout", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("health-timeout"))
	if err == nil {
		t.Fatal("expected error when agent ping times out, got nil")
	}

	// Error message must carry health-gate context.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "health") && !strings.Contains(errMsg, "agent") &&
		!strings.Contains(errMsg, "timeout") && !strings.Contains(errMsg, "deadline") &&
		!strings.Contains(errMsg, "ping") && !strings.Contains(errMsg, "ready") {
		t.Errorf("error message does not describe health gate failure: %q", errMsg)
	}

	// Rollback must fire: the existing defer cleanupVM runs when retErr != nil.
	if !deleteQemuCalled {
		t.Error("expected rollback (DeleteQemu) to run after health gate timeout, but it did not")
	}
}

// --------------------------------------------------------------------------
// Scenario 5: enabled + context cancelled before deadline → error + rollback
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_Enabled_ContextCancelled(t *testing.T) {
	t.Parallel()
	// Disable production floor so context expiry (200ms) drives the test, not the floor.
	defer setHealthPollMinInterval(0)()

	blockCh := make(chan struct{})
	n := &healthNodes{
		pingFn: func(ctx context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-blockCh:
				resp := sdknodes.CreateQemuAgentPingResponse(`{}`)
				return &resp, nil
			}
		},
	}
	deleteQemuCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteQemuCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}

	// Long health-check timeout so only parent context expiry drives failure.
	deps := buildHealthDeps(n, healthCheckCfg(true, 60, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	defer close(blockCh)

	args := mkArgs("agent-health-ctx", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(ctx, args, mkCtx("health-ctx-cancel"))
	if err == nil {
		t.Fatal("expected error when context is cancelled during health ping")
	}
	if deleteQemuCalled {
		// Rollback is attempted with a detached context (contextWithoutCancel).
		// Whether DeleteQemu is called depends on timing; not an assertion.
		_ = deleteQemuCalled
	}
}

// --------------------------------------------------------------------------
// Scenario 6: explicit enabled=false is identical to absent (default OFF)
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_ExplicitFalse_NoPingCalls(t *testing.T) {
	t.Parallel()

	n := &healthNodes{}
	// Explicit enabled=false — must not call ping.
	deps := buildHealthDeps(n, healthCheckCfg(false, 30, 1))

	args := mkArgs("agent-health-explicit-false", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("health-explicit-false"))
	if err != nil {
		t.Fatalf("create_vm with explicit health_check.enabled=false: unexpected error: %v", err)
	}
	if n.pingCalls != 0 {
		t.Errorf("expected 0 ping calls when enabled=false, got %d", n.pingCalls)
	}
}

// --------------------------------------------------------------------------
// Config unit tests (validate-only-when-set pattern)
// --------------------------------------------------------------------------

func TestHealthCheckConfig_Defaults(t *testing.T) {
	t.Parallel()

	// Nil HealthCheck block — disabled, all accessors return safe defaults.
	cfg := &config.CPIConfig{}
	if cfg.HealthCheckEnabled() {
		t.Error("HealthCheckEnabled must be false when HealthCheck is nil")
	}
	if cfg.HealthCheckTimeoutSec() <= 0 {
		t.Errorf("HealthCheckTimeoutSec must return a positive default, got %d", cfg.HealthCheckTimeoutSec())
	}
	if cfg.HealthCheckIntervalSec() <= 0 {
		t.Errorf("HealthCheckIntervalSec must return a positive default, got %d", cfg.HealthCheckIntervalSec())
	}
}

func TestHealthCheckConfig_EnabledOverride(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := &config.CPIConfig{
		HealthCheck: &config.HealthCheckConfig{
			Enabled:     &enabled,
			TimeoutSec:  120,
			IntervalSec: 3,
		},
	}
	if !cfg.HealthCheckEnabled() {
		t.Error("HealthCheckEnabled must be true when Enabled=*true")
	}
	if cfg.HealthCheckTimeoutSec() != 120 {
		t.Errorf("HealthCheckTimeoutSec: want 120, got %d", cfg.HealthCheckTimeoutSec())
	}
	if cfg.HealthCheckIntervalSec() != 3 {
		t.Errorf("HealthCheckIntervalSec: want 3, got %d", cfg.HealthCheckIntervalSec())
	}
}

func TestHealthCheckConfig_Validate_TimeoutRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		timeout   int
		interval  int
		wantError bool
	}{
		{"valid", 300, 5, false},
		{"zero timeout uses default", 0, 5, false},
		{"negative timeout", -1, 5, true},
		{"too large timeout", 3601, 5, true},
		{"zero interval valid (no-sleep)", 300, 0, false},
		{"negative interval invalid", 300, -1, true},
		{"max valid", 3600, 3600, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enabled := true
			cfg := minimalValidCPIConfig()
			cfg.HealthCheck = &config.HealthCheckConfig{
				Enabled:     &enabled,
				TimeoutSec:  tc.timeout,
				IntervalSec: tc.interval,
			}
			err := cfg.Validate()
			if tc.wantError && err == nil {
				t.Errorf("expected validation error for %+v, got nil", tc)
			}
			if !tc.wantError && err != nil {
				t.Errorf("unexpected validation error for %+v: %v", tc, err)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Scenario 7: interval=0 + production floor — loop does not busy-spin
//
// Asserts that the production floor (1s) is applied when IntervalSec==0.
// The test uses a 2s timeout so at most 2 ping calls can occur with the
// 1s floor, verifying the floor is in effect. Without a floor a tight loop
// would accumulate far more calls before the deadline fires.
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_ZeroInterval_FlooredToProdMin(t *testing.T) {
	// Not parallel: this test relies on the production-default floor value (1s).
	// Running in parallel with tests that override the floor via
	// setHealthPollMinInterval would produce a racy result even with atomic storage.
	defer setHealthPollMinInterval(1 * time.Second)()

	// The floor (1s default) must limit call count to ≤3 within a 2s window.

	n := &healthNodes{
		pingFn: func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			// Always transient so the loop retries until the deadline.
			return nil, &sdkerrors.ConnectionError{Cause: errors.New("not ready")}
		},
		statusFn: func(_ context.Context, _, _ string) (*sdknodes.ListQemuStatusCurrentResponse, error) {
			return &sdknodes.ListQemuStatusCurrentResponse{Status: "running", Vmid: 100}, nil
		},
	}
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		return &sdknodes.DeleteQemuResponse{}, nil
	}

	// interval=0 in config; production floor clamps to 1s.
	// 2s timeout → at most 2 floor-period sleeps → ≤3 ping calls.
	deps := buildHealthDeps(n, healthCheckCfg(true, 2, 0))

	args := mkArgs("agent-floor-test", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	start := time.Now()
	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("health-floor"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error when agent never ready")
	}
	// Floor is 1s; 2s timeout. A tight loop without floor would accumulate
	// hundreds of calls; with the floor we expect ≤4.
	if n.pingCalls > 4 {
		t.Errorf("floor not applied: got %d ping calls in %v (expected ≤4 with 1s floor)",
			n.pingCalls, elapsed)
	}
}

// --------------------------------------------------------------------------
// Scenario 8: permanent ping error → fails fast, well before timeout
// --------------------------------------------------------------------------

func TestCreateVM_HealthGate_PermanentPingError_FailsFast(t *testing.T) {
	t.Parallel()
	// Leave production floor; the permanent-error path must not reach the floor
	// sleep at all — it returns immediately on first permanent ping failure.

	n := &healthNodes{
		pingFn: func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
			// 401 Unauthorized — permanent, non-transport error.
			return nil, &sdkerrors.APIError{HTTPCode: 401, Message: "unauthorized"}
		},
		statusFn: func(_ context.Context, _, _ string) (*sdknodes.ListQemuStatusCurrentResponse, error) {
			return &sdknodes.ListQemuStatusCurrentResponse{Status: "running", Vmid: 100}, nil
		},
	}
	deleteQemuCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteQemuCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}

	// Long timeout (30s) — permanent error must fail in well under 1s, not 30s.
	deps := buildHealthDeps(n, healthCheckCfg(true, 30, 5))

	args := mkArgs("agent-permanent-err", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		standardNetworks(), []string{}, map[string]any{})

	start := time.Now()
	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("health-permanent"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error on permanent ping failure")
	}
	// Must fail fast — well under the 30s timeout. Assert under 5s to give
	// generous margin for slow CI while still catching the busy-wait regression.
	if elapsed > 5*time.Second {
		t.Errorf("permanent ping error did not fail fast: elapsed %v (want <5s)", elapsed)
	}
	if n.pingCalls != 1 {
		t.Errorf("expected exactly 1 ping call before permanent fail-fast, got %d", n.pingCalls)
	}
	if !deleteQemuCalled {
		t.Error("expected rollback (DeleteQemu) after permanent ping error")
	}
}

// minimalValidCPIConfig returns a CPIConfig that passes validation without
// optional fields. Used to test optional block validation in isolation.
func minimalValidCPIConfig() *config.CPIConfig {
	c := &config.CPIConfig{
		Host:           "pve.test.local",
		Port:           8006,
		User:           "root",
		APIToken:       "test-token",
		Node:           "pve",
		VMStorage:      "local-lvm",
		DiskStorage:    "local-lvm",
		NetworkBridge:  "vmbr0",
		AgentMode:      "noagent",
		VMDiskFormat:   "qcow2",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   8999,
	}
	c.ApplyDefaults()
	return c
}

// --------------------------------------------------------------------------
// §7.29 boot-path agent integrity / checksum assertion
// --------------------------------------------------------------------------

// testAgentSHA is a valid 64-hex SHA-256 used as the expected agent digest.
const testAgentSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// readyPingFn returns a healthNodes pingFn that reports the agent ready.
func readyPingFn() func(context.Context, string, string) (*sdknodes.CreateQemuAgentPingResponse, error) {
	return func(_ context.Context, _, _ string) (*sdknodes.CreateQemuAgentPingResponse, error) {
		resp := sdknodes.CreateQemuAgentPingResponse(`{}`)
		return &resp, nil
	}
}

// execReturning builds an agentExecStatusFn that reports an exited sha256sum
// whose stdout is "<digest>  <path>".
func execStatusReturning(digest string, exitCode int64) func(context.Context, string, string, *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error) {
		out := digest + "  /var/vcap/bosh/bin/bosh-agent\n"
		ec := exitCode
		return &sdknodes.ListQemuAgentExecStatusResponse{Exited: true, Exitcode: &ec, OutData: &out}, nil
	}
}

func healthCfgWithSHA() *config.HealthCheckConfig {
	hc := healthCheckCfg(true, 10, 0)
	hc.ExpectedAgentSHA256 = testAgentSHA
	return hc
}

func TestCreateVM_AgentChecksum_Match(t *testing.T) {
	t.Parallel()
	defer handlers.SetAgentChecksumTimings(50*time.Millisecond, time.Millisecond)()

	n := &healthNodes{
		pingFn: readyPingFn(),
		agentExecFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
			return &sdknodes.CreateQemuAgentExecResponse{Pid: 42}, nil
		},
		agentExecStatusFn: execStatusReturning(testAgentSHA, 0),
	}
	deleteCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}
	deps := buildHealthDeps(n, healthCfgWithSHA())

	args := mkArgs("agent-checksum-match", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512}, standardNetworks(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("checksum-match"))
	if err != nil {
		t.Fatalf("matching checksum must succeed, got: %v", err)
	}
	if n.execCalls < 1 {
		t.Error("expected sha256sum exec to run")
	}
	if deleteCalled {
		t.Error("rollback must NOT run when checksum matches")
	}
}

func TestCreateVM_AgentChecksum_Mismatch_FailsAndRollsBack(t *testing.T) {
	t.Parallel()
	defer handlers.SetAgentChecksumTimings(50*time.Millisecond, time.Millisecond)()

	wrong := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	n := &healthNodes{
		pingFn: readyPingFn(),
		agentExecFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
			return &sdknodes.CreateQemuAgentExecResponse{Pid: 7}, nil
		},
		agentExecStatusFn: execStatusReturning(wrong, 0),
	}
	deleteCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}
	deps := buildHealthDeps(n, healthCfgWithSHA())

	args := mkArgs("agent-checksum-mismatch", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512}, standardNetworks(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("checksum-mismatch"))
	if err == nil {
		t.Fatal("mismatched checksum must fail create_vm")
	}
	if !strings.Contains(err.Error(), "agent integrity check failed") {
		t.Errorf("unexpected error message: %v", err)
	}
	if !deleteCalled {
		t.Error("rollback (DeleteQemu) must run on checksum mismatch")
	}
}

func TestCreateVM_AgentChecksum_ExecError_FailOpen(t *testing.T) {
	t.Parallel()
	defer handlers.SetAgentChecksumTimings(50*time.Millisecond, time.Millisecond)()

	n := &healthNodes{
		pingFn: readyPingFn(),
		agentExecFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
			return nil, errors.New("guest agent not running")
		},
	}
	deleteCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}
	deps := buildHealthDeps(n, healthCfgWithSHA())

	args := mkArgs("agent-checksum-execerr", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512}, standardNetworks(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("checksum-execerr"))
	if err != nil {
		t.Fatalf("exec error must be fail-open (success), got: %v", err)
	}
	if deleteCalled {
		t.Error("rollback must NOT run on fail-open exec error")
	}
}

// execPidFn returns an agentExecFn that always reports a started PID.
func execPidFn(pid int64) func(context.Context, string, string, *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
	return func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuAgentExecParams) (*sdknodes.CreateQemuAgentExecResponse, error) {
		return &sdknodes.CreateQemuAgentExecResponse{Pid: pid}, nil
	}
}

// runChecksumFailOpenCase drives create_vm with the checksum assertion enabled
// and the given exec-status behavior, asserting the run succeeds (fail-open) and
// no rollback fires.
func runChecksumFailOpenCase(t *testing.T, name string, statusFn func(context.Context, string, string, *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error)) {
	t.Helper()
	defer handlers.SetAgentChecksumTimings(40*time.Millisecond, time.Millisecond)()

	n := &healthNodes{
		pingFn:            readyPingFn(),
		agentExecFn:       execPidFn(99),
		agentExecStatusFn: statusFn,
	}
	deleteCalled := false
	n.deleteQemuFn = func(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
		deleteCalled = true
		return &sdknodes.DeleteQemuResponse{}, nil
	}
	deps := buildHealthDeps(n, healthCfgWithSHA())

	args := mkArgs(name, testStemcellCID,
		map[string]any{"cores": 1, "memory": 512}, standardNetworks(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx(name))
	if err != nil {
		t.Fatalf("%s: must be fail-open (success), got: %v", name, err)
	}
	if deleteCalled {
		t.Errorf("%s: rollback must NOT run on fail-open", name)
	}
}

func TestCreateVM_AgentChecksum_NonZeroExit_FailOpen(t *testing.T) {
	t.Parallel()
	// sha256sum could not read the binary (exit 1) → cannot confirm a mismatch.
	runChecksumFailOpenCase(t, "agent-checksum-nonzero", execStatusReturning(testAgentSHA, 1))
}

func TestCreateVM_AgentChecksum_Unparseable_FailOpen(t *testing.T) {
	t.Parallel()
	runChecksumFailOpenCase(t, "agent-checksum-unparseable",
		func(_ context.Context, _, _ string, _ *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error) {
			garbage := "not-a-digest\n"
			ec := int64(0)
			return &sdknodes.ListQemuAgentExecStatusResponse{Exited: true, Exitcode: &ec, OutData: &garbage}, nil
		})
}

func TestCreateVM_AgentChecksum_NeverExits_TimeoutFailOpen(t *testing.T) {
	t.Parallel()
	// Command never reports exited → awaitAgentExec hits its bound and fails open.
	runChecksumFailOpenCase(t, "agent-checksum-neverexits",
		func(_ context.Context, _, _ string, _ *sdknodes.ListQemuAgentExecStatusParams) (*sdknodes.ListQemuAgentExecStatusResponse, error) {
			return &sdknodes.ListQemuAgentExecStatusResponse{Exited: false}, nil
		})
}

func TestCreateVM_AgentChecksum_EmptyExpected_NoExec(t *testing.T) {
	t.Parallel()

	n := &healthNodes{pingFn: readyPingFn()}
	// Health enabled, but no expected checksum → assertion skipped, no exec call.
	deps := buildHealthDeps(n, healthCheckCfg(true, 10, 0))

	args := mkArgs("agent-checksum-empty", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512}, standardNetworks(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("checksum-empty"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.execCalls != 0 {
		t.Errorf("no expected checksum: want 0 exec calls, got %d", n.execCalls)
	}
}
