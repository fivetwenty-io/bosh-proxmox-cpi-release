package pve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	cacheTestHost  = "pve1.dc1.example.com"
	cacheTestPort  = 8006
	cacheTestToken = "cpi@pve!probe=0123456789abcdef"
)

// newCacheDir makes a private directory for a cache and returns its path.
func newCacheDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cacheTestConfig is a token config for one host whose cache directory is dir.
func cacheTestConfig(mode config.DiscoveryMode, dir string) *config.CPIConfig {
	verify := true
	return &config.CPIConfig{
		Host:                 cacheTestHost,
		Port:                 cacheTestPort,
		APIToken:             cacheTestToken,
		APIEndpointDiscovery: mode,
		VerifySSL:            &verify,
		EndpointCacheDir:     dir,
	}
}

// pinHex is a pin of 32 copies of b, in the hex form the cache stores.
func pinHex(b byte) string { return strings.Repeat(fmt.Sprintf("%02x", b), 32) }

// cachedNode is one node of a seeded cache file.
type cachedNode struct {
	name   string
	online bool
}

// cacheSeed describes the cache file a test starts from.
type cacheSeed struct {
	nodes []cachedNode
	// marks are keyed by the address of the endpoint they name.
	marks map[string]endpointcache.DownMark
}

// seedEndpointCache writes a cache file for cfg, the way an earlier process
// would have, under the key the client derives from the plan.
func seedEndpointCache(t *testing.T, cfg *config.CPIConfig, seed cacheSeed) {
	t.Helper()
	_, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatalf("wireEndpoints: %v", err)
	}
	plan := wiring.plan
	user, credential := cacheCredential(cfg)
	key := endpointcache.KeyConfig{
		Endpoints: configuredAddresses(&plan), VerifySSL: cfg.VerifySSLValue(), CACert: cfg.PVECACertPEM, User: user,
	}
	choice := endpointcache.OptionsFor(cfg).Resolve()
	if !choice.Usable() {
		t.Fatalf("the seed directory is not usable: %v", choice.Err)
	}
	ctx := context.Background()
	store, err := endpointcache.OpenStore(ctx, choice.Root, key, credential,
		endpointcache.StoreOptions{Logger: log.NewNopLogger(), ConfiguredMode: cfg.DiscoveryModeValue()})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()

	now := time.Now().UTC().Truncate(time.Second)
	section := endpointcache.DiscoverySection{
		DiscoveredAt: now.Add(-5 * time.Minute), NextRefreshAfter: now.Add(time.Hour),
		Outcome: endpointcache.OutcomeOK, Quorate: true,
	}
	pool := make([]string, 0, len(seed.nodes))
	for i, n := range seed.nodes {
		address, err := endpointcache.DeriveAddress(plan.endpoints[0].Address, n.name)
		if err != nil {
			t.Fatalf("DeriveAddress(%s): %v", n.name, err)
		}
		host, portText, _ := net.SplitHostPort(address)
		port, _ := strconv.Atoi(portText)
		section.Nodes = append(section.Nodes, endpointcache.DiscoveredNode{
			Name: n.name, Address: host, Port: port, Pins: []string{pinHex(byte(0xa0 + i)), pinHex(byte(0xb0 + i))},
			Online: n.online, ConfirmedAt: now.Add(-5 * time.Minute),
		})
		pool = append(pool, address)
	}
	change := endpointcache.Change{
		Result: &endpointcache.DiscoveryResult{
			Cluster: endpointcache.ClusterIdentity{
				RootCASHA256: pinHex(0x3c), ObservedAt: now.Add(-5 * time.Minute),
				ObservedVia: plan.endpoints[0].Address,
			},
			Discovery: section,
		},
		Marks: seed.marks,
		Pool:  pool,
	}
	_, err = store.UpdatePayload(ctx, now, func(endpointcache.Stored) (endpointcache.Change, error) {
		return change, nil
	})
	if err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
}

// readSeededCache reads the cache file of cfg back, as a later process would.
func readSeededCache(t *testing.T, cfg *config.CPIConfig) *endpointcache.Payload {
	t.Helper()
	_, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatalf("wireEndpoints: %v", err)
	}
	user, credential := cacheCredential(cfg)
	key := endpointcache.KeyConfig{
		Endpoints: configuredAddresses(&wiring.plan), VerifySSL: cfg.VerifySSLValue(), CACert: cfg.PVECACertPEM,
		User: user,
	}
	choice := endpointcache.OptionsFor(cfg).Resolve()
	if !choice.Usable() {
		t.Fatalf("the directory is not usable: %v", choice.Err)
	}
	store, err := endpointcache.OpenStore(context.Background(), choice.Root, key, credential,
		endpointcache.StoreOptions{Logger: log.NewNopLogger(), ConfiguredMode: cfg.DiscoveryModeValue()})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	payload, _, err := store.ReadPayload(time.Now(), nil)
	if err != nil {
		t.Fatalf("ReadPayload: %v", err)
	}
	return payload
}

