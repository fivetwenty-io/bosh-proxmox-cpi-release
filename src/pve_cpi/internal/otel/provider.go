// Package otel builds the CPI's opt-in OpenTelemetry tracing pipeline.
//
// Tracing is disabled by default (config.OTelConfig zero value). When
// disabled, Setup returns a no-op tracer and a no-op shutdown func: no SDK
// TracerProvider, exporter, or network client is ever constructed, so a CPI
// deployment that never sets pve.otel.enabled sees zero behavior change and
// zero added overhead.
//
// When enabled, the pipeline is:
//   - an OTLP exporter pointed at cfg.ExporterEndpoint, using otlptracehttp
//     (OTLP http/protobuf) unless cfg.Protocol is exactly "grpc", in which
//     case otlptracegrpc is used instead
//   - sdktrace.TracerProvider with a BatchSpanProcessor wrapping that
//     exporter, sampling via ParentBased(TraceIDRatioBased(cfg.SampleRatio))
//   - a Resource carrying service.name = cfg.ServiceName
//
// SDK-internal errors (failed exports, malformed responses, etc.) are routed
// through the package-wide shared error handler (see setErrorHandlerOnce in
// metrics.go, installed at most once regardless of which of Setup/
// SetupMetrics/SetupLogs runs first) to logger.Warn so they land in the
// CPI's existing structured stderr stream and never touch stdout (stdout is
// reserved for the CPI JSON-RPC response and --version output) and never
// abort a CPI action.
//
// The returned shutdown func calls TracerProvider.Shutdown, which force-
// flushes any buffered spans before stopping the pipeline. Shutdown does not
// impose its own timeout: the caller must bound the context it passes in
// (the pve.otel.export_timeout_ms property exists for exactly this purpose)
// so process exit is never blocked indefinitely by a slow or unreachable
// collector.
package otel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// tracerName identifies this instrumentation library to the tracing backend.
// It is the CPI's Go module path, matching the OTel convention of using the
// instrumenting package's import path as the tracer name.
const tracerName = "github.com/fivetwenty-io/bosh-pve-cpi"

// defaultServiceName is used when cfg.ServiceName is empty. Normal callers
// reach Setup only after config.ApplyDefaults, which already fills this in
// when Enabled is true; this is a defensive fallback for callers (tests,
// future callers) that construct an OTelConfig without going through
// ApplyDefaults first.
const defaultServiceName = "bosh-pve-cpi"

// noopShutdown is the shutdown func returned when tracing is disabled. It
// performs no work, opens no network connection, and never returns an error.
func noopShutdown(context.Context) error { return nil }

