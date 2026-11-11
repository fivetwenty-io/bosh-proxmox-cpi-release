package pve

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// discoveryNameToken finds every word that holds endpoint_discovery.
var discoveryNameToken = regexp.MustCompile(`[A-Za-z0-9_.]*endpoint_discovery[A-Za-z0-9_]*`)

// newDiscoveryNames are the names the endpoint cache adds. None of them may
// be a removed key.
var newDiscoveryNames = []string{"api_endpoint_discovery", "endpoint_cache_dir"}

// repoFile reads a file at a path relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestDiscoveryPropertyUsesOneNameEverywhere checks the property in the job
// spec and templates, the cpi-config override keys, the validation messages in
// the Go sources, and the log lines. Every word that holds endpoint_discovery
// is api_endpoint_discovery, with or without the pve prefix, and the removed
// keys carry none of the new names.
func TestDiscoveryPropertyUsesOneNameEverywhere(t *testing.T) {
	var scanned []string
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned = append(scanned, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) < 100 {
		t.Fatalf("scanned only %d Go files, the walk is wrong", len(scanned))
	}
	sources := map[string]string{
		"jobs/pve_cpi/spec":                   repoFile(t, "jobs/pve_cpi/spec"),
		"jobs/pve_cpi/templates/cpi.json.erb": repoFile(t, "jobs/pve_cpi/templates/cpi.json.erb"),
	}
	for _, path := range scanned {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources[path] = string(raw)
	}
	found := 0
	for path, text := range sources {
		for _, token := range discoveryNameToken.FindAllString(text, -1) {
			found++
			bare := strings.TrimPrefix(strings.TrimPrefix(token, "pve."), "pve_")
			if bare != "api_endpoint_discovery" {
				t.Errorf("%s names the property %q, want api_endpoint_discovery", path, token)
			}
		}
	}
	if found < 20 {
		t.Errorf("found the property name %d times, the scan is wrong", found)
	}
	for _, name := range []string{"pve.api_endpoint_discovery", "pve.endpoint_cache_dir"} {
		if !strings.Contains(sources["jobs/pve_cpi/spec"], "  "+name+":") {
			t.Errorf("the job spec does not declare %s", name)
		}
	}
	if !strings.Contains(sources["jobs/pve_cpi/templates/cpi.json.erb"], `"pve.api_endpoint_discovery`) &&
		!strings.Contains(sources["jobs/pve_cpi/templates/cpi.json.erb"], "api_endpoint_discovery") {
		t.Error("the job template does not read api_endpoint_discovery")
	}

	removed := sources[filepath.Join("..", "..", "internal", "config", "removed_keys.go")]
	if removed == "" {
		t.Fatal("removed_keys.go was not scanned")
	}
	for _, name := range newDiscoveryNames {
		if strings.Contains(removed, name) {
			t.Errorf("the removed-keys list holds the new name %s", name)
		}
	}
	list := sources["jobs/pve_cpi/templates/cpi.json.erb"]
	start := strings.Index(list, "removed_placement_keys = [")
	end := strings.Index(list, "removed_set = []")
	if start < 0 || end < start {
		t.Fatal("the template's removed-keys list was not found")
	}
	for _, name := range newDiscoveryNames {
		if strings.Contains(list[start:end], name) {
			t.Errorf("the template's removed-keys list holds the new name %s", name)
		}
	}
}

// TestDiscoveryLogLinesNameTheProperty runs the paths that log about the mode
// and checks that every line that names the property uses its one name.
func TestDiscoveryLogLinesNameTheProperty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	cfg := cacheTestConfig(config.DiscoveryModeOn, dir)
	got := openCacheFor(t, cfg)
	cfg.APIEndpointDiscovery = config.DiscoveryMode("sometimes")
	_, wiring, err := wireEndpoints(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	openEndpointCache(cfg, &wiring.plan, logger)

	entries := append(got.obs.All(), obs.All()...)
	if len(entries) < 2 {
		t.Fatalf("only %d lines were logged", len(entries))
	}
	assertLinesNameTheProperty(t, entries)
}

