package endpointcache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// sweepNow is the sweep's clock in these tests. Files get their ages from it.
var sweepNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// Names the cache makes for one key.
const (
	sweepBase     = "pve1.dc1.example.com-5b0e6c2f4a1d9e07"
	sweepKey      = sweepBase + ".json"
	sweepLock     = sweepBase + ".lock"
	sweepDiscover = sweepBase + ".discover.lock"
	sweepTemp     = ".tmp-0123456789abcdef0123456789abcdef"
)

// sweepFile creates the file name in dir at mode 0600, aged by age before
// sweepNow.
func sweepFile(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	writeStoreFile(t, path, []byte("x"), 0o600)
	mtime := sweepNow.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func sweepDir(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := newStoreDir(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func runSweep(t *testing.T, root *os.Root, keep ...string) SweepResult {
	t.Helper()
	result := Sweep(root, sweepNow, keep...)
	if result.Err != nil {
		t.Fatalf("Sweep: %v", result.Err)
	}
	return result
}

func TestSweepRemovesTemporaryFilesOlderThanAnHour(t *testing.T) {
	dir, root := sweepDir(t)
	sweepFile(t, dir, sweepTemp, 61*time.Minute)
	sweepFile(t, dir, ".tmp-fresh", 59*time.Minute)
	sweepFile(t, dir, ".tmp-edge", time.Hour)
	result := runSweep(t, root)
	if !reflect.DeepEqual(result.Removed, []string{sweepTemp}) {
		t.Errorf("removed %v, want only the temporary file older than an hour", result.Removed)
	}
	if got := storeDirNames(t, dir); !reflect.DeepEqual(got, []string{".tmp-edge", ".tmp-fresh"}) {
		t.Errorf("left %v", got)
	}
}

func TestSweepRemovesCacheAndLockFilesUnmodifiedForThirtyDays(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepKey, 31*day)
	sweepFile(t, dir, sweepLock, 31*day)
	sweepFile(t, dir, sweepDiscover, 31*day)
	fresh := "other.example.org-0000000000000001"
	sweepFile(t, dir, fresh+".json", 29*day)
	sweepFile(t, dir, fresh+".lock", 29*day)
	sweepFile(t, dir, fresh+".discover.lock", 30*day)
	result := runSweep(t, root)
	if want := []string{sweepDiscover, sweepKey, sweepLock}; !reflect.DeepEqual(result.Removed, want) {
		t.Errorf("removed %v, want %v", result.Removed, want)
	}
	if got := storeDirNames(t, dir); len(got) != 3 {
		t.Errorf("left %v", got)
	}
}

func TestSweepKeepsALockBesideAFreshKeyFile(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepKey, time.Hour)
	sweepFile(t, dir, sweepLock, 40*day)
	sweepFile(t, dir, sweepDiscover, 40*day)
	if result := runSweep(t, root); len(result.Removed) != 0 {
		t.Errorf("removed %v, want the locks of a fresh key file kept", result.Removed)
	}
	// The key file is the only one that has aged: a lock in daily use keeps its place, and the key goes.
	sweepFile(t, dir, sweepKey, 40*day)
	sweepFile(t, dir, sweepLock, time.Minute)
	sweepFile(t, dir, sweepDiscover, time.Minute)
	result := runSweep(t, root)
	if !reflect.DeepEqual(result.Removed, []string{sweepKey}) {
		t.Errorf("removed %v, want only the old key file", result.Removed)
	}
}

func TestSweepNeverRemovesTheLocksItHolds(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepKey, 60*day)
	sweepFile(t, dir, sweepLock, 60*day)
	sweepFile(t, dir, sweepDiscover, 60*day)
	result := runSweep(t, root, sweepLock, sweepKey)
	if !reflect.DeepEqual(result.Removed, []string{sweepDiscover}) {
		t.Errorf("removed %v, want only the file that was not held", result.Removed)
	}
	if got := storeDirNames(t, dir); !reflect.DeepEqual(got, []string{sweepKey, sweepLock}) {
		t.Errorf("left %v", got)
	}
}

