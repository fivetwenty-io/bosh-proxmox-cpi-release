package pve

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

const workerSecondEndpoint = "pve9.dc1.example.com:8006"

// failoverReads sends each discovery read of the worker's client through a
// real endpoint pool over two configured endpoints, so the endpoint that
// answers a read is the one the pool's walk chose. The first endpoint can be
// taken down, and the reads that follow hop to the second.
type failoverReads struct {
	pool  *endpointpool.Pool
	down  atomic.Bool
	first *pvetest.Server
	other *pvetest.Server
	// firstBeforeDown is the number of requests the first endpoint had read
	// when it was taken down.
	firstBeforeDown atomic.Int32
	// beforeRefuse, when set before the first read, runs in each dial to the
	// first endpoint after takeDown, before the dial is refused.
	beforeRefuse func()
}

func newFailoverReads(t *testing.T) *failoverReads {
	t.Helper()
	return newFailoverReadsWith(t, nil)
}

// newFailoverReadsWith is newFailoverReads with the first endpoint answering
// from routes, the default routes when nil.
func newFailoverReadsWith(t *testing.T, routes *pvetest.Routes) *failoverReads {
	t.Helper()
	ca := pvetest.NewCA(t)
	f := &failoverReads{
		first: pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve1.dc1.example.com"}}),
			routes),
		other: pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve9.dc1.example.com"}}), nil),
	}
	refused := pvetest.NewRefused(t)
	base := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			switch addr {
			case workerSeed:
				if f.down.Load() {
					if f.beforeRefuse != nil {
						f.beforeRefuse()
					}
					return refused.DialContext(ctx, network, refused.Addr)
				}
				return (&net.Dialer{}).DialContext(ctx, network, f.first.Addr)
			case workerSecondEndpoint:
				return (&net.Dialer{}).DialContext(ctx, network, f.other.Addr)
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: net.UnknownNetworkError(addr)}
		},
	}
	pool, err := endpointpool.New(endpointpool.Config{
		Base: base,
		Endpoints: []endpointpool.Endpoint{
			{Address: workerSeed, Kind: endpointpool.KindConfigured},
			{Address: workerSecondEndpoint, Kind: endpointpool.KindConfigured},
		},
	})
	if err != nil {
		t.Fatalf("endpointpool.New: %v", err)
	}
	t.Cleanup(pool.CloseIdleConnections)
	f.pool = pool
	return f
}

// read sends one configured-only GET through the pool, as the SDK would for
// the read it stands in for, and reads the answer to its end as the SDK does.
//
// The read to the end is what lets takeDown work. A body closed before its end
// leaves the transport to drain the rest on its own goroutine and only then
// return the connection to the idle pool, after Close has returned. A takeDown
// in that window finds no idle connection to drop, and when the drain ends
// while the next read waits for a connection, the transport hands this one to
// it, so the first endpoint answers that read and the reads after it. A body
// read to its end returns the connection to the idle pool before the final
// Read returns, so it is there for takeDown to drop.
func (f *failoverReads) read(t *testing.T, ctx context.Context) {
	t.Helper()
	req, err := http.NewRequestWithContext(endpointpool.WithConfiguredOnly(ctx), http.MethodGet,
		"https://"+workerSeed+"/api2/json/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "PVEAPIToken="+workerUser+"="+workerCredential)
	resp, err := f.pool.RoundTrip(req)
	if err != nil {
		t.Fatalf("read through the pool: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read the answer through the pool: %v", err)
	}
}

// client returns the environment's cluster client with every read also sent
// through the pool. after runs once the named read has been answered.
func (f *failoverReads) client(t *testing.T, e *workerEnv, after map[string]func()) *discClient {
	t.Helper()
	c := markedClient(e.cluster, e.reqs)
	done := func(kind string) {
		if fn := after[kind]; fn != nil {
			fn()
		}
	}
	nodesFn, certFn := c.nodes.listNodesFn, c.nodes.listCertificatesInfoFn
	statusFn := c.cluster.listStatusFn
	c.nodes.listNodesFn = func(ctx context.Context) (*sdknodes.ListNodesResponse, error) {
		f.read(t, ctx)
		defer done("nodes")
		return nodesFn(ctx)
	}
	c.cluster.listStatusFn = func(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
		f.read(t, ctx)
		defer done("status")
		return statusFn(ctx)
	}
	c.nodes.listCertificatesInfoFn = func(
		ctx context.Context, node string,
	) (*sdknodes.ListCertificatesInfoResponse, error) {
		f.read(t, ctx)
		defer done("certificates")
		return certFn(ctx, node)
	}
	return c
}

// takeDown makes the first endpoint refuse every dial and drops its idle
// connections, so the next read hops.
func (f *failoverReads) takeDown() {
	f.firstBeforeDown.Store(f.first.Counts.Requests.Load())
	f.down.Store(true)
	f.pool.CloseIdleConnections()
}

// mustStayDown fails the test when the first endpoint read a request after
// takeDown, which would mean a read the test counts as hopped was answered by
// the endpoint it counts as down.
func (f *failoverReads) mustStayDown(t *testing.T) {
	t.Helper()
	if !f.down.Load() {
		t.Fatal("the first endpoint was never taken down, so the reads never hopped")
	}
	if n := f.first.Counts.Requests.Load() - f.firstBeforeDown.Load(); n != 0 {
		t.Fatalf("the first endpoint read %d requests after it was taken down", n)
	}
}

// A read whose answer reaches the client in two parts is the case in which the
// transport would drain a body closed early on its own goroutine. The second
// part comes after a pause shorter than that drain waits for, and the dial the
// next read makes to the downed first endpoint is held until a connection goes
// idle. A read that closed its body early returns before the second part, so
// the drain ends after takeDown, and the transport hands the connection to the
// next read that is waiting for one, which the first endpoint then answers. A
// read to the end leaves the connection idle before takeDown, which drops it,
// and the next read hops to the second endpoint.
func TestFailoverReadsTakeDownDropsTheConnectionOfTheReadBeforeIt(t *testing.T) {
	t.Parallel()
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodGet, "/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "}")
	})
	f := newFailoverReadsWith(t, routes)
	idle := make(chan struct{}, 1)
	f.beforeRefuse = func() {
		select {
		case <-idle:
		case <-time.After(time.Second):
		}
	}
	trace := &httptrace.ClientTrace{PutIdleConn: func(err error) {
		if err == nil {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	}}
	f.read(t, httptrace.WithClientTrace(context.Background(), trace))
	f.takeDown()
	rec := &endpointpool.AnswerRecorder{}
	f.read(t, endpointpool.WithAnswerRecorder(context.Background(), rec))
	f.mustStayDown(t)
	if got := rec.Last(); got != workerSecondEndpoint {
		t.Fatalf("the read after takeDown was answered by %q, want %s", got, workerSecondEndpoint)
	}
}

