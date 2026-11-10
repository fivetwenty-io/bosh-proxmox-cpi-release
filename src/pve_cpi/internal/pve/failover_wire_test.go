package pve

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file run the real failover wrapper under the CPI's real
// retry layers. Every client comes from newClient with a resolved
// multi-endpoint config, and every endpoint is a pvetest server, so no test
// touches a live cluster.
//
// A call's scope release clears the scopeless tethers of every pool in the
// process (see endpointpool.WithAffinityScope). Each test here either releases
// a scope or sends a scopeless mutation, so none of them runs in parallel, and
// none of their subtests does either. Go runs a package's serial tests before
// it resumes the parallel ones, so these tests never overlap a parallel test
// that holds a tether.

// derivedCtxKey keys the value a derived context carries in a test.
type derivedCtxKey struct{}

// tetherPoolID is the pool every POST /pools in this file creates.
const tetherPoolID = "tether-pool"

// tetherPoolBody is what GET /pools/<id> answers.
var tetherPoolBody = map[string]any{"comment": "from the lab", "members": []any{}}

// tetherLab holds the certificate authority that signs the leaf every server in
// a test presents, and the client config that trusts it.
type tetherLab struct {
	ca   *pvetest.CA
	leaf *pvetest.Leaf
}

func newTetherLab(t *testing.T) *tetherLab {
	t.Helper()
	ca := pvetest.NewCA(t)
	return &tetherLab{ca: ca, leaf: ca.NewLeaf(t, pvetest.LeafOptions{})}
}

// live starts a Live server. A nil routes serves the defaults.
func (l *tetherLab) live(t *testing.T, routes *pvetest.Routes) *pvetest.Server {
	t.Helper()
	return pvetest.NewLive(t, l.leaf, routes)
}

// hijacker starts a server that reads every request in full and then drops the
// connection without an answer, so a mutation sent to it may have taken effect.
func (l *tetherLab) hijacker(t *testing.T) *pvetest.Server {
	t.Helper()
	return pvetest.NewBodyThenHijack(t, l.leaf)
}

// config builds a token-auth config that verifies against the lab's CA, whose
// host is the first server and whose endpoint list holds every server given.
func (l *tetherLab) config(servers ...*pvetest.Server) *config.CPIConfig {
	verify := true
	host, port := servers[0].HostPort()
	cfg := &config.CPIConfig{
		Host:         host,
		Port:         port,
		APIToken:     "root@pam!test=tok-tether",
		VerifySSL:    &verify,
		PVECACertPEM: string(l.ca.CertPEM()),
	}
	for _, s := range servers {
		cfg.Endpoints = append(cfg.Endpoints, s.Addr)
	}
	return cfg
}

// client builds a CPI client with its own pool over the servers, through
// newClient.
func (l *tetherLab) client(t *testing.T, servers ...*pvetest.Server) Client {
	t.Helper()
	return l.clientFor(t, l.config(servers...))
}

func (l *tetherLab) clientFor(t *testing.T, cfg *config.CPIConfig) Client {
	t.Helper()
	c, err := newClient(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

// sdkStandIn returns an http.Client whose transport is the pool that
// wireEndpoints installs for the servers, which is the pool newClient hands the
// SDK.
func (l *tetherLab) sdkStandIn(t *testing.T, servers ...*pvetest.Server) *http.Client {
	t.Helper()
	opts, wiring, err := wireEndpoints(l.config(servers...), log.NewNopLogger())
	if err != nil {
		t.Fatalf("wireEndpoints: %v", err)
	}
	base := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: l.ca.Pool(), MinVersion: tls.VersionTLS12},
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
	}
	if opts.WrapTransport == nil {
		t.Fatal("wireEndpoints installed no transport wrapper for a pool of two")
	}
	rt := opts.WrapTransport(base)
	if rt == nil {
		t.Fatalf("the pool did not build: %v", wiring.poolErr)
	}
	t.Cleanup(func() {
		if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		base.CloseIdleConnections()
	})
	return &http.Client{Transport: rt}
}

