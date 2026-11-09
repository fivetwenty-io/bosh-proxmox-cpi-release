package endpointpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// waitFor is how long a wire test waits for a server to see a request it
// expects.
const waitFor = 10 * time.Second

// fanOutRoute is the create route that the concurrent tests send to.
const fanOutRoute = "/nodes/pve1/qemu"

// postOnce sends one POST to fanOutRoute at host and returns its status, or
// its error. It does not use t, so a goroutine may call it.
func postOnce(ctx context.Context, p *Pool, host, body string) (int, error) {
	ctx = connectContext(ctx, http.MethodPost, fanOutRoute)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+pvetest.APIPrefix+fanOutRoute,
		strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp, err := p.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// mutations returns how many POST, PUT, and DELETE requests the servers read.
func mutations(servers ...*pvetest.Server) int32 {
	return pvetest.TotalMethod(http.MethodPost, servers...) + pvetest.TotalMethod(http.MethodPut, servers...) +
		pvetest.TotalMethod(http.MethodDelete, servers...)
}

// mustUnreachableAt fails the test unless err is a walk's connect-phase
// failure of exactly one hop, to addr.
func mustUnreachableAt(t *testing.T, err error, addr string) {
	t.Helper()
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) || !IsConnectPhase(err) {
		t.Fatalf("err = %v, want a connect-phase EntryUnreachableError", err)
	}
	if got := eu.Endpoints(); len(got) != 1 || got[0] != addr {
		t.Fatalf("hops = %v, want only %s", got, addr)
	}
}

// moveActive sends a read that A answers with a 503, so the read hops to B and
// B becomes active, as a read in another goroutine of the call would.
func moveActive(t *testing.T, ctx context.Context, p *Pool, a, b string) {
	t.Helper()
	resp, err := send(t, ctx, p, http.MethodGet, a, "/cluster/resources", "")
	mustStatus(t, resp, err, http.StatusOK)
	mustActive(t, p, b)
}

// injectTether puts a fixed scopeless tether straight into the table, as a
// request that won the key elsewhere would have left it.
func injectTether(p *Pool, key tetherKey, target tetherTarget, conflicted bool) {
	p.tethers.mu.Lock()
	defer p.tethers.mu.Unlock()
	if p.tethers.byKey == nil {
		p.tethers.byKey = make(map[tetherKey][]*tether)
	}
	p.tethers.byKey[key] = append(p.tethers.byKey[key],
		&tether{key: key, target: target, fixed: true, logged: true, conflicted: conflicted})
}

// closeTracker is a request body that records whether it was closed.
type closeTracker struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeTracker) Close() error {
	c.closed.Store(true)
	return nil
}

// After a POST that may have reached A, every later mutation with its key goes
// only to A, even with A closed and B healthy, and fails without a walk. That
// holds on the call's own context, on a context built from
// context.Background() inside the call, and for a different query string. A
// GET on the same path still fails over.
func TestTetherKeepsAMaybeSentPostOnItsEndpoint(t *testing.T) {
	l := newLab(t)
	a := pvetest.NewBodyThenHijack(t, l.leaf)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()

	_, err := send(t, ctx, p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	if err == nil || IsConnectPhase(err) {
		t.Fatalf("first POST: %v, want a maybe-sent error", err)
	}
	l.d.set(a.Addr, pvetest.NewRefused(t).DialContext)

	attempts := []struct {
		name string
		ctx  context.Context
		path string
	}{
		{"the call's context", ctx, "/nodes/pve1/qemu"},
		{"a background context", context.Background(), "/nodes/pve1/qemu"},
		{"another query string", ctx, "/nodes/pve1/qemu?vmid=102"},
	}
	for _, at := range attempts {
		_, err := send(t, at.ctx, p, http.MethodPost, a.Addr, at.path, "vmid=102")
		if err == nil {
			t.Fatalf("%s: the replay succeeded", at.name)
		}
		mustUnreachableAt(t, err, a.Addr)
	}
	if n := b.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("B got %d POSTs, want 0", n)
	}
	if n := a.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("A got %d POSTs, want 1", n)
	}
	if n := len(l.failovers()); n != 0 {
		t.Fatalf("failover lines = %d, want 0", n)
	}

	resp, err := send(t, ctx, p, http.MethodGet, a.Addr, "/nodes/pve1/qemu", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Method(http.MethodGet); n != 1 {
		t.Fatalf("B got %d GETs, want the read to fail over", n)
	}
}

