package cpi

import (
	"context"
	"sync"
)

// rollbackCtxKey is the unexported key type for the rollback holder stored in
// a context. An unexported type prevents collisions with any other package's
// context keys.
type rollbackCtxKey struct{}

// rollbackHolder carries a stack of registered cleanup functions that fire at
// most once, in reverse registration order (last-registered first), like
// deferred cleanups. It is stored in a context by withRollbackHolder and
// accessed by RegisterRollback / fireRollback. A stack (rather than a single
// fn) lets independent participants register their own undo — e.g. create_vm
// registers VM teardown and the lb_register hook registers LB deregistration,
// so a post-hook failure unwinds both.
type rollbackHolder struct {
	mu    sync.Mutex
	fns   []func(context.Context)
	fired bool
}

// withRollbackHolder installs a fresh *rollbackHolder into ctx when one is not
// already present. If a holder already exists (e.g. a nested WrapHandler call),
// it is left unchanged to preserve the outer cleanup registration.
func withRollbackHolder(ctx context.Context) context.Context {
	if ctx.Value(rollbackCtxKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, rollbackCtxKey{}, &rollbackHolder{})
}

// RegisterRollback pushes fn onto the rollback stack of the rollbackHolder
// installed in ctx. On fireRollback the registered functions run in reverse
// order (last-registered first). When ctx carries no holder (bare context or
// unhooked dispatch) the call is a no-op.
//
// Callers should register a cleanup only after the resource it undoes actually
// exists. Registering before creation is harmless when the holder never fires
// (handlerErr != nil prevents fireRollback), but registering after ensures the
// closure captures a valid resource id.
func RegisterRollback(ctx context.Context, fn func(context.Context)) {
	h, _ := ctx.Value(rollbackCtxKey{}).(*rollbackHolder)
	if h == nil || fn == nil {
		return
	}
	h.mu.Lock()
	h.fns = append(h.fns, fn)
	h.mu.Unlock()
}

// fireRollback runs every registered cleanup exactly once, in reverse
// registration order. Subsequent calls are no-ops (idempotent). Cleanups run
// outside the mutex so a slow cleanup does not hold the lock; a panic in one
// cleanup does not prevent the rest from running.
//
// When ctx carries no holder, the stack is empty, or it has already fired,
// fireRollback is a no-op.
func fireRollback(ctx context.Context) {
	h, _ := ctx.Value(rollbackCtxKey{}).(*rollbackHolder)
	if h == nil {
		return
	}

	h.mu.Lock()
	if h.fired || len(h.fns) == 0 {
		h.mu.Unlock()
		return
	}
	fns := h.fns
	h.fns = nil
	h.fired = true
	h.mu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		runRollbackFn(ctx, fns[i])
	}
}

// runRollbackFn invokes one cleanup, recovering from a panic so a single
// misbehaving cleanup cannot abort the remaining unwinds.
func runRollbackFn(ctx context.Context, fn func(context.Context)) {
	defer func() { _ = recover() }()
	fn(ctx)
}
