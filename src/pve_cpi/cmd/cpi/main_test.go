package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
func boolPtr(b bool) *bool { return &b }

// nilPVEClient satisfies pve.Client without making real API calls.
// All methods return nil service values; the dispatcher never invokes services
// in tests because all 22 methods return NotImplemented before touching services.
type nilPVEClient struct{}

func (nilPVEClient) QEMU() qemu.Service                     { return nil }
func (nilPVEClient) Storage() storage.Service               { return nil }
func (nilPVEClient) CloudInit() cloudinit.Service           { return nil }
func (nilPVEClient) Tasks() tasks.Service                   { return nil }
func (nilPVEClient) Nodes() nodes.Service                   { return nil }
func (nilPVEClient) Cluster() cluster.Service               { return nil }
func (nilPVEClient) ClusterStorage() clusterstorage.Service { return nil }
func (nilPVEClient) Pools() pve.PoolService                 { return nil }

// compile-time check.
var _ pve.Client = nilPVEClient{}

// minimalCfg returns a CPIConfig that satisfies Validate without talking to PVE.
func minimalCfg() *config.CPIConfig {
	cfg := &config.CPIConfig{
		Host:           "pve.test",
		Port:           8006,
		User:           "root",
		APIToken:       "root@pam!tok=secret",
		Realm:          "pam",
		Node:           "pve",
		VMStorage:      "local",
		DiskStorage:    "local",
		NetworkBridge:  "vmbr0",
		VerifySSL:      boolPtr(true),
		AgentMode:      "cloudinit",
		VMDiskFormat:   "qcow2",
		LogLevel:       "info",
		VMIDRangeStart: 100,
	}
	cfg.StemcellStorage = cfg.VMStorage
	return cfg
}

// makeTestDeps returns cfg and logger suitable for runCPI tests.
//
//nolint:unparam // cfg result used by most callers; some discard it with _
func makeTestDeps(t *testing.T) (*config.CPIConfig, *log.Logger) {
	t.Helper()
	cfg := minimalCfg()
	logger := log.NewNopLogger()
	return cfg, logger
}

// validRequest returns a JSON-encoded JSON-RPC request line for the given method.
func validRequest(method string) string {
	return `{"method":"` + method + `","arguments":[],"context":{"request_id":"test-req-1"},"api_version":2}` + "\n"
}

// parseResponse decodes one JSON-RPC response from data.
func parseResponse(t *testing.T, data string) jsonrpc.Response {
	t.Helper()
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &resp); err != nil {
		t.Fatalf("parseResponse: unmarshal failed: %v\nraw: %s", err, data)
	}
	return resp
}

// TestRunCPI_EOF verifies that empty input returns nil (clean exit) and writes no output.
// Covers the sc.Scan() → false with no error (clean EOF) path.
func TestRunCPI_EOF(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	r := strings.NewReader("")
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil)
	if err != nil {
		t.Fatalf("expected nil error on EOF, got: %v", err)
	}
	if w.Len() != 0 {
		t.Fatalf("expected empty output on EOF, got: %q", w.String())
	}
}

// TestRunCPI_SingleRequest feeds a valid JSON-RPC request and verifies a response is written.
func TestRunCPI_SingleRequest(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	r := strings.NewReader(validRequest("info"))
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp := parseResponse(t, w.String())
	// "info" is pre-registered as NotImplemented in the dispatcher.
	if resp.Error == nil {
		t.Fatalf("expected error body (NotImplemented), got nil error with result: %v", resp.Result)
	}
	if !strings.Contains(resp.Error.Type, "NotImplemented") {
		t.Errorf("expected NotImplemented error type, got %q", resp.Error.Type)
	}
}

// scanErrReader fails every Read with a persistent non-EOF error, simulating
// a wedged pipe (EIO) or a Director dying mid-write.
type scanErrReader struct{}

func (scanErrReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("simulated stdin I/O error (EIO)")
}

// TestRunCPI_ScanErrorTerminates verifies a non-EOF, non-ErrTooLong read
// error terminates runCPI after exactly one CloudError response.
// bufio.Scanner is single-shot after any read error, so continuing the loop
// would re-fail identically and spin hot, flooding stdout with repeated
// CloudError responses.
func TestRunCPI_ScanErrorTerminates(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	done := make(chan error, 1)
	go func() {
		done <- runCPI(context.Background(), scanErrReader{}, &w, d, logger, defaultMaxLineBytes, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scan error should terminate cleanly, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runCPI did not return after a persistent scan error (hot spin)")
	}

	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 CloudError response, got %d:\n%s", len(lines), w.String())
	}
	resp := parseResponse(t, lines[0])
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "request read failed") {
		t.Fatalf("expected request-read-failed CloudError, got: %+v", resp)
	}
}

