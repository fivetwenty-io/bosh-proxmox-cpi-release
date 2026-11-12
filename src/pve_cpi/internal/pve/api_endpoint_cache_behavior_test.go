package pve

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// The tests in this file pin the behaviors of the cache-backed pool that an
// operator sees, such as the level of each line, the timeouts of the pool a
// refused cache falls back to, and the marks a later process reads. Those that
// touch the process registry do not run in parallel.

// resetStartedAwayLog forgets which starts away this process has described, so
// a test that expects the line gets it whatever ran before.
func resetStartedAwayLog(t *testing.T) {
	t.Helper()
	reset := func() {
		startedAwayLog.mu.Lock()
		defer startedAwayLog.mu.Unlock()
		startedAwayLog.said = nil
	}
	reset()
	t.Cleanup(reset)
}

// setCacheClock fixes the clock the cache uses for the rest of the test.
func setCacheClock(t *testing.T, now time.Time) {
	t.Helper()
	original := cacheClock
	cacheClock = func() time.Time { return now }
	t.Cleanup(func() { cacheClock = original })
}

// applySeedChange applies change to the cache file of cfg, as an earlier
// process would have.
func applySeedChange(t *testing.T, cfg *config.CPIConfig, change endpointcache.Change) {
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
	_, err = store.UpdatePayload(context.Background(), time.Now().UTC().Truncate(time.Second),
		func(endpointcache.Stored) (endpointcache.Change, error) { return change, nil })
	if err != nil {
		t.Fatalf("apply the change: %v", err)
	}
}

// cacheFilePath is the path of the cache file of cfg, which the cache opened
// for it names.
func cacheFilePath(t *testing.T, cfg *config.CPIConfig) string {
	t.Helper()
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatalf("no cache to name the file: %v", got.obs.All())
	}
	name := got.cache.store.FileName()
	got.cache.release()
	return filepath.Join(cfg.EndpointCacheDir, name)
}

func countAt(entries []log.Entry, level slog.Level) int {
	n := 0
	for _, e := range entries {
		if e.Level == level {
			n++
		}
	}
	return n
}

// A pool that refuses the cached nodes falls back to the configured endpoints,
// and the pool of one it builds keeps the dial and handshake defaults, because
// discovery is still effectively on and the pool is meant to grow.
func TestFallbackPoolKeepsTheDiscoveryTimeoutDefaults(t *testing.T) {
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level slog.Level
	}{{config.DiscoveryModeAuto, slog.LevelDebug}, {config.DiscoveryModeOn, slog.LevelWarn}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := cacheTestConfig(tc.mode, newCacheDir(t))
			seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
			opts, wiring, err := wireEndpoints(cfg)
			if err != nil {
				t.Fatal(err)
			}
			opts.APIToken = cfg.APIToken
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			cache := openEndpointCache(cfg, &wiring.plan, logger)
			t.Cleanup(cache.release)
			if cache == nil || len(wiring.plan.endpoints) != 3 || len(cache.sink.pool) != 2 {
				t.Fatalf("setup: endpoints %+v, cache %v", wiring.plan.endpoints, cache)
			}
			// A discovered endpoint with the configured endpoint's own address makes
			// the pool refuse the list. The load skips such nodes, so the plan is
			// extended by hand.
			wiring.plan.endpoints = append(wiring.plan.endpoints, endpointpool.Endpoint{
				Address: wiring.plan.endpoints[0].Address, Kind: endpointpool.KindDiscovered, Node: "pve1",
				Pins: []endpointpool.Pin{{1}},
			})

			opts = wiring.apply(opts, logger, cache)
			if opts.DialTimeoutSec != 5 || opts.TLSHandshakeTimeoutSec != 10 {
				t.Errorf("dial %ds and handshake %ds, want 5 and 10 with discovery on",
					opts.DialTimeoutSec, opts.TLSHandshakeTimeoutSec)
			}
			if _, err := sdkclient.NewClient(opts); err != nil || wiring.poolErr != nil {
				t.Fatalf("sdkclient.NewClient: %v (pool error %v)", err, wiring.poolErr)
			}
			if wiring.pool == nil || wiring.pool.Len() != 1 {
				t.Fatalf("pool = %v, want the configured endpoint alone", wiring.pool)
			}
			if got := wiring.pool.HeaderDeadline(); got != 0 {
				t.Errorf("header deadline = %s, want none for a pool of one", got)
			}
			if len(cache.discovered) != 0 || len(cache.sink.pool) != 0 {
				t.Errorf("the cache still holds discovered %v and a sink pool %v", cache.discovered, cache.sink.pool)
			}
			if lines := linesAt(obs.All(), tc.level, "the pool refused the cached nodes"); len(lines) != 1 {
				t.Errorf("refusal lines at %v = %v, want one; all: %v", tc.level, lines, obs.All())
			}
		})
	}
}

