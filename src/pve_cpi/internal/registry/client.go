// Package registry provides an HTTP client for the BOSH registry service.
// The registry stores agent settings keyed by instance ID and exposes a
// simple REST API used by CPIs to configure BOSH agents on new VMs.
package registry

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// settingsEnvelope is the JSON envelope used for PUT bodies and GET responses.
// The BOSH registry wraps the agent settings JSON as a string value, not nested JSON.
type settingsEnvelope struct {
	Settings string `json:"settings"`
}

// Client is an HTTP client for the BOSH registry service.
// Instances are safe for concurrent use after construction.
type Client struct {
	endpoint       string
	user           string
	pass           string
	http           *http.Client
	configuredHost string   // host extracted from endpoint at construction; invariant-checked before every request
	allowedHosts   []string // optional host allow-list; empty disables filtering
}

// defaultClientTimeout is the per-attempt timeout applied by NewClient when
// the caller does not override it via Options.Timeout.
const defaultClientTimeout = 30 * time.Second

// Options carries optional knobs for NewClientWithOptions. The zero value is
// valid and selects safe defaults: TLS 1.2 floor, system trust pool, 30s
// per-attempt timeout.
type Options struct {
	// CACertPEM is an optional PEM-encoded CA certificate (or chain) appended
	// to the system trust pool when verifying the registry's TLS certificate.
	// Empty means use the system pool unmodified.
	CACertPEM string

	// Timeout overrides the per-attempt http.Client.Timeout. Zero or negative
	// values fall back to defaultClientTimeout.
	Timeout time.Duration

	// AllowedHosts is an optional list of host patterns that restrict which
	// hosts the client may contact. Each entry is an exact host or a wildcard
	// prefix ("*.example.com"). Empty (default) disables host-allow-list
	// filtering; the configuredHost invariant and disabled redirects still
	// apply regardless. Sourced from config.RegistryAllowedHosts.
	AllowedHosts []string
}

// NewClient constructs a Client for the registry at endpoint with default
// transport options (TLS 1.2 floor, system trust pool, 30-second per-attempt
// timeout). Trailing slashes on endpoint are trimmed.
//
// This is a thin wrapper over NewClientWithOptions retained for backward
// compatibility with existing call sites.
func NewClient(endpoint, user, pass string) *Client {
	c, _ := NewClientWithOptions(endpoint, user, pass, Options{})
	return c
}

// NewClientWithOptions constructs a Client with explicit transport options.
//
// Security posture:
//   - TLS 1.2 floor is pinned on the underlying transport for every request.
//   - HTTP redirects are disabled: CheckRedirect returns an error so any 3xx
//     response surfaces immediately as a CloudError instead of being silently
//     followed to a potentially attacker-controlled host. BOSH registries do
//     not issue redirects; disabling them is safe and prevents SSRF via redirect.
//   - The configuredHost field records the endpoint host at construction time.
//     doWithRetry enforces an invariant that req.URL.Host equals configuredHost
//     before every http.Do call, catching accidental URL mutation bugs.
//   - When opts.AllowedHosts is non-empty, doWithRetry additionally verifies
//     req.URL.Host against the allow-list (exact match or "*.example.com" wildcard).
//
// Returns an error only when opts.CACertPEM is non-empty and cannot be parsed
// (either x509.SystemCertPool fails or no certificates are decoded from PEM).
// Callers may safely ignore the error when they pass an empty CACertPEM.
func NewClientWithOptions(endpoint, user, pass string, opts Options) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.CACertPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			// On platforms without a system pool (or when retrieval fails) start
			// from an empty pool so the caller-supplied CA is still honored.
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(opts.CACertPEM)) {
			return nil, cpierrors.Cloud("registry: NewClientWithOptions: no PEM certificates parsed from CACertPEM")
		}
		tlsCfg.RootCAs = pool
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultClientTimeout
	}

	// Extract the configured host from the endpoint URL so we can enforce the
	// configuredHost invariant in doWithRetry. Parse errors here are non-fatal
	// because the config validator has already accepted the endpoint; we fall
	// back to the raw endpoint string as a best-effort host value.
	trimmedEndpoint := strings.TrimRight(endpoint, "/")
	configuredHost := trimmedEndpoint
	if u, parseErr := url.Parse(trimmedEndpoint); parseErr == nil && u.Host != "" {
		configuredHost = u.Host
	}

	// Documented: TLS 1.2 floor pinned for every registry request; CACertPEM (when
	// provided) augments the system trust pool, never replaces it.
	transport := &http.Transport{TLSClientConfig: tlsCfg}

	// Copy AllowedHosts to avoid retaining a reference to the caller's slice.
	var allowedHosts []string
	if len(opts.AllowedHosts) > 0 {
		allowedHosts = make([]string, len(opts.AllowedHosts))
		copy(allowedHosts, opts.AllowedHosts)
	}

	return &Client{
		endpoint:       trimmedEndpoint,
		user:           user,
		pass:           pass,
		configuredHost: configuredHost,
		allowedHosts:   allowedHosts,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			// Disable automatic redirect following. BOSH registries do not redirect;
			// any 3xx response is unexpected and potentially dangerous (SSRF via
			// redirect to a non-configured host). Returning an error here causes
			// http.Client.Do to return the error, which doWithRetry treats as a
			// non-retriable terminal failure — the caller receives a CloudError.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return fmt.Errorf("registry: redirects disabled for security (SSRF prevention)")
			},
		},
	}, nil
}

