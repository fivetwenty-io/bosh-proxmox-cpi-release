package pvetest_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// client returns an HTTP/1.1 client that trusts ca and verifies "localhost".
// Keep-alives stay on unless the test turns them off, and responseHeader sets
// the response-header deadline when it is positive.
func client(ca *pvetest.CA, keepAlive bool, responseHeader time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:       ca.ClientConfig("localhost"),
		DisableKeepAlives:     !keepAlive,
		ResponseHeaderTimeout: responseHeader,
	}
	return &http.Client{Transport: tr}
}

func get(c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

func post(c *http.Client, url, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestLive_ServesRoutesAndRecords(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	c := client(ca, false, 0)

	if s.Kind != pvetest.KindLive {
		t.Fatalf("Kind = %q", s.Kind)
	}
	if host, port := s.HostPort(); host != "127.0.0.1" || port == 0 || s.Addr != net.JoinHostPort(host, strconv.Itoa(port)) {
		t.Fatalf("HostPort = %q %d, Addr = %q", host, port, s.Addr)
	}

	resp, err := get(c, s.URL+"/api2/json/version?x=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"version":"9.0.2"`) {
		t.Fatalf("version body = %q", body)
	}
	resp, err = post(c, s.URL+"/api2/json/pools", "poolid=a")
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = readBody(t, resp)

	if got := s.Counts.Requests.Load(); got != 2 {
		t.Fatalf("Requests = %d, want 2", got)
	}
	if s.Counts.Method("GET") != 1 || s.Counts.Method("POST") != 1 || s.Counts.Method("DELETE") != 0 {
		t.Fatalf("method counts GET=%d POST=%d DELETE=%d", s.Counts.Method("GET"), s.Counts.Method("POST"),
			s.Counts.Method("DELETE"))
	}
	if s.Counts.Conns.Load() != 2 || s.Counts.Handshakes.Load() != 2 {
		t.Fatalf("Conns=%d Handshakes=%d, want 2 and 2 with keep-alives off", s.Counts.Conns.Load(),
			s.Counts.Handshakes.Load())
	}
	seen := s.Seen()
	if len(seen) != 2 || seen[0].Path != "/api2/json/version" || seen[0].RawQuery != "x=1" ||
		seen[1].Method != "POST" || !bytes.Equal(seen[1].Body, []byte("poolid=a")) ||
		seen[1].ServerName != "localhost" {
		t.Fatalf("Seen = %+v", seen)
	}
	ev := s.Wait(t, pvetest.EventRequest)
	if ev.Request.Method != "GET" {
		t.Fatalf("first request event = %+v", ev)
	}
	if ev := s.Wait(t, pvetest.EventRequest); ev.Request.Method != "POST" {
		t.Fatalf("second request event = %+v", ev)
	}
	if ev := s.Wait(t, pvetest.EventConn); !strings.HasPrefix(ev.Addr, "127.0.0.1:") {
		t.Fatalf("conn event Addr = %q, want the client's loopback address", ev.Addr)
	}
}

func TestLive_FailRouteAndRequestHook(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/cluster/resources", 503, "overloaded")
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), routes)
	c := client(ca, false, 0)

	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	s.SetOnRequest(func(*http.Request) {
		close(entered)
		select {
		case <-release:
		case <-s.Done():
		}
	})
	type result struct {
		code int
		err  error
		// early is true when the response ended before the test released the hook.
		early bool
	}
	done := make(chan result, 1)
	go func() {
		resp, err := get(c, s.URL+"/api2/json/cluster/resources")
		if err != nil {
			done <- result{err: err, early: !released.Load()}
			return
		}
		_ = readBody(t, resp)
		done <- result{code: resp.StatusCode, early: !released.Load()}
	}()
	<-entered
	released.Store(true)
	close(release)
	if r := <-done; r.err != nil || r.code != 503 || r.early {
		t.Fatalf("held response = %+v, want 503 after the release", r)
	}
	s.SetOnRequest(nil)
}

func TestLive_StopListeningRefusesLaterDials(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	c := client(ca, false, 0)

	resp, err := get(c, s.URL+"/api2/json/version")
	if err != nil {
		t.Fatalf("GET before stop: %v", err)
	}
	_ = readBody(t, resp)
	s.StopListening()
	s.StopListening()
	if _, err = get(c, s.URL+"/api2/json/version"); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("GET after StopListening = %v, want connection refused", err)
	}
	s.Close()
	s.Close()
}

