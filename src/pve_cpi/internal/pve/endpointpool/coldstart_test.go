package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// coldToken is the API token header the tests' requests carry.
const coldToken = "PVEAPIToken=root@pam!cpi=secret"

// fakeProber records every probe and answers with fn, or with probeStatus
// when fn is nil.
type fakeProber struct {
	calls atomic.Int32
	mu    sync.Mutex
	reqs  []ProbeRequest
	fn    func(ctx context.Context, req ProbeRequest) error
}

func (f *fakeProber) Probe(ctx context.Context, req ProbeRequest) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return probeStatus(ctx, req)
}

func (f *fakeProber) probed() []ProbeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProbeRequest(nil), f.reqs...)
}

// probeStatus sends the membership probe the way a real prober does and
// accepts any 2xx.
func probeStatus(ctx context.Context, req ProbeRequest) error {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+req.Candidate.Address+ProbePath, nil)
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", req.Authorization)
	resp, err := req.Transport.RoundTrip(hreq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe answered %d", resp.StatusCode)
	}
	return nil
}

// candidate starts a live server whose leaf the lab's CA signed for name, and
// routes name:8006 to it, so it stands for a node the az_map names.
func (l *lab) candidate(t *testing.T, name string, routes *pvetest.Routes) (*pvetest.Server, Candidate) {
	t.Helper()
	s := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{name}}), routes)
	addr := net.JoinHostPort(name, "8006")
	l.d.set(addr, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, s.Addr)
	})
	return s, Candidate{Node: strings.SplitN(name, ".", 2)[0], Address: addr}
}

// coldConfig is the lab's config for a pool with the candidates and prober.
func (l *lab) coldConfig(prober Prober, cands []Candidate, eps ...Endpoint) Config {
	return Config{
		Base:       baseFor(l.ca, l.d),
		Logger:     l.logger,
		Endpoints:  eps,
		Candidates: cands,
		Prober:     prober,
	}
}

// tokenRequest is apiRequest with the API token header.
func tokenRequest(t *testing.T, ctx context.Context, method, addr, path, body string) *http.Request {
	t.Helper()
	req := apiRequest(t, ctx, method, addr, path, body)
	req.Header.Set("Authorization", coldToken)
	return req
}

// namedCandidates returns n candidates pve2.example.com and on, with no
// server behind them.
func namedCandidates(n int) []Candidate {
	out := make([]Candidate, 0, n)
	for i := range n {
		name := "pve" + strconv.Itoa(i+2)
		out = append(out, Candidate{Node: name, Address: name + ".example.com:8006"})
	}
	return out
}

func mustUnreachable(t *testing.T, err error, want ...string) {
	t.Helper()
	var eu *EntryUnreachableError
	if !errors.As(err, &eu) {
		t.Fatalf("err = %v, want an EntryUnreachableError", err)
	}
	got := eu.Endpoints()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("hops = %v, want %v", got, want)
	}
}

func TestGate(t *testing.T) {
	nodes := []string{"pve2", "pve3"}
	cases := []struct {
		name   string
		nodes  []string
		usable int
		out    string
		want   bool
	}{
		{"missing file or hint only", nodes, 0, "", true},
		{"ok with every node stale", nodes, 0, "ok", true},
		{"no_status with every node stale", nodes, 0, "no_status", true},
		{"no_candidates", nodes, 0, "no_candidates", true},
		{"public_suffix", nodes, 0, "public_suffix", true},
		{"standalone", nodes, 0, "standalone", false},
		{"usable discovered entry", nodes, 1, "ok", false},
		{"usable entry under no_candidates", nodes, 2, "no_candidates", false},
		{"negative count", nodes, -1, "", false},
		{"unknown outcome", nodes, 0, "later", false},
		{"outcome in another case", nodes, 0, "OK", false},
		{"no az_map nodes", nil, 0, "", false},
		{"only blank names", []string{"", "  "}, 0, "", false},
		{"one name among blanks", []string{"", "pve2"}, 0, "no_candidates", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Gate(tc.nodes, tc.usable, tc.out); got != tc.want {
				t.Fatalf("Gate(%q, %d, %q) = %v, want %v", tc.nodes, tc.usable, tc.out, got, tc.want)
			}
		})
	}
}

