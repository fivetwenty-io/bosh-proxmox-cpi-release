package endpointpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// redirectRoute is the API path the redirecting endpoint answers on.
const redirectRoute = "/nodes/pve1/qemu"

// redirectTarget is a listener outside the pool that a redirect points at. It
// counts every request it reads and keeps the Authorization header of each.
type redirectTarget struct {
	url      string
	requests atomic.Int32
	auth     atomic.Value
}

// plainTarget starts a plain HTTP listener, the kind of address a redirect
// that changes the scheme and the port names.
func plainTarget(t *testing.T) *redirectTarget {
	t.Helper()
	target := &redirectTarget{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.requests.Add(1)
		target.auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	target.url = srv.URL
	return target
}

// redirecting returns routes whose redirectRoute answers every method with
// status and a Location of to.
func redirecting(status int, to string) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.HandleFunc("", redirectRoute, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, status)
	})
	return routes
}

// followed sends one request through the default redirect policy of an
// http.Client that uses p, which is how the SDK sends.
func followed(t *testing.T, p *Pool, method, addr string) (*http.Response, error) {
	t.Helper()
	req := apiRequest(t, context.Background(), method, addr, redirectRoute, bodyFor(method))
	req.Header.Set("Authorization", "PVEAPIToken=root@pam!cpi=SECRET")
	return (&http.Client{Transport: p}).Do(req)
}

func bodyFor(method string) string {
	if method == http.MethodPost {
		return "vmid=101"
	}
	return ""
}

// A redirect is refused for a read and for a mutation, whether it is a 302 or a
// 307 and whether it points at a plain address or at another TLS listener. The
// listener it names sees no request, the endpoint that sent the redirect saw
// only the first leg, and neither the active endpoint nor any demotion changes.
func TestRedirectFollowUpIsRefused(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
			for _, secure := range []bool{false, true} {
				name := fmt.Sprintf("%s %d secure=%v", method, status, secure)
				t.Run(name, func(t *testing.T) { mustRefuseRedirect(t, method, status, secure) })
			}
		}
	}
}

// mustRefuseRedirect makes a request that a pool endpoint answers with status
// and a Location on another listener, which is TLS when secure is set and
// plain HTTP otherwise, and checks that the client's follow-up is refused.
func mustRefuseRedirect(t *testing.T, method string, status int, secure bool) {
	t.Helper()
	l := newLab(t)
	targetURL, hits := "", func() int32 { return 0 }
	if secure {
		other := l.live(t, nil)
		targetURL, hits = other.URL, other.Counts.Requests.Load
	} else {
		target := plainTarget(t)
		targetURL, hits = target.url, target.requests.Load
	}
	a := l.live(t, redirecting(status, targetURL+pvetest.APIPrefix+"/version"))
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	resp, err := followed(t, p, method, a.Addr)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("err = %v, want ErrRedirectRefused", err)
	}
	if !strings.Contains(err.Error(), a.Addr) {
		t.Fatalf("err = %v, want it to name the host %s that sent the redirect", err, a.Addr)
	}
	if n := hits(); n != 0 {
		t.Fatalf("the redirect target read %d requests, want 0", n)
	}
	if n := a.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the redirecting endpoint read %d requests, want only the first leg", n)
	}
	if n := b.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the other endpoint got %d connections, want 0", n)
	}
	mustActive(t, p, a.Addr)
	mustDemoted(t, p, a.Addr, false)
	mustDemoted(t, p, b.Addr, false)
}

// followUp builds the request Go's client builds for a redirect from prior: a
// request with Response set to the answer that carried the Location.
func followUp(t *testing.T, method, addr, fromAddr string) *http.Request {
	t.Helper()
	first := apiRequest(t, context.Background(), method, fromAddr, redirectRoute, bodyFor(method))
	req := apiRequest(t, context.Background(), method, addr, redirectRoute, bodyFor(method))
	req.Response = &http.Response{StatusCode: http.StatusTemporaryRedirect, Request: first}
	return req
}

// A refused follow-up is turned away before any tether claim or dial. It claims
// no tether, leaves the tethers a first leg made alone, closes the body it was
// given, opens no connection to the address it names, and does not demote an
// endpoint it names even when that endpoint would refuse the dial.
func TestRedirectFollowUpTouchesNoStateAndNoConnection(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			l := newLab(t)
			a := l.live(t, nil)
			down := l.refused(t)
			p := l.pool(t, 0, configured(a.Addr), configured(down.Addr))
			unlisted := l.live(t, nil)

			for _, to := range []string{down.Addr, unlisted.Addr} {
				req := followUp(t, method, to, a.Addr)
				tracker := &closeTracker{Reader: strings.NewReader("vmid=101")}
				req.Body = tracker
				resp, err := p.RoundTrip(req)
				if resp != nil {
					t.Fatalf("to %s: got a response, want none", to)
				}
				if !errors.Is(err, ErrRedirectRefused) {
					t.Fatalf("to %s: err = %v, want ErrRedirectRefused", to, err)
				}
				if IsConnectPhase(err) || IsMaybeSent(err) {
					t.Fatalf("to %s: err = %v, want it unmarked", to, err)
				}
				if !tracker.closed.Load() {
					t.Fatalf("to %s: the refused request's body was not closed", to)
				}
			}
			p.tethers.mu.Lock()
			held := len(p.tethers.byKey)
			p.tethers.mu.Unlock()
			if held != 0 {
				t.Fatalf("the pool holds %d tether keys after refusals, want 0", held)
			}
			if down.Counts.Dials.Load() != 0 || unlisted.Counts.Conns.Load() != 0 || a.Counts.Conns.Load() != 0 {
				t.Fatalf("dials: down=%d unlisted=%d a=%d, want none", down.Counts.Dials.Load(),
					unlisted.Counts.Conns.Load(), a.Counts.Conns.Load())
			}
			mustActive(t, p, a.Addr)
			mustDemoted(t, p, down.Addr, false)
		})
	}
}

