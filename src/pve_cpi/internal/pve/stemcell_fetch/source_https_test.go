package stemcellfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// The source's client must trust both test servers' certs. Build a
	// combined transport that accepts both test CAs.
	srcTransport := sourceServer.Client().Transport.(*http.Transport).Clone()
	tgtTLSConfig := targetServer.Client().Transport.(*http.Transport).TLSClientConfig
	srcTLSConfig := srcTransport.TLSClientConfig
	// Merge: copy RootCAs from target into source transport's TLS config.
	// Simplest: just set InsecureSkipVerify for the redirect test only.
	srcTLSConfig.InsecureSkipVerify = true //nolint:gosec // test-only
	tgtTLSConfig.InsecureSkipVerify = true //nolint:gosec // test-only
	srcTransport.TLSClientConfig = srcTLSConfig

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
