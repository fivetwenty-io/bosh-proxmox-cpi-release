// Package main is the entrypoint for the BOSH PVE CPI binary.
//
// The binary reads JSON-RPC requests from stdin, dispatches each to the
// registered CPI method handler, and writes JSON-RPC responses to stdout.
// All log output targets stderr so it does not corrupt the JSON-RPC stream.
//
// Usage:
//
//	cpi --config <path>   # normal operation
//	cpi --version         # print version string and exit 0
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/hooks"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/otel"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/version"
)

// exitSignaled is the exit code returned when the process catches SIGINT or SIGTERM.
const exitSignaled = 130

// poolsPreflightTimeout bounds each pool-visibility probe issued by
// preflightPoolAccess so a hung or slow PVE API call cannot stall CPI
// startup indefinitely. A probe that does not return in time is classified
// as transient (Warn-only, boot proceeds) rather than blocking forever.
const poolsPreflightTimeout = 10 * time.Second

// defaultMaxLineBytes is the maximum allowed size of a single JSON-RPC request line (64 MiB).
// bufio.Scanner returns bufio.ErrTooLong if this limit is exceeded; the loop
// treats that as a decode error, writes a CloudError, and continues.
const defaultMaxLineBytes = 64 * 1024 * 1024

// defaultOTelShutdownTimeoutMs bounds the OTel shutdown flush when
// cfg.OTel.ExportTimeoutMs is unset (0). Config defaulting fills 5000 only
// when tracing is enabled, so this fallback is reached when tracing is off —
// including logs-only or metrics-only deployments, whose exporter flush is
// then bounded by the same 5000ms the traces default would give.
const defaultOTelShutdownTimeoutMs = 5000

// fallbackTracerName identifies the no-op tracer runCPI builds when called
// with a nil tracer. Production startup (runWithArgs) always supplies the
// non-nil tracer returned by otel.Setup (real or its own no-op), so this path
// is only reached by test call sites that pass nil to exercise runCPI/
// dispatchOne without constructing a tracer.
const fallbackTracerName = "github.com/fivetwenty-io/bosh-pve-cpi/cmd/cpi"

// fallbackMeterName identifies the no-op Meter runWithArgs falls back to when
// the metrics-setup factory (production: otel.SetupMetrics) returns an
// error. It mirrors fallbackTracerName's convention and role: a Meter is
// still handed to the rest of startup so nothing downstream has to nil-check
// it, but every instrument it creates is a cheap no-op (fail-open).
const fallbackMeterName = "github.com/fivetwenty-io/bosh-pve-cpi/cmd/cpi"

// noopSignalShutdown is used in place of a logs/metrics shutdown func when
// the corresponding setup factory returned a nil shutdown (defensive) or
// when its setup failed and the signal is treated as disabled for the rest
// of the process. It performs no work and never returns an error.
func noopSignalShutdown(context.Context) error { return nil }

// setupOTelLogsAndMetrics builds the opt-in logs and metrics pipelines.
// Setup errors for both are fail-open (unlike the tracer's hard-fail in
// runWithArgs, which predates these signals): a broken pve.otel.logs.* or
// pve.otel.metrics.* configuration must not brick an otherwise-healthy CPI
// invocation, since each signal is independently opt-in. A setup failure is
// logged at Warn and the affected signal is treated as disabled for the
// rest of the process (nil handler / no-op Meter, no-op shutdown).
//
// The returned logger is the process logger to use from here on: when the
// logs pipeline is up it is rebuilt via log.NewLoggerWithHandlers so every
// subsequent log call fans out to the OTel bridge; otherwise it is the
// logger passed in. The returned shutdown funcs are never nil.
func setupOTelLogsAndMetrics(
	ctx context.Context,
	cfg *config.CPIConfig,
	stderr io.Writer,
	logger *log.Logger,
	opts runOptions,
) (*log.Logger, metric.Meter, func(context.Context) error, func(context.Context) error) {
	loggerFactory := opts.LoggerProviderFactory
	if loggerFactory == nil {
		loggerFactory = otel.SetupLogs
	}
	logsHandler, logsShutdown, logsErr := loggerFactory(ctx, cfg.OTel, logger)
	if logsErr != nil {
		logger.Warn("otel logs init failed", log.Err(logsErr))
		logsHandler, logsShutdown = nil, nil
	}
	if logsShutdown == nil {
		logsShutdown = noopSignalShutdown
	}
	// A non-nil handler means logs are enabled and the pipeline was built
	// successfully: rebuild the process logger so every subsequent log call
	// fans out to the OTel bridge. cfg.LogLevel already parsed successfully
	// in runWithArgs's log.NewLogger call, so NewLoggerWithHandlers
	// re-parsing the same string cannot fail in practice; the error branch
	// is defensive only.
	if logsHandler != nil {
		if rebuilt, buildErr := log.NewLoggerWithHandlers(cfg.LogLevel, stderr, logsHandler); buildErr != nil {
			logger.Warn("otel logs: rebuilding logger with OTel handler failed", log.Err(buildErr))
		} else {
			logger = rebuilt
		}
	}

	meterFactory := opts.MeterProviderFactory
	if meterFactory == nil {
		meterFactory = otel.SetupMetrics
	}
	meter, metricsShutdown, metricsErr := meterFactory(ctx, cfg.OTel, logger)
	if metricsErr != nil {
		logger.Warn("otel metrics init failed", log.Err(metricsErr))
		meter, metricsShutdown = metricnoop.NewMeterProvider().Meter(fallbackMeterName), nil
	}
	if metricsShutdown == nil {
		metricsShutdown = noopSignalShutdown
	}

	return logger, meter, logsShutdown, metricsShutdown
}

