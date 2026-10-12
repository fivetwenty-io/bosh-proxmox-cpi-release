// Tests for Deps.WithRequestOverrides and RequestOverrideRuntime — the
// per-request pve_* context config override mechanism that fixes BOSH
// cpi-config multi-CPI requests silently targeting the wrong PVE cluster
// (see context_override.go).
package handlers_test

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// overrideTestEntryToken is the placeholder api_token test entries carry
// whenever they change pve_host, because an entry on another host never
// inherits the job-level credentials and must name its own.
const overrideTestEntryToken = "fake-entry@pve!cpi=FAKE-ENTRY-TOKEN-PLACEHOLDER"

// overrideTestBaseConfig returns a minimal, fully valid job-level CPIConfig
// for WithRequestOverrides tests. agent_mode "noagent" keeps bundle
// construction (RequestOverrideRuntime.resolve) free of ConfigDrive/
// ISO-storage requirements so these tests exercise only the override/client
// routing mechanism itself.
func overrideTestBaseConfig() *config.CPIConfig {
	verifySSL := true
	return &config.CPIConfig{
		Host:           "job-cluster.example",
		Port:           8006,
		User:           "root",
		Password:       "job-password",
		VMStorage:      "local",
		DiskStorage:    "local",
		NetworkBridge:  "vmbr0",
		VerifySSL:      &verifySSL,
		AgentMode:      "noagent",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
		VMIDRangeEnd:   5999,
		RebootMode:     "soft",
		RebootTimeout:  60,
		NetworkMode:    "auto",
		SDNZoneType:    "simple",
	}
}

// fakeTransportClientFactory is a RequestOverrideRuntime.ClientFactory fake
// that records, in call order, every cfg.Host it was invoked with, and
// returns one distinct *mockPVEClient per distinct host — so a test can
// assert BOTH which host a given request's override resolved to (by
// identity of the returned client) AND that RequestOverrideRuntime's cache
// avoids rebuilding a client for a host already seen.
type fakeTransportClientFactory struct {
	mu     sync.Mutex
	calls  []string // cfg.Host, one entry per ACTUAL factory invocation (cache misses only)
	byHost map[string]*mockPVEClient
	// failHost, when non-empty, makes Factory return an error for that one
	// host, simulating an unreachable/misconfigured overridden cluster.
	failHost string
}

func (f *fakeTransportClientFactory) Factory(cfg *config.CPIConfig, _ *log.Logger) (pve.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cfg.Host)
	if cfg.Host == f.failHost {
		return nil, errors.New("simulated: cannot reach PVE host " + cfg.Host)
	}
	if f.byHost == nil {
		f.byHost = make(map[string]*mockPVEClient)
	}
	if c, ok := f.byHost[cfg.Host]; ok {
		return c, nil
	}
	c := &mockPVEClient{}
	f.byHost[cfg.Host] = c
	return c, nil
}

// -----------------------------------------------------------------------
// No-op paths
// -----------------------------------------------------------------------

func TestWithRequestOverrides_NoExtra_UsesJobClientUnchanged(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    jobClient,
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{RequestID: "r1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PVE != jobClient {
		t.Error("a request with no context overrides must keep using the job-level PVE client")
	}
	if got.Config != base {
		t.Error("a request with no context overrides must keep using the job-level Config pointer")
	}
	if len(tracker.calls) != 0 {
		t.Errorf("ClientFactory must not be invoked at all when no overrides are present, got %d call(s)", len(tracker.calls))
	}
}

func TestWithRequestOverrides_NilOverridesRuntime_Disabled(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	// Overrides left nil — every existing handler-unit-test Deps literal in
	// this codebase does exactly this, so pve_* context keys must be inert.
	deps := handlers.Deps{Config: base, PVE: jobClient, Logger: log.NewNopLogger()}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "should-be-ignored.example"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PVE != jobClient || got.Config != base {
		t.Error("a Deps with Overrides==nil must ignore pve_* context keys entirely")
	}
}

// -----------------------------------------------------------------------
// Dispatcher/integration-style: sequential requests with different pve_host
// context values construct/use different clients; identical overrides reuse
// the cached client.
// -----------------------------------------------------------------------

func TestWithRequestOverrides_DifferentHosts_GetDifferentClients(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    jobClient,
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}
	ctx := context.Background()

	// Request 1: no override — must use the job client (sequential requests
	// without overrides are unaffected by any of the below).
	plain, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{RequestID: "req-plain"})
	if err != nil {
		t.Fatalf("plain request: unexpected error: %v", err)
	}
	if plain.PVE != jobClient {
		t.Error("plain request must target the job client")
	}

	// Request 2: overrides to cluster A.
	depsA, errA := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-a",
		Extra:     map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az1.example"},
	})
	if errA != nil {
		t.Fatalf("cluster A request: unexpected error: %v", errA)
	}
	if depsA.Config.Host != "az1.example" {
		t.Errorf("depsA.Config.Host = %q, want az1.example", depsA.Config.Host)
	}
	if depsA.PVE == jobClient {
		t.Error("cluster A request must NOT use the job-level PVE client")
	}

	// Request 3: overrides to cluster B (a different host).
	depsB, errB := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-b",
		Extra:     map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az2.example"},
	})
	if errB != nil {
		t.Fatalf("cluster B request: unexpected error: %v", errB)
	}
	if depsB.Config.Host != "az2.example" {
		t.Errorf("depsB.Config.Host = %q, want az2.example", depsB.Config.Host)
	}
	if depsB.PVE == depsA.PVE {
		t.Fatal("requests overriding to two DIFFERENT pve_host values must get two DIFFERENT PVE clients — " +
			"this is the exact live defect: a second cpi-config entry executing against the first entry's cluster")
	}
	if depsB.PVE == jobClient || depsA.PVE == jobClient {
		t.Error("neither overridden request may reuse the job-level PVE client")
	}

	// Request 4: overrides to cluster A again — must reuse depsA's cached client.
	depsA2, errA2 := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-a-again",
		Extra:     map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az1.example"},
	})
	if errA2 != nil {
		t.Fatalf("cluster A repeat request: unexpected error: %v", errA2)
	}
	if depsA2.PVE != depsA.PVE {
		t.Error("a second request overriding to the SAME pve_host must reuse the cached PVE client, not rebuild one")
	}

	// The ORIGINAL deps value (and the job cfg it was built from) must never
	// have been mutated by any overridden request above.
	if deps.PVE != jobClient {
		t.Error("the original Deps.PVE must remain the job client after overridden requests ran")
	}
	if base.Host != "job-cluster.example" {
		t.Errorf("base.Host mutated to %q, want unchanged \"job-cluster.example\"", base.Host)
	}

	// Exactly two ACTUAL client builds: az1.example and az2.example. The
	// repeat az1 request must be a cache hit (no third factory call).
	if got, want := len(tracker.calls), 2; got != want {
		t.Errorf("ClientFactory invoked %d time(s), want %d (one per distinct host; repeat host is a cache hit): calls=%v",
			got, want, tracker.calls)
	}
}

