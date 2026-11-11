package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// refusedDial returns a dial that a refused server answers, for an address the
// test routes to it for a while and routes back to a live server afterward.
func (l *lab) refusedDial(t *testing.T) dialFunc {
	t.Helper()
	r := pvetest.NewRefused(t)
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return r.DialContext(ctx, network, r.Addr)
	}
}

// mustRefusedAsDropped fails the test unless err is the refusal of a request
// to a candidate that DropDiscovered took out: proven unsent, and naming the
// identity change.
func mustRefusedAsDropped(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "which discovery dropped when the cluster identity changed") {
		t.Fatalf("err = %v, want the refusal that names the identity change", err)
	}
	if IsMaybeSent(err) {
		t.Fatalf("err = %v, want it proven unsent", err)
	}
}

// A cold start put the lock on the candidate while the seed was down. The seed
// then came back and discovery found a new cluster identity, so the drop takes
// the candidate out with the discovered endpoints. The lock's release, which
// is tethered to the candidate, is refused before any dial and proven unsent,
// and it is neither sent to the candidate nor redirected to the seed.
func TestDropDiscoveredRefusesATetherToTheAcceptedCandidate(t *testing.T) {
	l := newLab(t)
	seed := l.live(t, nil)
	l.d.set(seed.Addr, l.refusedDial(t))
	cand, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr)))
	resp, err := p.RoundTrip(tokenRequest(t, context.Background(), http.MethodPost, seed.Addr, "/pools",
		"poolid=bosh-lock-x"))
	mustStatus(t, resp, err, http.StatusOK)
	if n := cand.Counts.Method(http.MethodPost); n != 1 {
		t.Fatalf("candidate got %d POSTs, want the lock", n)
	}

	l.d.set(seed.Addr, nil)
	p.CloseIdleConnections()
	if n := p.DropDiscovered(); n != 1 {
		t.Fatalf("DropDiscovered took out %d endpoints, want the accepted candidate", n)
	}

	conns := seed.Counts.Conns.Load()
	release := tokenRequest(t, context.Background(), http.MethodDelete, seed.Addr, "/pools", "")
	release.URL.RawQuery = "poolid=bosh-lock-x"
	resp, err = p.RoundTrip(release)
	closeBody(resp)
	mustRefusedAsDropped(t, err)
	if n := cand.Counts.Method(http.MethodDelete); n != 0 {
		t.Fatalf("candidate got %d DELETEs after the drop", n)
	}
	if n := seed.Counts.Method(http.MethodDelete); n != 0 {
		t.Fatalf("the seed got %d DELETEs, and a refused tether is never redirected", n)
	}
	if n := seed.Counts.Conns.Load(); n != conns {
		t.Fatalf("the refused release opened %d connections to the seed", n-conns)
	}
	p.mu.Lock()
	routed := len(p.routed)
	p.mu.Unlock()
	if routed != 0 {
		t.Fatalf("%d routed clones, want the refusal before any clone is chosen", routed)
	}
	if lines := l.entries(slog.LevelError, "pve endpoint "+c.Address); len(lines) != 0 {
		t.Fatalf("the drop logged %v, and it is no interception", lines)
	}
}

// After the drop a walk never reaches the candidate again, even when every
// configured endpoint is down, and a walk that finds the cold start over
// stops with its own failures.
func TestDropDiscoveredKeepsLaterWalksOffTheCandidate(t *testing.T) {
	l := newLab(t)
	seed := l.live(t, nil)
	l.d.set(seed.Addr, l.refusedDial(t))
	cand, c := l.candidate(t, "pve2.example.com", nil)
	p := newPool(t, l.coldConfig(&fakeProber{}, []Candidate{c}, configured(seed.Addr)))
	read := func() (*http.Response, error) {
		return p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, seed.Addr, "/cluster/resources", ""))
	}
	resp, err := read()
	mustStatus(t, resp, err, http.StatusOK)
	requests := cand.Counts.Requests.Load()

	p.DropDiscovered()
	_, err = read()
	mustUnreachable(t, err, seed.Addr)
	if n := cand.Counts.Requests.Load(); n != requests {
		t.Fatalf("candidate got %d requests after the drop", n-requests)
	}
	mustActive(t, p, seed.Addr)

	l.d.set(seed.Addr, nil)
	resp, err = read()
	mustStatus(t, resp, err, http.StatusOK)
	if n := cand.Counts.Requests.Load(); n != requests {
		t.Fatalf("candidate got %d requests after the seed came back", n-requests)
	}
}

