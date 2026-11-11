package pve

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const identityReplacedLine = "another process replaced the cluster identity"

// loadedWatch returns the watch a process holds when it loaded the file as it
// stands now, which is what openEndpointCache builds from its read.
func (e *workerEnv) loadedWatch() *clusterIdentityWatch {
	e.t.Helper()
	p := e.payload()
	if p == nil || p.Cluster == nil {
		e.t.Fatal("the file holds no identity to load")
	}
	return newClusterIdentityWatch(e.logger, e.store.FileName(), p)
}

// runWith starts one refresh in a process that loaded the file under watch,
// against pool, and waits for it. It returns the worker, so a test can run the
// worker's checks again.
func (e *workerEnv) runWith(watch *clusterIdentityWatch, pool DiscoveryPool) *DiscoveryWorker {
	e.t.Helper()
	e.begin()
	w, err := newDiscoveryWorker(DiscoveryWorkerConfig{
		Store: e.store, Mode: e.mode, Logger: e.logger, Resolver: e.resolver.resolve,
		Registry: e.registry, Now: e.clock.Now, Identity: watch,
	})
	if err != nil {
		e.t.Fatalf("newDiscoveryWorker: %v", err)
	}
	b := e.binding()
	b.Pool = pool
	if err := w.bind(b); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	w.FirstAnswer(workerSeed)
	e.drain()
	return w
}

// recordVerifiedReplacement runs the refresh of another process that finds
// the cluster under a new root CA with verify_ssl true, and so replaces the
// stored identity. The process that loaded the pool before that is the one
// under test, so the replacing process drops into a pool of its own.
func (e *workerEnv) recordVerifiedReplacement() {
	e.t.Helper()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.pool.drops.Store(0)
	e.begin()
}

// A verified identity replacement that another process stored after this
// process loaded its pool leaves a fresh file with no conflict and no refresh
// due, and the worker still drops the discovered endpoints the old identity
// vouched for.
func TestDiscoveryWorker_IdentityReplacedByAnotherProcessDropsTheLoadedPool(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	watch := e.loadedWatch()
	e.recordVerifiedReplacement()
	if p := e.payload(); p.Cluster.IdentityConflict || p.Cluster.RootCASHA256 == "" {
		t.Fatalf("identity %+v, want a replaced identity and no conflict", p.Cluster)
	}

	pool := &discPool{}
	e.runWith(watch, pool)
	if n := pool.drops.Load(); n != 1 {
		t.Fatalf("DropDiscovered called %d times, want once", n)
	}
	if n := e.requests(); n != 0 {
		t.Fatalf("a fresh file sent %d requests", n)
	}
	e.noLineAt(log.LevelInfo)
	e.wantLine(identityReplacedLine, log.LevelDebug)
}

