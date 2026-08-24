package otel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// --------------------------------------------------------------------------
// Disabled path: no-op tracer, no-op shutdown, zero network activity.
// --------------------------------------------------------------------------

func TestSetup_Disabled_NoopTracerAndShutdown(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{Enabled: false}

	start := time.Now()
	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for disabled config: %v", err)
	}
	if tracer == nil {
		t.Fatal("Setup returned nil tracer for disabled config")
	}
	if shutdown == nil {
		t.Fatal("Setup returned nil shutdown for disabled config")
	}

	_, span := tracer.Start(context.Background(), "disabled-span")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("disabled Setup+shutdown took %v, want near-instant (no network dial)", elapsed)
	}
}

// TestSetup_Disabled_ImpossibleEndpoint_NeverDials asserts that even when an
// endpoint is present in cfg, Enabled=false means it is never touched: no
// exporter is built and no dial is attempted against an address that would
// error/hang if actually contacted.
func TestSetup_Disabled_ImpossibleEndpoint_NeverDials(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          false,
		ExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port, refuses connections
	}

	start := time.Now()
	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for disabled config: %v", err)
	}

	_, span := tracer.Start(context.Background(), "span")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("disabled Setup with impossible endpoint took %v, want near-instant (no dial attempted)", elapsed)
	}
}

// --------------------------------------------------------------------------
// Enabled path: exporter/processor/sampler wiring, exercised without network
// via an in-memory exporter (mirrors Setup's internal split between exporter
// construction and TracerProvider wiring in newTracerAndShutdown).
// --------------------------------------------------------------------------

// preservingExporter wraps tracetest.InMemoryExporter and ignores Shutdown.
// tracetest.InMemoryExporter.Shutdown clears its recorded spans (it doubles
// as a Reset), which would otherwise erase the very spans these tests need
// to inspect immediately after exercising the real shutdown/flush path.
type preservingExporter struct {
	*tracetest.InMemoryExporter
}

func (preservingExporter) Shutdown(context.Context) error { return nil }

func TestEnabledPipeline_ExportsSpanAfterShutdown(t *testing.T) {
	exporter := preservingExporter{tracetest.NewInMemoryExporter()}
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     1.0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg)

	_, span := tracer.Start(context.Background(), "test-span")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	if spans[0].Name != "test-span" {
		t.Errorf("exported span name = %q, want %q", spans[0].Name, "test-span")
	}
}

func TestEnabledPipeline_SampleRatioZero_DropsSpans(t *testing.T) {
	exporter := preservingExporter{tracetest.NewInMemoryExporter()}
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg)

	// A span started with no parent context and a zero sample ratio is
	// dropped by the root TraceIDRatioBased{0} sampler wrapped in
	// ParentBased (no parent to defer to, so the root decision applies).
	_, span := tracer.Start(context.Background(), "dropped-span")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}

	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("got %d exported spans with sample_ratio=0, want 0", got)
	}
}

func TestEnabledPipeline_SampleRatioOne_KeepsSpans(t *testing.T) {
	exporter := preservingExporter{tracetest.NewInMemoryExporter()}
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     1.0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg)

	_, span := tracer.Start(context.Background(), "kept-span")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}

	if got := len(exporter.GetSpans()); got != 1 {
		t.Fatalf("got %d exported spans with sample_ratio=1, want 1", got)
	}
}

// --------------------------------------------------------------------------
// Shutdown-failure logging ownership: the shutdown func returned by Setup
// must not itself Warn-log a shutdown/flush failure. cmd/cpi/main.go's
// composed defer is the sole owner of that Warn (it holds the bounded
// timeout context the failure is reported against); logging it a second
// time here would duplicate the message on every failed shutdown.
// --------------------------------------------------------------------------

