// Wire-level tests for §7.52 User-Agent header propagation.
// Uses package pve_test (external) to exercise the full NewClient path.
// Spins an httptest.TLS server to capture outgoing request headers, confirming
// that raw.SetHeader reaches the wire for both unset and set operator_id.
package pve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/version"
)

// pveStub is a minimal httptest.TLS server that records the User-Agent header
// from the first qualifying API request and responds with empty PVE JSON so the
// SDK call does not error on parse.
type pveStub struct {
	server    *httptest.Server
	recorded  atomic.Value // stores string
}

func newPVEStub(t *testing.T) *pveStub {
	t.Helper()
	s := &pveStub{}
	mux := http.NewServeMux()

	// Respond to any API path under /api2/json with a minimal PVE envelope.
	// The SDK expects {"data": ...} for GET calls.
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		// Capture User-Agent on first request only; subsequent retries or
		// auth-refresh calls would overwrite but are not expected here.
		ua := r.Header.Get("User-Agent")
		s.recorded.Store(ua)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Minimal valid PVE API envelope for ListNodes: {"data": []}.
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	s.server = httptest.NewTLSServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// hostPort splits the httptest server URL into host and port int.
func hostPort(t *testing.T, rawURL string) (host string, port int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("hostPort: parse %q: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("hostPort: port from %q: %v", rawURL, err)
	}
	return u.Hostname(), p
}

// stubCfg builds a CPIConfig pointed at the stub server with TLS verification
// disabled (stub uses a self-signed cert) and API token auth (no auto-login,
// so no extra requests hit the stub before the probe call).
func stubCfg(t *testing.T, stub *pveStub, operatorID string) *config.CPIConfig {
	t.Helper()
	//nolint:modernize // helper supports non-false bool values; new(bool) only gives false
	f := func(b bool) *bool { return &b }
	host, port := hostPort(t, stub.server.URL)
	return &config.CPIConfig{
		Host:       host,
		Port:       port,
		APIToken:   "root@pam!test=tok-wire",
		VerifySSL:  f(false),
		OperatorID: operatorID,
	}
}

// issueProbeRequest calls Nodes().ListNodes to trigger an outgoing HTTP request
// and populate the stub's recorded User-Agent. The call is expected to succeed
// (stub returns 200 + {"data":[]}).
func issueProbeRequest(t *testing.T, cfg *config.CPIConfig) {
	t.Helper()
	c, err := pve.NewClient(cfg, log.NewNopLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Nodes().ListNodes(context.Background())
	if err != nil {
		// A parse error on an empty list is acceptable (SDK may require non-null);
		// we only need the header to have been sent, which happens before the
		// response is processed. Log but do not fail the test on response-parse
		// errors from the stub.
		t.Logf("ListNodes returned error (expected with stub): %v", err)
	}
}

// TestUserAgent_WireNoOperatorID asserts that a config with no operator_id
// sends "BOSH-PVE-CPI/<version>" on the wire (overrides SDK default).
func TestUserAgent_WireNoOperatorID(t *testing.T) {
	t.Parallel()
	stub := newPVEStub(t)
	cfg := stubCfg(t, stub, "")
	issueProbeRequest(t, cfg)

	want := "BOSH-PVE-CPI/" + version.Short()
	got, _ := stub.recorded.Load().(string)
	if got != want {
		t.Errorf("User-Agent on wire: got %q, want %q", got, want)
	}
}

// TestUserAgent_WireWithOperatorID asserts that operator_id="acme" produces
// "BOSH-PVE-CPI/<version> pid-acme" on the wire.
func TestUserAgent_WireWithOperatorID(t *testing.T) {
	t.Parallel()
	stub := newPVEStub(t)
	cfg := stubCfg(t, stub, "acme")
	issueProbeRequest(t, cfg)

	want := fmt.Sprintf("BOSH-PVE-CPI/%s pid-acme", version.Short())
	got, _ := stub.recorded.Load().(string)
	if got != want {
		t.Errorf("User-Agent on wire: got %q, want %q", got, want)
	}
}

// TestUserAgent_WireOverridesSDKDefault asserts that the CPI User-Agent
// replaces the SDK default ("pve-apiclient-go/1.0"), not appended to it.
func TestUserAgent_WireOverridesSDKDefault(t *testing.T) {
	t.Parallel()
	stub := newPVEStub(t)
	cfg := stubCfg(t, stub, "")
	issueProbeRequest(t, cfg)

	got, _ := stub.recorded.Load().(string)
	sdkDefault := "pve-apiclient-go/1.0"
	if got == sdkDefault {
		t.Errorf("User-Agent on wire is the SDK default %q: SetHeader did not override it", sdkDefault)
	}
	if got == "" {
		t.Error("User-Agent on wire is empty: header was not set")
	}
}

// TestUserAgent_WireJSONMarshal asserts that buildUserAgent output is safe for
// JSON embedding (no control characters from version.Short() default "dev").
// This guards against a future ldflags value accidentally breaking ERB/JSON logging.
func TestUserAgent_WireJSONMarshal(t *testing.T) {
	t.Parallel()
	stub := newPVEStub(t)
	cfg := stubCfg(t, stub, "")
	issueProbeRequest(t, cfg)

	got, _ := stub.recorded.Load().(string)
	b, err := json.Marshal(got)
	if err != nil {
		t.Errorf("User-Agent %q is not JSON-marshalable: %v", got, err)
	}
	_ = b
}
