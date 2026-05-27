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
	"syscall"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/version"
)

// exitSignaled is the exit code returned when the process catches SIGINT or SIGTERM.
const exitSignaled = 130

// maxLineBytes is the maximum allowed size of a single JSON-RPC request line (64 MiB).
// bufio.Scanner returns bufio.ErrTooLong if this limit is exceeded; the loop
// treats that as a decode error, writes a CloudError, and continues.
//
// Declared as a var (not const) so the ErrTooLong test can shrink the cap to
// a few KiB and avoid writing 64+ MiB of payload per run; production code
// never mutates it.
var maxLineBytes = 64 * 1024 * 1024

func main() {
	os.Exit(run())
}

// run is the thin os-wired entry point. It delegates to runWithArgs so the
// startup logic is testable in-process without spawning a subprocess.
func run() int {
	return runWithArgs(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// runWithArgs contains the full startup and event loop. Accepting args, stdin,
// stdout, and stderr as parameters makes every flag-parse and config-load path
// testable without spawning a subprocess or manipulating os.Args.
//
// Returning an int rather than calling os.Exit directly ensures that all
// deferred calls (including signal.NotifyContext's cancel) fire before the
// process exits.
func runWithArgs(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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

	client, err := pve.NewClient(cfg, logger)
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

	d := cpi.NewDispatcher(logger)
	handlers.RegisterAll(d, handlers.Deps{
		Config:   cfg,
		PVE:      client,
		Agent:    bootAgent,
		Logger:   logger,
		Resolver: backendResolver,
	})

	runErr := runCPI(rootCtx, stdin, stdout, d, logger)
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

		resp := d.Handle(reqCtx, req)

		if writeErr := writeResponse(bw, resp); writeErr != nil {
			return fmt.Errorf("cpi: write response: %w", writeErr)
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