// TestRunCPI_CancelUnblocksIdleRead verifies context cancellation terminates a
// runCPI blocked in a read with no pending input: the AfterFunc closes the
// read side, the blocked read returns, and runCPI reports errSignaled instead
// of requiring SIGKILL.
func TestRunCPI_CancelUnblocksIdleRead(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pw.Close() //nolint:errcheck // test cleanup
	var w bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	d := cpi.NewDispatcher(logger)
	done := make(chan error, 1)
	go func() {
		done <- runCPI(ctx, pr, &w, d, logger, defaultMaxLineBytes, nil)
	}()

	// Give runCPI time to park in the blocking read, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case runErr := <-done:
		if !errors.Is(runErr, errSignaled) {
			t.Fatalf("expected errSignaled after cancel, got: %v", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runCPI stayed blocked in read after context cancellation")
	}
}

// TestRunCPI_MalformedJSON feeds garbage JSON followed by a valid request.
// Unique invariant: loop continues after a decode error — the second request
// produces a NotImplemented response, proving the decoder state resets per line.
func TestRunCPI_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	// Garbage JSON followed by a valid request. The decoder should recover and
	// process the second request after emitting a CloudError for the first.
	input := "{not valid json}\n" + validRequest("has_vm")
	r := strings.NewReader(input)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect two responses: one CloudError + one NotImplemented.
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d:\n%s", len(lines), w.String())
	}

	// First response: CloudError from the malformed JSON.
	var firstResp jsonrpc.Response
	if err := json.Unmarshal([]byte(lines[0]), &firstResp); err != nil {
		t.Fatalf("parse first response: %v", err)
	}
	if firstResp.Error == nil {
		t.Fatal("expected error in first response (malformed input)")
	}
	if !strings.Contains(firstResp.Error.Type, "CloudError") {
		t.Errorf("expected CloudError type, got %q", firstResp.Error.Type)
	}

	// Second response: NotImplemented for "has_vm".
	var secondResp jsonrpc.Response
	if err := json.Unmarshal([]byte(lines[1]), &secondResp); err != nil {
		t.Fatalf("parse second response: %v", err)
	}
	if secondResp.Error == nil {
		t.Fatal("expected error in second response (NotImplemented)")
	}
	if !strings.Contains(secondResp.Error.Type, "NotImplemented") {
		t.Errorf("expected NotImplemented, got %q", secondResp.Error.Type)
	}
}

// TestRunCPI_TwoRequests sends two valid JSON-RPC requests and verifies both
// responses are written in order.
func TestRunCPI_TwoRequests(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	req1 := `{"method":"create_stemcell","arguments":[],"context":{"request_id":"req-1"},"api_version":2}` + "\n"
	req2 := `{"method":"delete_stemcell","arguments":[],"context":{"request_id":"req-2"},"api_version":2}` + "\n"
	r := strings.NewReader(req1 + req2)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d:\n%s", len(lines), w.String())
	}

	for i, line := range lines {
		var resp jsonrpc.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("parse response[%d]: %v", i, err)
		}
		// Both methods are pre-registered as NotImplemented.
		if resp.Error == nil {
			t.Errorf("response[%d]: expected error body, got nil", i)
			continue
		}
		if !strings.Contains(resp.Error.Type, "NotImplemented") {
			t.Errorf("response[%d]: expected NotImplemented, got %q", i, resp.Error.Type)
		}
	}
}

// TestMain_VersionFlag compiles the binary and invokes it with --version,
// verifying the output contains the expected prefix and exits 0.
func TestMain_VersionFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping --version binary test in short mode")
	}

	binPath, err := buildOnce(t)
	if err != nil {
		t.Fatalf("buildOnce: %v", err)
	}

	ctx := t.Context()
	cmd := makeExecCmd(ctx, binPath, "--version")
	out, execErr := cmd.CombinedOutput()
	if execErr != nil {
		t.Fatalf("--version exited non-zero: %v\noutput: %s", execErr, out)
	}
	output := strings.TrimSpace(string(out))
	if !strings.Contains(output, "bosh-proxmox-cpi") {
		t.Errorf("--version output does not contain 'bosh-proxmox-cpi': %q", output)
	}
}

// --------------------------------------------------------------------------
// Explicit dispatch / EOF / malformed-JSON envelope coverage.
// --------------------------------------------------------------------------

