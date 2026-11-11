package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
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

// acceptedLab is a pool of two seeds that refuse and one candidate, after a
// token read accepted the candidate, which then held the probe and the read.
type acceptedLab struct {
	l      *lab
	seeds  [2]*pvetest.Refused
	cand   *pvetest.Server
	c      Candidate
	prober *fakeProber
	p      *Pool
	// hang makes the candidate's /cluster/resources wait until its client
	// gives up.
	hang atomic.Bool
}

func newAcceptedLab(t *testing.T, mutate func(*Config)) *acceptedLab {
	t.Helper()
	a := &acceptedLab{l: newLab(t), prober: &fakeProber{}}
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		if a.hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	a.seeds = [2]*pvetest.Refused{a.l.refused(t), a.l.refused(t)}
	a.cand, a.c = a.l.candidate(t, "pve2.example.com", routes)
	cfg := a.l.coldConfig(a.prober, []Candidate{a.c}, configured(a.seeds[0].Addr), configured(a.seeds[1].Addr))
	cfg.headerDeadline = 100 * time.Millisecond
	if mutate != nil {
		mutate(&cfg)
	}
	a.p = newPool(t, cfg)
	resp, err := a.p.RoundTrip(a.read(t, context.Background()))
	mustStatus(t, resp, err, http.StatusOK)
	a.mustCandidateRequests(t, 2)
	return a
}

// read is a token read on the first seed.
func (a *acceptedLab) read(t *testing.T, ctx context.Context) *http.Request {
	return tokenRequest(t, ctx, http.MethodGet, a.seeds[0].Addr, "/cluster/resources", "")
}

// route sends seed i's dials to s.
func (a *acceptedLab) route(i int, s *pvetest.Server) {
	a.l.d.set(a.seeds[i].Addr, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, s.Addr)
	})
}

func (a *acceptedLab) mustCandidateRequests(t *testing.T, want int32) {
	t.Helper()
	if n := a.cand.Counts.Requests.Load(); n != want {
		t.Fatalf("candidate got %d requests, want %d", n, want)
	}
	if n := a.prober.calls.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
}

// After a candidate was accepted, a later walk reaches it only under the
// conditions of the cold start: every configured endpoint failed in the
// connect phase in that walk, and the request is a token request that walks
// the whole pool and follows no tether.
func TestColdStartAcceptedCandidateStaysBehindTheColdStartConditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(t *testing.T, a *acceptedLab) *http.Request
	}{
		{"a seed failing TLS verification", func(t *testing.T, a *acceptedLab) *http.Request {
			a.route(0, a.l.otherLeaf(t))
			return a.read(t, context.Background())
		}},
		{"a seed meeting its header deadline", func(t *testing.T, a *acceptedLab) *http.Request {
			a.route(0, pvetest.NewHung(t, a.l.leaf))
			return a.read(t, context.Background())
		}},
		{"a seed answering a status hop", func(t *testing.T, a *acceptedLab) *http.Request {
			routes := pvetest.NewRoutes()
			routes.Fail(http.MethodGet, "/cluster/resources", http.StatusBadGateway, "down")
			a.route(0, a.l.live(t, routes))
			return a.read(t, context.Background())
		}},
		{"the login", func(t *testing.T, a *acceptedLab) *http.Request {
			return tokenRequest(t, context.Background(), http.MethodPost, a.seeds[0].Addr, "/access/ticket", "u=p")
		}},
		{"a configured-only request, as discovery sends", func(t *testing.T, a *acceptedLab) *http.Request {
			return a.read(t, WithConfiguredOnly(context.Background()))
		}},
		{"a routed request", func(t *testing.T, a *acceptedLab) *http.Request {
			return tokenRequest(t, context.Background(), http.MethodGet, a.l.refused(t).Addr, "/cluster/resources", "")
		}},
		{"a tethered mutation", func(t *testing.T, a *acceptedLab) *http.Request {
			req := tokenRequest(t, context.Background(), http.MethodPost, a.seeds[0].Addr, fanOutRoute, "vmid=101")
			injectTether(a.p, tetherKeyOf(req), targetOf(endpointOf(t, a.p, a.seeds[0].Addr)), false)
			return req
		}},
		{"a ticket request", func(t *testing.T, a *acceptedLab) *http.Request {
			req := apiRequest(t, context.Background(), http.MethodGet, a.seeds[0].Addr, "/cluster/resources", "")
			req.Header.Set("Cookie", "PVEAuthCookie=ticket")
			return req
		}},
		{"a request without a token", func(t *testing.T, a *acceptedLab) *http.Request {
			return apiRequest(t, context.Background(), http.MethodGet, a.seeds[0].Addr, "/cluster/resources", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAcceptedLab(t, nil)
			resp, _ := a.p.RoundTrip(tc.req(t, a))
			closeBody(resp)
			a.mustCandidateRequests(t, 2)
			if !strings.Contains(tc.name, "seed") {
				return
			}
			// The same walk with every seed refusing does reach the
			// candidate, so the case above was barred by its own hop.
			a.l.d.set(a.seeds[0].Addr, a.seeds[0].DialContext)
			a.p.CloseIdleConnections()
			resp, err := a.p.RoundTrip(a.read(t, context.Background()))
			mustStatus(t, resp, err, http.StatusOK)
			a.mustCandidateRequests(t, 3)
		})
	}
}

