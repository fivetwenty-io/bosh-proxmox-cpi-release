package endpointpool

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// coldBegun reports whether a walk has claimed p's cold start.
func coldBegun(p *Pool) bool {
	p.cold.mu.Lock()
	defer p.cold.mu.Unlock()
	return p.cold.begun
}

// onlyProbes fails the test unless every request s saw was the membership
// probe, so the token never reached s with a request the walk carries.
func onlyProbes(t *testing.T, s *pvetest.Server) {
	t.Helper()
	for _, r := range s.Seen() {
		if r.Path != ProbePath {
			t.Fatalf("the candidate got %s %s with Authorization %q, want only the probe", r.Method, r.Path,
				r.Header.Get("Authorization"))
		}
	}
}

// probeRequest builds the membership probe for req under ctx.
func probeRequest(t *testing.T, ctx context.Context, req ProbeRequest) *http.Request {
	t.Helper()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+req.Candidate.Address+ProbePath, nil)
	if err != nil {
		t.Error(err)
		return nil
	}
	hreq.Header.Set("Authorization", req.Authorization)
	return hreq
}

// A pool given candidates without a prober, or with a typed nil one, carries
// no candidates, says so at Warn, and never sends to a candidate.
func TestNewWithoutAProberCarriesNoCandidates(t *testing.T) {
	var typedNil *fakeProber
	for _, tc := range []struct {
		name   string
		prober Prober
	}{
		{"nil", nil},
		{"typed nil", typedNil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			cand, c := l.candidate(t, "pve2.example.com", nil)
			p := newPool(t, l.coldConfig(tc.prober, []Candidate{c}, configured(seed.Addr)))
			if p.cold != nil || p.candidateCount() != 0 {
				t.Fatalf("the pool carries %d candidates without a prober", p.candidateCount())
			}
			warn := l.entries(slog.LevelWarn, "pve endpoints: 1 cold-start candidates from placement.az_map came "+
				"without a prober")
			if len(warn) != 1 {
				t.Fatalf("warn lines = %v", warn)
			}
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustUnreachable(t, err, seed.Addr)
			if n := cand.Counts.Conns.Load(); n != 0 {
				t.Fatalf("the candidate got %d connections", n)
			}
		})
	}
	if !noProber(Prober(nil)) || noProber(&fakeProber{}) {
		t.Fatal("noProber misreads a prober")
	}
}

