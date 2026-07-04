package main

// run_coverage_test.go — tests that lift run() and runCPI() branch coverage
// to >=75% without a live PVE endpoint.
//
// Strategy:
//  D1a (in-process): unit tests for runCPI branches not covered by existing
//       tests — blank lines, errSignaled, ErrTooLong, writeResponse success,
//       missing-method decodeRequest error, write-failure paths.
//  D1b (in-process): tests for run() early-exit paths via runWithArgs —
//       no args, unexpected positional args, --config missing-file,
//       valid config + empty stdin reaching handlers.RegisterAll.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// --------------------------------------------------------------------------
// D1a — in-process runCPI branch tests
// --------------------------------------------------------------------------

// TestRunCPI_BlankLinesSkipped feeds several blank lines followed by a valid
// request. Blank lines are skipped silently; exactly one response is written.
func TestRunCPI_BlankLinesSkipped(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	input := "\n\n   \n" + validRequest("info") + "\n\n"
	r := strings.NewReader(input)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	if err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil); err != nil {
		t.Fatalf("runCPI returned unexpected error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response (blank lines skipped), got %d:\n%s", len(lines), w.String())
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected NotImplemented error, got nil error")
	}
}

// TestRunCPI_MissingMethodField sends a JSON object that lacks "method".
// decodeRequest must return the "missing required field" error, which runCPI
// wraps in a CloudError envelope and continues.
func TestRunCPI_MissingMethodField(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	input := `{"arguments":[],"context":{},"api_version":2}` + "\n"
	r := strings.NewReader(input)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	if err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil); err != nil {
		t.Fatalf("runCPI returned unexpected error: %v", err)
	}

	out := strings.TrimSpace(w.String())
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for missing method field, got nil error")
	}
	if !strings.Contains(resp.Error.Type, "CloudError") {
		t.Errorf("error type = %q, want CloudError", resp.Error.Type)
	}
	if !strings.Contains(resp.Error.Message, "method") {
		t.Errorf("error message %q should mention 'method'", resp.Error.Message)
	}
}

// TestRunCPI_ContextCancelledBetweenRequests cancels the context while the
// first request's handler is executing and verifies runCPI returns errSignaled.
// Determinism comes from the handler closing handlerRunning before returning:
// the test cancels at that moment, so by the time runCPI reaches its
// post-writeResponse select on ctx.Done(), the context is already cancelled.
// No reliance on polling, sleep, or pipe close ordering.
func TestRunCPI_ContextCancelledBetweenRequests(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	pr, pw := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	d := cpi.NewDispatcher(logger)
	var w bytes.Buffer

	// Override "info" with a handler that signals when it has begun and
	// only returns once the test acknowledges. The test cancels between
	// these two events; runCPI's post-writeResponse signal check then
	// fires deterministically.
	handlerStarted := make(chan struct{})
	resume := make(chan struct{})
	if err := d.Register("info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		close(handlerStarted)
		<-resume
		return map[string]any{"ok": true}, nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runErr = runCPI(ctx, pr, &w, d, logger, defaultMaxLineBytes, nil)
	}()

	_, _ = fmt.Fprintln(pw, strings.TrimRight(validRequest("info"), "\n"))
	<-handlerStarted
	cancel()
	close(resume)
	// Let runCPI finish writeResponse and hit its post-write select. Close
	// the pipe so a subsequent Scan call (should not happen — the select
	// fires first) returns rather than blocking forever.
	_ = pw.Close()
	<-done

	if !errors.Is(runErr, errSignaled) {
		t.Fatalf("runCPI: want errSignaled, got %v", runErr)
	}
}

// TestRunCPI_WriteResponseSuccessPath registers a handler that returns a
// non-nil result so writeResponse takes the EncodeSuccess branch.
func TestRunCPI_WriteResponseSuccessPath(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	d := cpi.NewDispatcher(logger)
	// Override "info" with a handler that returns a non-nil, JSON-serializable result.
	if err := d.Register("info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]any{"api_version": "2.0", "stemcell_formats": []string{}}, nil
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}

	req := `{"method":"info","arguments":[],"context":{"request_id":"success-1"},"api_version":2}` + "\n"
	r := strings.NewReader(req)
	var w bytes.Buffer

	if err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil); err != nil {
		t.Fatalf("runCPI returned unexpected error: %v", err)
	}

	out := strings.TrimSpace(w.String())
	if out == "" {
		t.Fatal("expected success response on stdout, got empty buffer")
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Errorf("expected nil error on success response, got: %+v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result on success response")
	}
}

