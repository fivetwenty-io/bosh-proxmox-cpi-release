package cpi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// debugLogger returns a debug-level Logger writing JSON lines to buf so a test
// can inspect the emitted records.
func debugLogger(t *testing.T, buf *bytes.Buffer) *log.Logger {
	t.Helper()
	l, err := log.NewLogger("debug", buf)
	if err != nil {
		t.Fatalf("NewLogger(debug): %v", err)
	}
	return l
}

// secretReq builds a create_vm request whose argument tree carries an mbus URL
// with embedded credentials and a blobstore secret.
func secretReq() *jsonrpc.Request {
	env := map[string]any{
		"bosh": map[string]any{
			"mbus": "nats://nats:s3cr3t-mbus@10.0.0.1:4222",
			"blobstore": map[string]any{
				"options": map[string]any{"secret_access_key": "AKIA-LEAK-ME"},
			},
		},
	}
	raw, _ := json.Marshal(env)
	return &jsonrpc.Request{
		Method:     "create_vm",
		Arguments:  []json.RawMessage{raw},
		Context:    jsonrpc.Context{RequestID: "req-redact-1"},
		APIVersion: 2,
	}
}

// echoSecretHandler returns a result that also embeds a secret so the response
// trace path is exercised.
func echoSecretHandler() cpi.HandlerFunc {
	return func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return map[string]any{"mbus": "nats://nats:resp-secret@10.0.0.1:4222", "id": "vm-9"}, nil
	}
}

// TestDispatcher_RequestTraceOff verifies that without WithRequestTrace the
// dispatcher emits no request/response payload trace — byte-identical logging.
func TestDispatcher_RequestTraceOff(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := cpi.NewDispatcherWithOptions(debugLogger(t, &buf))
	mustRegister(t, d, "create_vm", echoSecretHandler())

	resp := d.Handle(context.Background(), secretReq())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := buf.String()
	if strings.Contains(out, "cpi request") || strings.Contains(out, "cpi response") {
		t.Errorf("trace emitted with redaction off; log must be byte-identical:\n%s", out)
	}
}

// TestDispatcher_RequestTraceOn verifies the opt-in trace is emitted and that no
// secret literal from request args or the handler result reaches the log buffer.
func TestDispatcher_RequestTraceOn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := cpi.NewDispatcherWithOptions(debugLogger(t, &buf), cpi.WithRequestTrace(true))
	mustRegister(t, d, "create_vm", echoSecretHandler())

	resp := d.Handle(context.Background(), secretReq())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := buf.String()
	if !strings.Contains(out, "cpi request") {
		t.Errorf("expected request trace line, got:\n%s", out)
	}
	if !strings.Contains(out, "cpi response") {
		t.Errorf("expected response trace line, got:\n%s", out)
	}
	for _, secret := range []string{"s3cr3t-mbus", "AKIA-LEAK-ME", "resp-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into trace log:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("expected <redacted> placeholder in trace, got:\n%s", out)
	}
}