// A pool that accepts the cached nodes keeps them, and the sink names them as
// the pool's own, so the merge keeps their marks.
func TestMarkSinkNamesTheDiscoveredEndpointsOfThePool(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil {
		t.Fatal("no cache")
	}
	want := []string{"pve2.dc1.example.com:8006", "pve3.dc1.example.com:8006"}
	if !slices.Equal(got.cache.sink.pool, want) {
		t.Errorf("sink pool = %v, want %v", got.cache.sink.pool, want)
	}
}

// fakeUpdater records the changes a mark write hands to the store.
type fakeUpdater struct {
	mu      sync.Mutex
	changes []endpointcache.Change
	err     error
	// during runs inside the update, as a drain that gives up does.
	during func()
}

func (f *fakeUpdater) UpdatePayload(
	_ context.Context, _ time.Time, build func(stored endpointcache.Stored) (endpointcache.Change, error),
) (endpointcache.UpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.during != nil {
		f.during()
	}
	change, err := build(endpointcache.Stored{})
	if err != nil {
		return endpointcache.UpdateResult{}, err
	}
	f.changes = append(f.changes, change)
	return endpointcache.UpdateResult{}, f.err
}

func (f *fakeUpdater) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.changes)
}

func TestMarkSinkWriteHandsTheStoreTheMarkAndThePool(t *testing.T) {
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	store := &fakeUpdater{}
	pool := []string{"pve2.dc1.example.com:8006"}
	sink := &cacheMarkSink{store: store, mode: config.DiscoveryModeOn, logger: logger, now: time.Now, pool: pool}
	at := time.Now().UTC().Truncate(time.Second)
	sink.write(context.Background(), endpointpool.Mark{Endpoint: "pve1.dc1.example.com:8006", At: at},
		endpointcache.MarkDial)
	if store.count() != 1 {
		t.Fatalf("the store saw %d updates, want one", store.count())
	}
	change := store.changes[0]
	if !slices.Equal(change.Pool, pool) {
		t.Errorf("change pool = %v, want %v", change.Pool, pool)
	}
	mark := change.Marks["pve1.dc1.example.com:8006"]
	if mark.Class != endpointcache.MarkDial || !mark.DownAt.Equal(at) {
		t.Errorf("change marks = %v, want a dial mark at %s", change.Marks, at)
	}
	if !change.PinFailedAt.IsZero() {
		t.Errorf("a dial mark set PinFailedAt to %s", change.PinFailedAt)
	}
	if e, bad := anyLineAbove(obs.All(), slog.LevelDebug); bad {
		t.Errorf("a recorded mark logged %q at %v", e.Message, e.Level)
	}
}

// A write whose context a drain cancelled never reaches the store, and the
// cancellation is not reported as a failure of the mark.
func TestMarkSinkWriteWithACancelledContextSkipsTheStoreAndSaysNothing(t *testing.T) {
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	store := &fakeUpdater{}
	sink := &cacheMarkSink{store: store, mode: config.DiscoveryModeOn, logger: logger, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink.write(ctx, endpointpool.Mark{Endpoint: "pve1.dc1.example.com:8006", At: time.Now()}, endpointcache.MarkDial)
	if store.count() != 0 {
		t.Errorf("the store saw %d updates from a cancelled write, want none", store.count())
	}
	if len(obs.All()) != 0 {
		t.Errorf("a cancelled write logged %v", obs.All())
	}
}

func TestMarkSinkWriteLogsAStoreFailureAtTheLevelOfTheMode(t *testing.T) {
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level slog.Level
	}{{config.DiscoveryModeAuto, slog.LevelDebug}, {config.DiscoveryModeOn, slog.LevelWarn}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			store := &fakeUpdater{err: errors.New("the disk is full")}
			sink := &cacheMarkSink{store: store, mode: tc.mode, logger: logger, now: time.Now}
			sink.write(context.Background(), endpointpool.Mark{Endpoint: "pve1:8006", At: time.Now()},
				endpointcache.MarkDial)
			if lines := linesAt(obs.All(), tc.level, "the down mark could not be recorded"); len(lines) != 1 {
				t.Errorf("lines at %v = %v, want one; all: %v", tc.level, lines, obs.All())
			}
		})
	}
}

