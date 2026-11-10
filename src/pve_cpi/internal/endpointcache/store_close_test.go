package endpointcache

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// seededStore opens a store on a fresh directory and writes one attempt into
// it, so a read of the file succeeds.
func seededStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := newStoreDir(t)
	s := openTestStore(t, dir, "secret-a", StoreOptions{})
	if _, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Attempt: failedAttempt(FailureNodesEmpty)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// noPanic runs f and reports a panic as a test failure that names the step, so
// a method that reaches for a closed directory fails its own case.
func noPanic(t *testing.T, step string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", step, r)
		}
	}()
	f()
}

// tempFiles returns the names in dir that a cut-off write would leave.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var temps []string
	for _, name := range storeDirNames(t, dir) {
		if strings.HasPrefix(name, privatefile.TempPrefix) {
			temps = append(temps, name)
		}
	}
	return temps
}

func wantClosed(t *testing.T, step string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "store is closed") {
		t.Errorf("%s returned %v, want the closed-store error", step, err)
	}
}

// Every method of a closed store, and of a store that was never opened, answers
// with the closed-store error and does not touch the directory. The private
// steps are called directly, because Update's own checks stop most callers
// before they reach them.
func TestStoreMethodsOfAClosedStoreReturnTheClosedError(t *testing.T) {
	ctx := context.Background()
	closedStore := func(t *testing.T) *Store {
		s, _ := seededStore(t)
		s.secretReplaced = true
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	stores := map[string]func(t *testing.T) *Store{
		"a closed store":            closedStore,
		"a store never opened":      func(*testing.T) *Store { return &Store{} },
		"a nil store":               func(*testing.T) *Store { return nil },
		"a store closed twice":      func(t *testing.T) *Store { s := closedStore(t); _ = s.Close(); return s },
		"a store with no directory": func(*testing.T) *Store { return &Store{guard: new(rootGuard)} },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			noPanic(t, "Read", func() { wantClosed(t, "Read", s.Read(new(Payload))) })
			noPanic(t, "ReadPayload", func() {
				_, _, err := s.ReadPayload(payloadNow, nil)
				wantClosed(t, "ReadPayload", err)
			})
			noPanic(t, "Update", func() {
				written, err := Update(ctx, s, func(*Payload) (*Payload, error) { return &Payload{}, nil })
				wantClosed(t, "Update", err)
				if written {
					t.Error("Update wrote through a closed store")
				}
			})
			noPanic(t, "UpdatePayload", func() {
				res, err := s.UpdatePayload(ctx, payloadNow, func(Stored) (Change, error) { return Change{}, nil })
				wantClosed(t, "UpdatePayload", err)
				if res.Locked || res.Written {
					t.Errorf("UpdatePayload reported %+v", res)
				}
			})
			noPanic(t, "Sweep", func() { wantClosed(t, "Sweep", s.Sweep(payloadNow).Err) })
			noPanic(t, "sweepUnderLock", func() { wantClosed(t, "sweepUnderLock", s.sweepUnderLock(payloadNow).Err) })
			noPanic(t, "TryLockDiscovery", func() {
				lock, held, err := s.TryLockDiscovery(ctx)
				wantClosed(t, "TryLockDiscovery", err)
				if lock != nil || held {
					t.Error("TryLockDiscovery returned a lock")
				}
			})
			noPanic(t, "lock", func() {
				lock, err := s.lock(ctx)
				wantClosed(t, "lock", err)
				if lock != nil {
					t.Error("lock returned a lock")
				}
			})
			noPanic(t, "write", func() { wantClosed(t, "write", s.write(&Payload{})) })
			noPanic(t, "SecretReplaced", func() {
				if path, replaced := s.SecretReplaced(); replaced || path != "" {
					t.Errorf("SecretReplaced returned %q, %v", path, replaced)
				}
			})
			noPanic(t, "Close", func() {
				if err := s.Close(); err != nil {
					t.Errorf("Close returned %v", err)
				}
			})
		})
	}
}

