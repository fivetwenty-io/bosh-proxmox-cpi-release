package pve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	workerSeed       = "pve1.dc1.example.com:8006"
	workerUser       = "cpi@pve!discovery"
	workerCredential = "token-secret-0123456789"
)

// discRequests records every request the discovery reads send, and whether
// each carried the configured-only marker.
type discRequests struct {
	mu       sync.Mutex
	total    int
	unmarked []string
	kinds    map[string]int
}

func (r *discRequests) note(ctx context.Context, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total++
	if r.kinds == nil {
		r.kinds = map[string]int{}
	}
	r.kinds[kind]++
	if !endpointpool.IsConfiguredOnly(ctx) {
		r.unmarked = append(r.unmarked, kind)
	}
}

func (r *discRequests) snapshot() (total int, unmarked []string, kinds map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds = map[string]int{}
	for k, v := range r.kinds {
		kinds[k] = v
	}
	return r.total, append([]string(nil), r.unmarked...), kinds
}

// markedClient is the cluster's client with every read recorded in reqs.
func markedClient(d *discCluster, reqs *discRequests) *discClient {
	c := d.client()
	nodesFn, certFn := c.nodes.listNodesFn, c.nodes.listCertificatesInfoFn
	statusFn, membersFn := c.cluster.listStatusFn, c.cluster.listConfigNodesFn
	c.nodes.listNodesFn = func(ctx context.Context) (*sdknodes.ListNodesResponse, error) {
		reqs.note(ctx, "nodes")
		return nodesFn(ctx)
	}
	c.nodes.listCertificatesInfoFn = func(
		ctx context.Context, node string,
	) (*sdknodes.ListCertificatesInfoResponse, error) {
		reqs.note(ctx, "certificates")
		return certFn(ctx, node)
	}
	c.cluster.listStatusFn = func(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
		reqs.note(ctx, "status")
		return statusFn(ctx)
	}
	c.cluster.listConfigNodesFn = func(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		reqs.note(ctx, "members")
		return membersFn(ctx)
	}
	return c
}

// discResolver resolves every name except the ones it is told to fail.
type discResolver struct {
	mu      sync.Mutex
	fail    map[string]bool
	failAll bool
}

func (r *discResolver) resolve(_ context.Context, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAll || r.fail[host] {
		return errors.New("no such host")
	}
	return nil
}

func (r *discResolver) setFailAll(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failAll = fail
}

// discClock is a clock the test moves.
type discClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *discClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *discClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type workerEnvOptions struct {
	seed      string
	verifySSL bool
	mode      config.DiscoveryMode
}

// workerEnv is one cache directory and store, a fake cluster, and an
// isolated worker registry.
type workerEnv struct {
	t        *testing.T
	dir      string
	key      endpointcache.KeyConfig
	mode     config.DiscoveryMode
	store    *endpointcache.Store
	cluster  *discCluster
	reqs     *discRequests
	resolver *discResolver
	logger   *log.Logger
	obs      *log.Observer
	registry *endpointcache.Registry
	clock    *discClock

	pool    *discPool
	seen    int
	runReqs int
}

func newWorkerEnv(t *testing.T, opts workerEnvOptions) *workerEnv {
	t.Helper()
	if opts.seed == "" {
		opts.seed = workerSeed
	}
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	e := &workerEnv{
		t:        t,
		dir:      dir,
		key:      endpointcache.KeyConfig{Endpoints: []string{opts.seed}, VerifySSL: opts.verifySSL, User: workerUser},
		mode:     opts.mode,
		cluster:  newDiscCluster(t),
		reqs:     &discRequests{},
		resolver: &discResolver{fail: map[string]bool{}},
		logger:   logger,
		obs:      obs,
		registry: endpointcache.NewRegistry(),
		clock:    &discClock{now: time.Now().UTC().Truncate(time.Second)},
		pool:     &discPool{},
	}
	e.store = e.openStore()
	return e
}

// openStore opens a store on the environment's directory and key, as another
// process would.
func (e *workerEnv) openStore() *endpointcache.Store {
	e.t.Helper()
	choice := endpointcache.Resolve(endpointcache.Options{Override: e.dir})
	if !choice.Usable() {
		e.t.Fatalf("Resolve(%s): %v", e.dir, choice.Err)
	}
	s, err := endpointcache.OpenStore(context.Background(), choice.Root, e.key, workerCredential,
		endpointcache.StoreOptions{Logger: e.logger, ConfiguredMode: e.mode})
	if err != nil {
		e.t.Fatalf("OpenStore: %v", err)
	}
	e.t.Cleanup(func() {
		if err := s.Close(); err != nil {
			e.t.Error(err)
		}
	})
	return s
}

func (e *workerEnv) newWorker() *DiscoveryWorker {
	e.t.Helper()
	w, err := NewDiscoveryWorker(DiscoveryWorkerConfig{
		Store: e.store, Mode: e.mode, Logger: e.logger, Resolver: e.resolver.resolve,
		Registry: e.registry, Now: e.clock.Now,
	})
	if err != nil {
		e.t.Fatalf("NewDiscoveryWorker: %v", err)
	}
	return w
}

func (e *workerEnv) binding() DiscoveryBinding {
	return DiscoveryBinding{Client: markedClient(e.cluster, e.reqs), Pool: e.pool}
}

// discPool stands in for the endpoint pool. It reports the pin failure the
// test sets, or what refreshDue returns when that is set, and counts drops.
type discPool struct {
	pinFailed  atomic.Bool
	drops      atomic.Int32
	refreshDue func() bool
}