// A drain that gives up cancels the write while the store is working on it, and
// the failure that follows is the drain's doing and not the disk's.
func TestMarkSinkWriteSaysNothingWhenTheDrainCancelsItMidWrite(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &fakeUpdater{err: context.Canceled, during: cancel}
			sink := &cacheMarkSink{store: store, mode: mode, logger: logger, now: time.Now}
			sink.write(ctx, endpointpool.Mark{Endpoint: "pve1:8006", At: time.Now()}, endpointcache.MarkDial)
			if got := store.count(); got != 1 {
				t.Fatalf("the store saw %d writes, want the one that was cancelled", got)
			}
			if got := obs.All(); len(got) != 0 {
				t.Errorf("a write the drain cancelled logged %v, want nothing", got)
			}
		})
	}
}

// A call without a cache has no cache to drop, and the pool and plan code
// reports through the same methods whether there is one or not.
func TestTheCacheMethodsTheWiringCallsAreSafeOnNoCache(t *testing.T) {
	var none *endpointCache
	none.poolRefused(errors.New("refused"))
	none.dropDiscovered()
	none.bind(nil, nil)
}

// A drain can begin after the first check and before the store is open. The
// call then gives the store back and runs without a cache.
func TestACallWhoseStoreOpensAfterTheDrainBeganGivesTheStoreBack(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := cacheTestConfig(mode, newCacheDir(t))
			_, wiring, err := wireEndpoints(cfg)
			if err != nil {
				t.Fatalf("wireEndpoints: %v", err)
			}
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			drained := false
			trigger := &drainOnLine{
				next: logger.Handler(),
				text: "pve endpoints: cache directory",
				fn: func() {
					drained = true
					endpointcache.Drain(context.Background(), time.Second)
				},
			}
			t.Cleanup(endpointcache.Open)
			cache := openEndpointCache(cfg, &wiring.plan, &log.Logger{Logger: slog.New(trigger)})
			if !drained {
				t.Fatal("the drain never began")
			}
			if cache != nil || !wiring.plan.cacheUnusable {
				t.Errorf("cache = %v, cacheUnusable = %v, want no cache", cache, wiring.plan.cacheUnusable)
			}
			if got := descriptorsOnto(t, cfg.EndpointCacheDir); got != 0 {
				t.Errorf("%d descriptors of the cache directory stay open, want the store given back", got)
			}
			// The late refusal says so at Debug, whatever the mode, because the drain
			// is a state of the process and not a fault in the directory.
			if lines := linesAt(observed.All(), slog.LevelDebug, drainStartedText); len(lines) != 1 {
				t.Errorf("drain lines = %v, want one Debug line; all: %v", lines, observed.All())
			}
			if e, bad := anyLineAbove(observed.All(), slog.LevelDebug); bad {
				t.Errorf("line %q is at %v, want nothing above Debug", e.Message, e.Level)
			}
		})
	}
}

