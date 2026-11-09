package pvetest

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"syscall"
	"testing"
)

// garbage is what a Garbage server writes. Its first byte is not a TLS record
// type, so a client fails with a record-header error before any certificate.
var garbage = []byte("\x00\x01\x02not a tls record, just bytes\r\n")

// NewLive starts a TLS server that answers routes and records every request.
// A nil routes serves NewRoutes.
func NewLive(tb testing.TB, leaf *Leaf, routes *Routes) *Server {
	tb.Helper()
	requireLeaf(tb, leaf)
	if routes == nil {
		routes = NewRoutes()
	}
	s := newServer(tb, KindLive, listenLoopback(tb), leaf)
	s.serveHTTP(leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.observe(r, serverName(r))
		routes.ServeHTTP(w, r)
	}))
	return s
}

// NewReset returns a server that resets every client at the first byte. Its
// listener stays bound for the life of the test, so no parallel test's server
// can be handed the port, and it accepts each connection, reads one byte of what
// the client sent, and closes with a TCP reset.
//
// The TCP connect succeeds, so a client does not see ECONNREFUSED from its dial.
// A TLS client has just written its hello, and its handshake read fails with
// ECONNRESET ("read: connection reset by peer"). The read happens after the
// client sent the hello and before any request byte exists, so the spec counts
// it as a connect-phase failure in the handshake phase and a request never
// reaches the server, whatever its method. A pool's transport tags it
// tls_handshake. A test that needs the dial class uses NewRefused.
func NewReset(tb testing.TB) *Server {
	tb.Helper()
	return newResetServer(tb, KindReset)
}

func newResetServer(tb testing.TB, kind Kind) *Server {
	tb.Helper()
	s := newServer(tb, kind, listenLoopback(tb), nil)
	s.serveRaw(func(conn net.Conn) {
		var one [1]byte
		if _, err := conn.Read(one[:]); err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
	})
	return s
}

// Refused stands for an endpoint whose port refuses connections. Its DialContext
// returns the error a refused dial returns, without depending on a free port,
// so no parallel test can turn the refusal into an answer. Install it as a
// transport's DialContext, and give the fake routes its Addr to name. A client
// that dials Addr for real reaches a held listener that resets the connection,
// which is the NewReset behavior.
type Refused struct {
	*Server
}

// NewRefused starts a Refused server.
func NewRefused(tb testing.TB) *Refused {
	tb.Helper()
	return &Refused{Server: newResetServer(tb, KindRefused)}
}

// DialContext fails at once with a *net.OpError whose Op is "dial", whose Net is
// the network asked for, and whose Err is an *os.SyscallError for "connect"
// wrapping syscall.ECONNREFUSED. That is the error a dial to a closed port
// returns on Linux and macOS, so errors.Is matches syscall.ECONNREFUSED and the
// pool tags it as the dial class. A context that has already ended wins, as it
// does for a real dial, and the error wraps ctx.Err() instead.
func (r *Refused) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	r.Counts.Dials.Add(1)
	r.emit(EventDial, Event{Addr: r.Addr})
	if err := ctx.Err(); err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Addr: r.listener.Addr(), Err: err}
	}
	return nil, &net.OpError{
		Op:   "dial",
		Net:  network,
		Addr: r.listener.Addr(),
		Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}
}

// Dropped stands for an endpoint whose packets vanish. Its TCP listener
// accepts connections and never writes, so a TLS handshake gets no reply and
// stalls until the client gives up, and its DialContext stands in for a dial
// that the network never completes.
type Dropped struct {
	*Server
}

// NewDropped starts a Dropped server.
func NewDropped(tb testing.TB) *Dropped {
	tb.Helper()
	s := newServer(tb, KindDropped, listenLoopback(tb), nil)
	s.serveRaw(func(net.Conn) { <-s.done })
	return &Dropped{Server: s}
}

// DialContext blocks until ctx ends or the server closes, then fails the way a
// dial that timed out does. The error is a *net.OpError with Op "dial" that
// wraps ctx.Err(), so errors.Is matches context.DeadlineExceeded and
// context.Canceled. Install it as a transport's DialContext.
func (d *Dropped) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	d.Counts.Dials.Add(1)
	d.emit(EventDial, Event{Addr: d.Addr})
	opErr := func(err error) error {
		return &net.OpError{Op: "dial", Net: network, Addr: d.listener.Addr(), Err: err}
	}
	select {
	case <-ctx.Done():
		return nil, opErr(ctx.Err())
	case <-d.done:
		return nil, opErr(net.ErrClosed)
	}
}

