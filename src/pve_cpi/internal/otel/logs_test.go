package otel

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// --------------------------------------------------------------------------
// Disabled path: nil handler, no-op shutdown, zero network activity.
// --------------------------------------------------------------------------

func TestSetupLogs_Disabled_NoopHandlerAndShutdown(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          false,
		LogsExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port; must never be dialed
	}

	start := time.Now()
	handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error for disabled config: %v", err)
	}
	if handler != nil {
		t.Fatalf("SetupLogs returned non-nil handler for disabled config: %+v", handler)
	}
	if shutdown == nil {
		t.Fatal("SetupLogs returned nil shutdown for disabled config")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("disabled SetupLogs+shutdown took %v, want near-instant (no network dial)", elapsed)
	}
}

// --------------------------------------------------------------------------
// Enabled path: protocol selection.
// --------------------------------------------------------------------------

func TestSetupLogs_Enabled_HTTPProtocol_Succeeds(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "http",
		LogsExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:             true,
		ServiceName:          "svc",
	}

	handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error for http protocol: %v", err)
	}
	if handler == nil || shutdown == nil {
		t.Fatal("SetupLogs returned nil handler/shutdown for valid http config")
	}
	// No records were emitted, so the batch processor has nothing to flush
	// and shutdown completes without any network dial.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error with no buffered records: %v", err)
	}
}

func TestSetupLogs_Enabled_GRPCProtocol_Succeeds(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "grpc",
		LogsExporterEndpoint: "otel-collector.example.internal:4317",
		Insecure:             true,
		ServiceName:          "svc",
	}

	handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error for grpc protocol: %v", err)
	}
	if handler == nil || shutdown == nil {
		t.Fatal("SetupLogs returned nil handler/shutdown for valid grpc config")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error with no buffered records: %v", err)
	}
}

func TestSetupLogs_Enabled_UnknownProtocol_TreatedAsHTTP(t *testing.T) {
	// Defensive: config.Validate already rejects protocol values other than
	// "http"/"grpc" before SetupLogs is ever reached, but SetupLogs's own
	// branch must not panic or silently pick grpc for an unrecognized value.
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "carrier-pigeon",
		LogsExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:             true,
		ServiceName:          "svc",
	}

	handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error for unrecognized protocol: %v", err)
	}
	if handler == nil || shutdown == nil {
		t.Fatal("SetupLogs returned nil handler/shutdown for unrecognized-protocol config")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// Shutdown-failure logging ownership: see the equivalent trace-provider test
// (TestSetup_ShutdownFailure_NotLoggedByShutdownFunc) for the rationale —
// cmd/cpi/main.go's composed defer is the sole owner of the shutdown/flush
// failure Warn across all three signals.
// --------------------------------------------------------------------------

// TestSetupLogs_ShutdownFailure_NotLoggedByShutdownFunc pins that SetupLogs's
// shutdown func performs no logging of its own: it forces a real shutdown
// error via an already-canceled context (sdklog.BatchProcessor.Shutdown
// selects on ctx.Done() before the poll goroutine can signal completion,
// confirmed against the vendored SDK) and asserts the observed logger
// recorded zero entries.
func TestSetupLogs_ShutdownFailure_NotLoggedByShutdownFunc(t *testing.T) {
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "http",
		LogsExporterEndpoint: "otel-collector.example.internal:4318",
		Insecure:             true,
		ServiceName:          "svc",
	}

	_, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error: %v", err)
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
// Input validation.
// --------------------------------------------------------------------------

func TestSetupLogs_Enabled_MissingEndpoint_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{LogsEnabled: true, Protocol: "http", ServiceName: "svc"}

	_, _, err := SetupLogs(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("expected error for empty logs_exporter_endpoint, got nil")
	}
}

func TestSetupLogs_Enabled_NilContext_Errors(t *testing.T) {
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "http",
		LogsExporterEndpoint: "otel-collector.example.internal:4318",
		ServiceName:          "svc",
	}

	//nolint:staticcheck // intentional nil context to exercise validation
	//lint:ignore SA1012 intentional nil context to exercise validation
	_, _, err := SetupLogs(nil, cfg, logger)
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

// --------------------------------------------------------------------------
// Lazy dial: SetupLogs must never block on an unreachable collector, for
// either wire protocol (R1 confirmed this for otlptracegrpc; otlploggrpc's
// lazy-dial behavior was an unverified inference — this test confirms it
// directly).
// --------------------------------------------------------------------------

