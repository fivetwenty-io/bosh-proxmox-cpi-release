package cpi_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// recordedDuration captures one WithDurationRecorder invocation for assertion.
type recordedDuration struct {
	ctx        context.Context
	method     string
	outcome    string
	durationMs float64
}

// durationSpy is a concurrency-safe cpi.WithDurationRecorder callback that
// appends every call it receives, for tests to inspect afterward.
type durationSpy struct {
	mu    sync.Mutex
	calls []recordedDuration
}

func (s *durationSpy) record(ctx context.Context, method, outcome string, durationMs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedDuration{ctx: ctx, method: method, outcome: outcome, durationMs: durationMs})
}

func (s *durationSpy) snapshot() []recordedDuration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedDuration, len(s.calls))
	copy(out, s.calls)
	return out
}

// --------------------------------------------------------------------------
// TestDispatcher_DurationRecorder_Success
// --------------------------------------------------------------------------

// TestDispatcher_DurationRecorder_Success verifies a successful dispatch calls
// the recorder exactly once with outcome "success" and a positive duration.
func TestDispatcher_DurationRecorder_Success(t *testing.T) {
	t.Parallel()
	spy := &durationSpy{}
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithDurationRecorder(spy.record))
	mustRegister(t, d, "has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]bool{"exists": true}, nil
	}))

	resp := d.Handle(context.Background(), makeReq("has_vm"))
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorder called %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].method != "has_vm" {
		t.Errorf("method = %q, want %q", calls[0].method, "has_vm")
	}
	if calls[0].outcome != "success" {
		t.Errorf("outcome = %q, want %q", calls[0].outcome, "success")
	}
	if calls[0].durationMs < 0 {
		t.Errorf("durationMs = %v, want >= 0", calls[0].durationMs)
	}
}

// --------------------------------------------------------------------------
// TestDispatcher_DurationRecorder_HandlerError
// --------------------------------------------------------------------------

// TestDispatcher_DurationRecorder_HandlerError verifies a handler error calls
// the recorder exactly once with outcome "error".
func TestDispatcher_DurationRecorder_HandlerError(t *testing.T) {
	t.Parallel()
	spy := &durationSpy{}
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithDurationRecorder(spy.record))
	mustRegister(t, d, "has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, errBoom
	}))

	resp := d.Handle(context.Background(), makeReq("has_vm"))
	if resp.Error == nil {
		t.Fatal("expected error response, got success")
	}

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorder called %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].outcome != "error" {
		t.Errorf("outcome = %q, want %q", calls[0].outcome, "error")
	}
}

// --------------------------------------------------------------------------
// TestDispatcher_DurationRecorder_MarshalError
// --------------------------------------------------------------------------

// TestDispatcher_DurationRecorder_MarshalError is the core regression test for
// the fix this file exists to verify: a handler that succeeds (nil error) but
// returns a result json.Marshal rejects must record outcome "marshal_error",
// not "success". Before this fix, a hook wrapping the handler call recorded
// "success" because the marshal step runs after the wrapped handler (and any
// hooks around it) already returned.
func TestDispatcher_DurationRecorder_MarshalError(t *testing.T) {
	t.Parallel()
	spy := &durationSpy{}
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithDurationRecorder(spy.record))
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return make(chan int), nil // not JSON-serialisable
	}))

	resp := d.Handle(context.Background(), makeReq("info"))
	if resp.Error == nil {
		t.Fatal("expected CloudError for non-marshalable result, got success")
	}

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorder called %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].outcome != "marshal_error" {
		t.Errorf("outcome = %q, want %q", calls[0].outcome, "marshal_error")
	}
}

// --------------------------------------------------------------------------
// TestDispatcher_DurationRecorder_Timeout
// --------------------------------------------------------------------------