// hijackAndClose reads nothing more, takes the connection, and closes it with
// no answer, so the client sees a connection that dropped after the request.
func hijackAndClose(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// zeroBackoff makes a retry helper replay at once.
func zeroBackoff(int) time.Duration { return 0 }

// seenCount returns how many requests with the method and the API-relative
// path the server has read.
func seenCount(s *pvetest.Server, method, path string) int {
	n := 0
	for _, r := range s.Seen() {
		if r.Method == method && r.Path == pvetest.APIPrefix+path {
			n++
		}
	}
	return n
}

// mutationsSeen lists, as "METHOD path", every request the server read other
// than a read and the login.
func mutationsSeen(s *pvetest.Server) []string {
	var out []string
	for _, r := range s.Seen() {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		}
		if r.Path == pvetest.APIPrefix+"/access/ticket" {
			continue
		}
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// requireNoMutations fails the test when the server read any mutation.
func requireNoMutations(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := mutationsSeen(s); len(got) != 0 {
		t.Errorf("%s saw %d mutations, want none: %v", name, len(got), got)
	}
}

// requireNoRequests fails the test when the server read any request at all,
// which is how a test proves that a tethered request did not walk.
func requireNoRequests(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := len(s.Seen()); got != 0 {
		t.Errorf("%s saw %d requests, want none: %v", name, got, s.Seen())
	}
}

// requireWalkOnlyTo fails the test unless err is a connect-phase walk that
// tried exactly one endpoint, addr, and so did not walk on to any other.
func requireWalkOnlyTo(t *testing.T, err error, addr string) {
	t.Helper()
	var walk *endpointpool.EntryUnreachableError
	if !errors.As(err, &walk) || !endpointpool.IsConnectPhase(err) {
		t.Fatalf("error %v is not a connect-phase EntryUnreachableError", err)
	}
	if got := walk.Endpoints(); !slices.Equal(got, []string{addr}) {
		t.Fatalf("the walk tried %v, want only %s", got, addr)
	}
}

// requireMaybeSent fails the test unless err leaves open that the request reached
// the server. A failure the pool does not tag as a connect-phase failure, such as
// a connection dropped after the request went out, leaves it open, and so does a
// connect-phase failure the pool marks with MaybeSentError.
func requireMaybeSent(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || errors.Is(err, endpointpool.ErrTetherConflict) ||
		(endpointpool.IsConnectPhase(err) && !endpointpool.IsMaybeSent(err)) {
		t.Fatalf("%s: error = %v (%T), want a maybe-sent error", what, err, err)
	}
}

// moveActiveConcurrently reads from another goroutine, the way a concurrent
// read in the same call would. The caller arranges for the active endpoint's
// answer to make that read hop.
func moveActiveConcurrently(t *testing.T, c Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Pools().GetPoolComment(context.Background(), "p1")
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("the concurrent read: %v", err)
	}
}

// After a POST that may have reached A, every replay from the CPI's retry
// helpers goes to A alone, with A closed and B healthy. The replay fails
// without a walk, so B reads no request at all.
func TestTetherMaybeSentPostStaysOnItsServerThroughTheRetryHelpers(t *testing.T) {
	const attempts = 4
	layers := []struct {
		name  string
		retry func(ctx context.Context, op func() error) error
	}{
		{"RetryOnTransient", func(ctx context.Context, op func() error) error {
			return RetryOnTransient(ctx, nil, "tether.maybe_sent", attempts, op)
		}},
		{"RetryOnTransientOrLock", func(ctx context.Context, op func() error) error {
			return RetryOnTransientOrLock(ctx, nil, "tether.maybe_sent", attempts, op)
		}},
	}
	for _, layer := range layers {
		t.Run(layer.name, func(t *testing.T) {
			l := newTetherLab(t)
			a, b := l.hijacker(t), l.live(t, nil)
			c := l.client(t, a, b)
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()
			ctx = WithTestBackoff(ctx, zeroBackoff)

			calls := 0
			var first error
			err := layer.retry(ctx, func() error {
				calls++
				opErr := c.Pools().CreatePool(ctx, tetherPoolID, "held")
				if calls == 1 {
					first = opErr
					a.Close()
				}
				return opErr
			})

			requireMaybeSent(t, "the first POST", first)
			if !IsTransientTransport(first) {
				t.Fatalf("the first error is not transient, so the helper would not replay it: %v", first)
			}
			if calls != attempts {
				t.Errorf("the helper made %d attempts, want %d", calls, attempts)
			}
			requireWalkOnlyTo(t, err, a.Addr)
			if n := seenCount(a, http.MethodPost, "/pools"); n != 1 {
				t.Errorf("A read %d POST /pools, want 1", n)
			}
			requireNoRequests(t, "B", b)
		})
	}
}

// sdkTerminalTransportError mirrors the SDK's rule that a failed dial or DNS
// lookup ends its retry loop.
func sdkTerminalTransportError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// sdkResendLoop stands in for the SDK's retry middleware on a request that
// opted in to retrying a mutation. The SDK sets that option through
// WithForceRetry, which lives in a package internal to the SDK module, so no
// CPI code or test can set it. The loop copies what the middleware does. It
// sends the same request on the same context, rewinds the body before each
// resend, waits delay times the attempt number between sends, and stops at once
// on a failed dial or DNS lookup or when no retries remain. afterAttempt runs
// after each send with the 1-based attempt number.
func sdkResendLoop(
	hc *http.Client, req *http.Request, retries int, delay time.Duration, afterAttempt func(attempt int),
) (attempts int, err error) {
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-req.Context().Done():
				return attempts, req.Context().Err()
			case <-time.After(delay * time.Duration(attempt)):
			}
			if req.GetBody != nil {
				body, bodyErr := req.GetBody()
				if bodyErr != nil {
					return attempts, bodyErr
				}
				req.Body = body
			}
		}
		attempts++
		resp, doErr := hc.Do(req)
		if afterAttempt != nil {
			afterAttempt(attempts)
		}
		if doErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return attempts, nil
		}
		if sdkTerminalTransportError(doErr) || attempt >= retries {
			return attempts, doErr
		}
	}
}