func TestSweepLeavesTheSecretAlone(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, "secret", year)
	sweepFile(t, dir, "secret.lock", year)
	if result := runSweep(t, root); len(result.Removed) != 0 {
		t.Errorf("removed %v", result.Removed)
	}
	if got := storeDirNames(t, dir); !reflect.DeepEqual(got, []string{"secret", "secret.lock"}) {
		t.Errorf("left %v", got)
	}
}

func TestSweepLeavesForeignFilesAlone(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	// Names that end in .json or .lock but are not the cache's, and names with no suffix.
	foreign := []string{
		"audit-x.json", "notes.txt", "audit.lock", "backup.json.bak", "x.discover.lock",
		"pve1.dc1.example.com-5b0e6c2f4a1d9e0.json",   // 15 digest characters
		"pve1.dc1.example.com-5b0e6c2f4a1d9e077.json", // 17 digest characters
		"pve1.dc1.example.com-5B0E6C2F4A1D9E07.json",  // upper-case digest
		"PVE1.dc1.example.com-5b0e6c2f4a1d9e07.json",  // upper-case host
		"-5b0e6c2f4a1d9e07.json",                      // no host
		".-5b0e6c2f4a1d9e07.json",                     // hidden
		".tmp", ".tmpx", "tmp-0123", ".hidden", "secret.bak", "secret2",
	}
	for _, name := range foreign {
		sweepFile(t, dir, name, year)
	}
	// An old file in a subdirectory, and a subdirectory that is named like a key file.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	sweepFile(t, dir, filepath.Join("sub", sweepKey), year)
	sweepFile(t, dir, filepath.Join("sub", sweepTemp), year)
	if err := os.Mkdir(filepath.Join(dir, "old-0000000000000002.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	sweepFile(t, dir, filepath.Join("old-0000000000000002.json", sweepTemp), year)
	if err := os.Chtimes(
		filepath.Join(dir, "old-0000000000000002.json"),
		sweepNow.Add(-year),
		sweepNow.Add(-year),
	); err != nil {
		t.Fatal(err)
	}
	before := storeDirNames(t, dir)

	if result := runSweep(t, root); len(result.Removed) != 0 {
		t.Errorf("removed %v", result.Removed)
	}
	if after := storeDirNames(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("the directory changed:\n before %v\n after  %v", before, after)
	}
	for _, name := range []string{filepath.Join("sub", sweepKey), filepath.Join("sub", sweepTemp),
		filepath.Join("old-0000000000000002.json", sweepTemp)} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s did not survive: %v", name, err)
		}
	}
}

func TestSweepLeavesSymlinksAndTheirTargetsAlone(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	outside := filepath.Join(t.TempDir(), "outside")
	writeStoreFile(t, outside, []byte("x"), 0o600)
	old := sweepNow.Add(-year)
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sweepKey, sweepLock, sweepTemp} {
		if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if result := runSweep(t, root); len(result.Removed) != 0 {
		t.Errorf("removed %v", result.Removed)
	}
	if len(storeDirNames(t, dir)) != 3 {
		t.Errorf("a symlink was removed: %v", storeDirNames(t, dir))
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Errorf("the target was removed: %v", err)
	}
}

func TestSweepLeavesFilesAnotherUserOwns(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepKey, year)
	sweepFile(t, dir, sweepLock, year)
	sweepFile(t, dir, sweepTemp, year)
	notOurs := func(fs.FileInfo) bool { return false }
	result := endpointTexts.sweepWith(root, sweepNow, nil, notOurs, "")
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("a file another user owns was swept: %+v", result)
	}
	ours := func(fs.FileInfo) bool { return true }
	result = endpointTexts.sweepWith(root, sweepNow, nil, ours, "")
	if result.Err != nil || len(result.Removed) != 3 {
		t.Errorf("owned files were kept: %+v", result)
	}
}

