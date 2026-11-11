package ha

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// stubLive is a live source that counts its calls and answers with facts.
type stubLive struct {
	facts func(nodes []NodeRef) *SlowFacts
	calls int
}

func (s *stubLive) Slow(_ context.Context, _ string, nodes []NodeRef) (*SlowFacts, error) {
	s.calls++
	return s.facts(nodes), nil
}

var cacheTestNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// cacheTestCluster is the cluster name the cache tests ask about.
const cacheTestCluster = "lab"

func internalNodes(names ...string) []NodeRef {
	out := make([]NodeRef, 0, len(names))
	for _, n := range names {
		out = append(out, NodeRef{Name: n, Online: true})
	}
	return out
}

// internalCleanFacts builds fully clean slow facts for nodes.
func internalCleanFacts(nodes []NodeRef) *SlowFacts {
	f := &SlowFacts{
		Nodes:          map[string]NodeSlowFacts{},
		RulesSupported: true,
		ConfigVotes:    map[string]int{},
		QDevice:        QDevice{Read: true},
		SysConsole:     true,
	}
	for _, n := range nodes {
		f.Nodes[n.Name] = NodeSlowFacts{
			Probed: n.Online, Version: "9.2.1", CPU: CPUInfo{Known: true, Model: "Xeon A", Flags: []string{"avx"}},
		}
		f.ConfigVotes[n.Name] = 1
	}
	return f
}

// internalCache builds a profile cache over live whose directory is a fresh
// private override and whose clock is fixed.
func internalCache(t *testing.T, live SlowSource) *ProfileCache {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logger, _ := log.NewObservedLogger(log.LevelDebug)
	return NewProfileCache(live, ProfileCacheOptions{
		Dir: endpointcache.DirOptions{Override: dir},
		Key: endpointcache.KeyConfig{
			Endpoints: []string{"pve1.dc1.example.com:8006"}, VerifySSL: true, User: "cpi@pve!ha",
		},
		Credential: "11111111-2222-3333-4444-555555555555",
		Log:        logger,
		Now:        func() time.Time { return cacheTestNow },
	})
}

// seedProfile stores facts in the cache's file as though a probe had made them
// at stored, bypassing the checks the cache's own write makes.
func seedProfile(t *testing.T, c *ProfileCache, stored time.Time, facts SlowFacts) {
	t.Helper()
	store := c.openStore(context.Background(), log.NewNopLogger())
	if store == nil {
		t.Fatal("the cache cannot open its store")
	}
	defer func() { _ = store.Close() }()
	written, err := endpointcache.Update(context.Background(), store, func(*profilePayload) (*profilePayload, error) {
		return &profilePayload{
			Key:            store.Key(),
			Cluster:        cacheTestCluster,
			StoredAtMillis: stored.UnixMilli(),
			Facts:          facts,
		}, nil
	})
	if err != nil || !written {
		t.Fatalf("seed the profile file: written %v, err %v", written, err)
	}
}

// storedProfile reads the cache's file back, or nil when it holds none.
func storedProfile(t *testing.T, c *ProfileCache) *profilePayload {
	t.Helper()
	store := c.openStore(context.Background(), log.NewNopLogger())
	if store == nil {
		t.Fatal("the cache cannot open its store")
	}
	defer func() { _ = store.Close() }()
	var got profilePayload
	if err := store.Read(&got); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read the profile file: %v", err)
	}
	return &got
}