func (p *discPool) RefreshDue() bool {
	if p.refreshDue != nil {
		return p.refreshDue()
	}
	return p.pinFailed.Load()
}

func (p *discPool) DropDiscovered() int {
	p.drops.Add(1)
	return 0
}

// begin marks the start of a run, so lines and requests count from here.
func (e *workerEnv) begin() {
	e.seen = e.obs.Len()
	e.runReqs, _, _ = e.reqs.snapshot()
}

// run starts one refresh through the first-answer hook and waits for it.
func (e *workerEnv) run(prepare ...func(*DiscoveryWorker)) {
	e.t.Helper()
	e.begin()
	w := e.newWorker()
	for _, p := range prepare {
		p(w)
	}
	if err := w.Bind(e.binding()); err != nil {
		e.t.Fatalf("Bind: %v", err)
	}
	w.FirstAnswer(workerSeed)
	e.drain()
}

func (e *workerEnv) drain() {
	e.t.Helper()
	res := e.registry.Drain(context.Background(), 30*time.Second)
	e.registry.Open()
	if len(res.Cancelled) > 0 || len(res.Panics) > 0 {
		e.t.Fatalf("drain cancelled %v, panics %v", res.Cancelled, res.Panics)
	}
}

// requests returns how many reads the last run sent.
func (e *workerEnv) requests() int {
	total, _, _ := e.reqs.snapshot()
	return total - e.runReqs
}

// lines returns the log lines since the last run began.
func (e *workerEnv) lines() []log.Entry {
	return e.obs.All()[e.seen:]
}

// line returns the first line since the last run began that contains text.
func (e *workerEnv) line(text string) (log.Entry, bool) {
	for _, entry := range e.lines() {
		if strings.Contains(entry.Message, text) {
			return entry, true
		}
	}
	return log.Entry{}, false
}

// wantLine fails unless a line since the last run began contains text at level.
func (e *workerEnv) wantLine(text string, level slog.Level) log.Entry {
	e.t.Helper()
	entry, ok := e.line(text)
	if !ok {
		e.t.Fatalf("no line contains %q; lines: %s", text, e.dump())
	}
	if entry.Level != level {
		e.t.Fatalf("line %q logged at %v, want %v", entry.Message, entry.Level, level)
	}
	return entry
}

func (e *workerEnv) noLine(text string) {
	e.t.Helper()
	if entry, ok := e.line(text); ok {
		e.t.Fatalf("unexpected line %q at %v", entry.Message, entry.Level)
	}
}

// noLineAt fails when a line since the last run began is at level or above.
func (e *workerEnv) noLineAt(level slog.Level) {
	e.t.Helper()
	for _, entry := range e.lines() {
		if entry.Level >= level {
			e.t.Fatalf("unexpected %v line %q", entry.Level, entry.Message)
		}
	}
}

func (e *workerEnv) dump() string {
	var b strings.Builder
	for _, entry := range e.lines() {
		b.WriteString("\n  " + entry.Level.String() + " " + entry.Message)
	}
	return b.String()
}

// payload reads the file as a reader does at the clock's time, and returns
// nil when there is no file.
func (e *workerEnv) payload() *endpointcache.Payload {
	e.t.Helper()
	p, _, err := e.store.ReadPayload(e.clock.Now(), nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("ReadPayload: %v", err)
	}
	return p
}

func (e *workerEnv) filePath() string { return filepath.Join(e.dir, e.store.FileName()) }

// tempFiles lists the temporary files a write left behind.
func (e *workerEnv) tempFiles() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	var temps []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), privatefile.TempPrefix) {
			temps = append(temps, entry.Name())
		}
	}
	return temps
}

func cachedNodeNames(p *endpointcache.Payload) []string {
	if p == nil || p.Discovery == nil {
		return nil
	}
	names := make([]string, 0, len(p.Discovery.Nodes))
	for _, node := range p.Discovery.Nodes {
		names = append(names, node.Name)
	}
	return names
}

// replaceRoot gives the cluster a second root CA, which is a new identity.
func (d *discCluster) replaceRoot() {
	root, fingerprint, der := clusterIdentityRootFixture(d.t, 12, true)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.root, d.rootFP, d.rootDER = root, fingerprint, der
}

// setNodeFingerprints reports every node with a /nodes fingerprint made from
// label, and makes every certificates/info read but local's fail, so each
// peer's observed pin set holds only the new fingerprint.
func (d *discCluster) setNodeFingerprints(label, local string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodeRows = nil
	for _, name := range d.names {
		fingerprint, _ := discFingerprint(label + name)
		d.nodeRows = append(d.nodeRows, jsonRow(d.t, map[string]any{
			"node": name, "status": "online", "ssl_fingerprint": fingerprint,
		}))
		if name != local {
			d.certFailure[name] = errors.New("certificates/info failed")
		}
	}
}

// makeStandalone makes the cluster a single host that is in no cluster.
func (d *discCluster) makeStandalone() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names = []string{"pve1"}
	d.nodeRows = []json.RawMessage{d.nodeRow("pve1", true)}
	d.statusRows = []json.RawMessage{
		jsonRow(d.t, map[string]any{"type": "node", "name": "pve1", "online": 1, "local": 1}),
	}
}

func (d *discCluster) set(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f()
}

