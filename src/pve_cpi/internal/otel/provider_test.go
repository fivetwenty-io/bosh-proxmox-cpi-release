package otel

import (
	"context"
	"errors"
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
