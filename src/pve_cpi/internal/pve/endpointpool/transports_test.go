package endpointpool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// Every way a connection can fail before a request byte exists comes back
// tagged with its phase and the endpoint, for a read and a mutation alike, and
// the server never sees a request.
func TestConnectPhaseFailuresAreTagged(t *testing.T) {
	ca := pvetest.NewCA(t)
	other := pvetest.NewCA(t)

	type tc struct {
		name  string
		setup func(t *testing.T, d *dialers, base *http.Transport) (addr string, requests func() int32)
		phase Phase
		check func(t *testing.T, err error)
	}
	cases := []tc{
		{
			name: "refused dial",
			setup: func(t *testing.T, d *dialers, _ *http.Transport) (string, func() int32) {
				r := pvetest.NewRefused(t)
				d.set(r.Addr, r.DialContext)
				return r.Addr, r.Counts.Requests.Load
			},
			phase: PhaseDial,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Errorf("want ECONNREFUSED in the chain: %v", err)
				}
				var op *net.OpError
				if !errors.As(err, &op) || op.Op != "dial" {
					t.Errorf("want a dial *net.OpError in the chain: %v", err)
				}
			},
		},
		{
			name: "dial timeout",
			setup: func(t *testing.T, d *dialers, _ *http.Transport) (string, func() int32) {
				dr := pvetest.NewDropped(t)
				d.set(dr.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
					ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
					return dr.DialContext(ctx, network, addr)
				})
				return dr.Addr, dr.Counts.Requests.Load
			},
			phase: PhaseDial,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("want a dial deadline in the chain: %v", err)
				}
				if errors.Is(err, ErrSendByPassed) {
					t.Errorf("a dial timeout with no send-by marker is not a send-by refusal: %v", err)
				}
			},
		},
		{
			name: "name lookup",
			setup: func(t *testing.T, d *dialers, _ *http.Transport) (string, func() int32) {
				const addr = "pve9.example.invalid:8006"
				d.set(addr, func(context.Context, string, string) (net.Conn, error) {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
						Err: "no such host", Name: "pve9.example.invalid", IsNotFound: true,
					}}
				})
				return addr, func() int32 { return 0 }
			},
			phase: PhaseDNS,
			check: func(t *testing.T, err error) {
				var dnsErr *net.DNSError
				if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
					t.Errorf("want the *net.DNSError in the chain: %v", err)
				}
			},
		},
		{
			name: "handshake garbage",
			setup: func(t *testing.T, _ *dialers, _ *http.Transport) (string, func() int32) {
				g := pvetest.NewGarbage(t)
				return g.Addr, g.Counts.Requests.Load
			},
			phase: PhaseTLSHandshake,
			check: func(t *testing.T, err error) {
				var rec tls.RecordHeaderError
				if !errors.As(err, &rec) {
					t.Errorf("want a tls.RecordHeaderError in the chain: %v", err)
				}
			},
		},
		{
			name: "handshake reset",
			setup: func(t *testing.T, _ *dialers, _ *http.Transport) (string, func() int32) {
				r := pvetest.NewReset(t)
				return r.Addr, r.Counts.Requests.Load
			},
			phase: PhaseTLSHandshake,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, syscall.ECONNRESET) {
					t.Errorf("want ECONNRESET in the chain: %v", err)
				}
			},
		},
		{
			name: "handshake timeout",
			setup: func(t *testing.T, _ *dialers, base *http.Transport) (string, func() int32) {
				base.TLSHandshakeTimeout = 100 * time.Millisecond
				dr := pvetest.NewDropped(t)
				return dr.Addr, dr.Counts.Requests.Load
			},
			phase: PhaseTLSHandshake,
			check: func(t *testing.T, err error) {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					t.Errorf("want a timeout net.Error in the chain: %v", err)
				}
				if !strings.Contains(err.Error(), "net/http: TLS handshake timeout") {
					t.Errorf("want Go's handshake timeout text: %v", err)
				}
			},
		},
		{
			name: "configured endpoint with an untrusted chain",
			setup: func(t *testing.T, _ *dialers, _ *http.Transport) (string, func() int32) {
				s := pvetest.NewLive(t, other.NewLeaf(t, pvetest.LeafOptions{}), nil)
				return s.Addr, s.Counts.Requests.Load
			},
			phase: PhaseTLSVerify,
			check: func(t *testing.T, err error) {
				var ua x509.UnknownAuthorityError
				if !errors.As(err, &ua) {
					t.Errorf("want an x509.UnknownAuthorityError in the chain: %v", err)
				}
			},
		},
		{
			name: "configured endpoint with a certificate for another name",
			setup: func(t *testing.T, _ *dialers, _ *http.Transport) (string, func() int32) {
				leaf := ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve2.other.example"}})
				s := pvetest.NewLive(t, leaf, nil)
				return s.Addr, s.Counts.Requests.Load
			},
			phase: PhaseTLSVerify,
			check: func(t *testing.T, err error) {
				var he x509.HostnameError
				if !errors.As(err, &he) {
					t.Errorf("want an x509.HostnameError in the chain: %v", err)
				}
			},
		},
	}
	for _, c := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(c.name+"/"+method, func(t *testing.T) {
				d := &dialers{}
				base := baseFor(ca, d)
				addr, requests := c.setup(t, d, base)
				p := newPool(t, Config{Base: base, Endpoints: []Endpoint{configured(addr)}})
				tr := endpointOf(t, p, addr).transports.pick(false)
				body := ""
				if method == http.MethodPost {
					body = "vmid=101"
				}
				resp, err := send(t, context.Background(), tr, method, addr, "/nodes/pve1/qemu", body)
				if resp != nil {
					_ = resp.Body.Close()
				}
				mustConnectPhase(t, err, addr, c.phase)
				c.check(t, err)
				if n := requests(); n != 0 {
					t.Errorf("the server saw %d requests, want none", n)
				}
			})
		}
	}
}

