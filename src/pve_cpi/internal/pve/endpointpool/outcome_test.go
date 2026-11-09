package endpointpool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
)

// timeoutError is a net.Error that timed out, the shape of a response-header
// deadline expiry.
type timeoutError struct{}

func (timeoutError) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestClassOf(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   reqClass
	}{
		{"", "/api2/json/version", classIdempotent},
		{http.MethodGet, "/api2/json/version", classIdempotent},
		{http.MethodHead, "/api2/json/version", classIdempotent},
		{http.MethodOptions, "/api2/json/version", classIdempotent},
		{http.MethodPost, "/api2/json/access/ticket", classLogin},
		{http.MethodPost, "/api2/json/nodes/pve1/qemu", classMutation},
		{http.MethodPut, "/api2/json/access/ticket", classMutation},
		{http.MethodDelete, "/api2/json/pools/x", classMutation},
		{http.MethodTrace, "/api2/json/version", classMutation},
	}
	for _, c := range cases {
		req := &http.Request{Method: c.method, URL: &url.URL{Path: c.path}}
		if got := classOf(req); got != c.want {
			t.Errorf("classOf(%s %s) = %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

// The outcome table's error rows, checked in order: the send-by refusal, the
// caller's own context, a connect-phase failure, an untagged error before any
// connection, the header deadline, and any other transport error.
func TestClassifyErrors(t *testing.T) {
	dial := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: syscall.ECONNREFUSED}
	verify := &ConnectPhaseError{Phase: PhaseTLSVerify, Endpoint: "a:8006", Err: errors.New("x509: bad name")}
	plaintext := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: errPlaintextRefused}
	sendBy := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: ErrSendByPassed}
	reset := errors.New("read: connection reset by peer")

	cases := []struct {
		name string
		a    attempt
		want outcome
	}{
		{
			"send-by refusal of a read",
			attempt{class: classIdempotent, err: sendBy},
			outcome{action: actReturn, sent: sentNever, sendBy: true, class: classSendBy},
		},
		{
			"send-by refusal of a mutation after a connection",
			attempt{class: classMutation, err: sendBy, gotConn: true, callerDone: true},
			outcome{action: actReturn, sent: sentNever, sendBy: true, class: classSendBy},
		},
		{
			"caller gave up before a connection, even with the dial's tag",
			attempt{class: classMutation, err: dial, callerDone: true},
			outcome{action: actReturn, sent: sentNever, class: classCancelled},
		},
		{
			"caller gave up on a read after the connection",
			attempt{class: classIdempotent, err: context.Canceled, callerDone: true, gotConn: true},
			outcome{action: actReturn, sent: sentMaybe, class: classCancelled},
		},
		{
			"caller gave up on a mutation after the connection",
			attempt{class: classMutation, err: context.DeadlineExceeded, callerDone: true, gotConn: true},
			outcome{action: actReturn, sent: sentMaybe, demote: true, class: classCancelled},
		},
		{
			"dial failure of a read",
			attempt{class: classIdempotent, kind: KindConfigured, err: dial},
			outcome{action: actHop, demote: true, sent: sentNever, unreachable: true, class: "dial"},
		},
		{
			"dial failure of the login",
			attempt{class: classLogin, kind: KindConfigured, err: dial},
			outcome{action: actHop, demote: true, sent: sentNever, unreachable: true, class: "dial"},
		},
		{
			"dial failure of a mutation proven unsent",
			attempt{class: classMutation, err: dial, provenUnsent: true},
			outcome{action: actHop, demote: true, sent: sentNever, unreachable: true, class: "dial"},
		},
		{
			"dial failure of a mutation Go may have replayed",
			attempt{class: classMutation, err: dial},
			outcome{action: actReturn, demote: true, sent: sentMaybe, class: "dial"},
		},
		{
			"verification failure of a configured endpoint",
			attempt{class: classIdempotent, kind: KindConfigured, err: verify},
			outcome{
				action: actHop, demote: true, sent: sentNever, unreachable: true, verifyAlert: true,
				class: "tls_verify",
			},
		},
		{
			"verification failure of a discovered endpoint",
			attempt{class: classIdempotent, kind: KindDiscovered, err: verify},
			outcome{action: actHop, demote: true, sent: sentNever, unreachable: true, class: "tls_verify"},
		},
		{
			"plain http refused by the pool hops without blame",
			attempt{class: classIdempotent, kind: KindDiscovered, err: plaintext},
			outcome{action: actHop, sent: sentNever, unreachable: true, class: "dial"},
		},
		{
			"untagged error before any connection",
			attempt{class: classIdempotent, err: errors.New("unsupported protocol scheme")},
			outcome{action: actReturn, sent: sentNever, class: classRequest},
		},
		{
			"header deadline on a hop that had one",
			attempt{class: classIdempotent, err: timeoutError{}, gotConn: true, withDeadline: true},
			outcome{action: actHop, demote: true, sent: sentMaybe, unreachable: true, class: classHeaderDeadline},
		},
		{
			"timeout on a hop without the deadline is a transport error",
			attempt{class: classIdempotent, err: timeoutError{}, gotConn: true},
			outcome{action: actHop, demote: true, sent: sentMaybe, class: classTransport},
		},
		{
			"reset of a read",
			attempt{class: classIdempotent, err: reset, gotConn: true},
			outcome{action: actHop, demote: true, sent: sentMaybe, class: classTransport},
		},
		{
			"reset of the login",
			attempt{class: classLogin, err: reset, gotConn: true},
			outcome{action: actHop, demote: true, sent: sentMaybe, class: classTransport},
		},
		{
			"reset of a mutation",
			attempt{class: classMutation, err: reset, gotConn: true},
			outcome{action: actReturn, demote: true, sent: sentMaybe, class: classTransport},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.a); got != c.want {
				t.Fatalf("classify = %+v, want %+v", got, c.want)
			}
		})
	}
}

