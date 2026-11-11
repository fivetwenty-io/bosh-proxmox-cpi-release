package pve

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
)

// privateTempRoot makes a fresh directory to serve as the process's temporary
// directory, with no symlink in its path, and points TMPDIR at it.
func privateTempRoot(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(resolved, "tmp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	return dir
}

// temporaryCacheName is the name of the temporary layer's directory.
func temporaryCacheName() string { return "pve_cpi-endpoint-cache." + strconv.Itoa(os.Geteuid()) }

// A call that starts after the exit drain began touches nothing on any layer.
// With no override the temporary layer would create its directory, and it must
// not, so the check comes before the directory is chosen and not after it is
// opened.
func TestACallAfterTheDrainBeganCreatesNoTemporaryDirectory(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			tmp := privateTempRoot(t)
			cfg := cacheTestConfig(mode, "")
			endpointcache.Drain(context.Background(), time.Second)
			t.Cleanup(endpointcache.Open)

			got := openCacheFor(t, cfg)
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
					got.plan.cacheUnusable)
			}
			if lines := linesAt(got.obs.All(), slog.LevelDebug, drainStartedText); len(lines) != 1 {
				t.Errorf("drain lines = %v, want one Debug line; all: %v", lines, got.obs.All())
			}
			if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
				t.Errorf("line %q is at %v, want nothing above Debug", e.Message, e.Level)
			}
			if names, err := readDirNames(tmp); err != nil || len(names) != 0 {
				t.Errorf("the temporary directory holds %v (err %v), want it untouched", names, err)
			}
		})
	}
}

// The temporary layer asks whether its directory overlaps the allocation
// journal before it creates the directory, so a refused call leaves nothing
// behind, whether the temporary directory lies inside the journal, is the
// journal, holds the journal, or reaches the journal through a symlink.
func TestATemporaryDirectoryThatOverlapsTheJournalIsRefusedBeforeItIsCreated(t *testing.T) {
	type layout struct {
		name string
		// build makes the directories the case needs and returns the temporary
		// directory and the journal directory.
		build func(t *testing.T, root string) (tmp, journal string)
		// left is the directory that must hold nothing after the refusal.
		left func(tmp, journal string) string
	}
	mkdir := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	layouts := []layout{
		{"the temporary directory is inside the journal", func(t *testing.T, root string) (string, string) {
			journal := mkdir(t, filepath.Join(root, "journal"))
			return mkdir(t, filepath.Join(journal, "tmp")), journal
		}, func(tmp, _ string) string { return tmp }},
		{"the temporary directory is a symlink into the journal", func(t *testing.T, root string) (string, string) {
			journal := mkdir(t, filepath.Join(root, "journal"))
			mkdir(t, filepath.Join(journal, "tmp"))
			link := filepath.Join(root, "link")
			if err := os.Symlink(journal, link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(link, "tmp"), journal
		}, func(_, journal string) string { return filepath.Join(journal, "tmp") }},
		{"the journal is the layer's own directory and is missing", func(t *testing.T, root string) (string, string) {
			tmp := mkdir(t, filepath.Join(root, "tmp"))
			return tmp, filepath.Join(tmp, temporaryCacheName())
		}, func(tmp, _ string) string { return tmp }},
		{"the journal would be inside the layer's directory", func(t *testing.T, root string) (string, string) {
			tmp := mkdir(t, filepath.Join(root, "tmp"))
			return tmp, filepath.Join(tmp, temporaryCacheName(), "journal")
		}, func(tmp, _ string) string { return tmp }},
	}
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		level := endpointcache.Level(mode)
		for _, l := range layouts {
			t.Run(string(mode)+"/"+l.name, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				tmp, journal := l.build(t, root)
				t.Setenv("TMPDIR", tmp)
				cfg := cacheTestConfig(mode, "")
				cfg.StorageAllocationJournalDir = journal

				got := openCacheFor(t, cfg)
				if got.cache != nil || !got.plan.cacheUnusable {
					t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
						got.plan.cacheUnusable)
				}
				if lines := linesAt(got.obs.All(), level, "allocation journal directory"); len(lines) != 1 {
					t.Errorf("lines at %v = %v; all: %v", level, lines, got.obs.All())
				}
				if names, err := readDirNames(l.left(tmp, journal)); err != nil || len(names) != 0 {
					t.Errorf("%s holds %v (err %v), want the refused layer to have created nothing",
						l.left(tmp, journal), names, err)
				}
			})
		}
	}
}

// A journal that cannot be examined cannot be compared with the temporary
// directory, so the guard refuses the layer before it creates the directory.
// The check has to fail closed here, because the check after the open would
// refuse the directory only after the layer had left it behind. A journal that
// is a symlink loop cannot be resolved, while the directory beside it can still
// be searched, so the layer beside the journal declines and the temporary layer
// meets the guard.
func TestATemporaryDirectoryIsRefusedBeforeItIsCreatedWhenTheJournalCannotBeCompared(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		level := endpointcache.Level(mode)
		t.Run(string(mode), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(root, "tmp")
			if err := os.Mkdir(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			loop := filepath.Join(root, "journal")
			if err := os.Symlink(loop, loop); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", tmp)
			cfg := cacheTestConfig(mode, "")
			cfg.StorageAllocationJournalDir = loop

			got := openCacheFor(t, cfg)
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v and unusable %v, want no cache and an unusable directory", got.cache,
					got.plan.cacheUnusable)
			}
			if lines := linesAt(got.obs.All(), level, "could not be compared"); len(lines) != 1 {
				t.Errorf("lines at %v = %v; all: %v", level, lines, got.obs.All())
			}
			if names, err := readDirNames(tmp); err != nil || len(names) != 0 {
				t.Errorf("the temporary directory holds %v (err %v), want the refused layer to have created nothing",
					names, err)
			}
		})
	}
}

// A journal that does not overlap the temporary directory leaves the layer to
// create it, so the guard refuses nothing it should allow.
func TestATemporaryDirectoryBesideTheJournalIsCreatedAndUsed(t *testing.T) {
	tmp := privateTempRoot(t)
	journal := filepath.Join(filepath.Dir(tmp), "journal")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := cacheTestConfig(config.DiscoveryModeOn, "")
	cfg.StorageAllocationJournalDir = journal

	got := openCacheFor(t, cfg)
	if got.cache == nil || got.plan.cacheUnusable {
		t.Fatalf("cache %v and unusable %v, want a cache; lines: %v", got.cache, got.plan.cacheUnusable,
			got.obs.All())
	}
	if _, err := os.Stat(filepath.Join(tmp, temporaryCacheName())); err != nil {
		t.Errorf("the layer's directory is missing: %v", err)
	}
}

// The started-away line is said once for each pair of first and start
// endpoints, and a pair the process has not said yet is said even when its
// first endpoint was said before.
func TestFirstStartedAwayKeysOnTheFirstAndTheStartEndpoint(t *testing.T) {
	unique := t.Name()
	first, startA, startB := unique+"-first:8006", unique+"-a:8006", unique+"-b:8006"
	if !firstStartedAway(first, startA) {
		t.Fatal("the first start away was reported as already said")
	}
	if firstStartedAway(first, startA) {
		t.Error("the same start away was said twice")
	}
	if !firstStartedAway(first, startB) {
		t.Error("a start on another endpoint from the same first endpoint was taken for the one already said")
	}
	if !firstStartedAway(unique+"-other:8006", startA) {
		t.Error("a start from another first endpoint was taken for the one already said")
	}
}
