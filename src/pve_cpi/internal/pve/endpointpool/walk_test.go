package endpointpool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// lab is the set of test servers, dialers, and logger one walk test uses.
type lab struct {
	ca     *pvetest.CA
	leaf   *pvetest.Leaf
	d      *dialers
	logger *log.Logger
	obs    *log.Observer
}

func newLab(t *testing.T) *lab {
	t.Helper()
	ca := pvetest.NewCA(t)
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	return &lab{ca: ca, leaf: ca.NewLeaf(t, pvetest.LeafOptions{}), d: &dialers{}, logger: logger, obs: obs}
}

// live starts a live server with the lab's leaf. A nil routes serves the
// defaults.
func (l *lab) live(t *testing.T, routes *pvetest.Routes) *pvetest.Server {
	t.Helper()
	return pvetest.NewLive(t, l.leaf, routes)
}

// refused starts a Refused server and routes its address to its dial.
func (l *lab) refused(t *testing.T) *pvetest.Refused {
	t.Helper()
	r := pvetest.NewRefused(t)
	l.d.set(r.Addr, r.DialContext)
	return r
}

// dropped starts a Dropped server and routes its address to its dial.
func (l *lab) dropped(t *testing.T) *pvetest.Dropped {
	t.Helper()
	r := pvetest.NewDropped(t)
	l.d.set(r.Addr, r.DialContext)
	return r
}

// pool builds a pool over the endpoints, verifying against the lab's CA. A
// positive deadline replaces the 60-second response-header deadline.
func (l *lab) pool(t *testing.T, deadline time.Duration, eps ...Endpoint) *Pool {
	t.Helper()
	return newPool(t, Config{Base: baseFor(l.ca, l.d), Logger: l.logger, Endpoints: eps, headerDeadline: deadline})
}

// entries returns the log entries at level whose message starts with prefix.
func (l *lab) entries(level slog.Level, prefix string) []log.Entry {
	var out []log.Entry
	for _, e := range l.obs.All() {
		if e.Level == level && strings.HasPrefix(e.Message, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func (l *lab) failovers() []log.Entry { return l.entries(slog.LevelWarn, "pve endpoint failover: ") }

// readAll reads and closes a response body.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// mustStatus fails the test unless the round trip answered with status, and
// returns the body.
func mustStatus(t *testing.T, resp *http.Response, err error, status int) string {
	t.Helper()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, status, body)
	}
	return body
}

func mustActive(t *testing.T, p *Pool, addr string) {
	t.Helper()
	if active, _ := p.current(); active.addr != addr {
		t.Fatalf("active endpoint = %s, want %s", active.addr, addr)
	}
}

func mustDemoted(t *testing.T, p *Pool, addr string, want bool) {
	t.Helper()
	if got := p.demotedNow(endpointOf(t, p, addr)); got != want {
		t.Fatalf("%s demoted = %v, want %v", addr, got, want)
	}
}

func mustAttrs(t *testing.T, e log.Entry, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got, _ := e.Attrs[k].(string); got != v {
			t.Fatalf("attribute %s = %v, want %q (entry %q)", k, e.Attrs[k], v, e.Message)
		}
	}
}