func TestDiscoveryWorker_FirstRefreshCachesTheNodes(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()

	p := e.payload()
	if got := cachedNodeNames(p); strings.Join(got, ",") != "pve2,pve3" {
		t.Fatalf("cached nodes %v, want pve2 and pve3", got)
	}
	if p.Discovery.Outcome != endpointcache.OutcomeOK || !p.Discovery.Quorate {
		t.Fatalf("discovery %+v, want a quorate ok", p.Discovery)
	}
	if want := e.clock.Now().Add(endpointcache.RefreshInterval); !p.Discovery.NextRefreshAfter.Equal(want) {
		t.Fatalf("next refresh %v, want %v", p.Discovery.NextRefreshAfter, want)
	}
	if p.Cluster == nil || p.Cluster.ObservedVia != workerSeed {
		t.Fatalf("identity %+v, want it observed via the seed", p.Cluster)
	}
	e.wantLine("api endpoint discovery: cached 2 of 3 nodes of cluster "+p.Cluster.RootCASHA256[:8]+
		" via "+workerSeed+" (quorate, 2 online)", log.LevelInfo)
	if e.pool.drops.Load() != 0 {
		t.Fatal("a first use must not drop the discovered endpoints")
	}
}

func TestDiscoveryWorker_EveryRequestIsConfiguredOnly(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	total, unmarked, kinds := e.reqs.snapshot()
	if total == 0 {
		t.Fatal("the refresh sent no request")
	}
	if len(unmarked) > 0 {
		t.Fatalf("requests without the configured-only marker: %v", unmarked)
	}
	for _, kind := range []string{"nodes", "status", "certificates"} {
		if kinds[kind] == 0 {
			t.Fatalf("the refresh sent no %s request; sent %v", kind, kinds)
		}
	}
}

func TestDiscoveryWorker_DueConditions(t *testing.T) {
	t.Parallel()
	t.Run("a fresh node list is not due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(59 * time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh that is not due sent %d requests", n)
		}
		e.wantLine("api endpoint discovery: no refresh due", log.LevelDebug)
	})
	t.Run("a passed next refresh is due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		if e.requests() == 0 {
			t.Fatal("a refresh past next_refresh_after sent no request")
		}
		if got := e.payload().Discovery.DiscoveredAt; !got.Equal(e.clock.Now()) {
			t.Fatalf("discovered_at %v, want the second refresh %v", got, e.clock.Now())
		}
	})
	t.Run("a pin failure in this process after the floor is due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.pool.pinFailed.Store(true)
		e.clock.advance(endpointcache.PinRefreshFloor)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a pin failure within the floor sent %d requests", n)
		}
		e.clock.advance(time.Second)
		e.run()
		if e.requests() == 0 {
			t.Fatal("a pin failure past the floor sent no request")
		}
	})
	t.Run("a pin mark from another process after the floor is due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(endpointcache.PinRefreshFloor + time.Second)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh with no pin failure sent %d requests", n)
		}
		other := e.openStore()
		at := e.clock.Now()
		if _, err := other.UpdatePayload(context.Background(), at,
			func(endpointcache.Stored) (endpointcache.Change, error) {
				return endpointcache.Change{Marks: map[string]endpointcache.DownMark{
					"pve2.dc1.example.com:8006": {DownAt: at, Class: endpointcache.MarkPin},
				}}, nil
			}); err != nil {
			t.Fatalf("write the pin mark: %v", err)
		}
		e.run()
		if e.requests() == 0 {
			t.Fatal("a pin mark past the floor sent no request")
		}
	})
	t.Run("a mark of another class is not a pin failure", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.clock.advance(endpointcache.PinRefreshFloor + time.Second)
		at := e.clock.Now()
		if _, err := e.store.UpdatePayload(context.Background(), at,
			func(endpointcache.Stored) (endpointcache.Change, error) {
				return endpointcache.Change{Marks: map[string]endpointcache.DownMark{
					"pve2.dc1.example.com:8006": {DownAt: at, Class: endpointcache.MarkDial},
				}}, nil
			}); err != nil {
			t.Fatalf("write the dial mark: %v", err)
		}
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a dial mark sent %d requests", n)
		}
	})
	t.Run("an invalid file is due and is replaced", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		if err := os.WriteFile(e.filePath(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.run()
		if got := cachedNodeNames(e.payload()); len(got) != 2 {
			t.Fatalf("cached nodes %v, want the invalid file replaced", got)
		}
	})
	t.Run("a standing conflict is never due", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.cluster.replaceRoot()
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		if p := e.payload(); p.Cluster == nil || !p.Cluster.IdentityConflict {
			t.Fatalf("identity %+v, want a recorded conflict", p.Cluster)
		}
		e.pool.pinFailed.Store(true)
		e.clock.advance(2 * endpointcache.MaxRefreshWait)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a standing conflict sent %d requests", n)
		}
	})
}

