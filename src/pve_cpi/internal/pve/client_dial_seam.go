package pve

import (
	"context"
	"net"
	"sync/atomic"
)

// clientDial is the dial function a test hands every client built afterwards.
// Only a test in this package sets it, through setClientDialForTest, so it is
// nil in the binary and the client dials as it always has.
type clientDial func(ctx context.Context, network, addr string) (net.Conn, error)

var clientDialOverride atomic.Pointer[clientDial]

// dialOverride returns the dial function a test installed, or nil.
func dialOverride() clientDial {
	if f := clientDialOverride.Load(); f != nil {
		return *f
	}
	return nil
}
