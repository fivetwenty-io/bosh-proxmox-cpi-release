package endpointpool

import (
	"context"
	"crypto/tls"
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

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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

// lateSendBy wraps the TLS dials of o so a pin failure or a verification
// failure comes back as the dialer reports one that finished after the
// send-by bound passed, with the failure wrapped in ErrSendByPassed. The
// handshake and the certificate checks are the real ones. Only the moment the
// dialer reads the clock is moved, because a real bound passes in the instant
// between the handshake's end and that read.
func lateSendBy(o *ownedTransports) {
	for _, tr := range []*http.Transport{o.plain, o.idempotent} {
		dial := tr.DialTLSContext
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			cpe, ok := AsConnectPhase(err)
			if ok && (cpe.Phase == PhasePin || cpe.Phase == PhaseTLSVerify) && !errors.Is(err, ErrSendByPassed) {
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
	if note, ok := p.walk.noteFor(dep); !ok || note.class != string(PhasePin) ||
		strings.Contains(note.text, ErrSendByPassed.Error()) {
		t.Fatalf("failure note = %+v, %v, want the certificate refusal with class pin", note, ok)
	}
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
		p.certFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhaseDial, Endpoint: d.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, errors.New("connection refused"))})
		p.certFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: d.Addr, Err: ErrSendByPassed})
		p.certFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhaseTLSHandshake, Endpoint: d.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, ErrPinMismatch)})
		p.certFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: d.Addr, Err: ErrPinMismatch})
		p.certFailureAfterSendBy(dep, refuseIneligible(d.Addr))
		cep := endpointOf(t, p, c.Addr)
		p.certFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhasePin, Endpoint: c.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, ErrPinMismatch)})
		refused := &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}
		p.certFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: c.Addr, Err: ErrSendByPassed})
		p.certFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: c.Addr, Err: refused})
		p.certFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhaseTLSHandshake, Endpoint: c.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, refused)})
		p.certFailureAfterSendBy(cep, &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: c.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, errWrongAddress)})
		p.certFailureAfterSendBy(dep, &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: d.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, refused)})
		p.certFailureAfterSendBy(nil, &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: c.Addr,
			Err: fmt.Errorf("%w: %w", ErrSendByPassed, refused)})
		if p.ineligibleNow(dep) || p.ineligibleNow(cep) || p.RefreshDue() || len(sink.all()) != 0 {
			t.Fatalf("only a late certificate refusal in the endpoint's own refusal phase counts: ineligible %v %v, "+
				"refresh %v, marks %+v", p.ineligibleNow(dep), p.ineligibleNow(cep), p.RefreshDue(), sink.all())
		}
		mustDemoted(t, p, d.Addr, false)
		mustDemoted(t, p, c.Addr, false)
		if lines := l.entries(slog.LevelError, "pve endpoint "); len(lines) != 0 {
			t.Fatalf("error lines = %+v, want none", lines)
		}
	})
}

// A late pin failure leaves the failure note a normal one leaves, so a later
// failover line names the certificate refusal with the class pin instead of an
// earlier failure it cannot name.
func TestALatePinFailureLeavesTheSameFailoverLine(t *testing.T) {
	lines := make(map[bool]log.Entry, 2)
	for _, late := range []bool{false, true} {
		l := newLab(t)
		c := l.live(t, nil)
		d := l.otherLeaf(t)
		p := l.poolWith(t, nil, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, Pin{9}))
		dep := endpointOf(t, p, d.Addr)
		if _, ok := p.promote(dep, 0); !ok {
			t.Fatal("the discovered endpoint should become active")
		}
		o, err := p.routedTransportsAt(d.Addr, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if late {
			lateSendBy(o)
		}
		ctx := WithSendBy(context.Background(), time.Now().Add(time.Hour), http.MethodGet, "/version")
		resp, err := send(t, ctx, p, http.MethodGet, d.Addr, "/version", "")
		closeBody(resp)
		if err == nil || !p.ineligibleNow(dep) {
			t.Fatalf("late %v: the routed read should fail and make the endpoint ineligible, got %v", late, err)
		}
		resp, err = send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
		mustStatus(t, resp, err, http.StatusOK)
		found := l.entries(slog.LevelWarn, "pve endpoint failover: "+d.Addr+" unreachable")
		if len(found) != 1 {
			t.Fatalf("late %v: failover lines = %+v, want one", late, found)
		}
		lines[late] = found[0]
	}
	for late, e := range lines {
		if class, _ := e.Attrs["class"].(string); class != string(PhasePin) {
			t.Errorf("late %v: failover class = %q, want pin (line %q)", late, class, e.Message)
		}
		if !strings.Contains(e.Message, "certificate refused for this discovered endpoint") &&
			!strings.Contains(e.Message, "failed to verify certificate") {
			t.Errorf("late %v: the failover line should name the certificate refusal, got %q", late, e.Message)
		}
		if strings.Contains(e.Message, "an earlier failure in this call") {
			t.Errorf("late %v: the failover line names no cause: %q", late, e.Message)
		}
	}
}