func TestDiscoveryWorker_SkipsWhileAnotherProcessHoldsTheDiscoverLock(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	other := e.openStore()
	lock, held, err := other.TryLockDiscovery(context.Background())
	if err != nil || !held {
		t.Fatalf("TryLockDiscovery: held %v, err %v", held, err)
	}
	e.run()
	if n := e.requests(); n != 0 {
		t.Fatalf("a refresh whose discover lock another process holds sent %d requests", n)
	}
	e.wantLine("another process holds the discover lock; skipping the refresh", log.LevelDebug)
	if e.payload() != nil {
		t.Fatal("a skipped refresh wrote the file")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	e.run()
	if e.requests() == 0 || len(cachedNodeNames(e.payload())) != 2 {
		t.Fatal("the refresh did not run once the discover lock was free")
	}
}

func TestDiscoveryWorker_LogLevels(t *testing.T) {
	t.Parallel()
	t.Run("an unchanged node list logs at Debug", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
		e.run()
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("api endpoint discovery: cached 2 of 3 nodes", log.LevelDebug)
	})
	t.Run("a changed node set logs at Info", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.resolver.mu.Lock()
		e.resolver.fail["pve3.dc1.example.com"] = true
		e.resolver.mu.Unlock()
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("api endpoint discovery: cached 1 of 3 nodes", log.LevelInfo)
		e.wantLine("left out node pve3, because its derived name did not resolve", log.LevelDebug)
	})
	for _, tc := range []struct {
		mode   config.DiscoveryMode
		steady slog.Level
	}{
		{config.DiscoveryModeAuto, log.LevelDebug},
		{config.DiscoveryModeOn, log.LevelWarn},
	} {
		t.Run("no cluster status under "+string(tc.mode), func(t *testing.T) {
			t.Parallel()
			e := newWorkerEnv(t, workerEnvOptions{mode: tc.mode})
			e.cluster.set(func() { e.cluster.statusErr = errors.New("HTTP 403") })
			e.run()
			if p := e.payload(); p.Discovery.Outcome != endpointcache.OutcomeNoStatus {
				t.Fatalf("outcome %q, want no_status", p.Discovery.Outcome)
			}
			e.wantLine("cached 2 of 3 nodes of cluster", log.LevelInfo)
			e.wantLine("(no cluster status)", log.LevelInfo)
			e.clock.advance(endpointcache.RefreshInterval)
			e.run()
			e.wantLine("(no cluster status)", tc.steady)
			e.cluster.set(func() { e.cluster.statusErr = nil })
			e.clock.advance(endpointcache.RefreshInterval)
			e.run()
			e.wantLine("(quorate, 2 online)", log.LevelInfo)
		})
		t.Run("zero candidates under "+string(tc.mode), func(t *testing.T) {
			t.Parallel()
			e := newWorkerEnv(t, workerEnvOptions{mode: tc.mode})
			e.resolver.setFailAll(true)
			e.run()
			e.wantLine("api endpoint discovery: 2 of 2 derived names did not resolve "+
				"(pve2.dc1.example.com, pve3.dc1.example.com); next attempt in 24h", log.LevelInfo)
			e.clock.advance(endpointcache.MaxRefreshWait)
			e.run()
			e.wantLine("derived names did not resolve", tc.steady)
		})
		t.Run("a standalone host under "+string(tc.mode), func(t *testing.T) {
			t.Parallel()
			e := newWorkerEnv(t, workerEnvOptions{mode: tc.mode})
			e.cluster.makeStandalone()
			e.run()
			e.wantLine("api endpoint discovery: "+workerSeed+" is a standalone host with no peers; next attempt in 24h",
				log.LevelInfo)
			e.clock.advance(endpointcache.MaxRefreshWait)
			e.run()
			e.wantLine("is a standalone host with no peers", tc.steady)
		})
	}
	t.Run("a public suffix domain", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{seed: "pve1.lan:8006"})
		e.begin()
		w := e.newWorker()
		if err := w.Bind(e.binding()); err != nil {
			t.Fatal(err)
		}
		w.FirstAnswer("pve1.lan:8006")
		e.drain()
		e.wantLine("api endpoint discovery: the domain of pve1.lan:8006 is a public suffix, so no node names are "+
			"derived under it; next attempt in 24h", log.LevelInfo)
		if p := e.payload(); p.Discovery.Outcome != endpointcache.OutcomePublicSuffix {
			t.Fatalf("outcome %q, want public_suffix", p.Discovery.Outcome)
		}
	})
	t.Run("an ambiguous node name is left out with a Debug line", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.cluster.set(func() {
			e.cluster.names = append(e.cluster.names, "localhost")
			e.cluster.nodeRows = append(e.cluster.nodeRows, e.cluster.nodeRow("localhost", true))
		})
		e.run()
		e.wantLine("left out node localhost, because a resolver could read its name as a loopback", log.LevelDebug)
		e.wantLine("cached 2 of 4 nodes", log.LevelInfo)
	})
}

func TestDiscoveryWorker_IdentityChangeUnderVerifySSLTrueReplacesAndLogsError(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()
	before := e.payload().Cluster.RootCASHA256
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	after := e.payload().Cluster.RootCASHA256
	if after == before {
		t.Fatal("the identity was not replaced")
	}
	e.wantLine("endpoint cache: cluster identity changed from "+before[:8]+" to "+after[:8]+
		" through configured endpoint "+workerSeed+"; with verify_ssl true the cached nodes and pins are replaced",
		log.LevelError)
	e.wantLine("cached 2 of 3 nodes", log.LevelInfo)
	if e.pool.drops.Load() != 1 {
		t.Fatalf("DropDiscovered called %d times, want once", e.pool.drops.Load())
	}
}

func TestDiscoveryWorker_IdentityChangeUnderVerifySSLFalseIsRefused(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	before := e.payload()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	errorLine := e.wantLine("with verify_ssl false this is refused, delete the cache file to accept it", log.LevelError)
	if !strings.HasPrefix(errorLine.Message, "endpoint cache: cluster identity changed from "+
		before.Cluster.RootCASHA256[:8]) {
		t.Fatalf("identity line %q does not name the stored identity", errorLine.Message)
	}
	e.noLine("api endpoint discovery: cached")
	p := e.payload()
	if !p.Cluster.IdentityConflict || p.Cluster.RootCASHA256 != before.Cluster.RootCASHA256 {
		t.Fatalf("identity %+v, want the stored one with a conflict", p.Cluster)
	}
	if want := e.clock.Now().Add(endpointcache.MaxRefreshWait); !p.Discovery.NextRefreshAfter.Equal(want) {
		t.Fatalf("next refresh %v, want the attempt's %v", p.Discovery.NextRefreshAfter, want)
	}
	if e.pool.drops.Load() != 1 {
		t.Fatalf("DropDiscovered called %d times, want once", e.pool.drops.Load())
	}
}