func TestNewRefusesBadCandidates(t *testing.T) {
	l := newLab(t)
	const seed = "10.0.0.1:8006"
	good := Candidate{Node: "pve2", Address: "pve2.example.com:8006"}
	prober := &fakeProber{}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no prober", func(c *Config) { c.Prober = nil }, "need a prober"},
		{"verify_ssl false", func(c *Config) { c.Base = baseFor(nil, l.d) }, "verify_ssl true"},
		{"no CA", func(c *Config) {
			c.Base = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		}, "operator's CA"},
		{"discovered endpoint", func(c *Config) {
			c.Endpoints = append(c.Endpoints, discovered("10.0.0.9:8006", Pin{1}))
		}, "discovered endpoint 10.0.0.9:8006"},
		{"too many", func(c *Config) { c.Candidates = namedCandidates(maxCandidates + 1) }, "at most 64"},
		{"empty node", func(c *Config) { c.Candidates = []Candidate{{Address: good.Address}} }, "empty"},
		{"spaced node", func(c *Config) {
			c.Candidates = []Candidate{{Node: " pve2", Address: good.Address}}
		}, "surrounding space"},
		{"bad address", func(c *Config) { c.Candidates = []Candidate{{Node: "pve2", Address: "pve2"}} }, "pve2"},
		{"configured address", func(c *Config) {
			c.Candidates = []Candidate{{Node: "pve1", Address: seed}}
		}, "is a configured endpoint"},
		{"address twice", func(c *Config) {
			c.Candidates = []Candidate{good, {Node: "pve3", Address: "PVE2.example.com:8006"}}
		}, "listed twice"},
		{"node twice", func(c *Config) {
			c.Candidates = []Candidate{good, {Node: "pve2", Address: "pve3.example.com:8006"}}
		}, "node pve2 is listed twice"},
		{"candidate kind in endpoints", func(c *Config) {
			c.Endpoints = append(c.Endpoints, Endpoint{Address: "10.0.0.8:8006", Kind: kindCandidate})
		}, "Config.Candidates carries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := l.coldConfig(prober, []Candidate{good}, configured(seed))
			tc.mutate(&cfg)
			p, err := New(cfg)
			if err == nil {
				p.CloseIdleConnections()
				t.Fatal("New accepted the candidates")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if prober.calls.Load() != 0 {
		t.Fatal("New probed a candidate")
	}
}

// Candidates count toward neither the pool's size nor its header deadline,
// and the pool-built line says how many there are. A one-seed pool keeps its
// seed's clone without a deadline, so a slow read on it still answers.
func TestColdStartCandidatesLeaveTheSeedAsItIs(t *testing.T) {
	l := newLab(t)
	const deadline = 50 * time.Millisecond
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * deadline)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	seed := l.live(t, routes)
	prober := &fakeProber{}
	cfg := l.coldConfig(prober, namedCandidates(2), configured(seed.Addr))
	cfg.headerDeadline = deadline
	p := newPool(t, cfg)
	if p.Len() != 1 || p.HeaderDeadline() != 0 {
		t.Fatalf("Len %d, HeaderDeadline %v, want 1 and none", p.Len(), p.HeaderDeadline())
	}
	lines := l.entries(slog.LevelDebug, "pve endpoints: pool [")
	if len(lines) != 1 || !strings.HasSuffix(lines[0].Message, ", with 2 cold-start candidates from placement.az_map") {
		t.Fatalf("pool-built lines = %v", lines)
	}
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if prober.calls.Load() != 0 {
		t.Fatal("a seed that answered started the cold start")
	}

	two := l.live(t, nil)
	cfg = l.coldConfig(prober, namedCandidates(2), configured(seed.Addr), configured(two.Addr))
	cfg.headerDeadline = deadline
	if p2 := newPool(t, cfg); p2.HeaderDeadline() != deadline {
		t.Fatalf("two seeds have deadline %v, want %v", p2.HeaderDeadline(), deadline)
	}
}

// With the seed down, a token read probes the candidate with the request's
// token, goes to it once the probe proves it, and makes it active, so the next
// read goes straight to it without another probe. The failover line says
// where the candidate came from, and no mark names the candidate.
func TestColdStartSendsToTheAcceptedCandidate(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	var auth atomic.Value
	cand, c := l.candidate(t, "pve2.example.com", nil)
	cand.SetOnRequest(func(r *http.Request) {
		if r.URL.Path == ProbePath {
			auth.Store(r.Header.Get("Authorization"))
		}
	})
	prober := &fakeProber{}
	sink := &markRecorder{}
	cfg := l.coldConfig(prober, []Candidate{c}, configured(seed.Addr))
	cfg.MarkSink = sink
	p := newPool(t, cfg)

	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if got := prober.probed(); len(got) != 1 || got[0].Candidate != c || got[0].Authorization != coldToken {
		t.Fatalf("probes = %+v, want one of %+v with the token", got, c)
	}
	if got, _ := auth.Load().(string); got != coldToken {
		t.Fatalf("probe Authorization = %q, want the token", got)
	}
	if n := cand.Counts.Requests.Load(); n != 2 {
		t.Fatalf("candidate got %d requests, want the probe and the read", n)
	}
	mustActive(t, p, c.Address)
	lines := l.failovers()
	if len(lines) != 1 || !strings.Contains(lines[0].Message, "a cold-start candidate for node pve2 from "+
		"placement.az_map that proved its membership, which stays active for this call") {
		t.Fatalf("failover lines = %v", lines)
	}
	wantMarks(t, sink.all(), map[string]Phase{seed.Addr: PhaseDial})

	resp, err = p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if prober.calls.Load() != 1 || seed.Counts.Dials.Load() != 1 || cand.Counts.Requests.Load() != 3 {
		t.Fatalf("probes %d, seed dials %d, candidate requests %d, want 1, 1, 3",
			prober.calls.Load(), seed.Counts.Dials.Load(), cand.Counts.Requests.Load())
	}

	// The login and a configured-only request never take the candidate.
	_, err = p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, seed.Addr, "/access/ticket", "u=p"))
	mustUnreachable(t, err, seed.Addr)
	_, err = p.RoundTrip(tokenRequest(t, WithConfiguredOnly(context.Background()), http.MethodGet, seed.Addr,
		"/cluster/status", ""))
	mustUnreachable(t, err, seed.Addr)
	if n := cand.Counts.Requests.Load(); n != 3 {
		t.Fatalf("candidate got %d requests after the login, want 3", n)
	}
	if order, _ := p.walkOrder(true); len(order) != 1 || order[0].addr != seed.Addr {
		t.Fatalf("configured-only order = %v", order)
	}
}

