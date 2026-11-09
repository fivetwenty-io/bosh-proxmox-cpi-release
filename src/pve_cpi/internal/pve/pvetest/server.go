package pvetest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Kind names the way a Server misbehaves.
type Kind string

// The server kinds.
const (
	KindLive           Kind = "live"
	KindRefused        Kind = "refused"
	KindDropped        Kind = "dropped"
	KindGarbage        Kind = "garbage"
	KindHung           Kind = "hung"
	KindBodyThenHijack Kind = "body-then-hijack"
	KindReuseCloser    Kind = "reuse-closer"
)

// EventKind names a thing a server announces on its event channels.
type EventKind int

// The event kinds.
const (
	// EventConn fires when the server accepts a TCP connection.
	EventConn EventKind = iota
	// EventRequest fires when the server reads a request.
	EventRequest
	// EventDial fires when a Dropped server's DialContext is entered.
	EventDial
	// EventReuse fires when a ReuseCloser is about to close a reused connection.
	EventReuse

	eventKinds
)

const (
	eventBuffer = 1024
	// maxRecordedBody caps the request body bytes that Request keeps.
	maxRecordedBody = 64 << 10
	// waitLimit is the failsafe that stops Wait from hanging a test forever.
	waitLimit = 30 * time.Second
)

// Request is one request a server read. The Request in an Event leaves Body
// empty, because the event fires before the body is read, and Seen holds the
// body once it has been read.
type Request struct {
	Method     string
	Path       string
	RawQuery   string
	Header     http.Header
	Body       []byte
	ServerName string
}

// Event is one announcement from a server.
type Event struct {
	Kind    EventKind
	Request Request
	Addr    string
}

// Counters count what a server saw. Every field is safe to read while the
// server runs.
type Counters struct {
	// Conns counts accepted TCP connections.
	Conns atomic.Int32
	// Handshakes counts TLS handshakes whose client hello the server accepted.
	Handshakes atomic.Int32
	// Requests counts requests the server read.
	Requests atomic.Int32
	// Reused counts connections a ReuseCloser closed on a later request.
	Reused atomic.Int32
	// Dials counts DialContext calls on a Dropped server.
	Dials atomic.Int32

	mu      sync.Mutex
	methods map[string]int32
}

// Method returns how many requests with the HTTP method the server read.
func (c *Counters) Method(method string) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.methods[method]
}

func (c *Counters) addMethod(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.methods == nil {
		c.methods = make(map[string]int32)
	}
	c.methods[method]++
}

// TotalMethod sums Method(method) across servers, which is how a test asserts
// that exactly one POST reached any server.
func TotalMethod(method string, servers ...*Server) int32 {
	var n int32
	for _, s := range servers {
		n += s.Counts.Method(method)
	}
	return n
}

// Server is one test endpoint. Use the New* constructors, which register Close
// with the test's cleanup.
type Server struct {
	// Kind is the way the server behaves.
	Kind Kind
	// Host is the loopback address the server listens on.
	Host string
	// Port is the TCP port.
	Port int
	// Addr is Host and Port joined as "host:port".
	Addr string
	// URL is "https://host:port" with no path.
	URL string
	// Leaf is the certificate the server presents. It is nil for kinds that
	// speak no TLS.
	Leaf *Leaf
	// Counts holds the server's counters.
	Counts Counters

	listener   net.Listener
	listenOnce sync.Once
	closeOnce  sync.Once
	done       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	httpClose  func()

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	seen   []Request
	events map[EventKind]chan Event

	// dropped counts, per event kind, the events emit discarded because the
	// kind's queue was full.
	dropped [eventKinds]atomic.Int32

	onRequest atomic.Pointer[func(*http.Request)]
	onReuse   atomic.Pointer[func()]
}

func newServer(tb testing.TB, kind Kind, ln net.Listener, leaf *Leaf) *Server {
	tb.Helper()
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		tb.Fatalf("pvetest: listener address %v is not TCP", ln.Addr())
	}
	host := tcp.IP.String()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ctx:      ctx,
		cancel:   cancel,
		Kind:     kind,
		Host:     host,
		Port:     tcp.Port,
		Addr:     net.JoinHostPort(host, strconv.Itoa(tcp.Port)),
		Leaf:     leaf,
		listener: ln,
		done:     make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
		events: map[EventKind]chan Event{
			EventConn:    make(chan Event, eventBuffer),
			EventRequest: make(chan Event, eventBuffer),
			EventDial:    make(chan Event, eventBuffer),
			EventReuse:   make(chan Event, eventBuffer),
		},
	}
	s.URL = "https://" + s.Addr
	tb.Cleanup(s.Close)
	return s
}

func listenLoopback(tb testing.TB) net.Listener {
	tb.Helper()
	ln, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("pvetest: listen: %v", err)
	}
	return ln
}

func requireLeaf(tb testing.TB, leaf *Leaf) {
	tb.Helper()
	if leaf == nil {
		tb.Fatal("pvetest: this server kind needs a leaf certificate")
	}
}

// tlsConfig returns the server-side TLS configuration. It offers only
// http/1.1, because the failover wrapper refuses a transport that could
// negotiate h2, and it counts a handshake once the client's hello is accepted.
func (s *Server) tlsConfig(leaf *Leaf) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{leaf.Cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
		VerifyConnection: func(tls.ConnectionState) error {
			s.Counts.Handshakes.Add(1)
			return nil
		},
	}
}

// HostPort returns the host and port in the shape the CPI configuration takes.
func (s *Server) HostPort() (host string, port int) { return s.Host, s.Port }

