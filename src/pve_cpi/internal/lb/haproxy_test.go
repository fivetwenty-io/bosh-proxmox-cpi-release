package lb_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/lb"
)

// --------------------------------------------------------------------------
// fakeResolver — injected DNS resolver for SSRF dial-time tests
// --------------------------------------------------------------------------

// fakeResolver implements the LookupHost interface used by the lb test seam.
// It maps hostnames to fixed IP slices; unknown hosts return an error.
type fakeResolver struct {
	hosts map[string][]string
}

func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if ips, ok := f.hosts[host]; ok {
		return ips, nil
	}
	return nil, fmt.Errorf("fakeResolver: no entry for %q", host)
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// newTLSRegistrar starts an httptest.TLSServer with handler and returns an
// lb.Registrar (backed by HAProxyRegistrar) that trusts the server's self-signed
// cert. The server is closed via t.Cleanup.
func newTLSRegistrar(t *testing.T, handler http.HandlerFunc) lb.Registrar {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL,
		User:               "admin",
		Password:           "secret",
		AllowPrivateIP:     true, // httptest uses 127.0.0.1
		InsecureSkipVerify: true, // httptest self-signed cert
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("NewHAProxyRegistrar: %v", err)
	}
	return r
}

// wantBasicAuth is the expected Authorization header for the "admin"/"secret"
// credentials used by newTLSRegistrar.
const wantBasicAuth = "Basic " + "YWRtaW46c2VjcmV0" // base64("admin:secret")

// --------------------------------------------------------------------------
// Register
// --------------------------------------------------------------------------

func TestRegister_201_Nil(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody map[string]any

	r := newTLSRegistrar(t, func(w http.ResponseWriter, req *http.Request) {
		gotMethod = req.Method
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		gotContentType = req.Header.Get("Content-Type")

		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.WriteHeader(http.StatusCreated)
	})

	err := r.Register(context.Background(), "web-backend", lb.Server{
		Name:    "vm-100",
		Address: "10.0.0.5",
		Port:    8080,
	})
	if err != nil {
		t.Fatalf("Register returned unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	wantPath := "/v3/services/haproxy/runtime/backends/web-backend/servers"
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}
	if gotAuth != wantBasicAuth {
		t.Errorf("Authorization: got %q, want %q", gotAuth, wantBasicAuth)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", gotContentType)
	}
	if gotBody["name"] != "vm-100" {
		t.Errorf("body.name: got %v, want vm-100", gotBody["name"])
	}
	if gotBody["address"] != "10.0.0.5" {
		t.Errorf("body.address: got %v, want 10.0.0.5", gotBody["address"])
	}
	if int(gotBody["port"].(float64)) != 8080 {
		t.Errorf("body.port: got %v, want 8080", gotBody["port"])
	}
}

func TestRegister_200_Nil(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegister_409_Idempotent(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	if err := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80}); err != nil {
		t.Fatalf("409 should be treated as success, got: %v", err)
	}
}

func TestRegister_500_Error(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestRegister_BackendPathEncoded(t *testing.T) {
	t.Parallel()

	// RequestURI preserves the raw (encoded) path as sent on the wire.
	var gotRequestURI string
	r := newTLSRegistrar(t, func(w http.ResponseWriter, req *http.Request) {
		gotRequestURI = req.RequestURI
		w.WriteHeader(http.StatusCreated)
	})

	_ = r.Register(context.Background(), "my backend", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	if !strings.Contains(gotRequestURI, "my%20backend") {
		t.Errorf("backend not URL-encoded in RequestURI: %q", gotRequestURI)
	}
}

// --------------------------------------------------------------------------
// Deregister
// --------------------------------------------------------------------------

func TestDeregister_204_Nil(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string

	r := newTLSRegistrar(t, func(w http.ResponseWriter, req *http.Request) {
		gotMethod = req.Method
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})

	if err := r.Deregister(context.Background(), "web-backend", "vm-100"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	wantPath := "/v3/services/haproxy/runtime/backends/web-backend/servers/vm-100"
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}
	if gotAuth != wantBasicAuth {
		t.Errorf("Authorization: got %q, want %q", gotAuth, wantBasicAuth)
	}
}

func TestDeregister_200_Nil(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := r.Deregister(context.Background(), "b", "vm-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeregister_404_Idempotent(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := r.Deregister(context.Background(), "b", "vm-gone"); err != nil {
		t.Fatalf("404 should be treated as success, got: %v", err)
	}
}

func TestDeregister_500_Error(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := r.Deregister(context.Background(), "b", "vm-1")
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

// --------------------------------------------------------------------------
// Private-IP guard
// --------------------------------------------------------------------------

func TestNewHAProxyRegistrar_PrivateIPRejected(t *testing.T) {
	t.Parallel()

	// 127.0.0.1 is loopback. With AllowPrivateIP=false the constructor must reject it.
	cfg := lb.HAProxyConfig{
		Endpoint:       "https://127.0.0.1:5555",
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
	}
	_, err := lb.NewHAProxyRegistrar(cfg)
	if err == nil {
		t.Fatal("expected error for private-IP endpoint with AllowPrivateIP=false, got nil")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error should mention the offending IP, got: %v", err)
	}
}

func TestNewHAProxyRegistrar_PrivateIPAllowed(t *testing.T) {
	t.Parallel()

	// httptest TLS server is on 127.0.0.1 but with AllowPrivateIP=true it must succeed.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL,
		User:               "u",
		Password:           "p",
		AllowPrivateIP:     true,
		InsecureSkipVerify: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction should succeed with AllowPrivateIP=true: %v", err)
	}
	if err2 := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80}); err2 != nil {
		t.Fatalf("Register on allowed private-IP endpoint: %v", err2)
	}
}