// A fresh mutation whose seed proved it unsent goes to the accepted candidate,
// its tether follows the candidate through the candidate's own clones, and the
// next mutation with its key goes there too, never through a routed clone.
func TestColdStartMutationTetherFollowsTheCandidate(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr)))
	for range 2 {
		resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, seed.Addr, fanOutRoute,
			"vmid=101"))
		mustStatus(t, resp, err, http.StatusOK)
	}
	if n := cand.Counts.Method(http.MethodPost); n != 2 {
		t.Fatalf("candidate got %d POSTs, want 2", n)
	}
	p.mu.Lock()
	routed := len(p.routed)
	p.mu.Unlock()
	if routed != 0 {
		t.Fatalf("%d routed clones, want the mutation on the candidate's own", routed)
	}
}

// A walk makes at most four hops, so one seed leaves room for three
// candidates, and four seeds leave none.
func TestColdStartCountsAgainstTheHopCap(t *testing.T) {
	for seeds := 1; seeds <= 4; seeds++ {
		t.Run(strconv.Itoa(seeds), func(t *testing.T) {
			l := newLab(t)
			var eps []Endpoint
			var want []string
			for range seeds {
				r := l.refused(t)
				eps = append(eps, configured(r.Addr))
				want = append(want, r.Addr)
			}
			prober := &fakeProber{fn: func(context.Context, ProbeRequest) error { return errors.New("no answer") }}
			cands := namedCandidates(4)
			p := newPool(t, l.coldConfig(prober, cands, eps...))
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, eps[0].Address,
				"/cluster/resources", ""))
			for _, c := range cands[:4-seeds] {
				want = append(want, c.Address)
			}
			mustUnreachable(t, err, want...)
			if got := int(prober.calls.Load()); got != 4-seeds {
				t.Fatalf("%d probes, want %d", got, 4-seeds)
			}
			if seeds < 4 && !strings.Contains(err.Error(), "cold-start candidate for node pve2 from placement.az_map") {
				t.Fatalf("err = %v, want it to name the refused candidate", err)
			}
		})
	}
}

