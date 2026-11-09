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
	"strings"
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

// uploadFallbackCase is one routed-upload fault and the answer each phase of a
// direct-to-node upload must give for it. post is the answer for a fault from
// the upload's own POST, and poll is the answer for a fault from polling the
// finished upload's task.
type uploadFallbackCase struct {
	name string
	err  error
	post bool
	poll bool
}

// uploadFallbackCases is the whole case table the shared fallback decision is
// pinned against. An error with no marker is all a client without the endpoint
// wrapper produces, so it keeps the answer it has always had in both phases.
func uploadFallbackCases(t *testing.T) []uploadFallbackCase {
	t.Helper()
	// A real connection stands in for the one Go attaches to a record-header
	// error that came from a handshake whose first record was not TLS.
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cert := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	firstRecord := &url.Error{Op: "Post", URL: "https://10.0.0.13:22/x",
		Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake", Conn: client}}
	laterRecord := &url.Error{Op: "Post", URL: "https://10.0.0.13:22/x",
		Err: tls.RecordHeaderError{Msg: "oversized record received with length 20000"}}
	alert := &url.Error{Op: "Post", URL: "https://10.0.0.13:8006/x", Err: tls.AlertError(40)}
	tlsText := errors.New("request failed: tls: handshake failure")
	remoteAlert := errors.New("request failed: remote error: tls: bad certificate")
	quotedBody := sdkerrors.ParseAPIError(500, []byte(`{"message":"upstream said tls: handshake failure"}`))
	if !strings.Contains(quotedBody.Error(), "tls:") {
		t.Fatalf("the quoted-body fixture must carry \"tls:\" in its text: %v", quotedBody)
	}
	pinTag := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhasePin, Endpoint: "10.0.0.2:8006",
		Err: errors.New("certificate does not match the pin")}
	handshakeTag := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseTLSHandshake, Endpoint: "10.0.0.2:8006",
		Err: errors.New("handshake aborted")}
	dialTag := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseDial, Endpoint: "10.0.0.2:8006", Err: refused}
	marked := func(err error) error { return &endpointpool.MaybeSentError{Err: err} }
	return []uploadFallbackCase{
		{"refused dial", fmt.Errorf("Post: %w", refused), true, true},
		{"refused dial, marked", marked(fmt.Errorf("Post: %w", refused)), true, true},
		{"certificate failure", fmt.Errorf("Post: %w", cert), true, true},
		{"certificate failure, marked", marked(fmt.Errorf("Post: %w", cert)), true, true},
		{"read error", fmt.Errorf("Post: %w", read), true, true},
		{"read error, marked", marked(fmt.Errorf("Post: %w", read)), false, true},
		{"first-record header error", fmt.Errorf("Post: %w", firstRecord), true, true},
		{"first-record header error, marked", marked(fmt.Errorf("Post: %w", firstRecord)), true, true},
		{"later record header error", fmt.Errorf("Post: %w", laterRecord), true, true},
		{"later record header error, marked", marked(fmt.Errorf("Post: %w", laterRecord)), false, true},
		{"alert error", fmt.Errorf("Post: %w", alert), true, true},
		{"alert error, marked", marked(fmt.Errorf("Post: %w", alert)), true, true},
		{"tls text error", fmt.Errorf("Post: %w", tlsText), true, true},
		{"tls text error, marked", marked(fmt.Errorf("Post: %w", tlsText)), false, true},
		{"remote alert text", fmt.Errorf("Post: %w", remoteAlert), true, true},
		{"remote alert text, marked", marked(fmt.Errorf("Post: %w", remoteAlert)), false, true},
		{"response body quoting tls", quotedBody, false, false},
		{"response body quoting tls, marked", marked(quotedBody), false, false},
		{"word ending in tls", errors.New("subtls: not a crypto/tls message"), false, false},
		{"connect-phase tag", fmt.Errorf("Post: %w", pinTag), true, true},
		{"connect-phase tag, handshake", fmt.Errorf("Post: %w", handshakeTag), true, true},
		{"connect-phase tag inside marker", marked(pinTag), false, true},
		{"connect-phase tag around marker", &endpointpool.ConnectPhaseError{Phase: endpointpool.PhasePin,
			Endpoint: "10.0.0.2:8006", Err: marked(errors.New("pin check"))}, false, true},
		{"connect-phase dial tag inside marker", marked(dialTag), true, true},
		{"mid-request EOF", fmt.Errorf("upload: %w", io.EOF), false, false},
		{"mid-request EOF, marked", marked(fmt.Errorf("upload: %w", io.EOF)), false, false},
		{"unrelated", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
}

// TestUploadFallbackEligible_CaseTable pins the shared decision for every
// fault under both phases, bare and wrapped, so the poll and both upload loops
// that call it can't drift from one table.
func TestUploadFallbackEligible_CaseTable(t *testing.T) {
	t.Parallel()
	for _, tc := range uploadFallbackCases(t) {
		variants := map[string]error{"bare": tc.err}
		if tc.err != nil {
			variants["wrapped"] = fmt.Errorf("request failed after 1 attempt(s): %w", tc.err)
		}
		for kind, err := range variants {
			if got := pve.UploadFallbackEligible(err, pve.UploadPostPhase); got != tc.post {
				t.Errorf("%s (%s): UploadFallbackEligible(post) = %v, want %v", tc.name, kind, got, tc.post)
			}
			if got := pve.UploadFallbackEligible(err, pve.UploadPollPhase); got != tc.poll {
				t.Errorf("%s (%s): UploadFallbackEligible(poll) = %v, want %v", tc.name, kind, got, tc.poll)
			}
			if got := pve.IsRoutedUploadFallbackEligible(err); got != tc.post {
				t.Errorf("%s (%s): IsRoutedUploadFallbackEligible = %v, want the post-phase answer %v",
					tc.name, kind, got, tc.post)
			}
			if tc.post && !tc.poll {
				t.Errorf("%s: the table has a POST-eligible fault that is not poll-eligible", tc.name)
			}
		}
	}
}

// TestUploadFallbackEligible_UnknownPhaseFailsClosed verifies a phase value the
// helper doesn't know gets the narrowed POST answer, never the wider poll one.
func TestUploadFallbackEligible_UnknownPhaseFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range uploadFallbackCases(t) {
		if got := pve.UploadFallbackEligible(tc.err, pve.UploadFaultPhase(99)); got != tc.post {
			t.Errorf("%s: unknown phase = %v, want the post-phase answer %v", tc.name, got, tc.post)
		}
	}
}