// factFaults are the ways a reading can be incomplete or inconsistent. Every
// one must keep the facts out of the file and out of an answer.
var factFaults = map[string]func(f *SlowFacts){
	"rules error":        func(f *SlowFacts) { f.RulesErr = "501" },
	"votes error":        func(f *SlowFacts) { f.ConfigVotesErr = "timeout" },
	"nil votes":          func(f *SlowFacts) { f.ConfigVotes = nil },
	"permissions error":  func(f *SlowFacts) { f.PermissionsErr = "403" },
	"storages error":     func(f *SlowFacts) { f.StoragesErr = "timeout" },
	"qdevice not read":   func(f *SlowFacts) { f.QDevice.Read = false },
	"qdevice read error": func(f *SlowFacts) { f.QDevice.ReadErr = "timeout" },
	"version error": func(f *SlowFacts) {
		f.Nodes["pve2"] = withNode(f.Nodes["pve2"], func(n *NodeSlowFacts) { n.VersionErr = "x" })
	},
	"empty version": func(f *SlowFacts) {
		f.Nodes["pve2"] = withNode(f.Nodes["pve2"], func(n *NodeSlowFacts) { n.Version = "" })
	},
	"cpu error": func(f *SlowFacts) {
		f.Nodes["pve2"] = withNode(f.Nodes["pve2"], func(n *NodeSlowFacts) { n.CPUErr = "x" })
	},
	"cpu unknown, no error": func(f *SlowFacts) {
		f.Nodes["pve2"] = withNode(f.Nodes["pve2"], func(n *NodeSlowFacts) { n.CPU.Known = false })
	},
}

func withNode(n NodeSlowFacts, change func(*NodeSlowFacts)) NodeSlowFacts {
	change(&n)
	return n
}

func TestProfileCacheDoesNotStoreFactsWithAnyFault(t *testing.T) {
	nodes := internalNodes("pve1", "pve2", "pve3")
	for name, fault := range factFaults {
		t.Run(name, func(t *testing.T) {
			live := &stubLive{facts: func(n []NodeRef) *SlowFacts {
				f := internalCleanFacts(n)
				fault(f)
				return f
			}}
			c := internalCache(t, live)
			for i := 1; i <= 2; i++ {
				if _, err := c.Slow(context.Background(), cacheTestCluster, nodes); err != nil {
					t.Fatal(err)
				}
				if live.calls != i {
					t.Fatalf("call %d: %d live probes, want one per call because nothing may be stored", i, live.calls)
				}
			}
			if got := storedProfile(t, c); got != nil {
				t.Errorf("the file holds %+v, want none", got)
			}
		})
	}
}

func TestProfileCacheDoesNotAnswerFromStoredFactsWithAnyFault(t *testing.T) {
	nodes := internalNodes("pve1", "pve2", "pve3")
	for name, fault := range factFaults {
		t.Run(name, func(t *testing.T) {
			live := &stubLive{facts: internalCleanFacts}
			c := internalCache(t, live)
			faulty := internalCleanFacts(nodes)
			fault(faulty)
			seedProfile(t, c, cacheTestNow.Add(-time.Second), *faulty)

			got, err := c.Slow(context.Background(), cacheTestCluster, nodes)
			if err != nil || live.calls != 1 || got.Cached {
				t.Fatalf(
					"a stored reading with a fault answered: live probes %d, cached %v, err %v",
					live.calls,
					got != nil && got.Cached,
					err,
				)
			}
			if stored := storedProfile(
				t,
				c,
			); stored == nil || stored.Facts.RulesErr != "" || stored.Facts.ConfigVotes == nil {
				t.Errorf("the clean live reading must replace the faulty file, got %+v", stored)
			}
		})
	}
}

func TestProfileCacheAFileWithADifferentNodeCountIsAMiss(t *testing.T) {
	// The stored facts name three nodes and the call asks about two. The
	// per-name check passes, because both requested nodes are stored, so only
	// the count catches a node that left the cluster.
	live := &stubLive{facts: internalCleanFacts}
	c := internalCache(t, live)
	three := internalNodes("pve1", "pve2", "pve3")
	two := internalNodes("pve1", "pve2")

	if _, err := c.Slow(context.Background(), cacheTestCluster, three); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Slow(context.Background(), cacheTestCluster, three); err != nil || live.calls != 1 {
		t.Fatalf("the same three nodes must answer from the file, got %d probes, err %v", live.calls, err)
	}
	got, err := c.Slow(context.Background(), cacheTestCluster, two)
	if err != nil || live.calls != 2 || got.Cached || len(got.Nodes) != 2 {
		t.Fatalf("a node that left must force a probe: %d probes, cached %v, nodes %v, err %v",
			live.calls, got != nil && got.Cached, got.Nodes, err)
	}
	if stored := storedProfile(t, c); stored == nil || len(stored.Facts.Nodes) != 2 {
		t.Errorf("the file must now describe the two nodes, got %+v", stored)
	}
}