// Discovery's own reads walk the configured endpoints and can hop. The refresh
// begins on an answer from the first endpoint, which then goes down after the
// node list was read, so the cluster root is read through the second. The
// observation and its log lines name the endpoint that answered the identity
// read, and a Debug line says the run's reads came from more than one.
func TestDiscoveryWorker_ObservationNamesTheEndpointThatAnsweredTheIdentityRead(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{
		verifySSL: true, endpoints: []string{workerSeed, workerSecondEndpoint},
	})
	f := newFailoverReads(t)
	e.client = f.client(t, e, map[string]func(){"nodes": f.takeDown})
	e.run()
	f.mustStayDown(t)

	p := e.payload()
	if p.Cluster == nil || p.Cluster.ObservedVia != workerSecondEndpoint {
		t.Fatalf("identity %+v, want it observed via the endpoint that answered the identity read, %s",
			p.Cluster, workerSecondEndpoint)
	}
	entry, ok := e.line("the reads were answered by more than one configured endpoint")
	if !ok || entry.Level != log.LevelDebug {
		t.Fatalf("no Debug line about the reads' endpoints in %s", e.dump())
	}
	if !strings.Contains(entry.Message, "the cluster identity came from "+workerSecondEndpoint) {
		t.Fatalf("line %q does not name the endpoint of the identity read", entry.Message)
	}
	if entry.Attrs["triggered_by"] != workerSeed || entry.Attrs["endpoint"] != workerSecondEndpoint {
		t.Fatalf("line attrs %v, want the trigger %s and the endpoint %s", entry.Attrs, workerSeed,
			workerSecondEndpoint)
	}
}

// When the cluster identity changes while the refresh's trigger is down, the
// Error line and the stored observation name the endpoint that reported the
// new identity and not the one whose answer began the refresh.
func TestDiscoveryWorker_IdentityChangeLineNamesTheEndpointThatReportedIt(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{
		verifySSL: true, endpoints: []string{workerSeed, workerSecondEndpoint},
	})
	f := newFailoverReads(t)
	e.client = f.client(t, e, nil)
	e.run()
	if p := e.payload(); p.Cluster == nil || p.Cluster.ObservedVia != workerSeed {
		t.Fatalf("identity %+v, want the first run observed via %s", p.Cluster, workerSeed)
	}
	before := e.payload().Cluster.RootCASHA256

	f.takeDown()
	e.cluster.replaceRoot()
	e.clock.advance(endpointcache.RefreshInterval)
	e.run()
	f.mustStayDown(t)

	p := e.payload()
	if p.Cluster.RootCASHA256 == before {
		t.Fatal("the identity was not replaced")
	}
	e.wantLine("endpoint cache: cluster identity changed from "+before[:8]+" to "+p.Cluster.RootCASHA256[:8]+
		" through configured endpoint "+workerSecondEndpoint+"; with verify_ssl true the cached nodes and pins "+
		"are replaced", log.LevelError)
	if p.Cluster.ObservedVia != workerSecondEndpoint {
		t.Fatalf("observed via %q, want %s", p.Cluster.ObservedVia, workerSecondEndpoint)
	}
	e.noLine("the reads were answered by more than one configured endpoint")
}