// drainProcessRegistry waits for the background work of the process registry,
// runs the after-drain releases, and opens the registry for the next test.
func drainProcessRegistry(t *testing.T) endpointcache.DrainResult {
	t.Helper()
	result := endpointcache.Drain(context.Background(), 30*time.Second)
	endpointcache.Open()
	if len(result.Cancelled) > 0 || len(result.Panics) > 0 {
		t.Fatalf("the drain cancelled %v and met panics %v", result.Cancelled, result.Panics)
	}
	return result
}

// linesAt returns the messages at level that contain text.
func linesAt(entries []log.Entry, level slog.Level, text string) []string {
	var out []string
	for _, e := range entries {
		if e.Level == level && strings.Contains(e.Message, text) {
			out = append(out, e.Message)
		}
	}
	return out
}

// anyLineAbove reports the first entry above level.
func anyLineAbove(entries []log.Entry, level slog.Level) (log.Entry, bool) {
	for _, e := range entries {
		if e.Level > level {
			return e, true
		}
	}
	return log.Entry{}, false
}

// openedCache plans cfg and opens its endpoint cache with an observed logger.
type openedCache struct {
	cache *endpointCache
	plan  endpointPlan
	obs   *log.Observer
}

func openCacheFor(t *testing.T, cfg *config.CPIConfig) openedCache {
	t.Helper()
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	_, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatalf("wireEndpoints: %v", err)
	}
	cache := openEndpointCache(cfg, &wiring.plan, logger)
	t.Cleanup(cache.release)
	return openedCache{cache: cache, plan: wiring.plan, obs: obs}
}

func TestOpenEndpointCacheLoadsCachedNodesAndMarks(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	markedAt := time.Now().UTC().Add(-20 * time.Second).Truncate(time.Second)
	seedEndpointCache(t, cfg, cacheSeed{
		nodes: []cachedNode{{"pve2", true}, {"pve3", false}},
		marks: map[string]endpointcache.DownMark{
			net.JoinHostPort(cacheTestHost, "8006"): {DownAt: markedAt, Class: endpointcache.MarkDial},
		},
	})

	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatalf("no cache; lines: %v", got.obs.All())
	}
	if got.plan.configured != 1 || len(got.plan.endpoints) != 3 {
		t.Fatalf("plan holds %d endpoints with %d configured, want 3 with 1", len(got.plan.endpoints),
			got.plan.configured)
	}
	first := got.plan.endpoints[0]
	if first.Kind != endpointpool.KindConfigured || first.Online || !first.DownMarkedAt.Equal(markedAt) {
		t.Errorf("configured endpoint = %+v, want offline flag unset and the cached mark time", first)
	}
	for i, want := range []struct {
		address string
		node    string
		online  bool
	}{{"pve2.dc1.example.com:8006", "pve2", true}, {"pve3.dc1.example.com:8006", "pve3", false}} {
		ep := got.plan.endpoints[1+i]
		if ep.Address != want.address || ep.Kind != endpointpool.KindDiscovered || ep.Node != want.node ||
			ep.Online != want.online || len(ep.Pins) != 2 {
			t.Errorf("discovered endpoint %d = %+v, want %+v with two pins", i, ep, want)
		}
	}
	if lines := linesAt(got.obs.All(), slog.LevelDebug, "pve endpoints: cache directory "); len(lines) != 1 ||
		!strings.Contains(lines[0], "from pve.endpoint_cache_dir") {
		t.Errorf("cache directory lines = %v, want one Debug line naming the override", lines)
	}
	if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
		t.Errorf("a default-level call logged %q at %v", e.Message, e.Level)
	}
}

func TestOpenEndpointCacheNeverSetsOnlineOnAConfiguredEndpoint(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
	cfg.Endpoints = []string{
		net.JoinHostPort(cacheTestHost, "8006"), net.JoinHostPort("pve9.dc1.example.com", "8006"),
	}
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})

	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatalf("no cache; lines: %v", got.obs.All())
	}
	for _, ep := range got.plan.endpoints[:got.plan.configured] {
		if ep.Online {
			t.Errorf("configured endpoint %s has Online set", ep.Address)
		}
	}
	if len(got.plan.endpoints) != 3 || !got.plan.endpoints[2].Online {
		t.Errorf("endpoints = %+v, want the cached online node last", got.plan.endpoints)
	}
}

func TestOpenEndpointCacheKeyComesFromThePlanForAHostOnlyConfig(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	if len(cfg.Endpoints) != 0 {
		t.Fatal("the config must list no endpoints, only pve.host")
	}
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})

	if got := openCacheFor(t, cfg); got.cache == nil || len(got.plan.endpoints) != 2 {
		t.Fatalf("the cache did not open under the key from the plan: %+v", got.plan.endpoints)
	}

	// Another host reads the same directory under another key, so the file is
	// no cache for it.
	other := cacheTestConfig(config.DiscoveryModeAuto, cfg.EndpointCacheDir)
	other.Host = "pve7.dc1.example.com"
	got := openCacheFor(t, other)
	if got.cache == nil || len(got.plan.endpoints) != 1 {
		t.Fatalf("another host's cache must hold no nodes: %+v", got.plan.endpoints)
	}
}

