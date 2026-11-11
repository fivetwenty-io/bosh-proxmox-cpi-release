package pve

import (
	"context"
	"net"
	"testing"
)

// setClientDialForTest makes every client built afterwards establish its
// connections with dial, and restores the previous dial function when the test
// ends. A wire test uses it to send a derived node name such as
// pve2.dc1.example.com to a loopback server, which a client built by
// NewClientWithTracer gives no other way to do. The setter lives in a test
// file, so no code outside this package's tests can replace the dialer.
func setClientDialForTest(t *testing.T, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	d := clientDial(dial)
	prev := clientDialOverride.Swap(&d)
	t.Cleanup(func() { clientDialOverride.Store(prev) })
}
