package storageinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock                   { return &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type fakeSource struct {
	mu              sync.Mutex
	defs            []json.RawMessage
	statuses        map[string][]json.RawMessage
	errors          map[string]error
	definitionErr   error
	definitionCalls int
	calls           map[string]int
	hook            func(context.Context, string, int) ([]json.RawMessage, error)
}

func (s *fakeSource) Definitions(context.Context) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.definitionCalls++
	return slices.Clone(s.defs), s.definitionErr
}
func (s *fakeSource) Statuses(ctx context.Context, node string) ([]json.RawMessage, error) {
	s.mu.Lock()
	s.calls[node]++
	call := s.calls[node]
	hook := s.hook
	data, err := slices.Clone(s.statuses[node]), s.errors[node]
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, node, call)
	}
	return data, err
}
func (s *fakeSource) set(node string, statuses ...json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[node] = statuses
}
func (s *fakeSource) counts() (int, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]int{}
	for k, v := range s.calls {
		m[k] = v
	}
	return s.definitionCalls, m
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func definition(t *testing.T, id string, extra map[string]any) json.RawMessage {
	t.Helper()
	m := map[string]any{"storage": id, "type": "nfs", "server": "NAS", "export": "/" + id, "content": "images,iso", "shared": 1}
	for k, v := range extra {
		m[k] = v
	}
	return raw(t, m)
}
func status(t *testing.T, id string, total, available uint64) json.RawMessage {
	t.Helper()
	return raw(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": total, "avail": available})
}
func policy(ids ...string) *config.CPIConfig {
	return &config.CPIConfig{EphemeralStorageSet: "active", StorageSets: map[string]config.StorageSet{"active": {Names: ids, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
}
func sourceFor(t *testing.T, ids ...string) *fakeSource {
	t.Helper()
	s := &fakeSource{statuses: map[string][]json.RawMessage{}, errors: map[string]error{}, calls: map[string]int{}}
	for _, id := range ids {
		s.defs = append(s.defs, definition(t, id, nil))
		s.statuses["n1"] = append(s.statuses["n1"], status(t, id, 1000, 600))
		s.statuses["n2"] = append(s.statuses["n2"], status(t, id, 1000, 500))
	}
	return s
}
func collector(t *testing.T, s Source, clock *fakeClock) *Collector {
	t.Helper()
	c, err := NewCollector(s, Options{Clock: clock.Now, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func discover(t *testing.T, c *Collector, cfg *config.CPIConfig, nodes ...string) *Snapshot {
	t.Helper()
	s, err := c.Discover(context.Background(), cfg, Request{Nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDiscoveryConsolidatesPhysicalCapacityAndFreezesMembership(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a", "b")
	c := collector(t, source, clock)
	cfg := policy("a")
	set := cfg.StorageSets["active"]
	set.Names = nil
	set.NamePattern = "^[ab]$"
	cfg.StorageSets["active"] = set
	cfg.StorageSets["unused"] = config.StorageSet{Names: []string{"unavailable-unused"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}
	s := discover(t, c, cfg, "n2", "n1")
	defs, calls := source.counts()
	if defs != 1 || calls["n1"] != 1 || calls["n2"] != 1 {
		t.Fatalf("not one initial fetch per definition/node: %d %v", defs, calls)
	}
	members, ok := s.Members("active")
	if !ok || !slices.Equal(members, []string{"a", "b"}) {
		t.Fatalf("members: %v", members)
	}
	b, ok := s.Backing("nfs://nas/a")
	if !ok || b.TotalBytes != 1000 || b.AvailableBytes != 500 {
		t.Fatalf("physical capacity multiplied: %+v", b)
	}
	// All accessor copies and the original policy can change without affecting s.
	members[0] = "corrupt"
	b.Reports[0].Generation = 0
	nodes := s.Nodes()
	nodes[0] = "corrupt"
	cfg.StorageSets["active"] = config.StorageSet{}
	if got, _ := s.Members("active"); got[0] != "a" {
		t.Fatal("members exposed mutable aliases")
	}
	if got, _ := s.Backing("nfs://nas/a"); got.Reports[0].Generation == 0 {
		t.Fatal("reports exposed mutable aliases")
	}
	source.mu.Lock()
	source.defs = append(source.defs, definition(t, "added", nil))
	source.mu.Unlock()
	fresh, err := c.Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.Members("active"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatal("refresh expanded frozen membership")
	}
	defs, _ = source.counts()
	if defs != 1 {
		t.Fatal("status refresh rediscovered definitions")
	}
}

func TestMissingIDsAliasesEncryptionAndDisjointness(t *testing.T) {
	t.Parallel()
	t.Run("all missing IDs", func(t *testing.T) {
		s := sourceFor(t, "a")
		cfg := policy("z", "missing")
		cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"d": {Members: []string{"also-missing"}}}
		_, err := collector(t, s, newClock()).Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}})
		if err == nil {
			t.Fatal("accepted missing literals")
		}
		for _, id := range []string{"z", "missing", "also-missing"} {
			if !strings.Contains(err.Error(), id) {
				t.Fatalf("missing ID omitted: %v", err)
			}
		}
	})
	t.Run("aliases within set", func(t *testing.T) {
		s := sourceFor(t, "a", "b")
		s.defs[1] = definition(t, "b", map[string]any{"server": "nas", "export": "//a/"})
		_, err := collector(t, s, newClock()).Discover(context.Background(), policy("a", "b"), Request{Nodes: []string{"n1"}})
		if err == nil || !strings.Contains(err.Error(), "aliases") {
			t.Fatalf("alias validation: %v", err)
		}
	})
	t.Run("policy aliases legal", func(t *testing.T) {
		s := sourceFor(t, "a")
		cfg := policy("a")
		cfg.StorageSets["other"] = cfg.StorageSets["active"]
		_, err := collector(t, s, newClock()).Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}, SetNames: []string{"other"}})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("regex assertion conflict", func(t *testing.T) {
		s := sourceFor(t, "a", "b")
		s.defs[1] = definition(t, "b", map[string]any{"export": "/a"})
		cfg := policy("a")
		yes, no := true, false
		a := cfg.StorageSets["active"]
		a.Encrypted = &yes
		cfg.StorageSets["active"] = a
		cfg.StorageSets["other"] = config.StorageSet{NamePattern: "^b$", Encrypted: &no, Strategy: a.Strategy}
		_, err := collector(t, s, newClock()).Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}, SetNames: []string{"other"}})
		if err == nil || !strings.Contains(err.Error(), "contradictory") {
			t.Fatalf("alias encryption: %v", err)
		}
	})
	for _, explicitRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("disjoint root=%v", explicitRoot), func(t *testing.T) {
			s := sourceFor(t, "a", "b", "e")
			s.defs[1] = definition(t, "b", map[string]any{"export": "/a"})
			cfg := policy("a")
			cfg.PersistentStorageSet = "persistent"
			cfg.StorageSets["persistent"] = config.StorageSet{Names: []string{"b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}
			if explicitRoot {
				cfg.RootStorageSet = "active"
				cfg.EphemeralStorageSet = "ephemeral"
				cfg.StorageSets["ephemeral"] = config.StorageSet{Names: []string{"e"}, Strategy: cfg.StorageSets["active"].Strategy}
			}
			c := collector(t, s, newClock())
			if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}}); err == nil {
				t.Fatal("disjointness accepted physical overlap")
			}
			allow := false
			cfg.RequireDisjointStorageSets = &allow
			if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}}); err != nil {
				t.Fatalf("explicit overlap permission rejected: %v", err)
			}
		})
	}
}