// -----------------------------------------------------------------------
// Error paths
// -----------------------------------------------------------------------

func TestWithRequestOverrides_MalformedOverride_NonRetriableCloudError(t *testing.T) {
	t.Parallel()
	deps := handlers.Deps{
		Config: overrideTestBaseConfig(),
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: (&fakeTransportClientFactory{}).Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_port": "not-a-number"},
	})
	if err == nil {
		t.Fatal("expected an error for a malformed pve_port override, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("error %v is not a *cpierrors.Error", err)
	}
	if cpiErr.OkToRetry() {
		t.Error("a malformed override is a manifest/authoring bug — must be non-retriable, got OkToRetry()=true")
	}
}

func TestWithRequestOverrides_ClientBuildFailure_RetriableCloudError(t *testing.T) {
	t.Parallel()
	tracker := &fakeTransportClientFactory{failHost: "unreachable.example"}
	deps := handlers.Deps{
		Config: overrideTestBaseConfig(),
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "unreachable.example"},
	})
	if err == nil {
		t.Fatal("expected an error when the overridden cluster's client cannot be built, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("error %v is not a *cpierrors.Error", err)
	}
	if !cpiErr.OkToRetry() {
		t.Error("a transient build failure against the overridden cluster should be retriable, got OkToRetry()=false")
	}
}

// TestWithRequestOverrides_OnlyUnknownPVEKeys_HardError covers M2 (A13
// review): a request whose context carries pve_* keys but NONE of them are
// supported overrides must be rejected, not silently fall through to the
// job-level cluster. This is the exact shape a systematic misspelling, a
// dropped/renamed key prefix, or a cpi-config templating bug would produce —
// and silently continuing on the job-level cluster here is indistinguishable
// from LF16's "succeeds while landing on the wrong cluster" defect class.
func TestWithRequestOverrides_OnlyUnknownPVEKeys_HardError(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    jobClient,
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	// pve_placement_enabled is a real pve.* property but deliberately not
	// part of the per-request override set — here it is the ONLY pve_* key
	// present, so zero overrides apply.
	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_placement_enabled": true},
	})
	if err == nil {
		t.Fatal("expected an error when every pve_* key in the request is unsupported, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("error %v is not a *cpierrors.Error", err)
	}
	if cpiErr.OkToRetry() {
		t.Error("an all-unknown pve_* context is a manifest/cpi-config authoring bug — must be non-retriable, got OkToRetry()=true")
	}
	if len(tracker.calls) != 0 {
		t.Errorf("ClientFactory must not be invoked when the request is rejected before resolve(), got %d call(s)", len(tracker.calls))
	}
}

// TestWithRequestOverrides_MixedKnownAndUnknownPVEKeys_WarnsAndApplies is the
// mixed-case companion to the hard-error test above: when AT LEAST ONE
// pve_* key applies, the request demonstrably reached the cluster its
// recognized overrides named, so unsupported keys alongside it stay
// Warn-only by design (a cpi-config properties block routinely carries the
// FULL pve.* property set for an entry, most of which is intentionally not
// per-request overridable).
func TestWithRequestOverrides_MixedKnownAndUnknownPVEKeys_WarnsAndApplies(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    jobClient,
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{
			"pve_host":              "az1.example", // supported — applies
			"pve_api_token":         overrideTestEntryToken,
			"pve_placement_enabled": true, // unsupported — Warn only
		},
	})
	if err != nil {
		t.Fatalf("a mix of applied and unsupported pve_* keys must never fail the request, got error: %v", err)
	}
	if got.Config.Host != "az1.example" {
		t.Errorf("got.Config.Host = %q, want az1.example (the applied override)", got.Config.Host)
	}
	if got.PVE == jobClient {
		t.Error("the applied override must still route to a non-job-level PVE client")
	}
	if len(tracker.calls) != 1 {
		t.Errorf("ClientFactory should be invoked exactly once, got %d call(s)", len(tracker.calls))
	}
}

// -----------------------------------------------------------------------
// S8 follow-up — pve.reject_tls_downgrade_overrides
// -----------------------------------------------------------------------

// TestWithRequestOverrides_RejectTLSDowngradeOverrides_GenuineDowngrade_Rejected
// covers the knob's core case: a job-level config that itself verifies TLS
// (VerifySSL=true), with reject_tls_downgrade_overrides enabled, must refuse
// a request whose context applies pve_verify_ssl=false — and must do so
// BEFORE building/caching a PVE client bundle for the downgraded config.
func TestWithRequestOverrides_RejectTLSDowngradeOverrides_GenuineDowngrade_Rejected(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	base.RejectTLSDowngradeOverrides = boolPtr(true)
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_verify_ssl": false},
	})
	if err == nil {
		t.Fatal("expected an error when reject_tls_downgrade_overrides is enabled and a request downgrades TLS verification, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("error %v is not a *cpierrors.Error", err)
	}
	if cpiErr.OkToRetry() {
		t.Error("a rejected TLS downgrade is a manifest/cpi-config authoring or policy decision — must be non-retriable, got OkToRetry()=true")
	}
	if !strings.Contains(err.Error(), "reject_tls_downgrade_overrides") {
		t.Errorf("error message %q must name the knob (reject_tls_downgrade_overrides)", err.Error())
	}
	if !strings.Contains(err.Error(), "pve_verify_ssl") {
		t.Errorf("error message %q must name the offending override key (pve_verify_ssl)", err.Error())
	}
	if len(tracker.calls) != 0 {
		t.Errorf("ClientFactory must not be invoked when the downgrade is rejected before resolve(), got %d call(s)", len(tracker.calls))
	}
}

// TestWithRequestOverrides_RejectTLSDowngradeOverrides_BaseAlreadyFalse_NotRejected
// covers the "not a downgrade" carve-out: when the job-level config already
// has verify_ssl=false, an override that also sets pve_verify_ssl=false does
// not change the effective TLS posture, so the knob must NOT reject it —
// the request proceeds exactly as it did before this knob existed.
func TestWithRequestOverrides_RejectTLSDowngradeOverrides_BaseAlreadyFalse_NotRejected(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	base.VerifySSL = boolPtr(false)
	base.RejectTLSDowngradeOverrides = boolPtr(true)
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_verify_ssl": false, "pve_api_token": overrideTestEntryToken, "pve_host": "az1.example"},
	})
	if err != nil {
		t.Fatalf("a request that does not actually downgrade TLS verification (base already verify_ssl=false) must not be rejected, got error: %v", err)
	}
	if got.Config.VerifySSLValue() {
		t.Error("effective config VerifySSLValue() = true, want false")
	}
	if len(tracker.calls) != 1 {
		t.Errorf("ClientFactory should be invoked exactly once, got %d call(s)", len(tracker.calls))
	}
}

