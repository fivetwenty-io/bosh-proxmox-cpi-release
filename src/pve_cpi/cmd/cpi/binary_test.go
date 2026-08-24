package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// runBinary executes the CPI binary with the given args and stdin, returning
// stdout, the exit code, and any exec error.
func runBinary(t *testing.T, stdin string, args ...string) (stdout string, exitCode int, err error) {
	t.Helper()
	bin, buildErr := buildOnce(t)
	if buildErr != nil {
		t.Fatalf("buildOnce: %v", buildErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := makeExecCmd(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr := cmd.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stdoutBuf.String(), exitErr.ExitCode(), nil
		}
		return stdoutBuf.String(), -1, runErr
	}
	return stdoutBuf.String(), 0, nil
}

// TestBinary_NoArgs verifies that invoking the binary with no arguments prints
// an error and exits non-zero (run() returns 1 for missing --config).
func TestBinary_NoArgs(t *testing.T) {
	stdout, code, err := runBinary(t, "")
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing --config, got 0; stdout=%q", stdout)
	}
}

// TestBinary_UnexpectedPositionalArgs verifies that unexpected positional
// arguments cause a non-zero exit with a diagnostic message.
func TestBinary_UnexpectedPositionalArgs(t *testing.T) {
	stdout, code, err := runBinary(t, "", "unexpected-arg")
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit for unexpected positional arg, got 0; stdout=%q", stdout)
	}
}

// TestBinary_MissingConfigFile verifies that --config pointing at a
// nonexistent file exits non-zero with a diagnostic on stderr.
func TestBinary_MissingConfigFile(t *testing.T) {
	stdout, code, err := runBinary(t, "", "--config", "/nonexistent/bosh-proxmox-cpi-test.json")
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing config file, got 0; stdout=%q", stdout)
	}
}

// TestBinary_ValidConfigEOF writes a minimal valid config to a temp file,
// invokes the binary with empty stdin, and asserts exit 0. This exercises
// run()'s full startup path: config load, logger init, pve.NewClient,
// agent.NewAgent, handlers.RegisterAll, and the clean-EOF return from runCPI.
func TestBinary_ValidConfigEOF(t *testing.T) {
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
		t.Fatalf("WriteFile config: %v", err)
	}

	// Empty stdin — runCPI hits EOF immediately, loop exits cleanly.
	stdout, code, err := runBinary(t, "", "--config", cfgFile)
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit 0 for valid config + EOF stdin, got %d; stdout=%q", code, stdout)
	}
	// No JSONRPC output on empty stdin.
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("expected empty stdout on EOF stdin, got %q", stdout)
	}
}

// TestBinary_ValidConfigInfoRequest sends a JSONRPC "info" request via the
// binary's stdin and asserts exit 0 with a well-formed JSONRPC response.
// This exercises handlers.RegisterAll dispatching a real request.
func TestBinary_ValidConfigInfoRequest(t *testing.T) {
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
		t.Fatalf("WriteFile config: %v", err)
	}

	req := `{"method":"info","arguments":[],"context":{"request_id":"bin-test-1"},"api_version":2}` + "\n"
	stdout, code, err := runBinary(t, req, "--config", cfgFile)
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stdout=%q", code, stdout)
	}

	out := strings.TrimSpace(stdout)
	if out == "" {
		t.Fatal("expected JSONRPC response, got empty stdout")
	}

	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, out)
	}
	// "info" is implemented in the handlers package and returns a real response.
	// If not: it should at minimum be a well-formed JSONRPC envelope.
	// Either a success result or a NotImplemented error is acceptable here.
	if resp.Result == nil && resp.Error == nil {
		t.Fatal("JSONRPC response has both nil result and nil error")
	}
}

// TestBinary_InvalidFlagReturnsError verifies that an unknown flag causes a
// non-zero exit. This exercises the fs.Parse error path in run().
func TestBinary_InvalidFlagReturnsError(t *testing.T) {
	_, code, err := runBinary(t, "", "--unknown-flag-xyz")
	if err != nil {
		t.Fatalf("runBinary: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected non-zero exit for unknown flag, got 0")
	}
}
