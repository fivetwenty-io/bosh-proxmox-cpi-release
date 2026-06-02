package cpi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func TestNewDispatcherWithOptions_NoOptsBehavesLikeNewDispatcher(t *testing.T) {
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger())
	// info is pre-registered as a NotImplemented placeholder; dispatching it
	// returns an error response — identical to NewDispatcher.
	resp := d.Handle(context.Background(), &jsonrpc.Request{
		Method:  "info",
		Context: jsonrpc.Context{RequestID: "r1"},
	})
	if resp.Error == nil {
		t.Error("expected NotImplemented error for an un-overridden info handler")
	}
}

func TestNewDispatcherWithOptions_HookFiresPerDispatch(t *testing.T) {
	before, after := 0, 0
	hook := cpi.HookFunc{
		BeforeFn: func(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
			before++
			return ctx
		},
		AfterFn: func(_ context.Context, _ string, r any, e error) (any, error) {
			after++
			return r, e
		},
	}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithHooks(hook))
	mustRegister(t, d, "info", cpi.HandlerFunc(func(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) {
		return map[string]any{"api_version": 2}, nil
	}))

	resp := d.Handle(context.Background(), &jsonrpc.Request{
		Method:  "info",
		Context: jsonrpc.Context{RequestID: "r1"},
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if before != 1 || after != 1 {
		t.Errorf("hook fired before=%d after=%d; want 1/1", before, after)
	}
}
