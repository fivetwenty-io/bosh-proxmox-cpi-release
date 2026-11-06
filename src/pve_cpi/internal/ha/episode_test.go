package ha_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// episodeClock is a settable clock for the store.
type episodeClock struct{ t time.Time }

func (c *episodeClock) now() time.Time { return c.t }

func newEpisodeStore(t *testing.T) (*ha.EpisodeStore, *episodeClock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	clk := &episodeClock{t: time.Unix(1_800_000_000, 0)}
	return &ha.EpisodeStore{Dir: dir, Now: clk.now}, clk, dir
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
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("cache directory was not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("cache directory mode = %04o, want 0700", info.Mode().Perm())
	}
	fileInfo, err := os.Lstat(filepath.Join(dir, ha.EpisodeFileName))
	if err != nil {
		t.Fatalf("record was not written: %v", err)
	}
	if fileInfo.Mode().Perm()&0o077 != 0 {
		t.Errorf("record mode = %04o, want no group or other access", fileInfo.Mode().Perm())
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
	if err := os.RemoveAll(dir); err != nil {
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
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("want a parse error, got %v", err)
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

func TestEpisodeStore_WrongVersionAndOversizeReadAsLost(t *testing.T) {
	t.Parallel()
	for name, body := range map[string][]byte{
		"version":  []byte(`{"version":99,"episodes":{"vm:101":{"uid":"uid-a","first_seen":1}}}`),
		"oversize": []byte(`{"version":1,"pad":"` + strings.Repeat("x", 1<<20) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, _, dir := newEpisodeStore(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ha.EpisodeFileName), body, 0o600); err != nil {
				t.Fatal(err)
			}
			ep, err := store.Observe("vm:101", "uid-a")
			if err == nil {
				t.Fatal("want an error for an unusable record")
			}
			if ep.Recorded {
				t.Fatal("an unusable record must not supply a first observation")
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
	if err == nil || !strings.Contains(err.Error(), "grants access") {
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
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("want a refusal of the symlink, got %v", err)
	}
	if ep.Recorded {
		t.Fatal("a symlinked record must not be trusted")
	}
}

func TestEpisodeStore_UnwritableDirectoryStillAnswers(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "missing-parent", "cache")
	store := &ha.EpisodeStore{Dir: parent}
	ep, err := store.Observe("vm:101", "uid-a")
	if err == nil {
		t.Fatal("want an error when the directory cannot be created")
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

func TestDefaultCacheDirNamesTheEffectiveUser(t *testing.T) {
	t.Parallel()
	got := ha.DefaultCacheDir()
	if !strings.HasPrefix(filepath.Base(got), "pve_cpi-endpoint-cache.") {
		t.Fatalf("DefaultCacheDir() = %q", got)
	}
	if (&ha.EpisodeStore{}).Now != nil {
		t.Fatal("zero store has no clock")
	}
}
