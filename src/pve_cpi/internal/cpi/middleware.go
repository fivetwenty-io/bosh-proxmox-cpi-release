package cpi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// Hook is a dispatch middleware callback pair wrapped around a single CPI
// handler invocation. Before runs prior to the wrapped handler; After runs
// after it with the handler's result and error.
//
// Before returns a (possibly augmented) context that is threaded into both the
// wrapped handler and the matching After call. This lets a hook carry per-call
// state — e.g. a start timestamp for duration measurement — without storing it
// on the hook value, so a single Hook instance is safe for concurrent dispatch.
//
// Contract: After may replace the result or the error, but it must not turn a
// non-nil error into nil, and must not flip a retriable classification, without
// an explicit documented reason in the hook implementation. Built-in hooks
// observe only and return the result and error unchanged.
type Hook interface {
	Before(ctx context.Context, method string, args []json.RawMessage, reqCtx jsonrpc.Context) context.Context
	After(ctx context.Context, method string, result any, err error) (any, error)
}

// HookFunc adapts plain functions to the Hook interface. A nil field is a
// no-op: BeforeFn nil returns ctx unchanged; AfterFn nil returns result and err
// unchanged.
//
// Deliberate test seam: no production Hook is built this way (main.go wires
// concrete Hook implementations directly), but HookFunc lets tests construct
// ad hoc Before/After behavior inline without a named type per test case.
type HookFunc struct {
	BeforeFn func(context.Context, string, []json.RawMessage, jsonrpc.Context) context.Context
	AfterFn  func(context.Context, string, any, error) (any, error)
}

// Before implements Hook by calling BeforeFn when set.
func (h HookFunc) Before(ctx context.Context, method string, args []json.RawMessage, reqCtx jsonrpc.Context) context.Context {
	if h.BeforeFn == nil {
		return ctx
	}
	return h.BeforeFn(ctx, method, args, reqCtx)
}

// After implements Hook by calling AfterFn when set.
func (h HookFunc) After(ctx context.Context, method string, result any, err error) (any, error) {
	if h.AfterFn == nil {
		return result, err
	}
	return h.AfterFn(ctx, method, result, err)
}

var _ Hook = HookFunc{}

// WrapHandler returns inner wrapped by hooks. With an empty or nil hooks slice
// it returns inner unchanged — no wrapper is allocated and the call stack is
// identical to an unhooked dispatch. Before callbacks fire in slice order
// (outer-to-inner); After callbacks fire in reverse (inner-to-outer), so hooks
// nest like conventional middleware.
//
// For create_vm specifically, WrapHandler installs a rollback holder into the
// context before invoking the handler, then checks whether a post-hook (After)
// introduced a new error after the handler itself succeeded. When
// handlerErr==nil && err!=nil (hook turned success into failure), fireRollback
// is called on a cancellation-detached context so cleanup completes even when
// the caller's context has been cancelled. This prevents an orphaned VM when a
// post-hook (e.g. stemcell provenance, HA membership) fails after the VM is
// already committed — without double-cleanup with the handler's own defer.
func WrapHandler(method string, inner Handler, hooks []Hook) Handler {
	if len(hooks) == 0 {
		return inner
	}
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		ctx = withRollbackHolder(ctx)
		for _, h := range hooks {
			ctx = h.Before(ctx, method, args, reqCtx)
		}
		result, handlerErr := inner.Handle(ctx, args, reqCtx)
		err := handlerErr
		for i := len(hooks) - 1; i >= 0; i-- {
			result, err = hooks[i].After(ctx, method, result, err)
		}
		if method == "create_vm" && handlerErr == nil && err != nil {
			// Detach from the caller's cancellation so a Director-cancelled call
			// still cleans up, but apply a bounded deadline so a wedged PVE
			// during rollback cannot hang the dispatch indefinitely.
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackCleanupTimeout)
			fireRollback(rbCtx)
			cancel()
		}
		return result, err
	})
}

// rollbackCleanupTimeout bounds the post-hook rollback cleanup so a wedged PVE
// API cannot hang dispatch indefinitely while detached from the caller context.
const rollbackCleanupTimeout = 2 * time.Minute
