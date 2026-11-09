package pve

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// sdkWrapTransportError mirrors what the SDK does with a transport failure
// from its round tripper. The client wraps it in a *url.Error, then a dial or
// DNS failure becomes a *ConnectionError and a refused certificate keeps the
// retry loop's plain wrapping.
func sdkWrapTransportError(doErr error) error {
	urlErr := &url.Error{Op: "Get", URL: "https://pve1:8006/api2/json/version", Err: doErr}
	var (
		dnsErr *net.DNSError
		opErr  *net.OpError
	)
	if errors.As(urlErr, &dnsErr) || (errors.As(urlErr, &opErr) && opErr.Op == "dial") {
		return &sdkerrors.ConnectionError{
			Host: "pve1", Port: 8006, Message: "failed to establish a connection", Cause: urlErr,
		}
	}
	return fmt.Errorf("request failed after 1 attempt(s): %w", urlErr)
}

func walkFailure(endpoint string, phase endpointpool.Phase, cause error) endpointpool.EndpointFailure {
	return endpointpool.EndpointFailure{
		Endpoint: endpoint,
		Err:      &endpointpool.ConnectPhaseError{Phase: phase, Endpoint: endpoint, Err: cause},
	}
}

// An EntryUnreachableError must classify as transient transport for every walk
// except one in which every hop failed certificate verification, so a walk that
// reached no endpoint is retried like a refused dial, and a refused certificate
// stays permanent as it is for a single endpoint today.
func TestIsTransientTransportClassifiesEntryUnreachableWalks(t *testing.T) {
	refused := errors.New("connection refused")
	verify := func(endpoint string) endpointpool.EndpointFailure {
		cause := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
		return walkFailure(endpoint, endpointpool.PhaseTLSVerify, cause)
	}
	cases := []struct {
		name     string
		attempts []endpointpool.EndpointFailure
		want     bool
		wantConn bool
	}{
		{"dial walk", []endpointpool.EndpointFailure{
			walkFailure("pve1:8006", endpointpool.PhaseDial, refused),
			walkFailure("pve2:8006", endpointpool.PhaseDial, refused),
		}, true, true},
		{"dns walk", []endpointpool.EndpointFailure{
			walkFailure("pve1:8006", endpointpool.PhaseDNS, &net.DNSError{Err: "no such host", Name: "pve1"}),
		}, true, true},
		{"header deadline walk", []endpointpool.EndpointFailure{
			{Endpoint: "pve1:8006", Err: errors.New("response header timeout")},
			{Endpoint: "pve2:8006", Err: errors.New("response header timeout")},
		}, true, true},
		{"mixed walk", []endpointpool.EndpointFailure{
			verify("pve1:8006"),
			walkFailure("pve2:8006", endpointpool.PhaseDial, refused),
		}, true, true},
		{"empty walk", nil, true, true},
		{"all-verify walk", []endpointpool.EndpointFailure{verify("pve1:8006"), verify("pve2:8006")}, false, false},
		{"single verify hop", []endpointpool.EndpointFailure{verify("pve1:8006")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			walk := endpointpool.NewEntryUnreachableError(tc.attempts)
			err := sdkWrapTransportError(walk)
			if got := IsTransientTransport(err); got != tc.want {
				t.Errorf("IsTransientTransport = %v, want %v for %v", got, tc.want, err)
			}
			var connErr *sdkerrors.ConnectionError
			if got := errors.As(err, &connErr); got != tc.wantConn {
				t.Errorf("reads as ConnectionError = %v, want %v", got, tc.wantConn)
			}
			var entry *endpointpool.EntryUnreachableError
			if !errors.As(err, &entry) {
				t.Error("the walk should stay reachable through the SDK's wrapping")
			}
		})
	}
}