// A drain that begins after the store registered its release and before the
// first read closes the store under the read. The call reports the drain, at
// Debug whatever the mode, and gives the store back. It does not blame the
// directory by saying the cache file could not be read.
func TestADrainBetweenTheOpenAndTheFirstReadIsReportedAsTheDrain(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			dir := newCacheDir(t)
			// A secret of the wrong length makes the open log a Warn after it
			// registered the release and before the read, which is the window.
			if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("too short"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := cacheTestConfig(mode, dir)
			_, wiring, err := wireEndpoints(cfg)
			if err != nil {
				t.Fatalf("wireEndpoints: %v", err)
			}
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			drained := false
			trigger := &drainOnLine{
				next: logger.Handler(),
				text: "replaced the directory secret",
				fn: func() {
					drained = true
					endpointcache.Drain(context.Background(), time.Second)
				},
			}
			t.Cleanup(endpointcache.Open)
			cache := openEndpointCache(cfg, &wiring.plan, &log.Logger{Logger: slog.New(trigger)})
			if !drained {
				t.Fatal("the drain never began")
			}
			if cache != nil || !wiring.plan.cacheUnusable {
				t.Errorf("cache = %v, cacheUnusable = %v, want no cache", cache, wiring.plan.cacheUnusable)
			}
			if lines := linesAt(observed.All(), slog.LevelDebug, drainStartedText); len(lines) != 1 {
				t.Errorf("drain lines = %v, want one Debug line; all: %v", lines, observed.All())
			}
			for _, text := range []string{"could not be read", "store is closed"} {
				for _, e := range observed.All() {
					if strings.Contains(e.Message, text) {
						t.Errorf("line %q at %v blames the directory for the drain", e.Message, e.Level)
					}
				}
			}
			for _, e := range observed.All() {
				if e.Level > slog.LevelDebug && !strings.Contains(e.Message, "replaced the directory secret") {
					t.Errorf("line %q is at %v, want only the secret's Warn above Debug", e.Message, e.Level)
				}
			}
			if got := descriptorsOnto(t, dir); got != 0 {
				t.Errorf("%d descriptors of the cache directory stay open, want the store given back", got)
			}
		})
	}
}

// descriptorsOnto counts the open file descriptors of this process that refer
// to dir.
func descriptorsOnto(t *testing.T, dir string) int {
	t.Helper()
	var want syscall.Stat_t
	if err := syscall.Stat(dir, &want); err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	count := 0
	for fd := 0; fd < 4096; fd++ {
		var got syscall.Stat_t
		if err := syscall.Fstat(fd, &got); err == nil && got.Dev == want.Dev && got.Ino == want.Ino {
			count++
		}
	}
	return count
}

// drainOnLine runs fn the first time a record whose message contains text goes
// by, and passes every record on.
type drainOnLine struct {
	next slog.Handler
	text string
	fn   func()
	once *sync.Once
}

func (h *drainOnLine) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *drainOnLine) Handle(ctx context.Context, r slog.Record) error {
	if h.once == nil {
		h.once = new(sync.Once)
	}
	if strings.Contains(r.Message, h.text) {
		h.once.Do(h.fn)
	}
	return h.next.Handle(ctx, r)
}

func (h *drainOnLine) WithAttrs(a []slog.Attr) slog.Handler {
	return &drainOnLine{next: h.next.WithAttrs(a), text: h.text, fn: h.fn, once: h.once}
}

func (h *drainOnLine) WithGroup(n string) slog.Handler {
	return &drainOnLine{next: h.next.WithGroup(n), text: h.text, fn: h.fn, once: h.once}
}

// The pool's phase names and the cache's mark classes are the same set, so a
// phase the pool reports is a mark the cache records.
func TestEveryPoolPhaseIsARecordableMarkClass(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join("endpointpool", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no pool sources (%v)", err)
	}
	var phases []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				typ, ok := value.Type.(*ast.Ident)
				if !ok || typ.Name != "Phase" || len(value.Values) != 1 {
					continue
				}
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok {
					continue
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				phases = append(phases, text)
			}
		}
	}
	if len(phases) < 5 {
		t.Fatalf("found phases %v, want at least the five the pool emits", phases)
	}
	for _, phase := range phases {
		if !endpointpool.Phase(phase).Valid() {
			t.Errorf("phase %q is declared but is not Valid()", phase)
		}
		if !endpointcache.MarkClass(phase).Valid() {
			t.Errorf("phase %q is not a mark class, so the cache would drop its marks", phase)
		}
	}
	classes := []endpointcache.MarkClass{
		endpointcache.MarkDial, endpointcache.MarkDNS, endpointcache.MarkTLSHandshake, endpointcache.MarkTLSVerify,
		endpointcache.MarkPin,
	}
	for _, class := range classes {
		if !slices.Contains(phases, string(class)) {
			t.Errorf("mark class %q is not a phase the pool emits", class)
		}
	}
}