// otelActionDurationMetricName is the sole OTel metrics instrument this CPI
// creates: a histogram of one dispatched CPI action's wall-clock
// duration, in milliseconds. No per-PVE-call metrics exist by design — spans
// already cover that at finer granularity.
const otelActionDurationMetricName = "cpi.action.duration"

// newOTelDurationRecorder adapts histogram into a cpi.WithDurationRecorder
// callback. Unlike the hook this replaced, the callback performs no outcome
// classification itself: cpi.Dispatcher.Handle already computes the final
// outcome ("success", "error", or "marshal_error" — the last reachable only
// once a handler's result fails json.Marshal, a step that runs after the
// wrapped handler and its hooks have already returned, so no hook could ever
// see it) and calls this exactly once per dispatched request, after every
// reclassification Handle itself performs (including the per-method timeout
// rewrite, which Handle still records here as "error" — the same value the
// wrapped handler's hooks observed before Handle turned it into a retriable
// timeout response — and a recovered handler panic, likewise recorded as
// "error" to match the error response the Director receives). ctx is
// Handle's own request ctx (its own span, if any,
// used for exemplar correlation only — this call never dials out with it),
// mirroring how the hook this replaced recorded against the handler's ctx; it
// may already be canceled by the time some outcomes are recorded (timeout,
// marshal_error), which is expected and harmless for a Record call. It is
// installed only when pve.otel.metrics.enabled is true; when disabled, no
// histogram is created and no recorder is passed to the dispatcher (see
// runWithArgs), so a metrics-disabled deployment pays zero per-call overhead
// from this callback.
func newOTelDurationRecorder(histogram metric.Float64Histogram) func(ctx context.Context, method, outcome string, durationMs float64) {
	return func(ctx context.Context, method, outcome string, durationMs float64) {
		histogram.Record(ctx, durationMs, metric.WithAttributes(
			attribute.String("cpi.method", method),
			attribute.String("outcome", outcome),
		))
	}
}

func main() {
	os.Exit(run())
}

