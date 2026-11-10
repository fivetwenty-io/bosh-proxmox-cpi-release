package endpointpool

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// markRecorder is a MarkSink that keeps every mark it is handed. onMark, when
// set before the pool sends anything, runs first on the caller's goroutine.
type markRecorder struct {
	mu     sync.Mutex
	marks  []Mark
	onMark func(Mark)
}

func (r *markRecorder) MarkDown(m Mark) {
	if r.onMark != nil {
		r.onMark(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks = append(r.marks, m)
}

func (r *markRecorder) all() []Mark {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Mark(nil), r.marks...)
}

// answerRecorder is a FirstAnswerHook that keeps the endpoint of every call.
type answerRecorder struct {
	mu        sync.Mutex
	endpoints []string
	onAnswer  func()
}

func (r *answerRecorder) FirstAnswer(endpoint string) {
	if r.onAnswer != nil {
		r.onAnswer()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = append(r.endpoints, endpoint)
}

func (r *answerRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.endpoints...)
}

// node returns a discovered endpoint for a named node.
func node(addr, name string, online bool, pins ...Pin) Endpoint {
	return Endpoint{Address: addr, Kind: KindDiscovered, Node: name, Online: online, Pins: pins}
}

// marked returns ep with a down mark written at at.
func marked(ep Endpoint, at time.Time) Endpoint {
	ep.DownMarkedAt = at
	return ep
}

// poolWith builds a pool over the lab's base and logger with the given hooks
// and clock, any of which may be nil.
func (l *lab) poolWith(t *testing.T, sink MarkSink, hook FirstAnswerHook, clock *fakeClock, eps ...Endpoint) *Pool {
	t.Helper()
	cfg := Config{Base: baseFor(l.ca, l.d), Logger: l.logger, Endpoints: eps, MarkSink: sink, FirstAnswer: hook}
	if clock != nil {
		cfg.clock = clock.now
	}
	return newPool(t, cfg)
}

// wantMarks fails the test unless marks holds exactly the endpoint and class
// pairs in want, in any order.
func wantMarks(t *testing.T, marks []Mark, want map[string]Phase) {
	t.Helper()
	got := make(map[string]Phase, len(marks))
	for _, m := range marks {
		if _, dup := got[m.Endpoint]; dup {
			t.Fatalf("endpoint %s was marked more than once: %+v", m.Endpoint, marks)
		}
		got[m.Endpoint] = m.Class
	}
	if len(got) != len(want) {
		t.Fatalf("marks = %+v, want %v", marks, want)
	}
	for addr, class := range want {
		if got[addr] != class {
			t.Fatalf("mark for %s = %q, want %q (all %+v)", addr, got[addr], class, marks)
		}
	}
}

// Discovered endpoints follow the configured ones, those whose node a quorate
// status reported online first, each group in node-name order, and endpoints
// with one node name keep the order they were listed in.
func TestWalkOrderPutsOnlineDiscoveredNodesFirstByName(t *testing.T) {
	l := newLab(t)
	p := l.poolWith(t, nil, nil, nil,
		node("z.example:8006", "pve9", true, Pin{1}),
		node("a.example:8006", "pve2", false, Pin{2}),
		configured("c1.example:8006"),
		node("b.example:8006", "pve3", true, Pin{3}),
		node("c.example:8006", "pve1", false, Pin{4}),
		node("dup1.example:8006", "pve5", false, Pin{5}),
		configured("c2.example:8006"),
		node("dup0.example:8006", "pve5", false, Pin{6}),
	)
	order, _ := p.walkOrder(false)
	wantOrder(t, order, "c1.example:8006", "c2.example:8006", "b.example:8006", "z.example:8006",
		"c.example:8006", "a.example:8006", "dup1.example:8006", "dup0.example:8006")
	order, _ = p.walkOrder(true)
	wantOrder(t, order, "c1.example:8006", "c2.example:8006")
	built := l.entries(slog.LevelDebug, "pve endpoints: pool [")
	if len(built) != 1 || !strings.Contains(built[0].Message, "c2.example:8006 configured, b.example:8006 discovered, "+
		"z.example:8006 discovered, c.example:8006 discovered") {
		t.Fatalf("the pool-built line should list the sorted order: %+v", built)
	}
}

// A down mark younger than two minutes moves its endpoint to the end of the
// walk, beside the demoted endpoints and in preference order. A mark exactly
// two minutes old has expired, a mark up to 30 seconds in the future counts,
// and one further ahead is ignored until the clock brings it within the
// allowance. Marks age with the clock, and the active endpoint stays where it
// is when a mark expires.
func TestWalkOrderMovesMarkedEndpointsToTheEnd(t *testing.T) {
	l := newLab(t)
	clock := newFakeClock()
	now := clock.now()
	p := l.poolWith(t, nil, nil, clock,
		marked(configured("c1:8006"), now.Add(-30*time.Second)),
		configured("c2:8006"),
		marked(configured("c3:8006"), now.Add(-2*time.Minute)),
		marked(configured("c4:8006"), now.Add(20*time.Second)),
		marked(configured("c5:8006"), now.Add(45*time.Second)),
		marked(node("d1:8006", "pve1", true, Pin{1}), now.Add(-10*time.Second)),
		node("d2:8006", "pve2", false, Pin{2}),
	)
	mustActive(t, p, "c2:8006")
	order, _ := p.walkOrder(false)
	wantOrder(t, order, "c2:8006", "c3:8006", "c5:8006", "d2:8006", "c1:8006", "c4:8006", "d1:8006")
	order, _ = p.walkOrder(true)
	wantOrder(t, order, "c2:8006", "c3:8006", "c5:8006", "c1:8006", "c4:8006")

	p.demote(endpointOf(t, p, "c3:8006"))
	order, _ = p.walkOrder(false)
	wantOrder(t, order, "c2:8006", "c5:8006", "d2:8006", "c1:8006", "c3:8006", "c4:8006", "d1:8006")

	// Two minutes later the marks on c1 and d1 have expired. The marks on c4
	// and c5 are now 100 and 75 seconds old, so both are in effect, c5's
	// because its time is no longer too far ahead.
	clock.advance(2 * time.Minute)
	order, _ = p.walkOrder(false)
	wantOrder(t, order, "c2:8006", "c1:8006", "d1:8006", "d2:8006", "c3:8006", "c4:8006", "c5:8006")
	mustActive(t, p, "c2:8006")
}

// The pool starts on the first endpoint of the walk order, so a marked first
// endpoint makes it start on the next unmarked one, and StartedAway says so.
func TestPoolStartsOnTheFirstUnmarkedEndpoint(t *testing.T) {
	clock := newFakeClock()
	now := clock.now()
	recent := now.Add(-40 * time.Second)
	cases := []struct {
		name  string
		eps   []Endpoint
		start string
		away  bool
	}{
		{"no marks", []Endpoint{configured("c1:8006"), configured("c2:8006")}, "c1:8006", false},
		{"first marked", []Endpoint{marked(configured("c1:8006"), recent), configured("c2:8006")}, "c2:8006", true},
		{"expired mark", []Endpoint{marked(configured("c1:8006"), now.Add(-3*time.Minute)), configured("c2:8006")},
			"c1:8006", false},
		{"every endpoint marked", []Endpoint{
			marked(configured("c1:8006"), recent), marked(configured("c2:8006"), recent),
		}, "c1:8006", false},
		{"every configured endpoint marked", []Endpoint{
			marked(configured("c1:8006"), recent), marked(configured("c2:8006"), recent),
			node("d1:8006", "pve3", true, Pin{3}),
		}, "d1:8006", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			p := l.poolWith(t, nil, nil, clock, c.eps...)
			mustActive(t, p, c.start)
			away, ok := p.StartedAway()
			if ok != c.away {
				t.Fatalf("StartedAway = %+v, %v, want %v", away, ok, c.away)
			}
			if ok && (away.First != "c1:8006" || away.Start != c.start || !away.MarkedAt.Equal(recent)) {
				t.Fatalf("StartedAway = %+v", away)
			}
			built := l.entries(slog.LevelDebug, "pve endpoints: pool [")
			if len(built) != 1 || !strings.HasSuffix(built[0].Message, "starting on "+c.start) {
				t.Fatalf("the pool-built line should name %s: %+v", c.start, built)
			}
		})
	}
}

// A mark never removes an endpoint, so a walk whose unmarked endpoints fail
// still reaches the marked one, and its answer makes it active.
func TestWalkStillReachesAMarkedEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.refused(t)
	p := l.poolWith(t, nil, nil, nil, marked(configured(a.Addr), time.Now().Add(-10*time.Second)), configured(b.Addr))
	mustActive(t, p, b.Addr)
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := a.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the marked endpoint got %d requests, want 1", n)
	}
	mustActive(t, p, a.Addr)
}

// A discovered endpoint that fails its pin gets no request, is left out of
// every later walk and every routed request, makes a refresh due, logs the
// pin-mismatch line once at Warn, and gets one mark with the class pin.
func TestPinFailureMakesADiscoveredEndpointIneligible(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
	sink := &markRecorder{}
	clock := newFakeClock()
	p := l.poolWith(t, sink, nil, clock, configured(c.Addr), node(d.Addr, "pve3", true, Pin{9}))
	dep := endpointOf(t, p, d.Addr)
	if _, ok := p.promote(dep, 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}

	resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := d.Counts.Requests.Load(); n != 0 {
		t.Fatalf("the endpoint that failed its pin got %d requests", n)
	}
	if !p.ineligibleNow(dep) || !p.RefreshDue() {
		t.Fatalf("ineligible %v, refresh due %v, want both", p.ineligibleNow(dep), p.RefreshDue())
	}
	mustActive(t, p, c.Addr)
	wantMarks(t, sink.all(), map[string]Phase{d.Addr: PhasePin})
	if m := sink.all()[0]; !m.At.Equal(clock.now()) {
		t.Fatalf("mark time = %v, want the pool clock's %v", m.At, clock.now())
	}

	warns := l.entries(slog.LevelWarn, "pve endpoint "+d.Addr+": ")
	if len(warns) != 1 {
		t.Fatalf("pin-mismatch lines = %d, want 1: %+v", len(warns), warns)
	}
	want := "pve endpoint " + d.Addr + ": certificate is not one of the 1 pinned for node pve3; " +
		"skipping it until discovery refreshes"
	if warns[0].Message != want {
		t.Fatalf("pin-mismatch line = %q, want %q", warns[0].Message, want)
	}
	mustAttrs(t, warns[0], map[string]string{"endpoint": d.Addr, "node": "pve3", "class": "pin"})
	if pins, _ := warns[0].Attrs["pins"].(int64); pins != 1 {
		t.Fatalf("pins attribute = %v, want 1", warns[0].Attrs["pins"])
	}
	if cause, _ := warns[0].Attrs["error"].(string); !strings.Contains(cause, "is not one of the 1 pinned") {
		t.Fatalf("error attribute = %q", cause)
	}

	order, _ := p.walkOrder(false)
	wantOrder(t, order, c.Addr)
	conns := d.Counts.Conns.Load()
	for range 3 {
		resp, err = send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
		mustStatus(t, resp, err, http.StatusOK)
	}
	resp, err = send(t, context.Background(), p, http.MethodGet, d.Addr, "/version", "")
	closeBody(resp)
	if !errors.Is(err, ErrPinMismatch) || !errors.Is(err, errIneligible) || !IsConnectPhase(err) || IsMaybeSent(err) {
		t.Fatalf("a routed request to the ineligible endpoint should be refused unsent, got %v", err)
	}
	if n := d.Counts.Conns.Load(); n != conns {
		t.Fatalf("the ineligible endpoint got %d more connections", n-conns)
	}
	if n := len(l.entries(slog.LevelWarn, "pve endpoint "+d.Addr+": ")); n != 1 {
		t.Fatalf("pin-mismatch lines = %d after later requests, want 1", n)
	}
	wantMarks(t, sink.all(), map[string]Phase{d.Addr: PhasePin})
}

// Concurrent walks that all start on a discovered endpoint with a bad
// certificate make it ineligible once, log one line, and write one mark, and
// none of them sends it a request.
func TestPinFailureUnderConcurrentWalksIsRecordedOnce(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
	sink := &markRecorder{}
	p := l.poolWith(t, sink, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, Pin{9}))
	if _, ok := p.promote(endpointOf(t, p, d.Addr), 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
			if err != nil {
				t.Error(err)
				return
			}
			closeBody(resp)
		})
	}
	close(start)
	wg.Wait()
	if n := d.Counts.Requests.Load(); n != 0 {
		t.Fatalf("the endpoint that failed its pin got %d requests", n)
	}
	if n := len(l.entries(slog.LevelWarn, "pve endpoint "+d.Addr+": ")); n != 1 {
		t.Fatalf("pin-mismatch lines = %d, want 1", n)
	}
	wantMarks(t, sink.all(), map[string]Phase{d.Addr: PhasePin})
}

// A pinned leaf whose chain fails under verify_ssl true was refused before its
// pins were checked, so the line says so with its cause, and an endpoint with
// no node name reads "its node".
func TestPinMismatchLineForARefusedChain(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	foreign := pvetest.NewCA(t).NewLeaf(t, pvetest.LeafOptions{})
	d := pvetest.NewLive(t, foreign, nil)
	p := l.poolWith(t, nil, nil, nil, configured(c.Addr), discovered(d.Addr, PinOf(foreign.DER), Pin{7}))
	if _, ok := p.promote(endpointOf(t, p, d.Addr), 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}
	resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	warns := l.entries(slog.LevelWarn, "pve endpoint "+d.Addr+": ")
	if len(warns) != 1 {
		t.Fatalf("pin-mismatch lines = %d, want 1", len(warns))
	}
	msg := warns[0].Message
	if !strings.HasPrefix(msg, "pve endpoint "+d.Addr+": certificate refused (") ||
		!strings.Contains(msg, "x509: certificate signed by unknown authority") ||
		!strings.HasSuffix(msg, "), so it was not checked against the 2 pinned for its node; "+
			"skipping it until discovery refreshes") {
		t.Fatalf("pin-mismatch line = %q", msg)
	}
	mustAttrs(t, warns[0], map[string]string{"endpoint": d.Addr, "node": "", "class": "pin"})
}

// The mark sink gets the class of each endpoint's first connect-phase failure,
// once per endpoint however often it fails, and a pool without a sink still
// works.
func TestMarkSinkGetsTheClassOfEachFirstFailure(t *testing.T) {
	cases := []struct {
		name  string
		class Phase
		setup func(t *testing.T, l *lab) Endpoint
	}{
		{"dial", PhaseDial, func(t *testing.T, l *lab) Endpoint { return configured(l.refused(t).Addr) }},
		{"dns", PhaseDNS, func(_ *testing.T, l *lab) Endpoint {
			const addr = "pve9.example.invalid:8006"
			l.d.set(addr, func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
					Err: "no such host", Name: "pve9.example.invalid", IsNotFound: true,
				}}
			})
			return configured(addr)
		}},
		{"tls_handshake", PhaseTLSHandshake, func(t *testing.T, _ *lab) Endpoint {
			return configured(pvetest.NewReset(t).Addr)
		}},
		{"tls_verify", PhaseTLSVerify, func(t *testing.T, l *lab) Endpoint {
			leaf := l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"other.example"}})
			return configured(pvetest.NewLive(t, leaf, nil).Addr)
		}},
		{"pin", PhasePin, func(t *testing.T, l *lab) Endpoint {
			leaf := l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve4.other.example"}})
			return node(pvetest.NewLive(t, leaf, nil).Addr, "pve4", true, Pin{4})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			y := l.refused(t)
			x := c.setup(t, l)
			sink := &markRecorder{}
			p := l.poolWith(t, sink, nil, nil, configured(y.Addr), x)
			for range 3 {
				resp, err := send(t, context.Background(), p, http.MethodGet, y.Addr, "/version", "")
				closeBody(resp)
				if !IsConnectPhase(err) {
					t.Fatalf("every endpoint fails in the connect phase, got %v", err)
				}
			}
			wantMarks(t, sink.all(), map[string]Phase{y.Addr: PhaseDial, x.Address: c.class})

			// The same failures through a pool without a sink.
			quiet := l.poolWith(t, nil, nil, nil, configured(y.Addr), x)
			resp, err := send(t, context.Background(), quiet, http.MethodGet, y.Addr, "/version", "")
			closeBody(resp)
			if !IsConnectPhase(err) {
				t.Fatalf("a pool without a sink fails the same way, got %v", err)
			}
		})
	}
}

// A routed request that fails in the connect phase on one of the pool's
// endpoints marks it, once.
func TestRoutedConnectFailureMarksThePoolEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.refused(t)
	sink := &markRecorder{}
	p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(b.Addr))
	for range 2 {
		resp, err := send(t, context.Background(), p, http.MethodGet, b.Addr, "/version", "")
		closeBody(resp)
		if !IsConnectPhase(err) {
			t.Fatalf("routed refused dial: %v", err)
		}
	}
	wantMarks(t, sink.all(), map[string]Phase{b.Addr: PhaseDial})
}

// Failures that are not connect-phase failures the endpoint caused write no
// mark: a header deadline, a drop after the connection, a status, a refused
// send-by deadline, and a caller that gave up during the dial.
func TestNoMarkForFailuresOutsideTheConnectPhase(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, l *lab, sink *markRecorder)
	}{
		{"header deadline", func(t *testing.T, l *lab, sink *markRecorder) {
			a, b := pvetest.NewHung(t, l.leaf), l.live(t, nil)
			p := newPool(t, Config{Base: baseFor(l.ca, l.d), Logger: l.logger, MarkSink: sink,
				Endpoints: []Endpoint{configured(a.Addr), configured(b.Addr)}, headerDeadline: 50 * time.Millisecond})
			resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
			mustStatus(t, resp, err, http.StatusOK)
			mustDemoted(t, p, a.Addr, true)
		}},
		{"drop after the connection", func(t *testing.T, l *lab, sink *markRecorder) {
			a, b := pvetest.NewBodyThenHijack(t, l.leaf), l.live(t, nil)
			p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(b.Addr))
			resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
			mustStatus(t, resp, err, http.StatusOK)
			mustDemoted(t, p, a.Addr, true)
		}},
		{"status", func(t *testing.T, l *lab, sink *markRecorder) {
			routes := pvetest.NewRoutes()
			routes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
			a, b := l.live(t, routes), l.live(t, nil)
			p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(b.Addr))
			resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
			mustStatus(t, resp, err, http.StatusOK)
			mustDemoted(t, p, a.Addr, true)
		}},
		{"send-by", func(t *testing.T, l *lab, sink *markRecorder) {
			a := l.refused(t)
			p := l.poolWith(t, sink, nil, nil, configured(a.Addr), configured(l.live(t, nil).Addr))
			ctx := WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodDelete, "/pools")
			resp, err := send(t, ctx, p, http.MethodDelete, a.Addr, "/pools", "")
			closeBody(resp)
			if !errors.Is(err, ErrSendByPassed) {
				t.Fatalf("want the send-by refusal, got %v", err)
			}
		}},
		{"caller gave up during the dial", func(t *testing.T, l *lab, sink *markRecorder) {
			// The dial to each address ends the caller's context and then
			// fails, so the failure arrives after the caller gave up. The first
			// address takes a walk, and the second a routed request.
			addrs := []string{"pve8.example.invalid:8006", "pve9.example.invalid:8006"}
			cancels := make(map[string]context.CancelFunc, len(addrs))
			for _, addr := range addrs {
				l.d.set(addr, func(context.Context, string, string) (net.Conn, error) {
					cancels[addr]()
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
				})
			}
			p := l.poolWith(t, sink, nil, nil, configured(addrs[0]), configured(addrs[1]),
				configured(l.live(t, nil).Addr))
			for _, addr := range addrs {
				ctx, cancel := context.WithCancel(context.Background())
				cancels[addr] = cancel
				resp, err := send(t, ctx, p, http.MethodGet, addr, "/version", "")
				closeBody(resp)
				cancel()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s: want the caller's cancellation, got %v", addr, err)
				}
				mustDemoted(t, p, addr, false)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			sink := &markRecorder{}
			c.run(t, l, sink)
			if marks := sink.all(); len(marks) != 0 {
				t.Fatalf("marks = %+v, want none", marks)
			}
		})
	}
}

// The pool calls the mark sink and the first-answer hook outside every lock it
// holds, so either may call back into the pool. Each hook first checks that
// none of the pool's locks is held, which only the request's own goroutine
// could hold here, and calls back into the pool only when none is, so a
// broken pool fails the test instead of deadlocking it.
func TestHooksAreCalledOutsideThePoolsLocks(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	r := l.refused(t)
	d := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve3.other.example"}}), nil)
	sink, hook := &markRecorder{}, &answerRecorder{}
	var p *Pool
	reenter := func(from string) {
		held := false
		for name, mu := range map[string]*sync.Mutex{
			"pool": &p.mu, "walk": &p.walk.mu, "tether": &p.tethers.mu, "scopeless": &scopelessTables.mu,
		} {
			if !mu.TryLock() {
				t.Errorf("the %s lock is held while the pool calls the %s", name, from)
				held = true
				continue
			}
			mu.Unlock()
		}
		if held {
			return
		}
		order, _ := p.walkOrder(false)
		_, _ = p.current()
		_ = p.ineligibleNow(order[0])
		_, _ = p.walk.noteFor(order[0])
		_ = p.RefreshDue()
	}
	sink.onMark = func(Mark) { reenter("mark sink") }
	hook.onAnswer = func() { reenter("first-answer hook") }
	p = l.poolWith(t, sink, hook, nil, configured(c.Addr), configured(r.Addr), node(d.Addr, "pve3", true, Pin{9}))
	if _, ok := p.promote(endpointOf(t, p, d.Addr), 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}

	// The walk fails the pin on d and is answered by c, and a routed request
	// is refused on r.
	resp, err := send(t, context.Background(), p, http.MethodPost, c.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	resp, err = send(t, context.Background(), p, http.MethodGet, r.Addr, "/version", "")
	closeBody(resp)
	if !IsConnectPhase(err) {
		t.Fatalf("routed refused dial: %v", err)
	}
	wantMarks(t, sink.all(), map[string]Phase{d.Addr: PhasePin, r.Addr: PhaseDial})
	if got := hook.all(); len(got) != 1 || got[0] != c.Addr {
		t.Fatalf("first answers = %v, want [%s]", got, c.Addr)
	}
}

// A pin failure on an endpoint whose first failure had another class writes a
// second mark with the class pin, because the pin class is what makes other
// processes refresh. No third mark follows.
func TestPinFailureAfterAnotherFailureWritesAPinMark(t *testing.T) {
	l := newLab(t)
	sink := &markRecorder{}
	p := l.poolWith(t, sink, nil, nil, configured("c1:8006"), node("d1:8006", "pve1", true, Pin{1}))
	d1 := endpointOf(t, p, "d1:8006")
	dial := &ConnectPhaseError{Phase: PhaseDial, Endpoint: d1.addr, Err: errors.New("connection refused")}
	pin := &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch}
	p.connectFailed(d1, PhaseDial, dial)
	p.connectFailed(d1, PhaseDial, dial)
	p.connectFailed(d1, PhasePin, pin)
	p.connectFailed(d1, PhasePin, pin)
	marks := sink.all()
	if len(marks) != 2 || marks[0].Class != PhaseDial || marks[1].Class != PhasePin {
		t.Fatalf("marks = %+v, want a dial mark and then a pin mark", marks)
	}
	if !p.ineligibleNow(d1) {
		t.Fatal("the pin failure should make the endpoint ineligible")
	}
	// A configured endpoint never becomes ineligible, whatever its phase.
	c1 := endpointOf(t, p, "c1:8006")
	p.connectFailed(c1, PhasePin, pin)
	if p.ineligibleNow(c1) {
		t.Fatal("a configured endpoint must stay eligible")
	}
}

// The first-answer hook fires once, after the first request a configured
// endpoint answers. An answer from a discovered endpoint, a 503 from a
// configured one, and an answer from a routed host outside the pool do not
// fire it.
func TestFirstAnswerFiresOnceForAConfiguredAnswer(t *testing.T) {
	l := newLab(t)
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/cluster/status", http.StatusServiceUnavailable, "busy")
	c := l.live(t, routes)
	d := l.live(t, nil)
	outside := l.live(t, nil)
	hook := &answerRecorder{}
	p := l.poolWith(t, nil, hook, nil, configured(c.Addr), node(d.Addr, "pve2", true, PinOf(l.leaf.DER)))
	if _, ok := p.promote(endpointOf(t, p, d.Addr), 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}

	resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := d.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the discovered endpoint should have answered, got %d requests", n)
	}
	resp, err = send(t, WithConfiguredOnly(context.Background()), p, http.MethodGet, c.Addr, "/cluster/status", "")
	mustStatus(t, resp, err, http.StatusServiceUnavailable)
	resp, err = send(t, context.Background(), p, http.MethodGet, outside.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if got := hook.all(); len(got) != 0 {
		t.Fatalf("first answers = %v before any configured answer", got)
	}

	for range 3 {
		resp, err = send(t, context.Background(), p, http.MethodPost, c.Addr, "/access/ticket",
			"username=root&password=secret")
		mustStatus(t, resp, err, http.StatusOK)
	}
	if got := hook.all(); len(got) != 1 || got[0] != c.Addr {
		t.Fatalf("first answers = %v, want [%s]", got, c.Addr)
	}

	// An answered mutation counts too, and a pool without a hook is fine.
	hook2 := &answerRecorder{}
	p2 := l.poolWith(t, nil, hook2, nil, configured(c.Addr))
	resp, err = send(t, context.Background(), p2, http.MethodPost, c.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	if got := hook2.all(); len(got) != 1 || got[0] != c.Addr {
		t.Fatalf("first answers = %v, want [%s]", got, c.Addr)
	}
	quiet := l.poolWith(t, nil, nil, nil, configured(c.Addr))
	resp, err = send(t, context.Background(), quiet, http.MethodGet, c.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
}

// isAnswerStatus follows the outcome table's answer row.
func TestIsAnswerStatus(t *testing.T) {
	for status, want := range map[int]bool{
		200: true, 204: true, 401: true, 404: true, 500: true, 504: true,
		502: false, 503: false, 595: false, 596: false, 597: false, 599: false,
	} {
		if got := isAnswerStatus(status); got != want {
			t.Errorf("isAnswerStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

// The ticket login and a request with the configured-only marker never reach
// a discovered endpoint, even when every configured endpoint is marked and
// refuses and the pool started on the discovered endpoint, while a normal
// request still reaches it.
func TestLoginAndDiscoveryNeverReachADiscoveredEndpoint(t *testing.T) {
	l := newLab(t)
	c1, c2 := l.refused(t), l.refused(t)
	d := l.live(t, nil)
	recent := time.Now().Add(-20 * time.Second)
	p := l.poolWith(t, nil, nil, nil,
		marked(configured(c1.Addr), recent), marked(configured(c2.Addr), recent),
		node(d.Addr, "pve3", true, PinOf(l.leaf.DER)))
	mustActive(t, p, d.Addr)

	resp, err := send(t, context.Background(), p, http.MethodPost, c1.Addr, "/access/ticket",
		"username=root&password=secret")
	closeBody(resp)
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || strings.Join(eu.Endpoints(), " ") != c1.Addr+" "+c2.Addr {
		t.Fatalf("the login should walk only the configured endpoints, got %v", err)
	}
	resp, err = send(t, WithConfiguredOnly(context.Background()), p, http.MethodGet, c1.Addr, "/cluster/status", "")
	closeBody(resp)
	if !errors.As(err, &eu) || strings.Join(eu.Endpoints(), " ") != c1.Addr+" "+c2.Addr {
		t.Fatalf("a configured-only request should walk only the configured endpoints, got %v", err)
	}
	if n := d.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the discovered endpoint got %d connections", n)
	}

	resp, err = send(t, context.Background(), p, http.MethodGet, c1.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	resp, err = send(t, context.Background(), p, http.MethodPost, c1.Addr, "/access/ticket",
		"username=root&password=secret")
	closeBody(resp)
	if err == nil {
		t.Fatal("the login must fail while every configured endpoint refuses")
	}
	for _, seen := range d.Seen() {
		if seen.Path != pvetest.APIPrefix+"/version" {
			t.Fatalf("the discovered endpoint saw %s %s", seen.Method, seen.Path)
		}
	}
}

// A mutation whose tether names a discovered endpoint that later failed its
// pin is refused before any dial, and it never moves to another endpoint.
func TestTetherToAnIneligibleEndpointIsRefused(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := l.live(t, nil)
	p := l.poolWith(t, nil, nil, nil, configured(c.Addr), node(d.Addr, "pve3", true, PinOf(l.leaf.DER)))
	dep := endpointOf(t, p, d.Addr)

	first := apiRequest(t, context.Background(), http.MethodPost, c.Addr, "/nodes/pve3/qemu", "vmid=101")
	h := newTetherHold(first)
	if err := p.claim(h, targetOf(dep)); err != nil {
		t.Fatal(err)
	}
	p.fix(h, dep.addr)
	if !p.makeIneligible(dep, &ConnectPhaseError{Phase: PhasePin, Endpoint: dep.addr, Err: ErrPinMismatch}) {
		t.Fatal("the endpoint should become ineligible")
	}
	if p.makeIneligible(dep, nil) {
		t.Fatal("an endpoint becomes ineligible once")
	}

	resp, err := send(t, context.Background(), p, http.MethodPost, c.Addr, "/nodes/pve3/qemu", "vmid=101")
	closeBody(resp)
	if !errors.Is(err, errIneligible) || !IsConnectPhase(err) || IsMaybeSent(err) {
		t.Fatalf("the tethered mutation should be refused unsent, got %v", err)
	}
	if n := d.Counts.Conns.Load() + c.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("the refused mutation reached a server (%d)", n)
	}
	// The refusal hands nothing to a transport, so the walker's own outcome
	// is a proven-unsent pin hop with no demotion of its own.
	w := p.newWalker(apiRequest(t, context.Background(), http.MethodGet, c.Addr, "/version", ""), false,
		defaultBounds())
	hop := w.try(0, dep, nil)
	if hop.oc.sent != sentNever || hop.oc.demote || hop.oc.class != string(PhasePin) || hop.oc.action != actHop {
		t.Fatalf("ineligible hop outcome = %+v", hop.oc)
	}
	if blamesEndpoint(hop.err) {
		t.Fatal("the refusal must not blame the endpoint again")
	}
}

// advance and promote never make an ineligible endpoint active, and advance
// skips a marked endpoint when an unmarked one is left.
func TestAdvanceAndPromoteSkipIneligibleAndMarkedEndpoints(t *testing.T) {
	l := newLab(t)
	clock := newFakeClock()
	p := l.poolWith(t, nil, nil, clock,
		configured("c1:8006"), marked(configured("c2:8006"), clock.now().Add(-time.Minute)), configured("c3:8006"),
		node("d1:8006", "pve1", true, Pin{1}))
	c1, d1 := endpointOf(t, p, "c1:8006"), endpointOf(t, p, "d1:8006")
	if to := p.advance(c1, 0); to == nil || to.addr != "c3:8006" {
		t.Fatalf("advance should skip the marked c2 and install c3, got %v", to)
	}
	_, gen := p.current()
	p.makeIneligible(d1, &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch})
	if _, ok := p.promote(d1, gen); ok {
		t.Fatal("a success on an ineligible endpoint must not apply")
	}
	mustActive(t, p, "c3:8006")
	// Every endpoint left is at the back, so advance takes the first of them
	// in preference order and never the ineligible d1.
	if to := p.advance(endpointOf(t, p, "c3:8006"), gen); to == nil || to.addr != "c1:8006" {
		t.Fatalf("with c1 and c3 demoted, c2 marked, and d1 ineligible, advance should install c1, got %v", to)
	}
	order, _ := p.walkOrder(false)
	wantOrder(t, order, "c1:8006", "c2:8006", "c3:8006")
}

// New refuses a configured endpoint marked online.
func TestNewRefusesAnOnlineConfiguredEndpoint(t *testing.T) {
	_, err := New(Config{
		Base:      baseFor(nil, nil),
		Endpoints: []Endpoint{{Address: "pve1:8006", Kind: KindConfigured, Online: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "is marked online") {
		t.Fatalf("New should refuse an online configured endpoint, got %v", err)
	}
}

// markInEffect follows the two-minute lifetime and the 30-second allowance.
func TestMarkInEffect(t *testing.T) {
	now := newFakeClock().now()
	for name, c := range map[string]struct {
		at   time.Time
		want bool
	}{
		"none":                 {time.Time{}, false},
		"just written":         {now, true},
		"one second short":     {now.Add(-markLifetime + time.Second), true},
		"exactly two minutes":  {now.Add(-markLifetime), false},
		"30 seconds ahead":     {now.Add(markFutureAllowance), true},
		"31 seconds ahead":     {now.Add(markFutureAllowance + time.Second), false},
		"an hour old":          {now.Add(-time.Hour), false},
		"a day in the future":  {now.Add(24 * time.Hour), false},
		"one nanosecond ahead": {now.Add(time.Nanosecond), true},
	} {
		if got := markInEffect(c.at, now); got != c.want {
			t.Errorf("%s: markInEffect = %v, want %v", name, got, c.want)
		}
	}
}