// A POST answered with any status fixes its tether without demoting the
// entry, and its replay goes back to the same server after a read in the call
// moved the active endpoint to B. That holds for every 5xx the retry layers
// replay and for a 595 the entry caused.
func TestTetherAfterAStatus(t *testing.T) {
	for _, status := range []int{
		statusBrokenBackend, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, statusNoNextHop,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			l := newLab(t)
			routes := pvetest.NewRoutes()
			routes.Fail(http.MethodPost, "/nodes/pve1/qemu", status, "backend trouble")
			routes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
			a := l.live(t, routes)
			b := l.live(t, nil)
			p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(b.Addr))
			ctx, release := WithAffinityScope(context.Background())
			defer release()

			resp, err := send(t, ctx, p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
			mustStatus(t, resp, err, status)
			mustDemoted(t, p, a.Addr, false)
			mustActive(t, p, a.Addr)

			moveActive(t, ctx, p, a.Addr, b.Addr)
			resp, err = send(t, ctx, p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
			mustStatus(t, resp, err, status)
			if n := a.Counts.Method(http.MethodPost); n != 2 {
				t.Fatalf("A got %d POSTs, want 2", n)
			}
			if n := b.Counts.Method(http.MethodPost); n != 0 {
				t.Fatalf("B got %d POSTs, want 0", n)
			}
			mustActive(t, p, b.Addr)
		})
	}
}

// A POST refused at A's dial hops to B, and the tether it fixes there keeps
// the next POST with its key on B even after A is healthy and active again.
// Reads follow the active endpoint.
func TestTetherRefusedPostHopsAndStaysOnItsNewEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	l.d.set(a.Addr, pvetest.NewRefused(t).DialContext)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	resp, err := send(t, context.Background(), p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)

	l.d.set(a.Addr, nil)
	_, gen := p.current()
	if _, ok := p.promote(endpointOf(t, p, a.Addr), gen); !ok {
		t.Fatal("promote A")
	}
	mustActive(t, p, a.Addr)
	resp, err = send(t, context.Background(), p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Method(http.MethodPost); n != 2 {
		t.Fatalf("B got %d POSTs, want 2", n)
	}
	if n := a.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("A got %d POSTs, want 0", n)
	}
	if n := a.Counts.Method(http.MethodGet); n != 1 {
		t.Fatalf("A got %d GETs, want the read on the active endpoint", n)
	}
}

// A streamed upload that fails in the connect phase releases its reservation
// and advances the active endpoint, so the loop's next attempt reserves afresh
// on the new one and fixes its tether there.
func TestTetherUploadReleasesItsReservation(t *testing.T) {
	l := newLab(t)
	a := l.refused(t)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	const route = "/nodes/pve1/storage/local/upload"
	upload := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+a.Addr+pvetest.APIPrefix+route,
			io.NopCloser(strings.NewReader("image bytes")))
		if err != nil {
			t.Fatal(err)
		}
		return p.RoundTrip(req)
	}
	key := tetherKey{method: http.MethodPost, route: route}
	tethers := func() []*tether {
		p.tethers.mu.Lock()
		defer p.tethers.mu.Unlock()
		return append([]*tether(nil), p.tethers.byKey[key]...)
	}

	_, err := upload()
	mustUnreachableAt(t, err, a.Addr)
	if n := len(tethers()); n != 0 {
		t.Fatalf("%d tethers after the connect-phase failure, want 0", n)
	}
	mustActive(t, p, b.Addr)

	resp, err := upload()
	mustStatus(t, resp, err, http.StatusOK)
	if got := tethers(); len(got) != 1 || !got[0].fixed || got[0].target.addr != b.Addr {
		t.Fatalf("tethers after the retry = %+v, want one fixed on B", got)
	}
}

