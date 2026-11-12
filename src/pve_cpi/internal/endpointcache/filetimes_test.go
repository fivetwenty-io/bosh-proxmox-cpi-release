package endpointcache

import (
	"os"
	"testing"
	"time"
)

// touchAdvancesLockTime touches a stale lock and fails unless its modification
// time moves to about now.
func touchAdvancesLockTime(t *testing.T) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	lock, held, err := endpointTexts.policy.TryLock(root, "touch.lock")
	if err != nil || !held {
		t.Fatalf("TryLock = held %v, err %v", held, err)
	}
	defer func() { _ = lock.Close() }()
	old := time.Now().Add(-time.Hour)
	if err := root.Chtimes("touch.lock", old, old); err != nil {
		t.Fatal(err)
	}
	before, err := lock.File.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := endpointTexts.touchLock(lock); err != nil {
		t.Fatalf("touchLock: %v", err)
	}
	after, err := lock.File.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Fatalf("modification time %v did not advance past %v", after.ModTime(), before.ModTime())
	}
	if d := time.Since(after.ModTime()); d > time.Minute || d < -time.Minute {
		t.Fatalf("modification time %v is not near now", after.ModTime())
	}
}

func TestTouchLockAdvancesTheLockFileTime(t *testing.T) {
	t.Parallel()
	touchAdvancesLockTime(t)
}
