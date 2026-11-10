package endpointcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// createRaceRetries bounds how often a lock open that fails with ENOENT is
	// retried. macOS can fail the loser's O_CREATE open that way while the
	// winner's create is in flight, and the file exists a moment later.
	createRaceRetries = 5
	// lockReplacedRetries bounds how often a lock is taken again after the
	// lock file's name turned out to hold another inode, or none, once the
	// flock succeeded. Each retry means someone removed or replaced the file
	// in the instant between the open and the check, so a few in a row mean
	// the directory is being tampered with, not raced.
	lockReplacedRetries = 3
)

// ErrLockReplaced means a cache lock file was removed or replaced each time
// this process locked it, lockReplacedRetries times after the first. Nothing
// was locked. A write or a discovery that gets it is dropped, and a store
// that gets it for secret.lock is not opened, so the caller runs without a
// cache for that call.
var ErrLockReplaced = errors.New("endpoint cache: lock file kept changing while it was locked")

// ErrLockWaitAbandoned means the caller's context ended while a cache lock
// wait was running. The returned error also wraps the context's own error.
var ErrLockWaitAbandoned = errors.New("endpoint cache: lock wait ended with the caller's context")

// waitHeldLock waits within ctx for the exclusive lock on name, then confirms
// it with heldLock.
func (t *texts) waitHeldLock(ctx context.Context, r *os.Root, name string) (*privatefile.Lock, error) {
	lock, _, err := t.heldLock(r, name, func() (*privatefile.Lock, bool, error) {
		lock, err := t.waitCreatedLock(ctx, r, name)
		return lock, err == nil, err
	})
	return lock, err
}

// tryHeldLock takes the exclusive lock on name without waiting for another
// holder, then confirms it with heldLock. held is false, with no error, when
// another holder has the lock now.
func (t *texts) tryHeldLock(ctx context.Context, r *os.Root, name string) (*privatefile.Lock, bool, error) {
	return t.heldLock(r, name, func() (*privatefile.Lock, bool, error) {
		for attempt := 0; ; attempt++ {
			lock, held, err := t.policy.TryLock(r, name)
			if !errors.Is(err, fs.ErrNotExist) || attempt == createRaceRetries {
				return lock, held, err
			}
			if err := privatefile.Pause(ctx); err != nil {
				return nil, false, err
			}
		}
	})
}

// heldLock takes a lock through take and keeps it only when name still names
// the inode that was locked. A lock file removed under its holder lets the
// next opener create and lock a new inode while the old one is still held, and
// two holders would then run at once, so a lock on an inode the name no
// longer reaches is released and taken again, at most lockReplacedRetries
// more times before ErrLockReplaced. A kept lock also has its file's times
// updated through the descriptor, so a lock in daily use never looks
// abandoned to the sweep.
func (t *texts) heldLock(r *os.Root, name string, take func() (*privatefile.Lock, bool, error)) (*privatefile.Lock, bool, error) {
	for attempt := 0; ; attempt++ {
		lock, held, err := take()
		if err != nil || !held {
			return nil, held, err
		}
		current, err := t.lockNamesInode(r, name, lock)
		if err == nil && current {
			if err = t.touchLock(lock); err == nil {
				return lock, true, nil
			}
		}
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: release lock file %s: %w", t.name, name, closeErr))
		}
		if err != nil {
			return nil, false, err
		}
		if attempt == lockReplacedRetries {
			return nil, false, fmt.Errorf("%w: %s", t.lockReplaced, name)
		}
	}
}

// lockNamesInode reports whether name in r is the inode lock holds. A name
// that no longer exists reports false with no error.
func (t *texts) lockNamesInode(r *os.Root, name string, lock *privatefile.Lock) (bool, error) {
	locked, err := lock.File.Stat()
	if err != nil {
		return false, fmt.Errorf("%s: stat locked file %s: %w", t.name, name, err)
	}
	named, err := r.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(locked, named), nil
}

// touchLock sets the locked file's access and modification times to now
// through its open descriptor, so the times land on the locked inode whatever
// the name holds by then.
func (t *texts) touchLock(lock *privatefile.Lock) error {
	if err := unix.Futimes(int(lock.File.Fd()), nil); err != nil {
		return fmt.Errorf("%s: update lock file times: %w", t.name, err)
	}
	return nil
}

// waitCreatedLock waits for the lock file name, which whichever process first
// needs it creates, so several processes can race to create it. An open that
// fails with ENOENT during that race is retried createRaceRetries times within
// the same deadline. A directory that is gone keeps failing and the error is
// returned.
func (t *texts) waitCreatedLock(ctx context.Context, r *os.Root, name string) (*privatefile.Lock, error) {
	for attempt := 0; ; attempt++ {
		lock, err := t.policy.WaitLock(ctx, r, name)
		if !errors.Is(err, fs.ErrNotExist) || attempt == createRaceRetries {
			return lock, err
		}
		if err := privatefile.Pause(ctx); err != nil {
			return nil, err
		}
	}
}

// lockWaitError classifies an error from a bounded lock wait. A wait that ended
// because the caller's own context ended reports ErrLockWaitAbandoned with the
// context's error, a wait that ran out its own bound reports busy, and any
// other error, such as an unsafe lock file, is returned as it is.
func (t *texts) lockWaitError(parent context.Context, err, busy error) error {
	waitEnded := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	switch parentErr := parent.Err(); {
	case waitEnded && parentErr != nil:
		return fmt.Errorf("%w: %w", t.lockAbandoned, parentErr)
	case waitEnded:
		return busy
	}
	return err
}
