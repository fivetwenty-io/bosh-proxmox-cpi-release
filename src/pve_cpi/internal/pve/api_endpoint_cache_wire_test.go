package pve

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file build clients through newClient with a cache directory
// and send requests to loopback servers. They drain the process registry, so
// none of them runs in parallel.

// cachePoolRoutes answers the pool comment read the tests send.
func cachePoolRoutes() *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	return routes
}

// readPoolComment sends one read through c and fails the test when it errors.
func readPoolComment(t *testing.T, c Client) {
	t.Helper()
	if _, _, err := c.Pools().GetPoolComment(context.Background(), "p1"); err != nil {
		t.Fatalf("GetPoolComment: %v", err)
	}
}

// seenPaths lists the API-relative paths the server read with method.
func seenPaths(s *pvetest.Server, method string) []string {
	var out []string
	for _, r := range s.Seen() {
		if r.Method == method {
			out = append(out, strings.TrimPrefix(r.Path, pvetest.APIPrefix))
		}
	}
	return out
}

func TestHostOnlyTokenWithAnEmptyDirectoryRunsDiscoveryAfterTheFirstAnsweredRequest(t *testing.T) {
	lab := newTetherLab(t)
	srv := lab.live(t, cachePoolRoutes())
	cfg := lab.config(srv)
	cfg.Endpoints = nil
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	cfg.EndpointCacheDir = newCacheDir(t)
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if got := srv.Seen(); len(got) != 0 {
		t.Fatalf("building the client sent %v, want no request", got)
	}
	readPoolComment(t, c)
	drainProcessRegistry(t)

	reads := seenPaths(srv, http.MethodGet)
	if !slices.Contains(reads, "/pools/p1") || !slices.Contains(reads, "/nodes") {
		t.Fatalf("server saw reads %v, want the pool read and then the discovery reads", reads)
	}
	if reads[0] != "/pools/p1" {
		t.Errorf("the first request was %s, want the caller's read before any discovery read", reads[0])
	}
	payload := readSeededCache(t, cfg)
	if payload == nil || payload.Discovery == nil {
		t.Fatalf("the cache file holds no discovery section after the drain: %+v", payload)
	}
	if e, bad := anyLineAbove(obs.All(), slog.LevelInfo); bad {
		t.Errorf("a discovery that found nothing to report logged %q at %v", e.Message, e.Level)
	}
}

func TestPasswordClientUnderAutoCreatesNoCacheFileAndNoDiscoveryRead(t *testing.T) {
	lab := newTetherLab(t)
	srv := lab.live(t, cachePoolRoutes())
	cfg := lab.config(srv)
	cfg.Endpoints = nil
	cfg.APIToken = ""
	cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	cfg.EndpointCacheDir = newCacheDir(t)

	c, err := newClient(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	res := drainProcessRegistry(t)
	if res.Started != 0 {
		t.Errorf("the drain saw %d workers, want none", res.Started)
	}
	if names, err := readDirNames(cfg.EndpointCacheDir); err != nil || len(names) != 0 {
		t.Errorf("cache directory holds %v (err %v), want no file", names, err)
	}
	if reads := seenPaths(srv, http.MethodGet); slices.Contains(reads, "/nodes") {
		t.Errorf("a password client under auto sent a discovery read: %v", reads)
	}
}

func TestACallThatFailsOverAndReturnsAtOnceStillWritesItsMarkBeforeExit(t *testing.T) {
	dead := deadWireEndpoint(t)
	live := liveWireEndpoint(t)
	cfg := wireConfig(dead, live)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)

	c, err := newClient(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	// The call returns here, as a CPI process does, and the exit drain runs.
	drainProcessRegistry(t)

	payload := readSeededCache(t, cfg)
	mark, ok := payload.Marks[dead.address()]
	if !ok {
		t.Fatalf("marks %v, want one for the endpoint that refused the connection", payload.Marks)
	}
	if mark.Class != endpointcache.MarkDial || time.Since(mark.DownAt) > time.Minute {
		t.Errorf("mark = %+v, want a fresh dial mark", mark)
	}
	if _, ok := payload.Marks[live.address()]; ok {
		t.Errorf("the endpoint that answered carries a mark: %v", payload.Marks)
	}
}

func TestACachedMarkMakesTheNextCallStartOnTheSecondEndpointAndSayOnce(t *testing.T) {
	lab := newTetherLab(t)
	first, second := lab.live(t, cachePoolRoutes()), lab.live(t, cachePoolRoutes())
	cfg := lab.config(first, second)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)
	seedEndpointCache(t, cfg, cacheSeed{marks: map[string]endpointcache.DownMark{
		first.Addr: {DownAt: time.Now().UTC().Add(-40 * time.Second), Class: endpointcache.MarkDial},
	}})
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	readPoolComment(t, c)
	drainProcessRegistry(t)

	if got := seenCount(first, http.MethodGet, "/pools/p1"); got != 0 {
		t.Errorf("the marked first endpoint saw %d reads, want none", got)
	}
	if got := seenCount(second, http.MethodGet, "/pools/p1"); got != 2 {
		t.Errorf("the second endpoint saw %d reads, want both", got)
	}
	want := "pve endpoints: starting on " + second.Addr + " because " + first.Addr + " was marked down"
	if lines := linesAt(obs.All(), slog.LevelInfo, want); len(lines) != 1 {
		t.Errorf("started-away lines = %v, want one Info line", lines)
	}
	if e, bad := anyLineAbove(obs.All(), slog.LevelInfo); bad {
		t.Errorf("unexpected %v line %q", e.Level, e.Message)
	}
}

