package pve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// gatedBinding returns a binding whose GET /nodes read blocks until gate is
// closed or its context ends, and a channel that closes when the read starts.
func gatedBinding(e *workerEnv, gate <-chan struct{}) (DiscoveryBinding, <-chan struct{}) {
	entered := make(chan struct{})
	var once sync.Once
	client := markedClient(e.cluster, e.reqs)
	inner := client.nodes.listNodesFn
	client.nodes.listNodesFn = func(ctx context.Context) (*sdknodes.ListNodesResponse, error) {
		once.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return inner(ctx)
	}
	return DiscoveryBinding{Client: client, Pool: e.pool}, entered
}

// waitFor fails the test unless ch closes within ten seconds.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// workerWithClock returns a worker on the environment's store that reads time
// from now.
func (e *workerEnv) workerWithClock(now func() time.Time) *DiscoveryWorker {
	e.t.Helper()
	w, err := NewDiscoveryWorker(DiscoveryWorkerConfig{
		Store: e.store, Mode: e.mode, Logger: e.logger, Resolver: e.resolver.resolve,
		Registry: e.registry, Now: now,
	})
	if err != nil {
		e.t.Fatalf("NewDiscoveryWorker: %v", err)
	}
	return w
}

// startWorker binds w to the environment, sends the first answer through
// answer, and waits for the refresh to end.
func (e *workerEnv) startWorker(w *DiscoveryWorker, answer string) {
	e.t.Helper()
	e.begin()
	if err := w.Bind(e.binding()); err != nil {
		e.t.Fatalf("Bind: %v", err)
	}
	w.FirstAnswer(answer)
	e.drain()
}

// A node refused under verify_ssl false keeps its stored confirmation, and a
// reader hides it after 24 hours while the file keeps knowing it. The summary
// of each hourly refresh is then a steady state at Debug, and it counts only
// the nodes a reader loads.
func TestDiscoveryWorker_HeldNodePastADayKeepsTheSummaryAtDebug(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.setNodeFingerprints("rotated:", "pve1")
	for hour := 1; hour <= 27; hour++ {
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		line := e.wantLine("api endpoint discovery: cached", log.LevelDebug)
		count := "cached 2 of 3 nodes"
		if hour > 24 {
			count = "cached 0 of 3 nodes"
		}
		if !strings.Contains(line.Message, count) {
			t.Fatalf("refresh %d logged %q, want %q", hour, line.Message, count)
		}
	}
	if got := cachedNodeNames(e.payload()); len(got) != 0 {
		t.Fatalf("the reader view holds %v after 27 hours, want none", got)
	}
}

// The caller closes the store after the exit drain, which cancels a worker and
// does not wait for it. A worker that is still unwinding must not race the
// close, and it must not report the closed store as a failure. The reads hold
// the worker inside its refresh until the drain cancels it, so the worker's
// next store access always comes after the drain, the same moment the caller
// closes the store.
func TestDiscoveryWorker_StoreClosedAfterTheDrainIsNotARaceOrAWarn(t *testing.T) {
	t.Parallel()
	for range 10 {
		e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
		binding, entered := gatedBinding(e, make(chan struct{}))
		var running sync.WaitGroup
		w := e.newWorker()
		start := w.start
		w.start = func(name string, mode config.DiscoveryMode, fn func(ctx context.Context)) error {
			running.Add(1)
			return start(name, mode, func(ctx context.Context) {
				defer running.Done()
				fn(ctx)
			})
		}
		if err := w.Bind(binding); err != nil {
			t.Fatal(err)
		}
		e.begin()
		w.FirstAnswer(workerSeed)
		waitFor(t, entered, "the discovery reads to start")
		e.registry.Drain(context.Background(), 0)
		if err := e.store.Close(); err != nil {
			t.Fatal(err)
		}
		running.Wait()
		e.registry.Open()
		for _, entry := range e.lines() {
			if entry.Level >= log.LevelWarn {
				t.Fatalf("a cancelled worker logged %v %q", entry.Level, entry.Message)
			}
		}
	}
}

