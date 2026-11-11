package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// These tests send a redirect through the SDK's real client with the CPI's
// redirect refusal underneath it, once through the failover pool and once
// through the wrapper a client without the pool gets. They prove that each
// refusal ends the request after one attempt, that no retry loop of the SDK or
// of the CPI sends it again, and that no secret reaches the error.

// redirectStack names the transport the CPI puts under the SDK's client.
type redirectStack string

const (
	stackPool   redirectStack = "pool"
	stackNoPool redirectStack = "no pool"
)

// redirectRig is an SDK client whose node answers with a redirect to a plain
// listener on another port.
type redirectRig struct {
	api sdkclient.Client
	// node is the endpoint that answers with the redirect.
	node *pvetest.Server
	// second is the pool's other endpoint, nil without the pool.
	second *pvetest.Server
	// trips counts the requests the SDK handed to the CPI's transport.
	trips atomic.Int32
	// target counts the requests the redirect's target read.
	target atomic.Int32
}

// redirectRigOptions changes how a rig is built.
type redirectRigOptions struct {
	stack redirectStack
	// policy is the SDK's CheckRedirect, nil for the SDK's default refusal.
	policy func(req *http.Request, via []*http.Request) error
	// password makes the client log in with a ticket, whose login the node
	// answers with the redirect.
	password bool
}

// routeRedirects answers each of the rig's routes with a redirect to location.
func routeRedirects(location string) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	answer := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, location, status) }
	}
	routes.HandleFunc(http.MethodGet, "/pools/p1", answer(http.StatusFound))
	routes.HandleFunc(http.MethodPost, "/nodes/pve9/qemu", answer(http.StatusTemporaryRedirect))
	routes.HandleFunc(http.MethodPost, "/access/ticket", answer(http.StatusFound))
	return routes
}

func newRedirectRig(t *testing.T, o redirectRigOptions) *redirectRig {
	t.Helper()
	rig := &redirectRig{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rig.target.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	location := "http://" + redirectUserInfo + "@" + target.Listener.Addr().String() + pvetest.APIPrefix +
		"/version?ticket=" + redirectQuerySecret + "#" + redirectFragment

	ca := pvetest.NewCA(t)
	rig.node = pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), routeRedirects(location))
	if o.stack == stackPool {
		rig.second = pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	}

	var (
		pool    *endpointpool.Pool
		poolErr error
	)
	host, port := rig.node.HostPort()
	opts := sdkclient.Options{
		Host:          host,
		Port:          port,
		Protocol:      sdkclient.ProtocolHTTPS,
		Timeout:       10 * time.Second,
		SSLOptions:    &sdkclient.SSLOptions{VerifyMode: sdkclient.SSLVerifyNone},
		CheckRedirect: o.policy,
		WrapTransport: func(base *http.Transport) http.RoundTripper {
			var next http.RoundTripper
			if o.stack == stackPool {
				pool, poolErr = endpointpool.New(endpointpool.Config{Base: base, Endpoints: []endpointpool.Endpoint{
					{Address: rig.node.Addr, Kind: endpointpool.KindConfigured},
					{Address: rig.second.Addr, Kind: endpointpool.KindConfigured},
				}})
				if poolErr != nil {
					return nil
				}
				next = pool
			} else {
				next = endpointpool.RefuseRedirects(base)
			}
			return roundTripCounter(&rig.trips, next)
		},
	}
	if o.password {
		opts.Username, opts.Password, opts.AutoLogin = "root@pam", "hunter2-password", true
	} else {
		opts.APIToken = coldWireToken
	}
	api, err := sdkclient.NewClient(opts)
	if poolErr != nil {
		t.Fatalf("build the pool: %v", poolErr)
	}
	if err != nil {
		t.Fatalf("build the SDK client: %v", err)
	}
	if pool != nil {
		t.Cleanup(pool.CloseIdleConnections)
	}
	rig.api = api
	return rig
}

