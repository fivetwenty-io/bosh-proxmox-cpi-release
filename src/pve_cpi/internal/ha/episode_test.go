package ha_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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
	names, err := readNames(dir)
	if err != nil || len(names) != 2 || names[0] != "ha-recovery-episodes.json" ||
		names[1] != "ha-recovery-episodes.lock" {
		t.Errorf("the directory holds %v, %v, want only the record and its lock, and no temporary file", names, err)
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
		"version": []byte(
			`{"version":99,"sha256":"` + privatefile.SHA256Hex([]byte(payload)) + `","payload":` + payload + `}`,
		),
		"checksum": []byte(
			`{"version":1,"sha256":"` + privatefile.SHA256Hex([]byte("other")) + `","payload":` + payload + `}`,
		),
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
		t.Fatalf(
			"the clock must run across calls: recorded=%v elapsed=%s, want recorded and 7m",
			second.Recorded,
			second.Elapsed(),
		)
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
		t.Fatalf("a first observation ahead of the clock must not count: recorded=%v elapsed=%s",
			ep.Recorded, ep.Elapsed())
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

// warnLines returns the Warn entries the observed logger holds.
func warnLines(logs *log.Observer) []log.Entry {
	var out []log.Entry
	for _, e := range logs.All() {
		if e.Level == slog.LevelWarn {
			out = append(out, e)
		}
	}
	return out
}

func TestEpisodeStore_DirectoryRefusalsNameTheEpisodeRecord(t *testing.T) {
	t.Parallel()
	notADirectory := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(wide, 0o755); err != nil { //nolint:gosec // the test makes the mode unsafe on purpose
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil { //nolint:gosec // the umask may have narrowed it
		t.Fatal(err)
	}
	for name, override := range map[string]string{
		"a file":          notADirectory,
		"a wide mode":     wide,
		"a relative path": "relative/dir",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: override}}
			_, err := store.Observe("vm:101", "uid-a")
			if err == nil {
				t.Fatal("want the directory refused")
			}
			if !strings.Contains(err.Error(), ha.EpisodeName+":") {
				t.Errorf("the refusal %q must call the record %q", err, ha.EpisodeName)
			}
			if strings.Contains(err.Error(), "endpoint cache") {
				t.Errorf("the refusal %q names the endpoint cache, which the record is not", err)
			}
		})
	}
}

func TestEpisodeNameIsAValidDisplayName(t *testing.T) {
	t.Parallel()
	// A name the directory choice reads as invalid falls back to the endpoint
	// cache's, so the refusal of an unusable directory proves the name took.
	choice := endpointcache.DirOptions{Override: "relative/dir"}.Named(ha.EpisodeName).Resolve()
	if choice.Usable() || !strings.Contains(choice.Err.Error(), ha.EpisodeName) {
		t.Fatalf("the directory choice did not take the name %q: %v", ha.EpisodeName, choice.Err)
	}
}

func TestEpisodeStore_TheWarnSaysWhenTheFallbackFailsToo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	// The temporary directory is relative, so the fallback is refused too.
	t.Setenv("TMPDIR", "relative")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing}, Log: logger}
	if _, err := store.Observe("vm:101", "uid-a"); err == nil {
		t.Fatal("want an error when neither the override nor the fallback can hold the record")
	}
	warns := warnLines(logs)
	if len(warns) != 1 {
		t.Fatalf("logged %d warnings, want one", len(warns))
	}
	msg := warns[0].Message
	if strings.Contains(msg, "instead") || !strings.Contains(msg, "cannot be used either") {
		t.Errorf("the warning promises a fallback that failed: %q", msg)
	}
	if got, _ := warns[0].Attrs["fallback_error"].(string); got == "" {
		t.Errorf("the warning must carry the fallback's refusal, got %+v", warns[0].Attrs)
	}
	if warns[0].Attrs["path"] != missing {
		t.Errorf("the warning must name the missing directory %s, got %+v", missing, warns[0].Attrs)
	}
}