// The tag survives the http.Client's *url.Error wrapping, which is how the SDK
// sees it.
func TestConnectPhaseTagSurvivesTheClient(t *testing.T) {
	r := pvetest.NewRefused(t)
	d := &dialers{}
	d.set(r.Addr, r.DialContext)
	p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(r.Addr)}})
	c := &http.Client{Transport: endpointOf(t, p, r.Addr).transports.pick(false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, r.URL+"/api2/json/pools",
		strings.NewReader("poolid=x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("want a *url.Error, got %T: %v", err, err)
	}
	mustConnectPhase(t, err, r.Addr, PhaseDial)
}

// A configured endpoint follows the operator's TLS settings exactly. With a
// CA, the chain is checked against it and the name against the endpoint's own
// host, which also goes out as SNI. With verify_ssl false, any certificate is
// accepted.
func TestConfiguredEndpointFollowsTheOperatorsTLS(t *testing.T) {
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	addr := net.JoinHostPort("localhost", fmt.Sprint(s.Port))
	d := &dialers{}
	d.set(addr, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, s.Addr)
	})
	p := newPool(t, Config{Base: baseFor(ca, d), Endpoints: []Endpoint{configured(addr)}})
	mustOK(t, endpointOf(t, p, addr).transports.pick(false), http.MethodGet, addr, "/version", "")
	seen := s.Seen()
	if len(seen) != 1 || seen[0].ServerName != "localhost" {
		t.Fatalf("want one request with SNI localhost, got %+v", seen)
	}

	foreign := pvetest.NewCA(t)
	s2 := pvetest.NewLive(t, foreign.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"elsewhere.example"}}), nil)
	p2 := newPool(t, Config{Base: baseFor(nil, nil), Endpoints: []Endpoint{configured(s2.Addr)}})
	mustOK(t, endpointOf(t, p2, s2.Addr).transports.pick(false), http.MethodGet, s2.Addr, "/version", "")
}

// Go replays a GET on its own after a reused connection closes before the
// answer. When that replay dials a dead address, the final error carries the
// tag. The earlier attempt was written, but a GET is idempotent, so the tag's
// promise for a mutation is not weakened.
func TestGoRetryOfAGetAfterAReusedCloseIsTagged(t *testing.T) {
	ca := pvetest.NewCA(t)
	rc := pvetest.NewReuseCloser(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	p := newPool(t, Config{Base: baseFor(ca, nil), Endpoints: []Endpoint{configured(rc.Addr)}})
	tr := endpointOf(t, p, rc.Addr).transports.pick(false)
	mustOK(t, tr, http.MethodGet, rc.Addr, "/version", "")

	rc.SetOnReuse(rc.StopListening)
	resp, err := send(t, context.Background(), tr, http.MethodGet, rc.Addr, "/version", "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	mustConnectPhase(t, err, rc.Addr, PhaseDial)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("want the retry's refused dial in the chain: %v", err)
	}
	if n := rc.Counts.Reused.Load(); n != 1 {
		t.Errorf("reused closes = %d, want 1", n)
	}
}

// Go replays a mutation on its own only after a failure that wrote nothing.
// When the replay dials a dead address, the final error carries the tag, and
// the server saw only the first POST, so the tag still proves the failed POST
// was never sent.
func TestGoRetryOfAPostAfterANothingWrittenFailureIsTagged(t *testing.T) {
	ca := pvetest.NewCA(t)
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	refused := pvetest.NewRefused(t)

	var (
		first *breakableConn
		dials atomic.Int32
	)
	d := &dialers{}
	d.set(live.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) > 1 {
			return refused.DialContext(ctx, network, addr)
		}
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		first = &breakableConn{Conn: c}
		return first, nil
	})
	p := newPool(t, Config{Base: baseFor(ca, d), Endpoints: []Endpoint{configured(live.Addr)}})
	tr := endpointOf(t, p, live.Addr).transports.pick(false)
	mustOK(t, tr, http.MethodPost, live.Addr, "/pools", "poolid=a")

	first.broken.Store(true)
	resp, err := send(t, context.Background(), tr, http.MethodPost, live.Addr, "/pools", "poolid=b")
	if resp != nil {
		_ = resp.Body.Close()
	}
	mustConnectPhase(t, err, live.Addr, PhaseDial)
	if n := live.Counts.Method(http.MethodPost); n != 1 {
		t.Errorf("the server saw %d POSTs, want only the first", n)
	}
	if n := refused.Counts.Dials.Load(); n != 1 {
		t.Errorf("Go's replay dialed %d times, want 1", n)
	}
}

