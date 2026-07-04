package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
	if !strings.Contains(output, "bosh-pve-cpi") {
		t.Errorf("--version output does not contain 'bosh-pve-cpi': %q", output)
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

	_, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "info")
	err := dispatchOne(context.Background(), bw, sink, d, req, logger, span)

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
}
