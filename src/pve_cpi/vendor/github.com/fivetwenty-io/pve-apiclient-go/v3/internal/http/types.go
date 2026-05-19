package http

import (
	"crypto/x509"
	"fmt"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/cache"
)

// SSLVerifyMode defines how SSL certificates should be verified.
type SSLVerifyMode int

const (
	SSLVerifyNone SSLVerifyMode = iota
	SSLVerifyPeer
	SSLVerifyHost
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
	AutoLogin    bool

	SSLOptions *SSLOptions

	Timeout   time.Duration
	KeepAlive int

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
