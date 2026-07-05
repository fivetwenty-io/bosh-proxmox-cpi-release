package main

// otel_wiring_test.go verifies the OTel wiring added to runCPI/dispatchOne/
// runWithArgs:
//   - one root span per CPI action, named after the method, carrying a
//     request_id attribute, marked Error on handler error/panic
//   - stdout-purity: tracing must never add, remove, or reorder a single byte
//     on stdout (the JSON-RPC response stream)
//   - the OTel shutdown/flush func runs on every exit path with a
//     bounded-deadline context, and a shutdown error never changes the
//     process's exit code
//   - the per-request logger (built in runCPI) carries trace_id/span_id once
//     it is derived from a ctx that already has the root span attached

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// preservingExporter wraps tracetest.InMemoryExporter and ignores Shutdown.
// tracetest.InMemoryExporter.Shutdown clears its recorded spans (it doubles
// as a "reset" call), which would erase the spans this test needs to inspect
// after runWithArgs's own defer calls the injected shutdown func. Mirrors the
// identical workaround in internal/otel/provider_test.go.
type preservingExporter struct {
	*tracetest.InMemoryExporter
}

func (preservingExporter) Shutdown(context.Context) error { return nil }

// newSyncTracer builds a tracer backed by an in-memory exporter using
// WithSyncer (not WithBatcher): spans are exported synchronously on End(),
// so tests can inspect exporter.GetSpans() immediately after the request that
// produced them, with no batching delay to race against.
func newSyncTracer() (trace.Tracer, preservingExporter) {
	exporter := preservingExporter{tracetest.NewInMemoryExporter()}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	return provider.Tracer("otel-wiring-test"), exporter
}

// writeOTelWiringConfig writes a minimal valid CPI config, with extraJSON
// (e.g. `,"otel":{...}`) appended before the closing brace, and returns its path.
func writeOTelWiringConfig(t *testing.T, extraJSON string) string {
	t.Helper()
	return writeOTelWiringConfigAtLevel(t, "warn", extraJSON)
}

// writeOTelWiringConfigAtLevel is writeOTelWiringConfig with an explicit
// log_level. Tests whose anti-vacuous guard depends on a fanned-out log
// record use "debug" here, since the configured level applies to the OTel
// logs handler exactly as it does to stderr — at the default "warn" a clean
// request emits nothing the fan-out would carry.
func writeOTelWiringConfigAtLevel(t *testing.T, level, extraJSON string) string {
	t.Helper()
	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "` + level + `",
  "verify_ssl": false` + extraJSON + `
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return cfgFile
}

// nilTracerClientFactory satisfies the tracer-aware ClientFactory signature
// with nilPVEClient (defined in main_test.go), for tests that only exercise
// tracer/shutdown wiring and don't need a real PVE client.
func nilTracerClientFactory(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
	return nilPVEClient{}, nil
}

// requestIDAttr returns the string value of the "request_id" attribute on
// span, or "" with ok=false if absent.
func requestIDAttr(span tracetest.SpanStub) (string, bool) {
	for _, kv := range span.Attributes {
		if string(kv.Key) == "request_id" {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// --------------------------------------------------------------------------
// Root span: exactly one per CPI action, named after the method, with
// request_id attribute, Error status on handler error.
// --------------------------------------------------------------------------

func TestOTelWiring_RootSpan_SuccessAndError(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	tracer, exporter := newSyncTracer()

	d := cpi.NewDispatcher(logger)
	if err := d.Register("info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]any{"ok": true}, nil
	})); err != nil {
		t.Fatalf("Register info: %v", err)
	}
	if err := d.Register("has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, cpierrors.Cloud("synthetic handler failure")
	})); err != nil {
		t.Fatalf("Register has_vm: %v", err)
	}

	input := validRequest("info") + validRequest("has_vm")
	var w bytes.Buffer
	if err := runCPI(context.Background(), strings.NewReader(input), &w, d, logger, defaultMaxLineBytes, tracer); err != nil {
		t.Fatalf("runCPI: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected exactly 2 exported root spans (one per action), got %d: %+v", len(spans), spans)
	}

	byName := make(map[string]tracetest.SpanStub, len(spans))
	for _, s := range spans {
		byName[s.Name] = s
	}

	infoSpan, ok := byName["info"]
	if !ok {
		t.Fatalf("no span named %q among exported spans: %+v", "info", spans)
	}
	if infoSpan.Status.Code == codes.Error {
		t.Errorf("success-path span status = Error, want non-error; status=%+v", infoSpan.Status)
	}
	if reqID, ok := requestIDAttr(infoSpan); !ok || reqID != "test-req-1" {
		t.Errorf("info span request_id attribute = %q (present=%v), want %q", reqID, ok, "test-req-1")
	}

	hasVMSpan, ok := byName["has_vm"]
	if !ok {
		t.Fatalf("no span named %q among exported spans: %+v", "has_vm", spans)
	}
	if hasVMSpan.Status.Code != codes.Error {
		t.Errorf("handler-error-path span status = %v, want codes.Error", hasVMSpan.Status.Code)
	}
	if reqID, ok := requestIDAttr(hasVMSpan); !ok || reqID != "test-req-1" {
		t.Errorf("has_vm span request_id attribute = %q (present=%v), want %q", reqID, ok, "test-req-1")
	}
}