func TestLive_RejectsMissingLeaf(t *testing.T) {
	t.Parallel()
	ft := &fakeTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		pvetest.NewLive(ft, nil, nil)
	}()
	if !ft.failed.Load() {
		t.Fatal("NewLive with a nil leaf must fail the test")
	}
}

func TestLive_WrongCAFailsBeforeHandler(t *testing.T) {
	t.Parallel()
	ca, other := pvetest.NewCA(t), pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)

	_, err := get(client(other, false, 0), s.URL+"/api2/json/version")
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("GET with the wrong CA = %v, want an unknown-authority error", err)
	}
	if s.Counts.Requests.Load() != 0 {
		t.Fatal("a client that fails verification must not reach the handler")
	}
}

func TestRefused_ResetsTheHandshakeAndKeepsItsPort(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewRefused(t)

	// The listener stays bound, so no other server can be handed the port.
	if ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", s.Addr); err == nil {
		_ = ln.Close()
		t.Fatalf("a refused server's port %s must stay bound", s.Addr)
	}

	_, err := get(client(ca, false, 0), s.URL+"/api2/json/version")
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "read" || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("GET = %v, want a handshake read that the server reset", err)
	}
	if errors.As(err, &op) && op.Op == "dial" {
		t.Fatalf("GET = %v: the TCP connect must succeed", err)
	}
	if s.Counts.Conns.Load() != 1 || s.Counts.Handshakes.Load() != 0 || s.Counts.Requests.Load() != 0 {
		t.Fatalf("Conns=%d Handshakes=%d Requests=%d, want 1, 0, 0", s.Counts.Conns.Load(),
			s.Counts.Handshakes.Load(), s.Counts.Requests.Load())
	}
}

func TestRefused_ResetsEveryMethodBeforeARequestExists(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewRefused(t)
	c := client(ca, false, 0)

	for range 3 {
		if _, err := post(c, s.URL+"/api2/json/pools", "poolid=a"); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("POST = %v, want a reset", err)
		}
	}
	if s.Counts.Conns.Load() != 3 || s.Counts.Requests.Load() != 0 || len(s.Seen()) != 0 {
		t.Fatalf("Conns=%d Requests=%d, want 3 and 0", s.Counts.Conns.Load(), s.Counts.Requests.Load())
	}
}

func TestDropped_DialContextEndsWithItsContext(t *testing.T) {
	t.Parallel()
	d := pvetest.NewDropped(t)

	expired, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	_, err := d.DialContext(expired, "tcp", d.Addr)
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DialContext = %v, want a dial OpError wrapping DeadlineExceeded", err)
	}
	var to interface{ Timeout() bool }
	if !errors.As(err, &to) || !to.Timeout() {
		t.Fatalf("DialContext error %v must report Timeout()", err)
	}
	if d.Counts.Dials.Load() != 1 {
		t.Fatalf("Dials = %d, want 1", d.Counts.Dials.Load())
	}
	if ev := d.Wait(t, pvetest.EventDial); ev.Addr != d.Addr {
		t.Fatalf("dial event = %+v", ev)
	}

	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = d.DialContext(canceled, "tcp", d.Addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DialContext = %v", err)
	}
}

func TestDropped_DialContextEndsWhenServerCloses(t *testing.T) {
	t.Parallel()
	d := pvetest.NewDropped(t)
	errc := make(chan error, 1)
	go func() {
		_, err := d.DialContext(context.Background(), "tcp", d.Addr)
		errc <- err
	}()
	d.Wait(t, pvetest.EventDial)
	d.Close()
	if err := <-errc; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("DialContext after Close = %v, want net.ErrClosed", err)
	}
}

func TestDropped_HandshakeGetsNoReply(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	d := pvetest.NewDropped(t)

	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", d.Addr)
	if err != nil {
		t.Fatalf("TCP dial must succeed: %v", err)
	}
	defer func() { _ = conn.Close() }()
	d.Wait(t, pvetest.EventConn)

	tc := tls.Client(conn, ca.ClientConfig("localhost"))
	hs := make(chan error, 1)
	go func() { hs <- tc.HandshakeContext(context.Background()) }()
	// The server never writes, so the only thing that can end the handshake is
	// the client giving up. Closing the connection here is that, and the handshake
	// error being the client's own closed connection proves the server sent
	// neither a reply nor a reset beforehand.
	_ = conn.Close()
	if err := <-hs; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("handshake = %v, want the client's own closed connection", err)
	}
	if d.Counts.Handshakes.Load() != 0 || d.Counts.Requests.Load() != 0 {
		t.Fatal("a dropped server completes no handshake and reads no request")
	}
}

