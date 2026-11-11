package endpointpool

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// candidateLock is a pool of one refused seed and one candidate, after a
// cluster lock, a POST /pools, went to the candidate through the cold start,
// which tethered the lock's family to the candidate.
type candidateLock struct {
	l    *lab
	p    *Pool
	seed *pvetest.Refused
	cand *pvetest.Server
	c    Candidate
}

func newCandidateLock(t *testing.T) *candidateLock {
	t.Helper()
	k := &candidateLock{l: newLab(t)}
	k.seed = k.l.refused(t)
	k.cand, k.c = k.l.candidate(t, "pve2.example.com", nil)
	k.p = newPool(t, k.l.coldConfig(&fakeProber{}, []Candidate{k.c}, configured(k.seed.Addr)))
	resp, err := k.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, k.seed.Addr, "/pools",
		"poolid=bosh-lock-x"))
	mustStatus(t, resp, err, http.StatusOK)
	return k
}

// release is the lock's release, a DELETE /pools in the lock's family.
func (k *candidateLock) release(t *testing.T) *http.Request {
	req := tokenRequest(t, context.Background(), http.MethodDelete, k.seed.Addr, "/pools", "")
	req.URL.RawQuery = "poolid=bosh-lock-x"
	return req
}

// A lock taken on the accepted candidate is released there: the release
// follows its family's tether to the candidate, through the candidate's own
// clones, and never dials the seed.
func TestColdStartLockReleaseFollowsItsTetherToTheCandidate(t *testing.T) {
	k := newCandidateLock(t)
	dials := k.seed.Counts.Dials.Load()
	resp, err := k.p.RoundTrip(k.release(t))
	mustStatus(t, resp, err, http.StatusOK)
	if n := k.cand.Counts.Method(http.MethodDelete); n != 1 {
		t.Fatalf("candidate got %d DELETEs, want the release", n)
	}
	if n := k.seed.Counts.Dials.Load(); n != dials {
		t.Fatalf("the release dialed the seed %d times", n-dials)
	}
	k.p.mu.Lock()
	routed := len(k.p.routed)
	k.p.mu.Unlock()
	if routed != 0 {
		t.Fatalf("%d routed clones, want the release on the candidate's own", routed)
	}
}

// Once the accepted candidate failed TLS verification, a mutation tethered to
// it is refused before any dial and proven unsent, and it goes nowhere else.
func TestColdStartTetherOnARevokedCandidateIsRefused(t *testing.T) {
	k := newCandidateLock(t)
	bad := k.l.otherLeaf(t)
	k.l.d.set(k.c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, bad.Addr)
	})
	k.p.CloseIdleConnections()
	_, err := k.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, k.seed.Addr,
		"/cluster/resources", ""))
	mustUnreachable(t, err, k.seed.Addr, k.c.Address)
	prefix := "pve endpoint " + k.c.Address + ": the cold-start candidate"
	if lines := k.l.entries(slog.LevelError, prefix); len(lines) != 1 {
		t.Fatalf("revocation lines = %v", lines)
	}
	dials, badConns := k.seed.Counts.Dials.Load(), bad.Counts.Conns.Load()
	_, err = k.p.RoundTrip(k.release(t))
	if err == nil || !strings.Contains(err.Error(), "which failed TLS verification after it proved membership") {
		t.Fatalf("err = %v, want the revoked tether refusal", err)
	}
	if IsMaybeSent(err) {
		t.Fatalf("err = %v, want it proven unsent", err)
	}
	if n := k.seed.Counts.Dials.Load(); n != dials {
		t.Fatalf("the refused release dialed the seed %d times", n-dials)
	}
	if n := bad.Counts.Conns.Load(); n != badConns {
		t.Fatalf("the refused release dialed the candidate's address %d times", n-badConns)
	}
	if n := k.cand.Counts.Method(http.MethodDelete); n != 0 {
		t.Fatalf("candidate got %d DELETEs", n)
	}
}