// A walk in which every configured endpoint failed to connect goes on to the
// accepted candidate without another probe, and a seed that answers again is
// used ahead of it, a down-marked one included.
func TestColdStartAcceptedCandidateFollowsEveryConfiguredEndpoint(t *testing.T) {
	t.Run("every seed down", func(t *testing.T) {
		a := newAcceptedLab(t, nil)
		resp, err := a.p.RoundTrip(a.read(t, context.Background()))
		mustStatus(t, resp, err, http.StatusOK)
		a.mustCandidateRequests(t, 3)
		for i, s := range a.seeds {
			if n := s.Counts.Dials.Load(); n != 2 {
				t.Fatalf("seed %d dialed %d times, want 2", i, n)
			}
		}
	})
	t.Run("a recovered seed", func(t *testing.T) {
		a := newAcceptedLab(t, nil)
		a.route(1, a.l.live(t, nil))
		resp, err := a.p.RoundTrip(a.read(t, context.Background()))
		mustStatus(t, resp, err, http.StatusOK)
		a.mustCandidateRequests(t, 2)
		mustActive(t, a.p, a.seeds[1].Addr)
		back := a.l.entries(slog.LevelWarn, "pve endpoint failover: "+a.seeds[1].Addr+" is used again, so requests "+
			"go to it again rather than to the cold-start candidate "+a.c.Address+" from placement.az_map")
		if len(back) != 1 {
			t.Fatalf("failover lines = %+v", a.l.failovers())
		}
	})
	t.Run("a down-marked seed", func(t *testing.T) {
		a := newAcceptedLab(t, func(cfg *Config) { cfg.Endpoints[0].DownMarkedAt = time.Now() })
		live := a.l.live(t, nil)
		a.route(0, live)
		resp, err := a.p.RoundTrip(a.read(t, context.Background()))
		mustStatus(t, resp, err, http.StatusOK)
		a.mustCandidateRequests(t, 2)
		if n := live.Counts.Requests.Load(); n != 1 {
			t.Fatalf("the down-marked seed got %d requests, want 1", n)
		}
	})
}

// An accepted candidate that hangs never holds up a seed that recovered,
// because the seed comes first, and a candidate that is not its walk's final
// hop meets the header deadline and hops on, as any endpoint does.
func TestColdStartHangingCandidateHopsOnItsHeaderDeadline(t *testing.T) {
	t.Run("recovered seed first", func(t *testing.T) {
		a := newAcceptedLab(t, nil)
		a.hang.Store(true)
		a.route(0, a.l.live(t, nil))
		start := time.Now()
		resp, err := a.p.RoundTrip(a.read(t, context.Background()))
		mustStatus(t, resp, err, http.StatusOK)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("the read took %v", took)
		}
		a.mustCandidateRequests(t, 2)
	})
	t.Run("candidate ahead of a seed", func(t *testing.T) {
		a := newAcceptedLab(t, nil)
		a.hang.Store(true)
		live := a.l.live(t, nil)
		a.route(0, live)
		w := a.p.newWalker(a.read(t, context.Background()), false, defaultBounds())
		w.order, w.limit = []*endpoint{a.p.memberAt(a.c.Address), endpointOf(t, a.p, a.seeds[0].Addr)}, 2
		start := time.Now()
		resp, _, err := w.run()
		mustStatus(t, resp, err, http.StatusOK)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("the walk took %v behind a hanging candidate", took)
		}
		a.mustCandidateRequests(t, 3)
		if n := live.Counts.Requests.Load(); n != 1 {
			t.Fatalf("the seed got %d requests, want 1", n)
		}
	})
}