// TestWithRequestOverrides_TLSDowngrade_KnobOff_WarnOnlyUnchanged confirms
// the knob's off-by-default contract: with reject_tls_downgrade_overrides
// unset, a genuine TLS-verification downgrade still proceeds (byte-identical
// to every release before this knob existed) and is still logged at Warn.
func TestWithRequestOverrides_TLSDowngrade_KnobOff_WarnOnlyUnchanged(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig() // RejectTLSDowngradeOverrides left nil (off)
	tracker := &fakeTransportClientFactory{}
	observedLogger, obs := log.NewObservedLogger(log.LevelDebug)
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: observedLogger,
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_verify_ssl": false, "pve_api_token": overrideTestEntryToken, "pve_host": "az1.example"},
	})
	if err != nil {
		t.Fatalf("knob-off TLS downgrade must proceed (warn-only), got error: %v", err)
	}
	if got.Config.VerifySSLValue() {
		t.Error("effective config VerifySSLValue() = true, want false")
	}
	if len(tracker.calls) != 1 {
		t.Errorf("ClientFactory should be invoked exactly once, got %d call(s)", len(tracker.calls))
	}

	found := false
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "disables TLS verification") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a Warn log entry about the TLS-verification downgrade, found none")
	}
}

// -----------------------------------------------------------------------
// M3 (A13 review) — bounded bundle cache
// -----------------------------------------------------------------------

// freshClientFactory is a RequestOverrideRuntime.ClientFactory fake that
// returns a DISTINCT *mockPVEClient instance on every invocation, regardless
// of cfg.Host — unlike fakeTransportClientFactory (which memoizes one client
// per host, modeling "the runtime's own cache is working"). Eviction tests
// need a factory that does NOT memoize by host, so a rebuild after eviction
// is distinguishable by pointer identity from the client that was evicted —
// fakeTransportClientFactory's memoization would otherwise mask a broken
// eviction path by transparently handing back the same instance regardless
// of how many times the RequestOverrideRuntime cache actually rebuilt it.
type freshClientFactory struct {
	mu    sync.Mutex
	calls []string // cfg.Host, one entry per invocation
}

func (f *freshClientFactory) Factory(cfg *config.CPIConfig, _ *log.Logger) (pve.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cfg.Host)
	return &mockPVEClient{}, nil
}

// TestWithRequestOverrides_BundleCache_EvictsLeastRecentlyUsed covers M3:
// the override bundle cache must not grow without bound, and eviction must
// follow least-recently-used order (not, say, insertion order or random).
// With MaxCachedBundles set to 2:
//  1. override(A), override(B) — cache holds {A,B}, B most recently used.
//  2. Re-request A (a cache HIT — no rebuild) to make A the most recently
//     used, leaving B as the sole least-recently-used entry.
//  3. override(C) — cache is at capacity; B (now LRU) must be evicted, A
//     and C remain cached.
//  4. Re-requesting B must therefore rebuild (a fresh ClientFactory call
//     yielding a NEW client instance), while re-requesting A must still be
//     a cache hit (no new call, same instance as step 2).
func TestWithRequestOverrides_BundleCache_EvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	tracker := &freshClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory:    tracker.Factory,
			Logger:           log.NewNopLogger(),
			MaxCachedBundles: 2,
		},
	}
	ctx := context.Background()

	override := func(host string) handlers.Deps {
		t.Helper()
		d, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{
			"pve_api_token": overrideTestEntryToken, "pve_host": host,
		}})
		if err != nil {
			t.Fatalf("override to %s: unexpected error: %v", host, err)
		}
		return d
	}
	callCount := func() int {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return len(tracker.calls)
	}

	depsA1 := override("az-a.example") // build #1 — cache: {A}
	depsB1 := override("az-b.example") // build #2 — cache: {B(MRU),A}

	// Touch A again: cache hit, no rebuild, and A becomes most recently used.
	depsA2 := override("az-a.example")
	if got, want := callCount(), 2; got != want {
		t.Fatalf("re-requesting az-a.example (cache hit expected): ClientFactory calls = %d, want %d", got, want)
	}
	if depsA2.PVE != depsA1.PVE {
		t.Error("re-requesting a still-cached host must return the SAME client instance (cache hit), got a different one")
	}

	// Insert C: cache at capacity (2) with A now most-recently-used and B
	// least-recently-used — B must be the one evicted.
	depsC1 := override("az-c.example") // build #3 — cache: {C(MRU),A}
	if got, want := callCount(), 3; got != want {
		t.Fatalf("after inserting az-c.example at capacity: ClientFactory calls = %d, want %d", got, want)
	}

	// B was evicted: re-requesting it must rebuild (a NEW client instance).
	depsB2 := override("az-b.example")
	if got, want := callCount(), 4; got != want {
		t.Errorf("re-requesting evicted host az-b.example: ClientFactory calls = %d, want %d (a cache hit would keep this at 3)", got, want)
	}
	if depsB2.PVE == depsB1.PVE {
		t.Error("the rebuilt client for the evicted host az-b.example must be a NEW instance, not the original (which was evicted, not reused)")
	}

	// C must still be a cache hit: it was inserted more recently than A (the
	// LRU victim of B's own rebuild-insert above — cache: {C,A} before this
	// call, and inserting B at capacity evicts the back element, A).
	depsC2 := override("az-c.example")
	if got, want := callCount(), 4; got != want {
		t.Errorf("re-requesting az-c.example (should still be cached): ClientFactory calls = %d, want %d (a rebuild means it was wrongly evicted)", got, want)
	}
	if depsC2.PVE != depsC1.PVE {
		t.Error("az-c.example should still be the SAME cached client instance — it must not have been evicted")
	}
}