// A refresh that meets a standing conflict records its attempt and logs no
// conflict line, because the code that reads the file logs that.
func TestDiscoveryWorker_StandingConflictRecordsAnAttemptAndLogsNothing(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.clock.advance(3 * time.Hour)
	e.begin()
	e.newWorker().discover(context.Background(), workerSeed, e.binding())
	e.noLineAt(log.LevelInfo)
	p := e.payload()
	if !p.Cluster.IdentityConflict {
		t.Fatal("the conflict was lost")
	}
	if !p.Discovery.DiscoveredAt.Equal(e.clock.Now()) ||
		!p.Discovery.NextRefreshAfter.Equal(e.clock.Now().Add(endpointcache.MaxRefreshWait)) {
		t.Fatalf("discovery %+v, want the attempt at %v", p.Discovery, e.clock.Now())
	}
}

func TestDiscoveryWorker_PinChangeWarnsOncePerRefresh(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.setNodeFingerprints("rotated:", "pve1")
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	warns := 0
	for _, entry := range e.lines() {
		if entry.Level == log.LevelWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("%d Warn lines, want one; lines: %s", warns, e.dump())
	}
	e.wantLine("endpoint cache: the pin set of known nodes pve2, pve3 changed through configured endpoint "+
		workerSeed+"; with verify_ssl false the stored pins stay", log.LevelWarn)
}

func TestDiscoveryWorker_StrictShrinkLogsAtDebug(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.set(func() { e.cluster.certFailure["pve2"] = errors.New("certificates/info failed") })
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	e.wantLine("endpoint cache: known node pve2 reported fewer pins than stored", log.LevelDebug)
	e.noLineAt(log.LevelWarn)
}

// A drain that cancels the worker after its decision and before its write
// leaves the earlier file whole and no temporary file.
func TestDiscoveryWorker_DrainCutsTheWriteOffWithoutATornFile(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	before := e.payload()
	e.resolver.mu.Lock()
	e.resolver.fail["pve3.dc1.example.com"] = true
	e.resolver.mu.Unlock()
	e.clock.advance(endpointcache.RefreshInterval)
	e.begin()
	w := e.newWorker()
	var drained atomic.Bool
	w.beforeWrite = func(context.Context) {
		e.registry.Drain(context.Background(), 0)
		drained.Store(true)
	}
	done := make(chan struct{})
	b := e.binding()
	if err := e.registry.Go(discoveryWorkerName, e.mode, func(ctx context.Context) {
		defer close(done)
		w.refresh(ctx, workerSeed, b)
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	e.registry.Open()
	if !drained.Load() {
		t.Fatal("the refresh never reached its write")
	}
	after := e.payload()
	if strings.Join(cachedNodeNames(after), ",") != strings.Join(cachedNodeNames(before), ",") ||
		!after.Discovery.DiscoveredAt.Equal(before.Discovery.DiscoveredAt) {
		t.Fatalf("the cancelled write changed the file: %+v", after.Discovery)
	}
	if temps := e.tempFiles(); len(temps) > 0 {
		t.Fatalf("temporary files left behind: %v", temps)
	}
	e.noLine("api endpoint discovery: cached")
	e.noLine("the cache write failed")
}

func TestDiscoveryWorker_FailedWriteIsDroppedWithADebugLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level slog.Level
	}{
		{config.DiscoveryModeAuto, log.LevelDebug},
		{config.DiscoveryModeOn, log.LevelWarn},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			e := newWorkerEnv(t, workerEnvOptions{mode: tc.mode})
			// A directory in the file's place makes the rename fail.
			e.run(func(w *DiscoveryWorker) {
				w.beforeWrite = func(context.Context) {
					if err := os.Mkdir(e.filePath(), 0o700); err != nil {
						t.Error(err)
					}
				}
			})
			e.wantLine("api endpoint discovery: the cache write failed; the discovery result is dropped", tc.level)
			e.noLine("api endpoint discovery: cached")
			if temps := e.tempFiles(); len(temps) > 0 {
				t.Fatalf("temporary files left behind: %v", temps)
			}
		})
	}
}

func TestDiscoveryWorker_ExpiredLockWaitSkipsTheWrite(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	path := filepath.Join(e.dir, e.store.Key().LockName())
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	e.run()
	e.wantLine("endpoint cache lock wait expired; skipping the cache write", log.LevelDebug)
	e.noLine("api endpoint discovery: cached")
	e.noLine("the cache write failed")
	if e.payload() != nil {
		t.Fatal("a refresh whose lock wait expired wrote the file")
	}
}

func TestDiscoveryWorker_BackOffHoldsForADay(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(e *workerEnv){
		"zero candidates": func(e *workerEnv) { e.resolver.setFailAll(true) },
		"standalone host": func(e *workerEnv) { e.cluster.makeStandalone() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newWorkerEnv(t, workerEnvOptions{})
			setup(e)
			e.run()
			// A pin failure does not cut the wait short, because there is
			// no discovered node to fail one.
			e.pool.pinFailed.Store(true)
			for _, step := range []time.Duration{2 * time.Hour, 21*time.Hour + 59*time.Minute + 59*time.Second} {
				e.clock.advance(step)
				e.run()
				if n := e.requests(); n != 0 {
					t.Fatalf("a refresh inside the 24-hour wait sent %d requests", n)
				}
			}
			e.clock.advance(time.Second)
			e.run()
			if e.requests() == 0 {
				t.Fatal("the refresh did not run once the 24-hour wait passed")
			}
		})
	}
}

