package pve

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// nodesTestKey is a key with two configured endpoints.
func nodesTestKey() endpointcache.Key {
	return endpointcache.Key{Endpoints: []string{workerSeed, workerSecondEndpoint}}
}

func TestStatusEndpointNodesPairsTheLocalNodeWithTheEndpointThatAnswered(t *testing.T) {
	t.Parallel()
	key := nodesTestKey()
	answered := DiscoveryReads{
		Status: DiscoveryStatus{Available: true, Local: "pve1"}, StatusVia: workerSecondEndpoint,
	}
	want := map[string]string{workerSecondEndpoint: "pve1"}
	if got := statusEndpointNodes(key, answered); !reflect.DeepEqual(got, want) {
		t.Errorf("statusEndpointNodes = %v, want %v", got, want)
	}
	// The key's spelling wins over the pool's.
	upper := answered
	upper.StatusVia = "PVE9.dc1.example.com:8006"
	if got := statusEndpointNodes(key, upper); !reflect.DeepEqual(got, want) {
		t.Errorf("a differently spelled answering endpoint gave %v", got)
	}
	for name, reads := range map[string]DiscoveryReads{
		"status not read":       {Status: DiscoveryStatus{Local: "pve1"}, StatusVia: workerSeed},
		"no local node":         {Status: DiscoveryStatus{Available: true}, StatusVia: workerSeed},
		"no answering endpoint": {Status: DiscoveryStatus{Available: true, Local: "pve1"}},
		"answered elsewhere": {
			Status: DiscoveryStatus{Available: true, Local: "pve1"}, StatusVia: "other.example.com:8006",
		},
		"a dotted name":  {Status: DiscoveryStatus{Available: true, Local: "pve1.example.com"}, StatusVia: workerSeed},
		"a numeric name": {Status: DiscoveryStatus{Available: true, Local: "1234"}, StatusVia: workerSeed},
		"a spaced name":  {Status: DiscoveryStatus{Available: true, Local: "pve 1"}, StatusVia: workerSeed},
	} {
		if got := statusEndpointNodes(key, reads); len(got) != 0 {
			t.Errorf("%s: statusEndpointNodes = %v, want none", name, got)
		}
	}
}

func TestDiscoveryWorker_LearnEndpointNodesHandsThePoolTheNameAndSaysSoAtDebug(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{endpoints: []string{workerSeed, workerSecondEndpoint}})
	w := e.newWorker()
	reads := DiscoveryReads{Status: DiscoveryStatus{Available: true, Local: "pve1"}, StatusVia: workerSecondEndpoint}
	e.begin()
	got := w.learnEndpointNodes(context.Background(), DiscoveryBinding{Pool: e.pool}, reads)
	want := map[string]string{workerSecondEndpoint: "pve1"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(e.pool.learned(), want) {
		t.Errorf("returned %v and handed the pool %v, want %v", got, e.pool.learned(), want)
	}
	entry := e.wantLine("configured endpoint "+workerSecondEndpoint+" serves node pve1", log.LevelDebug)
	if entry.Attrs["endpoint"] != workerSecondEndpoint || entry.Attrs["node"] != "pve1" {
		t.Errorf("attrs %v", entry.Attrs)
	}
}

func TestDiscoveryWorker_LearnEndpointNodesLeavesOutANameTheCacheWouldRefuse(t *testing.T) {
	t.Parallel()
	const refused = "pve1.attacker.example"
	e := newWorkerEnv(t, workerEnvOptions{endpoints: []string{workerSeed, workerSecondEndpoint}})
	w := e.newWorker()
	e.begin()
	reads := DiscoveryReads{Status: DiscoveryStatus{Available: true, Local: refused}, StatusVia: workerSeed}
	if got := w.learnEndpointNodes(context.Background(), DiscoveryBinding{Pool: e.pool}, reads); len(got) != 0 {
		t.Errorf("returned %v, want none", got)
	}
	if learned := e.pool.learned(); len(learned) != 0 {
		t.Errorf("handed the pool %v, want none", learned)
	}
	entry := e.wantLine("is not a usable node name", log.LevelDebug)
	if strings.Contains(entry.Message, refused) {
		t.Errorf("the line echoes the refused name: %q", entry.Message)
	}
	for _, v := range entry.Attrs {
		if strings.Contains(fmt.Sprint(v), refused) {
			t.Errorf("the line's attrs echo the refused name: %v", entry.Attrs)
		}
	}
	// A status that names no endpoint or no node says nothing at all.
	e.begin()
	for _, quiet := range []DiscoveryReads{
		{Status: DiscoveryStatus{Available: true, Local: "pve1"}},
		{Status: DiscoveryStatus{Available: true}, StatusVia: workerSeed},
		{StatusVia: workerSeed},
	} {
		w.learnEndpointNodes(context.Background(), DiscoveryBinding{Pool: e.pool}, quiet)
	}
	if lines := e.lines(); len(lines) != 0 || len(e.pool.learned()) != 0 {
		t.Errorf("lines %v and names %v, want neither", lines, e.pool.learned())
	}
}

