package handlers

import (
	"context"
	"sync"
)

// nodeInflightRegistry holds one buffered channel per node name. The channel
// acts as a counting semaphore: each token in the buffer represents one
// outstanding mutating PVE call slot. When the buffer is full, acquire blocks
// until a caller releases a slot or the context is cancelled.
//
// The registry is process-scoped: semaphore sizes are fixed on first creation
// for a given node. To resize, restart the CPI process (matching PVE session
// semantics). A limit of 0 or negative means unlimited: no channel is created
// and acquire returns immediately with a no-op release.
type nodeInflightRegistry struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

// NewInflightRegistry returns an empty per-node in-flight registry. main.go
// constructs one and injects it via Deps.Inflight so all handlers built from
// the same Deps share it; tests construct local instances for isolation.
func NewInflightRegistry() *nodeInflightRegistry { //nolint:revive // unexported-return: deliberate — the type stays internal, the constructor is the only cross-package surface
	return &nodeInflightRegistry{m: map[string]chan struct{}{}}
}

// acquire acquires one slot for node under the given limit. When limit <= 0,
// no gating is applied: the registry is not consulted and a no-op release
// function is returned immediately. This preserves byte-identical behavior for
// the default unlimited configuration. A nil receiver behaves the same as
// limit <= 0 (unlimited), so Deps literals that omit Inflight stay safe.
//
// When limit > 0, a buffered channel of size limit is lazily created for node
// on first call. Subsequent calls with a different limit for the same node reuse
// the original channel (the limit is process-stable). The function blocks until
// either a slot becomes available or ctx is cancelled.
//
// The returned release function must be called exactly once, typically via
// defer. It is safe to call release multiple times: a sync.Once guard ensures
// the slot is returned to the semaphore at most once, preventing a double-free
// that would inflate available capacity.
//
// Error cases:
//   - ctx already cancelled on entry: returns ctx.Err() immediately.
//   - ctx cancelled while blocked: returns ctx.Err() with no slot consumed.
//   - limit <= 0: always returns (noop, nil).
func (r *nodeInflightRegistry) acquire(ctx context.Context, node string, limit int) (release func(), err error) {
	noop := func() {}

	if r == nil || limit <= 0 {
		return noop, nil
	}

	ch := r.getOrCreate(node, limit)

	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-ch })
		}, nil
	case <-ctx.Done():
		return noop, ctx.Err()
	}
}

// getOrCreate returns the semaphore channel for node, creating one of the given
// size when it does not yet exist. The size is fixed at creation: a later
// acquire call with a different limit for the same node reuses the original
// channel unchanged.
func (r *nodeInflightRegistry) getOrCreate(node string, size int) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.m[node]; ok {
		return ch
	}
	ch := make(chan struct{}, size)
	r.m[node] = ch
	return ch
}