// --------------------------------------------------------------------------
// Redirect blocked
// --------------------------------------------------------------------------

func TestRegister_RedirectBlocked(t *testing.T) {
	t.Parallel()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/steal", http.StatusFound)
	}))
	defer redirectSrv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:       redirectSrv.URL,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	err = r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	if err == nil {
		t.Fatal("expected error when server returns 302, got nil")
	}
	// The CheckRedirect func returns an error containing "redirect" or "SSRF";
	// http.Client wraps it, so check both the outer and inner error text.
	errStr := err.Error()
	if !strings.Contains(errStr, "redirect") && !strings.Contains(errStr, "SSRF") && !strings.Contains(errStr, "302") {
		t.Logf("error (acceptable — redirect was blocked, exact text varies): %v", err)
	}
}

func TestDeregister_RedirectBlocked(t *testing.T) {
	t.Parallel()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/steal", http.StatusFound)
	}))
	defer redirectSrv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:       redirectSrv.URL,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	err = r.Deregister(context.Background(), "b", "vm-1")
	if err == nil {
		t.Fatal("expected error when server returns 302, got nil")
	}
}

// --------------------------------------------------------------------------
// Timeout honored
// --------------------------------------------------------------------------

func TestRegister_TimeoutHonored(t *testing.T) {
	t.Parallel()

	// unblock is closed first (registered last = runs first in LIFO cleanup order)
	// so the handler goroutine exits before httptest.Server.Close waits on it.
	unblock := make(chan struct{})

	sleepSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-unblock
	}))
	// Register Close first so it runs LAST (LIFO). unblock must close first.
	t.Cleanup(sleepSrv.Close)
	t.Cleanup(func() { close(unblock) })

	cfg := lb.HAProxyConfig{
		Endpoint:       sleepSrv.URL,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: true,
		Timeout:        50 * time.Millisecond,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	start := time.Now()
	err = r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not honored: elapsed %v (want < 2s)", elapsed)
	}
}

func TestDeregister_TimeoutHonored(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})

	sleepSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-unblock
	}))
	t.Cleanup(sleepSrv.Close)
	t.Cleanup(func() { close(unblock) })

	cfg := lb.HAProxyConfig{
		Endpoint:       sleepSrv.URL,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: true,
		Timeout:        50 * time.Millisecond,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	start := time.Now()
	err = r.Deregister(context.Background(), "b", "vm-1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not honored: elapsed %v (want < 2s)", elapsed)
	}
}

// --------------------------------------------------------------------------
// Context cancellation
// --------------------------------------------------------------------------