// assertLinesNameTheProperty checks that every line that names the property
// uses its one name, and that no line at Info or above talks about discovery
// without naming it.
func assertLinesNameTheProperty(t *testing.T, entries []log.Entry) {
	t.Helper()
	for _, e := range entries {
		for _, token := range discoveryNameToken.FindAllString(e.Message, -1) {
			if token != "api_endpoint_discovery" {
				t.Errorf("line %q names the property %q", e.Message, token)
			}
		}
		named := strings.HasPrefix(e.Message, "api endpoint discovery") ||
			strings.Contains(e.Message, "api_endpoint_discovery")
		if e.Level >= slog.LevelInfo && strings.Contains(e.Message, "discovery") && !named {
			t.Errorf("line %q talks about discovery without the property's name", e.Message)
		}
	}
}

// TestColdStartLogLinesNameTheProperty runs the cold start's own paths, which
// are a walk that refuses one candidate and accepts the next, a map that holds
// a value the node-name helper refuses, a seed that derives no address, a gate
// that turns the cold start off, and a hint that cannot be written, under each
// mode, and checks every line it logs the way the mode and directory lines are
// checked.
func TestColdStartLogLinesNameTheProperty(t *testing.T) {
	entries := make([]log.Entry, 0, 64)
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		lab := newColdLab(t)
		cfg := lab.config(t)
		cfg.APIEndpointDiscovery = mode
		cfg.Placement = &config.PlacementConfig{AZMap: map[string][]string{
			"az1": {"pve1", "pve2", "pve3", "bad.name.example"},
		}}
		lab.node("pve2."+coldWireDomain, coldNodeOptions{status: http.StatusForbidden})
		good := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
		c, obs := lab.process(t, cfg)
		mustCreate(t, c, good)
		drainProcessRegistry(t)
		entries = append(entries, obs.All()...)

		// A cache that holds a usable discovered node turns the cold start off.
		gated := lab.config(t)
		gated.APIEndpointDiscovery = mode
		seedEndpointCache(t, gated, cacheSeed{nodes: []cachedNode{{name: "pve4", online: true}}})
		_, gatedObs := lab.process(t, gated)
		drainProcessRegistry(t)
		entries = append(entries, gatedObs.All()...)

		// A seed whose derivation the helper refuses yields no candidates.
		bare := lab.config(t)
		bare.APIEndpointDiscovery = mode
		bare.Host = "pve1.com"
		_, bareObs := lab.process(t, bare)
		drainProcessRegistry(t)
		entries = append(entries, bareObs.All()...)

		// A hint the store cannot write, and a hint the registry refuses.
		logger, hintObs := log.NewObservedLogger(log.LevelDebug)
		failing := &endpointCache{mode: mode, logger: logger, sink: &cacheMarkSink{
			store: &fakeUpdater{err: errors.New("the disk is full")}, mode: mode, logger: logger, now: time.Now,
			start: endpointcache.Go,
		}}
		failing.writeHint(context.Background(), "pve3", time.Now())
		endpointcache.Drain(context.Background(), time.Second)
		failing.recordColdStart("pve3")
		endpointcache.Open()
		entries = append(entries, hintObs.All()...)
	}

	for _, want := range []string{
		"leaves out a node whose name is not a usable node name",
		"from placement.az_map refused",
		"no cold start from placement.az_map",
		"the cold start from placement.az_map has no candidates",
		"the cold-start hint could not be recorded",
		"the cold-start hint is not recorded",
	} {
		found := false
		for _, e := range entries {
			if strings.Contains(e.Message, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no cold-start line contains %q, so the scan is not reaching it", want)
		}
	}
	assertLinesNameTheProperty(t, entries)
}
