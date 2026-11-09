package endpointpool

import (
	"errors"
	"net"
	"net/http/httptrace"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeClock is a clock a test moves by hand. It is safe for concurrent use,
// because the transport's trace hooks and a test server's handler read it from
// other goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// boundsOn returns the default bounds measured on clock.
func boundsOn(clock *fakeClock) bounds {
	b := defaultBounds()
	b.now = clock.now
	return b
}

func TestDefaultBounds(t *testing.T) {
	b := defaultBounds()
	if b.hops != 4 || b.connect != 30*time.Second || b.total != 90*time.Second || b.now == nil {
		t.Fatalf("defaultBounds = %+v", b)
	}
}

func TestHopLimitAndFinalHop(t *testing.T) {
	b := defaultBounds()
	for eligible, want := range map[int]int{0: 0, 1: 1, 3: 3, 4: 4, 6: 4} {
		if got := b.hopLimit(eligible); got != want {
			t.Errorf("hopLimit(%d) = %d, want %d", eligible, got, want)
		}
	}
	cases := []struct {
		i, n  int
		final bool
	}{
		{0, 1, true},
		{0, 2, false},
		{1, 2, true},
		{2, 4, false},
		{3, 4, true},
	}
	for _, c := range cases {
		if got := finalHop(c.i, c.n); got != c.final {
			t.Errorf("finalHop(%d, %d) = %v, want %v", c.i, c.n, got, c.final)
		}
	}
}

// A new hop may start while both budgets have time left, and not once either
// has reached its limit.
func TestBudgetAllowsHop(t *testing.T) {
	clock := newFakeClock()
	w := boundsOn(clock).begin()
	if !w.allowsHop() {
		t.Fatal("a fresh budget refused a hop")
	}
	w.spend(29 * time.Second)
	w.spend(-time.Second)
	if !w.allowsHop() {
		t.Fatal("29s of connect time refused a hop")
	}
	w.spend(time.Second)
	if w.allowsHop() {
		t.Fatal("30s of connect time allowed a hop")
	}

	w = boundsOn(clock).begin()
	clock.advance(89 * time.Second)
	if !w.allowsHop() {
		t.Fatal("89s into the walk refused a hop")
	}
	clock.advance(time.Second)
	if w.allowsHop() {
		t.Fatal("90s into the walk allowed a hop")
	}
}

// The meter counts the time from GetConn to GotConn, counts a wait that ended
// without a connection when the hop returns, and adds every round of Go's own
// retries.
func TestConnectMeter(t *testing.T) {
	clock := newFakeClock()
	m := newConnectMeter(clock.now)
	tr := m.trace()
	tr.GetConn("a:8006")
	clock.advance(3 * time.Second)
	tr.GotConn(httptrace.GotConnInfo{})
	clock.advance(time.Minute)
	tr.GetConn("a:8006")
	clock.advance(2 * time.Second)
	spent, got := m.result()
	if spent != 5*time.Second || !got {
		t.Fatalf("result = %v, %v, want 5s, true", spent, got)
	}
	if spent, _ := m.result(); spent != 5*time.Second {
		t.Fatalf("second result = %v, want 5s", spent)
	}

	m = newConnectMeter(clock.now)
	tr = m.trace()
	tr.GetConn("a:8006")
	clock.advance(4 * time.Second)
	if spent, got := m.result(); spent != 4*time.Second || got {
		t.Fatalf("failed wait = %v, %v, want 4s, false", spent, got)
	}
}

func TestFailuresAllUnreachableIsOneDialError(t *testing.T) {
	var f failures
	f.add("a:8006", &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: syscall.ECONNREFUSED}, true)
	f.add("b:8006", timeoutError{}, true)
	err := f.err()
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) {
		t.Fatalf("err = %T %v, want *EntryUnreachableError", err, err)
	}
	if got := eu.Endpoints(); len(got) != 2 || got[0] != "a:8006" || got[1] != "b:8006" {
		t.Fatalf("Endpoints = %v", got)
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("err unwraps to %v, want a dial *net.OpError", op)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatal("err lost the refused dial's cause")
	}
}

func TestFailuresAllVerificationIsTheCertificateError(t *testing.T) {
	var f failures
	cause := errors.New("x509: certificate is valid for other, not pve1")
	f.add("a:8006", &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: "a:8006", Err: cause}, true)
	f.add("b:8006", &ConnectPhaseError{Phase: PhasePin, Endpoint: "b:8006", Err: ErrPinMismatch}, true)
	err := f.err()
	var op *net.OpError
	if errors.As(err, &op) {
		t.Fatalf("an all-verification walk unwrapped to a dial error: %v", err)
	}
	if !errors.Is(err, cause) || !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want both certificate causes", err)
	}
}

// A walk whose hops include a failure that is not unreachable returns the last
// hop's error as it is.
func TestFailuresWithAnotherFailureReturnTheLastError(t *testing.T) {
	var f failures
	f.add("a:8006", &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: syscall.ECONNREFUSED}, true)
	reset := errors.New("read: connection reset by peer")
	f.add("b:8006", reset, false)
	f.add("c:8006", &ConnectPhaseError{Phase: PhaseDial, Endpoint: "c:8006", Err: syscall.ECONNREFUSED}, true)
	last := f.last
	if err := f.err(); !errors.Is(err, last) {
		t.Fatalf("err = %v, want the last hop's error %v", err, last)
	}
	var g failures
	g.add("a:8006", reset, false)
	if err := g.err(); !errors.Is(err, reset) {
		t.Fatalf("err = %v, want %v", err, reset)
	}
}