// The same sequence with the real pool refuses a request to the endpoint the
// old identity vouched for, before any dial.
func TestDiscoveryWorker_IdentityReplacedByAnotherProcessRefusesTheLoadedDiscoveredEndpoint(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	watch := e.loadedWatch()
	e.recordVerifiedReplacement()

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var dials atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = conn.Close()
		}
	}()
	discoveredAddr := listener.Addr().String()
	pool, err := endpointpool.New(endpointpool.Config{
		Base: &http.Transport{},
		Endpoints: []endpointpool.Endpoint{
			{Address: workerSeed, Kind: endpointpool.KindConfigured},
			{Address: discoveredAddr, Kind: endpointpool.KindDiscovered, Node: "pve2", Pins: []endpointpool.Pin{{9}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.CloseIdleConnections)
	e.runWith(watch, pool)
	if n := e.requests(); n != 0 {
		t.Fatalf("a fresh file sent %d requests", n)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://"+discoveredAddr+"/api2/json/version", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pool.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !endpointpool.IsConnectPhase(err) || endpointpool.IsMaybeSent(err) ||
		!strings.Contains(err.Error(), "the cluster identity changed") {
		t.Fatalf("a request to a dropped endpoint should be refused unsent with the drop's cause, got %v", err)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the dropped endpoint was dialed %d times", n)
	}
}

// The control for both: a process whose identity matches the file drops
// nothing and says nothing, whether a refresh is due or not.
func TestDiscoveryWorker_UnchangedIdentityDropsNothingAndLogsNothing(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	watch := e.loadedWatch()
	e.begin()

	pool := &discPool{}
	e.runWith(watch, pool)
	if n := pool.drops.Load(); n != 0 {
		t.Fatalf("DropDiscovered called %d times, want none", n)
	}
	e.noLineAt(log.LevelInfo)
	e.noLine(identityReplacedLine)

	e.clock.advance(endpointcache.RefreshInterval)
	e.runWith(watch, pool)
	if n := pool.drops.Load(); n != 0 {
		t.Fatalf("a refresh that finds the same cluster called DropDiscovered %d times, want none", n)
	}
	e.noLineAt(log.LevelWarn)
	e.noLine(identityReplacedLine)
}

// A refresh in this process that itself accepts a new identity drops the pool
// once through the trust decision, and the identity it stored is then the one
// the pool matches, so the worker's next check is no foreign change.
func TestDiscoveryWorker_AnIdentityTheWorkerAcceptedIsNotAForeignChange(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	watch := e.loadedWatch()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)

	pool := &discPool{}
	w := e.runWith(watch, pool)
	e.wantLine("the cached nodes and pins are replaced", log.LevelError)
	if n := pool.drops.Load(); n != 1 {
		t.Fatalf("the accepted replacement called DropDiscovered %d times, want once", n)
	}
	e.noLine(identityReplacedLine)

	e.begin()
	if w.due(context.Background(), DiscoveryBinding{Pool: pool}) {
		t.Fatal("a refresh is due right after one stored its result")
	}
	if n := pool.drops.Load(); n != 1 {
		t.Fatalf("the next check called DropDiscovered %d times in all, want the first one only", n)
	}
	e.noLine(identityReplacedLine)
}

// A cache file that was deleted while this process held its pool, and then
// rebuilt by this process's own refresh under another identity, is a first
// discovery as the trust rules see it. The pool still holds endpoints from the
// old identity, so the worker drops them.
func TestDiscoveryWorker_AFirstDiscoveryAfterTheFileWasDeletedDropsTheOldIdentitysEndpoints(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	watch := e.loadedWatch()
	if err := os.Remove(e.filePath()); err != nil {
		t.Fatal(err)
	}
	e.cluster.replaceRoot()

	pool := &discPool{}
	e.runWith(watch, pool)
	e.noLine(identityReplacedLine + " with")
	if n := pool.drops.Load(); n != 1 {
		t.Fatalf("DropDiscovered called %d times, want once; lines: %s", n, e.dump())
	}
}

// A process that loaded no identity has nothing to replace, so the first
// identity a discovery stores drops nothing.
func TestDiscoveryWorker_AFirstDiscoveryOnAnEmptyFileDropsNothing(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	pool := &discPool{}
	e.runWith(newClusterIdentityWatch(e.logger, e.store.FileName(), nil), pool)
	if n := pool.drops.Load(); n != 0 {
		t.Fatalf("DropDiscovered called %d times, want none", n)
	}
	if p := e.payload(); p == nil || p.Cluster == nil {
		t.Fatal("the refresh stored no identity")
	}
}

// A worker that is the first to see a standing conflict says so at Warn,
// once, however many of its checks meet it.
func TestDiscoveryWorker_AConflictTheWorkerFindsFirstWarnsOnce(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	watch := e.loadedWatch()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	if p := e.payload(); !p.Cluster.IdentityConflict {
		t.Fatalf("identity %+v, want a recorded conflict", p.Cluster)
	}

	pool := &discPool{}
	e.begin()
	w := e.runWith(watch, pool)
	w.dropOnStoredIdentity(DiscoveryBinding{Pool: pool})
	w.due(context.Background(), DiscoveryBinding{Pool: pool})
	if n := countAt(e.lines(), log.LevelWarn); n != 1 {
		t.Fatalf("%d Warn lines, want the conflict once: %s", n, e.dump())
	}
	e.wantLine("the cached cluster identity differs", log.LevelWarn)
	if n := pool.drops.Load(); n != 3 {
		t.Fatalf("DropDiscovered called %d times, want once for each check", n)
	}
}

// The conflict is also found by the update under the lock when another process
// records it between the worker's checks. That path warns through the same
// watch.
func TestDiscoveryWorker_AConflictTheUpdateFindsWarnsOnce(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.clock.advance(3 * time.Hour)
	e.begin()
	w := e.newWorker()
	w.discover(context.Background(), workerSeed, e.binding())
	w.discover(context.Background(), workerSeed, e.binding())
	if n := countAt(e.lines(), log.LevelWarn); n != 1 {
		t.Fatalf("%d Warn lines, want the conflict once: %s", n, e.dump())
	}
}

// openCacheThenRecordConflict opens the cache of a verify_ssl false config whose file
// holds nodes, then records the identity conflict in it as another process
// would, and returns the open cache. The file held no conflict when the cache
// loaded it.
func openCacheThenRecordConflict(t *testing.T) openedCache {
	t.Helper()
	cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
	verify := false
	cfg.VerifySSL = &verify
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil || len(got.cache.discovered) != 2 {
		t.Fatalf("setup: cache %v, lines %v", got.cache, got.obs.All())
	}
	applySeedChange(t, cfg, endpointcache.Change{Conflict: true})
	return got
}

// Another process records the conflict after this process loaded a clean file.
// The cache's own load cannot cover that call, so the worker's check warns,
// and a later load of the same conflict through the cache stays silent.
func TestAConflictRecordedAfterTheLoadWarnsOnceWhicheverPathSeesItFirst(t *testing.T) {
	t.Run("the worker first, then the cache", func(t *testing.T) {
		got := openCacheThenRecordConflict(t)
		pool := &discPool{}
		if got.cache.worker.due(context.Background(), DiscoveryBinding{Pool: pool}) {
			t.Fatal("a refresh is due while a conflict stands")
		}
		if n := pool.drops.Load(); n != 1 {
			t.Fatalf("DropDiscovered called %d times, want once", n)
		}
		got.cache.load(&got.plan, conflictedPayload(t, got.cache))
		if lines := linesAt(got.obs.All(), log.LevelWarn, "cluster identity differs"); len(lines) != 1 {
			t.Fatalf("conflict lines = %v, want one Warn; all: %v", lines, got.obs.All())
		}
	})
	t.Run("the cache first, then the worker", func(t *testing.T) {
		got := openCacheThenRecordConflict(t)
		got.cache.load(&got.plan, conflictedPayload(t, got.cache))
		pool := &discPool{}
		got.cache.worker.due(context.Background(), DiscoveryBinding{Pool: pool})
		got.cache.worker.due(context.Background(), DiscoveryBinding{Pool: pool})
		if lines := linesAt(got.obs.All(), log.LevelWarn, "cluster identity differs"); len(lines) != 1 {
			t.Fatalf("conflict lines = %v, want one Warn; all: %v", lines, got.obs.All())
		}
		if n := pool.drops.Load(); n != 2 {
			t.Fatalf("DropDiscovered called %d times, want once for each check", n)
		}
	})
}

// The control: a clean file warns about no conflict and drops nothing, through
// the cache and its worker.
func TestACleanCacheFileWarnsAboutNothingAndDropsNothing(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
	verify := false
	cfg.VerifySSL = &verify
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatalf("no cache: %v", got.obs.All())
	}
	pool := &discPool{}
	got.cache.worker.due(context.Background(), DiscoveryBinding{Pool: pool})
	if n := pool.drops.Load(); n != 0 {
		t.Fatalf("DropDiscovered called %d times, want none", n)
	}
	if n := countAt(got.obs.All(), log.LevelWarn); n != 0 {
		t.Fatalf("%d Warn lines on a clean file: %v", n, got.obs.All())
	}
}

// conflictedPayload reads the cache file as a later call would, which holds
// the conflict.
func conflictedPayload(t *testing.T, cache *endpointCache) *endpointcache.Payload {
	t.Helper()
	p, _, err := cache.store.ReadPayload(cacheClock(), nil)
	if err != nil {
		t.Fatalf("ReadPayload: %v", err)
	}
	if !standingConflict(p) {
		t.Fatalf("setup: payload holds no conflict: %+v", p.Cluster)
	}
	return p
}