func TestOpenEndpointCacheModeTable(t *testing.T) {
	cases := []struct {
		name     string
		mode     config.DiscoveryMode
		password bool
		verify   bool
		wantOpen bool
		// reasonAt is the level of the config's reason line, or zero for none.
		wantReason bool
	}{
		{"token under auto", config.DiscoveryModeAuto, false, true, true, false},
		{"token under on", config.DiscoveryModeOn, false, true, true, false},
		{"token under off", config.DiscoveryModeOff, false, true, false, false},
		{"password under auto", config.DiscoveryModeAuto, true, true, false, true},
		{"password under on with verify", config.DiscoveryModeOn, true, true, true, false},
		{"token under auto without verify", config.DiscoveryModeAuto, false, false, true, false},
		{"password under on without verify", config.DiscoveryModeOn, true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newCacheDir(t)
			cfg := cacheTestConfig(tc.mode, dir)
			cfg.VerifySSL = &tc.verify
			if tc.password {
				cfg.APIToken = ""
				cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
			}
			got := openCacheFor(t, cfg)
			effective, reason := config.EffectiveDiscoveryMode(cfg)
			if (got.cache != nil) != tc.wantOpen || (effective == config.DiscoveryModeOn) != tc.wantOpen {
				t.Fatalf("cache open = %v and effective mode %q, want open = %v", got.cache != nil, effective,
					tc.wantOpen)
			}
			names, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantOpen && len(names) != 0 {
				t.Errorf("a client with discovery off left %d entries in the cache directory", len(names))
			}
			if tc.wantOpen && len(names) == 0 {
				t.Error("a client with discovery on opened no file in the cache directory")
			}
			if tc.wantReason {
				if lines := linesAt(got.obs.All(), slog.LevelDebug, reason); len(lines) != 1 || reason == "" {
					t.Errorf("reason %q logged as %v, want one Debug line under auto", reason, lines)
				}
			}
			if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
				t.Errorf("the call logged %q at %v", e.Message, e.Level)
			}
			if got.plan.cacheUnusable {
				t.Error("a mode that resolves to off is not an unusable directory")
			}
		})
	}
}

func TestOpenEndpointCachePasswordUnderAutoCreatesNoFile(t *testing.T) {
	dir := newCacheDir(t)
	cfg := cacheTestConfig(config.DiscoveryModeAuto, dir)
	cfg.APIToken = ""
	cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
	if got := openCacheFor(t, cfg); got.cache != nil {
		t.Fatal("password auth under auto opened a cache")
	}
	if names, err := os.ReadDir(dir); err != nil || len(names) != 0 {
		t.Fatalf("directory holds %v (err %v), want no file", names, err)
	}
	if endpointDiscoveryOn(cfg) {
		t.Error("the config-only answer must be off for password auth under auto")
	}
}

func TestOpenEndpointCacheUnusableDirectoryLevels(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{missing, loose} {
		for _, tc := range []struct {
			mode  config.DiscoveryMode
			level slog.Level
		}{{config.DiscoveryModeAuto, slog.LevelDebug}, {config.DiscoveryModeOn, slog.LevelWarn}} {
			t.Run(filepath.Base(dir)+" under "+string(tc.mode), func(t *testing.T) {
				cfg := cacheTestConfig(tc.mode, dir)
				got := openCacheFor(t, cfg)
				if got.cache != nil || !got.plan.cacheUnusable {
					t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
						got.plan.cacheUnusable)
				}
				lines := linesAt(got.obs.All(), tc.level, "api endpoint discovery off: ")
				if len(lines) != 1 {
					t.Fatalf("lines at %v = %v; all lines: %v", tc.level, lines, got.obs.All())
				}
				for _, e := range got.obs.All() {
					if e.Level > tc.level {
						t.Errorf("line %q is above %v", e.Message, tc.level)
					}
				}
				if !endpointDiscoveryOn(cfg) || !got.plan.failover() || got.plan.discoveryEffective() ||
					got.plan.install() {
					t.Errorf("plan = %+v: the config-only answer stays on, the effective mode is off, no wrapper",
						got.plan)
				}
			})
		}
	}
}