func TestAMarkThatHasRunOutStartsOnTheFirstEndpointAndSaysNothing(t *testing.T) {
	lab := newTetherLab(t)
	first, second := lab.live(t, cachePoolRoutes()), lab.live(t, cachePoolRoutes())
	cfg := lab.config(first, second)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)
	seedEndpointCache(t, cfg, cacheSeed{marks: map[string]endpointcache.DownMark{
		first.Addr: {DownAt: time.Now().UTC().Add(-3 * time.Minute), Class: endpointcache.MarkDial},
	}})
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	readPoolComment(t, c)
	drainProcessRegistry(t)
	if got := seenCount(first, http.MethodGet, "/pools/p1"); got != 1 {
		t.Errorf("the first endpoint saw %d reads, want the one the call sent", got)
	}
	if lines := linesAt(obs.All(), slog.LevelInfo, "starting on"); len(lines) != 0 {
		t.Errorf("started-away lines = %v, want none", lines)
	}
}

func TestATicketLoginAndAConfiguredOnlyRequestNeverReachADiscoveredEndpoint(t *testing.T) {
	first, second := deadWireEndpoint(t), deadWireEndpoint(t)
	build := func(t *testing.T, password bool) Client {
		t.Helper()
		cfg := wireConfig(first, second)
		verify := true
		cfg.VerifySSL = &verify
		cfg.APIEndpointDiscovery = config.DiscoveryModeOn
		cfg.EndpointCacheDir = newCacheDir(t)
		if password {
			cfg.APIToken = ""
			cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
		}
		seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{"pve9", true}}})
		c, err := newClient(cfg, log.NewNopLogger(), nil)
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		// The refused dials write their marks in the background.
		t.Cleanup(func() { drainProcessRegistry(t) })
		return c
	}
	discovered := net.JoinHostPort("pve9", strconv.Itoa(first.port))
	walked := func(t *testing.T, err error) []string {
		t.Helper()
		var walk *endpointpool.EntryUnreachableError
		if !errors.As(err, &walk) {
			t.Fatalf("error %v is not a walk that reached no endpoint", err)
		}
		return walk.Endpoints()
	}
	configured := []string{first.address(), second.address()}

	t.Run("a ticket login", func(t *testing.T) {
		c := build(t, true)
		_, _, err := c.Pools().GetPoolComment(context.Background(), "p1")
		if err == nil {
			t.Fatal("GetPoolComment with every configured endpoint down = nil error")
		}
		if got := walked(t, err); !slices.Equal(got, configured) {
			t.Errorf("the login walked %v, want only the configured endpoints %v", got, configured)
		}
	})
	t.Run("a configured-only request", func(t *testing.T) {
		c := build(t, false)
		ctx := endpointpool.WithConfiguredOnly(context.Background())
		_, _, err := c.Pools().GetPoolComment(ctx, "p1")
		if err == nil {
			t.Fatal("GetPoolComment with every configured endpoint down = nil error")
		}
		if got := walked(t, err); !slices.Equal(got, configured) {
			t.Errorf("the configured-only request walked %v, want only %v", got, configured)
		}
	})
	t.Run("an ordinary request does reach it", func(t *testing.T) {
		c := build(t, false)
		_, _, err := c.Pools().GetPoolComment(context.Background(), "p1")
		if err == nil {
			t.Fatal("GetPoolComment with every endpoint down = nil error")
		}
		if got := walked(t, err); !slices.Equal(got, append(slices.Clone(configured), discovered)) {
			t.Errorf("the ordinary request walked %v, want the configured endpoints and then %s", got, discovered)
		}
	})
}
