package cpi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// panicHandler is a handler that panics with a nil pointer dereference,
// simulating the most common unhandled runtime error in a real handler.
type panicHandler struct{}

func (panicHandler) Handle(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	var p *string
	_ = *p // nil dereference → runtime panic
	return nil, nil
}

// panicStringHandler panics with a plain string value.
type panicStringHandler struct{}

func (panicStringHandler) Handle(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	panic("something went very wrong")
}

// TestDispatcher_HandlerPanic_ReturnsCloudError verifies that a nil-deref panic
// inside a handler does not crash the dispatcher. The dispatcher must recover
// and return a non-retriable CloudError whose message contains the method name
// and the request_id.
func TestDispatcher_HandlerPanic_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", panicHandler{})

	req := &jsonrpc.Request{
		Method:     "create_vm",
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "panic-req-001"},
		APIVersion: 2,
	}

	// Must not panic (if it does the test runner itself would catch it and fail).
	resp := d.Handle(context.Background(), req)

	if resp == nil {
		t.Fatal("Handle returned nil; expected a non-nil response after panic recovery")
	}
	if resp.Error == nil {
		t.Fatal("expected error response after handler panic; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError (TypeCloud)", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("panic-recovered error must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "create_vm") {
		t.Errorf("error message %q does not contain method name %q", resp.Error.Message, "create_vm")
	}
	if !strings.Contains(resp.Error.Message, "panic-req-001") {
		t.Errorf("error message %q does not contain request_id %q", resp.Error.Message, "panic-req-001")
	}
}

// TestDispatcher_HandlerPanicString_ReturnsCloudError verifies the same guarantee
// with a string panic value (not a runtime.Error).
func TestDispatcher_HandlerPanicString_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "delete_vm", panicStringHandler{})

	req := makeReqWithID("delete_vm", "panic-req-002")

	resp := d.Handle(context.Background(), req)

	if resp == nil {
		t.Fatal("Handle returned nil after string panic")
	}
	if resp.Error == nil {
		t.Fatal("expected error after string panic; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("string-panic error must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "delete_vm") {
		t.Errorf("error message %q missing method", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "panic-req-002") {
		t.Errorf("error message %q missing request_id", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "something went very wrong") {
		t.Errorf("error message %q missing panic value", resp.Error.Message)
	}
}

// TestDispatcher_NonPanickingHandler_Unchanged verifies that the recover path
// does not alter the response for handlers that return normally.
func TestDispatcher_NonPanickingHandler_Unchanged(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcher(nopLogger())
	want := map[string]string{"api_version": "2.0"}
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return want, nil
	}))

	resp := d.Handle(context.Background(), makeReq("info"))

	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error for non-panicking handler: %+v", resp.Error)
	}
	var got map[string]string
	if err := json.Unmarshal(resp.Result.(json.RawMessage), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["api_version"] != want["api_version"] {
		t.Errorf("result api_version = %q; want %q", got["api_version"], want["api_version"])
	}
}

// TestDispatcher_HandlerPanic_WithHooks verifies recovery works when middleware
// hooks are active (the panic occurs inside the wrapped handler).
func TestDispatcher_HandlerPanic_WithHooks(t *testing.T) {
	t.Parallel()

	hookFired := false
	hook := cpi.HookFunc{
		BeforeFn: func(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
			hookFired = true
			return ctx
		},
	}

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithHooks(hook))
	mustRegister(t, d, "has_vm", panicHandler{})

	resp := d.Handle(context.Background(), makeReqWithID("has_vm", "panic-hook-req-003"))

	if !hookFired {
		t.Error("Before hook did not fire before panic")
	}
	if resp == nil {
		t.Fatal("Handle returned nil after panic with hooks")
	}
	if resp.Error == nil {
		t.Fatal("expected error after panic with hooks; got nil")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("panic with hooks must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "has_vm") {
		t.Errorf("error message %q missing method", resp.Error.Message)
	}
}

// TestDispatcher_BeforeHookPanic_ReturnsCloudError verifies that a panic inside
// a Before hook is recovered by the dispatcher's defer. The Before hook fires
// before the inner handler, inside the WrapHandler closure, which is called by
// h.Handle at the same stack level the recover covers.
func TestDispatcher_BeforeHookPanic_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	hook := cpi.HookFunc{
		BeforeFn: func(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
			panic("before hook exploded")
		},
	}

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithHooks(hook))
	// Register a well-behaved handler; the panic fires before it is reached.
	mustRegister(t, d, "reboot_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, nil
	}))

	resp := d.Handle(context.Background(), makeReqWithID("reboot_vm", "hook-before-req-004"))

	if resp == nil {
		t.Fatal("Handle returned nil after Before hook panic")
	}
	if resp.Error == nil {
		t.Fatal("expected error after Before hook panic; got nil")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("Before hook panic must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "reboot_vm") {
		t.Errorf("error message %q missing method", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "hook-before-req-004") {
		t.Errorf("error message %q missing request_id", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "before hook exploded") {
		t.Errorf("error message %q missing panic value", resp.Error.Message)
	}
}

// TestDispatcher_AfterHookPanic_ReturnsCloudError verifies that a panic inside
// an After hook is also caught. After hooks run after the inner handler returns,
// still inside the WrapHandler closure covered by the top-level recover.
func TestDispatcher_AfterHookPanic_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	hook := cpi.HookFunc{
		AfterFn: func(_ context.Context, _ string, r any, e error) (any, error) {
			panic("after hook exploded")
		},
	}

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithHooks(hook))
	mustRegister(t, d, "set_vm_metadata", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, nil // handler succeeds; After hook panics
	}))

	resp := d.Handle(context.Background(), makeReqWithID("set_vm_metadata", "hook-after-req-005"))

	if resp == nil {
		t.Fatal("Handle returned nil after After hook panic")
	}
	if resp.Error == nil {
		t.Fatal("expected error after After hook panic; got nil")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("After hook panic must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "set_vm_metadata") {
		t.Errorf("error message %q missing method", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "hook-after-req-005") {
		t.Errorf("error message %q missing request_id", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "after hook exploded") {
		t.Errorf("error message %q missing panic value", resp.Error.Message)
	}
}

// TestDispatcher_NilRequest_ReturnsCloudError verifies that passing a nil
// *jsonrpc.Request to Handle does not crash via an unguarded nil dereference
// inside the recover defer. The dispatcher should return a typed CloudError.
func TestDispatcher_NilRequest_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcher(nopLogger())

	// Must not panic.
	resp := d.Handle(context.Background(), nil)

	if resp == nil {
		t.Fatal("Handle(nil req) returned nil; expected non-nil response")
	}
	if resp.Error == nil {
		t.Fatal("expected error for nil request; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("nil-request error must not be retriable")
	}
}

// makeReqWithID builds a *jsonrpc.Request with a specific request_id.
func makeReqWithID(method, requestID string) *jsonrpc.Request {
	return &jsonrpc.Request{
		Method:     method,
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: requestID},
		APIVersion: 2,
	}
}
