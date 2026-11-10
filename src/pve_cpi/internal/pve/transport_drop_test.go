package pve_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// errServerClosedIdleText mirrors the message of net/http's unexported
// errServerClosedIdle sentinel, the other arm of the keep-alive race whose
// io.EOF arm the predicate already covers. It carries no chain, so an exact
// string match per unwrap link is the only handle.
var errServerClosedIdleText = errors.New("http: server closed idle connection")

// TestIsTransportConnectionDrop_LifecycleSentinels verifies the two
// connection-lifecycle races that previously escaped every transient
// classifier: the server closing an idle keep-alive connection between pickup
// and write, and the pool handing out an already-closed connection
// (net.ErrClosed). Both must classify as a transport drop, as transient
// transport, and as retriable through WrapError.
func TestIsTransportConnectionDrop_LifecycleSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{
			name: "server closed idle connection via url.Error",
			err:  &url.Error{Op: "Post", URL: "https://pve:8006/api2/json/nodes", Err: errServerClosedIdleText},
		},
		{
			name: "net.ErrClosed via url.Error",
			err:  &url.Error{Op: "Post", URL: "https://pve:8006/api2/json/nodes", Err: net.ErrClosed},
		},
		{
			name: "net.ErrClosed via net.OpError",
			err:  &net.OpError{Op: "write", Err: net.ErrClosed},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !pve.IsTransportConnectionDrop(tc.err) {
				t.Errorf("IsTransportConnectionDrop(%v) = false, want true", tc.err)
			}

			if !pve.IsTransientTransport(tc.err) {
				t.Errorf("IsTransientTransport(%v) = false, want true", tc.err)
			}

			wrapped := pve.WrapError(tc.err)
			if !cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
				t.Errorf("WrapError(%v) = %v, want TypeRetriableCloud", tc.err, wrapped)
			}
		})
	}
}

// TestIsTransportConnectionDrop_SubstringControl guards the allow-list
// discipline: the idle-connection sentinel matches per unwrap link by
// full-string equality, never by substring, so an arbitrary error message
// that merely contains the phrase inside a larger sentence must not match.
func TestIsTransportConnectionDrop_SubstringControl(t *testing.T) {
	t.Parallel()

	lookalike := errors.New("storage note: http: server closed idle connection counters were reset")

	if pve.IsTransportConnectionDrop(lookalike) {
		t.Errorf("IsTransportConnectionDrop(%v) = true, want false for a substring lookalike", lookalike)
	}

	wrapped := pve.WrapError(lookalike)
	if cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
		t.Errorf("WrapError(%v) = %v, want non-retriable for a substring lookalike", lookalike, wrapped)
	}
}

// TestWrapError_TypedConnectionErrorFromDrop verifies the SDK v3.9.1 shape:
// a drop surfaced by the SDK as its typed *ConnectionError (with the raw
// sentinel as Cause) classifies retriable through the existing typed branch.
func TestWrapError_TypedConnectionErrorFromDrop(t *testing.T) {
	t.Parallel()

	sdkShape := &sdkerrors.ConnectionError{
		Host:    "pve",
		Port:    8006,
		Message: "connection dropped after 1 attempt(s)",
		Cause:   errServerClosedIdleText,
	}

	if !pve.IsTransientTransport(sdkShape) {
		t.Errorf("IsTransientTransport(typed ConnectionError) = false, want true")
	}

	wrapped := pve.WrapError(sdkShape)
	if !cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
		t.Errorf("WrapError(typed ConnectionError) = %v, want TypeRetriableCloud", wrapped)
	}
}