// A read hops past a refused entry to the next endpoint, which becomes active
// and carries the next request directly. The change is logged once at Warn,
// and the response carries the caller's request.
func TestWalkHopsAReadPastARefusedEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/cluster/resources", "")
	resp, err := p.RoundTrip(req)
	mustStatus(t, resp, err, http.StatusOK)
	if resp.Request != req {
		t.Fatal("the response does not carry the caller's request")
	}
	if req.URL.Host != a.Addr {
		t.Fatalf("the caller's request was changed: URL host %s", req.URL.Host)
	}
	if got := b.Seen(); len(got) != 1 || got[0].Path != "/api2/json/cluster/resources" {
		t.Fatalf("B saw %+v", got)
	}
	mustActive(t, p, b.Addr)
	mustDemoted(t, p, a.Addr, true)

	warns := l.failovers()
	if len(warns) != 1 {
		t.Fatalf("failover lines = %d, want 1", len(warns))
	}
	want := "pve endpoint failover: " + a.Addr + " unreachable ("
	tail := "), request sent to " + b.Addr + ", which stays active for this call"
	if !strings.HasPrefix(warns[0].Message, want) || !strings.HasSuffix(warns[0].Message, tail) {
		t.Fatalf("failover line = %q", warns[0].Message)
	}
	if !strings.Contains(warns[0].Message, "connection refused") {
		t.Fatalf("failover line lacks the cause: %q", warns[0].Message)
	}
	mustAttrs(t, warns[0], map[string]string{"from": a.Addr, "to": b.Addr, "class": "dial"})

	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if dials := a.Counts.Dials.Load(); dials != 1 {
		t.Fatalf("A was dialed %d times, want 1", dials)
	}
	if n := b.Counts.Requests.Load(); n != 2 {
		t.Fatalf("B got %d requests, want 2", n)
	}
	if n := len(l.failovers()); n != 1 {
		t.Fatalf("failover lines after a request on the active endpoint = %d, want 1", n)
	}
}

// Each hop carries the endpoint's own spelling in its URL, so the Host header
// and SNI name the endpoint the hop went to.
func TestWalkHopUsesTheEndpointSpelling(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	_, port, err := net.SplitHostPort(b.Addr)
	if err != nil {
		t.Fatal(err)
	}
	named := "localhost:" + port
	l.d.set(named, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, b.Addr)
	})
	p := l.pool(t, 0, configured(a.Addr), configured(named))
	b.SetOnRequest(func(r *http.Request) {
		if r.Host != named {
			t.Errorf("Host = %q, want %q", r.Host, named)
		}
	})
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if got := b.Seen(); len(got) != 1 || got[0].ServerName != "localhost" {
		t.Fatalf("B saw %+v, want SNI localhost", got)
	}
}

// Six dead endpoints stop after four hops, with one dial-shaped error that
// lists the four.
func TestWalkStopsAfterFourHops(t *testing.T) {
	l := newLab(t)
	eps := make([]Endpoint, 0, 6)
	dead := make([]*pvetest.Refused, 0, 6)
	for range 6 {
		r := l.refused(t)
		dead = append(dead, r)
		eps = append(eps, configured(r.Addr))
	}
	p := l.pool(t, 0, eps...)
	_, err := send(t, context.Background(), p, http.MethodGet, dead[0].Addr, "/version", "")
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) {
		t.Fatalf("err = %T %v, want *EntryUnreachableError", err, err)
	}
	got := eu.Endpoints()
	if len(got) != 4 {
		t.Fatalf("hops = %v, want four", got)
	}
	for i, r := range dead {
		want := int32(0)
		if i < 4 {
			want = 1
			if got[i] != r.Addr {
				t.Fatalf("hop %d went to %s, want %s", i, got[i], r.Addr)
			}
		}
		if n := r.Counts.Dials.Load(); n != want {
			t.Fatalf("endpoint %d dialed %d times, want %d", i, n, want)
		}
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("err does not unwrap to a dial error: %v", err)
	}
	if !IsConnectPhase(err) || IsMaybeSent(err) {
		t.Fatalf("err lost its connect-phase proof: %v", err)
	}
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0 when nothing answered", n)
	}
}

// Dials that each take 14 seconds of connect time stop the walk at the
// 30-second connect budget after the third hop.
func TestWalkStopsAtTheConnectBudget(t *testing.T) {
	l := newLab(t)
	clock := newFakeClock()
	eps := make([]Endpoint, 0, 6)
	dials := make([]atomic.Int32, 6)
	for i := range dials {
		r := pvetest.NewRefused(t)
		l.d.set(r.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials[i].Add(1)
			clock.advance(14 * time.Second)
			return r.DialContext(ctx, network, addr)
		})
		eps = append(eps, configured(r.Addr))
	}
	p := l.pool(t, 0, eps...)
	req := apiRequest(t, context.Background(), http.MethodGet, eps[0].Address, "/version", "")
	_, err := p.roundTrip(req, boundsOn(clock))
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || len(eu.Attempts) != 3 {
		t.Fatalf("err = %v, want three hops", err)
	}
	for i, want := range []int32{1, 1, 1, 0, 0, 0} {
		if got := dials[i].Load(); got != want {
			t.Fatalf("endpoint %d dialed %d times, want %d", i, got, want)
		}
	}
}

