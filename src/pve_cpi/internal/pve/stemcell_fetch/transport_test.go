package stemcellfetch

import (
	"net/http"
	"testing"
	"time"
)

func TestDefaultTransportConfig(t *testing.T) {
	t.Parallel()
	tc := DefaultTransportConfig()
	if tc.DialTimeout != 30*time.Second {
		t.Errorf("DialTimeout = %v, want 30s", tc.DialTimeout)
	}
	if tc.TLSHandshakeTimeout != 15*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 15s", tc.TLSHandshakeTimeout)
	}
	if tc.ResponseHeaderTimeout != 2*time.Minute {
		t.Errorf("ResponseHeaderTimeout = %v, want 2m", tc.ResponseHeaderTimeout)
	}
	if tc.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tc.IdleConnTimeout)
	}
}

func TestTransportConfig_WithDefaults_FillsZeroFields(t *testing.T) {
	t.Parallel()
	// Only DialTimeout set; the other three should fall back to defaults.
	tc := TransportConfig{DialTimeout: 5 * time.Second}.WithDefaults()
	if tc.DialTimeout != 5*time.Second {
		t.Errorf("DialTimeout = %v, want 5s (explicit value preserved)", tc.DialTimeout)
	}
	if tc.TLSHandshakeTimeout != 15*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 15s (default)", tc.TLSHandshakeTimeout)
	}
	if tc.ResponseHeaderTimeout != 2*time.Minute {
		t.Errorf("ResponseHeaderTimeout = %v, want 2m (default)", tc.ResponseHeaderTimeout)
	}
	if tc.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s (default)", tc.IdleConnTimeout)
	}
}

func TestTransportConfig_ApplyTransport_SetsTimeouts(t *testing.T) {
	t.Parallel()
	tc := TransportConfig{
		DialTimeout:           7 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 9 * time.Second,
		IdleConnTimeout:       10 * time.Second,
	}
	tr := tc.applyTransport(&http.Transport{})
	if tr.DialContext == nil {
		t.Error("DialContext not set")
	}
	if tr.TLSHandshakeTimeout != 8*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 8s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 9*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 9s", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != 10*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 10s", tr.IdleConnTimeout)
	}
}

func TestNewHTTPSSource_AppliesTransportTimeouts(t *testing.T) {
	t.Parallel()
	src := newHTTPSSource(TransportConfig{ResponseHeaderTimeout: 11 * time.Second})
	tr, ok := src.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", src.client.Transport)
	}
	if tr.ResponseHeaderTimeout != 11*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 11s", tr.ResponseHeaderTimeout)
	}
	// Unset fields fall back to defaults.
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s (default)", tr.IdleConnTimeout)
	}
}

func TestNewBlobstoreSource_AppliesTransportTimeouts(t *testing.T) {
	t.Parallel()
	src := newBlobstoreSource(TransportConfig{DialTimeout: 12 * time.Second})
	tr, ok := src.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", src.client.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext not set")
	}
	if tr.TLSHandshakeTimeout != 15*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 15s (default)", tr.TLSHandshakeTimeout)
	}
}
