package log_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// validTestSpanContext returns a manufactured, valid, sampled SpanContext with
// deterministic non-zero TraceID/SpanID so tests can assert on exact hex values.
func validTestSpanContext() trace.SpanContext {
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		panic(err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		panic(err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

// TestWithContext_ValidSpan_AttachesTraceAndSpanID verifies a ctx carrying a
// valid, recording SpanContext yields log records with trace_id/span_id
// fields matching the SpanContext's hex-encoded IDs.
func TestWithContext_ValidSpan_AttachesTraceAndSpanID(t *testing.T) {
	t.Parallel()

	base, obs := log.NewObservedLogger(log.LevelInfo)
	sc := validTestSpanContext()
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	l := base.WithContext(ctx)
	l.Info("traced message")

	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]

	gotTraceID, ok := entry.Attrs["trace_id"]
	if !ok {
		t.Fatal("expected trace_id attr, missing")
	}
	if gotTraceID != sc.TraceID().String() {
		t.Errorf("trace_id = %v, want %v", gotTraceID, sc.TraceID().String())
	}

	gotSpanID, ok := entry.Attrs["span_id"]
	if !ok {
		t.Fatal("expected span_id attr, missing")
	}
	if gotSpanID != sc.SpanID().String() {
		t.Errorf("span_id = %v, want %v", gotSpanID, sc.SpanID().String())
	}
}

// TestWithContext_PlainContext_NoTraceFields verifies a plain context (no
// span attached) produces zero trace_id/span_id fields — no behavior change
// when tracing is inactive.
func TestWithContext_PlainContext_NoTraceFields(t *testing.T) {
	t.Parallel()

	base, obs := log.NewObservedLogger(log.LevelInfo)
	ctx := context.Background()

	l := base.WithContext(ctx)
	l.Info("plain message")

	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry := entries[0]

	if v, ok := entry.Attrs["trace_id"]; ok {
		t.Errorf("expected no trace_id attr on plain ctx, got %v", v)
	}
	if v, ok := entry.Attrs["span_id"]; ok {
		t.Errorf("expected no span_id attr on plain ctx, got %v", v)
	}
}

// TestWithContext_InvalidSpanContext_NoTraceFields verifies a ctx carrying an
// invalid SpanContext (zero SpanID with an otherwise-valid TraceID, and the
// fully-zero-value SpanContext) attaches no trace_id/span_id fields.
func TestWithContext_InvalidSpanContext_NoTraceFields(t *testing.T) {
	t.Parallel()

	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}

	cases := map[string]trace.SpanContext{
		"zero value": trace.SpanContext{},
		"trace id only, zero span id": trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID,
			// SpanID intentionally left zero-value: makes the SpanContext invalid.
		}),
	}

	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			base, obs := log.NewObservedLogger(log.LevelInfo)
			ctx := trace.ContextWithSpanContext(context.Background(), sc)

			l := base.WithContext(ctx)
			l.Info("invalid span message")

			entries := obs.All()
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			entry := entries[0]

			if v, ok := entry.Attrs["trace_id"]; ok {
				t.Errorf("expected no trace_id attr for invalid SpanContext, got %v", v)
			}
			if v, ok := entry.Attrs["span_id"]; ok {
				t.Errorf("expected no span_id attr for invalid SpanContext, got %v", v)
			}
		})
	}
}