// TestSetup_ShutdownFailure_NotLoggedByShutdownFunc pins that Setup's
// shutdown func performs no logging of its own: it forces a real,
// non-simulated shutdown error (an already-canceled context, which
// sdktrace.TracerProvider.Shutdown detects before running any span
// processor and returns as ctx.Err() — confirmed against the vendored SDK,
// not merely assumed) and asserts the observed logger recorded zero
// entries, proving the failure path is silent at this layer.
func TestSetup_ShutdownFailure_NotLoggedByShutdownFunc(t *testing.T) {
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	_, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := shutdown(canceledCtx); err == nil {
		t.Fatal("expected shutdown to return an error for an already-canceled context")
	}

	if entries := observer.All(); len(entries) != 0 {
		t.Fatalf("shutdown func logged %d entries on failure, want 0 (caller owns shutdown-failure logging): %+v", len(entries), entries)
	}
}

// --------------------------------------------------------------------------
// Setup input validation (enabled path).
// --------------------------------------------------------------------------

func TestSetup_Enabled_EmptyEndpoint_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{Enabled: true, ServiceName: "svc", SampleRatio: 1.0, ExportTimeoutMs: 5000}

	_, _, err := Setup(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("expected error for empty exporter_endpoint, got nil")
	}
}

func TestSetup_Enabled_NilContext_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	//nolint:staticcheck // intentional nil context to exercise validation
	//lint:ignore SA1012 intentional nil context to exercise validation
	_, _, err := Setup(nil, cfg, logger)
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

func TestSetup_Enabled_MalformedEndpointURL_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "grpc://collector.example.internal:4317",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	_, _, err := Setup(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("expected error for unsupported endpoint URL scheme, got nil")
	}
}

func TestSetup_Enabled_HostPortEndpoint_Succeeds(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:         true,
		ServiceName:      "svc",
		SampleRatio:      0.5,
		ExportTimeoutMs:  5000,
	}

	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for valid host:port endpoint: %v", err)
	}
	if tracer == nil || shutdown == nil {
		t.Fatal("Setup returned nil tracer/shutdown for valid config")
	}
	// Shutdown without ever exporting: bounded, must not hang or panic. No
	// spans were started, so the batch processor has nothing to flush and
	// no network dial is required to report success.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error with no buffered spans: %v", err)
	}
}

func TestSetup_Enabled_FullURLEndpoint_Succeeds(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "https://otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for valid full-URL endpoint: %v", err)
	}
	if tracer == nil || shutdown == nil {
		t.Fatal("Setup returned nil tracer/shutdown for valid config")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error with no buffered spans: %v", err)
	}
}

// --------------------------------------------------------------------------
// otel.SetErrorHandler wiring: internal SDK errors route to logger.Warn.
// --------------------------------------------------------------------------

