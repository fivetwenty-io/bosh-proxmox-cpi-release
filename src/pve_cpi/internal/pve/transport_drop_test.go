package pve_test

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