// A send-by deadline bounds connection setup for the request it guards.
func TestSendByBoundsConnectionSetup(t *testing.T) {
	t.Run("already passed, so nothing is dialed", func(t *testing.T) {
		dr := pvetest.NewDropped(t)
		d := &dialers{}
		d.set(dr.Addr, dr.DialContext)
		p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(dr.Addr)}})
		ctx := WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodDelete, "/pools")
		resp, err := send(t, ctx, endpointOf(t, p, dr.Addr).transports.pick(false), http.MethodDelete, dr.Addr,
			"/pools", "")
		if resp != nil {
			_ = resp.Body.Close()
		}
		mustConnectPhase(t, err, dr.Addr, PhaseDial)
		if !errors.Is(err, ErrSendByPassed) {
			t.Fatalf("want ErrSendByPassed: %v", err)
		}
		if n := dr.Counts.Dials.Load(); n != 0 {
			t.Fatalf("dials = %d, want none", n)
		}
	})

	t.Run("a dial that cannot finish in time is abandoned", func(t *testing.T) {
		dr := pvetest.NewDropped(t)
		d := &dialers{}
		d.set(dr.Addr, dr.DialContext)
		p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(dr.Addr)}})
		start := time.Now()
		ctx := WithSendBy(context.Background(), start.Add(100*time.Millisecond), http.MethodPut, "/pools/a")
		resp, err := send(t, ctx, endpointOf(t, p, dr.Addr).transports.pick(false), http.MethodPut, dr.Addr,
			"/pools/a", "comment=x")
		if resp != nil {
			_ = resp.Body.Close()
		}
		mustConnectPhase(t, err, dr.Addr, PhaseDial)
		if !errors.Is(err, ErrSendByPassed) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want ErrSendByPassed over the dial's deadline: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("the dial ran %s past a 100ms send-by deadline", elapsed)
		}
	})

	t.Run("a handshake that cannot finish in time is abandoned", func(t *testing.T) {
		dr := pvetest.NewDropped(t)
		p := newPool(t, Config{Base: baseFor(nil, nil), Endpoints: []Endpoint{configured(dr.Addr)}})
		ctx := WithSendBy(context.Background(), time.Now().Add(100*time.Millisecond), http.MethodDelete, "/pools")
		resp, err := send(t, ctx, endpointOf(t, p, dr.Addr).transports.pick(false), http.MethodDelete, dr.Addr,
			"/pools", "")
		if resp != nil {
			_ = resp.Body.Close()
		}
		mustConnectPhase(t, err, dr.Addr, PhaseTLSHandshake)
		if !errors.Is(err, ErrSendByPassed) {
			t.Fatalf("want ErrSendByPassed: %v", err)
		}
		if strings.Contains(err.Error(), "TLS handshake timeout") {
			t.Fatalf("a handshake the send-by bound cut off is not a handshake timeout: %v", err)
		}
	})

	t.Run("a request the marker does not name is not bound", func(t *testing.T) {
		r := pvetest.NewRefused(t)
		d := &dialers{}
		d.set(r.Addr, r.DialContext)
		p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(r.Addr)}})
		ctx := WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodDelete, "/pools")
		resp, err := send(t, ctx, endpointOf(t, p, r.Addr).transports.pick(false), http.MethodGet, r.Addr,
			"/pools", "")
		if resp != nil {
			_ = resp.Body.Close()
		}
		mustConnectPhase(t, err, r.Addr, PhaseDial)
		if errors.Is(err, ErrSendByPassed) {
			t.Fatalf("a readback is not guarded by the DELETE's marker: %v", err)
		}
		if n := r.Counts.Dials.Load(); n != 1 {
			t.Fatalf("dials = %d, want 1", n)
		}
	})

	t.Run("a request with time left goes through", func(t *testing.T) {
		ca := pvetest.NewCA(t)
		s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
		p := newPool(t, Config{Base: baseFor(ca, nil), Endpoints: []Endpoint{configured(s.Addr)}})
		ctx := WithSendBy(context.Background(), time.Now().Add(time.Minute), http.MethodDelete, "/pools")
		resp, err := send(t, ctx, endpointOf(t, p, s.Addr).transports.pick(false), http.MethodDelete, s.Addr,
			"/pools", "")
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		_ = resp.Body.Close()
		if n := s.Counts.Method(http.MethodDelete); n != 1 {
			t.Fatalf("DELETEs = %d, want 1", n)
		}
	})
}

// connectContext puts the bound of the request's own marker on the context,
// and clears a bound an earlier hop left there when no marker names the
// request.
func TestConnectContext(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	ctx := WithSendBy(context.Background(), deadline, http.MethodDelete, "/pools")
	if got := connectBound(connectContext(ctx, http.MethodDelete, "/api2/json/pools")); !got.Equal(deadline) {
		t.Fatalf("bound = %v, want %v", got, deadline)
	}
	bound := connectContext(ctx, http.MethodDelete, "/pools")
	if got := connectBound(connectContext(bound, http.MethodGet, "/pools")); !got.IsZero() {
		t.Fatalf("a readback inherited the bound %v", got)
	}
	plain := context.Background()
	if connectContext(plain, http.MethodGet, "/version") != plain {
		t.Fatal("a context with no marker and no bound should come back unchanged")
	}
}

// An owned transport dials only the address it was built for, so a defect in
// routing can never send a request through another endpoint's verifier.
func TestOwnedTransportRefusesAnotherAddress(t *testing.T) {
	ca := pvetest.NewCA(t)
	a := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	b := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	p := newPool(t, Config{Base: baseFor(ca, nil), Endpoints: []Endpoint{configured(a.Addr)}})
	resp, err := send(t, context.Background(), endpointOf(t, p, a.Addr).transports.pick(false), http.MethodPost,
		b.Addr, "/pools", "poolid=x")
	if resp != nil {
		_ = resp.Body.Close()
	}
	mustConnectPhase(t, err, a.Addr, PhaseDial)
	if !errors.Is(err, errWrongAddress) {
		t.Fatalf("want errWrongAddress: %v", err)
	}
	if n := b.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the other server saw %d connections", n)
	}
}

