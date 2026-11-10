package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// closeCountingConn counts the times its connection is closed.
type closeCountingConn struct {
	net.Conn
	closes *atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// DropDiscovered takes every discovered endpoint out of the walk and the
// routed path, moves the active endpoint off a discovered one, and writes no
// mark, no line, and no refresh.
func TestDropDiscoveredTakesEveryDiscoveredEndpointOut(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d1, d2 := l.live(t, nil), l.live(t, nil)
	sink := &markRecorder{}
	p := l.poolWith(t, sink, nil, nil, configured(c.Addr),
		node(d1.Addr, "pve2", true, PinOf(l.leaf.DER)), node(d2.Addr, "pve3", false, PinOf(l.leaf.DER)))
	var closes atomic.Int32
	l.d.set(d1.Addr, func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &closeCountingConn{Conn: conn, closes: &closes}, nil
	})
	dep := endpointOf(t, p, d1.Addr)
	if _, ok := p.promote(dep, 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}
	_, gen := p.current()
	// A connection to the discovered endpoint is open and idle before the drop.
	resp, err := send(t, context.Background(), p, http.MethodGet, d1.Addr, "/version", "")
	mustStatus(t, resp, err, http.StatusOK)
	conns := d1.Counts.Conns.Load()
	if conns == 0 || closes.Load() != 0 {
		t.Fatalf("before the drop: %d connections, %d closed, want one open", conns, closes.Load())
	}

	if n := p.DropDiscovered(); n != 2 {
		t.Fatalf("DropDiscovered took out %d endpoints, want 2", n)
	}
	if closes.Load() == 0 {
		t.Fatal("the drop left the dropped endpoint's idle connection open")
	}
	mustActive(t, p, c.Addr)
	if _, after := p.current(); after != gen+1 {
		t.Fatalf("generation %d after the drop, want %d", after, gen+1)
	}
	if _, ok := p.promote(dep, gen); ok {
		t.Fatal("a success that started before the drop must not make the dropped endpoint active")
	}
	order, _ := p.walkOrder(false)
	wantOrder(t, order, c.Addr)

	for range 3 {
		resp, err = send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
		mustStatus(t, resp, err, http.StatusOK)
	}
	resp, err = send(t, context.Background(), p, http.MethodGet, d2.Addr, "/version", "")
	closeBody(resp)
	if !errors.Is(err, errDropped) || errors.Is(err, errIneligible) || errors.Is(err, ErrPinMismatch) ||
		!IsConnectPhase(err) || IsMaybeSent(err) {
		t.Fatalf("a routed request to a dropped endpoint should be refused unsent, got %v", err)
	}
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) || !strings.Contains(err.Error(), "the cluster identity changed") {
		t.Fatalf("the drop's refusal should be a certificate error that names the identity change, got %v", err)
	}
	if n := d1.Counts.Conns.Load(); n != conns {
		t.Fatalf("the dropped endpoint got %d more connections", n-conns)
	}
	if n := d2.Counts.Conns.Load(); n != 0 {
		t.Fatalf("the dropped endpoint d2 got %d connections", n)
	}
	if marks := sink.all(); len(marks) != 0 {
		t.Fatalf("the drop wrote marks %+v", marks)
	}
	if p.RefreshDue() {
		t.Fatal("the drop made a refresh due")
	}
	if n := len(l.entries(slog.LevelWarn, "pve endpoint ")); n != 0 {
		t.Fatalf("the drop logged %d Warn lines", n)
	}
	if n := p.DropDiscovered(); n != 0 {
		t.Fatalf("a second drop took out %d endpoints, want 0", n)
	}
}

// A pool whose active endpoint is configured keeps it and its generation.
func TestDropDiscoveredKeepsAConfiguredActiveEndpoint(t *testing.T) {
	l := newLab(t)
	p := l.poolWith(t, nil, nil, nil, configured("c1:8006"), configured("c2:8006"),
		node("d1:8006", "pve2", true, Pin{1}))
	_, gen := p.current()
	if n := p.DropDiscovered(); n != 1 {
		t.Fatalf("DropDiscovered took out %d endpoints, want 1", n)
	}
	mustActive(t, p, "c1:8006")
	if _, after := p.current(); after != gen {
		t.Fatalf("generation %d after the drop, want %d", after, gen)
	}
	order, _ := p.walkOrder(false)
	wantOrder(t, order, "c1:8006", "c2:8006")

	only := l.poolWith(t, nil, nil, nil, configured("c1:8006"))
	if n := only.DropDiscovered(); n != 0 {
		t.Fatalf("a pool without discovered endpoints dropped %d", n)
	}
	mustActive(t, only, "c1:8006")
}

// An active discovered endpoint that already failed its pin moves to a
// configured endpoint, and it is not counted again.
func TestDropDiscoveredMovesOffAnIneligibleActiveEndpoint(t *testing.T) {
	l := newLab(t)
	p := l.poolWith(t, nil, nil, nil, configured("c1:8006"),
		node("d1:8006", "pve2", true, Pin{1}), node("d2:8006", "pve3", true, Pin{2}))
	d1 := endpointOf(t, p, "d1:8006")
	if _, ok := p.promote(d1, 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}
	p.makeIneligible(d1, &ConnectPhaseError{Phase: PhasePin, Endpoint: d1.addr, Err: ErrPinMismatch})
	mustActive(t, p, "d1:8006")
	if n := p.DropDiscovered(); n != 1 {
		t.Fatalf("DropDiscovered took out %d endpoints, want only d2", n)
	}
	mustActive(t, p, "c1:8006")
}

// Walks that run while the endpoints are dropped send nothing to a dropped
// endpoint once the drop returns, and the race detector sees every access
// under the pool's lock.
func TestDropDiscoveredUnderConcurrentWalks(t *testing.T) {
	l := newLab(t)
	c := l.live(t, nil)
	d := l.live(t, nil)
	p := l.poolWith(t, nil, nil, nil, configured(c.Addr), node(d.Addr, "pve2", true, PinOf(l.leaf.DER)))
	if _, ok := p.promote(endpointOf(t, p, d.Addr), 0); !ok {
		t.Fatal("promote the discovered endpoint")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
			if err != nil {
				t.Error(err)
				return
			}
			closeBody(resp)
		})
	}
	wg.Go(func() {
		<-start
		p.DropDiscovered()
	})
	close(start)
	wg.Wait()
	before := d.Counts.Requests.Load()
	for range 4 {
		resp, err := send(t, context.Background(), p, http.MethodGet, c.Addr, "/version", "")
		mustStatus(t, resp, err, http.StatusOK)
	}
	if n := d.Counts.Requests.Load(); n != before {
		t.Fatalf("the dropped endpoint got %d requests after the drop", n-before)
	}
	mustActive(t, p, c.Addr)
}
