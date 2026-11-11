package endpointpool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// statusRoutes returns candidate routes whose probe path answers with code
// once hang is false, and whose probe waits for the client to go away while
// hang is true, signalling arrived when it starts waiting.
func statusRoutes(code int, hang *atomic.Bool, arrived chan<- struct{}) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		if hang != nil && hang.Swap(false) {
			arrived <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	return routes
}

// mustHandOnLine checks the one end line of a run that stopped for reason and
// passed unprobed candidates to the next request.
func mustHandOnLine(t *testing.T, l *lab, reason string, unprobed int64) {
	t.Helper()
	end := l.entries(slog.LevelDebug, "pve endpoints: the cold start from placement.az_map accepted no candidate")
	if len(end) != 1 || end[0].Attrs["reason"] != reason || end[0].Attrs["unprobed"] != unprobed ||
		!strings.HasSuffix(end[0].Message, "which the next request that reaches the cold start probes") {
		t.Fatalf("end lines = %+v, want one %s line with %d unprobed", end, reason, unprobed)
	}
}

// A probe that the owner's own cancellation cuts before its candidate answered
// refuses nothing. The candidate counts as never probed, so with one candidate
// the cold start is not over, and the next request probes it and is answered.
// That holds whether the prober waited on its context without sending the
// probe or the probe was in flight at the candidate.
func TestColdStartProbeCutByTheOwnerHandsOnItsCandidate(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		name := "the prober waits on its context"
		if inFlight {
			name = "the probe waits at the candidate"
		}
		t.Run(name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			var hang atomic.Bool
			hang.Store(inFlight)
			arrived := make(chan struct{}, 1)
			cand, c := l.candidate(t, "pve2.example.com", statusRoutes(http.StatusOK, &hang, arrived))
			entered := make(chan struct{})
			var first atomic.Bool
			first.Store(true)
			prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
				if inFlight || !first.Swap(false) {
					return probeStatus(ctx, req)
				}
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}}
			p := newPool(t, l.coldConfig(prober, []Candidate{c}, configured(seed.Addr)))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				resp, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodGet, seed.Addr, "/cluster/resources", ""))
				closeBody(resp)
				done <- err
			}()
			if inFlight {
				<-arrived
			} else {
				<-entered
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("owner err = %v, want context.Canceled", err)
			}
			mustHandOnLine(t, l, "cancelled", 1)
			if !coldBegun(p) {
				t.Fatal("the cut run left the cold start unbegun")
			}
			resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustStatus(t, resp, err, http.StatusOK)
			if n := prober.calls.Load(); n != 2 {
				t.Fatalf("prober called %d times, want the cut probe and one more", n)
			}
			if n := cand.Counts.Method(http.MethodGet); n < 2 {
				t.Fatalf("candidate got %d GETs, want the probe and the read", n)
			}
		})
	}
}

// A run checks before each candidate that its request's context leaves the
// probe limit, so the owner's deadline never cuts a probe it starts. When it
// does not, the run stops with the candidates it did not probe passed on, and
// the next request probes them. The candidate refused before the stop is not
// probed again.
func TestColdStartRunStopsBeforeAProbeTheOwnerCannotFinish(t *testing.T) {
	l := newLab(t)
	seed := l.refused(t)
	_, a := l.candidate(t, "pve2.example.com", nil)
	_, b := l.candidate(t, "pve3.example.com", nil)
	var pve2, pve3 atomic.Int32
	prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
		if req.Candidate.Node == "pve2" {
			pve2.Add(1)
			time.Sleep(250 * time.Millisecond)
			return errors.New("pve2 is not a member")
		}
		pve3.Add(1)
		return probeStatus(ctx, req)
	}}
	cfg := l.coldConfig(prober, []Candidate{a, b}, configured(seed.Addr))
	cfg.probeLimit = 300 * time.Millisecond
	p := newPool(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer cancel()
	_, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodGet, seed.Addr, "/cluster/resources", ""))
	mustUnreachable(t, err, seed.Addr, a.Address)
	if n := pve3.Load(); n != 0 {
		t.Fatalf("pve3 probed %d times by a request with less than the probe limit left", n)
	}
	mustHandOnLine(t, l, "short", 1)
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
		"/cluster/resources", ""))
	mustStatus(t, resp, err, http.StatusOK)
	if n2, n3 := pve2.Load(), pve3.Load(); n2 != 1 || n3 != 1 {
		t.Fatalf("probes pve2 %d and pve3 %d, want one each", n2, n3)
	}
}

// A candidate that answered its probe, or whose probe failed for a reason of
// its own, is refused even when the owner's context ended before the prober
// returned, so the next request never probes it again.
func TestColdStartProbeTheOwnerOutlivedStillRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, l *lab, c Candidate, cfg *Config)
		probe func(ctx context.Context, req ProbeRequest, cancel context.CancelFunc) error
	}{
		{"the candidate answered 403", nil,
			func(ctx context.Context, req ProbeRequest, cancel context.CancelFunc) error {
				err := probeStatus(ctx, req)
				cancel()
				return err
			}},
		{"the candidate refused the dial", func(t *testing.T, l *lab, c Candidate, _ *Config) {
			gone := l.refused(t)
			l.d.set(c.Address, gone.DialContext)
		}, func(ctx context.Context, req ProbeRequest, cancel context.CancelFunc) error {
			err := probeStatus(ctx, req)
			cancel()
			return err
		}},
		{"the probe limit passed", func(_ *testing.T, _ *lab, _ Candidate, cfg *Config) {
			cfg.probeLimit = 100 * time.Millisecond
		}, func(ctx context.Context, _ ProbeRequest, cancel context.CancelFunc) error {
			<-ctx.Done()
			cancel()
			return ctx.Err()
		}},
		{"the prober panicked", nil, func(_ context.Context, _ ProbeRequest, cancel context.CancelFunc) error {
			cancel()
			panic("prober bug")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			_, c := l.candidate(t, "pve2.example.com", statusRoutes(http.StatusForbidden, nil, nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prober := &fakeProber{fn: func(pctx context.Context, req ProbeRequest) error {
				return tc.probe(pctx, req, cancel)
			}}
			cfg := l.coldConfig(prober, []Candidate{c}, configured(seed.Addr))
			if tc.setup != nil {
				tc.setup(t, l, c, &cfg)
			}
			p := newPool(t, cfg)
			if _, err := p.RoundTrip(tokenRequest(t, ctx, http.MethodGet, seed.Addr, "/cluster/resources",
				"")); err == nil {
				t.Fatal("the owner was answered")
			}
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustUnreachable(t, err, seed.Addr)
			if n := prober.calls.Load(); n != 1 {
				t.Fatalf("prober called %d times, want the refused candidate never probed again", n)
			}
		})
	}
}
