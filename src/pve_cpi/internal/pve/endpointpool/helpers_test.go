package endpointpool

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// dialFunc is the shape of a transport's DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// dialers is a base DialContext that routes each address to its own dial
// function, so one endpoint can refuse or drop while another reaches a live
// server. An address with no function gets a real dial.
type dialers struct {
	mu sync.Mutex
	by map[string]dialFunc
}

func (d *dialers) set(addr string, f dialFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.by == nil {
		d.by = make(map[string]dialFunc)
	}
	d.by[addr] = f
}

func (d *dialers) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	f := d.by[addr]
	d.mu.Unlock()
	if f == nil {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return f(ctx, network, addr)
}

// baseFor returns a base transport shaped like the one the SDK builds. With a
// CA it verifies against that CA, as verify_ssl true with pve.ca_cert does,
// and with a nil CA it skips verification, as verify_ssl false does.
func baseFor(ca *pvetest.CA, d *dialers) *http.Transport {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca == nil {
		cfg.InsecureSkipVerify = true
	} else {
		cfg.RootCAs = ca.Pool()
	}
	t := &http.Transport{
		TLSClientConfig:     cfg,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     15 * time.Second,
	}
	if d != nil {
		t.DialContext = d.DialContext
	}
	return t
}

func configured(addr string) Endpoint { return Endpoint{Address: addr, Kind: KindConfigured} }

func discovered(addr string, pins ...Pin) Endpoint {
	return Endpoint{Address: addr, Kind: KindDiscovered, Pins: pins}
}

// newPool builds a pool and closes its idle connections when the test ends.
func newPool(t *testing.T, cfg Config) *Pool {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.CloseIdleConnections)
	return p
}

// endpointOf returns the pool's endpoint for addr, failing the test when the
// pool has none.
func endpointOf(t *testing.T, p *Pool, addr string) *endpoint {
	t.Helper()
	ep := p.lookup(addr)
	if ep == nil {
		t.Fatalf("pool has no endpoint %s", addr)
	}
	return ep
}

// send makes one request on tr to the API path at addr, the way the walk
// will: the send-by bound that guards the request goes on its context first.
func send(t *testing.T, ctx context.Context, tr http.RoundTripper, method, addr, path, body string,
) (*http.Response, error) {
	t.Helper()
	return tr.RoundTrip(apiRequest(t, ctx, method, addr, path, body))
}

// apiRequest builds the request send makes, so a test can inspect it after
// the round trip.
func apiRequest(t *testing.T, ctx context.Context, method, addr, path, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	ctx = connectContext(ctx, method, path)
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+pvetest.APIPrefix+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// closeBody closes a response's body when there is one.
func closeBody(resp *http.Response) {
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// mustOK sends a request that must get a 200, and drains and closes the body
// so the connection goes back to the idle pool.
func mustOK(t *testing.T, tr http.RoundTripper, method, addr, path, body string) {
	t.Helper()
	resp, err := send(t, context.Background(), tr, method, addr, path, body)
	if err != nil {
		t.Fatalf("%s %s on %s: %v", method, path, addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s on %s: status %d", method, path, addr, resp.StatusCode)
	}
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 {
		t.Fatalf("%s %s on %s: protocol %s, want HTTP/1.1", method, path, addr, resp.Proto)
	}
}

// mustConnectPhase fails the test unless err is a connect-phase failure for
// addr in the given phase.
func mustConnectPhase(t *testing.T, err error, addr string, phase Phase) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s connect-phase failure for %s, got success", phase, addr)
	}
	cpe, ok := AsConnectPhase(err)
	if !ok {
		t.Fatalf("want a %s connect-phase failure for %s, got %T: %v", phase, addr, err, err)
	}
	if cpe.Phase != phase {
		t.Fatalf("phase = %s, want %s (%v)", cpe.Phase, phase, err)
	}
	if cpe.Endpoint != addr {
		t.Fatalf("endpoint = %q, want %q", cpe.Endpoint, addr)
	}
}

// breakableConn passes reads and writes through until it is broken, and from
// then on every write fails before a byte reaches the network.
type breakableConn struct {
	net.Conn
	broken atomic.Bool
}

func (c *breakableConn) Write(b []byte) (int, error) {
	if c.broken.Load() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}
	}
	return c.Conn.Write(b)
}
