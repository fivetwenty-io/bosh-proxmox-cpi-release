package pve

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// countingTripper counts the round trips the SDK hands the pool.
type countingTripper struct {
	next  http.RoundTripper
	trips atomic.Int64
}

func (c *countingTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.trips.Add(1)
	return c.next.RoundTrip(req)
}

// switchingDialer sends the dials for one address to whichever server it
// currently names, so a test can make a discovered endpoint's address present
// a different certificate.
type switchingDialer struct {
	mu   sync.Mutex
	addr string
	to   string
}

func (d *switchingDialer) point(to string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.to = to
}

func (d *switchingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	if addr == d.addr {
		addr = d.to
	}
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// mutationRoutes answers the two mutations the test tethers.
func mutationRoutes() *pvetest.Routes {
	r := pvetest.NewRoutes()
	r.JSON(http.MethodPost, "/nodes/pve3/qemu/101/status/start", http.StatusOK, "UPID:pve3:start")
	r.JSON(http.MethodDelete, "/nodes/pve3/qemu/101", http.StatusOK, "UPID:pve3:destroy")
	return r
}

// Through the SDK's own retry loop, a request routed to a discovered endpoint
// that failed its pin, and a mutation tethered to one, are each handed to the
// pool once and refused once, though the SDK may retry them three times. The
// SDK stops because the refusal is a certificate failure, as a pin the
// handshake refused is. The endpoint's address gets no connection after the
// one whose certificate failed.
func TestTheSDKDoesNotRetryARefusedIneligibleEndpoint(t *testing.T) {
	ca := pvetest.NewCA(t)
	leaf := ca.NewLeaf(t, pvetest.LeafOptions{})
	c := pvetest.NewLive(t, leaf, nil)
	good := pvetest.NewLive(t, leaf, mutationRoutes())
	bad := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), mutationRoutes())
	// The discovered endpoint's address is good's, and the dialer can send it
	// to bad, whose leaf is outside the pin set.
	dialer := &switchingDialer{addr: good.Addr, to: good.Addr}

	var (
		pool    *endpointpool.Pool
		counter *countingTripper
		poolErr error
	)
	host, port := c.HostPort()
	api, err := sdkclient.NewClient(sdkclient.Options{
		Host:       host,
		Port:       port,
		Protocol:   sdkclient.ProtocolHTTPS,
		APIToken:   "root@pam!cpi=00000000-0000-0000-0000-000000000000",
		Timeout:    10 * time.Second,
		SSLOptions: &sdkclient.SSLOptions{VerifyMode: sdkclient.SSLVerifyNone},
		WrapTransport: func(base *http.Transport) http.RoundTripper {
			base.DialContext = dialer.DialContext
			pool, poolErr = endpointpool.New(endpointpool.Config{Base: base, Endpoints: []endpointpool.Endpoint{
				{Address: c.Addr, Kind: endpointpool.KindConfigured},
				{Address: good.Addr, Kind: endpointpool.KindDiscovered, Node: "pve3", Online: true,
					Pins: []endpointpool.Pin{endpointpool.PinOf(leaf.DER)}},
			}})
			if poolErr != nil {
				return nil
			}
			counter = &countingTripper{next: pool}
			return counter
		},
	})
	if poolErr != nil {
		t.Fatalf("build the pool: %v", poolErr)
	}
	if err != nil {
		t.Fatalf("build the SDK client: %v", err)
	}
	t.Cleanup(pool.CloseIdleConnections)
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	t.Cleanup(release)
	retrying := func(ctx context.Context) context.Context {
		return sdkclient.WithRetryDelay(sdkclient.WithRetries(ctx, 3), time.Millisecond)
	}
	routed := func(ctx context.Context) context.Context { return sdkclient.WithHost(ctx, good.Addr) }

	// Both mutations go to the discovered endpoint while its certificate is
	// in the pin set, which tethers them there for the call.
	if _, err := api.PostCtx(routed(ctx), "/nodes/pve3/qemu/101/status/start", nil); err != nil {
		t.Fatalf("the routed POST while the endpoint is eligible: %v", err)
	}
	if _, err := api.DeleteCtx(routed(ctx), "/nodes/pve3/qemu/101", nil); err != nil {
		t.Fatalf("the routed DELETE while the endpoint is eligible: %v", err)
	}

	// The address now presents a leaf outside the pin set. A routed read
	// fails its pin in the handshake, which makes the endpoint ineligible.
	pool.CloseIdleConnections()
	dialer.point(bad.Addr)
	before := counter.trips.Load()
	_, err = api.GetCtx(retrying(routed(ctx)), "/version", nil)
	if !errors.Is(err, endpointpool.ErrPinMismatch) {
		t.Fatalf("the handshake should refuse the pin, got %v", err)
	}
	if n := counter.trips.Load() - before; n != 1 {
		t.Fatalf("the pin failure reached the pool %d times, want once", n)
	}
	if !pool.RefreshDue() {
		t.Fatal("the pin failure should make a refresh due")
	}
	badConns, goodConns := bad.Counts.Conns.Load(), good.Counts.Conns.Load()

	calls := []struct {
		name string
		call func() error
	}{
		{"a routed read", func() error {
			_, err := api.GetCtx(retrying(routed(ctx)), "/version", nil)
			return err
		}},
		{"a tethered POST", func() error {
			_, err := api.PostCtx(retrying(ctx), "/nodes/pve3/qemu/101/status/start", nil)
			return err
		}},
		{"a tethered DELETE", func() error {
			_, err := api.DeleteCtx(retrying(ctx), "/nodes/pve3/qemu/101", nil)
			return err
		}},
	}
	for _, tc := range calls {
		before := counter.trips.Load()
		err := tc.call()
		if n := counter.trips.Load() - before; n != 1 {
			t.Fatalf("%s reached the pool %d times, want once (%v)", tc.name, n, err)
		}
		var cve *tls.CertificateVerificationError
		if !errors.As(err, &cve) || !errors.Is(err, endpointpool.ErrPinMismatch) {
			t.Fatalf("%s should fail as a certificate refusal, got %T: %v", tc.name, err, err)
		}
		if endpointpool.IsMaybeSent(err) {
			t.Fatalf("%s was refused before any send, got %v", tc.name, err)
		}
	}
	if n := bad.Counts.Conns.Load() - badConns + good.Counts.Conns.Load() - goodConns; n != 0 {
		t.Fatalf("the ineligible endpoint's address got %d connections", n)
	}
	if n := pvetest.TotalMethod(http.MethodPost, c) + pvetest.TotalMethod(http.MethodDelete, c); n != 0 {
		t.Fatalf("a tethered mutation moved to the configured endpoint (%d)", n)
	}

	// The configured endpoint still answers through the same client.
	if _, err := api.GetCtx(ctx, "/version", nil); err != nil {
		t.Fatalf("a read on the walk: %v", err)
	}
}
