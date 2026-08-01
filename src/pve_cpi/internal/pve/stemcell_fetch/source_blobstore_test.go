package stemcellfetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testBlobstoreSource constructs a blobstoreSource wired to the plain (non-TLS)
// test server's http.Client. Timeout is shortened for tests.
func testBlobstoreSource(server *httptest.Server) *blobstoreSource {
	c := server.Client()
	c.Timeout = 30 * time.Second
	return &blobstoreSource{client: c}
}

// blobstoreCreds builds a blobstoreCredentials pointed at the given server URL.
func blobstoreCreds(server *httptest.Server, username, password string) blobstoreCredentials {
	return blobstoreCredentials{
		Endpoint: server.URL,
		Username: username,
		Password: password,
	}
}

// TestBlobstoreSource_Fetch_Success: server enforces Basic auth and serves a
// known body at /{blobID}; Fetch returns the body and correct content length.
func TestBlobstoreSource_Fetch_Success(t *testing.T) {
	t.Parallel()

	const (
		wantUser   = "agent"
		wantPass   = "hunter2"
		wantBlobID = "abc-123-def-456"
		wantBody   = "stemcell-blob-payload"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != wantUser || p != wantPass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/"+wantBlobID {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "21")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer server.Close()

	src := testBlobstoreSource(server)
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:" + wantBlobID,
		BlobID: wantBlobID,
	}
	creds := blobstoreCreds(server, wantUser, wantPass)

	body, contentLength, err := src.Fetch(ctx, ref, creds)
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	if contentLength != 21 {
		t.Errorf("contentLength = %d, want 21", contentLength)
	}

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

// TestBlobstoreSource_Fetch_404: server returns 404 for an unknown blob;
// Fetch returns an error that mentions 404.
func TestBlobstoreSource_Fetch_404(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	src := testBlobstoreSource(server)
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:missing-blob-id",
		BlobID: "missing-blob-id",
	}
	creds := blobstoreCreds(server, "", "")

	_, _, err := src.Fetch(ctx, ref, creds)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error missing status 404: %v", err)
	}
}

// TestBlobstoreSource_Fetch_401: server returns 401 when credentials are wrong;
// Fetch returns an error that mentions 401.
func TestBlobstoreSource_Fetch_401(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "correct-user" || p != "correct-pass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	src := testBlobstoreSource(server)
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:some-blob",
		BlobID: "some-blob",
	}
	creds := blobstoreCreds(server, "wrong-user", "wrong-pass")

	_, _, err := src.Fetch(ctx, ref, creds)
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error missing status 401: %v", err)
	}
}

// TestBlobstoreSource_Fetch_EndpointTrailingSlash: endpoint with a trailing
// slash and endpoint without one both produce the same final URL.
func TestBlobstoreSource_Fetch_EndpointTrailingSlash(t *testing.T) {
	t.Parallel()

	const (
		blobID   = "trim-slash-test"
		wantBody = "trimmed-ok"
	)

	var capturedPaths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPaths = append(capturedPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer server.Close()

	src := testBlobstoreSource(server)
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:" + blobID,
		BlobID: blobID,
	}

	// With trailing slash.
	credsWithSlash := blobstoreCredentials{Endpoint: server.URL + "/", Username: "", Password: ""}
	body1, _, err := src.Fetch(ctx, ref, credsWithSlash)
	if err != nil {
		t.Fatalf("Fetch (trailing slash): unexpected error: %v", err)
	}
	_ = body1.Close()

	// Without trailing slash.
	credsNoSlash := blobstoreCredentials{Endpoint: server.URL, Username: "", Password: ""}
	body2, _, err := src.Fetch(ctx, ref, credsNoSlash)
	if err != nil {
		t.Fatalf("Fetch (no trailing slash): unexpected error: %v", err)
	}
	_ = body2.Close()

	if len(capturedPaths) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(capturedPaths))
	}
	if capturedPaths[0] != capturedPaths[1] {
		t.Errorf("path mismatch: with-slash=%q, without-slash=%q", capturedPaths[0], capturedPaths[1])
	}
	wantPath := "/" + blobID
	if capturedPaths[0] != wantPath {
		t.Errorf("path = %q, want %q", capturedPaths[0], wantPath)
	}
}