// A probe counts as connect time, so a seed that spends 4 seconds and probes
// that spend 25 each leave room for two probes, and the walk ends within 55
// seconds. A seed whose own attempt outlasts the connect budget never reaches
// the cold start.
func TestColdStartCountsAgainstTheBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dial   time.Duration
		probes int32
		spent  time.Duration
	}{
		{"two probes fit", 4 * time.Second, 2, 54 * time.Second},
		{"seed over the connect budget", 31 * time.Second, 0, 31 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			clock := newFakeClock()
			seed := pvetest.NewRefused(t)
			l.d.set(seed.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
				clock.advance(tc.dial)
				return seed.DialContext(ctx, network, addr)
			})
			prober := &fakeProber{fn: func(context.Context, ProbeRequest) error {
				clock.advance(probeLimit)
				return errors.New("no answer")
			}}
			p := newPool(t, l.coldConfig(prober, namedCandidates(3), configured(seed.Addr)))
			start := clock.now()
			_, err := p.roundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""), boundsOn(clock))
			if err == nil {
				t.Fatal("the walk answered")
			}
			if got := prober.calls.Load(); got != tc.probes {
				t.Fatalf("%d probes, want %d", got, tc.probes)
			}
			if got := clock.now().Sub(start); got != tc.spent || got > 55*time.Second {
				t.Fatalf("walk spent %v, want %v", got, tc.spent)
			}
		})
	}
}

// Every request that may not reach a candidate leaves the cold start alone:
// a walk where a configured endpoint met its header deadline, failed TLS
// verification, or answered a status, the login, a configured-only request, a
// routed request, a tethered mutation, an upload without GetBody, and a
// request without an API token or with a ticket.
func TestColdStartNeverRunsForBarredRequests(t *testing.T) {
	type setup struct {
		l     *lab
		cfg   Config
		seeds []string
	}
	cases := []struct {
		name  string
		build func(t *testing.T, s *setup)
		req   func(t *testing.T, s *setup, p *Pool) *http.Request
	}{
		{"header deadline", func(t *testing.T, s *setup) {
			hung := pvetest.NewHung(t, s.l.leaf)
			r := s.l.refused(t)
			s.seeds = []string{hung.Addr, r.Addr}
			s.cfg.headerDeadline = 50 * time.Millisecond
		}, nil},
		{"tls verify", func(t *testing.T, s *setup) { s.seeds = []string{s.l.otherLeaf(t).Addr} }, nil},
		{"status hop", func(t *testing.T, s *setup) {
			routes := pvetest.NewRoutes()
			routes.Fail(http.MethodGet, "/cluster/resources", http.StatusBadGateway, "down")
			s.seeds = []string{s.l.live(t, routes).Addr}
		}, nil},
		{"login", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			return tokenRequest(t, context.Background(), http.MethodPost, s.seeds[0], "/access/ticket", "u=p")
		}},
		{"configured only", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			return tokenRequest(t, WithConfiguredOnly(context.Background()), http.MethodGet, s.seeds[0],
				"/cluster/resources", "")
		}},
		{"routed", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			return tokenRequest(t, context.Background(), http.MethodGet, s.l.refused(t).Addr, "/cluster/resources", "")
		}},
		{"tethered mutation", nil, func(t *testing.T, s *setup, p *Pool) *http.Request {
			req := tokenRequest(t, context.Background(), http.MethodPost, s.seeds[0], fanOutRoute, "vmid=101")
			injectTether(p, tetherKeyOf(req), targetOf(endpointOf(t, p, s.seeds[0])), false)
			return req
		}},
		{"upload without GetBody", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			req := tokenRequest(t, context.Background(), http.MethodPost, s.seeds[0], fanOutRoute, "")
			req.Body, req.GetBody = io.NopCloser(strings.NewReader("disk")), nil
			return req
		}},
		{"no token", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			return apiRequest(t, context.Background(), http.MethodGet, s.seeds[0], "/cluster/resources", "")
		}},
		{"ticket cookie", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			req := apiRequest(t, context.Background(), http.MethodGet, s.seeds[0], "/cluster/resources", "")
			req.Header.Set("Cookie", "PVEAuthCookie=ticket")
			return req
		}},
		{"token beside a CSRF token", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			req := tokenRequest(t, context.Background(), http.MethodGet, s.seeds[0], "/cluster/resources", "")
			req.Header.Set("CSRFPreventionToken", "csrf")
			return req
		}},
		{"bare token scheme", nil, func(t *testing.T, s *setup, _ *Pool) *http.Request {
			req := apiRequest(t, context.Background(), http.MethodGet, s.seeds[0], "/cluster/resources", "")
			req.Header.Set("Authorization", apiTokenScheme)
			return req
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			cand, c := l.candidate(t, "pve2.example.com", nil)
			prober := &fakeProber{}
			s := &setup{l: l, cfg: l.coldConfig(prober, []Candidate{c})}
			if tc.build != nil {
				tc.build(t, s)
			} else {
				s.seeds = []string{l.refused(t).Addr}
			}
			for _, a := range s.seeds {
				s.cfg.Endpoints = append(s.cfg.Endpoints, configured(a))
			}
			p := newPool(t, s.cfg)
			req := tokenRequest(t, context.Background(), http.MethodGet, s.seeds[0], "/cluster/resources", "")
			if tc.req != nil {
				req = tc.req(t, s, p)
			}
			resp, _ := p.RoundTrip(req)
			closeBody(resp)
			if n := prober.calls.Load(); n != 0 {
				t.Fatalf("%d probes, want none", n)
			}
			if n := cand.Counts.Conns.Load(); n != 0 {
				t.Fatalf("the candidate got %d connections, want none", n)
			}
			p.cold.mu.Lock()
			begun := p.cold.begun
			p.cold.mu.Unlock()
			if begun {
				t.Fatal("the cold start began")
			}
		})
	}
}