// The status read of the refresh goes through a real pool, so the endpoint that
// answers it is the one the walk chose. The first endpoint goes down after the
// node list, which moves the status read to the second endpoint, and the local
// node it named belongs to that endpoint.
func TestDiscoveryWorker_RecordsTheLocalNodeAgainstTheEndpointThatAnsweredTheStatusRead(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{
		verifySSL: true, endpoints: []string{workerSeed, workerSecondEndpoint},
	})
	f := newFailoverReads(t)
	e.client = f.client(t, e, map[string]func(){"nodes": f.takeDown})
	e.run()
	f.mustStayDown(t)

	want := map[string]string{workerSecondEndpoint: "pve1"}
	p := e.payload()
	if p.Cluster == nil || !reflect.DeepEqual(p.Cluster.EndpointNodes, want) {
		t.Fatalf("cluster %+v, want endpoint_nodes %v", p.Cluster, want)
	}
	if got := e.pool.learned(); !reflect.DeepEqual(got, want) {
		t.Errorf("the pool learned %v, want %v", got, want)
	}
}

func TestDiscoveryWorker_RecordsTheLocalNodeAgainstTheFirstEndpointWhenItAnswers(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{
		verifySSL: true, endpoints: []string{workerSeed, workerSecondEndpoint},
	})
	e.client = newFailoverReads(t).client(t, e, nil)
	e.run()

	want := map[string]string{workerSeed: "pve1"}
	if p := e.payload(); p.Cluster == nil || !reflect.DeepEqual(p.Cluster.EndpointNodes, want) {
		t.Fatalf("cluster %+v, want endpoint_nodes %v", p.Cluster, want)
	}
	// A later refresh through the other endpoint adds its name, and the first
	// endpoint's stays.
	e.clock.advance(endpointcache.RefreshInterval)
	e.cluster.setNodes("pve9", []string{"pve1", "pve9"})
	f := newFailoverReads(t)
	f.takeDown()
	e.client = f.client(t, e, nil)
	e.run()
	f.mustStayDown(t)
	want = map[string]string{workerSeed: "pve1", workerSecondEndpoint: "pve9"}
	if p := e.payload(); p.Cluster == nil || !reflect.DeepEqual(p.Cluster.EndpointNodes, want) {
		t.Fatalf("after a refresh through the second endpoint: cluster %+v, want endpoint_nodes %v", p.Cluster, want)
	}
}

// A client that reports no answering endpoint, as one without a pool does,
// leaves the refresh to record the nodes and no endpoint's name.
func TestDiscoveryWorker_RecordsNoNodeNameWhenTheClientReportsNoAnsweringEndpoint(t *testing.T) {
	t.Parallel()
	e := newWorkerEnv(t, workerEnvOptions{verifySSL: true})
	e.run()

	p := e.payload()
	if p == nil || p.Cluster == nil || p.Discovery == nil || len(p.Discovery.Nodes) == 0 {
		t.Fatalf("payload %+v, want the discovered nodes", p)
	}
	if len(p.Cluster.EndpointNodes) != 0 || len(e.pool.learned()) != 0 {
		t.Errorf("endpoint_nodes %v and pool names %v, want none", p.Cluster.EndpointNodes, e.pool.learned())
	}
}

func TestNameConfiguredEndpointsAppliesTheCachedNames(t *testing.T) {
	t.Parallel()
	plan := func() *endpointPlan {
		return &endpointPlan{
			endpoints: []endpointpool.Endpoint{
				{Address: workerSeed, Kind: endpointpool.KindConfigured},
				{Address: "PVE9.dc1.example.com:8006", Kind: endpointpool.KindConfigured, Node: "pve9-test"},
				{Address: "pve5.dc1.example.com:8006", Kind: endpointpool.KindConfigured},
				{Address: "pve6.dc1.example.com:8006", Kind: endpointpool.KindDiscovered},
			},
			configured: 3,
		}
	}
	nodes := func(p *endpointPlan) []string {
		out := make([]string, len(p.endpoints))
		for i, ep := range p.endpoints {
			out[i] = ep.Node
		}
		return out
	}
	cluster := &endpointcache.ClusterIdentity{EndpointNodes: map[string]string{
		workerSeed:                  "pve1",
		workerSecondEndpoint:        "pve2",
		"pve6.dc1.example.com:8006": "pve6",
	}}

	p := plan()
	nameConfiguredEndpoints(p, &endpointcache.Payload{Cluster: cluster})
	// A name already on the endpoint stays, a discovered entry takes none, and
	// an endpoint with no stored name stays unnamed.
	if got, want := nodes(p), []string{"pve1", "pve9-test", "", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("nodes after the load = %q, want %q", got, want)
	}

	for name, payload := range map[string]*endpointcache.Payload{
		"no cluster section": {},
		"a standing conflict": {Cluster: &endpointcache.ClusterIdentity{
			IdentityConflict: true, EndpointNodes: cluster.EndpointNodes,
		}},
		"a section without names": {Cluster: &endpointcache.ClusterIdentity{}},
	} {
		p := plan()
		nameConfiguredEndpoints(p, payload)
		if got, want := nodes(p), []string{"", "pve9-test", "", ""}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: nodes = %q, want %q", name, got, want)
		}
	}
}
