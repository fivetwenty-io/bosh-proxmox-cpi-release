package endpointpool

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// A configured endpoint built without a node name learns one afterwards. Until
// it does, a 503 for a path that names a node comes back with no hop, because
// the pool cannot tell the node is the entry's own. Once SetEndpointNode names
// it, the same 503 moves the read to the next endpoint.
func TestSetEndpointNodeMakesANodeScopedStatusHop(t *testing.T) {
	l := newLab(t)
	a := l.live(t, failing(http.MethodGet, "/nodes/pve1/status", http.StatusServiceUnavailable, "busy"))
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	resp, err := send(t, context.Background(), p, http.MethodGet, a.Addr, "/nodes/pve1/status", "")
	mustStatus(t, resp, err, http.StatusServiceUnavailable)
	if n := b.Counts.Requests.Load(); n != 0 {
		t.Fatalf("B got %d requests before the node was known, want 0", n)
	}
	mustDemoted(t, p, a.Addr, false)

	p.SetEndpointNode(a.Addr, "pve1")
	resp, err = send(t, context.Background(), p, http.MethodGet, a.Addr, "/nodes/pve1/status", "")
	mustStatus(t, resp, err, http.StatusOK)
	if n := b.Counts.Requests.Load(); n != 1 {
		t.Fatalf("B got %d requests after the node was known, want the one hop", n)
	}
	mustDemoted(t, p, a.Addr, true)
	mustActive(t, p, b.Addr)
}

// A 595 names an unreachable node in the path. With the entry's node learned, a
// 595 for another node still never moves a request, and a mutation never moves
// for a 595 even on the entry's own node, so only the read of the entry's own
// node hops.
func TestSetEndpointNodeKeepsA595ForAnotherNodeAndForAMutationInPlace(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		hops   bool
	}{
		{"read of its own node", http.MethodGet, "/nodes/pve1/qemu", "", true},
		{"read of another node", http.MethodGet, "/nodes/pve2/qemu", "", false},
		{"mutation on its own node", http.MethodPost, "/nodes/pve1/qemu", "vmid=101", false},
		{"mutation on another node", http.MethodPost, "/nodes/pve2/qemu", "vmid=101", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLab(t)
			a := l.live(t, failing(c.method, c.path, statusNoNextHop, "node unreachable"))
			b := l.live(t, nil)
			p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))
			p.SetEndpointNode(a.Addr, "pve1")

			resp, err := send(t, context.Background(), p, c.method, a.Addr, c.path, c.body)
			if c.hops {
				mustStatus(t, resp, err, http.StatusOK)
				mustDemoted(t, p, a.Addr, true)
				return
			}
			mustStatus(t, resp, err, statusNoNextHop)
			if n := b.Counts.Requests.Load(); n != 0 {
				t.Fatalf("B got %d requests, want 0", n)
			}
			mustDemoted(t, p, a.Addr, false)
			mustActive(t, p, a.Addr)
		})
	}
}

// A hung entry holds a node-scoped read for the caller's whole deadline when
// the pool does not know the entry's node, and gives it the response-header
// deadline once SetEndpointNode names the node the path addresses.
func TestSetEndpointNodeGivesANodeScopedReadItsHeaderDeadline(t *testing.T) {
	const deadline = 100 * time.Millisecond
	t.Run("node unknown", func(t *testing.T) {
		l := newLab(t)
		a := pvetest.NewHung(t, l.leaf)
		b := l.live(t, nil)
		p := l.pool(t, deadline, configured(a.Addr), configured(b.Addr))
		ctx, cancel := context.WithTimeout(context.Background(), 4*deadline)
		defer cancel()
		_, err := send(t, ctx, p, http.MethodGet, a.Addr, "/nodes/pve1/status", "")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the caller's deadline, because no header deadline applies", err)
		}
		if n := b.Counts.Requests.Load(); n != 0 {
			t.Fatalf("B got %d requests, want 0", n)
		}
	})
	t.Run("node learned", func(t *testing.T) {
		l := newLab(t)
		a := pvetest.NewHung(t, l.leaf)
		b := l.live(t, nil)
		p := l.pool(t, deadline, configured(a.Addr), configured(b.Addr))
		p.SetEndpointNode(a.Addr, "pve1")
		// The bound keeps a pool that gave the read no deadline from hanging the test.
		ctx, cancel := context.WithTimeout(context.Background(), 20*deadline)
		defer cancel()
		resp, err := send(t, ctx, p, http.MethodGet, a.Addr, "/nodes/pve1/status", "")
		mustStatus(t, resp, err, http.StatusOK)
		mustDemoted(t, p, a.Addr, true)
		warns := l.failovers()
		if len(warns) != 1 {
			t.Fatalf("failover lines = %d, want 1", len(warns))
		}
		mustAttrs(t, warns[0], map[string]string{"class": classHeaderDeadline})
	})
}

