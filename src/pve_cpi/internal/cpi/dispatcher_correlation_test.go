package cpi_test

// dispatcher_correlation_test.go verifies dispatcher-side trace correlation:
// Dispatcher.Handle's "dispatch" log line carries trace_id/span_id when ctx
// carries a valid OTel span, with no duplicated log keys — d.logger is the
// fixed process-startup logger (never derived from ctx via WithContext), so
// appending log.SpanFields(ctx) must add exactly one trace_id and one span_id
// key alongside the single method/request_id pair each line already attaches
// explicitly.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// keyCountRecord captures one slog record's message plus a per-key occurrence
// count. Unlike a map[string]any capture (which silently collapses a repeated
// key to its last-written value), this preserves enough information to prove
// a key was NOT duplicated.
type keyCountRecord struct {
	Message  string
	KeyCount map[string]int
}

// keyCountHandler is a minimal slog.Handler that records keyCountRecord for
// every Handle call. It ignores WithAttrs/WithGroup (Dispatcher never calls
// them on the logger it's constructed with) — safe here to keep this test
// handler minimal without also implementing full slog.Handler chaining.
type keyCountHandler struct {
	mu      sync.Mutex
	records []keyCountRecord
}

func (h *keyCountHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *keyCountHandler) Handle(_ context.Context, r slog.Record) error {
	counts := make(map[string]int, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		counts[a.Key]++
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, keyCountRecord{Message: r.Message, KeyCount: counts})
	h.mu.Unlock()
	return nil
}

func (h *keyCountHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *keyCountHandler) WithGroup(string) slog.Handler      { return h }

func (h *keyCountHandler) all() []keyCountRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]keyCountRecord, len(h.records))
	copy(out, h.records)
	return out
}

// TestHandle_DispatchLog_TraceCorrelation_NoDuplicateKeys drives one
// successful "info" dispatch through a ctx carrying a valid, sampled
// SpanContext and asserts the resulting "dispatch" log record carries
// trace_id/span_id plus exactly one occurrence each of method/request_id
// (i.e., SpanFields never duplicates the fields dispatcher.go attaches
// explicitly).
func TestHandle_DispatchLog_TraceCorrelation_NoDuplicateKeys(t *testing.T) {
	t.Parallel()

	h := &keyCountHandler{}
	testLogger := &log.Logger{Logger: slog.New(h)}

	d := cpi.NewDispatcher(testLogger)
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]any{"ok": true}, nil
	}))

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
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	req := &jsonrpc.Request{
		Method:     "info",
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "corr-req-1"},
		APIVersion: 2,
	}
	resp := d.Handle(ctx, req)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}

	var dispatchRecord *keyCountRecord
	for _, rec := range h.all() {
		if rec.Message == "dispatch" {
			r := rec
			dispatchRecord = &r
			break
		}
	}
	if dispatchRecord == nil {
		t.Fatalf("no %q log record captured; records: %+v", "dispatch", h.all())
	}

	for _, key := range []string{"method", "request_id", "trace_id", "span_id"} {
		if got := dispatchRecord.KeyCount[key]; got != 1 {
			t.Errorf("key %q appears %d times in dispatch record, want exactly 1 (record: %+v)",
				key, got, dispatchRecord.KeyCount)
		}
	}
}

// TestHandle_DispatchLog_NoSpan_NoTraceFields verifies a plain ctx (no OTel
// span attached) produces a "dispatch" record with zero trace_id/span_id keys
// — byte-identical field set to prior releases when tracing is inactive.
func TestHandle_DispatchLog_NoSpan_NoTraceFields(t *testing.T) {
	t.Parallel()

	h := &keyCountHandler{}
	testLogger := &log.Logger{Logger: slog.New(h)}

	d := cpi.NewDispatcher(testLogger)
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]any{"ok": true}, nil
	}))

	req := &jsonrpc.Request{
		Method:     "info",
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "no-span-req-1"},
		APIVersion: 2,
	}
	resp := d.Handle(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}

	var dispatchRecord *keyCountRecord
	for _, rec := range h.all() {
		if rec.Message == "dispatch" {
			r := rec
			dispatchRecord = &r
			break
		}
	}
	if dispatchRecord == nil {
		t.Fatalf("no %q log record captured; records: %+v", "dispatch", h.all())
	}

	for _, key := range []string{"trace_id", "span_id"} {
		if got := dispatchRecord.KeyCount[key]; got != 0 {
			t.Errorf("key %q appears %d times with no span in ctx, want 0", key, got)
		}
	}
	if got := dispatchRecord.KeyCount["request_id"]; got != 1 {
		t.Errorf("request_id appears %d times, want exactly 1", got)
	}
}
