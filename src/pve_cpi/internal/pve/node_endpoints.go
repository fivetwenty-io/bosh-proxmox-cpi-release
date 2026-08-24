package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// NodeEndpointResolver resolves the pveproxy address to dial for a node-scoped
// storage upload. A multipart POST to /nodes/{node}/storage/{storage}/upload
// sent to the configured endpoint is proxied by that node's pveproxy to the
// target node's pveproxy, and under burst load the cross-node hop sheds
// connections mid-request; dialing the target node directly removes the hop.
//
// Resolution order per node: an explicit pve.node_endpoints entry wins; when
// discovery is allowed, the /cluster/status name-to-IP map fills the rest;
// otherwise no override applies and the upload takes the proxied path.
// Discovery is memoized once per process (the CPI runs one process per
// request; the stemcell replication fan-out within one request is the
// multi-lookup case) and a discovery failure resolves to "no override"; it
// must never fail the upload. A resolved address whose dial then fails is
// reported back via MarkDirectRouteFailed, after which the node resolves to
// "no override" for the rest of the process.
//
// A nil resolver is valid everywhere and never overrides.
type NodeEndpointResolver struct {
	c              Client
	logger         *log.Logger
	explicit       map[string]string
	configuredHost string
	allowDiscovery bool

	mu            sync.Mutex
	discovered    map[string]string
	discoveryDone bool
	discoveryGate chan struct{} // non-nil while a fetch is in flight
	failedDirect  map[string]bool
}

// NewNodeEndpointResolver builds a resolver over c. explicit is the operator's
// pve.node_endpoints map (node name to host or host:port; may be nil),
// configuredHost is the job-level pve.host (a resolution equal to it yields no
// override, since there is no hop to remove), and allowDiscovery gates the
// /cluster/status fallback. Callers gate discovery on verify_ssl being
// disabled: the discovered address is the corosync link0 IP, and stock PVE
// node certificates carry DNS SANs for the node hostname and FQDN, usually
// not that IP, so a verifying deployment must route via explicit entries.
func NewNodeEndpointResolver(c Client, explicit map[string]string, configuredHost string, allowDiscovery bool, logger *log.Logger) *NodeEndpointResolver {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &NodeEndpointResolver{
		c:              c,
		logger:         logger,
		explicit:       explicit,
		configuredHost: configuredHost,
		allowDiscovery: allowDiscovery,
	}
}

// HostFor returns the address to dial for node and true, or "" and false when
// no override applies: no mapping, discovery disallowed or failed, or the
// resolved address equals the configured endpoint host.
func (r *NodeEndpointResolver) HostFor(ctx context.Context, node string) (string, bool) {
	if r == nil || node == "" {
		return "", false
	}
	r.mu.Lock()
	failed := r.failedDirect[node]
	r.mu.Unlock()
	if failed {
		return "", false
	}
	if h, ok := r.explicit[node]; ok && h != "" {
		if hostPartEquals(h, r.configuredHost) {
			return "", false
		}
		return h, true
	}
	if !r.allowDiscovery || ctx == nil {
		return "", false
	}
	h := r.discoveredMap(ctx)[node]
	if h == "" || hostPartEquals(h, r.configuredHost) {
		return "", false
	}
	return h, true
}

// MarkDirectRouteFailed records that dialing node's resolved direct address
// failed (TLS certificate verification or a dial-phase connection failure).
// Subsequent HostFor calls for that node yield no override for the rest of
// the process, so every later upload in the same request takes the proxied
// path at once instead of re-failing the same dial. Nil-safe.
func (r *NodeEndpointResolver) MarkDirectRouteFailed(node string) {
	if r == nil || node == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failedDirect == nil {
		r.failedDirect = make(map[string]bool)
	}
	r.failedDirect[node] = true
}

