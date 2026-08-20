package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Offline wire-protocol conformance: the compiled binary against a dead PVE
// endpoint. Every CPI invocation must produce exactly one well-formed JSONRPC
// envelope on stdout and exit 0, regardless of how unreachable the backend
// is — the Director parses stdout unconditionally, so a stray log line or a
// non-zero exit on a transport fault would break every retry it attempts.
// ---------------------------------------------------------------------------

// closedPort returns a 127.0.0.1 TCP port that was just bound and released,
// so a dial to it refuses immediately (no black-hole timeout).
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// deadEndpointConfig writes a valid CPI config pointing at the given closed
// 127.0.0.1 port, with the dial timeout floored at 1s and the transient retry
// budget capped at a single attempt so each refused dial fails fast instead
// of consuming the default 8-attempt backoff curve.
func deadEndpointConfig(t *testing.T, port int) string {
	t.Helper()
	cfgJSON := fmt.Sprintf(`{
  "host": "127.0.0.1",
  "port": %d,
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false,
  "pve_api_dial_timeout_sec": 1,
  "retry": {"transient": {"max_attempts": 1}}
}`, port)
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	return cfgFile
}

// parseSingleEnvelope asserts stdout carries exactly one non-empty line and
// that line parses as a JSONRPC response envelope.
func parseSingleEnvelope(t *testing.T, stdout string) jsonrpc.Response {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("expected exactly one envelope line on stdout, got %d lines: %q", len(lines), stdout)
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("stdout is not a JSONRPC envelope: %v\nraw: %s", err, lines[0])
	}
	return resp
}

// TestBinary_DeadEndpoint_MethodClasses drives one method from each CPI
// method class against a refused endpoint and asserts the wire contract:
// exit 0, a single envelope, a null result, and a retriable transport error
// (connection refusal is transient by classification — WrapError maps SDK
// ConnectionError to Bosh::Clouds::RetriableCloudError with ok_to_retry).
func TestBinary_DeadEndpoint_MethodClasses(t *testing.T) {
	port := closedPort(t)
	cfgFile := deadEndpointConfig(t, port)

	encodedDiskCID, err := pve.EncodeDiskCID("local-lvm:vm-9001-disk-0", nil)
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}

	cases := []struct {
		name string
		req  string
	}{
		{"query_has_vm", `{"method":"has_vm","arguments":["100"],"context":{"request_id":"cf-1"},"api_version":2}`},
		{"delete_delete_vm", `{"method":"delete_vm","arguments":["100"],"context":{"request_id":"cf-2"},"api_version":2}`},
		{"create_create_disk", `{"method":"create_disk","arguments":[1024,{}],"context":{"request_id":"cf-3"},"api_version":2}`},
		{"attach_attach_disk", fmt.Sprintf(`{"method":"attach_disk","arguments":["100",%q],"context":{"request_id":"cf-4"},"api_version":2}`, encodedDiskCID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, code, runErr := runBinary(t, tc.req+"\n", "--config", cfgFile)
			if runErr != nil {
				t.Fatalf("runBinary: %v", runErr)
			}
			if code != 0 {
				t.Fatalf("expected exit 0 (errors travel in the envelope), got %d; stdout=%q", code, stdout)
			}
			resp := parseSingleEnvelope(t, stdout)
			if resp.Error == nil {
				t.Fatalf("expected an error envelope against a dead endpoint, got result=%v", resp.Result)
			}
			if resp.Result != nil {
				t.Errorf("result must be null alongside an error, got %v", resp.Result)
			}
			if resp.Error.Type != "Bosh::Clouds::RetriableCloudError" {
				t.Errorf("error type = %q; want Bosh::Clouds::RetriableCloudError (refused dial is transient)", resp.Error.Type)
			}
			if !resp.Error.OkToRetry {
				t.Error("ok_to_retry = false; want true for a transport-layer fault")
			}
		})
	}
}

// TestBinary_APIVersionMatrix verifies the api_version handshake: absent, 2,
// and 3 all yield a successful info envelope whose result pins api_version 2
// (the compatibility floor for directors below 283); a non-integer value is
// rejected as a malformed request but still answers with a single well-formed
// error envelope and exit 0.
func TestBinary_APIVersionMatrix(t *testing.T) {
	port := closedPort(t)
	cfgFile := deadEndpointConfig(t, port) // info never dials, but keep one config

	okCases := []struct {
		name string
		req  string
	}{
		{"absent", `{"method":"info","arguments":[],"context":{"request_id":"av-0"}}`},
		{"v2", `{"method":"info","arguments":[],"context":{"request_id":"av-2"},"api_version":2}`},
		{"v3", `{"method":"info","arguments":[],"context":{"request_id":"av-3"},"api_version":3}`},
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, code, runErr := runBinary(t, tc.req+"\n", "--config", cfgFile)
			if runErr != nil {
				t.Fatalf("runBinary: %v", runErr)
			}
			if code != 0 {
				t.Fatalf("expected exit 0, got %d; stdout=%q", code, stdout)
			}
			resp := parseSingleEnvelope(t, stdout)
			if resp.Error != nil {
				t.Fatalf("info must succeed, got error: %+v", resp.Error)
			}
			result, ok := resp.Result.(map[string]any)
			if !ok {
				t.Fatalf("info result is not an object: %T %v", resp.Result, resp.Result)
			}
			if got, want := result["api_version"], float64(2); got != want {
				t.Errorf("info api_version = %v; want %v (pinned for director-282 compatibility)", got, want)
			}
		})
	}

	t.Run("garbage", func(t *testing.T) {
		req := `{"method":"info","arguments":[],"context":{"request_id":"av-x"},"api_version":"not-a-number"}`
		stdout, code, runErr := runBinary(t, req+"\n", "--config", cfgFile)
		if runErr != nil {
			t.Fatalf("runBinary: %v", runErr)
		}
		if code != 0 {
			t.Fatalf("expected exit 0 for a malformed request, got %d; stdout=%q", code, stdout)
		}
		resp := parseSingleEnvelope(t, stdout)
		if resp.Error == nil {
			t.Fatalf("expected a request-decode error envelope, got result=%v", resp.Result)
		}
	})
}

// TestBinary_StdoutPurity_MultiRequest sends a successful request and a
// dead-endpoint failure through one binary invocation and asserts stdout is
// nothing but envelopes: one line per request, every line valid JSON. Log
// output (log_level error still fires for the failure) must stay on stderr.
func TestBinary_StdoutPurity_MultiRequest(t *testing.T) {
	port := closedPort(t)
	cfgFile := deadEndpointConfig(t, port)

	stdin := `{"method":"info","arguments":[],"context":{"request_id":"sp-1"},"api_version":2}` + "\n" +
		`{"method":"has_vm","arguments":["100"],"context":{"request_id":"sp-2"},"api_version":2}` + "\n"
	stdout, code, runErr := runBinary(t, stdin, "--config", cfgFile)
	if runErr != nil {
		t.Fatalf("runBinary: %v", runErr)
	}
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stdout=%q", code, stdout)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly two envelope lines, got %d: %q", len(lines), stdout)
	}
	for i, line := range lines {
		var resp jsonrpc.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Errorf("stdout line %d is not a JSONRPC envelope: %v\nraw: %s", i+1, err, line)
		}
	}
}