// Four hung endpoints, each costing 40 seconds of walk time, stop at the
// 90-second walk budget after the third hop, so the final hop, which would
// wait with no deadline, never starts.
func TestWalkStopsAtTheWalkBudget(t *testing.T) {
	l := newLab(t)
	clock := newFakeClock()
	eps := make([]Endpoint, 0, 4)
	hung := make([]*pvetest.Server, 0, 4)
	for range 4 {
		s := pvetest.NewHung(t, l.leaf)
		s.SetOnRequest(func(*http.Request) { clock.advance(40 * time.Second) })
		hung = append(hung, s)
		eps = append(eps, configured(s.Addr))
	}
	p := l.pool(t, 50*time.Millisecond, eps...)
	req := apiRequest(t, context.Background(), http.MethodGet, hung[0].Addr, "/version", "")
	_, err := p.roundTrip(req, boundsOn(clock))
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || len(eu.Attempts) != 3 {
		t.Fatalf("err = %v, want three hops", err)
	}
	for i, s := range hung {
		want := int32(1)
		if i == 3 {
			want = 0
		}
		if n := s.Counts.Requests.Load(); n != want {
			t.Fatalf("hung endpoint %d got %d requests, want %d", i, n, want)
		}
	}
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		t.Fatalf("a walk of header-deadline expiries does not read as a dial error: %v", err)
	}
}

// A hung entry makes a read hop once the response-header deadline expires,
// and the entry is demoted.
func TestWalkHungEntryHopsARead(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewHung(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, 100*time.Millisecond, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
	mustStatus(t, resp, err, http.StatusOK)
	if a.Counts.Requests.Load() != 1 || b.Counts.Requests.Load() != 1 {
		t.Fatalf("requests A=%d B=%d, want 1 each", a.Counts.Requests.Load(), b.Counts.Requests.Load())
	}
	mustDemoted(t, p, a.Addr, true)
	mustActive(t, p, b.Addr)
	warns := l.failovers()
	if len(warns) != 1 {
		t.Fatalf("failover lines = %d, want 1", len(warns))
	}
	mustAttrs(t, warns[0], map[string]string{"class": classHeaderDeadline})
}

// The final hop carries no header deadline, so an endpoint that answers after
// the deadline still answers the read. The pool holds configured endpoints
// only, the first refuses, and the second answers after three deadlines.
func TestWalkFinalHopWaitsPastTheDeadline(t *testing.T) {
	l := newLab(t)
	const deadline = 100 * time.Millisecond
	a := l.refused(t)
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * deadline)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	b := l.live(t, routes)
	p := l.pool(t, deadline, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, b.Addr)
}

// A mutation never gets the header deadline and never hops after its
// connection exists, so a hung entry holds a POST until the caller gives up,
// and the next endpoint sees no POST.
func TestWalkHungEntryHoldsAMutation(t *testing.T) {
	l := newLab(t)
	const deadline = 100 * time.Millisecond
	a := pvetest.NewHung(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, deadline, configured(a.Addr), configured(b.Addr))
	ctx, cancel := context.WithTimeout(context.Background(), 4*deadline)
	defer cancel()
	start := time.Now()
	_, err := send(t, ctx, p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if elapsed := time.Since(start); elapsed < 4*deadline {
		t.Fatalf("the POST returned after %v, before the caller's deadline", elapsed)
	}
	if n := pvetest.TotalMethod(http.MethodPost, a, b); n != 1 {
		t.Fatalf("POSTs across servers = %d, want 1", n)
	}
	mustDemoted(t, p, a.Addr, true)
	mustActive(t, p, a.Addr)
}

// A read of another node's path uses the clone with no header deadline,
// because the entry cannot be blamed for that node's stall.
func TestWalkOtherNodePathHasNoDeadline(t *testing.T) {
	l := newLab(t)
	const deadline = 100 * time.Millisecond
	a := pvetest.NewHung(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, deadline, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr))
	ctx, cancel := context.WithTimeout(context.Background(), 4*deadline)
	defer cancel()
	_, err := send(t, ctx, p, http.MethodGet, a.Addr, "/nodes/pve2/status", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)
}

