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

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/hooks"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/version"
)

// exitSignaled is the exit code returned when the process catches SIGINT or SIGTERM.
const exitSignaled = 130

// defaultMaxLineBytes is the maximum allowed size of a single JSON-RPC request line (64 MiB).
// bufio.Scanner returns bufio.ErrTooLong if this limit is exceeded; the loop
// treats that as a decode error, writes a CloudError, and continues.
const defaultMaxLineBytes = 64 * 1024 * 1024

func main() {
	os.Exit(run())
}

// runOptions holds optional overrides for runWithArgs. The zero value is valid
// and selects production defaults for every field.
type runOptions struct {
	// ClientFactory constructs the PVE client from a loaded config. When nil,
	// runWithArgs uses pve.NewClient. Tests inject a factory that returns a
	// nilPVEClient to exercise the pve.NewClient error path without a live PVE.
	ClientFactory func(cfg *config.CPIConfig, logger *log.Logger) (pve.Client, error)

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

	clientFactory := opts.ClientFactory
	if clientFactory == nil {
		clientFactory = pve.NewClient
	}
	client, err := clientFactory(cfg, logger)
	if err != nil {
		logger.Error("pve client init failed", log.Err(err))
		return 1
	}

	bootAgent, err := agent.NewAgent(cfg, client, logger)
	if err != nil {
		logger.Error("agent init failed", log.Err(err))
		return 1
	}

	// Root context cancelled on SIGINT/SIGTERM. defer cancel() fires when
	// runWithArgs returns, deregistering the signal handler and releasing
	// resources even though main calls os.Exit — the exit is deferred until
	// after run() returns.
	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

	// Apply the operator's task-poll cadence process-wide before serving. With
	// an unset retry.task_poll block these resolve to the shipped defaults, so
	// polling is identical to prior releases.
	tp := cfg.RetryTaskPoll()
	pve.ConfigureTaskPolling(tp.BaseMs, tp.CapMs, tp.JitterPct)

	// Apply the operator's pushback-backoff curve process-wide. With an unset
	// retry.pushback block these resolve to the shipped defaults (5s/60s), so
	// backoff is byte-identical to prior releases.
	pb := cfg.RetryPushback()
	pve.ConfigurePushbackBackoff(pb.BaseMs, pb.CapMs)

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
	d := cpi.NewDispatcherWithOptions(logger, dispatcherOpts...)
	handlers.RegisterAll(d, handlers.Deps{
		Config:   cfg,
		PVE:      client,
		Agent:    bootAgent,
		Logger:   logger,
		Resolver: backendResolver,
	})

	maxLine := opts.MaxLineBytes
	if maxLine <= 0 {
		maxLine = defaultMaxLineBytes
	}

	runErr := runCPI(rootCtx, stdin, stdout, d, logger, maxLine)
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
) error {
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

		// Build a per-request context carrying request_id and method for structured logging.
		reqCtx := log.WithRequestID(ctx, req.Context.RequestID)
		reqCtx = log.WithMethod(reqCtx, req.Method)
		reqLogger := logger.WithContext(reqCtx)
		reqCtx = log.IntoContext(reqCtx, reqLogger)

		// dispatchOne handles the request and writes the response to bw.
		// A deferred recover here is a backstop for panics that occur outside the
		// dispatcher's own recover (e.g., in writeResponse or helper code called
		// before d.Handle). The dispatcher already recovers handler panics; this
		// catches anything else in the per-request path.
		// w is passed alongside bw so the backstop can write a clean CloudError
		// to a fresh bufio.Writer if bw's internal state was corrupted by a panic
		// mid-flush.
		if loopErr := dispatchOne(reqCtx, bw, w, d, req, logger); loopErr != nil {
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

// dispatchOne dispatches req through d, writes the response to bw, and returns
// any write error. It also catches panics that occur outside the dispatcher's
// own recover (e.g., in writeResponse) and converts them to a non-retriable
// CloudError. When a panic occurs mid-write, bw may have partial buffered bytes
// in an indeterminate state; the backstop discards those bytes by resetting bw
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
	if writeErr := writeResponse(bw, resp); writeErr != nil {
		return fmt.Errorf("cpi: write response: %w", writeErr)
	}
	return nil
}
