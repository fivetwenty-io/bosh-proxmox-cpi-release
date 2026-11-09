package endpointpool

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

func addrs(eps []*endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		out = append(out, ep.addr)
	}
	return out
}

func wantOrder(t *testing.T, got []*endpoint, want ...string) {
	t.Helper()
	if g := strings.Join(addrs(got), " "); g != strings.Join(want, " ") {
		t.Fatalf("order = [%s], want [%s]", g, strings.Join(want, " "))
	}
}

func TestNewRefusesBadInput(t *testing.T) {
	base := baseFor(nil, nil)
	cases := map[string][]Endpoint{
		"no endpoints":          nil,
		"no configured":         {discovered("pve2:8006", Pin{1})},
		"no port":               {configured("pve1")},
		"port zero":             {configured("pve1:0")},
		"bad host":              {configured("pve 1:8006")},
		"duplicate":             {configured("pve1:8006"), configured("PVE1:8006")},
		"duplicate across kind": {configured("[::1]:8006"), discovered("[0::1]:8006", Pin{1})},
		"unknown kind":          {{Address: "pve1:8006"}},
		"configured with pins":  {{Address: "pve1:8006", Kind: KindConfigured, Pins: []Pin{{1}}}},
		"discovered no pins":    {configured("pve1:8006"), discovered("pve2:8006")},
	}
	for name, eps := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := New(Config{Base: base, Endpoints: eps}); err == nil {
				t.Fatalf("New accepted %v as %s", eps, p.describe())
			}
		})
	}
	if _, err := New(Config{Endpoints: []Endpoint{configured("pve1:8006")}}); !errors.Is(err, ErrUnsupportedBase) {
		t.Fatalf("a nil base must be refused with ErrUnsupportedBase: %v", err)
	}
}

// Configured endpoints come first in their order and discovered ones follow in
// theirs, whatever order the caller listed them in, and the pool starts on the
// first one.
func TestPoolOrdersConfiguredBeforeDiscovered(t *testing.T) {
	var buf bytes.Buffer
	logger, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(t, Config{
		Base:   baseFor(nil, nil),
		Logger: logger,
		Endpoints: []Endpoint{
			discovered("pve3:8006", Pin{3}),
			configured("PVE1:8006"),
			{Address: "pve4:8006", Kind: KindDiscovered, Node: " pve4 ", Pins: []Pin{{4}, {44}}},
			configured("pve2:8006"),
		},
	})
	if p.Len() != 4 {
		t.Fatalf("Len = %d", p.Len())
	}
	order, gen := p.walkOrder(false)
	wantOrder(t, order, "pve1:8006", "pve2:8006", "pve3:8006", "pve4:8006")
	if gen != 0 {
		t.Fatalf("generation = %d, want 0", gen)
	}
	active, _ := p.current()
	if active.addr != "pve1:8006" || active.kind != KindConfigured {
		t.Fatalf("active = %s %s", active.addr, active.kind)
	}
	pve4 := endpointOf(t, p, "pve4:8006")
	if pve4.node != "pve4" || pve4.pins != 2 || pve4.kind != KindDiscovered {
		t.Fatalf("pve4 = %+v", pve4)
	}
	const line = "pve endpoints: pool [pve1:8006 configured, pve2:8006 configured, pve3:8006 discovered, " +
		"pve4:8006 discovered], starting on pve1:8006"
	if !strings.Contains(buf.String(), line) || !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Fatalf("want the pool-built line at Debug, got %s", buf.String())
	}
	if strings.Count(buf.String(), "pve endpoints: pool") != 1 {
		t.Fatalf("the pool-built line should appear once: %s", buf.String())
	}
}