// With the default 60-second deadline, only an attributable read that is not
// on the walk's final hop takes the clone that carries it.
func TestWalkDeadlineChoice(t *testing.T) {
	l := newLab(t)
	a, b, c := l.live(t, nil), l.live(t, nil), l.live(t, nil)
	p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr),
		configured(c.Addr))
	ep := endpointOf(t, p, a.Addr)
	if got := ep.transports.pick(true).ResponseHeaderTimeout; got != 60*time.Second {
		t.Fatalf("idempotent clone deadline = %v, want 60s", got)
	}
	if got := ep.transports.pick(false).ResponseHeaderTimeout; got != 0 {
		t.Fatalf("plain clone deadline = %v, want none", got)
	}
	cases := []struct {
		method string
		path   string
		hop    int
		want   bool
	}{
		{http.MethodGet, "/cluster/resources", 0, true},
		{http.MethodGet, "/cluster/resources", 1, true},
		{http.MethodGet, "/cluster/resources", 2, false},
		{http.MethodHead, "/version", 0, true},
		{http.MethodGet, "/nodes/pve1/status", 0, true},
		{http.MethodGet, "/nodes/pve2/status", 0, false},
		{http.MethodPost, "/access/ticket", 0, false},
		{http.MethodPost, "/nodes/pve1/qemu", 0, false},
		{http.MethodDelete, "/pools/x", 0, false},
	}
	for _, c := range cases {
		req := apiRequest(t, context.Background(), c.method, a.Addr, c.path, "")
		w := p.newWalker(req, false, defaultBounds())
		if got := w.wantsDeadline(c.hop, attributable(req.URL.Path, ep)); got != c.want {
			t.Errorf("%s %s hop %d: deadline = %v, want %v", c.method, c.path, c.hop, got, c.want)
		}
	}
}

// A slow read that started on A before a failover answers after the walk has
// moved to B, and the generation guard keeps B active.
func TestWalkGenerationGuardKeepsTheNewEndpoint(t *testing.T) {
	l := newLab(t)
	release := make(chan struct{})
	routes := pvetest.NewRoutes()
	routes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
	a := l.live(t, routes)
	a.SetOnRequest(func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			<-release
		}
	})
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	type result struct {
		resp *http.Response
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
		slow <- result{resp, err}
	}()
	if ev := a.Wait(t, pvetest.EventRequest); !strings.HasSuffix(ev.Request.Path, "/version") {
		t.Fatalf("first request on A = %s, want /version", ev.Request.Path)
	}

	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/cluster/resources", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, b.Addr)

	close(release)
	r := <-slow
	mustStatus(t, r.resp, r.err, http.StatusOK)
	mustActive(t, p, b.Addr)
	mustDemoted(t, p, a.Addr, true)
	if n := len(l.failovers()); n != 1 {
		t.Fatalf("failover lines = %d, want 1", n)
	}
}

