// Package swaptransport replaces *http.Transport values while requests run.
// net/http reads a transport's exported fields, such as MaxIdleConns and
// MaxIdleConnsPerHost, without a lock the caller can take, so writing them on
// a transport in use is a data race. This package never writes a transport
// after it is in use: a Transport installed as an http.Client's RoundTripper
// switches later requests to a tuned Clone, and Retired cleans up the
// transports that were replaced.
//
// Closing a replaced transport's idle connections closes the connections
// pooled on it at that moment, and nothing more is guaranteed. A replaced
// transport can still gain pooled connections afterwards. A request that was
// waiting for a connection during the swap resets the transport's close-idle
// state, so the connection it releases, and those that other requests still
// in flight on that transport release after it, are pooled rather than
// closed. Those connections must not live forever, so every transport this
// package hands requests to has a finite IdleConnTimeout:
// the configured value, or DefaultIdleConnTimeout when it is zero or negative.
// A connection pooled on a replaced transport is therefore closed by
// net/http's idle timer at the latest IdleConnTimeout after its request ends,
// and sooner when CloseIdleConnections reaches that transport.
//
// Closing an idle connection calls its net.Conn's Close method, which can be
// caller code, such as a connection returned by a caller's DialContext, and
// that code may call back into the SDK. This package therefore never closes a
// connection while it holds a lock. Update and Retired.Add hand the caller an
// Idle to close instead, and a caller that holds a lock of its own closes it
// only after releasing that lock.
//
// Releasing the SDK's locks is not enough on its own. net/http also calls a
// connection's Close while holding the transport's internal idle lock: when
// the idle timer expires a connection, and when a connection returning to the
// pool pushes the transport past MaxIdleConns and evicts the oldest one. If
// that Close calls back into code that closes the same transport's idle
// connections and waits for it, the wait needs the lock net/http is holding
// and never ends. Code reachable from a connection's Close, such as a setter
// that retires a transport, therefore closes an Idle with CloseInBackground,
// which does not wait.
package swaptransport

import (
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultIdleConnTimeout is the IdleConnTimeout given to a transport
// configured with none, matching net/http's DefaultTransport.
const DefaultIdleConnTimeout = 90 * time.Second

// maxRetired bounds how many replaced transports a Retired keeps, so a caller
// that retunes in a loop does not grow memory without limit.
const maxRetired = 8

// FiniteIdleTimeout returns timeout when it is positive and
// DefaultIdleConnTimeout otherwise.
func FiniteIdleTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}

	return DefaultIdleConnTimeout
}

// Idle is a set of transports whose idle connections still need closing.
// Code that collects an Idle while holding a lock calls CloseIdleConnections
// only after releasing every lock it holds, as the package doc explains.
type Idle []*http.Transport

// CloseIdleConnections closes the idle connections of every transport in the
// set. The caller must hold no lock that a net.Conn's Close method could need.
func (idle Idle) CloseIdleConnections() {
	for _, t := range idle {
		t.CloseIdleConnections()
	}
}

// CloseInBackground closes the idle connections of every transport in the set
// on a new goroutine and returns without waiting, so it is safe to call from
// a net.Conn's Close while net/http holds a transport's idle lock, as the
// package doc explains. The goroutine is added to wg, which must not be nil,
// so a caller can wait for it to finish. It ends once every transport's CloseIdleConnections
// returns, which takes as long as the slowest Close of a connection in the
// set. An empty set starts no goroutine.
func (idle Idle) CloseInBackground(wg *sync.WaitGroup) {
	if len(idle) == 0 {
		return
	}

	wg.Go(idle.CloseIdleConnections)
}

// Retired holds the most recent replaced transports, at most maxRetired, so
// CloseIdleConnections can still reach connections released to them after the
// swap. A transport dropped from the list has its idle connections closed as
// it is dropped; anything it pools later is closed by its IdleConnTimeout.
// Retired never closes a connection while holding its lock. The zero value is
// ready to use.
type Retired struct {
	mu   sync.Mutex
	list []*http.Transport
}

// Add keeps old, dropping the oldest kept transport when the list is full,
// and returns the transports whose idle connections the caller must close:
// old, and the dropped transport when there is one. Add closes nothing itself,
// so the caller can call it under a lock and close the result after releasing
// that lock.
func (r *Retired) Add(old *http.Transport) Idle {
	r.mu.Lock()
	defer r.mu.Unlock()

	idle := Idle{old}

	if len(r.list) == maxRetired {
		idle = append(idle, r.list[0])
		r.list = append(r.list[:0], r.list[1:]...)
	}

	r.list = append(r.list, old)

	return idle
}

// Snapshot returns the kept transports, for the caller to close after
// releasing any lock it holds.
func (r *Retired) Snapshot() Idle {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(Idle(r.list))
}

// CloseIdleConnections closes the idle connections of every kept transport.
// It holds no lock while closing, so the caller must hold none either.
func (r *Retired) CloseIdleConnections() {
	r.Snapshot().CloseIdleConnections()
}

// Len reports how many replaced transports are kept, at most maxRetired.
func (r *Retired) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.list)
}

// Transport is an http.RoundTripper that sends each request on the current
// *http.Transport, loaded atomically, so Update can replace that transport
// while requests run.
type Transport struct {
	current atomic.Pointer[http.Transport]

	// mu serializes Update.
	mu sync.Mutex

	retired Retired
}

// New returns a Transport that sends on t. It sets t's IdleConnTimeout to
// DefaultIdleConnTimeout when t has none, so t must not be in use yet, and the
// caller must not write t's fields afterwards.
func New(t *http.Transport) *Transport {
	t.IdleConnTimeout = FiniteIdleTimeout(t.IdleConnTimeout)

	s := &Transport{}
	s.current.Store(t)

	return s
}

// RoundTrip sends req on the current transport.
func (s *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.current.Load().RoundTrip(req) //nolint:wrapcheck // a transport must hand back the inner error untouched
}

// CloseIdleConnections closes the idle connections of the current transport
// and of the retired transports Update keeps. It holds no lock while closing.
func (s *Transport) CloseIdleConnections() {
	s.current.Load().CloseIdleConnections()
	s.retired.CloseIdleConnections()
}

// Load returns the transport that later requests use. The caller must not
// write its fields.
func (s *Transport) Load() *http.Transport {
	return s.current.Load()
}

// Update passes apply a Clone of the current transport. When apply changes
// the clone and returns true, Update installs the clone for later requests,
// with a finite IdleConnTimeout as the package doc describes, retires the
// transport it replaced, and returns the transports whose idle connections
// the caller must close: the replaced one, and a retired one the bound
// dropped. Requests already in flight finish on the replaced transport. When
// apply returns false, the current transport stays in place, the clone is
// discarded, and Update returns nil.
//
// Update closes no connection itself, so the caller may hold a lock around
// it. The caller calls CloseIdleConnections on the result once it holds no
// lock, because closing a connection runs caller code, or CloseInBackground
// when it can be reached from a connection's Close. apply runs under Update's
// lock and must not call Update.
func (s *Transport) Update(apply func(next *http.Transport) bool) Idle {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.current.Load()
	next := old.Clone()

	if !apply(next) {
		return nil
	}

	next.IdleConnTimeout = FiniteIdleTimeout(next.IdleConnTimeout)

	s.current.Store(next)

	return s.retired.Add(old)
}

// RetiredCount reports how many replaced transports the Transport keeps,
// which is at most maxRetired.
func (s *Transport) RetiredCount() int {
	return s.retired.Len()
}
