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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
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
	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "warn",
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