// Every owned clone speaks only HTTP/1.1, even when the base asks for HTTP/2
// and the server offers it.
func TestOwnedClonesSpeakOnlyHTTP1(t *testing.T) {
	ca := pvetest.NewCA(t)
	leaf := ca.NewLeaf(t, pvetest.LeafOptions{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf.Cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	base := baseFor(ca, nil)
	base.ForceAttemptHTTP2 = true
	base.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
		"h2": func(string, *tls.Conn) http.RoundTripper { return http.DefaultTransport },
	}
	base.Protocols = new(http.Protocols)
	base.Protocols.SetHTTP2(true)
	p := newPool(t, Config{Base: base, Endpoints: []Endpoint{configured(addr), discovered("pve2:8006", Pin{1})}})

	for _, ep := range p.endpoints {
		for _, tr := range []*http.Transport{ep.transports.plain, ep.transports.idempotent} {
			if err := assertHTTP1(tr); err != nil {
				t.Fatalf("%s: %v", ep.addr, err)
			}
			if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
				t.Fatalf("%s: NextProtos = %v", ep.addr, got)
			}
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := endpointOf(t, p, addr).transports.pick(true).RoundTrip(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 1 || string(body) != "HTTP/1.1" {
		t.Fatalf("client saw %s and the server saw %q, want HTTP/1.1 on both", resp.Proto, body)
	}
	if base.TLSClientConfig.NextProtos[0] != "h2" {
		t.Fatal("building the pool changed the caller's base transport")
	}
}

// The idempotent clone carries the header deadline and the plain clone does
// not, so a hung node fails a read with a timeout that is not a connect-phase
// failure, because the request was sent.
func TestHeaderDeadlineClones(t *testing.T) {
	ca := pvetest.NewCA(t)
	hung := pvetest.NewHung(t, ca.NewLeaf(t, pvetest.LeafOptions{}))
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	p := newPool(t, Config{
		Base:           baseFor(ca, nil),
		Endpoints:      []Endpoint{configured(hung.Addr), configured(live.Addr)},
		headerDeadline: 100 * time.Millisecond,
	})
	o := endpointOf(t, p, hung.Addr).transports
	if o.idempotent == o.plain {
		t.Fatal("a pool of two endpoints needs two clones per endpoint")
	}
	if o.plain.ResponseHeaderTimeout != 0 || o.idempotent.ResponseHeaderTimeout != 100*time.Millisecond {
		t.Fatalf("header deadlines: plain %s, idempotent %s", o.plain.ResponseHeaderTimeout,
			o.idempotent.ResponseHeaderTimeout)
	}
	resp, err := send(t, context.Background(), o.pick(true), http.MethodGet, hung.Addr, "/cluster/resources", "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("want the header deadline, got %v", err)
	}
	if IsConnectPhase(err) {
		t.Fatalf("a request that was written must not carry the connect-phase tag: %v", err)
	}
	if n := hung.Counts.Method(http.MethodGet); n != 1 {
		t.Fatalf("the hung server saw %d GETs, want 1", n)
	}
}

// Pools of one endpoint keep today's single transport and no header deadline,
// and larger pools default to 60 seconds.
func TestHeaderDeadlineDependsOnPoolSize(t *testing.T) {
	one := newPool(t, Config{Base: baseFor(nil, nil), Endpoints: []Endpoint{configured("pve1:8006")}})
	o := endpointOf(t, one, "pve1:8006").transports
	if o.idempotent != o.plain || o.plain.ResponseHeaderTimeout != 0 {
		t.Fatal("a pool of one endpoint should keep one clone with no header deadline")
	}
	two := newPool(t, Config{
		Base:      baseFor(nil, nil),
		Endpoints: []Endpoint{configured("pve1:8006"), discovered("pve2:8006", Pin{2})},
	})
	for _, ep := range two.endpoints {
		if ep.transports.idempotent.ResponseHeaderTimeout != defaultHeaderDeadline {
			t.Fatalf("%s: header deadline %s", ep.addr, ep.transports.idempotent.ResponseHeaderTimeout)
		}
		if ep.transports.plain.ResponseHeaderTimeout != 0 {
			t.Fatalf("%s: the plain clone has a header deadline", ep.addr)
		}
	}
}

// A candidate's clones use the 5-second dial and 10-second handshake caps,
// and a cap only ever shortens the base transport's own limit.
func TestConnectCaps(t *testing.T) {
	d := &connectDialer{handshakeTimeout: 0, handshakeCap: candidateHandshakeCap}
	ctx, cancel, timed := d.handshakeContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !timed || !ok || time.Until(dl) > candidateHandshakeCap {
		t.Fatalf("handshake deadline %v (timed %v), want within %s", dl, timed, candidateHandshakeCap)
	}
	d = &connectDialer{handshakeTimeout: 2 * time.Second, handshakeCap: candidateHandshakeCap}
	ctx2, cancel2, _ := d.handshakeContext(context.Background())
	defer cancel2()
	if dl, _ := ctx2.Deadline(); time.Until(dl) > 2*time.Second {
		t.Fatal("the cap must not lengthen a shorter base handshake timeout")
	}

	dr := pvetest.NewDropped(t)
	var sawDeadline time.Duration
	d = &connectDialer{
		dialCap: 50 * time.Millisecond,
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dl, ok := ctx.Deadline(); ok {
				sawDeadline = time.Until(dl)
			}
			return dr.DialContext(ctx, network, addr)
		},
	}
	if _, err := d.dialTCP(context.Background(), "tcp", dr.Addr); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the dial cap to end the dial: %v", err)
	}
	if sawDeadline <= 0 || sawDeadline > 50*time.Millisecond {
		t.Fatalf("dial saw a deadline %s away, want within the 50ms cap", sawDeadline)
	}
}

// The plain DialContext, which net/http uses for an http URL, tags its
// failures and honors the send-by bound too.
func TestPlainDialIsTagged(t *testing.T) {
	r := pvetest.NewRefused(t)
	d := &dialers{}
	d.set(r.Addr, r.DialContext)
	p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(r.Addr)}})
	tr := endpointOf(t, p, r.Addr).transports.pick(false)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+r.Addr+"/api2/json/pools",
		strings.NewReader("poolid=x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	mustConnectPhase(t, err, r.Addr, PhaseDial)

	ctx := connectContext(WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodPost, "/pools"),
		http.MethodPost, "/pools")
	if _, err := tr.DialContext(ctx, "tcp", r.Addr); !errors.Is(err, ErrSendByPassed) {
		t.Fatalf("want ErrSendByPassed from the plain dial: %v", err)
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "elsewhere:1"); !errors.Is(err, errWrongAddress) {
		t.Fatalf("want errWrongAddress from the plain dial: %v", err)
	}

	live := pvetest.NewLive(t, pvetest.NewCA(t).NewLeaf(t, pvetest.LeafOptions{}), nil)
	p2 := newPool(t, Config{Base: baseFor(nil, nil), Endpoints: []Endpoint{configured(live.Addr)}})
	conn, err := endpointOf(t, p2, live.Addr).transports.plain.DialContext(context.Background(), "tcp", live.Addr)
	if err != nil {
		t.Fatalf("plain dial to a listening address: %v", err)
	}
	_ = conn.Close()
}

