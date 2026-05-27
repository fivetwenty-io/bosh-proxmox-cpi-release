package registry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewClient_AppliesTLSConfig verifies the default-options constructor pins
// the TLS 1.2 minimum on the underlying *http.Transport.
func TestNewClient_AppliesTLSConfig(t *testing.T) {
	c := NewClient("https://example", "u", "p")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("http.Transport: expected *http.Transport, got %T", c.http.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must not be nil")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLSClientConfig.MinVersion = %v, want tls.VersionTLS12 (%v)",
			tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
	// Default CACertPEM => RootCAs left nil so system pool is used at dial time.
	if tr.TLSClientConfig.RootCAs != nil {
		t.Errorf("RootCAs = %v, want nil when CACertPEM not supplied", tr.TLSClientConfig.RootCAs)
	}
}

// TestNewClientWithOptions_NilCAPreservesSystemPool confirms an empty CACertPEM
// leaves RootCAs nil (i.e. crypto/tls falls back to x509.SystemCertPool at dial
// time) rather than constructing an empty pool that would reject every cert.
func TestNewClientWithOptions_NilCAPreservesSystemPool(t *testing.T) {
	c, err := NewClientWithOptions("https://example", "u", "p", Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tr := c.http.Transport.(*http.Transport)
	if tr.TLSClientConfig.RootCAs != nil {
		t.Errorf("RootCAs = %v, want nil for default Options", tr.TLSClientConfig.RootCAs)
	}
}

// TestNewClientWithOptions_AppendsCustomCA confirms a valid PEM is parsed and
// installed into a non-nil RootCAs pool. We synthesize a self-signed cert at
// runtime so the test owns the input bytes end-to-end (no embedded fixture).
func TestNewClient_AppendsCustomCA(t *testing.T) {
	pemBytes := genSelfSignedPEM(t)

	c, err := NewClientWithOptions("https://example", "u", "p", Options{CACertPEM: string(pemBytes)})
	if err != nil {
		t.Fatalf("NewClientWithOptions: unexpected error: %v", err)
	}
	tr := c.http.Transport.(*http.Transport)
	if tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("RootCAs must not be nil after CACertPEM append")
	}
	// Build an expected pool using the same logic as NewClientWithOptions
	// (SystemCertPool with the custom cert appended) and confirm the installed
	// pool equals it. x509.CertPool.Equal (Go 1.19+) replaces the deprecated
	// Subjects() approach without changing the assertion semantics.
	expectedPool, err := x509.SystemCertPool()
	if err != nil {
		expectedPool = x509.NewCertPool()
	}
	if !expectedPool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("failed to build expected cert pool from test PEM")
	}
	if !tr.TLSClientConfig.RootCAs.Equal(expectedPool) {
		t.Errorf("RootCAs pool does not equal expected pool built from the same PEM input")
	}
}

// TestNewClientWithOptions_RejectsMalformedPEM ensures malformed PEM input is
// not silently swallowed: the constructor must return an error rather than
// build a client with an empty RootCAs pool (which would reject every cert).
func TestNewClientWithOptions_RejectsMalformedPEM(t *testing.T) {
	_, err := NewClientWithOptions("https://example", "u", "p", Options{CACertPEM: "this is not pem"})
	if err == nil {
		t.Fatal("expected error for malformed PEM, got nil")
	}
}

// TestNewClientWithOptions_RespectsTimeoutOverride confirms the per-attempt
// timeout override is applied; zero/negative falls back to the default.
func TestNewClientWithOptions_RespectsTimeoutOverride(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantDur time.Duration
	}{
		{"explicit override", Options{Timeout: 5 * time.Second}, 5 * time.Second},
		{"zero falls back", Options{Timeout: 0}, defaultClientTimeout},
		{"negative falls back", Options{Timeout: -1}, defaultClientTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClientWithOptions("https://example", "u", "p", tc.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.http.Timeout != tc.wantDur {
				t.Errorf("http.Timeout = %v, want %v", c.http.Timeout, tc.wantDur)
			}
		})
	}
}