// --------------------------------------------------------------------------
// Stdout purity: tracing must add zero stdout bytes. Hard regression gate for
// the CPI's stdin/stdout/stderr contract.
// --------------------------------------------------------------------------

func TestOTelWiring_StdoutPurity_TracingOnVsOff(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "")
	req := validRequest("info")

	// Tracing off: default TracerFactory resolves to otel.Setup with the
	// config's zero-value (disabled) OTel block, i.e. the real no-op path.
	var stdoutOff, stderrOff bytes.Buffer
	codeOff := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutOff, &stderrOff,
		runOptions{ClientFactory: nilTracerClientFactory},
	)

	// Tracing on: TracerFactory substitutes a real recording tracer backed by
	// an in-memory exporter, bypassing cfg entirely (proves stdout purity
	// independent of whatever the config's otel block says).
	tracer, exporter := newSyncTracer()
	tracerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (trace.Tracer, func(context.Context) error, error) {
		return tracer, func(context.Context) error { return nil }, nil
	}
	var stdoutOn, stderrOn bytes.Buffer
	codeOn := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutOn, &stderrOn,
		runOptions{ClientFactory: nilTracerClientFactory, TracerFactory: tracerFactory},
	)

	if codeOff != codeOn {
		t.Fatalf("exit codes differ between tracing off/on: off=%d on=%d; stderrOff=%q stderrOn=%q",
			codeOff, codeOn, stderrOff.String(), stderrOn.String())
	}
	if !bytes.Equal(stdoutOff.Bytes(), stdoutOn.Bytes()) {
		t.Fatalf("stdout differs between tracing off and on (byte-for-byte diff required):\noff=%q\non=%q",
			stdoutOff.String(), stdoutOn.String())
	}
	if len(exporter.GetSpans()) == 0 {
		t.Fatal("test setup broken: expected at least one exported span in the tracing-on run, got 0")
	}
}

// --------------------------------------------------------------------------
// Bounded shutdown flush: runs on every exit path; its error never changes
// the process's exit code; its context always carries a deadline bounded by
// pve.otel.export_timeout_ms.
// --------------------------------------------------------------------------

func TestOTelWiring_Shutdown_ErrorDoesNotChangeExitCode(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "")

	okFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (trace.Tracer, func(context.Context) error, error) {
		return noop.NewTracerProvider().Tracer("test"), func(context.Context) error { return nil }, nil
	}
	errFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (trace.Tracer, func(context.Context) error, error) {
		return noop.NewTracerProvider().Tracer("test"), func(context.Context) error {
			return errors.New("simulated shutdown/export failure")
		}, nil
	}

	var stdoutOK, stderrOK bytes.Buffer
	codeOK := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""), // EOF immediately
		&stdoutOK, &stderrOK,
		runOptions{ClientFactory: nilTracerClientFactory, TracerFactory: okFactory},
	)

	var stdoutErr, stderrErr bytes.Buffer
	codeErr := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""),
		&stdoutErr, &stderrErr,
		runOptions{ClientFactory: nilTracerClientFactory, TracerFactory: errFactory},
	)

	if codeOK != 0 {
		t.Fatalf("baseline (nil shutdown error) run: expected exit code 0, got %d; stderr=%q", codeOK, stderrOK.String())
	}
	if codeErr != codeOK {
		t.Fatalf("shutdown error changed the exit code: nil-error=%d forced-error=%d", codeOK, codeErr)
	}
	if stdoutErr.String() != stdoutOK.String() {
		t.Errorf("shutdown error altered stdout: ok=%q err=%q", stdoutOK.String(), stdoutErr.String())
	}
	if !strings.Contains(stderrErr.String(), "otel shutdown") {
		t.Errorf("expected shutdown failure logged at Warn on stderr, got %q", stderrErr.String())
	}
}

func TestOTelWiring_Shutdown_ContextHasBoundedDeadline(t *testing.T) {
	t.Parallel()

	const exportTimeoutMs = 1500
	cfgFile := writeOTelWiringConfig(t, fmt.Sprintf(
		`,"otel":{"enabled":true,"exporter_endpoint":"127.0.0.1:1","sample_ratio":1.0,"service_name":"otel-wiring-test","export_timeout_ms":%d}`,
		exportTimeoutMs,
	))

	var capturedDeadline time.Time
	var hadDeadline bool
	tracerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (trace.Tracer, func(context.Context) error, error) {
		return noop.NewTracerProvider().Tracer("test"), func(shutdownCtx context.Context) error {
			capturedDeadline, hadDeadline = shutdownCtx.Deadline()
			return nil
		}, nil
	}

	before := time.Now()
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory, TracerFactory: tracerFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	if !hadDeadline {
		t.Fatal("shutdown was invoked with a context.Background()-equivalent ctx (no deadline); want one bounded by export_timeout_ms")
	}
	// remaining is measured from before runWithArgs starts, so it is expected
	// to be export_timeout_ms plus whatever startup work (config load, logger
	// init, agent/hook setup) ran before the deadline context was built — an
	// unbounded (context.Background()) ctx would instead report ok=false
	// above, and a ctx bounded by some other value would fall well outside
	// this generous startup-time allowance.
	const startupAllowance = 2 * time.Second
	minWant := time.Duration(exportTimeoutMs) * time.Millisecond
	maxWant := minWant + startupAllowance
	if remaining := capturedDeadline.Sub(before); remaining <= 0 || remaining < minWant || remaining > maxWant {
		t.Errorf("shutdown ctx deadline %v after call time is out of bounds; want in (%v, %v] for export_timeout_ms=%dms",
			remaining, minWant, maxWant, exportTimeoutMs)
	}
}