func TestDiscoveryWorker_ReadFailures(t *testing.T) {
	t.Parallel()
	rootless := func(e *workerEnv, gone bool) {
		e.cluster.set(func() {
			for _, name := range e.cluster.names {
				e.cluster.rootless[name] = gone
			}
		})
	}
	// failureOnly checks that p holds only the failed attempt e just made.
	failureOnly := func(t *testing.T, e *workerEnv, p *endpointcache.Payload, failure endpointcache.DiscoveryFailure) {
		t.Helper()
		if p == nil || p.Discovery == nil || p.Cluster != nil {
			t.Fatalf("payload %+v, want a discovery section and no identity", p)
		}
		d := p.Discovery
		if d.LastFailure != failure || d.Outcome != "" || len(d.Nodes) != 0 || !d.DiscoveredAt.Equal(e.clock.Now()) ||
			!d.NextRefreshAfter.Equal(e.clock.Now().Add(endpointcache.RefreshInterval)) {
			t.Fatalf("discovery %+v, want only the %s attempt at %v", d, failure, e.clock.Now())
		}
	}
	t.Run("an unreadable root CA on a fresh cache records the failure and waits an hour", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		rootless(e, true)
		e.run()
		line := e.wantLine("api endpoint discovery: no result through "+workerSeed+
			", because the cluster root CA could not be read; next attempt in 1h", log.LevelInfo)
		if got := line.Attrs["failure"]; got != string(endpointcache.FailureRootCAUnreadable) {
			t.Fatalf("failure attribute %v", got)
		}
		failureOnly(t, e, e.payload(), endpointcache.FailureRootCAUnreadable)
		// The attempt holds the next refresh back on a fresh cache too.
		e.clock.advance(30 * time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh inside the attempt's wait sent %d requests", n)
		}
		// The same failure an hour later logs at Debug.
		e.clock.advance(30 * time.Minute)
		e.run()
		e.wantLine("because the cluster root CA could not be read", log.LevelDebug)
		failureOnly(t, e, e.payload(), endpointcache.FailureRootCAUnreadable)
	})
	t.Run("the same failure logs at Warn under on", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
		rootless(e, true)
		e.run()
		e.wantLine("because the cluster root CA could not be read", log.LevelInfo)
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("because the cluster root CA could not be read", log.LevelWarn)
	})
	t.Run("an unreadable root CA after a result keeps the nodes, and the recovery logs at Info", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		before := e.payload()
		rootless(e, true)
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("because the cluster root CA could not be read; next attempt in 1h", log.LevelInfo)
		p := e.payload()
		if strings.Join(cachedNodeNames(p), ",") != strings.Join(cachedNodeNames(before), ",") ||
			p.Cluster.RootCASHA256 != before.Cluster.RootCASHA256 || p.Discovery.Outcome != before.Discovery.Outcome ||
			!reflect.DeepEqual(p.Discovery.Nodes, before.Discovery.Nodes) {
			t.Fatalf("a refresh with no root CA changed the result: %+v", p.Discovery)
		}
		if p.Discovery.LastFailure != endpointcache.FailureRootCAUnreadable ||
			!p.Discovery.DiscoveredAt.Equal(e.clock.Now()) ||
			!p.Discovery.NextRefreshAfter.Equal(e.clock.Now().Add(endpointcache.RefreshInterval)) {
			t.Fatalf("discovery %+v, want the attempt recorded at %v", p.Discovery, e.clock.Now())
		}
		e.clock.advance(30 * time.Minute)
		e.run()
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh inside the attempt's wait sent %d requests", n)
		}
		// The root CA is readable again, and the same node list is cached
		// at Info because the result follows a failure.
		rootless(e, false)
		e.clock.advance(30 * time.Minute)
		e.run()
		e.wantLine("api endpoint discovery: cached 2 of 3 nodes", log.LevelInfo)
		if p := e.payload(); p.Discovery.LastFailure != "" {
			t.Fatalf("a result left last_failure %q", p.Discovery.LastFailure)
		}
		// The next unchanged result is back at Debug.
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("api endpoint discovery: cached 2 of 3 nodes", log.LevelDebug)
	})
	t.Run("a new reason logs at Info again", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		rootless(e, true)
		e.run()
		e.cluster.set(func() { e.cluster.nodesErr = errors.New("HTTP 500") })
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("because GET /nodes gave no usable node list", log.LevelInfo)
		failureOnly(t, e, e.payload(), endpointcache.FailureNodesUnreadable)
	})
	t.Run("a failed node list keeps the cached nodes", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.run()
		e.cluster.set(func() { e.cluster.nodesErr = errors.New("HTTP 500") })
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("because GET /nodes gave no usable node list", log.LevelInfo)
		p := e.payload()
		if len(cachedNodeNames(p)) != 2 || p.Discovery.LastFailure != endpointcache.FailureNodesUnreadable {
			t.Fatalf("discovery %+v, want the two cached nodes and the nodes_unreadable failure", p.Discovery)
		}
	})
	t.Run("an empty node list records its own failure", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.cluster.set(func() { e.cluster.nodeRows = nil })
		e.run()
		e.wantLine("because GET /nodes gave no usable node list", log.LevelInfo)
		failureOnly(t, e, e.payload(), endpointcache.FailureNodesEmpty)
	})
	t.Run("an invalid node name records its own failure", func(t *testing.T) {
		t.Parallel()
		e := newWorkerEnv(t, workerEnvOptions{})
		e.cluster.set(func() {
			e.cluster.nodeRows = append(e.cluster.nodeRows, jsonRow(t, map[string]any{
				"node": "api.attacker.example", "status": "online",
			}))
		})
		e.run()
		e.wantLine("because a node name from GET /nodes is not a valid pve-node label", log.LevelInfo)
		failureOnly(t, e, e.payload(), endpointcache.FailureNodeNameInvalid)
	})
}