// roundTripCounter counts every request it passes to next.
func roundTripCounter(n *atomic.Int32, next http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		n.Add(1)
		return next.RoundTrip(req)
	})
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// send makes the rig's request for method. The SDK may retry a GET three
// times.
func (rig *redirectRig) send(ctx context.Context, method string) error {
	ctx = sdkclient.WithRetryDelay(sdkclient.WithRetries(ctx, 3), time.Millisecond)
	if method == http.MethodGet {
		_, err := rig.api.GetCtx(ctx, "/pools/p1", nil)
		return err //nolint:wrapcheck // the test reads the SDK's error as it is
	}
	_, err := rig.api.PostCtx(ctx, "/nodes/pve9/qemu", map[string]interface{}{"vmid": coldWireVMID})
	return err //nolint:wrapcheck // the test reads the SDK's error as it is
}

// mustNotHaveFollowed checks that nothing reached the redirect's target and
// that the pool's other endpoint got no connection.
func (rig *redirectRig) mustNotHaveFollowed(t *testing.T) {
	t.Helper()
	if n := rig.target.Load(); n != 0 {
		t.Errorf("the redirect's target read %d requests, want none", n)
	}
	if rig.second != nil {
		if n := rig.second.Counts.Conns.Load(); n != 0 {
			t.Errorf("the pool's other endpoint got %d connections, want none", n)
		}
	}
}

// When a CheckRedirect policy follows redirects, the SDK installs no refusal
// of its own, and the CPI's refusal of the follow-up request is the one that
// fires. It carries the SDK's permanent marker, so the SDK sends the GET once
// where it would otherwise send it four times. Without the marker the first
// leg reaches the node once per attempt.
func TestTheCPIRedirectRefusalIsNotRetriedByTheSDK(t *testing.T) {
	for _, stack := range []redirectStack{stackPool, stackNoPool} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(fmt.Sprintf("%s, %s", stack, method), func(t *testing.T) {
				rig := newRedirectRig(t, redirectRigOptions{stack: stack, policy: sdkclient.FollowRedirects(5)})
				err := rig.send(context.Background(), method)
				if !errors.Is(err, endpointpool.ErrRedirectRefused) {
					t.Fatalf("err = %v, want the CPI's ErrRedirectRefused", err)
				}
				if !errors.Is(err, sdkclient.ErrRedirectRefused) || !IsRedirectRefused(err) {
					t.Errorf("err = %v, want it to match the SDK's ErrRedirectRefused too", err)
				}
				if !sdkclient.IsPermanent(err) {
					t.Errorf("err = %v, want the SDK's permanent marker", err)
				}
				if n := redirectLegs(rig.node, method); n != 1 {
					t.Errorf("the node read %d %s requests, want 1", n, method)
				}
				// The first leg and the refused follow-up each pass through
				// the CPI's transport once.
				if n := rig.trips.Load(); n != 2 {
					t.Errorf("the CPI's transport saw %d requests, want 2", n)
				}
				rig.mustNotHaveFollowed(t)
				mustHoldNoRedirectSecret(t, method, err)
			})
		}
	}
}

// With the SDK's default policy the SDK's transport refuses the redirect answer
// above the CPI's transport, so the CPI's refusal never sees a follow-up. The
// CPI's own retry loop stops at the refusal too, for an API request and for a
// ticket login, which the SDK reports inside a ConnectionError.
func TestTheSDKRedirectRefusalStopsTheCPIRetryLoop(t *testing.T) {
	for _, stack := range []redirectStack{stackPool, stackNoPool} {
		for _, login := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, login %v", stack, login), func(t *testing.T) {
				rig := newRedirectRig(t, redirectRigOptions{stack: stack, password: login})
				ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return time.Millisecond })
				calls := 0
				err := RetryOnTransient(ctx, log.NewNopLogger(), "redirect", 5, func() error {
					calls++
					return rig.send(ctx, http.MethodGet)
				})
				if calls != 1 {
					t.Errorf("the retry loop ran the request %d times, want once", calls)
				}
				mustBeAPermanentSDKRefusal(t, err)
				route, legs := http.MethodGet, redirectLegs(rig.node, http.MethodGet)
				if login {
					route, legs = "ticket POST", ticketPosts(rig.node)
					var connErr *sdkerrors.ConnectionError
					if !errors.As(err, &connErr) {
						t.Errorf("err = %v, want the SDK's ConnectionError for a login", err)
					}
				}
				if legs != 1 {
					t.Errorf("the node read %d %s requests, want 1", legs, route)
				}
				if n := rig.trips.Load(); n != 1 {
					t.Errorf("the CPI's transport saw %d requests, want 1", n)
				}
				rig.mustNotHaveFollowed(t)
				mustHoldNoRedirectSecret(t, route, err)
				if strings.Contains(err.Error(), "hunter2-password") {
					t.Errorf("the error holds the password: %v", err)
				}
			})
		}
	}
}