// failWriter returns errWriteFail from the first Write that would exceed
// threshold bytes, simulating an I/O failure on the response stream.
type failWriter struct {
	mu        sync.Mutex
	written   int
	threshold int
}

var errWriteFail = errors.New("failWriter: simulated write failure")

func (f *failWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.written >= f.threshold {
		return 0, errWriteFail
	}
	n := len(p)
	if f.written+n > f.threshold {
		n = f.threshold - f.written
	}
	f.written += n
	return n, nil
}

// TestRunCPI_WriteResponseFailure exercises the "cpi: write response" error
// return path by providing a writer that fails after the first few bytes.
func TestRunCPI_WriteResponseFailure(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	d := cpi.NewDispatcher(logger)
	req := validRequest("info")
	r := strings.NewReader(req)
	// Fail after 0 bytes so bufio.Writer flush triggers the error.
	w := &failWriter{threshold: 0}

	err := runCPI(context.Background(), r, w, d, logger, defaultMaxLineBytes, nil)
	if err == nil {
		t.Fatal("expected error from write failure, got nil")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("error %q should mention write failure", err.Error())
	}
}

// TestRunCPI_ErrTooLong feeds a line that exceeds the configured maxLineBytes
// and verifies runCPI writes a CloudError and returns nil (clean exit after
// ErrTooLong). Uses runOptions.MaxLineBytes (4 KiB) so the oversized payload
// stays small — no package-var mutation, safe to run in parallel.
func TestRunCPI_ErrTooLong(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)

	const smallCap = 4 * 1024 // 4 KiB — keeps the oversized payload tiny

	// One oversized line (a few bytes > smallCap) triggers bufio.ErrTooLong.
	oversized := bytes.Repeat([]byte("x"), smallCap+1)
	oversized = append(oversized, '\n')
	r := bytes.NewReader(oversized)

	var w bytes.Buffer
	d := cpi.NewDispatcher(logger)

	runErr := runCPI(context.Background(), r, &w, d, logger, smallCap, nil)

	// runCPI should return nil after ErrTooLong (scanner is not reusable).
	if runErr != nil {
		t.Fatalf("runCPI returned %v, want nil after ErrTooLong", runErr)
	}

	// The output buffer must contain a CloudError response for the oversized line.
	out := strings.TrimSpace(w.String())
	if out == "" {
		t.Fatal("expected CloudError response for oversized line, got empty buffer")
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal CloudError response: %v\nraw: %s", err, out)
	}
	if resp.Error == nil {
		t.Fatal("expected error in response for oversized line, got nil error")
	}
}

// --------------------------------------------------------------------------
// D1b — in-process runWithArgs tests for run() bootstrap paths
// --------------------------------------------------------------------------

// TestRunWithArgs_NoConfig verifies that missing --config flag returns 1.
func TestRunWithArgs_NoConfig(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{}, strings.NewReader(""), &stdout, &stderr, runOptions{})
	if code != 1 {
		t.Errorf("expected exit code 1 for missing --config, got %d", code)
	}
}

// TestRunWithArgs_VersionFlag verifies that --version prints a version string
// to stdout and returns 0.
func TestRunWithArgs_VersionFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{"--version"}, strings.NewReader(""), &stdout, &stderr, runOptions{})
	if code != 0 {
		t.Errorf("expected exit code 0 for --version, got %d; stderr=%q", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if !strings.Contains(out, "bosh-pve-cpi") {
		t.Errorf("--version output %q does not contain 'bosh-pve-cpi'", out)
	}
}

// TestRunWithArgs_UnknownFlag verifies that an unknown flag returns 1.
func TestRunWithArgs_UnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{"--unknown-xyz"}, strings.NewReader(""), &stdout, &stderr, runOptions{})
	if code != 1 {
		t.Errorf("expected exit code 1 for unknown flag, got %d", code)
	}
}

// TestRunWithArgs_PositionalArgs verifies that unexpected positional args return 1.
func TestRunWithArgs_PositionalArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{"unexpected"}, strings.NewReader(""), &stdout, &stderr, runOptions{})
	if code != 1 {
		t.Errorf("expected exit code 1 for positional arg, got %d", code)
	}
}

// TestRunWithArgs_MissingConfigFile verifies that --config pointing at a
// nonexistent path returns 1 with a diagnostic on stderr.
func TestRunWithArgs_MissingConfigFile(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", "/nonexistent/bosh-pve-cpi-test-config.json"},
		strings.NewReader(""),
		&stdout, &stderr,
		runOptions{},
	)
	if code != 1 {
		t.Errorf("expected exit code 1 for missing config file, got %d", code)
	}
	if !strings.Contains(stderr.String(), "config") {
		t.Errorf("stderr %q should mention 'config'", stderr.String())
	}
}