// TestWithRequestOverrides_BundleCache_DefaultCapApplied confirms a
// RequestOverrideRuntime that never sets MaxCachedBundles still bounds the
// cache (via the package default) rather than growing without limit — this
// is the exact "Deps literal never sets this field explicitly" shape
// production wiring and every other test in this file uses.
func TestWithRequestOverrides_BundleCache_DefaultCapApplied(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
			// MaxCachedBundles intentionally left unset (0).
		},
	}
	ctx := context.Background()

	// One more than the documented default cap (16) — the exact count
	// doesn't matter here, only that SOME bound is enforced rather than the
	// cache growing to match every distinct host seen.
	const distinctHosts = 20
	for i := 0; i < distinctHosts; i++ {
		host := fmt.Sprintf("az-%02d.example", i)
		if _, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{
			"pve_api_token": overrideTestEntryToken, "pve_host": host,
		}}); err != nil {
			t.Fatalf("override to %s: unexpected error: %v", host, err)
		}
	}
	// Re-request the FIRST host again; if the default cap were absent (an
	// unbounded map, the pre-M3 behavior), this would be a cache hit
	// (calls stays at distinctHosts). With a default cap smaller than
	// distinctHosts, the first host was evicted long ago and this forces a
	// rebuild (calls increases).
	callsBefore := len(tracker.calls)
	if callsBefore != distinctHosts {
		t.Fatalf("sanity check: expected %d factory calls after %d distinct hosts, got %d", distinctHosts, distinctHosts, callsBefore)
	}
	if _, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{
		"pve_api_token": overrideTestEntryToken, "pve_host": "az-00.example",
	}}); err != nil {
		t.Fatalf("re-request az-00.example: unexpected error: %v", err)
	}
	if got := len(tracker.calls); got != callsBefore+1 {
		t.Errorf("re-requesting the first-seen host after %d other distinct hosts: ClientFactory calls = %d, want %d "+
			"(the default cache cap must have evicted it; an unbounded cache would make this a hit)",
			distinctHosts, got, callsBefore+1)
	}
}

// -----------------------------------------------------------------------
// M4 (A13 review) — genuinely concurrent resolve()
// -----------------------------------------------------------------------

// TestWithRequestOverrides_Concurrent_SameIdentity_SingleBuild fires N
// goroutines through WithRequestOverrides with the SAME override identity
// (same pve_host, and therefore the same effective-config cache key) and
// asserts the ClientFactory is invoked exactly once and every caller
// receives the identical client instance — exercising the
// build-outside-lock / re-check-under-lock / keep-first-and-discard-
// redundant path under REAL concurrency (run with -race), not just by
// code inspection.
func TestWithRequestOverrides_Concurrent_SameIdentity_SingleBuild(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()

	// blockUntil gates every Factory call so all N goroutines are guaranteed
	// to be racing resolve() concurrently rather than serializing through by
	// chance — each call blocks until released, maximizing the window in
	// which the double-build race (the thing this test exists to exercise)
	// could occur if the cache logic were broken.
	release := make(chan struct{})
	tracker := &fakeTransportClientFactory{}
	blockingFactory := func(cfg *config.CPIConfig, logger *log.Logger) (pve.Client, error) {
		<-release
		return tracker.Factory(cfg, logger)
	}

	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: blockingFactory,
			Logger:        log.NewNopLogger(),
		},
	}

	const n = 32
	var wg sync.WaitGroup
	results := make([]pve.Client, n)
	errs := make([]error, n)
	var startWG sync.WaitGroup
	startWG.Add(n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			startWG.Done()
			startWG.Wait() // best-effort: line every goroutine up before any resolve() call proceeds
			d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
				Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "shared-az.example"},
			})
			errs[i] = err
			if err == nil {
				results[i] = d.PVE
			}
		}()
	}
	// Release all blocked factory calls once every goroutine has had a
	// chance to reach (and block inside) resolve()'s unlocked build section.
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	first := results[0]
	if first == nil {
		t.Fatal("goroutine 0: got nil PVE client")
	}
	for i, r := range results {
		if r != first {
			t.Errorf("goroutine %d got a different client instance than goroutine 0 — the cache did not converge on a single build", i)
		}
	}
	if got, want := len(tracker.calls), 1; got != want {
		t.Errorf("ClientFactory invoked %d time(s) for %d concurrent requests sharing one override identity, want exactly %d", got, n, want)
	}
}