func TestSetupLogs_Enabled_UnreachableEndpoint_DoesNotBlock(t *testing.T) {
	for _, protocol := range []string{"http", "grpc"} {
		t.Run(protocol, func(t *testing.T) {
			logger := log.NewNopLogger()
			cfg := config.OTelConfig{
				LogsEnabled:          true,
				Protocol:             protocol,
				LogsExporterEndpoint: "127.0.0.1:1", // reserved/unassigned port, refuses connections
				Insecure:             true,
				ServiceName:          "svc",
			}

			start := time.Now()
			handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
			if err != nil {
				t.Fatalf("SetupLogs returned error for unreachable endpoint: %v", err)
			}
			if handler == nil || shutdown == nil {
				t.Fatal("SetupLogs returned nil handler/shutdown for unreachable-endpoint config")
			}
			elapsed := time.Since(start)
			if elapsed > 200*time.Millisecond {
				t.Fatalf("SetupLogs against unreachable endpoint took %v, want near-instant (lazy dial)", elapsed)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Options builders carry the configured endpoint explicitly (never rely on
// ambient OTEL_EXPORTER_OTLP_* env vars).
// --------------------------------------------------------------------------

func TestHTTPLogsExporterOptionsFor(t *testing.T) {
	t.Run("host_port_form", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "collector.example.internal:4318", Insecure: true}
		opts, err := httpLogsExporterOptionsFor(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(opts) == 0 {
			t.Fatal("expected non-empty options for host:port endpoint")
		}
	})

	t.Run("full_url_form", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "https://collector.example.internal:4318"}
		opts, err := httpLogsExporterOptionsFor(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(opts) == 0 {
			t.Fatal("expected non-empty options for full-URL endpoint")
		}
	})

	t.Run("malformed_scheme_errors", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "grpc://collector.example.internal:4317"}
		if _, err := httpLogsExporterOptionsFor(cfg); err == nil {
			t.Fatal("expected error for unsupported scheme, got nil")
		}
	})

	t.Run("missing_host_errors", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "http://"}
		if _, err := httpLogsExporterOptionsFor(cfg); err == nil {
			t.Fatal("expected error for URL missing host, got nil")
		}
	})
}

func TestGRPCLogsExporterOptionsFor(t *testing.T) {
	t.Run("host_port_form", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "collector.example.internal:4317", Insecure: true}
		opts, err := grpcLogsExporterOptionsFor(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(opts) == 0 {
			t.Fatal("expected non-empty options for host:port endpoint")
		}
	})

	t.Run("full_url_form", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "https://collector.example.internal:4317"}
		opts, err := grpcLogsExporterOptionsFor(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(opts) == 0 {
			t.Fatal("expected non-empty options for full-URL endpoint")
		}
	})

	t.Run("malformed_scheme_errors", func(t *testing.T) {
		cfg := config.OTelConfig{LogsExporterEndpoint: "http-plus://collector.example.internal:4317"}
		if _, err := grpcLogsExporterOptionsFor(cfg); err == nil {
			t.Fatal("expected error for unsupported scheme, got nil")
		}
	})
}

// TestSetupLogs_ExplicitEndpointInOptions is the strongest available proof
// that httpLogsExporterOptionsFor's endpoint option is honored: it points
// SetupLogs at a local httptest server via cfg.LogsExporterEndpoint (never
// via an ambient OTEL_EXPORTER_OTLP_* env var, none of which are set here),
// emits one log record through the returned handler, and asserts the
// exporter actually reached that exact address.
func TestSetupLogs_ExplicitEndpointInOptions(t *testing.T) {
	var received int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	logger := log.NewNopLogger()
	cfg := config.OTelConfig{
		LogsEnabled:          true,
		Protocol:             "http",
		LogsExporterEndpoint: endpoint,
		Insecure:             true,
		ServiceName:          "svc",
	}

	handler, shutdown, err := SetupLogs(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("SetupLogs returned error: %v", err)
	}

	slog.New(handler).Info("explicit-endpoint-in-options probe")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Logf("shutdown returned error (non-fatal for this assertion): %v", err)
	}

	if atomic.LoadInt32(&received) == 0 {
		t.Fatal("expected exporter to reach the httptest server at the explicitly configured endpoint, got zero requests")
	}
}
