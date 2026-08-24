package otel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// --------------------------------------------------------------------------
// Disabled path: no-op meter, no-op shutdown, zero network activity.
// --------------------------------------------------------------------------

func TestSetupMetrics_Disabled_NoopMeterAndShutdown(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{MetricsEnabled: false}

	start := time.Now()
	meter, shutdown, err := SetupMetrics(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupMetrics returned error for disabled config: %v", err)
	}
	if meter == nil {
		t.Fatal("SetupMetrics returned nil meter for disabled config")
	}
	if shutdown == nil {
		t.Fatal("SetupMetrics returned nil shutdown for disabled config")
	}

	if _, err := meter.Int64Counter("disabled.counter"); err != nil {
		t.Fatalf("noop meter Int64Counter returned error: %v", err)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("disabled SetupMetrics+shutdown took %v, want near-instant (no network dial)", elapsed)
	}
}

func TestSetupMetrics_Disabled_ImpossibleEndpoint_NeverDials(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		MetricsEnabled:          false,
		MetricsExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port, refuses connections
	}

	start := time.Now()
	meter, shutdown, err := SetupMetrics(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupMetrics returned error for disabled config: %v", err)
	}

	if _, err := meter.Int64Counter("disabled.counter"); err != nil {
		t.Fatalf("noop meter Int64Counter returned error: %v", err)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("disabled SetupMetrics with impossible endpoint took %v, want near-instant (no dial attempted)", elapsed)
	}
}

// --------------------------------------------------------------------------
// SetupMetrics input validation (enabled path).
// --------------------------------------------------------------------------

func TestSetupMetrics_Enabled_EmptyEndpoint_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{MetricsEnabled: true, ServiceName: "svc", Protocol: "http"}

	_, _, err := SetupMetrics(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("expected error for empty metrics_exporter_endpoint, got nil")
	}
}

func TestSetupMetrics_Enabled_NilContext_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		MetricsEnabled:          true,
		MetricsExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:             "svc",
		Protocol:                "http",
	}

	//nolint:staticcheck // intentional nil context to exercise validation
	//lint:ignore SA1012 intentional nil context to exercise validation
	_, _, err := SetupMetrics(nil, cfg, logger)
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

func TestSetupMetrics_Enabled_MalformedEndpointURL_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		MetricsEnabled:          true,
		MetricsExporterEndpoint: "grpc://collector.example.internal:4317",
		ServiceName:             "svc",
		Protocol:                "http",
	}

	_, _, err := SetupMetrics(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("expected error for unsupported endpoint URL scheme, got nil")
	}
}