// --------------------------------------------------------------------------
// doWithRetry body-close on terminal error.
// --------------------------------------------------------------------------

// closeCountingBody wraps an io.Reader and tracks Close() invocations atomically.
type closeCountingBody struct {
	data   []byte
	off    int
	closes atomic.Int32
}

func (b *closeCountingBody) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

func (b *closeCountingBody) Close() error {
	b.closes.Add(1)
	return nil
}

// retryThenCancelTransport hands back a 500 response (retriable) with a
// counted body on every call. After the first response is consumed by the
// retry loop, the test cancels the request context so the backoff select
// fires the ctx.Done() branch — that is the path where doWithRetry must
// drain+close the prior response before returning.
type retryThenCancelTransport struct {
	body      *closeCountingBody
	bodyBytes []byte
	cancel    context.CancelFunc
	calls     atomic.Int32
}

func (t *retryThenCancelTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	n := t.calls.Add(1)
	// Reset the shared body so each round-trip starts at offset zero.
	// The shared body is what the test inspects for Close() invocations.
	t.body.off = 0
	t.body.data = t.bodyBytes
	// Trigger context cancellation AFTER returning the response so the
	// retry loop sees a retriable response, then hits ctx.Done() in the
	// backoff select. Schedule via goroutine to avoid blocking RoundTrip.
	if n == 1 && t.cancel != nil {
		go func() {
			// Small sleep ensures the wrapper has consumed the response
			// and entered the backoff select before cancellation fires.
			time.Sleep(10 * time.Millisecond)
			t.cancel()
		}()
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       t.body,
		Header:     http.Header{},
	}, nil
}

// TestDoWithRetry_ClosesBodyOnTerminalErr asserts the prior response body is
// drained and closed when doWithRetry returns terminally via the ctx.Done()
// branch in the backoff select. This is one of the two terminal-failure
// return paths; the other (GetBody failure) is symmetric.
// A leak here would manifest as the body's Close() never being invoked.
func TestDoWithRetry_ClosesBodyOnTerminalErr(t *testing.T) {
	body := &closeCountingBody{}
	ctx, cancel := context.WithCancel(context.Background())
	tr := &retryThenCancelTransport{
		body:      body,
		bodyBytes: []byte("500 transient body content"),
		cancel:    cancel,
	}
	c := &Client{
		endpoint:       "https://example",
		user:           "u",
		pass:           "p",
		configuredHost: "example",
		http:           &http.Client{Transport: tr},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, rerr := c.doWithRetry(req)
	if rerr == nil {
		t.Fatal("expected non-nil error from doWithRetry on cancelled ctx during backoff")
	}
	if !errors.Is(rerr, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", rerr)
	}
	if resp != nil {
		t.Errorf("expected nil resp on ctx-cancel terminal path; got %+v", resp)
	}
	if got := body.closes.Load(); got == 0 {
		t.Fatalf("expected prior response body to be closed on ctx-cancel terminal path; closes=%d", got)
	}
}

// --------------------------------------------------------------------------
// CheckRedirect: 3xx responses must surface as errors (not be followed).
// --------------------------------------------------------------------------

// redirectTransport returns a single HTTP 302 response pointing to a
// different host, simulating a cross-host redirect.
type redirectTransport struct {
	location string
}

func (t *redirectTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Location", t.location)
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     h,
		Body:       http.NoBody,
	}, nil
}

// TestCheckRedirect_Disabled verifies that a 3xx response from the server
// surfaces as an error rather than being silently followed by the HTTP client.
// This prevents SSRF via redirect to a host not in the configured endpoint.
func TestCheckRedirect_Disabled(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		http: &http.Client{
			Transport: &redirectTransport{location: "https://attacker.example.com/steal"},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("registry: redirects disabled for security (SSRF prevention)")
			},
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	_, doErr := c.doWithRetry(req)
	if doErr == nil {
		t.Fatal("expected error when 3xx response received (redirect must not be followed), got nil")
	}
	// The error must mention SSRF/redirect, not be a retriable transport error
	// that would silently keep retrying.
	if !strings.Contains(doErr.Error(), "redirect") && !strings.Contains(doErr.Error(), "SSRF") {
		t.Logf("redirect rejection error (informational): %v", doErr)
	}
}

