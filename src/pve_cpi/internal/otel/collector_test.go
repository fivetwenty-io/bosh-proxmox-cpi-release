package otel

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// lazyDialBound is how long a setup call may take against a collector that
	// never answers. A lazy exporter returns in microseconds, so the bound
	// leaves a wide margin for a loaded runner, and an exporter that dials or
	// handshakes eagerly stays blocked until lazyDialCtxTimeout and misses it.
	lazyDialBound = 2 * time.Second

	// lazyDialCtxTimeout is the deadline of the context handed to a setup call
	// against a silent collector. It sits well above lazyDialBound so that an
	// eager dial is caught by the bound and never by the context.
	lazyDialCtxTimeout = 10 * time.Second
)

// silentCollector is a local TCP listener that accepts connections and never
// reads or writes. A client that dials or handshakes during setup blocks on
// it, and a client that stays lazy never touches it.
type silentCollector struct {
	addr     string
	ln       net.Listener
	accepted atomic.Int32
	wg       sync.WaitGroup
	once     sync.Once
	mu       sync.Mutex
	conns    []net.Conn
}

func newSilentCollector(t *testing.T) *silentCollector {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for silent collector: %v", err)
	}
	c := &silentCollector{addr: ln.Addr().String(), ln: ln}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			c.mu.Lock()
			c.conns = append(c.conns, conn)
			c.mu.Unlock()
		}
	}()

	t.Cleanup(func() { c.contacts() })
	return c
}

// contacts stops the listener and reports how many connections it accepted.
// A connection made before the call is accepted before the accept loop sees
// the close, so the count is final once the loop has exited.
func (c *silentCollector) contacts() int {
	c.once.Do(func() {
		_ = c.ln.Close()
		c.wg.Wait()
		c.mu.Lock()
		for _, conn := range c.conns {
			_ = conn.Close()
		}
		c.mu.Unlock()
	})
	return int(c.accepted.Load())
}
