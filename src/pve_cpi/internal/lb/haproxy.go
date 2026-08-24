package lb

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/netguard"
)

// defaultTimeout is the per-request HTTP timeout when HAProxyConfig.Timeout is
// zero or negative.
const defaultTimeout = 30 * time.Second

// maxRespBody caps bytes read from any DPA response to prevent runaway
// allocation on malformed or hostile responses.
const maxRespBody = 1 << 20 // 1 MiB

// HAProxyConfig carries constructor options for HAProxyRegistrar.
type HAProxyConfig struct {
	// Endpoint is the base URL of the HAProxy Data Plane API, e.g.
	// https://haproxy.example.com:5555.
	Endpoint string

	// User is the HTTP Basic Auth username for the DPA.
	User string

	// Password is the HTTP Basic Auth password for the DPA.
	Password string

	// CACertPEM is an optional PEM-encoded CA certificate (or chain) appended
	// to the system trust pool when verifying the DPA's TLS certificate.
	// Empty means use the system trust pool unmodified.
	CACertPEM string

	// AllowPrivateIP disables the private/loopback IP rejection guard when
	// true. Default false (guard active): every TCP dial resolves the target
	// address and rejects any IP that is private, loopback, link-local, or
	// unspecified. Set true only for lab/test deployments.
	AllowPrivateIP bool

	// Timeout overrides the per-request http.Client.Timeout. Zero or negative
	// values fall back to defaultTimeout.
	Timeout time.Duration

	// InsecureSkipVerify disables TLS certificate verification. Must only be
	// used for lab/test deployments; never in production.
	InsecureSkipVerify bool

	// resolver is an optional DNS resolver injected by tests to control
	// hostname resolution without real network calls. Production callers
	// leave nil; net.DefaultResolver is used.
	resolver interface {
		LookupHost(ctx context.Context, host string) ([]string, error)
	}
}

// HAProxyRegistrar implements LBRegistrar against the HAProxy Data Plane API
// v3 runtime endpoint. Runtime registration does not require an HAProxy reload.
//
// Safe for concurrent use after construction.
type HAProxyRegistrar struct {
	endpoint string
	user     string
	password string
	http     *http.Client
}

// isPrivateOrSpecial returns true when ip is a non-globally-routable address.
// Delegates to the shared netguard classification so every SSRF-guarded
// client in the CPI (LB registrar, stemcell fetcher) rejects the same set.
func isPrivateOrSpecial(ip net.IP) bool {
	return netguard.IsPrivateOrSpecial(ip)
}

// resolverFunc is the interface used to inject a test DNS resolver.
type resolverFunc interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// checkEndpointIPsLiteral checks only IP-literal endpoints at construct time.
// Hostname endpoints are no longer checked at construction; the dial-time guard
// (ssrfDialContext) is the authoritative per-connection check.
func checkEndpointIPsLiteral(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname: guard enforced at dial time. No construct-time DNS lookup
		// (eliminates fail-open DNS-failure path and TOCTOU window).
		return nil
	}
	if isPrivateOrSpecial(ip) {
		return cpierrors.Cloud(
			"lb: endpoint resolves to private/loopback IP %s; set allow_private_ip=true to override",
			ip.String(),
		)
	}
	return nil
}

// ssrfDialContext returns a DialContext function that resolves the target host
// on every connection attempt and rejects any private/loopback/link-local/
// unspecified IP before dialing. This closes the DNS-rebinding (TOCTOU) window:
// even if a hostname flips from public to private between construct time and
// request time, the guard fires per-connection.
//
// When res is non-nil it is used for resolution (test seam); otherwise
// net.DefaultResolver is used.
func ssrfDialContext(base *net.Dialer, res resolverFunc) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return netguard.DialGuard{
		Base:      base,
		Resolver:  res,
		ErrPrefix: "lb",
		Hint:      "set allow_private_ip=true to override",
	}.DialContext()
}