func TestDomainsIncludeOutsideMembersAndRejectBackingDuplicates(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a", "b")
	source.set("n1", status(t, "a", 2000, 900), status(t, "b", 1000, 600))
	source.set("n2", status(t, "a", 2000, 800), status(t, "b", 1000, 500))
	cfg := policy("a")
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"shared": {Members: []string{"a", "b"}}}
	c := collector(t, source, clock)
	s := discover(t, c, cfg, "n1", "n2")
	d, ok := s.Domain("shared")
	if !ok || d.Reason != "" || d.TotalBytes != 1000 || d.AvailableBytes != 500 || len(d.CapacityKeys) != 2 {
		t.Fatalf("domain envelope: %+v", d)
	}
	d.Members[0] = "corrupt"
	d.Reports[0].Generation = 0
	if original, _ := s.Domain("shared"); original.Members[0] != "a" || original.Reports[0].Generation == 0 {
		t.Fatal("domain leaked aliases")
	}
	source.set("n1", status(t, "a", 2000, 900), raw(t, map[string]any{"storage": "b", "active": 0, "enabled": 1}))
	source.set("n2", status(t, "a", 2000, 800), raw(t, map[string]any{"storage": "b", "active": 0, "enabled": 1}))
	fresh, err := c.Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	d, _ = fresh.Domain("shared")
	if d.Reason == "" {
		t.Fatal("domain omitted unusable outside member")
	}
	if _, err := NewLedger().Candidate(fresh, "n1", "a", 1, Limits{}); err == nil {
		t.Fatal("admitted unavailable domain")
	}
	source.defs[1] = definition(t, "b", map[string]any{"export": "/a"})
	if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}}); err == nil {
		t.Fatal("accepted exact aliases in domain")
	}
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"one": {Members: []string{"a"}}, "two": {Members: []string{"b"}}}
	if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}}); err == nil {
		t.Fatal("same backing admitted to multiple domains")
	}
}