// coldStartFixture is one seed that refuses, one live candidate, and a prober
// that blocks until released, with every walk that waits for the cold start
// reporting on waiting.
type coldStartFixture struct {
	l       *lab
	seed    *pvetest.Refused
	cand    *pvetest.Server
	prober  *fakeProber
	p       *Pool
	entered chan struct{}
	release chan struct{}
	waiting chan struct{}
}

func newColdStartFixture(t *testing.T, refuse bool) *coldStartFixture {
	t.Helper()
	l := newLab(t)
	f := &coldStartFixture{
		l:       l,
		seed:    l.refused(t),
		entered: make(chan struct{}),
		release: make(chan struct{}),
		waiting: make(chan struct{}, 16),
	}
	var c Candidate
	f.cand, c = l.candidate(t, "pve2.example.com", nil)
	var once sync.Once
	f.prober = &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		once.Do(func() { close(f.entered) })
		<-f.release
		if refuse {
			return errors.New("no answer")
		}
		return probeStatus(ctx, req)
	}}
	cfg := l.coldConfig(f.prober, []Candidate{c}, configured(f.seed.Addr))
	cfg.onColdStartWait = func() { f.waiting <- struct{}{} }
	f.p = newPool(t, cfg)
	return f
}

type walkResult struct {
	status int
	err    error
}

