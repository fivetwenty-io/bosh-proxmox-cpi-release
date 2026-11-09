package endpointpool

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// failing returns routes that answer method on path with status and reason,
// and serve the defaults otherwise.
func failing(method, path string, status int, reason string) *pvetest.Routes {
	r := pvetest.NewRoutes()
	r.Fail(method, path, status, reason)
	return r
}

// A 503 from the entry moves a read once. The second 503 comes back as it is,
// and the third endpoint is never tried.
func TestStatusHopIsOncePerRequest(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy-a"))
	b := l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy-b"))
	c := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr), configured(c.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
	body := mustStatus(t, resp, err, http.StatusServiceUnavailable)
	if !strings.Contains(body, "busy-b") {
		t.Fatalf("body = %q, want B's answer", body)
	}
	if a.Counts.Requests.Load() != 1 || b.Counts.Requests.Load() != 1 || c.Counts.Requests.Load() != 0 {
		t.Fatalf("requests A=%d B=%d C=%d, want 1, 1, 0",
			a.Counts.Requests.Load(), b.Counts.Requests.Load(), c.Counts.Requests.Load())
	}
	mustDemoted(t, p, a.Addr, true)
	mustDemoted(t, p, b.Addr, false)
	mustActive(t, p, a.Addr)
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0", n)
	}
}

// A 502 from the entry moves a read to an endpoint that answers, which becomes
// active.
func TestStatusHopReachesAnAnswer(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusBadGateway, "proxy"))
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, b.Addr)
	warns := l.failovers()
	if len(warns) != 1 || !strings.Contains(warns[0].Message, "(HTTP 502 Bad Gateway)") {
		t.Fatalf("failover lines = %+v", warns)
	}
	mustAttrs(t, warns[0], map[string]string{"class": classStatus})
}

// A 503 for another node's path comes back with no hop and no effect.
func TestStatusForAnotherNodeDoesNotHop(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/nodes/pve9/status", http.StatusServiceUnavailable, "busy"))
	b := l.live(t, nil)
	p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/nodes/pve9/status", "")
	mustStatus(t, resp, err, http.StatusServiceUnavailable)
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)
}

// A 595 on a path that names no node moves a read and the login, and never a
// mutation.
func TestStatus595OnTheEntry(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		hops   bool
	}{
		{"read", http.MethodGet, "/cluster/resources", "", true},
		{"login", http.MethodPost, "/access/ticket", "username=root&password=secret", true},
		{"mutation", http.MethodPost, "/cluster/backup", "starttime=00:00", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			a := l.live(t, failing(c.method, c.path, statusNoNextHop, "no next hop"))
			b := l.live(t, nil)
			p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
			resp, err := send(t, context.Background(), p, c.method, a.Addr, c.path, c.body)
			if c.hops {
				mustStatus(t, resp, err, http.StatusOK)
				mustDemoted(t, p, a.Addr, true)
				mustActive(t, p, b.Addr)
				if c.body != "" && string(b.Seen()[0].Body) != c.body {
					t.Fatalf("B saw body %q, want %q", b.Seen()[0].Body, c.body)
				}
				return
			}
			mustStatus(t, resp, err, statusNoNextHop)
			if n := b.Counts.Requests.Load(); n != 0 {
				t.Fatalf("B got %d requests, want 0", n)
			}
			mustDemoted(t, p, a.Addr, false)
			mustActive(t, p, a.Addr)
		})
	}
}

// A 595 for another node hops nothing. One for the entry's own recorded node
// moves a read.
func TestStatus595ByNode(t *testing.T) {
	l := newLab(t)
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/nodes/pve2/qemu", statusNoNextHop, "pve2 down")
	routes.Fail(http.MethodGet, "/nodes/pve1/qemu", statusNoNextHop, "pvedaemon down")
	a := l.live(t, routes)
	b := l.live(t, nil)
	p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr))

	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/nodes/pve2/qemu", "")
	mustStatus(t, resp, err, statusNoNextHop)
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests for another node's 595, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)

	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/nodes/pve1/qemu", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Requests.Load(); n != 1 {
		t.Fatalf("B got %d requests for the entry's own 595, want 1", n)
	}
	mustDemoted(t, p, a.Addr, true)
}

// A 596 is returned without a hop. It demotes the entry for a read and leaves
// it alone for a mutation.
func TestStatus596(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		body    string
		demoted bool
	}{
		{"read", http.MethodGet, "", true},
		{"mutation", http.MethodPost, "vmid=101", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			a := l.live(t, failing(c.method, "/cluster/resources", statusBrokenBackend, "broken"))
			b := l.live(t, nil)
			p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
			resp, err := send(t, context.Background(), p, c.method, a.Addr, "/cluster/resources", c.body)
			mustStatus(t, resp, err, statusBrokenBackend)
			if n := b.Counts.Requests.Load(); n != 0 {
				t.Fatalf("B got %d requests, want 0", n)
			}
			mustDemoted(t, p, a.Addr, c.demoted)
			mustActive(t, p, a.Addr)
		})
	}
}