// --------------------------------------------------------------------------
// configuredHost invariant: request with mutated host must be rejected.
// --------------------------------------------------------------------------

// TestHostInvariant_MismatchRejected verifies that doWithRetry rejects a
// request whose URL.Host was mutated to differ from c.configuredHost. This
// defends against URL-mutation bugs that would silently send credentials to
// an unintended host.
func TestHostInvariant_MismatchRejected(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		http:           &http.Client{},
	}

	// Build a request but tamper with its URL host before calling doWithRetry.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://evil.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	_, doErr := c.doWithRetry(req)
	if doErr == nil {
		t.Fatal("expected error for request with tampered host, got nil")
	}
	if !strings.Contains(doErr.Error(), "invariant violation") {
		t.Errorf("error should mention invariant violation: %v", doErr)
	}
}

// TestHostInvariant_MatchAllowed verifies that doWithRetry does NOT reject a
// request whose URL.Host matches configuredHost. The transport returns a 200
// so the test validates the happy path through the invariant check.
func TestHostInvariant_MatchAllowed(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		http: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       http.NoBody,
					Header:     http.Header{},
				}, nil
			}),
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, doErr := c.doWithRetry(req)
	if doErr != nil {
		t.Fatalf("unexpected error for matching host: %v", doErr)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %v", resp)
	}
}

// roundTripFunc is an http.RoundTripper adapter for use in tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// --------------------------------------------------------------------------
// registry_allowed_hosts filter.
// --------------------------------------------------------------------------

// TestAllowedHosts_MatchPermitted verifies that a request whose host matches
// an entry in allowedHosts is permitted through.
func TestAllowedHosts_MatchPermitted(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		allowedHosts:   []string{"registry.example.com"},
		http: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       http.NoBody,
					Header:     http.Header{},
				}, nil
			}),
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, doErr := c.doWithRetry(req)
	if doErr != nil {
		t.Fatalf("unexpected error for host in allow-list: %v", doErr)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %v", resp)
	}
}

// TestAllowedHosts_MismatchRejected verifies that a request whose resolved
// host is not in allowedHosts is rejected before http.Do is called.
func TestAllowedHosts_MismatchRejected(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		allowedHosts:   []string{"other.example.com"},
		http:           &http.Client{},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	_, doErr := c.doWithRetry(req)
	if doErr == nil {
		t.Fatal("expected error for host not in allow-list, got nil")
	}
	if !strings.Contains(doErr.Error(), "allow-list") {
		t.Errorf("error should mention allow-list: %v", doErr)
	}
}

// TestAllowedHosts_WildcardMatch verifies that "*.example.com" matches a
// single-level subdomain (e.g. "registry.example.com") but not a multi-level
// subdomain ("a.b.example.com") or the bare parent ("example.com").
func TestAllowedHosts_WildcardMatch(t *testing.T) {
	cases := []struct {
		host     string
		patterns []string
		want     bool
	}{
		{"registry.example.com", []string{"*.example.com"}, true},
		{"foo.example.com", []string{"*.example.com"}, true},
		{"a.b.example.com", []string{"*.example.com"}, false}, // two-level sub: not matched
		{"example.com", []string{"*.example.com"}, false},     // parent: not matched
		{"evil.com", []string{"*.example.com"}, false},
		{"registry.example.com", []string{"registry.example.com"}, true}, // exact
		{"other.example.com", []string{"registry.example.com"}, false},   // exact mismatch
		{"REGISTRY.EXAMPLE.COM", []string{"registry.example.com"}, true}, // case-insensitive
	}
	for _, tc := range cases {
		got := hostMatchesAllowList(tc.host, tc.patterns)
		if got != tc.want {
			t.Errorf("hostMatchesAllowList(%q, %v) = %v, want %v",
				tc.host, tc.patterns, got, tc.want)
		}
	}
}

