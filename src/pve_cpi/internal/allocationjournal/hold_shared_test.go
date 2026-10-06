package allocationjournal

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// createdDisk creates a disk record and returns its ID with the handle that
// still holds the record's lock.
func createdDisk(t *testing.T, j *Journal) (string, *Handle) {
	t.Helper()
	id, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.CreateDisk(context.Background(), id, intent())
	if err != nil {
		t.Fatal(err)
	}
	return id, h
}

// release runs a release HoldShared returned and fails the test on an error.
func release(t *testing.T, done func() error) {
	t.Helper()
	if err := done(); err != nil {
		t.Fatal(err)
	}
}

// heldFor reports whether HoldShared on id is still waiting after wait, and
// releases the lock when it got it.
func heldFor(t *testing.T, j *Journal, id string, wait time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	done, err := j.HoldShared(ctx, id)
	if err == nil {
		release(t, done)
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HoldShared: %v, want it to wait out the deadline", err)
	}
	return true
}

// TestHoldSharedWaitsForTheExclusiveHolder checks that HoldShared waits while
// a lifecycle's handle holds the record lock, and that it returns as soon as
// the handle closes.
func TestHoldSharedWaitsForTheExclusiveHolder(t *testing.T) {
	j, _ := fixture(t)
	id, h := createdDisk(t, j)
	type result struct {
		done func() error
		err  error
	}
	got := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done, err := j.HoldShared(ctx, id)
		got <- result{done, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("HoldShared returned while the handle held the lock: %v", r.err)
	case <-time.After(200 * time.Millisecond):
	}
	closeHandle(t, h)
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("HoldShared after the handle closed: %v", r.err)
		}
		release(t, r.done)
	case <-time.After(5 * time.Second):
		t.Fatal("HoldShared never returned after the handle closed")
	}
}

// TestHoldSharedEndsWithTheContext checks that HoldShared returns the
// context's error when the deadline passes while a handle holds the lock.
func TestHoldSharedEndsWithTheContext(t *testing.T) {
	j, _ := fixture(t)
	id, h := createdDisk(t, j)
	defer closeHandle(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done, err := j.HoldShared(ctx, id)
	if !errors.Is(err, context.DeadlineExceeded) || done != nil {
		t.Fatalf("HoldShared = %v, %v, want no release and the deadline's error", done != nil, err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := j.HoldShared(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("HoldShared on a cancelled context: %v, want context.Canceled", err)
	}
}

// TestHoldSharedLetsReadersShare checks that two shared holders hold the lock
// together, and that an Acquire waits until both have released it.
func TestHoldSharedLetsReadersShare(t *testing.T) {
	j, _ := fixture(t)
	id, h := createdDisk(t, j)
	closeHandle(t, h)
	first, err := j.HoldShared(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	second, err := j.HoldShared(ctx, id)
	if err != nil {
		t.Fatalf("a second reader waited for the first: %v", err)
	}
	acquireWithin := func(wait time.Duration) (*Handle, error) {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		return j.Acquire(ctx, id)
	}
	if h, err := acquireWithin(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		if h != nil {
			closeHandle(t, h)
		}
		t.Fatalf("Acquire with two readers holding the lock: %v, want it to wait", err)
	}
	release(t, first)
	if h, err := acquireWithin(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		if h != nil {
			closeHandle(t, h)
		}
		t.Fatalf("Acquire with one reader holding the lock: %v, want it to wait", err)
	}
	release(t, second)
	h, err = acquireWithin(2 * time.Second)
	if err != nil {
		t.Fatalf("Acquire after both readers released: %v", err)
	}
	closeHandle(t, h)
}

// journalTree reads every entry under dir with its mode, size, modification
// time, and content.
func journalTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := info.Mode().String() + " " + info.ModTime().String()
		if !d.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += " " + string(body)
		}
		tree[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestHoldSharedCreatesNothing checks that HoldShared on an allocation whose
// lock file doesn't exist returns at once and leaves the journal's directory
// exactly as it was.
func TestHoldSharedCreatesNothing(t *testing.T) {
	j, dir := fixture(t)
	id, h := createdDisk(t, j)
	closeHandle(t, h)
	unheld, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	before := journalTree(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, target := range []string{unheld, id} {
		done, err := j.HoldShared(ctx, target)
		if err != nil {
			t.Fatalf("HoldShared %s: %v", target, err)
		}
		release(t, done)
	}
	if after := journalTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("HoldShared changed the journal's directory\nbefore %v\nafter  %v", before, after)
	}
	if _, err := j.HoldShared(ctx, "not-an-allocation"); err == nil || !strings.Contains(err.Error(), "invalid allocation UUID") {
		t.Errorf("HoldShared on a malformed ID: %v, want the invalid UUID refusal", err)
	}
}

// TestHoldSharedSeesALockFreedByExit checks that a lock held by a process
// that exits without closing its handle is free for HoldShared once the
// process is gone.
func TestHoldSharedSeesALockFreedByExit(t *testing.T) {
	j, dir := fixture(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJournalChild$")
	cmd.Env = append(os.Environ(), "ALLOCATION_JOURNAL_CHILD=planned", "ALLOCATION_JOURNAL_DIRECTORY="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	data := make([]byte, 37)
	if _, err = io.ReadFull(stdout, data); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(data))
	if !heldFor(t, j, id, 100*time.Millisecond) {
		t.Fatal("HoldShared got the lock while the child held it")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("killed child unexpectedly succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := j.HoldShared(ctx, id)
	if err != nil {
		t.Fatalf("HoldShared after the holder exited: %v", err)
	}
	release(t, done)
}

// TestTryHoldSharedNeverWaits checks that TryHoldShared reports a lock a
// lifecycle's handle holds as not held, at once and without an error, and
// that it takes the lock in shared mode once the handle closes, beside
// another shared holder. A lock file that doesn't exist counts as free, and
// a malformed ID is refused.
func TestTryHoldSharedNeverWaits(t *testing.T) {
	j, dir := fixture(t)
	id, h := createdDisk(t, j)
	start := time.Now()
	done, held, err := j.TryHoldShared(id)
	if err != nil || held || done != nil {
		t.Fatalf("TryHoldShared while the handle held the lock = %v, %v, %v, want not held with no release and no error", done != nil, held, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("TryHoldShared took %s while the handle held the lock, want it to return at once", took)
	}
	closeHandle(t, h)
	first, held, err := j.TryHoldShared(id)
	if err != nil || !held {
		t.Fatalf("TryHoldShared after the handle closed = %v, %v, want the lock", held, err)
	}
	second, held, err := j.TryHoldShared(id)
	if err != nil || !held {
		t.Fatalf("a second TryHoldShared beside the first = %v, %v, want the shared lock", held, err)
	}
	release(t, first)
	release(t, second)
	unheld, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	before := journalTree(t, dir)
	done, held, err = j.TryHoldShared(unheld)
	if err != nil || !held {
		t.Fatalf("TryHoldShared on an allocation with no lock file = %v, %v, want a free lock", held, err)
	}
	release(t, done)
	if after := journalTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("TryHoldShared changed the journal's directory\nbefore %v\nafter  %v", before, after)
	}
	if _, _, err := j.TryHoldShared("not-an-allocation"); err == nil || !strings.Contains(err.Error(), "invalid allocation UUID") {
		t.Errorf("TryHoldShared on a malformed ID: %v, want the invalid UUID refusal", err)
	}
}