// TestIsTransientTransport_AuthNoTicketRescue covers the textual safety net
// for SDK versions predating v3.9.1, whose ticket-login failure surfaced as
// the bare "authentication failed: no ticket received" sentinel with no
// status chain. Against a live cluster that shape is overwhelmingly a
// transient pveproxy fault, so it classifies retriable. A different
// authentication failure must not ride along.
func TestIsTransientTransport_AuthNoTicketRescue(t *testing.T) {
	t.Parallel()

	rescued := errors.New("forced re-authentication failed: authentication failed: no ticket received")
	if !pve.IsTransientTransport(rescued) {
		t.Errorf("IsTransientTransport(%v) = false, want true (pre-v3.9.1 rescue)", rescued)
	}

	control := errors.New("authentication failed: invalid credentials")
	if pve.IsTransientTransport(control) {
		t.Errorf("IsTransientTransport(%v) = true, want false for a non-ticket auth failure", control)
	}
}

// TestIsTransientTransport_LoginStatusChains covers the SDK v3.9.1 shape,
// where a failed ticket login carries the real HTTP status in its chain: a
// 5xx login is a transient server fault (retriable) while a 401 login is a
// credential verdict (permanent), even though both messages contain the
// no-ticket sentinel text the pre-v3.9.1 rescue matches on.
func TestIsTransientTransport_LoginStatusChains(t *testing.T) {
	t.Parallel()

	noTicket := errors.New("authentication failed: no ticket received")

	transient := fmt.Errorf("auto-login failed: %w: %w", noTicket,
		fmt.Errorf("login failed with status 503: %w",
			sdkerrors.ParseAPIError(503, []byte(`{"message":"service unavailable"}`))))
	if !pve.IsTransientTransport(transient) {
		t.Errorf("IsTransientTransport(5xx login chain) = false, want true; err = %v", transient)
	}

	verdict := fmt.Errorf("auto-login failed: %w: %w", noTicket,
		fmt.Errorf("login failed with status 401: %w",
			sdkerrors.ParseAPIError(401, []byte(`{"message":"authentication failure"}`))))
	if pve.IsTransientTransport(verdict) {
		t.Errorf("IsTransientTransport(401 login chain) = true, want false (credential verdict); err = %v", verdict)
	}

	wrapped := pve.WrapError(verdict)
	if cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
		t.Errorf("WrapError(401 login chain) = retriable %v, want permanent", wrapped)
	}
}

// dropWireClient builds a CPI client over the servers through the real
// constructor, so its multi-endpoint pool wraps the SDK's transport. The first
// server is the configured host, and the client verifies against ca.
func dropWireClient(t *testing.T, ca *pvetest.CA, servers ...*pvetest.Server) pve.Client {
	t.Helper()
	verify := true
	host, port := servers[0].HostPort()
	cfg := &config.CPIConfig{
		Host:                 host,
		Port:                 port,
		APIToken:             "root@pam!test=tok-drop",
		APIEndpointDiscovery: config.DiscoveryModeOff,
		VerifySSL:            &verify,
		PVECACertPEM:         string(ca.CertPEM()),
	}
	for _, s := range servers {
		cfg.Endpoints = append(cfg.Endpoints, s.Addr)
	}
	c, err := pve.NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	return c
}

// postsToPools counts the POST /pools requests the servers read, by method and
// path, so a request a later change adds to the clients, such as a login, is not
// counted as the mutation under test.
func postsToPools(servers ...*pvetest.Server) int {
	n := 0
	for _, s := range servers {
		for _, r := range s.Seen() {
			if r.Method == http.MethodPost && r.Path == pvetest.APIPrefix+"/pools" {
				n++
			}
		}
	}
	return n
}