// runOptions holds optional overrides for runWithArgs. The zero value is valid
// and selects production defaults for every field.
type runOptions struct {
	// ClientFactory constructs the PVE client from a loaded config and the
	// tracer resolved via TracerFactory. When nil, runWithArgs uses
	// pve.NewClientWithTracer. Tests inject a factory that returns a
	// nilPVEClient to exercise the pve.NewClientWithTracer error path without
	// a live PVE. tracer is never nil in production (otel.Setup always
	// returns a real-or-no-op trace.Tracer); a no-op tracer decorates PVE
	// service calls with zero added overhead, identical to the pre-tracing
	// behavior.
	ClientFactory func(cfg *config.CPIConfig, logger *log.Logger, tracer trace.Tracer) (pve.Client, error)

	// TracerFactory constructs the OTel tracer and its bounded shutdown func.
	// When nil, runWithArgs uses otel.Setup. Tests inject a factory backed by
	// an in-memory tracetest exporter to assert exported root spans and
	// log/trace correlation without a network collector, or to force a
	// shutdown error to prove it never changes the process exit code.
	TracerFactory func(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (trace.Tracer, func(context.Context) error, error)

	// LoggerProviderFactory constructs the OTel logs bridge slog.Handler and
	// its bounded shutdown func, mirroring TracerFactory's injection seam.
	// When nil, runWithArgs uses otel.SetupLogs. A nil returned handler (the
	// disabled-path default, and the fail-open outcome of a setup error) means
	// the process logger keeps its pre-OTel handler unmodified. Tests inject a
	// factory returning a spy slog.Handler to assert the process logger fans
	// records out to it once logs are enabled, or one that returns an error to
	// prove a logs-setup failure never fails the CPI (the signal is simply
	// treated as disabled and startup continues).
	LoggerProviderFactory func(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (slog.Handler, func(context.Context) error, error)

	// MeterProviderFactory constructs the OTel Meter used to create the
	// cpi.action.duration histogram, and its bounded shutdown func, mirroring
	// TracerFactory's injection seam. When nil, runWithArgs uses
	// otel.SetupMetrics. Tests inject a factory backed by an in-memory metric
	// reader to assert the histogram records the correct method/outcome
	// attributes, or one that returns an error to prove a metrics-setup
	// failure never fails the CPI (fail-open: metrics are treated as disabled
	// and startup continues with a no-op Meter).
	MeterProviderFactory func(ctx context.Context, cfg config.OTelConfig, logger *log.Logger) (metric.Meter, func(context.Context) error, error)

	// MaxLineBytes overrides the per-request line size cap passed to runCPI.
	// Zero selects defaultMaxLineBytes (64 MiB). Tests set a small value (e.g.
	// 4 KiB) to keep oversized payloads manageable without mutating a package var.
	MaxLineBytes int
}

// run is the thin os-wired entry point. It delegates to runWithArgs so the
// startup logic is testable in-process without spawning a subprocess.
func run() int {
	return runWithArgs(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, runOptions{})
}

// preflightPoolAccess probes PVE resource-pool visibility for every
// statically named pool layer (pve.vm_pool, pve.stemcell_template_pool)
// before the CPI serves its first request. Both default on ("bosh" /
// "bosh-templates" per jobs/pve_cpi/spec), so this runs for a zero-config
// deployment unless the operator explicitly opts both out. The
// pve.vm_pool_template layer (default on, "bosh-{director}-{deployment}")
// cannot be probed here: its names are rendered per create_vm call and do
// not exist until first use, so a missing /pool grant for those surfaces as
// a named, non-retriable create_vm error (see ensureResolvedPool in
// internal/cpi/handlers) rather than at boot.
//
// Design: the cheapest side-effect-free signal that proves the CPI's
// identity can read a pool path is GET /pools/{poolid}
// (pve.PoolService.GetPoolComment) issued against each configured pool
// name -- it exercises exactly the Pool.Audit grant PVE checks per-pool,
// without ever creating or mutating anything (pool creation is deferred to
// pve.EnsurePoolExists at first real use, inside create_vm/create_stemcell).
// There is no side-effect-free way to probe Pool.Allocate specifically --
// PVE only enforces it on a mutating call -- so a clean probe here proves
// read access only; the failure message below still names both grants so an
// operator fixes both at once instead of discovering the Allocate gap
// separately on the first real create_vm/create_stemcell call.
//
// A pool that does not exist yet is NOT a failure: GetPoolComment maps both
// PVE not-found shapes -- a 404, and the 500-with-text "pool 'x' does not
// exist" live PVE actually returns -- to (found=false, err=nil), and the CPI
// creates the pool lazily on first use. That case is logged at Debug and says
// so; it is the normal state of a zero-config first boot, where neither
// default pool exists yet, and must never look like an API fault.
// Only a classified permission error (pve.IsPoolPermissionDenied: HTTP
// 401/403) fails this preflight. Every other error (network fault, PVE 5xx,
// context deadline) is logged at Warn and treated as transient, so a
// startup-time PVE hiccup never blocks the CPI from booting -- the same
// fault simply resurfaces on the first real pool-touching request, where the
// normal per-request retry/error path takes over.
//
// Skipped entirely (zero PVE calls) when both pve.vm_pool and
// pve.stemcell_template_pool resolve to "" -- the operator has opted out of
// pool assignment entirely, so there is nothing to probe. Also a no-op when
// cfg, logger, client, or client.Pools() is nil (defensive: should never
// happen once clientFactory has already succeeded, but a wiring gap must
// never panic or block startup).
func preflightPoolAccess(ctx context.Context, cfg *config.CPIConfig, client pve.Client, logger *log.Logger) error {
	if cfg == nil || logger == nil || client == nil || client.Pools() == nil {
		return nil
	}
	if cfg.VMPool == "" && cfg.StemcellTemplatePool == "" {
		return nil
	}

	pools := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	for _, p := range []string{cfg.VMPool, cfg.StemcellTemplatePool} {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		pools = append(pools, p)
	}

	for _, poolID := range pools {
		probeCtx, cancel := context.WithTimeout(ctx, poolsPreflightTimeout)
		_, found, err := client.Pools().GetPoolComment(probeCtx, poolID)
		cancel()
		if err == nil {
			if !found {
				logger.Debug("pools preflight: pool does not exist yet; it will be created on first use",
					log.String("pool", poolID))
				continue
			}
			logger.Debug("pools preflight: pool visible", log.String("pool", poolID))
			continue
		}
		if pve.IsPoolPermissionDenied(err) {
			return cpierrors.Cloud(
				"pools preflight: PVE denied read access to pool %q: %s -- grant the CPI's "+
					"identity Pool.Audit AND Pool.Allocate on /pool/%s (Pool.Allocate cannot be "+
					"probed here without a mutating call, but is required by create_vm and "+
					"create_stemcell the first time either assigns a VM to this pool); "+
					"alternatively, set pve.vm_pool: \"\" and pve.stemcell_template_pool: \"\" "+
					"to disable pool assignment entirely",
				poolID, err.Error(), poolID)
		}
		logger.Warn("pools preflight: could not confirm pool access (non-fatal; treating as transient, boot continuing)",
			log.String("pool", poolID), log.Err(err))
	}
	return nil
}

// runWithArgs contains the full startup and event loop. Accepting args, stdin,
// stdout, stderr, and opts as parameters makes every flag-parse, config-load,
// and client-init path testable without spawning a subprocess or manipulating
// os.Args.
//
// opts.ClientFactory overrides pve.NewClientWithTracer when non-nil. The zero value of
// runOptions selects production defaults, so production callers pass runOptions{}.
//
// Returning an int rather than calling os.Exit directly ensures that all
// deferred calls (including signal.NotifyContext's cancel) fire before the
// process exits.
func runWithArgs(args []string, stdin io.Reader, stdout, stderr io.Writer, opts runOptions) int {
	fs := flag.NewFlagSet("cpi", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to CPI JSON config file (required)")
	showVersion := fs.Bool("version", false, "print version string and exit 0")

	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError writes usage; add context line for clarity.
		fmt.Fprintf(stderr, "cpi: flag parse: %s\n", err)
		return 1
	}
	// Reject unexpected positional arguments.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "cpi: unexpected arguments: %v\n", fs.Args())
		fs.Usage()
		return 1
	}

	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}

	if *configPath == "" {
		fmt.Fprintln(stderr, "cpi: --config is required")
		fs.Usage()
		return 1
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "cpi: config load failed: %s\n", err)
		return 1
	}

	logger, err := log.NewLogger(cfg.LogLevel, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "cpi: logger init failed: %s\n", err)
		return 1
	}

	// Root context cancelled on SIGINT/SIGTERM. defer cancel() fires when
	// runWithArgs returns, deregistering the signal handler and releasing
	// resources even though main calls os.Exit — the exit is deferred until
	// after run() returns. Built before tracer setup because otel.Setup dials
	// the exporter's HTTP client against this ctx when tracing is enabled.
	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	tracerFactory := opts.TracerFactory
	if tracerFactory == nil {
		tracerFactory = otel.Setup
	}
	tracer, otelShutdown, err := tracerFactory(rootCtx, cfg.OTel, logger)
	if err != nil {
		logger.Error("otel tracer init failed", log.Err(err))
		return 1
	}

	logger, meter, logsShutdown, metricsShutdown := setupOTelLogsAndMetrics(rootCtx, cfg, stderr, logger, opts)

	// Bounded shutdown flush covers every exit path downstream of this point:
	// client init failure, agent init failure, hook errors, runCPI's error
	// return, and the normal EOF return. Each signal's shutdown gets its own
	// freshly bounded deadline (rather than sharing one context/deadline
	// across all three) so a slow or hung collector on one signal cannot
	// starve the flush budget of another. The deadline's parent is
	// context.Background() rather than rootCtx: by the time this defer runs,
	// rootCtx may already be cancelled (e.g. by the same SIGTERM that ended
	// the request loop), which would leave zero budget to flush buffered
	// spans/logs/metrics. A flush/export failure is logged at Warn and never
	// changes the process's exit code: every deadline is bounded, and an
	// export failure must never fail a CPI action.
	defer func() {
		timeoutMs := cfg.OTel.ExportTimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = defaultOTelShutdownTimeoutMs
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
		defer shutdownCancel()
		if shutdownErr := otelShutdown(shutdownCtx); shutdownErr != nil {
			logger.Warn("otel shutdown/flush failed", log.ErrScrubbed(shutdownErr))
		}

		logsShutdownCtx, logsShutdownCancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
		defer logsShutdownCancel()
		if shutdownErr := logsShutdown(logsShutdownCtx); shutdownErr != nil {
			logger.Warn("otel logs shutdown/flush failed", log.ErrScrubbed(shutdownErr))
		}

		metricsShutdownCtx, metricsShutdownCancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
		defer metricsShutdownCancel()
		if shutdownErr := metricsShutdown(metricsShutdownCtx); shutdownErr != nil {
			logger.Warn("otel metrics shutdown/flush failed", log.ErrScrubbed(shutdownErr))
		}
	}()

	clientFactory := opts.ClientFactory
	if clientFactory == nil {
		clientFactory = pve.NewClientWithTracer
	}
	// With tracing disabled, hand the client a nil tracer rather than the
	// no-op one: NewClientWithTracer skips the decorator layer entirely on
	// nil, so a tracing-off deployment routes PVE calls through the exact
	// same code path as before tracing existed.
	clientTracer := tracer
	if !cfg.OTelEnabled() {
		clientTracer = nil
	}
	client, err := clientFactory(cfg, logger, clientTracer)
	if err != nil {
		logger.Error("pve client init failed", log.Err(err))
		return 1
	}

	// Fail fast when the configured pool layers (pve.vm_pool,
	// pve.stemcell_template_pool -- both default on) are not readable by the
	// CPI's identity, naming the exact grant to fix, instead of surfacing an
	// opaque permission error on the first create_vm/create_stemcell call.
	// See preflightPoolAccess's doc comment for the probe design and the
	// fail-fast (permission) vs Warn-only (transient) classification.
	if pfErr := preflightPoolAccess(rootCtx, cfg, client, logger); pfErr != nil {
		logger.Error("pools preflight failed", log.Err(pfErr))
		return 1
	}

	// Resolve pve.iso_storage_follow_vm_storage (default ON; explicit
	// false opts out) once, in place on cfg,
	// before any Deps or agent are built downstream. Mutating cfg directly
	// (not a copy) keeps every consumer — the boot agent, and deps.Config
	// read by create_vm's HA migration-safety check — looking at the same
	// effective iso_storage value. A no-op (zero PVE calls) when the operator
	// explicitly disabled the flag, or when iso_storage was pinned to
	// anything other than the spec default "local".
	cfg.ISOStorage = agent.ResolveISOStorage(rootCtx, cfg, client, logger)

	// When agent_mode="auto", the primary boot agent is always configdrive.
	// Pass a synthetic cfg copy with the normalized boot mode so factory.go's
	// default-error branch stays accurate and iso/stemcell storage resolves
	// normally. All other modes pass cfg unchanged.
	cfgForBoot := *cfg
	cfgForBoot.AgentMode = cfg.BootAgentMode()
	// Direct-to-node upload routing: explicit pve.node_endpoints entries win,
	// /cluster/status discovery fills the rest when TLS verification is off
	// (the discovered corosync IP is usually absent from stock node cert
	// SANs). Shared by the boot agent (ConfigDrive ISO uploads) and Deps
	// (stemcell image uploads).
	nodeEndpoints := pve.NewNodeEndpointResolver(client, cfg.NodeEndpoints, cfg.Host, !cfg.VerifySSLValue(), logger)
	bootAgent, err := agent.NewAgent(&cfgForBoot, client, nodeEndpoints, logger)
	if err != nil {
		logger.Error("agent init failed", log.Err(err))
		return 1
	}

	// 60s TTL keeps the /storage index fresh enough to catch operator-driven
	// storage.cfg edits between deploys while shielding a busy create_disk
	// burst from per-call lookups.
	storageInfoCache := pve.NewStorageInfoCache(client.ClusterStorage(), 60*time.Second)
	backendResolver := pve.NewBackendResolver(client, storageInfoCache, cfg.Node)

	// Resolve configured middleware hooks via the registry. config.Validate has
	// already rejected unknown names, so a miss here is defensive only.
	hookDeps := hooks.Deps{
		Logger:          logger,
		LBRegister:      cfg.LBRegisterConfig(),
		ExternalCommand: cfg.ExternalCommandConfig(),
		Annotator:       handlers.NewVMAnnotator(handlers.Deps{Config: cfg, PVE: client, Logger: logger}),
		Metrics:         cfg.MetricsConfig(),
	}
	var hookChain []cpi.Hook
	for _, name := range cfg.HooksValue() {
		ctor, ok := hooks.Registry[name]
		if !ok {
			logger.Error("unknown hook configured", log.String("hook", name))
			return 1
		}
		hookChain = append(hookChain, ctor(hookDeps))
	}
	// Metrics hook is registered outside the named-hooks list: it is controlled
	// via a dedicated pve.metrics block rather than pve.hooks, so it is always
	// active when enabled regardless of hook ordering.
	if cfg.MetricsConfig() != nil {
		mh, mhErr := hooks.NewMetricsHook(hookDeps)
		if mhErr != nil {
			logger.Error("metrics hook init failed", log.Err(mhErr))
			return 1
		}
		hookChain = append(hookChain, mh)
		logger.Info("metrics hook enabled", log.String("path", cfg.MetricsConfig().FilePath))
	}
	// OTel action-duration metric (cpi.action.duration) is opt-in per
	// pve.otel.metrics.enabled, independent of the file-based metrics hook
	// above. Off (the default): no histogram is created and no recorder is
	// installed, so a metrics-disabled deployment pays zero per-call overhead
	// beyond this one bool check. Wired into the dispatcher via
	// WithDurationRecorder (below, with dispatcherOpts) rather than the hook
	// chain: a hook's After runs inside the wrapped handler call, before
	// Handle's own post-handler steps (the timeout rewrite and the
	// json.Marshal of the result), so it can never observe a marshal failure.
	var otelDurationRecorder func(ctx context.Context, method, outcome string, durationMs float64)
	if cfg.OTelMetricsEnabled() {
		histogram, histErr := meter.Float64Histogram(
			otelActionDurationMetricName,
			metric.WithUnit("ms"),
			metric.WithDescription("Duration of one dispatched CPI action, in milliseconds."),
		)
		if histErr != nil {
			logger.Warn("otel metrics: cpi.action.duration histogram init failed", log.Err(histErr))
		} else {
			otelDurationRecorder = newOTelDurationRecorder(histogram)
			logger.Info("otel metrics: cpi.action.duration histogram enabled")
		}
	}

	// Apply the operator's task-poll cadence process-wide before serving. With
	// an unset retry.task_poll block these resolve to the shipped defaults, so
	// polling is identical to prior releases.
	tp := cfg.RetryTaskPoll()
	pve.ConfigureTaskPolling(tp.BaseMs, tp.CapMs, tp.JitterPct)

	// Enable progress-aware adaptive task polling when the operator opts in
	// (§7.28). Default off → fixed-cadence polling, byte-identical to prior
	// releases.
	pve.ConfigureAdaptiveTaskPoll(cfg.TaskPollAdaptiveEnabled())

	// Apply the operator's pushback-backoff curve process-wide. With an unset
	// retry.pushback block these resolve to the shipped defaults (5s/60s), so
	// backoff is byte-identical to prior releases.
	pb := cfg.RetryPushback()
	pve.ConfigurePushbackBackoff(pb.BaseMs, pb.CapMs)

	// Apply the operator's storage-lock backoff curve process-wide. With an
	// unset retry.storage_lock block these resolve to the shipped defaults
	// (2s/30s/30%), so backoff is byte-identical to prior releases.
	sl := cfg.RetryStorageLock()
	pve.ConfigureStorageLockBackoff(sl.BaseMs, sl.CapMs, sl.JitterPct)

	// Apply the operator's transient attempt budget process-wide. With an
	// unset retry.transient block this is 0 and the shipped default
	// (DefaultTransientMaxAttempts) stays in force.
	pve.ConfigureTransientRetry(cfg.RetryTransientMaxAttempts())

	// Apply the operator's storage upload attempt budget process-wide. With
	// an unset retry.storage_upload block this is 0 and the shipped default
	// (DefaultStorageUploadMaxAttempts) stays in force.
	pve.ConfigureStorageUploadRetry(cfg.RetryStorageUploadMaxAttempts())

	// The transient classifier backs dispatchError's last-resort fallback:
	// a raw transport error a handler failed to classify still surfaces as
	// retriable. Wired here because internal/cpi cannot import internal/pve.
	dispatcherOpts := []func(*cpi.Dispatcher){
		cpi.WithHooks(hookChain...),
		cpi.WithTransientClassifier(pve.IsTransientTransport),
	}
	if otelDurationRecorder != nil {
		dispatcherOpts = append(dispatcherOpts, cpi.WithDurationRecorder(otelDurationRecorder))
	}
	// Per-method deadline envelope is opt-in. When enabled, install a resolver
	// sized by the operator's per-class budgets so a wedged operation converts
	// into a retriable timeout instead of holding a Director queue slot forever.
	if cfg.OperationTimeoutEnabled() {
		dispatcherOpts = append(dispatcherOpts, cpi.WithMethodTimeouts(
			cpi.NewMethodTimeoutResolver(
				time.Duration(cfg.OperationTimeoutCreateSec())*time.Second,
				time.Duration(cfg.OperationTimeoutDeleteSec())*time.Second,
				time.Duration(cfg.OperationTimeoutQuerySec())*time.Second,
				time.Duration(cfg.OperationTimeoutDefaultSec())*time.Second,
			),
		))
		logger.Info("operation timeout envelope enabled",
			log.Int("create_sec", cfg.OperationTimeoutCreateSec()),
			log.Int("delete_sec", cfg.OperationTimeoutDeleteSec()),
			log.Int("query_sec", cfg.OperationTimeoutQuerySec()),
			log.Int("default_sec", cfg.OperationTimeoutDefaultSec()),
		)
	}
	// Redacted request/response trace is opt-in. When enabled, the dispatcher
	// emits a debug-level, credential-masked trace of each call's payloads. Off
	// (default) adds no log records — byte-identical to prior releases.
	if cfg.RedactLogsEnabled() {
		dispatcherOpts = append(dispatcherOpts, cpi.WithRequestTrace(true))
		logger.Info("redacted request/response trace enabled")
	}
	d := cpi.NewDispatcherWithOptions(logger, dispatcherOpts...)

	// Per-request pve_* context config overrides (BOSH cpi-config multi-CPI
	// support — see internal/cpi/handlers/context_override.go). The
	// ClientFactory closure reuses the exact clientFactory/clientTracer pair
	// already resolved above for the job-level client, so an overridden
	// request's PVE client is decorated identically (tracing on/off,
	// injected test factory) to the job-level one — only cfg and logger vary
	// per call.
	overrideRuntime := &handlers.RequestOverrideRuntime{
		ClientFactory: func(effCfg *config.CPIConfig, effLogger *log.Logger) (pve.Client, error) {
			return clientFactory(effCfg, effLogger, clientTracer)
		},
		Logger:   logger,
		BaseHost: cfg.Host,
	}

	handlers.RegisterAll(d, handlers.Deps{
		Config:        cfg,
		PVE:           client,
		Agent:         bootAgent,
		Logger:        logger,
		Resolver:      backendResolver,
		NodeEndpoints: nodeEndpoints,
		Inflight:      handlers.NewInflightRegistry(),
		Overrides:     overrideRuntime,
	})

	maxLine := opts.MaxLineBytes
	if maxLine <= 0 {
		maxLine = defaultMaxLineBytes
	}

	runErr := runCPI(rootCtx, stdin, stdout, d, logger, maxLine, tracer)
	if runErr != nil {
		if errors.Is(runErr, errSignaled) {
			return exitSignaled
		}
		logger.Error("cpi loop terminated with error", log.Err(runErr))
		return 1
	}
	return 0
}