// twoCandidates is one seed that refuses and two live candidates, pve2 and
// pve3, with a prober that hands pve2's probe to block and probes pve3.
type twoCandidates struct {
	l      *lab
	seed   *pvetest.Refused
	c2, c3 *pvetest.Server
	cands  []Candidate
	prober *fakeProber
	p      *Pool
	// probed counts the probes of each candidate by node.
	mu     sync.Mutex
	probed map[string]int
}

func newTwoCandidates(t *testing.T, block func(ctx context.Context) error, mutate func(*Config)) *twoCandidates {
	t.Helper()
	l := newLab(t)
	f := &twoCandidates{l: l, seed: l.refused(t), probed: make(map[string]int)}
	var a, b Candidate
	f.c2, a = l.candidate(t, "pve2.example.com", nil)
	f.c3, b = l.candidate(t, "pve3.example.com", nil)
	f.cands = []Candidate{a, b}
	f.prober = &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		f.mu.Lock()
		f.probed[req.Candidate.Node]++
		f.mu.Unlock()
		if req.Candidate.Node == "pve2" && block != nil {
			return block(ctx)
		}
		return probeStatus(ctx, req)
	}}
	cfg := l.coldConfig(f.prober, f.cands, configured(f.seed.Addr))
	if mutate != nil {
		mutate(&cfg)
	}
	f.p = newPool(t, cfg)
	return f
}

func (f *twoCandidates) mustProbed(t *testing.T, pve2, pve3 int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probed["pve2"] != pve2 || f.probed["pve3"] != pve3 {
		t.Fatalf("probes = %v, want pve2 %d and pve3 %d", f.probed, pve2, pve3)
	}
}

// A walk that probes the candidates and is cancelled passes the candidates it
// never probed to the walk waiting for it, which goes on where it stopped: the
// refused candidate is not probed again, no second cold start begins, and the
// end line says why the run stopped and how many candidates it left.
func TestColdStartOwnerCancelledHandsOnTheRest(t *testing.T) {
	entered := make(chan struct{})
	waiting := make(chan struct{}, 4)
	f := newTwoCandidates(t, func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}, func(cfg *Config) { cfg.onColdStartWait = func() { waiting <- struct{}{} } })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, waiter := make(chan walkResult, 1), make(chan walkResult, 1)
	walk := func(ctx context.Context, out chan<- walkResult) {
		req := tokenRequest(t, ctx, http.MethodGet, f.seed.Addr, "/cluster/resources", "")
		go func() {
			resp, err := f.p.roundTrip(req, defaultBounds())
			if err != nil {
				out <- walkResult{err: err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			out <- walkResult{status: resp.StatusCode}
		}()
	}
	walk(ctx, owner)
	<-entered
	walk(context.Background(), waiter)
	<-waiting
	cancel()
	if r := <-owner; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("owner err = %v, want context.Canceled", r.err)
	}
	if r := <-waiter; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("waiter got %d, %v, want 200 from pve3", r.status, r.err)
	}
	f.mustProbed(t, 1, 1)
	end := f.l.entries(slog.LevelDebug, "pve endpoints: the cold start from placement.az_map accepted no candidate")
	if len(end) != 1 || end[0].Attrs["reason"] != "cancelled" || end[0].Attrs["unprobed"] != int64(1) ||
		!strings.HasSuffix(end[0].Message, "with 1 candidates never probed, which the next request that reaches "+
			"the cold start probes") {
		t.Fatalf("end lines = %+v", end)
	}
	resp, err := f.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr,
		"/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	f.mustProbed(t, 1, 1)
	if n := f.c3.Counts.Requests.Load(); n != 3 {
		t.Fatalf("pve3 got %d requests, want the probe and two reads", n)
	}
	if n := f.c2.Counts.Requests.Load(); n != 0 {
		t.Fatalf("pve2 got %d requests", n)
	}
}

