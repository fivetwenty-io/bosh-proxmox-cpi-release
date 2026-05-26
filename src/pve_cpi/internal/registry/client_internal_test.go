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
	"io"
	"math/big"
	"net/http"
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
	// Parse the cert and confirm the pool reports it as a known subject.
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("test PEM did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	subjects := tr.TLSClientConfig.RootCAs.Subjects() //nolint:staticcheck // SystemCertPool may be nil; we only inspect our own pool here for testing.
	found := false
	for _, raw := range subjects {
		if string(raw) == string(cert.RawSubject) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("custom CA subject %q not found in pool subjects (n=%d)", cert.Subject, len(subjects))
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
		endpoint: "https://example",
		user:     "u",
		pass:     "p",
		http:     &http.Client{Transport: tr},
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