// A cache file that exists but cannot be read makes the directory unusable,
// and the level of the line follows the mode.
func TestACacheFileThatCannotBeReadMakesTheDirectoryUnusable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level slog.Level
	}{{config.DiscoveryModeAuto, slog.LevelDebug}, {config.DiscoveryModeOn, slog.LevelWarn}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := cacheTestConfig(tc.mode, newCacheDir(t))
			seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
			path := cacheFilePath(t, cfg)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

			got := openCacheFor(t, cfg)
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
					got.plan.cacheUnusable)
			}
			if lines := linesAt(got.obs.All(), tc.level, "the cache file could not be read"); len(lines) != 1 {
				t.Errorf("lines at %v = %v; all: %v", tc.level, lines, got.obs.All())
			}
			if e, bad := anyLineAbove(got.obs.All(), tc.level); bad {
				t.Errorf("line %q is above %v", e.Message, tc.level)
			}
			if len(got.plan.endpoints) != 1 || got.plan.install() {
				t.Errorf("endpoints %d and install %v, want the one configured endpoint and no wrapper",
					len(got.plan.endpoints), got.plan.install())
			}
			if !got.plan.failover() && got.plan.discoveryEffective() {
				t.Errorf("an unusable directory left discovery effective")
			}
		})
	}
}

// A cache file this process cannot use is the same as no file. It gives no
// nodes and it keeps the mode on. It is logged once at the mode's level, which
// is Debug under auto and Warn under an explicit on, because the operator asked
// for a cache this call cannot read, and nothing above that level.
func TestACacheFileTheStoreRejectsIsIgnoredAtTheModesLevel(t *testing.T) {
	mutations := map[string]func(t *testing.T, path string){
		"garbage": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("this is not a cache file"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a bad MAC": func(t *testing.T, path string) {
			rewriteEnvelope(t, path, func(env map[string]any) {
				mac, _ := env["hmac_sha256"].(string)
				flipped := "0"
				if strings.HasPrefix(mac, "0") {
					flipped = "1"
				}
				env["hmac_sha256"] = flipped + mac[1:]
			})
		},
		"a future version": func(t *testing.T, path string) {
			rewriteEnvelope(t, path, func(env map[string]any) { env["version"] = 99 })
		},
		"an empty file": func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		for mode, level := range map[config.DiscoveryMode]slog.Level{
			config.DiscoveryModeAuto: slog.LevelDebug,
			config.DiscoveryModeOn:   slog.LevelWarn,
		} {
			t.Run(name+" under "+string(mode), func(t *testing.T) {
				cfg := cacheTestConfig(mode, newCacheDir(t))
				seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
				mutate(t, cacheFilePath(t, cfg))

				got := openCacheFor(t, cfg)
				if got.cache == nil || got.plan.cacheUnusable {
					t.Fatalf("cache %v and unusable %v, want a usable cache with no nodes", got.cache,
						got.plan.cacheUnusable)
				}
				if len(got.plan.endpoints) != 1 {
					t.Errorf("endpoints = %+v, want the configured endpoint alone", got.plan.endpoints)
				}
				if e, bad := anyLineAbove(got.obs.All(), level); bad {
					t.Errorf("a file the store rejects logged %q at %v, above %v", e.Message, e.Level, level)
				}
				const ignored = "the cache file is not usable and is ignored"
				if lines := linesAt(got.obs.All(), level, ignored); len(lines) != 1 {
					t.Errorf("ignored-file lines = %v, want one %v line; all: %v", lines, level, got.obs.All())
				}
				if !got.plan.discoveryEffective() {
					t.Error("discovery is no longer effective after an ignored file")
				}
			})
		}
	}
}