func TestDiscoveryWorker_StartsOnceAndAfterBind(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	w := e.newWorker()
	// An answer before Bind is kept, and Bind starts its refresh.
	w.FirstAnswer(workerSeed)
	w.FirstAnswer(workerSeed)
	if err := w.Bind(e.binding()); err != nil {
		t.Fatal(err)
	}
	w.FirstAnswer(workerSeed)
	e.drain()
	if total, _, kinds := e.reqs.snapshot(); total == 0 || kinds["nodes"] != 1 {
		t.Fatalf("the worker sent %v, want one refresh", kinds)
	}
	if err := w.Bind(e.binding()); !errors.Is(err, errDiscoveryWorkerBound) {
		t.Fatalf("a second Bind returned %v", err)
	}
}

func TestDiscoveryWorker_RefusesBadInput(t *testing.T) {
	t.Parallel()
	if _, err := NewDiscoveryWorker(DiscoveryWorkerConfig{}); !errors.Is(err, errDiscoveryWorkerStore) {
		t.Fatalf("a worker without a store returned %v", err)
	}
	e := newWorkerEnv(t, workerEnvOptions{})
	if err := e.newWorker().Bind(DiscoveryBinding{Pool: e.pool}); !errors.Is(err, errDiscoveryWorkerClient) {
		t.Fatalf("a binding without a client returned %v", err)
	}
	if err := e.newWorker().Bind(DiscoveryBinding{Client: markedClient(e.cluster, e.reqs)}); !errors.Is(err,
		errDiscoveryWorkerPool) {
		t.Fatalf("a binding without a pool returned %v", err)
	}
	var nilWorker *DiscoveryWorker
	nilWorker.FirstAnswer(workerSeed)

	t.Run("an endpoint outside the key", func(t *testing.T) {
		e.begin()
		w := e.newWorker()
		if err := w.Bind(e.binding()); err != nil {
			t.Fatal(err)
		}
		w.FirstAnswer("pve9.dc1.example.com:8006")
		e.drain()
		e.wantLine("the answering endpoint is not one of the configured endpoints", log.LevelDebug)
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh through an unconfigured endpoint sent %d requests", n)
		}
	})
	t.Run("a closed registry", func(t *testing.T) {
		e.begin()
		w := e.newWorker()
		if err := w.Bind(e.binding()); err != nil {
			t.Fatal(err)
		}
		e.registry.Drain(context.Background(), 0)
		w.FirstAnswer(workerSeed)
		e.registry.Open()
		e.wantLine("api endpoint discovery: refresh not started", log.LevelDebug)
	})
	t.Run("an unreadable cache file", func(t *testing.T) {
		e.run()
		if err := os.Chmod(e.filePath(), 0o644); err != nil { // #nosec G302 -- the test breaks the file's mode
			t.Fatal(err)
		}
		e.clock.advance(endpointcache.RefreshInterval)
		e.run()
		e.wantLine("could not read the cache file; skipping the refresh", log.LevelDebug)
		if n := e.requests(); n != 0 {
			t.Fatalf("a refresh with an unreadable file sent %d requests", n)
		}
	})
}

func TestDiscoveryWorker_Helpers(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		24 * time.Hour: "24h", time.Hour: "1h", 10 * time.Minute: "10m", 90 * time.Second: "1m30s", 0: "0s",
	} {
		if got := waitText(d); got != want {
			t.Errorf("waitText(%v) = %q, want %q", d, got, want)
		}
	}
	names := make([]string, 12)
	for i := range names {
		names[i] = "n" + string(rune('a'+i))
	}
	if got := nameList(names); !strings.HasSuffix(got, "nj, and 2 more") {
		t.Errorf("nameList = %q", got)
	}
	if shortID("") != "none" || shortID("abcdef0123") != "abcdef01" {
		t.Error("shortID")
	}
	if _, err := cacheOutcome("bogus"); !errors.Is(err, endpointcache.ErrInvalidChange) {
		t.Errorf("cacheOutcome(bogus) = %v", err)
	}
	for _, outcome := range []DiscoveryOutcome{
		DiscoveryOK, DiscoveryNoStatus, DiscoveryNoCandidates, DiscoveryStandalone, DiscoveryPublicSuffix,
	} {
		got, err := cacheOutcome(outcome)
		if err != nil || string(got) != string(outcome) || !got.Valid() {
			t.Errorf("cacheOutcome(%q) = %q, %v", outcome, got, err)
		}
	}
	for _, reason := range []DropReason{
		DropAmbiguousName, DropDuplicateName, DropSeedNode, DropNoFingerprint, DropPublicSuffix,
		DropUnaddressable, DropConfigured, DropUnresolved, DropOverCap,
	} {
		if dropReasonText(reason) == string(reason) {
			t.Errorf("drop reason %q has no text", reason)
		}
	}
	for err, want := range map[error]string{
		context.DeadlineExceeded: "ran out of time",
		ErrDiscoveryInput:        "cannot derive node names",
		errors.New("other"):      "the discovery reads failed",
	} {
		if got := discoveryFailureReason(err); !strings.Contains(got, want) {
			t.Errorf("discoveryFailureReason(%v) = %q", err, got)
		}
	}
	if statusText(DiscoveryStatus{Available: true}) != "not quorate" {
		t.Error("statusText for a status without quorum")
	}
	for err, want := range map[error]endpointcache.DiscoveryFailure{
		&discoveryReadError{
			base: ErrDiscoveryNodes, cause: errors.New("HTTP 500"),
		}: endpointcache.FailureNodesUnreadable,
		ErrDiscoveryNodes:    endpointcache.FailureNodesEmpty,
		ErrDiscoveryNodeName: endpointcache.FailureNodeNameInvalid,
		&discoveryReadError{
			base: ErrDiscoveryRoot, cause: errors.New("HTTP 403"),
		}: endpointcache.FailureRootCAUnreadable,
		fmt.Errorf("ReadDiscovery: read ended: %w", context.DeadlineExceeded): endpointcache.FailureReadsTimedOut,
		ErrDiscoveryInput:   endpointcache.FailureReadsFailed,
		errors.New("other"): endpointcache.FailureReadsFailed,
	} {
		if got := discoveryFailure(err); got != want || !got.Valid() {
			t.Errorf("discoveryFailure(%v) = %q, want %q", err, got, want)
		}
	}
}

