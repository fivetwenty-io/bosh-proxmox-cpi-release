package ha_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const cacheToken = "cpi@pve!ha=11111111-2222-3333-4444-555555555555"

// cacheClock is the profile cache's clock in these tests.
type cacheClock struct{ t time.Time }

func (c *cacheClock) Now() time.Time          { return c.t }
func (c *cacheClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newCacheClock() *cacheClock {
	return &cacheClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
}

// cacheDir returns a fresh private directory for the override.
func cacheDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cacheCfg is a cluster entry that keys a cache in override.
func cacheCfg(override string) *config.CPIConfig {
	cfg := cfgMode(config.HAModeAuto)
	cfg.Host = "pve1.dc1.example.com"
	cfg.APIToken = cacheToken
	cfg.EndpointCacheDir = override
	return cfg
}

// cachedDeps wires the fake cluster behind the profile cache for cfg.
func cachedDeps(t *testing.T, w *world, cfg *config.CPIConfig, clock *cacheClock) ha.Deps {
	t.Helper()
	opts, ok := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if !ok {
		t.Fatal("the entry cannot key a profile cache")
	}
	opts.Now = clock.Now
	return w.deps(cfg).WithProfileCache(opts)
}

func resolveWith(t *testing.T, d ha.Deps) *ha.Profile {
	t.Helper()
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return p
}

// slowProbes counts the slow-changing probes the fake cluster has answered.
func slowProbes(w *world) int { return w.cluster.calls["confignodes"] }

func profileFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ha-profile-") && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestProfileCacheAFreshEntryAnswersWithoutProbing(t *testing.T) {
	dir := cacheDir(t)
	w := three()
	clock := newCacheClock()
	d := cachedDeps(t, w, cacheCfg(dir), clock)

	first := resolveWith(t, d)
	managerAfterFirst, statusAfterFirst := w.cluster.calls["manager"], w.cluster.calls["status"]
	if slowProbes(w) != 1 || w.storage.calls != 1 {
		t.Fatalf("the first call must probe once, got %d config reads and %d storage reads", slowProbes(w), w.storage.calls)
	}
	files := profileFiles(t, dir)
	if len(files) != 1 || !regexp.MustCompile(`^ha-profile-pve1\.dc1\.example\.com-[0-9a-f]{16}\.json$`).MatchString(files[0]) {
		t.Fatalf("profile files %v, want one ha-profile-<host>-<digest>.json", files)
	}

	clock.advance(59 * time.Second)
	second := resolveWith(t, d)
	if slowProbes(w) != 1 || w.storage.calls != 1 {
		t.Errorf("a fresh entry must not probe again, got %d config reads and %d storage reads", slowProbes(w), w.storage.calls)
	}
	if !reflect.DeepEqual(second.Nodes, first.Nodes) || !reflect.DeepEqual(second.Quorum, first.Quorum) ||
		!reflect.DeepEqual(second.CPUDomains, first.CPUDomains) || !reflect.DeepEqual(second.Storages, first.Storages) ||
		second.Effective != first.Effective || second.MinVersion != first.MinVersion || second.SysConsole != first.SysConsole {
		t.Errorf("cached profile %+v differs from the live one %+v", second, first)
	}
	if w.cluster.calls["manager"] != 2*managerAfterFirst || w.cluster.calls["status"] != 2*statusAfterFirst {
		t.Errorf("node health must be read on every call, got manager %d status %d after one call of %d and %d",
			w.cluster.calls["manager"], w.cluster.calls["status"], managerAfterFirst, statusAfterFirst)
	}
}

func TestProfileCacheAnExpiredEntryProbesAndIsReplaced(t *testing.T) {
	dir := cacheDir(t)
	w := three()
	clock := newCacheClock()
	d := cachedDeps(t, w, cacheCfg(dir), clock)
	resolveWith(t, d)

	clock.advance(ha.ProfileCacheTTL)
	resolveWith(t, d)
	if slowProbes(w) != 2 {
		t.Fatalf("an entry exactly one TTL old must probe again, got %d config reads", slowProbes(w))
	}
	resolveWith(t, d)
	if slowProbes(w) != 2 {
		t.Errorf("the replaced entry must answer, got %d config reads", slowProbes(w))
	}
}