func TestStrictObservationFailuresAndHealthyPairs(t *testing.T) {
	t.Parallel()
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`{"storage":"a","active":1,"enabled":1,"total":0,"avail":0}`), json.RawMessage(`{"storage":"a","active":1,"enabled":1,"total":100,"avail":101}`), json.RawMessage(`{"storage":"a","active":1,"enabled":1,"total":100,"avail":-1}`), json.RawMessage(`{"storage":"a","active":1,"enabled":1,"total":100.1,"avail":1}`), json.RawMessage(`{"storage":"a","active":1,"enabled":1,"total":"100x","avail":1}`), json.RawMessage(`{"storage":"a","active":null,"enabled":1,"total":100,"avail":1}`)} {
		t.Run(bad.String(), func(t *testing.T) {
			source := sourceFor(t, "a")
			if bad == nil {
				source.statuses["n1"] = nil
			} else {
				source.set("n1", bad)
			}
			c := collector(t, source, newClock())
			s := discover(t, c, policy("a"), "n1", "n2")
			badPair, _ := s.Pair("n1", "a")
			healthy, _ := s.Pair("n2", "a")
			if badPair.Reason == "" || healthy.Reason != "" || len(s.Issues()) == 0 {
				t.Fatalf("partial failure lost: %+v %+v %v", badPair, healthy, s.Issues())
			}
			_, err := c.Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1"}})
			var observation *ObservationError
			if !errors.As(err, &observation) {
				t.Fatalf("all failed observations became success: %v", err)
			}
		})
	}
	for _, scenario := range []string{"authorization", "nil definitions", "malformed definition"} {
		t.Run(scenario, func(t *testing.T) {
			source := sourceFor(t, "a")
			switch scenario {
			case "authorization":
				source.definitionErr = errors.New("permission denied")
			case "nil definitions":
				source.defs = nil
			case "malformed definition":
				source.defs = []json.RawMessage{json.RawMessage(`{"storage":"a","type":"nfs","shared":1.5}`)}
			}
			_, err := collector(t, source, newClock()).Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1"}})
			var observation *ObservationError
			if !errors.As(err, &observation) {
				t.Fatalf("expected observation error: %v", err)
			}
		})
	}
	for _, value := range []string{`"18446744073709551615"`, `18446744073709551615`, `0`, `"17"`} {
		if _, err := exactUint(json.RawMessage(value)); err != nil {
			t.Fatalf("valid exact integer %s: %v", value, err)
		}
	}
	for _, value := range []string{`18446744073709551616`, `1e3`, `" 1"`, `"01"`, `null`, `true`, `1.0`} {
		if _, err := exactUint(json.RawMessage(value)); err == nil {
			t.Fatalf("accepted lossy integer %s", value)
		}
	}
}

