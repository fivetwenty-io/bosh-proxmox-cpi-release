package pve

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// A request with less than one probe's time left may not start the cold start.
// Create_vm's first pool request, the VMID claim's create, runs on the create's
// own bound, which leaves it the time, so it starts the cold start. A request
// with a short context fails with the dial error it would have had before the
// cold start existed, sends nothing to a candidate, and leaves the cold start
// to the next request that has the time.
func TestColdStartWaitsForARequestWithTimeToProbe(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	c, obs := lab.process(t, cfg)

	short, cancel := context.WithTimeout(context.Background(), endpointpool.ProbeLimit-time.Second)
	defer cancel()
	_, _, err := c.Pools().GetPoolComment(short, "p1")

	mustFailToConnect(t, err)
	for _, n := range []*coldNode{pve2, pve3} {
		if got := n.srv.Counts.Conns.Load(); got != 0 || lab.dials(n.addr) != 0 {
			t.Errorf("%s saw %d connections after a request with a short context, want none", n.host, got)
		}
	}
	const refusal = "does not start the cold start from placement.az_map, because its context has less than " +
		"25s left"
	if lines := linesAt(obs.All(), log.LevelDebug, refusal); len(lines) != 1 {
		t.Errorf("Debug lines naming the refusal = %v, want one in %v", lines, obs.All())
	}

	_, _, err = c.Pools().GetPoolComment(context.Background(), "p1")

	if err != nil {
		t.Fatalf("the read with time to spare failed: %v", err)
	}
	if got, want := pve2.requests(), []string{"GET /cluster/status", "GET /pools/p1"}; !slices.Equal(got, want) {
		t.Errorf("pve2 read %v, want the probe and then the read, because the barred request left the cold start "+
			"for this one", got)
	}
	if n := pve3.srv.Counts.Requests.Load(); n != 0 {
		t.Errorf("pve3 read %d requests, want none, because pve2 was accepted", n)
	}
	pve2.tokenOnEveryRequest(t)
	drainProcessRegistry(t)
}

// A file whose discovered nodes are all past the 24 hour limit holds no usable
// node, so the pool carries none and the cold start stays possible. The gate
// tests count usable nodes with a number, and this one counts them through the
// load the client runs.
func TestColdStartRunsOverACacheWhoseNodesAreAllStale(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	seedEndpointCache(t, cfg, cacheSeed{
		nodes:   []cachedNode{{name: "pve4", online: true}, {name: "pve5", online: true}},
		nodeAge: endpointcache.NodeTTL + time.Hour,
	})
	c, obs := lab.process(t, cfg)

	mustCreate(t, c, pve2)

	probeThenCreate := []string{"GET /cluster/status", "POST /nodes/pve9/qemu"}
	if got := pve2.requests(); !slices.Equal(got, probeThenCreate) {
		t.Errorf("pve2 read %v, want the probe and the create", got)
	}
	for _, name := range []string{"pve4", "pve5"} {
		if n := lab.dials(name + "." + coldWireDomain + ":8006"); n != 0 {
			t.Errorf("the client dialed the stale node %s %d times, want none", name, n)
		}
	}
	const gated = "pve endpoints: no cold start from placement.az_map"
	if lines := linesAt(obs.All(), log.LevelDebug, gated); len(lines) != 0 {
		t.Errorf("the gate turned the cold start off over stale nodes: %v", lines)
	}
	drainProcessRegistry(t)
}

// A file that holds only a cold-start hint, which a first cold start leaves
// before any discovery has run, carries no discovered node and no outcome. The
// cold start must stay possible, and the hint must put its node first.
func TestColdStartRunsOverACacheThatHoldsOnlyAHint(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	seedEndpointCache(t, cfg, cacheSeed{hint: "pve3"})
	if payload := readSeededCacheNamed(t, cfg, coldWireNames); payload == nil || payload.ColdStart == nil ||
		payload.Discovery != nil {
		t.Fatalf("the seed is %+v, want a file with a hint and no discovery section", payload)
	}
	c, _ := lab.process(t, cfg)

	mustCreate(t, c, pve3)

	probeThenCreate := []string{"GET /cluster/status", "POST /nodes/pve9/qemu"}
	if got := pve3.requests(); !slices.Equal(got, probeThenCreate) {
		t.Errorf("pve3 read %v, want the probe and the create", got)
	}
	if got := pve2.srv.Counts.Requests.Load(); got != 0 {
		t.Errorf("pve2 read %d requests, want none, because the hint put pve3 first", got)
	}
	drainProcessRegistry(t)
}