func TestGarbage_HandshakeFailsWithRecordHeaderError(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewGarbage(t)

	_, err := get(client(ca, false, 0), s.URL+"/api2/json/version")
	var rh tls.RecordHeaderError
	if !errors.As(err, &rh) {
		t.Fatalf("GET = %v, want a tls.RecordHeaderError", err)
	}
	if !strings.Contains(err.Error(), "first record does not look like a TLS handshake") {
		t.Fatalf("error text = %q", err)
	}
	if s.Counts.Conns.Load() != 1 || s.Counts.Requests.Load() != 0 {
		t.Fatalf("Conns=%d Requests=%d", s.Counts.Conns.Load(), s.Counts.Requests.Load())
	}
}

func TestHung_ReadsRequestAndNeverAnswers(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewHung(t, ca.NewLeaf(t, pvetest.LeafOptions{}))

	// A response-header deadline in the past ends the wait once the request is
	// written, so the test needs no sleep and still proves the server read it.
	c := client(ca, false, time.Nanosecond)
	_, err := post(c, s.URL+"/api2/json/pools", "poolid=a")
	var to interface{ Timeout() bool }
	if err == nil || !errors.As(err, &to) || !to.Timeout() {
		t.Fatalf("POST = %v, want a response-header timeout", err)
	}
	ev := s.Wait(t, pvetest.EventRequest)
	if ev.Request.Method != "POST" || ev.Request.Path != "/api2/json/pools" {
		t.Fatalf("request event = %+v", ev)
	}
	if s.Counts.Handshakes.Load() != 1 || s.Counts.Method("POST") != 1 {
		t.Fatalf("Handshakes=%d POST=%d", s.Counts.Handshakes.Load(), s.Counts.Method("POST"))
	}
}

func TestHung_HoldsTheConnectionUntilClose(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewHung(t, ca.NewLeaf(t, pvetest.LeafOptions{}))
	c := client(ca, false, 0)

	type result struct {
		err error
		// early is true when the request ended before the test closed the server.
		early bool
	}
	var closing atomic.Bool
	errc := make(chan result, 1)
	go func() {
		resp, err := get(c, s.URL+"/api2/json/cluster/resources")
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- result{err: err, early: !closing.Load()}
	}()
	s.Wait(t, pvetest.EventRequest)
	closing.Store(true)
	s.Close()
	if r := <-errc; r.err == nil || r.early {
		t.Fatalf("waiting request = %+v, want a failure that only the server's Close caused", r)
	}
	if s.Counts.Method("GET") != 1 {
		t.Fatalf("GET count = %d, want 1", s.Counts.Method("GET"))
	}
}

func TestBodyThenHijack_ReadsWholeBodyThenDrops(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewBodyThenHijack(t, ca.NewLeaf(t, pvetest.LeafOptions{}))

	body := strings.Repeat("x", 3*64<<10)
	_, err := post(client(ca, false, 0), s.URL+"/api2/json/pools", body)
	if err == nil {
		t.Fatal("POST to a hijacking server must fail")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("POST error = %v, want EOF or a reset", err)
	}
	if s.Counts.Method("POST") != 1 {
		t.Fatalf("POST count = %d, want 1", s.Counts.Method("POST"))
	}
	seen := s.Seen()
	if len(seen) != 1 {
		t.Fatalf("Seen = %d requests, want 1", len(seen))
	}
	if len(seen[0].Body) != 64<<10 {
		t.Fatalf("Seen keeps the first 64 KiB of the body, got %d bytes", len(seen[0].Body))
	}
}

func TestReuseCloser_ServesFirstRequestThenClosesReusedConnection(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewReuseCloser(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	c := client(ca, true, 0)

	resp, err := get(c, s.URL+"/api2/json/version")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "9.0.2") {
		t.Fatalf("first body = %q", body)
	}

	// The hook closes the listener before the connection closes, so Go's
	// internal retry of the idempotent GET dials a refused address.
	var hookRan atomic.Bool
	s.SetOnReuse(func() {
		hookRan.Store(true)
		s.StopListening()
	})
	_, err = get(c, s.URL+"/api2/json/version")
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("second GET = %v, want the retry's refused dial", err)
	}
	if !hookRan.Load() || s.Counts.Reused.Load() != 1 {
		t.Fatalf("hook ran=%v Reused=%d", hookRan.Load(), s.Counts.Reused.Load())
	}
	if s.Counts.Requests.Load() != 1 {
		t.Fatalf("Requests = %d: the reused request must not be read", s.Counts.Requests.Load())
	}
	s.Wait(t, pvetest.EventReuse)
}

