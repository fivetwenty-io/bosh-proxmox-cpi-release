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
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