// A routed upload that may have reached its direct host tethers the key to
// that host, so the proxied retry, which names the primary, goes back to the
// direct host and to no pool endpoint.
func TestTetherRoutedMaybeSentKeepsTheRetryOnTheDirectHost(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	r := pvetest.NewBodyThenHijack(t, l.leaf)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	const route = "/nodes/pve3/storage/local/upload"

	_, err := send(t, ctx, p, http.MethodPost, r.Addr, route, "data")
	if !IsMaybeSent(err) {
		t.Fatalf("direct upload: %v, want a maybe-sent error", err)
	}
	_, err = send(t, ctx, p, http.MethodPost, a.Addr, route, "data")
	if !IsMaybeSent(err) {
		t.Fatalf("proxied retry: %v, want it sent to the direct host and marked maybe sent", err)
	}
	if n := r.Counts.Method(http.MethodPost); n != 2 {
		t.Fatalf("the direct host got %d POSTs, want 2", n)
	}
	seen := r.Seen()
	if len(seen) != 2 || string(seen[1].Body) != "data" {
		t.Fatalf("the direct host saw %+v", seen)
	}
	if n := mutations(a, b); n != 0 {
		t.Fatalf("pool endpoints got %d mutations, want 0", n)
	}
}

// A routed upload whose direct host refuses the dial releases its
// reservation, so the proxied fallback walks the pool.
func TestTetherRoutedConnectFailureLetsTheFallbackWalk(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	r := l.refused(t)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	const route = "/nodes/pve3/storage/local/upload"

	_, err := send(t, context.Background(), p, http.MethodPost, r.Addr, route, "data")
	if !IsConnectPhase(err) || IsMaybeSent(err) {
		t.Fatalf("direct upload: %v, want a connect-phase failure", err)
	}
	resp, err := send(t, context.Background(), p, http.MethodPost, a.Addr, route, "data")
	mustStatus(t, resp, err, http.StatusOK)
	if n := a.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("A got %d POSTs, want 1", n)
	}
}