func TestProfileCacheDoesNotStoreFactsForOtherNodesThanAskedAbout(t *testing.T) {
	extra := &stubLive{
		facts: func([]NodeRef) *SlowFacts { return internalCleanFacts(internalNodes("pve1", "pve2", "pve3")) },
	}
	c := internalCache(t, extra)
	two := internalNodes("pve1", "pve2")
	for i := 1; i <= 2; i++ {
		if _, err := c.Slow(context.Background(), cacheTestCluster, two); err != nil || extra.calls != i {
			t.Fatalf("call %d: %d probes, err %v, want a probe each time", i, extra.calls, err)
		}
	}
	if got := storedProfile(t, c); got != nil {
		t.Errorf("facts for three nodes were stored for a call about two: %+v", got)
	}
}

func TestNodeMismatchTable(t *testing.T) {
	facts := internalCleanFacts(internalNodes("pve1", "pve2", "pve3"))
	cases := map[string]struct {
		nodes []NodeRef
		want  string
	}{
		"same nodes":    {internalNodes("pve1", "pve2", "pve3"), ""},
		"a node left":   {internalNodes("pve1", "pve2"), "the cluster's node set changed"},
		"a node joined": {internalNodes("pve1", "pve2", "pve3", "pve4"), "the cluster's node set changed"},
		"a node renamed": {
			internalNodes("pve1", "pve2", "pve9"), "the cluster's node set changed",
		},
		"a node offline": {
			[]NodeRef{{Name: "pve1", Online: true}, {Name: "pve2", Online: true}, {Name: "pve3"}},
			"node pve3 changed its online state",
		},
	}
	for name, tc := range cases {
		if got := nodeMismatch(facts, tc.nodes); got != tc.want {
			t.Errorf("%s: nodeMismatch = %q, want %q", name, got, tc.want)
		}
	}
}

func TestUncleanReasonNamesTheFirstFault(t *testing.T) {
	if got := uncleanReason(internalCleanFacts(internalNodes("pve1", "pve2", "pve3"))); got != "" {
		t.Fatalf("clean facts gave %q", got)
	}
	for name, fault := range factFaults {
		f := internalCleanFacts(internalNodes("pve1", "pve2", "pve3"))
		fault(f)
		if got := uncleanReason(f); got == "" {
			t.Errorf("%s: facts with the fault were called clean", name)
		}
	}
	// An offline node is not asked, so it has no version or CPU to be unclean.
	f := internalCleanFacts([]NodeRef{{Name: "pve1", Online: true}, {Name: "pve2"}})
	if got := uncleanReason(f); got != "" {
		t.Errorf("an offline node made the facts unclean: %q", got)
	}
}

// goldenFacts is a fully populated reading, so the golden file below names
// every field of the stored schema.
func goldenFacts() SlowFacts {
	return SlowFacts{
		Nodes: map[string]NodeSlowFacts{"pve1": {
			Probed: true, Version: "9.2.1", VersionErr: "v", CPU: CPUInfo{
				Known: true,
				Model: "Xeon A",
				Flags: []string{"avx"},
			}, CPUErr: "c",
		}},
		RulesSupported: true, RulesErr: "r",
		ConfigVotes: map[string]int{"pve1": 1}, ConfigVotesErr: "cv",
		QDevice:    QDevice{Read: true, ReadErr: "qe", Configured: true, Votes: 1, Status: QDeviceUnknown},
		SysConsole: true, VMReplicate: true, PermissionsErr: "pe",
		Storages: []pve.StorageInfo{{
			Name: "nfs1", Type: "nfs", Shared: true, Disabled: true, Nodes: []string{"pve1"}, Path: "/p", Server: "s",
			Export: "/e", Content: "images", IsMountpoint: true,
		}},
		StoragesErr: "se",
		Cached:      true,
	}
}