// rewriteEnvelope reads the cache file at path as JSON, lets edit change it,
// and writes it back.
func rewriteEnvelope(t *testing.T, path string, edit func(env map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("the seeded file is not JSON: %v", err)
	}
	edit(env)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestACacheFileForAnotherKeyIsNotTheFileOfThisKey(t *testing.T) {
	dir := newCacheDir(t)
	cfg := cacheTestConfig(config.DiscoveryModeOn, dir)
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	other := cacheTestConfig(config.DiscoveryModeOn, dir)
	other.APIToken = "cpi@pve!other=fedcba9876543210"
	if cacheFilePath(t, cfg) == cacheFilePath(t, other) {
		t.Fatal("two credentials share a cache file")
	}
}

// A stored identity conflict is the one signal that the cache stays empty
// until the operator deletes the file, so it is a Warn, once, and the nodes
// the file holds are not used.
func TestAStoredIdentityConflictWarnsOnceAndLoadsNoNodes(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := cacheTestConfig(mode, newCacheDir(t))
			verify := false
			cfg.VerifySSL = &verify
			seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
			applySeedChange(t, cfg, endpointcache.Change{Conflict: true})

			got := openCacheFor(t, cfg)
			if got.cache == nil {
				t.Fatalf("no cache: %v", got.obs.All())
			}
			if lines := linesAt(got.obs.All(), slog.LevelWarn, "cluster identity differs"); len(lines) != 1 {
				t.Errorf("conflict lines = %v, want one Warn; all: %v", lines, got.obs.All())
			}
			if n := countAt(got.obs.All(), slog.LevelWarn); n != 1 {
				t.Errorf("%d Warn lines, want only the conflict; all: %v", n, got.obs.All())
			}
			if len(got.plan.endpoints) != 1 || len(got.cache.discovered) != 0 {
				t.Errorf("endpoints %+v and discovered %v, want the configured endpoint alone", got.plan.endpoints,
					got.cache.discovered)
			}
		})
	}
}

// The same file read through a whole client leaves a pool of the configured
// endpoint alone and says so once.
func TestAStoredIdentityConflictLeavesTheClientWithoutDiscoveredEndpoints(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
	verify := false
	cfg.VerifySSL = &verify
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	applySeedChange(t, cfg, endpointcache.Change{Conflict: true})
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	if _, err := newClient(cfg, logger, nil); err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { drainProcessRegistry(t) })
	if lines := linesAt(obs.All(), slog.LevelWarn, "cluster identity differs"); len(lines) != 1 {
		t.Errorf("conflict lines = %v, want one Warn; all: %v", lines, obs.All())
	}
	pools := linesAt(obs.All(), slog.LevelDebug, "pve endpoints: pool [")
	if len(pools) != 1 || strings.Contains(pools[0], "discovered") {
		t.Errorf("pool lines = %v, want one pool with no discovered endpoint", pools)
	}
}

// Marks load onto every configured endpoint and every discovered node, and the
// pool starts on the first endpoint that carries none.
func TestMarksLoadOntoEveryConfiguredAndDiscoveredEndpoint(t *testing.T) {
	const (
		first  = "pve1.dc1.example.com:8006"
		second = "pve1b.dc1.example.com:8006"
		third  = "pve3.dc1.example.com:8006"
	)
	build := func(t *testing.T, marks map[string]endpointcache.DownMark) *config.CPIConfig {
		cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
		cfg.Endpoints = []string{first, second}
		seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve3", true}}, marks: marks})
		return cfg
	}
	now := time.Now().UTC().Truncate(time.Second)
	markAt := map[string]time.Time{first: now.Add(-50 * time.Second), second: now.Add(-40 * time.Second),
		third: now.Add(-30 * time.Second)}
	allMarks := map[string]endpointcache.DownMark{}
	for address, at := range markAt {
		allMarks[address] = endpointcache.DownMark{DownAt: at, Class: endpointcache.MarkDial}
	}

	t.Run("the plan carries each mark", func(t *testing.T) {
		got := openCacheFor(t, build(t, allMarks))
		if got.cache == nil || len(got.plan.endpoints) != 3 {
			t.Fatalf("setup: endpoints %+v", got.plan.endpoints)
		}
		for _, ep := range got.plan.endpoints {
			if want := markAt[ep.Address]; !ep.DownMarkedAt.Equal(want) {
				t.Errorf("%s (%s) is marked down at %s, want %s", ep.Address, ep.Kind, ep.DownMarkedAt, want)
			}
		}
	})
	t.Run("the pool starts on the endpoint nobody marked", func(t *testing.T) {
		marks := map[string]endpointcache.DownMark{
			first:  allMarks[first],
			second: allMarks[second],
		}
		wiring, _, _ := buildCachePool(t, build(t, marks))
		away, started := wiring.pool.StartedAway()
		if !started || away.First != first || away.Start != third {
			t.Errorf("start away = %+v (%v), want a start on %s because of %s", away, started, third, first)
		}
	})
	t.Run("a mark on the first and a discovered endpoint starts on the second", func(t *testing.T) {
		marks := map[string]endpointcache.DownMark{
			first: allMarks[first],
			third: allMarks[third],
		}
		wiring, _, _ := buildCachePool(t, build(t, marks))
		away, started := wiring.pool.StartedAway()
		if !started || away.Start != second {
			t.Errorf("start away = %+v (%v), want a start on %s", away, started, second)
		}
	})
}

