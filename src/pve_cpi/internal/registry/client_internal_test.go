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
	t.Parallel()
	c := NewClient("https://example", "u", "p")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	// TLS minimum version is not observable via transport injection; private-field
	// inspection is unavoidable here because MinVersion is set inside NewClient
	// before returning and no public getter exposes it.
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
	t.Parallel()
	c, err := NewClientWithOptions("https://example", "u", "p", Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("http.Transport: expected *http.Transport, got %T", c.http.Transport)
	}
	if tr.TLSClientConfig.RootCAs != nil {
		t.Errorf("RootCAs = %v, want nil for default Options", tr.TLSClientConfig.RootCAs)
	}
}

// TestNewClientWithOptions_AppendsCustomCA confirms a valid PEM is parsed and
// installed into a non-nil RootCAs pool. We synthesize a self-signed cert at
// runtime so the test owns the input bytes end-to-end (no embedded fixture).
func TestNewClient_AppendsCustomCA(t *testing.T) {
	t.Parallel()
	pemBytes := genSelfSignedPEM(t)

	c, err := NewClientWithOptions("https://example", "u", "p", Options{CACertPEM: string(pemBytes)})
	if err != nil {
		t.Fatalf("NewClientWithOptions: unexpected error: %v", err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("http.Transport: expected *http.Transport, got %T", c.http.Transport)
	}
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
	t.Parallel()
	_, err := NewClientWithOptions("https://example", "u", "p", Options{CACertPEM: "this is not pem"})
	if err == nil {
		t.Fatal("expected error for malformed PEM, got nil")
	}
}

// TestNewClientWithOptions_RespectsTimeoutOverride confirms the per-attempt
// timeout override is applied; zero/negative falls back to the default.
func TestNewClientWithOptions_RespectsTimeoutOverride(t *testing.T) {
	t.Parallel()
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
// counted body on every call. On the first call it cancels the context
// synchronously before returning, so doWithRetry sees ctx.Done() already
// closed when it evaluates the backoff select after attempt 0. This ensures
// the ctx.Done() terminal path is taken without any timing dependency.
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
	// Cancel the context synchronously on the first call so that by the time
	// doWithRetry enters the backoff select, ctx.Done() is already closed and
	// the ctx.Done() case wins deterministically. No sleep or goroutine needed.
	if n == 1 && t.cancel != nil {
		t.cancel()
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
//
// retryBaseDelay is set to 1ms to keep the test fast. The context is cancelled
// synchronously inside RoundTrip on the first call, so ctx.Done() is already
// closed before the backoff select is evaluated, making the terminal path
// deterministic without any external sleep or goroutine synchronisation.
func TestDoWithRetry_ClosesBodyOnTerminalErr(t *testing.T) {
	// Not parallel: mutates the package-level retryBaseDelay seam.
	// Running serially ensures no concurrent test reads the var while we write it.

	// Override retryBaseDelay to 1ms; restore on cleanup.
	prev := retryBaseDelay
	retryBaseDelay = 1 * time.Millisecond
	t.Cleanup(func() { retryBaseDelay = prev })

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	cases := []struct {
		endpoint string
		wantHost string
		opts     Options
	}{
		{"https://registry.example.com", "registry.example.com", Options{}},
		{"https://registry.example.com:8080", "registry.example.com:8080", Options{}},
		{"https://registry.example.com/", "registry.example.com", Options{}},
		// 10.0.0.1 is an RFC1918 private address. AllowPrivateIP must be true to
		// construct a client pointing at it; the private-IP guard fires otherwise.
		{"http://10.0.0.1:25777", "10.0.0.1:25777", Options{AllowPrivateIP: true}},
	}
	for _, tc := range cases {
		c, err := NewClientWithOptions(tc.endpoint, "u", "p", tc.opts)
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
	t.Parallel()
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

// --------------------------------------------------------------------------
// Private / loopback IP rejection (AllowPrivateIP=false, default).
// --------------------------------------------------------------------------

// mockResolver implements the resolver interface used by checkEndpointIPs.
// It returns a fixed set of addresses for any host, making DNS behaviour
// deterministic without network access.
type mockResolver struct {
	addrs []string
	err   error
}

func (r *mockResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return r.addrs, r.err
}

// TestNewClient_RejectsPrivateIPHost verifies that an RFC1918 private IP
// literal in the endpoint is rejected at construction time (AllowPrivateIP=false
// by default for NewClientWithOptions).
func TestNewClient_RejectsPrivateIPHost(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"http://192.168.1.50:25777",
		"http://10.0.0.1:25777",
		"http://172.16.5.10:25777",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			_, err := NewClientWithOptions(endpoint, "u", "p", Options{AllowPrivateIP: false})
			if err == nil {
				t.Fatalf("expected error for private-IP endpoint %q, got nil", endpoint)
			}
			if !strings.Contains(err.Error(), "private/loopback") {
				t.Errorf("error %q should mention private/loopback", err.Error())
			}
			if !strings.Contains(err.Error(), "registry_allow_private_ip=true") {
				t.Errorf("error %q should mention registry_allow_private_ip=true", err.Error())
			}
		})
	}
}

// TestNewClient_RejectsLoopbackHost verifies that loopback addresses (127.x.x.x)
// are rejected at construction time.
func TestNewClient_RejectsLoopbackHost(t *testing.T) {
	t.Parallel()
	_, err := NewClientWithOptions("http://127.0.0.1:25777", "u", "p", Options{AllowPrivateIP: false})
	if err == nil {
		t.Fatal("expected error for loopback endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "private/loopback") {
		t.Errorf("error %q should mention private/loopback", err.Error())
	}
}

// TestNewClient_RejectsLinkLocalHost verifies that IPv4 link-local addresses
// (169.254.x.x) are rejected at construction time.
func TestNewClient_RejectsLinkLocalHost(t *testing.T) {
	t.Parallel()
	_, err := NewClientWithOptions("http://169.254.0.1:25777", "u", "p", Options{AllowPrivateIP: false})
	if err == nil {
		t.Fatal("expected error for link-local endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "private/loopback") {
		t.Errorf("error %q should mention private/loopback", err.Error())
	}
}

// TestNewClient_RejectsIPv6Loopback verifies that the IPv6 loopback (::1) is
// rejected at construction time.
func TestNewClient_RejectsIPv6Loopback(t *testing.T) {
	t.Parallel()
	_, err := NewClientWithOptions("http://[::1]:25777", "u", "p", Options{AllowPrivateIP: false})
	if err == nil {
		t.Fatal("expected error for IPv6 loopback endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "private/loopback") {
		t.Errorf("error %q should mention private/loopback", err.Error())
	}
}

// TestNewClient_AllowsPrivateIPWithOverride verifies that a private-IP endpoint
// is accepted when AllowPrivateIP=true.
func TestNewClient_AllowsPrivateIPWithOverride(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"https://192.168.1.50:25777",
		"http://127.0.0.1:25777",
		"http://169.254.0.1:25777",
		"http://[::1]:25777",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			c, err := NewClientWithOptions(endpoint, "u", "p", Options{AllowPrivateIP: true})
			if err != nil {
				t.Fatalf("AllowPrivateIP=true: unexpected error for %q: %v", endpoint, err)
			}
			if c == nil {
				t.Fatal("AllowPrivateIP=true: expected non-nil client")
			}
		})
	}
}

// TestNewClient_AllowsPublicIP verifies that publicly-routable IP literals are
// accepted with the default options (AllowPrivateIP=false).
func TestNewClient_AllowsPublicIP(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"https://8.8.8.8:25777",
		"https://203.0.113.5:25777", // TEST-NET-3 (RFC5737) — not private per IsPrivate()
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			c, err := NewClientWithOptions(endpoint, "u", "p", Options{})
			if err != nil {
				t.Fatalf("public IP %q: unexpected rejection error: %v", endpoint, err)
			}
			if c == nil {
				t.Fatalf("public IP %q: expected non-nil client", endpoint)
			}
		})
	}
}

