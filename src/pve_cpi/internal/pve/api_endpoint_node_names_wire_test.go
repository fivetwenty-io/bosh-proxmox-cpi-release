package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file run the production discovery worker in one client and
// build a second client over the file it wrote. No test seam names an endpoint,
// so a second client attributes a node-scoped request to the endpoint only
// through the name the worker read from /cluster/status and the cache carried.
// They drain the process registry, so none runs in parallel.

const (
	nodeNamesLocal = "pve1"
	nodeNamesOther = "pve2"
)

// nodeNamesRoutes answers the reads the first process sends, the way a
// standalone node whose local name is local does, and answers every other
// request under the node's own path with failure, and under another node's
// path with failure too, when failure is not zero. The pool read stands for the
// request that triggers the worker.
func nodeNamesRoutes(t *testing.T, lab *tetherLab, local string, failure int) *pvetest.Routes {
	t.Helper()
	routes := pvetest.NewRoutes()
	routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	routes.JSON(http.MethodGet, "/cluster/status", http.StatusOK, []any{
		map[string]any{"type": "node", "name": local, "local": 1, "online": 1},
	})
	routes.JSON(http.MethodGet, "/nodes", http.StatusOK, []any{
		map[string]any{"node": local, "status": "online"},
	})
	routes.JSON(http.MethodGet, "/nodes/"+local+"/certificates/info", http.StatusOK, []any{
		map[string]any{"filename": "pve-root-ca.pem", "pem": string(lab.ca.CertPEM())},
	})
	if failure != 0 {
		for _, node := range []string{nodeNamesLocal, nodeNamesOther} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				routes.Fail(method, "/nodes/"+node+"/", failure, "backend trouble")
			}
		}
	}
	return routes
}

// nodeNamesBackup answers every node-scoped request the second process sends.
func nodeNamesBackup() *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	routes.JSON(http.MethodGet, "/nodes/"+nodeNamesLocal+"/storage/local/content", http.StatusOK, []any{})
	routes.JSON(http.MethodGet, "/nodes/"+nodeNamesOther+"/storage/local/content", http.StatusOK, []any{})
	routes.JSON(http.MethodPost, "/nodes/"+nodeNamesLocal+"/qemu", http.StatusOK, "UPID:pve1:0001:create")
	return routes
}

func TestAConfiguredEndpointsNodeNameReachesTheNextProcessThroughTheCache(t *testing.T) {
	lab := newTetherLab(t)
	a := lab.live(t, nodeNamesRoutes(t, lab, nodeNamesLocal, http.StatusServiceUnavailable))
	b := lab.live(t, nodeNamesBackup())
	cfg := lab.config(a, b)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)

	first := lab.clientFor(t, cfg)
	readPoolComment(t, first)
	drainProcessRegistry(t)

	written := readSeededCache(t, cfg)
	if written.Cluster == nil || len(written.Cluster.EndpointNodes) != 1 ||
		written.Cluster.EndpointNodes[a.Addr] != nodeNamesLocal {
		t.Fatalf("the cache holds endpoint_nodes %+v, want %s to serve %s", written.Cluster, a.Addr, nodeNamesLocal)
	}

	// The second process runs no discovery, so the name can only come from the
	// file. The first endpoint answers 503 for its own node, which is its own
	// trouble, and the read moves on.
	second := lab.clientFor(t, cfg)
	if _, err := second.Nodes().ListStorageContent(context.Background(), nodeNamesLocal, "local", nil); err != nil {
		t.Fatalf("ListStorageContent: %v", err)
	}
	if n := seenCount(a, http.MethodGet, "/nodes/"+nodeNamesLocal+"/storage/local/content"); n != 1 {
		t.Errorf("the first endpoint read %d of the node-scoped GET, want the one it failed", n)
	}
	if n := seenCount(b, http.MethodGet, "/nodes/"+nodeNamesLocal+"/storage/local/content"); n != 1 {
		t.Errorf("the second endpoint read %d of the node-scoped GET, want the one that hopped to it", n)
	}
	drainProcessRegistry(t)
}