// --------------------------------------------------------------------------
// Log correlation: the per-request logger, retrieved from ctx inside a
// handler, must carry trace_id/span_id once tracing is active. This proves
// the root span is started (main.go's tracer.Start call in runCPI) strictly
// before the request logger is derived via logger.WithContext — deriving the
// logger first would silently drop trace correlation from every handler line.
// --------------------------------------------------------------------------

func TestOTelWiring_LogCorrelation_TraceIDPresentWhenTracingEnabled(t *testing.T) {
	t.Parallel()

	tracer, _ := newSyncTracer()

	var stderrBuf bytes.Buffer
	logger, err := log.NewLogger("info", &stderrBuf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	const marker = "otel-wiring-handler-log-marker"
	d := cpi.NewDispatcher(logger)
	if err := d.Register("info", cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// Simulates a handler that logs through the ctx-carried, per-request
		// logger (log.FromContext) rather than a fixed process-level logger.
		log.FromContext(ctx).Info(marker)
		return map[string]any{"ok": true}, nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	input := validRequest("info")
	var stdout bytes.Buffer
	if err := runCPI(context.Background(), strings.NewReader(input), &stdout, d, logger, defaultMaxLineBytes, tracer); err != nil {
		t.Fatalf("runCPI: %v", err)
	}

	var markerLine string
	for _, line := range strings.Split(strings.TrimSpace(stderrBuf.String()), "\n") {
		if strings.Contains(line, marker) {
			markerLine = line
			break
		}
	}
	if markerLine == "" {
		t.Fatalf("handler log marker %q not found in stderr:\n%s", marker, stderrBuf.String())
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(markerLine), &entry); err != nil {
		t.Fatalf("unmarshal log line: %v\nraw: %s", err, markerLine)
	}
	traceID, ok := entry["trace_id"].(string)
	if !ok || traceID == "" {
		t.Fatalf("handler log entry missing non-empty trace_id field: %v", entry)
	}
	if spanID, ok := entry["span_id"].(string); !ok || spanID == "" {
		t.Fatalf("handler log entry missing non-empty span_id field: %v", entry)
	}
}

// TestEndRootSpanErr_ScrubsCredentialURLs verifies the root span's error
// recording scrubs credential-bearing URLs before export, matching the
// scrubbing the logs apply via ErrScrubbed — the collector is an external
// sink and must not receive what stderr deliberately masks.
func TestEndRootSpanErr_ScrubsCredentialURLs(t *testing.T) {
	t.Parallel()
	tracer, exporter := newSyncTracer()

	_, span := tracer.Start(context.Background(), "create_vm")
	rawErr := errors.New("stemcell fetch https://bosh:s3cretpw@blob.lab.internal/img?X-Amz-Signature=deadbeef1234 failed")
	endRootSpanErr(span, rawErr)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	got := spans[0]
	if got.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", got.Status.Code)
	}
	for _, secret := range []string{"s3cretpw", "deadbeef1234"} {
		if strings.Contains(got.Status.Description, secret) {
			t.Errorf("root span status leaks credential %q: %q", secret, got.Status.Description)
		}
		for _, ev := range got.Events {
			for _, attr := range ev.Attributes {
				if v := attr.Value.AsString(); strings.Contains(v, secret) {
					t.Errorf("root span event attribute %s leaks credential %q: %q", attr.Key, secret, v)
				}
			}
		}
	}
	if !strings.Contains(got.Status.Description, log.RedactedPlaceholder) {
		t.Errorf("root span status not scrubbed, want %q marker: %q", log.RedactedPlaceholder, got.Status.Description)
	}
}

// --------------------------------------------------------------------------
// LoggerProviderFactory / MeterProviderFactory wiring — logger swap,
// duration-metric recording, the cfg-gated instrument-creation guard,
// composed shutdown, and setup-failure fail-open.
// --------------------------------------------------------------------------

// spySlogHandler is a minimal slog.Handler spy: every Handle call is
// recorded so a test can assert the process logger fanned at least one
// record out to it once LoggerProviderFactory returns it as the OTel logs
// bridge handler. Enabled always reports true so it never filters a record.
type spySlogHandler struct {
	mu      sync.Mutex
	records []string
}

func (s *spySlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (s *spySlogHandler) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r.Message)
	return nil
}

func (s *spySlogHandler) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *spySlogHandler) WithGroup(string) slog.Handler      { return s }

