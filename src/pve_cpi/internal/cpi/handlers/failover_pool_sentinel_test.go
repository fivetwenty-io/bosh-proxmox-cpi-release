package handlers

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The wire tests in the failover_*_test.go files build a real pve.Client from a
// resolved multi-endpoint config, so every request runs through the endpoint
// pool's RoundTripper against TLS servers from internal/pve/pvetest. None of
// them touches a live cluster.
//
// Releasing a call's affinity scope clears the scopeless tethers of every pool
// in the process (see endpointpool.WithAffinityScope), so these tests are all
// serial. A parallel test that sent a scopeless mutation through a pooled
// client would lose its tether to the scope release of one of these.

// fwLab holds the certificate authority and the leaf the live servers of one
// test present.
type fwLab struct {
	leaf *pvetest.Leaf
}

func newFWLab(t *testing.T) *fwLab {
	t.Helper()
	return &fwLab{leaf: pvetest.NewCA(t).NewLeaf(t, pvetest.LeafOptions{})}
}

// live starts a TLS server that answers routes, or the default routes when
// routes is nil.
func (l *fwLab) live(t *testing.T, routes *pvetest.Routes) *pvetest.Server {
	t.Helper()
	return pvetest.NewLive(t, l.leaf, routes)
}

// fwClosedAddr returns the address of a loopback port that refuses connections.
func fwClosedAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// fwClient builds a client through the production constructor whose first
// address is pve.host and whose list holds every address given, in order.
// The client verifies nothing, which the servers' leaf would satisfy anyway,
// so a routed host outside the list needs no extra trust.
func fwClient(t *testing.T, addrs ...string) pve.Client {
	t.Helper()
	host, portText, err := net.SplitHostPort(addrs[0])
	if err != nil {
		t.Fatalf("split %q: %v", addrs[0], err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port of %q: %v", addrs[0], err)
	}
	verify := false
	c, err := pve.NewClientWithTracer(&config.CPIConfig{
		Host:      host,
		Port:      port,
		Endpoints: addrs,
		APIToken:  "root@pam!test=tok-failover",
		VerifySSL: &verify,
	}, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	return c
}

// fwCtx returns a call context the way the dispatcher builds one, with its own
// affinity scope released when the test ends, plus a zero backoff for the
// retry helpers and a one-millisecond base for the SDK's own retries.
func fwCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	t.Cleanup(release)
	ctx = pve.WithTestBackoff(ctx, func(int) time.Duration { return 0 })
	return sdkclient.WithRetryDelay(ctx, time.Millisecond)
}

// fwFastPolling shortens the task poll for the test.
func fwFastPolling(t *testing.T) {
	t.Helper()
	t.Cleanup(pve.SetTaskPollingForTest(1, 1, 0))
}

// fwMutations lists every mutation the server read as "METHOD path", in the
// order they arrived and without the API prefix. A mutation is any method
// other than GET, HEAD, and OPTIONS.
func fwMutations(s *pvetest.Server) []string {
	seen := s.Seen()
	out := make([]string, 0, len(seen))
	for _, req := range seen {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		}
		out = append(out, req.Method+" "+strings.TrimPrefix(req.Path, pvetest.APIPrefix))
	}
	return out
}

// fwCount counts the mutations in got that match want exactly.
func fwCount(got []string, want string) int {
	n := 0
	for _, g := range got {
		if g == want {
			n++
		}
	}
	return n
}

// fwNoMutations fails the test when the server read any mutation, which is what
// "the request stayed on the other server" means.
func fwNoMutations(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := fwMutations(s); len(got) != 0 {
		t.Errorf("%s read mutations %v, want none", name, got)
	}
}

// fwGets counts the GET requests the server read.
func fwGets(s *pvetest.Server) int { return int(s.Counts.Method(http.MethodGet)) }

// fwDropAfterBody reads the whole request and then resets the connection with
// no response, so the client's read fails with a connection reset after every
// byte it sent. A mutation answered this way may have taken effect.
func fwDropAfterBody(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, isTLS := conn.(*tls.Conn); isTLS {
		conn = tc.NetConn()
	}
	if tcp, isTCP := conn.(*net.TCPConn); isTCP {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// fwFirstThen serves first for the first request and then for every later one.
func fwFirstThen(first, then http.HandlerFunc) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first(w, r)
			return
		}
		then(w, r)
	}
}

const (
	fwSentinel = "bosh-lock-disk-transfer"
	fwClaim    = "claim-owner-1"
)

// fwPoolRoutes answers the sentinel's create and delete with status, and its
// claim read with fwClaim.
func fwPoolRoutes(status int) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	if status == http.StatusOK {
		routes.JSON(http.MethodPost, "/pools", status, nil)
	} else {
		routes.Fail(http.MethodPost, "/pools", status, "backend trouble")
	}
	routes.JSON(http.MethodDelete, "/pools", http.StatusOK, nil)
	routes.JSON(http.MethodGet, "/pools/", http.StatusOK, map[string]any{"comment": fwClaim, "members": []any{}})
	return routes
}