func TestTheEndpointThatAnsweredTheStatusReadGetsTheLocalNode(t *testing.T) {
	lab := newTetherLab(t)
	b := lab.live(t, nodeNamesRoutes(t, lab, nodeNamesOther, 0))
	dead := deadWireEndpoint(t)
	cfg := lab.config(b)
	cfg.Host, cfg.Port = dead.host, dead.port
	cfg.Endpoints = []string{dead.address(), b.Addr}
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)

	first := lab.clientFor(t, cfg)
	readPoolComment(t, first)
	drainProcessRegistry(t)

	// The first endpoint refuses every connection, so the second answered the
	// status read, and the local node it named belongs to the second.
	written := readSeededCache(t, cfg)
	if written.Cluster == nil || len(written.Cluster.EndpointNodes) != 1 ||
		written.Cluster.EndpointNodes[b.Addr] != nodeNamesOther {
		t.Fatalf("the cache holds %+v, want only %s to serve %s", written.Cluster, b.Addr, nodeNamesOther)
	}
	if _, ok := written.Cluster.EndpointNodes[dead.address()]; ok {
		t.Errorf("the endpoint that never answered was given a node: %v", written.Cluster.EndpointNodes)
	}
}

func TestA595IsTheEndpointsTroubleOnlyForAReadOfItsOwnNode(t *testing.T) {
	lab := newTetherLab(t)
	a := lab.live(t, nodeNamesRoutes(t, lab, nodeNamesLocal, 595))
	b := lab.live(t, nodeNamesBackup())
	cfg := lab.config(a, b)
	cfg.APIEndpointDiscovery = config.DiscoveryModeOn
	cfg.EndpointCacheDir = newCacheDir(t)
	first := lab.clientFor(t, cfg)
	readPoolComment(t, first)
	drainProcessRegistry(t)
	written := readSeededCache(t, cfg)
	if written.Cluster == nil || written.Cluster.EndpointNodes[a.Addr] != nodeNamesLocal {
		t.Fatalf("the cache holds %+v, want %s to serve %s", written.Cluster, a.Addr, nodeNamesLocal)
	}

	content := func(node string) string { return "/nodes/" + node + "/storage/local/content" }
	t.Run("a read of the endpoint's own node hops", func(t *testing.T) {
		c := lab.clientFor(t, cfg)
		if _, err := c.Nodes().ListStorageContent(context.Background(), nodeNamesLocal, "local", nil); err != nil {
			t.Fatalf("ListStorageContent: %v", err)
		}
		if n := seenCount(b, http.MethodGet, content(nodeNamesLocal)); n != 1 {
			t.Errorf("the second endpoint read %d of the GET, want the one that hopped to it", n)
		}
	})
	t.Run("a read of another node does not hop", func(t *testing.T) {
		c := lab.clientFor(t, cfg)
		before := seenCount(b, http.MethodGet, content(nodeNamesOther))
		_, err := c.Nodes().ListStorageContent(context.Background(), nodeNamesOther, "local", nil)
		if err == nil {
			t.Fatal("ListStorageContent: nil, want the 595 the first endpoint relayed")
		}
		if n := seenCount(b, http.MethodGet, content(nodeNamesOther)) - before; n != 0 {
			t.Errorf("the second endpoint read %d of a GET for another node, want none", n)
		}
		if n := seenCount(a, http.MethodGet, content(nodeNamesOther)); n != 1 {
			t.Errorf("the first endpoint read %d of the GET, want the one it answered 595", n)
		}
	})
	t.Run("a mutation on the endpoint's own node does not hop", func(t *testing.T) {
		c := lab.clientFor(t, cfg)
		before := seenCount(b, http.MethodPost, "/nodes/"+nodeNamesLocal+"/qemu")
		_, err := c.QEMU().Create(context.Background(), nodeNamesLocal, map[string]interface{}{"vmid": 4242})
		if err == nil {
			t.Fatal("Create: nil, want the 595 the first endpoint relayed")
		}
		if n := seenCount(b, http.MethodPost, "/nodes/"+nodeNamesLocal+"/qemu") - before; n != 0 {
			t.Errorf("the second endpoint read %d of the POST, want none, because a mutation is not replayed", n)
		}
		if n := seenCount(a, http.MethodPost, "/nodes/"+nodeNamesLocal+"/qemu"); n != 1 {
			t.Errorf("the first endpoint read %d of the POST, want the one it answered 595", n)
		}
	})
	drainProcessRegistry(t)
}