func TestOpenEndpointCacheMissingSiblingLineStaysDebugUnderOn(t *testing.T) {
	const note = "pve endpoints: no cache directory beside the allocation journal, because the journal " +
		"directory does not exist"
	original := chooseCacheDirectory
	t.Cleanup(func() { chooseCacheDirectory = original })
	for _, tc := range []struct {
		name string
		mode config.DiscoveryMode
		dir  func(t *testing.T) string
	}{
		{"usable layer under auto", config.DiscoveryModeAuto, newCacheDir},
		{"usable layer under on", config.DiscoveryModeOn, newCacheDir},
		{"unusable layer under on", config.DiscoveryModeOn, func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "missing")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir(t)
			chooseCacheDirectory = func(cfg *config.CPIConfig) (endpointcache.Choice, []string) {
				return endpointcache.OptionsFor(cfg).Resolve(), []string{note}
			}
			got := openCacheFor(t, cacheTestConfig(tc.mode, dir))
			entries := got.obs.All()
			if lines := linesAt(entries, slog.LevelDebug, note); len(lines) != 1 {
				t.Errorf("the missing-sibling line appears %d times at Debug, want once; lines: %v", len(lines),
					entries)
			}
			for _, e := range entries {
				if strings.Contains(e.Message, note) && e.Level != slog.LevelDebug {
					t.Errorf("the missing-sibling line is at %v", e.Level)
				}
			}
			warns := 0
			for _, e := range entries {
				if e.Level == slog.LevelWarn {
					warns++
				}
			}
			wantWarns := 0
			if got.cache == nil && tc.mode == config.DiscoveryModeOn {
				wantWarns = 1
			}
			if warns != wantWarns {
				t.Errorf("%d Warn lines, want %d; lines: %v", warns, wantWarns, entries)
			}
		})
	}
}

func TestOpenEndpointCacheKeepsTheFilesOfTwoCredentialsApart(t *testing.T) {
	dir := newCacheDir(t)
	cfg := cacheTestConfig(config.DiscoveryModeAuto, dir)
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	// The same host with a different credential reads the same directory.
	other := cacheTestConfig(config.DiscoveryModeAuto, dir)
	other.APIToken = "cpi@pve!other=fedcba9876543210"
	got := openCacheFor(t, other)
	if got.cache == nil || len(got.plan.endpoints) != 1 {
		t.Fatalf("another credential must start with no nodes: cache %v, endpoints %+v", got.cache, got.plan.endpoints)
	}
	if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
		t.Errorf("a file for another key logged %q at %v", e.Message, e.Level)
	}
}

func TestOpenEndpointCacheSkipsACachedNodeThePoolWouldRefuse(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil || len(got.plan.endpoints) != 3 {
		t.Fatalf("setup: endpoints %+v", got.plan.endpoints)
	}
	// A node that repeats an endpoint already in the pool is left out.
	repeat := endpointcache.DiscoveredNode{
		Name: "pve2", Address: "PVE2.dc1.example.com", Port: 8006, Pins: []string{pinHex(1)}, Online: true,
	}
	seen := normalizedEndpointSet(configuredAddresses(&got.plan))
	for _, ep := range got.plan.endpoints[1:] {
		seen[normalizedEndpoint(ep.Address)] = true
	}
	if _, ok := got.cache.endpointOf(repeat, &endpointcache.Payload{}, seen); ok {
		t.Error("a repeat of a pool endpoint was kept")
	}
	bad := endpointcache.DiscoveredNode{Name: "pve8", Address: "pve8.dc1.example.com", Port: 8006, Pins: []string{"zz"}}
	if _, ok := got.cache.endpointOf(bad, &endpointcache.Payload{}, map[string]bool{}); ok {
		t.Error("a node with a pin that does not parse was kept")
	}
	none := endpointcache.DiscoveredNode{Name: "pve8", Address: "pve8.dc1.example.com", Port: 8006}
	if _, ok := got.cache.endpointOf(none, &endpointcache.Payload{}, map[string]bool{}); ok {
		t.Error("a node with no pins was kept")
	}
}

// buildCachePool runs the client's construction steps up to the pool, so a test
// can read the options and the pool that a cache produced.
func buildCachePool(t *testing.T, cfg *config.CPIConfig) (*endpointWiring, sdkclient.Options, *endpointCache) {
	t.Helper()
	opts, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatalf("wireEndpoints: %v", err)
	}
	opts.APIToken = cfg.APIToken
	cache := openEndpointCache(cfg, &wiring.plan, log.NewNopLogger())
	t.Cleanup(cache.release)
	opts = wiring.apply(opts, log.NewNopLogger(), cache)
	if _, err := sdkclient.NewClient(opts); err != nil || wiring.poolErr != nil {
		t.Fatalf("sdkclient.NewClient: %v (pool error %v)", err, wiring.poolErr)
	}
	return wiring, opts, cache
}

func TestPoolBuiltFromAFileWithANodeStartsWithThePoolTimeouts(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})

	wiring, opts, _ := buildCachePool(t, cfg)
	if wiring.pool == nil || wiring.pool.Len() != 2 {
		t.Fatalf("pool = %v, want a pool of two", wiring.pool)
	}
	if opts.DialTimeoutSec != 5 || opts.TLSHandshakeTimeoutSec != 10 {
		t.Errorf("dial %ds and handshake %ds, want 5 and 10", opts.DialTimeoutSec, opts.TLSHandshakeTimeoutSec)
	}
	if got := wiring.pool.HeaderDeadline(); got != 60*time.Second {
		t.Errorf("header deadline = %s, want 60s", got)
	}
}