// TestSetupMetrics_Enabled_UnreachableEndpoint_NonBlocking asserts that an
// unreachable-but-syntactically-valid collector endpoint does not block
// SetupMetrics: both otlpmetrichttp.New and otlpmetricgrpc.New perform lazy,
// non-blocking client construction — connection errors surface only
// on export/shutdown, never at construction.
func TestSetupMetrics_Enabled_UnreachableEndpoint_NonBlocking(t *testing.T) {
	for _, protocol := range []string{"http", "grpc"} {
		protocol := protocol
		t.Run(protocol, func(t *testing.T) {
			logger := log.NewNopLogger()
			cfg := config.OTelConfig{
				MetricsEnabled:          true,
				MetricsExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port, refuses connections
				Insecure:                true,
				ServiceName:             "svc",
				Protocol:                protocol,
			}

			start := time.Now()
			meter, shutdown, err := SetupMetrics(context.Background(), cfg, logger)
			if err != nil {
				t.Fatalf("SetupMetrics returned error for unreachable endpoint: %v", err)
			}
			if meter == nil || shutdown == nil {
				t.Fatal("SetupMetrics returned nil meter/shutdown for valid config")
			}
			elapsed := time.Since(start)
			if elapsed > 500*time.Millisecond {
				t.Fatalf("SetupMetrics with unreachable endpoint took %v, want near-instant (lazy dial)", elapsed)
			}

			const bound = 2 * time.Second
			shutdownCtx, cancel := context.WithTimeout(context.Background(), bound)
			defer cancel()
			// Error is expected/acceptable here (no buffered metrics, but the
			// underlying reader may still attempt a final collect against an
			// unreachable target); the assertion is that it returns within
			// the bounded context (with reasonable scheduling slack), never
			// hangs past it. shutdownCtx.Err() alone cannot distinguish
			// "returned right at the deadline" (correct, bounded) from
			// "ignored the deadline" (a hang) since Err() is non-nil at the
			// deadline either way — only wall-clock elapsed time can.
			shutdownStart := time.Now()
			_ = shutdown(shutdownCtx)
			shutdownElapsed := time.Since(shutdownStart)
			if shutdownElapsed > bound+500*time.Millisecond {
				t.Fatalf("shutdown against unreachable endpoint took %v, want within bound %v (+slack)", shutdownElapsed, bound)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Shutdown-failure logging ownership: see the equivalent trace-provider test
// (TestSetup_ShutdownFailure_NotLoggedByShutdownFunc) for the rationale —
// cmd/cpi/main.go's composed defer is the sole owner of the shutdown/flush
// failure Warn across all three signals.
// --------------------------------------------------------------------------

// TestSetupMetrics_ShutdownFailure_NotLoggedByShutdownFunc pins that
// SetupMetrics's shutdown func performs no logging of its own: it forces a
// real shutdown error via an already-canceled context
// (sdkmetric.PeriodicReader.Shutdown's internal collect propagates
// ctx.Err(), confirmed against the vendored SDK) and asserts the observed
// logger recorded zero entries.
func TestSetupMetrics_ShutdownFailure_NotLoggedByShutdownFunc(t *testing.T) {
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	cfg := config.OTelConfig{
		MetricsEnabled:          true,
		MetricsExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:                true,
		ServiceName:             "svc",
		Protocol:                "http",
	}

	_, shutdown, err := SetupMetrics(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupMetrics returned error: %v", err)
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
// Protocol selection: cfg.Protocol picks otlpmetrichttp vs otlpmetricgrpc.
// --------------------------------------------------------------------------

func TestNewMetricExporter_ProtocolSelection(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		wantType string
	}{
		{"http_explicit", "http", "*otlpmetrichttp.Exporter"},
		{"http_default_non_grpc_value", "", "*otlpmetrichttp.Exporter"},
		{"http_defensive_unknown_value", "quic", "*otlpmetrichttp.Exporter"},
		{"grpc", "grpc", "*otlpmetricgrpc.Exporter"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.OTelConfig{
				MetricsExporterEndpoint: "127.0.0.1:1",
				Insecure:                true,
				Protocol:                tc.protocol,
			}

			exporter, err := newMetricExporter(context.Background(), cfg)
			if err != nil {
				t.Fatalf("newMetricExporter returned error: %v", err)
			}

			switch tc.wantType {
			case "*otlpmetrichttp.Exporter":
				if _, ok := exporter.(*otlpmetrichttp.Exporter); !ok {
					t.Fatalf("protocol %q: got exporter type %T, want *otlpmetrichttp.Exporter", tc.protocol, exporter)
				}
			case "*otlpmetricgrpc.Exporter":
				if _, ok := exporter.(*otlpmetricgrpc.Exporter); !ok {
					t.Fatalf("protocol %q: got exporter type %T, want *otlpmetricgrpc.Exporter", tc.protocol, exporter)
				}
			}
		})
	}
}

// --------------------------------------------------------------------------
// Delta temporality selector: every InstrumentKind reports Delta,
// overriding the SDK's CUMULATIVE default.
// --------------------------------------------------------------------------

func TestDeltaTemporalitySelector_AllInstrumentKinds(t *testing.T) {
	kinds := []sdkmetric.InstrumentKind{
		sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindUpDownCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter,
		sdkmetric.InstrumentKindObservableGauge,
		sdkmetric.InstrumentKindGauge,
	}

	for _, kind := range kinds {
		got := deltaTemporalitySelector(kind)
		if got != metricdata.DeltaTemporality {
			t.Errorf("deltaTemporalitySelector(%v) = %v, want DeltaTemporality", kind, got)
		}
	}
}

// --------------------------------------------------------------------------
// Explicit endpoint wiring: options builder always carries cfg's endpoint,
// never an ambient ...OTLP_* env var, proven end-to-end against a
// real local listener rather than by reflecting into the unexported oconf
// option-application internals (not importable from this package).
// --------------------------------------------------------------------------

func TestMetricHTTPOptionsFor_ExplicitEndpoint_HostPort_ReachesConfiguredServer(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	cfg := config.OTelConfig{MetricsExporterEndpoint: endpoint, Insecure: true, Protocol: "http"}

	exporter, err := newMetricExporter(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newMetricExporter returned error: %v", err)
	}

	reader := sdkmetric.NewPeriodicReader(exporter)
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(meterName)

	counter, err := meter.Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter returned error: %v", err)
	}
	counter.Add(context.Background(), 1)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("provider.Shutdown returned error: %v", err)
	}

	if gotHost != endpoint {
		t.Fatalf("collector received request Host %q, want %q (cfg.MetricsExporterEndpoint, not an ambient env endpoint)", gotHost, endpoint)
	}
}

func TestMetricGRPCOptionsFor_ExplicitEndpoint_ReachesConfiguredListener(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to open local listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	cfg := config.OTelConfig{MetricsExporterEndpoint: ln.Addr().String(), Insecure: true, Protocol: "grpc"}

	exporter, err := newMetricExporter(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newMetricExporter returned error: %v", err)
	}

	reader := sdkmetric.NewPeriodicReader(exporter)
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(meterName)

	counter, err := meter.Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter returned error: %v", err)
	}
	counter.Add(context.Background(), 1)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The bare TCP listener cannot complete a real gRPC handshake, so
	// Shutdown/export is expected to error; what matters is that the
	// dial target was the exact configured endpoint, proven by the
	// listener receiving a connection attempt.
	_ = provider.Shutdown(shutdownCtx)

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatalf("listener at cfg.MetricsExporterEndpoint (%s) never received a connection attempt", cfg.MetricsExporterEndpoint)
	}
}
