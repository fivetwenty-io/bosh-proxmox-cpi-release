package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

const (
	coldCreateDomain = "dc1.example.com"
	coldCreateToken  = "root@pam!cpi=0123456789abcdef"
)

// lockSentinels is the lock pools a fake PVE holds, keyed by pool id, with the
// comment each was created with.
type lockSentinels struct {
	mu       sync.Mutex
	comments map[string]string
}

// routes serves the pool routes the VMID claim uses: a create that stores the
// comment, a read that returns it, and a delete that removes it.
func (l *lockSentinels) routes(routes *pvetest.Routes) {
	reply := func(w http.ResponseWriter, status int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
	routes.HandleFunc(http.MethodPost, "/pools", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			reply(w, http.StatusBadRequest, nil)
			return
		}
		l.mu.Lock()
		l.comments[r.PostForm.Get("poolid")] = r.PostForm.Get("comment")
		l.mu.Unlock()
		reply(w, http.StatusOK, nil)
	})
	routes.HandleFunc(http.MethodGet, "/pools/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, pvetest.APIPrefix+"/pools/")
		l.mu.Lock()
		comment, ok := l.comments[id]
		l.mu.Unlock()
		if !ok {
			reply(w, http.StatusNotFound, nil)
			return
		}
		reply(w, http.StatusOK, map[string]any{"comment": comment, "members": []any{}})
	})
	routes.HandleFunc(http.MethodDelete, "/pools", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		delete(l.comments, r.URL.Query().Get("poolid"))
		l.mu.Unlock()
		reply(w, http.StatusOK, nil)
	})
}

// TestCreateVMColdStartsOnItsFirstRequestAndSendsTheRestToTheCandidate runs the
// real create_vm handler, with a production client for the pool and QEMU
// services, against a seed whose name does not resolve and one live node that
// placement.az_map names. The handler's first request to PVE is the VMID
// claim's create of its sentinel pool, which carries the create's own bound and
// so has the time the probe needs. That request starts the cold start, and it
// is the only one that does: the probe is the node's first request, the claim's
// create is its second, and every later request goes straight to the accepted
// node, with the token alone on each. The POST that creates the guest is among
// them. The hint is written for the next process.
func TestCreateVMColdStartsOnItsFirstRequestAndSendsTheRestToTheCandidate(t *testing.T) {
	pvetest.InstallNameServer(t, func(name string) bool { return strings.HasPrefix(name, "pve1.") })
	ca := pvetest.NewCA(t)
	node := "pve2." + coldCreateDomain
	locks := &lockSentinels{comments: map[string]string{}}
	routes := pvetest.NewRoutes()
	routes.JSON(http.MethodGet, "/cluster/status", http.StatusOK, []any{
		map[string]any{"type": "cluster", "name": "lab", "nodes": 2, "quorate": 1},
		map[string]any{"type": "node", "name": "pve1", "local": 0, "online": 1},
		map[string]any{"type": "node", "name": "pve2", "local": 1, "online": 1},
	})
	routes.JSON(http.MethodPost, "/nodes/pve/qemu", http.StatusOK, "UPID:pve2:0001:create")
	locks.routes(routes)
	srv := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{node}}), routes)
	_, port := srv.HostPort()

	verify := true
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clientCfg := &config.CPIConfig{
		Host:                 "pve1." + coldCreateDomain,
		Port:                 port,
		APIToken:             coldCreateToken,
		APIEndpointDiscovery: config.DiscoveryModeOn,
		VerifySSL:            &verify,
		PVECACertPEM:         string(ca.CertPEM()),
		EndpointCacheDir:     cacheDir,
		Placement:            &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1", "pve2"}}},
	}
	logger, observed := log.NewObservedLogger(log.LevelDebug)
	client, err := pve.NewClientWithTracer(clientCfg, logger, nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}

	q := &vmMockQEMU{}
	deps := buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	deps.PVE.(*mockPVEClient).poolsSvc = client.Pools()
	q.createFn = func(ctx context.Context, node string, params map[string]any) (string, error) {
		return client.QEMU().Create(ctx, node, params)
	}
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	defer release()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	_, err = handlers.HandleCreateVM(deps).Handle(ctx,
		mkArgs("ag-1", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("cold-start-create"))
	if err != nil {
		t.Fatalf("create_vm: %v\nlog: %v", err, observed.All())
	}

	seen := srv.Seen()
	requests := make([]string, 0, len(seen))
	for _, r := range seen {
		requests = append(requests, r.Method+" "+strings.TrimPrefix(r.Path, pvetest.APIPrefix))
		want := []string{"PVEAPIToken=" + coldCreateToken}
		if got := r.Header.Values("Authorization"); !slices.Equal(got, want) {
			t.Errorf("%s %s carried Authorization %v, want the token alone", r.Method, r.Path, got)
		}
	}
	if len(requests) < 3 || requests[0] != "GET /cluster/status" || requests[1] != "POST /pools" {
		t.Fatalf("the node read %v, want the probe first and then the claim's create", requests)
	}
	if n := srv.Counts.Method(http.MethodGet); n < 2 {
		t.Errorf("the node read %d GETs, want the probe and the claim's confirming read", n)
	}
	probes := 0
	for _, r := range requests {
		if r == "GET /cluster/status" {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("the node read %d probes, want one cold start for the process: %v", probes, requests)
	}
	if !slices.Contains(requests, "POST /nodes/pve/qemu") {
		t.Errorf("the guest's create never reached the accepted node: %v", requests)
	}
	if !slices.Contains(requests, "DELETE /pools") {
		t.Errorf("the claim was never released on the accepted node: %v", requests)
	}
	locks.mu.Lock()
	left := len(locks.comments)
	locks.mu.Unlock()
	if left != 0 {
		t.Errorf("%d lock sentinels stand after create_vm returned", left)
	}

	endpointcache.Drain(context.Background(), 30*time.Second)
	endpointcache.Open()
	files, err := filepath.Glob(filepath.Join(cacheDir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Errorf("cache files = %v (%v), want the one that holds the cold_start hint and the seed's mark", files, err)
	}
}