// errSignaled is returned by runCPI when the parent context is cancelled by a signal.
var errSignaled = errors.New("cpi: terminated by signal")

// runCPI is the testable inner loop. It reads JSON-RPC requests from r until
// EOF or ctx cancellation, dispatches each via the cpi.Dispatcher, and writes
// responses to w.
//
// Input handling: r is scanned line-by-line (newline-delimited JSON per the BOSH
// spec). Each line is decoded independently with a fresh json.Decoder so a
// malformed line does not corrupt the decoder state for subsequent lines. Empty
// lines are skipped silently.
//
// Inputs:
//   - ctx: cancelled by SIGINT/SIGTERM; cancellation finishes the current request then returns errSignaled.
//   - r: JSON-RPC request stream (typically os.Stdin).
//   - w: JSON-RPC response stream (typically os.Stdout); wrapped in bufio.Writer and flushed after each response.
//   - d: dispatcher with handlers pre-registered.
//   - logger: structured logger; all output goes to the sink configured in NewLogger (stderr).
//   - tracer: starts one root span per CPI action, named after req.Method. A
//     nil tracer (test call sites only; production always passes the
//     non-nil tracer otel.Setup returns) falls back to a local no-op tracer
//     so runCPI never dereferences a nil interface value.
//
// Return values:
//   - nil on clean EOF.
//   - errSignaled when ctx is done before EOF.
//   - Other errors indicate unrecoverable I/O failures writing to w.
//
// Decode errors (malformed JSON, missing "method") are non-fatal: runCPI logs
// the error, writes a CloudError JSON-RPC response to w, and continues. Scan
// errors (line too long, I/O failure reading r) are terminal: bufio.Scanner
// never recovers from a read error, so runCPI writes one CloudError response
// and returns instead of spinning on the dead scanner.
func runCPI(
	ctx context.Context,
	r io.Reader,
	w io.Writer,
	d *cpi.Dispatcher,
	logger *log.Logger,
	maxLineBytes int,
	tracer trace.Tracer,
) error {
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer(fallbackTracerName)
	}
	bw := bufio.NewWriter(w)

	// Scan line-by-line so a malformed line never corrupts the decoder state
	// for subsequent valid lines (json.Decoder does not recover from syntax errors).
	sc := bufio.NewScanner(r)
	// Small initial buffer, grown on demand up to the maxLineBytes ceiling —
	// Scanner.Buffer treats the slice as the initial buffer only, so eagerly
	// allocating the full 64 MiB would cost that much RSS on every process
	// spawn for requests that are almost always a few KiB.
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	// A blocked read(2) on stdin does not observe context cancellation, so a
	// SIGTERM arriving while the CPI is idle between requests would otherwise
	// leave the process unkillable short of SIGKILL. Closing the read side
	// unblocks sc.Scan(); the scanner then reports an error or EOF, both of
	// which terminate the loop below.
	if c, ok := r.(io.Closer); ok {
		stopClose := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stopClose()
	}

	for {
		// Check for context cancellation between requests.
		select {
		case <-ctx.Done():
			return errSignaled
		default:
		}

		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				// A SIGTERM-triggered stdin close (see AfterFunc above)
				// surfaces here as a read error — that is clean shutdown,
				// not a failure.
				if ctx.Err() != nil {
					return errSignaled
				}
				// Scanner error (line too long or I/O error reading r).
				// bufio.Scanner is single-shot after ANY read error: once
				// its internal err is set, every subsequent Scan() returns
				// false immediately with the same error, so re-entering the
				// loop would spin hot writing the identical CloudError
				// forever. Report once and exit cleanly.
				logger.Warn("stdin: scan error", log.Err(err))
				cpiErr := cpierrors.Cloud("request read failed: %s", err.Error())
				if writeErr := writeErrorResponse(bw, cpiErr); writeErr != nil {
					return fmt.Errorf("cpi: write scan error response: %w", writeErr)
				}
				return nil
			}
			// sc.Scan() returned false with no error — clean EOF.
			return nil
		}

		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			// Skip blank lines (BOSH Director sometimes emits trailing newlines).
			continue
		}

		req, decErr := decodeRequest(line)
		if decErr != nil {
			logger.Warn("jsonrpc: decode error", log.Err(decErr))
			cpiErr := cpierrors.Cloud("request decode failed: %s", decErr.Error())
			if writeErr := writeErrorResponse(bw, cpiErr); writeErr != nil {
				return fmt.Errorf("cpi: write decode error response: %w", writeErr)
			}
			continue
		}

		// Root span for this CPI action, named after the JSON-RPC method (e.g.
		// "create_vm"), carrying a request_id attribute. Started before
		// log.WithContext so the per-request logger's trace_id/span_id
		// extraction (log.go WithContext) sees a span-carrying ctx — building
		// the logger before the span exists would silently drop trace
		// correlation from every handler log line.
		reqCtx, span := tracer.Start(ctx, req.Method, trace.WithAttributes(
			attribute.String("request_id", req.Context.RequestID),
		))

		// Build a per-request context carrying request_id and method for structured logging.
		reqCtx = log.WithRequestID(reqCtx, req.Context.RequestID)
		reqCtx = log.WithMethod(reqCtx, req.Method)
		reqLogger := logger.WithContext(reqCtx)
		reqCtx = log.IntoContext(reqCtx, reqLogger)

		// dispatchOne handles the request, ends the root span (before any
		// response bytes reach stdout), and writes the response to bw.
		// A deferred recover here is a backstop for panics that occur outside the
		// dispatcher's own recover (e.g., in writeResponse or helper code called
		// before d.Handle). The dispatcher already recovers handler panics; this
		// catches anything else in the per-request path.
		// w is passed alongside bw so the backstop can write a clean CloudError
		// to a fresh bufio.Writer if bw's internal state was corrupted by a panic
		// mid-flush.
		if loopErr := dispatchOne(reqCtx, bw, w, d, req, logger, span, tracer); loopErr != nil {
			return loopErr
		}

		// Check for signal after completing the request. This ensures the current
		// request always runs to completion even when SIGINT/SIGTERM arrives mid-flight.
		select {
		case <-ctx.Done():
			return errSignaled
		default:
		}
	}
}