// walk runs one token read through the pool on its own goroutine under b.
func (f *coldStartFixture) walk(t *testing.T, ctx context.Context, b bounds, out chan<- walkResult) {
	req := tokenRequest(t, ctx, http.MethodGet, f.seed.Addr, "/cluster/resources", "")
	go func() {
		resp, err := f.p.roundTrip(req, b)
		if err != nil {
			out <- walkResult{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		out <- walkResult{status: resp.StatusCode}
	}()
}

// Concurrent walks that reach the cold start share one run: the prober is
// called once, every walk sends to the candidate it accepted, and only the
// walk whose answer moved the active endpoint logs the failover.
func TestColdStartConcurrentWalksShareOneRun(t *testing.T) {
	f := newColdStartFixture(t, false)
	clock := newFakeClock()
	const walks = 6
	out := make(chan walkResult, walks)
	f.walk(t, context.Background(), boundsOn(clock), out)
	<-f.entered
	for range walks - 1 {
		f.walk(t, context.Background(), boundsOn(clock), out)
	}
	for range walks - 1 {
		<-f.waiting
	}
	close(f.release)
	for range walks {
		if r := <-out; r.err != nil || r.status != http.StatusOK {
			t.Fatalf("walk got %d, %v, want 200", r.status, r.err)
		}
	}
	if n := f.prober.calls.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
	if n := f.cand.Counts.Requests.Load(); n != walks+1 {
		t.Fatalf("candidate got %d requests, want %d", n, walks+1)
	}
	if lines := f.l.failovers(); len(lines) != 1 {
		t.Fatalf("failover lines = %v, want one", lines)
	}
}

// A cold start that accepts nothing ends it for the process: the walks that
// waited return their own unreachable error, and a later walk neither probes
// nor waits.
func TestColdStartRefusedRunEndsTheColdStart(t *testing.T) {
	f := newColdStartFixture(t, true)
	clock := newFakeClock()
	out := make(chan walkResult, 2)
	f.walk(t, context.Background(), boundsOn(clock), out)
	<-f.entered
	f.walk(t, context.Background(), boundsOn(clock), out)
	<-f.waiting
	close(f.release)
	var withCandidate int
	for range 2 {
		r := <-out
		var eu *EntryUnreachableError
		if !errors.As(r.err, &eu) {
			t.Fatalf("err = %v, want an EntryUnreachableError", r.err)
		}
		if len(eu.Endpoints()) == 2 {
			withCandidate++
		}
	}
	if withCandidate != 1 {
		t.Fatalf("%d walks listed the candidate, want only the one that probed it", withCandidate)
	}
	f.walk(t, context.Background(), boundsOn(clock), out)
	r := <-out
	mustUnreachable(t, r.err, f.seed.Addr)
	if n := f.prober.calls.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
	if n := len(f.waiting); n != 0 {
		t.Fatalf("%d walks waited after the run ended", n)
	}
	if n := f.cand.Counts.Conns.Load(); n != 0 {
		t.Fatalf("candidate got %d connections from a prober that never probed", n)
	}
	if n := len(f.l.entries(slog.LevelDebug, "pve endpoints: no cold-start candidate")); n != 1 {
		t.Fatalf("%d end lines, want 1", n)
	}
}

// A walk waiting for the cold start stops when its caller gives up, and the
// run it waited for still completes for the walk that runs it.
func TestColdStartWaiterHonoursItsContext(t *testing.T) {
	f := newColdStartFixture(t, false)
	clock := newFakeClock()
	owner, waiter := make(chan walkResult, 1), make(chan walkResult, 1)
	f.walk(t, context.Background(), boundsOn(clock), owner)
	<-f.entered
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.walk(t, ctx, boundsOn(clock), waiter)
	<-f.waiting
	cancel()
	if r := <-waiter; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("waiter err = %v, want context.Canceled", r.err)
	}
	close(f.release)
	if r := <-owner; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("owner got %d, %v", r.status, r.err)
	}
}

// A walk waiting for the cold start waits only for what is left of its own
// walk budget, measured from the walk's start, and then returns its own
// unreachable error.
func TestColdStartWaiterStopsAtItsWalkBudget(t *testing.T) {
	f := newColdStartFixture(t, false)
	owner, waiter := make(chan walkResult, 1), make(chan walkResult, 1)
	f.walk(t, context.Background(), boundsOn(newFakeClock()), owner)
	<-f.entered

	const left = 20 * time.Millisecond
	clock := newFakeClock()
	b := boundsOn(clock)
	b.connect = 2 * walkBudget
	f.l.d.set(f.seed.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
		clock.advance(walkBudget - left)
		return f.seed.DialContext(ctx, network, addr)
	})
	f.walk(t, context.Background(), b, waiter)
	<-f.waiting
	r := <-waiter
	mustUnreachable(t, r.err, f.seed.Addr)
	close(f.release)
	if r := <-owner; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("owner got %d, %v", r.status, r.err)
	}
}

// A walk whose budget has already run out when it reaches a running cold
// start does not wait at all.
func TestColdStartWaiterWithNoBudgetLeftDoesNotWait(t *testing.T) {
	f := newColdStartFixture(t, false)
	owner := make(chan walkResult, 1)
	f.walk(t, context.Background(), boundsOn(newFakeClock()), owner)
	<-f.entered
	w := &walker{p: f.p, req: tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr, "/", "")}
	clock := newFakeClock()
	w.bounds = boundsOn(clock)
	w.budget = w.bounds.begin()
	clock.advance(walkBudget)
	if stop := w.awaitColdStart(f.p.cold, f.p.cold.done); stop != stopBudget {
		t.Fatalf("stop = %v, want stopBudget", stop)
	}
	if n := len(f.waiting); n != 0 {
		t.Fatal("the walk waited with no budget left")
	}
	close(f.release)
	if r := <-owner; r.err != nil {
		t.Fatalf("owner: %v", r.err)
	}
}