func (s *spySlogHandler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

var _ slog.Handler = (*spySlogHandler)(nil)

// TestOTelWiring_LogsEnabled_SwapsLoggerHandler verifies that when
// LoggerProviderFactory returns a non-nil handler, runWithArgs rebuilds the
// process logger via log.NewLoggerWithHandlers so every subsequent log
// record (including the dispatcher's per-request "dispatch" line, built
// from the same logger variable passed to cpi.NewDispatcherWithOptions)
// fans out to it.
func TestOTelWiring_LogsEnabled_SwapsLoggerHandler(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfigAtLevel(t, "debug", "")
	req := validRequest("info")

	spy := &spySlogHandler{}
	loggerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (slog.Handler, func(context.Context) error, error) {
		return spy, func(context.Context) error { return nil }, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory, LoggerProviderFactory: loggerFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	if spy.count() == 0 {
		t.Fatal("expected the process logger to fan at least one record out to the injected OTel logs handler, got zero records")
	}
}

// newManualMeter builds a Meter backed by an sdkmetric.ManualReader so a test
// can synchronously Collect recorded data points without waiting on a
// PeriodicReader export interval or a real network collector.
func newManualMeter() (metric.Meter, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return provider.Meter("otel-wiring-metrics-test"), reader
}

// findHistogramDataPoints locates the metricdata.Histogram[float64] data
// points for the action-duration metric within rm, failing the test if the
// metric exists but is not a float64 histogram.
func findHistogramDataPoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	name := otelActionDurationMetricName
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q data type = %T, want metricdata.Histogram[float64]", name, m.Data)
			}
			return hist.DataPoints
		}
	}
	return nil
}

// histogramAttr returns the string value of attribute key on dp, or "" with
// ok=false if absent.
func histogramAttr(dp metricdata.HistogramDataPoint[float64], key string) (string, bool) {
	v, ok := dp.Attributes.Value(attribute.Key(key))
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

// TestOTelWiring_MetricsEnabled_RecordsDurationForSuccessAndError verifies
// that with pve.otel.metrics.enabled=true, one cpi.action.duration histogram
// observation is recorded per dispatched action, carrying cpi.method and an
// outcome of "success" or "error" matching the handler's own result — "info"
// (HandleInfo always succeeds) and "has_vm" with no arguments (fails fast
// with a CloudError, never touching the PVE client).
func TestOTelWiring_MetricsEnabled_RecordsDurationForSuccessAndError(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, `,"otel":{"metrics_enabled":true,"metrics_exporter_endpoint":"127.0.0.1:1"}`)

	meter, reader := newManualMeter()
	meterFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (metric.Meter, func(context.Context) error, error) {
		return meter, func(context.Context) error { return nil }, nil
	}

	input := validRequest("info") + validRequest("has_vm")
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(input),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory, MeterProviderFactory: meterFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	points := findHistogramDataPoints(t, rm)
	if len(points) != 2 {
		t.Fatalf("expected 2 %s data points (one per dispatched action), got %d: %+v", otelActionDurationMetricName, len(points), points)
	}

	outcomeByMethod := make(map[string]string, len(points))
	for _, dp := range points {
		method, ok := histogramAttr(dp, "cpi.method")
		if !ok {
			t.Fatalf("data point missing cpi.method attribute: %+v", dp)
		}
		outcome, ok := histogramAttr(dp, "outcome")
		if !ok {
			t.Fatalf("data point missing outcome attribute: %+v", dp)
		}
		outcomeByMethod[method] = outcome
		if dp.Count != 1 {
			t.Errorf("method %q: data point Count = %d, want 1", method, dp.Count)
		}
	}
	if got := outcomeByMethod["info"]; got != "success" {
		t.Errorf(`"info" data point outcome = %q, want "success" (all: %+v)`, got, outcomeByMethod)
	}
	if got := outcomeByMethod["has_vm"]; got != "error" {
		t.Errorf(`"has_vm" data point outcome = %q, want "error" (all: %+v)`, got, outcomeByMethod)
	}
}

// TestOTelWiring_MetricsEnabled_RecordsMarshalError verifies the fix this file
// exists to guard: a handler that returns a nil error but a result
// json.Marshal rejects must be recorded on the exported cpi.action.duration
// histogram with outcome "marshal_error", not "success" — the outcome a hook
// wrapping the handler call would have recorded, since the marshal step runs
// after the wrapped handler (and any hooks) already returned. This exercises
// newOTelDurationRecorder wired the same way runWithArgs wires it (same
// histogram, same instrument name/unit/description/attribute keys), but
// drives the dispatcher directly with a custom handler: runWithArgs itself
// offers no seam to inject a non-marshalable production handler.
func TestOTelWiring_MetricsEnabled_RecordsMarshalError(t *testing.T) {
	t.Parallel()

	meter, reader := newManualMeter()
	histogram, err := meter.Float64Histogram(
		otelActionDurationMetricName,
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of one dispatched CPI action, in milliseconds."),
	)
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}

	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithDurationRecorder(newOTelDurationRecorder(histogram)))
	if regErr := d.Register("info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return make(chan int), nil // not JSON-serialisable
	})); regErr != nil {
		t.Fatalf("Register: %v", regErr)
	}

	resp := d.Handle(context.Background(), &jsonrpc.Request{Method: "info", Context: jsonrpc.Context{RequestID: "marshal-error-test"}})
	if resp.Error == nil {
		t.Fatal("expected CloudError for non-marshalable result, got success")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	points := findHistogramDataPoints(t, rm)
	if len(points) != 1 {
		t.Fatalf("expected 1 %s data point, got %d: %+v", otelActionDurationMetricName, len(points), points)
	}
	if method, ok := histogramAttr(points[0], "cpi.method"); !ok || method != "info" {
		t.Errorf("cpi.method = %q (ok=%v), want %q", method, ok, "info")
	}
	if outcome, ok := histogramAttr(points[0], "outcome"); !ok || outcome != "marshal_error" {
		t.Errorf("outcome = %q (ok=%v), want %q", outcome, ok, "marshal_error")
	}
}

