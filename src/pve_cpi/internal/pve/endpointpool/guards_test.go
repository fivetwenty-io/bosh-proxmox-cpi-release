package endpointpool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The pool refuses a base transport whose settings its clones cannot keep.
func TestCheckBaseRefusals(t *testing.T) {
	cases := map[string]func(*http.Transport){
		"proxy": func(b *http.Transport) {
			b.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
		},
		"DialTLSContext": func(b *http.Transport) {
			b.DialTLSContext = func(context.Context, string, string) (net.Conn, error) { return nil, nil }
		},
		"MaxConnsPerHost":       func(b *http.Transport) { b.MaxConnsPerHost = 4 },
		"ResponseHeaderTimeout": func(b *http.Transport) { b.ResponseHeaderTimeout = time.Second },
		"VerifyPeerCertificate": func(b *http.Transport) {
			b.TLSClientConfig.VerifyPeerCertificate = func([][]byte, [][]*x509.Certificate) error { return nil }
		},
		"VerifyConnection": func(b *http.Transport) {
			b.TLSClientConfig.VerifyConnection = func(tls.ConnectionState) error { return nil }
		},
		"ServerName": func(b *http.Transport) { b.TLSClientConfig.ServerName = "pve1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			base := baseFor(nil, nil)
			mutate(base)
			if err := checkBase(base); !errors.Is(err, ErrUnsupportedBase) {
				t.Fatalf("checkBase: %v", err)
			}
			_, err := New(Config{Base: base, Endpoints: []Endpoint{configured("pve1:8006")}})
			if !errors.Is(err, ErrUnsupportedBase) {
				t.Fatalf("New: %v", err)
			}
		})
	}
	if err := checkBase(nil); !errors.Is(err, ErrUnsupportedBase) {
		t.Fatalf("checkBase(nil): %v", err)
	}
	if err := checkBase(&http.Transport{}); err != nil {
		t.Fatalf("a bare transport is fine: %v", err)
	}
	base := baseFor(nil, nil)
	base.ForceAttemptHTTP2 = true
	base.DialContext = (&net.Dialer{}).DialContext
	if err := checkBase(base); err != nil {
		t.Fatalf("HTTP/2 settings and a dialer are replaced in the clones, not refused: %v", err)
	}
}

// assertHTTP1 refuses every way a clone could negotiate HTTP/2 or bypass the
// pool's dialer.
func TestAssertHTTP1Refusals(t *testing.T) {
	good := func() *http.Transport {
		o, err := newOwnedTransports(baseFor(nil, nil), transportSpec{addr: "pve1:8006"})
		if err != nil {
			t.Fatal(err)
		}
		return o.plain.Clone()
	}
	if err := assertHTTP1(good()); err != nil {
		t.Fatalf("a pool clone should pass: %v", err)
	}
	cases := map[string]func(*http.Transport){
		"ForceAttemptHTTP2": func(tr *http.Transport) { tr.ForceAttemptHTTP2 = true },
		"nil TLSNextProto":  func(tr *http.Transport) { tr.TLSNextProto = nil },
		"h2 in TLSNextProto": func(tr *http.Transport) {
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{"h2": nil}
		},
		"Protocols": func(tr *http.Transport) {
			tr.Protocols = new(http.Protocols)
			tr.Protocols.SetHTTP1(true)
		},
		"h2 in NextProtos":          func(tr *http.Transport) { tr.TLSClientConfig.NextProtos = []string{"h2"} },
		"no tagging DialContext":    func(tr *http.Transport) { tr.DialContext = nil },
		"no tagging DialTLSContext": func(tr *http.Transport) { tr.DialTLSContext = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tr := good()
			mutate(tr)
			if err := assertHTTP1(tr); !errors.Is(err, errOwnedTransport) {
				t.Fatalf("assertHTTP1: %v", err)
			}
		})
	}
	if err := assertHTTP1(nil); !errors.Is(err, errOwnedTransport) {
		t.Fatalf("assertHTTP1(nil): %v", err)
	}
}