// A Close that lands right after a step took the directory leaves that step
// whole. The hook closes the store at the one moment a step could reach for a
// directory that is gone, so a step that used the store's field and not the
// directory it took panics or fails here on every run.
func TestStoreStepThatTookTheDirectoryFinishesWhenCloseLands(t *testing.T) {
	ctx := context.Background()
	steps := map[string]func(t *testing.T, s *Store){
		"Read": func(t *testing.T, s *Store) {
			if err := s.Read(new(Payload)); err != nil {
				t.Errorf("Read: %v", err)
			}
		},
		"ReadPayload": func(t *testing.T, s *Store) {
			if _, _, err := s.ReadPayload(payloadNow, nil); err != nil {
				t.Errorf("ReadPayload: %v", err)
			}
		},
		"Sweep": func(t *testing.T, s *Store) {
			if err := s.Sweep(payloadNow).Err; err != nil {
				t.Errorf("Sweep: %v", err)
			}
		},
		"sweepUnderLock": func(t *testing.T, s *Store) {
			if err := s.sweepUnderLock(payloadNow).Err; err != nil {
				t.Errorf("sweepUnderLock: %v", err)
			}
		},
		"TryLockDiscovery": func(t *testing.T, s *Store) {
			lock, held, err := s.TryLockDiscovery(ctx)
			if err != nil || !held {
				t.Fatalf("TryLockDiscovery: held %v, %v", held, err)
			}
			if err := lock.Close(); err != nil {
				t.Errorf("release the discover lock: %v", err)
			}
		},
		"lock": func(t *testing.T, s *Store) {
			lock, err := s.lock(ctx)
			if err != nil {
				t.Fatalf("lock: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Errorf("release the lock: %v", err)
			}
		},
		"write": func(t *testing.T, s *Store) {
			if err := s.write(&Payload{Key: s.Key()}); err != nil {
				t.Errorf("write: %v", err)
			}
		},
		"SecretReplaced": func(t *testing.T, s *Store) {
			if path, replaced := s.SecretReplaced(); !replaced || path == "" {
				t.Errorf("SecretReplaced returned %q, %v", path, replaced)
			}
		},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			s, dir := seededStore(t)
			s.secretReplaced = true
			var uses int
			s.afterUse = func() {
				uses++
				if err := s.Close(); err != nil {
					t.Errorf("Close inside %s: %v", name, err)
				}
			}
			noPanic(t, name, func() { step(t, s) })
			if uses != 1 {
				t.Errorf("%s took the directory %d times, want once", name, uses)
			}
			if temps := tempFiles(t, dir); len(temps) != 0 {
				t.Errorf("%s left %v behind", name, temps)
			}
			// The Close that landed during the step closed the directory when the
			// step ended, so the next step is refused.
			s.afterUse = nil
			wantClosed(t, "a step after the Close", s.Read(new(Payload)))
		})
	}
}

// Update is five steps, and each takes the directory for itself. A Close that
// lands at any one of them lets that step finish and fails the steps after it,
// without a panic, a torn file, or a temporary file left behind.
func TestStoreUpdateAcrossACloseAtEveryStep(t *testing.T) {
	steps := []string{"the closed check", "the lock", "the read", "the sweep", "the write"}
	build := func(Stored) (Change, error) {
		return Change{Attempt: &DiscoveryAttempt{
			At: payloadNow.Add(time.Minute), NextRefreshAfter: payloadNow.Add(time.Hour), Failure: FailureReadsFailed,
		}}, nil
	}
	t.Run("an update takes the directory once per step", func(t *testing.T) {
		s, _ := seededStore(t)
		var uses int
		s.afterUse = func() { uses++ }
		res, err := s.UpdatePayload(context.Background(), payloadNow.Add(time.Minute), build)
		if err != nil || !res.Written {
			t.Fatalf("UpdatePayload: %+v, %v", res, err)
		}
		if uses != len(steps) {
			t.Errorf("an update took the directory %d times, want %d: %v", uses, len(steps), steps)
		}
	})
	for closeAt := 1; closeAt <= len(steps); closeAt++ {
		t.Run("a Close at "+steps[closeAt-1], func(t *testing.T) {
			s, dir := seededStore(t)
			before := readStoreFile(t, dir+"/"+s.FileName())
			var uses int
			s.afterUse = func() {
				uses++
				if uses == closeAt {
					_ = s.Close()
				}
			}
			var res UpdateResult
			var err error
			noPanic(t, "UpdatePayload", func() {
				res, err = s.UpdatePayload(context.Background(), payloadNow.Add(time.Minute), build)
			})
			if closeAt == len(steps) {
				if err != nil || !res.Written {
					t.Errorf("a Close during the write left %+v, %v, want the write finished", res, err)
				}
			} else {
				wantClosed(t, "UpdatePayload", err)
			}
			after := readStoreFile(t, dir+"/"+s.FileName())
			if (string(before) == string(after)) == res.Written {
				t.Errorf("written %v but the file changed is %v", res.Written, string(before) != string(after))
			}
			if temps := tempFiles(t, dir); len(temps) != 0 {
				t.Errorf("a temporary file stayed: %v", temps)
			}
			other := openTestStore(t, dir, "secret-a", StoreOptions{})
			if _, _, err := other.ReadPayload(payloadNow.Add(2*time.Minute), nil); err != nil {
				t.Errorf("the file does not read back whole: %v", err)
			}
		})
	}
}