func TestSetup_Enabled_ErrorHandlerRoutesToLogger(t *testing.T) {
	// errorHandlerOnce is now shared package-wide across Setup/SetupMetrics/
	// SetupLogs: whichever of those three runs first
	// in the test binary wins the one-time otelapi.SetErrorHandler install.
	// Reset it here so this test deterministically installs its own logger's
	// handler regardless of what ran earlier in the package's test suite.
	errorHandlerOnce = sync.Once{}
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	cfg := config.OTelConfig{
		Enabled:          true,
		ExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	_, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	otelapi.Handle(errors.New("simulated otel sdk error"))

	entries := observer.All()
	found := false
	for _, e := range entries {
		if e.Level != log.LevelWarn {
			continue
		}
		if errVal, ok := e.Attrs["error"]; ok {
			if s, ok := errVal.(string); ok && s != "" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected a Warn entry carrying the otel error, got entries: %+v", entries)
	}
}

// --------------------------------------------------------------------------
// normalizeEndpoint: the single shared endpoint classifier/validator behind
// every trace/logs/metrics x http/grpc option builder in this package.
// Exercised once here across all three field names, rather than duplicated
// per-signal, since the five call sites delegate to this exact function.
// --------------------------------------------------------------------------

func TestNormalizeEndpoint(t *testing.T) {
	const (
		fieldTrace   = "exporter_endpoint"
		fieldLogs    = "logs_exporter_endpoint"
		fieldMetrics = "metrics_exporter_endpoint"
	)

	cases := []struct {
		name       string
		raw        string
		wantIsURL  bool
		wantEndp   string
		wantErrFmt string // %s is substituted with the field name; "" means no error expected
	}{
		{
			name:      "bare_host_port",
			raw:       "otel-collector.example.internal:4318",
			wantIsURL: false,
			wantEndp:  "otel-collector.example.internal:4318",
		},
		{
			name:      "full_http_url",
			raw:       "http://otel-collector.example.internal:4318",
			wantIsURL: true,
			wantEndp:  "http://otel-collector.example.internal:4318",
		},
		{
			name:      "full_https_url",
			raw:       "https://otel-collector.example.internal:4318",
			wantIsURL: true,
			wantEndp:  "https://otel-collector.example.internal:4318",
		},
		{
			name:       "bad_scheme",
			raw:        "grpc://collector.example.internal:4317",
			wantErrFmt: `otel: %s URL "grpc://collector.example.internal:4317" must use http or https scheme, got "grpc"`,
		},
		{
			name:       "missing_host",
			raw:        "http://",
			wantErrFmt: `otel: %s URL "http://" is missing a host`,
		},
		{
			name:       "unparseable_url",
			raw:        "http://[::1",
			wantErrFmt: `otel: %s URL "http://[::1" is invalid`,
		},
		{
			name:      "leading_trailing_whitespace_host_port",
			raw:       "  otel-collector.example.internal:4318  ",
			wantIsURL: false,
			wantEndp:  "otel-collector.example.internal:4318",
		},
		{
			name:      "leading_trailing_whitespace_url",
			raw:       "  https://otel-collector.example.internal:4318  ",
			wantIsURL: true,
			wantEndp:  "https://otel-collector.example.internal:4318",
		},
	}

	for _, tc := range cases {
		for _, field := range []string{fieldTrace, fieldLogs, fieldMetrics} {
			t.Run(tc.name+"/"+field, func(t *testing.T) {
				isURL, endpoint, err := normalizeEndpoint(field, tc.raw)

				if tc.wantErrFmt == "" {
					if err != nil {
						t.Fatalf("normalizeEndpoint(%q, %q) returned unexpected error: %v", field, tc.raw, err)
					}
					if isURL != tc.wantIsURL {
						t.Errorf("normalizeEndpoint(%q, %q) isURL = %v, want %v", field, tc.raw, isURL, tc.wantIsURL)
					}
					if endpoint != tc.wantEndp {
						t.Errorf("normalizeEndpoint(%q, %q) endpoint = %q, want %q", field, tc.raw, endpoint, tc.wantEndp)
					}
					return
				}

				if err == nil {
					t.Fatalf("normalizeEndpoint(%q, %q) expected error, got nil", field, tc.raw)
				}
				// The "unparseable_url" case wraps url.Parse's own error via
				// %w, whose exact text is a Go stdlib implementation detail;
				// check only the fixed prefix/suffix this package controls.
				if tc.name == "unparseable_url" {
					wantPrefix := fmt.Sprintf("otel: invalid %s URL %q:", field, tc.raw)
					if !strings.HasPrefix(err.Error(), wantPrefix) {
						t.Errorf("normalizeEndpoint(%q, %q) error = %q, want prefix %q", field, tc.raw, err.Error(), wantPrefix)
					}
					return
				}
				wantErr := fmt.Sprintf(tc.wantErrFmt, field)
				if err.Error() != wantErr {
					t.Errorf("normalizeEndpoint(%q, %q) error = %q, want %q", field, tc.raw, err.Error(), wantErr)
				}
			})
		}
	}
}

// --------------------------------------------------------------------------
// gRPC protocol (cfg.Protocol == "grpc"): fallback selection, option
// translation, and lazy-dial behavior.
// --------------------------------------------------------------------------

func TestIsGRPCProtocol(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		want     bool
	}{
		{"exact grpc selects grpc", "grpc", true},
		{"empty (unset) falls back to http", "", false},
		{"explicit http stays http", "http", false},
		{"unrecognized value falls back to http", "quic", false},
		{"case mismatch falls back to http", "GRPC", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isGRPCProtocol(config.OTelConfig{Protocol: tc.protocol})
			if got != tc.want {
				t.Errorf("isGRPCProtocol(Protocol=%q) = %v, want %v", tc.protocol, got, tc.want)
			}
		})
	}
}

