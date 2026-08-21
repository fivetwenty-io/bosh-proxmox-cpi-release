package http

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	issl "github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/ssl"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/cache"
)

// Package-level string constants used across multiple files in this package.
const (
	// Content-type strings.
	contentTypeJSON           = "application/json"
	contentTypeFormURLEncoded = "application/x-www-form-urlencoded"
	contentTypeTextPlain      = "text/plain"

	// Protocol string.
	protoHTTPS = "https"

	// Log field keys.
	logFieldMethod = "method"
	logFieldURL    = "url"
	logFieldError  = "error"

	// Redaction sentinel.
	redactedValue = "REDACTED"

	// Redact param keys used in defaultLogConfig.
	redactParamPassword = "password"
)

// SSLVerifyMode selects whether server certificates are verified.
//
// Only two effective behaviors exist: SSLVerifyNone disables verification
// (InsecureSkipVerify), and every other mode performs Go's full standard-library
// verification — chain validation AND hostname matching. SSLVerifyPeer,
// SSLVerifyHost, and SSLVerifyFull are therefore equivalent today; they all
// deliver full verification and differ only in name. The finer-grained modes
// are retained for API compatibility but do not relax verification: there is no
// "verify chain but skip hostname" path, by design (it would be the weaker,
// surprising option). For certificate-pinning instead of CA verification, use
// the fingerprint options (CachedFingerprints / VerifyFingerprintCallback).
type SSLVerifyMode int

const (
	// SSLVerifyNone disables certificate verification (InsecureSkipVerify).
	SSLVerifyNone SSLVerifyMode = iota
	// SSLVerifyPeer performs full chain + hostname verification.
	SSLVerifyPeer
	// SSLVerifyHost performs full chain + hostname verification (same as Peer/Full).
	SSLVerifyHost
	// SSLVerifyFull performs full chain + hostname verification.
	SSLVerifyFull
)

// SSLOptions contains SSL/TLS specific configuration.
type SSLOptions struct {
	VerifyHostname bool
	VerifyMode     SSLVerifyMode
	CACert         string
	ClientCert     string
	ClientKey      string
}

// Options contains configuration options for the HTTP client.
type Options struct {
	Host     string
	Port     int
	Protocol string

	Username     string
	Password     string
	APIToken     string
	APITokenName string
	Ticket       string
	CSRFToken    string
	AutoLogin    bool

	SSLOptions *SSLOptions

	Timeout   time.Duration
	KeepAlive int

	// Transport tuning — all fields default to zero, which preserves the existing
	// transport behaviour (see createHTTPTransport for the zero-knob contract).
	DialTimeoutSec         int // TCP dial timeout seconds; 0 = no explicit timeout (Go default)
	TLSHandshakeTimeoutSec int // TLS handshake timeout seconds; 0 = no explicit timeout (Go default)
	MaxIdleConnsPerHost    int // max idle conns per host; 0 = falls back to KeepAlive
	IdleConnTimeoutSec     int // idle connection timeout seconds; 0 = constants.LongTimeout()
	TCPKeepAliveSec        int // TCP keepalive probe interval seconds; 0 = Go default

	// DialContext, when non-nil, establishes every connection the transport
	// makes, replacing the TCP dial entirely. It takes precedence over
	// DialTimeoutSec and TCPKeepAliveSec, which configure a dialer this field
	// replaces; a caller supplying its own dial function owns its own
	// timeouts. Opt-in; nil (default) keeps the transport's existing dialing.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// Proxy, when non-nil, selects the proxy for a request, exactly as
	// http.Transport.Proxy does. Opt-in; nil (default) sends every request
	// direct, which is this transport's long-standing behaviour and the
	// reason HTTP_PROXY/HTTPS_PROXY have no effect on it. Pass
	// http.ProxyFromEnvironment to honour those variables.
	Proxy func(*http.Request) (*url.URL, error)

	// Caching configuration
	CacheConfig *cache.Config

	// Extra options for auth/TLS parity with perl client
	CookieName   string
	PVENewFormat bool
	// TLS fingerprinting & verification
	CachedFingerprints          map[string]bool
	ManualVerification          bool
	RegisterFingerprintCallback func(string)
	VerifyFingerprintCallback   func(*x509.Certificate) bool
	// ManualVerifyCallback is consulted for a certificate whose fingerprint is
	// not already trusted (via CachedFingerprints or FingerprintCachePath),
	// receiving the fingerprint, certificate, and host; return true to trust
	// it. Opt-in; nil (default) rejects unknown certificates when manual
	// verification is otherwise enabled.
	ManualVerifyCallback func(issl.ManualVerificationRequest) bool
	// FingerprintCachePath, when non-empty, enables persistent Trust-On-First-Use
	// certificate pinning: fingerprints already trusted for Host/Port are loaded
	// from this file at construction, and any fingerprint accepted via
	// ManualVerifyCallback is written back to it. Opt-in; "" (default) keeps
	// fingerprint trust memory-only for the process lifetime.
	FingerprintCachePath string
}

// BaseURL returns the base URL for API requests.
func (o *Options) BaseURL() string {
	return fmt.Sprintf("%s://%s:%d/api2/json", o.Protocol, o.Host, o.Port)
}

// Response represents a parsed API response.
type Response struct {
	Data   interface{}
	Errors map[string]string
	Code   int
}