// A refresh the drain cancels during its reads records no attempt and logs no
// failure line, because the drain reports the cancelled worker.
func TestDiscoveryWorker_CancelledReadsWriteAndLogNothing(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	before := e.payload()
	e.cluster.set(func() { e.cluster.hang["pve1"] = true })
	e.clock.advance(endpointcache.RefreshInterval)
	e.begin()
	w := e.newWorker()
	b := e.binding()
	done := make(chan struct{})
	if err := e.registry.Go(discoveryWorkerName, e.mode, func(ctx context.Context) {
		defer close(done)
		w.refresh(ctx, workerSeed, b)
	}); err != nil {
		t.Fatal(err)
	}
	res := e.registry.Drain(context.Background(), 50*time.Millisecond)
	<-done
	e.registry.Open()
	if len(res.Cancelled) != 1 {
		t.Fatalf("drain cancelled %v, want the discovery worker", res.Cancelled)
	}
	e.noLine("api endpoint discovery: no result")
	e.noLine("the cache write failed")
	if p := e.payload(); !p.Discovery.DiscoveredAt.Equal(before.Discovery.DiscoveredAt) {
		t.Fatalf("a cancelled refresh recorded an attempt: %+v", p.Discovery)
	}
}

func TestDiscoveryWorker_DiscoverLockErrorSkipsTheRefresh(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{mode: config.DiscoveryModeOn})
	path := filepath.Join(e.dir, e.store.Key().DiscoverLockName())
	if err := os.WriteFile(path, nil, 0o644); err != nil { // #nosec G306 -- the test breaks the lock file's mode
		t.Fatal(err)
	}
	e.run()
	e.wantLine("api endpoint discovery: could not take the discover lock; skipping the refresh", log.LevelWarn)
	if n := e.requests(); n != 0 {
		t.Fatalf("a refresh without its discover lock sent %d requests", n)
	}
}

// A refresh that another process finished between the due check and the
// discover lock is not run again, because the worker checks again under the
// lock.
func TestDiscoveryWorker_ChecksAgainUnderTheDiscoverLock(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.clock.advance(endpointcache.RefreshInterval)
	e.begin()
	other := e.newWorker()
	otherBinding := e.binding()
	var first atomic.Bool
	b := e.binding()
	// The first due check asks for the pool's pin state, and the other
	// process finishes its refresh at that moment.
	b.Pool = &discPool{refreshDue: func() bool {
		if first.CompareAndSwap(false, true) {
			other.discover(context.Background(), workerSeed, otherBinding)
		}
		return false
	}}
	w := e.newWorker()
	if err := w.Bind(b); err != nil {
		t.Fatal(err)
	}
	w.FirstAnswer(workerSeed)
	e.drain()
	if _, _, kinds := e.reqs.snapshot(); kinds["nodes"] != 2 {
		t.Fatalf("GET /nodes ran %d times in all, want the first run's and the other process's only", kinds["nodes"])
	}
	e.wantLine("api endpoint discovery: no refresh due", log.LevelDebug)
}

// The verify_ssl false identity refusal takes every discovered endpoint out of
// the real pool the worker is bound to, so a request to one is refused before
// any dial.
func TestDiscoveryWorker_IdentityRefusalDropsThePoolsDiscoveredEndpoints(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{})
	e.run()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)

	// A loopback port nothing listens on, so a request the drop failed to
	// refuse ends in a local dial refusal and never leaves the host.
	discoveredAddr := "127.0.0.1:9"
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
	e.begin()
	w := e.newWorker()
	if err := w.Bind(DiscoveryBinding{Client: markedClient(e.cluster, e.reqs), Pool: pool}); err != nil {
		t.Fatal(err)
	}
	w.FirstAnswer(workerSeed)
	e.drain()
	e.wantLine("with verify_ssl false this is refused", log.LevelError)
	if n := pool.DropDiscovered(); n != 0 {
		t.Fatalf("the worker left %d discovered endpoints in the pool", n)
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
	var certErr *tls.CertificateVerificationError
	if errors.Is(err, endpointpool.ErrPinMismatch) || !errors.As(err, &certErr) ||
		!endpointpool.IsConnectPhase(err) || endpointpool.IsMaybeSent(err) ||
		!strings.Contains(err.Error(), "the cluster identity changed") {
		t.Fatalf("a request to a dropped endpoint should be refused unsent with the drop's cause, got %v", err)
	}
}