// rawRequest builds a request for the API path at addr, with a form body for a
// POST, the way the SDK builds one.
func rawRequest(t *testing.T, ctx context.Context, method, addr, path string) *http.Request {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("poolid=" + tetherPoolID)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+pvetest.APIPrefix+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

// rawDo sends a request and drains its answer.
func rawDo(hc *http.Client, req *http.Request) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// An SDK-level resend of a mutation that may have reached A is another
// RoundTrip on the same context, so it meets A's tether. With A closed the
// resend fails at the dial and the loop stops, and with A open every resend
// reaches A. Either way B reads no request.
func TestTetherMaybeSentPostStaysOnItsServerThroughTheSDKRetryLayer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeAfter bool
	}{
		{"A closed after the first send", true},
		{"A open for every send", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newTetherLab(t)
			a, b := l.hijacker(t), l.live(t, nil)
			hc := l.sdkStandIn(t, a, b)
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()
			const retries = 3

			req := rawRequest(t, ctx, http.MethodPost, a.Addr, "/pools")
			attempts, err := sdkResendLoop(hc, req, retries, time.Millisecond, func(n int) {
				if n == 1 && tc.closeAfter {
					a.Close()
				}
			})

			if tc.closeAfter {
				if attempts != 2 {
					t.Errorf("attempts = %d, want 2 (the resend fails at the dial)", attempts)
				}
				requireWalkOnlyTo(t, err, a.Addr)
				if n := seenCount(a, http.MethodPost, "/pools"); n != 1 {
					t.Errorf("A read %d POST /pools, want 1", n)
				}
			} else {
				if attempts != retries+1 {
					t.Errorf("attempts = %d, want %d", attempts, retries+1)
				}
				requireMaybeSent(t, "the last resend", err)
				if n := seenCount(a, http.MethodPost, "/pools"); n != retries+1 {
					t.Errorf("A read %d POST /pools, want %d", n, retries+1)
				}
			}
			requireNoRequests(t, "B", b)
		})
	}
}