// A refresh whose context ended before it began touches nothing, and a due
// check that fails after the context ended says nothing.
func TestDiscoveryWorker_CancelledContextTouchesNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("a refresh returns before it asks the pool or the store", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
		var asked atomic.Int32
		e.pool.refreshDue = func() bool {
			asked.Add(1)
			return true
		}
		e.begin()
		e.newWorker().refresh(ctx, workerSeed, e.binding())
		if asked.Load() != 0 || e.requests() != 0 || len(e.lines()) != 0 {
			t.Fatalf("a cancelled refresh asked the pool %d times, sent %d requests, logged %s",
				asked.Load(), e.requests(), e.dump())
		}
	})
	t.Run("a due check on a closed store is silent", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
		if err := e.store.Close(); err != nil {
			t.Fatal(err)
		}
		e.begin()
		w := e.newWorker()
		if due := w.due(ctx, e.binding()); due || len(e.lines()) != 0 {
			t.Fatalf("a cancelled due check on a closed store: due %v, lines %s", due, e.dump())
		}
		// The same read with a live context is a failure worth a line.
		if due := w.due(context.Background(), e.binding()); due {
			t.Fatal("a closed store made a refresh due")
		}
		e.wantLine("could not read the cache file; skipping the refresh", log.LevelWarn)
	})
}

// The worker holds the discover lock from the second due check to the end of
// the write, so no second process discovers while its reads are out.
func TestDiscoveryWorker_HoldsTheDiscoverLockWhileTheReadsRun(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	binding, entered := gatedBinding(e, gate)
	e.begin()
	w := e.newWorker()
	if err := w.Bind(binding); err != nil {
		t.Fatal(err)
	}
	w.FirstAnswer(workerSeed)
	waitFor(t, entered, "the discovery reads to start")
	other := e.openStore()
	lock, held, err := other.TryLockDiscovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held {
		_ = lock.Close()
		t.Fatal("a second process took the discover lock while the first one's reads were out")
	}
	release()
	e.drain()
	lock, held, err = other.TryLockDiscovery(context.Background())
	if err != nil || !held {
		t.Fatalf("the discover lock was not free after the refresh: held %v, err %v", held, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// The pool calls the hook on the request's goroutine, so FirstAnswer returns
// at once and the refresh runs on its own goroutine.
func TestDiscoveryWorker_FirstAnswerDoesNotWaitForTheRefresh(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	binding, entered := gatedBinding(e, gate)
	e.begin()
	w := e.newWorker()
	if err := w.Bind(binding); err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() {
		w.FirstAnswer(workerSeed)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("FirstAnswer waited for the refresh to end")
	}
	waitFor(t, entered, "the discovery reads to start")
	release()
	e.drain()
}

// The decision and a failed attempt use the clock the update was given. A
// clock that moves a minute on every reading shows any other reading.
func TestDiscoveryWorker_DecisionAndAttemptUseTheUpdatesClock(t *testing.T) {
	t.Parallel()
	steppingClock := func(base time.Time) func() time.Time {
		var mu sync.Mutex
		var readings int
		return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			readings++
			return base.Add(time.Duration(readings) * time.Minute)
		}
	}
	t.Run("a result is written", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		base := e.clock.Now()
		e.startWorker(e.workerWithClock(steppingClock(base)), workerSeed)
		p, _, err := e.store.ReadPayload(base.Add(time.Hour), nil)
		if err != nil || p == nil || p.Discovery == nil || len(p.Discovery.Nodes) != 2 {
			t.Fatalf("the result was not written: %+v, %v; lines: %s", p, err, e.dump())
		}
	})
	t.Run("a failed attempt is stamped", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.cluster.set(func() {
			for _, name := range e.cluster.names {
				e.cluster.rootless[name] = true
			}
		})
		base := e.clock.Now()
		e.startWorker(e.workerWithClock(steppingClock(base)), workerSeed)
		p, _, err := e.store.ReadPayload(base.Add(time.Hour), nil)
		if err != nil || p == nil || p.Discovery == nil ||
			p.Discovery.LastFailure != endpointcache.FailureRootCAUnreadable {
			t.Fatalf("the attempt was not recorded: %+v, %v; lines: %s", p, err, e.dump())
		}
	})
}

