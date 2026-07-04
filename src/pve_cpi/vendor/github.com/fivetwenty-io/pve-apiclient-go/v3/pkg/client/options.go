package client

import (
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/cache"
	pvectx "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/context"
)

// ExecutionMode indicates where the client is running (local PVE node or remote).
type ExecutionMode = pvectx.ExecutionMode

// Execution mode constants.
const (
	ExecutionModeRemote  = pvectx.ExecutionModeRemote
	ExecutionModeLocal   = pvectx.ExecutionModeLocal
	ExecutionModeUnknown = pvectx.ExecutionModeUnknown
)

// CacheConfig holds cache configuration.
type CacheConfig = cache.Config

// CacheStats holds cache statistics.
type CacheStats = cache.CacheStats

var (
	ErrHostRequired                           = errors.New("host is required")
	ErrAuthenticationCredentialsRequired      = errors.New("authentication credentials required (username/password, API token, or ticket)")
	ErrPasswordRequiredForUsernameAuth        = errors.New("password required when using username authentication")
	ErrInvalidProtocol                        = errors.New("invalid protocol")
	ErrInvalidPort                            = errors.New("invalid port")
	ErrClientKeyRequiredWithClientCertificate = errors.New("client key required when client certificate is specified")
	ErrClientCertificateRequiredWithClientKey = errors.New("client certificate required when client key is specified")
)

// SSLVerifyMode defines how SSL certificates should be verified.
type SSLVerifyMode int

const (
	// Protocol constants.
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
)

// SSL verification modes. Kept in a dedicated const block so iota starts at 0,
// matching the numeric values used by the internal/http package. Combining
// with the protocol-string block above silently shifted iota by 2, mapping
// pkg/client.SSLVerifyNone (intended 0) to internalhttp.SSLVerifyHost (2)
// after the SSLVerifyMode() cast in pkg/client.NewClient — leaving
// InsecureSkipVerify=false even when callers asked for SSLVerifyNone.
const (
	// SSLVerifyNone disables SSL verification (insecure).
	SSLVerifyNone SSLVerifyMode = iota
	// SSLVerifyPeer verifies the peer certificate.
	SSLVerifyPeer
	// SSLVerifyHost verifies the hostname matches the certificate.
	SSLVerifyHost
	// SSLVerifyFull performs full SSL verification.
	SSLVerifyFull
)

// Options contains configuration options for the PVE client.
type Options struct {
	// Connection
	Host     string // Hostname or IP address of the PVE server
	Port     int    // Port number (default: 8006)
	Protocol string // Protocol to use: ProtocolHTTP or ProtocolHTTPS (default: ProtocolHTTPS)

	// Authentication
	Username     string // Username for authentication (e.g., "root@pam")
	Password     string // Password for authentication
	APIToken     string // API token for authentication (alternative to username/password)
	APITokenName string // Name prefix for API token in Authorization header (default: "PVEAPIToken")
	Ticket       string // Authentication ticket (obtained after login)
	CSRFToken    string // CSRF prevention token
	AutoLogin    bool   // Automatically login on first API call if username/password provided (default: false)

	// Execution Context
	ExecutionMode  ExecutionMode // Detected or manually set execution mode (local/remote/unknown)
	AutoDetectMode bool          // Automatically detect execution mode on client creation (default: true)

	// SSL/TLS
	SSLOptions                  *SSLOptions                  // SSL/TLS configuration
	CachedFingerprints          map[string]bool              // Cached certificate fingerprints
	ManualVerification          bool                         // Enable manual certificate verification
	RegisterFingerprintCallback func(string)                 // Callback for registering new fingerprints
	VerifyFingerprintCallback   func(*x509.Certificate) bool // Callback for verifying certificates
	// ManualVerifyCallback, when set, is consulted for a certificate whose
	// fingerprint is not already trusted, receiving the fingerprint,
	// certificate, and host. Return true to trust it for this client's
	// lifetime (and persist it when FingerprintCachePath is set). Opt-in;
	// nil (default) rejects unknown certificates when manual verification is
	// otherwise enabled.
	ManualVerifyCallback func(FingerprintVerificationRequest) bool
	// FingerprintCachePath, when non-empty, enables persistent Trust-On-First-Use
	// certificate pinning: fingerprints already trusted for Host/Port are
	// loaded from this file at construction, and any fingerprint accepted via
	// ManualVerifyCallback is written back to it. Opt-in; "" (default) keeps
	// fingerprint trust memory-only for the process lifetime.
	FingerprintCachePath string

	// HTTP Client
	Timeout   time.Duration // Request timeout (default: 30s)
	KeepAlive int           // Number of keep-alive connections (default: 10)

	// HTTP Transport tuning — all fields default to zero, which preserves the
	// existing transport behaviour (see internal/http createHTTPTransport).
	DialTimeoutSec         int // TCP dial timeout seconds; 0 = no explicit timeout (Go default)
	TLSHandshakeTimeoutSec int // TLS handshake timeout seconds; 0 = no explicit timeout (Go default)
	MaxIdleConnsPerHost    int // max idle conns per host; 0 = falls back to KeepAlive
	IdleConnTimeoutSec     int // idle connection timeout seconds; 0 = constants.LongTimeout()
	TCPKeepAliveSec        int // TCP keepalive probe interval seconds; 0 = Go default

	// Caching
	CacheConfig *CacheConfig // Cache configuration (nil = disabled)

	// Misc
	CookieName   string // Name of the authentication cookie (default: "PVEAuthCookie")
	PVENewFormat bool   // Use new PVE format for certain operations
}