// TestOTelWiring_MetricsDisabledByConfig_NoHistogramRecorded verifies that
// pve.otel.metrics.enabled=false (the default) prevents the
// cpi.action.duration histogram from ever being created or recorded, even
// when the injected MeterProviderFactory bypasses cfg and hands back a fully
// working, real (non-noop) Meter — proving it is runWithArgs's own
// cfg.OTelMetricsEnabled() gate around the histogram/hook registration, not
// otel.SetupMetrics's internal disabled-path noop, that enforces "zero
// instrument creation... when disabled".
func TestOTelWiring_MetricsDisabledByConfig_NoHistogramRecorded(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "") // no otel block: MetricsEnabled defaults false

	meter, reader := newManualMeter()
	meterFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (metric.Meter, func(context.Context) error, error) {
		return meter, func(context.Context) error { return nil }, nil
	}

	req := validRequest("info")
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory, MeterProviderFactory: meterFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	if points := findHistogramDataPoints(t, rm); len(points) != 0 {
		t.Fatalf("expected zero %s data points with pve.otel.metrics.enabled=false, got %d: %+v", otelActionDurationMetricName, len(points), points)
	}
}

// TestOTelWiring_AllNewSignalsDisabled_DefaultFactoriesRegressionSafe is a
// lightweight cmd/cpi-level regression check that with no otel block at all
// (logs/metrics/protocol all default), the default LoggerProviderFactory/
// MeterProviderFactory (otel.SetupLogs/otel.SetupMetrics) produce no
// observable side effect: the run completes normally, a response is written,
// and no logs/metrics setup-failure warning is ever logged (there is nothing
// to fail when both signals are off).
func TestOTelWiring_AllNewSignalsDisabled_DefaultFactoriesRegressionSafe(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "")
	req := validRequest("info")

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatal("expected a CPI JSON-RPC response on stdout")
	}
	for _, unwanted := range []string{"otel logs init failed", "otel metrics init failed", "otel logs shutdown", "otel metrics shutdown"} {
		if strings.Contains(stderr.String(), unwanted) {
			t.Errorf("stderr unexpectedly contains %q with all new OTel signals disabled: %q", unwanted, stderr.String())
		}
	}
}

// TestOTelWiring_ComposedShutdown_OneSignalErrorDoesNotBlockOthers verifies
// that the trace/logs/metrics shutdown funcs are all invoked independently:
// a trace shutdown error and a logs shutdown error must not prevent the
// metrics shutdown from running (or vice versa), and none of the three
// change the process's exit code.
func TestOTelWiring_ComposedShutdown_OneSignalErrorDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "")

	var traceCalled, logsCalled, metricsCalled atomic.Bool

	tracerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (trace.Tracer, func(context.Context) error, error) {
		return noop.NewTracerProvider().Tracer("test"), func(context.Context) error {
			traceCalled.Store(true)
			return errors.New("simulated trace shutdown failure")
		}, nil
	}
	loggerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (slog.Handler, func(context.Context) error, error) {
		return nil, func(context.Context) error {
			logsCalled.Store(true)
			return errors.New("simulated logs shutdown failure")
		}, nil
	}
	meterFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (metric.Meter, func(context.Context) error, error) {
		return metricnoop.NewMeterProvider().Meter("test"), func(context.Context) error {
			metricsCalled.Store(true)
			return nil
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""), // EOF immediately
		&stdout, &stderr,
		runOptions{
			ClientFactory:         nilTracerClientFactory,
			TracerFactory:         tracerFactory,
			LoggerProviderFactory: loggerFactory,
			MeterProviderFactory:  meterFactory,
		},
	)
	if code != 0 {
		t.Fatalf("shutdown errors changed the exit code: got %d, want 0; stderr=%q", code, stderr.String())
	}
	if !traceCalled.Load() {
		t.Error("trace shutdown was never called")
	}
	if !logsCalled.Load() {
		t.Error("logs shutdown was never called")
	}
	if !metricsCalled.Load() {
		t.Error("metrics shutdown was never called despite trace/logs shutdown errors (must not be skipped)")
	}
	if !strings.Contains(stderr.String(), "otel shutdown/flush failed") {
		t.Errorf("expected trace shutdown failure logged at Warn, got stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "otel logs shutdown/flush failed") {
		t.Errorf("expected logs shutdown failure logged at Warn, got stderr=%q", stderr.String())
	}
}