// The key's first endpoint seeds the derived names, whichever configured
// endpoint answers, and every key endpoint is a configured one.
func TestDiscoveryWorker_KeyWithSeveralEndpoints(t *testing.T) {
	t.Parallel()
	reopen := func(e *workerEnv, endpoints ...string) {
		e.key.Endpoints = endpoints
		e.store = e.openStore()
	}
	t.Run("a later endpoint answers", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		reopen(e, workerSeed, "10.0.0.2:8006")
		e.startWorker(e.newWorker(), "10.0.0.2:8006")
		p := e.payload()
		if p == nil || p.Discovery == nil || p.Cluster == nil {
			t.Fatalf("nothing was cached; lines: %s", e.dump())
		}
		if got := cachedNodeNames(p); !slices.Equal(got, []string{"pve2", "pve3"}) {
			t.Fatalf("cached nodes %v, want pve2 and pve3 under the first endpoint's domain", got)
		}
		for _, node := range p.Discovery.Nodes {
			if want := node.Name + ".dc1.example.com"; node.Address != want {
				t.Errorf("node %s address %q, want %q", node.Name, node.Address, want)
			}
		}
		if p.Cluster.ObservedVia != "10.0.0.2:8006" {
			t.Errorf("observed_via %q, want the answering endpoint", p.Cluster.ObservedVia)
		}
	})
	t.Run("a node at a configured endpoint is left out", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		reopen(e, workerSeed, "pve2.dc1.example.com:8006")
		e.startWorker(e.newWorker(), workerSeed)
		if got := cachedNodeNames(e.payload()); !slices.Equal(got, []string{"pve3"}) {
			t.Fatalf("cached nodes %v, want only pve3; lines: %s", got, e.dump())
		}
		e.wantLine("left out node pve2, because its address is a configured endpoint", log.LevelDebug)
	})
	t.Run("a failure names the key's spelling of the answering endpoint", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.cluster.set(func() {
			for _, name := range e.cluster.names {
				e.cluster.rootless[name] = true
			}
		})
		e.startWorker(e.newWorker(), strings.ToUpper(workerSeed))
		e.wantLine("api endpoint discovery: no result through "+workerSeed+", because", log.LevelInfo)
	})
	t.Run("observed_via keeps the key's spelling of the answering endpoint", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.startWorker(e.newWorker(), strings.ToUpper(workerSeed))
		p := e.payload()
		if p == nil || p.Cluster == nil || p.Cluster.ObservedVia != workerSeed {
			t.Fatalf("payload %+v, want observed_via %q; lines: %s", p, workerSeed, e.dump())
		}
	})
}

// A pin failure in another process counts after its mark is gone, until a
// discovery answers, and only while it is later than the last discovery.
func TestDiscoveryWorker_PinFailureInAnotherProcess(t *testing.T) {
	t.Parallel()
	const discoveryAge = 11 * time.Minute
	failAt := func(e *workerEnv, at time.Time, pinFailed bool) {
		t.Helper()
		change := endpointcache.Change{Marks: map[string]endpointcache.DownMark{
			"pve2.dc1.example.com:8006": {DownAt: at, Class: endpointcache.MarkPin},
		}}
		if pinFailed {
			change.PinFailedAt = at
		}
		if _, err := e.openStore().UpdatePayload(context.Background(), at,
			func(endpointcache.Stored) (endpointcache.Change, error) { return change, nil }); err != nil {
			t.Fatalf("write the pin failure: %v", err)
		}
	}
	t.Run("a failure older than its mark still makes the refresh due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		discovered := e.clock.Now()
		e.clock.advance(5 * time.Minute)
		failAt(e, e.clock.Now(), true)
		e.clock.advance(discoveryAge - 5*time.Minute)
		e.run()
		if e.requests() == 0 {
			t.Fatal("a pin failure after the last discovery and past the floor sent no request")
		}
		p := e.payload()
		if !p.Discovery.DiscoveredAt.After(discovered) || !p.Discovery.PinFailedAt.IsZero() {
			t.Fatalf("the refresh left discovered_at %v and pin_failed_at %v", p.Discovery.DiscoveredAt,
				p.Discovery.PinFailedAt)
		}
	})
	t.Run("a mark with no recorded time does not outlast itself", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(5 * time.Minute)
		failAt(e, e.clock.Now(), false)
		e.clock.advance(discoveryAge - 5*time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("an expired pin mark sent %d requests", n)
		}
	})
	t.Run("a failure within the floor waits for it", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(time.Minute)
		failAt(e, e.clock.Now(), true)
		e.clock.advance(3 * time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a pin failure inside the floor sent %d requests", n)
		}
		e.clock.advance(endpointcache.PinRefreshFloor)
		e.run()
		if e.requests() == 0 {
			t.Fatal("a pin failure past the floor sent no request")
		}
	})
	t.Run("a failed attempt leaves the failure and spends it", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(time.Minute)
		failedAt := e.clock.Now()
		failAt(e, failedAt, true)
		e.cluster.set(func() {
			for _, name := range e.cluster.names {
				e.cluster.rootless[name] = true
			}
		})
		e.clock.advance(discoveryAge)
		e.run()
		if e.requests() == 0 {
			t.Fatal("the pin failure made no refresh due")
		}
		p := e.payload()
		if p.Discovery.LastFailure != endpointcache.FailureRootCAUnreadable || !p.Discovery.PinFailedAt.Equal(failedAt) {
			t.Fatalf("after the attempt: last_failure %q, pin_failed_at %v, want the failure and %v",
				p.Discovery.LastFailure, p.Discovery.PinFailedAt, failedAt)
		}
		e.clock.advance(endpointcache.PinRefreshFloor + time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a pin failure the attempt spent sent %d requests", n)
		}
	})
}