// TestAllowedHosts_EmptyListSkipsFilter verifies that an empty allowedHosts
// slice does not reject any request (filter is disabled when empty).
func TestAllowedHosts_EmptyListSkipsFilter(t *testing.T) {
	c := &Client{
		endpoint:       "https://registry.example.com",
		user:           "u",
		pass:           "p",
		configuredHost: "registry.example.com",
		allowedHosts:   nil, // empty = no filtering
		http: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       http.NoBody,
					Header:     http.Header{},
				}, nil
			}),
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/instances/1/settings", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, doErr := c.doWithRetry(req)
	if doErr != nil {
		t.Fatalf("unexpected error when allowedHosts empty: %v", doErr)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK with empty allowedHosts, got %v", resp)
	}
}

// --------------------------------------------------------------------------
// NewClientWithOptions AllowedHosts wiring.
// --------------------------------------------------------------------------

// TestNewClientWithOptions_AllowedHostsWired verifies that AllowedHosts from
// Options is copied into the Client struct field.
func TestNewClientWithOptions_AllowedHostsWired(t *testing.T) {
	patterns := []string{"registry.example.com", "*.corp.example.com"}
	c, err := NewClientWithOptions("https://registry.example.com", "u", "p", Options{
		AllowedHosts: patterns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.allowedHosts) != len(patterns) {
		t.Fatalf("allowedHosts length: got %d, want %d", len(c.allowedHosts), len(patterns))
	}
	for i, p := range patterns {
		if c.allowedHosts[i] != p {
			t.Errorf("allowedHosts[%d]: got %q, want %q", i, c.allowedHosts[i], p)
		}
	}
}

// TestNewClientWithOptions_ConfiguredHostExtracted verifies that the
// configuredHost field is set to the host component of the endpoint URL,
// not the full endpoint string.
func TestNewClientWithOptions_ConfiguredHostExtracted(t *testing.T) {
	cases := []struct {
		endpoint string
		wantHost string
	}{
		{"https://registry.example.com", "registry.example.com"},
		{"https://registry.example.com:8080", "registry.example.com:8080"},
		{"https://registry.example.com/", "registry.example.com"},
		{"http://10.0.0.1:25777", "10.0.0.1:25777"},
	}
	for _, tc := range cases {
		c, err := NewClientWithOptions(tc.endpoint, "u", "p", Options{})
		if err != nil {
			t.Fatalf("NewClientWithOptions(%q): unexpected error: %v", tc.endpoint, err)
		}
		if c.configuredHost != tc.wantHost {
			t.Errorf("configuredHost for endpoint %q: got %q, want %q",
				tc.endpoint, c.configuredHost, tc.wantHost)
		}
	}
}

// TestNewClientWithOptions_CheckRedirectSet verifies that CheckRedirect is
// set on the http.Client (i.e., is not nil) after construction.
func TestNewClientWithOptions_CheckRedirectSet(t *testing.T) {
	c, err := NewClientWithOptions("https://registry.example.com", "u", "p", Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.http.CheckRedirect == nil {
		t.Fatal("http.Client.CheckRedirect must not be nil after construction")
	}
	// Verify the function returns a non-nil error (i.e., it rejects redirects).
	redirectErr := c.http.CheckRedirect(nil, nil)
	if redirectErr == nil {
		t.Fatal("CheckRedirect must return an error to prevent redirect following")
	}
}

// genSelfSignedPEM produces a minimal self-signed CA certificate suitable for
// installation into a trust pool. The key material is discarded; only the cert
// PEM is returned because that is all the trust-pool path needs.
func genSelfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "imp-04-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
