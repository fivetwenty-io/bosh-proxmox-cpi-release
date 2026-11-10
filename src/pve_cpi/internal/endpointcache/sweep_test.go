package endpointcache

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	if err := os.Chtimes(filepath.Join(dir, "old-0000000000000002.json"), sweepNow.Add(-year), sweepNow.Add(-year)); err != nil {
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
	sweepFile(t, dir, sweepTemp, year)
	notOurs := func(fs.FileInfo) bool { return false }
	result := sweepWith(root, sweepNow, nil, notOurs)
	if result.Err != nil || len(result.Removed) != 0 {
		t.Errorf("a file another user owns was swept: %+v", result)
	}
	ours := func(fs.FileInfo) bool { return true }
	result = sweepWith(root, sweepNow, nil, ours)
	if result.Err != nil || len(result.Removed) != 2 {
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