// A candidate whose certificate does not cover its derived name is refused at
// the handshake, before the probe's request or the token leaves.
func TestColdStartCandidateFailingVerificationGetsNoRequest(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	bad := pvetest.NewLive(t, l.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve9.example.com"}}), nil)
	c := Candidate{Node: "pve2", Address: "pve2.example.com:8006"}
	l.d.set(c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, bad.Addr)
	})
	var probeErr error
	prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		probeErr = probeStatus(ctx, req)
		return probeErr
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr, c.Address)
	mustConnectPhase(t, probeErr, c.Address, PhaseTLSVerify)
	if n := bad.Counts.Requests.Load(); n != 0 {
		t.Fatalf("the unverified candidate got %d requests", n)
	}
}

// The pool accepts a candidate only on a probe it sent that got a 2xx, so a
// prober that says yes without probing, or after a 403, accepts nothing, and a
// prober that panics refuses its candidate, with the first panic at Warn and
// the next at Debug.
func TestColdStartAcceptsOnlyAProvedProbe(t *testing.T) {
	cases := []struct {
		name string
		fn   func(ctx context.Context, req ProbeRequest) error
		want string
	}{
		{"no probe", func(context.Context, ProbeRequest) error { return nil }, "without probing"},
		{"forbidden", func(ctx context.Context, req ProbeRequest) error {
			_ = probeStatus(ctx, req)
			return nil
		}, "answered with HTTP 403"},
		{"panic", func(context.Context, ProbeRequest) error { panic("boom") }, "prober panicked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			routes := pvetest.NewRoutes()
			routes.Fail(http.MethodGet, "/cluster/status", http.StatusForbidden, "no")
			_, c2 := l.candidate(t, "pve2.example.com", routes)
			_, c3 := l.candidate(t, "pve3.example.com", routes)
			prober := &fakeProber{fn: tc.fn}
			p := newPool(t, l.coldConfig(prober, []Candidate{c2, c3}, configured(seed.Addr)))
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustUnreachable(t, err, seed.Addr, c2.Address, c3.Address)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if tc.name != "panic" {
				return
			}
			warn := l.entries(slog.LevelWarn, "pve endpoint pve2.example.com:8006: the cold-start prober panicked")
			debug := l.entries(slog.LevelDebug, "pve endpoint pve3.example.com:8006: the cold-start prober panicked")
			if len(warn) != 1 || len(debug) != 1 {
				t.Fatalf("panic lines: warn %v, debug %v", warn, debug)
			}
		})
	}
}

// The probe transport sends one GET of the probe path over https to its
// candidate, and refuses anything else before any dial.
func TestProbeTransportRefusesOtherRequests(t *testing.T) {
	l := newLab(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured("10.0.0.1:8006")))
	newReq := func(method, url string, body io.Reader) *http.Request {
		req, err := http.NewRequestWithContext(context.Background(), method, url, body)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	base := "https://" + c.Address
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"method", newReq(http.MethodPost, base+ProbePath, nil)},
		{"body", newReq(http.MethodGet, base+ProbePath, strings.NewReader("x"))},
		{"scheme", newReq(http.MethodGet, "http://"+c.Address+ProbePath, nil)},
		{"host", newReq(http.MethodGet, "https://pve3.example.com:8006"+ProbePath, nil)},
		{"path", newReq(http.MethodGet, base+APIPrefix+"/cluster/resources", nil)},
		{"no URL", &http.Request{Method: http.MethodGet}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &probeTransport{c: p.cold.candidates[0]}
			if _, err := rt.RoundTrip(tc.req); !errors.Is(err, errProbeRequest) {
				t.Fatalf("err = %v, want the probe refusal", err)
			}
			if err := rt.proof(); !errors.Is(err, errNotProbed) {
				t.Fatalf("proof = %v, want errNotProbed", err)
			}
		})
	}
	if n := cand.Counts.Conns.Load(); n != 0 {
		t.Fatalf("a refused probe made %d connections", n)
	}
	rt := &probeTransport{c: p.cold.candidates[0]}
	resp, err := rt.RoundTrip(newReq(http.MethodGet, base+ProbePath, nil))
	mustStatus(t, resp, err, http.StatusOK)
	if _, err := rt.RoundTrip(newReq(http.MethodGet, base+ProbePath, nil)); !errors.Is(err, errProbeReused) {
		t.Fatalf("second probe err = %v, want errProbeReused", err)
	}
	if err := rt.proof(); err != nil {
		t.Fatalf("proof = %v", err)
	}
	if rt.c.probe.ResponseHeaderTimeout != probeHeaderDeadline {
		t.Fatalf("probe header deadline = %v", rt.c.probe.ResponseHeaderTimeout)
	}
	if ep := rt.c.ep; ep.transports.plain.ResponseHeaderTimeout != 0 || ep.transports.idempotent != ep.transports.plain {
		t.Fatal("the candidate's request clone carries a header deadline")
	}
}