// mustBeAPermanentSDKRefusal checks that err is the SDK's refusal and not the
// CPI's, that it does not read as transient, and that both of the CPI's error
// mappings make it a CPI error the Director does not retry.
func mustBeAPermanentSDKRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, sdkclient.ErrRedirectRefused) || !IsRedirectRefused(err) {
		t.Fatalf("err = %v, want the SDK's ErrRedirectRefused", err)
	}
	if errors.Is(err, endpointpool.ErrRedirectRefused) {
		t.Errorf("err = %v, want the SDK's transport to refuse before a follow-up exists", err)
	}
	if IsTransientTransport(err) {
		t.Errorf("err = %v reads as transient", err)
	}
	var cpiErr *cpierrors.Error
	if wrapped := WrapError(err); !errors.As(wrapped, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("WrapError(%v) = %v, want a permanent CPI error", err, wrapped)
	}
	if wrapped := WrapMutationError(err); !errors.As(wrapped, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("WrapMutationError(%v) = %v, want a permanent CPI error", err, wrapped)
	}
}

// ticketPosts counts the ticket logins a node read.
func ticketPosts(node *pvetest.Server) int {
	n := 0
	for _, r := range node.Seen() {
		if r.Method == http.MethodPost && r.Path == pvetest.APIPrefix+"/access/ticket" {
			n++
		}
	}
	return n
}

// The classifiers recognize a refusal from either layer behind the wrapping
// the SDK adds, including the ConnectionError and the auto-login prefix of a
// ticket login, and never treat it as a reason to retry.
func TestRedirectRefusalClassification(t *testing.T) {
	sdkRefusal := &sdkclient.RedirectError{Status: http.StatusFound, From: "https://pve1:8006/api2/json/access/ticket"}
	cpiRefusal := sdkclient.Permanent(fmt.Errorf("%w from pve1:8006", endpointpool.ErrRedirectRefused))
	cases := map[string]error{
		"the SDK's refusal": fmt.Errorf("request failed after 1 attempt(s), not retried: %w", sdkRefusal),
		"the CPI's refusal": fmt.Errorf("request failed after 1 attempt(s), not retried: %w", cpiRefusal),
		"a login refusal": fmt.Errorf("auto-login failed: %w",
			&sdkerrors.ConnectionError{Message: "login request failed", Cause: sdkRefusal}),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if !IsRedirectRefused(err) {
				t.Fatalf("IsRedirectRefused(%v) = false", err)
			}
			if IsTransientTransport(err) || IsRetryableOrLockFault(err) {
				t.Errorf("%v reads as a reason to retry", err)
			}
			if !stopsRetry(err) {
				t.Errorf("stopsRetry(%v) = false", err)
			}
			if !cpierrors.IsType(WrapError(err), cpierrors.TypeCloud) {
				t.Errorf("WrapError(%v) = %v, want a permanent cloud error", err, WrapError(err))
			}
			if !cpierrors.IsType(WrapMutationError(err), cpierrors.TypeCloud) {
				t.Errorf("WrapMutationError(%v) = %v, want a permanent cloud error", err, WrapMutationError(err))
			}
			if !cpierrors.IsType(WrapConfigReadError(err), cpierrors.TypeCloud) {
				t.Errorf("WrapConfigReadError(%v) = %v, want a permanent cloud error", err, WrapConfigReadError(err))
			}
		})
	}
	for _, other := range []error{nil, errors.New("redirect refused"), endpointpool.ErrSendByPassed,
		&sdkerrors.ConnectionError{Message: "login request failed", Cause: errors.New("connection refused")}} {
		if IsRedirectRefused(other) {
			t.Errorf("IsRedirectRefused(%v) = true", other)
		}
	}
}