func TestOwnedByEffectiveUser(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByEffectiveUser(info) {
		t.Error("a directory this process made is not owned by the effective user")
	}
	if ownedByEffectiveUser(fakeFileInfo{}) {
		t.Error("a file with no ownership data counts as owned")
	}
	if os.Geteuid() != 0 {
		root, err := os.Lstat("/")
		if err != nil {
			t.Fatal(err)
		}
		if ownedByEffectiveUser(root) {
			t.Error("the filesystem root counts as owned by a non-root user")
		}
	}
}

// fakeFileInfo carries no ownership data.
type fakeFileInfo struct{ fs.FileInfo }

func (fakeFileInfo) Sys() any { return nil }

func TestSweepReportsAFileItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepKey, year)
	sweepFile(t, dir, sweepLock, year)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	result := Sweep(root, sweepNow)
	if result.Err == nil || len(result.Removed) != 0 {
		t.Errorf("Sweep: %+v, want an error and nothing removed", result)
	}
}

func TestSweepReportsAClosedRoot(t *testing.T) {
	_, root := sweepDir(t)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if result := Sweep(root, sweepNow); result.Err == nil {
		t.Error("Sweep of a closed root reported no error")
	}
}

func TestSweepIgnoresAnEmptyDirectory(t *testing.T) {
	_, root := sweepDir(t)
	if result := Sweep(root, sweepNow); result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("Sweep of an empty directory: %+v", result)
	}
}

func TestSweepNamesMatchTheKeyFileNames(t *testing.T) {
	key := payloadTestKey()
	for _, name := range []string{key.FileName(), key.LockName(), key.DiscoverLockName()} {
		if _, ok := sweepAge(name, nil); !ok {
			t.Errorf("the sweep does not recognize %s", name)
		}
	}
	// Hosts that sanitize to the extremes: 63 characters, and the fallback.
	long := Key{Endpoints: []string{"a." + strings.Repeat("b", 70) + ":8006"}}
	fallback := Key{Endpoints: []string{"[::]:8006"}}
	for _, k := range []Key{long, fallback} {
		for _, name := range []string{k.FileName(), k.LockName(), k.DiscoverLockName()} {
			if _, ok := sweepAge(name, nil); !ok {
				t.Errorf("the sweep does not recognize %s", name)
			}
		}
	}
	if base, ok := lockKeyFile(key.LockName()); !ok || base != key.FileName() {
		t.Errorf("lockKeyFile(%s) = %s, %v, want %s", key.LockName(), base, ok, key.FileName())
	}
	if base, ok := lockKeyFile(key.DiscoverLockName()); !ok || base != key.FileName() {
		t.Errorf("lockKeyFile(%s) = %s, %v, want %s", key.DiscoverLockName(), base, ok, key.FileName())
	}
	if _, ok := lockKeyFile(key.FileName()); ok {
		t.Error("a key file has a key file")
	}
	if _, ok := lockKeyFile("secret.lock"); ok {
		t.Error("secret.lock has a key file")
	}
}

func TestSweepWithAPrefixLeavesEveryOtherKindAlone(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	const prefixed = HAProfilePrefix + sweepBase + ".json"
	const prefixedLock = HAProfilePrefix + sweepBase + ".lock"
	sweepFile(t, dir, sweepKey, year)
	sweepFile(t, dir, sweepLock, year)
	sweepFile(t, dir, prefixed, year)
	sweepFile(t, dir, prefixedLock, year)
	sweepFile(t, dir, sweepTemp, year)
	ours := func(fs.FileInfo) bool { return true }

	result := endpointTexts.sweepWith(root, sweepNow, nil, ours, HAProfilePrefix)
	if result.Err != nil || !sameNames(result.Removed, []string{prefixed, prefixedLock, sweepTemp}) {
		t.Errorf(
			"a sweep limited to %s removed %+v, want the prefixed files and the temporary file",
			HAProfilePrefix,
			result,
		)
	}
	result = endpointTexts.sweepWith(root, sweepNow, nil, ours, "")
	if result.Err != nil || !sameNames(result.Removed, []string{sweepKey, sweepLock}) {
		t.Errorf("a sweep with no prefix removed %+v, want the files that were left", result)
	}
}

