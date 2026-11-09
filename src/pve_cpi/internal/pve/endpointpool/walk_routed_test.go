package endpointpool

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// A request for a host outside the pool goes once to that host, carries the
// caller's request on its response, and leaves the active endpoint alone.
func TestRoutedRequestGoesOnceToItsHost(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	r := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	req := apiRequest(t, context.Background(), http.MethodPost, r.Addr, "/nodes/pve3/storage/local/upload", "data")
	resp, err := p.RoundTrip(req)
	mustStatus(t, resp, err, http.StatusOK)
	if resp.Request != req {
		t.Fatal("the response does not carry the caller's request")
	}
	if r.Counts.Requests.Load() != 1 || a.Counts.Conns.Load() != 0 || b.Counts.Conns.Load() != 0 {
		t.Fatalf("requests R=%d, connections A=%d B=%d", r.Counts.Requests.Load(), a.Counts.Conns.Load(),
			b.Counts.Conns.Load())
	}
	mustActive(t, p, a.Addr)
}

// A routed request that reached its host and then lost the connection is
// marked maybe sent, a read or a mutation alike.
func TestRoutedTransportErrorIsMarkedMaybeSent(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		l := newLab(t)
		a := l.live(t, nil)
		r := pvetest.NewBodyThenHijack(t, l.leaf)
		p := l.pool(t, 0, configured(a.Addr), configured(l.live(t, nil).Addr))
		_, err := send(t, context.Background(), p, method, r.Addr, "/nodes/pve3/qemu", "vmid=101")
		if !IsMaybeSent(err) {
			t.Fatalf("%s: err = %v, want it marked maybe sent", method, err)
		}
		if n := r.Counts.Method(method); n != 1 {
			t.Fatalf("%s: R got %d requests, want 1", method, n)
		}
	}
}

// A routed connect-phase failure keeps its proof for a mutation that provably
// never left, and is marked maybe sent for one that Go may have replayed.
func TestRoutedConnectPhaseFailure(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	r := l.refused(t)
	p := l.pool(t, 0, configured(a.Addr), configured(l.live(t, nil).Addr))

	_, err := send(t, context.Background(), p, http.MethodPost, r.Addr, "/nodes/pve3/qemu", "vmid=101")
	if !IsConnectPhase(err) || IsMaybeSent(err) || !provesNeverSent(apiRequest(t, context.Background(),
		http.MethodPost, r.Addr, "/nodes/pve3/qemu", "vmid=101"), err) {
		t.Fatalf("plain POST: err = %v, want an unmarked connect-phase failure", err)
	}

	req := apiRequest(t, context.Background(), http.MethodPost, r.Addr, "/nodes/pve3/qemu", "vmid=101")
	req.Header.Set("Idempotency-Key", "k1")
	if _, err := p.RoundTrip(req); !IsMaybeSent(err) {
		t.Fatalf("POST with Idempotency-Key: err = %v, want it marked maybe sent", err)
	}

	if _, err := send(t, context.Background(), p, http.MethodGet, r.Addr, "/version", ""); !IsConnectPhase(err) ||
		IsMaybeSent(err) {
		t.Fatalf("GET: err = %v, want an unmarked connect-phase failure", err)
	}
	mustActive(t, p, a.Addr)
}

// A routed request to one of the pool's own endpoints uses that endpoint's
// clones. A connect-phase failure demotes it, and an answer never makes it
// active.
func TestRoutedRequestToAPoolEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.refused(t)
	c := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr), configured(c.Addr))

	if _, err := send(t, context.Background(), p, http.MethodGet, b.Addr, "/version", ""); !IsConnectPhase(err) {
		t.Fatalf("err = %v, want a connect-phase failure", err)
	}
	mustDemoted(t, p, b.Addr, true)

	resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, a.Addr)
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0", n)
	}
}

// A configured-only request goes to a routed host only when it is a
// configured endpoint, so a credential never reaches a discovered or unlisted
// address.
func TestRoutedConfiguredOnly(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	c2 := l.live(t, nil)
	d := l.live(t, nil)
	other := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(c2.Addr), discovered(d.Addr, PinOf(l.leaf.DER)))

	for _, s := range []*pvetest.Server{d, other} {
		body := &trackedBody{Reader: strings.NewReader("username=root&password=secret")}
		req := apiRequest(t, context.Background(), http.MethodPost, s.Addr, "/access/ticket", "x")
		req.Body = body
		_, err := p.RoundTrip(req)
		if err == nil || !strings.Contains(err.Error(), "not a configured endpoint") || !body.closed {
			t.Fatalf("login to %s: err = %v, body closed %v", s.Addr, err, body.closed)
		}
		if n := s.Counts.Conns.Load(); n != 0 {
			t.Fatalf("%s got %d connections", s.Addr, n)
		}
	}
	_, err := send(t, WithConfiguredOnly(context.Background()), p, http.MethodGet, d.Addr, "/cluster/status", "")
	if err == nil {
		t.Fatal("a configured-only read reached a discovered endpoint")
	}

	resp, err := send(t, context.Background(), p, http.MethodPost, c2.Addr, "/access/ticket",
		"username=root&password=secret")
	mustStatus(t, resp, err, http.StatusOK)
	if n := c2.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the configured endpoint got %d logins, want 1", n)
	}
}

// A routed request to a configured endpoint that fails verification logs the
// Error line and does not move.
func TestRoutedConfiguredVerificationFailure(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"other.example"}}), nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	_, err := send(t, context.Background(), p, http.MethodGet, b.Addr, "/version", "")
	cpe, ok := AsConnectPhase(err)
	if !ok || cpe.Phase != PhaseTLSVerify {
		t.Fatalf("err = %v, want a tls_verify failure", err)
	}
	errs := l.entries(slog.LevelError, "pve endpoint "+b.Addr+": TLS verification failed (")
	if len(errs) != 1 || !strings.HasSuffix(errs[0].Message, "; no other endpoint is tried for this request") {
		t.Fatalf("verification lines = %+v", errs)
	}
	if n := a.Counts.Requests.Load(); n != 0 {
		t.Fatalf("A got %d requests, want 0", n)
	}
}

// A routed request whose send-by deadline passes during the dial is refused,
// logged, and never marked maybe sent.
func TestRoutedSendByPassingDuringTheDial(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	r := l.dropped(t)
	p := l.pool(t, 0, configured(a.Addr), configured(l.live(t, nil).Addr))
	ctx := WithSendBy(context.Background(), time.Now().Add(150*time.Millisecond), http.MethodPost, "/nodes/pve3/qemu")
	_, err := send(t, ctx, p, http.MethodPost, r.Addr, "/nodes/pve3/qemu", "vmid=101")
	if !errors.Is(err, ErrSendByPassed) || IsMaybeSent(err) {
		t.Fatalf("err = %v, want an unmarked ErrSendByPassed", err)
	}
	if n := len(l.entries(slog.LevelDebug, "pve endpoints: POST /nodes/pve3/qemu not sent")); n != 1 {
		t.Fatalf("send-by lines = %d, want 1", n)
	}
}

func TestURLAddress(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://PVE1.example:8006/api2/json", "pve1.example:8006", true},
		{"https://pve1.example/api2/json", "pve1.example:443", true},
		{"http://pve1.example/api2/json", "pve1.example:80", true},
		{"https://[::1]:8006/", "[::1]:8006", true},
		{"ftp://pve1.example/", "", false},
		{"https://:8006/", "", false},
	}
	for _, c := range cases {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := urlAddress(req.URL)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("urlAddress(%s) = %q, %v, want %q, ok %v", c.raw, got, err, c.want, c.ok)
		}
	}
}