// A refused follow-up leaves a tether the first leg fixed exactly as it was,
// so a later mutation with the same key still goes to that endpoint.
func TestRedirectFollowUpLeavesAFixedTetherAlone(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()

	resp, err := send(t, ctx, p, http.MethodPost, a.Addr, redirectRoute, "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	p.tethers.mu.Lock()
	before := len(p.tethers.byKey)
	p.tethers.mu.Unlock()
	if before != 1 {
		t.Fatalf("the first leg left %d tether keys, want 1", before)
	}

	req := followUp(t, http.MethodPost, b.Addr, a.Addr)
	req = req.WithContext(connectContext(ctx, http.MethodPost, redirectRoute))
	if _, err := p.RoundTrip(req); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("err = %v, want ErrRedirectRefused", err)
	}
	p.tethers.mu.Lock()
	var kept []*tether
	for _, list := range p.tethers.byKey {
		kept = append(kept, list...)
	}
	p.tethers.mu.Unlock()
	if len(kept) != 1 || kept[0].target.addr != a.Addr || !kept[0].fixed || kept[0].conflicted {
		t.Fatalf("tethers after the refusal = %+v, want the one fixed on %s", kept, a.Addr)
	}
	if b.Counts.Conns.Load() != 0 {
		t.Fatalf("the refused follow-up opened %d connections to %s", b.Counts.Conns.Load(), b.Addr)
	}
}

// Requests whose Response is nil, the only kind a caller or the SDK builds, are
// unchanged. A read, a mutation, and a routed request each get their answer,
// and the guard wrapper hands the same request to its base without a change.
func TestRequestWithoutResponseIsUnchanged(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	r := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	mustOK(t, p, http.MethodGet, a.Addr, "/version", "")
	mustOK(t, p, http.MethodPost, a.Addr, redirectRoute, "vmid=101")
	mustOK(t, p, http.MethodGet, r.Addr, "/version", "")
	if r.Counts.Requests.Load() != 1 || a.Counts.Requests.Load() != 2 {
		t.Fatalf("requests: a=%d r=%d, want 2 and 1", a.Counts.Requests.Load(), r.Counts.Requests.Load())
	}

	var seen *http.Request
	guard := RefuseRedirects(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}))
	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/version", "")
	resp, err := guard.RoundTrip(req)
	mustStatus(t, resp, err, http.StatusOK)
	if seen != req {
		t.Fatal("the guard did not pass the caller's request to its base")
	}
	if RefuseRedirects(nil) != nil {
		t.Fatal("RefuseRedirects(nil) returned a round tripper")
	}
}

// The guard refuses a follow-up before its base sees it, closes its body, and
// passes CloseIdleConnections on.
func TestRefuseRedirectsGuard(t *testing.T) {
	var sent, closed atomic.Int32
	guard := RefuseRedirects(&closingTripper{
		roundTripFunc: func(*http.Request) (*http.Response, error) {
			sent.Add(1)
			return nil, errors.New("the base must not see a redirect")
		},
		closed: &closed,
	})
	req := followUp(t, http.MethodPost, "elsewhere:8080", "pve1:8006")
	tracker := &closeTracker{Reader: strings.NewReader("vmid=101")}
	req.Body = tracker
	if _, err := guard.RoundTrip(req); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("err = %v, want ErrRedirectRefused", err)
	}
	if sent.Load() != 0 || !tracker.closed.Load() {
		t.Fatalf("base saw %d requests, body closed = %v", sent.Load(), tracker.closed.Load())
	}
	guard.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	if closed.Load() != 1 {
		t.Fatalf("CloseIdleConnections reached the base %d times, want 1", closed.Load())
	}
}

// A follow-up whose answering request is missing still gets a typed refusal
// that names no host, and a nil request is a defect the dispatcher reports.
func TestRedirectRefusalWithoutAPriorRequest(t *testing.T) {
	req := apiRequest(t, context.Background(), http.MethodGet, "pve1:8006", "/version", "")
	req.Response = &http.Response{StatusCode: http.StatusFound}
	err := refuseRedirect(req)
	if !errors.Is(err, ErrRedirectRefused) || !strings.Contains(err.Error(), "an unknown host") {
		t.Fatalf("err = %v, want a refusal naming an unknown host", err)
	}
	if refuseRedirect(nil) != nil {
		t.Fatal("refuseRedirect(nil) refused")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// closingTripper is a round tripper that counts CloseIdleConnections calls.
type closingTripper struct {
	roundTripFunc
	closed *atomic.Int32
}

func (c *closingTripper) CloseIdleConnections() { c.closed.Add(1) }
