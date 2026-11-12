package ha_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// Both HA stores choose their directory through endpointcache.Resolve, so they
// meet the same journal exclusion as the endpoint cache. These tests lay out a
// journal at state/journal with a symlink, alias, to state, so a path can name
// the journal's tree in a spelling that a lexical comparison does not see.

const (
	// aliasStaleCacheTemp has the shape of the cache's own scratch files.
	aliasStaleCacheTemp = ".tmp-0123456789abcdef0123456789abcdef"
	// aliasStaleJournalTemp has the shape of a journal scratch file, which
	// starts with the same prefix but carries a UUID.
	aliasStaleJournalTemp = ".tmp-0f8fad5b-d9cb-469f-a165-70867728950e"
)

// aliasLayout is the directories of one case.
type aliasLayout struct {
	state   string
	journal string
	alias   string
	inner   string
	tmp     string
}

// newAliasLayout builds state/journal, state/journal/inner, state/journal/tmp,
// and the alias link, all in a directory that is itself free of symlinks, so a
// path means the same on macOS and Linux. The journal holds a record and two
// stale scratch files that a sweep of the wrong directory would remove.
func newAliasLayout(t *testing.T) aliasLayout {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := aliasLayout{
		state:   filepath.Join(root, "state"),
		journal: filepath.Join(root, "state", "journal"),
		alias:   filepath.Join(root, "alias"),
		inner:   filepath.Join(root, "state", "journal", "inner"),
		tmp:     filepath.Join(root, "state", "journal", "tmp"),
	}
	for _, dir := range []string{l.state, l.journal, l.inner, l.tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(l.state, l.alias); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"authority.json", aliasStaleCacheTemp, aliasStaleJournalTemp} {
		path := filepath.Join(l.journal, name)
		if err := os.WriteFile(path, []byte("journal"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// aliasEntry is what a snapshot records about a file.
type aliasEntry struct {
	size  int64
	mtime time.Time
	mode  os.FileMode
	dir   bool
}

// snapshot lists every entry below dir with its size, time, and mode, so a test
// sees a file that was added, rewritten, or removed.
func snapshot(t *testing.T, dir string) map[string]aliasEntry {
	t.Helper()
	out := map[string]aliasEntry{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = aliasEntry{size: info.Size(), mtime: info.ModTime(), mode: info.Mode(), dir: d.IsDir()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertJournalUntouched fails when the journal's tree differs from before.
func assertJournalUntouched(t *testing.T, l aliasLayout, before map[string]aliasEntry) {
	t.Helper()
	after := snapshot(t, l.journal)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the journal directory changed\nbefore: %v\nafter:  %v", before, after)
	}
	for _, name := range []string{"authority.json", aliasStaleCacheTemp, aliasStaleJournalTemp} {
		if _, err := os.Lstat(filepath.Join(l.journal, name)); err != nil {
			t.Errorf("the journal lost %s: %v", name, err)
		}
	}
}

// aliasCase names a pair of paths that overlap through a symlink or equal each
// other. override is pve.endpoint_cache_dir and journal is
// pve.storage_allocation_journal_dir, as an operator would write them.
type aliasCase struct {
	name     string
	override func(aliasLayout) string
	journal  func(aliasLayout) string
}

var aliasCases = []aliasCase{
	{
		name:     "the cache directory is the journal directory",
		override: func(l aliasLayout) string { return l.journal },
		journal:  func(l aliasLayout) string { return l.journal },
	},
	{
		name:     "the cache directory names the journal through a symlinked ancestor",
		override: func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
		journal:  func(l aliasLayout) string { return l.journal },
	},
	{
		name:     "the journal directory names itself through a symlinked ancestor",
		override: func(l aliasLayout) string { return l.journal },
		journal:  func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
	},
	{
		name:     "both name the journal through the same symlinked ancestor",
		override: func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
		journal:  func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
	},
	{
		name:     "the cache directory sits inside the journal through a symlinked ancestor",
		override: func(l aliasLayout) string { return filepath.Join(l.alias, "journal", "inner") },
		journal:  func(l aliasLayout) string { return l.journal },
	},
	{
		name:     "the cache directory sits inside a journal that names itself through a symlinked ancestor",
		override: func(l aliasLayout) string { return l.inner },
		journal:  func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
	},
	{
		name:     "the cache directory contains a journal that names itself through a symlinked ancestor",
		override: func(l aliasLayout) string { return l.state },
		journal:  func(l aliasLayout) string { return filepath.Join(l.alias, "journal") },
	},
	{
		name:     "the cache directory contains the journal",
		override: func(l aliasLayout) string { return l.state },
		journal:  func(l aliasLayout) string { return l.journal },
	},
}

func TestProfileCacheRefusesADirectoryThatOverlapsTheJournal(t *testing.T) {
	for _, tc := range aliasCases {
		t.Run(tc.name, func(t *testing.T) {
			l := newAliasLayout(t)
			before := snapshot(t, l.journal)
			cfg := cacheCfg(tc.override(l))
			cfg.StorageAllocationJournalDir = tc.journal(l)
			w := three()
			clock := newCacheClock()
			d := cachedDeps(t, w, cfg, clock)

			resolveWith(t, d)
			resolveWith(t, d)
			if slowProbes(w) != 2 {
				t.Errorf("%d slow probes for two calls, want both to probe because no cache is available",
					slowProbes(w))
			}
			assertJournalUntouched(t, l, before)
		})
	}
}

func TestEpisodeStoreRefusesADirectoryThatOverlapsTheJournal(t *testing.T) {
	for _, tc := range aliasCases {
		t.Run(tc.name, func(t *testing.T) {
			l := newAliasLayout(t)
			before := snapshot(t, l.journal)
			clk := &episodeClock{t: time.Unix(1_800_000_000, 0)}
			store := &ha.EpisodeStore{
				Dir: endpointcache.DirOptions{Override: tc.override(l), JournalDir: tc.journal(l)}, Now: clk.now,
			}

			ep, err := store.Observe("vm:101", "uid-a")
			if !errors.Is(err, endpointcache.ErrJournalOverlap) {
				t.Fatalf("want the journal overlap refusal, got %v", err)
			}
			if ep.Recorded || ep.Elapsed() != 0 {
				t.Errorf("recorded=%v elapsed=%s, want a clock that starts now", ep.Recorded, ep.Elapsed())
			}
			assertJournalUntouched(t, l, before)
		})
	}
}

// An override that overlaps the journal exists, so the episode store's fallback
// to the layers after the override, which is for a missing directory, does not
// apply, and a temporary layer that would be reachable must stay unused.
func TestEpisodeStoreDoesNotFallBackFromAnOverrideThatOverlapsTheJournal(t *testing.T) {
	l := newAliasLayout(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	store := &ha.EpisodeStore{
		Dir: endpointcache.DirOptions{Override: filepath.Join(l.alias, "journal"), JournalDir: l.journal},
	}
	if _, err := store.Observe("vm:101", "uid-a"); !errors.Is(err, endpointcache.ErrJournalOverlap) {
		t.Fatalf("want the journal overlap refusal, got %v", err)
	}
	if names, err := readNames(tmp); err != nil || len(names) != 0 {
		t.Errorf("the temporary directory holds %v (err %v), want the fallback unused", names, err)
	}
}

// keepTheSiblingUncreated lets a group write in the journal's parent, so the
// CPI declines to create the directory beside the journal, or an override
// there, and the temporary layer is reached.
func keepTheSiblingUncreated(t *testing.T, l aliasLayout) {
	t.Helper()
	if err := os.Chmod(l.state, 0o770); err != nil { //nolint:gosec // the test opens the mode on purpose
		t.Fatal(err)
	}
}

func readNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// With no override the temporary layer creates its directory, so the journal
// check has to run before the mkdir. A temporary directory that sits inside the
// journal through an ancestor symlink must end with nothing created there.
func TestProfileCacheCreatesNothingInTheJournalForATemporaryDirectoryInsideIt(t *testing.T) {
	l := newAliasLayout(t)
	keepTheSiblingUncreated(t, l)
	t.Setenv("TMPDIR", filepath.Join(l.alias, "journal", "tmp"))
	before := snapshot(t, l.journal)
	cfg := cacheCfg("")
	cfg.StorageAllocationJournalDir = l.journal
	w := three()
	d := cachedDeps(t, w, cfg, newCacheClock())

	resolveWith(t, d)
	resolveWith(t, d)
	if slowProbes(w) != 2 {
		t.Errorf("%d slow probes for two calls, want both to probe because no cache is available", slowProbes(w))
	}
	assertJournalUntouched(t, l, before)
}

func TestEpisodeStoreCreatesNothingInTheJournalForATemporaryDirectoryInsideIt(t *testing.T) {
	l := newAliasLayout(t)
	keepTheSiblingUncreated(t, l)
	t.Setenv("TMPDIR", filepath.Join(l.alias, "journal", "tmp"))
	before := snapshot(t, l.journal)

	for name, override := range map[string]string{
		"no override":        "",
		"a missing override": filepath.Join(l.state, "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: override, JournalDir: l.journal}}
			if _, err := store.Observe("vm:101", "uid-a"); !errors.Is(err, endpointcache.ErrJournalOverlap) {
				t.Fatalf("want the journal overlap refusal, got %v", err)
			}
			assertJournalUntouched(t, l, before)
		})
	}
}

// A directory that only sits beside the journal, whichever spelling reaches
// it, is not refused, so the exclusion does not turn the caches off for a
// layout that is allowed.
func TestHAStoresStillUseADirectoryBesideTheJournalReachedThroughASymlink(t *testing.T) {
	l := newAliasLayout(t)
	cache := filepath.Join(l.state, "cache")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, l.journal)
	journal := filepath.Join(l.alias, "journal")

	cfg := cacheCfg(filepath.Join(l.alias, "cache"))
	cfg.StorageAllocationJournalDir = journal
	w := three()
	d := cachedDeps(t, w, cfg, newCacheClock())
	resolveWith(t, d)
	resolveWith(t, d)
	if slowProbes(w) != 1 || len(profileFiles(t, cache)) != 1 {
		t.Errorf("%d slow probes and files %v, want one probe and one profile file in %s", slowProbes(w),
			profileFiles(t, cache), cache)
	}

	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: cache, JournalDir: journal}}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatalf("the episode record beside the journal was refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cache, ha.EpisodeFileName)); err != nil {
		t.Errorf("the episode record is missing: %v", err)
	}
	assertJournalUntouched(t, l, before)
}

// The profile cache's sweep removes the cache's own stale scratch files and
// nothing else that starts with the same prefix, such as a scratch file with a
// UUID, which the allocation journal writes.
func TestProfileCacheSweepLeavesAForeignScratchFileAlone(t *testing.T) {
	dir := cacheDir(t)
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{aliasStaleCacheTemp, aliasStaleJournalTemp} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	w := three()
	clock := newCacheClock()
	clock.t = time.Now()
	d := cachedDeps(t, w, cacheCfg(dir), clock)
	resolveWith(t, d)

	if len(profileFiles(t, dir)) != 1 {
		t.Fatalf("profile files %v, want the write that runs the sweep", profileFiles(t, dir))
	}
	if _, err := os.Lstat(filepath.Join(dir, aliasStaleCacheTemp)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cache's stale scratch file survived the sweep: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, aliasStaleJournalTemp)); err != nil {
		t.Errorf("the sweep removed a scratch file the cache did not make: %v", err)
	}
}

// The temporary layer names its directory after the effective user, so a
// directory the guard let through is still the one the layer chose.
func TestHAStoresTemporaryDirectoryBesideTheJournalIsUsed(t *testing.T) {
	l := newAliasLayout(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{JournalDir: filepath.Join(l.alias, "journal")}}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, "pve_cpi-endpoint-cache."+strconv.Itoa(os.Geteuid()), ha.EpisodeFileName)
	if _, err := os.Lstat(want); err != nil {
		t.Errorf("the record is not in the temporary layer: %v", err)
	}
}