// A mutation whose key is tethered to a routed host follows it even when the
// request itself names another routed host.
func TestTetherRoutedRequestFollowsItsTether(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	const route = "/nodes/pve1/qemu"
	resp, err := send(t, context.Background(), p, http.MethodPost, b.Addr, route, "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	other := l.live(t, nil)
	resp, err = send(t, context.Background(), p, http.MethodPost, other.Addr, route, "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Method(http.MethodPost); n != 2 {
		t.Fatalf("B got %d POSTs, want 2", n)
	}
	if n := mutations(a, other); n != 0 {
		t.Fatalf("the other hosts got %d mutations, want 0", n)
	}
}

// A configured-only mutation whose key is tethered to a discovered endpoint is
// refused before any dial, because a credential never reaches an address the
// operator did not list.
func TestTetherConfiguredOnlyNeverFollowsToADiscoveredEndpoint(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := l.live(t, nil)
	p := l.pool(t, 0, configured(c.Addr), discovered(d.Addr, PinOf(l.leaf.DER)))
	req := apiRequest(t, context.Background(), http.MethodPost, c.Addr, "/nodes/pve1/qemu", "")
	injectTether(p, tetherKeyOf(req), targetOf(endpointOf(t, p, d.Addr)), false)

	_, err := send(t, WithConfiguredOnly(context.Background()), p, http.MethodPost, c.Addr, "/nodes/pve1/qemu",
		"vmid=101")
	if err == nil || !strings.Contains(err.Error(), "configured-only") {
		t.Fatalf("err = %v, want the configured-only refusal", err)
	}
	if n := c.Counts.Conns.Load() + d.Counts.Conns.Load(); n != 0 {
		t.Fatalf("%d connections, want none", n)
	}
}

// A key marked conflicted refuses every later mutation with it before
// dispatch, closes the request's body, sends nothing, and logs once at Error.
func TestTetherConflictRefusesBeforeDispatch(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	resp, err := send(t, ctx, p, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "vmid=101")
	mustStatus(t, resp, err, http.StatusOK)

	// The test hook: a request that ended maybe sent on B while its key's
	// tether names A, which only a defect can produce.
	h := newTetherHold(apiRequest(t, ctx, http.MethodPost, a.Addr, "/nodes/pve1/qemu", ""))
	if err := p.claim(h, targetOf(endpointOf(t, p, b.Addr))); err != nil || h.target.addr != a.Addr {
		t.Fatalf("claim: %v, target %s", err, h.target.addr)
	}
	p.settle(h, outcome{sent: sentMaybe, addr: b.Addr})

	for range 3 {
		body := &closeTracker{Reader: strings.NewReader("vmid=101")}
		req := apiRequest(t, ctx, http.MethodPost, a.Addr, "/nodes/pve1/qemu", "")
		req.Body = body
		_, err := p.RoundTrip(req)
		if !errors.Is(err, ErrTetherConflict) || IsConnectPhase(err) || IsMaybeSent(err) {
			t.Fatalf("err = %v, want ErrTetherConflict alone", err)
		}
		if !body.closed.Load() {
			t.Fatal("the refused request's body was not closed")
		}
	}
	if n := mutations(a, b); n != 1 {
		t.Fatalf("servers got %d mutations, want the first one only", n)
	}
	if n := len(l.entries(slog.LevelError, "pve endpoints: ")); n != 1 {
		t.Fatalf("Error lines = %d, want 1", n)
	}
}

// When the key gains a tether between a walk's hops, the walk follows it on
// its next hop instead of trying the endpoint its order names, and a
// conflicted key ends the walk with the refusal.
func TestTetherWalkClaimsAgainBeforeItsNextHop(t *testing.T) {
	const route = "/nodes/pve1/qemu"
	setup := func(t *testing.T) (*lab, *Pool, *pvetest.Server, *pvetest.Server, *pvetest.Server, func(func())) {
		l := newLab(t)
		a, b, c := l.live(t, nil), l.live(t, nil), l.live(t, nil)
		refused := pvetest.NewRefused(t)
		var during atomic.Pointer[func()]
		l.d.set(a.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
			if f := during.Load(); f != nil {
				(*f)()
			}
			return refused.DialContext(ctx, network, addr)
		})
		p := l.pool(t, 0, configured(a.Addr), configured(b.Addr), configured(c.Addr))
		return l, p, a, b, c, func(f func()) { during.Store(&f) }
	}
	key := tetherKey{method: http.MethodPost, route: route}

	t.Run("pool endpoint", func(t *testing.T) {
		_, p, a, b, c, during := setup(t)
		during(func() { injectTether(p, key, targetOf(endpointOf(t, p, c.Addr)), false) })
		resp, err := send(t, context.Background(), p, http.MethodPost, a.Addr, route, "vmid=101")
		mustStatus(t, resp, err, http.StatusOK)
		if n := c.Counts.Method(http.MethodPost); n != 1 {
			t.Fatalf("C got %d POSTs, want 1", n)
		}
		if seen := c.Seen(); len(seen) != 1 || string(seen[0].Body) != "vmid=101" {
			t.Fatalf("C saw %+v, want the rebuilt body", seen)
		}
		if n := b.Counts.Method(http.MethodPost); n != 0 {
			t.Fatalf("B got %d POSTs, want 0", n)
		}
	})

	t.Run("routed host", func(t *testing.T) {
		l, p, a, b, c, during := setup(t)
		r := l.live(t, nil)
		during(func() { injectTether(p, key, tetherTarget{addr: r.Addr, urlHost: r.Addr}, false) })
		resp, err := send(t, context.Background(), p, http.MethodPost, a.Addr, route, "vmid=101")
		mustStatus(t, resp, err, http.StatusOK)
		if seen := r.Seen(); len(seen) != 1 || string(seen[0].Body) != "vmid=101" {
			t.Fatalf("the routed host saw %+v", seen)
		}
		if n := mutations(b, c); n != 0 {
			t.Fatalf("pool endpoints got %d mutations, want 0", n)
		}
	})

	t.Run("conflicted", func(t *testing.T) {
		_, p, a, b, c, during := setup(t)
		during(func() { injectTether(p, key, targetOf(endpointOf(t, p, c.Addr)), true) })
		_, err := send(t, context.Background(), p, http.MethodPost, a.Addr, route, "vmid=101")
		if !errors.Is(err, ErrTetherConflict) {
			t.Fatalf("err = %v, want ErrTetherConflict", err)
		}
		if n := mutations(b, c); n != 0 {
			t.Fatalf("servers got %d mutations, want 0", n)
		}
	})
}