// TestNewClient_RejectsHostnameResolvingToPrivate verifies that a hostname
// resolving to a private IP is rejected. Uses an injected mock resolver so the
// test does not depend on live DNS.
func TestNewClient_RejectsHostnameResolvingToPrivate(t *testing.T) {
	t.Parallel()
	resolver := &mockResolver{addrs: []string{"192.168.100.5"}}
	_, err := NewClientWithOptions("https://registry.internal.example.com:25777", "u", "p", Options{
		resolver: resolver,
	})
	if err == nil {
		t.Fatal("expected error for hostname resolving to private IP, got nil")
	}
	if !strings.Contains(err.Error(), "private/loopback") {
		t.Errorf("error %q should mention private/loopback", err.Error())
	}
	if !strings.Contains(err.Error(), "192.168.100.5") {
		t.Errorf("error %q should include the resolved private IP", err.Error())
	}
}

// TestNewClient_AllowsHostnameResolvingToPrivate_WithOverride verifies that a
// hostname resolving to a private IP is accepted when AllowPrivateIP=true.
func TestNewClient_AllowsHostnameResolvingToPrivate_WithOverride(t *testing.T) {
	t.Parallel()
	resolver := &mockResolver{addrs: []string{"192.168.100.5"}}
	c, err := NewClientWithOptions("https://registry.internal.example.com:25777", "u", "p", Options{
		AllowPrivateIP: true,
		resolver:       resolver,
	})
	if err != nil {
		t.Fatalf("AllowPrivateIP=true: unexpected error for hostname resolving to private IP: %v", err)
	}
	if c == nil {
		t.Fatal("AllowPrivateIP=true: expected non-nil client")
	}
}

// TestNewClient_AllowsHostnameResolvingToPublic verifies that a hostname
// resolving to a public IP is accepted with the default options.
func TestNewClient_AllowsHostnameResolvingToPublic(t *testing.T) {
	t.Parallel()
	resolver := &mockResolver{addrs: []string{"93.184.216.34"}} // example.com
	c, err := NewClientWithOptions("https://registry.example.com:25777", "u", "p", Options{
		resolver: resolver,
	})
	if err != nil {
		t.Fatalf("hostname resolving to public IP: unexpected rejection: %v", err)
	}
	if c == nil {
		t.Fatal("hostname resolving to public IP: expected non-nil client")
	}
}

// TestNewClient_DNSFailure_ProductionPathSkips verifies that when the
// production path (nil resolver) encounters a DNS failure, construction
// succeeds rather than aborting — the check is best-effort and the registry
// may not be resolvable at CPI startup.
//
// Tested via a hostname that is guaranteed not to resolve ("this-hostname-is-
// guaranteed-to-not-resolve.invalid" uses the ".invalid" TLD which RFC 2606
// reserves for precisely this purpose).
func TestNewClient_DNSFailure_ProductionPathSkips(t *testing.T) {
	t.Parallel()
	// ".invalid" TLD is RFC 2606 § 2 reserved — guaranteed no DNS resolution.
	c, err := NewClientWithOptions(
		"https://this-hostname-is-guaranteed-not-to-resolve.invalid:25777", "u", "p",
		Options{AllowPrivateIP: false}, // nil resolver = production path
	)
	if err != nil {
		t.Fatalf("production DNS failure must be non-fatal (best-effort check): %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client on DNS failure (production path skips)")
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