// retryMaxAttempts is the total number of attempts (1 initial + 2 retries).
const retryMaxAttempts = 3

// retryBaseDelay is the base backoff interval before the first retry.
//
// Declared as a var (not const) so tests can swap the value via t.Cleanup
// to drive retry loops without sleeping 200–400 ms per retry in CI.
var retryBaseDelay = 200 * time.Millisecond

// maxRegistryRespBody caps the bytes read from any registry response body.
// The BOSH registry settings document is well under 64 KiB in production; a
// 1 MiB ceiling absorbs reasonable growth while preventing a malicious or
// runaway endpoint from forcing the CPI to allocate unbounded memory while
// reading a response. Applied uniformly to error-message bodies and to
// the settings envelope itself.
const maxRegistryRespBody = 1 << 20

// isRetriable reports whether the HTTP response or transport error warrants a retry.
//
// Retried:
//   - net.Error with Timeout() == true (ETIMEDOUT, transport-layer deadline)
//   - io.ErrUnexpectedEOF (server closed the connection mid-response)
//   - syscall.ECONNRESET (connection reset by peer), including wrapped variants
//   - HTTP 5xx status codes
//   - HTTP 408 (Request Timeout)
//
// Not retried:
//   - context.Canceled or context.DeadlineExceeded (caller cancelled)
//   - HTTP 4xx except 408
//   - HTTP 2xx (success)
func isRetriable(resp *http.Response, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return true
		}
		if errors.Is(err, syscall.ECONNRESET) {
			return true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return true
		}
		// Catch ECONNRESET wrapped inside *net.OpError.
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			if errors.Is(opErr.Err, syscall.ECONNRESET) {
				return true
			}
		}
		return false
	}
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusRequestTimeout { // 408
		return true
	}
	return resp.StatusCode >= 500
}

// backoffDelay returns the jittered exponential delay for attempt index i
// (0-based: i=0 is the delay before the first retry).
// Formula: base * 2^i * jitter, where jitter is uniform in [0.75, 1.25).
func backoffDelay(i int) time.Duration {
	base := retryBaseDelay
	for j := 0; j < i; j++ {
		base *= 2
	}
	jitter := 0.75 + rand.Float64()*0.5 // #nosec G404 -- jitter; non-cryptographic
	return time.Duration(float64(base) * jitter)
}

