package pve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
// the failover pool. The SDK's transport refuses the redirect answer above the
// pool and above the wrapper a client without the pool gets, so the request is
// sent once, is not retried, and the CPI's own refusal never sees a follow-up.

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
	// The Location carries user information, a query, and a fragment, none
	// of which may reach the error.
	location := scheme + "://" + redirectUserInfo + "@" + otherAddr + pvetest.APIPrefix + "/version?ticket=" +
		redirectQuerySecret + "#" + redirectFragment

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

	// The SDK may retry the GET three times, so one request on the node shows
	// that the refusal stopped its retry loop.
	ctx := sdkclient.WithRetryDelay(sdkclient.WithRetries(context.Background(), 3), time.Millisecond)
	_, _, err = c.Pools().GetPoolComment(ctx, "p1")
	mustBeTheSDKRefusal(t, "GET", err)
	_, err = c.QEMU().Create(ctx, "pve9", map[string]interface{}{"vmid": coldWireVMID})
	mustBeTheSDKRefusal(t, "POST", err)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if n := redirectLegs(node, method); n != 1 {
			t.Errorf("the node read %d %s requests on the redirecting route, want 1", n, method)
		}
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

// The parts of a redirect's Location that must not reach an error.
const (
	redirectUserInfo    = "intruder:hunter2"
	redirectQuerySecret = "SECRET-TICKET-0451"
	redirectFragment    = "fragment-0451"
)

// mustBeTheSDKRefusal checks that err is the SDK's refusal of a redirect
// answer, marked permanent, that the CPI's own refusal did not fire because no
// follow-up request was built, and that the error holds no secret.
func mustBeTheSDKRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, sdkclient.ErrRedirectRefused) || !IsRedirectRefused(err) {
		t.Errorf("%s: err = %v, want the SDK's ErrRedirectRefused", what, err)
		return
	}
	if !sdkclient.IsPermanent(err) {
		t.Errorf("%s: err = %v, want the permanent marker", what, err)
	}
	if errors.Is(err, endpointpool.ErrRedirectRefused) {
		t.Errorf("%s: err = %v, want the SDK's transport to refuse before a follow-up reaches the CPI", what, err)
	}
	mustHoldNoRedirectSecret(t, what, err)
}

// mustHoldNoRedirectSecret checks that err names no part of the token or of
// the Location that could carry a credential.
func mustHoldNoRedirectSecret(t *testing.T, what string, err error) {
	t.Helper()
	text := err.Error()
	_, tokenSecret, _ := strings.Cut(coldWireToken, "=")
	for _, secret := range []string{tokenSecret, "hunter2", redirectUserInfo, redirectQuerySecret, redirectFragment,
		"ticket="} {
		if strings.Contains(text, secret) {
			t.Errorf("%s: the error holds %q: %s", what, secret, text)
		}
	}
}

// redirectLegs counts the requests a node read on the two routes the redirect
// tests send for method.
func redirectLegs(node *pvetest.Server, method string) int {
	path := pvetest.APIPrefix + "/pools/p1"
	if method == http.MethodPost {
		path = pvetest.APIPrefix + "/nodes/pve9/qemu"
	}
	n := 0
	for _, r := range node.Seen() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}