// discoveredMap returns the memoized /cluster/status name-to-IP map, fetching
// it on first use. The fetch runs outside the resolver lock so the stemcell
// replication fan-out's first concurrent lookups do not serialize behind it;
// late arrivals wait on the in-flight fetch and share its outcome. A genuine
// fetch failure is memoized as nil for the process lifetime, but a fetch that
// died with its caller's context (cancellation, deadline) leaves the outcome
// open so the next caller retries with a live context.
func (r *NodeEndpointResolver) discoveredMap(ctx context.Context) map[string]string {
	for {
		r.mu.Lock()
		if r.discoveryDone {
			m := r.discovered
			r.mu.Unlock()
			return m
		}
		if gate := r.discoveryGate; gate != nil {
			r.mu.Unlock()
			select {
			case <-gate:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		gate := make(chan struct{})
		r.discoveryGate = gate
		r.mu.Unlock()

		m, err := ClusterNodeAddressMap(ctx, r.c)

		r.mu.Lock()
		r.discoveryGate = nil
		switch {
		case err == nil:
			r.discovered = m
			r.discoveryDone = true
		case ctx.Err() != nil:
			// Not the cluster's fault; leave the outcome open.
		default:
			r.discoveryDone = true
			r.logger.Warn("node endpoints: cluster status discovery failed; uploads take the proxied path",
				log.Err(err))
		}
		close(gate)
		r.mu.Unlock()
		if err != nil {
			return nil
		}
		return m
	}
}

// hostPartEquals reports whether the host part of addr (which may carry a
// :port suffix) equals host. Comparison is textual only; a hostname never
// equals an IP here, so a pve.node_endpoints entry that names the endpoint
// node by an address textually different from pve.host (a hostname mapped to
// its own IP, or vice versa) still resolves to a direct, pinned dial rather
// than "no override". Under verify_ssl: true that pinned dial verifies
// against the mapped address, not pve.host, and stock PVE node certificates
// carry DNS SANs for the node's hostname and FQDN, not its bare IP — so this
// is not the harmless no-hop case it looks like: the handshake fails,
// MarkDirectRouteFailed disables the route for the rest of the process
// (self-healing, but only after a failed attempt and a Warn log naming a
// "failed" dial to the operator's own endpoint).
func hostPartEquals(addr, host string) bool {
	if addr == host {
		return true
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h == host
	}
	return false
}

// uploadHostKey carries the CPI-chosen direct upload host in a context. The
// SDK's own override key is unexported, so tests and fakes assert against
// this CPI-owned twin instead.
type uploadHostKey struct{}

// UploadContext returns ctx with the SDK per-request host override applied
// for host, plus a CPI-owned copy of the same value readable via
// UploadHostFromContext. The override presumes TLS fingerprint pinning stays
// unset in buildTransportOpts: enabling any pinning option makes the SDK
// reject overridden requests with ErrHostOverrideFingerprint.
func UploadContext(ctx context.Context, host string) context.Context {
	ctx = sdkclient.WithHost(ctx, host)
	return context.WithValue(ctx, uploadHostKey{}, host)
}

// UploadHostFromContext returns the direct upload host stamped by
// UploadContext, or "" when the context carries none.
func UploadHostFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if h, ok := ctx.Value(uploadHostKey{}).(string); ok {
		return h
	}
	return ""
}

// IsTLSCertVerificationFailure reports whether err (or its chain) is a TLS
// certificate verification failure: an unknown authority, a hostname or IP
// SAN mismatch, or an otherwise invalid certificate. Upload paths use it to
// fall back from a direct node dial to the configured endpoint when the node
// certificate does not cover the routed address; nothing was sent when the
// handshake failed, so the un-pinned re-attempt is safe.
func IsTLSCertVerificationFailure(err error) bool {
	if err == nil {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return true
	}
	var systemRoots x509.SystemRootsError
	if errors.As(err, &systemRoots) {
		return true
	}
	// SDK transports flatten dial errors into their own typed errors whose
	// chains do not always preserve the crypto/tls error value, so keep a
	// narrow textual match on the standard library's stable phrasings.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "certificate is valid for") ||
		strings.Contains(msg, "certificate signed by unknown authority") ||
		strings.Contains(msg, "failed to verify certificate")
}

// IsDirectDialFailure reports whether err (or its chain) died before a
// connection to the target existed: a DNS resolution failure or a dial-phase
// error (connection refused, unreachable host or network, dial timeout).
// Upload paths use it alongside IsTLSCertVerificationFailure and
// IsRoutedHandshakeFailure to fall back from an unusable direct node address
// to the configured endpoint; nothing was sent when the dial failed, so the
// un-pinned re-attempt is safe. A drop on an established connection (EOF
// mid-request) deliberately stays out: that is the transient fault the
// pinned retry budget exists to ride. See IsRoutedHandshakeFailure for the
// narrower, more aggressive predicate that upload fallback sites also apply,
// which does treat some read-phase failures as fallback-eligible.
func IsDirectDialFailure(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// IsRoutedHandshakeFailure reports whether err (or its chain) is a TLS
// handshake-phase failure that IsTLSCertVerificationFailure does not already
// cover, or a read-phase network error observed while establishing a
// connection. Two shapes:
//
//   - A non-verification TLS handshake failure: the peer answered the dial
//     but does not speak the expected protocol at all (a plain-TCP listener
//     on the port, an L4 load balancer, a plaintext pveproxy on 8007), or
//     speaks TLS but aborts the handshake (a protocol alert). These surface
//     as tls.RecordHeaderError or tls.AlertError, or — once the SDK's error
//     wrapping is accounted for — as a "tls: ..." prefixed message that
//     neither typed check catches.
//
//   - A read-phase net.OpError. Once the TCP connect succeeds, both "the
//     handshake stalled or was reset before completing" and "an established
//     connection dropped mid-response" surface as the identical
//     net.OpError{Op: "read"} shape; nothing on the wire distinguishes them.
//     IsDirectDialFailure deliberately excludes this shape everywhere (see
//     its doc comment): an established-connection drop is the transient
//     fault the pinned retry budget exists to ride, and that exclusion must
//     hold for the general-purpose predicate.
//
// This predicate exists for a narrower use: upload call sites invoke it only
// once a request already carries a direct-to-node host override
// (UploadHostFromContext(ctx) != ""), at the same fallback-classification
// point that already checks IsTLSCertVerificationFailure and
// IsDirectDialFailure. On that boundary the two outcomes of guessing wrong
// are not symmetric: routing a genuinely-established-connection drop to the
// un-pinned fallback still succeeds (the proxied path was always going to
// work), while leaving a genuine handshake stall pinned burns the whole
// retry budget against a route that will never complete (see the PVE API
// transport's lack of a default handshake deadline, documented at
// client.go's buildTransportOpts). Ambiguity therefore resolves toward
// fallback here, deliberately more aggressively than IsDirectDialFailure.
//
// nil → false.
func IsRoutedHandshakeFailure(err error) bool {
	if err == nil {
		return false
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "read" {
		return true
	}
	// Textual safety net for TLS handshake failures the SDK's transport
	// re-wraps into a shape errors.As cannot see through (e.g. a
	// non-standard-library HTTP transport, or a future crypto/tls error
	// type). Every crypto/tls error message is "tls: ..."-prefixed; a false
	// positive here still only widens the fallback-eligible set at the same
	// bounded call sites described above.
	return strings.Contains(strings.ToLower(err.Error()), "tls:")
}

// IsRoutedUploadFallbackEligible reports whether err, observed on a
// direct-to-node upload dial, should fall back to the configured (proxied)
// endpoint: the OR of IsTLSCertVerificationFailure, IsDirectDialFailure, and
// IsRoutedHandshakeFailure. Upload call sites already gate on
// directHost != "" (equivalently, UploadHostFromContext(ctx) != "") before
// calling this, so it is deliberately not context-aware itself. Extracted
// as one call so each fallback site's own control flow — reopening the
// upload un-pinned, memoizing the route as failed — stays the readable part
// of the function.
func IsRoutedUploadFallbackEligible(err error) bool {
	return IsTLSCertVerificationFailure(err) || IsDirectDialFailure(err) || IsRoutedHandshakeFailure(err)
}
