package pve

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The tests in this file cover the down marks of a call whose discovery is
// off. They drain the process registry, so none of them runs in parallel.

func TestDiscoveryOffWithAUsableDirectoryWritesAMarkAndTheNextProcessSkipsTheDeadEndpoint(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeOff, config.DiscoveryModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			dead := deadWireEndpoint(t)
			live := liveWireEndpoint(t)
			cfg := wireConfig(dead, live)
			cfg.APIEndpointDiscovery = mode
			cfg.EndpointCacheDir = newCacheDir(t)

			// The first process meets the dead endpoint and fails over.
			c, err := newClient(cfg, log.NewNopLogger(), nil)
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}
			readPoolComment(t, c)
			drainProcessRegistry(t)

			payload := readSeededCache(t, cfg)
			mark, ok := payload.Marks[dead.address()]
			if !ok || mark.Class != endpointcache.MarkDial || time.Since(mark.DownAt) > time.Minute {
				t.Fatalf("marks %v, want a fresh dial mark for the dead endpoint", payload.Marks)
			}
			if _, ok := payload.Marks[live.address()]; ok {
				t.Errorf("the live endpoint carries a mark: %v", payload.Marks)
			}
			if payload.Discovery != nil {
				t.Errorf("a call with discovery off wrote a discovery section: %+v", payload.Discovery)
			}
			assertPrivateCacheFiles(t, cfg.EndpointCacheDir)

			// The next process reads the mark and starts on the live endpoint.
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			next, err := newClient(cfg, logger, nil)
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}
			readPoolComment(t, next)
			drainProcessRegistry(t)
			want := "pve endpoints: starting on " + live.address() + " because " + dead.address() + " was marked down"
			if lines := linesAt(obs.All(), slog.LevelInfo, want); len(lines) != 1 {
				t.Errorf("started-away lines = %v, want one Info line", lines)
			}
			assertNoEndpointNoise(t, obs.All(), slog.LevelInfo)
		})
	}
}

func TestDiscoveryOffWithNoUsableDirectoryWritesNothingAndAddsNoWarning(t *testing.T) {
	dead := deadWireEndpoint(t)
	live := liveWireEndpoint(t)
	cfg := wireConfig(dead, live)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOff
	missing := filepath.Join(t.TempDir(), "no-parent", "missing")
	cfg.EndpointCacheDir = missing
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	if res := drainProcessRegistry(t); res.Started != 0 {
		t.Errorf("the drain saw %d workers, want none", res.Started)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the missing directory was touched, stat error %v", err)
	}
	assertNoEndpointNoise(t, obs.All(), slog.LevelDebug)
}

func TestDiscoveryOffWithOneEndpointWritesNoFile(t *testing.T) {
	live := liveWireEndpoint(t)
	cfg := wireConfig(live)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOff
	cfg.EndpointCacheDir = newCacheDir(t)

	c, err := newClient(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	drainProcessRegistry(t)
	if names, err := readDirNames(cfg.EndpointCacheDir); err != nil || len(names) != 0 {
		t.Errorf("cache directory holds %v (err %v), want no file", names, err)
	}
}

func TestDiscoveryOffCacheHoldsMarksOnly(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeOff, newCacheDir(t))
	first := net.JoinHostPort(cacheTestHost, strconv.Itoa(cacheTestPort))
	cfg.Endpoints = []string{first, "pve2.example.com:8006"}
	seedEndpointCache(t, cfg, cacheSeed{
		nodes: []cachedNode{{"pve3", true}},
		marks: map[string]endpointcache.DownMark{
			first: {DownAt: time.Now().UTC().Add(-20 * time.Second), Class: endpointcache.MarkDial},
		},
	})
	got := openCacheFor(t, cfg)
	if got.cache == nil || !got.cache.marksOnly {
		t.Fatalf("cache = %+v, want a marks-only cache", got.cache)
	}
	if got.cache.worker != nil || len(got.cache.discovered) != 0 || len(got.plan.endpoints) != 2 {
		t.Errorf("a marks-only cache holds a worker or discovered nodes: %+v", got.plan.endpoints)
	}
	if got.plan.endpoints[0].DownMarkedAt.IsZero() {
		t.Error("the marked configured endpoint carries no mark")
	}
}

func TestDiscoveryOnStillBuildsTheWorkerAndLoadsNodes(t *testing.T) {
	cfg := cacheTestConfig(config.DiscoveryModeOn, newCacheDir(t))
	seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve2", true}}})
	got := openCacheFor(t, cfg)
	if got.cache == nil || got.cache.marksOnly || got.cache.worker == nil {
		t.Fatalf("cache = %+v, want a full cache with a worker", got.cache)
	}
	if len(got.plan.endpoints) != 2 {
		t.Errorf("endpoints = %+v, want the seed and the cached node", got.plan.endpoints)
	}
}

// assertPrivateCacheFiles fails the test when a cache file is readable by
// anyone but its owner.
func assertPrivateCacheFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("cache directory holds %v (err %v), want the cache files", entries, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v, want owner-only", e.Name(), info.Mode().Perm())
		}
	}
}

// assertNoEndpointNoise fails the test on a line above level, other than the
// TLS warning every unverified test config logs.
func assertNoEndpointNoise(t *testing.T, entries []log.Entry, level slog.Level) {
	t.Helper()
	for _, e := range entries {
		if e.Level > level && !strings.Contains(e.Message, "TLS verification disabled") &&
			!strings.Contains(e.Message, "pve endpoint failover:") {
			t.Errorf("unexpected %v line %q", e.Level, e.Message)
		}
	}
}