func TestEpisodeStore_TheWarnNamesTheFallbackThatWorked(t *testing.T) {
	temporary := temporaryCacheDir(t)
	missing := filepath.Join(t.TempDir(), "missing")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing}, Log: logger}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	warns := warnLines(logs)
	if len(warns) != 1 || !strings.Contains(warns[0].Message, "uses the temporary cache directory instead") {
		t.Fatalf("want one warning that names the fallback, got %+v", warns)
	}
	if warns[0].Attrs["fallback"] != temporary {
		t.Errorf("the warning must name %s, got %+v", temporary, warns[0].Attrs)
	}
}

func TestEpisodeStore_ForCallWarnsOnceHoweverManyTimesItObserves(t *testing.T) {
	temporaryCacheDir(t)
	missing := filepath.Join(t.TempDir(), "missing")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	base := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing}, Log: logger}

	call := base.ForCall()
	for _, sid := range []string{"vm:101", "vm:101", "vm:102"} {
		if _, err := call.Observe(sid, "uid-a"); err != nil {
			t.Fatal(err)
		}
	}
	if warns := warnLines(logs); len(warns) != 1 {
		t.Errorf("a call's store logged %d warnings for three observations, want one", len(warns))
	}

	// The copy leaves the store it came from alone, and the next call warns again.
	if _, err := base.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := base.ForCall().Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if warns := warnLines(logs); len(warns) != 3 {
		t.Errorf("logged %d warnings, want one per call and one per observation of an ordinary store", len(warns))
	}

	if _, err := (*ha.EpisodeStore)(nil).ForCall().Observe("vm:101", "uid-a"); err != nil {
		t.Errorf("a call's store made from a nil store must work: %v", err)
	}
}

func TestEpisodeStore_TheWarnNamesTheSiblingBesideTheJournalWhenThatIsTheFallback(t *testing.T) {
	temporary := temporaryCacheDir(t)
	journal, sibling := journalBesideCache(t)
	missing := filepath.Join(t.TempDir(), "missing")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing, JournalDir: journal}, Log: logger}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	warns := warnLines(logs)
	if len(warns) != 1 {
		t.Fatalf("logged %d warnings, want one", len(warns))
	}
	msg := warns[0].Message
	if strings.Contains(msg, "temporary") || !strings.Contains(msg, "uses the cache directory beside the allocation "+
		"journal directory instead") {
		t.Errorf("the warning must name the directory beside the journal and not the temporary one: %q", msg)
	}
	if warns[0].Attrs["fallback"] != sibling {
		t.Errorf("the warning must name %s, got %+v", sibling, warns[0].Attrs)
	}
	if _, err := os.Lstat(filepath.Join(sibling, ha.EpisodeFileName)); err != nil {
		t.Errorf("the record must sit beside the journal: %v", err)
	}
	if _, err := os.Lstat(temporary); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary layer was used although the sibling was usable: %v", err)
	}
}

func TestEpisodeStore_TheWarnDoesNotBlameTheTemporaryDirectoryWhenTheSiblingIsRefused(t *testing.T) {
	temporaryCacheDir(t)
	journal, sibling := journalBesideCache(t)
	if err := os.Chmod(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	store := &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: missing, JournalDir: journal}, Log: logger}
	if _, err := store.Observe("vm:101", "uid-a"); err == nil {
		t.Fatal("want an error when the directory beside the journal is refused")
	}
	warns := warnLines(logs)
	if len(warns) != 1 {
		t.Fatalf("logged %d warnings, want one", len(warns))
	}
	msg := warns[0].Message
	if strings.Contains(msg, "temporary") || !strings.Contains(msg, "the cache directory beside the allocation "+
		"journal directory cannot be used either") {
		t.Errorf("the warning must blame the directory beside the journal and not the temporary one: %q", msg)
	}
	if warns[0].Attrs["fallback"] != sibling {
		t.Errorf("the warning must name %s, got %+v", sibling, warns[0].Attrs)
	}
}

