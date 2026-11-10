package pve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// The tests in this file build clients through newClient with more than one
// endpoint and send real requests to loopback servers. The tests that send a
// mutation do not run in parallel and send it under their own affinity scope,
// because every release of a call's scope clears the scopeless tethers of every
// pool in the process (see endpointpool.WithAffinityScope), and the scope tests
// in this package run in parallel.

// wireEndpoint is one loopback endpoint of a test.
type wireEndpoint struct {
	host string
	port int
	hits atomic.Int64
	// posts counts POST /pools requests.
	posts atomic.Int64
}

func (e *wireEndpoint) address() string { return net.JoinHostPort(e.host, strconv.Itoa(e.port)) }

// liveWireEndpoint starts a TLS server that answers GET /pools/<id> and
// POST /pools.
func liveWireEndpoint(t *testing.T) *wireEndpoint {
	t.Helper()
	e := &wireEndpoint{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/pools/", func(w http.ResponseWriter, _ *http.Request) {
		e.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"comment":"from ` + e.address() + `","members":[]}}`))
	})
	mux.HandleFunc("/api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.Method == http.MethodPost {
			e.posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	e.host = u.Hostname()
	e.port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", srv.URL, err)
	}
	return e
}

// deadWireEndpoint returns an endpoint whose port refuses connections.
func deadWireEndpoint(t *testing.T) *wireEndpoint {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", l.Addr())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return &wireEndpoint{host: "127.0.0.1", port: addr.Port}
}

// wireConfig builds a token-auth config whose host is the first endpoint and
// whose list holds every endpoint given.
func wireConfig(list ...*wireEndpoint) *config.CPIConfig {
	verify := false
	cfg := &config.CPIConfig{
		Host:      list[0].host,
		Port:      list[0].port,
		APIToken:  "root@pam!test=tok-wire",
		VerifySSL: &verify,
	}
	for _, e := range list {
		cfg.Endpoints = append(cfg.Endpoints, e.address())
	}
	return cfg
}

// A client with two configured endpoints whose first refuses connections reads
// from the second, and the walk's failover leaves the first alone afterwards.
func TestNewClientWithTwoEndpointsFailsOverAReadToTheSecond(t *testing.T) {
	t.Parallel()
	dead, live := deadWireEndpoint(t), liveWireEndpoint(t)
	c, err := NewClientWithTracer(wireConfig(dead, live), log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	comment, found, err := c.Pools().GetPoolComment(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetPoolComment with the first endpoint down: %v", err)
	}
	if !found || !strings.Contains(comment, live.address()) {
		t.Errorf("comment = %q found = %v, want an answer from %s", comment, found, live.address())
	}
	if live.hits.Load() != 1 {
		t.Errorf("second endpoint served %d requests, want 1", live.hits.Load())
	}
}

// A client with a list of one endpoint, and one with only pve.host, keep the
// SDK's own transport, so a refused connection is an error and nothing fails
// over.
func TestNewClientWithOneEndpointDoesNotFailOver(t *testing.T) {
	t.Parallel()
	for name, shape := range map[string]func(*config.CPIConfig){
		"no list":     func(cfg *config.CPIConfig) { cfg.Endpoints = nil },
		"list of one": func(cfg *config.CPIConfig) {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dead := deadWireEndpoint(t)
			cfg := wireConfig(dead)
			shape(cfg)
			c, err := NewClientWithTracer(cfg, log.NewNopLogger(), nil)
			if err != nil {
				t.Fatalf("NewClientWithTracer: %v", err)
			}
			if _, _, err := c.Pools().GetPoolComment(context.Background(), "p1"); err == nil {
				t.Fatal("GetPoolComment against a refused port = nil error, want the connection failure")
			}
		})
	}
}

// A mutation that was never sent, because the first endpoint refused the
// connection, goes to the second endpoint once, and the pool keeps the next
// mutation with the same key there.
func TestNewClientWithTwoEndpointsSendsANeverSentMutationToTheSecond(t *testing.T) {
	dead, live := deadWireEndpoint(t), liveWireEndpoint(t)
	c, err := NewClientWithTracer(wireConfig(dead, live), log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	defer release()
	for i := range 2 {
		if err := c.Pools().CreatePool(ctx, "bosh-lock", "held"); err != nil {
			t.Fatalf("CreatePool %d with the first endpoint down: %v", i, err)
		}
	}
	if got := live.posts.Load(); got != 2 {
		t.Errorf("second endpoint received %d POSTs, want 2", got)
	}
}

// The login goes through the failover transport as well, so a password client
// whose first endpoint is down still gets its ticket from the second.
func TestNewClientWithTwoEndpointsLogsInThroughTheSecond(t *testing.T) {
	t.Parallel()
	dead := deadWireEndpoint(t)
	var logins, reads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/ticket", func(w http.ResponseWriter, _ *http.Request) {
		logins.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"ticket":"PVE:root@pam:1","CSRFPreventionToken":"csrf","username":"root@pam"}}`))
	})
	mux.HandleFunc("/api2/json/pools/", func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"comment":"ok","members":[]}}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	live := &wireEndpoint{host: u.Hostname(), port: port}
	cfg := wireConfig(dead, live)
	cfg.APIToken = ""
	cfg.User, cfg.Password, cfg.Realm = "root", "secret", "pam"
	c, err := NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	if _, _, err := c.Pools().GetPoolComment(context.Background(), "p1"); err != nil {
		t.Fatalf("GetPoolComment with password auth and the first endpoint down: %v", err)
	}
	if logins.Load() == 0 || reads.Load() != 1 {
		t.Errorf("second endpoint saw %d logins and %d reads, want a login and one read", logins.Load(), reads.Load())
	}
}

