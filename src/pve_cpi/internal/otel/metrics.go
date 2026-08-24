// Metrics support for the CPI's opt-in OpenTelemetry pipeline.
//
// Metrics are disabled by default (cfg.MetricsEnabled false). When
// disabled, SetupMetrics returns a no-op Meter and a no-op shutdown func: no
// SDK MeterProvider, exporter, or network client is ever constructed.
//
// When enabled, the pipeline is:
//   - an OTLP exporter, otlpmetrichttp or otlpmetricgrpc selected by
//     cfg.Protocol ("grpc" selects gRPC; any other value, including empty,
//     selects http — defensive default, mirrors the trace provider), pointed
//     at cfg.MetricsExporterEndpoint
//   - a DELTA TemporalitySelector applied to the exporter for every
//     InstrumentKind, so a one-shot CPI process never emits a misleading
//     CUMULATIVE series that resets to zero on each invocation
//   - sdkmetric.MeterProvider with a PeriodicReader wrapping that exporter
//   - a Resource carrying service.name = cfg.ServiceName, matching the trace
//     provider's resource so both signals correlate to the same service
//
// SDK-internal errors are routed through the same otel.SetErrorHandler as
// the trace provider (set once, package-wide; see Setup).
//
// The returned shutdown func calls MeterProvider.Shutdown, which flushes any
// buffered metrics before stopping the pipeline. As with Setup, Shutdown
// does not impose its own timeout: the caller must bound the context it
// passes in.
package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// errorHandlerOnce is the single, package-wide guard for
// otelapi.SetErrorHandler. Setup (traces), SetupMetrics, and SetupLogs all
// call setErrorHandlerOnce below instead of calling otelapi.SetErrorHandler
// directly, so there is exactly one global handler install across the whole
// otel package for the lifetime of the process, regardless of which
// signal(s) are enabled or the order their Setup*/SetupLogs/SetupMetrics
// calls run in. The first call wins; a later call is a documented no-op
// (sync.Once), which is safe here because every closure installed does the
// same logger.Warn(log.ErrScrubbed(...)) work — see setErrorHandlerOnce.
var errorHandlerOnce sync.Once

// setErrorHandlerOnce installs the shared SDK error handler exactly once for
// the whole otel package, no matter how many of Setup/SetupMetrics/SetupLogs
// are called or in what order. SDK-internal errors (failed exports,
// malformed responses, etc.) are routed to logger.Warn so they land in the
// CPI's existing structured stderr stream and never touch stdout, and never
// abort a CPI action.
func setErrorHandlerOnce(logger *log.Logger) {
	errorHandlerOnce.Do(func() {
		otelapi.SetErrorHandler(otelapi.ErrorHandlerFunc(func(handlerErr error) {
			logger.Warn("otel internal error", log.ErrScrubbed(handlerErr))
		}))
	})
}

// meterName identifies this instrumentation library to the metrics backend,
// matching tracerName's convention of using the instrumenting package's
// import path.
const meterName = "github.com/fivetwenty-io/bosh-proxmox-cpi"

// deltaTemporalitySelector returns metricdata.DeltaTemporality for every
// InstrumentKind, overriding the SDK default of CumulativeTemporality.
// A short-lived, one-shot CPI process has no meaningful notion of
// a cumulative counter across invocations: each process exit resets state,
// so a collector expecting a monotonic cumulative series would be misled.
// Delta reports "what changed this export" instead, which is correct for a
// process that runs once and exits.
func deltaTemporalitySelector(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.DeltaTemporality
}