// holdSweepLock takes the lock file name the way a writer's flock does, but
// leaves its times alone, so a test can age the file and still have it held.
func holdSweepLock(t *testing.T, root *os.Root, name string) {
	t.Helper()
	lock, held, err := endpointTexts.policy.TryLockExisting(root, name)
	if err != nil || !held {
		t.Fatalf("hold %s: held=%v err=%v", name, held, err)
	}
	t.Cleanup(func() { _ = lock.Close() })
}

// onSweepLock runs fn when the sweep holds a victim's lock and is about to
// look at it again, for the file name only.
func onSweepLock(t *testing.T, name string, fn func()) *bool {
	t.Helper()
	fired := false
	afterSweepLock = func(got string) {
		if got == name {
			fired = true
			fn()
		}
	}
	t.Cleanup(func() { afterSweepLock = nil })
	return &fired
}

// A writer that locks a stale lock file between the sweep's stat and its
// unlink holds the lock, so the sweep must leave the file and a second
// process must not get a lock on a new inode beside the writer.
func TestSweepSkipsALockAWriterTakesAfterTheStat(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepLock, 31*day)

	var writer *privatefile.Lock
	owned := func(info fs.FileInfo) bool {
		if writer == nil && info.Name() == sweepLock {
			var held bool
			var err error
			writer, held, err = endpointTexts.tryHeldLock(context.Background(), root, sweepLock)
			if err != nil || !held {
				t.Fatalf("writer lock: held=%v err=%v", held, err)
			}
		}
		return ownedByEffectiveUser(info)
	}
	result := endpointTexts.sweepWith(root, sweepNow, nil, owned, "")
	if writer == nil {
		t.Fatal("the writer never took the lock")
	}
	defer func() { _ = writer.Close() }()
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("sweep of a lock a writer holds: %+v", result)
	}
	second, held, err := endpointTexts.tryHeldLock(context.Background(), root, sweepLock)
	if err != nil {
		t.Fatalf("second locker: %v", err)
	}
	if held {
		_ = second.Close()
		t.Errorf("two holders of %s: the sweep unlinked the lock the writer holds", sweepLock)
	}
}

// A writer that renames a fresh payload over a stale one between the sweep's
// stat and its unlink must keep it.
func TestSweepKeepsAPayloadReplacedAfterTheStat(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepKey, 31*day)
	sweepFile(t, dir, sweepLock, 31*day)
	replaced := false
	owned := func(info fs.FileInfo) bool {
		if !replaced && info.Name() == sweepKey {
			replaced = true
			tmp := filepath.Join(dir, "new.tmp")
			writeStoreFile(t, tmp, []byte("fresh"), 0o600)
			if err := os.Rename(tmp, filepath.Join(dir, sweepKey)); err != nil {
				t.Fatal(err)
			}
		}
		return ownedByEffectiveUser(info)
	}
	result := endpointTexts.sweepWith(root, sweepNow, nil, owned, "")
	if !replaced {
		t.Fatal("the payload was never replaced")
	}
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("sweep of a payload a writer replaced: %+v", result)
	}
	got, err := os.ReadFile(filepath.Join(dir, sweepKey))
	if err != nil || string(got) != "fresh" {
		t.Errorf("fresh payload: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, sweepLock)); err != nil {
		t.Errorf("the lock beside a fresh payload is gone: %v", err)
	}
}