// A POST answered with a 5xx or the proxy codes fixes its tether without
// demoting A. The CPI's retry helper replays it, and the replay reaches A after
// a concurrent read moved the active endpoint to B. B reads no POST, the replay
// leaves the active endpoint where the read put it, and A is never demoted by a
// mutation.
func TestTetherStatusReplaysStayOnTheSameServer(t *testing.T) {
	for _, status := range []int{596, 500, 502, 503, 504, 595} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			l := newTetherLab(t)
			aRoutes := pvetest.NewRoutes()
			aRoutes.Fail(http.MethodPost, "/pools", status, "backend trouble")
			aRoutes.JSON(http.MethodGet, "/pools/", http.StatusOK, tetherPoolBody)
			bRoutes := pvetest.NewRoutes()
			bRoutes.JSON(http.MethodGet, "/pools/", http.StatusOK, tetherPoolBody)
			a, b := l.live(t, aRoutes), l.live(t, bRoutes)
			c := l.client(t, a, b)
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()
			ctx = WithTestBackoff(ctx, zeroBackoff)

			calls := 0
			var first error
			err := RetryOnTransient(ctx, nil, "tether.status", 2, func() error {
				calls++
				if calls == 2 {
					// The status did not demote A, so a read still goes to A.
					if _, _, readErr := c.Pools().GetPoolComment(context.Background(), "p1"); readErr != nil {
						t.Errorf("read after the status: %v", readErr)
					}
					if seenCount(a, http.MethodGet, "/pools/p1") != 1 || seenCount(b, http.MethodGet, "/pools/p1") != 0 {
						t.Errorf("the read after the status went to A=%d B=%d, want A=1 B=0",
							seenCount(a, http.MethodGet, "/pools/p1"), seenCount(b, http.MethodGet, "/pools/p1"))
					}
					// A concurrent read now meets a busy A, hops, and makes B active.
					aRoutes.Fail(http.MethodGet, "/pools/", http.StatusServiceUnavailable, "busy")
					moveActiveConcurrently(t, c)
				}
				opErr := c.Pools().CreatePool(ctx, tetherPoolID, "held")
				if calls == 1 {
					first = opErr
				}
				return opErr
			})

			if first == nil || !IsTransientTransport(first) {
				t.Fatalf("the first error = %v, want a transient one the helper replays", first)
			}
			if calls != 2 || err == nil {
				t.Fatalf("calls = %d err = %v, want two attempts that both met the status", calls, err)
			}
			if n := seenCount(a, http.MethodPost, "/pools"); n != 2 {
				t.Errorf("A read %d POST /pools, want 2", n)
			}
			requireNoMutations(t, "B", b)
			// The replay left B active: the next read goes to B and not back to A.
			aReads := seenCount(a, http.MethodGet, "/pools/p1")
			if _, _, readErr := c.Pools().GetPoolComment(context.Background(), "p1"); readErr != nil {
				t.Fatalf("read after the replay: %v", readErr)
			}
			if got := seenCount(a, http.MethodGet, "/pools/p1"); got != aReads {
				t.Errorf("A read %d GETs after the replay, want %d: the replay changed the active endpoint", got, aReads)
			}
			if got := seenCount(b, http.MethodGet, "/pools/p1"); got != 2 {
				t.Errorf("B read %d GETs, want the concurrent read and the one after the replay", got)
			}
		})
	}
}