// NewGarbage starts a server that writes bytes that are not a TLS record to
// every connection and then reads until the client hangs up. A TLS client
// fails its handshake with a record-header error.
func NewGarbage(tb testing.TB) *Server {
	tb.Helper()
	s := newServer(tb, KindGarbage, listenLoopback(tb), nil)
	s.serveRaw(func(conn net.Conn) {
		if _, err := conn.Write(garbage); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	return s
}

// NewHung starts a TLS server that completes the handshake, reads each request
// down to the last byte of its body, and never answers. The connection stays
// open until the server closes, so a client's response-header deadline is the
// only thing that ends the wait.
func NewHung(tb testing.TB, leaf *Leaf) *Server {
	tb.Helper()
	requireLeaf(tb, leaf)
	s := newServer(tb, KindHung, listenLoopback(tb), leaf)
	s.serveRaw(func(conn net.Conn) {
		tc, ok := s.handshake(conn, leaf)
		if !ok {
			return
		}
		br := bufio.NewReader(tc)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			s.observe(req, tc.ConnectionState().ServerName)
			_, _ = io.Copy(io.Discard, req.Body)
			select {
			case <-s.done:
				return
			default:
			}
		}
	})
	return s
}

// NewBodyThenHijack starts a TLS server that reads each request's whole body
// and then hijacks the connection and closes it without writing a byte. The
// request reached the server, so a mutation sent here may have taken effect.
func NewBodyThenHijack(tb testing.TB, leaf *Leaf) *Server {
	tb.Helper()
	requireLeaf(tb, leaf)
	s := newServer(tb, KindBodyThenHijack, listenLoopback(tb), leaf)
	s.serveHTTP(leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.observe(r, serverName(r))
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "pvetest: connection cannot be hijacked", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	return s
}

// ReuseCloser answers the first request on each connection from its routes and
// closes the connection when a later request arrives on it, before reading
// that request. Go's transport sees a reused connection die without a response
// and retries an idempotent request, so a test closes the listener from the
// reuse hook to make that retry a refused dial.
//
// The server covers a GET, which Go retries after an EOF on a reused
// connection. It does not cover a POST. Go replays a non-idempotent request
// only after a nothingWrittenError, which means the write to the reused
// connection failed before any byte went out, and this server closes after the
// client has already written. A test of the POST case needs a DialTLSContext
// fake on the transport whose first connection fails its first write.
type ReuseCloser struct {
	*Server
}

// NewReuseCloser starts a ReuseCloser. A nil routes serves NewRoutes.
func NewReuseCloser(tb testing.TB, leaf *Leaf, routes *Routes) *ReuseCloser {
	tb.Helper()
	requireLeaf(tb, leaf)
	if routes == nil {
		routes = NewRoutes()
	}
	s := newServer(tb, KindReuseCloser, listenLoopback(tb), leaf)
	s.serveRaw(func(conn net.Conn) {
		tc, ok := s.handshake(conn, leaf)
		if !ok {
			return
		}
		br := bufio.NewReader(tc)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		s.observe(req, tc.ConnectionState().ServerName)
		_, _ = io.Copy(io.Discard, req.Body)
		if err := writeRouted(tc, routes, req); err != nil {
			return
		}
		if _, err := br.Peek(1); err != nil {
			return
		}
		s.Counts.Reused.Add(1)
		s.emit(EventReuse, Event{Addr: conn.RemoteAddr().String()})
		if f := s.onReuse.Load(); f != nil {
			(*f)()
		}
	})
	return &ReuseCloser{Server: s}
}

// SetOnReuse installs a function that runs after a reused connection's next
// request has started to arrive and before the server closes the connection.
// Call StopListening from it to make the client's retry dial fail. Pass nil to
// remove it.
func (r *ReuseCloser) SetOnReuse(f func()) {
	if f == nil {
		r.onReuse.Store(nil)
		return
	}
	r.onReuse.Store(&f)
}

// writeRouted serves req from routes and writes the response to w with a
// Content-Length, so the client keeps the connection for reuse.
func writeRouted(w io.Writer, routes *Routes, req *http.Request) error {
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	resp := rec.Result()
	// Response.Write omits the body of a HEAD answer only when it knows the
	// request, and without it a HEAD would carry bytes the client never expects.
	resp.Request = req
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp.Write(w)
}

func serverName(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return r.TLS.ServerName
}
