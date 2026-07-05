// Logs signal pipeline. See the package doc comment in provider.go for the
// shared design (disabled-by-default, no-op when off, error routing).
//
// SetupLogs mirrors Setup's shape but is gated by cfg.LogsEnabled, which is
// independent of cfg.Enabled (traces): a deployment may export logs without
// traces, or vice versa. When enabled it builds an otlploghttp or
// otlploggrpc exporter (selected by cfg.Protocol; any value other than
// "grpc" is treated as "http", since Protocol validation already happens
// upstream in config.Validate) wrapped in an sdklog.LoggerProvider with a
// batch processor, and returns an otelslog.Handler (slog.Handler) built from
// that provider.
package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// SetupLogs builds the logs pipeline described by cfg and returns the
// slog.Handler callers should attach to internal/log's logger, a shutdown
// func the caller must invoke (with a bounded-deadline context) on every
// process exit path, and an error if the pipeline could not be constructed.
//
// cfg.LogsEnabled == false is the default and returns a nil handler (the
// caller keeps its existing handler unmodified) plus a no-op shutdown — no
// exporter, no LoggerProvider, no network dial.
//
// cfg.LogsEnabled == true additionally requires cfg.LogsExporterEndpoint to
// be non-empty (config.ApplyDefaults already falls back to
// cfg.ExporterEndpoint when this field is empty; this is a defensive check
// for callers that construct an OTelConfig without going through
// ApplyDefaults first) and ctx to be non-nil (used to build the exporter's
// client).
func SetupLogs(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (slog.Handler, func(context.Context) error, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	if !cfg.LogsEnabled {
		return nil, noopShutdown, nil
	}

	if ctx == nil {
		return nil, nil, errors.New("otel: SetupLogs requires a non-nil context when logs are enabled")
	}
	if strings.TrimSpace(cfg.LogsExporterEndpoint) == "" {
		return nil, nil, errors.New("otel: logs_exporter_endpoint must not be empty when logs are enabled")
	}

	exporter, err := newLogsExporter(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	setErrorHandlerOnce(logger)

	handler, shutdown := newLogsHandlerAndShutdown(exporter, cfg)
	return handler, shutdown, nil
}

// newLogsExporter builds the otlploghttp or otlploggrpc exporter selected by
// cfg.Protocol. Both exporters perform a lazy, non-blocking dial: New()
// returns successfully even when the collector is unreachable, so this
// never blocks on network I/O.
func newLogsExporter(ctx context.Context, cfg config.OTelConfig) (sdklog.Exporter, error) {
	if cfg.Protocol == protocolGRPC {
		opts, err := grpcLogsExporterOptionsFor(cfg)
		if err != nil {
			return nil, err
		}
		exporter, err := otlploggrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otel: failed to build otlploggrpc exporter: %w", err)
		}
		return exporter, nil
	}

	opts, err := httpLogsExporterOptionsFor(cfg)
	if err != nil {
		return nil, err
	}
	exporter, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel: failed to build otlploghttp exporter: %w", err)
	}
	return exporter, nil
}

// httpLogsExporterOptionsFor translates cfg into otlploghttp options. See
// normalizeEndpoint (provider.go) for the endpoint-syntax rules shared with
// every other signal/protocol option builder in this package. The endpoint
// is always passed explicitly — never left for the exporter to pick up from
// ambient OTEL_EXPORTER_OTLP_* env vars.
func httpLogsExporterOptionsFor(cfg config.OTelConfig) ([]otlploghttp.Option, error) {
	isURL, endpoint, err := normalizeEndpoint("logs_exporter_endpoint", cfg.LogsExporterEndpoint)
	if err != nil {
		return nil, err
	}
	if isURL {
		return []otlploghttp.Option{otlploghttp.WithEndpointURL(endpoint)}, nil
	}

	opts := []otlploghttp.Option{otlploghttp.WithEndpoint(endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	return opts, nil
}

// grpcLogsExporterOptionsFor translates cfg into otlploggrpc options. Same
// endpoint-normalization rules as httpLogsExporterOptionsFor.
func grpcLogsExporterOptionsFor(cfg config.OTelConfig) ([]otlploggrpc.Option, error) {
	isURL, endpoint, err := normalizeEndpoint("logs_exporter_endpoint", cfg.LogsExporterEndpoint)
	if err != nil {
		return nil, err
	}
	if isURL {
		return []otlploggrpc.Option{otlploggrpc.WithEndpointURL(endpoint)}, nil
	}

	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	return opts, nil
}

// newLogsHandlerAndShutdown builds the LoggerProvider from an
// already-constructed exporter and returns the slog.Handler plus a shutdown
// func. Split out from SetupLogs so tests can exercise the batch
// processor/resource wiring with an in-memory/fake exporter instead of a
// real otlploghttp/otlploggrpc/network exporter.
//
// The returned shutdown func does not log a shutdown/flush failure itself:
// the caller composing this func with the trace and metrics shutdown funcs
// (cmd/cpi/main.go) owns the single Warn for all three signals. Callers
// invoking this func directly (e.g. tests) must inspect the returned error
// themselves.
func newLogsHandlerAndShutdown(exporter sdklog.Exporter, cfg config.OTelConfig) (slog.Handler, func(context.Context) error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	res := resource.NewSchemaless(semconv.ServiceNameKey.String(serviceName))
	processor := sdklog.NewBatchProcessor(exporter)

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(res),
	)

	handler := otelslog.NewHandler(tracerName, otelslog.WithLoggerProvider(provider))

	shutdown := func(shutdownCtx context.Context) error {
		return provider.Shutdown(shutdownCtx)
	}

	return handler, shutdown
}