// goldenProfileFile is the stored payload's exact text. A change to it is a
// change to the file's schema and bumps profileCacheVersion, together with this
// test.
const goldenProfileFile = `{"key":{"endpoints":null,"verify_ssl":false,"ca_sha256":"","credential_id":""},` +
	`"cluster":"lab",` +
	`"stored_at_ms":1,"facts":{"nodes":{"pve1":{"probed":true,"version":"9.2.1","version_err":"v",` +
	`"cpu":{"known":true,"model":"Xeon A","flags":["avx"]},"cpu_err":"c"}},"rules_supported":true,` +
	`"rules_err":"r","config_votes":{"pve1":1},"config_votes_err":"cv","qdevice":{"read":true,"read_err":"qe",` +
	`"configured":true,"votes":1,"status":"unknown"},"sys_console":true,"vm_replicate":true,` +
	`"permissions_err":"pe","storages":[{"name":"nfs1","type":"nfs","shared":true,"disabled":true,` +
	`"nodes":["pve1"],"path":"/p","server":"s","export":"/e","content":"images","is_mountpoint":true}],` +
	`"storages_err":"se"}}`

func TestTheStoredProfileHasAPinnedSchema(t *testing.T) {
	if profileCacheVersion != 2 {
		t.Fatalf(
			"profileCacheVersion = %d, but this golden text is the version 2 schema; update both together",
			profileCacheVersion,
		)
	}
	got, err := json.Marshal(profilePayload{Cluster: "lab", StoredAtMillis: 1, Facts: goldenFacts()})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenProfileFile {
		t.Errorf("stored profile\n%s\nwant\n%s", got, goldenProfileFile)
	}
	var back profilePayload
	dec := json.NewDecoder(strings.NewReader(goldenProfileFile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("the golden text does not decode strictly: %v", err)
	}
	want := goldenFacts()
	want.Cached = false
	if !reflect.DeepEqual(back.Facts, want) {
		t.Errorf("decoded %+v, want %+v", back.Facts, want)
	}
}

// TestEveryStoredFieldHasAnExplicitJSONTag walks the stored types, so a field
// added to one of them cannot reach the file under its Go name.
func TestEveryStoredFieldHasAnExplicitJSONTag(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(t *testing.T, typ reflect.Type, path string)
	walk = func(t *testing.T, typ reflect.Type, path string) {
		t.Helper()
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag, ok := field.Tag.Lookup("json")
			name, _, _ := strings.Cut(tag, ",")
			if !ok || name == "" {
				t.Errorf("%s.%s has no explicit JSON name", path, field.Name)
			}
			if name != "-" {
				walk(t, field.Type, path+"."+field.Name)
			}
		}
	}
	walk(t, reflect.TypeOf(profilePayload{}), "profilePayload")
	if !seen[reflect.TypeOf(
		pve.StorageInfo{},
	)] || !seen[reflect.TypeOf(CPUInfo{})] || !seen[reflect.TypeOf(QDevice{})] {
		t.Errorf("the walk missed a stored type, saw %d", len(seen))
	}
}

// seedProfileFor is seedProfile for the cluster called cluster.
func seedProfileFor(t *testing.T, c *ProfileCache, cluster string, stored time.Time, facts SlowFacts) {
	t.Helper()
	store := c.openStore(context.Background(), log.NewNopLogger())
	if store == nil {
		t.Fatal("the cache cannot open its store")
	}
	defer func() { _ = store.Close() }()
	written, err := endpointcache.Update(context.Background(), store, func(*profilePayload) (*profilePayload, error) {
		return &profilePayload{
			Key:            store.Key(),
			Cluster:        cluster,
			StoredAtMillis: stored.UnixMilli(),
			Facts:          facts,
		}, nil
	})
	if err != nil || !written {
		t.Fatalf("seed the profile file: written %v, err %v", written, err)
	}
}

