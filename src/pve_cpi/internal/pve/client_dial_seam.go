package pve

import (
	"context"
	"net"
	"sync/atomic"
)

// clientDial is the dial function a test hands every client built afterwards.
// It is nil outside tests, and then the client dials as it always has.
type clientDial func(ctx context.Context, network, addr string) (net.Conn, error)

var clientDialOverride atomic.Pointer[clientDial]

// dialOverride returns the dial function a test installed, or nil.
func dialOverride() clientDial {
	if f := clientDialOverride.Load(); f != nil {
		return *f
	}
	return nil
}

// SetClientDialForTest makes every client built afterwards establish its
// connections with dial, and returns a restore function. A wire test uses it
// to send a derived node name such as pve2.dc1.example.com to a loopback
// server, which a client built by NewClientWithTracer gives no other way to do.
//
//	defer pve.SetClientDialForTest(dial)()
func SetClientDialForTest(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func() {
	d := clientDial(dial)
	prev := clientDialOverride.Swap(&d)
	return func() { clientDialOverride.Store(prev) }
}