// TestOTelWiring_LogsAndMetricsSetupFailure_FailOpen verifies that a
// LoggerProviderFactory/MeterProviderFactory setup error never fails the
// CPI: the process still completes, still writes a CPI response, and the
// exit path is otherwise unchanged — only the tracer's setup error retains
// the pre-existing hard-fail contract (untouched by this task).
// --------------------------------------------------------------------------
// Stdout-purity byte-diff extended to grpc-protocol-on, logs-enabled,
// and metrics-enabled variants; env-var-alone and config-vs-env precedence
// negative tests; a listener-based proof that logs-grpc dials its configured
// endpoint.
// --------------------------------------------------------------------------

// newAcceptListener opens a local TCP listener on an ephemeral port and
// starts a background goroutine accepting exactly one connection, signaling
// connCh once it lands. Used to prove (without speaking any wire protocol)
// that an OTLP exporter actually attempted a network export, or conversely
// that it never did. The listener is closed via t.Cleanup so the accept
// goroutine unblocks and exits once the test is done with it.
func newAcceptListener(t *testing.T) (ln net.Listener, connCh chan struct{}) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open local listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	connCh = make(chan struct{}, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			select {
			case connCh <- struct{}{}:
			default:
			}
			_ = conn.Close()
		}
	}()
	return ln, connCh
}

// TestOTelWiring_StdoutPurity_GRPCProtocolOnVsOff extends the tracing
// stdout-purity byte-diff to a grpc-protocol-on variant that exercises the
// real production pipeline (no TracerFactory injection): unlike
// TestOTelWiring_StdoutPurity_TracingOnVsOff, whose injected TracerFactory
// bypasses cfg entirely, this run lets runWithArgs's default TracerFactory
// (otel.Setup) build a real otlptracegrpc exporter from cfg.Protocol="grpc",
// so a bug that leaked protocol-selection or grpc-dial diagnostics onto
// stdout would be caught here. The anti-vacuous guard is a live local TCP
// listener standing in for the collector: since spans cannot be inspected
// through an in-memory exporter when the real exporter is in use, observing
// a connection attempt during the request's forced shutdown-flush is the
// available proof that a span was genuinely exported, not just claimed by
// exit code 0.
func TestOTelWiring_StdoutPurity_GRPCProtocolOnVsOff(t *testing.T) {
	t.Parallel()

	req := validRequest("info")

	offCfgFile := writeOTelWiringConfig(t, "")
	var stdoutOff, stderrOff bytes.Buffer
	codeOff := runWithArgs(
		[]string{"--config", offCfgFile},
		strings.NewReader(req),
		&stdoutOff, &stderrOff,
		runOptions{ClientFactory: nilTracerClientFactory},
	)

	ln, connCh := newAcceptListener(t)

	onCfgFile := writeOTelWiringConfig(t, fmt.Sprintf(
		`,"otel":{"enabled":true,"protocol":"grpc","exporter_endpoint":%q,"insecure":true,"sample_ratio":1.0,"service_name":"otel-wiring-grpc-test","export_timeout_ms":1500}`,
		ln.Addr().String(),
	))
	var stdoutOn, stderrOn bytes.Buffer
	codeOn := runWithArgs(
		[]string{"--config", onCfgFile},
		strings.NewReader(req),
		&stdoutOn, &stderrOn,
		runOptions{ClientFactory: nilTracerClientFactory},
	)

	if codeOff != codeOn {
		t.Fatalf("exit codes differ between grpc-protocol off/on: off=%d on=%d; stderrOff=%q stderrOn=%q",
			codeOff, codeOn, stderrOff.String(), stderrOn.String())
	}
	if !bytes.Equal(stdoutOff.Bytes(), stdoutOn.Bytes()) {
		t.Fatalf("stdout differs between grpc-protocol off and on (byte-for-byte diff required):\noff=%q\non=%q",
			stdoutOff.String(), stdoutOn.String())
	}
	select {
	case <-connCh:
		// grpc exporter dialed the configured listener: a span was genuinely
		// flushed and exported, not just claimed by exit code 0.
	case <-time.After(3 * time.Second):
		t.Fatal("no connection observed at grpc exporter_endpoint within timeout; test setup broken (span export never attempted)")
	}
}

// TestOTelWiring_StdoutPurity_LogsEnabledVsOff extends the stdout-purity
// byte-diff to the logs signal, mirroring
// TestOTelWiring_StdoutPurity_TracingOnVsOff's on/off structure with
// LoggerProviderFactory in place of TracerFactory. The anti-vacuous guard
// (spy.count() > 0) mirrors TestOTelWiring_LogsEnabled_SwapsLoggerHandler:
// the dispatcher's per-request log line always fans out to the injected
// handler once logs are enabled.
func TestOTelWiring_StdoutPurity_LogsEnabledVsOff(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfigAtLevel(t, "debug", "")
	req := validRequest("info")

	var stdoutOff, stderrOff bytes.Buffer
	codeOff := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutOff, &stderrOff,
		runOptions{ClientFactory: nilTracerClientFactory},
	)

	spy := &spySlogHandler{}
	loggerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (slog.Handler, func(context.Context) error, error) {
		return spy, func(context.Context) error { return nil }, nil
	}
	var stdoutOn, stderrOn bytes.Buffer
	codeOn := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutOn, &stderrOn,
		runOptions{ClientFactory: nilTracerClientFactory, LoggerProviderFactory: loggerFactory},
	)

	if codeOff != codeOn {
		t.Fatalf("exit codes differ between logs off/on: off=%d on=%d; stderrOff=%q stderrOn=%q",
			codeOff, codeOn, stderrOff.String(), stderrOn.String())
	}
	if !bytes.Equal(stdoutOff.Bytes(), stdoutOn.Bytes()) {
		t.Fatalf("stdout differs between logs off and on (byte-for-byte diff required):\noff=%q\non=%q",
			stdoutOff.String(), stdoutOn.String())
	}
	if spy.count() == 0 {
		t.Fatal("test setup broken: expected at least one log record fanned to the injected OTel logs handler in the logs-on run, got 0")
	}
}