func TestProfileCacheNeverServesAFileStoredForAnotherCluster(t *testing.T) {
	// Two clusters can share endpoints, a credential, and node names, such as a
	// cluster rebuilt under the same host names, so the file's key is the same
	// for both and only the cluster's name tells them apart.
	nodes := internalNodes("pve1", "pve2", "pve3")
	live := &stubLive{facts: internalCleanFacts}
	c := internalCache(t, live)

	if _, err := c.Slow(context.Background(), "lab", nodes); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Slow(context.Background(), "lab", nodes); err != nil || !got.Cached || live.calls != 1 {
		t.Fatalf("the same cluster must be answered from the file: cached %v, probes %d, err %v",
			got != nil && got.Cached, live.calls, err)
	}

	got, err := c.Slow(context.Background(), "prod", nodes)
	if err != nil || got.Cached || live.calls != 2 {
		t.Fatalf("another cluster at the same endpoints must probe: cached %v, probes %d, err %v",
			got != nil && got.Cached, live.calls, err)
	}
	if stored := storedProfile(t, c); stored == nil || stored.Cluster != "prod" {
		t.Errorf("the file must now belong to the cluster that probed, got %+v", stored)
	}
	if got, err := c.Slow(context.Background(), "lab", nodes); err != nil || got.Cached || live.calls != 3 {
		t.Errorf("the first cluster must not read the second one's file: cached %v, probes %d, err %v",
			got != nil && got.Cached, live.calls, err)
	}
}

func TestProfileCacheDoesNotReadOrWriteWhenTheClusterHasNoName(t *testing.T) {
	nodes := internalNodes("pve1", "pve2", "pve3")
	live := &stubLive{facts: internalCleanFacts}
	c := internalCache(t, live)

	for i := 1; i <= 2; i++ {
		if got, err := c.Slow(context.Background(), "", nodes); err != nil || got.Cached || live.calls != i {
			t.Fatalf("call %d: an unnamed cluster must probe every time: cached %v, probes %d, err %v",
				i, got != nil && got.Cached, live.calls, err)
		}
	}
	if stored := storedProfile(t, c); stored != nil {
		t.Errorf("facts of an unnamed cluster were stored: %+v", stored)
	}

	// A file that holds facts for a named cluster does not answer an unnamed one.
	seedProfileFor(t, c, "lab", cacheTestNow.Add(-time.Second), *internalCleanFacts(nodes))
	if got, err := c.Slow(context.Background(), "", nodes); err != nil || got.Cached {
		t.Errorf("a stored file answered a call with no cluster name: cached %v, err %v", got != nil && got.Cached, err)
	}
	if stored := storedProfile(t, c); stored == nil || stored.Cluster != "lab" {
		t.Errorf("the unnamed call must leave the named cluster's file alone, got %+v", stored)
	}
}

func TestProfileCacheAFileWithNoClusterNameIsAMiss(t *testing.T) {
	// A file from before the payload carried the name, or a damaged one, has an
	// empty name, which never equals a real cluster's.
	nodes := internalNodes("pve1", "pve2", "pve3")
	live := &stubLive{facts: internalCleanFacts}
	c := internalCache(t, live)
	seedProfileFor(t, c, "", cacheTestNow.Add(-time.Second), *internalCleanFacts(nodes))

	if got, err := c.Slow(context.Background(), "lab", nodes); err != nil || got.Cached || live.calls != 1 {
		t.Fatalf("a file with no cluster name answered: cached %v, probes %d, err %v",
			got != nil && got.Cached, live.calls, err)
	}
	if stored := storedProfile(t, c); stored == nil || stored.Cluster != "lab" {
		t.Errorf("the probe must replace the unnamed file, got %+v", stored)
	}
}