// A request with a send-by deadline, or with less than the probe limit left on
// its context, never starts the cold start, so it cannot burn candidates that
// a later request with time to spare probes.
func TestColdStartShortRequestsNeverClaimIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		req  func(t *testing.T, ctx context.Context, seed string) *http.Request
		why  string
	}{
		{"send-by", func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			func(t *testing.T, ctx context.Context, seed string) *http.Request {
				ctx = WithSendBy(ctx, time.Now().Add(time.Minute), http.MethodDelete, "/pools/p1")
				return tokenRequest(t, ctx, http.MethodDelete, seed, "/pools/p1", "")
			}, "it carries a send-by deadline"},
		{"short deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), probeLimit-5*time.Second)
		}, func(t *testing.T, ctx context.Context, seed string) *http.Request {
			return tokenRequest(t, ctx, http.MethodGet, seed, "/cluster/resources", "")
		}, "its context has less than 25s left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTwoCandidates(t, nil, nil)
			ctx, cancel := tc.ctx()
			defer cancel()
			resp, err := f.p.RoundTrip(tc.req(t, ctx, f.seed.Addr))
			closeBody(resp)
			mustUnreachable(t, err, f.seed.Addr)
			f.mustProbed(t, 0, 0)
			if coldBegun(f.p) {
				t.Fatal("the short request began the cold start")
			}
			barred := f.l.entries(slog.LevelDebug, "pve endpoints: no configured endpoint could connect, and ")
			if len(barred) != 1 || !strings.Contains(barred[0].Message, "because "+tc.why) {
				t.Fatalf("barred lines = %+v", barred)
			}
			resp, err = f.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr,
				"/cluster/resources", ""))
			mustStatus(t, resp, err, http.StatusOK)
			f.mustProbed(t, 1, 0)
		})
	}
}

// A send-by mutation that waits for another walk's cold start stops when its
// send-by deadline passes, naming the seed it was refused after.
func TestColdStartWaiterStopsAtItsSendBy(t *testing.T) {
	f := newColdStartFixture(t, false)
	owner := make(chan walkResult, 1)
	f.walk(t, context.Background(), defaultBounds(), owner)
	<-f.entered
	ctx := WithSendBy(context.Background(), time.Now().Add(200*time.Millisecond), http.MethodPost, fanOutRoute)
	req := tokenRequest(t, ctx, http.MethodPost, f.seed.Addr, fanOutRoute, "vmid=101")
	done := make(chan error, 1)
	go func() {
		resp, err := f.p.RoundTrip(req)
		closeBody(resp)
		done <- err
	}()
	<-f.waiting
	select {
	case err := <-done:
		mustConnectPhase(t, err, f.seed.Addr, PhaseDial)
		if !errors.Is(err, ErrSendByPassed) {
			t.Fatalf("err = %v, want ErrSendByPassed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter outlived its send-by deadline")
	}
	close(f.release)
	if r := <-owner; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("owner got %d, %v", r.status, r.err)
	}
	if n := f.cand.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("the candidate got %d POSTs", n)
	}
}

// A walk that waited for the cold start and has no budget left when it ends
// does not send to the accepted candidate.
func TestColdStartWaiterOutOfBudgetAfterTheWaitDoesNotHop(t *testing.T) {
	f := newColdStartFixture(t, false)
	owner, waiter := make(chan walkResult, 1), make(chan walkResult, 1)
	f.walk(t, context.Background(), boundsOn(newFakeClock()), owner)
	<-f.entered
	clock := newFakeClock()
	f.walk(t, context.Background(), boundsOn(clock), waiter)
	<-f.waiting
	clock.advance(walkBudget)
	close(f.release)
	if r := <-owner; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("owner got %d, %v", r.status, r.err)
	}
	r := <-waiter
	mustUnreachable(t, r.err, f.seed.Addr)
	if n := f.cand.Counts.Requests.Load(); n != 2 {
		t.Fatalf("candidate got %d requests, want the probe and the owner's read", n)
	}
}

// A walk whose own request ends right after its probe proved the candidate
// returns the caller's error and sends nothing, and the candidate stays
// accepted for the next request.
func TestColdStartCancelledAfterAProvedProbeSendsNothing(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prober := &fakeProber{fn: func(pctx context.Context, req ProbeRequest) error {
		err := probeStatus(pctx, req)
		cancel()
		return err
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodGet, seed.Addr, "/cluster/resources", ""))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := cand.Counts.Requests.Load(); n != 1 {
		t.Fatalf("candidate got %d requests, want the probe only", n)
	}
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if n := prober.calls.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
}