func TestUploadPhaseFor(t *testing.T) {
	t.Parallel()
	if got := pve.UploadPhaseFor(""); got != pve.UploadPostPhase {
		t.Errorf("no pending UPID = %v, want the post phase", got)
	}
	if got := pve.UploadPhaseFor("UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"); got != pve.UploadPollPhase {
		t.Errorf("a pending UPID = %v, want the poll phase", got)
	}
}

// TestIsRoutedHandshakeFailure_MaybeSentArms verifies the read arm, the "tls:"
// text arm, and a record-header error from an established connection return
// false under the marker, while a first-record header error and an alert keep
// their answer.
func TestIsRoutedHandshakeFailure_MaybeSentArms(t *testing.T) {
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
	later := &endpointpool.MaybeSentError{Err: tls.RecordHeaderError{Msg: "oversized record received with length 20000"}}
	if pve.IsRoutedHandshakeFailure(later) {
		t.Error("a marked record-header error from an established connection must not be a handshake failure")
	}
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	first := &endpointpool.MaybeSentError{Err: tls.RecordHeaderError{Msg: "not tls", Conn: client}}
	if !pve.IsRoutedHandshakeFailure(first) {
		t.Error("a first-record header error keeps its answer under the marker")
	}
	if !pve.IsRoutedHandshakeFailure(tls.RecordHeaderError{Msg: "oversized record received with length 20000"}) {
		t.Error("an unmarked record-header error keeps its answer")
	}
}