// TestRunCPI_DispatchesRequest verifies that runCPI parses a valid JSON-RPC
// request from r, hands it to the dispatcher, and writes a well-formed
// response envelope to w. Uses a pre-registered method ("info") that the
// dispatcher returns as NotImplemented; the test asserts the round-trip
// produced exactly one response carrying that error type. Complements
// TestRunCPI_SingleRequest by also confirming the request_id flows through.
func TestRunCPI_DispatchesRequest(t *testing.T) {
	t.Parallel()
	_, logger := makeTestDeps(t)
	req := `{"method":"info","arguments":[],"context":{"request_id":"dispatch-id-1"},"api_version":2}` + "\n"
	r := strings.NewReader(req)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	if err := runCPI(context.Background(), r, &w, d, logger, defaultMaxLineBytes, nil); err != nil {
		t.Fatalf("runCPI returned unexpected error: %v", err)
	}

	out := strings.TrimSpace(w.String())
	if out == "" {
		t.Fatal("expected response on stdout, got empty buffer")
	}
	// Exactly one response line — no extra writes from a clean dispatch.
	if lines := strings.Split(out, "\n"); len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d:\n%s", len(lines), out)
	}

	resp := parseResponse(t, out)
	if resp.Error == nil {
		t.Fatalf("expected NotImplemented error envelope, got result: %v", resp.Result)
	}
	if !strings.Contains(resp.Error.Type, "NotImplemented") {
		t.Errorf("error type: got %q, want substring NotImplemented", resp.Error.Type)
	}
	// Result field must be null when Error is set (BOSH JSON-RPC contract).
	if resp.Result != nil {
		t.Errorf("expected nil Result on error response, got %v", resp.Result)
	}
}

// panicOnFirstWrite is an io.Writer that panics on the first Write call and
// delegates subsequent writes to buf. It simulates a broken write path (e.g.,
// a corrupted response encoder) to exercise the dispatchOne backstop recover.
type panicOnFirstWrite struct {
	calls atomic.Int32
	buf   bytes.Buffer
}

func (w *panicOnFirstWrite) Write(p []byte) (int, error) {
	if w.calls.Add(1) == 1 {
		panic("write path exploded")
	}
	return w.buf.Write(p)
}

// TestDispatchOne_WriteResponsePanic_EmitsCloudError verifies that dispatchOne's
// deferred recover catches a panic from writeResponse (i.e., a panic that occurs
// outside the dispatcher's own recover), emits a CloudError JSON body to bw, and
// returns nil (not a write error) so the loop can continue.
//
// Mechanism: the underlying writer panics on the first Write call (which comes
// from writeResponse encoding the handler's normal result). The recover fires,
// calls writeErrorResponse on the same bw — which now delegates to buf because
// calls > 1. The output in buf is a valid CloudError JSON-RPC response.
func TestDispatchOne_WriteResponsePanic_EmitsCloudError(t *testing.T) {
	t.Parallel()

	_, logger := makeTestDeps(t)
	d := cpi.NewDispatcher(logger)
	// "info" returns NotImplemented — a normal (non-panicking) handler. The panic
	// comes from the writer, not the handler, so it bypasses the dispatcher recover
	// and is caught by dispatchOne's backstop.
	req := &jsonrpc.Request{
		Method:     "info",
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "backstop-req-006"},
		APIVersion: 2,
	}

	sink := &panicOnFirstWrite{}
	bw := bufio.NewWriter(sink)

	tracer, exporter := newSyncTracer()
	_, span := tracer.Start(context.Background(), "info")
	err := dispatchOne(context.Background(), bw, sink, d, req, logger, span, tracer)

	// dispatchOne must not propagate the panic, and must return nil (write of the
	// CloudError succeeded — the second write path no longer panics).
	if err != nil {
		t.Fatalf("dispatchOne returned unexpected error: %v", err)
	}

	// The CloudError JSON must be present in sink.buf (written on the 2nd+ call).
	out := sink.buf.String()
	if out == "" {
		t.Fatal("expected CloudError JSON in sink; got empty buffer")
	}

	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("could not parse CloudError from sink: %v\nraw: %s", err, out)
	}
	if resp.Error == nil {
		t.Fatal("expected error body in recovered response; got nil error")
	}
	if !strings.Contains(resp.Error.Type, "CloudError") {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("backstop-recovered error must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "info") {
		t.Errorf("error message %q missing method name", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "backstop-req-006") {
		t.Errorf("error message %q missing request_id", resp.Error.Message)
	}

	// The request's root span ("info") is already ended (with its own status,
	// via endRootSpan) by the time writeResponse panics, so a fresh
	// cpi.response_write_failure span is the only telemetry that can surface
	// this failure.
	spans := exporter.GetSpans()
	var failSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "cpi.response_write_failure" {
			failSpan = &spans[i]
			break
		}
	}
	if failSpan == nil {
		t.Fatalf("no span named %q among exported spans: %+v", "cpi.response_write_failure", spans)
	}
	if failSpan.Status.Code != codes.Error {
		t.Errorf("write-failure span status = %v, want codes.Error", failSpan.Status.Code)
	}
	if !strings.Contains(failSpan.Status.Description, "backstop-req-006") {
		t.Errorf("write-failure span status description %q missing request_id", failSpan.Status.Description)
	}
	if reqID, ok := requestIDAttr(*failSpan); !ok || reqID != "backstop-req-006" {
		t.Errorf("write-failure span request_id attribute = %q (present=%v), want %q", reqID, ok, "backstop-req-006")
	}
	var methodAttr string
	var methodOK bool
	for _, kv := range failSpan.Attributes {
		if string(kv.Key) == "cpi.method" {
			methodAttr = kv.Value.AsString()
			methodOK = true
			break
		}
	}
	if !methodOK || methodAttr != "info" {
		t.Errorf("write-failure span cpi.method attribute = %q (present=%v), want %q", methodAttr, methodOK, "info")
	}
	if len(failSpan.Events) == 0 || !strings.Contains(failSpan.Events[0].Name, "exception") {
		t.Errorf("write-failure span missing recorded error event: %+v", failSpan.Events)
	}
}