func TestPoolBuiltFromAnEmptyDirectoryKeepsTheSingleEndpointSettings(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	wiring, opts, cache := buildCachePool(t, cfg)
	if cache == nil || wiring.pool == nil || wiring.pool.Len() != 1 {
		t.Fatalf("cache %v, pool %v, want a cache and a pool of one", cache, wiring.pool)
	}
	if opts.DialTimeoutSec != 0 || opts.TLSHandshakeTimeoutSec != 0 || wiring.pool.HeaderDeadline() != 0 {
		t.Errorf("dial %d, handshake %d, header %s, want today's settings for a pool of one",
			opts.DialTimeoutSec, opts.TLSHandshakeTimeoutSec, wiring.pool.HeaderDeadline())
	}
}

func TestPoolFallsBackToConfiguredEndpointsWhenItRefusesTheCache(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	opts, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts.APIToken = cfg.APIToken
	// A cache node with the configured endpoint's own address makes the pool
	// refuse the list. The load skips such nodes, so the plan is built by hand.
	wiring.plan.endpoints = append(wiring.plan.endpoints, endpointpool.Endpoint{
		Address: wiring.plan.endpoints[0].Address, Kind: endpointpool.KindDiscovered, Node: "pve1",
		Pins: []endpointpool.Pin{{1}},
	})
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	cache := &endpointCache{mode: cfg.DiscoveryModeValue(), logger: logger}
	opts = wiring.apply(opts, logger, cache)
	if _, err := sdkclient.NewClient(opts); err != nil || wiring.poolErr != nil {
		t.Fatalf("sdkclient.NewClient: %v (pool error %v)", err, wiring.poolErr)
	}
	if wiring.pool == nil || wiring.pool.Len() != 1 {
		t.Fatalf("pool = %v, want the configured endpoint alone", wiring.pool)
	}
	if lines := linesAt(obs.All(), slog.LevelDebug, "the pool refused the cached nodes"); len(lines) != 1 {
		t.Errorf("refusal lines = %v, want one at Debug under auto", lines)
	}
}

func TestPinningAssertionIgnoresTheCacheAndItsDirectory(t *testing.T) {
	pinned := sdkclient.Options{CachedFingerprints: map[string]bool{"aa:bb": true}}
	dir := newCacheDir(t)
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeOn, config.DiscoveryModeAuto} {
		cfg := cacheTestConfig(mode, dir)
		_, wiring, err := wireEndpoints(cfg)
		if err != nil {
			t.Fatal(err)
		}
		before := assertPinningForPlan(pinned, cfg, wiring.plan)
		if before == nil {
			t.Fatalf("mode %s with pinning built", mode)
		}
		for _, want := range []string{"pve.endpoints", "pve.api_endpoint_discovery", "fingerprint pinning"} {
			if !strings.Contains(before.Error(), want) {
				t.Errorf("mode %s: message %q lacks %q", mode, before.Error(), want)
			}
		}
		// The cache fills and the directory goes bad, and the result is the same.
		seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
		cache := openEndpointCache(cfg, &wiring.plan, log.NewNopLogger())
		after := assertPinningForPlan(pinned, cfg, wiring.plan)
		cache.release()
		if after == nil || after.Error() != before.Error() {
			t.Errorf("mode %s: after the cache filled, error = %v, want %v", mode, after, before)
		}
		wiring.plan.cacheUnusable = true
		if again := assertPinningForPlan(pinned, cfg, wiring.plan); again == nil || again.Error() != before.Error() {
			t.Errorf("mode %s: with an unusable directory, error = %v, want %v", mode, again, before)
		}
	}
	// Password auth under auto resolves to off, so pinning is allowed.
	cfg := cacheTestConfig(config.DiscoveryModeAuto, dir)
	cfg.APIToken = ""
	cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
	_, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertPinningForPlan(pinned, cfg, wiring.plan); err != nil {
		t.Errorf("password auth under auto with pinning = %v, want none", err)
	}
}

func TestOpenEndpointCacheStoreClosesAfterTheDrain(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	// The store accepts work until the drain, then it is closed.
	if _, _, err := got.cache.store.ReadPayload(time.Now(), nil); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadPayload before the drain: %v", err)
	}
	drainProcessRegistry(t)
	if _, _, err := got.cache.store.ReadPayload(time.Now(), nil); err == nil {
		t.Error("the store still serves reads after the drain")
	}
	// A call that starts after the drain began runs without a cache.
	endpointcache.Drain(context.Background(), time.Second)
	defer endpointcache.Open()
	late := openCacheFor(t, cfg)
	if late.cache != nil || !late.plan.cacheUnusable {
		t.Errorf("a call after the drain began opened a cache: %v", late.cache)
	}
	if lines := linesAt(late.obs.All(), slog.LevelDebug, "the exit drain has started"); len(lines) != 1 {
		t.Errorf("drain lines = %v, want one Debug line", lines)
	}
	if names, err := os.ReadDir(cfg.EndpointCacheDir); err != nil || len(names) == 0 {
		t.Errorf("directory entries %v (err %v)", names, err)
	}
}