// A drop while a walk probes a candidate accepts nothing, even when the probe
// then succeeds. The candidate gets no request beyond the probe, the
// candidates no walk had probed are never probed, and a walk waiting for the
// run ends with its own failures. The drop is no interception, so nothing is
// logged at Error.
func TestDropDiscoveredDuringAProbeAcceptsNothing(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	waiting := make(chan struct{}, 4)
	f := newTwoCandidates(t, func(ctx context.Context, req ProbeRequest) error {
		close(entered)
		<-release
		return probeStatus(ctx, req)
	}, func(cfg *Config) { cfg.onColdStartWait = func() { waiting <- struct{}{} } })
	owner, waiter := make(chan walkResult, 1), make(chan walkResult, 1)
	walk := func(out chan<- walkResult) {
		req := tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr, "/cluster/resources", "")
		go func() {
			resp, err := f.p.RoundTrip(req)
			closeBody(resp)
			out <- walkResult{err: err}
		}()
	}
	walk(owner)
	<-entered
	walk(waiter)
	<-waiting

	if n := f.p.DropDiscovered(); n != 2 {
		t.Fatalf("DropDiscovered took out %d candidates, want both", n)
	}
	close(release)

	mustUnreachable(t, (<-owner).err, f.seed.Addr)
	mustUnreachable(t, (<-waiter).err, f.seed.Addr)
	f.mustProbed(t, 1, 0)
	if n := f.c2.Counts.Requests.Load(); n != 1 {
		t.Fatalf("pve2 got %d requests, want only its probe", n)
	}
	if n := f.c3.Counts.Requests.Load(); n != 0 {
		t.Fatalf("pve3 got %d requests, and the drop left it unprobed", n)
	}
	if acc := f.p.cold.acceptedEndpoint(); acc != nil {
		t.Fatalf("the cold start accepted %s after the drop", acc.addr)
	}
	end := f.l.entries(slog.LevelDebug, "pve endpoints: the cold start from placement.az_map accepted no candidate")
	if len(end) != 1 || end[0].Attrs["reason"] != "identity_changed" || end[0].Attrs["probed"] != int64(1) ||
		end[0].Attrs["unprobed"] != int64(1) {
		t.Fatalf("end lines = %+v", end)
	}
	if lines := f.l.entries(slog.LevelError, "pve endpoint "); len(lines) != 0 {
		t.Fatalf("the drop logged %v at Error", lines)
	}

	_, err := f.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr,
		"/cluster/resources", ""))
	mustUnreachable(t, err, f.seed.Addr)
	f.mustProbed(t, 1, 0)
}

// A drop before any walk reaches the cold start leaves every candidate
// unprobed for the rest of the call.
func TestDropDiscoveredBeforeTheColdStartProbesNothing(t *testing.T) {
	f := newTwoCandidates(t, nil, nil)
	if n := f.p.DropDiscovered(); n != 2 {
		t.Fatalf("DropDiscovered took out %d candidates, want both", n)
	}
	if n := f.p.DropDiscovered(); n != 0 {
		t.Fatalf("a second DropDiscovered took out %d", n)
	}
	_, err := f.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, f.seed.Addr,
		"/cluster/resources", ""))
	mustUnreachable(t, err, f.seed.Addr)
	f.mustProbed(t, 0, 0)
	if n := f.c2.Counts.Requests.Load() + f.c3.Counts.Requests.Load(); n != 0 {
		t.Fatalf("the candidates got %d requests", n)
	}
}

// A candidate that failed TLS verification before the drop keeps its own
// refusal, and the drop does not take it out a second time.
func TestDropDiscoveredLeavesARevokedCandidateItsOwnRefusal(t *testing.T) {
	k := newCandidateLock(t)
	bad := k.l.otherLeaf(t)
	k.l.d.set(k.c.Address, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, bad.Addr)
	})
	k.p.CloseIdleConnections()
	_, err := k.p.RoundTrip(tokenRequest(t, context.Background(), http.MethodGet, k.seed.Addr,
		"/cluster/resources", ""))
	mustUnreachable(t, err, k.seed.Addr, k.c.Address)
	if n := k.p.DropDiscovered(); n != 0 {
		t.Fatalf("DropDiscovered took out %d, want none, because the candidate was already out", n)
	}
	_, err = k.p.RoundTrip(k.release(t))
	if err == nil || !strings.Contains(err.Error(), "which failed TLS verification after it proved membership") {
		t.Fatalf("err = %v, want the revoked tether refusal", err)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) && errors.Is(err, errDropped) {
		t.Fatalf("err = %v, want no mention of the drop", err)
	}
}