// --------------------------------------------------------------------------
// Exit drain of the endpoint cache's background workers. These tests use the
// process-wide registry, so none of them runs in parallel, and neither may
// any test that starts a worker through the registry or a client factory.
// Every runWithArgs call opens and drains that one registry, so a parallel
// test's drain can close the run while another test registers a worker, which
// refuses the worker, or cancel it at the first test's bound. Tests that only
// send `info` or inject a factory that starts nothing may stay parallel.
// --------------------------------------------------------------------------

// drainTestSlack is how far past its bound a drained run may end before a
// test calls it late, generous for the race detector on a busy runner.
const drainTestSlack = 2 * time.Second

// drainTestGuard is how long a test waits for a drain that should end for a
// reason other than its bound before it fails by name, so a broken rule never
// shows up as a package timeout.
const drainTestGuard = 5 * time.Second

// finishesWithin runs fn on its own goroutine and fails the test, naming the
// rule that what describes, if fn does not return within drainTestGuard.
func finishesWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(drainTestGuard):
		t.Fatalf("%s: still waiting after %s", what, drainTestGuard)
	}
}

// holdSIGTERM keeps a handler registered for SIGTERM until the test ends, so a
// signal that lands after runWithArgs removed its own handler, as it would
// when a rule under test breaks, cannot kill the test binary.
func holdSIGTERM(t *testing.T) {
	t.Helper()
	held := make(chan os.Signal, 1)
	signal.Notify(held, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(held) })
}

// sendSIGTERM delivers SIGTERM to the test process.
func sendSIGTERM(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
}

// waitDrainStarted waits until the process registry's drain has started,
// which Go reports by refusing a worker.
func waitDrainStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := endpointcache.Go("probe", config.DiscoveryModeAuto, func(context.Context) {})
		if errors.Is(err, endpointcache.ErrRegistryClosed) {
			return
		}
		if err != nil {
			t.Fatalf("Go: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the exit drain never started")
		}
		time.Sleep(time.Millisecond)
	}
}

// drainLine is a log line a test reads back, with the level it was logged at.
type drainLine struct {
	level   slog.Level
	message string
}

// lineSpyHandler records every line the process logger fans out to it, with
// its level, once LoggerProviderFactory returns it as the OTel logs handler.
type lineSpyHandler struct {
	mu    sync.Mutex
	lines []drainLine
}

func (h *lineSpyHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *lineSpyHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, drainLine{level: r.Level, message: r.Message})
	return nil
}

func (h *lineSpyHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lineSpyHandler) WithGroup(string) slog.Handler      { return h }

// levelOf returns the level of the first line with message, and whether there
// is one.
func (h *lineSpyHandler) levelOf(message string) (slog.Level, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.lines {
		if line.message == message {
			return line.level, true
		}
	}
	return 0, false
}

// factory returns a LoggerProviderFactory that hands the spy to the process
// logger and calls onShutdown when the logs pipeline is flushed.
func (h *lineSpyHandler) factory(
	onShutdown func(),
) func(context.Context, config.OTelConfig, *log.Logger) (slog.Handler, func(context.Context) error, error) {
	return func(context.Context, config.OTelConfig, *log.Logger) (slog.Handler, func(context.Context) error, error) {
		return h, func(context.Context) error {
			onShutdown()
			return nil
		}, nil
	}
}

var _ slog.Handler = (*lineSpyHandler)(nil)

// The drain's log lines, as logDrainResult words them.
const (
	panicLine     = "endpoint cache: background worker panicked"
	cancelledLine = "endpoint cache: cancelled background work still running at exit"
	finishedLine  = "endpoint cache: background work finished before exit"
)

// drainInfoRequest is an `info` request, which answers from constants.
const drainInfoRequest = `{"method":"info","arguments":[],"context":{"request_id":"drain-1"},"api_version":2}` + "\n"

