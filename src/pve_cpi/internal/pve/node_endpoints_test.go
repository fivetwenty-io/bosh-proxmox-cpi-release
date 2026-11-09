package pve_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

func TestClusterNodeAddressMap_DecodesNameAndIP(t *testing.T) {
	t.Parallel()
	c := newPeersClient(
		statusRow("cluster", "lab", "", 1),           // non-node row excluded
		statusRow("node", "pve1", "192.168.1.10", 1), // included
		statusRow("node", "pve2", "192.168.1.20", 0), // offline excluded
		statusRow("node", "pve3", "", 1),             // missing ip excluded
		statusRow("node", "", "192.168.1.40", 1),     // missing name excluded
		json.RawMessage(`"not an object"`),           // malformed skipped
		statusRow("node", "pve5", "192.168.1.50", 1), // included
	)

	addrs, err := pve.ClusterNodeAddressMap(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"pve1": "192.168.1.10", "pve5": "192.168.1.50"}
	if len(addrs) != len(want) {
		t.Fatalf("addrs = %v, want %v", addrs, want)
	}
	for node, ip := range want {
		if addrs[node] != ip {
			t.Errorf("addrs[%q] = %q, want %q", node, addrs[node], ip)
		}
	}
}

func TestNodeEndpointResolver_ExplicitMapWinsOverDiscovery(t *testing.T) {
	t.Parallel()
	c := newPeersClient(statusRow("node", "pve2", "10.0.0.2", 1))
	r := pve.NewNodeEndpointResolver(c,
		map[string]string{"pve2": "pve2.example.com"}, "pve1.example.com", true, log.NewNopLogger())

	h, ok := r.HostFor(context.Background(), "pve2")
	if !ok || h != "pve2.example.com" {
		t.Errorf("HostFor(pve2) = %q, %v; want explicit pve2.example.com, true", h, ok)
	}
}

func TestNodeEndpointResolver_DiscoveryFallbackAndMemoization(t *testing.T) {
	t.Parallel()
	calls := 0
	resp := sdkcluster.ListStatusResponse([]json.RawMessage{
		statusRow("node", "pve2", "10.0.0.2", 1),
		statusRow("node", "pve3", "10.0.0.3", 1),
	})
	c := &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
				calls++
				return &resp, nil
			},
		},
	}
	r := pve.NewNodeEndpointResolver(c, nil, "pve1.example.com", true, log.NewNopLogger())

	if h, ok := r.HostFor(context.Background(), "pve2"); !ok || h != "10.0.0.2" {
		t.Errorf("HostFor(pve2) = %q, %v; want discovered 10.0.0.2, true", h, ok)
	}
	if h, ok := r.HostFor(context.Background(), "pve3"); !ok || h != "10.0.0.3" {
		t.Errorf("HostFor(pve3) = %q, %v; want discovered 10.0.0.3, true", h, ok)
	}
	if _, ok := r.HostFor(context.Background(), "pve9"); ok {
		t.Error("HostFor(pve9): expected no override for an unknown node")
	}
	if calls != 1 {
		t.Errorf("ListStatus calls = %d, want 1 (memoized)", calls)
	}
}

func TestNodeEndpointResolver_DiscoveryDisallowed(t *testing.T) {
	t.Parallel()
	c := &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
				t.Error("discovery must not run when disallowed")
				return nil, errors.New("unreachable")
			},
		},
	}
	r := pve.NewNodeEndpointResolver(c,
		map[string]string{"pve2": "pve2.example.com"}, "pve1.example.com", false, log.NewNopLogger())

	if h, ok := r.HostFor(context.Background(), "pve2"); !ok || h != "pve2.example.com" {
		t.Errorf("HostFor(pve2) = %q, %v; want explicit entry, true", h, ok)
	}
	if _, ok := r.HostFor(context.Background(), "pve3"); ok {
		t.Error("HostFor(pve3): expected no override with discovery disallowed")
	}
}

func TestNodeEndpointResolver_ConfiguredHostYieldsNoOverride(t *testing.T) {
	t.Parallel()
	c := newPeersClient(statusRow("node", "pve1", "pve1.example.com", 1))
	r := pve.NewNodeEndpointResolver(c, map[string]string{
		"pve1": "pve1.example.com",
		"pve2": "pve1.example.com:8006",
	}, "pve1.example.com", true, log.NewNopLogger())

	if _, ok := r.HostFor(context.Background(), "pve1"); ok {
		t.Error("HostFor(pve1): an entry equal to the configured host must not override")
	}
	if _, ok := r.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2): host:port whose host equals the configured host must not override")
	}
}

