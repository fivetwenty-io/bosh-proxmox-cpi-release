package pve

import (
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// TestDiscoveryWriteIsTheNextProcessesDiscoveredEndpoint runs the production
// discovery worker in one client and builds a second client over the file it
// wrote, with the seed gone. Neither side is a hand-built fixture. The first
// process answers one read, so its worker reads the cluster and writes pve2
// with its pin to the cache, and the exit drain waits for the write. The second
// process loads that file, puts pve2 behind the configured seed in its pool,
// and sends its read to pve2 over the pin and the name the worker recorded. A
// change to what the worker writes (the address, the port, the pins, or the
// confirmation time) or to what the load accepts fails here, where each side's
// own tests would still pass on their own fixtures.
func TestDiscoveryWriteIsTheNextProcessesDiscoveredEndpoint(t *testing.T) {
	pvetest.InstallNameServer(t, nil)
	lab := newColdLab(t)
	cfg := lab.config(t)
	// No az_map, so a read that reaches pve2 can only come from the cache and
	// never from the cold start.
	cfg.Placement = nil
	pve2Leaf := lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve2." + coldWireDomain}})
	pve2Routes := pvetest.NewRoutes()
	pve2Routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	pve2 := pvetest.NewLive(t, pve2Leaf, pve2Routes)
	lab.route("pve2."+coldWireDomain, pve2)

	pve1Routes := pvetest.NewRoutes()
	pve1Routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	pve1Routes.JSON(http.MethodGet, "/cluster/status", http.StatusOK, []any{
		map[string]any{"type": "cluster", "name": "lab", "nodes": 2, "quorate": 1},
		map[string]any{"type": "node", "name": "pve1", "local": 1, "online": 1},
		map[string]any{"type": "node", "name": "pve2", "local": 0, "online": 1},
	})
	pve1Routes.JSON(http.MethodGet, "/nodes", http.StatusOK, []any{
		map[string]any{"node": "pve1", "status": "online"},
		map[string]any{"node": "pve2", "status": "online", "ssl_fingerprint": pve2Leaf.FingerprintColon()},
	})
	pve1Routes.JSON(http.MethodGet, "/nodes/pve2/certificates/info", http.StatusOK, []any{
		map[string]any{"filename": "pveproxy-ssl.pem", "fingerprint": pve2Leaf.FingerprintColon()},
	})
	pve1Routes.JSON(http.MethodGet, "/nodes/pve1/certificates/info", http.StatusOK, []any{
		map[string]any{"filename": "pve-root-ca.pem", "pem": string(lab.ca.CertPEM())},
	})
	pve1Leaf := lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve1." + coldWireDomain}})
	pve1 := pvetest.NewLive(t, pve1Leaf, pve1Routes)
	seed := lab.route("pve1."+coldWireDomain, pve1)

	first, _ := lab.process(t, cfg)
	readPoolComment(t, first)
	drainProcessRegistry(t)

	written := readSeededCache(t, cfg)
	if got := cachedNodeNames(written); !slices.Equal(got, []string{"pve2"}) {
		t.Fatalf("the first process cached nodes %v, want pve2 alone, because pve1 is the configured seed", got)
	}
	node := written.Discovery.Nodes[0]
	if node.Address != "pve2."+coldWireDomain || node.Port != coldWirePort ||
		!slices.Equal(node.Pins, []string{pve2Leaf.Fingerprint()}) {
		t.Fatalf("cached node = %+v, want pve2.%s:%d with the pin of its certificate",
			node, coldWireDomain, coldWirePort)
	}
	pve1Reads := pve1.Counts.Requests.Load()
	if n := pve2.Counts.Requests.Load(); n != 0 {
		t.Fatalf("pve2 read %d requests in the first process, want none, because discovery reads configured nodes only",
			n)
	}

	lab.unroute(seed)
	second, secondLog := lab.process(t, cfg)
	readPoolComment(t, second)
	drainProcessRegistry(t)

	if got := seedAndDiscovered(secondLog.All()); got != "pve1."+coldWireDomain+":8006 configured, pve2."+
		coldWireDomain+":8006 discovered" {
		t.Errorf("the second process built the pool %q, want the seed and then the cached node", got)
	}
	if n := seenCount(pve2, http.MethodGet, "/pools/p1"); n != 1 {
		t.Errorf("pve2 read %d of the second process's reads, want the one that failed over to it", n)
	}
	if n := pve1.Counts.Requests.Load(); n != pve1Reads {
		t.Errorf("pve1 read %d more requests after it was gone", n-pve1Reads)
	}
	if len(linesAt(secondLog.All(), log.LevelWarn, "request sent to pve2."+coldWireDomain+":8006")) != 1 {
		t.Errorf("the second process's lines: %v, want one failover Warn that names pve2", secondLog.All())
	}
}

// seedAndDiscovered returns the endpoint list of the pool-built line.
func seedAndDiscovered(entries []log.Entry) string {
	const open, closeText = "pve endpoints: pool [", "], starting on"
	for _, e := range entries {
		msg := e.Message
		start := strings.Index(msg, open)
		end := strings.Index(msg, closeText)
		if start >= 0 && end > start {
			return msg[start+len(open) : end]
		}
	}
	return ""
}

// route sends the client's dials of host:8006 to srv and returns the address.
func (l *coldLab) route(host string, srv *pvetest.Server) string {
	address := net.JoinHostPort(host, strconv.Itoa(coldWirePort))
	l.mu.Lock()
	l.targets[address] = srv.Addr
	l.mu.Unlock()
	return address
}

// unroute makes address refuse connections, which stands for a host that went
// down.
func (l *coldLab) unroute(address string) {
	l.mu.Lock()
	delete(l.targets, address)
	l.mu.Unlock()
}