// A demoted endpoint moves behind every endpoint that is not demoted, keeping
// its relative order, and the configured-only order leaves out discovered
// endpoints.
func TestWalkOrderWithDemotions(t *testing.T) {
	p := newPool(t, Config{
		Base: baseFor(nil, nil),
		Endpoints: []Endpoint{
			configured("pve1:8006"), configured("pve2:8006"),
			discovered("pve3:8006", Pin{3}), discovered("pve4:8006", Pin{4}),
		},
	})
	pve1, pve2, pve3 := endpointOf(t, p, "pve1:8006"), endpointOf(t, p, "pve2:8006"), endpointOf(t, p, "pve3:8006")

	p.demote(pve3)
	p.demote(pve1)
	order, _ := p.walkOrder(false)
	wantOrder(t, order, "pve2:8006", "pve4:8006", "pve1:8006", "pve3:8006")
	order, _ = p.walkOrder(true)
	wantOrder(t, order, "pve2:8006", "pve1:8006")
	if !p.demotedNow(pve1) || p.demotedNow(pve2) {
		t.Fatal("demotion state is wrong")
	}

	// Every endpoint demoted keeps preference order and still reaches them all.
	p.demote(pve2)
	p.demote(endpointOf(t, p, "pve4:8006"))
	order, _ = p.walkOrder(false)
	wantOrder(t, order, "pve1:8006", "pve2:8006", "pve3:8006", "pve4:8006")
}

// The active endpoint leads the walk, unless it is demoted or a
// configured-only walk cannot use it.
func TestWalkOrderStartsAtTheActiveEndpoint(t *testing.T) {
	p := newPool(t, Config{
		Base: baseFor(nil, nil),
		Endpoints: []Endpoint{
			configured("pve1:8006"), configured("pve2:8006"), discovered("pve3:8006", Pin{3}),
		},
	})
	pve2, pve3 := endpointOf(t, p, "pve2:8006"), endpointOf(t, p, "pve3:8006")
	if _, ok := p.promote(pve2, 0); !ok {
		t.Fatal("promote under the current generation should apply")
	}
	order, gen := p.walkOrder(false)
	wantOrder(t, order, "pve2:8006", "pve1:8006", "pve3:8006")
	if gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}
	if _, ok := p.promote(pve3, gen); !ok {
		t.Fatal("promote under the current generation should apply")
	}
	order, _ = p.walkOrder(false)
	wantOrder(t, order, "pve3:8006", "pve1:8006", "pve2:8006")
	order, _ = p.walkOrder(true)
	wantOrder(t, order, "pve1:8006", "pve2:8006")

	p.demote(pve3)
	order, _ = p.walkOrder(false)
	wantOrder(t, order, "pve1:8006", "pve2:8006", "pve3:8006")
}

// A success moves the active endpoint and clears its demotion only when the
// attempt started under the current generation, so a slow request from before
// a failover cannot move the pool back.
func TestPromoteGenerationGuard(t *testing.T) {
	p := newPool(t, Config{
		Base:      baseFor(nil, nil),
		Endpoints: []Endpoint{configured("pve1:8006"), configured("pve2:8006")},
	})
	pve1, pve2 := endpointOf(t, p, "pve1:8006"), endpointOf(t, p, "pve2:8006")
	_, started := p.current()

	p.demote(pve1)
	if prev, ok := p.promote(pve2, started); !ok || prev != pve1 {
		t.Fatalf("the failover's success should apply and name pve1 as the endpoint it replaced, got %v, %v",
			prev, ok)
	}
	if _, ok := p.promote(pve1, started); ok {
		t.Fatal("a stale success must not apply")
	}
	active, gen := p.current()
	if active != pve2 || gen != 1 || !p.demotedNow(pve1) {
		t.Fatalf("active %s gen %d demoted %v", active.addr, gen, p.demotedNow(pve1))
	}

	// A success on the active endpoint under the current generation clears a
	// demotion without raising the generation.
	p.demote(pve2)
	if prev, ok := p.promote(pve2, gen); !ok || prev != nil {
		t.Fatalf("a current success on the active endpoint should apply without a change, got %v, %v", prev, ok)
	}
	if _, g := p.current(); g != gen || p.demotedNow(pve2) {
		t.Fatalf("generation %d (want %d), demoted %v", g, gen, p.demotedNow(pve2))
	}

	// A current success on a demoted endpoint brings it back as active.
	if _, ok := p.promote(pve1, gen); !ok {
		t.Fatal("a current success should apply")
	}
	if active, g := p.current(); active != pve1 || g != gen+1 || p.demotedNow(pve1) {
		t.Fatalf("active %s gen %d demoted %v", active.addr, g, p.demotedNow(pve1))
	}
}

