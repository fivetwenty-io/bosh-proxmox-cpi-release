package otel

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
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
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     1.0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg, logger)

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
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg, logger)

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
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		Enabled:         true,
		ServiceName:     "pve-cpi-test",
		SampleRatio:     1.0,
		ExportTimeoutMs: 5000,
	}

	tracer, shutdown := newTracerAndShutdown(exporter, cfg, logger)

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