// A 597 to 599 comes back with no effect, and a 4xx is an answer.
func TestStatusOtherProxyCodesAndAnswers(t *testing.T) {
	for _, status := range []int{597, 598, 599, http.StatusNotFound, http.StatusInternalServerError} {
		l := newLab(t)
		a := l.live(t, failing(http.MethodGet, "/cluster/resources", status, "status"))
		b := l.live(t, nil)
		p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
		resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
		mustStatus(t, resp, err, status)
		if n := b.Counts.Requests.Load(); n != 0 {
			t.Fatalf("status %d: B got %d requests, want 0", status, n)
		}
		mustDemoted(t, p, a.Addr, false)
	}
}

// A status hop with nowhere to go returns the response as it is, with no
// demotion.
func TestStatusHopWithNowhereToGo(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy"))
	p := l.pool(t, 0, configured(a.Addr))
	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/cluster/resources", "")
	resp, err := p.RoundTrip(req)
	if body := mustStatus(t, resp, err, http.StatusServiceUnavailable); !strings.Contains(body, "busy") {
		t.Fatalf("body = %q", body)
	}
	if resp.Request != req {
		t.Fatal("the response does not carry the caller's request")
	}
	mustDemoted(t, p, a.Addr, false)
}

// When a status hop leads only to endpoints that cannot be reached, the walk
// returns the response it moved away from, readable again.
func TestStatusHopReturnsTheHeldResponse(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy-a"))
	b := l.refused(t)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/cluster/resources", "")
	resp, err := p.RoundTrip(req)
	body := mustStatus(t, resp, err, http.StatusServiceUnavailable)
	if !strings.Contains(body, "busy-a") || resp.ContentLength != int64(len(body)) {
		t.Fatalf("held response body %q, length %d", body, resp.ContentLength)
	}
	if resp.Request != req {
		t.Fatal("the held response does not carry the caller's request")
	}
	if n := b.Counts.Dials.Load(); n != 1 {
		t.Fatalf("B was dialed %d times, want 1", n)
	}
	mustDemoted(t, p, a.Addr, true)
	mustDemoted(t, p, b.Addr, true)
}

// The SDK's retries of one request share its one status hop. The SDK sends
// every retry as the same *http.Request through an http.Client whose timeout
// gives each round trip a context of its own, so four round trips of one read
// against three endpoints that all answer 503 cost the SDK's four tries plus
// one status hop. After the hop the retries go to the endpoint that answered,
// and only the entry the status hop moved away from is demoted.
func TestStatusHopIsSharedAcrossSDKRetries(t *testing.T) {
	l := newLab(t)
	busy := func() *pvetest.Server {
		return l.live(t, failing(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy"))
	}
	a, b, c := busy(), busy(), busy()
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr), configured(c.Addr))
	client := &http.Client{Transport: p, Timeout: time.Minute}
	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/cluster/resources", "")
	for range 4 {
		resp, err := client.Do(req)
		mustStatus(t, resp, err, http.StatusServiceUnavailable)
	}
	sa, sb, sc := a.Counts.Requests.Load(), b.Counts.Requests.Load(), c.Counts.Requests.Load()
	if sa != 1 || sb != 4 || sc != 0 {
		t.Fatalf("sends A=%d B=%d C=%d, want 1, 4, 0 (five in all)", sa, sb, sc)
	}
	mustDemoted(t, p, a.Addr, true)
	mustDemoted(t, p, b.Addr, false)
	mustDemoted(t, p, c.Addr, false)

	// Another request has a status hop of its own.
	resp, err := client.Do(apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusServiceUnavailable)
	if n := c.Counts.Requests.Load(); n != 1 {
		t.Fatalf("C got %d requests, want the new request's status hop", n)
	}
}

// A 596 demotes the entry only when the entry caused it, by the same
// attribution as a 502, 503, or 595. One for another node's path says nothing
// about the entry. Neither hops.
func TestStatus596ByNode(t *testing.T) {
	cases := []struct {
		path    string
		demoted bool
	}{
		{"/nodes/pve1/status", true},
		{"/nodes/pve9/status", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			l := newLab(t)
			a := l.live(t, failing(http.MethodGet, c.path, statusBrokenBackend, "broken"))
			b := l.live(t, nil)
			p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr))
			resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, c.path, "")
			mustStatus(t, resp, err, statusBrokenBackend)
			if n := b.Counts.Requests.Load(); n != 0 {
				t.Fatalf("B got %d requests, want 0", n)
			}
			mustDemoted(t, p, a.Addr, c.demoted)
			mustActive(t, p, a.Addr)
		})
	}
}