func TestNodeEndpointResolver_DiscoveryErrorResolvesToNoOverride(t *testing.T) {
	t.Parallel()
	calls := 0
	c := &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
				calls++
				return nil, fmt.Errorf("boom")
			},
		},
	}
	r := pve.NewNodeEndpointResolver(c, nil, "pve1.example.com", true, log.NewNopLogger())

	if _, ok := r.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2): expected no override when discovery fails")
	}
	// The failed outcome is memoized; a second lookup must not re-fetch.
	if _, ok := r.HostFor(context.Background(), "pve3"); ok {
		t.Error("HostFor(pve3): expected no override when discovery failed")
	}
	if calls != 1 {
		t.Errorf("ListStatus calls = %d, want 1 (failure memoized)", calls)
	}
}

func TestNodeEndpointResolver_NilResolverAndEmptyNode(t *testing.T) {
	t.Parallel()
	var r *pve.NodeEndpointResolver
	if _, ok := r.HostFor(context.Background(), "pve2"); ok {
		t.Error("nil resolver must not override")
	}
	r2 := pve.NewNodeEndpointResolver(newPeersClient(), map[string]string{"pve2": "x"}, "h", true, log.NewNopLogger())
	if _, ok := r2.HostFor(context.Background(), ""); ok {
		t.Error("empty node must not override")
	}
}

func TestUploadContext_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := pve.UploadContext(context.Background(), "pve2.example.com")
	if got := pve.UploadHostFromContext(ctx); got != "pve2.example.com" {
		t.Errorf("UploadHostFromContext = %q, want pve2.example.com", got)
	}
	if got := pve.UploadHostFromContext(context.Background()); got != "" {
		t.Errorf("UploadHostFromContext(plain ctx) = %q, want empty", got)
	}
}

func TestNodeEndpointResolver_MarkDirectRouteFailed(t *testing.T) {
	t.Parallel()
	r := pve.NewNodeEndpointResolver(newPeersClient(),
		map[string]string{"pve2": "pve2.example.com"}, "pve1.example.com", false, log.NewNopLogger())

	if _, ok := r.HostFor(context.Background(), "pve2"); !ok {
		t.Fatal("HostFor(pve2): expected an override before the route is marked failed")
	}
	r.MarkDirectRouteFailed("pve2")
	if _, ok := r.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2): expected no override after MarkDirectRouteFailed")
	}

	var nilResolver *pve.NodeEndpointResolver
	nilResolver.MarkDirectRouteFailed("pve2") // must not panic
}

func TestNodeEndpointResolver_CancelledDiscoveryNotMemoized(t *testing.T) {
	t.Parallel()
	resp := sdkcluster.ListStatusResponse([]json.RawMessage{
		statusRow("node", "pve2", "10.0.0.2", 1),
	})
	c := &mockClient{
		clusterSvc: &statusStubCluster{
			listStatusFn: func(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return &resp, nil
			},
		},
	}
	r := pve.NewNodeEndpointResolver(c, nil, "pve1.example.com", true, log.NewNopLogger())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := r.HostFor(cancelled, "pve2"); ok {
		t.Error("HostFor with a cancelled ctx: expected no override")
	}
	// The cancellation must not poison the process; a live ctx retries.
	if h, ok := r.HostFor(context.Background(), "pve2"); !ok || h != "10.0.0.2" {
		t.Errorf("HostFor after cancelled first fetch = %q, %v; want discovered 10.0.0.2, true", h, ok)
	}
}

func TestIsDirectDialFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dial op error", fmt.Errorf("Post: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), true},
		{"read op error", fmt.Errorf("Post: %w",
			&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}), false},
		{"dns error", fmt.Errorf("Post: %w",
			&net.DNSError{Err: "no such host", Name: "pve2.invalid"}), true},
		{"sdk connection error over dial", &sdkerrors.ConnectionError{
			Host: "10.0.0.2", Port: 8006, Message: "failed to establish a connection",
			Cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}}, true},
		{"mid-request drop", fmt.Errorf("upload: %w", io.EOF), false},
		{"unrelated", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := pve.IsDirectDialFailure(tc.err); got != tc.want {
			t.Errorf("%s: IsDirectDialFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsRoutedHandshakeFailure_F01NonVerificationTLSHandshake is the F-01
// regression: a TLS handshake failure that is not certificate verification
// (a non-TLS listener on the routed port, or a protocol-level handshake
// abort) must be fallback-eligible so the routed upload degrades to the
// proxied path instead of failing permanently. Before the fix neither
// IsTLSCertVerificationFailure nor IsDirectDialFailure matched this shape.
func TestIsRoutedHandshakeFailure_F01NonVerificationTLSHandshake(t *testing.T) {
	t.Parallel()
	// The exact wrap shape the SDK produces: fmt.Errorf("request failed
	// after %d attempt(s): %w", ...) around *url.Error{Err:
	// tls.RecordHeaderError{...}}.
	err := fmt.Errorf("request failed after 1 attempt(s): %w", &url.Error{
		Op:  "Post",
		URL: "https://10.0.0.13:22/api2/json/x",
		Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
	})
	if pve.IsTLSCertVerificationFailure(err) {
		t.Error("IsTLSCertVerificationFailure must not match a non-verification TLS handshake failure")
	}
	if pve.IsDirectDialFailure(err) {
		t.Error("IsDirectDialFailure must not match a non-verification TLS handshake failure")
	}
	if !pve.IsRoutedHandshakeFailure(err) {
		t.Error("IsRoutedHandshakeFailure = false, want true for tls.RecordHeaderError")
	}
}

// TestIsRoutedHandshakeFailure_F02HandshakePhaseReadTimeout is the F-02
// regression: a read-phase net.OpError that occurs before any response is
// received (the shape a stalled or reset TLS handshake produces once the
// TCP connect has already succeeded) must be fallback-eligible, not merely
// retryable, so the retry loop does not stay pinned to a dead route for the
// entire budget. Before the fix this shape was retryable
// (IsTransientTransport true via net.Error.Timeout()) but not
// fallback-eligible.
func TestIsRoutedHandshakeFailure_F02HandshakePhaseReadTimeout(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("Post: %w", &url.Error{
		Op:  "Post",
		URL: "https://10.0.0.13:8006/api2/json/x",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: errTimeoutOp{}},
	})
	if !pve.IsTransientTransport(err) {
		t.Fatal("test setup: expected the fixture to satisfy IsTransientTransport (Timeout()==true)")
	}
	if pve.IsDirectDialFailure(err) {
		t.Error("IsDirectDialFailure must not match a read-phase net.OpError (see its doc comment)")
	}
	if !pve.IsRoutedHandshakeFailure(err) {
		t.Error("IsRoutedHandshakeFailure = false, want true for a read-phase net.OpError")
	}
}

// errTimeoutOp implements net.Error with Timeout()==true, the shape
// net.OpError.Timeout() delegates to, without needing a real syscall error.
type errTimeoutOp struct{}

func (errTimeoutOp) Error() string   { return "i/o timeout" }
func (errTimeoutOp) Timeout() bool   { return true }
func (errTimeoutOp) Temporary() bool { return true }

func TestIsRoutedHandshakeFailure_EstablishedConnectionDropStillExcludedFromDirectDial(t *testing.T) {
	t.Parallel()
	// Mirrors the existing "read op error" case in TestIsDirectDialFailure:
	// a plain read-phase net.OpError with no Timeout() signal, the shape a
	// mid-response drop on an established connection produces.
	err := fmt.Errorf("Post: %w",
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")})
	if pve.IsDirectDialFailure(err) {
		t.Error("IsDirectDialFailure must still exclude a read-phase net.OpError; " +
			"the fallback distinction lives in IsRoutedHandshakeFailure, not here")
	}
	// IsRoutedHandshakeFailure deliberately does NOT preserve this exclusion:
	// the wire signal cannot distinguish a handshake-phase read failure from
	// an established-connection drop for this exact shape, and upload
	// fallback sites accept that ambiguity resolves toward fallback (see the
	// predicate's doc comment).
	if !pve.IsRoutedHandshakeFailure(err) {
		t.Error("IsRoutedHandshakeFailure = false, want true (ambiguous read-phase shape resolves to fallback)")
	}
}

func TestIsRoutedHandshakeFailure_NoFalsePositiveOnUnrelatedErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"unrelated", errors.New("boom")},
		{"mid-request EOF", fmt.Errorf("upload: %w", io.EOF)},
		{"dial op error", fmt.Errorf("Post: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})},
	}
	for _, tc := range cases {
		if got := pve.IsRoutedHandshakeFailure(tc.err); got {
			t.Errorf("%s: IsRoutedHandshakeFailure = true, want false", tc.name)
		}
	}
}

func TestIsTLSCertVerificationFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"tls verification error", fmt.Errorf("dial: %w",
			&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), true},
		{"x509 unknown authority", fmt.Errorf("dial: %w", x509.UnknownAuthorityError{}), true},
		{"x509 hostname mismatch", fmt.Errorf("dial: %w",
			x509.HostnameError{Certificate: &x509.Certificate{}, Host: "10.0.0.2"}), true},
		{"textual SAN mismatch", errors.New("tls: failed to verify certificate: x509: certificate is valid for pve02, pve02.example.com, not 10.0.0.2"), true},
		{"plain transport drop", fmt.Errorf("upload: %w", io.EOF), false},
		{"unrelated", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := pve.IsTLSCertVerificationFailure(tc.err); got != tc.want {
			t.Errorf("%s: IsTLSCertVerificationFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// uploadFallbackFaults are the routed-upload faults the fallback predicates are
// pinned against. The first group is what a client without the endpoint wrapper
// produces, so an unmarked error must keep the answer it has always had.
func uploadFallbackFaults() []struct {
	name string
	err  error
	want bool
} {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cert := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	record := &url.Error{Op: "Post", URL: "https://10.0.0.13:22/x",
		Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}
	alert := &url.Error{Op: "Post", URL: "https://10.0.0.13:8006/x", Err: tls.AlertError(40)}
	tlsText := errors.New("request failed: tls: handshake failure")
	return []struct {
		name string
		err  error
		want bool
	}{
		{"refused dial", fmt.Errorf("Post: %w", refused), true},
		{"certificate failure", fmt.Errorf("Post: %w", cert), true},
		{"read error", fmt.Errorf("Post: %w", read), true},
		{"record header error", fmt.Errorf("Post: %w", record), true},
		{"alert error", fmt.Errorf("Post: %w", alert), true},
		{"tls text error", fmt.Errorf("Post: %w", tlsText), true},
		{"mid-request EOF", fmt.Errorf("upload: %w", io.EOF), false},
		{"unrelated", errors.New("boom"), false},
		{"nil", nil, false},
	}
}

func TestIsRoutedUploadFallbackEligible_UnmarkedErrorsKeepTheirAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range uploadFallbackFaults() {
		if got := pve.IsRoutedUploadFallbackEligible(tc.err); got != tc.want {
			t.Errorf("%s: IsRoutedUploadFallbackEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsRoutedUploadFallbackEligible_MaybeSentNarrowsTheAmbiguousArms verifies
// the read arm and the "tls:" text arm return false for an error the endpoint
// wrapper marked as possibly sent, while a refused dial, a certificate failure,
// a record-header error, and an alert error keep their answer.
func TestIsRoutedUploadFallbackEligible_MaybeSentNarrowsTheAmbiguousArms(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"refused dial":        true,
		"certificate failure": true,
		"read error":          false,
		"record header error": true,
		"alert error":         true,
		"tls text error":      false,
	}
	for _, tc := range uploadFallbackFaults() {
		w, ok := want[tc.name]
		if !ok {
			continue
		}
		marked := &endpointpool.MaybeSentError{Err: tc.err}
		if got := pve.IsRoutedUploadFallbackEligible(marked); got != w {
			t.Errorf("%s (marked): IsRoutedUploadFallbackEligible = %v, want %v", tc.name, got, w)
		}
		wrapped := fmt.Errorf("request failed after 1 attempt(s): %w", marked)
		if got := pve.IsRoutedUploadFallbackEligible(wrapped); got != w {
			t.Errorf("%s (marked, wrapped): IsRoutedUploadFallbackEligible = %v, want %v", tc.name, got, w)
		}
	}
}

func TestIsRoutedHandshakeFailure_MaybeSentReadAndTLSTextArmsReturnFalse(t *testing.T) {
	t.Parallel()
	read := &endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w",
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")})}
	if pve.IsRoutedHandshakeFailure(read) {
		t.Error("a marked read-phase error must not be a handshake failure")
	}
	text := &endpointpool.MaybeSentError{Err: errors.New("request failed: tls: bad record MAC")}
	if pve.IsRoutedHandshakeFailure(text) {
		t.Error("a marked \"tls:\" text error must not be a handshake failure")
	}
	record := &endpointpool.MaybeSentError{Err: tls.RecordHeaderError{Msg: "not tls"}}
	if !pve.IsRoutedHandshakeFailure(record) {
		t.Error("a record-header error keeps its answer under the marker")
	}
}

// TestIsRoutedUploadFallbackEligible_ConnectPhaseTag verifies a connect-phase
// tagged error is one more positive signal, that a tagged error which also
// carries the maybe-sent marker is not counted, and that a refused dial under
// the marker still falls back.
func TestIsRoutedUploadFallbackEligible_ConnectPhaseTag(t *testing.T) {
	t.Parallel()
	tagged := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhasePin, Endpoint: "10.0.0.2:8006",
		Err: errors.New("certificate does not match the pin")}
	if !pve.IsRoutedUploadFallbackEligible(fmt.Errorf("Post: %w", tagged)) {
		t.Error("a connect-phase tagged error must be fallback-eligible")
	}
	if pve.IsRoutedUploadFallbackEligible(&endpointpool.MaybeSentError{Err: tagged}) {
		t.Error("a maybe-sent marker must not be overridden by a connect-phase tag")
	}
	refused := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseDial, Endpoint: "10.0.0.2:8006",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}
	if !pve.IsRoutedUploadFallbackEligible(&endpointpool.MaybeSentError{Err: refused}) {
		t.Error("a refused dial must still fall back under the marker")
	}
}