// FingerprintVerificationRequest carries the details of a certificate whose
// fingerprint is not already trusted (via CachedFingerprints or
// FingerprintCachePath), passed to Options.ManualVerifyCallback so it can
// render an accept/reject decision (e.g. an interactive prompt or an
// operator-facing UI).
type FingerprintVerificationRequest struct {
	// Fingerprint is the normalized (colon-separated, uppercase) SHA256
	// fingerprint of the presented certificate.
	Fingerprint string
	// Certificate is the presented certificate; callers may inspect
	// Certificate.Subject, Issuer, NotBefore/NotAfter, etc.
	Certificate *x509.Certificate
	// Host is the server host the certificate was presented for.
	Host string
}

// SSLOptions contains SSL/TLS specific configuration.
type SSLOptions struct {
	VerifyHostname bool          // Verify that the hostname matches the certificate
	VerifyMode     SSLVerifyMode // SSL verification mode
	CACert         string        // Path to CA certificate file
	ClientCert     string        // Path to client certificate file
	ClientKey      string        // Path to client key file
}

// Validate checks if the options are valid.
func (o *Options) Validate() error {
	err := o.validateHost()
	if err != nil {
		return err
	}

	err = o.validateAuthentication()
	if err != nil {
		return err
	}

	err = o.validateProtocol()
	if err != nil {
		return err
	}

	err = o.validatePort()
	if err != nil {
		return err
	}

	return o.validateSSLOptions()
}

// GetBaseURL returns the base URL for API requests.
func (o *Options) GetBaseURL() string {
	return fmt.Sprintf("%s://%s:%d/api2/json", o.Protocol, o.Host, o.Port)
}

// IsUsingAPIToken returns true if API token authentication is being used.
func (o *Options) IsUsingAPIToken() bool {
	return o.APIToken != ""
}

// IsUsingTicket returns true if ticket authentication is being used.
func (o *Options) IsUsingTicket() bool {
	return o.Ticket != ""
}

// NeedsLogin returns true if login is required.
func (o *Options) NeedsLogin() bool {
	return !o.IsUsingAPIToken() && !o.IsUsingTicket() && o.Username != ""
}

func (o *Options) validateHost() error {
	if o.Host == "" {
		return ErrHostRequired
	}

	return nil
}

func (o *Options) validateAuthentication() error {
	if o.Username == "" && o.APIToken == "" && o.Ticket == "" {
		return ErrAuthenticationCredentialsRequired
	}

	if o.Username != "" && o.Password == "" && o.Ticket == "" {
		return ErrPasswordRequiredForUsernameAuth
	}

	return nil
}

func (o *Options) validateProtocol() error {
	if o.Protocol != "" && o.Protocol != ProtocolHTTP && o.Protocol != ProtocolHTTPS {
		return fmt.Errorf("%w: %s (must be 'http' or 'https')", ErrInvalidProtocol, o.Protocol)
	}

	return nil
}

func (o *Options) validatePort() error {
	if o.Port < 0 || o.Port > 65535 {
		return fmt.Errorf("%w: %d", ErrInvalidPort, o.Port)
	}

	return nil
}

func (o *Options) validateSSLOptions() error {
	if o.SSLOptions == nil {
		return nil
	}

	if o.SSLOptions.ClientCert != "" && o.SSLOptions.ClientKey == "" {
		return ErrClientKeyRequiredWithClientCertificate
	}

	if o.SSLOptions.ClientKey != "" && o.SSLOptions.ClientCert == "" {
		return ErrClientCertificateRequiredWithClientKey
	}

	return nil
}

func (o *Options) setDefaults() {
	if o.Protocol == "" {
		o.Protocol = ProtocolHTTPS
	}

	if o.Port == 0 {
		o.Port = constants.ProxmoxDefaultPort // PVE uses this port for both HTTP and HTTPS by default
	}

	if o.Timeout == 0 {
		o.Timeout = constants.DefaultClientTimeout()
	}

	if o.KeepAlive == 0 {
		o.KeepAlive = constants.DefaultMaxConcurrency
	}

	if o.CookieName == "" {
		o.CookieName = "PVEAuthCookie"
	}

	if o.CachedFingerprints == nil {
		o.CachedFingerprints = make(map[string]bool)
	}

	if o.SSLOptions == nil && o.Protocol == ProtocolHTTPS {
		o.SSLOptions = &SSLOptions{
			VerifyMode:     SSLVerifyPeer,
			VerifyHostname: true,
		}
	}

	// Auto-detect execution mode if not explicitly set
	// Note: AutoDetectMode defaults to false (opt-in)
	if o.AutoDetectMode && o.ExecutionMode == 0 {
		detector := pvectx.NewDetector()
		o.ExecutionMode = detector.DetectMode()
	}
}
