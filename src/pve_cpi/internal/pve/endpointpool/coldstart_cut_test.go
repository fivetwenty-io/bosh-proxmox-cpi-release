package endpointpool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
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

// mustHandOnLine checks the one end line of a run that stopped for reason,
// probed candidates to an answer, and passed unprobed candidates to the next
// request.
func mustHandOnLine(t *testing.T, l *lab, reason string, probed, unprobed int64) {
	t.Helper()
	end := l.entries(slog.LevelDebug, "pve endpoints: the cold start from placement.az_map accepted no candidate")
	if len(end) != 1 || end[0].Attrs["reason"] != reason || end[0].Attrs["probed"] != probed ||
		end[0].Attrs["unprobed"] != unprobed ||
		!strings.HasSuffix(end[0].Message, "which the next request that reaches the cold start probes") {
		t.Fatalf("end lines = %+v, want one %s line with %d probed and %d unprobed", end, reason, probed, unprobed)
	}
}

// A probe that the owner's own cancellation cuts before its candidate answered
// refuses nothing. The candidate counts as never probed, so with one candidate
// the cold start is not over, and the next request probes it and is answered.
// That holds whether the prober waited on its context without sending the
// probe or the probe was in flight at the candidate, and whether the owner was
// cancelled plainly or with a cause, which the transport returns in place of a
// context error.
func TestColdStartProbeCutByTheOwnerHandsOnItsCandidate(t *testing.T) {
	cases := []struct {
		name     string
		inFlight bool
		cause    bool
	}{
		{"the prober waits on its context", false, false},
		{"the probe waits at the candidate", true, false},
		{"the probe waits at the candidate and the owner is cancelled with a cause", true, true},
	}
	for _, tc := range cases {
		inFlight := tc.inFlight
		t.Run(tc.name, func(t *testing.T) {
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
			ctx, cancelCause := context.WithCancelCause(context.Background())
			cancel := func() { cancelCause(nil) }
			if tc.cause {
				cancel = func() { cancelCause(errors.New("the director gave up on the call")) }
			}
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
			mustHandOnLine(t, l, "cancelled", 0, 1)
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
	mustHandOnLine(t, l, "short", 1, 1)
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
		{"the candidate's dial ran out its own cap", func(_ *testing.T, l *lab, c Candidate, cfg *Config) {
			cfg.candidateDialCap = 100 * time.Millisecond
			l.d.set(c.Address, func(ctx context.Context, _, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
			})
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

// A candidate that sent any byte of a response before the owner's
// cancellation cut its probe has answered, so it is refused and never probed
// again. That holds for an informational 1xx block, which the transport
// consumes without returning it, as much as for a final header block, and for
// the first bytes of a status line that never complete into either.
func TestColdStartProbeCutAfterAResponseByteRefuses(t *testing.T) {
	// send writes the candidate's partial answer and reports whether it took
	// the connection over, in which case the handler has nothing left to hold.
	cases := []struct {
		name string
		send func(t *testing.T, w http.ResponseWriter) (hijacked bool)
	}{
		{"a 103 Early Hints block", func(_ *testing.T, w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusEarlyHints)
			return false
		}},
		{"a final header block", func(_ *testing.T, w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			return false
		}},
		{"part of a status line", func(t *testing.T, w http.ResponseWriter) bool {
			conn, bw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return false
			}
			_, _ = bw.WriteString("HTTP/1.1 2")
			if err := bw.Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
			return true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			seed := l.refused(t)
			var hang atomic.Bool
			hang.Store(true)
			routes := pvetest.NewRoutes()
			routes.HandleFunc(http.MethodGet, "/cluster/status", func(w http.ResponseWriter, r *http.Request) {
				if hang.Swap(false) {
					if !tc.send(t, w) {
						<-r.Context().Done()
					}
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"data":[]}`)
			})
			_, c := l.candidate(t, "pve2.example.com", routes)
			gotByte := make(chan struct{})
			var once atomic.Bool
			prober := &fakeProber{fn: func(ctx context.Context, req ProbeRequest) error {
				return probeStatus(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
					GotFirstResponseByte: func() {
						if once.CompareAndSwap(false, true) {
							close(gotByte)
						}
					},
				}), req)
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
			<-gotByte
			cancel()
			if err := <-done; err == nil {
				t.Fatal("the owner was answered")
			}
			_, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr,
				"/cluster/resources", ""))
			mustUnreachable(t, err, seed.Addr)
			if n := prober.calls.Load(); n != 1 {
				t.Fatalf("prober called %d times, want the candidate that answered never probed again", n)
			}
		})
	}
}

// deadlineOnly is a context whose deadline is d and whose Err stays nil, as a
// context's does between its deadline passing and its timer firing.
type deadlineOnly struct {
	context.Context
	d time.Time
}

func (c deadlineOnly) Deadline() (time.Time, bool) { return c.d, true }

// The probe transport reads its own context as ended once its deadline has
// passed, even before the context's timer fired, so a failure that the probe's
// deadline caused is never read as the candidate's. A context that is alive
// and before its deadline has not ended.
func TestProbeTransportEndedCountsAPassedDeadline(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"no deadline and alive", context.Background(), false},
		{"a deadline ahead", deadlineOnly{context.Background(), time.Now().Add(time.Hour)}, false},
		{"a deadline passed before its timer fired", deadlineOnly{context.Background(), time.Now().Add(-time.Second)},
			true},
		{"cancelled", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&probeTransport{ctx: tc.ctx}).ended(); got != tc.want {
				t.Fatalf("ended() = %v, want %v", got, tc.want)
			}
		})
	}
}