// A write that Close cuts off at its first step finishes, and one that fails
// after the Close removes its temporary file, because the write holds the
// directory until it ends.
func TestStoreWriteCutOffByCloseLeavesNoTemporaryFile(t *testing.T) {
	ctx := context.Background()
	merge := func(s *Store) func(*storeTestPayload) (*storeTestPayload, error) {
		return func(*storeTestPayload) (*storeTestPayload, error) {
			return &storeTestPayload{Key: s.Key(), Note: "new"}, nil
		}
	}
	t.Run("a Close during the write lets it finish", func(t *testing.T) {
		dir := newStoreDir(t)
		s := openTestStore(t, dir, "secret-a", StoreOptions{})
		storePut(t, s, "old")
		root := s.guard.root
		inner := s.ops.Write
		s.ops.Write = func(f *os.File, b []byte) (int, error) {
			if err := s.Close(); err != nil {
				t.Errorf("Close during the write: %v", err)
			}
			return inner(f, b)
		}
		written, err := Update(ctx, s, merge(s))
		if err != nil || !written {
			t.Fatalf("Update: written %v, %v", written, err)
		}
		if temps := tempFiles(t, dir); len(temps) != 0 {
			t.Errorf("the write left %v behind", temps)
		}
		if _, err := root.Lstat("."); !errors.Is(err, os.ErrClosed) {
			t.Errorf("the directory is still open after the write ended: %v", err)
		}
		reader := openTestStore(t, dir, "secret-a", StoreOptions{})
		var got storeTestPayload
		if err := reader.Read(&got); err != nil || got.Note != "new" {
			t.Errorf("the file holds %+v, %v, want the new payload whole", got, err)
		}
	})
	t.Run("a write that fails after the Close removes its temporary file", func(t *testing.T) {
		dir := newStoreDir(t)
		s := openTestStore(t, dir, "secret-a", StoreOptions{})
		storePut(t, s, "old")
		boom := errors.New("rename refused")
		s.ops.Rename = func(*os.Root, string, string) error {
			_ = s.Close()
			return boom
		}
		written, err := Update(ctx, s, merge(s))
		if written || !errors.Is(err, boom) {
			t.Fatalf("Update: written %v, %v, want the rename's failure", written, err)
		}
		if temps := tempFiles(t, dir); len(temps) != 0 {
			t.Errorf("the failed write left %v behind", temps)
		}
		reader := openTestStore(t, dir, "secret-a", StoreOptions{})
		var got storeTestPayload
		if err := reader.Read(&got); err != nil || got.Note != "old" {
			t.Errorf("the file holds %+v, %v, want the old payload whole", got, err)
		}
	})
}