func TestEpisodeStore_ClustersWithTheSameServiceIDKeepTheirOwnClocks(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	first := *store
	first.Scope = "cluster-a"
	second := *store
	second.Scope = "cluster-b"

	if _, err := first.Observe("vm:105", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(2 * time.Minute)
	if ep, err := second.Observe("vm:105", "uid-b"); err != nil || ep.Recorded {
		t.Fatalf("the other cluster's first observation: recorded=%v err=%v", ep.Recorded, err)
	}
	clk.t = clk.t.Add(3 * time.Minute)
	ep, err := first.Observe("vm:105", "uid-a")
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Recorded || ep.Elapsed() != 5*time.Minute {
		t.Fatalf("the first cluster's clock must run on undisturbed: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
	ep, err = second.Observe("vm:105", "uid-b")
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Recorded || ep.Elapsed() != 3*time.Minute {
		t.Fatalf("the second cluster's clock: recorded=%v elapsed=%s, want 3m", ep.Recorded, ep.Elapsed())
	}
}

// TestEpisodeStore_ARecordFromBeforeScopingIsIgnoredAndSafe covers an upgrade.
// An earlier release keyed the entry by the bare service ID, and the entry may
// belong to any cluster, so a scoped store does not read it. The episode then
// starts its clock once more, which only delays a handover, and the unscoped
// entry stays in the file until it ages out.
func TestEpisodeStore_ARecordFromBeforeScopingIsIgnoredAndSafe(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	legacy := *store
	if _, err := legacy.Observe("vm:105", "uid-a"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(20 * time.Minute)

	scoped := *store
	scoped.Scope = "cluster-a"
	ep, err := scoped.Observe("vm:105", "uid-a")
	if err != nil {
		t.Fatalf("a scoped store must read a file with unscoped entries: %v", err)
	}
	if ep.Recorded || ep.Elapsed() != 0 {
		t.Fatalf("the unscoped entry must not answer for a cluster: recorded=%v elapsed=%s", ep.Recorded, ep.Elapsed())
	}
	clk.t = clk.t.Add(time.Minute)
	if ep, err = scoped.Observe("vm:105", "uid-a"); err != nil || !ep.Recorded || ep.Elapsed() != time.Minute {
		t.Fatalf("the scoped entry must persist: %+v, %v", ep, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ha.EpisodeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"vm:105@cluster-a"`) || !strings.Contains(string(raw), `"vm:105"`) {
		t.Errorf("the record should hold the scoped entry and the aging unscoped one: %s", raw)
	}
}

func TestEpisodeStore_AFutureEntryIsPrunedWhenAnotherServiceIsObserved(t *testing.T) {
	t.Parallel()
	store, clk, _ := newEpisodeStore(t)
	future := *store
	future.Now = func() time.Time { return clk.t.Add(48 * time.Hour) }
	if _, err := future.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Observe("vm:102", "uid-z"); err != nil {
		t.Fatal(err)
	}
	ep, err := future.Observe("vm:101", "uid-a")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Recorded {
		t.Fatal("an entry first seen in the future must be pruned by the next write")
	}
}

// Two observations that change the record at once must both land. The second
// starts while the first is about to write, and it either waits for the lock
// or, without one, writes first and has its entry dropped by the first's
// write.
func TestEpisodeStore_ConcurrentObservationsKeepEachOthersEpisodes(t *testing.T) {
	t.Parallel()
	store, _, _ := newEpisodeStore(t)
	other := *store
	done := make(chan error, 1)
	var once sync.Once
	ha.SetEpisodeBeforeWrite(store, func() {
		once.Do(func() {
			go func() {
				_, err := other.Observe("vm:102", "uid-b")
				done <- err
			}()
			select {
			case err := <-done:
				done <- err
			case <-time.After(200 * time.Millisecond):
			}
		})
	})
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatalf("first observation: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("second observation: %v", err)
	}

	for _, ep := range []struct{ sid, uid string }{{"vm:101", "uid-a"}, {"vm:102", "uid-b"}} {
		got, err := other.Observe(ep.sid, ep.uid)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Recorded {
			t.Errorf("%s: the episode was dropped by the observation that wrote at the same time", ep.sid)
		}
	}
}

// A lock that another process holds past the wait leaves a new episode
// unrecorded with its clock starting now, and the error says the lock is busy.
// An episode that is already recorded needs no change, so it still answers.
func TestEpisodeStore_ABusyLockStartsTheClockNowAndRecordedEpisodesStillAnswer(t *testing.T) {
	t.Parallel()
	store, clk, dir := newEpisodeStore(t)
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "ha-recovery-episodes.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockFile.Close() })
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	clk.t = clk.t.Add(time.Minute)
	recorded, err := store.Observe("vm:101", "uid-a")
	if err != nil || !recorded.Recorded || recorded.Elapsed() != time.Minute {
		t.Fatalf("a recorded episode: recorded=%v elapsed=%s err=%v, want recorded, 1m, and no error",
			recorded.Recorded, recorded.Elapsed(), err)
	}
	fresh, err := store.Observe("vm:102", "uid-b")
	if err == nil || !strings.Contains(err.Error(), "busy") || !strings.Contains(err.Error(), ha.EpisodeName) {
		t.Fatalf("want a busy lock error that names the record, got %v", err)
	}
	if fresh.Recorded || fresh.Elapsed() != 0 {
		t.Fatalf("a new episode under a busy lock: recorded=%v elapsed=%s, want its clock starting now",
			fresh.Recorded, fresh.Elapsed())
	}
}

// Another process reads the clock a moment apart from this one, and the record
// keeps whole seconds, so an entry a little ahead of this clock is kept and
// trusted. One further ahead than endpointcache.FutureSkew is still pruned.
func TestEpisodeStore_AnEntryWithinTheSkewIsKeptAndTrusted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ahead time.Duration
		kept  bool
	}{
		{time.Second, true},
		{endpointcache.FutureSkew, true},
		{endpointcache.FutureSkew + time.Second, false},
	} {
		t.Run(tc.ahead.String(), func(t *testing.T) {
			t.Parallel()
			store, clk, _ := newEpisodeStore(t)
			ahead := *store
			ahead.Now = func() time.Time { return clk.t.Add(tc.ahead) }
			if _, err := ahead.Observe("vm:101", "uid-a"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Observe("vm:102", "uid-b"); err != nil {
				t.Fatal(err)
			}
			ep, err := ahead.Observe("vm:101", "uid-a")
			if err != nil {
				t.Fatal(err)
			}
			if ep.Recorded != tc.kept {
				t.Fatalf("an entry %s ahead: kept=%v after another write, want %v", tc.ahead, ep.Recorded, tc.kept)
			}
			if !tc.kept {
				return
			}
			again, err := store.Observe("vm:101", "uid-a")
			if err != nil {
				t.Fatal(err)
			}
			if !again.Recorded || again.Elapsed() != 0 || !again.FirstSeen.Equal(clk.t.Add(tc.ahead)) {
				t.Fatalf("an entry %s ahead: recorded=%v elapsed=%s first=%s, want it trusted with no time elapsed",
					tc.ahead, again.Recorded, again.Elapsed(), again.FirstSeen)
			}
		})
	}
}

func TestEpisodeScope(t *testing.T) {
	t.Parallel()
	cfg := func(hosts ...string) *config.CPIConfig { return &config.CPIConfig{Endpoints: hosts} }

	a := ha.EpisodeScope(cfg("pve1.example:8006", "pve2.example:8006"))
	if len(a) != 16 || strings.Contains(a, "pve") {
		t.Fatalf("scope = %q, want 16 hex characters that hold no host name", a)
	}
	if got := ha.EpisodeScope(cfg("pve2.example:8006", "pve1.example:8006")); got != a {
		t.Errorf("the order of the list must not change the scope: %q vs %q", got, a)
	}
	if got := ha.EpisodeScope(cfg("pve1.example", "pve2.example")); got != a {
		t.Errorf("a missing port means 8006: %q vs %q", got, a)
	}
	if got := ha.EpisodeScope(cfg("pve9.example:8006")); got == a {
		t.Error("different endpoints must give different scopes")
	}
	hostOnly := ha.EpisodeScope(&config.CPIConfig{Host: "pve1.example", Port: 8006})
	if hostOnly == "" || hostOnly != ha.EpisodeScope(cfg("pve1.example:8006")) {
		t.Errorf("a host and port is the one-entry list: %q", hostOnly)
	}
	if ha.EpisodeScope(nil) != "" || ha.EpisodeScope(&config.CPIConfig{}) != "" {
		t.Error("an entry with no endpoint has no scope")
	}
}