// drainTestWorker is a background worker the test controls. It ignores
// cancellation until the test releases it, standing in for a cache write
// stuck in a system call.
type drainTestWorker struct {
	ctx     chan context.Context
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

// newDrainTestWorker returns a worker that is released and awaited when the
// test ends, so it never outlives the test.
func newDrainTestWorker(t *testing.T) *drainTestWorker {
	t.Helper()
	w := &drainTestWorker{ctx: make(chan context.Context, 1), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() {
		w.finish()
		select {
		case <-w.done:
		case <-time.After(10 * time.Second):
			t.Error("drain test worker never returned")
		}
	})
	return w
}

// start registers the worker with the process registry, for a cluster config
// with the auto discovery mode.
func (w *drainTestWorker) start(name string) error {
	return w.startWith(name, config.DiscoveryModeAuto)
}

// startWith registers the worker for a cluster config with the given
// configured discovery mode.
func (w *drainTestWorker) startWith(name string, mode config.DiscoveryMode) error {
	return endpointcache.Go(name, mode, func(ctx context.Context) {
		defer close(w.done)
		w.ctx <- ctx
		<-w.release
	})
}

// finish releases the worker.
func (w *drainTestWorker) finish() { w.once.Do(func() { close(w.release) }) }

// waitStarted waits until the worker runs, which means the call under test
// has opened its run of the registry.
func (w *drainTestWorker) waitStarted(t *testing.T) {
	t.Helper()
	w.cancelled(t)
}

// cancelled reports whether the worker started and its context has ended.
func (w *drainTestWorker) cancelled(t *testing.T) bool {
	t.Helper()
	select {
	case ctx := <-w.ctx:
		w.ctx <- ctx
		return ctx.Err() != nil
	case <-time.After(10 * time.Second):
		t.Fatal("drain test worker never started")
		return false
	}
}

// factoryStarting returns a client factory that starts background work with
// start and then returns nilPVEClient, the way a client that begins a
// discovery refresh hands back a usable client.
func factoryStarting(
	t *testing.T,
	start func() error,
) func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
	t.Helper()
	return func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		if err := start(); err != nil {
			t.Errorf("start background work: %v", err)
		}
		return nilPVEClient{}, nil
	}
}

// drainRun runs one CPI call and reports its exit code, output, and duration.
func drainRun(args []string, stdin string, opts runOptions) (code int, stdout, stderr string, took time.Duration) {
	var out, errOut bytes.Buffer
	start := time.Now()
	code = runWithArgs(args, strings.NewReader(stdin), &out, &errOut, opts)
	return code, out.String(), errOut.String(), time.Since(start)
}

// `info` starts no background worker and pays nothing for the drain, through
// the production client factory and with a token-authenticated config that
// leaves discovery on by default.
func TestInfoStartsNoBackgroundWork(t *testing.T) {
	cfg := minimalCfg()
	cfg.LogLevel = "debug"
	cfgPath := writeStorageJournalConfig(t, cfg)

	code, stdout, stderr, took := drainRun([]string{"--config", cfgPath}, drainInfoRequest, runOptions{})
	if code != 0 {
		t.Fatalf("info exit %d: %s", code, stderr)
	}
	if resp := parseResponse(t, stdout); resp.Error != nil {
		t.Fatalf("info returned an error: %+v", resp.Error)
	}
	if strings.Contains(stderr, "background work") {
		t.Fatalf("info logged background work: %s", stderr)
	}
	if took >= endpointcache.DrainWait {
		t.Fatalf("info took %s, as long as the drain's bound", took)
	}
}

// drainWait selects the 3 second default for a zero or negative wait and
// keeps any other wait.
func TestDrainWaitSelectsTheDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		wait time.Duration
		want time.Duration
	}{
		{name: "zero", wait: 0, want: endpointcache.DrainWait},
		{name: "negative", wait: -1, want: endpointcache.DrainWait},
		{name: "explicit", wait: 5 * time.Second, want: 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := drainWait(tc.wait); got != tc.want {
				t.Fatalf("drainWait(%s) = %s, want %s", tc.wait, got, tc.want)
			}
		})
	}
}

// The drain stops at its bound for a worker that ignores cancellation,
// cancels it, and leaves the exit code and the response alone.
func TestDrainStopsAtItsBoundAndKeepsExitCode(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	worker := newDrainTestWorker(t)
	const bound = 200 * time.Millisecond

	code, stdout, stderr, took := drainRun([]string{"--config", cfgPath}, drainInfoRequest,
		runOptions{
			ClientFactory: factoryStarting(t, func() error { return worker.start("discovery") }),
			DrainWait:     bound,
		})
	if code != 0 {
		t.Fatalf("exit %d with a slow worker, want 0: %s", code, stderr)
	}
	if resp := parseResponse(t, stdout); resp.Error != nil {
		t.Fatalf("info returned an error: %+v", resp.Error)
	}
	if took < bound {
		t.Fatalf("run ended after %s, before the %s drain bound", took, bound)
	}
	if took > bound+drainTestSlack {
		t.Fatalf("run took %s, past the %s drain bound", took, bound)
	}
	if !worker.cancelled(t) {
		t.Fatal("the drain did not cancel the slow worker")
	}
	if !strings.Contains(stderr, "endpoint cache: cancelled background work still running at exit") ||
		!strings.Contains(stderr, "discovery") {
		t.Fatalf("stderr lacks the cancelled-work line naming the worker: %s", stderr)
	}
}

