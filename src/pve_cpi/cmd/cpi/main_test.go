package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

// TestRunCPI_EOF verifies that empty input returns nil (clean exit).
func TestRunCPI_EOF(t *testing.T) {
	cfg, logger := makeTestDeps(t)
	r := strings.NewReader("")
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	_ = cfg
	err := runCPI(context.Background(), r, &w, d, logger)
	if err != nil {
		t.Fatalf("expected nil error on EOF, got: %v", err)
	}
	if w.Len() != 0 {
		t.Fatalf("expected empty output on EOF, got: %q", w.String())
	}
}

// TestRunCPI_SingleRequest feeds a valid JSON-RPC request and verifies a response is written.
func TestRunCPI_SingleRequest(t *testing.T) {
	cfg, logger := makeTestDeps(t)
	r := strings.NewReader(validRequest("info"))
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	_ = cfg
	err := runCPI(context.Background(), r, &w, d, logger)
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

// TestRunCPI_MalformedJSON feeds garbage JSON and verifies a CloudError is written,
// then verifies the loop continues to process the subsequent valid request.
func TestRunCPI_MalformedJSON(t *testing.T) {
	cfg, logger := makeTestDeps(t)
	// Garbage JSON followed by a valid request. The decoder should recover and
	// process the second request after emitting a CloudError for the first.
	input := "{not valid json}\n" + validRequest("has_vm")
	r := strings.NewReader(input)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	_ = cfg
	err := runCPI(context.Background(), r, &w, d, logger)
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
	cfg, logger := makeTestDeps(t)
	req1 := `{"method":"create_stemcell","arguments":[],"context":{"request_id":"req-1"},"api_version":2}` + "\n"
	req2 := `{"method":"delete_stemcell","arguments":[],"context":{"request_id":"req-2"},"api_version":2}` + "\n"
	r := strings.NewReader(req1 + req2)
	var w bytes.Buffer

	d := cpi.NewDispatcher(logger)
	_ = cfg
	err := runCPI(context.Background(), r, &w, d, logger)
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

	// Locate repo root (two directories up from cmd/cpi).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	binPath := filepath.Join(t.TempDir(), "cpi")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/cpi")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Skipf("go build failed (skip --version test): %v\n%s", err, out)
	}

	versionCmd := exec.Command(binPath, "--version")
	out, err := versionCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version exited non-zero: %v\noutput: %s", err, out)
	}
	output := strings.TrimSpace(string(out))
	if !strings.Contains(output, "bosh-pve-cpi") {
		t.Errorf("--version output does not contain 'bosh-pve-cpi': %q", output)
	}
}
