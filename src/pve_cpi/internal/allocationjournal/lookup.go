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