// A slow worker never changes a failing call's exit code either.
func TestSlowWorkerKeepsFailingExitCode(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	failingWrite := func(opts runOptions) int {
		code := runWithArgs([]string{"--config", cfgPath}, strings.NewReader(drainInfoRequest),
			failingWriter{}, &bytes.Buffer{}, opts)
		return code
	}
	baseline := failingWrite(runOptions{ClientFactory: nilTracerClientFactory, DrainWait: 50 * time.Millisecond})
	if baseline != 1 {
		t.Fatalf("baseline exit %d, want 1 from the failed response write", baseline)
	}
	worker := newDrainTestWorker(t)
	slow := failingWrite(runOptions{
		ClientFactory: factoryStarting(t, func() error { return worker.start("down mark") }),
		DrainWait:     50 * time.Millisecond,
	})
	if slow != baseline {
		t.Fatalf("slow worker changed the exit code from %d to %d", baseline, slow)
	}
	if !worker.cancelled(t) {
		t.Fatal("the drain did not cancel the slow worker")
	}
}

// failingWriter fails every write, so runCPI cannot write its response and
// the call exits 1.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("simulated stdout failure") }

// A call that starts a short write and returns at once still waits for it, so
// the write lands before the process exits.
func TestDrainWaitsForWorkThatFinishesInTime(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	var wrote atomic.Bool
	start := func() error {
		return endpointcache.Go("down mark", config.DiscoveryModeAuto, func(ctx context.Context) {
			select {
			case <-time.After(100 * time.Millisecond):
				wrote.Store(true)
			case <-ctx.Done():
			}
		})
	}
	code, _, stderr, took := drainRun([]string{"--config", cfgPath}, drainInfoRequest,
		runOptions{ClientFactory: factoryStarting(t, start)})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !wrote.Load() {
		t.Fatal("the call exited before its background write finished")
	}
	if took > endpointcache.DrainWait {
		t.Fatalf("run took %s; the drain should end when the work does", took)
	}
	if !strings.Contains(stderr, "endpoint cache: background work finished before exit") {
		t.Fatalf("stderr lacks the finished-work line: %s", stderr)
	}
}

// A panicking worker is reported at Error and leaves the exit code alone.
func TestPanickingWorkerKeepsExitCode(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "warn", "")
	start := func() error {
		return endpointcache.Go(
			"discovery",
			config.DiscoveryModeAuto,
			func(context.Context) { panic("simulated worker defect") },
		)
	}
	spy := &lineSpyHandler{}
	code, stdout, stderr, _ := drainRun([]string{"--config", cfgPath}, drainInfoRequest,
		runOptions{ClientFactory: factoryStarting(t, start), LoggerProviderFactory: spy.factory(func() {})})
	if code != 0 {
		t.Fatalf("exit %d after a worker panic, want 0: %s", code, stderr)
	}
	if resp := parseResponse(t, stdout); resp.Error != nil {
		t.Fatalf("info returned an error: %+v", resp.Error)
	}
	if !strings.Contains(stderr, "endpoint cache: background worker panicked") ||
		!strings.Contains(stderr, "simulated worker defect") {
		t.Fatalf("stderr lacks the panic line: %s", stderr)
	}
	if level, ok := spy.levelOf(panicLine); !ok || level != slog.LevelError {
		t.Fatalf("panic line logged at %v (present %v), want Error", level, ok)
	}
}

// A subcommand that builds a PVE client is drained too, within the same
// bound, and a panic there reaches stderr although the subcommand logs
// through a no-op logger.
func TestSubcommandDrainsBackgroundWork(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	const bound = 100 * time.Millisecond
	worker := newDrainTestWorker(t)
	start := func() error {
		if err := worker.start("discovery"); err != nil {
			return err
		}
		return endpointcache.Go(
			"down mark",
			config.DiscoveryModeAuto,
			func(context.Context) { panic("simulated subcommand defect") },
		)
	}
	failing := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		if err := start(); err != nil {
			t.Errorf("start background work: %v", err)
		}
		return nil, errors.New("dial refused")
	}
	code, _, stderr, took := drainRun([]string{"stemcell-refs", "report", "--config", cfgPath}, "",
		runOptions{ClientFactory: failing, DrainWait: bound})
	if code != 1 {
		t.Fatalf("exit %d, want 1 from the client failure: %s", code, stderr)
	}
	if took < bound || took > bound+drainTestSlack {
		t.Fatalf("subcommand took %s, want about the %s bound", took, bound)
	}
	if !worker.cancelled(t) {
		t.Fatal("the subcommand's drain did not cancel the slow worker")
	}
	if !strings.Contains(stderr, "simulated subcommand defect") {
		t.Fatalf("stderr lacks the panic line: %s", stderr)
	}
}