func TestInactiveDisabledRestrictionsAndMixedLocalCompanions(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "a", "inactive", "disabled", "restricted", "local")
	source.defs[2] = definition(t, "disabled", map[string]any{"disable": 1})
	source.defs[3] = definition(t, "restricted", map[string]any{"nodes": "n2"})
	source.defs[4] = definition(t, "local", map[string]any{"type": "dir", "path": "/var/lib/vz", "shared": 0})
	source.set("n1", status(t, "a", 1000, 600), raw(t, map[string]any{"storage": "inactive", "active": 0, "enabled": 1}), status(t, "disabled", 1000, 500), status(t, "restricted", 1000, 500), status(t, "local", 1000, 400))
	source.set("n2", status(t, "a", 1000, 500), raw(t, map[string]any{"storage": "inactive", "active": 0, "enabled": 1}), status(t, "disabled", 1000, 500), status(t, "restricted", 1000, 500), status(t, "local", 2000, 1500))
	c := collector(t, source, newClock())
	s, err := c.Discover(context.Background(), policy("a", "inactive", "disabled", "restricted"), Request{Nodes: []string{"n1", "n2"}, CompanionStorageIDs: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"inactive", "disabled", "restricted"} {
		if pair, _ := s.Pair("n1", id); pair.Reason == "" {
			t.Fatalf("admitted %s", id)
		}
	}
	if pair, _ := s.Pair("n2", "restricted"); pair.Reason != "" {
		t.Fatalf("lost healthy restricted node: %+v", pair)
	}
	n1, _ := s.Pair("n1", "local")
	n2, _ := s.Pair("n2", "local")
	if n1.Reason != "" || n2.Reason != "" || n1.BackingKey != n2.BackingKey || n1.CapacityKey == n2.CapacityKey {
		t.Fatalf("local capacity was collapsed: %+v %+v", n1, n2)
	}
	b1, _ := s.Backing(n1.CapacityKey)
	b2, _ := s.Backing(n2.CapacityKey)
	if b1.TotalBytes != 1000 || b2.TotalBytes != 2000 {
		t.Fatal("local totals consolidated across nodes")
	}
	if _, err := NewLedger().Candidate(s, "n1", "local", 300, Limits{}); err != nil {
		t.Fatalf("supported legacy companion rejected: %v", err)
	}
	if info, ok := s.Definition("restricted"); !ok {
		t.Fatal("missing definition")
	} else {
		info.Nodes[0] = "n1"
		original, _ := s.Definition("restricted")
		if original.Nodes[0] != "n2" {
			t.Fatal("definition nodes leaked alias")
		}
	}
	setMode := discover(t, c, policy("local"), "n1", "n2")
	if pair, _ := setMode.Pair("n1", "local"); pair.Reason == "" {
		t.Fatal("set accepted non-NFS companion")
	}
}

func TestContradictoryTotalsRefreshOnce(t *testing.T) {
	t.Parallel()
	for _, heals := range []bool{false, true} {
		t.Run(fmt.Sprint(heals), func(t *testing.T) {
			source := sourceFor(t, "a")
			source.hook = func(_ context.Context, node string, call int) ([]json.RawMessage, error) {
				total := uint64(1000)
				if node == "n2" && (!heals || call == 1) {
					total = 2000
				}
				return []json.RawMessage{status(t, "a", total, 500)}, nil
			}
			c := collector(t, source, newClock())
			_, err := c.Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1", "n2"}})
			if heals && err != nil {
				t.Fatal(err)
			}
			if !heals && err == nil {
				t.Fatal("persistent contradictory totals accepted")
			}
			defs, calls := source.counts()
			if defs != 1 || calls["n1"] != 2 || calls["n2"] != 2 {
				t.Fatalf("refresh count: %d %v", defs, calls)
			}
		})
	}
}

func TestDefinitionRevalidationPreservesOldIdentity(t *testing.T) {
	t.Parallel()
	for _, change := range []map[string]any{{"export": "/repointed"}, {"server": "other"}, {"disable": 1}, {"content": "iso"}, {"nodes": "n2"}, {"type": "cifs"}} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			source := sourceFor(t, "a")
			c := collector(t, source, newClock())
			s := discover(t, c, policy("a"), "n1")
			source.defs[0] = definition(t, "a", change)
			if err := c.RevalidateDefinitions(context.Background(), s, []string{"a"}); err == nil {
				t.Fatal("changed safety fields accepted")
			}
			old, _ := s.Definition("a")
			if old.BackingKey() != "nfs://nas/a" {
				t.Fatal("old plan was repointed")
			}
		})
	}
	source := sourceFor(t, "a")
	c := collector(t, source, newClock())
	s := discover(t, c, policy("a"), "n1")
	source.defs[0] = definition(t, "a", map[string]any{"server": "nas", "export": "//a/", "content": "iso,images"})
	if err := c.RevalidateDefinitions(context.Background(), s, []string{"a"}); err != nil {
		t.Fatalf("equivalent definition rejected: %v", err)
	}
}

