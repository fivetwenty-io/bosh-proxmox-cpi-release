package stemcellfetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testHTTPSSource constructs an httpsSource wired to the TLS test server's
// client (which trusts the test cert). Timeout is shortened for tests.
func testHTTPSSource(server *httptest.Server) *httpsSource {
	c := server.Client()
	c.Timeout = 30 * time.Second
	return &httpsSource{client: c}
}

// newTLSPoolFromServers builds a *x509.CertPool containing the leaf certificates
// from each httptest.Server so a single transport can trust all of them without
// disabling verification.
func newTLSPoolFromServers(t *testing.T, servers ...*httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	for _, srv := range servers {
		if srv.TLS == nil || len(srv.TLS.Certificates) == 0 {
			t.Fatalf("newTLSPoolFromServers: server %s has no TLS certificates", srv.URL)
		}
		for _, tlsCert := range srv.TLS.Certificates {
			for _, der := range tlsCert.Certificate {
				cert, err := x509.ParseCertificate(der)
				if err != nil {
					t.Fatalf("newTLSPoolFromServers: parse cert from %s: %v", srv.URL, err)
				}
				pool.AddCert(cert)
			}
		}
	}
	return pool
}

// TestHTTPSSource_Fetch_Unauthenticated: server returns 200 + known body;
// client reads body content and verifies it.
func TestHTTPSSource_Fetch_Unauthenticated(t *testing.T) {
	t.Parallel()

	const wantBody = "stemcell-payload-unauthenticated"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer server.Close()

	src := testHTTPSSource(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/stemcell.qcow2", Scheme: "https"}

	body, _, err := src.Fetch(ctx, ref, noCreds{})
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

// TestHTTPSSource_Fetch_Basic: server requires Basic auth.
// Success case: correct credentials → 200 + body.
// Failure case: wrong credentials → 401 surfaced as error.
func TestHTTPSSource_Fetch_Basic(t *testing.T) {
	t.Parallel()

	const (
		wantUser = "alice"
		wantPass = "s3cr3t"
		wantBody = "basic-auth-ok"
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != wantUser || p != wantPass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	})

	server := httptest.NewTLSServer(handler)
	defer server.Close()

	src := testHTTPSSource(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/stemcell.qcow2", Scheme: "https"}

	t.Run("correct credentials succeed", func(t *testing.T) {
		creds := basicCreds{Username: wantUser, Password: wantPass}
		body, _, err := src.Fetch(ctx, ref, creds)
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		defer func() { _ = body.Close() }()
		got, _ := io.ReadAll(body)
		if string(got) != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
	})

	t.Run("wrong credentials surface 401 as error", func(t *testing.T) {
		creds := basicCreds{Username: "wrong", Password: "wrong"}
		_, _, err := src.Fetch(ctx, ref, creds)
		if err == nil {
			t.Fatal("expected error for wrong credentials, got nil")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error missing status 401: %v", err)
		}
	})
}

// TestHTTPSSource_Fetch_Bearer: server requires Bearer token auth.
// Success case: correct token → 200 + body.
// Failure case: wrong token → 401 surfaced as error.
func TestHTTPSSource_Fetch_Bearer(t *testing.T) {
	t.Parallel()

	const (
		wantToken = "valid-bearer-token-xyz"
		wantBody  = "bearer-auth-ok"
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	})

	server := httptest.NewTLSServer(handler)
	defer server.Close()

	src := testHTTPSSource(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/stemcell.qcow2", Scheme: "https"}

	t.Run("correct token succeeds", func(t *testing.T) {
		creds := bearerCreds{BearerToken: wantToken}
		body, _, err := src.Fetch(ctx, ref, creds)
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		defer func() { _ = body.Close() }()
		got, _ := io.ReadAll(body)
		if string(got) != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
	})

	t.Run("wrong token surfaces 401 as error", func(t *testing.T) {
		creds := bearerCreds{BearerToken: "wrong-token"}
		_, _, err := src.Fetch(ctx, ref, creds)
		if err == nil {
			t.Fatal("expected error for wrong token, got nil")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error missing status 401: %v", err)
		}
	})
}

// TestHTTPSSource_Fetch_404: server returns 404; error must mention 404.
func TestHTTPSSource_Fetch_404(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	src := testHTTPSSource(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/missing.qcow2", Scheme: "https"}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error missing status code 404: %v", err)
	}
}

// TestHTTPSSource_Fetch_Redirect: server returns 302 to a second endpoint;
// http.Client follows the redirect and final body is read correctly.
func TestHTTPSSource_Fetch_Redirect(t *testing.T) {
	t.Parallel()

	const wantBody = "redirect-target-body"

	// Target handler: returns the final body.
	targetMux := http.NewServeMux()
	targetServer := httptest.NewTLSServer(targetMux)
	defer targetServer.Close()

	targetMux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	})

	// Source handler: redirects to /final on targetServer.
	sourceMux := http.NewServeMux()
	sourceServer := httptest.NewTLSServer(sourceMux)
	defer sourceServer.Close()

	sourceMux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetServer.URL+"/final", http.StatusFound)
	})

	// The source's client must trust both test servers' certs. Build a merged
	// pool containing both leaf certs so verification remains enabled.
	pool := newTLSPoolFromServers(t, sourceServer, targetServer)
	srcTransport := sourceServer.Client().Transport.(*http.Transport).Clone()
	srcTransport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12} //nolint:gosec // test-only cert pool

	src := &httpsSource{
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: srcTransport,
		},
	}

	ctx := context.Background()
	ref := Reference{URL: sourceServer.URL + "/redirect", Scheme: "https"}

	body, _, err := src.Fetch(ctx, ref, noCreds{})
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body after redirect = %q, want %q", got, wantBody)
	}
}