// decodeRequest parses a single JSON-RPC request from a line of bytes.
// Returns a wrapped error for malformed JSON or a missing "method" field.
func decodeRequest(line []byte) (*jsonrpc.Request, error) {
	var req jsonrpc.Request
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("jsonrpc: decode request: %w", err)
	}
	if req.Method == "" {
		return nil, errors.New("jsonrpc: request missing required field \"method\"")
	}
	return &req, nil
}

// writeResponse encodes resp to bw and flushes. When resp carries an error body
// it uses EncodeError; otherwise EncodeSuccess.
func writeResponse(bw *bufio.Writer, resp *jsonrpc.Response) error {
	var encErr error
	if resp.Error != nil {
		encErr = jsonrpc.EncodeError(bw, resp.Error.Type, resp.Error.Message, resp.Error.OkToRetry, resp.Log)
	} else {
		encErr = jsonrpc.EncodeSuccess(bw, resp.Result, resp.Log)
	}
	if encErr != nil {
		return encErr
	}
	return bw.Flush()
}

// writeErrorResponse encodes a *cpierrors.Error as a CloudError JSON-RPC response to bw and flushes.
func writeErrorResponse(bw *bufio.Writer, e *cpierrors.Error) error {
	if err := jsonrpc.EncodeError(bw, string(e.Type()), e.Error(), e.OkToRetry(), ""); err != nil {
		return err
	}
	return bw.Flush()
}