func TestCacheAliasesJournal(t *testing.T) {
	// The test directory is resolved first, so no symlink sits in any path
	// below unless a case puts it there. macOS keeps its temporary directory
	// behind a symlink and Linux does not, and the cases must mean the same on
	// both.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	journal := mk("journal")
	inside := mk("journal", "cache")
	sibling := mk("sibling")
	parent := mk("outer")
	nestedJournal := mk("outer", "journal")
	link := filepath.Join(root, "link")
	if err := os.Symlink(journal, link); err != nil {
		t.Fatal(err)
	}
	linkedAncestor := filepath.Join(root, "ancestor")
	if err := os.Symlink(root, linkedAncestor); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dirLikeFile := filepath.Join(file, "sub")
	linkedNested := filepath.Join(root, "nested-link")
	if err := os.Symlink(nestedJournal, linkedNested); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "parent-link")
	if err := os.Symlink(parent, linkedParent); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cache   string
		journal string
		want    bool
		wantErr bool
	}{
		{"no journal", sibling, "", false, false},
		{"the same directory", journal, journal, true, false},
		{"inside the journal", inside, journal, true, false},
		{"a sibling", sibling, journal, false, false},
		{"a journal that does not exist yet", sibling, filepath.Join(root, "later"), false, false},
		{"a cache that contains the journal", parent, nestedJournal, true, false},
		{"a cache that contains a journal that does not exist yet", parent, filepath.Join(parent, "later"), true,
			false},
		{"a cache that contains a journal below a missing directory", parent,
			filepath.Join(parent, "missing", "deeper", "journal"), true, false},
		{"a cache beside a journal that does not exist yet", sibling, filepath.Join(parent, "later"), false, false},
		{"a cache that contains the journal through a symlink", parent, linkedNested, true, false},
		{"a cache reached through a symlink that contains the journal", linkedParent, nestedJournal, true, false},
		{"a symlinked ancestor above a containing cache", filepath.Join(linkedAncestor, "outer"),
			filepath.Join(linkedAncestor, "outer", "journal"), true, false},
		{"a symlinked ancestor on the journal of a containing cache", parent,
			filepath.Join(linkedAncestor, "outer", "journal"), true, false},
		{"a journal that does not exist yet below a symlink to the cache", parent, filepath.Join(linkedParent, "later"),
			true, false},
		{"a journal below missing directories under a symlink to the cache", parent,
			filepath.Join(linkedParent, "missing", "deeper", "journal"), true, false},
		{"a journal that does not exist yet below a symlink to another directory", sibling,
			filepath.Join(linkedParent, "later"), false, false},
		{"a journal that does not exist yet below a symlinked ancestor of the cache", journal,
			filepath.Join(linkedAncestor, "journal", "later"), true, false},
		{"a journal that does not exist yet beside a cache under a symlinked ancestor", inside,
			filepath.Join(linkedAncestor, "journal", "later"), false, false},
		{"a journal that does not exist yet below a symlinked ancestor of a sibling", sibling,
			filepath.Join(linkedAncestor, "outer", "later"), false, false},
		{"a cache that does not exist yet beside the journal", filepath.Join(root, "new-cache"), journal, false, false},
		{"a cache that does not exist yet inside the journal", filepath.Join(journal, "new-cache"), journal, true,
			false},
		{"a cache below missing directories inside the journal", filepath.Join(journal, "a", "b", "new-cache"), journal,
			true, false},
		{"a cache that does not exist yet inside the journal through a symlink", filepath.Join(link, "new-cache"),
			journal, true, false},
		{"a cache that does not exist yet inside a symlinked ancestor of the journal",
			filepath.Join(linkedAncestor, "journal", "new-cache"), journal, true, false},
		{"a cache that does not exist yet beside a journal that does not exist yet", filepath.Join(root, "m1"),
			filepath.Join(root, "m2"), false, false},
		{"a cache that does not exist yet and a journal below it", filepath.Join(root, "m1"),
			filepath.Join(root, "m1", "journal"), true, false},
		{"a cache that does not exist yet below a journal that does not exist yet", filepath.Join(root, "m1", "cache"),
			filepath.Join(root, "m1"), true, false},
		{"a cache that does not exist yet reached through a symlink to a journal that does not exist yet",
			filepath.Join(linkedParent, "m1", "cache"), filepath.Join(parent, "m1"), true, false},
		{"a cache that cannot be examined", dirLikeFile, journal, false, true},
		{"the filesystem root as the cache", string(filepath.Separator), journal, true, false},
		{"the journal reached through a symlink", journal, link, true, false},
		{"the cache reached through a symlink", link, journal, true, false},
		{"a cache inside a symlinked journal", inside, link, true, false},
		{"a symlinked ancestor of the cache", filepath.Join(linkedAncestor, "journal", "cache"), journal, true, false},
		{"a symlinked ancestor of the journal", inside, filepath.Join(linkedAncestor, "journal"), true, false},
		{"a journal with trailing space", inside, journal + " ", true, false},
		{"a journal that cannot be examined", sibling, dirLikeFile, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cacheAliasesJournal(tc.cache, tc.journal)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("cacheAliasesJournal(%q, %q) = %v, %v; want %v, error %v", tc.cache, tc.journal, got, err,
					tc.want, tc.wantErr)
			}
		})
	}
}

