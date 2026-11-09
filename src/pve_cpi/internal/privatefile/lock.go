package privatefile

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// Lock is an exclusive advisory lock held on an open private file.
type Lock struct{ File *os.File }

// TryLock takes the exclusive lock on name without waiting, creating the file
// when it is missing. held is false, with no error, when another holder has
// the lock now.
func (p Policy) TryLock(r *os.Root, name string) (*Lock, bool, error) {
	f, err := p.Open(r, name, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, false, f.Close()
	}
	if err != nil {
		return nil, false, errors.Join(err, f.Close())
	}
	return &Lock{File: f}, true, nil
}

// Close releases the lock and closes its file.
func (l *Lock) Close() error {
	return errors.Join(syscall.Flock(int(l.File.Fd()), syscall.LOCK_UN), l.File.Close())
}

// WaitLock polls TryLock until it holds the lock or ctx ends, so a caller
// bounds the wait with a context deadline.
func (p Policy) WaitLock(ctx context.Context, r *os.Root, name string) (*Lock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, ok, err := p.TryLock(r, name)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		if err := Pause(ctx); err != nil {
			return nil, err
		}
	}
}

// Pause sleeps for the interval between lock attempts, or until ctx ends.
func Pause(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
