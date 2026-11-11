package handlers

import (
	"context"
	"crypto/tls"
	"errors"
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
// It binds a port, closes it, and hands out the address, so the port is free
// again from then on. The files that use it must stay serial, because a
// parallel test could bind the port in that window. No test here calls
// t.Parallel. The seam in pvetest that keeps a refused server's port bound
// needs a dial hook that a client built by pve.NewClientWithTracer does not
// expose, so these tests cannot use it.
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
		// The wire tests count the requests their fakes see, so no discovery
		// reads may join them.
		APIEndpointDiscovery: config.DiscoveryModeOff,
	}, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	return c
}

// fwCallDeadline bounds every call a wire test makes. The tests finish in
// milliseconds, so the bound only matters when a rule under test regresses and
// a call would wait for its production deadline, which for a task poll is
// hundreds of seconds. With the bound, that regression fails the test in
// seconds with a deadline error.
const fwCallDeadline = 10 * time.Second

// fwCtx returns a call context the way the dispatcher builds one, with its own
// affinity scope released when the test ends, plus a zero backoff for the
// retry helpers, a one-millisecond base for the SDK's own retries, and a
// deadline of fwCallDeadline.
func fwCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(ctx, fwCallDeadline)
	t.Cleanup(cancel)
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

// fwAssertDeleteRefusedAt checks that err is the sentinel delete's own
// connection failure against the closed server a. A release that gave up
// earlier, such as one whose claim read failed, would also return an error
// and leave the other server without a mutation, but it would not carry the
// refused delete.
func fwAssertDeleteRefusedAt(t *testing.T, err error, a *pvetest.Server) {
	t.Helper()
	if errors.Is(err, pve.ErrLockClaimChanged) {
		t.Errorf("the release failed on a changed claim, not on the delete: %v", err)
	}
	for _, want := range []string{
		"DELETE request to \"/pools\"",
		"connect phase dial failed for " + a.Addr,
		"connection refused",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the release error lacks %q, so the delete was not what failed: %v", want, err)
		}
	}
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
			fwAssertDeleteRefusedAt(t, err, a)
			fwNoMutations(t, "the second server", b)
			if got := fwCount(fwMutations(a), "DELETE /pools"); got != 0 {
				t.Errorf("the closed server read %d deletes", got)
			}
			if fwGets(b) == 0 {
				t.Error(
					"the claim read never failed over to the second server, so the test did not exercise a read hop",
				)
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
	err := svc.releasePoisonedSentinel(pve.WithExpectedLockClaim(ctx, fwClaim), "bosh-lock-another")
	if err == nil {
		t.Fatal("the release of another sentinel succeeded with the family's server gone")
	}
	fwAssertDeleteRefusedAt(t, err, a)
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
	if got, want := fwMutations(
		a,
	), []string{"POST /pools", "DELETE /pools"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the first server read %v, want %v", got, want)
	}
	fwNoMutations(t, "the second server", b)
	if fwGets(b) != 1 {
		t.Errorf("the second server read %d GETs, want the one claim read that moved the active endpoint", fwGets(b))
	}
}

// The storage-replica fan-out starts one template build per member from
// goroutines that share a call context, and each build allocates a VMID with
// reads and then creates the VM with a POST. The test drives
// replicateStemcellToStorages with two members. The first worker's create reaches the first server and is
// held open. The second worker's allocation read is then answered with a 503,
// so it hops to the second server and moves the pool's active endpoint there.
// The second worker's create is still sent to the first server, because the
// reservation names the server that took the first. The first server answers
// every create with a 596, which fixes each tether without moving it, so the
// retry loop of the allocation replays each create, and every replay goes to
// the first server as well. The second server reads no mutation.
func TestStorageReplicaFanOutCreatesStayOnTheServerOfTheFirst(t *testing.T) {
	const node = "pve1"
	const createPath = "/nodes/" + node + "/qemu"
	lab := newFWLab(t)
	arrived := make(chan struct{}, 16)
	gate := make(chan struct{})
	firstCreate := make(chan struct{})
	var openGate, markFirstCreate sync.Once
	t.Cleanup(func() { openGate.Do(func() { close(gate) }) })
	var reads atomic.Int32
	// The VMID allocation lists the cluster's nodes and each node's guests, so both
	// servers name the one node and list no guests.
	members := func() *pvetest.Routes {
		routes := pvetest.NewRoutes()
		routes.JSON(http.MethodGet, "/nodes", http.StatusOK, []map[string]string{{"node": node, "status": "online"}})
		return routes
	}
	aRoutes := members()
	aRoutes.HandleFunc(http.MethodPost, createPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		markFirstCreate.Do(func() { close(firstCreate) })
		arrived <- struct{}{}
		<-gate
		w.WriteHeader(596)
	})
	// The first allocation read is answered. Every later one waits for the first
	// create to arrive and then fails with a 503, which sends the read to the
	// second server.
	aRoutes.HandleFunc(http.MethodGet, "/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) > 1 {
			select {
			case <-firstCreate:
			case <-r.Context().Done():
				return
			}
			http.Error(w, `{"data":null,"message":"busy"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	a := lab.live(t, aRoutes)
	b := lab.live(t, members())
	deps := Deps{
		PVE:    fwClient(t, a.Addr, b.Addr),
		Config: &config.CPIConfig{StemcellReplicationConcurrency: 2},
		Logger: log.NewNopLogger(),
	}
	ctx := sdkclient.WithRetries(fwCtx(t), 0)
	targets := []storageReplicaTarget{
		{StorageID: "ns_2", Tag: pve.ReplicaStorageTagForStorage("ns_2")},
		{StorageID: "ns_3", Tag: pve.ReplicaStorageTagForStorage("ns_3")},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		replicateStemcellToStorages(ctx, deps, node, "nfs", "stem.qcow2", "abcd1234ef00", targets, nil,
			":heavy:nfs:import/stem.qcow2", "dir-a", pve.StemcellKindHeavy,
			stemcellCloudProps{Name: "ubuntu-noble", Version: "1.585"}, "test")
	}()
	for _, which := range []string{"first", "second"} {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):

			t.Fatalf("the %s create never reached the first server (the second read %d GETs and %d mutations)",
				which, fwGets(b), len(fwMutations(b)))
		}
	}
	if fwGets(b) == 0 {
		t.Fatal("no read moved to the second server, so the test did not exercise a moved endpoint")
	}
	openGate.Do(func() { close(gate) })
	select {
	case <-done:
	case <-time.After(fwCallDeadline):
		t.Fatal("the fan-out did not finish after the first server answered its creates")
	}

	// Each member makes the allocation loop's three attempts, and every attempt
	// is a create answered with a 596.
	const allocationAttempts = 3
	if got, want := fwCount(fwMutations(a), "POST "+createPath), allocationAttempts*len(targets); got != want {
		t.Errorf("the first server read %d creates, want %d (%d attempts for each of %d members)", got, want,
			allocationAttempts, len(targets))
	}
	fwNoMutations(t, "the second server", b)
}