// A path spelled in another letter case reaches the same directory on a case
// folding file system, and only the identity check sees it.
func TestCacheAliasesJournalSeesADirectoryUnderAnotherSpelling(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "Journal")
	cache := filepath.Join(journal, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	respelled := filepath.Join(root, "jOURNAL")
	if _, err := os.Stat(respelled); err != nil {
		t.Skip("this file system distinguishes letter case")
	}
	got, err := cacheAliasesJournal(cache, respelled)
	if err != nil || !got {
		t.Errorf("cacheAliasesJournal under another spelling = %v, %v, want true", got, err)
	}
}

func TestOpenEndpointCacheRefusesADirectoryThatAliasesTheJournal(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "journal")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(journal, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	// The journal is named through a symlinked ancestor.
	ancestor := filepath.Join(root, "ancestor")
	if err := os.Symlink(root, ancestor); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		journal string
		mode    config.DiscoveryMode
		level   slog.Level
	}{
		{"plain path under auto", journal, config.DiscoveryModeAuto, slog.LevelDebug},
		{"plain path under on", journal, config.DiscoveryModeOn, slog.LevelWarn},
		{"symlinked ancestor under on", filepath.Join(ancestor, "journal"), config.DiscoveryModeOn, slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cacheTestConfig(tc.mode, cache)
			cfg.StorageAllocationJournalDir = tc.journal
			got := openCacheFor(t, cfg)
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("a cache directory inside the journal opened: %v", got.cache)
			}
			if lines := linesAt(got.obs.All(), tc.level, "allocation journal directory"); len(lines) != 1 {
				t.Errorf("lines at %v = %v; all: %v", tc.level, lines, got.obs.All())
			}
			if names, err := os.ReadDir(cache); err != nil || len(names) != 0 {
				t.Errorf("the refused directory holds %v (err %v), want it untouched", names, err)
			}
		})
	}

	// A directory that contains the journal is refused too, whether the journal
	// is reached by its own path or through a symlinked ancestor of the cache.
	contains := filepath.Join(root, "outer")
	nested := filepath.Join(contains, "journal")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		cache   string
		journal string
		mode    config.DiscoveryMode
		level   slog.Level
	}{
		{"a cache that contains the journal under auto", contains, nested, config.DiscoveryModeAuto, slog.LevelDebug},
		{"a cache that contains the journal under on", contains, nested, config.DiscoveryModeOn, slog.LevelWarn},
		{"a cache reached through a symlinked ancestor that contains the journal", filepath.Join(ancestor, "outer"),
			nested, config.DiscoveryModeOn, slog.LevelWarn},
		{"a journal reached through a symlink below the cache", contains, filepath.Join(ancestor, "outer", "journal"),
			config.DiscoveryModeOn, slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cacheTestConfig(tc.mode, tc.cache)
			cfg.StorageAllocationJournalDir = tc.journal
			before, err := readDirNames(tc.cache)
			if err != nil {
				t.Fatal(err)
			}
			got := openCacheFor(t, cfg)
			if got.cache != nil {
				t.Fatalf("a cache directory that contains the journal opened: %v", got.cache)
			}
			if !got.plan.cacheUnusable {
				t.Error("the plan was not marked cache-unusable")
			}
			if lines := linesAt(got.obs.All(), tc.level, "allocation journal directory"); len(lines) != 1 {
				t.Errorf("lines at %v = %v; all: %v", tc.level, lines, got.obs.All())
			}
			if after, err := readDirNames(tc.cache); err != nil || !slices.Equal(before, after) {
				t.Errorf("the refused directory held %v and holds %v (err %v), want it untouched", before, after, err)
			}
		})
	}

	// A directory beside the journal stays usable.
	beside := newCacheDir(t)
	cfg := cacheTestConfig(config.DiscoveryModeAuto, beside)
	cfg.StorageAllocationJournalDir = journal
	if got := openCacheFor(t, cfg); got.cache == nil || got.plan.cacheUnusable {
		t.Errorf("a cache directory beside the journal was refused: %v", got.obs.All())
	}
}