// A configured endpoint whose certificate fails verification is skipped, and
// the failure is logged at Error with the tls_verify class once per endpoint.
func TestWalkConfiguredVerificationFailureLogsAtError(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"other.example"}}), nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	errs := l.entries(slog.LevelError, "pve endpoint "+a.Addr+": TLS verification failed (")
	if len(errs) != 1 {
		t.Fatalf("verification lines = %d, want 1", len(errs))
	}
	if !strings.HasSuffix(errs[0].Message, "; this can mean interception; trying the next endpoint") {
		t.Fatalf("verification line = %q", errs[0].Message)
	}
	mustAttrs(t, errs[0], map[string]string{"endpoint": a.Addr, "class": "tls_verify"})
	if n := a.Counts.Requests.Load(); n != 0 {
		t.Fatalf("A got %d requests past a failed verification", n)
	}

	_, gen := p.current()
	if !p.promote(endpointOf(t, p, a.Addr), gen) {
		t.Fatal("promote A")
	}
	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := len(l.entries(slog.LevelError, "pve endpoint ")); n != 1 {
		t.Fatalf("verification lines after a second failure = %d, want 1", n)
	}
	warns := l.failovers()
	if len(warns) != 2 {
		t.Fatalf("failover lines = %d, want one per change", len(warns))
	}
	mustAttrs(t, warns[1], map[string]string{"from": a.Addr, "to": b.Addr, "class": "tls_verify"})
}

// When every endpoint fails verification, the walk surfaces the certificate
// error, not a dial error.
func TestWalkAllVerificationFailuresSurfaceTheCertificateError(t *testing.T) {
	l := newLab(t)
	bad := l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"other.example"}})
	a, b := pvetest.NewLive(t, bad, nil), pvetest.NewLive(t, bad, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	_, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || len(eu.Attempts) != 2 {
		t.Fatalf("err = %v, want two hops", err)
	}
	var op *net.OpError
	if errors.As(err, &op) {
		t.Fatalf("an all-verification walk reads as a dial error: %v", err)
	}
	if n := len(l.entries(slog.LevelError, "pve endpoint ")); n != 2 {
		t.Fatalf("verification lines = %d, want one per endpoint", n)
	}
}

// A request whose send-by deadline has passed is refused before any dial, and
// each refusal is logged at Debug with its method and path.
func TestWalkRefusesAPassedSendByBeforeAnyDial(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx := WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodDelete, "/pools/cpi")
	for range 2 {
		_, err := send(t, ctx, p, http.MethodDelete, a.Addr, "/pools/cpi", "")
		if !errors.Is(err, ErrSendByPassed) {
			t.Fatalf("err = %v, want ErrSendByPassed", err)
		}
		if IsMaybeSent(err) {
			t.Fatalf("a refused send-by is marked maybe sent: %v", err)
		}
	}
	if a.Counts.Conns.Load() != 0 || b.Counts.Conns.Load() != 0 {
		t.Fatalf("connections A=%d B=%d, want none", a.Counts.Conns.Load(), b.Counts.Conns.Load())
	}
	lines := l.entries(slog.LevelDebug, "pve endpoints: DELETE /pools/cpi not sent")
	if len(lines) != 2 {
		t.Fatalf("send-by lines = %d, want one per refusal", len(lines))
	}
	if lines[0].Message != "pve endpoints: DELETE /pools/cpi not sent, send-by deadline passed" {
		t.Fatalf("send-by line = %q", lines[0].Message)
	}
	mustAttrs(t, lines[0], map[string]string{"method": http.MethodDelete, "path": "/pools/cpi"})
	mustDemoted(t, p, a.Addr, false)

	resp, err := send(t, ctx, p, http.MethodGet, a.Addr, "/pools/cpi", "")
	mustStatus(t, resp, err, http.StatusOK)
}