// A sentinel create under /pools that may have landed on one server is followed
// by the cleanup delete of releasePoisonedSentinel, which differs in method and
// shares only the pools family with the create. With the first server gone and
// the second one healthy, the delete must not reach the second server, whatever
// the create was answered with.
func TestPoolSentinelCreateThenReleaseNeverReachesTheOtherServer(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError, 596} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			lab := newFWLab(t)
			a := lab.live(t, fwPoolRoutes(status))
			b := lab.live(t, fwPoolRoutes(http.StatusOK))
			client := fwClient(t, a.Addr, b.Addr)
			ctx := fwCtx(t)
			svc := &managedPoolService{PoolService: client.Pools()}

			_ = client.Pools().CreatePool(ctx, fwSentinel, fwClaim)
			if n := fwCount(fwMutations(a), "POST /pools"); n == 0 {
				t.Fatalf("the create never reached the first server: %v", fwMutations(a))
			}
			a.Close()

			err := svc.releasePoisonedSentinel(pve.WithExpectedLockClaim(ctx, fwClaim), fwSentinel)
			if err == nil {
				t.Fatal("the release succeeded with its server gone, so it was sent somewhere else")
			}
			fwNoMutations(t, "the second server", b)
			if got := fwCount(fwMutations(a), "DELETE /pools"); got != 0 {
				t.Errorf("the closed server read %d deletes", got)
			}
			if fwGets(b) == 0 {
				t.Error("the claim read never failed over to the second server, so the test did not exercise a read hop")
			}
		})
	}
}

// The delete of another sentinel is in the same family, so it stays on the
// server that got the create of the first one.
func TestPoolSentinelReleaseOfAnotherSentinelStaysOnTheCreateServer(t *testing.T) {
	lab := newFWLab(t)
	a := lab.live(t, fwPoolRoutes(http.StatusOK))
	b := lab.live(t, fwPoolRoutes(http.StatusOK))
	client := fwClient(t, a.Addr, b.Addr)
	ctx := fwCtx(t)
	svc := &managedPoolService{PoolService: client.Pools()}

	if err := client.Pools().CreatePool(ctx, fwSentinel, fwClaim); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	a.Close()
	if err := svc.releasePoisonedSentinel(pve.WithExpectedLockClaim(ctx, fwClaim), "bosh-lock-another"); err == nil {
		t.Fatal("the release of another sentinel succeeded with the family's server gone")
	}
	fwNoMutations(t, "the second server", b)
}

// When a read has already moved the active endpoint to the second server, the
// create's delete still goes to the first server, because the tether names it,
// and it succeeds there.
func TestPoolSentinelReleaseGoesToTheCreateServerAfterAReadMovedTheActiveEndpoint(t *testing.T) {
	lab := newFWLab(t)
	aRoutes := fwPoolRoutes(http.StatusOK)
	aRoutes.HandleFunc(http.MethodGet, "/pools/", fwDropAfterBody)
	a := lab.live(t, aRoutes)
	b := lab.live(t, fwPoolRoutes(http.StatusOK))
	client := fwClient(t, a.Addr, b.Addr)
	ctx := fwCtx(t)
	svc := &managedPoolService{PoolService: client.Pools()}

	if err := client.Pools().CreatePool(ctx, fwSentinel, fwClaim); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if err := svc.releasePoisonedSentinel(pve.WithExpectedLockClaim(ctx, fwClaim), fwSentinel); err != nil {
		t.Fatalf("releasePoisonedSentinel: %v", err)
	}
	if got, want := fwMutations(a), []string{"POST /pools", "DELETE /pools"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the first server read %v, want %v", got, want)
	}
	fwNoMutations(t, "the second server", b)
	if fwGets(b) != 1 {
		t.Errorf("the second server read %d GETs, want the one claim read that moved the active endpoint", fwGets(b))
	}
}

// The storage-replica fan-out starts one create per member from goroutines that
// share a call context. A read that fails over between two creates moves the
// pool's active endpoint to the second server, and the second create is still
// sent to the first, because the reservation names the server that took the
// first. Both creates are answered with a 596, which fixes each tether without
// moving it, and both replays go to the first server as well.
func TestStorageReplicaFanOutCreatesStayOnTheServerOfTheFirst(t *testing.T) {
	const node = "pve1"
	lab := newFWLab(t)
	arrived := make(chan struct{}, 4)
	gate := make(chan struct{})
	aRoutes := pvetest.NewRoutes()
	aRoutes.HandleFunc(http.MethodPost, "/nodes/"+node+"/qemu", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		arrived <- struct{}{}
		<-gate
		w.WriteHeader(596)
	})
	aRoutes.Fail(http.MethodGet, "/cluster/resources", http.StatusServiceUnavailable, "busy")
	a := lab.live(t, aRoutes)
	b := lab.live(t, nil)
	client := fwClient(t, a.Addr, b.Addr)
	ctx := sdkclient.WithRetries(fwCtx(t), 0)

	var wg sync.WaitGroup
	attempts := make(chan error, 4)
	replica := func(vmid int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The first attempt, then the replay the retry helper makes after the 596.
			for range 2 {
				_, err := client.QEMU().Create(ctx, node, map[string]any{"vmid": vmid})
				attempts <- err
			}
		}()
	}
	awaitA := func(which string) {
		t.Helper()
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s create never reached the first server (the second read %d POSTs)", which,
				b.Counts.Method(http.MethodPost))
		}
	}

	replica(101)
	awaitA("first")
	if _, err := client.Cluster().ListResources(ctx, nil); err != nil {
		t.Fatalf("the read that moves the active endpoint failed: %v", err)
	}
	if fwGets(b) == 0 {
		t.Fatal("the read did not move to the second server, so the test did not exercise a moved endpoint")
	}
	replica(102)
	awaitA("second")
	close(gate)
	wg.Wait()
	close(attempts)
	for err := range attempts {
		if err == nil {
			t.Error("a create succeeded against a server that answered 596")
		}
	}
	if got := fwCount(fwMutations(a), "POST /nodes/"+node+"/qemu"); got != 4 {
		t.Errorf("the first server read %d creates, want 4", got)
	}
	fwNoMutations(t, "the second server", b)
}