// A signal ends the drain's wait at once, and the cancelled-work line follows
// the configured mode of the cluster config that started the worker, Debug
// under auto and Warn under an explicit "on".
func TestDrainBackgroundWorkSignalAndLevel(t *testing.T) {
	cases := []struct {
		mode config.DiscoveryMode
		want slog.Level
	}{
		{mode: config.DiscoveryModeAuto, want: slog.LevelDebug},
		{mode: config.DiscoveryModeOn, want: slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			endpointcache.Open()
			worker := newDrainTestWorker(t)
			if err := worker.startWith("discovery", tc.mode); err != nil {
				t.Fatal(err)
			}
			signalled, cancel := context.WithCancel(context.Background())
			cancel()
			logger, observed := log.NewObservedLogger(slog.LevelDebug)

			start := time.Now()
			finishesWithin(t, "a drain with an ended context cancels at once", func() {
				drainBackgroundWork(signalled, logger, time.Hour)
			})
			if took := time.Since(start); took > drainTestSlack {
				t.Fatalf("drain after a signal took %s", took)
			}
			if !worker.cancelled(t) {
				t.Fatal("the drain did not cancel the worker")
			}
			entries := observed.All()
			if len(entries) != 1 || entries[0].Level != tc.want || entries[0].Message != cancelledLine {
				t.Fatalf("entries = %+v, want one cancelled-work line at %s", entries, tc.want)
			}
		})
	}
}

// A cancelled worker that serves a cluster config with an explicit "on" is
// reported at Warn, although the job-level mode, which this config leaves
// unset, reads as auto.
func TestCancelledWorkerLevelFollowsItsOwnConfigMode(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	for _, tc := range []struct {
		mode config.DiscoveryMode
		want slog.Level
	}{
		{mode: config.DiscoveryModeOn, want: slog.LevelWarn},
		{mode: config.DiscoveryModeAuto, want: slog.LevelDebug},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			worker := newDrainTestWorker(t)
			spy := &lineSpyHandler{}
			code, _, stderr, _ := drainRun([]string{"--config", cfgPath}, drainInfoRequest, runOptions{
				ClientFactory: factoryStarting(
					t,
					func() error { return worker.startWith("discovery", tc.mode) },
				),
				LoggerProviderFactory: spy.factory(func() {}),
				DrainWait:             50 * time.Millisecond,
			})
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if level, ok := spy.levelOf(cancelledLine); !ok || level != tc.want {
				t.Fatalf("cancelled-work line at %v (present %v), want %v", level, ok, tc.want)
			}
		})
	}
}

// A run's last drain never waits again for a worker the first drain gave up
// on, so a stuck worker costs one bound, not two.
func TestLastDrainWaitsForNothingAfterTheFirst(t *testing.T) {
	endpointcache.Open()
	worker := newDrainTestWorker(t)
	if err := worker.start("discovery"); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(context.Background(), log.NewNopLogger(), 20*time.Millisecond)
	if !worker.cancelled(t) {
		t.Fatal("the first drain did not cancel the worker")
	}
	assertLastDrainWaitsForNothing(t)
}

// A drain that a signal ended early leaves nothing for the last drain to wait
// for, so a signalled process does not wait out the bound the first drain
// skipped.
func TestLastDrainWaitsForNothingAfterASignalledDrain(t *testing.T) {
	endpointcache.Open()
	worker := newDrainTestWorker(t)
	if err := worker.start("discovery"); err != nil {
		t.Fatal(err)
	}
	signalled, cancel := context.WithCancel(context.Background())
	cancel()
	finishesWithin(t, "a drain with an ended context cancels at once", func() {
		drainBackgroundWork(signalled, log.NewNopLogger(), time.Hour)
	})
	if !worker.cancelled(t) {
		t.Fatal("the first drain did not cancel the worker")
	}
	assertLastDrainWaitsForNothing(t)
}

// assertLastDrainWaitsForNothing runs the last drain with a long bound and
// fails unless it returns at once and writes nothing.
func assertLastDrainWaitsForNothing(t *testing.T) {
	t.Helper()
	var stderr bytes.Buffer
	start := time.Now()
	finishesWithin(t, "the last drain waits for a worker an earlier drain cancelled", func() {
		drainRemainingBackgroundWork(&stderr, time.Hour)
	})
	if took := time.Since(start); took > drainTestSlack {
		t.Fatalf("the last drain waited %s for a worker the first drain cancelled", took)
	}
	if stderr.Len() != 0 {
		t.Fatalf("the last drain wrote %q with no panic to report", stderr.String())
	}
}