// A mutation whose own request ends right after its probe proved the candidate
// is proven unsent and leaves no tether, so the next mutation with its key
// still reaches the candidate the cold start accepted.
func TestColdStartCancelledMutationAfterAProvedProbeLeavesNoTether(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prober := &fakeProber{fn: func(pctx context.Context, req ProbeRequest) error {
		err := probeStatus(pctx, req)
		cancel()
		return err
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodPost, seed.Addr, fanOutRoute, "vmid=101"))
	if !errors.Is(err, context.Canceled) || IsMaybeSent(err) {
		t.Fatalf("err = %v, want context.Canceled, proven unsent", err)
	}
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, seed.Addr, fanOutRoute,
		"vmid=102"))
	mustStatus(t, resp, err, http.StatusOK)
	if n := cand.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("candidate got %d POSTs, want the second mutation only", n)
	}
}

// A mutation whose key gains a tether on the accepted candidate while its seed
// dial runs is refused before the candidate hop.
func TestColdStartReclaimRefusesATetherOnTheCandidate(t *testing.T) {
	a := newAcceptedLab(t, nil)
	cand := a.p.memberAt(a.c.Address)
	req := tokenRequest(t, context.Background(), http.MethodPost, a.seeds[0].Addr, fanOutRoute, "vmid=101")
	var once sync.Once
	refuse := func(s *pvetest.Refused) dialFunc {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			once.Do(func() { injectTether(a.p, tetherKeyOf(req), targetOf(cand), false) })
			return s.DialContext(ctx, network, addr)
		}
	}
	a.l.d.set(a.seeds[1].Addr, refuse(a.seeds[1]))
	_, err := a.p.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "a tethered request never goes to a cold-start candidate") {
		t.Fatalf("err = %v, want the tethered refusal", err)
	}
	if n := a.cand.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("candidate got %d POSTs", n)
	}
	a.p.mu.Lock()
	routed := len(a.p.routed)
	a.p.mu.Unlock()
	if routed != 0 {
		t.Fatal("the refused mutation built a routed clone")
	}
}

// A mutation whose body cannot be rebuilt for the candidate hop ends with the
// rebuild error and sends nothing to the candidate.
func TestColdStartBodyRebuildFailureEndsTheWalk(t *testing.T) {
	a := newAcceptedLab(t, nil)
	req := tokenRequest(t, context.Background(), http.MethodPost, a.seeds[0].Addr, fanOutRoute, "vmid=101")
	calls := 0
	getBody := req.GetBody
	req.GetBody = func() (io.ReadCloser, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("body gone")
		}
		return getBody()
	}
	_, err := a.p.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "body gone") {
		t.Fatalf("err = %v, want the rebuild error", err)
	}
	if n := a.cand.Counts.Method(http.MethodPost); n != 0 {
		t.Fatalf("candidate got %d POSTs", n)
	}
}

// A routed request to the address of an accepted candidate that failed TLS
// verification since is refused before any dial.
func TestColdStartRoutedRequestToARevokedCandidateIsRefused(t *testing.T) {
	a := newAcceptedLab(t, nil)
	cand := a.p.memberAt(a.c.Address)
	a.p.candidateFailed(cand, PhaseTLSVerify, &tls.CertificateVerificationError{Err: errors.New("bad")})
	conns := a.cand.Counts.Conns.Load()
	_, err := a.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, a.c.Address,
		"/cluster/resources", ""))
	mustConnectPhase(t, err, a.c.Address, PhaseTLSVerify)
	if !errors.Is(err, errCandidateRevoked) {
		t.Fatalf("err = %v, want the revoked refusal", err)
	}
	if n := a.cand.Counts.Conns.Load(); n != conns {
		t.Fatal("the routed request dialed the revoked candidate")
	}
}