// SetupMetrics builds the metrics pipeline described by cfg and returns the
// Meter callers use to create instruments, a shutdown func the caller must
// invoke (with a bounded-deadline context) on every process exit path, and
// an error if the pipeline could not be constructed.
//
// cfg.MetricsEnabled == false is the default and returns a no-op Meter
// (every instrument-creation and recording call is a cheap no-op) plus a
// no-op shutdown — no exporter, no MeterProvider, no network dial.
//
// cfg.MetricsEnabled == true additionally requires
// cfg.MetricsExporterEndpoint to be non-empty (an empty endpoint would
// otherwise silently fall back to the exporter's ambient-env-var or
// library-default endpoint, masking an operator misconfiguration)
// and ctx to be non-nil (used to build the exporter's client).
//
// Instrument creation (e.g. the cpi.action.duration histogram) is the
// caller's responsibility; SetupMetrics only builds the provider and hands
// back the Meter used to create instruments.
func SetupMetrics(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (metric.Meter, func(context.Context) error, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	if !cfg.MetricsEnabled {
		return noop.NewMeterProvider().Meter(meterName), noopShutdown, nil
	}

	if ctx == nil {
		return nil, nil, errors.New("otel: SetupMetrics requires a non-nil context when metrics are enabled")
	}
	if strings.TrimSpace(cfg.MetricsExporterEndpoint) == "" {
		return nil, nil, errors.New("otel: metrics_exporter_endpoint must not be empty when metrics are enabled")
	}

	exporter, err := newMetricExporter(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	setErrorHandlerOnce(logger)

	meter, shutdown := newMeterAndShutdown(exporter, cfg)
	return meter, shutdown, nil
}

// newMetricExporter builds the otlpmetrichttp or otlpmetricgrpc exporter
// selected by cfg.Protocol. "grpc" selects otlpmetricgrpc; any other value
// (including empty, though ApplyDefaults normally fills "http" before this
// is reached) defensively selects otlpmetrichttp, matching the "no
// unrecognized protocol value crashes construction" posture already applied
// to the trace provider's http-only path.
func newMetricExporter(ctx context.Context, cfg config.OTelConfig) (sdkmetric.Exporter, error) {
	if cfg.Protocol == protocolGRPC {
		opts, err := metricGRPCOptionsFor(cfg)
		if err != nil {
			return nil, err
		}
		exporter, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otel: failed to build otlpmetricgrpc exporter: %w", err)
		}
		return exporter, nil
	}

	opts, err := metricHTTPOptionsFor(cfg)
	if err != nil {
		return nil, err
	}
	exporter, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel: failed to build otlpmetrichttp exporter: %w", err)
	}
	return exporter, nil
}

// metricHTTPOptionsFor translates cfg into otlpmetrichttp options, mirroring
// exporterOptionsFor's endpoint-form handling (see normalizeEndpoint,
// provider.go) and always applying the DELTA temporality selector. The
// endpoint option is always derived from cfg.MetricsExporterEndpoint —
// never left unset — so the exporter never falls back to reading ambient
// OTEL_EXPORTER_OTLP_* env vars.
func metricHTTPOptionsFor(cfg config.OTelConfig) ([]otlpmetrichttp.Option, error) {
	isURL, endpoint, err := normalizeEndpoint("metrics_exporter_endpoint", cfg.MetricsExporterEndpoint)
	if err != nil {
		return nil, err
	}

	var endpointOpt otlpmetrichttp.Option
	if isURL {
		endpointOpt = otlpmetrichttp.WithEndpointURL(endpoint)
	} else {
		endpointOpt = otlpmetrichttp.WithEndpoint(endpoint)
	}

	opts := []otlpmetrichttp.Option{
		endpointOpt,
		otlpmetrichttp.WithTemporalitySelector(deltaTemporalitySelector),
	}
	if !isURL && cfg.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	return opts, nil
}

// metricGRPCOptionsFor translates cfg into otlpmetricgrpc options, mirroring
// metricHTTPOptionsFor's endpoint-form handling and always applying the
// DELTA temporality selector. The endpoint option is always derived
// from cfg.MetricsExporterEndpoint for the same ambient-env-var-avoidance
// reason as the http path.
func metricGRPCOptionsFor(cfg config.OTelConfig) ([]otlpmetricgrpc.Option, error) {
	isURL, endpoint, err := normalizeEndpoint("metrics_exporter_endpoint", cfg.MetricsExporterEndpoint)
	if err != nil {
		return nil, err
	}

	var endpointOpt otlpmetricgrpc.Option
	if isURL {
		endpointOpt = otlpmetricgrpc.WithEndpointURL(endpoint)
	} else {
		endpointOpt = otlpmetricgrpc.WithEndpoint(endpoint)
	}

	opts := []otlpmetricgrpc.Option{
		endpointOpt,
		otlpmetricgrpc.WithTemporalitySelector(deltaTemporalitySelector),
	}
	if !isURL && cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return opts, nil
}

// newMeterAndShutdown builds the MeterProvider from an already-constructed
// exporter and returns the Meter plus a shutdown func. Split out from
// SetupMetrics so tests can exercise the PeriodicReader/temporality/resource
// wiring with an in-memory exporter instead of a real
// otlpmetrichttp/otlpmetricgrpc/network exporter.
//
// The returned shutdown func does not log a shutdown/flush failure itself:
// the caller composing this func with the trace and logs shutdown funcs
// (cmd/cpi/main.go) owns the single Warn for all three signals. Callers
// invoking this func directly (e.g. tests) must inspect the returned error
// themselves.
func newMeterAndShutdown(exporter sdkmetric.Exporter, cfg config.OTelConfig) (metric.Meter, func(context.Context) error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	res := resource.NewSchemaless(semconv.ServiceNameKey.String(serviceName))
	reader := sdkmetric.NewPeriodicReader(exporter)

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	meter := provider.Meter(meterName)

	shutdown := func(shutdownCtx context.Context) error {
		return provider.Shutdown(shutdownCtx)
	}

	return meter, shutdown
}
