package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// handlerWithRollback is a Handler that "succeeds" and registers a rollback
// function that increments a shared counter. Used to verify cleanup fires.
func handlerWithRollback(counter *int) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		cpi.RegisterRollback(ctx, func(_ context.Context) {
			*counter++
		})
		return "ok", nil
	})
}

// errHook returns an AfterFn that injects errSentinel regardless of the
// incoming error, for the named method only.
func errHookForMethod(method string, errSentinel error) cpi.HookFunc {
	return cpi.HookFunc{
		AfterFn: func(_ context.Context, m string, r any, e error) (any, error) {
			if m == method {
				return r, errSentinel
			}
			return r, e
		},
	}
}

// TestRollback_PostHookErrorTriggersCleanup: handler succeeds and registers
// rollback; a post-hook injects an error for create_vm. Cleanup must fire
// exactly once and the error must propagate to the caller.
func TestRollback_PostHookErrorTriggersCleanup(t *testing.T) {
	hookErr := errors.New("post-hook failure")
	counter := 0

	wrapped := cpi.WrapHandler(
		"create_vm",
		handlerWithRollback(&counter),
		[]cpi.Hook{errHookForMethod("create_vm", hookErr)},
	)

	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if !errors.Is(err, hookErr) {
		t.Errorf("expected hook error to propagate; got %v", err)
	}
	if counter != 1 {
		t.Errorf("expected cleanup called once; called %d times", counter)
	}
}

// TestRollback_HandlerErrorNoDoubleFire: handler returns an error and also
// registers rollback; hook passes the error through. fireRollback must NOT
// fire because handlerErr != nil (the handler's own defer owns cleanup).
func TestRollback_HandlerErrorNoDoubleFire(t *testing.T) {
	handlerErr := errors.New("handler failed")
	counter := 0

	handlerWithErr := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		cpi.RegisterRollback(ctx, func(_ context.Context) {
			counter++
		})
		return nil, handlerErr
	})
	// Hook passes errors through unchanged.
	passthroughHook := cpi.HookFunc{}

	wrapped := cpi.WrapHandler("create_vm", handlerWithErr, []cpi.Hook{passthroughHook})
	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if !errors.Is(err, handlerErr) {
		t.Errorf("expected handler error to propagate; got %v", err)
	}
	if counter != 0 {
		t.Errorf("expected no cleanup call (handler owns it); called %d times", counter)
	}
}

// TestRollback_SuccessNoCleanup: handler succeeds, hooks pass through (no
// error injected). Cleanup must not be called.
func TestRollback_SuccessNoCleanup(t *testing.T) {
	counter := 0

	wrapped := cpi.WrapHandler(
		"create_vm",
		handlerWithRollback(&counter),
		[]cpi.Hook{cpi.HookFunc{}},
	)

	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter != 0 {
		t.Errorf("expected no cleanup on success; called %d times", counter)
	}
}

// TestRollback_NonCreateVMUnaffected: method is "delete_vm"; a hook injects an
// error. Rollback must NOT fire — the guard is create_vm-only.
func TestRollback_NonCreateVMUnaffected(t *testing.T) {
	hookErr := errors.New("delete hook error")
	counter := 0

	wrapped := cpi.WrapHandler(
		"delete_vm",
		handlerWithRollback(&counter),
		[]cpi.Hook{errHookForMethod("delete_vm", hookErr)},
	)

	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if !errors.Is(err, hookErr) {
		t.Errorf("expected hook error to propagate; got %v", err)
	}
	if counter != 0 {
		t.Errorf("expected no rollback for delete_vm; called %d times", counter)
	}
}