// hostMatchesAllowList reports whether host matches at least one entry in patterns.
// Each pattern is either an exact host ("registry.example.com") or a wildcard
// prefix pattern ("*.example.com"). Wildcard patterns match any single-level
// subdomain of the suffix: "*.example.com" matches "foo.example.com" but not
// "foo.bar.example.com" or "example.com". The comparison is case-insensitive.
//
// The host argument may include a port (e.g. "registry.example.com:443"); the
// port is stripped before comparison so patterns need only carry the hostname.
func hostMatchesAllowList(host string, patterns []string) bool {
	// Strip port if present.
	bare := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		bare = h
	}
	bare = strings.ToLower(bare)
	for _, p := range patterns {
		// Strip port from pattern for a forgiving comparison.
		pBare := p
		if ph, _, err := net.SplitHostPort(p); err == nil {
			pBare = ph
		}
		pBare = strings.ToLower(pBare)
		if strings.HasPrefix(pBare, "*.") {
			// Wildcard: "*.example.com" matches exactly one label prefix.
			suffix := pBare[1:] // ".example.com"
			if strings.HasSuffix(bare, suffix) {
				// Ensure the matched prefix is exactly one label (no nested dot).
				prefix := bare[:len(bare)-len(suffix)]
				if prefix != "" && !strings.Contains(prefix, ".") {
					return true
				}
			}
		} else if bare == pBare {
			return true
		}
	}
	return false
}

// doWithRetry executes req with up to retryMaxAttempts total attempts.
// Between attempts it waits for a jittered exponential backoff, or returns
// immediately if ctx is already cancelled.
//
// The 30-second http.Client.Timeout applies per attempt, not to the total.
// The request body is re-wound via req.GetBody between retries; callers must
// ensure req.GetBody is set when the method has a body (Put sets it automatically
// using bytes.NewReader; Get and Delete have no body).
func (c *Client) doWithRetry(req *http.Request) (*http.Response, error) {
	// drainAndClose discards then closes the prior response body so the
	// connection is returned to the keep-alive pool and no fd is leaked.
	// Bounded by maxRegistryRespBody to cap the discard cost in case a
	// server hands back a large error page.
	drainAndClose := func(r *http.Response) {
		if r == nil || r.Body == nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxRegistryRespBody))
		_ = r.Body.Close() // #nosec G104 -- close on drained body; nothing actionable
	}

	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			// Re-wind the request body for the retry attempt. If GetBody
			// fails the previous response body (if any) must still be
			// drained+closed before returning so the connection is released.
			if req.GetBody != nil {
				body, gberr := req.GetBody()
				if gberr != nil {
					drainAndClose(resp)
					return nil, gberr
				}
				req.Body = body
			}
			// Wait for backoff interval or context cancellation. Drain+close
			// the prior response before returning on context cancellation so
			// the body is not abandoned mid-stream.
			delay := backoffDelay(attempt - 1)
			select {
			case <-req.Context().Done():
				drainAndClose(resp)
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
		}

		// Drain and close the previous failed response body before retrying.
		if resp != nil {
			drainAndClose(resp)
		}

		// configuredHost invariant: verify req.URL.Host equals the host from the
		// endpoint supplied at construction time. The registry client only builds
		// URLs from c.endpoint, so a mismatch here indicates a bug (URL mutation)
		// rather than a legitimate multi-host scenario. Always-on; no config gate.
		if req.URL.Host != c.configuredHost {
			drainAndClose(resp)
			return nil, cpierrors.Cloud(
				"registry: request host %q does not match configured host %q (invariant violation)",
				req.URL.Host, c.configuredHost,
			)
		}
		// registry_allowed_hosts filter: when non-empty, verify req.URL.Host
		// matches at least one pattern (exact or "*.example.com" wildcard).
		if len(c.allowedHosts) > 0 && !hostMatchesAllowList(req.URL.Host, c.allowedHosts) {
			drainAndClose(resp)
			return nil, cpierrors.Cloud(
				"registry: request host %q is not in registry_allowed_hosts allow-list",
				req.URL.Host,
			)
		}
		// URL host enforced via configuredHost invariant + CheckRedirect rejects redirects; registry_allowed_hosts may further constrain.
		resp, err = c.http.Do(req) // #nosec G704 -- SSRF via taint analysis: req.URL is operator-configured and host-gated by configuredHost + AllowedHosts.
		if !isRetriable(resp, err) {
			// Terminal: when both resp and err are non-nil (rare but possible
			// with some net.http edge cases) close the body now since the
			// caller receives the err and will not see resp.
			if err != nil && resp != nil {
				drainAndClose(resp)
				return nil, err
			}
			return resp, err
		}
	}
	// Loop exhausted. As above, if both resp and err are non-nil, drain+close
	// since callers treating err as terminal will not consume resp.
	if err != nil && resp != nil {
		drainAndClose(resp)
		return nil, err
	}
	return resp, err
}