// endRootSpanErr finishes span, marking it Error (RecordError + SetStatus)
// when err is non-nil, then ends it. Ending an already-ended span is a
// documented no-op in the OTel SDK (recordingSpan.End checks isRecording
// before doing any work), so callers may safely invoke this more than once
// for the same span — the second call neither re-exports nor overwrites the
// status recorded by the first.
// Error text is scrubbed before it leaves the process: response errors can
// echo PVE-returned values embedding token-bearing URLs, and the span
// exporter must apply the same scrubbing the logs do.
func endRootSpanErr(span trace.Span, err error) {
	if err != nil {
		msg := log.ScrubMessage(err.Error())
		span.RecordError(errors.New(msg))
		span.SetStatus(codes.Error, msg)
	}
	span.End()
}

// endRootSpan finishes the CPI action's root span (started in runCPI) based
// on the dispatcher's response. cpi.Dispatcher.Handle already converts a
// recovered handler panic into resp.Error before returning, so this single
// check covers both "handler error" and "handler panic" per the tracing
// acceptance criteria. Must be called before any response bytes reach
// stdout — callers call it immediately after d.Handle returns, ahead of
// writeResponse.
func endRootSpan(span trace.Span, resp *jsonrpc.Response) {
	var err error
	if resp != nil && resp.Error != nil {
		err = fmt.Errorf("%s: %s", resp.Error.Type, resp.Error.Message)
	}
	endRootSpanErr(span, err)
}