// TestWithRequestOverrides_Concurrent_MixedIdentities fires N goroutines
// through WithRequestOverrides spread across several DISTINCT override
// identities concurrently (run with -race) and asserts: every goroutine
// targeting the same host converges on the same client instance, distinct
// hosts get distinct client instances, and the factory is invoked exactly
// once per distinct host (never more, regardless of how many concurrent
// requests raced to build it first).
func TestWithRequestOverrides_Concurrent_MixedIdentities(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	hosts := []string{"az1.example", "az2.example", "az3.example", "az4.example"}
	const perHost = 16
	total := len(hosts) * perHost

	var wg sync.WaitGroup
	var mu sync.Mutex
	byHost := make(map[string]map[pve.Client]struct{})
	wg.Add(total)
	for _, host := range hosts {
		for i := 0; i < perHost; i++ {
			host := host
			go func() {
				defer wg.Done()
				d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
					Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": host},
				})
				if err != nil {
					t.Errorf("host %s: unexpected error: %v", host, err)
					return
				}
				mu.Lock()
				if byHost[host] == nil {
					byHost[host] = make(map[pve.Client]struct{})
				}
				byHost[host][d.PVE] = struct{}{}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	if len(byHost) != len(hosts) {
		t.Fatalf("saw results for %d distinct hosts, want %d", len(byHost), len(hosts))
	}
	seenClients := make(map[pve.Client]string)
	for host, clients := range byHost {
		if len(clients) != 1 {
			t.Errorf("host %s: %d distinct client instances observed across %d concurrent requests, want exactly 1",
				host, len(clients), perHost)
		}
		for c := range clients {
			if otherHost, ok := seenClients[c]; ok {
				t.Errorf("client instance shared between host %s and host %s — distinct override identities must never share a client", host, otherHost)
			}
			seenClients[c] = host
		}
	}
	if got, want := len(tracker.calls), len(hosts); got != want {
		t.Errorf("ClientFactory invoked %d time(s) across %d distinct hosts (%d concurrent requests each), want exactly %d (one per host)",
			got, len(hosts), perHost, want)
	}
}

// -----------------------------------------------------------------------
// F1: entry-level placement reaches AZ resolution
// -----------------------------------------------------------------------

// TestWithRequestOverrides_EntryPlacementAZMap_ResolvesEntryAZ reproduces the
// exact V7 live failure shape end to end, one layer above
// config.ApplyContextOverrides: a cpi-config entry whose properties define
// placement.az_map with "z3" is dispatched against a job-level config whose
// az_map does NOT know z3.
//
// Before placement was overridable, the director's Warn named pve_placement as
// unsupported, the request fell back to the job-level (empty) az_map, and
// create_vm hard-failed with
//
//	create_vm: availability_zone "z3" is not defined in placement.az_map
//
// even though the dispatched entry plainly defined it. This asserts the AZ now
// resolves from the ENTRY, that the job-level config is untouched, and that
// pve_placement no longer appears in the unsupported-key list.
func TestWithRequestOverrides_EntryPlacementAZMap_ResolvesEntryAZ(t *testing.T) {
	t.Parallel()

	base := overrideTestBaseConfig()
	// The job-level cluster knows only z1 — z3 belongs to the other cluster.
	base.Placement = &config.PlacementConfig{
		AZMap: map[string][]string{"z1": {"job-n1", "job-n2", "job-n3"}},
	}
	base.ApplyDefaults()

	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	// The nested shape a director actually merges from a cpi-config entry's
	// `properties:` hash.
	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		RequestID: "r-az2",
		Extra: map[string]any{
			"pve": map[string]any{
				"host":      "az2-cluster.example",
				"api_token": overrideTestEntryToken,
				"placement": map[string]any{
					"az_map": map[string]any{
						"z3": []any{"pve-az2-1", "pve-az2-2", "pve-az2-3"},
					},
					"pin_az_via_ha_rules": true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The AZ the entry defines now resolves, against the entry's nodes.
	nodes, ok := got.Config.AZCandidates("z3")
	if !ok {
		t.Fatal(`AZ "z3" did not resolve from the dispatched entry's placement.az_map — this is the V7 create_vm failure`)
	}
	if len(nodes) != 3 || nodes[0] != "pve-az2-1" {
		t.Errorf(`AZ "z3" resolved to %#v, want the entry's node list`, nodes)
	}
	if !got.Config.HANodeAffinityPinEnabled() {
		t.Error("the entry's pin_az_via_ha_rules must be in effect for this request")
	}

	// The job-level cluster's own AZ is NOT inherited: an entry that defines
	// placement defines it completely.
	if _, leaked := got.Config.AZCandidates("z1"); leaked {
		t.Error(`job-level AZ "z1" must not survive into an entry that defines its own placement block`)
	}

	// The job-level config is untouched for every other request.
	if _, gone := base.Placement.AZMap["z1"]; !gone {
		t.Error("job-level az_map lost its own AZ — base must never be mutated")
	}
	if _, leaked := base.Placement.AZMap["z3"]; leaked {
		t.Error("the entry's AZ leaked into the job-level az_map")
	}
}

// TestWithRequestOverrides_PlacementNoLongerUnsupported pins the other half of
// the V7 evidence: the director's context carried pve_placement and the CPI
// logged it under unsupported_keys. A context carrying ONLY pve_placement must
// now apply cleanly rather than being rejected as an all-unsupported request.
func TestWithRequestOverrides_PlacementNoLongerUnsupported(t *testing.T) {
	t.Parallel()

	base := overrideTestBaseConfig()
	base.ApplyDefaults()
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		RequestID: "r-placement-only",
		Extra: map[string]any{
			"pve_placement": map[string]any{
				"az_map": map[string]any{"z3": []any{"pve-az2-1"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("a context carrying only pve_placement must not be rejected as all-unsupported: %v", err)
	}
	if _, ok := got.Config.AZCandidates("z3"); !ok {
		t.Error("pve_placement was accepted but did not take effect")
	}
}

// TestWithRequestOverrides_EntryPlacementInvalid_FailsThatRequest proves an
// entry with a bad az_map fails its own requests loudly, matching what
// validation does to a bad job-level config at boot, rather than silently
// falling back to the job-level placement.
func TestWithRequestOverrides_EntryPlacementInvalid_FailsThatRequest(t *testing.T) {
	t.Parallel()

	base := overrideTestBaseConfig()
	base.Placement = &config.PlacementConfig{AZMap: map[string][]string{"z1": {"job-n1"}}}
	base.ApplyDefaults()

	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}

	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		RequestID: "r-bad",
		Extra: map[string]any{
			"pve": map[string]any{
				"placement": map[string]any{
					"az_map": map[string]any{"z3": []any{}}, // no nodes
				},
			},
		},
	})
	if err == nil {
		t.Fatal("an entry with an invalid az_map must fail its own requests, not fall back to the job-level placement")
	}
	// The rejection must come from VALIDATING the block, not from the
	// all-keys-unsupported path — otherwise this test would still pass with
	// placement unregistered, which is the state it exists to rule out.
	if !strings.Contains(err.Error(), "az_map") {
		t.Errorf("error must name the offending az_map (validation rejection), got: %v", err)
	}
	if strings.Contains(err.Error(), "none are supported") {
		t.Errorf("placement was rejected as an unsupported key rather than validated: %v", err)
	}
	if len(tracker.calls) != 0 {
		t.Errorf("no PVE client should be built for a rejected override, got %d call(s)", len(tracker.calls))
	}
}

// -----------------------------------------------------------------------
// node_endpoints scoping: the job-level explicit map names nodes of the
// job-level cluster, so an override bundle inherits it only when the
// override still targets the job-level host.
// -----------------------------------------------------------------------

func TestWithRequestOverrides_NodeEndpointsFollowBaseHost(t *testing.T) {
	t.Parallel()
	jobClient := &mockPVEClient{}
	base := overrideTestBaseConfig()
	base.NodeEndpoints = map[string]string{"pve2": "pve2-direct.example"}
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    jobClient,
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
			BaseHost:      base.Host,
			BasePort:      base.Port,
		},
	}
	ctx := context.Background()

	// An override that keeps the job-level host inherits the explicit map.
	sameHost, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-same-host",
		Extra:     map[string]any{"pve_node": "pve9"},
	})
	if err != nil {
		t.Fatalf("same-host override: unexpected error: %v", err)
	}
	if h, ok := sameHost.NodeEndpoints.HostFor(ctx, "pve2"); !ok || h != "pve2-direct.example" {
		t.Errorf("same-host override HostFor(pve2) = %q, %v; want the job-level entry pve2-direct.example", h, ok)
	}

	// An override targeting a different cluster must drop the map: its node
	// names belong to the job-level cluster and would misroute uploads.
	// Discovery stays gated off here (verify_ssl is true in the base config).
	otherHost, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-other-host",
		Extra:     map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az2.example"},
	})
	if err != nil {
		t.Fatalf("other-host override: unexpected error: %v", err)
	}
	if h, ok := otherHost.NodeEndpoints.HostFor(ctx, "pve2"); ok {
		t.Errorf("other-host override HostFor(pve2) = %q; want no override (job-level map must not leak across clusters)", h)
	}

	// The endpoint is host and port, so a second cluster behind the job host
	// at another port (one tunnel or load balancer in front of two clusters)
	// must drop the map too.
	otherPort, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{
		RequestID: "req-other-port",
		Extra:     map[string]any{"pve_api_token": overrideTestEntryToken, "pve_port": float64(8443)},
	})
	if err != nil {
		t.Fatalf("other-port override: unexpected error: %v", err)
	}
	if h, ok := otherPort.NodeEndpoints.HostFor(ctx, "pve2"); ok {
		t.Errorf("other-port override HostFor(pve2) = %q; want no override (same host, different endpoint)", h)
	}
}

