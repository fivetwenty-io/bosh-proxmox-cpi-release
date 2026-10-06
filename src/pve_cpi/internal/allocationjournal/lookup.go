package allocationjournal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// InspectVM returns a read-only snapshot of an active VM generation. Terminal
// deleted/cleaned history is not active and returns found=false. Errors never
// prove absence. Use InspectVMContext to propagate a request's cancellation.
func (j *Journal) InspectVM(agentID string) (Record, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return j.InspectVMContext(ctx, agentID)
}

// InspectVMContext does not acquire allocation ownership or create lock files.
// It serializes with index writers and authority recovery, while ordinary
// record updates remain observable through their atomic replacement snapshots.
// AcquireVM must recheck the generation and caller intent before any mutation.
func (j *Journal) InspectVMContext(ctx context.Context, agentID string) (record Record, found bool, retErr error) {
	if !nonblank(agentID) {
		return Record{}, false, fmt.Errorf("journal: agent ID is required")
	}
	lock, err := j.readIndexLock(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer func() {
		retErr = errors.Join(retErr, lock.close())
		if retErr != nil {
			record = Record{}
			found = false
		}
	}()
	if err := j.checkAuthority(); err != nil {
		return Record{}, false, err
	}
	idx, err := j.readIndex()
	if err != nil {
		return Record{}, false, err
	}
	records, err := j.lookupRecords(ctx)
	if err != nil {
		return Record{}, false, err
	}
	if err := validateGenerationRecords(idx, records); err != nil {
		return Record{}, false, err
	}
	id := idx.ActiveVMs[pathKey(agentID)]
	if id == "" {
		return Record{}, false, nil
	}
	var r Record
	for candidateIndex := range records {
		candidate := records[candidateIndex]
		if candidate.ID == id {
			r = candidate
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	if r.Kind != "vm" || r.AgentID != agentID {
		return Record{}, false, fmt.Errorf("%w: indexed VM identity mismatch", ErrCorrupt)
	}
	if generationClosed(r.State) {
		return Record{}, false, nil
	}
	return r, true, nil
}

func (j *Journal) readIndexLock(ctx context.Context) (*fileLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openPrivate(j.root, "index.lock", os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open existing generation lock: %w", ErrReconciliationRequired, err)
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(err, f.Close())
		}
		if err = pause(ctx); err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
}

// HoldShared waits until no lifecycle holds the record lock of allocation id,
// and then holds that lock in shared mode until the returned release runs. A
// lifecycle's Acquire holds the lock exclusively from before its first step
// until its handle closes, so while the shared lock is held no lifecycle is
// partway through a step on the allocation, and a read made then sees the
// record and the resources it names as that lifecycle left them. Readers share
// the lock with each other, and an Acquire that arrives meanwhile waits for
// every reader to release it.
//
// The kernel drops a record lock when the process holding it exits, so a lock
// that a crashed call held is free at once. HoldShared waits only while a live
// process holds the lock, and it returns ctx's error when ctx ends first.
//
// HoldShared never writes a record or creates a file. When the lock file is
// missing, no lifecycle has ever held the allocation, so it returns at once
// with a release that does nothing.
func (j *Journal) HoldShared(ctx context.Context, id string) (func() error, error) {
	if !allocationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("journal: invalid allocation UUID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := j.openSharedLock(id)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return func() error { return nil }, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		held, err := trySharedLock(f)
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		if held {
			return (&fileLock{file: f}).close, nil
		}
		if err := pause(ctx); err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
}

// TryHoldShared takes the record lock of allocation id in shared mode when no
// lifecycle holds it, and never waits. held is false, with no error and no
// release, when a lifecycle holds the lock now. Otherwise the lock is held in
// shared mode until the returned release runs, with the same meaning as a
// lock HoldShared returns. Like HoldShared, it never writes a record or
// creates a file, and a missing lock file counts as free.
func (j *Journal) TryHoldShared(id string) (release func() error, held bool, err error) {
	if !allocationIDPattern.MatchString(id) {
		return nil, false, fmt.Errorf("journal: invalid allocation UUID")
	}
	f, err := j.openSharedLock(id)
	if err != nil {
		return nil, false, err
	}
	if f == nil {
		return func() error { return nil }, true, nil
	}
	held, err = trySharedLock(f)
	if err != nil || !held {
		return nil, false, errors.Join(err, f.Close())
	}
	return (&fileLock{file: f}).close, true, nil
}

// openSharedLock opens the record lock file of allocation id for a shared
// hold. It returns no file and no error when the lock file is missing, which
// means no lifecycle has ever held the allocation.
func (j *Journal) openSharedLock(id string) (*os.File, error) {
	f, err := openPrivate(j.root, id+".lock", os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return f, err
}

// trySharedLock tries once to lock f in shared mode. It reports false with no
// error when another process holds the lock exclusively.
func trySharedLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// InspectDiskToken returns the disk record whose disk token is token, and it
// reads no other record. A disk record's token is DiskCorrelationToken of its
// ID, and the record's file is named for that ID, so the file names alone
// show which record could hold the token. A corrupt record of another
// allocation therefore never fails the lookup, unlike List.
//
// found is false when no record file's name gives the token, or when the one
// that does holds a VM record, which carries no disk token. A directory that
// can't be listed, and a record file that gives the token but can't be read
// or doesn't validate, return an error, because neither shows that no record
// names the token. CreateDisk refuses a token that any record has ever held,
// and the token is a hash of the ID, so at most one record file gives it.
func (j *Journal) InspectDiskToken(token string) (Record, bool, error) {
	if !nonblank(token) {
		return Record{}, false, fmt.Errorf("journal: disk token is required")
	}
	dir, err := j.root.Open(".")
	if err != nil {
		return Record{}, false, err
	}
	entries, readErr := dir.ReadDir(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return Record{}, false, err
	}
	for _, entry := range entries {
		id, ok := strings.CutPrefix(entry.Name(), "allocation-")
		if !ok {
			continue
		}
		if id, ok = strings.CutSuffix(id, ".json"); !ok {
			continue
		}
		// A name whose ID isn't an allocation UUID can't hold a valid
		// record, because Inspect requires the record's ID to match its
		// file's name, so it can't hold this token either.
		candidate, err := DiskCorrelationToken(id)
		if err != nil || candidate != token {
			continue
		}
		r, err := j.Inspect(id)
		if err != nil {
			return Record{}, false, err
		}
		if r.Kind != "disk" {
			return Record{}, false, nil
		}
		return r, true, nil
	}
	return Record{}, false, nil
}

// Check cancellation between every bounded record read. Index writers cannot
// add generations during this scan; lifecycle writers may atomically replace
// records. A detected inode replacement fails closed and may be retried.
func (j *Journal) lookupRecords(ctx context.Context) ([]Record, error) {
	d, err := j.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	err = errors.Join(err, d.Close())
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(e.Name(), "allocation-") && strings.HasSuffix(e.Name(), ".json") {
			id := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "allocation-"), ".json")
			r, err := j.Inspect(id)
			if err != nil {
				return nil, err
			}
			records = append(records, r)
		}
	}
	return records, ctx.Err()
}