// TestDispatcher_DurationRecorder_Timeout verifies that when the per-method
// deadline fires and Handle rewrites the response into a retriable timeout,
// the recorder still receives outcome "error" — not a distinct "timeout"
// value. This preserves the outcome the wrapped handler's hooks observed
// before Handle's post-handler timeout rewrite ran; the duration metric's
// outcome vocabulary is success/error/marshal_error only, and "timeout" stays
// a dispatch-log-only distinction.
func TestDispatcher_DurationRecorder_Timeout(t *testing.T) {
	t.Parallel()
	spy := &durationSpy{}
	resolver := func(method string) time.Duration {
		if method == "create_vm" {
			return 20 * time.Millisecond
		}
		return 0
	}
	d := cpi.NewDispatcherWithOptions(nopLogger(),
		cpi.WithMethodTimeouts(resolver),
		cpi.WithDurationRecorder(spy.record),
	)
	observed := make(chan struct{})
	mustRegister(t, d, "create_vm", blockingHandler{observed: observed})

	resp := d.Handle(context.Background(), req("create_vm"))
	if resp.Error == nil || !resp.Error.OkToRetry {
		t.Fatalf("expected retriable timeout error, got: %+v", resp)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("handler did not observe context cancellation")
	}

	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("recorder called %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].outcome != "error" {
		t.Errorf("outcome = %q, want %q (timeout is recorded as error, not a distinct outcome)", calls[0].outcome, "error")
	}
}

// --------------------------------------------------------------------------
// TestDispatcher_NilDurationRecorder_NoPanic
// --------------------------------------------------------------------------

// TestDispatcher_NilDurationRecorder_NoPanic verifies that a dispatcher built
// without WithDurationRecorder (the default, nil recorder) dispatches
// normally across every outcome branch without panicking.
func TestDispatcher_NilDurationRecorder_NoPanic(t *testing.T) {
	t.Parallel()
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]bool{"exists": true}, nil
	}))
	mustRegister(t, d, "detach_disk", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, errBoom
	}))
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return make(chan int), nil
	}))

	if resp := d.Handle(context.Background(), makeReq("has_vm")); resp.Error != nil {
		t.Errorf("has_vm: unexpected error: %+v", resp.Error)
	}
	if resp := d.Handle(context.Background(), makeReq("detach_disk")); resp.Error == nil {
		t.Error("detach_disk: expected error, got success")
	}
	if resp := d.Handle(context.Background(), makeReq("info")); resp.Error == nil {
		t.Error("info: expected marshal CloudError, got success")
	}
}

// --------------------------------------------------------------------------
// TestDispatcher_DurationRecorder_ReceivesRequestCtx
// --------------------------------------------------------------------------

// durationCtxKey is a private key used only by
// TestDispatcher_DurationRecorder_ReceivesRequestCtx to stash a marker value
// on the ctx passed into Handle, so the test can prove the recorder receives
// that same ctx (not context.Background() or some other derived value).
type durationCtxKey struct{}

// TestDispatcher_DurationRecorder_ReceivesRequestCtx verifies the recorder is
// called with Handle's own request ctx (the one Handle was invoked with),
// not callCtx (the possibly timeout-wrapped context passed to the handler)
// or a detached context.Background(). It stashes a marker value on the
// request ctx via context.WithValue and asserts the recorder observes it,
// covering both a plain success dispatch and the marshal_error branch (whose
// ctx is Handle's request ctx even though the handler itself already
// returned).
func TestDispatcher_DurationRecorder_ReceivesRequestCtx(t *testing.T) {
	t.Parallel()
	spy := &durationSpy{}
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithDurationRecorder(spy.record))
	mustRegister(t, d, "has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]bool{"exists": true}, nil
	}))
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return make(chan int), nil // not JSON-serialisable -> marshal_error branch
	}))

	marked := context.WithValue(context.Background(), durationCtxKey{}, "marker")
	d.Handle(marked, makeReq("has_vm"))
	d.Handle(marked, makeReq("info"))

	calls := spy.snapshot()
	if len(calls) != 2 {
		t.Fatalf("recorder called %d times, want 2: %+v", len(calls), calls)
	}
	for i, call := range calls {
		if call.ctx == nil {
			t.Fatalf("call %d (%s): ctx is nil", i, call.outcome)
		}
		if v, _ := call.ctx.Value(durationCtxKey{}).(string); v != "marker" {
			t.Errorf("call %d (%s): ctx.Value(durationCtxKey{}) = %q, want %q", i, call.outcome, v, "marker")
		}
	}
}

// errBoom is a plain sentinel error for handler-error test paths.
var errBoom = &plainError{"boom"}

type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }
