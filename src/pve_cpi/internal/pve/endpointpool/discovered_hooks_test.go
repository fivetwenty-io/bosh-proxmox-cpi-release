package endpointpool

import (
	"context"
	"crypto/tls"
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
)

// panickingSink is a MarkSink whose every call panics after it counts the
// call.
type panickingSink struct {
	mu    sync.Mutex
	calls []Mark
}

func (s *panickingSink) MarkDown(m Mark) {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	s.mu.Unlock()
	panic("sink defect for " + m.Endpoint)
}

func (s *panickingSink) all() []Mark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mark(nil), s.calls...)
}

// panickingHook is a FirstAnswerHook whose every call panics after it counts
// the call.
type panickingHook struct {
	mu    sync.Mutex
	calls []string
}

func (h *panickingHook) FirstAnswer(endpoint string) {
	h.mu.Lock()
	h.calls = append(h.calls, endpoint)
	h.mu.Unlock()
	panic("hook defect")
}

func (h *panickingHook) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// refuseDial routes addr in the lab to a dial that fails as a refused TCP
// connection does, after it runs before when before is not nil.
func (l *lab) refuseDial(addr string, before func()) {
	l.d.set(addr, func(context.Context, string, string) (net.Conn, error) {
		if before != nil {
			before()
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	})
}

// hookRun is what a short sequence of requests leaves behind, for comparing
// a pool whose sink and hook panic with one that has neither.
type hookRun struct {
	readStatus, mutationStatus int
	active                     string
	demotedA, demotedC         bool
	noteA                      string
	tetheredTo                 string
	tetherFixed                bool
	routedPhase                Phase
	routedMaybeSent            bool
}

// runHookSequence sends a read on the walk, which fails to dial a and is
// answered by b, then a mutation, which goes to b and tethers there, and
// then a read routed to c, which fails to dial.
func runHookSequence(t *testing.T, p *Pool, a, c string) hookRun {
	t.Helper()
	var r hookRun
	resp, err := send(t, context.Background(), p, http.MethodGet, a, "/version", "")
	if err != nil {
		t.Fatalf("walked read: %v", err)
	}
	r.readStatus = resp.StatusCode
	closeBody(resp)
	req := apiRequest(t, context.Background(), http.MethodPost, a, "/nodes/pve1/qemu", "vmid=101")
	resp, err = p.RoundTrip(req)
	if err != nil {
		t.Fatalf("mutation: %v", err)
	}
	r.mutationStatus = resp.StatusCode
	closeBody(resp)
	p.tethers.mu.Lock()
	if tethers := p.tethers.byKey[tetherKeyOf(req)]; len(tethers) == 1 {
		r.tetheredTo, r.tetherFixed = tethers[0].target.addr, tethers[0].fixed
	}
	p.tethers.mu.Unlock()
	resp, err = send(t, context.Background(), p, http.MethodGet, c, "/version", "")
	closeBody(resp)
	if cpe, ok := AsConnectPhase(err); ok && cpe.Endpoint == c {
		r.routedPhase = cpe.Phase
	}
	r.routedMaybeSent = IsMaybeSent(err)
	active, _ := p.current()
	r.active = active.addr
	r.demotedA = p.demotedNow(endpointOf(t, p, a))
	r.demotedC = p.demotedNow(endpointOf(t, p, c))
	if note, ok := p.walk.noteFor(endpointOf(t, p, a)); ok {
		r.noteA = note.class
	}
	return r
}

// A sink and a hook that panic cost their own work and nothing else. The
// answers, the routed error, the demotions and failure note, the mutation's
// tether, and the active endpoint are the same as in a pool without them. The
// first panic of each is logged once at Warn, and a later panic of the sink
// at Debug.
func TestPanickingSinkAndHookLeaveTheRequestUnchanged(t *testing.T) {
	l := newLab(t)
	const a, c = "pve8.example.invalid:8006", "pve9.example.invalid:8006"
	l.refuseDial(a, nil)
	l.refuseDial(c, nil)
	b := l.live(t, nil)
	sink, hook := &panickingSink{}, &panickingHook{}
	got := runHookSequence(t, l.poolWith(t, sink, hook, nil, configured(a), configured(b.Addr), configured(c)), a, c)
	want := hookRun{
		readStatus: http.StatusOK, mutationStatus: http.StatusOK, active: b.Addr,
		demotedA: true, demotedC: true, noteA: string(PhaseDial),
		tetheredTo: b.Addr, tetherFixed: true, routedPhase: PhaseDial,
	}
	if got != want {
		t.Fatalf("with a panicking sink and hook:\n got %+v\nwant %+v", got, want)
	}
	quiet := runHookSequence(t, l.poolWith(t, nil, nil, nil, configured(a), configured(b.Addr), configured(c)), a, c)
	if quiet != want {
		t.Fatalf("without a sink or hook:\n got %+v\nwant %+v", quiet, want)
	}

	if got := sink.all(); len(got) != 2 || got[0].Endpoint != a || got[1].Endpoint != c {
		t.Fatalf("sink calls = %+v, want one for %s and one for %s", got, a, c)
	}
	if got := hook.all(); len(got) != 1 || got[0] != b.Addr {
		t.Fatalf("hook calls = %v, want [%s]", got, b.Addr)
	}
	sinkWarns := l.entries(slog.LevelWarn, "pve endpoint "+a+": the down-mark sink panicked")
	if len(sinkWarns) != 1 {
		t.Fatalf("sink panic lines at Warn = %+v, want one for %s", sinkWarns, a)
	}
	mustAttrs(t, sinkWarns[0], map[string]string{"endpoint": a, "class": "dial", "panic": "sink defect for " + a})
	if n := len(l.entries(slog.LevelWarn, "pve endpoint "+c+": the down-mark sink panicked")); n != 0 {
		t.Fatalf("a second sink panic should not log at Warn, got %d lines", n)
	}
	if n := len(l.entries(slog.LevelDebug, "pve endpoint "+c+": the down-mark sink panicked")); n != 1 {
		t.Fatalf("the second sink panic should log once at Debug, got %d lines", n)
	}
	hookWarns := l.entries(slog.LevelWarn, "pve endpoint "+b.Addr+": the first-answer hook panicked")
	if len(hookWarns) != 1 {
		t.Fatalf("hook panic lines at Warn = %+v, want one", hookWarns)
	}
	mustAttrs(t, hookWarns[0], map[string]string{"endpoint": b.Addr, "panic": "hook defect"})
	if _, has := hookWarns[0].Attrs["class"]; has {
		t.Fatalf("the hook line carries no class: %+v", hookWarns[0].Attrs)
	}
}

// mergeNewest applies marks in the order they arrived the way the cache's
// merge does, keeping a mark that is as new as or newer than the one stored
// for its endpoint, and returns the class left for each endpoint.
func mergeNewest(marks []Mark) map[string]Phase {
	kept := make(map[string]Mark, len(marks))
	for _, m := range marks {
		if prior, seen := kept[m.Endpoint]; !seen || !m.At.Before(prior.At) {
			kept[m.Endpoint] = m
		}
	}
	out := make(map[string]Phase, len(kept))
	for addr, m := range kept {
		out[addr] = m.Class
	}
	return out
}

// A dial mark never overwrites a pin mark for the same endpoint. When the
// dial mark is decided first but reaches the sink after the pin mark, it is
// dated before the pin mark, so a merge that keeps the newest mark keeps the
// pin mark, even on a clock that does not move. A dial failure that comes
// after the pin failure gets no mark at all.
func TestADialMarkNeverOverwritesAPinMark(t *testing.T) {
	t.Run("dial decided first and delivered last", func(t *testing.T) {
		l := newLab(t)
		release, entered := make(chan struct{}), make(chan struct{})
		sink := &markRecorder{}
		sink.onMark = func(m Mark) {
			if m.Class == PhaseDial {
				close(entered)
				<-release
			}
		}
		clock := newFakeClock()
		p := l.poolWith(t, sink, nil, clock, configured("c1:8006"), node("d1:8006", "pve1", true, Pin{1}))
		d1 := endpointOf(t, p, "d1:8006")
		var wg sync.WaitGroup
		wg.Go(func() {
			p.connectFailed(d1, PhaseDial, &ConnectPhaseError{Phase: PhaseDial, Endpoint: d1.addr,
				Err: errors.New("connection refused")})
		})
		<-entered
		p.connectFailed(d1, PhasePin, &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch})
		close(release)
		wg.Wait()
		marks := sink.all()
		if len(marks) != 2 || marks[0].Class != PhasePin || marks[1].Class != PhaseDial {
			t.Fatalf("marks = %+v, want the pin mark delivered before the held dial mark", marks)
		}
		if !marks[0].At.After(marks[1].At) || !marks[1].At.Equal(clock.now()) {
			t.Fatalf("the pin mark must be dated after the dial mark: %+v", marks)
		}
		if got := mergeNewest(marks); got[d1.addr] != PhasePin {
			t.Fatalf("a newest-wins merge kept %q, want pin", got[d1.addr])
		}
	})
	t.Run("dial after pin", func(t *testing.T) {
		l := newLab(t)
		sink := &markRecorder{}
		p := l.poolWith(t, sink, nil, newFakeClock(), configured("c1:8006"), node("d1:8006", "pve1", true, Pin{1}))
		d1 := endpointOf(t, p, "d1:8006")
		p.connectFailed(d1, PhasePin, &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch})
		p.connectFailed(d1, PhaseDial, &ConnectPhaseError{Phase: PhaseDial, Endpoint: d1.addr,
			Err: errors.New("connection refused")})
		wantMarks(t, sink.all(), map[string]Phase{d1.addr: PhasePin})
	})
	t.Run("concurrent failures", func(t *testing.T) {
		// Many goroutines fail the same endpoints with dial and pin failures
		// at once. Each endpoint gets at most one mark of each class, and a
		// newest-wins merge of the marks in the order they arrived keeps the
		// pin mark for every endpoint.
		l := newLab(t)
		sink := &markRecorder{}
		const endpoints = 8
		eps := make([]Endpoint, 0, 1+endpoints)
		eps = append(eps, configured("c1:8006"))
		for i := range endpoints {
			eps = append(eps, node("d"+string(rune('a'+i))+":8006", "pve"+string(rune('a'+i)), true, Pin{byte(i + 1)}))
		}
		p := l.poolWith(t, sink, nil, newFakeClock(), eps...)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, in := range eps[1:] {
			ep := endpointOf(t, p, in.Address)
			for g := range 8 {
				wg.Go(func() {
					<-start
					phase, cause := PhaseDial, errors.New("connection refused")
					if g%2 == 1 {
						phase, cause = PhasePin, ErrPinMismatch
					}
					p.connectFailed(ep, phase, &ConnectPhaseError{Phase: phase, Endpoint: ep.addr, Err: cause})
				})
			}
		}
		close(start)
		wg.Wait()
		marks := sink.all()
		perClass := make(map[string]int)
		for _, m := range marks {
			perClass[m.Endpoint+" "+string(m.Class)]++
		}
		for key, n := range perClass {
			if n != 1 {
				t.Fatalf("%s was marked %d times: %+v", key, n, marks)
			}
		}
		merged := mergeNewest(marks)
		if len(merged) != endpoints {
			t.Fatalf("merged marks = %v, want one per discovered endpoint", merged)
		}
		for addr, class := range merged {
			if class != PhasePin {
				t.Fatalf("a newest-wins merge kept %q for %s, want pin (marks %+v)", class, addr, marks)
			}
		}
	})
}

// A mark more than 30 seconds ahead of the pool's clock when the pool is
// built is dropped, so it never takes effect, even after the clock reaches its
// time. A mark exactly 30 seconds ahead is kept, takes effect at once, and
// expires two minutes after its own time.
func TestNewDropsAMarkFromTooFarAhead(t *testing.T) {
	clock := newFakeClock()
	built := clock.now()
	atLimit, beyond := built.Add(markFutureAllowance), built.Add(markFutureAllowance+time.Nanosecond)
	for name, c := range map[string]struct {
		at   time.Time
		want time.Time
	}{
		"no mark":                     {time.Time{}, time.Time{}},
		"in the past":                 {built.Add(-time.Minute), built.Add(-time.Minute)},
		"exactly 30 seconds ahead":    {atLimit, atLimit},
		"one nanosecond beyond":       {beyond, time.Time{}},
		"a day ahead":                 {built.Add(24 * time.Hour), time.Time{}},
		"one nanosecond short of 30s": {atLimit.Add(-time.Nanosecond), atLimit.Add(-time.Nanosecond)},
	} {
		if got := acceptedMark(c.at, built); !got.Equal(c.want) {
			t.Errorf("%s: acceptedMark = %v, want %v", name, got, c.want)
		}
	}

	l := newLab(t)
	p := l.poolWith(t, nil, nil, clock,
		marked(configured("c1:8006"), atLimit), marked(configured("c2:8006"), beyond), configured("c3:8006"))
	mustActive(t, p, "c2:8006")
	if away, ok := p.StartedAway(); !ok || away.First != "c1:8006" || !away.MarkedAt.Equal(atLimit) {
		t.Fatalf("StartedAway = %+v, %v, want a start away from c1", away, ok)
	}
	for _, step := range []struct {
		advance time.Duration
		want    []string
	}{
		{0, []string{"c2:8006", "c3:8006", "c1:8006"}},
		// The clock reaches the dropped mark's time, which a kept mark would
		// have made take effect.
		{markFutureAllowance + time.Nanosecond, []string{"c2:8006", "c3:8006", "c1:8006"}},
		{time.Minute, []string{"c2:8006", "c3:8006", "c1:8006"}},
		// Two minutes after the kept mark's own time it has expired.
		{markLifetime - time.Minute - time.Nanosecond, []string{"c2:8006", "c1:8006", "c3:8006"}},
	} {
		clock.advance(step.advance)
		order, _ := p.walkOrder(false)
		wantOrder(t, order, step.want...)
	}
	if got := endpointOf(t, p, "c2:8006").markedAt; !got.IsZero() {
		t.Fatalf("the dropped mark is still loaded: %v", got)
	}
}

// A request answered while the hooks are unset never reaches the guard, and
// the guard leaves a call that returns normally alone.
func TestGuardHookIgnoresACallThatReturns(t *testing.T) {
	l := newLab(t)
	sink, hook := &markRecorder{}, &answerRecorder{}
	p := l.poolWith(t, sink, hook, nil, configured("c1:8006"))
	p.markDown(Mark{Endpoint: "c1:8006", Class: PhaseDial, At: time.Now()})
	p.tellFirstAnswer("c1:8006")
	if len(sink.all()) != 1 || len(hook.all()) != 1 {
		t.Fatalf("sink %+v, hook %v, want one call each", sink.all(), hook.all())
	}
	for _, e := range l.obs.All() {
		if strings.Contains(e.Message, "panicked") {
			t.Fatalf("a call that returns logged %q", e.Message)
		}
	}
	quiet := l.poolWith(t, nil, nil, nil, configured("c1:8006"))
	quiet.markDown(Mark{Endpoint: "c1:8006", Class: PhaseDial, At: time.Now()})
}

// mustCertificateRefusal fails the test unless err is the pool's refusal of an
// ineligible endpoint, unsent and shaped as a certificate failure, so the SDK
// treats it as terminal the way it treats a pin the handshake refused.
func mustCertificateRefusal(t *testing.T, err error) {
	t.Helper()
	var cve *tls.CertificateVerificationError
	if !errors.As(err, &cve) {
		t.Fatalf("the refusal should be a certificate verification error, got %T: %v", err, err)
	}
	if !errors.Is(err, errIneligible) || !errors.Is(err, ErrPinMismatch) || IsMaybeSent(err) {
		t.Fatalf("the refusal should name the ineligible endpoint and stay unsent, got %v", err)
	}
}