// A mutation sent on context.Background(), the way a rollback or parker path
// sends one, meets the tether its call's scope fixed.
func TestTetherBackgroundContextMeetsItsCallsTether(t *testing.T) {
	l := newTetherLab(t)
	a, b := l.hijacker(t), l.live(t, nil)
	c := l.client(t, a, b)
	ctx, release := endpointpool.WithAffinityScope(t.Context())
	defer release()

	requireMaybeSent(t, "the call's POST", c.Pools().CreatePool(ctx, tetherPoolID, "held"))
	a.Close()

	for name, replay := range map[string]context.Context{
		"a background context":       context.Background(),
		"the call's context":         ctx,
		"a context derived from it":  context.WithValue(ctx, derivedCtxKey{}, "derived"),
		"a background context again": context.Background(),
	} {
		requireWalkOnlyTo(t, c.Pools().CreatePool(replay, tetherPoolID, "replay "+name), a.Addr)
	}
	if n := seenCount(a, http.MethodPost, "/pools"); n != 1 {
		t.Errorf("A read %d POST /pools, want 1", n)
	}
	requireNoRequests(t, "B", b)
}

// Two pools built for two clients never share tethers, whether the second
// pool's request carries the first pool's scope or none at all.
func TestTetherTwoPoolsNeverShareTethers(t *testing.T) {
	l := newTetherLab(t)
	a, b := l.hijacker(t), l.live(t, nil)
	first := l.client(t, a, b)
	second := l.client(t, b, a)
	ctx, release := endpointpool.WithAffinityScope(t.Context())
	defer release()

	requireMaybeSent(t, "the first pool's POST", first.Pools().CreatePool(ctx, tetherPoolID, "held"))

	// Were the tether shared, the second pool's POST would follow it to A.
	if err := second.Pools().CreatePool(context.Background(), tetherPoolID, "scopeless"); err != nil {
		t.Fatalf("the second pool's scopeless POST: %v", err)
	}
	if err := second.Pools().CreatePool(ctx, tetherPoolID, "same scope"); err != nil {
		t.Fatalf("the second pool's POST on the first pool's scope: %v", err)
	}
	if n := seenCount(a, http.MethodPost, "/pools"); n != 1 {
		t.Errorf("A read %d POST /pools, want only the first pool's 1", n)
	}
	if n := seenCount(b, http.MethodPost, "/pools"); n != 2 {
		t.Errorf("B read %d POST /pools, want the second pool's 2", n)
	}

	// The first pool is still tethered to A.
	a.Close()
	requireWalkOnlyTo(t, first.Pools().CreatePool(ctx, tetherPoolID, "replay"), a.Addr)
	if n := seenCount(b, http.MethodPost, "/pools"); n != 2 {
		t.Errorf("B read %d POST /pools after the first pool's replay, want 2", n)
	}
}

// A GET on the path a tethered POST used still fails over, because the tether
// binds mutations and nothing else.
func TestTetherGetOnTheSamePathStillFailsOver(t *testing.T) {
	l := newTetherLab(t)
	a, b := l.hijacker(t), l.live(t, nil)
	c := l.client(t, a, b)
	ctx, release := endpointpool.WithAffinityScope(t.Context())
	defer release()

	requireMaybeSent(t, "the POST", c.Pools().CreatePool(ctx, tetherPoolID, "held"))
	a.Close()

	lister, ok := c.Pools().(PoolCommentLister)
	if !ok {
		t.Fatal("the client's pool service does not list pools")
	}
	if _, err := lister.ListPoolComments(ctx); err != nil {
		t.Fatalf("GET /pools with A closed: %v", err)
	}
	if n := seenCount(b, http.MethodGet, "/pools"); n != 1 {
		t.Errorf("B read %d GET /pools, want the failed-over read", n)
	}
	// The same path as a POST is still refused its hop.
	requireWalkOnlyTo(t, c.Pools().CreatePool(ctx, tetherPoolID, "replay"), a.Addr)
	requireNoMutations(t, "B", b)
}

// familyStep is one mutation in a tether family test.
type familyStep struct {
	name string
	send func(ctx context.Context, c Client) error
}