// A walk that reaches no endpoint surfaces an error that names every endpoint
// the walk tried, and the audit description names them too.
func TestNewClientWithTwoDeadEndpointsNamesBothInTheAuditDescription(t *testing.T) {
	t.Parallel()
	first, second := deadWireEndpoint(t), deadWireEndpoint(t)
	c, err := NewClientWithTracer(wireConfig(first, second), log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	_, _, err = c.Pools().GetPoolComment(context.Background(), "p1")
	if err == nil {
		t.Fatal("GetPoolComment with both endpoints down = nil error")
	}
	var walk *endpointpool.EntryUnreachableError
	if !errors.As(err, &walk) {
		t.Fatalf("error %v does not carry the walk's EntryUnreachableError", err)
	}
	want := "connection to " + first.address() + ", " + second.address() + " failed"
	if got := DescribeAuditError(err); got != want {
		t.Errorf("DescribeAuditError = %q, want %q", got, want)
	}
	if !IsTransientTransport(err) {
		t.Error("a walk that reached no endpoint should read as transient transport")
	}
}

// A pool that cannot be built fails the client's construction with the pool's
// own reason, not the SDK's generic wrapper error.
func TestNewClientReportsAPoolThatCannotBeBuilt(t *testing.T) {
	t.Parallel()
	a := liveWireEndpoint(t)
	cfg := wireConfig(a, a)
	_, err := NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err == nil {
		t.Fatal("NewClientWithTracer with a repeated endpoint = nil error")
	}
	for _, want := range []string{"pve client init", "endpoint pool", "listed twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "nil RoundTripper") {
		t.Errorf("error %q reports the SDK's generic wrapper failure, not the pool's reason", err.Error())
	}
}

// A first endpoint that is not pve.host is refused before any transport exists.
func TestNewClientRefusesAFirstEndpointThatIsNotTheHost(t *testing.T) {
	t.Parallel()
	a, b := liveWireEndpoint(t), liveWireEndpoint(t)
	cfg := wireConfig(a, b)
	cfg.Endpoints = []string{b.address(), a.address()}
	_, err := NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err == nil || !strings.Contains(err.Error(), "is not pve.host with pve.port") {
		t.Fatalf("NewClientWithTracer error = %v, want the first-endpoint refusal", err)
	}
}

// A routing config and a list together build, because no pinning is set.
func TestNewClientWithTwoEndpointsAndNodeEndpointsBuilds(t *testing.T) {
	t.Parallel()
	a, b := liveWireEndpoint(t), liveWireEndpoint(t)
	cfg := wireConfig(a, b)
	cfg.NodeEndpoints = map[string]string{"pve2": b.address()}
	if _, err := NewClientWithTracer(cfg, log.NewNopLogger(), nil); err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
}