// After a connect-phase failure of a streamed upload, advance demotes the
// endpoint and moves the active endpoint on, once per generation.
func TestAdvance(t *testing.T) {
	p := newPool(t, Config{
		Base: baseFor(nil, nil),
		Endpoints: []Endpoint{
			configured("pve1:8006"), configured("pve2:8006"), discovered("pve3:8006", Pin{3}),
		},
	})
	pve1, pve2, pve3 := endpointOf(t, p, "pve1:8006"), endpointOf(t, p, "pve2:8006"), endpointOf(t, p, "pve3:8006")
	if to := p.advance(pve1, 0); to != pve2 {
		t.Fatalf("advance from the active endpoint should install pve2, got %v", to)
	}
	if active, gen := p.current(); active != pve2 || gen != 1 || !p.demotedNow(pve1) {
		t.Fatalf("active %s gen %d", active.addr, gen)
	}
	if p.advance(pve1, 0) != nil {
		t.Fatal("a stale advance must not move the active endpoint")
	}
	if p.advance(pve3, 1) != nil {
		t.Fatal("advance from an endpoint that is not active must not move it")
	}
	if !p.demotedNow(pve3) {
		t.Fatal("advance always demotes the failed endpoint")
	}
	// Every other endpoint is demoted, so the next one in preference order is
	// chosen among the demoted.
	if to := p.advance(pve2, 1); to != pve1 {
		t.Fatalf("advance should install the demoted pve1 when nothing else is left, got %v", to)
	}
	if active, _ := p.current(); active != pve1 {
		t.Fatalf("active = %s, want pve1:8006", active.addr)
	}

	single := newPool(t, Config{Base: baseFor(nil, nil), Endpoints: []Endpoint{configured("pve1:8006")}})
	only := endpointOf(t, single, "pve1:8006")
	if single.advance(only, 0) != nil {
		t.Fatal("a pool of one endpoint has nowhere to advance to")
	}
	if active, gen := single.current(); active != only || gen != 0 {
		t.Fatalf("active %s gen %d", active.addr, gen)
	}
}

// A routed host that is not an endpoint gets one clone with the tag, the
// operator's TLS, and no header deadline, built on first use and reused. A
// routed host that is an endpoint uses that endpoint's clones, so a discovered
// address keeps its pin.
func TestRoutedTransports(t *testing.T) {
	ca := pvetest.NewCA(t)
	live := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	refused := pvetest.NewRefused(t)
	d := &dialers{}
	d.set(refused.Addr, refused.DialContext)
	p := newPool(t, Config{
		Base:      baseFor(ca, d),
		Endpoints: []Endpoint{configured("pve1:8006"), discovered("pve2:8006", Pin{2})},
	})

	o, err := p.routedTransports(&url.URL{Scheme: "https", Host: live.Addr})
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.routedTransports(&url.URL{Scheme: "https", Host: live.Addr})
	if err != nil || again != o {
		t.Fatalf("the routed clone should be built once and reused: %v", err)
	}
	if o.idempotent != o.plain || o.plain.ResponseHeaderTimeout != 0 {
		t.Fatal("a routed clone has no header deadline")
	}
	if o.plain.TLSClientConfig.VerifyConnection != nil || o.plain.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("a routed clone follows the operator's TLS, with no pin")
	}
	mustOK(t, o.pick(false), http.MethodPost, live.Addr, "/nodes/pve9/storage/local/upload", "content=iso")

	r, err := p.routedTransports(&url.URL{Scheme: "https", Host: refused.Addr})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := send(t, context.Background(), r.pick(false), http.MethodPost, refused.Addr,
		"/nodes/pve9/storage/local/upload", "content=iso")
	if resp != nil {
		_ = resp.Body.Close()
	}
	mustConnectPhase(t, err, refused.Addr, PhaseDial)

	pinned, err := p.routedTransports(&url.URL{Scheme: "https", Host: "PVE2:8006"})
	if err != nil || pinned != endpointOf(t, p, "pve2:8006").transports {
		t.Fatalf("a routed request to a discovered endpoint must use its pinned clones: %v", err)
	}
	if pinned.plain.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("the discovered endpoint's clone lost its pin")
	}

	for in, want := range map[string]string{"https://pve9.example": "pve9.example:443", "http://pve9": "pve9:80"} {
		u, _ := url.Parse(in)
		o, err := p.routedTransports(u)
		if err != nil || o.addr != want {
			t.Errorf("%s: addr %v, %v; want %s", in, o, err, want)
		}
	}
	for _, u := range []*url.URL{nil, {Scheme: "ftp", Host: "pve9"}, {Scheme: "https", Host: "pve 9:8006"}} {
		if _, err := p.routedTransports(u); err == nil {
			t.Errorf("routedTransports(%v) should fail", u)
		}
	}
	p.CloseIdleConnections()
}