func TestProfileCacheReplacesANewerFileThatBelongsToAnotherCluster(t *testing.T) {
	// A file that another process wrote while this one probed is kept only when
	// it could answer this cluster's next call.
	nodes := internalNodes("pve1", "pve2", "pve3")
	now := cacheTestNow
	var c *ProfileCache
	live := &stubLive{}
	live.facts = func(n []NodeRef) *SlowFacts {
		now = now.Add(5 * time.Second)
		seedProfileFor(t, c, "prod", now, *internalCleanFacts(n))
		return internalCleanFacts(n)
	}
	c = internalCache(t, live)
	c.opts.Now = func() time.Time { return now }

	if _, err := c.Slow(context.Background(), "lab", nodes); err != nil {
		t.Fatal(err)
	}
	if stored := storedProfile(t, c); stored == nil || stored.Cluster != "lab" {
		t.Errorf("a newer file for another cluster must not be kept, got %+v", stored)
	}
}

func TestEveryProfileCacheLogLineCallsItTheHAProfileCache(t *testing.T) {
	nodes := internalNodes("pve1", "pve2", "pve3")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	run := func(c *ProfileCache) {
		t.Helper()
		c.opts.Log = logger
		for i := 0; i < 2; i++ {
			if _, err := c.Slow(context.Background(), cacheTestCluster, nodes); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A miss that stores, then a hit.
	run(internalCache(t, &stubLive{facts: internalCleanFacts}))
	// Facts that must not be stored.
	run(internalCache(t, &stubLive{facts: func(n []NodeRef) *SlowFacts {
		f := internalCleanFacts(n)
		f.RulesErr = "501"
		return f
	}}))
	// A cluster with no name.
	c := internalCache(t, &stubLive{facts: internalCleanFacts})
	c.opts.Log = logger
	if _, err := c.Slow(context.Background(), "", nodes); err != nil {
		t.Fatal(err)
	}
	// A directory the cache refuses.
	unsafe := internalCache(t, &stubLive{facts: internalCleanFacts})
	if err := os.Chmod(
		unsafe.opts.Dir.Override,
		0o755,
	); err != nil { //nolint:gosec // the test makes the mode unsafe on purpose
		t.Fatal(err)
	}
	run(unsafe)
	// A secret of the wrong length.
	short := internalCache(t, &stubLive{facts: internalCleanFacts})
	if err := os.WriteFile(filepath.Join(short.opts.Dir.Override, "secret"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(short)
	// A key the store cannot open.
	unopenable := internalCache(t, &stubLive{facts: internalCleanFacts})
	unopenable.opts.Key.User = ""
	run(unopenable)
	// Stored facts that have expired.
	expiring := internalCache(t, &stubLive{facts: internalCleanFacts})
	clock := cacheTestNow
	expiring.opts.Now = func() time.Time { return clock }
	run(expiring)
	clock = clock.Add(2 * ProfileCacheTTL)
	run(expiring)
	// A stored file that is damaged.
	damaged := internalCache(t, &stubLive{facts: internalCleanFacts})
	run(damaged)
	files, err := filepath.Glob(filepath.Join(damaged.opts.Dir.Override, "ha_profile-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want one profile file to damage, got %v, %v", files, err)
	}
	if err := os.WriteFile(files[0], []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(damaged)

	seen := 0
	for _, e := range logs.All() {
		if !strings.Contains(strings.ToLower(e.Message), "profile cache") {
			continue
		}
		seen++
		if !strings.HasPrefix(e.Message, "HA profile cache: ") {
			t.Errorf("log line %q must start with the display name \"HA profile cache\"", e.Message)
		}
	}
	if seen < 9 {
		t.Errorf("the scenarios logged only %d profile cache lines, so the check proves little", seen)
	}
}