// Done is closed when the server starts closing. A hook that blocks should
// select on it, because Close waits for running handlers.
func (s *Server) Done() <-chan struct{} { return s.done }

// Seen returns a copy of the requests the server has read, oldest first.
func (s *Server) Seen() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.seen...)
}

// Wait blocks until the server announces an event of the kind and returns it.
// Each kind has its own queue, so waiting for one kind never discards another.
// The test fails when the server closes first or after thirty seconds, which
// is a failsafe and not a delay a passing test pays. A failure names how many
// events of the kind the server dropped because the queue held 1024 unread
// events.
func (s *Server) Wait(tb testing.TB, kind EventKind) Event {
	tb.Helper()
	timer := time.NewTimer(waitLimit)
	defer timer.Stop()
	select {
	case ev := <-s.events[kind]:
		return ev
	case <-s.done:
		tb.Fatalf("pvetest: %s server on %s closed while waiting for event %d (%d events of that kind dropped)",
			s.Kind, s.Addr, kind, s.Dropped(kind))
	case <-timer.C:
		tb.Fatalf("pvetest: %s server on %s saw no event %d within %v (%d events of that kind dropped)",
			s.Kind, s.Addr, kind, waitLimit, s.Dropped(kind))
	}
	return Event{}
}

// Dropped returns how many events of the kind the server discarded because the
// kind's queue was full. A test that expects more than 1024 unread events of a
// kind reads them with Wait as they arrive.
func (s *Server) Dropped(kind EventKind) int32 {
	if kind < 0 || kind >= eventKinds {
		return 0
	}
	return s.dropped[kind].Load()
}

// StopListening closes the listener and leaves established connections open,
// so later dials are refused while a reused connection still answers. It is
// safe to call more than once and from inside a hook. The port is free from
// then on, so a server that a parallel test starts could be handed it, which
// leaves a short window between this call and the dial that is meant to be
// refused.
func (s *Server) StopListening() {
	s.listenOnce.Do(func() { _ = s.listener.Close() })
}

// Close stops the server, closes its connections, and waits for its goroutines.
// It is safe to call more than once.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()
		s.StopListening()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		if s.httpClose != nil {
			s.httpClose()
		}
		s.wg.Wait()
	})
}

// SetOnRequest installs a function that runs for every request after it is
// counted and before the route answers. Block inside it on a channel to hold a
// response. Pass nil to remove it. It applies to Live, BodyThenHijack, Hung, and
// ReuseCloser servers.
func (s *Server) SetOnRequest(f func(*http.Request)) {
	if f == nil {
		s.onRequest.Store(nil)
		return
	}
	s.onRequest.Store(&f)
}

func (s *Server) emit(kind EventKind, ev Event) {
	ev.Kind = kind
	select {
	case s.events[kind] <- ev:
	default:
		s.dropped[kind].Add(1)
	}
}

// observe counts and records a request and runs the request hook. It counts and
// announces the request before it reads any of the body, so a client that
// stalls a streamed upload still shows in Counts and Wait. The recorded body
// is filled in once the first 64 KiB, or all of the body when it is shorter,
// has been read. It leaves r.Body readable from its first byte.
func (s *Server) observe(r *http.Request, serverName string) {
	rec := Request{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		Header:     r.Header.Clone(),
		ServerName: serverName,
	}
	s.Counts.Requests.Add(1)
	s.Counts.addMethod(r.Method)
	s.mu.Lock()
	idx := len(s.seen)
	s.seen = append(s.seen, rec)
	s.mu.Unlock()
	s.emit(EventRequest, Event{Request: rec})

	head, _ := io.ReadAll(io.LimitReader(r.Body, maxRecordedBody))
	orig := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), orig), orig}
	s.mu.Lock()
	s.seen[idx].Body = head
	s.mu.Unlock()
	if f := s.onRequest.Load(); f != nil {
		(*f)(r)
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

// serveRaw accepts connections on the server's own listener and runs handle for
// each in its own goroutine. handle owns the connection until it returns.
func (s *Server) serveRaw(handle func(net.Conn)) {
	s.wg.Go(func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				return
			}
			if !s.track(conn) {
				_ = conn.Close()
				return
			}
			s.Counts.Conns.Add(1)
			s.emit(EventConn, Event{Addr: conn.RemoteAddr().String()})
			s.wg.Go(func() {
				defer s.untrack(conn)
				handle(conn)
			})
		}
	})
}

// serveHTTP starts an httptest-style HTTP server on the server's listener.
func (s *Server) serveHTTP(leaf *Leaf, h http.Handler) {
	srv := &http.Server{
		Handler:           h,
		TLSConfig:         s.tlsConfig(leaf),
		ReadHeaderTimeout: waitLimit,
		ConnState: func(c net.Conn, st http.ConnState) {
			if st == http.StateNew {
				s.Counts.Conns.Add(1)
				s.emit(EventConn, Event{Addr: c.RemoteAddr().String()})
			}
		},
	}
	tlsLn := tls.NewListener(s.listener, srv.TLSConfig)
	s.wg.Go(func() { _ = srv.Serve(tlsLn) })
	s.httpClose = func() { _ = srv.Close() }
}

func (s *Server) handshake(conn net.Conn, leaf *Leaf) (*tls.Conn, bool) {
	tc := tls.Server(conn, s.tlsConfig(leaf))
	if err := tc.HandshakeContext(s.ctx); err != nil {
		return nil, false
	}
	return tc, true
}
