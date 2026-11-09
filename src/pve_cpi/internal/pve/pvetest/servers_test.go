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
	s.Wait(t, pvetest.EventConn)
}

func TestLive_FailRouteAndRequestHook(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/cluster/resources", 503, "overloaded")
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), routes)
	c := client(ca, false, 0)

	entered, release := make(chan struct{}), make(chan struct{})
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
	}
	done := make(chan result, 1)
	go func() {
		resp, err := get(c, s.URL+"/api2/json/cluster/resources")
		if err != nil {
			done <- result{err: err}
			return
		}
		_ = readBody(t, resp)
		done <- result{code: resp.StatusCode}
	}()
	<-entered
	select {
	case r := <-done:
		t.Fatalf("response arrived while the hook held it: %+v", r)
	default:
	}
	close(release)
	if r := <-done; r.err != nil || r.code != 503 {
		t.Fatalf("held response = %+v, want 503", r)
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

func TestRefused_DialIsRefused(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewRefused(t)

	_, err := get(client(ca, false, 0), s.URL+"/api2/json/version")
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("GET = %v, want a refused dial", err)
	}
	if s.Counts.Conns.Load() != 0 || s.Counts.Requests.Load() != 0 {
		t.Fatalf("refused server counted Conns=%d Requests=%d", s.Counts.Conns.Load(), s.Counts.Requests.Load())
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
	// The handshake can only end when the client closes its side, because the
	// server never writes. Closing the connection here is the client giving up.
	select {
	case err := <-hs:
		t.Fatalf("handshake ended with no server reply: %v", err)
	default:
	}
	_ = conn.Close()
	if err := <-hs; err == nil {
		t.Fatal("handshake must fail once the client gives up")
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

	errc := make(chan error, 1)
	go func() {
		_, err := get(c, s.URL+"/api2/json/cluster/resources")
		errc <- err
	}()
	s.Wait(t, pvetest.EventRequest)
	select {
	case err := <-errc:
		t.Fatalf("hung server answered or dropped the request: %v", err)
	default:
	}
	s.Close()
	if err := <-errc; err == nil {
		t.Fatal("closing the server must fail the waiting request")
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