// The outcome table's status rows. A mutation never moves, demotes, or
// promotes on a status.
func TestClassifyStatuses(t *testing.T) {
	answer := outcome{action: actAnswer, sent: sentAnswered}
	ret := outcome{action: actReturn, sent: sentAnswered, class: classStatus}
	statusHop := outcome{action: actHop, statusHop: true, demote: true, sent: sentAnswered, class: classStatus}
	broken := outcome{action: actReturn, demote: true, sent: sentAnswered, class: classStatus}

	cases := []struct {
		name string
		a    attempt
		want outcome
	}{
		{"200 read", attempt{class: classIdempotent, status: 200, attributable: true}, answer},
		{"404 read", attempt{class: classIdempotent, status: 404}, answer},
		{"500 read", attempt{class: classIdempotent, status: 500, attributable: true}, answer},
		{"504 read", attempt{class: classIdempotent, status: 504, attributable: true}, answer},
		{"200 login", attempt{class: classLogin, status: 200, attributable: true}, answer},
		{"502 read from the entry", attempt{class: classIdempotent, status: 502, attributable: true}, statusHop},
		{"503 read from the entry", attempt{class: classIdempotent, status: 503, attributable: true}, statusHop},
		{"595 read from the entry", attempt{class: classIdempotent, status: 595, attributable: true}, statusHop},
		{"595 login from the entry", attempt{class: classLogin, status: 595, attributable: true}, statusHop},
		{"503 read for another node", attempt{class: classIdempotent, status: 503}, ret},
		{"595 read for another node", attempt{class: classIdempotent, status: 595}, ret},
		{
			"second 503 after the status hop",
			attempt{class: classIdempotent, status: 503, attributable: true, statusHopped: true}, ret,
		},
		{"596 read", attempt{class: classIdempotent, status: 596, attributable: true}, broken},
		{"596 login", attempt{class: classLogin, status: 596}, broken},
		{"597 read", attempt{class: classIdempotent, status: 597, attributable: true}, ret},
		{"599 read", attempt{class: classIdempotent, status: 599, attributable: true}, ret},
		{"600 read", attempt{class: classIdempotent, status: 600, attributable: true}, answer},
		{"200 mutation", attempt{class: classMutation, status: 200, attributable: true}, ret},
		{"503 mutation from the entry", attempt{class: classMutation, status: 503, attributable: true}, ret},
		{"595 mutation from the entry", attempt{class: classMutation, status: 595, attributable: true}, ret},
		{"596 mutation", attempt{class: classMutation, status: 596, attributable: true}, ret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.a); got != c.want {
				t.Fatalf("classify = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestNoteOf(t *testing.T) {
	dial := &ConnectPhaseError{Phase: PhaseDial, Endpoint: "a:8006", Err: errors.New("dial tcp: i/o timeout")}
	if n := noteOf(nil, dial, "dial"); n.text != "dial tcp: i/o timeout" || n.class != "dial" {
		t.Fatalf("connect-phase note = %+v", n)
	}
	if n := noteOf(nil, &ConnectPhaseError{Phase: PhaseDial}, "dial"); n.text != "connect phase dial failed" {
		t.Fatalf("connect-phase note without a cause = %+v", n)
	}
	if n := noteOf(nil, errors.New("EOF"), classTransport); n.text != "EOF" {
		t.Fatalf("transport note = %+v", n)
	}
	resp := &http.Response{Status: "503 Service Unavailable", StatusCode: 503}
	if n := noteOf(resp, nil, classStatus); n.text != "HTTP 503 Service Unavailable" {
		t.Fatalf("status note = %+v", n)
	}
	if n := noteOf(nil, nil, ""); n.text != "unknown failure" {
		t.Fatalf("empty note = %+v", n)
	}
}

// trackedBody records whether it was closed.
type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestHoldResponseKeepsASmallBody(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(`{"data":null}`)}
	resp := &http.Response{StatusCode: 503, Body: body, ContentLength: -1, TransferEncoding: []string{"chunked"}}
	held := holdResponse(resp)
	if held == nil {
		t.Fatal("holdResponse dropped a small body")
	}
	if !body.closed {
		t.Fatal("holdResponse left the original body open")
	}
	req := &http.Request{Method: http.MethodGet}
	got := held.response(req)
	if got.Request != req || got.ContentLength != int64(len(`{"data":null}`)) || got.TransferEncoding != nil {
		t.Fatalf("held response = %+v", got)
	}
	b, err := io.ReadAll(got.Body)
	if err != nil || string(b) != `{"data":null}` {
		t.Fatalf("held body = %q, %v", b, err)
	}
}

func TestHoldResponseDropsWhatItCannotKeep(t *testing.T) {
	big := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", heldBodyLimit+1))}
	if holdResponse(&http.Response{Body: big}) != nil {
		t.Fatal("holdResponse kept a body over the limit")
	}
	if !big.closed {
		t.Fatal("holdResponse left an oversized body open")
	}
	broken := &trackedBody{Reader: iotest.ErrReader(errors.New("read failed"))}
	if holdResponse(&http.Response{Body: broken}) != nil {
		t.Fatal("holdResponse kept a body it could not read")
	}
	if !broken.closed {
		t.Fatal("holdResponse left an unreadable body open")
	}
}
