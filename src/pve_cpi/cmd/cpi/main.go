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
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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

// defaultMaxLineBytes is the maximum allowed size of a single JSON-RPC request line (64 MiB).
// bufio.Scanner returns bufio.ErrTooLong if this limit is exceeded; the loop
// treats that as a decode error, writes a CloudError, and continues.
const defaultMaxLineBytes = 64 * 1024 * 1024

// defaultOTelShutdownTimeoutMs bounds the OTel shutdown flush when
// cfg.OTel.ExportTimeoutMs is unset (0). It is only ever reached when tracing
// is disabled, in which case the shutdown func is a no-op that ignores the
// deadline entirely — this value exists purely so the shutdown context always
// carries a real deadline rather than an implicit zero-duration one.
const defaultOTelShutdownTimeoutMs = 5000

// fallbackTracerName identifies the no-op tracer runCPI builds when called
// with a nil tracer. Production startup (runWithArgs) always supplies the
// non-nil tracer returned by otel.Setup (real or its own no-op), so this path
// is only reached by test call sites that pass nil to exercise runCPI/
// dispatchOne without constructing a tracer.
const fallbackTracerName = "github.com/fivetwenty-io/bosh-pve-cpi/cmd/cpi"

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

// runWithArgs contains the full startup and event loop. Accepting args, stdin,
// stdout, stderr, and opts as parameters makes every flag-parse, config-load,
// and client-init path testable without spawning a subprocess or manipulating
// os.Args.
//
// opts.ClientFactory overrides pve.NewClient when non-nil. The zero value of
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
	// Bounded shutdown flush covers every exit path downstream of this point:
	// client init failure, agent init failure, hook errors, runCPI's error
	// return, and the normal EOF return. The deadline's parent is
	// context.Background() rather than rootCtx: by the time this defer runs,
	// rootCtx may already be cancelled (e.g. by the same SIGTERM that ended
	// the request loop), which would leave zero budget to flush buffered
	// spans. A flush/export failure is logged at Warn and never changes the
	// process's exit code: the flush deadline is bounded, and an export
	// failure must never fail a CPI action.
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

	// When agent_mode="auto", the primary boot agent is always configdrive.
	// Pass a synthetic cfg copy with AgentMode="cloudinit" so factory.go's
	// default-error branch stays accurate and iso/stemcell storage resolves
	// normally. All other modes pass cfg unchanged.
	cfgForBoot := *cfg
	if cfgForBoot.AgentMode == "auto" {
		cfgForBoot.AgentMode = "cloudinit"
	}
	bootAgent, err := agent.NewAgent(&cfgForBoot, client, logger)
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

	dispatcherOpts := []func(*cpi.Dispatcher){cpi.WithHooks(hookChain...)}
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
	handlers.RegisterAll(d, handlers.Deps{
		Config:   cfg,
		PVE:      client,
		Agent:    bootAgent,
		Logger:   logger,
		Resolver: backendResolver,
		Inflight: handlers.NewInflightRegistry(),
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
// Decode errors (malformed JSON, missing "method", line too long) are non-fatal:
// runCPI logs the error, writes a CloudError JSON-RPC response to w, and continues.
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
	buf := make([]byte, maxLineBytes)
	sc.Buffer(buf, maxLineBytes)

	for {
		// Check for context cancellation between requests.
		select {
		case <-ctx.Done():
			return errSignaled
		default:
		}

		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				// Scanner error (e.g. line too long or I/O error reading r).
				logger.Warn("stdin: scan error", log.Err(err))
				cpiErr := cpierrors.Cloud("request read failed: %s", err.Error())
				if writeErr := writeErrorResponse(bw, cpiErr); writeErr != nil {
					return fmt.Errorf("cpi: write scan error response: %w", writeErr)
				}
				// Continue the loop after a scan error only if sc is still usable.
				// After ErrTooLong the scanner is not reusable, so exit cleanly.
				if errors.Is(err, bufio.ErrTooLong) {
					return nil
				}
				continue
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
		if loopErr := dispatchOne(reqCtx, bw, w, d, req, logger, span); loopErr != nil {
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
func dispatchOne(
	ctx context.Context,
	bw *bufio.Writer,
	w io.Writer,
	d *cpi.Dispatcher,
	req *jsonrpc.Request,
	logger *log.Logger,
	span trace.Span,
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