// Setup builds the tracing pipeline described by cfg and returns the tracer
// handlers should use to start spans, a shutdown func the caller must invoke
// (with a bounded-deadline context) on every process exit path, and an error
// if the pipeline could not be constructed.
//
// cfg.Enabled == false is the default and returns a no-op tracer (every
// Start call is a cheap, non-recording, non-exported span) plus a no-op
// shutdown — no exporter, no TracerProvider, no network dial.
//
// cfg.Enabled == true additionally requires cfg.ExporterEndpoint to be
// non-empty (an empty endpoint would otherwise silently fall back to the
// exporter's built-in default endpoint, masking an operator
// misconfiguration) and ctx to be non-nil (used to build the exporter's
// client).
//
// cfg.Protocol selects the OTLP wire protocol: exactly "grpc" builds an
// otlptracegrpc exporter, any other value (including "", "http", or an
// unrecognized string) builds the otlptracehttp exporter. Rejecting an
// invalid cfg.Protocol value is config-validation's job (internal/config);
// Setup treats anything that is not literally "grpc" as "http" so that a
// config which somehow reaches Setup without having been validated (e.g. a
// caller building OTelConfig directly, bypassing config.Validate) still
// gets a working, previously-supported pipeline instead of an error.
func Setup(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (trace.Tracer, func(context.Context) error, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	if !cfg.Enabled {
		return noop.NewTracerProvider().Tracer(tracerName), noopShutdown, nil
	}

	if ctx == nil {
		return nil, nil, errors.New("otel: Setup requires a non-nil context when tracing is enabled")
	}
	if strings.TrimSpace(cfg.ExporterEndpoint) == "" {
		return nil, nil, errors.New("otel: exporter_endpoint must not be empty when tracing is enabled")
	}

	var exporter *otlptrace.Exporter
	var err error

	if isGRPCProtocol(cfg) {
		var grpcOpts []otlptracegrpc.Option
		grpcOpts, err = grpcExporterOptionsFor(cfg)
		if err != nil {
			return nil, nil, err
		}
		exporter, err = otlptracegrpc.New(ctx, grpcOpts...)
		if err != nil {
			return nil, nil, fmt.Errorf("otel: failed to build otlptracegrpc exporter: %w", err)
		}
	} else {
		var httpOpts []otlptracehttp.Option
		httpOpts, err = exporterOptionsFor(cfg)
		if err != nil {
			return nil, nil, err
		}
		exporter, err = otlptracehttp.New(ctx, httpOpts...)
		if err != nil {
			return nil, nil, fmt.Errorf("otel: failed to build otlptracehttp exporter: %w", err)
		}
	}

	setErrorHandlerOnce(logger)

	tracer, shutdown := newTracerAndShutdown(exporter, cfg, logger)
	return tracer, shutdown, nil
}

// protocolGRPC is the only cfg.Protocol value that selects the OTLP/gRPC
// exporters; every other value selects OTLP/HTTP. schemeHTTP and schemeHTTPS
// are the only URL schemes accepted in the full-URL endpoint form, shared by
// the trace, logs, and metrics option builders.
const (
	protocolGRPC = "grpc"
	schemeHTTP   = "http"
	schemeHTTPS  = "https"
)

// isGRPCProtocol reports whether cfg selects the OTLP gRPC wire protocol.
// Only the exact string "grpc" selects gRPC; every other value (including
// "", "http", and any unrecognized string) selects http. Rejecting an
// unrecognized cfg.Protocol value is config-validation's job
// (internal/config); this function only decides which already-supported
// exporter to build, so it treats an unvalidated/unrecognized value the same
// as the documented default.
func isGRPCProtocol(cfg config.OTelConfig) bool {
	return cfg.Protocol == protocolGRPC
}

// endpointForm classifies cfg.ExporterEndpoint per the two forms documented
// on pve.otel.exporter_endpoint (jobs/pve_cpi/spec): "host:port or full
// URL". It is shared by exporterOptionsFor (http) and grpcExporterOptionsFor
// (grpc) below, since both wire protocols accept the same endpoint syntax
// and must reject the same malformed inputs identically.
//
//   - If the endpoint contains a "://" scheme separator, it is treated as a
//     full URL: the scheme (which must be http or https) determines
//     transport security and cfg.Insecure is ignored, since the scheme
//     already states the operator's intent unambiguously. isURL is true.
//   - Otherwise the endpoint is treated as a bare "host:port" pair and
//     cfg.Insecure selects insecure (true) vs secure/TLS (false, the
//     default). isURL is false.
func endpointForm(cfg config.OTelConfig) (isURL bool, endpoint string, err error) {
	endpoint = strings.TrimSpace(cfg.ExporterEndpoint)

	if !strings.Contains(endpoint, "://") {
		return false, endpoint, nil
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false, "", fmt.Errorf("otel: invalid exporter_endpoint URL %q: %w", cfg.ExporterEndpoint, err)
	}
	if parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS {
		return false, "", fmt.Errorf("otel: exporter_endpoint URL %q must use http or https scheme, got %q", cfg.ExporterEndpoint, parsed.Scheme)
	}
	if parsed.Host == "" {
		return false, "", fmt.Errorf("otel: exporter_endpoint URL %q is missing a host", cfg.ExporterEndpoint)
	}
	return true, endpoint, nil
}

// exporterOptionsFor translates cfg into otlptracehttp options. See
// endpointForm for the endpoint-syntax rules shared with the grpc path.
func exporterOptionsFor(cfg config.OTelConfig) ([]otlptracehttp.Option, error) {
	isURL, endpoint, err := endpointForm(cfg)
	if err != nil {
		return nil, err
	}
	if isURL {
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts, nil
}

// grpcExporterOptionsFor translates cfg into otlptracegrpc options, the
// gRPC-protocol counterpart to exporterOptionsFor. It always sets an
// explicit endpoint option derived from cfg (WithEndpointURL or
// WithEndpoint) rather than leaving the endpoint unset, so the exporter
// never falls back to reading the ambient OTEL_EXPORTER_OTLP_ENDPOINT /
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT environment variables: cfg is the CPI's
// sole source of truth for where spans go. See endpointForm for the
// endpoint-syntax rules shared with the http path.
func grpcExporterOptionsFor(cfg config.OTelConfig) ([]otlptracegrpc.Option, error) {
	isURL, endpoint, err := endpointForm(cfg)
	if err != nil {
		return nil, err
	}
	if isURL {
		return []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(endpoint)}, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return opts, nil
}

// newTracerAndShutdown builds the TracerProvider from an already-constructed
// exporter and returns the tracer plus a shutdown func. Split out from Setup
// so tests can exercise the BatchSpanProcessor/sampler/resource wiring with
// an in-memory exporter (sdktrace/tracetest) instead of a real
// otlptracehttp/network exporter.
func newTracerAndShutdown(exporter sdktrace.SpanExporter, cfg config.OTelConfig, logger *log.Logger) (trace.Tracer, func(context.Context) error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	res := resource.NewSchemaless(semconv.ServiceNameKey.String(serviceName))
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	tracer := provider.Tracer(tracerName)

	shutdown := func(shutdownCtx context.Context) error {
		if err := provider.Shutdown(shutdownCtx); err != nil {
			logger.Warn("otel shutdown/export failed", log.ErrScrubbed(err))
			return err
		}
		return nil
	}

	return tracer, shutdown
}
