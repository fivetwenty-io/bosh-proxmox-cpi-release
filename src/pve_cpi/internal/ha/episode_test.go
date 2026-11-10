package ha_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// episodeClock is a settable clock for the store.
type episodeClock struct{ t time.Time }

func (c *episodeClock) now() time.Time { return c.t }

func newEpisodeStore(t *testing.T) (*ha.EpisodeStore, *episodeClock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	clk := &episodeClock{t: time.Unix(1_800_000_000, 0)}
	return &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: dir}, Now: clk.now}, clk, dir
}

func TestEpisodeStore_FirstObservationStartsTheClockAndIsRemembered(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)

	ep, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatalf("first observation: %v", err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("first observation must start the clock, got recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
	fileInfo, err := os.Lstat(filepath.Join(dir, ha.EpisodeFileName))
	if err != nil {
		t.Fatalf("record was not written: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %04o, want 0600", fileInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("the directory holds %v, %v, want only the record and no temporary file", entries, err)
	}

	clk.t = clk.t.Add(7 * time.Minute)
	again, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatalf("second observation: %v", err)
	}
	if !again.Recorded || again.Elapsed() != 7*time.Minute {
		t.Fatalf("second observation: recorded=%v elapsed=%s, want recorded and 7m", again.Recorded, again.Elapsed())
	}
}

func TestEpisodeStore_NewUIDRestartsTheClock(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(9 * time.Minute)
	ep, err := store.Observe("vm:101", "uid-b")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("a new uid is a new episode: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
	clk.t = clk.t.Add(time.Minute)
	ep, err = store.Observe("vm:101", "uid-b")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Elapsed() != time.Minute {
		t.Fatalf("the new episode's clock runs from its own first observation, got %s", ep.Elapsed())
	}
}

func TestEpisodeStore_KeepsServicesApart(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	if _, err := store.Observe("vm:102", "uid-z"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Minute)
	ep, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Elapsed() != 2*time.Minute {
		t.Fatalf("vm:101 elapsed = %s, want 2m", ep.Elapsed())
	}
}

func TestEpisodeStore_LostRecordRestartsTheClock(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ha.EpisodeFileName)); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(30 * time.Minute)
	ep, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatalf("a missing record is not an error: %v", err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("a lost record must restart the clock: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
}

func TestEpisodeStore_CorruptRecordRestartsTheClockAndIsRewritten(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ha.EpisodeFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	ep, err := store.Observe("vm:101", "uid-a")
	if !errors.Is(err, privatefile.ErrCorrupt) {
		t.Fatalf("want a corrupt-record error, got %v", err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("an unreadable record must restart the clock: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
	clk.t = clk.t.Add(time.Minute)
	ep, err = store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatalf("the rewritten record must read back: %v", err)
	}
	if !ep.Recorded || ep.Elapsed() != time.Minute {
		t.Fatalf("rewritten record: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
}

func TestEpisodeStore_UnusableRecordsReadAsLost(t *testing.T) {
	t.Parallel()
	payload := `{"episodes":{"vm:101":{"uid":"uid-a","first_seen":1}}}`
	for name, body := range map[string][]byte{
		"version":  []byte(`{"version":99,"sha256":"` + privatefile.SHA256Hex([]byte(payload)) + `","payload":` + payload + `}`),
		"checksum": []byte(`{"version":1,"sha256":"` + privatefile.SHA256Hex([]byte("other")) + `","payload":` + payload + `}`),
		"unknown field": []byte(`{"version":1,"sha256":"` + privatefile.SHA256Hex([]byte(`{"episodes":{},"x":1}`)) +
			`","payload":{"episodes":{},"x":1}}`),
		"oversize":                 []byte(`{"version":1,"pad":"` + strings.Repeat("x", 1<<20) + `"}`),
		"the earlier plain format": []byte(`{"version":1,"episodes":{"vm:101":{"uid":"uid-a","first_seen":1}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, _, dir := newEpisodeStore(t)
			if err := os.WriteFile(filepath.Join(dir, ha.EpisodeFileName), body, 0o600); err != nil {
				t.Fatal(err)
			}
			ep, err := store.Observe("vm:101", "uid-a")
			if !errors.Is(err, privatefile.ErrCorrupt) {
				t.Fatalf("want a corrupt-record error, got %v", err)
			}
			if ep.Recorded {
				t.Fatal("an unusable record must not supply a first observation")
			}
			again, err := store.Observe("vm:101", "uid-a")
			if err != nil || !again.Recorded {
				t.Fatalf("the record must be rewritten and read back: recorded=%v err=%v", again.Recorded, err)
			}
		})
	}
}

func TestEpisodeStore_UnsafeDirectoryIsNeverTrusted(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the test widens the mode on purpose
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	ep, err := store.Observe("vm:101", "uid-a")
	if !errors.Is(err, privatefile.ErrUnsafeMode) {
		t.Fatalf("want a refusal of the open directory, got %v", err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatal("a record in an unsafe directory must not be trusted")
	}
}

func TestEpisodeStore_SymlinkedRecordIsNeverTrusted(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ha.EpisodeFileName)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	data, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	ep, err := store.Observe("vm:101", "uid-a")
	if !errors.Is(err, privatefile.ErrUnsafeFileType) && !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("want a refusal of the symlink, got %v", err)
	}
	if ep.Recorded {
		t.Fatal("a symlinked record must not be trusted")
	}
}

// temporaryCacheDir points the temporary layer at a fresh directory and
// returns the directory the layer will make inside it.
func temporaryCacheDir(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "ha-episode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	return filepath.Join(tmp, "pve_cpi-endpoint-cache."+strconv.Itoa(os.Geteuid()))
}

func TestEpisodeStore_MissingOverrideDirectoryFallsBackToTheTemporaryLayer(t *testing.T) {
	temporary := temporaryCacheDir(t)
	missing := filepath.Join(t.TempDir(), "missing-parent", "cache")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	clk := &episodeClock{t: time.Unix(1_800_000_000, 0)}
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing}, Log: logger, Now: clk.now}

	first, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatalf("a missing override must not stop the record: %v", err)
	}
	if first.Recorded || first.Elapsed() != 0 {
		t.Fatalf("first observation: recorded=%v elapsed=%s", first.Recorded, first.Elapsed())
	}
	clk.t = clk.t.Add(7 * time.Minute)
	second, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Recorded || second.Elapsed() != 7*time.Minute {
		t.Fatalf("the clock must run across calls: recorded=%v elapsed=%s, want recorded and 7m", second.Recorded, second.Elapsed())
	}

	if _, err := os.Lstat(filepath.Join(temporary, ha.EpisodeFileName)); err != nil {
		t.Errorf("the record must sit in the temporary layer: %v", err)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the override directory must never be created: %v", statErr)
	}
	warns := 0
	for _, e := range logs.All() {
		if e.Level != slog.LevelWarn {
			continue
		}
		warns++
		if !strings.Contains(e.Message, "pve.endpoint_cache_dir does not exist") || e.Attrs["path"] != missing {
			t.Errorf("warning %+v does not name the missing override directory %s", e, missing)
		}
	}
	if warns != 2 {
		t.Errorf("logged %d warnings for two calls, want one per call", warns)
	}
}

func TestEpisodeStore_AnOverrideThatFailsThePrivateChecksDoesNotFallBack(t *testing.T) {
	temporary := temporaryCacheDir(t)
	store, clk, dir := newEpisodeStore(t)
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	store.Log = logger
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the test widens the mode on purpose
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	ep, err := store.Observe("vm:101", "uid-a")
	if !errors.Is(err, privatefile.ErrUnsafeMode) {
		t.Fatalf("want the mode refusal of the override, got %v", err)
	}
	if ep.Recorded {
		t.Fatal("an unsafe override must not supply a record")
	}
	if _, statErr := os.Lstat(temporary); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the temporary layer was used after the override was refused: %v", statErr)
	}
	for _, e := range logs.All() {
		if e.Level == slog.LevelWarn {
			t.Errorf("an existing override logged the missing-directory warning: %+v", e)
		}
	}
}

func TestEpisodeStore_AMissingOverrideWithNoUsableFallbackStillAnswers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	// The temporary directory is relative, so the fallback is refused too.
	t.Setenv("TMPDIR", "relative")
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing}}
	ep, err := store.Observe("vm:101", "uid-a")
	if err == nil {
		t.Fatal("want an error when neither the override nor the fallback can hold the record")
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatal("the clock starts now when nothing can be recorded")
	}
}

func TestEpisodeStore_FutureFirstObservationRestartsTheClock(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(-time.Hour)
	ep, err := store.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("a first observation ahead of the clock must not count: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
}

func TestEpisodeStore_PrunesOldEpisodes(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(8 * 24 * time.Hour)
	if _, err := store.Observe("vm:102", "uid-b"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ha.EpisodeFileName)) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "vm:101") {
		t.Fatalf("an episode past retention must be pruned: %s", data)
	}
}

func TestEpisodeStore_RejectsEmptyIdentity(t *testing.T) {
	t.Parallel()
	store, _, _ := newEpisodeStore(t)
	for _, pair := range [][2]string{{"", "uid"}, {"vm:1", " "}} {
		if _, err := store.Observe(pair[0], pair[1]); err == nil {
			t.Errorf("Observe(%q, %q): want an error", pair[0], pair[1])
		}
	}
}

func TestEpisodeStore_WithoutAnOverrideUsesTheTemporaryLayer(t *testing.T) {
	tmp, err := os.MkdirTemp("", "ha-episode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	if _, err := (&ha.EpisodeStore{}).Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "pve_cpi-endpoint-cache.*", ha.EpisodeFileName))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the record must sit in $TMPDIR/pve_cpi-endpoint-cache.<euid>: %v, %v", matches, err)
	}
}

func TestEpisodeStore_SitsBesideTheProfileCache(t *testing.T) {
	t.Parallel()
	dir := cacheDir(t)
	w := three()
	d := cachedDeps(t, w, cacheCfg(dir), newCacheClock())
	resolveWith(t, d)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: dir}}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ha.EpisodeFileName)); err != nil {
		t.Errorf("the record must sit in the profile cache's directory: %v", err)
	}
	if len(profileFiles(t, dir)) != 1 {
		t.Errorf("the profile cache file must still be there: %v", profileFiles(t, dir))
	}
}