// A directory secret of the wrong length is replaced, and the operator hears
// of it, because every cache file written under the old one is rebuilt.
func TestAReplacedDirectorySecretWarns(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			dir := newCacheDir(t)
			if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("too short"), 0o600); err != nil {
				t.Fatal(err)
			}
			got := openCacheFor(t, cacheTestConfig(mode, dir))
			if got.cache == nil {
				t.Fatalf("no cache: %v", got.obs.All())
			}
			if lines := linesAt(got.obs.All(), slog.LevelWarn, "replaced the directory secret"); len(lines) != 1 {
				t.Errorf("secret lines = %v, want one Warn; all: %v", lines, got.obs.All())
			}
			info, err := os.Stat(filepath.Join(dir, "secret"))
			if err != nil || info.Size() != 32 {
				t.Errorf("the secret is %v bytes (err %v), want 32", info, err)
			}
			again := openCacheFor(t, cacheTestConfig(mode, dir))
			if lines := linesAt(again.obs.All(), slog.LevelWarn, "replaced the directory secret"); len(lines) != 0 {
				t.Errorf("a second open warned again: %v", lines)
			}
		})
	}
}

// Two cached nodes with one address are one endpoint, and the pool is not
// handed a list it would refuse.
func TestTwoCachedNodesWithOneAddressLoadOnce(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeAuto, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}, {"pve3", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil || len(got.plan.endpoints) != 3 {
		t.Fatalf("setup: endpoints %+v", got.plan.endpoints)
	}
	node := func(name string) endpointcache.DiscoveredNode {
		return endpointcache.DiscoveredNode{
			Name: name, Address: "shared.dc1.example.com", Port: 8006, Pins: []string{pinHex(1)}, Online: true,
		}
	}
	seen := normalizedEndpointSet(configuredAddresses(&got.plan))
	payload := &endpointcache.Payload{}
	if _, ok := got.cache.endpointOf(node("pve8"), payload, seen); !ok {
		t.Fatal("the first node of the address was refused")
	}
	if _, ok := got.cache.endpointOf(node("pve9"), payload, seen); ok {
		t.Error("a second node with the same address was kept")
	}
}

func TestCacheCredentialCarriesTheRealmOfAPasswordUser(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cfg            config.CPIConfig
		user, password string
	}{
		{"a token", config.CPIConfig{APIToken: "cpi@pve!probe=sekret", User: "root", Realm: "pam"},
			"cpi@pve!probe", "sekret"},
		{"a user in the pam realm", config.CPIConfig{User: "root", Realm: "pam", Password: "pw"}, "root@pam", "pw"},
		{"a user in the pve realm", config.CPIConfig{User: "root", Realm: "pve", Password: "pw"}, "root@pve", "pw"},
		{"a user with no realm", config.CPIConfig{User: "root", Password: "pw"}, "root@pam", "pw"},
		{"a user that names its realm", config.CPIConfig{User: "root@pve", Realm: "pam", Password: "pw"},
			"root@pve", "pw"},
		{"no user", config.CPIConfig{Password: "pw"}, "", "pw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, credential := cacheCredential(&tc.cfg)
			if user != tc.user || credential != tc.password {
				t.Errorf("cacheCredential = %q, %q; want %q, %q", user, credential, tc.user, tc.password)
			}
		})
	}
}