// The phase classifiers sort each failure into its phase.
func TestPhaseClassifiers(t *testing.T) {
	if got := dialPhase(&net.DNSError{Err: "no such host"}); got != PhaseDNS {
		t.Errorf("DNS error: %s", got)
	}
	if got := dialPhase(errors.New("connection refused")); got != PhaseDial {
		t.Errorf("other dial error: %s", got)
	}
	verify := map[string]error{
		"verification error": &tls.CertificateVerificationError{Err: errors.New("x")},
		"unknown authority":  x509.UnknownAuthorityError{},
		"hostname":           x509.HostnameError{},
		"invalid":            x509.CertificateInvalidError{},
		"system roots":       x509.SystemRootsError{},
	}
	for name, err := range verify {
		if got := handshakePhase(fmt.Errorf("handshake: %w", err)); got != PhaseTLSVerify {
			t.Errorf("%s: %s", name, got)
		}
	}
	pin := &tls.CertificateVerificationError{Err: fmt.Errorf("%w: other", ErrPinMismatch)}
	if got := handshakePhase(pin); got != PhasePin {
		t.Errorf("pin mismatch: %s", got)
	}
	if got := handshakePhase(handshakeTimeoutError{}); got != PhaseTLSHandshake {
		t.Errorf("timeout: %s", got)
	}
	if !(handshakeTimeoutError{}).Temporary() {
		t.Error("the handshake timeout should read as temporary, as Go's own does")
	}
}