// Put serialises settings to JSON, wraps it in the registry envelope, and
// sends a PUT to /instances/{instanceID}/settings. Non-2xx responses are
// returned as a CloudError containing the HTTP status and response body.
// Transient failures (5xx, network timeout, ECONNRESET) are retried up to
// retryMaxAttempts times with jittered exponential backoff.
func (c *Client) Put(ctx context.Context, instanceID string, settings any) error {
	if instanceID == "" {
		return cpierrors.Cloud("registry: Put: instanceID must not be empty")
	}

	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return cpierrors.Cloud("registry: Put: marshal settings: %s", err.Error())
	}

	env := settingsEnvelope{Settings: string(settingsJSON)}
	body, err := json.Marshal(env)
	if err != nil {
		return cpierrors.Cloud("registry: Put: marshal envelope: %s", err.Error())
	}

	reqURL := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	bodyReader := bytes.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bodyReader)
	if err != nil {
		return cpierrors.Cloud("registry: Put: build request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)
	// GetBody allows doWithRetry to rewind the body between attempts.
	req.GetBody = func() (io.ReadCloser, error) {
		if _, seekErr := bodyReader.Seek(0, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}
		return io.NopCloser(bodyReader), nil
	}

	resp, err := c.doWithRetry(req)
	if err != nil {
		return cpierrors.Cloud("registry: Put: %s", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRegistryRespBody))
		return cpierrors.Cloud(
			"registry: Put %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}
	return nil
}

// Get fetches /instances/{instanceID}/settings and returns the raw JSON of
// the settings value (the string stored inside the envelope, re-parsed as
// json.RawMessage so callers can Unmarshal into concrete types).
// A 404 response returns a CloudError; callers may test with cpierrors.IsType.
// Transient failures are retried up to retryMaxAttempts times.
func (c *Client) Get(ctx context.Context, instanceID string) (json.RawMessage, error) {
	if instanceID == "" {
		return nil, cpierrors.Cloud("registry: Get: instanceID must not be empty")
	}

	reqURL := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get: build request: %s", err.Error())
	}
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.doWithRetry(req)
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get: %s", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, cpierrors.Cloud("registry: Get %s: not found (404)", instanceID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRegistryRespBody))
		return nil, cpierrors.Cloud(
			"registry: Get %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryRespBody))
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get %s: read body: %s", instanceID, err.Error())
	}

	var env settingsEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, cpierrors.Cloud("registry: Get %s: unmarshal envelope: %s", instanceID, err.Error())
	}

	// env.Settings is the JSON-encoded string; parse it so callers get raw JSON.
	return json.RawMessage(env.Settings), nil
}

// Delete sends DELETE /instances/{instanceID}/settings.
// A 404 response is treated as success (idempotent). Non-2xx, non-404
// responses are returned as a CloudError. Transient failures are retried
// up to retryMaxAttempts times with jittered exponential backoff.
func (c *Client) Delete(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return cpierrors.Cloud("registry: Delete: instanceID must not be empty")
	}

	reqURL := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, http.NoBody)
	if err != nil {
		return cpierrors.Cloud("registry: Delete: build request: %s", err.Error())
	}
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.doWithRetry(req)
	if err != nil {
		return cpierrors.Cloud("registry: Delete: %s", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 is idempotent — the record is already gone.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRegistryRespBody))
		return cpierrors.Cloud(
			"registry: Delete %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}
	return nil
}
