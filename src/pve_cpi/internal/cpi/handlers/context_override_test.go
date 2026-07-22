// Tests for Deps.WithRequestOverrides and RequestOverrideRuntime — the
// per-request pve_* context config override mechanism that fixes BOSH
// cpi-config multi-CPI requests silently targeting the wrong PVE cluster
// (see context_override.go).
package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

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
		Extra: map[string]any{"pve_host": "should-be-ignored.example"},
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
		Extra:     map[string]any{"pve_host": "az1.example"},
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
		Extra:     map[string]any{"pve_host": "az2.example"},
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
		Extra:     map[string]any{"pve_host": "az1.example"},
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
		Extra: map[string]any{"pve_host": "unreachable.example"},
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
			"pve_placement_enabled": true,          // unsupported — Warn only
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
		d, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{"pve_host": host}})
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
		if _, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{"pve_host": host}}); err != nil {
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
	if _, err := deps.WithRequestOverrides(ctx, jsonrpc.Context{Extra: map[string]any{"pve_host": "az-00.example"}}); err != nil {
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
				Extra: map[string]any{"pve_host": "shared-az.example"},
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
					Extra: map[string]any{"pve_host": host},
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