func TestProfileCacheAFutureTimestampIsAMiss(t *testing.T) {
	dir := cacheDir(t)
	w := three()
	clock := newCacheClock()
	d := cachedDeps(t, w, cacheCfg(dir), clock)
	resolveWith(t, d)

	clock.advance(-time.Minute)
	resolveWith(t, d)
	if slowProbes(w) != 2 {
		t.Errorf("an entry stored in the future must not answer, got %d config reads", slowProbes(w))
	}
}

func TestProfileCacheACorruptFileProbesAndIsRebuilt(t *testing.T) {
	dir := cacheDir(t)
	w := three()
	clock := newCacheClock()
	d := cachedDeps(t, w, cacheCfg(dir), clock)
	resolveWith(t, d)
	path := filepath.Join(dir, profileFiles(t, dir)[0])

	for name, data := range map[string]string{
		"garbage":      "not json",
		"empty":        "",
		"wrong hmac":   `{"version":1,"hmac_sha256":"00","payload":{}}`,
		"unknown keys": `{"version":1,"hmac_sha256":"00","payload":{},"extra":1}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		before := slowProbes(w)
		p := resolveWith(t, d)
		if slowProbes(w) != before+1 {
			t.Errorf("%s: a damaged file must probe, got %d config reads from %d", name, slowProbes(w), before)
		}
		if p.Effective != ha.EffectiveFull {
			t.Errorf("%s: the profile must be the live one, got %s", name, p.Effective)
		}
		again := slowProbes(w)
		resolveWith(t, d)
		if slowProbes(w) != again {
			t.Errorf("%s: the rebuilt file must answer", name)
		}
	}
}

func TestProfileCacheAMissingDirectoryProbesEveryCall(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	// The temporary layer is the episode record's fallback, and the profile
	// cache has none, so nothing may appear there.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	w := three()
	d := cachedDeps(t, w, cacheCfg(missing), newCacheClock())
	for i := 1; i <= 2; i++ {
		p := resolveWith(t, d)
		if p.Effective != ha.EffectiveFull || slowProbes(w) != i {
			t.Fatalf("call %d: effective %s after %d config reads, want a live probe each time", i, p.Effective, slowProbes(w))
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the override directory must never be created: %v", err)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("a missing override must not fall back to the temporary layer: %v, %v", entries, err)
	}
}

// debugErrors returns the err attribute of every Debug line of logs whose
// message contains message.
func debugErrors(logs *log.Observer, message string) []string {
	var out []string
	for _, e := range logs.All() {
		if e.Level == slog.LevelDebug && strings.Contains(e.Message, message) {
			out = append(out, fmt.Sprint(e.Attrs["error"]))
		}
	}
	return out
}

// otherUsersPrivateDirectory returns a directory that another user owns and
// that grants no access to anyone else, so only the owner check can refuse it.
func otherUsersPrivateDirectory(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/private/var/root", "/var/root", "/root"} {
		info, err := os.Lstat(candidate)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			return candidate
		}
	}
	t.Skip("no mode 0700 directory owned by another user to refuse")
	return ""
}

func TestProfileCacheAnUnsafeDirectoryProbesEveryCall(t *testing.T) {
	cases := map[string]struct {
		build func(t *testing.T) string
		// refusal is the text of the refusal that must have fired, which tells
		// the mode check from the owner check.
		refusal string
	}{
		"mode open to others": {
			build: func(t *testing.T) string {
				dir := cacheDir(t)
				if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the test makes the directory unsafe on purpose
					t.Fatal(err)
				}
				return dir
			},
			refusal: "unsafe permissions",
		},
		"owned by another user": {
			build:   otherUsersPrivateDirectory,
			refusal: "unsafe ownership",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := tc.build(t)
			w := three()
			cfg := cacheCfg(dir)
			opts, _ := ha.ProfileCacheOptionsFromConfig(cfg, nil)
			logger, logs := log.NewObservedLogger(slog.LevelDebug)
			opts.Log = logger
			opts.Now = newCacheClock().Now
			d := w.deps(cfg).WithProfileCache(opts)
			resolveWith(t, d)
			resolveWith(t, d)
			if slowProbes(w) != 2 {
				t.Errorf("an unsafe directory must give no cache, got %d config reads", slowProbes(w))
			}
			if len(profileFiles(t, dir)) != 0 {
				t.Errorf("an unsafe directory must stay unwritten")
			}
			refusals := debugErrors(logs, "no usable cache directory")
			if len(refusals) != 2 {
				t.Fatalf("logged %d refusals, want one per call", len(refusals))
			}
			for _, text := range refusals {
				if !strings.Contains(text, tc.refusal) {
					t.Errorf("refusal %q does not say %q", text, tc.refusal)
				}
			}
		})
	}
}

func TestProfileCacheASingleHostEntryHasAKey(t *testing.T) {
	cfg := cacheCfg(cacheDir(t))
	cfg.Endpoints = nil
	cfg.Port = 8443
	opts, ok := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if !ok {
		t.Fatal("an entry with only pve.host must key a cache")
	}
	if got := opts.Key.Endpoints; len(got) != 1 || got[0] != "pve1.dc1.example.com:8443" {
		t.Errorf("endpoint list %v, want the one-entry list made from host and port", got)
	}
	cfg.Port = 0
	opts, _ = ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if got := opts.Key.Endpoints; len(got) != 1 || got[0] != "pve1.dc1.example.com:8006" {
		t.Errorf("endpoint list %v, want the default port", got)
	}

	dir := cacheDir(t)
	w := three()
	d := cachedDeps(t, w, cacheCfg(dir), newCacheClock())
	resolveWith(t, d)
	resolveWith(t, d)
	if slowProbes(w) != 1 || len(profileFiles(t, dir)) != 1 {
		t.Errorf("a single-host entry must cache: %d config reads, files %v", slowProbes(w), profileFiles(t, dir))
	}
}

func TestProfileCacheTheEndpointListKeysTheFile(t *testing.T) {
	dir := cacheDir(t)
	clock := newCacheClock()
	one := cacheCfg(dir)
	pool := cacheCfg(dir)
	pool.Endpoints = []string{"pve1.dc1.example.com:8006", "pve2.dc1.example.com:8006"}

	first, second := three(), three()
	resolveWith(t, cachedDeps(t, first, one, clock))
	resolveWith(t, cachedDeps(t, second, pool, clock))
	if slowProbes(second) != 1 || len(profileFiles(t, dir)) != 2 {
		t.Errorf("two endpoint lists must keep two files: %d config reads, files %v",
			slowProbes(second), profileFiles(t, dir))
	}
}

func TestProfileCacheTheOverrideCanBeADurablePath(t *testing.T) {
	durable := filepath.Join(t.TempDir(), "store", "director", "pve_cpi-endpoint-cache")
	if err := os.MkdirAll(durable, 0o700); err != nil {
		t.Fatal(err)
	}
	w := three()
	clock := newCacheClock()
	resolveWith(t, cachedDeps(t, w, cacheCfg(durable), clock))
	if len(profileFiles(t, durable)) != 1 {
		t.Fatalf("the profile must land in the override, which holds %v", profileFiles(t, durable))
	}
	// A new process finds the entry the earlier one left.
	w2 := three()
	resolveWith(t, cachedDeps(t, w2, cacheCfg(durable), clock))
	if slowProbes(w2) != 0 {
		t.Errorf("a new process must read the stored entry, got %d config reads", slowProbes(w2))
	}
}

func TestProfileCacheDefaultsToTheEndpointCacheDirectory(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ha-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	w := three()
	d := cachedDeps(t, w, cacheCfg(""), newCacheClock())
	resolveWith(t, d)
	resolveWith(t, d)
	if slowProbes(w) != 1 {
		t.Fatalf("the temporary layer must cache, got %d config reads", slowProbes(w))
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "pve_cpi-endpoint-cache.*", "ha-profile-*.json"))
	if err != nil || len(matches) != 1 {
		t.Errorf("the profile must sit in $TMPDIR/pve_cpi-endpoint-cache.<euid>: %v, %v", matches, err)
	}
}

func TestProfileCacheANodeSetChangeProbes(t *testing.T) {
	dir := cacheDir(t)
	clock := newCacheClock()
	w := three()
	resolveWith(t, cachedDeps(t, w, cacheCfg(dir), clock))

	grown := nNodes(4)
	resolveWith(t, cachedDeps(t, grown, cacheCfg(dir), clock))
	if slowProbes(grown) != 1 {
		t.Errorf("a node that joined must force a probe, got %d config reads", slowProbes(grown))
	}
	shrunk := nNodes(3)
	shrunk.nodes[2].name = "pve9"
	resolveWith(t, cachedDeps(t, shrunk, cacheCfg(dir), clock))
	if slowProbes(shrunk) != 1 {
		t.Errorf("a renamed node must force a probe, got %d config reads", slowProbes(shrunk))
	}
}

func TestProfileCacheAnOnlineStateChangeProbes(t *testing.T) {
	dir := cacheDir(t)
	clock := newCacheClock()
	w := three()
	resolveWith(t, cachedDeps(t, w, cacheCfg(dir), clock))

	down := three()
	down.nodes[2].online = false
	resolveWith(t, cachedDeps(t, down, cacheCfg(dir), clock))
	if slowProbes(down) != 1 {
		t.Errorf("a node that went offline must force a probe, got %d config reads", slowProbes(down))
	}
	// The offline node was not asked for its version, so coming back needs one.
	back := three()
	resolveWith(t, cachedDeps(t, back, cacheCfg(dir), clock))
	if slowProbes(back) != 1 {
		t.Errorf("a node that came back must force a probe, got %d config reads", slowProbes(back))
	}
}

func TestProfileCacheNeverStoresAnIncompleteReading(t *testing.T) {
	cases := map[string]func(w *world){
		"version unreadable":     func(w *world) { w.nodes[1].versionErr = errors.New("timeout") },
		"node status unreadable": func(w *world) { w.nodes[1].statusErr = errors.New("timeout") },
		"no CPU in the status":   func(w *world) { w.nodes[1].noCPU = true },
		"rules unreadable":       func(w *world) { w.rulesErr = errors.New("501 not implemented") },
		"config nodes":           func(w *world) { w.configNodesErr = errors.New("timeout") },
		"qdevice":                func(w *world) { w.qdeviceErr = errors.New("timeout") },
		"permissions":            func(w *world) { w.permsErr = errors.New("403") },
		"storages":               func(w *world) { w.storageErr = errors.New("timeout") },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			dir := cacheDir(t)
			w := three()
			build(w)
			d := cachedDeps(t, w, cacheCfg(dir), newCacheClock())
			resolveWith(t, d)
			resolveWith(t, d)
			if slowProbes(w) != 2 {
				t.Errorf("an incomplete reading must not answer the next call, got %d config reads", slowProbes(w))
			}
			if len(profileFiles(t, dir)) != 0 {
				t.Errorf("an incomplete reading must not be stored, found %v", profileFiles(t, dir))
			}
		})
	}
}

func TestProfileCacheNeverHoldsTheQDeviceStatusOrNodeHealth(t *testing.T) {
	dir := cacheDir(t)
	w := nNodes(2)
	w.qdeviceBody = json.RawMessage(`{"votes":1,"connected":1}`)
	d := cachedDeps(t, w, cacheCfg(dir), newCacheClock())
	live := resolveWith(t, d)
	if live.QDevice.Status != ha.QDeviceConnected {
		t.Fatalf("live QDevice status = %s", live.QDevice.Status)
	}
	data, err := os.ReadFile(filepath.Join(dir, profileFiles(t, dir)[0])) // #nosec G304 -- the test reads the file it caused
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload struct {
			Facts struct {
				QDevice ha.QDevice
			}
		}
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if q := env.Payload.Facts.QDevice; !q.Configured || q.Votes != 1 || q.Status != ha.QDeviceUnknown {
		t.Errorf("stored QDevice %+v, want its configuration without a connection status", q)
	}
	for _, banned := range []string{"connected", "lrm", "watchdog", "HealthErr", "wait_for_agent_lock"} {
		if strings.Contains(string(data), banned) {
			t.Errorf("the file mentions %q, which it must never hold", banned)
		}
	}

	// A cached call reads the status again.
	w.qdeviceBody = json.RawMessage(`{"votes":1,"connected":0}`)
	cached := resolveWith(t, d)
	if slowProbes(w) != 1 {
		t.Fatalf("the second call must come from the cache, got %d config reads", slowProbes(w))
	}
	if cached.QDevice.Status != ha.QDeviceDisconnected {
		t.Errorf("cached call QDevice status = %s, want the fresh disconnected", cached.QDevice.Status)
	}
}

func TestProfileCacheARotatedCredentialIsAMiss(t *testing.T) {
	dir := cacheDir(t)
	clock := newCacheClock()
	w := three()
	resolveWith(t, cachedDeps(t, w, cacheCfg(dir), clock))

	rotated := cacheCfg(dir)
	rotated.APIToken = "cpi@pve!ha=99999999-2222-3333-4444-555555555555"
	other := three()
	resolveWith(t, cachedDeps(t, other, rotated, clock))
	if slowProbes(other) != 1 {
		t.Errorf("a rotated token must not read the old entry, got %d config reads", slowProbes(other))
	}
}

func TestProfileCacheAnEntryWithoutACredentialHasNoCache(t *testing.T) {
	cases := map[string]func(cfg *config.CPIConfig){
		"no credential": func(cfg *config.CPIConfig) { cfg.APIToken = "" },
		"no host":       func(cfg *config.CPIConfig) { cfg.Host = "" },
		"bad endpoint":  func(cfg *config.CPIConfig) { cfg.Endpoints = []string{"bad host:xx"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := cacheCfg(cacheDir(t))
			mutate(cfg)
			if _, ok := ha.ProfileCacheOptionsFromConfig(cfg, nil); ok {
				t.Error("the entry must not key a cache")
			}
		})
	}
	if _, ok := ha.ProfileCacheOptionsFromConfig(nil, nil); ok {
		t.Error("a nil config must not key a cache")
	}
}

func TestProfileCacheAPasswordEntryKeysWithItsRealm(t *testing.T) {
	cfg := cacheCfg(cacheDir(t))
	cfg.APIToken = ""
	cfg.User = "cpi"
	cfg.Password = "secret"
	opts, ok := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if !ok || opts.Key.User != "cpi@pam" || opts.Credential != "secret" {
		t.Errorf("options %+v, ok %v, want user cpi@pam", opts.Key, ok)
	}
	cfg.User, cfg.Realm = "cpi", "pve"
	opts, _ = ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if opts.Key.User != "cpi@pve" {
		t.Errorf("user %q, want cpi@pve", opts.Key.User)
	}
}

func TestDepsFromClientWiresTheCacheOnlyForAnEntryThatCanKeyOne(t *testing.T) {
	keyed := ha.DepsFromClient(&depsClient{}, cacheCfg(cacheDir(t)), nil)
	if keyed.Slow == nil {
		t.Error("an entry with a host and a credential must get the profile cache")
	}
	bare := ha.DepsFromClient(&depsClient{}, cfgMode("auto"), nil)
	if bare.Slow != nil {
		t.Error("an entry with no host or credential must probe every call")
	}
}

// failingLive is a live source that fails.
type failingLive struct{ err error }

func (f failingLive) Slow(context.Context, []ha.NodeRef) (*ha.SlowFacts, error) { return nil, f.err }

func TestProfileCachePassesALiveFailureThrough(t *testing.T) {
	cfg := cacheCfg(cacheDir(t))
	opts, _ := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	cache := ha.NewProfileCache(failingLive{err: errors.New("boom")}, opts)
	if _, err := cache.Slow(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Slow error = %v, want the live source's", err)
	}
	if _, err := ha.NewProfileCache(nil, opts).Slow(context.Background(), nil); err == nil {
		t.Error("a cache with no live source must fail closed")
	}
}

// funcLive is a live source made from a function.
type funcLive func(context.Context, []ha.NodeRef) (*ha.SlowFacts, error)

func (f funcLive) Slow(ctx context.Context, nodes []ha.NodeRef) (*ha.SlowFacts, error) {
	return f(ctx, nodes)
}

// cleanFacts builds fully clean slow facts for nodes.
func cleanFacts(nodes []ha.NodeRef, version string) *ha.SlowFacts {
	f := &ha.SlowFacts{
		Nodes:          map[string]ha.NodeSlowFacts{},
		RulesSupported: true,
		ConfigVotes:    map[string]int{},
		QDevice:        ha.QDevice{Read: true, Status: ha.QDeviceNone},
		SysConsole:     true,
	}
	for _, n := range nodes {
		f.Nodes[n.Name] = ha.NodeSlowFacts{
			Probed: n.Online, Version: version, CPU: ha.CPUInfo{Known: true, Model: "Xeon A"},
		}
		f.ConfigVotes[n.Name] = 1
	}
	return f
}

func TestProfileCacheKeepsANewerEntryAnotherProcessWroteWhileProbing(t *testing.T) {
	dir := cacheDir(t)
	cfg := cacheCfg(dir)
	nodes := []ha.NodeRef{{Name: "pve1", Online: true}, {Name: "pve2", Online: true}}
	clock := newCacheClock()
	opts, _ := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	opts.Now = clock.Now

	// The other process stores facts a few seconds after this one began.
	other := ha.NewProfileCache(funcLive(func(_ context.Context, n []ha.NodeRef) (*ha.SlowFacts, error) {
		return cleanFacts(n, "9.2.9"), nil
	}), opts)

	mine := ha.NewProfileCache(funcLive(func(ctx context.Context, n []ha.NodeRef) (*ha.SlowFacts, error) {
		clock.advance(5 * time.Second)
		if _, err := other.Slow(ctx, n); err != nil {
			t.Error(err)
		}
		return cleanFacts(n, "9.2.1"), nil
	}), opts)
	got, err := mine.Slow(context.Background(), nodes)
	if err != nil || got.Nodes["pve1"].Version != "9.2.1" {
		t.Fatalf("this call must return its own probe: %+v, %v", got, err)
	}

	clock.advance(time.Second)
	reader := ha.NewProfileCache(funcLive(func(context.Context, []ha.NodeRef) (*ha.SlowFacts, error) {
		t.Error("the stored entry must answer")
		return nil, errors.New("probed")
	}), opts)
	cached, err := reader.Slow(context.Background(), nodes)
	if err != nil || !cached.Cached || cached.Nodes["pve1"].Version != "9.2.9" {
		t.Errorf("the newer entry must stay: %+v, %v", cached, err)
	}
}

func TestProfileCacheLogsAReplacedSecretOnceAtWarn(t *testing.T) {
	dir := cacheDir(t)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cacheCfg(dir)
	opts, _ := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	opts.Log = logger
	opts.Now = newCacheClock().Now
	w := three()
	d := w.deps(cfg).WithProfileCache(opts)

	warnings := func() []log.Entry {
		var out []log.Entry
		for _, e := range logs.All() {
			if e.Level == slog.LevelWarn {
				out = append(out, e)
			}
		}
		return out
	}
	resolveWith(t, d)
	got := warnings()
	if len(got) != 1 || !strings.Contains(got[0].Message, "replaced the cache directory secret") ||
		got[0].Attrs["path"] != secret {
		t.Fatalf("warnings %+v, want one that names the replaced secret %s", got, secret)
	}
	resolveWith(t, d)
	resolveWith(t, d)
	if len(warnings()) != 1 {
		t.Errorf("later calls logged more warnings: %+v", warnings())
	}
	if data, err := os.ReadFile(secret); err != nil || len(data) != 32 { // #nosec G304 -- the test reads its own secret
		t.Errorf("the secret must now be 32 bytes: %d bytes, %v", len(data), err)
	}
	if slowProbes(w) != 1 {
		t.Errorf("the profile must cache after the secret is replaced, got %d config reads", slowProbes(w))
	}
}

func TestProfileCacheWriteSweepsOrphanedFiles(t *testing.T) {
	dir := cacheDir(t)
	clock := newCacheClock()
	touch := func(name string, age time.Duration) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := clock.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	const orphan = "ha-profile-pve1.dc1.example.com-0123456789abcdef.json"
	const orphanLock = "ha-profile-pve1.dc1.example.com-0123456789abcdef.lock"
	const recent = "ha-profile-pve1.dc1.example.com-fedcba9876543210.json"
	const staleTemp = ".tmp-0123456789abcdef0123456789abcdef"
	touch(orphan, 31*24*time.Hour)
	touch(orphanLock, 31*24*time.Hour)
	touch(recent, 29*24*time.Hour)
	touch(staleTemp, 2*time.Hour)

	w := three()
	d := cachedDeps(t, w, cacheCfg(dir), clock)
	resolveWith(t, d)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
	}
	for _, gone := range []string{orphan, orphanLock, staleTemp} {
		if have[gone] {
			t.Errorf("%s survived the write's sweep", gone)
		}
	}
	if !have[recent] {
		t.Errorf("the recent profile file %s was swept", recent)
	}
	files := profileFiles(t, dir)
	if len(files) != 2 {
		t.Errorf("profile files %v, want the new one and the recent one", files)
	}
	// A call that answers from the cache writes nothing and so sweeps nothing.
	touch(orphan, 31*24*time.Hour)
	resolveWith(t, d)
	if _, err := os.Lstat(filepath.Join(dir, orphan)); err != nil {
		t.Errorf("a call served from the cache must not sweep: %v", err)
	}
}

func TestProfileCacheMessagesCallTheFileTheHAProfileCache(t *testing.T) {
	dir := cacheDir(t)
	cfg := cacheCfg(dir)
	opts, _ := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	opts.Log = logger
	opts.Now = newCacheClock().Now
	w := three()
	d := w.deps(cfg).WithProfileCache(opts)
	resolveWith(t, d)
	path := filepath.Join(dir, profileFiles(t, dir)[0])

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolveWith(t, d)
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	resolveWith(t, d)

	unusable := debugErrors(logs, "stored file not usable")
	if len(unusable) != 2 {
		t.Fatalf("logged %d unusable-file lines, want 2", len(unusable))
	}
	for i, want := range []string{"invalid file", "unsafe permissions"} {
		if !strings.Contains(unusable[i], "HA profile cache") || strings.Contains(unusable[i], "endpoint cache") ||
			!strings.Contains(unusable[i], want) {
			t.Errorf("line %d err %q, want it to name the HA profile cache and say %q", i, unusable[i], want)
		}
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "endpoint cache") {
			t.Errorf("message %q calls the profile file the endpoint cache", e.Message)
		}
	}
}

func TestProfileCacheOptionsCarryTheDirectoryOptionsFromTheConfig(t *testing.T) {
	cfg := cacheCfg("/var/vcap/store/director/pve_cpi-endpoint-cache")
	cfg.StorageAllocationJournalDir = "/var/vcap/store/director/pve_cpi-journal"
	opts, ok := ha.ProfileCacheOptionsFromConfig(cfg, nil)
	if !ok {
		t.Fatal("the entry must key a cache")
	}
	if want := endpointcache.OptionsFor(cfg); opts.Dir != want {
		t.Errorf("Dir = %+v, want what OptionsFor builds, %+v", opts.Dir, want)
	}
	if opts.Dir.JournalDir != cfg.StorageAllocationJournalDir {
		t.Errorf("the journal directory did not reach the cache: %+v", opts.Dir)
	}
}