// TestBlobstoreSource_Fetch_EmptyBlobID: empty ref.BlobID returns an error
// without contacting any server.
func TestBlobstoreSource_Fetch_EmptyBlobID(t *testing.T) {
	t.Parallel()

	// No real server needed — error must fire before any network I/O.
	src := newBlobstoreSource(DefaultTransportConfig())
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:",
		BlobID: "",
	}
	creds := blobstoreCredentials{Endpoint: "https://blobstore.example.com", Username: "u", Password: "p"}

	_, _, err := src.Fetch(ctx, ref, creds)
	if err == nil {
		t.Fatal("expected error for empty BlobID, got nil")
	}
	if !strings.Contains(err.Error(), "empty blob id") {
		t.Errorf("error missing 'empty blob id': %v", err)
	}
}

// TestBlobstoreSource_Fetch_NoCreds: noCreds passed to Fetch returns an error
// because no endpoint is available.
func TestBlobstoreSource_Fetch_NoCreds(t *testing.T) {
	t.Parallel()

	src := newBlobstoreSource(DefaultTransportConfig())
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:some-blob",
		BlobID: "some-blob",
	}

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for noCreds, got nil")
	}
	if !strings.Contains(err.Error(), "credentials required") {
		t.Errorf("error missing 'credentials required': %v", err)
	}
}

// TestBlobstoreSource_Fetch_RawAuthCreds: rawAuthCreds containing a valid
// blobstore payload is decoded and used correctly.
func TestBlobstoreSource_Fetch_RawAuthCreds(t *testing.T) {
	t.Parallel()

	const (
		wantUser = "rawuser"
		wantPass = "rawpass"
		blobID   = "raw-creds-blob"
		wantBody = "raw-creds-body"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != wantUser || p != wantPass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer server.Close()

	src := testBlobstoreSource(server)
	ctx := context.Background()
	ref := Reference{
		Scheme: "bosh+blobstore",
		URL:    "bosh+blobstore:" + blobID,
		BlobID: blobID,
	}

	// Build rawAuthCreds as parseAuth would when blobstore type is encountered.
	rawPayload, _ := json.Marshal(map[string]string{
		"type":     "blobstore",
		"endpoint": server.URL,
		"username": wantUser,
		"password": wantPass,
	})
	creds := rawAuthCreds{authType: "blobstore", Raw: json.RawMessage(rawPayload)}

	body, _, err := src.Fetch(ctx, ref, creds)
	if err != nil {
		t.Fatalf("Fetch via rawAuthCreds: unexpected error: %v", err)
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

// ---- parseBlobstoreAuth ----

// TestParseBlobstoreAuth_Valid: well-formed payload with endpoint returns
// populated blobstoreCredentials.
func TestParseBlobstoreAuth_Valid(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]string{
		"type":     "blobstore",
		"endpoint": "https://blobstore.example.com/",
		"username": "bob",
		"password": "s3cr3t",
	})

	c, err := parseBlobstoreAuth(raw)
	if err != nil {
		t.Fatalf("parseBlobstoreAuth: unexpected error: %v", err)
	}
	if c.Endpoint != "https://blobstore.example.com/" {
		t.Errorf("Endpoint = %q, want %q", c.Endpoint, "https://blobstore.example.com/")
	}
	if c.Username != "bob" {
		t.Errorf("Username = %q, want %q", c.Username, "bob")
	}
	if c.Kind() != "blobstore" {
		t.Errorf("Kind() = %q, want %q", c.Kind(), "blobstore")
	}
}

// TestParseBlobstoreAuth_MissingEndpoint: payload without endpoint returns
// an error.
func TestParseBlobstoreAuth_MissingEndpoint(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]string{
		"type":     "blobstore",
		"username": "bob",
		"password": "s3cr3t",
	})

	_, err := parseBlobstoreAuth(raw)
	if err == nil {
		t.Fatal("expected error for missing endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "endpoint is required") {
		t.Errorf("error missing 'endpoint is required': %v", err)
	}
}