// A tether on the accepted candidate takes a mutation there only when the cold
// start itself could send it there, so a discovery mutation, one with a
// ticket, and one without an API token are refused before any dial.
func TestColdStartTetherOnTheCandidateNeedsWhatTheColdStartNeeds(t *testing.T) {
	cases := []struct {
		name string
		req  func(t *testing.T, k *candidateLock) *http.Request
		why  string
	}{
		{"a discovery mutation", func(t *testing.T, k *candidateLock) *http.Request {
			req := k.release(t)
			return req.WithContext(WithConfiguredOnly(req.Context()))
		}, "and a login or discovery request never goes to a cold-start candidate"},
		{"a ticket", func(t *testing.T, k *candidateLock) *http.Request {
			req := k.release(t)
			req.Header.Set("Cookie", "PVEAuthCookie=ticket")
			return req
		}, "and only a request with one API token and no ticket goes to a cold-start candidate"},
		{"no API token", func(t *testing.T, k *candidateLock) *http.Request {
			req := k.release(t)
			req.Header.Del("Authorization")
			return req
		}, "and only a request with one API token and no ticket goes to a cold-start candidate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := newCandidateLock(t)
			dials := k.seed.Counts.Dials.Load()
			_, err := k.p.RoundTrip(tc.req(t, k))
			if err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("err = %v, want the refusal %q", err, tc.why)
			}
			if IsMaybeSent(err) {
				t.Fatalf("err = %v, want it proven unsent", err)
			}
			if n := k.cand.Counts.Method(http.MethodDelete); n != 0 {
				t.Fatalf("candidate got %d DELETEs", n)
			}
			if n := k.seed.Counts.Dials.Load(); n != dials {
				t.Fatalf("the refused mutation dialed the seed %d times", n-dials)
			}
		})
	}
}

// An accepted candidate whose dial fails is only demoted, not taken out, so
// a later walk that meets the cold-start conditions still reaches it.
func TestColdStartCandidateDialFailureOnlyDemotes(t *testing.T) {
	a := newAcceptedLab(t, nil)
	gone := a.l.refused(t)
	a.l.d.set(a.c.Address, gone.DialContext)
	a.p.CloseIdleConnections()
	if _, err := a.p.RoundTrip(a.read(t, context.Background())); err == nil {
		t.Fatal("answered with the candidate down")
	}
	prefix := "pve endpoint " + a.c.Address + ": the cold-start candidate"
	if lines := a.l.entries(slog.LevelError, prefix); len(lines) != 0 {
		t.Fatalf("a dial failure revoked the candidate: %v", lines)
	}
	a.l.d.set(a.c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, a.cand.Addr)
	})
	resp, err := a.p.RoundTrip(a.read(t, context.Background()))
	mustStatus(t, resp, err, http.StatusOK)
}

// A walking mutation that reclaims a hop onto a tether naming the accepted
// candidate goes there only when the cold start itself could send it there.
// A mutation with a ticket and a discovery mutation are refused before any
// dial and proven unsent, so the candidate never gets the ticket cookie, the
// CSRF token, or a request the operator meant only for configured endpoints.
func TestColdStartReclaimRefusesWhatTheColdStartWouldNotSend(t *testing.T) {
	cases := []struct {
		name string
		req  func(t *testing.T, a *acceptedLab) *http.Request
		why  string
	}{
		{"a ticket", func(t *testing.T, a *acceptedLab) *http.Request {
			req := apiRequest(t, context.Background(), http.MethodPost, a.seeds[0].Addr, fanOutRoute, "vmid=101")
			req.Header.Set("Cookie", "PVEAuthCookie=ticket")
			req.Header.Set("CSRFPreventionToken", "csrf")
			return req
		}, "and only a request with one API token and no ticket goes to a cold-start candidate"},
		{"a discovery mutation", func(t *testing.T, a *acceptedLab) *http.Request {
			return tokenRequest(t, WithConfiguredOnly(context.Background()), http.MethodPost, a.seeds[0].Addr,
				fanOutRoute, "vmid=101")
		}, "and a login or discovery request never goes to a cold-start candidate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAcceptedLab(t, nil)
			cand := a.p.memberAt(a.c.Address)
			req := tc.req(t, a)
			var once sync.Once
			refuse := func(s *pvetest.Refused) dialFunc {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					once.Do(func() { injectTether(a.p, tetherKeyOf(req), targetOf(cand), false) })
					return s.DialContext(ctx, network, addr)
				}
			}
			a.l.d.set(a.seeds[1].Addr, refuse(a.seeds[1]))
			a.l.d.set(a.seeds[0].Addr, refuse(a.seeds[0]))
			resp, err := a.p.RoundTrip(req)
			closeBody(resp)
			if err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("err = %v, want the refusal %q", err, tc.why)
			}
			if IsMaybeSent(err) {
				t.Fatalf("err = %v, want it proven unsent", err)
			}
			if n := a.cand.Counts.Method(http.MethodPost); n != 0 {
				t.Fatalf("candidate got %d POSTs, want none", n)
			}
		})
	}
}