// A mutation that may have reached A tethers its whole family to A. Every
// other mutation of the family, whatever its method or path, goes to A alone
// and fails at the dial once A is closed, and B reads none of them. A mutation
// outside the family is not tethered and reaches B.
func TestTetherFamiliesKeepRePlannedWritesOnOneServer(t *testing.T) {
	haRes := "vm:100,vm:101"
	comment := "tether"
	force := true
	cases := []struct {
		name    string
		first   familyStep
		follows []familyStep
		control familyStep
	}{
		{
			name: "ha",
			first: familyStep{"PUT /cluster/ha/rules/r1", func(ctx context.Context, c Client) error {
				return c.Cluster().UpdateHaRules(ctx, "r1",
					&sdkcluster.UpdateHaRulesParams{Type: "resource-affinity", Resources: &haRes})
			}},
			follows: []familyStep{
				{"POST /cluster/ha/rules", func(ctx context.Context, c Client) error {
					return c.Cluster().CreateHaRules(ctx,
						&sdkcluster.CreateHaRulesParams{Rule: "r2", Type: "resource-affinity", Resources: haRes})
				}},
				{"DELETE /cluster/ha/rules/r3", func(ctx context.Context, c Client) error {
					return c.Cluster().DeleteHaRules(ctx, "r3")
				}},
				{"PUT /cluster/ha/resources/vm:100", func(ctx context.Context, c Client) error {
					return c.Cluster().UpdateHaResources(ctx, "vm:100", &sdkcluster.UpdateHaResourcesParams{Comment: &comment})
				}},
				{"DELETE /cluster/ha/resources/vm:101", func(ctx context.Context, c Client) error {
					return c.Cluster().DeleteHaResources(ctx, "vm:101", nil)
				}},
			},
			control: familyStep{"POST /pools", func(ctx context.Context, c Client) error {
				return c.Pools().CreatePool(ctx, tetherPoolID, "control")
			}},
		},
		{
			name: "replication",
			first: familyStep{"PUT /cluster/replication/100-0", func(ctx context.Context, c Client) error {
				return c.Cluster().UpdateReplication(ctx, "100-0", &sdkcluster.UpdateReplicationParams{Comment: &comment})
			}},
			follows: []familyStep{
				{"DELETE /cluster/replication/100-0", func(ctx context.Context, c Client) error {
					return c.Cluster().DeleteReplication(ctx, "100-0", nil)
				}},
				{"DELETE /cluster/replication/100-0 with force", func(ctx context.Context, c Client) error {
					return c.Cluster().DeleteReplication(ctx, "100-0", &sdkcluster.DeleteReplicationParams{Force: &force})
				}},
				{"POST /cluster/replication", func(ctx context.Context, c Client) error {
					return c.Cluster().CreateReplication(ctx,
						&sdkcluster.CreateReplicationParams{Id: "101-0", Target: "pve2", Type: "local"})
				}},
			},
			control: familyStep{"POST /pools", func(ctx context.Context, c Client) error {
				return c.Pools().CreatePool(ctx, tetherPoolID, "control")
			}},
		},
		{
			name: "pools",
			first: familyStep{"POST /pools", func(ctx context.Context, c Client) error {
				return c.Pools().CreatePool(ctx, tetherPoolID, "sentinel")
			}},
			follows: []familyStep{
				{"DELETE /pools", func(ctx context.Context, c Client) error {
					return c.Pools().DeletePool(ctx, tetherPoolID)
				}},
				{"PUT /pools/other", func(ctx context.Context, c Client) error {
					return c.Pools().AddVM(ctx, "other", 100)
				}},
			},
			control: familyStep{"PUT /cluster/replication/100-0", func(ctx context.Context, c Client) error {
				return c.Cluster().UpdateReplication(ctx, "100-0", &sdkcluster.UpdateReplicationParams{Comment: &comment})
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newTetherLab(t)
			a, b := l.hijacker(t), l.live(t, nil)
			c := l.client(t, a, b)
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()

			requireMaybeSent(t, tc.first.name, tc.first.send(ctx, c))
			a.Close()

			for _, step := range tc.follows {
				requireWalkOnlyTo(t, step.send(ctx, c), a.Addr)
			}
			if got := mutationsSeen(a); len(got) != 1 {
				t.Errorf("A read %d mutations, want only the first: %v", len(got), got)
			}
			requireNoMutations(t, "B", b)

			// A mutation of another family is free to go to B.
			if err := tc.control.send(ctx, c); err != nil {
				t.Fatalf("%s outside the family: %v", tc.control.name, err)
			}
			if got := mutationsSeen(b); len(got) != 1 {
				t.Errorf("B read %d mutations, want only the control: %v", len(got), got)
			}
		})
	}
}

// buildTetherConflict leaves a pool with two scopeless-visible tethers on the
// same key that name different servers, the one state the pool refuses with
// ErrTetherConflict. Two calls each hold a scope, and a read between their
// POSTs moves the active endpoint from A to B, so call one's POST may have
// reached A and call two's may have reached B. The returned function releases
// both scopes.
func buildTetherConflict(
	t *testing.T, a, b *pvetest.Server, post func(ctx context.Context) error, move func() error,
) (release func()) {
	t.Helper()
	ctx1, release1 := endpointpool.WithAffinityScope(t.Context())
	ctx2, release2 := endpointpool.WithAffinityScope(t.Context())
	requireMaybeSent(t, "call one's POST", post(ctx1))
	if err := move(); err != nil {
		t.Fatalf("the read that moves the active endpoint: %v", err)
	}
	requireMaybeSent(t, "call two's POST", post(ctx2))
	if seenCount(a, http.MethodPost, "/pools") != 1 || seenCount(b, http.MethodPost, "/pools") != 1 {
		t.Fatalf("setup: POSTs on A=%d B=%d, want one each",
			seenCount(a, http.MethodPost, "/pools"), seenCount(b, http.MethodPost, "/pools"))
	}
	return func() {
		release1()
		release2()
	}
}

// conflictRoutes serves a pool whose POST /pools may have taken effect and
// drops the connection, and whose read answers busy when busy is true.
func conflictRoutes(busy bool) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.Handle(http.MethodPost, "/pools", http.HandlerFunc(hijackAndClose))
	if busy {
		routes.Fail(http.MethodGet, "/pools/", http.StatusServiceUnavailable, "busy")
	} else {
		routes.JSON(http.MethodGet, "/pools/", http.StatusOK, tetherPoolBody)
	}
	return routes
}