// canonicalAddress makes two spellings of one endpoint equal and refuses
// anything that is not a host and a port.
func TestCanonicalAddress(t *testing.T) {
	good := map[string]string{
		"PVE1.Example.COM:8006":      "pve1.example.com:8006",
		"10.0.0.1:8006":              "10.0.0.1:8006",
		"[0:0:0:0:0:0:0:1]:8006":     "[::1]:8006",
		"[fe80::1%eth0]:8006":        "[fe80::1%eth0]:8006",
		"pve_1.lab:443":              "pve_1.lab:443",
		"pve1:1":                     "pve1:1",
		"pve1:65535":                 "pve1:65535",
		"pve1.example.com.:8006":     "pve1.example.com.:8006",
		"[::ffff:127.0.0.1]:8006":    "[::ffff:127.0.0.1]:8006",
		"xn--bcher-kva.example:8006": "xn--bcher-kva.example:8006",
	}
	for in, want := range good {
		got, err := canonicalAddress(in)
		if err != nil || got != want {
			t.Errorf("canonicalAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "pve1", "pve1:", ":8006", "pve1:0", "pve1:65536", "pve1:-1", "pve1:08006", "pve1:http",
		"pve 1:8006", "pve1/x:8006", "user@pve1:8006", "[::1:8006", strings.Repeat("a", 254) + ":8006",
	}
	for _, in := range bad {
		if got, err := canonicalAddress(in); err == nil {
			t.Errorf("canonicalAddress(%q) = %q, want an error", in, got)
		}
	}
}

// The pool's own refusals carry the connect-phase tag, because no byte of the
// request left, and each is told apart from a dial failure the endpoint
// caused, so the walk never demotes or marks down an endpoint for one. A real
// dial failure still classifies as the endpoint's own, with its phase and
// cause unchanged. Every case goes through the http.Client, which is how the
// SDK sees it, and every case is a mutation that the tag still proves unsent.
func TestPoolRefusalsAreToldApartFromDialFailures(t *testing.T) {
	ca := pvetest.NewCA(t)
	refused := pvetest.NewRefused(t)
	dropped := pvetest.NewDropped(t)
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	other := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	d := &dialers{}
	d.set(refused.Addr, refused.DialContext)
	d.set(dropped.Addr, dropped.DialContext)
	p := newPool(t, Config{
		Base: baseFor(ca, d),
		Endpoints: []Endpoint{
			configured(refused.Addr), configured(dropped.Addr), discovered(live.Addr, PinOf(live.Leaf.DER)),
		},
	})
	refusals := map[string]error{
		"ErrSendByPassed":     ErrSendByPassed,
		"errWrongAddress":     errWrongAddress,
		"errPlaintextRefused": errPlaintextRefused,
	}
	past := time.Now().Add(-time.Second)
	cases := []struct {
		name     string
		endpoint string
		req      func(t *testing.T) *http.Request
		refusal  error
		check    func(t *testing.T, err error)
	}{
		{
			name: "a refused dial", endpoint: refused.Addr,
			req: func(t *testing.T) *http.Request {
				return apiRequest(t, context.Background(), http.MethodPost, refused.Addr, "/pools", "poolid=x")
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, syscall.ECONNREFUSED) {
					t.Errorf("want ECONNREFUSED in the chain: %v", err)
				}
			},
		},
		{
			name: "a send-by deadline that passed before the dial", endpoint: refused.Addr,
			req: func(t *testing.T) *http.Request {
				ctx := WithSendBy(context.Background(), past, http.MethodDelete, "/pools/x")
				return apiRequest(t, ctx, http.MethodDelete, refused.Addr, "/pools/x", "")
			},
			refusal: ErrSendByPassed,
		},
		{
			name: "a send-by deadline that passed during the dial", endpoint: dropped.Addr,
			req: func(t *testing.T) *http.Request {
				ctx := WithSendBy(context.Background(), time.Now().Add(100*time.Millisecond), http.MethodPut, "/pools/x")
				return apiRequest(t, ctx, http.MethodPut, dropped.Addr, "/pools/x", "comment=x")
			},
			refusal: ErrSendByPassed,
		},
		{
			name: "an owned transport asked for another address", endpoint: refused.Addr,
			req: func(t *testing.T) *http.Request {
				return apiRequest(t, context.Background(), http.MethodPost, other.Addr, "/pools", "poolid=x")
			},
			refusal: errWrongAddress,
			check: func(t *testing.T, _ error) {
				if n := other.Counts.Conns.Load(); n != 0 {
					t.Errorf("the other server saw %d connections", n)
				}
			},
		},
		{
			name: "plain http to a discovered endpoint", endpoint: live.Addr,
			req: func(t *testing.T) *http.Request {
				req := apiRequest(t, context.Background(), http.MethodPost, live.Addr, "/access/ticket", "password=x")
				req.URL.Scheme = "http"
				return req
			},
			refusal: errPlaintextRefused,
			check: func(t *testing.T, _ error) {
				if n := live.Counts.Conns.Load(); n != 0 {
					t.Errorf("the discovered server saw %d connections", n)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dialsBefore := refused.Counts.Dials.Load()
			req := c.req(t)
			client := &http.Client{Transport: endpointOf(t, p, c.endpoint).transports.pick(false)}
			resp, err := client.Do(req)
			closeBody(resp)
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Fatalf("want a *url.Error, got %T: %v", err, err)
			}
			mustConnectPhase(t, err, c.endpoint, PhaseDial)
			if got, want := blamesEndpoint(err), c.refusal == nil; got != want {
				t.Fatalf("blamesEndpoint = %v, want %v: %v", got, want, err)
			}
			for name, sentinel := range refusals {
				if got, want := errors.Is(err, sentinel), c.refusal != nil && errors.Is(sentinel, c.refusal); got != want {
					t.Errorf("errors.Is(err, %s) = %v, want %v: %v", name, got, want, err)
				}
			}
			if !provesNeverSent(req, err) {
				t.Errorf("the tag should still prove the mutation unsent: %v", err)
			}
			if errors.Is(c.refusal, ErrSendByPassed) && c.endpoint == refused.Addr {
				if n := refused.Counts.Dials.Load() - dialsBefore; n != 0 {
					t.Errorf("a passed deadline dialed %d times", n)
				}
			}
			if c.check != nil {
				c.check(t, err)
			}
		})
	}
}

// blamesEndpoint is true for a connect-phase failure of every phase, however
// deeply it is wrapped, and false for the pool's own refusals in any phase, for
// a failure without the tag, and for nil.
func TestBlamesEndpoint(t *testing.T) {
	cause := errors.New("connection refused")
	for _, phase := range []Phase{PhaseDial, PhaseDNS, PhaseTLSHandshake, PhaseTLSVerify, PhasePin} {
		err := &ConnectPhaseError{Phase: phase, Endpoint: "pve1:8006", Err: cause}
		if !blamesEndpoint(err) || !blamesEndpoint(fmt.Errorf("post: %w", &url.Error{Op: "Post", Err: err})) {
			t.Errorf("%s: a connect-phase failure should blame the endpoint", phase)
		}
		for _, refusal := range []error{ErrSendByPassed, errWrongAddress, errPlaintextRefused} {
			refused := &ConnectPhaseError{Phase: phase, Endpoint: "pve1:8006", Err: fmt.Errorf("%w: %w", refusal, cause)}
			if blamesEndpoint(refused) || blamesEndpoint(fmt.Errorf("post: %w", refused)) {
				t.Errorf("%s: %v should not blame the endpoint", phase, refusal)
			}
		}
	}
	for name, err := range map[string]error{
		"nil":                nil,
		"an untagged error":  cause,
		"a maybe-sent error": &MaybeSentError{Err: cause},
		"a bare refusal":     ErrSendByPassed,
	} {
		if blamesEndpoint(err) {
			t.Errorf("%s should not blame an endpoint", name)
		}
	}
}

// provesNeverSent mirrors the cases in which Go's transport replays a request
// after a written attempt, and a redirect's later legs, and holds for any
// other request whose error carries the tag.
func TestProvesNeverSent(t *testing.T) {
	tagged := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "pve1:8006", Err: errors.New("refused")}
	refusal := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "pve1:8006", Err: ErrSendByPassed}
	build := func(method, body string, edit func(*http.Request)) *http.Request {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(context.Background(), method, "https://pve1:8006/api2/json/pools", r)
		if err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(req)
		}
		return req
	}
	streamed := func(req *http.Request) {
		req.Body = io.NopCloser(strings.NewReader("poolid=x"))
		req.GetBody = nil
	}
	cases := []struct {
		name string
		req  *http.Request
		err  error
		want bool
	}{
		{"a POST with a body", build(http.MethodPost, "poolid=x", nil), tagged, true},
		{"a POST without a body", build(http.MethodPost, "", nil), tagged, true},
		{"a DELETE", build(http.MethodDelete, "", nil), tagged, true},
		{"a refused send-by", build(http.MethodDelete, "", nil), refusal, true},
		{"a PUT with Idempotency-Key", build(http.MethodPut, "comment=x", func(r *http.Request) {
			r.Header.Set("Idempotency-Key", "k")
		}), tagged, false},
		{"a POST with X-Idempotency-Key", build(http.MethodPost, "", func(r *http.Request) {
			r.Header.Set("X-Idempotency-Key", "k")
		}), tagged, false},
		{"a POST with Idempotency-Key and a streamed body", build(http.MethodPost, "", func(r *http.Request) {
			r.Header.Set("Idempotency-Key", "k")
			streamed(r)
		}), tagged, true},
		{"a POST with the key under a name Go does not check", build(http.MethodPost, "", func(r *http.Request) {
			r.Header["idempotency-key"] = []string{"k"}
		}), tagged, true},
		{"a GET", build(http.MethodGet, "", nil), tagged, false},
		{"a HEAD", build(http.MethodHead, "", nil), tagged, false},
		{"an OPTIONS", build(http.MethodOptions, "", nil), tagged, false},
		{"a TRACE", build(http.MethodTrace, "", nil), tagged, false},
		{"an empty method", build(http.MethodPost, "", func(r *http.Request) { r.Method = "" }), tagged, false},
		{"a redirect leg", build(http.MethodPost, "poolid=x", func(r *http.Request) {
			r.Response = &http.Response{StatusCode: http.StatusTemporaryRedirect}
		}), tagged, false},
		{"an untagged error", build(http.MethodPost, "", nil), errors.New("reset"), false},
		{"a maybe-sent error around the tag", build(http.MethodPost, "", nil), &MaybeSentError{Err: tagged}, false},
		{"no request", nil, tagged, false},
		{"no error", build(http.MethodPost, "", nil), nil, false},
	}
	for _, c := range cases {
		if got := provesNeverSent(c.req, c.err); got != c.want {
			t.Errorf("%s: provesNeverSent = %v, want %v", c.name, got, c.want)
		}
	}
}