// TestHTTPSSource_Fetch_ContentLength: server sets Content-Length header;
// Fetch returns the matching contentLength value.
func TestHTTPSSource_Fetch_ContentLength(t *testing.T) {
	t.Parallel()

	const payload = "content-length-payload-1234"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "27")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	src := testHTTPSSource(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/stemcell.qcow2", Scheme: "https"}

	body, contentLength, err := src.Fetch(ctx, ref, noCreds{})
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	if contentLength != 27 {
		t.Errorf("contentLength = %d, want 27", contentLength)
	}

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("body = %q, want %q", got, payload)
	}
}

// TestHTTPSSource_Fetch_EmptyURL: empty ref.URL returns an error without
// contacting any server.
func TestHTTPSSource_Fetch_EmptyURL(t *testing.T) {
	t.Parallel()

	src := newHTTPSSource()
	ctx := context.Background()
	ref := Reference{URL: "", Scheme: "https"}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Errorf("error missing 'empty URL': %v", err)
	}
}

// TestHTTPSSource_Fetch_NonHTTPS: ref.URL with http:// scheme returns an
// error without contacting any server (scheme guard).
func TestHTTPSSource_Fetch_NonHTTPS(t *testing.T) {
	t.Parallel()

	src := newHTTPSSource()
	ctx := context.Background()
	ref := Reference{URL: "http://example.com/stemcell.qcow2", Scheme: "https"}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for http:// URL, got nil")
	}
	if !strings.Contains(err.Error(), "expected scheme https") {
		t.Errorf("error missing scheme guard message: %v", err)
	}
}

// newTestHTTPSSourceWithRedirectPolicy returns an httpsSource wired to server's
// TLS cert pool and with httpsOnlyRedirect installed. Use for tests that
// exercise redirect policy — not InsecureSkipVerify — so TLS verification
// remains active on every hop.
func newTestHTTPSSourceWithRedirectPolicy(server *httptest.Server) *httpsSource {
	transport := server.Client().Transport.(*http.Transport).Clone()
	return &httpsSource{
		client: &http.Client{
			Timeout:       30 * time.Second,
			Transport:     transport,
			CheckRedirect: httpsOnlyRedirect,
		},
	}
}

// TestHTTPSSource_Fetch_RefusesDowngradeRedirect verifies that httpsOnlyRedirect
// rejects any 302 whose Location header carries an http:// (non-TLS) target.
// Production error string: `stemcell_fetch(https): refusing redirect to non-https URL <url>`
func TestHTTPSSource_Fetch_RefusesDowngradeRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to a plain http:// URL — scheme downgrade.
		http.Redirect(w, r, "http://192.0.2.1/stemcell.qcow2", http.StatusFound)
	}))
	defer server.Close()

	src := newTestHTTPSSourceWithRedirectPolicy(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/stemcell.qcow2", Scheme: "https"}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for https→http redirect, got nil")
	}
	if !strings.Contains(err.Error(), "refusing redirect to non-https") {
		t.Errorf("error missing scheme-downgrade refusal text: %v", err)
	}
}

// TestHTTPSSource_Fetch_RedirectLoopCapped verifies that httpsOnlyRedirect stops
// following redirects after 10 hops. The test wires a chained https→https
// redirect loop on a single httptest.TLSServer and counts actual server hits via
// an atomic counter.
//
// Production error string: `stemcell_fetch(https): stopped after 10 redirects`
//
// Request-count mechanics: Go's http.Client calls CheckRedirect(req, via) where
// via holds all prior requests. httpsOnlyRedirect fires when len(via) >= 10.
// That check happens BEFORE dispatching the 11th request, so the server only
// ever receives 10 requests (1 initial + 9 followed redirects).
func TestHTTPSSource_Fetch_RedirectLoopCapped(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64

	var serverURL string // filled in after server starts

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		// Always redirect to the next hop URL. CheckRedirect fires before the
		// 11th dispatch, so this handler is called exactly 10 times.
		next := fmt.Sprintf("%s/hop/%d", serverURL, n)
		http.Redirect(w, r, next, http.StatusFound)
	})

	server := httptest.NewTLSServer(handler)
	defer server.Close()
	serverURL = server.URL

	src := newTestHTTPSSourceWithRedirectPolicy(server)
	ctx := context.Background()
	ref := Reference{URL: server.URL + "/hop/0", Scheme: "https"}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error after redirect cap, got nil")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("error missing redirect-cap text: %v", err)
	}

	// CheckRedirect fires when len(via)==10, before the 11th request is sent.
	// Server receives: 1 initial + 9 followed = 10 total.
	const wantRequests = 10
	if got := requestCount.Load(); got != wantRequests {
		t.Errorf("server received %d requests, want %d", got, wantRequests)
	}
}