// TestMutationSplitsNeverSentFromMaybeSent drives one POST through a real
// two-server pool for each way the first server can fail, and checks that the
// pool hops exactly when the request provably never left the client. A refused
// dial and a handshake reset prove it, so the POST goes to the second server
// and exactly one POST reaches any server. A connection dropped after the
// server read the request does not prove it, so the POST stays with the first
// server, the error does not read as connect-phase, and the second server reads
// no POST. These tests run serially, because the closed server's port must not
// be handed to a parallel test while the client dials it.
func TestMutationSplitsNeverSentFromMaybeSent(t *testing.T) {
	cases := []struct {
		name string
		// first starts the first server, which fails the POST in the way the case names.
		first func(t *testing.T, ca *pvetest.CA, leaf *pvetest.Leaf) *pvetest.Server
		// hops is whether the POST reaches the second server.
		hops bool
	}{
		{"a refused dial hops", func(t *testing.T, _ *pvetest.CA, leaf *pvetest.Leaf) *pvetest.Server {
			s := pvetest.NewLive(t, leaf, nil)
			s.Close()
			return s
		}, true},
		{"a handshake reset hops", func(t *testing.T, _ *pvetest.CA, _ *pvetest.Leaf) *pvetest.Server {
			return pvetest.NewReset(t)
		}, true},
		{"a drop after the request does not hop", func(t *testing.T, _ *pvetest.CA, leaf *pvetest.Leaf) *pvetest.Server {
			return pvetest.NewBodyThenHijack(t, leaf)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := pvetest.NewCA(t)
			leaf := ca.NewLeaf(t, pvetest.LeafOptions{})
			first := tc.first(t, ca, leaf)
			second := pvetest.NewLive(t, leaf, nil)
			c := dropWireClient(t, ca, first, second)
			ctx, release := endpointpool.WithAffinityScope(context.Background())
			defer release()

			err := c.Pools().CreatePool(ctx, "split-pool", "split")

			if tc.hops {
				if err != nil {
					t.Fatalf("CreatePool: %v, want the POST to hop to the second server", err)
				}
				if got := postsToPools(second); got != 1 {
					t.Errorf("the second server read %d POST /pools, want 1", got)
				}
				if got := postsToPools(first, second); got != 1 {
					t.Errorf("%d POST /pools reached any server, want exactly 1", got)
				}
				return
			}
			if err == nil {
				t.Fatal("CreatePool succeeded, want the dropped connection to surface")
			}
			if endpointpool.IsConnectPhase(err) && !endpointpool.IsMaybeSent(err) {
				t.Errorf("error %v proves the POST was never sent, but the server read it", err)
			}
			if !pve.IsTransientTransport(err) {
				t.Errorf("the dropped connection is not transient, so no retry layer would replay it: %v", err)
			}
			if got := postsToPools(first); got != 1 {
				t.Errorf("the first server read %d POST /pools, want 1", got)
			}
			if got := postsToPools(second); got != 0 {
				t.Errorf("the second server read %d POST /pools, want 0", got)
			}
		})
	}
}

// TestMutationWalkOfUnreachableServersIsConnectPhaseAndNotMaybeSent drives a
// POST at two servers that both reset the handshake. Neither received a byte of
// the request, so the walk's error is connect-phase and not marked maybe-sent,
// and no POST reaches either server.
func TestMutationWalkOfUnreachableServersIsConnectPhaseAndNotMaybeSent(t *testing.T) {
	ca := pvetest.NewCA(t)
	first, second := pvetest.NewReset(t), pvetest.NewReset(t)
	c := dropWireClient(t, ca, first, second)
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	defer release()

	err := c.Pools().CreatePool(ctx, "split-pool", "split")

	if err == nil {
		t.Fatal("CreatePool succeeded against two servers that reset every handshake")
	}
	if !endpointpool.IsConnectPhase(err) || endpointpool.IsMaybeSent(err) {
		t.Errorf("error %v: connect-phase = %v, maybe-sent = %v, want true and false",
			err, endpointpool.IsConnectPhase(err), endpointpool.IsMaybeSent(err))
	}
	var walk *endpointpool.EntryUnreachableError
	if !errors.As(err, &walk) || len(walk.Endpoints()) != 2 {
		t.Errorf("error %v: want a walk that tried both endpoints", err)
	}
	if got := postsToPools(first, second); got != 0 {
		t.Errorf("%d POST /pools reached a server that resets every handshake", got)
	}
}