// dispatchOne dispatches req through d, ends span before any response bytes
// reach stdout, writes the response to bw, and returns any write error. It
// also catches panics that occur outside the dispatcher's own recover (e.g.,
// in writeResponse) and converts them to a non-retriable CloudError. When a
// panic occurs mid-write, bw may have partial buffered bytes in an
// indeterminate state; the backstop discards those bytes by resetting bw
// against w (the underlying writer) before writing the CloudError, ensuring a
// clean JSON-RPC response reaches the Director rather than a partial + error
// concatenation.
//
// tracer is used only by the recover block below to emit a fresh
// cpi.response_write_failure span: span (the request's root span) is already
// ended by endRootSpan (below, before writeResponse runs) whenever the panic
// originates in writeResponse, so mutating span further is a documented no-op.
func dispatchOne(
	ctx context.Context,
	bw *bufio.Writer,
	w io.Writer,
	d *cpi.Dispatcher,
	req *jsonrpc.Request,
	logger *log.Logger,
	span trace.Span,
	tracer trace.Tracer,
) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			logger.Error("cpi loop panic recovered",
				log.String("method", req.Method),
				log.String("request_id", req.Context.RequestID),
				log.Any("panic", r),
				log.String("stack", string(stack)),
			)
			cpiErr := cpierrors.Cloud(
				"panic in %s [request_id=%s]: %v",
				req.Method, req.Context.RequestID, r,
			)
			// This panic occurs after the normal-path d.Handle/endRootSpan call
			// below (a panic inside d.Handle itself is already recovered by the
			// dispatcher's own defer, never reaching here), so span is normally
			// already ended with the correct status; endRootSpanErr's second call
			// is a documented no-op in that case. If a panic instead occurs before
			// d.Handle completes (e.g. a bug in ctx/req handling ahead of it), this
			// is the span's only chance to record the error before it ends.
			endRootSpanErr(span, cpiErr)
			// span is normally already ended (see above), so the write failure
			// itself would otherwise leave zero trace of the failure: emit a fresh
			// span dedicated to it.
			recordResponseWriteFailureSpan(ctx, tracer, logger, req, cpiErr)
			// Reset bw against the underlying writer to discard any bytes that were
			// buffered before the panic. Without this, a partial response followed by
			// the CloudError would produce a malformed concatenated output.
			bw.Reset(w)
			if writeErr := writeErrorResponse(bw, cpiErr); writeErr != nil {
				retErr = fmt.Errorf("cpi: write panic error response: %w", writeErr)
			}
		}
	}()

	resp := d.Handle(ctx, req)
	endRootSpan(span, resp)
	if writeErr := writeResponse(bw, resp); writeErr != nil {
		return fmt.Errorf("cpi: write response: %w", writeErr)
	}
	return nil
}

