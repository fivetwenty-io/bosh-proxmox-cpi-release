package stemcellfetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// blobstoreSource implements Source for "bosh+blobstore:<blob-id>" references.
// Minimal davcli-style HTTP client: GET {endpoint}/{blobID} with Basic auth.
// No external BOSH SDK dependency — keeps the CPI portable across Director
// versions.
type blobstoreSource struct {
	client *http.Client
}

// newBlobstoreSource returns a Source whose http.Client uses a 30-minute
// timeout and a TLS 1.2 floor. Large stemcells on slow Director blobstores
// can saturate the default 5-second timeout.
func newBlobstoreSource() *blobstoreSource {
	return &blobstoreSource{
		client: &http.Client{
			Timeout:   30 * time.Minute,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		},
	}
}

// blobstoreCredentials carries the davcli-style HTTP basic auth and blobstore
// endpoint for a BOSH Director blobstore.
//
// JSON shape: {"type":"blobstore","endpoint":"https://...","username":"...","password":"..."}
// username and password are individually optional for unauthenticated blobstores,
// but endpoint is required.
type blobstoreCredentials struct {
	// Type is read during JSON dispatch but not used at runtime.
	Endpoint string `json:"endpoint"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Apply sets HTTP Basic auth when credentials are present. No-op when both
// Username and Password are empty (unauthenticated blobstore).
func (b blobstoreCredentials) Apply(req *http.Request) error {
	if b.Username != "" || b.Password != "" {
		req.SetBasicAuth(b.Username, b.Password)
	}
	return nil
}

// Kind reports the auth scheme for logging and diagnostics.
func (blobstoreCredentials) Kind() string { return "blobstore" }

// parseBlobstoreAuth deserializes raw into blobstoreCredentials. Returns an
// error when endpoint is empty; username and password are individually optional.
//
// Failure modes:
//   - JSON unmarshal error → wrapped error
//   - empty endpoint → error (endpoint is required to build the fetch URL)
//
// SECURITY: when endpoint scheme is http (not https), Basic-auth credentials
// are transmitted in plaintext. A structured WARN log cannot be emitted here
// because blobstoreSource carries no logger field. Wire a logger into
// blobstoreSource and emit the warning (mirroring config.emitRegistryInsecureWarning)
// before accepting the http endpoint.
func parseBlobstoreAuth(raw json.RawMessage) (blobstoreCredentials, error) {
	var c blobstoreCredentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("stemcell_fetch(blobstore): parse credentials: %w", err)
	}
	if c.Endpoint == "" {
		return c, fmt.Errorf("stemcell_fetch(blobstore): endpoint is required")
	}
	return c, nil
}

// Fetch opens a streaming GET to {endpoint}/{blobID} and applies Basic auth
// when credentials include a username or password. The caller must drain and
// close the returned io.ReadCloser.
//
// ContentLength mirrors resp.ContentLength: -1 when the server does not
// report a Content-Length.
//
// Failure modes:
//   - empty ref.BlobID → error (malformed bosh+blobstore: URL)
//   - creds is noCreds (no endpoint) → error
//   - incompatible Credentials kind → error
//   - parseBlobstoreAuth error → propagated
//   - http.NewRequestWithContext error → wrapped error
//   - creds.Apply error → wrapped error
//   - network / DNS error → wrapped error from http.Client.Do
//   - HTTP status outside 2xx → error with status + body preview (≤512 bytes)
func (b *blobstoreSource) Fetch(ctx context.Context, ref Reference, creds Credentials) (io.ReadCloser, int64, error) {
	if ref.BlobID == "" {
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): empty blob id (URL %q)", ref.URL)
	}

	var c blobstoreCredentials
	switch v := creds.(type) {
	case blobstoreCredentials:
		c = v
	case rawAuthCreds:
		var err error
		c, err = parseBlobstoreAuth(v.Raw)
		if err != nil {
			return nil, 0, err
		}
	case noCreds:
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): credentials required (endpoint missing)")
	default:
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): incompatible credentials kind %q", creds.Kind())
	}

	fetchURL := strings.TrimRight(c.Endpoint, "/") + "/" + ref.BlobID

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): build request: %w", err)
	}
	if err := c.Apply(req); err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): apply auth: %w", err)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(blobstore): GET %q: %w", fetchURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := readPreview(resp.Body)
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf(
			"stemcell_fetch(blobstore): GET %q returned HTTP %d: %s", fetchURL, resp.StatusCode, preview)
	}
	return resp.Body, resp.ContentLength, nil
}