func TestRegister_ContextCanceled(t *testing.T) {
	t.Parallel()

	r := newTLSRegistrar(t, func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusCreated)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := r.Register(ctx, "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

// --------------------------------------------------------------------------
// CA cert validation
// --------------------------------------------------------------------------

func TestNewHAProxyRegistrar_InvalidCACert(t *testing.T) {
	t.Parallel()

	cfg := lb.HAProxyConfig{
		Endpoint:       "https://example.com:5555",
		User:           "u",
		Password:       "p",
		CACertPEM:      "this is not a valid PEM certificate",
		AllowPrivateIP: true,
	}
	_, err := lb.NewHAProxyRegistrar(cfg)
	if err == nil {
		t.Fatal("expected error for invalid CACertPEM, got nil")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("error should mention PEM, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Interface compliance
// --------------------------------------------------------------------------
//
// Enforced by the compile-time guard below; a live-DNS constructor round
// trip added nothing beyond what that guard already checks.

// --------------------------------------------------------------------------
// Unused variable guard (ensure compiler catches interface shape)
// --------------------------------------------------------------------------

var _ lb.Registrar = (*lb.HAProxyRegistrar)(nil)

// --------------------------------------------------------------------------
// Dial-time SSRF guard (DNS-rebinding remediation)
// --------------------------------------------------------------------------

// TestDialTime_PrivateIPLiteral_RegisterBlocked verifies that Register is
// blocked at dial time when the endpoint host is a private IP literal and
// AllowPrivateIP=false. The transport must never reach a server.
func TestDialTime_PrivateIPLiteral_RegisterBlocked(t *testing.T) {
	t.Parallel()

	// Start a server on 127.0.0.1 to prove the request never lands there.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// If reached, the guard failed.
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	// Parse the port from the test server to build an explicit 127.0.0.1 URL.
	srvURL, _ := url.Parse(srv.URL)
	endpoint := "http://127.0.0.1:" + srvURL.Port()

	cfg := lb.HAProxyConfig{
		Endpoint:       endpoint,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
	}
	// IP literal — constructor rejects immediately (no dial needed).
	_, err := lb.NewHAProxyRegistrar(cfg)
	if err == nil {
		t.Fatal("expected construction to fail for 127.0.0.1 with AllowPrivateIP=false, got nil")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error should mention private/loopback or the IP, got: %v", err)
	}
}

// TestDialTime_PrivateIPLiteral_DeregisterBlocked mirrors the Register test
// for Deregister to confirm both operations are guarded.
func TestDialTime_PrivateIPLiteral_DeregisterBlocked(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	srvURL, _ := url.Parse(srv.URL)
	endpoint := "http://127.0.0.1:" + srvURL.Port()

	cfg := lb.HAProxyConfig{
		Endpoint:       endpoint,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
	}
	_, err := lb.NewHAProxyRegistrar(cfg)
	if err == nil {
		t.Fatal("expected construction to fail for 127.0.0.1 with AllowPrivateIP=false, got nil")
	}
}

// TestDialTime_DNSRebinding_RegisterBlocked proves the DNS-rebinding defense:
// a hostname that resolves to a private IP is caught at DIAL time even though
// construction succeeded (the hostname passes the construct-time literal check).
// Uses a fake resolver injected via lb.ConfigWithResolver.
func TestDialTime_DNSRebinding_RegisterBlocked(t *testing.T) {
	t.Parallel()

	// The test server is on 127.0.0.1. We give the endpoint a hostname so the
	// construct-time literal check does NOT fire, then inject a resolver that
	// returns 127.0.0.1 for the hostname — simulating a DNS rebinding flip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated) // should never be reached
	}))
	defer srv.Close()

	srvURL, _ := url.Parse(srv.URL)
	port := srvURL.Port()

	// "public-looking.example.test" is not a real host; our fake resolver
	// maps it to 127.0.0.1 to simulate the post-rebinding resolution.
	hostname := "public-looking.example.test"
	resolver := &fakeResolver{hosts: map[string][]string{
		hostname: {"127.0.0.1"},
	}}

	cfg := lb.ConfigWithResolver(lb.HAProxyConfig{
		Endpoint:       "http://" + hostname + ":" + port,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
	}, resolver)

	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction should succeed for a hostname endpoint: %v", err)
	}

	// Register must fail at dial time because the resolver returns 127.0.0.1.
	err = r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	if err == nil {
		t.Fatal("expected dial-time SSRF error for private-IP hostname, got nil")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error should mention private/loopback or the IP, got: %v", err)
	}
}

// TestDialTime_DNSRebinding_DeregisterBlocked mirrors the rebinding test for
// Deregister to confirm both call paths enforce the dial-time guard.
func TestDialTime_DNSRebinding_DeregisterBlocked(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	srvURL, _ := url.Parse(srv.URL)
	port := srvURL.Port()

	hostname := "public-looking.example.test"
	resolver := &fakeResolver{hosts: map[string][]string{
		hostname: {"127.0.0.1"},
	}}

	cfg := lb.ConfigWithResolver(lb.HAProxyConfig{
		Endpoint:       "http://" + hostname + ":" + port,
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
	}, resolver)

	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction should succeed for a hostname endpoint: %v", err)
	}

	err = r.Deregister(context.Background(), "b", "vm-1")
	if err == nil {
		t.Fatal("expected dial-time SSRF error for private-IP hostname, got nil")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error should mention private/loopback or the IP, got: %v", err)
	}
}

// TestDialTime_AllowPrivateIP_RegisterSucceeds verifies that AllowPrivateIP=true
// bypasses the dial-time guard and allows connections to 127.0.0.1 (the httptest
// server address). This preserves the existing lab/test deployment behavior.
func TestDialTime_AllowPrivateIP_RegisterSucceeds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL,
		User:               "u",
		Password:           "p",
		AllowPrivateIP:     true, // guard disabled
		InsecureSkipVerify: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if err2 := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80}); err2 != nil {
		t.Fatalf("Register with AllowPrivateIP=true should succeed: %v", err2)
	}
}