// An accepted candidate that later fails TLS verification gets no mark, is
// logged at Error, and leaves every later walk. A walk that read its order
// before that refuses the hop before any dial.
func TestColdStartCandidateFailingVerificationLater(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	sink := &markRecorder{}
	cfg := l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr))
	cfg.MarkSink = sink
	p := newPool(t, cfg)
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)

	early := p.newWalker(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""),
		false, defaultBounds())
	if len(early.order) != 2 || early.order[0].addr != c.Address {
		t.Fatalf("order after acceptance = %v, want the candidate first", early.order)
	}

	bad := l.otherLeaf(t)
	l.d.set(c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, bad.Addr)
	})
	p.CloseIdleConnections()
	_, err = p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, c.Address, seed.Addr)
	if lines := l.entries(slog.LevelError, "pve endpoint "+c.Address+": the cold-start candidate"); len(lines) != 1 {
		t.Fatalf("error lines = %v", lines)
	}
	wantMarks(t, sink.all(), map[string]Phase{seed.Addr: PhaseDial})
	if order, _ := p.walkOrder(false); len(order) != 1 || order[0].addr != seed.Addr {
		t.Fatalf("order after the failure = %v", order)
	}

	requests := cand.Counts.Requests.Load() + bad.Counts.Requests.Load()
	resp, _, err = early.run()
	closeBody(resp)
	if !errors.Is(err, errCandidateRevoked) {
		t.Fatalf("early walk err = %v, want the revoked refusal", err)
	}
	if n := cand.Counts.Requests.Load() + bad.Counts.Requests.Load(); n != requests {
		t.Fatal("the early walk sent to the revoked candidate")
	}
	if ep := p.memberAt(c.Address); ep == nil {
		t.Fatal("memberAt lost the revoked candidate, which a tether on it must still find")
	}
}

// Candidates never count toward a configured endpoint's final hop, so a seed
// behind an accepted candidate is still final and gets no header deadline.
func TestColdStartCandidateLeavesTheFinalHop(t *testing.T) {
	l := newLab(t)
	seed, two := l.refused(t), l.refused(t)
	_, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr), configured(two.Addr)))
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	w := p.newWalker(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""),
		false, defaultBounds())
	if len(w.order) != 3 || w.order[0].kind != kindCandidate {
		t.Fatalf("order = %v", w.order)
	}
	w.made = 1
	if !w.wantsDeadline(1, true) {
		t.Fatal("the first seed behind the candidate lost its deadline")
	}
	w.made = 2
	if w.wantsDeadline(2, true) {
		t.Fatal("the last seed behind the candidate got a deadline")
	}
	w.order = []*endpoint{w.order[1], w.order[2], w.order[0]}
	w.made = 0
	if !w.wantsDeadline(0, true) {
		t.Fatal("the first seed ahead of the candidate lost its deadline")
	}
	w.made = 1
	if w.wantsDeadline(1, true) {
		t.Fatal("the candidate after the last seed made the seed non-final")
	}
}

func TestKindStringNamesTheCandidate(t *testing.T) {
	if got := kindCandidate.String(); got != "candidate" {
		t.Fatalf("String = %q", got)
	}
	if got := Kind(9).String(); got != "kind(9)" {
		t.Fatalf("String = %q", got)
	}
}