// TestWithRequestOverrides_NodeEndpoints_UnsetBasePortIsDefault asserts that
// a runtime whose BasePort is zero treats the job endpoint as port 8006, so a
// same-host override at 8006 keeps the job's node_endpoints map.
func TestWithRequestOverrides_NodeEndpoints_UnsetBasePortIsDefault(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	base.NodeEndpoints = map[string]string{"pve2": "pve2-direct.example"}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: (&fakeTransportClientFactory{}).Factory,
			Logger:        log.NewNopLogger(),
			BaseHost:      base.Host,
		},
	}
	ctx := context.Background()
	d, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{"pve_node": "pve9", "pve_port": float64(8006)}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h, ok := d.NodeEndpoints.HostFor(ctx, "pve2"); !ok || h != "pve2-direct.example" {
		t.Errorf("HostFor(pve2) = %q, %v; want the job-level entry, because port 8006 is the job endpoint", h, ok)
	}
}

// -----------------------------------------------------------------------
// Credentials travel with the endpoint: wire-level proof
// -----------------------------------------------------------------------

// Wire-test credential placeholders. Each is distinct, so the recorded
// traffic shows exactly which group reached which server.
const (
	wireJobUser       = "fake-job-user"
	wireJobRealm      = "fakejobrealm"
	wireJobTokenValue = "FAKE-JOB-TOKEN-PLACEHOLDER"
	wireJobToken      = wireJobUser + "@" + wireJobRealm + "!cpi=" + wireJobTokenValue
	wireJobPassword   = "FAKE-JOB-PASSWORD-PLACEHOLDER"
	wireEntryUser     = "fake-entry-user"
	wireEntryRealm    = "pve"
	wireEntryToken    = wireEntryUser + "@" + wireEntryRealm + "!cpi=FAKE-ENTRY-TOKEN-PLACEHOLDER"
	wireEntryPassword = "FAKE-ENTRY-PASSWORD-PLACEHOLDER"
)

// credentialRecordingServer is a TLS PVE stub that records every credential-
// bearing part of every request it receives: the Authorization header, the
// cookie, and the request body, which carries the username and password of a
// ticket login.
type credentialRecordingServer struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []string
}