// SetEndpointNode names a configured endpoint only. It ignores an address the
// pool does not hold, an empty name, and a discovered endpoint.
func TestSetEndpointNodeNamesOnlyAConfiguredEndpoint(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr),
		node("pve4.example.com:8006", "pve4", true, Pin{4}))

	p.SetEndpointNode("203.0.113.9:8006", "pve9")
	p.SetEndpointNode("not an address", "pve9")
	p.SetEndpointNode(a.Addr, "")
	p.SetEndpointNode(a.Addr, "  ")
	p.SetEndpointNode("pve4.example.com:8006", "pve9")
	for addr, want := range map[string]string{a.Addr: "", b.Addr: "", "pve4.example.com:8006": "pve4"} {
		if got := endpointOf(t, p, addr).nodeName(); got != want {
			t.Errorf("node of %s = %q, want %q", addr, got, want)
		}
	}

	p.SetEndpointNode(a.Addr, " pve1 ")
	if got := endpointOf(t, p, a.Addr).nodeName(); got != "pve1" {
		t.Errorf("node of %s = %q, want the trimmed name", a.Addr, got)
	}
	p.SetEndpointNode(a.Addr, "pve5")
	if got := endpointOf(t, p, a.Addr).nodeName(); got != "pve5" {
		t.Errorf("node of %s = %q, want the later name to replace the earlier", a.Addr, got)
	}
	if got := endpointOf(t, p, b.Addr).nodeName(); got != "" {
		t.Errorf("node of %s = %q, want none", b.Addr, got)
	}
}

// A name SetEndpointNode records replaces one the endpoint was built with.
func TestSetEndpointNodeReplacesTheNameTheEndpointWasBuiltWith(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	p := l.pool(t, 0, Endpoint{Address: a.Addr, Kind: KindConfigured, Node: "pve1"}, configured(l.live(t, nil).Addr))
	p.SetEndpointNode(a.Addr, "pve7")
	req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/nodes/pve7/status", "")
	if !attributable(req.URL.Path, endpointOf(t, p, a.Addr)) {
		t.Error("a path naming the learned node is not attributable to the entry")
	}
	req = apiRequest(t, context.Background(), http.MethodGet, a.Addr, "/nodes/pve1/status", "")
	if attributable(req.URL.Path, endpointOf(t, p, a.Addr)) {
		t.Error("a path naming the replaced node is still attributable to the entry")
	}
}

// SetEndpointNode runs while walks read the entry's node for the attribution
// rule and the header deadline. Run under -race, the test fails when the name
// is a plain field that the setter and the walks share.
func TestSetEndpointNodeRacesNoWalk(t *testing.T) {
	l := newLab(t)
	a := l.live(t, nil)
	b := l.live(t, nil)
	p := l.pool(t, 0, configured(a.Addr), configured(b.Addr))

	const walkers, perWalker = 8, 25
	var stop atomic.Bool
	var setters sync.WaitGroup
	for _, node := range []string{"pve1", "pve2"} {
		setters.Add(1)
		go func() {
			defer setters.Done()
			for !stop.Load() {
				p.SetEndpointNode(a.Addr, node)
			}
		}()
	}
	var walks sync.WaitGroup
	failures := make(chan string, walkers*perWalker)
	for i := range walkers {
		walks.Add(1)
		go func() {
			defer walks.Done()
			path := "/nodes/pve1/status"
			if i%2 == 1 {
				path = "/nodes/pve2/status"
			}
			for range perWalker {
				req := apiRequest(t, context.Background(), http.MethodGet, a.Addr, path, "")
				resp, err := p.RoundTrip(req)
				if err != nil {
					failures <- err.Error()
					continue
				}
				if resp.StatusCode != http.StatusOK {
					failures <- resp.Status
				}
				closeBody(resp)
			}
		}()
	}
	walks.Wait()
	stop.Store(true)
	setters.Wait()
	close(failures)
	for f := range failures {
		t.Errorf("a walk failed while the node was set: %s", f)
	}
	if got := endpointOf(t, p, a.Addr).nodeName(); !strings.HasPrefix(got, "pve") {
		t.Errorf("node after the race = %q, want one of the names set", got)
	}
}