// A payload replaced by another file that is as old as the first one still
// fails the inode comparison, so the sweep leaves it for the next pass.
func TestSweepKeepsAPayloadWhoseInodeChangedUnderTheLock(t *testing.T) {
	dir, root := sweepDir(t)
	const day = 24 * time.Hour
	sweepFile(t, dir, sweepKey, 31*day)
	sweepFile(t, dir, sweepLock, 31*day)
	fired := onSweepLock(t, sweepKey, func() {
		if err := os.Remove(filepath.Join(dir, sweepKey)); err != nil {
			t.Fatal(err)
		}
		sweepFile(t, dir, sweepKey, 32*day)
	})
	result := endpointTexts.sweepWith(root, sweepNow, nil, ownedByEffectiveUser, "")
	if !*fired {
		t.Fatal("the sweep never took the lock for the payload")
	}
	if result.Err != nil || slices.Contains(result.Removed, sweepKey) {
		t.Errorf("sweep removed a payload whose inode changed: %+v", result)
	}
	if _, err := os.Lstat(filepath.Join(dir, sweepKey)); err != nil {
		t.Errorf("replacement payload is gone: %v", err)
	}
}

// A lock another process holds is live, whatever its times say, and so is the
// payload it guards.
func TestSweepSkipsAFileWhoseLockIsBusy(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepKey, year)
	sweepFile(t, dir, sweepLock, year)
	sweepFile(t, dir, sweepDiscover, year)
	holdSweepLock(t, root, sweepLock)
	// The lock keeps its old times: only the flock says it is in use.
	old := sweepNow.Add(-year)
	if err := os.Chtimes(filepath.Join(dir, sweepLock), old, old); err != nil {
		t.Fatal(err)
	}

	result := runSweep(t, root)
	if !reflect.DeepEqual(result.Removed, []string{sweepDiscover}) {
		t.Errorf("removed %v, want only the discover lock, whose lock nobody holds", result.Removed)
	}
	if got := storeDirNames(t, dir); !reflect.DeepEqual(got, []string{sweepKey, sweepLock}) {
		t.Errorf("left %v, want the held lock and the payload it guards", got)
	}
}

func TestSweepSkipsADiscoverLockSomeoneHolds(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepDiscover, year)
	holdSweepLock(t, root, sweepDiscover)
	result := runSweep(t, root)
	if len(result.Removed) != 0 {
		t.Errorf("removed %v, want the held discover lock kept", result.Removed)
	}
}

// A lock replaced by an older file between the lock and the recheck is a
// different inode from the one that was locked, so the sweep leaves it.
func TestSweepKeepsALockReplacedAfterItWasLocked(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepLock, year)
	fired := onSweepLock(t, sweepLock, func() {
		if err := os.Remove(filepath.Join(dir, sweepLock)); err != nil {
			t.Fatal(err)
		}
		sweepFile(t, dir, sweepLock, year)
	})
	result := endpointTexts.sweepWith(root, sweepNow, nil, ownedByEffectiveUser, "")
	if !*fired {
		t.Fatal("the sweep never took the lock")
	}
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("sweep of a lock that was replaced: %+v", result)
	}
	if _, err := os.Lstat(filepath.Join(dir, sweepLock)); err != nil {
		t.Errorf("replacement lock is gone: %v", err)
	}
}

// A lock another process touched after the sweep took its decision is in use
// again, so the recheck of its age keeps it.
func TestSweepKeepsALockTouchedAfterTheStat(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepLock, year)
	fired := onSweepLock(t, sweepLock, func() {
		fresh := sweepNow.Add(-time.Minute)
		if err := os.Chtimes(filepath.Join(dir, sweepLock), fresh, fresh); err != nil {
			t.Fatal(err)
		}
	})
	result := endpointTexts.sweepWith(root, sweepNow, nil, ownedByEffectiveUser, "")
	if !*fired {
		t.Fatal("the sweep never took the lock")
	}
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("sweep of a lock used after the stat: %+v", result)
	}
}

// A key file written after the sweep's stat makes the lock beside it young.
func TestSweepKeepsALockWhoseKeyFileWasWrittenAfterTheStat(t *testing.T) {
	dir, root := sweepDir(t)
	const year = 365 * 24 * time.Hour
	sweepFile(t, dir, sweepLock, year)
	fired := onSweepLock(t, sweepLock, func() {
		sweepFile(t, dir, sweepKey, time.Minute)
	})
	result := endpointTexts.sweepWith(root, sweepNow, nil, ownedByEffectiveUser, "")
	if !*fired {
		t.Fatal("the sweep never took the lock")
	}
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("sweep of a lock beside a new key file: %+v", result)
	}
}