// A candidate that fails TLS verification has its idle connections closed, so
// nothing reuses a connection the pool opened to it before.
func TestColdStartRevokedCandidateClosesItsIdleConnections(t *testing.T) {
	a := newAcceptedLab(t, nil)
	cand := a.p.memberAt(a.c.Address)
	direct := func() {
		t.Helper()
		req := a.read(t, context.Background())
		req.URL.Host = a.c.Address
		resp, err := cand.transports.plain.RoundTrip(req)
		mustStatus(t, resp, err, http.StatusOK)
	}
	direct()
	conns := a.cand.Counts.Conns.Load()
	direct()
	if n := a.cand.Counts.Conns.Load(); n != conns {
		t.Fatalf("the idle connection was not reused, %d connections", n)
	}
	a.p.candidateFailed(cand, PhaseTLSVerify, &tls.CertificateVerificationError{Err: errors.New("bad")})
	direct()
	if n := a.cand.Counts.Conns.Load(); n != conns+1 {
		t.Fatalf("connections = %d, want a new one after the revocation", n)
	}
	a.p.candidateFailed(cand, PhaseTLSHandshake, errors.New("reset"))
	prefix := "pve endpoint " + a.c.Address + ": the cold-start candidate"
	if lines := a.l.entries(slog.LevelError, prefix); len(lines) != 1 {
		t.Fatalf("error lines = %v", lines)
	}
}

// Only a dial, DNS, or handshake failure that the endpoint caused opens the
// cold start.
func TestOpensColdStartPhases(t *testing.T) {
	hop := func(phase Phase, cause error) hopResult {
		return hopResult{
			err: &ConnectPhaseError{Phase: phase, Endpoint: "10.0.0.1:8006", Err: cause},
			oc:  outcome{action: actHop, unreachable: true, class: string(phase)},
		}
	}
	boom := errors.New("boom")
	statusHop := hop(PhaseDial, boom)
	statusHop.oc.statusHop = true
	notUnreachable := hop(PhaseDial, boom)
	notUnreachable.oc.unreachable = false
	for _, tc := range []struct {
		name string
		h    hopResult
		want bool
	}{
		{"dial", hop(PhaseDial, boom), true},
		{"dns", hop(PhaseDNS, boom), true},
		{"handshake", hop(PhaseTLSHandshake, boom), true},
		{"verify", hop(PhaseTLSVerify, boom), false},
		{"pin", hop(PhasePin, boom), false},
		{"send-by", hop(PhaseDial, ErrSendByPassed), false},
		{"wrong address", hop(PhaseDial, errWrongAddress), false},
		{"status hop", statusHop, false},
		{"not unreachable", notUnreachable, false},
		{"no error", hopResult{oc: outcome{action: actHop, unreachable: true, class: string(PhaseDial)}}, false},
		{"header deadline", hopResult{err: boom, oc: outcome{action: actHop, unreachable: true,
			class: classTransport}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := opensColdStart(tc.h); got != tc.want {
				t.Fatalf("opensColdStart = %v, want %v", got, tc.want)
			}
		})
	}
}

// A walk goes through the cold start once, and the accepted candidate is
// offered to a walk only when its order does not already hold it and the
// candidate has not failed TLS verification.
func TestColdStartOnceAndAcceptedForChecks(t *testing.T) {
	a := newAcceptedLab(t, nil)
	cand := a.p.memberAt(a.c.Address)
	w := a.p.newWalker(a.read(t, context.Background()), false, defaultBounds())
	w.coldFailed, w.made = 2, 2
	if !w.coldStartDue(len(w.order)) {
		t.Fatal("a walk with every seed down is not due")
	}
	if ep, stop := w.coldStart(); ep != cand || stop != stopNone {
		t.Fatalf("coldStart = %v, %v, want the accepted candidate", ep, stop)
	}
	if !w.coldDone || w.coldStartDue(len(w.order)) {
		t.Fatal("a walk is due for the cold start twice")
	}
	if a.p.acceptedFor(cand, nil) != cand {
		t.Fatal("acceptedFor refused a fresh walk")
	}
	if a.p.acceptedFor(cand, []*endpoint{cand}) != nil {
		t.Fatal("acceptedFor offered a candidate the walk already holds")
	}
	if a.p.acceptedFor(nil, nil) != nil {
		t.Fatal("acceptedFor offered no candidate")
	}
	a.p.candidateFailed(cand, PhaseTLSVerify, &tls.CertificateVerificationError{Err: errors.New("bad")})
	if a.p.acceptedFor(cand, nil) != nil {
		t.Fatal("acceptedFor offered a revoked candidate")
	}
}