// recordResponseWriteFailureSpan starts and immediately ends a fresh
// "cpi.response_write_failure" span carrying Error status, a
// log.ScrubMessage-scrubbed status/error message, and request_id/cpi.method
// attributes matching the root span's own attribute conventions. It exists
// because by the time dispatchOne's recover block runs for a writeResponse
// panic, the request's root span has normally already been ended by
// endRootSpan (called before writeResponse) — RecordError/SetStatus on an
// already-ended span are documented no-ops (see endRootSpanErr) — so a fresh
// span is the only way the write failure reaches the exported trace.
//
// This is best-effort telemetry emitted from inside an already-recovering
// panic path: it must never itself panic or otherwise alter the CloudError
// response dispatchOne's caller writes to stdout, so any panic here is
// recovered and logged rather than propagated.
func recordResponseWriteFailureSpan(
	ctx context.Context,
	tracer trace.Tracer,
	logger *log.Logger,
	req *jsonrpc.Request,
	panicErr error,
) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("cpi.response_write_failure span emission panicked",
				log.String("request_id", req.Context.RequestID),
				log.Any("panic", r),
			)
		}
	}()
	if tracer == nil {
		return
	}
	msg := log.ScrubMessage(panicErr.Error())
	_, failSpan := tracer.Start(ctx, "cpi.response_write_failure", trace.WithAttributes(
		attribute.String("request_id", req.Context.RequestID),
		attribute.String("cpi.method", req.Method),
	))
	failSpan.RecordError(errors.New(msg))
	failSpan.SetStatus(codes.Error, msg)
	failSpan.End()
}