// Building a pool never changes the caller's base transport, and later changes
// to the base never reach the pool's clones.
func TestPoolKeepsItsOwnCopyOfTheBase(t *testing.T) {
	base := baseFor(nil, nil)
	p := newPool(t, Config{Base: base, Endpoints: []Endpoint{configured("pve1:8006")}})
	base.TLSClientConfig.InsecureSkipVerify = false
	base.MaxIdleConnsPerHost = 99
	o, err := p.routedTransports(&url.URL{Scheme: "https", Host: "pve9:8006"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.plain.TLSClientConfig.InsecureSkipVerify || o.plain.MaxIdleConnsPerHost != 2 {
		t.Fatal("a routed clone saw a change made to the caller's base after New")
	}
	if base.DialTLSContext != nil || base.TLSClientConfig.ServerName != "" {
		t.Fatal("New changed the caller's base")
	}
	if p.verifySSL {
		t.Fatal("verify_ssl should be read once, at construction")
	}
}

// A base without a TLS config behaves as Go's default, verifying against the
// system pool, and still builds clones that speak only HTTP/1.1. A bare base
// gets Go's HTTP/2 defaults when it is cloned, and a base with its own dialer
// does not, so both shapes are covered.
func TestPoolWithoutABaseTLSConfig(t *testing.T) {
	bases := map[string]*http.Transport{
		"bare":        {},
		"with dialer": {DialContext: (&net.Dialer{}).DialContext},
	}
	for name, base := range bases {
		t.Run(name, func(t *testing.T) {
			p := newPool(t, Config{
				Base:      base,
				Endpoints: []Endpoint{configured("pve1:8006"), discovered("pve2:8006", Pin{2})},
			})
			if !p.verifySSL {
				t.Fatal("no TLS config means verification is on")
			}
			for _, ep := range p.endpoints {
				for _, tr := range []*http.Transport{ep.transports.plain, ep.transports.idempotent} {
					if err := assertHTTP1(tr); err != nil {
						t.Fatalf("%s: %v", ep.addr, err)
					}
					if cfg := tr.TLSClientConfig; cfg.ServerName != hostOf(ep.addr) {
						t.Fatalf("%s: ServerName %q", ep.addr, cfg.ServerName)
					}
				}
			}
		})
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{KindConfigured: "configured", KindDiscovered: "discovered", 0: "kind(0)"} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
	if got := hostOf("not an address"); got != "not an address" {
		t.Errorf("hostOf passes an unsplittable value through, got %q", got)
	}
	if got := hostOf("[::1]:8006"); got != "::1" {
		t.Errorf("hostOf([::1]:8006) = %q", got)
	}
}

// The pool's state survives concurrent walks, outcomes, routed lookups, and
// idle closes under the race detector.
func TestPoolIsSafeForConcurrentUse(t *testing.T) {
	p := newPool(t, Config{
		Base: baseFor(nil, nil),
		Endpoints: []Endpoint{
			configured("pve1:8006"), configured("pve2:8006"), discovered("pve3:8006", Pin{3}),
		},
	})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				order, gen := p.walkOrder(j%2 == 0)
				ep := order[j%len(order)]
				switch (i + j) % 4 {
				case 0:
					p.demote(ep)
				case 1:
					p.promote(ep, gen)
				case 2:
					p.advance(ep, gen)
				default:
					if _, err := p.routedTransports(&url.URL{Scheme: "https", Host: "pve9:8006"}); err != nil {
						t.Error(err)
					}
					p.CloseIdleConnections()
				}
				_ = p.RefreshDue()
				_ = p.demotedNow(ep)
			}
		})
	}
	wg.Wait()
	order, _ := p.walkOrder(false)
	if len(order) != 3 {
		t.Fatalf("every endpoint should still be in the walk, got %v", addrs(order))
	}
}