// A prober that starts its probe and returns nil before the probe is answered
// proves nothing. The candidate is refused while its 403 is still on the way,
// the cold start stays begun, the probe that was in flight gets no response,
// and no request the walk carries, nor the token with one, ever reaches the
// candidate.
func TestColdStartRefusesAProberThatReturnsBeforeItsAnswer(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	arrived, answer := make(chan struct{}), make(chan struct{})
	var once sync.Once
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		select {
		case <-answer:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusForbidden)
	})
	cand, c := l.candidate(t, "pve2.example.com", routes)
	late := make(chan error, 1)
	prober := &fakeProber{fn: func(_ context.Context, req ProbeRequest) error {
		hreq := probeRequest(t, context.Background(), req)
		go func() {
			resp, err := req.Transport.RoundTrip(hreq)
			if resp != nil {
				_ = resp.Body.Close()
				err = errors.New("the late probe got a response")
			}
			late <- err
		}()
		<-arrived
		return nil
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr, c.Address)
	if !errors.Is(err, errProbeInFlight) {
		t.Fatalf("err = %v, want the in-flight refusal", err)
	}
	close(answer)
	select {
	case lateErr := <-late:
		if lateErr == nil || strings.Contains(lateErr.Error(), "got a response") {
			t.Fatalf("the in-flight probe ended with %v, want an error and no response", lateErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight probe never ended")
	}
	if !coldBegun(p) {
		t.Fatal("the cold start is not begun after a refusal")
	}
	if p.memberAt(c.Address) != nil {
		t.Fatal("the refused candidate is a member")
	}
	_, err = p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr)
	if n := cand.Counts.Requests.Load(); n != 1 {
		t.Fatalf("the candidate got %d requests, want the one probe", n)
	}
	onlyProbes(t, cand)
	if n := prober.calls.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
}

// The probe transport is sealed when the prober returns, so a prober that
// keeps it and sends later sends nothing.
func TestColdStartRefusesAProbeSentAfterTheProberReturned(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	var kept ProbeRequest
	prober := &fakeProber{fn: func(_ context.Context, req ProbeRequest) error {
		kept = req
		return nil
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr, c.Address)
	if !errors.Is(err, errNotProbed) {
		t.Fatalf("err = %v, want errNotProbed", err)
	}
	if _, err := kept.Transport.RoundTrip(probeRequest(t, context.Background(), kept)); !errors.Is(err,
		errProbeSealed) {
		t.Fatalf("late probe err = %v, want errProbeSealed", err)
	}
	if n := cand.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the late probe made %d connections", n)
	}
}

// The pool accepts a candidate only on a complete proof, so a prober that
// accepts a 3xx, a probe that failed, or a probe whose body it left open
// accepts nothing, and a prober that panics after a proved probe refuses its
// candidate. The pool logs each refusal of its own at Debug.
func TestColdStartRefusesAnIncompleteProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		dial   bool
		fn     func(t *testing.T, ctx context.Context, req ProbeRequest) error
		want   error
		text   string
	}{
		{"redirect", http.StatusFound, false, func(t *testing.T, ctx context.Context, req ProbeRequest) error {
			resp, err := req.Transport.RoundTrip(probeRequest(t, ctx, req))
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil
		}, nil, "answered with HTTP 302 Found"},
		{"failed probe", http.StatusOK, true, func(t *testing.T, ctx context.Context, req ProbeRequest) error {
			_, _ = req.Transport.RoundTrip(probeRequest(t, ctx, req))
			return nil
		}, nil, "the prober accepted a probe that failed"},
		{"body left open", http.StatusOK, false, func(t *testing.T, ctx context.Context, req ProbeRequest) error {
			_, err := req.Transport.RoundTrip(probeRequest(t, ctx, req))
			return err
		}, errProbeBodyOpen, ""},
		{"panic after a proved probe", http.StatusOK, false,
			func(_ *testing.T, ctx context.Context, req ProbeRequest) error {
				if err := probeStatus(ctx, req); err != nil {
					return err
				}
				panic("boom")
			}, errProberPanicked, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			routes := pvetest.NewRoutes()
			routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "https://pve9.example.com:8006"+ProbePath)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"data":[]}`)
			})
			_, c := l.candidate(t, "pve2.example.com", routes)
			if tc.dial {
				gone := l.refused(t)
				l.d.set(c.Address, gone.DialContext)
			}
			prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error { return tc.fn(t, ctx, req) }}
			p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustUnreachable(t, err, seed.Addr, c.Address)
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want %q", err, tc.text)
			}
			debug := l.entries(slog.LevelDebug, "pve endpoint pve2.example.com:8006: the cold-start candidate for "+
				"node pve2 from placement.az_map is refused, because ")
			wantLines := 1
			if errors.Is(tc.want, errProberPanicked) {
				wantLines = 0
			}
			if len(debug) != wantLines {
				t.Fatalf("refusal lines = %v, want %d", debug, wantLines)
			}
		})
	}
}

// The seal closes the probe's response body when the prober left it open, so
// the candidate's connection is let go at once, and the prober can read
// nothing more from it.
func TestColdStartSealClosesTheBodyTheProberLeftOpen(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	gone := make(chan struct{})
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(gone)
		case <-time.After(10 * time.Second):
		}
	})
	_, c := l.candidate(t, "pve2.example.com", routes)
	var body io.ReadCloser
	prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		resp, err := req.Transport.RoundTrip(probeRequest(t, ctx, req))
		if err != nil {
			return err
		}
		body = resp.Body
		return nil
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	if !errors.Is(err, errProbeBodyOpen) {
		t.Fatalf("err = %v, want errProbeBodyOpen", err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the candidate's connection stayed open after the prober returned")
	}
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, errProbeSealed) {
		t.Fatalf("read after the seal = %v, want errProbeSealed", err)
	}
}

// The probe limit holds whatever context the prober puts on its request, so a
// candidate that sends its headers and stalls its body costs no more than the
// limit.
func TestColdStartProbeLimitBoundsAProberThatIgnoresItsContext(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	_, c := l.candidate(t, "pve2.example.com", routes)
	prober := &fakeProber{fn: func(_ context.Context, req ProbeRequest) error {
		resp, err := req.Transport.RoundTrip(probeRequest(t, context.Background(), req))
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}}
	cfg := l.coldConfig(prober, []Candidate{c}, configured(seed.Addr))
	cfg.probeLimit = 200 * time.Millisecond
	p := newPool(t, cfg)
	start := time.Now()
	_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr, c.Address)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the probe took %v under a 200ms limit", took)
	}
}

// The prober's context ends after the 25-second probe limit, however long the
// request has left.
func TestColdStartProberSeesTheProbeLimit(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	_, c := l.candidate(t, "pve2.example.com", nil)
	var left time.Duration
	prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("the probe context has no deadline")
		}
		left = time.Until(deadline)
		return probeStatus(ctx, req)
	}}
	p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
	if p.cold.limits != (coldLimits{probeLimit, candidateDialCap, candidateHandshakeCap, defaultHeaderDeadline}) {
		t.Fatalf("limits = %+v", p.cold.limits)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if left > probeLimit || left < probeLimit-5*time.Second {
		t.Fatalf("the probe had %v left, want about %v", left, probeLimit)
	}
}

// A candidate's connections send its own host as the TLS server name, for the
// probe and for the request alike.
func TestColdStartCandidateSendsItsOwnServerName(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	cand, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr)))
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	seen := cand.Seen()
	if len(seen) != 2 {
		t.Fatalf("candidate saw %d requests, want 2", len(seen))
	}
	for _, r := range seen {
		if r.ServerName != "pve2.example.com" {
			t.Fatalf("%s %s went with server name %q", r.Method, r.Path, r.ServerName)
		}
	}
}

// A candidate's dial is capped at 5 seconds and its handshake at its own cap,
// so a candidate that accepts the connection and never speaks costs no more
// than that.
func TestColdStartCandidateConnectCaps(t *testing.T) {
	t.Run("dial", func(t *testing.T) {
		l := newLab(t)
		seed := l.refused(t)
		cand, c := l.candidate(t, "pve2.example.com", nil)
		var left time.Duration
		l.d.set(c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
			if deadline, ok := ctx.Deadline(); ok {
				left = time.Until(deadline)
			}
			return (&net.Dialer{}).DialContext(ctx, network, cand.Addr)
		})
		p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr)))
		resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
			"/cluster/resources", ""))
		mustStatus(t, resp, err, http.StatusOK)
		if left > candidateDialCap || left < candidateDialCap-2*time.Second {
			t.Fatalf("the candidate's dial had %v, want about %v", left, candidateDialCap)
		}
	})
	t.Run("handshake", func(t *testing.T) {
		l := newLab(t)
		seed := l.refused(t)
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var (
			mu    sync.Mutex
			conns []net.Conn
		)
		t.Cleanup(func() {
			_ = ln.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range conns {
				_ = conn.Close()
			}
		})
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
			}
		}()
		c := Candidate{Node: "pve2", Address: "pve2.example.com:8006"}
		l.d.set(c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		})
		var probeErr error
		prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
			probeErr = probeStatus(ctx, req)
			return probeErr
		}}
		cfg := l.coldConfig(prober, []Candidate{c}, configured(seed.Addr))
		cfg.candidateHandshakeCap = 100 * time.Millisecond
		p := newPool(t, cfg)
		start := time.Now()
		_, err = p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
			"/cluster/resources", ""))
		mustUnreachable(t, err, seed.Addr, c.Address)
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("a silent candidate took %v under a 100ms handshake cap", took)
		}
		mustConnectPhase(t, probeErr, c.Address, PhaseTLSHandshake)
	})
}

// Only a request with exactly one API token header and no ticket may begin the
// cold start.
func TestTokenAuthorizationNeedsExactlyOneToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   string
	}{
		{"one token", http.Header{"Authorization": {coldToken}}, coldToken},
		{"two tokens", http.Header{"Authorization": {coldToken, coldToken}}, ""},
		{"no header", http.Header{}, ""},
		{"bare scheme", http.Header{"Authorization": {apiTokenScheme}}, ""},
		{"other scheme", http.Header{"Authorization": {"Bearer x"}}, ""},
		{"cookie", http.Header{"Authorization": {coldToken}, "Cookie": {"PVEAuthCookie=t"}}, ""},
		{"CSRF token", http.Header{"Authorization": {coldToken}, "Csrfpreventiontoken": {"c"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenAuthorization(&http.Request{Header: tc.header}); got != tc.want {
				t.Fatalf("tokenAuthorization = %q, want %q", got, tc.want)
			}
		})
	}
}

// Gate decides every discovery outcome the endpoint cache can record. The
// outcomes are read from the cache's source, because this package may not
// import the cache, so an outcome added there fails here until Gate decides
// it.
func TestGateCoversEveryCacheOutcome(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "endpointcache", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no endpoint cache source: %v", err)
	}
	fset := token.NewFileSet()
	var outcomes []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, discoveryOutcomes(t, f)...)
	}
	slices.Sort(outcomes)
	want := []string{outcomeNoCandidates, outcomeNoStatus, outcomeOK, outcomePublicSuffix, outcomeStandalone}
	slices.Sort(want)
	if !slices.Equal(outcomes, want) {
		t.Fatalf("the cache records outcomes %q, and Gate decides %q", outcomes, want)
	}
	nodes := []string{"pve2"}
	for _, o := range outcomes {
		if got := Gate(nodes, 0, o); got != (o != outcomeStandalone) {
			t.Fatalf("Gate(%q) = %v", o, got)
		}
	}
	if !Gate(nodes, 0, outcomeNone) {
		t.Fatal("Gate refuses a missing or invalid file")
	}
}

// discoveryOutcomes returns the string values of the DiscoveryOutcome
// constants f declares.
func discoveryOutcomes(t *testing.T, f *ast.File) []string {
	t.Helper()
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "DiscoveryOutcome" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a DiscoveryOutcome constant is not a string literal: %T", v)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, s)
			}
		}
	}
	return out
}
