package stemcellfetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpsSource implements Source for https:// references. It streams the
// remote body via an http.Client with redirect handling and applies the
// caller-provided Credentials (basicCreds or bearerCreds).
//
// Timeout default: 30 minutes per fetch (stemcells are large; production
// network paths to artifactory mirrors may be slow). Overridable for tests
// via httpsSource{client: customHTTPClient}.
type httpsSource struct {
	client *http.Client
}

// newHTTPSSource returns a Source whose http.Client uses a 30-minute total
// timeout, a TLS 1.2 floor, the supplied TransportConfig bounding dial/TLS/
// response-header/idle timeouts, and a redirect policy that requires every
// redirect target to use https:// (preventing accidental scheme downgrade and
// SSRF-adjacent redirects to internal endpoints). Up to 10 redirects are still
// permitted, matching Go's default cap.
func newHTTPSSource(tc TransportConfig) *httpsSource {
	return &httpsSource{
		client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: tc.applyTransport(&http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			}),
			CheckRedirect: httpsOnlyRedirect,
		},
	}
}

// httpsOnlyRedirect rejects any redirect whose target is not https://.
// Mirrors Go's default 10-redirect cap.
func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stemcell_fetch(https): stopped after 10 redirects")
	}
	if req.URL.Scheme != schemeHTTPS {
		return fmt.Errorf("stemcell_fetch(https): refusing redirect to non-https URL %q", req.URL.String())
	}
	return nil
}

// Fetch opens a streaming GET to ref.URL, applies creds, and returns the
// response body. The caller must drain and close the returned io.ReadCloser.
// ContentLength mirrors resp.ContentLength: -1 when the server does not
// report a Content-Length.
//
// Failure modes:
//   - empty ref.URL → error
//   - non-https scheme in ref.URL → error (prevents accidental plaintext use)
//   - URL parse failure → error
//   - creds.Apply error → error (bad auth config; request not sent)
//   - network / DNS error → wrapped error from http.Client.Do
//   - HTTP status outside 2xx → error with status + body preview (≤512 bytes)
func (h *httpsSource) Fetch(ctx context.Context, ref Reference, creds Credentials) (io.ReadCloser, int64, error) {
	if ref.URL == "" {
		return nil, 0, fmt.Errorf("stemcell_fetch(https): empty URL")
	}

	parsed, err := url.Parse(ref.URL)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(https): parse URL %q: %w", ref.URL, err)
	}
	if parsed.Scheme != schemeHTTPS {
		return nil, 0, fmt.Errorf("stemcell_fetch(https): expected scheme https, got %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(https): build request: %w", err)
	}

	if creds != nil {
		if err := creds.Apply(req); err != nil {
			return nil, 0, fmt.Errorf("stemcell_fetch(https): apply credentials (%s): %w", creds.Kind(), err)
		}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(https): GET %q: %w", ref.URL, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a brief preview of the body for the error message, then close.
		bodyPreview := readPreview(resp.Body)
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf(
			"stemcell_fetch(https): GET %q returned HTTP %d: %s",
			ref.URL, resp.StatusCode, bodyPreview)
	}

	return resp.Body, resp.ContentLength, nil
}

// readPreview reads up to 512 bytes from r without consuming the full body.
// Used to enrich error messages with the server's response prefix.
func readPreview(r io.Reader) string {
	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	if n == 0 {
		return "(no body)"
	}
	return strings.TrimSpace(string(buf[:n]))
}