func TestGRPCExporterOptionsFor_HostPort_NoInsecure(t *testing.T) {
	cfg := config.OTelConfig{ExporterEndpoint: "otel-collector.example.internal:4317"}

	opts, err := grpcExporterOptionsFor(cfg)
	if err != nil {
		t.Fatalf("grpcExporterOptionsFor returned error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("got %d options for host:port without Insecure, want 1 (WithEndpoint only)", len(opts))
	}
}

func TestGRPCExporterOptionsFor_HostPort_Insecure(t *testing.T) {
	cfg := config.OTelConfig{
		ExporterEndpoint: "otel-collector.example.internal:4317",
		Insecure:         true,
	}

	opts, err := grpcExporterOptionsFor(cfg)
	if err != nil {
		t.Fatalf("grpcExporterOptionsFor returned error: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("got %d options for host:port with Insecure=true, want 2 (WithEndpoint + WithInsecure)", len(opts))
	}
}

func TestGRPCExporterOptionsFor_FullURLEndpoint_IgnoresInsecure(t *testing.T) {
	cfg := config.OTelConfig{
		ExporterEndpoint: "https://otel-collector.example.internal:4317",
		Insecure:         true, // must be ignored: the URL scheme already states intent
	}

	opts, err := grpcExporterOptionsFor(cfg)
	if err != nil {
		t.Fatalf("grpcExporterOptionsFor returned error: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("got %d options for full URL endpoint, want 1 (WithEndpointURL only, Insecure ignored)", len(opts))
	}
}

func TestGRPCExporterOptionsFor_MalformedURLScheme_Errors(t *testing.T) {
	cfg := config.OTelConfig{ExporterEndpoint: "grpc://collector.example.internal:4317"}

	_, err := grpcExporterOptionsFor(cfg)
	if err == nil {
		t.Fatal("expected error for unsupported endpoint URL scheme, got nil")
	}
}

func TestSetup_Enabled_Grpc_LazyDial_NonBlocking(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		Protocol:         "grpc",
		ExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port, refuses connections
		Insecure:         true,
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	start := time.Now()
	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for grpc protocol: %v", err)
	}
	if tracer == nil || shutdown == nil {
		t.Fatal("Setup returned nil tracer/shutdown for grpc protocol")
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("grpc Setup took %v, want near-instant (otlptracegrpc.New dials lazily, never blocks)", elapsed)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(shutdownCtx) // no spans buffered; any error from the unreachable collector is irrelevant here
}

// TestSetup_Enabled_Grpc_ExplicitEndpoint_OverridesAmbientEnv proves the
// grpc options builder always carries an explicit endpoint derived from
// cfg, never the ambient OTEL_EXPORTER_OTLP_ENDPOINT /
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT env vars: it points cfg at a listener
// bound to an ephemeral local port while the env vars point at a refused
// port, then confirms the exported span's connection attempt lands on the
// listener cfg names, not the env-named one.
func TestSetup_Enabled_Grpc_ExplicitEndpoint_OverridesAmbientEnv(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open local listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	connCh := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			connCh <- struct{}{}
			_ = conn.Close()
		}
	}()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "127.0.0.1:1")

	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:          true,
		Protocol:         "grpc",
		ExporterEndpoint: ln.Addr().String(),
		Insecure:         true,
		ServiceName:      "svc",
		SampleRatio:      1.0,
		ExportTimeoutMs:  5000,
	}

	tracer, shutdown, err := Setup(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("Setup returned error for grpc protocol: %v", err)
	}

	_, span := tracer.Start(context.Background(), "grpc-explicit-endpoint-span")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(shutdownCtx) // flushes the buffered span, triggering the dial; export itself is expected to fail (not a real collector)

	select {
	case <-connCh:
		// explicit cfg endpoint was dialed, not the ambient env endpoint.
	case <-time.After(3 * time.Second):
		t.Fatal("no connection observed at cfg.ExporterEndpoint within timeout; grpc exporter may be using ambient OTEL_EXPORTER_OTLP_* env vars instead of cfg")
	}
}
