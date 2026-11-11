package pve

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// journalCacheConfig is a token config with no override whose journal directory
// is journal, so the cache directory comes from the layer beside the journal or
// the temporary directory.
func journalCacheConfig(mode config.DiscoveryMode, journal string) *config.CPIConfig {
	cfg := cacheTestConfig(mode, "")
	cfg.StorageAllocationJournalDir = journal
	return cfg
}

// journalBeside makes a journal directory and returns it with the path of the
// endpoint-cache directory beside it, which it does not create.
func journalBeside(t *testing.T) (journal, sibling string) {
	t.Helper()
	parent := t.TempDir()
	journal = filepath.Join(parent, "allocations")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	return journal, filepath.Join(parent, "endpoint-cache")
}

func temporaryCacheDir(parent string) string {
	return filepath.Join(parent, "pve_cpi-endpoint-cache."+strconv.Itoa(os.Geteuid()))
}

func TestOpenEndpointCacheUsesTheSiblingBesideTheJournal(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			journal, sibling := journalBeside(t)
			if err := os.Mkdir(sibling, 0o700); err != nil {
				t.Fatal(err)
			}
			got := openCacheFor(t, journalCacheConfig(mode, journal))
			if got.cache == nil || got.plan.cacheUnusable {
				t.Fatalf("cache %v, unusable %v, want an open cache beside the journal",
					got.cache, got.plan.cacheUnusable)
			}
			want := "pve endpoints: cache directory " + sibling + ", beside the allocation journal directory"
			if lines := linesAt(got.obs.All(), slog.LevelDebug, want); len(lines) != 1 {
				t.Errorf("the cache-directory line appears %d times at Debug, want once; lines: %v", len(lines),
					got.obs.All())
			}
			if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
				t.Errorf("logged %q at %v", e.Message, e.Level)
			}
		})
	}
}

func TestOpenEndpointCacheMissingSiblingFallsThroughToTheTemporaryDirectoryAtDebug(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			journal, sibling := journalBeside(t)
			got := openCacheFor(t, journalCacheConfig(mode, journal))
			if got.cache == nil {
				t.Fatalf("a missing sibling must fall through to a usable temporary directory; lines: %v",
					got.obs.All())
			}
			if _, err := os.Lstat(sibling); !os.IsNotExist(err) {
				t.Errorf("the sibling exists after the call: %v", err)
			}
			if _, err := os.Lstat(temporaryCacheDir(tmp)); err != nil {
				t.Errorf("the temporary directory was not used: %v", err)
			}
			if lines := linesAt(got.obs.All(), slog.LevelDebug, sibling); len(lines) != 1 ||
				!strings.Contains(lines[0], "pve endpoints: cache directory "+sibling+" (journal layer)") {
				t.Errorf("want one Debug line that names the missing sibling; got %v", lines)
			}
			if e, bad := anyLineAbove(got.obs.All(), slog.LevelDebug); bad {
				t.Errorf("logged %q at %v, want the missing sibling at Debug only", e.Message, e.Level)
			}
		})
	}
}

func TestOpenEndpointCacheRefusedSiblingGivesNoCacheAndNoFallThrough(t *testing.T) {
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level slog.Level
	}{
		{config.DiscoveryModeAuto, slog.LevelDebug},
		{config.DiscoveryModeOn, slog.LevelWarn},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			journal, sibling := journalBeside(t)
			if err := os.Mkdir(sibling, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(sibling, 0o770); err != nil {
				t.Fatal(err)
			}
			got := openCacheFor(t, journalCacheConfig(tc.mode, journal))
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v, unusable %v, want no cache for a sibling with group bits", got.cache,
					got.plan.cacheUnusable)
			}
			if _, err := os.Lstat(temporaryCacheDir(tmp)); !os.IsNotExist(err) {
				t.Errorf("a refused sibling fell through to the temporary directory: %v", err)
			}
			if lines := linesAt(got.obs.All(), tc.level, "api endpoint discovery off: "); len(lines) != 1 ||
				!strings.Contains(lines[0], sibling) {
				t.Errorf("want one %v line that names the sibling; got %v", tc.level, got.obs.All())
			}
		})
	}
}

// A journal whose parent cannot be searched hides the sibling, so the layer
// cannot tell a missing directory from one it may not see. It gives no cache,
// and it creates nothing in the temporary directory.
func TestOpenEndpointCacheUnsearchableJournalParentGivesNoCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can search a directory of mode 000, so the sibling can be examined")
	}
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn} {
		level := slog.LevelDebug
		if mode == config.DiscoveryModeOn {
			level = slog.LevelWarn
		}
		t.Run(string(mode), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(root, "tmp")
			locked := filepath.Join(root, "locked")
			for _, dir := range []string{tmp, filepath.Join(locked, "journal")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			t.Setenv("TMPDIR", tmp)

			got := openCacheFor(t, journalCacheConfig(mode, filepath.Join(locked, "journal")))
			if got.cache != nil || !got.plan.cacheUnusable {
				t.Fatalf("cache %v, unusable %v, want no cache", got.cache, got.plan.cacheUnusable)
			}
			if lines := linesAt(got.obs.All(), level, "permission denied"); len(lines) != 1 {
				t.Errorf("lines at %v = %v; all: %v", level, lines, got.obs.All())
			}
			if names, err := readDirNames(tmp); err != nil || len(names) != 0 {
				t.Errorf("the temporary directory holds %v (err %v), want nothing created", names, err)
			}
		})
	}
}
