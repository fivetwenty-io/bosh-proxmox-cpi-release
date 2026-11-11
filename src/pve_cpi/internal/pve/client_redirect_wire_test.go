package pve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// Go's redirect policy keeps the Authorization header when a redirect changes
// only the port or the scheme of the same host. These tests build the client
// through newClient, point a node at another port of its own host, and prove
// the token never reaches that listener, whether or not the client installs
// the failover pool.

// redirectOtherPort is the port the redirect names.
const redirectOtherPort = 8443

// listenerLog counts what a plain listener reads and keeps each Authorization.
type listenerLog struct {
	requests atomic.Int32
	auth     atomic.Value
}

func (l *listenerLog) note(r *http.Request) {
	l.requests.Add(1)
	l.auth.Store(r.Header.Get("Authorization"))
}

func (l *listenerLog) authorization() string {
	auth, _ := l.auth.Load().(string)
	return auth
}

func TestClientNeverFollowsARedirectToAnotherPortOfTheHost(t *testing.T) {
	cases := []struct {
		name string
		mode config.DiscoveryMode
		// endpoints lists pve.endpoints, and none means one host.
		endpoints []string
		// pool says the plan installs the failover pool.
		pool bool
	}{
		{"one host, discovery off, no pool", config.DiscoveryModeOff, nil, false},
		{"two endpoints, discovery off, a pool", config.DiscoveryModeOff,
			[]string{"pve1." + coldWireDomain + ":8006", "pve2." + coldWireDomain + ":8006"}, true},
		{"one host, discovery on, a pool", config.DiscoveryModeOn, nil, true},
	}
	targets := []string{"plain http", "https"}
	for _, tc := range cases {
		for _, scheme := range targets {
			for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
				name := fmt.Sprintf("%s, %d to %s", tc.name, status, scheme)
				t.Run(name, func(t *testing.T) {
					redirectScenario(t, tc.mode, tc.endpoints, tc.pool, scheme == "https", status)
				})
			}
		}
	}
}

func redirectScenario(t *testing.T, mode config.DiscoveryMode, endpoints []string, pool, tlsTarget bool, status int) {
	t.Helper()
	lab := newColdLab(t)
	host := "pve1." + coldWireDomain
	otherAddr := net.JoinHostPort(host, strconv.Itoa(redirectOtherPort))
	scheme := "http"
	if tlsTarget {
		scheme = "https"
	}
	location := scheme + "://" + otherAddr + pvetest.APIPrefix + "/version"

	// The node answers the two routes the test sends with a redirect.
	routes := pvetest.NewRoutes()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/pools/p1"
		if method == http.MethodPost {
			path = "/nodes/pve9/qemu"
		}
		routes.HandleFunc(method, path, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, status)
		})
	}
	node := pvetest.NewLive(t, lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{host}}), routes)
	lab.mu.Lock()
	lab.targets[net.JoinHostPort(host, strconv.Itoa(coldWirePort))] = node.Addr
	lab.mu.Unlock()
	if len(endpoints) > 1 {
		leaf := lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"pve2." + coldWireDomain}})
		second := pvetest.NewLive(t, leaf, nil)
		lab.mu.Lock()
		lab.targets[endpoints[1]] = second.Addr
		lab.mu.Unlock()
	}

	// The other listener of the same host is a plain HTTP server or a second
	// TLS server that trusts the same authority.
	var plainLog listenerLog
	var tlsOther *pvetest.Server
	if tlsTarget {
		tlsOther = pvetest.NewLive(t, lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{host}}), nil)
		lab.mu.Lock()
		lab.targets[otherAddr] = tlsOther.Addr
		lab.mu.Unlock()
	} else {
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plainLog.note(r)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(plain.Close)
		lab.mu.Lock()
		lab.targets[otherAddr] = plain.Listener.Addr().String()
		lab.mu.Unlock()
	}

	cfg := lab.config(t)
	cfg.Placement = nil
	cfg.Endpoints = endpoints
	cfg.APIEndpointDiscovery = mode
	if _, wiring, err := wireEndpoints(cfg); err != nil || wiring.plan.install() != pool {
		t.Fatalf("the plan installs the pool = %v (err %v), want %v", wiring != nil && wiring.plan.install(), err, pool)
	}
	logger, _ := log.NewObservedLogger(log.LevelDebug)
	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	ctx := sdkclient.WithRetryDelay(sdkclient.WithRetries(context.Background(), 1), time.Millisecond)
	if _, _, err := c.Pools().GetPoolComment(ctx, "p1"); !errors.Is(err, endpointpool.ErrRedirectRefused) {
		t.Errorf("GET: err = %v, want ErrRedirectRefused", err)
	}
	_, err = c.QEMU().Create(ctx, "pve9", map[string]interface{}{"vmid": coldWireVMID})
	if !errors.Is(err, endpointpool.ErrRedirectRefused) {
		t.Errorf("POST: err = %v, want ErrRedirectRefused", err)
	}
	if n := lab.dials(otherAddr); n != 0 {
		t.Errorf("the client dialed %s %d times, want none", otherAddr, n)
	}
	if n := plainLog.requests.Load(); n != 0 {
		t.Errorf("the plain listener read %d requests, want none (Authorization %q)", n, plainLog.authorization())
	}
	if tlsOther != nil {
		for _, r := range tlsOther.Seen() {
			t.Errorf("the second TLS listener read %s %s with Authorization %q", r.Method, r.Path,
				r.Header.Get("Authorization"))
		}
	}
	drainProcessRegistry(t)
}