// TestRollback_FireOnceIdempotent: two After hooks both return an error for
// create_vm. Despite both hooks failing, the cleanup function must fire exactly
// once (fireRollback is idempotent).
func TestRollback_FireOnceIdempotent(t *testing.T) {
	errA := errors.New("hook-a error")
	errB := errors.New("hook-b error")
	counter := 0

	hookA := cpi.HookFunc{
		AfterFn: func(_ context.Context, _ string, r any, _ error) (any, error) {
			return r, errA
		},
	}
	hookB := cpi.HookFunc{
		AfterFn: func(_ context.Context, _ string, r any, _ error) (any, error) {
			return r, errB
		},
	}

	wrapped := cpi.WrapHandler(
		"create_vm",
		handlerWithRollback(&counter),
		[]cpi.Hook{hookA, hookB},
	)

	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected error from hooks; got nil")
	}
	if counter != 1 {
		t.Errorf("expected cleanup called exactly once; called %d times", counter)
	}
}

// TestRollback_NoHolderNoPanic: RegisterRollback on a bare context (no holder
// installed by withRollbackHolder) must be a no-op and must not panic.
func TestRollback_NoHolderNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RegisterRollback panicked on bare context: %v", r)
		}
	}()
	// No WrapHandler, no withRollbackHolder — bare context.
	cpi.RegisterRollback(context.Background(), func(_ context.Context) {
		t.Error("cleanup should never be called on bare context")
	})
}

// TestRollback_ConcurrentDispatchIsolation proves each WrapHandler dispatch
// gets its own per-call rollback holder: 100 parallel post-hook-fail dispatches
// must each fire their own cleanup exactly once, with no cross-talk. Run under
// -race to catch holder sharing.
func TestRollback_ConcurrentDispatchIsolation(t *testing.T) {
	t.Parallel()
	const n = 100
	hookErr := errors.New("post-hook failure")
	counters := make([]int32, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			wrapped := cpi.WrapHandler(
				"create_vm",
				cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
					cpi.RegisterRollback(ctx, func(_ context.Context) {
						atomic.AddInt32(&counters[idx], 1)
					})
					return "ok", nil
				}),
				[]cpi.Hook{errHookForMethod("create_vm", hookErr)},
			)
			_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
			if !errors.Is(err, hookErr) {
				t.Errorf("dispatch %d: want hookErr, got %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if got := atomic.LoadInt32(&counters[i]); got != 1 {
			t.Errorf("dispatch %d cleanup fired %d times; want exactly 1", i, got)
		}
	}
}

// TestRollback_NestedHolderPreservesOuter verifies withRollbackHolder's
// already-present branch: a nested WrapHandler must not replace the outer
// holder, so the outer registration still drives cleanup.
func TestRollback_NestedHolderPreservesOuter(t *testing.T) {
	hookErr := errors.New("post-hook failure")
	count := 0
	inner := cpi.WrapHandler(
		"create_vm",
		cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			cpi.RegisterRollback(ctx, func(_ context.Context) { count++ })
			return "ok", nil
		}),
		[]cpi.Hook{cpi.HookFunc{}}, // inner no-op hook so a holder is installed
	)
	outer := cpi.WrapHandler(
		"create_vm",
		inner,
		[]cpi.Hook{errHookForMethod("create_vm", hookErr)},
	)
	_, err := outer.Handle(context.Background(), nil, jsonrpc.Context{})
	if !errors.Is(err, hookErr) {
		t.Fatalf("want hookErr, got %v", err)
	}
	if count != 1 {
		t.Errorf("nested holder: cleanup fired %d times; want 1", count)
	}
}

// TestRollback_StackFiresAllInReverse proves multiple registered cleanups all
// fire on a post-hook failure, in reverse registration order (last-registered
// first). This is what lets the lb_register hook unwind its LB registration
// alongside create_vm's VM teardown.
func TestRollback_StackFiresAllInReverse(t *testing.T) {
	hookErr := errors.New("post-hook failure")
	var order []string
	wrapped := cpi.WrapHandler(
		"create_vm",
		cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			cpi.RegisterRollback(ctx, func(_ context.Context) { order = append(order, "first") })
			cpi.RegisterRollback(ctx, func(_ context.Context) { order = append(order, "second") })
			return "ok", nil
		}),
		[]cpi.Hook{errHookForMethod("create_vm", hookErr)},
	)
	if _, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{}); !errors.Is(err, hookErr) {
		t.Fatalf("want hookErr, got %v", err)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("cleanups must fire last-registered-first; got %v", order)
	}
}