// A send-by deadline that passes while the entry's dial hangs refuses the
// request without a hop and without blaming the entry.
func TestWalkSendByPassingDuringTheDialDoesNotHop(t *testing.T) {
	l := newLab(t)
	a := l.dropped(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx := WithSendBy(context.Background(), time.Now().Add(200*time.Millisecond), http.MethodPost, "/pools")
	_, err := send(t, ctx, p, http.MethodPost, a.Addr, "/pools", "poolid=cpi")
	if !errors.Is(err, ErrSendByPassed) {
		t.Fatalf("err = %v, want ErrSendByPassed", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)
	if n := len(l.entries(slog.LevelDebug, "pve endpoints: POST /pools not sent")); n != 1 {
		t.Fatalf("send-by lines = %d, want 1", n)
	}
}

// A reused connection that the entry closes before reading makes Go retry the
// GET, the retry's dial is refused, and the walk hops to the next endpoint.
func TestWalkReusedConnectionClosedHopsARead(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewReuseCloser(t, l.leaf, nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	a.SetOnReuse(a.StopListening)
	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := a.Counts.Reused.Load(); n != 1 {
		t.Fatalf("A closed %d reused connections, want 1", n)
	}
	if n := b.Counts.Requests.Load(); n != 1 {
		t.Fatalf("B got %d requests, want 1", n)
	}
	mustActive(t, p, b.Addr)
}

// A POST that the entry read and then hung up on may have taken effect, so it
// reaches exactly one server across all of them, and its error comes back
// unchanged.
func TestWalkHijackedPostReachesOneServer(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewBodyThenHijack(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	_, err := send(t, context.Background(), p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	if err == nil {
		t.Fatal("a hijacked POST succeeded")
	}
	if IsConnectPhase(err) {
		t.Fatalf("a POST that reached the server reads as never sent: %v", err)
	}
	if n := pvetest.TotalMethod(http.MethodPost, a, b); n != 1 {
		t.Fatalf("POSTs across servers = %d, want 1", n)
	}
	mustDemoted(t, p, a.Addr, true)
	mustActive(t, p, a.Addr)
}

// A hijacked GET hops, because a read may be repeated.
func TestWalkHijackedReadHops(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewBodyThenHijack(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Requests.Load(); n != 1 {
		t.Fatalf("B got %d requests, want 1", n)
	}
	warns := l.failovers()
	if len(warns) != 1 {
		t.Fatalf("failover lines = %d, want 1", len(warns))
	}
	mustAttrs(t, warns[0], map[string]string{"class": classTransport})
}

// A POST refused at the dial provably never left, so it hops with its body
// rebuilt from GetBody.
func TestWalkRefusedPostHopsWithItsBody(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	resp, err := send(t, context.Background(), p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	seen := b.Seen()
	if len(seen) != 1 || seen[0].Method != http.MethodPost || string(seen[0].Body) != "vmid=101" {
		t.Fatalf("B saw %+v", seen)
	}
	// No status moves a mutation's active endpoint, so A stays active and
	// its demotion sends the next walk to B first.
	mustActive(t, p, a.Addr)
	mustDemoted(t, p, a.Addr, true)
	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := a.Counts.Dials.Load(); n != 1 {
		t.Fatalf("A was dialed %d times, want 1", n)
	}
	mustActive(t, p, b.Addr)
}

// A POST with an Idempotency-Key header may have been replayed by Go, so a
// connect-phase failure does not prove it unsent. It stays, and its error is
// marked maybe sent.
func TestWalkIdempotencyKeyPostDoesNotMove(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	req := apiRequest(t, context.Background(), http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	req.Header.Set("Idempotency-Key", "k1")
	_, err := p.RoundTrip(req)
	if !IsMaybeSent(err) || !IsConnectPhase(err) {
		t.Fatalf("err = %v, want a connect-phase failure marked maybe sent", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustDemoted(t, p, a.Addr, true)
}

// A streamed body cannot be rebuilt, so an upload never hops. A connect-phase
// failure advances the active endpoint so the upload's next attempt goes
// elsewhere, and that change is logged once.
func TestWalkUploadAdvancesTheActiveEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	upload := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://"+a.Addr+pvetest.APIPrefix+"/nodes/pve1/storage/local/upload",
			io.NopCloser(strings.NewReader("image bytes")))
		if err != nil {
			t.Fatal(err)
		}
		if req.GetBody != nil {
			t.Fatal("the upload body can be rebuilt")
		}
		return p.RoundTrip(req)
	}
	_, err := upload()
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || len(eu.Attempts) != 1 || !IsConnectPhase(err) {
		t.Fatalf("err = %v, want one connect-phase hop", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustActive(t, p, b.Addr)
	warns := l.failovers()
	if len(warns) != 1 || !strings.HasSuffix(warns[0].Message, "), next request goes to "+b.Addr) {
		t.Fatalf("failover lines = %+v", warns)
	}
	mustAttrs(t, warns[0], map[string]string{"from": a.Addr, "to": b.Addr, "class": "dial"})

	resp, err := upload()
	mustStatus(t, resp, err, http.StatusOK)
	if n := len(l.failovers()); n != 1 {
		t.Fatalf("failover lines after the retry = %d, want 1", n)
	}
}

// A caller that gives up while the entry's dial hangs gets its own error back,
// with no hop and no demotion.
func TestWalkCallerCancelBeforeAConnectionHasNoEffect(t *testing.T) {
	l := newLab(t)
	a := l.dropped(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := send(t, ctx, p, http.MethodGet, a.Addr, "/version", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)
	mustActive(t, p, a.Addr)
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0", n)
	}
}

// The login and a request with the configured-only marker walk only the
// configured endpoints, so a discovered endpoint never sees them.
func TestWalkConfiguredOnlySkipsDiscoveredEndpoints(t *testing.T) {
	l := newLab(t)
	c1 := l.refused(t)
	d1 := l.live(t, nil)
	c2 := l.live(t, nil)
	p := l.pool(t, 0, configured(c1.Addr), discovered(d1.Addr, PinOf(l.leaf.DER)), configured(c2.Addr))

	resp, err := send(t, context.Background(), p, http.MethodPost, c1.Addr, "/access/ticket",
		"username=root&password=secret")
	mustStatus(t, resp, err, http.StatusOK)
	resp, err = send(t, WithConfiguredOnly(context.Background()), p, http.MethodGet, c1.Addr, "/cluster/status", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := d1.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the discovered endpoint got %d connections", n)
	}
	if n := c2.Counts.Requests.Load(); n != 2 {
		t.Fatalf("the second configured endpoint got %d requests, want 2", n)
	}
	mustActive(t, p, c2.Addr)
}

// A configured-only walk that starts while a discovered endpoint is active
// leaves it active, because the walk never tried it.
func TestWalkConfiguredOnlyKeepsADiscoveredActiveEndpoint(t *testing.T) {
	l := newLab(t)
	c1 := l.live(t, nil)
	d1 := l.live(t, nil)
	p := l.pool(t, 0, configured(c1.Addr), discovered(d1.Addr, PinOf(l.leaf.DER)))
	_, gen := p.current()
	if !p.promote(endpointOf(t, p, d1.Addr), gen) {
		t.Fatal("promote the discovered endpoint")
	}
	resp, err := send(t, context.Background(), p, http.MethodPost, c1.Addr, "/access/ticket",
		"username=root&password=secret")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, d1.Addr)
	if n := d1.Counts.Requests.Load(); n != 0 {
		t.Fatalf("the discovered endpoint got %d requests", n)
	}
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0", n)
	}
	resp, err = send(t, context.Background(), p, http.MethodGet, c1.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := d1.Counts.Requests.Load(); n != 1 {
		t.Fatalf("a normal read did not start on the active discovered endpoint")
	}
}

// A body that GetBody cannot rebuild ends the walk with that error.
func TestWalkGetBodyFailureEndsTheWalk(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	req := apiRequest(t, context.Background(), http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	cause := errors.New("body source closed")
	req.GetBody = func() (io.ReadCloser, error) { return nil, cause }
	_, err := p.RoundTrip(req)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the GetBody failure", err)
	}
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests, want 0", n)
	}
}

// The walk refuses requests it cannot route, and closes their bodies.
func TestRoundTripRefusesUnroutableRequests(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr))
	if _, err := p.RoundTrip(nil); !errors.Is(err, errNilRequest) {
		t.Fatalf("nil request: %v", err)
	}
	body := &trackedBody{Reader: strings.NewReader("x")}
	req := &http.Request{Method: http.MethodPost, Body: body, Header: http.Header{}}
	if _, err := p.RoundTrip(req); !errors.Is(err, errNoURL) || !body.closed {
		t.Fatalf("no URL: %v, body closed %v", err, body.closed)
	}
	body = &trackedBody{Reader: strings.NewReader("x")}
	req = apiRequest(t, context.Background(), http.MethodPost, a.Addr, "/pools", "x")
	req.Body = body
	if _, err := (*Pool)(nil).RoundTrip(req); !errors.Is(err, errNilPool) || !body.closed {
		t.Fatalf("nil pool: %v, body closed %v", err, body.closed)
	}
	body = &trackedBody{Reader: strings.NewReader("x")}
	req = apiRequest(t, context.Background(), http.MethodPost, a.Addr, "/pools", "x")
	req.URL.Scheme = "ftp"
	req.URL.Host = "127.0.0.1"
	req.Body = body
	if _, err := p.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "unsupported scheme") || !body.closed {
		t.Fatalf("ftp scheme: %v, body closed %v", err, body.closed)
	}
	if n := a.Counts.Conns.Load(); n != 0 {
		t.Fatalf("A got %d connections", n)
	}
}

// A walk that ends because the caller's context ended between hops returns
// the context's error, and one that ends because the send-by deadline passed
// between hops returns the refusal and logs it.
func TestWalkHaltBetweenHops(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	p := l.pool(t, 0, configured(a.Addr), configured(l.live(t, nil).Addr))
	dial := &ConnectPhaseError{Phase: PhaseDial, Endpoint: a.Addr, Err: errors.New("connection refused")}
	hop := hopResult{ep: endpointOf(t, p, a.Addr), err: dial,
		oc: outcome{action: actHop, demote: true, sent: sentNever, unreachable: true, class: "dial"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := p.newWalker(apiRequest(t, ctx, http.MethodGet, a.Addr, "/version", ""), false, defaultBounds())
	if _, err := w.halt(hop, stopCaller, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller stop: %v", err)
	}
	mustDemoted(t, p, a.Addr, true)

	w = p.newWalker(apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/version", ""), false,
		defaultBounds())
	_, err := w.halt(hop, stopSendBy, 1)
	cpe, ok := AsConnectPhase(err)
	if !errors.Is(err, ErrSendByPassed) || !ok || cpe.Endpoint != w.order[1].addr {
		t.Fatalf("send-by stop: %v", err)
	}
	if n := len(l.entries(slog.LevelDebug, "pve endpoints: GET /version not sent")); n != 1 {
		t.Fatalf("send-by lines = %d, want 1", n)
	}
}

// The walk's stop checks, in order.
func TestWalkStopBefore(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	clock := newFakeClock()
	mk := func(ctx context.Context, method, body string) *walker {
		return p.newWalker(apiRequest(t, ctx, method, a.Addr, "/pools", body), false, boundsOn(clock))
	}
	if got := mk(context.Background(), http.MethodGet, "").stopBefore(2); got != stopExhausted {
		t.Fatalf("past the limit: %v", got)
	}
	w := mk(context.Background(), http.MethodGet, "")
	w.budget.spend(30 * time.Second)
	if got := w.stopBefore(1); got != stopBudget {
		t.Fatalf("spent budget: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := mk(ctx, http.MethodGet, "").stopBefore(1); got != stopCaller {
		t.Fatalf("ended context: %v", got)
	}
	passed := WithSendBy(context.Background(), time.Now().Add(-time.Second), http.MethodPost, "/pools")
	if got := mk(passed, http.MethodPost, "x").stopBefore(1); got != stopSendBy {
		t.Fatalf("passed send-by: %v", got)
	}
	w = mk(context.Background(), http.MethodPost, "x")
	w.req.GetBody = nil
	if got := w.stopBefore(1); got != stopBody {
		t.Fatalf("streamed body: %v", got)
	}
	if got := mk(context.Background(), http.MethodPost, "x").stopBefore(1); got != stopNone {
		t.Fatalf("rebuildable body: %v", got)
	}
}