// TestOTelWiring_StdoutPurity_MetricsEnabledVsOff extends the stdout-purity
// byte-diff to the metrics signal. Unlike the tracing/logs on/off pairs
// above, the "on" run needs its own config file: the cpi.action.duration
// histogram is gated by cfg.OTelMetricsEnabled() in main.go directly (not
// merely by whether a MeterProviderFactory was injected), so
// pve.otel.metrics.enabled must actually be true in the on-run's config for
// the anti-vacuous guard (a real recorded data point) to be reachable at
// all — mirrors TestOTelWiring_MetricsEnabled_RecordsDurationForSuccessAndError's
// config shape.
func TestOTelWiring_StdoutPurity_MetricsEnabledVsOff(t *testing.T) {
	t.Parallel()

	req := validRequest("info")

	offCfgFile := writeOTelWiringConfig(t, "")
	var stdoutOff, stderrOff bytes.Buffer
	codeOff := runWithArgs(
		[]string{"--config", offCfgFile},
		strings.NewReader(req),
		&stdoutOff, &stderrOff,
		runOptions{ClientFactory: nilTracerClientFactory},
	)

	onCfgFile := writeOTelWiringConfig(t, `,"otel":{"metrics_enabled":true,"metrics_exporter_endpoint":"127.0.0.1:1"}`)
	meter, reader := newManualMeter()
	meterFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (metric.Meter, func(context.Context) error, error) {
		return meter, func(context.Context) error { return nil }, nil
	}
	var stdoutOn, stderrOn bytes.Buffer
	codeOn := runWithArgs(
		[]string{"--config", onCfgFile},
		strings.NewReader(req),
		&stdoutOn, &stderrOn,
		runOptions{ClientFactory: nilTracerClientFactory, MeterProviderFactory: meterFactory},
	)

	if codeOff != codeOn {
		t.Fatalf("exit codes differ between metrics off/on: off=%d on=%d; stderrOff=%q stderrOn=%q",
			codeOff, codeOn, stderrOff.String(), stderrOn.String())
	}
	if !bytes.Equal(stdoutOff.Bytes(), stdoutOn.Bytes()) {
		t.Fatalf("stdout differs between metrics off and on (byte-for-byte diff required):\noff=%q\non=%q",
			stdoutOff.String(), stdoutOn.String())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	if points := findHistogramDataPoints(t, rm); len(points) == 0 {
		t.Fatal("test setup broken: expected at least one cpi.action.duration data point in the metrics-on run, got 0")
	}
}

// TestOTelWiring_EnvVarsAloneDoNotActivateTelemetry proves that ambient
// OTEL_EXPORTER_OTLP_* env vars have zero effect when every pve.otel.*
// signal is disabled (the default): runWithArgs uses the real, uninjected
// production factories (otel.Setup / otel.SetupLogs / otel.SetupMetrics),
// each of which returns its disabled-path no-op before ever inspecting an
// env var — so a collector listening at the env-var-named address must never
// see a connection, and stdout must stay byte-identical to a run with no env
// vars set at all.
//
// Uses t.Setenv, which forbids t.Parallel in this test (documented on
// t.Setenv itself); the auto-restore t.Setenv provides is sufficient
// isolation from other tests in this package.
func TestOTelWiring_EnvVarsAloneDoNotActivateTelemetry(t *testing.T) {
	cfgFile := writeOTelWiringConfig(t, "") // no otel block: every pve.otel.* signal defaults false
	req := validRequest("info")

	var stdoutBaseline, stderrBaseline bytes.Buffer
	codeBaseline := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutBaseline, &stderrBaseline,
		runOptions{ClientFactory: nilTracerClientFactory},
	)
	if codeBaseline != 0 {
		t.Fatalf("baseline (no env vars) run: expected exit code 0, got %d; stderr=%q", codeBaseline, stderrBaseline.String())
	}

	ln, connCh := newAcceptListener(t)

	for _, envVar := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		t.Setenv(envVar, ln.Addr().String())
	}

	var stdoutEnv, stderrEnv bytes.Buffer
	codeEnv := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdoutEnv, &stderrEnv,
		runOptions{ClientFactory: nilTracerClientFactory},
	)
	if codeEnv != 0 {
		t.Fatalf("env-vars-set run: expected exit code 0, got %d; stderr=%q", codeEnv, stderrEnv.String())
	}

	if !bytes.Equal(stdoutBaseline.Bytes(), stdoutEnv.Bytes()) {
		t.Fatalf("stdout differs between no-env-vars baseline and ambient-env-vars-set run (byte-for-byte diff required):\nbaseline=%q\nenv=%q",
			stdoutBaseline.String(), stdoutEnv.String())
	}

	select {
	case <-connCh:
		t.Fatal("collector listener observed a connection despite every pve.otel.* signal being disabled; " +
			"ambient OTEL_EXPORTER_OTLP_* env vars must never activate telemetry on their own")
	case <-time.After(300 * time.Millisecond):
		// No connection observed: no signal ever activated, as required. The
		// window only needs to cover the synchronous request+shutdown path
		// above, which has already completed by this point.
	}
}