// TestRunWithArgs_ValidConfigEOF writes a minimal valid config and calls
// runWithArgs with empty stdin. Exercises the full startup path including
// pve.NewClient, agent.NewAgent, and handlers.RegisterAll, then clean exit
// when runCPI sees EOF.
func TestRunWithArgs_ValidConfigEOF(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""), // EOF immediately
		&stdout, &stderr,
		runOptions{},
	)
	if code != 0 {
		t.Errorf("expected exit code 0 for valid config + EOF stdin, got %d; stderr=%q", code, stderr.String())
	}
}

// TestRunWithArgs_ValidConfigInfoRequest calls runWithArgs with a valid config
// and a single JSONRPC "info" request on stdin. Verifies exit 0 and a
// well-formed JSONRPC response, exercising handlers.RegisterAll dispatch.
func TestRunWithArgs_ValidConfigInfoRequest(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := `{"method":"info","arguments":[],"context":{"request_id":"rwa-test-1"},"api_version":2}` + "\n"
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{},
	)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Fatal("expected JSONRPC response on stdout, got empty buffer")
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, out)
	}
	// Both success and error are valid; the envelope itself must be well-formed.
	if resp.Result == nil && resp.Error == nil {
		t.Fatal("JSONRPC response has both nil result and nil error")
	}
}

// TestRunWithArgs_InvalidLogLevel verifies that an invalid log_level in the
// config causes runWithArgs to return 1. log.NewLogger rejects unrecognized
// levels with an error, so "logger init failed" is written to stderr and the
// process exits 1.
func TestRunWithArgs_InvalidLogLevel(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "INVALID_LOG_LEVEL_XYZ"
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""),
		&stdout, &stderr,
		runOptions{},
	)
	if code != 1 {
		t.Errorf("expected exit code 1 for invalid log level, got %d; stderr=%q", code, stderr.String())
	}
}

// Note: a prior TestRunCPI_WriteDecodeErrorResponseFailure test was removed.
// It exercised the "cpi: write decode error response" path but swallowed the
// result with `_ = err` and a comment accepting either outcome, contributing
// nothing to regression signal. The symmetric write-response failure path is
// already covered by TestRunCPI_WriteResponseFailure.

// TestDecodeRequest_MissingMethod directly tests the decodeRequest helper
// for the "missing method" branch (not covered by existing tests).
func TestDecodeRequest_MissingMethod(t *testing.T) {
	t.Parallel()
	line := []byte(`{"arguments":[],"context":{},"api_version":2}`)
	_, err := decodeRequest(line)
	if err == nil {
		t.Fatal("expected error for missing method field, got nil")
	}
	if !strings.Contains(err.Error(), "method") {
		t.Errorf("error %q should mention 'method'", err.Error())
	}
}

// TestWriteResponse_SuccessPath exercises writeResponse when resp.Error is nil,
// taking the EncodeSuccess branch. Uses a bytes.Buffer to verify JSON output.
func TestWriteResponse_SuccessPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	resp := &jsonrpc.Response{
		Result: json.RawMessage(`{"api_version":"2.0"}`),
		Error:  nil,
		Log:    "",
	}
	if err := writeResponse(bw, resp); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected JSON output, got empty buffer")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal writeResponse output: %v\nraw: %s", err, out)
	}
	// BOSH JSONRPC success envelope has "result" key.
	if _, ok := decoded["result"]; !ok {
		t.Errorf("success response missing 'result' key: %v", decoded)
	}
}

// TestWriteResponse_ErrorPath exercises writeResponse when resp.Error is set,
// taking the EncodeError branch.
func TestWriteResponse_ErrorPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	resp := &jsonrpc.Response{
		Result: nil,
		Error: &jsonrpc.ErrorBody{
			Type:      "NotImplemented",
			Message:   "test error",
			OkToRetry: false,
		},
		Log: "",
	}
	if err := writeResponse(bw, resp); err != nil {
		t.Fatalf("writeResponse: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected JSON output for error path, got empty buffer")
	}
}

// TestWriteErrorResponse_WritesCloudError verifies that writeErrorResponse
// produces a valid CloudError JSON envelope. Uses a bytes.Buffer writer.
func TestWriteErrorResponse_WritesCloudError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)

	// Build a *cpierrors.Error via the Cloud constructor (same as production code).
	cpiErr := cpierrors.Cloud("test cloud error")
	if err := writeErrorResponse(bw, cpiErr); err != nil {
		t.Fatalf("writeErrorResponse: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected JSON output from writeErrorResponse, got empty buffer")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal output: %v\nraw: %s", err, out)
	}
	if _, ok := decoded["error"]; !ok {
		t.Errorf("CloudError response missing 'error' key: %v", decoded)
	}
}