func newCredentialRecordingServer(t *testing.T) *credentialRecordingServer {
	t.Helper()
	s := &credentialRecordingServer{}
	s.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Record the form-decoded body too, so an escaped "@" or "!" in a login
		// cannot hide a credential from the searches below.
		decoded, _ := url.QueryUnescape(string(body))
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+
			" cookie="+r.Header.Get("Cookie")+" body="+string(body)+" decoded="+decoded)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api2/json/access/ticket" {
			_, _ = w.Write([]byte(`{"data":{"ticket":"PVE:fake-ticket-placeholder","CSRFPreventionToken":"fake-csrf-placeholder","username":"fake"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(s.server.Close)
	return s
}

// traffic returns everything the server has recorded, joined for searching.
func (s *credentialRecordingServer) traffic() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, "\n")
}

// requests returns how many requests the server has received.
func (s *credentialRecordingServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// endpoint returns the server's host and port.
func (s *credentialRecordingServer) endpoint(t *testing.T) (string, int) {
	t.Helper()
	u, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse stub port: %v", err)
	}
	return u.Hostname(), port
}

// caPEM returns the server's certificate as a PEM trust root.
func (s *credentialRecordingServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.server.Certificate().Raw}))
}

// assertEntryCredentialsOnly asserts that an overridden request reached only
// cluster B, that B saw the entry's own identity, and that none of the job's
// credential parts appears anywhere in B's recorded traffic.
func assertEntryCredentialsOnly(t *testing.T, clusterA, clusterB *credentialRecordingServer, jobLeaks []string) {
	t.Helper()
	if clusterB.requests() == 0 {
		t.Fatal("cluster B received no request, so the entry did not route there")
	}
	traffic := clusterB.traffic()
	for _, leak := range jobLeaks {
		if strings.Contains(traffic, leak) {
			t.Errorf("cluster B received a job-level credential part (the fixture value for one of user, realm, token, or password)")
		}
	}
	if !strings.Contains(traffic, wireEntryUser+"@"+wireEntryRealm) {
		t.Error("cluster B did not receive the entry's own identity")
	}
	if clusterA.requests() != 0 {
		t.Error("an overridden request reached cluster A")
	}
}

// TestWithRequestOverrides_SecondClusterNeverReceivesJobCredentials stands up
// two PVE stubs, A for the job-level cluster and B for a cpi-config entry, and
// drives real PVE clients at both through WithRequestOverrides. Cluster B must
// never receive the job's token, the job's password, the job's user, or the
// job's realm, whichever way the entry authenticates, and an entry that names
// no CA must not trust B through the job's private CA.
func TestWithRequestOverrides_SecondClusterNeverReceivesJobCredentials(t *testing.T) {
	t.Parallel()

	jobLeaks := []string{wireJobTokenValue, wireJobPassword, wireJobUser, wireJobRealm}

	setup := func(t *testing.T) (handlers.Deps, *credentialRecordingServer, *credentialRecordingServer) {
		t.Helper()
		clusterA := newCredentialRecordingServer(t)
		clusterB := newCredentialRecordingServer(t)
		hostA, portA := clusterA.endpoint(t)

		base := overrideTestBaseConfig()
		base.Host, base.Port = hostA, portA
		base.User, base.Realm = wireJobUser, wireJobRealm
		base.Password, base.APIToken = "", wireJobToken
		base.PVECACertPEM = clusterA.caPEM()

		factory := func(cfg *config.CPIConfig, l *log.Logger) (pve.Client, error) {
			return pve.NewClientWithTracer(cfg, l, nil)
		}
		jobClient, err := factory(base, log.NewNopLogger())
		if err != nil {
			t.Fatalf("build job client: %v", err)
		}
		deps := handlers.Deps{
			Config: base,
			PVE:    jobClient,
			Logger: log.NewNopLogger(),
			Overrides: &handlers.RequestOverrideRuntime{
				ClientFactory: factory,
				Logger:        log.NewNopLogger(),
				BaseHost:      base.Host,
			},
		}
		return deps, clusterA, clusterB
	}

	probe := func(t *testing.T, d handlers.Deps) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := d.PVE.Nodes().ListNodes(ctx)
		return err
	}

	t.Run("job request reaches cluster A with the job token", func(t *testing.T) {
		t.Parallel()
		deps, clusterA, clusterB := setup(t)
		d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{})
		if err != nil {
			t.Fatalf("WithRequestOverrides: %v", err)
		}
		if err := probe(t, d); err != nil {
			t.Fatalf("probe cluster A: %v", err)
		}
		if !strings.Contains(clusterA.traffic(), wireJobTokenValue) {
			t.Error("cluster A did not receive the job token, so this fixture cannot prove anything about B")
		}
		if clusterB.requests() != 0 {
			t.Error("a request with no overrides reached cluster B")
		}
	})

	entries := map[string]map[string]any{
		"token entry": {
			"api_token": wireEntryToken,
		},
		"user and password entry": {
			"user": wireEntryUser, "password": wireEntryPassword, "realm": wireEntryRealm,
		},
	}
	for name, creds := range entries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps, clusterA, clusterB := setup(t)
			hostB, portB := clusterB.endpoint(t)
			nested := map[string]any{"host": hostB, "port": float64(portB), "ca_cert": clusterB.caPEM()}
			maps.Copy(nested, creds)

			d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
				RequestID: "r-az2",
				Extra:     map[string]any{"pve": nested},
			})
			if err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			if err := probe(t, d); err != nil {
				t.Fatalf("probe cluster B: %v", err)
			}
			assertEntryCredentialsOnly(t, clusterA, clusterB, jobLeaks)
		})
	}

	// The realm is not part of the credential group and keeps inheriting, so
	// an entry that writes a bare user authenticates on B as user@<job
	// realm>. That leaks no secret, since B receives the entry's own user and
	// password, but the job realm may not exist on B, which is why the docs
	// tell operators to set realm on every entry for another cluster. This
	// pins the accepted behavior so the promise and the test agree.
	t.Run("bare user entry carries the job realm and nothing else", func(t *testing.T) {
		t.Parallel()
		deps, clusterA, clusterB := setup(t)
		hostB, portB := clusterB.endpoint(t)
		d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
			Extra: map[string]any{"pve": map[string]any{
				"host": hostB, "port": float64(portB), "ca_cert": clusterB.caPEM(),
				"user": wireEntryUser, "password": wireEntryPassword,
			}},
		})
		if err != nil {
			t.Fatalf("WithRequestOverrides: %v", err)
		}
		if err := probe(t, d); err != nil {
			t.Fatalf("probe cluster B: %v", err)
		}
		traffic := clusterB.traffic()
		if !strings.Contains(traffic, wireEntryUser+"@"+wireJobRealm) {
			t.Error("a bare entry user did not arrive qualified by the inherited job realm")
		}
		for _, leak := range []string{wireJobTokenValue, wireJobPassword, wireJobUser + "@"} {
			if strings.Contains(traffic, leak) {
				t.Error("cluster B received a job-level credential part (the fixture value for one of user, token, or password)")
			}
		}
		if clusterA.requests() != 0 {
			t.Error("an overridden request reached cluster A")
		}
	})

	t.Run("entry without ca_cert does not trust cluster B through the job CA", func(t *testing.T) {
		t.Parallel()
		deps, _, clusterB := setup(t)
		hostB, portB := clusterB.endpoint(t)
		d, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
			Extra: map[string]any{"pve": map[string]any{
				"host": hostB, "port": float64(portB), "api_token": wireEntryToken,
			}},
		})
		if err != nil {
			t.Fatalf("WithRequestOverrides: %v", err)
		}
		if d.Config.PVECACertPEM != "" {
			t.Error("the job's private CA followed the request to another endpoint")
		}
		// httptest serves one self-signed certificate for every server, so the
		// job's CA would have verified B. With the system pool it must not.
		if err := probe(t, d); err == nil {
			t.Error("cluster B's private certificate verified without the entry naming a CA")
		}
		if strings.Contains(clusterB.traffic(), wireJobTokenValue) {
			t.Error("cluster B received the job token")
		}
	})
}

// TestWithRequestOverrides_TLSDowngradeWarning_NamesCredentialGroup asserts
// that the TLS-downgrade warning says which credential group crosses the
// unverified connection, inherited or entry, and never prints a credential.
func TestWithRequestOverrides_TLSDowngradeWarning_NamesCredentialGroup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		extra     map[string]any
		wantGroup string
		wantText  string
	}{
		{
			name:      "same endpoint keeps the inherited group",
			extra:     map[string]any{"pve_verify_ssl": false},
			wantGroup: config.CredentialGroupInherited,
			wantText:  "the inherited job-level PVE credentials will be sent to the job-level host",
		},
		{
			name:      "other host uses the entry group",
			extra:     map[string]any{"pve_verify_ssl": false, "pve_host": "az1.example", "pve_api_token": overrideTestEntryToken},
			wantGroup: config.CredentialGroupEntry,
			wantText:  "the cpi-config entry's own PVE credentials will be sent to the effective host",
		},
		{
			name: "same endpoint with entry credentials uses the entry group",
			extra: map[string]any{
				"pve_verify_ssl": false, "pve_user": wireEntryUser, "pve_password": wireEntryPassword,
			},
			wantGroup: config.CredentialGroupEntry,
			wantText:  "the cpi-config entry's own PVE credentials will be sent to the effective host",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := overrideTestBaseConfig()
			observedLogger, obs := log.NewObservedLogger(log.LevelDebug)
			deps := handlers.Deps{
				Config: base,
				PVE:    &mockPVEClient{},
				Logger: observedLogger,
				Overrides: &handlers.RequestOverrideRuntime{
					ClientFactory: (&fakeTransportClientFactory{}).Factory,
					Logger:        log.NewNopLogger(),
				},
			}
			if _, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra}); err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			found := false
			for _, e := range obs.All() {
				rendered := fmt.Sprintf("%s %v", e.Message, e.Attrs)
				for _, secret := range []string{base.Password, overrideTestEntryToken, wireEntryPassword} {
					if strings.Contains(rendered, secret) {
						t.Errorf("a log line carries a credential fixture value: %q", e.Message)
					}
				}
				if e.Level != log.LevelWarn || !strings.Contains(e.Message, "disables TLS verification") {
					continue
				}
				found = true
				if strings.Contains(e.Message, "job-level PVE credentials will be sent to the overridden host") {
					t.Error("the warning still claims job-level credentials go to the overridden host")
				}
				if !strings.Contains(e.Message, tc.wantText) {
					t.Errorf("warning = %q, want it to contain %q", e.Message, tc.wantText)
				}
				if got := e.Attrs["credential_group"]; got != tc.wantGroup {
					t.Errorf("credential_group = %v, want %q", got, tc.wantGroup)
				}
			}
			if !found {
				t.Fatal("expected a Warn log entry about the TLS-verification downgrade, found none")
			}
		})
	}
}

// TestWithRequestOverrides_IncompleteCredentialGroup_NonRetriable asserts that
// an entry on another host with no credentials is refused before any client
// is built, as a non-retriable error that names property keys only.
func TestWithRequestOverrides_IncompleteCredentialGroup_NonRetriable(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	// A loaded job config never holds a token and a password together, because
	// ApplyDefaults clears the password, so the fixture holds the token alone.
	base.APIToken = wireJobToken
	base.Password = ""
	tracker := &fakeTransportClientFactory{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: tracker.Factory,
			Logger:        log.NewNopLogger(),
		},
	}
	for name, extra := range map[string]map[string]any{
		"host only":     {"pve_host": "az2.example"},
		"password only": {"pve_host": "az2.example", "pve_password": wireEntryPassword},
	} {
		_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: extra})
		if err == nil {
			t.Fatalf("%s: expected the request to be refused", name)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
			t.Errorf("%s: error %v must be a non-retriable CloudError", name, err)
		}
		if !strings.Contains(err.Error(), "set pve.api_token, or pve.user and pve.password") {
			t.Errorf("%s: error = %q, want the credential group remedy", name, err.Error())
		}
		for _, secret := range []string{wireJobTokenValue, wireEntryPassword} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error text carries a credential fixture value", name)
			}
		}
	}
	if len(tracker.calls) != 0 {
		t.Errorf("ClientFactory must not be invoked for a refused entry, got %d call(s)", len(tracker.calls))
	}
}

// TestWithRequestOverrides_CATrustWarnings asserts the two trust-root lines.
// An entry on another endpoint that drops the job's private CA says so, and a
// CA-only entry on the job endpoint that swaps the root under the inherited
// job credentials says so too. Neither line carries a certificate or a
// credential value.
func TestWithRequestOverrides_CATrustWarnings(t *testing.T) {
	t.Parallel()
	jobCA := handlers.SelfSignedCAPEMForTest(t)
	entryCA := handlers.SelfSignedCAPEMForTest(t)
	const dropText = "does not inherit the job-level pve.ca_cert"
	const swapText = "replaces the job-level pve.ca_cert"

	for _, tc := range []struct {
		name      string
		jobCA     string
		extra     map[string]any
		wantText  string // empty means neither trust line may fire
		wantGroup string
	}{
		{
			name:      "other host without ca_cert drops the job CA",
			jobCA:     jobCA,
			extra:     map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken},
			wantText:  dropText,
			wantGroup: config.CredentialGroupEntry,
		},
		{
			name:      "other port without ca_cert drops the job CA",
			jobCA:     jobCA,
			extra:     map[string]any{"pve_port": float64(8443), "pve_api_token": overrideTestEntryToken},
			wantText:  dropText,
			wantGroup: config.CredentialGroupEntry,
		},
		{
			name:  "other host with its own ca_cert is quiet",
			jobCA: jobCA,
			extra: map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken, "pve_ca_cert": entryCA},
		},
		{
			name:  "other host with no job CA is quiet",
			extra: map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken},
		},
		{
			name:  "other host with verify_ssl off is left to the downgrade warning",
			jobCA: jobCA,
			extra: map[string]any{"pve_host": "az2.example", "pve_api_token": overrideTestEntryToken, "pve_verify_ssl": false},
		},
		{
			name:      "ca_cert alone on the job endpoint swaps the root under the job credentials",
			jobCA:     jobCA,
			extra:     map[string]any{"pve_ca_cert": entryCA},
			wantText:  swapText,
			wantGroup: config.CredentialGroupInherited,
		},
		{
			name:  "ca_cert with the entry's own credentials on the job endpoint is quiet",
			jobCA: jobCA,
			extra: map[string]any{"pve_ca_cert": entryCA, "pve_api_token": overrideTestEntryToken},
		},
		{
			name:  "ca_cert equal to the job CA is quiet",
			jobCA: jobCA,
			extra: map[string]any{"pve_ca_cert": jobCA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := overrideTestBaseConfig()
			base.PVECACertPEM = tc.jobCA
			observedLogger, obs := log.NewObservedLogger(log.LevelDebug)
			deps := handlers.Deps{
				Config: base,
				PVE:    &mockPVEClient{},
				Logger: observedLogger,
				Overrides: &handlers.RequestOverrideRuntime{
					ClientFactory: (&fakeTransportClientFactory{}).Factory,
					Logger:        log.NewNopLogger(),
				},
			}
			if _, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra}); err != nil {
				t.Fatalf("WithRequestOverrides: %v", err)
			}
			assertTrustWarning(t, obs.All(), tc.wantText, tc.wantGroup, []string{
				base.Password, overrideTestEntryToken, jobCA, entryCA, "BEGIN CERTIFICATE",
			})
		})
	}
}

// assertTrustWarning checks that exactly the wanted trust-root line fired,
// with the wanted credential group, and that no log line carries a secret
// or certificate fixture value.
func assertTrustWarning(t *testing.T, entries []log.Entry, wantText, wantGroup string, secrets []string) {
	t.Helper()
	found := false
	for _, e := range entries {
		rendered := fmt.Sprintf("%s %v", e.Message, e.Attrs)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(rendered, secret) {
				t.Errorf("a log line carries a credential or certificate fixture value: %q", e.Message)
			}
		}
		isTrustLine := strings.Contains(e.Message, "does not inherit the job-level pve.ca_cert") ||
			strings.Contains(e.Message, "replaces the job-level pve.ca_cert")
		if !isTrustLine {
			continue
		}
		if wantText == "" || !strings.Contains(e.Message, wantText) {
			t.Errorf("unexpected trust warning %q", e.Message)
			continue
		}
		found = true
		if e.Level != log.LevelWarn {
			t.Errorf("trust warning level = %v, want Warn", e.Level)
		}
		if got := e.Attrs["credential_group"]; got != wantGroup {
			t.Errorf("credential_group = %v, want %q", got, wantGroup)
		}
		if e.Attrs["effective_pve_host"] == nil {
			t.Error("the trust warning must name the effective host")
		}
	}
	if wantText != "" && !found {
		t.Errorf("expected a Warn containing %q, found none", wantText)
	}
}