// Go counts a mutation that carries an Idempotency-Key or X-Idempotency-Key
// header as replayable, so it replays it after a written attempt dies on a
// reused connection. When the replay's dial fails, the final error carries the
// tag although the first attempt reached the server, so provesNeverSent must
// not call the mutation unsent. Without either header Go does not replay the
// mutation, and its error is not a connect-phase failure at all.
func TestGoReplayOfAMutationWithAnIdempotencyKey(t *testing.T) {
	for _, header := range []string{"Idempotency-Key", "X-Idempotency-Key", ""} {
		name := header
		if name == "" {
			name = "no header"
		}
		t.Run(name, func(t *testing.T) {
			ca := pvetest.NewCA(t)
			rc := pvetest.NewReuseCloser(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
			p := newPool(t, Config{Base: baseFor(ca, nil), Endpoints: []Endpoint{configured(rc.Addr)}})
			tr := endpointOf(t, p, rc.Addr).transports.pick(false)
			mustOK(t, tr, http.MethodPost, rc.Addr, "/pools", "poolid=a")

			rc.SetOnReuse(rc.StopListening)
			req := apiRequest(t, context.Background(), http.MethodPost, rc.Addr, "/pools", "poolid=b")
			if header != "" {
				req.Header.Set(header, "pool-b")
			}
			resp, err := tr.RoundTrip(req)
			closeBody(resp)
			if n := rc.Counts.Reused.Load(); n != 1 {
				t.Fatalf("the second POST should have reached the server on the reused connection: reused = %d", n)
			}
			if provesNeverSent(req, err) {
				t.Fatalf("a POST that reached the server is not proven unsent: %v", err)
			}
			if header == "" {
				if err == nil || IsConnectPhase(err) {
					t.Fatalf("Go should not replay a mutation without the header: %v", err)
				}
				return
			}
			mustConnectPhase(t, err, rc.Addr, PhaseDial)
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("want the replay's refused dial in the chain: %v", err)
			}
		})
	}
}

// legRecorder sends each leg of a client's request through the pool's
// transports for the leg's URL host, and records every leg it sent.
type legRecorder struct {
	p    *Pool
	legs []recordedLeg
}

type recordedLeg struct {
	req *http.Request
	err error
}

func (r *legRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	o, err := r.p.routedTransports(req.URL)
	if err != nil {
		return nil, err
	}
	resp, err := o.pick(false).RoundTrip(req)
	r.legs = append(r.legs, recordedLeg{req: req, err: err})
	return resp, err
}