// A mutation refused with ErrTetherConflict is not transient, so the CPI's
// retry helpers stop at the first attempt, and nothing reaches either server.
func TestTetherConflictIsNotReplayedByTheRetryHelpers(t *testing.T) {
	l := newTetherLab(t)
	a, b := l.live(t, conflictRoutes(true)), l.live(t, conflictRoutes(false))
	c := l.client(t, a, b)
	release := buildTetherConflict(t, a, b,
		func(ctx context.Context) error { return c.Pools().CreatePool(ctx, tetherPoolID, "held") },
		func() error { _, _, err := c.Pools().GetPoolComment(context.Background(), "p1"); return err })
	defer release()

	ctx := WithTestBackoff(context.Background(), zeroBackoff)
	for name, retry := range map[string]func(op func() error) error{
		"RetryOnTransient": func(op func() error) error {
			return RetryOnTransient(ctx, nil, "tether.conflict", 4, op)
		},
		"RetryOnTransientOrLock": func(op func() error) error {
			return RetryOnTransientOrLock(ctx, nil, "tether.conflict", 4, op)
		},
	} {
		calls := 0
		err := retry(func() error {
			calls++
			return c.Pools().CreatePool(context.Background(), tetherPoolID, "refused")
		})
		if !errors.Is(err, endpointpool.ErrTetherConflict) {
			t.Fatalf("%s: error = %v, want ErrTetherConflict", name, err)
		}
		if IsTransientTransport(err) {
			t.Errorf("%s: the refusal reads as transient", name)
		}
		if calls != 1 {
			t.Errorf("%s: made %d attempts, want 1", name, calls)
		}
	}
	for name, s := range map[string]*pvetest.Server{"A": a, "B": b} {
		if got := seenCount(s, http.MethodPost, "/pools"); got != 1 {
			t.Errorf("%s read %d POST /pools, want only the setup's 1", name, got)
		}
	}
}