// The final hop is decided from the hops the walk made, not from the position
// in the order. Two discovered entries turn ineligible during the first dial,
// so the slow third discovered endpoint is the second hop made, with one
// eligible entry after it, and it keeps its header deadline. The fourth
// discovered endpoint then answers.
func TestFinalHopCountsTheHopsMade(t *testing.T) {
	l := newLab(t)
	const a, d1, d2 = "pve6.example.invalid:8006", "pve1.example.invalid:8006", "pve2.example.invalid:8006"
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/version", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":{"version":"9.0.2"}}`))
	})
	d3 := l.live(t, routes)
	d4 := l.live(t, nil)
	var p *Pool
	l.refuseDial(a, func() {
		for _, addr := range []string{d1, d2} {
			dep := endpointOf(t, p, addr)
			p.makeIneligible(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: dep.addr, Err: ErrPinMismatch})
		}
	})
	p = newPool(t, Config{
		Base: baseFor(l.ca, l.d), Logger: l.logger, headerDeadline: 100 * time.Millisecond,
		Endpoints: []Endpoint{configured(a), node(d1, "pve1", true, Pin{1}), node(d2, "pve2", true, Pin{2}),
			node(d3.Addr, "pve3", true, PinOf(l.leaf.DER)), node(d4.Addr, "pve4", true, PinOf(l.leaf.DER))},
	})
	order, _ := p.walkOrder(false)
	wantOrder(t, order, a, d1, d2, d3.Addr, d4.Addr)
	resp, err := send(t, context.Background(), p, http.MethodGet, a, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, d4.Addr)
}

// The refusal of a hop and the count of eligible entries its deadline is
// decided from come from one lock acquisition, so the two never disagree about
// the hop's own endpoint. While another goroutine fails and restores the
// endpoint's pin, a reader that finds it eligible counts it, and one that finds
// it ineligible does not. Run under -race this also checks that the flag is
// only touched under Pool.mu.
func TestEligibilityAndCountAreReadTogether(t *testing.T) {
	l := newLab(t)
	p := l.poolWith(t, nil, nil, nil, configured("c1:8006"),
		node("d1:8006", "pve1", true, Pin{1}), node("d2:8006", "pve2", true, Pin{2}))
	d1, d2 := endpointOf(t, p, "d1:8006"), endpointOf(t, p, "d2:8006")
	order := []*endpoint{d1, d2}
	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			d1.ineligible = !d1.ineligible
			p.mu.Unlock()
		}
	})
	var (
		readers sync.WaitGroup
		bad     atomic.Int64
	)
	for range 4 {
		readers.Go(func() {
			for range 20000 {
				eligible, n := p.eligibleFrom(d1, order)
				if eligible != (n == 2) {
					bad.Add(1)
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	flipper.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d reads found the endpoint's eligibility and the count of eligible entries disagreeing", n)
	}
	if first, n := p.eligibleFrom(nil, nil); !first || n != 0 {
		t.Fatalf("eligibleFrom(nil, nil) = %v, %d, want true, 0", first, n)
	}
}

// A hop reads the refusal check and the count of eligible entries after it
// together. While another goroutine fails and restores the hop endpoint's pin,
// every hop that is not refused has a configured endpoint after it, so none of
// them is the final hop and none may take the clone without the header
// deadline.
func TestAHopNeverLosesItsDeadlineToAPinFailureOfItsOwnEndpoint(t *testing.T) {
	l := newLab(t)
	const d, c = "d1.example.invalid:8006", "c1.example.invalid:8006"
	l.refuseDial(d, nil)
	p := l.poolWith(t, nil, nil, nil, configured(c), node(d, "pve1", true, Pin{1}))
	dep, cep := endpointOf(t, p, d), endpointOf(t, p, c)
	var plainDials atomic.Int64
	plain := dep.transports.plain
	dial := plain.DialTLSContext
	plain.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		plainDials.Add(1)
		return dial(ctx, network, addr)
	}
	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			dep.ineligible = !dep.ineligible
			p.mu.Unlock()
		}
	})
	var (
		hops, refused int
		req           = apiRequest(t, context.Background(), http.MethodGet, c, "/version", "")
	)
	for range 20000 {
		w := p.newWalker(req, false, defaultBounds())
		w.order = []*endpoint{dep, cep}
		hop := w.try(0, dep, nil)
		if errors.Is(hop.err, errIneligible) {
			refused++
		} else {
			hops++
		}
	}
	close(stop)
	flipper.Wait()
	if hops == 0 || refused == 0 {
		t.Fatalf("hops %d, refused %d, want some of each so both outcomes are checked", hops, refused)
	}
	if n := plainDials.Load(); n != 0 {
		t.Fatalf("%d hops with an eligible endpoint after them went out without the header deadline", n)
	}
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