// Two requests hold one reservation on A when A refuses them both. Neither
// hops, because the other still holds the reservation when the first fails,
// and the second followed it. Once both have failed the reservation is gone,
// so the next mutation reserves afresh and reaches B.
func TestTetherSharedReservationDoesNotHop(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	refused := pvetest.NewRefused(t)
	arrived := make(chan struct{}, 2)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var calls atomic.Int32
	l.d.set(a.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
		n := calls.Add(1)
		arrived <- struct{}{}
		if n <= 2 {
			<-gates[n-1]
		}
		return refused.DialContext(ctx, network, addr)
	})
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()

	errs := []chan error{make(chan error, 1), make(chan error, 1)}
	start := func(i int) {
		go func() {
			_, err := postOnce(ctx, p, a.Addr, "vmid=101")
			errs[i] <- err
		}()
		select {
		case <-arrived:
		case <-time.After(waitFor):
			t.Fatalf("request %d never dialed A", i+1)
		}
	}
	start(0)
	start(1)
	for i := range 2 {
		close(gates[i])
		mustUnreachableAt(t, <-errs[i], a.Addr)
	}
	if n := b.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("B got %d POSTs, want 0", n)
	}

	status, err := postOnce(ctx, p, a.Addr, "vmid=101")
	if err != nil || status != http.StatusOK {
		t.Fatalf("the next POST: %d, %v", status, err)
	}
	if n := b.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("B got %d POSTs, want 1", n)
	}
}

// The storage-replica fan-out: goroutines that target one node each send
// POST /nodes/{node}/qemu. The first reserves the key on A and stalls there.
// A read in another goroutine moves the active endpoint to B, and the second
// POST still goes to A, because the reservation names A. Both get a 596, and
// both replays go to A as well. B never sees a POST.
func TestTetherStorageReplicaFanOut(t *testing.T) {
	l := newLab(t)
	arrived := make(chan struct{}, 4)
	gate := make(chan struct{})
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodPost, "/nodes/pve1/qemu", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-gate
		w.WriteHeader(statusBrokenBackend)
	})
	routes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
	a := l.live(t, routes)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()

	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	failures := make(chan error, 4)
	replica := func(vmid string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The first attempt, then the replay that RetryOnTransient makes
			// after the 596.
			for range 2 {
				status, err := postOnce(ctx, p, a.Addr, "vmid="+vmid)
				if err != nil {
					failures <- err
					return
				}
				statuses <- status
			}
		}()
	}
	awaitA := func(which string) {
		t.Helper()
		select {
		case <-arrived:
		case err := <-failures:
			t.Fatalf("%s POST failed: %v", which, err)
		case <-time.After(waitFor):
			t.Fatalf("the %s POST never reached A (B saw %d POSTs)", which, b.Counts.Method(http.MethodPost))
		}
	}

	replica("101")
	awaitA("first")
	moveActive(t, ctx, p, a.Addr, b.Addr)
	replica("102")
	awaitA("second")
	close(gate)
	wg.Wait()
	close(statuses)
	close(failures)
	for err := range failures {
		t.Fatalf("a replica POST failed: %v", err)
	}
	count := 0
	for s := range statuses {
		count++
		if s != statusBrokenBackend {
			t.Fatalf("status = %d, want 596", s)
		}
	}
	if count != 4 {
		t.Fatalf("%d POSTs answered, want 4", count)
	}
	if n := a.Counts.Method(http.MethodPost); n != 4 {
		t.Fatalf("A got %d POSTs, want 4", n)
	}
	if n := b.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("B got %d POSTs, want 0", n)
	}
	mustActive(t, p, b.Addr)
	if n := len(l.entries(slog.LevelDebug, "pve endpoints: POST /nodes/pve1/qemu may have reached")); n != 1 {
		t.Fatalf("tether lines = %d, want 1", n)
	}
	if n := len(l.failovers()); n != 1 {
		t.Fatalf("failover lines = %d, want only the read's", n)
	}
}

