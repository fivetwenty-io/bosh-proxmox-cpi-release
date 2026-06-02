package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// ptrHandler is a pointer-typed Handler so tests can assert WrapHandler returns
// the same instance (HandlerFunc is a func and not comparable with ==).
type ptrHandler struct{ calls int }

func (h *ptrHandler) Handle(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) {
	h.calls++
	return "ok", nil
}

func TestWrapHandler_EmptyReturnsSameHandler(t *testing.T) {
	inner := &ptrHandler{}
	if got := cpi.WrapHandler("create_vm", inner, nil); got != cpi.Handler(inner) {
		t.Error("nil hooks must return the inner handler unchanged (no wrapper allocated)")
	}
	if got := cpi.WrapHandler("create_vm", inner, []cpi.Hook{}); got != cpi.Handler(inner) {
		t.Error("empty hooks must return the inner handler unchanged")
	}
}

func TestWrapHandler_CallOrder(t *testing.T) {
	var order []string
	mkHook := func(name string) cpi.Hook {
		return cpi.HookFunc{
			BeforeFn: func(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
				order = append(order, "before:"+name)
				return ctx
			},
			AfterFn: func(_ context.Context, _ string, r any, e error) (any, error) {
				order = append(order, "after:"+name)
				return r, e
			},
		}
	}
	inner := cpi.HandlerFunc(func(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) {
		order = append(order, "handler")
		return "ok", nil
	})

	wrapped := cpi.WrapHandler("create_vm", inner, []cpi.Hook{mkHook("a"), mkHook("b")})
	if _, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"before:a", "before:b", "handler", "after:b", "after:a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v; want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q; want %q", i, order[i], want[i])
		}
	}
}

func TestWrapHandler_AfterReplacesError(t *testing.T) {
	sentinel := errors.New("replaced by hook")
	hook := cpi.HookFunc{
		AfterFn: func(_ context.Context, _ string, r any, _ error) (any, error) {
			return r, sentinel
		},
	}
	inner := cpi.HandlerFunc(func(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) {
		return "ok", nil
	})
	wrapped := cpi.WrapHandler("create_vm", inner, []cpi.Hook{hook})
	_, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{})
	if !errors.Is(err, sentinel) {
		t.Errorf("After error replacement not propagated; got %v", err)
	}
}

func TestWrapHandler_BeforeContextThreaded(t *testing.T) {
	type ctxKey struct{}
	hook := cpi.HookFunc{
		BeforeFn: func(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
			return context.WithValue(ctx, ctxKey{}, "v")
		},
		AfterFn: func(ctx context.Context, _ string, r any, e error) (any, error) {
			if ctx.Value(ctxKey{}) != "v" {
				t.Error("After did not receive the context returned by Before")
			}
			return r, e
		},
	}
	var sawInHandler any
	inner := cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		sawInHandler = ctx.Value(ctxKey{})
		return nil, nil
	})
	wrapped := cpi.WrapHandler("create_vm", inner, []cpi.Hook{hook})
	if _, err := wrapped.Handle(context.Background(), nil, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawInHandler != "v" {
		t.Errorf("handler did not see Before's context value; got %v", sawInHandler)
	}
}