// Close never waits for a step. The directory a Close parked stays usable to
// the step that holds it and closes when the last such step ends.
func TestStoreCloseParksTheDirectoryForARunningStep(t *testing.T) {
	s, _ := seededStore(t)
	first, endFirst := s.use()
	second, endSecond := s.use()
	if first == nil || first != second {
		t.Fatalf("the steps took %v and %v, want one open directory", first, second)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if again, endAgain := s.use(); again != nil {
		t.Errorf("a step took the directory %v after Close", again)
	} else {
		endAgain()
	}
	if _, err := first.Lstat("."); err != nil {
		t.Errorf("the directory closed under a running step: %v", err)
	}
	endFirst()
	if _, err := first.Lstat("."); err != nil {
		t.Errorf("the directory closed while a step still held it: %v", err)
	}
	endSecond()
	if _, err := first.Lstat("."); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the directory stayed open after the last step ended: %v", err)
	}
}

// closedPollLimit bounds how long a test waits for a step in another goroutine
// to see a Close that has already returned.
const closedPollLimit = 10 * time.Second

// readUntilClosed reads the store until a read returns the closed-store error,
// and returns every other result it saw on the way. It polls without any
// synchronization with the goroutine that closes the store, so the race
// detector still sees the two as concurrent, and it ends because a Close that
// has returned is seen by the next step. It stops at closedPollLimit.
func readUntilClosed(s *Store) (before []error, closed bool) {
	deadline := time.Now().Add(closedPollLimit)
	for time.Now().Before(deadline) {
		err := s.Read(new(Payload))
		if err != nil && strings.Contains(err.Error(), "store is closed") {
			return before, true
		}
		before = append(before, err)
		runtime.Gosched()
	}
	return before, false
}

// Close and a step in another goroutine, with nothing the race detector can
// see between them, are a race unless the store guards the directory. Under
// -race an unguarded access fails these on every run. The order of the two is
// not timed. The step polls until it sees the Close, and a step that began
// before the Close must have read the file whole.
func TestStoreCloseAndAStepInAnotherGoroutineAreNotARace(t *testing.T) {
	type outcome struct {
		before []error
		closed bool
	}
	t.Run("Close first, then a step", func(t *testing.T) {
		s, _ := seededStore(t)
		gate := make(chan struct{})
		done := make(chan outcome, 1)
		go func() {
			<-gate
			before, closed := readUntilClosed(s)
			done <- outcome{before, closed}
		}()
		close(gate)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if !got.closed {
			t.Fatalf("no Read saw the Close within %v", closedPollLimit)
		}
		for _, err := range got.before {
			if err != nil {
				t.Errorf("Read before the Close: %v", err)
			}
		}
		wantClosed(t, "Read after the Close", s.Read(new(Payload)))
	})
	t.Run("a step first, then Close", func(t *testing.T) {
		s, _ := seededStore(t)
		started := make(chan struct{})
		done := make(chan outcome, 1)
		go func() {
			err := s.Read(new(Payload))
			close(started)
			before, closed := readUntilClosed(s)
			done <- outcome{append([]error{err}, before...), closed}
		}()
		<-started
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if !got.closed {
			t.Fatalf("no Read saw the Close within %v", closedPollLimit)
		}
		for _, err := range got.before {
			if err != nil {
				t.Errorf("Read before the Close: %v", err)
			}
		}
	})
}

// A copy of a Store shares the directory and its closed state with the
// original, so a Close on either one closes both and the directory closes once.
func TestStoreCopySharesTheDirectoryAndItsClosedState(t *testing.T) {
	t.Run("Close on the original closes the copy", func(t *testing.T) {
		s, _ := seededStore(t)
		c := *s
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wantClosed(t, "Read on the copy", c.Read(new(Payload)))
		if root, done := c.use(); root != nil || c.guard.uses != 0 {
			t.Errorf("use on the copy gave root %v and left %d steps running", root, c.guard.uses)
		} else {
			done()
		}
		if !c.closed() {
			t.Error("the copy does not report that the store is closed")
		}
		if err := c.Close(); err != nil {
			t.Errorf("Close on the copy after the original's: %v", err)
		}
	})
	t.Run("Close on the copy closes the original", func(t *testing.T) {
		s, _ := seededStore(t)
		c := *s
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		wantClosed(t, "Read on the original", s.Read(new(Payload)))
		if err := s.Close(); err != nil {
			t.Errorf("Close on the original after the copy's: %v", err)
		}
	})
}