// The http.Client sends a redirect's next leg through the transport after the
// first leg was written and answered. When that leg fails in the connect
// phase, its error carries the tag although the mutation reached a server, so
// provesNeverSent must not call it unsent.
func TestRedirectLegIsNotProvenUnsent(t *testing.T) {
	ca := pvetest.NewCA(t)
	refused := pvetest.NewRefused(t)
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodPost, "/pools", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+refused.Addr+pvetest.APIPrefix+"/pools", http.StatusTemporaryRedirect)
	})
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), routes)
	d := &dialers{}
	d.set(refused.Addr, refused.DialContext)
	p := newPool(t, Config{
		Base:      baseFor(ca, d),
		Endpoints: []Endpoint{configured(live.Addr), configured(refused.Addr)},
	})
	rec := &legRecorder{p: p}
	req := apiRequest(t, context.Background(), http.MethodPost, live.Addr, "/pools", "poolid=x")
	resp, err := (&http.Client{Transport: rec}).Do(req)
	closeBody(resp)
	mustConnectPhase(t, err, refused.Addr, PhaseDial)
	if n := live.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("the first leg should have reached the server: POSTs = %d", n)
	}
	if len(rec.legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(rec.legs))
	}
	first, second := rec.legs[0], rec.legs[1]
	if first.err != nil || first.req.Response != nil {
		t.Fatalf("the first leg should be answered and carry no redirect response: %v", first.err)
	}
	if second.req.Response == nil || !IsConnectPhase(second.err) {
		t.Fatalf("the second leg should be a redirect leg that failed to connect: %v", second.err)
	}
	if provesNeverSent(second.req, second.err) {
		t.Fatalf("a redirect leg after a written leg is not proven unsent: %v", second.err)
	}
	if !blamesEndpoint(second.err) {
		t.Fatalf("the refused dial on the redirect leg is still the endpoint's own failure: %v", second.err)
	}
}

// A discovered endpoint and a cold-start candidate check certificates with the
// pool's own callback, so both of their clones refuse a plain http connection,
// which would carry the request and its credential with no check at all.
func TestPinnedClonesRefusePlainHTTP(t *testing.T) {
	ca := pvetest.NewCA(t)
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	p := newPool(t, Config{
		Base:      baseFor(ca, nil),
		Endpoints: []Endpoint{configured("pve1:8006"), discovered(live.Addr, PinOf(live.Leaf.DER))},
	})
	v, err := newCandidateVerifier(live.Host, true, ca.Pool(), nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newOwnedTransports(baseFor(ca, nil), transportSpec{addr: live.Addr, verify: v.verifyConnection})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(candidate.closeIdle)
	for name, o := range map[string]*ownedTransports{
		"discovered": endpointOf(t, p, live.Addr).transports,
		"candidate":  candidate,
	} {
		for _, tr := range []*http.Transport{o.plain, o.idempotent} {
			req := apiRequest(t, context.Background(), http.MethodPost, live.Addr, "/access/ticket", "password=x")
			req.URL.Scheme = "http"
			resp, err := tr.RoundTrip(req)
			closeBody(resp)
			mustConnectPhase(t, err, live.Addr, PhaseDial)
			if !errors.Is(err, errPlaintextRefused) || blamesEndpoint(err) {
				t.Fatalf("%s: want a plaintext refusal that does not blame the endpoint: %v", name, err)
			}
		}
		if _, err := o.plain.DialContext(context.Background(), "tcp", "elsewhere:1"); !errors.Is(err, errWrongAddress) {
			t.Fatalf("%s: the address check comes first: %v", name, err)
		}
	}
	if n := live.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the server saw %d connections", n)
	}
	mustOK(t, endpointOf(t, p, live.Addr).transports.pick(false), http.MethodGet, live.Addr, "/version", "")
}

// The pool compares addresses in canonical form, but the host goes out as SNI
// in the spelling the request named, as it does from Go's own transport. A
// configured endpoint and a routed host keep the spelling they were given as
// their clones' default, and a dial for another spelling of the same address
// sends that spelling without changing the clone's config.
func TestHostSpellingReachesTheWire(t *testing.T) {
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	port := fmt.Sprint(s.Port)
	wire := net.JoinHostPort("PVE1.Example.COM", port)
	canonical := net.JoinHostPort("pve1.example.com", port)
	routed := net.JoinHostPort("Stemcells.Example.COM", port)
	d := &dialers{}
	for _, a := range []string{wire, canonical, routed, strings.ToUpper(routed)} {
		d.set(a, func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, s.Addr)
		})
	}
	p := newPool(t, Config{Base: baseFor(nil, d), Endpoints: []Endpoint{configured(wire)}})
	ep := endpointOf(t, p, wire)
	if ep.addr != canonical || ep.urlHost != wire {
		t.Fatalf("addr %q urlHost %q, want the canonical key and the configured spelling", ep.addr, ep.urlHost)
	}
	cfg := ep.transports.plain.TLSClientConfig
	if cfg.ServerName != "PVE1.Example.COM" {
		t.Fatalf("ServerName %q, want the configured spelling", cfg.ServerName)
	}
	mustOK(t, ep.transports.pick(false), http.MethodGet, wire, "/version", "")
	mustOK(t, ep.transports.pick(false), http.MethodGet, canonical, "/version", "")

	o, err := p.routedTransports(&url.URL{Scheme: "https", Host: routed})
	if err != nil {
		t.Fatal(err)
	}
	if o.plain.TLSClientConfig.ServerName != "Stemcells.Example.COM" {
		t.Fatalf("routed ServerName %q, want the URL's spelling", o.plain.TLSClientConfig.ServerName)
	}
	mustOK(t, o.pick(false), http.MethodGet, routed, "/version", "")
	mustOK(t, o.pick(false), http.MethodGet, strings.ToUpper(routed), "/version", "")

	want := []string{"PVE1.Example.COM", "pve1.example.com", "Stemcells.Example.COM", "STEMCELLS.EXAMPLE.COM"}
	seen := s.Seen()
	if len(seen) != len(want) {
		t.Fatalf("requests = %d, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if seen[i].ServerName != w {
			t.Errorf("request %d: SNI %q, want %q", i, seen[i].ServerName, w)
		}
	}
	if cfg.ServerName != "PVE1.Example.COM" || o.plain.TLSClientConfig.ServerName != "Stemcells.Example.COM" {
		t.Fatal("a dial for another spelling changed the clone's own config")
	}
}