// TestParseBlobstoreAuth_MalformedJSON: non-JSON payload returns a parse error.
func TestParseBlobstoreAuth_MalformedJSON(t *testing.T) {
	t.Parallel()

	_, err := parseBlobstoreAuth(json.RawMessage(`{not json}`))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "parse credentials") {
		t.Errorf("error missing 'parse credentials': %v", err)
	}
}

// TestParseBlobstoreAuth_NoUsernameOrPassword: endpoint only (unauthenticated
// blobstore) is valid — username and password are optional.
func TestParseBlobstoreAuth_NoUsernameOrPassword(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]string{
		"type":     "blobstore",
		"endpoint": "https://open-blobstore.example.com",
	})

	c, err := parseBlobstoreAuth(raw)
	if err != nil {
		t.Fatalf("parseBlobstoreAuth: unexpected error for endpoint-only payload: %v", err)
	}
	if c.Username != "" || c.Password != "" {
		t.Errorf("expected empty username/password, got username=%q password=%q", c.Username, c.Password)
	}
}

// ---- ResolveSourceWith bosh+blobstore integration ----

// TestResolveSource_BoshBlobstore: well-formed bosh+blobstore URL returns a
// non-nil Source and a populated Reference with the correct BlobID.
func TestResolveSource_BoshBlobstore(t *testing.T) {
	t.Parallel()

	src, ref, err := ResolveSourceWith("bosh+blobstore:abc-123", DefaultTransportConfig())
	if err != nil {
		t.Fatalf("ResolveSourceWith: unexpected error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil Source, got nil")
	}
	if ref.Scheme != "bosh+blobstore" {
		t.Errorf("ref.Scheme = %q, want %q", ref.Scheme, "bosh+blobstore")
	}
	if ref.BlobID != "abc-123" {
		t.Errorf("ref.BlobID = %q, want %q", ref.BlobID, "abc-123")
	}
	if ref.URL != "bosh+blobstore:abc-123" {
		t.Errorf("ref.URL = %q, want %q", ref.URL, "bosh+blobstore:abc-123")
	}
	// Verify concrete type — blobstoreSource implements Source.
	if _, ok := src.(*blobstoreSource); !ok {
		t.Errorf("expected *blobstoreSource, got %T", src)
	}
}

// TestResolveSource_BoshBlobstore_EmptyBlob: "bosh+blobstore:" with no blob
// id returns an error.
func TestResolveSource_BoshBlobstore_EmptyBlob(t *testing.T) {
	t.Parallel()

	_, _, err := ResolveSourceWith("bosh+blobstore:", DefaultTransportConfig())
	if err == nil {
		t.Fatal("expected error for empty blob id, got nil")
	}
	if !strings.Contains(err.Error(), "empty blob id") {
		t.Errorf("error missing 'empty blob id': %v", err)
	}
}

// ---- blobstoreCredentials.Apply ----

// TestBlobstoreCredentials_Apply_WithAuth: Apply sets Basic auth header when
// both username and password are non-empty.
func TestBlobstoreCredentials_Apply_WithAuth(t *testing.T) {
	t.Parallel()

	c := blobstoreCredentials{
		Endpoint: "https://blobstore.example.com",
		Username: "alice",
		Password: "wonderland",
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://blobstore.example.com/blob", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := c.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	u, p, ok := req.BasicAuth()
	if !ok {
		t.Fatal("BasicAuth() returned ok=false")
	}
	if u != "alice" {
		t.Errorf("username = %q, want %q", u, "alice")
	}
	if p != "wonderland" {
		t.Errorf("password = %q, want %q", p, "wonderland")
	}
}

// TestBlobstoreCredentials_Apply_NoAuth: Apply is a no-op when both username
// and password are empty (unauthenticated blobstore).
func TestBlobstoreCredentials_Apply_NoAuth(t *testing.T) {
	t.Parallel()

	c := blobstoreCredentials{Endpoint: "https://open-blobstore.example.com"}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://open-blobstore.example.com/blob", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := c.Apply(req); err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}

	if req.Header.Get("Authorization") != "" {
		t.Errorf("expected no Authorization header for unauthenticated blobstore, got %q",
			req.Header.Get("Authorization"))
	}
}