// --------------------------------------------------------------------------
// D1d — clientFactory seam tests
// --------------------------------------------------------------------------

// errFactoryFail is returned by the failing factory stub.
var errFactoryFail = errors.New("test: pve client init failed")

// TestRunWithArgs_ClientFactoryError exercises the pve.NewClient error path in
// runWithArgs by injecting a factory that always returns errFactoryFail.
// Expects exit code 1 and no panic.
func TestRunWithArgs_ClientFactoryError(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	failFactory := func(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
		return nil, errFactoryFail
	}
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""),
		&stdout, &stderr,
		runOptions{ClientFactory: failFactory},
	)
	if code != 1 {
		t.Errorf("expected exit code 1 for client factory error, got %d; stderr=%q", code, stderr.String())
	}
}

// TestRunWithArgs_ClientFactorySuccess exercises the full startup path using
// the nilPVEClient factory (zero overhead, no network calls). Feeds an "info"
// request and expects exit 0 and a well-formed JSONRPC envelope.
func TestRunWithArgs_ClientFactorySuccess(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nilFactory := func(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
		return nilPVEClient{}, nil
	}
	req := `{"method":"info","arguments":[],"context":{"request_id":"factory-test-1"},"api_version":2}` + "\n"
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilFactory},
	)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Fatal("expected JSONRPC response on stdout, got empty buffer")
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, out)
	}
	if resp.Result == nil && resp.Error == nil {
		t.Fatal("JSONRPC response has both nil result and nil error")
	}
}

// TestRunWithArgs_AutoMode_NoRegistry_Starts exercises agent_mode=auto with no
// registry_endpoint. The primary boot agent resolves as configdrive (cloudinit
// path via synthetic mode) and registryAgent is nil. Expects exit 0 and a
// well-formed JSONRPC response.
func TestRunWithArgs_AutoMode_NoRegistry_Starts(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false,
  "agent_mode": "auto"
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nilFactory := func(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
		return nilPVEClient{}, nil
	}
	req := `{"method":"info","arguments":[],"context":{"request_id":"auto-noreg-1"},"api_version":2}` + "\n"
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilFactory},
	)
	if code != 0 {
		t.Errorf("expected exit code 0 for auto mode + no registry, got %d; stderr=%q", code, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Fatal("expected JSONRPC response on stdout, got empty buffer")
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, out)
	}
	if resp.Result == nil && resp.Error == nil {
		t.Fatal("JSONRPC response has both nil result and nil error")
	}
}

// TestRunWithArgs_RegistryKeys_Rejected verifies that legacy registry_* config
// keys are rejected at config load with a clear migration error. The BOSH
// registry was deprecated upstream and removed from this CPI; configs carrying
// registry_* keys must fail fast (exit 1) rather than silently start.
func TestRunWithArgs_RegistryKeys_Rejected(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false,
  "agent_mode": "auto",
  "registry_endpoint": "http://127.0.0.1:25777",
  "registry_user": "admin",
  "registry_password": "registry-pass",
  "registry_allow_private_ip": true
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nilFactory := func(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
		return nilPVEClient{}, nil
	}
	req := `{"method":"info","arguments":[],"context":{"request_id":"reg-reject-1"},"api_version":2}` + "\n"
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(req),
		&stdout, &stderr,
		runOptions{ClientFactory: nilFactory},
	)
	if code != 1 {
		t.Errorf("expected exit code 1 for registry_* keys, got %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no longer supported") {
		t.Errorf("expected migration error on stderr, got %q", stderr.String())
	}
}

// TestRunWithArgs_AgentInitError exercises the agent.NewAgent error path by
// injecting a nilPVEClient factory and setting agent_mode to an unsupported
// value. NewAgent returns NotSupported, runWithArgs returns 1.
func TestRunWithArgs_AgentInitError(t *testing.T) {
	t.Parallel()

	cfgJSON := `{
  "host": "127.0.0.1",
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false,
  "agent_mode": "unsupported-mode-xyz"
}`
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nilFactory := func(_ *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
		return nilPVEClient{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runWithArgs(
		[]string{"--config", cfgFile},
		strings.NewReader(""),
		&stdout, &stderr,
		runOptions{ClientFactory: nilFactory},
	)
	if code != 1 {
		t.Errorf("expected exit code 1 for agent init error, got %d; stderr=%q", code, stderr.String())
	}
}