// A clock that steps back hides the stored section from a reader, so every
// call is due. A failure that repeats is still the same failure, and its line
// stays at the steady level.
func TestDiscoveryWorker_ClockStepBackKeepsTheFailureLevelSteady(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.set(func() {
		for _, name := range e.cluster.names {
			e.cluster.rootless[name] = true
		}
	})
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.wantLine("because the cluster root CA could not be read", log.LevelInfo)
	e.clock.advance(-10 * time.Minute)
	for call := 1; call <= 3; call++ {
		e.run()
		if e.requests() == 0 {
			t.Fatalf("call %d sent no request, so the stepped-back clock did not hide the section", call)
		}
		e.wantLine("because the cluster root CA could not be read", log.LevelDebug)
	}
}

// An answer after Bind starts one refresh, whatever the number of answers.
func TestDiscoveryWorker_StartsOneRefreshForManyAnswers(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	var asked atomic.Int32
	e.pool.refreshDue = func() bool {
		asked.Add(1)
		return false
	}
	e.begin()
	w := e.newWorker()
	if err := w.Bind(e.binding()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w.FirstAnswer(workerSeed)
	}
	e.drain()
	// One refresh asks the pool before it takes the lock and once under it.
	if asked.Load() != 2 {
		t.Fatalf("the pool was asked %d times, want 2 for one refresh", asked.Load())
	}
}

// The identity line is logged when the trust decision made it, even when the
// write that follows fails.
func TestDiscoveryWorker_TrustLinesSurviveAFailedWrite(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run(func(w *DiscoveryWorker) {
		w.beforeWrite = func(context.Context) {
			if err := os.Remove(e.filePath()); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(e.filePath(), 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	e.wantLine("the cache write failed; the discovery result is dropped", log.LevelDebug)
	e.wantLine("endpoint cache: cluster identity changed", log.LevelError)
	e.noLine("api endpoint discovery: cached")
}

// A pin set that changes under verify_ssl true is accepted, so only
// verify_ssl false warns about it.
func TestDiscoveryWorker_PinChangeUnderVerifySSLTrueDoesNotWarn(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	e.cluster.setNodeFingerprints("rotated:", "pve1")
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.noLineAt(log.LevelWarn)
	e.wantLine("api endpoint discovery: cached 2 of 3 nodes", log.LevelDebug)
}

// A node that reports fewer pins is logged at Debug in the same refresh that
// warns about a node whose pins changed.
func TestDiscoveryWorker_ShrinkIsLoggedBesideAPinWarning(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	rotated, _ := discFingerprint("rotated:pve2")
	e.cluster.set(func() {
		e.cluster.nodeRows = []json.RawMessage{
			e.cluster.nodeRow("pve1", true),
			jsonRow(e.cluster.t, map[string]any{"node": "pve2", "status": "online", "ssl_fingerprint": rotated}),
			e.cluster.nodeRow("pve3", true),
		}
		e.cluster.certFailure["pve2"] = errors.New("certificates/info failed")
		e.cluster.certFailure["pve3"] = errors.New("certificates/info failed")
	})
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.wantLine("the pin set of known node pve2 changed", log.LevelWarn)
	e.wantLine("known node pve3 reported fewer pins than stored", log.LevelDebug)
}

// A failed refresh reports the records the update left out, as a result does.
func TestDiscoveryWorker_FailedRefreshLogsTheRecordsTheUpdateLeftOut(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.set(func() {
		for _, name := range e.cluster.names {
			e.cluster.rootless[name] = true
		}
	})
	e.clock.advance(endpointcache.NodeTTL + time.Hour)
	e.run()
	e.wantLine("because the cluster root CA could not be read", log.LevelInfo)
	e.wantLine("endpoint cache: records left out", log.LevelDebug)
}