// Many goroutines send mutations with one key while another goroutine keeps
// moving the active endpoint. The first reservation wins, so every mutation
// reaches one server.
func TestTetherConcurrentMutationsShareOneEndpoint(t *testing.T) {
	l := newLab(t)
	a, b := l.live(t, nil), l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	eps := []*endpoint{endpointOf(t, p, a.Addr), endpointOf(t, p, b.Addr)}

	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, gen := p.current()
			p.promote(eps[i%2], gen)
		}
	}()

	const senders = 16
	var wg sync.WaitGroup
	errs := make(chan error, senders)
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := postOnce(ctx, p, a.Addr, fmt.Sprintf("vmid=%d", 100+i))
			if err == nil && status != http.StatusOK {
				err = fmt.Errorf("status %d", status)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(stop)
	flipper.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a POST failed: %v", err)
		}
	}
	na, nb := a.Counts.Method(http.MethodPost), b.Counts.Method(http.MethodPost)
	if na+nb != senders || (na != 0 && nb != 0) {
		t.Fatalf("POSTs A=%d B=%d, want all %d on one server", na, nb, senders)
	}
}

// A mutation that may have reached A tethers its family, so a re-planned
// write with another method or path in the family goes only to A after the
// active endpoint moved to B, while the family's reads may reach B.
func TestTetherFamiliesKeepRePlannedWritesOnOneEndpoint(t *testing.T) {
	type call struct{ method, path string }
	cases := []struct {
		name  string
		first call
		then  []call
		read  string
	}{
		{"ha", call{http.MethodPut, "/cluster/ha/rules/cpi-aa-web"}, []call{
			{http.MethodPost, "/cluster/ha/rules"},
			{http.MethodDelete, "/cluster/ha/rules/cpi-aa-db"},
			{http.MethodPut, "/cluster/ha/resources/vm:101"},
		}, "/cluster/ha/rules"},
		{"replication", call{http.MethodPut, "/cluster/replication/101-0"}, []call{
			{http.MethodDelete, "/cluster/replication/101-0"},
			{http.MethodDelete, "/cluster/replication/101-0?force=1"},
			{http.MethodPost, "/cluster/replication"},
		}, "/cluster/replication"},
		{"pools", call{http.MethodPost, "/pools"}, []call{
			{http.MethodDelete, "/pools?poolid=cpi-lock"},
			{http.MethodPut, "/pools/cpi-lock"},
		}, "/pools"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			routes := pvetest.NewRoutes()
			routes.Fail(c.first.method, c.first.path, statusBrokenBackend, "broken backend")
			routes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
			a, b := l.live(t, routes), l.live(t, nil)
			p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
			ctx, release := WithAffinityScope(context.Background())
			defer release()

			resp, err := send(t, ctx, p, c.first.method, a.Addr, c.first.path, "x=1")
			mustStatus(t, resp, err, statusBrokenBackend)
			moveActive(t, ctx, p, a.Addr, b.Addr)
			for _, next := range c.then {
				resp, err := send(t, ctx, p, next.method, a.Addr, next.path, "x=1")
				mustStatus(t, resp, err, http.StatusOK)
			}
			resp, err = send(t, ctx, p, http.MethodGet, a.Addr, c.read, "")
			mustStatus(t, resp, err, http.StatusOK)

			if n := mutations(a); n != int32(1+len(c.then)) {
				t.Fatalf("A got %d mutations, want %d", n, 1+len(c.then))
			}
			if n := mutations(b); n != 0 {
				t.Fatalf("B got %d mutations, want 0", n)
			}
			if n := b.Counts.Method(http.MethodGet); n != 2 {
				t.Fatalf("B got %d reads, want the moved read and the family's read", n)
			}
		})
	}
}