// TestOTelWiring_ConfigEndpointOverridesConflictingEnvVar proves, end-to-end
// through runWithArgs's production tracer factory (no injection), that an
// explicit pve.otel.exporter_endpoint always wins over a conflicting ambient
// OTEL_EXPORTER_OTLP_*_ENDPOINT env var. internal/otel/provider_test.go
// already proves this at the Setup func level
// (TestSetup_Enabled_Grpc_ExplicitEndpoint_OverridesAmbientEnv); this test
// closes the same gap one layer up, through the CPI binary's actual
// config-load-then-Setup path. A direct assertion on the exporter's
// resolved endpoint is impractical through the production factory seam (no
// seam exposes the built exporter's internal state), so this uses the
// listener-based proof the task explicitly allows: cfg names a live local
// listener, the env vars name an unused/refused port, and only a connection
// at the cfg-named listener counts as success.
func TestOTelWiring_ConfigEndpointOverridesConflictingEnvVar(t *testing.T) {
	ln, connCh := newAcceptListener(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "127.0.0.1:1")

	cfgFile := writeOTelWiringConfig(t, fmt.Sprintf(
		`,"otel":{"enabled":true,"protocol":"grpc","exporter_endpoint":%q,"insecure":true,"sample_ratio":1.0,"service_name":"otel-wiring-precedence-test","export_timeout_ms":1500}`,
		ln.Addr().String(),
	))
	req := validRequest("info")

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}

	select {
	case <-connCh:
		// The explicit cfg.ExporterEndpoint listener was dialed, not the
		// ambient env-var-named (refused) port.
	case <-time.After(3 * time.Second):
		t.Fatal("no connection observed at cfg.exporter_endpoint within timeout; " +
			"the CPI may be resolving the exporter endpoint from ambient OTEL_EXPORTER_OTLP_* env vars instead of cfg")
	}
}

// TestOTelWiring_LogsGRPC_ExplicitEndpoint_DialsConfiguredListener is the
// listener-based proof that the logs signal's grpc
// protocol path dials cfg.LogsExporterEndpoint end-to-end through
// runWithArgs's production otel.SetupLogs default (no factory injection),
// closing a gap previously proven only by code inspection
// (grpcLogsExporterOptionsFor always building an explicit endpoint option).
// Mirrors internal/otel/logs_test.go's lazy-dial pattern and
// TestOTelWiring_ConfigEndpointOverridesConflictingEnvVar's listener
// technique; no gRPC wire protocol needs to be spoken, only a TCP accept.
func TestOTelWiring_LogsGRPC_ExplicitEndpoint_DialsConfiguredListener(t *testing.T) {
	t.Parallel()

	ln, connCh := newAcceptListener(t)

	cfgFile := writeOTelWiringConfigAtLevel(t, "debug", fmt.Sprintf(
		`,"otel":{"logs_enabled":true,"protocol":"grpc","logs_exporter_endpoint":%q,"insecure":true,"export_timeout_ms":1500}`,
		ln.Addr().String(),
	))
	req := validRequest("info")

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}

	select {
	case <-connCh:
		// logs_exporter_endpoint was dialed via the real logs-grpc pipeline.
	case <-time.After(3 * time.Second):
		t.Fatal("no connection observed at logs_exporter_endpoint within timeout; logs-grpc pipeline may not be dialing the configured endpoint")
	}
}

func TestOTelWiring_LogsAndMetricsSetupFailure_FailOpen(t *testing.T) {
	t.Parallel()

	cfgFile := writeOTelWiringConfig(t, "")
	req := validRequest("info")

	loggerFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (slog.Handler, func(context.Context) error, error) {
		return nil, nil, errors.New("simulated logs setup failure")
	}
	meterFactory := func(_ context.Context, _ config.OTelConfig, _ *log.Logger) (metric.Meter, func(context.Context) error, error) {
		return nil, nil, errors.New("simulated metrics setup failure")
	}

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilTracerClientFactory, LoggerProviderFactory: loggerFactory, MeterProviderFactory: meterFactory},
	)
	if code != 0 {
		t.Fatalf("expected exit code 0 despite logs/metrics setup failure (fail-open), got %d; stderr=%q", code, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatal("expected a CPI JSON-RPC response on stdout despite logs/metrics setup failure")
	}
	_ = parseResponse(t, stdout.String())
	if !strings.Contains(stderr.String(), "otel logs init failed") {
		t.Errorf(`expected "otel logs init failed" warning on stderr, got %q`, stderr.String())
	}
	if !strings.Contains(stderr.String(), "otel metrics init failed") {
		t.Errorf(`expected "otel metrics init failed" warning on stderr, got %q`, stderr.String())
	}
}