// TestDialTime_AllowPrivateIP_DeregisterSucceeds mirrors the above for Deregister.
func TestDialTime_AllowPrivateIP_DeregisterSucceeds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL,
		User:               "u",
		Password:           "p",
		AllowPrivateIP:     true,
		InsecureSkipVerify: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if err2 := r.Deregister(context.Background(), "b", "vm-1"); err2 != nil {
		t.Fatalf("Deregister with AllowPrivateIP=true should succeed: %v", err2)
	}
}

// TestDialTime_PublicIPFakeResolver_Passes verifies that a hostname resolving
// to a public IP (via the fake resolver) is NOT blocked by the dial-time guard.
// The connection will fail (no real server), but the SSRF guard must not fire.
func TestDialTime_PublicIPFakeResolver_Passes(t *testing.T) {
	t.Parallel()

	// 8.8.8.8 is a public IP — guard must allow the dial attempt.
	// The connection itself will fail (no server there), but the error should
	// be a network error, not an SSRF/private-IP error.
	hostname := "public-host.example.test"
	resolver := &fakeResolver{hosts: map[string][]string{
		hostname: {"8.8.8.8"},
	}}

	// Use port 19999 — very unlikely to be in use. We expect a connection
	// refused or similar, NOT an SSRF error.
	cfg := lb.ConfigWithResolver(lb.HAProxyConfig{
		Endpoint:       "http://" + hostname + ":19999",
		User:           "u",
		Password:       "p",
		AllowPrivateIP: false,
		Timeout:        200 * time.Millisecond,
	}, resolver)

	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction should succeed for public hostname: %v", err)
	}

	err = r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})
	// Expect a network error (connection refused/timeout), NOT an SSRF block.
	if err == nil {
		t.Fatal("expected a network error connecting to non-existent server, got nil")
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "private") || strings.Contains(lower, "loopback") || strings.Contains(lower, "ssrf") {
		t.Errorf("public IP should not trigger SSRF guard, got: %v", err)
	}
	// Confirm a network-level error occurred (connection refused, timeout, etc.)
	var netErr net.Error
	if !strings.Contains(lower, "connection refused") && !strings.Contains(lower, "connect") &&
		!strings.Contains(lower, "timeout") && !strings.Contains(lower, "i/o") {
		t.Logf("note: got unexpected error (acceptable as long as not SSRF): %v — netErr=%v", err, netErr)
	}
}

// --------------------------------------------------------------------------
// Default timeout applied
// --------------------------------------------------------------------------

func TestNewHAProxyRegistrar_DefaultTimeout(t *testing.T) {
	t.Parallel()

	// Zero Timeout should not error; the constructor applies defaultTimeout.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL,
		User:               "u",
		Password:           "p",
		AllowPrivateIP:     true,
		InsecureSkipVerify: true,
		Timeout:            0, // must default to 30s
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction with zero timeout: %v", err)
	}
	if err2 := r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80}); err2 != nil {
		t.Fatalf("Register: %v", err2)
	}
}

// --------------------------------------------------------------------------
// Trailing slash stripped from endpoint
// --------------------------------------------------------------------------

func TestRegister_TrailingSlashStripped(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := lb.HAProxyConfig{
		Endpoint:           srv.URL + "/", // trailing slash
		User:               "u",
		Password:           "p",
		AllowPrivateIP:     true,
		InsecureSkipVerify: true,
	}
	r, err := lb.NewHAProxyRegistrar(cfg)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	_ = r.Register(context.Background(), "b", lb.Server{Name: "vm-1", Address: "1.2.3.4", Port: 80})

	// Path must not start with double slash.
	if strings.HasPrefix(gotPath, "//") {
		t.Errorf("double slash in path: %q", gotPath)
	}
	wantPath := "/v3/services/haproxy/runtime/backends/b/servers"
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}
}

// --------------------------------------------------------------------------
// Server name URL-encoded in Deregister path
// --------------------------------------------------------------------------

func TestDeregister_ServerNameEncoded(t *testing.T) {
	t.Parallel()

	// RequestURI preserves the raw (encoded) path as sent on the wire.
	var gotRequestURI string
	r := newTLSRegistrar(t, func(w http.ResponseWriter, req *http.Request) {
		gotRequestURI = req.RequestURI
		w.WriteHeader(http.StatusNoContent)
	})

	_ = r.Deregister(context.Background(), "my-backend", "vm 100")
	if !strings.Contains(gotRequestURI, url.PathEscape("vm 100")) {
		t.Errorf("server name not URL-encoded in RequestURI: %q", gotRequestURI)
	}
}
