package handlers_test

// log_correlation_test.go verifies handler log correlation: a production
// handler (has_vm) that derives its logger via deps.Log(ctx) picks up the
// per-request, span-correlated logger cmd/cpi's runCPI stores in ctx —
// instead of the fixed startup deps.Logger — so handler log lines carry
// trace_id/span_id/request_id once tracing is active.

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestHandleHasVM_LogCorrelation_TraceIDSpanIDRequestID reproduces the exact
// ctx shape cmd/cpi/main.go's runCPI builds (span attached first, then
// request_id/method, then the per-request logger derived from that ctx and
// stored back into it) and asserts a real handler's log line carries all
// three correlation fields.
func TestHandleHasVM_LogCorrelation_TraceIDSpanIDRequestID(t *testing.T) {
	t.Parallel()

	base, obs := log.NewObservedLogger(log.LevelDebug)

	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	const requestID = "corr-req-42"
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = log.WithRequestID(ctx, requestID)
	ctx = log.WithMethod(ctx, "has_vm")
	reqLogger := base.WithContext(ctx)
	ctx = log.IntoContext(ctx, reqLogger)

	// deps.Logger is deliberately left as a plain NopLogger-equivalent
	// (testDepsFoundVM's default): this test proves the ctx-carried logger
	// wins over deps.Logger, per Deps.Log's documented precedence, not that
	// deps.Logger itself happens to be correlated.
	deps := testDepsFoundVM(101, nil, nil, nil, &mockAgentService{})
	h := handlers.HandleHasVM(deps)

	if _, err := h.Handle(ctx, marshalArgs("101"), jsonrpc.Context{RequestID: requestID}); err != nil {
		t.Fatalf("HandleHasVM: unexpected error: %v", err)
	}

	entries := obs.All()
	if len(entries) == 0 {
		t.Fatal("expected at least one log entry from has_vm, got 0")
	}

	var found bool
	for _, e := range entries {
		if e.Attrs["trace_id"] == sc.TraceID().String() &&
			e.Attrs["span_id"] == sc.SpanID().String() &&
			e.Attrs["request_id"] == requestID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no has_vm log entry carried trace_id=%s span_id=%s request_id=%s; entries: %+v",
			sc.TraceID().String(), sc.SpanID().String(), requestID, entries)
	}
}