// A SIGTERM that arrives while the main path's drain waits ends the wait at
// once, through the root context, and the process still exits with the code it
// computed and the response it wrote. The last drain follows the first, and
// it too returns at once.
func TestSignalEndsTheMainDrainAndKeepsExitCode(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	worker := newDrainTestWorker(t)
	holdSIGTERM(t)

	var stdout, stderr bytes.Buffer
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runWithArgs([]string{"--config", cfgPath}, strings.NewReader(drainInfoRequest), &stdout, &stderr,
			runOptions{
				ClientFactory: factoryStarting(t, func() error { return worker.start("discovery") }),
				DrainWait:     time.Hour,
			})
	}()
	t.Cleanup(func() {
		worker.finish()
		select {
		case <-codeCh:
		case <-time.After(10 * time.Second):
			t.Error("runWithArgs never returned")
		}
	})
	worker.waitStarted(t)
	waitDrainStarted(t)
	sendSIGTERM(t)
	start := time.Now()
	var code int
	select {
	case code = <-codeCh:
		codeCh <- code
	case <-time.After(drainTestGuard):
		t.Fatalf("SIGTERM did not end the exit drain: runWithArgs was still waiting after %s", drainTestGuard)
	}
	if took := time.Since(start); took > drainTestSlack {
		t.Fatalf("runWithArgs took %s to return after SIGTERM", took)
	}
	if code != 0 {
		t.Fatalf("exit %d after a signal during the drain, want the computed 0: %s", code, stderr.String())
	}
	if resp := parseResponse(t, stdout.String()); resp.Error != nil {
		t.Fatalf("info returned an error: %+v", resp.Error)
	}
	if !worker.cancelled(t) {
		t.Fatal("the signal did not cancel the slow worker")
	}
}

// A SIGTERM that arrives while a subcommand's drain waits ends the wait at
// once, and the subcommand keeps the exit code it computed instead of dying by
// signal.
func TestSignalEndsASubcommandDrainAndKeepsExitCode(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	worker := newDrainTestWorker(t)
	holdSIGTERM(t)
	//nolint:unparam // the client always fails so the subcommand exits with code 1
	failing := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		if err := worker.start("discovery"); err != nil {
			t.Errorf("start background work: %v", err)
		}
		return nil, errors.New("dial refused")
	}

	var stderr bytes.Buffer
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runWithArgs([]string{"stemcell-refs", "report", "--config", cfgPath}, strings.NewReader(""),
			&bytes.Buffer{}, &stderr, runOptions{ClientFactory: failing, DrainWait: time.Hour})
	}()
	t.Cleanup(func() {
		worker.finish()
		select {
		case <-codeCh:
		case <-time.After(10 * time.Second):
			t.Error("runWithArgs never returned")
		}
	})
	worker.waitStarted(t)
	waitDrainStarted(t)
	sendSIGTERM(t)
	start := time.Now()
	var code int
	select {
	case code = <-codeCh:
		codeCh <- code
	case <-time.After(drainTestGuard):
		t.Fatalf("SIGTERM did not end the subcommand's drain: still waiting after %s", drainTestGuard)
	}
	if took := time.Since(start); took > drainTestSlack {
		t.Fatalf("the subcommand took %s to return after SIGTERM", took)
	}
	if code != 1 {
		t.Fatalf("exit %d after a signal during the subcommand's drain, want the computed 1: %s", code, stderr.String())
	}
	if !worker.cancelled(t) {
		t.Fatal("the signal did not cancel the slow worker")
	}
}

// The drain runs before the OTel flush, so the drain's own line, and any line
// a finishing worker logs, reach the exporters before they shut down.
func TestDrainRunsBeforeTheOTelFlush(t *testing.T) {
	cfgPath := writeOTelWiringConfigAtLevel(t, "debug", "")
	start := func() error {
		return endpointcache.Go("down mark", config.DiscoveryModeAuto, func(context.Context) {})
	}
	spy := &lineSpyHandler{}
	// The flush runs on runWithArgs's own goroutine, which drainRun waits for.
	flushed, drainLogged := false, false
	code, _, stderr, _ := drainRun([]string{"--config", cfgPath}, drainInfoRequest, runOptions{
		ClientFactory: factoryStarting(t, start),
		LoggerProviderFactory: spy.factory(func() {
			flushed = true
			_, drainLogged = spy.levelOf(finishedLine)
		}),
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !flushed {
		t.Fatal("the logs pipeline was never flushed")
	}
	if !drainLogged {
		t.Fatal("the OTel flush ran before the drain logged its line")
	}
}