// A sink that is handed a mark starts a registered worker and returns at once.
func TestMarkSinkHandsTheWriteToAWorkerAndReturns(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	var (
		mu      sync.Mutex
		started []string
		pending []func(context.Context)
	)
	got.cache.sink.start = func(name string, mode config.DiscoveryMode, fn func(context.Context)) error {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, name+"/"+string(mode))
		pending = append(pending, fn)
		return nil
	}
	target := "pve2.dc1.example.com:8006"
	markedAt := time.Now().UTC().Truncate(time.Second)
	got.cache.sink.MarkDown(endpointpool.Mark{Endpoint: target, Class: endpointpool.PhaseDial, At: markedAt})

	mu.Lock()
	if !slices.Equal(started, []string{"down mark/auto"}) || len(pending) != 1 {
		t.Fatalf("started %v, want one down-mark worker under auto", started)
	}
	fn := pending[0]
	mu.Unlock()
	if p := readSeededCache(t, cfg); len(p.Marks) != 0 {
		t.Fatalf("the write ran inside MarkDown: marks %v", p.Marks)
	}
	fn(context.Background())
	p := readSeededCache(t, cfg)
	if mark := p.Marks[target]; mark.Class != endpointcache.MarkDial || !mark.DownAt.Equal(markedAt) {
		t.Errorf("mark = %+v, want a dial mark at %s", mark, markedAt)
	}
	if !p.Discovery.PinFailedAt.IsZero() {
		t.Errorf("a dial mark set PinFailedAt to %s", p.Discovery.PinFailedAt)
	}
}

func TestMarkSinkRefusedByAClosedRegistryLogsAndMovesOn(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	endpointcache.Drain(context.Background(), time.Second)
	defer endpointcache.Open()
	got.cache.sink.MarkDown(endpointpool.Mark{
		Endpoint: "pve1.dc1.example.com:8006", Class: endpointpool.PhaseDial, At: time.Now(),
	})
	if lines := linesAt(got.obs.All(), slog.LevelDebug, "the down mark is not recorded"); len(lines) != 1 {
		t.Errorf("lines = %v, want one Debug line", lines)
	}
	got.cache.sink.MarkDown(endpointpool.Mark{Endpoint: "pve1.dc1.example.com:8006", Class: "other", At: time.Now()})
	if lines := linesAt(got.obs.All(), slog.LevelDebug, "unknown class"); len(lines) != 1 {
		t.Errorf("unknown class lines = %v, want one Debug line", lines)
	}
}

func TestMarkSinkPinMarkSetsPinFailedAtAndKeepsTheNewest(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	got.cache.sink.start = func(_ string, _ config.DiscoveryMode, fn func(context.Context)) error {
		fn(context.Background())
		return nil
	}
	target := "pve2.dc1.example.com:8006"
	newer := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	older := newer.Add(-30 * time.Second)
	got.cache.sink.MarkDown(endpointpool.Mark{Endpoint: target, Class: endpointpool.PhasePin, At: newer})
	p := readSeededCache(t, cfg)
	if p.Marks[target].Class != endpointcache.MarkPin || !p.Discovery.PinFailedAt.Equal(newer) {
		t.Fatalf("after a pin mark: mark %+v, pin_failed_at %s, want a pin mark and %s", p.Marks[target],
			p.Discovery.PinFailedAt, newer)
	}
	got.cache.sink.MarkDown(endpointpool.Mark{Endpoint: target, Class: endpointpool.PhasePin, At: older})
	p = readSeededCache(t, cfg)
	if !p.Discovery.PinFailedAt.Equal(newer) {
		t.Errorf("an older pin mark moved pin_failed_at to %s, want %s", p.Discovery.PinFailedAt, newer)
	}
}

// The first-answer hook starts the refresh on a registered goroutine and
// returns while the refresh is still running.
func TestFirstAnswerHookHandsTheRefreshToAWorkerAndReturns(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	cfgPool := endpointpool.Config{}
	got.cache.attach(&cfgPool)
	if cfgPool.FirstAnswer == nil || cfgPool.MarkSink == nil {
		t.Fatalf("attach left the hook %v and the sink %v", cfgPool.FirstAnswer, cfgPool.MarkSink)
	}

	gate := make(chan struct{})
	reading := make(chan struct{}, 8)
	block := func(ctx context.Context) error {
		select {
		case reading <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return fmt.Errorf("the test refuses the read")
	}
	client := blockedReadsClient(block)
	if err := got.cache.worker.Bind(DiscoveryBinding{Client: client, Pool: &discPool{}}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	returned := make(chan struct{})
	go func() {
		cfgPool.FirstAnswer.FirstAnswer(net.JoinHostPort(cacheTestHost, "8006"))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the first-answer hook waited for the refresh")
	}
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never started a read")
	}
	close(gate)
	drainProcessRegistry(t)
}

// blockedReadsClient is a client whose discovery reads all call block and fail
// with its error.
func blockedReadsClient(block func(ctx context.Context) error) *discClient {
	return &discClient{
		nodes: &fakeNodesService{
			listNodesFn: func(ctx context.Context) (*sdknodes.ListNodesResponse, error) { return nil, block(ctx) },
			listCertificatesInfoFn: func(
				ctx context.Context, _ string,
			) (*sdknodes.ListCertificatesInfoResponse, error) {
				return nil, block(ctx)
			},
		},
		cluster: &fakeClusterService{
			listStatusFn: func(ctx context.Context) (*sdkcluster.ListStatusResponse, error) { return nil, block(ctx) },
			listConfigNodesFn: func(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
				return nil, block(ctx)
			},
		},
	}
}

// readDirNames lists the names in dir.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}
