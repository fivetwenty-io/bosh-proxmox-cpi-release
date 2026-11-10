package endpointpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// A walk whose last entry becomes ineligible while an earlier hop runs gives
// the final-hop treatment to the hop that is now last. Here the configured c
// is the last eligible endpoint once d fails its pin during a's dial, so c
// gets no header deadline and its slow answer is kept.
func TestTheLastEligibleHopIsTheFinalHop(t *testing.T) {
	l := newLab(t)
	const a = "pve8.example.invalid:8006"
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/version", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":{"version":"9.0.2"}}`))
	})
	c := l.live(t, routes)
	d := l.live(t, nil)
	var p *Pool
	l.refuseDial(a, func() {
		dep := endpointOf(t, p, d.Addr)
		p.makeIneligible(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: dep.addr, Err: ErrPinMismatch})
	})
	p = newPool(t, Config{
		Base: baseFor(l.ca, l.d), Logger: l.logger, headerDeadline: 100 * time.Millisecond,
		Endpoints: []Endpoint{configured(a), configured(c.Addr), node(d.Addr, "pve3", true, PinOf(l.leaf.DER))},
	})
	resp, err := send(t, context.Background(), p, http.MethodGet, a, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, c.Addr)
	if n := d.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the ineligible endpoint got %d connections", n)
	}
}

// A refused ineligible entry does not count toward the hop cap, because it
// hands nothing to a transport. Three configured endpoints refuse, the first
// discovered endpoint fails its pin during the first dial, and the fourth hop
// the walk makes goes to the second discovered endpoint, which answers.
func TestARefusedIneligibleEntryDoesNotUseAHop(t *testing.T) {
	l := newLab(t)
	const a, b, c, d1 = "pve6.example.invalid:8006", "pve7.example.invalid:8006", "pve8.example.invalid:8006",
		"pve9.example.invalid:8006"
	d2 := l.live(t, nil)
	var p *Pool
	l.refuseDial(a, func() {
		dep := endpointOf(t, p, d1)
		p.makeIneligible(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: dep.addr, Err: ErrPinMismatch})
	})
	l.refuseDial(b, nil)
	l.refuseDial(c, nil)
	var d1Dials atomic.Int64
	l.d.set(d1, func(context.Context, string, string) (net.Conn, error) {
		d1Dials.Add(1)
		return nil, errors.New("the ineligible endpoint was dialed")
	})
	p = l.poolWith(t, nil, nil, nil, configured(a), configured(b), configured(c),
		node(d1, "pve1", true, Pin{1}), node(d2.Addr, "pve2", true, PinOf(l.leaf.DER)))
	order, _ := p.walkOrder(false)
	wantOrder(t, order, a, b, c, d1, d2.Addr)
	resp, err := send(t, context.Background(), p, http.MethodGet, a, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, d2.Addr)
	if n := d1Dials.Load(); n != 0 {
		t.Fatalf("the ineligible endpoint was dialed %d times", n)
	}
}

// otherLeaf starts a live server whose leaf the lab's CA signed for a name
// that is not the server's address, so neither the pins nor the hostname
// check accept it.
func (l *lab) otherLeaf(t *testing.T) *pvetest.Server {
	t.Helper()
	return pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
}

// lateSendBy wraps the TLS dials of o so a pin failure comes back as the
// dialer reports one that finished after the send-by bound passed, with the
// failure wrapped in ErrSendByPassed. The handshake and the pin check are the
// real ones. Only the moment the dialer reads the clock is moved, because a
// real bound passes in the instant between the handshake's end and that read.
func lateSendBy(o *ownedTransports) {
	for _, tr := range []*http.Transport{o.plain, o.idempotent} {
		dial := tr.DialTLSContext
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			if cpe, ok := AsConnectPhase(err); ok && cpe.Phase == PhasePin && !errors.Is(err, ErrSendByPassed) {
				return nil, &ConnectPhaseError{Phase: cpe.Phase, Endpoint: cpe.Endpoint,
					Err: fmt.Errorf("%w: %w", ErrSendByPassed, cpe.Err)}
			}
			return conn, err
		}
	}
}

// mustCountLatePin checks every effect a pin failure has on d, which a late
// one must have too, and that err is still the send-by refusal.
func mustCountLatePin(t *testing.T, l *lab, p *Pool, sink *markRecorder, err error, d string, want map[string]Phase) {
	t.Helper()
	if !errors.Is(err, ErrSendByPassed) {
		t.Fatalf("the request should report the send-by refusal, got %v", err)
	}
	dep := endpointOf(t, p, d)
	if !p.ineligibleNow(dep) || !p.RefreshDue() {
		t.Fatalf("ineligible %v, refresh due %v, want both", p.ineligibleNow(dep), p.RefreshDue())
	}
	mustDemoted(t, p, d, true)
	wantMarks(t, sink.all(), want)
	warns := l.entries(slog.LevelWarn, "pve endpoint "+d+": certificate is not one of the 1 pinned")
	if len(warns) != 1 {
		t.Fatalf("pin-mismatch lines = %+v, want one", warns)
	}
	if cause, _ := warns[0].Attrs["error"].(string); cause == "" || strings.Contains(cause, ErrSendByPassed.Error()) {
		t.Fatalf("the pin line's cause should be the certificate refusal alone, got %q", cause)
	}
	// The refusal is not counted twice when the endpoint is refused again.
	if _, ok := p.makeIneligible(dep, err); ok {
		t.Fatal("the late pin failure should already have made the endpoint ineligible")
	}
}

// A pin failure that finishes after the send-by bound still counts, on the
// walk and on a routed request. The request reports the send-by refusal, and
// the endpoint becomes ineligible and demoted, a refresh becomes due, a pin
// mark is written, and the Warn line names the certificate refusal.
func TestALatePinFailureStillCounts(t *testing.T) {
	sendByCtx := func() context.Context {
		return WithSendBy(context.Background(), time.Now().Add(time.Hour), http.MethodGet, "/version")
	}
	t.Run("on the walk", func(t *testing.T) {
		l := newLab(t)
		const c = "pve8.example.invalid:8006"
		l.refuseDial(c, nil)
		d := l.otherLeaf(t)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, nil, configured(c), node(d.Addr, "pve3", true, Pin{9}))
		lateSendBy(endpointOf(t, p, d.Addr).transports)
		resp, err := send(t, sendByCtx(), p, http.MethodGet, c, "/version", "")
		closeBody(resp)
		mustCountLatePin(t, l, p, sink, err, d.Addr, map[string]Phase{c: PhaseDial, d.Addr: PhasePin})
	})
	t.Run("on a routed request", func(t *testing.T) {
		l := newLab(t)
		c := l.live(t, nil)
		d := l.otherLeaf(t)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, Pin{9}))
		o, err := p.routedTransportsAt(d.Addr, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		lateSendBy(o)
		resp, err := send(t, sendByCtx(), p, http.MethodGet, d.Addr, "/version", "")
		closeBody(resp)
		mustCountLatePin(t, l, p, sink, err, d.Addr, map[string]Phase{d.Addr: PhasePin})
		mustActive(t, p, c.Addr)
	})
	t.Run("a late failure of another kind demotes nothing", func(t *testing.T) {
		l := newLab(t)
		c := l.live(t, nil)
		d := l.live(t, nil)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, PinOf(l.leaf.DER)))
		dep := endpointOf(t, p, d.Addr)
		p.pinFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhaseDial, Endpoint: d.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, errors.New("connection refused"))})
		p.pinFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: d.Addr, Err: ErrSendByPassed})
		p.pinFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhaseTLSHandshake, Endpoint: d.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, ErrPinMismatch)})
		p.pinFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: d.Addr, Err: ErrPinMismatch})
		p.pinFailureAfterSendBy(nil, refuseIneligible(d.Addr))
		cep := endpointOf(t, p, c.Addr)
		p.pinFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhasePin, Endpoint: c.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, ErrPinMismatch)})
		if p.ineligibleNow(dep) || p.ineligibleNow(cep) || p.RefreshDue() || len(sink.all()) != 0 {
			t.Fatalf("only a late pin failure of a discovered endpoint counts: ineligible %v %v, refresh %v, marks %+v",
				p.ineligibleNow(dep), p.ineligibleNow(cep), p.RefreshDue(), sink.all())
		}
		mustDemoted(t, p, d.Addr, false)
	})
}

// closeWatch records whether a connection was closed.
type closeWatch struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeWatch) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// A discovered endpoint that becomes ineligible has its idle connections
// closed, so no request reuses a connection whose certificate the pool now
// refuses.
func TestMakeIneligibleClosesIdleConnections(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := l.live(t, nil)
	var (
		mu    sync.Mutex
		conns []*closeWatch
	)
	l.d.set(d.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		w := &closeWatch{Conn: raw, closed: make(chan struct{})}
		mu.Lock()
		conns = append(conns, w)
		mu.Unlock()
		return w, nil
	})
	p := l.poolWith(t, nil, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, PinOf(l.leaf.DER)))
	mustOK(t, p, http.MethodGet, d.Addr, "/version", "")
	mu.Lock()
	opened := append([]*closeWatch(nil), conns...)
	mu.Unlock()
	if len(opened) != 1 {
		t.Fatalf("connections opened = %d, want 1", len(opened))
	}
	select {
	case <-opened[0].closed:
		t.Fatal("the idle connection closed before the endpoint became ineligible")
	default:
	}
	dep := endpointOf(t, p, d.Addr)
	if _, ok := p.makeIneligible(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: d.Addr, Err: ErrPinMismatch}); !ok {
		t.Fatal("the endpoint should become ineligible")
	}
	select {
	case <-opened[0].closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle connection to the ineligible endpoint was not closed")
	}
	mustDemoted(t, p, d.Addr, true)
}