// The line that says a call started away from its first endpoint names how long
// ago the mark was written, and a process says it once.
func TestStartedAwayLineNamesTheAgeOfTheMarkAndIsSaidOncePerProcess(t *testing.T) {
	resetStartedAwayLog(t)
	lab := newTetherLab(t)
	first, second := lab.live(t, cachePoolRoutes()), lab.live(t, cachePoolRoutes())
	cfg := lab.config(first, second)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)
	now := time.Now().UTC().Truncate(time.Second)
	setCacheClock(t, now)
	seedEndpointCache(t, cfg, cacheSeed{marks: map[string]endpointcache.DownMark{
		first.Addr: {DownAt: now.Add(-40 * time.Second), Class: endpointcache.MarkDial},
	}})
	want := "pve endpoints: starting on " + second.Addr + " because " + first.Addr + " was marked down 40s ago"

	loggerOne, obsOne := log.NewObservedLogger(log.LevelDebug)
	if _, err := newClient(cfg, loggerOne, nil); err != nil {
		t.Fatalf("newClient: %v", err)
	}
	loggerTwo, obsTwo := log.NewObservedLogger(log.LevelDebug)
	if _, err := newClient(cfg, loggerTwo, nil); err != nil {
		t.Fatalf("newClient: %v", err)
	}
	drainProcessRegistry(t)
	if lines := linesAt(obsOne.All(), slog.LevelInfo, "starting on"); len(lines) != 1 || lines[0] != want {
		t.Errorf("first client lines = %q, want [%q]", lines, want)
	}
	if lines := linesAt(obsTwo.All(), slog.LevelInfo, "starting on"); len(lines) != 0 {
		t.Errorf("second client of the same process repeated the line: %q", lines)
	}
}

// A call that starts after the exit drain began opens nothing, whatever the
// mode, and says so at Debug, because the drain is a state of the process and
// not a fault of the directory.
func TestACallAfterTheDrainBeganTouchesNothing(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			dir := newCacheDir(t)
			cfg := cacheTestConfig(mode, dir)
			endpointcache.Drain(context.Background(), time.Second)
			t.Cleanup(endpointcache.Open)

			got := openCacheFor(t, cfg)
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
					got.plan.cacheUnusable)
			}
			if lines := linesAt(got.obs.All(), slog.LevelDebug, "the exit drain has started"); len(lines) != 1 {
				t.Errorf("drain lines = %v, want one Debug line; all: %v", lines, got.obs.All())
			}
			if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
				t.Errorf("line %q is at %v, want nothing above Debug", e.Message, e.Level)
			}
			if names, err := readDirNames(dir); err != nil || len(names) != 0 {
				t.Errorf("the directory holds %v (err %v), want it untouched", names, err)
			}
		})
	}
}

// The worker's discovery reads go through a client without the caller's tracer,
// so a background refresh adds no span to the call's trace.
func TestWorkerReadsAddNoSpansToTheCallsTrace(t *testing.T) {
	lab := newTetherLab(t)
	routes := cachePoolRoutes()
	routes.JSON(http.MethodGet, "/nodes", http.StatusOK, []any{
		map[string]any{"node": "pve1", "status": "online", "ssl_fingerprint": "AA:BB"},
	})
	routes.JSON(http.MethodGet, "/cluster/status", http.StatusOK, []any{
		map[string]any{"type": "cluster", "name": "lab", "quorate": 1},
	})
	srv := lab.live(t, routes)
	cfg := lab.config(srv)
	cfg.Endpoints = nil
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	cfg.EndpointCacheDir = newCacheDir(t)
	tracer, exporter := newTestTracer(t)

	c, err := newClient(cfg, log.NewNopLogger(), tracer)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	drainProcessRegistry(t)

	if reads := seenPaths(srv, http.MethodGet); !slices.Contains(reads, "/cluster/status") {
		t.Fatalf("the server saw reads %v, want the discovery reads that the traced client would span", reads)
	}
	spans := exporter.GetSpans()
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	if !slices.Contains(names, "pve.pools.get_pool_comment") {
		t.Errorf("spans = %v, want the caller's request", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "pve.cluster.") || strings.HasPrefix(name, "pve.nodes.") {
			t.Errorf("the background refresh added the span %q to the call's trace", name)
		}
	}
}
