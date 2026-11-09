package pvetest

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// NewRefused returns a server whose port nothing listens on, so a dial gets
// ECONNREFUSED at once. The port was free when the call returned, and a test
// that runs for long could in principle meet another process on it.
func NewRefused(tb testing.TB) *Server {
	tb.Helper()
	s := newServer(tb, KindRefused, listenLoopback(tb), nil)
	s.StopListening()
	return s
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