// The SDK's forced retry resends a refused mutation through its whole backoff,
// because the refusal is neither a dial nor a DNS failure. Every resend is
// refused before dispatch, so nothing reaches either server.
func TestTetherConflictSentNowhereThroughTheSDKRetryLayer(t *testing.T) {
	l := newTetherLab(t)
	a, b := l.live(t, conflictRoutes(true)), l.live(t, conflictRoutes(false))
	hc := l.sdkStandIn(t, a, b)
	release := buildTetherConflict(t, a, b,
		func(ctx context.Context) error {
			return rawDo(hc, rawRequest(t, ctx, http.MethodPost, a.Addr, "/pools"))
		},
		func() error {
			return rawDo(hc, rawRequest(t, context.Background(), http.MethodGet, a.Addr, "/pools/p1"))
		})
	defer release()

	const retries = 3
	req := rawRequest(t, context.Background(), http.MethodPost, a.Addr, "/pools")
	attempts, err := sdkResendLoop(hc, req, retries, time.Millisecond, nil)

	if !errors.Is(err, endpointpool.ErrTetherConflict) {
		t.Fatalf("error = %v, want ErrTetherConflict", err)
	}
	if attempts != retries+1 {
		t.Errorf("the loop made %d attempts, want %d: a refusal is retried through the backoff", attempts, retries+1)
	}
	for name, s := range map[string]*pvetest.Server{"A": a, "B": b} {
		if got := seenCount(s, http.MethodPost, "/pools"); got != 1 {
			t.Errorf("%s read %d POST /pools, want only the setup's 1", name, got)
		}
	}
}

// The SDK re-logs in after a 401 and resends the request itself. The resend of
// a POST that A answered goes to A again, although the re-login met a busy A,
// hopped to B, and made B active. B reads no POST /pools.
func TestTetherSDKResendAfterReloginStaysOnTheFirstServer(t *testing.T) {
	l := newTetherLab(t)
	defaults := pvetest.NewRoutes()
	var tickets, posts atomic.Int32
	aRoutes := pvetest.NewRoutes()
	aRoutes.HandleFunc(http.MethodPost, "/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		if tickets.Add(1) == 1 {
			defaults.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"data":null,"message":"busy"}`))
	})
	aRoutes.HandleFunc(http.MethodPost, "/pools", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"data":null,"message":"no ticket"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	a, b := l.live(t, aRoutes), l.live(t, nil)
	cfg := l.config(a, b)
	cfg.APIToken = ""
	cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
	c := l.clientFor(t, cfg)
	ctx, release := endpointpool.WithAffinityScope(t.Context())
	defer release()

	if err := c.Pools().CreatePool(ctx, tetherPoolID, "after a re-login"); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	if got := seenCount(a, http.MethodPost, "/pools"); got != 2 {
		t.Errorf("A read %d POST /pools, want the POST and its resend", got)
	}
	if got := seenCount(b, http.MethodPost, "/pools"); got != 0 {
		t.Errorf("B read %d POST /pools, want 0", got)
	}
	if got := seenCount(a, http.MethodPost, "/access/ticket"); got != 2 {
		t.Errorf("A read %d logins, want the first and the busy re-login", got)
	}
	if got := seenCount(b, http.MethodPost, "/access/ticket"); got != 1 {
		t.Errorf("B read %d logins, want the re-login that hopped", got)
	}
}
