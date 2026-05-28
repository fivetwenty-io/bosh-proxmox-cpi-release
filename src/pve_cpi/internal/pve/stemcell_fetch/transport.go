package stemcellfetch

import (
	"net"
	"net/http"
	"time"
)

// TransportConfig bounds the network steps of every stemcell-fetch HTTP request
// that goes through this package's https and bosh+blobstore sources. All four
// fields default to the values returned by DefaultTransportConfig when zero.
//
// Operators tune these via CPI config (jobs/pve_cpi/spec). The s3 and oci
// sources construct their own HTTP clients via aws-sdk-go-v2 and
// go-containerregistry respectively, so TransportConfig does not flow into
// them; tune those via their own SDK options if needed.
type TransportConfig struct {
	// DialTimeout bounds the TCP connect step.
	DialTimeout time.Duration

	// TLSHandshakeTimeout bounds the TLS handshake step.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout bounds the wait for the response headers after
	// the request is sent. Guards against slow-loris drips on the response-
	// header phase. The request body transfer is bounded by the outer
	// http.Client.Timeout (30 minutes), not by this field.
	ResponseHeaderTimeout time.Duration

	// IdleConnTimeout bounds how long an idle keep-alive connection lives in
	// the connection pool before being closed.
	IdleConnTimeout time.Duration
}

// DefaultTransportConfig returns the production defaults: 30s dial, 15s TLS
// handshake, 2m response header, 90s idle connection. These mirror the
// registry client's posture and are appropriate for stemcell mirrors reachable
// over normal network paths.
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		DialTimeout:           30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}
}

// WithDefaults returns tc with any zero field replaced by the corresponding
// DefaultTransportConfig value. Callers can use this to normalize partial
// configurations before passing them to source constructors.
func (tc TransportConfig) WithDefaults() TransportConfig {
	def := DefaultTransportConfig()
	if tc.DialTimeout <= 0 {
		tc.DialTimeout = def.DialTimeout
	}
	if tc.TLSHandshakeTimeout <= 0 {
		tc.TLSHandshakeTimeout = def.TLSHandshakeTimeout
	}
	if tc.ResponseHeaderTimeout <= 0 {
		tc.ResponseHeaderTimeout = def.ResponseHeaderTimeout
	}
	if tc.IdleConnTimeout <= 0 {
		tc.IdleConnTimeout = def.IdleConnTimeout
	}
	return tc
}

// applyTransport returns a *http.Transport carrying the bounded timeouts plus
// the caller-supplied per-source extras (e.g. TLS config). A net.Dialer with
// the DialTimeout is wired so dial-step stalls fail fast.
func (tc TransportConfig) applyTransport(base *http.Transport) *http.Transport {
	tc = tc.WithDefaults()
	dialer := &net.Dialer{Timeout: tc.DialTimeout}
	base.DialContext = dialer.DialContext
	base.TLSHandshakeTimeout = tc.TLSHandshakeTimeout
	base.ResponseHeaderTimeout = tc.ResponseHeaderTimeout
	base.IdleConnTimeout = tc.IdleConnTimeout
	return base
}