func TestBoundedConcurrencyAndCancellation(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "a")
	entered := make(chan string, 4)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	source.hook = func(ctx context.Context, node string, _ int) ([]json.RawMessage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for previous := maxActive.Load(); n > previous; previous = maxActive.Load() {
			if maxActive.CompareAndSwap(previous, n) {
				break
			}
		}
		entered <- node
		select {
		case <-release:
			return []json.RawMessage{status(t, "a", 1000, 500)}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := collector(t, source, newClock())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Discover(ctx, policy("a"), Request{Nodes: []string{"n1", "n2", "n3", "n4"}})
		done <- err
	}()
	<-entered
	<-entered
	if maxActive.Load() != 2 {
		t.Fatalf("unexpected active workers: %d", maxActive.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if active.Load() != 0 {
		t.Fatal("workers survived cancellation")
	}
	close(release)
}

func TestObservationFreshnessUsesRequestStart(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a")
	source.hook = func(context.Context, string, int) ([]json.RawMessage, error) {
		clock.Advance(6 * time.Second)
		return []json.RawMessage{status(t, "a", 1000, 500)}, nil
	}
	c := collector(t, source, clock)
	if _, err := c.Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1"}}); err == nil {
		t.Fatal("slow response received a new freshness lifetime")
	}
	source.hook = nil
	s := discover(t, c, policy("a"), "n1")
	if !s.Fresh(clock.Now()) {
		t.Fatal("fresh snapshot rejected")
	}
	clock.Advance(6 * time.Second)
	if s.Fresh(clock.Now()) {
		t.Fatal("expired snapshot accepted")
	}
	clock.Advance(-time.Hour)
	if s.Fresh(clock.Now()) {
		t.Fatal("backward clock extended snapshot life")
	}
}

func TestLegacyTierUsesInitialDefinitionsAndLexicalChoice(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "z-local", "a-local", "nfs")
	for i, id := range []string{"z-local", "a-local"} {
		source.defs[i] = definition(t, id, map[string]any{"type": "dir", "shared": 0, "path": "/" + id})
	}
	cfg := policy("nfs")
	local, shared := false, true
	cfg.StorageTiers = map[string]config.StorageTierCriteria{"local": {Types: []string{"DIR"}, Shared: &local}, "shared": {Types: []string{"NFS"}, Shared: &shared}}
	c := collector(t, source, newClock())
	s, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}, TierNames: []string{"local", "shared"}})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := s.TierStorage("local"); !ok || id != "a-local" {
		t.Fatalf("tier choice: %q %v", id, ok)
	}
	if id, ok := s.TierStorage("shared"); !ok || id != "nfs" {
		t.Fatalf("shared tier choice: %q %v", id, ok)
	}
	if pair, ok := s.Pair("n1", "a-local"); !ok || pair.Reason != "" {
		t.Fatalf("tier companion was not collected: %+v", pair)
	}
	if _, ok := s.Pair("n1", "z-local"); ok {
		t.Fatal("unused tier alternative collected")
	}
	defs, calls := source.counts()
	if defs != 1 || calls["n1"] != 1 {
		t.Fatalf("extra tier discovery: %d %v", defs, calls)
	}
	source.set("n1", status(t, "nfs", 1000, 500), raw(t, map[string]any{"storage": "a-local", "active": 0, "enabled": 1}), status(t, "z-local", 1000, 500))
	fresh, err := c.Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := fresh.TierStorage("local"); id != "a-local" {
		t.Fatal("inactive tier silently selected a different storage")
	}
	for _, name := range []string{"unknown", "no-match"} {
		cfg.StorageTiers["no-match"] = config.StorageTierCriteria{Types: []string{"absent"}}
		if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}, TierNames: []string{name}}); err == nil {
			t.Fatal("invalid tier succeeded")
		}
	}
}