// NewHAProxyRegistrar constructs an HAProxyRegistrar.
//
// Returns an error when:
//   - cfg.CACertPEM is non-empty and contains no parseable PEM certificates.
//   - cfg.AllowPrivateIP is false and the endpoint is an IP literal that is
//     private/loopback/link-local/unspecified (fast-fail; hostname endpoints
//     are enforced per-connection by the dial-time guard).
func NewHAProxyRegistrar(cfg HAProxyConfig) (*HAProxyRegistrar, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, // #nosec G402 -- operator opt-in for lab
	}

	if cfg.CACertPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.CACertPEM)) {
			return nil, cpierrors.Cloud("lb: NewHAProxyRegistrar: no PEM certificates parsed from CACertPEM")
		}
		tlsCfg.RootCAs = pool
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	trimmed := strings.TrimRight(cfg.Endpoint, "/")

	// IP-literal fast check: catch obvious private-IP literals immediately.
	if !cfg.AllowPrivateIP {
		if err := checkEndpointIPsLiteral(trimmed); err != nil {
			return nil, err
		}
	}

	// Build transport. When AllowPrivateIP is false, wire the SSRF dial guard
	// so every connection re-resolves and rejects private IPs. This is the
	// authoritative per-connection guard that closes the DNS-rebinding window.
	baseDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	if !cfg.AllowPrivateIP {
		transport.DialContext = ssrfDialContext(baseDialer, cfg.resolver)
	}

	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// Redirect disabled: HAProxy DPA does not redirect; any 3xx is unexpected
		// and potentially dangerous (SSRF). Returning an error here causes
		// http.Client.Do to surface it immediately as a hard failure.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("lb: redirects disabled for security (SSRF prevention)")
		},
	}

	return &HAProxyRegistrar{
		endpoint: trimmed,
		user:     cfg.User,
		password: cfg.Password,
		http:     httpClient,
	}, nil
}

// addServerRequest is the JSON body sent to the DPA runtime add-server endpoint.
type addServerRequest struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// buildURL constructs a request URL from the registrar's endpoint base and the
// given path segments. Each segment is individually URL-path-escaped so that
// segments containing spaces or special characters produce a correctly encoded
// request URL. The escaped path is set on both Path and RawPath so that
// http.Request.URL preserves the encoding through the Go HTTP client.
func (r *HAProxyRegistrar) buildURL(segments ...string) (*url.URL, error) {
	base, err := url.Parse(r.endpoint)
	if err != nil {
		return nil, fmt.Errorf("lb: parse endpoint %q: %w", r.endpoint, err)
	}
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	rawPath := strings.Join(escaped, "/")
	u := &url.URL{
		Scheme:  base.Scheme,
		Host:    base.Host,
		Path:    "/" + strings.Join(segments, "/"),
		RawPath: "/" + rawPath,
	}
	return u, nil
}

// Register adds s as a server in the named HAProxy backend via the DPA runtime
// API. Idempotent: HTTP 409 (server already exists) is treated as success.
//
// DPA endpoint: POST {Endpoint}/v3/services/haproxy/runtime/backends/{backend}/servers
func (r *HAProxyRegistrar) Register(ctx context.Context, backend string, s Server) error {
	body, err := json.Marshal(addServerRequest(s))
	if err != nil {
		return cpierrors.Cloud("lb: Register: marshal request: %s", err.Error())
	}

	u, err := r.buildURL("v3", "services", "haproxy", "runtime", "backends", backend, "servers")
	if err != nil {
		return cpierrors.Cloud("lb: Register: build URL: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return cpierrors.Cloud("lb: Register: build request: %s", err.Error())
	}
	req.URL = u // preserve RawPath
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.user, r.password)

	resp, err := r.http.Do(req)
	if err != nil {
		return cpierrors.Cloud("lb: Register: HTTP POST %s: %s", u.String(), err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusConflict:
		return nil
	default:
		return cpierrors.Cloud(
			"lb: Register: unexpected status %d from DPA (backend=%q server=%q)",
			resp.StatusCode, backend, s.Name,
		)
	}
}

// Deregister removes the server identified by serverName from the named HAProxy
// backend via the DPA runtime API. Idempotent: HTTP 404 (server not found) is
// treated as success.
//
// DPA endpoint: DELETE {Endpoint}/v3/services/haproxy/runtime/backends/{backend}/servers/{serverName}
func (r *HAProxyRegistrar) Deregister(ctx context.Context, backend, serverName string) error {
	u, err := r.buildURL("v3", "services", "haproxy", "runtime", "backends", backend, "servers", serverName)
	if err != nil {
		return cpierrors.Cloud("lb: Deregister: build URL: %s", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), http.NoBody)
	if err != nil {
		return cpierrors.Cloud("lb: Deregister: build request: %s", err.Error())
	}
	req.URL = u // preserve RawPath
	req.SetBasicAuth(r.user, r.password)

	resp, err := r.http.Do(req)
	if err != nil {
		return cpierrors.Cloud("lb: Deregister: HTTP DELETE %s: %s", u.String(), err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return cpierrors.Cloud(
			"lb: Deregister: unexpected status %d from DPA (backend=%q server=%q)",
			resp.StatusCode, backend, serverName,
		)
	}
}