func TestReuseCloser_RetryReachesLiveListenerWithoutHook(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewReuseCloser(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	c := client(ca, true, 0)

	for i := range 2 {
		resp, err := get(c, s.URL+"/api2/json/version")
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		_ = readBody(t, resp)
	}
	if s.Counts.Reused.Load() != 1 || s.Counts.Conns.Load() != 2 || s.Counts.Requests.Load() != 2 {
		t.Fatalf("Reused=%d Conns=%d Requests=%d, want 1, 2, 2", s.Counts.Reused.Load(), s.Counts.Conns.Load(),
			s.Counts.Requests.Load())
	}
	s.SetOnReuse(nil)
}

func TestReuseCloser_HeadAnswerLeavesTheConnectionInSync(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewReuseCloser(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	c := client(ca, true, 0)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, s.URL+"/api2/json/version", nil)
	if err != nil {
		t.Fatalf("new HEAD: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if resp.ContentLength <= 0 || readBody(t, resp) != "" {
		t.Fatalf("HEAD answered Content-Length %d and a body, want a length and no body", resp.ContentLength)
	}
	// A body that leaked after the HEAD headers would reach the idle connection
	// as bytes nobody asked for, and the client would drop it. The reused
	// connection must still be there for the next request to find and close.
	resp, err = get(c, s.URL+"/api2/json/version")
	if err != nil {
		t.Fatalf("GET after HEAD: %v", err)
	}
	_ = readBody(t, resp)
	if s.Counts.Reused.Load() != 1 || s.Counts.Conns.Load() != 2 || s.Counts.Requests.Load() != 2 {
		t.Fatalf("Reused=%d Conns=%d Requests=%d, want 1, 2, 2", s.Counts.Reused.Load(), s.Counts.Conns.Load(),
			s.Counts.Requests.Load())
	}
}

func TestLive_CountsARequestBeforeItsBodyArrives(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	tr := &http.Transport{TLSClientConfig: ca.ClientConfig("localhost"), ExpectContinueTimeout: waitFailsafe}
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)

	// Expect: 100-continue makes the client flush the headers and hold the body
	// until the server reads it, so the body stalls with no bytes sent at all.
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/api2/json/pools", pr)
	if err != nil {
		t.Fatalf("new POST: %v", err)
	}
	req.ContentLength = int64(len("poolid=a"))
	req.Header.Set("Expect", "100-continue")
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		_ = readBody(t, resp)
		done <- result{code: resp.StatusCode}
	}()

	ev := s.Wait(t, pvetest.EventRequest)
	if ev.Request.Method != "POST" || s.Counts.Requests.Load() != 1 || s.Counts.Method("POST") != 1 {
		t.Fatalf("event %+v, Requests=%d: a stalled upload must show before its body", ev, s.Counts.Requests.Load())
	}
	if seen := s.Seen(); len(seen) != 1 || len(seen[0].Body) != 0 {
		t.Fatalf("Seen = %+v, want the request with no body yet", seen)
	}

	if _, err = pw.Write([]byte("poolid=a")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	_ = pw.Close()
	if r := <-done; r.err != nil || r.code != 200 {
		t.Fatalf("POST = %+v", r)
	}
	if seen := s.Seen(); len(seen) != 1 || string(seen[0].Body) != "poolid=a" {
		t.Fatalf("Seen after the body = %+v, want the body filled in", seen)
	}
}

func TestTotalMethodSumsAcrossServers(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	leaf := ca.NewLeaf(t, pvetest.LeafOptions{})
	a, b := pvetest.NewLive(t, leaf, nil), pvetest.NewLive(t, leaf, nil)
	c := client(ca, false, 0)
	for _, s := range []*pvetest.Server{a, b, b} {
		resp, err := post(c, s.URL+"/api2/json/pools", "")
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = readBody(t, resp)
	}
	if got := pvetest.TotalMethod("POST", a, b); got != 3 {
		t.Fatalf("TotalMethod = %d, want 3", got)
	}
	if pvetest.TotalMethod("GET", a, b) != 0 {
		t.Fatal("TotalMethod(GET) must be 0")
	}
}

const waitFailsafe = 30 * time.Second

func TestWaitFailsWhenServerCloses(t *testing.T) {
	t.Parallel()
	s := pvetest.NewRefused(t)
	s.Close()
	ft := &fakeTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		s.Wait(ft, pvetest.EventRequest)
	}()
	if !ft.failed.Load() {
		t.Fatal("Wait on a closed server must fail the test")
	}
}