// A payload with no lock file is skipped, and the sweep does not make a lock
// file to guard it.
func TestSweepSkipsAPayloadThatHasNoLockFile(t *testing.T) {
	dir, root := sweepDir(t)
	sweepFile(t, dir, sweepKey, 365*24*time.Hour)
	result := runSweep(t, root)
	if len(result.Removed) != 0 {
		t.Errorf("removed %v, want a payload without a lock left alone", result.Removed)
	}
	if got := storeDirNames(t, dir); !reflect.DeepEqual(got, []string{sweepKey}) {
		t.Errorf("directory holds %v, want the payload alone and no new lock file", got)
	}
}

// A lock file that grants group access is not locked, and the sweep reports
// why instead of removing it.
func TestSweepReportsALockItCannotSafelyOpen(t *testing.T) {
	dir, root := sweepDir(t)
	sweepFile(t, dir, sweepLock, 365*24*time.Hour)
	if err := os.Chmod(filepath.Join(dir, sweepLock), 0o644); err != nil {
		t.Fatal(err)
	}
	result := Sweep(root, sweepNow)
	if !errors.Is(result.Err, privatefile.ErrUnsafeMode) || len(result.Removed) != 0 {
		t.Errorf("sweep of an unsafe lock: %+v, want an unsafe mode error and no removal", result)
	}
}

// A waiter that opened the lock file's old inode gets the lock when the sweep
// lets go of it, finds that the name no longer reaches that inode, and takes
// the lock again on a new one, so it ends as the only holder.
func TestSweepLeavesAWaiterOnTheOldInodeAsTheOnlyHolder(t *testing.T) {
	dir, root := sweepDir(t)
	sweepFile(t, dir, sweepLock, 365*24*time.Hour)
	type outcome struct {
		lock *privatefile.Lock
		err  error
	}
	done := make(chan outcome, 1)
	var oldInode fs.FileInfo
	fired := onSweepLock(t, sweepLock, func() {
		var err error
		if oldInode, err = os.Lstat(filepath.Join(dir, sweepLock)); err != nil {
			t.Fatal(err)
		}
		waiter, err := endpointTexts.policy.Open(root, sweepLock, os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			first := true
			lock, _, err := endpointTexts.heldLock(root, sweepLock, func() (*privatefile.Lock, bool, error) {
				if first {
					first = false
					// A blocking flock on the old inode, which the sweep holds.
					if err := syscall.Flock(int(waiter.Fd()), syscall.LOCK_EX); err != nil {
						return nil, false, err
					}
					return &privatefile.Lock{File: waiter}, true, nil
				}
				lock, err := endpointTexts.waitCreatedLock(context.Background(), root, sweepLock)
				return lock, err == nil, err
			})
			done <- outcome{lock, err}
		}()
		time.Sleep(50 * time.Millisecond)
	})
	result := endpointTexts.sweepWith(root, sweepNow, nil, ownedByEffectiveUser, "")
	if !*fired {
		t.Fatal("the sweep never took the lock")
	}
	if result.Err != nil || !reflect.DeepEqual(result.Removed, []string{sweepLock}) {
		t.Fatalf("sweep: %+v, want the stale lock removed", result)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got a lock")
	}
	if got.err != nil {
		t.Fatalf("waiter: %v", got.err)
	}
	defer func() { _ = got.lock.Close() }()
	held, err := got.lock.File.Stat()
	if err != nil {
		t.Fatal(err)
	}
	named, err := root.Lstat(sweepLock)
	if err != nil {
		t.Fatalf("the waiter's lock has no name: %v", err)
	}
	if !os.SameFile(held, named) || os.SameFile(held, oldInode) {
		t.Errorf("waiter holds an inode the name does not reach, or still the swept one")
	}
	other, ok, err := endpointTexts.tryHeldLock(context.Background(), root, sweepLock)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = other.Close()
		t.Errorf("a second holder got %s beside the waiter", sweepLock)
	}
}
