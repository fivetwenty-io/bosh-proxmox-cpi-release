package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
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
// An error that carries endpointpool.MaybeSentError changes three arms, and
// only for a fault from the upload's own POST (see UploadFaultPhase):
//
//   - The read arm returns false, because the connection may have dropped
//     after the body started.
//   - The "tls:" text arm returns false, for the same reason.
//   - The record-header arm accepts only a RecordHeaderError whose Conn field
//     is non-nil, which Go sets for the first record of a handshake that did
//     not look like TLS. The messages Go emits on an established connection
//     leave Conn nil and are rejected, because bytes may already have gone
//     out.
//
// The alert arm is unchanged. The dial and certificate arms of
// IsRoutedUploadFallbackEligible ignore the marker on purpose, because the
// spec lists them as proof that nothing was sent.
//
// A fault from the poll of an upload task that already finished ignores the
// marker; UploadFallbackEligible chooses between the two readings.
//
// nil → false.
func IsRoutedHandshakeFailure(err error) bool {
	return routedHandshakeFailure(err, true)
}

// routedHandshakeFailure is IsRoutedHandshakeFailure with the maybe-sent
// narrowing switched by honorMaybeSent. With honorMaybeSent false every arm
// answers as it did before the narrowing existed.
func routedHandshakeFailure(err error, honorMaybeSent bool) bool {
	if err == nil {
		return false
	}
	// The marker is read once so every arm answers from the same fact. When
	// the endpoint wrapper is installed it marks every routed error that is
	// not a connect-phase failure with endpointpool.MaybeSentError, and a
	// marked error may be a connection dropped after the upload's body
	// started, so the ambiguous arms may not send it to the proxied path,
	// which would upload the same bytes a second time. A client without the
	// wrapper never produces the marker, and these arms behave as they
	// always have.
	maybeSent := honorMaybeSent && endpointpool.IsMaybeSent(err)
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		// Conn is nil once a handshake finished or an alert was written, so
		// a marked error without it came from an established connection.
		return !maybeSent || recErr.Conn != nil
	}
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "read" {
		return !maybeSent
	}
	// Textual safety net for TLS handshake failures the SDK's transport
	// re-wraps into a shape errors.As cannot see through (e.g. a
	// non-standard-library HTTP transport, or a future crypto/tls error
	// type). See tlsTransportText for what counts as a match.
	return !maybeSent && tlsTransportText(err)
}

// tlsTextMarker finds "tls:" as the start of a crypto/tls message, either at
// the start of the text or after a space or colon, so a word that merely ends
// in "tls" does not match. A remote alert reads "remote error: tls: ...", which
// this covers through the colon.
var tlsTextMarker = regexp.MustCompile(`(^|[\s:])tls: `)

// tlsTransportText reports whether err's text carries a crypto/tls message and
// the error is a transport failure. An error that holds a PVE response (an
// API error or any of its subtypes) never counts, because the server answered
// and its body can quote "tls:" in a message that says nothing about this
// connection.
func tlsTransportText(err error) bool {
	if sdkerrors.IsAPIError(err) {
		return false
	}
	return tlsTextMarker.MatchString(strings.ToLower(err.Error()))
}

// UploadFaultPhase names the step of a direct-to-node upload that a fault came
// from, because the same fault means different things before and after the
// upload's POST completed.
type UploadFaultPhase int

const (
	// UploadPostPhase is a fault from the upload's own POST, or from any step
	// that holds no pending UPID. The POST may already have reached the routed
	// node, so a fallback could upload the same bytes twice, and only
	// IsRoutedUploadFallbackEligible's narrowed answer counts. It is the zero
	// value, so an unset phase fails closed.
	UploadPostPhase UploadFaultPhase = iota
	// UploadPollPhase is a fault from polling an upload task whose POST
	// already finished. A fallback from it only awaits the same UPID again
	// through the configured endpoint, so it can't duplicate anything and the
	// maybe-sent marker is ignored.
	UploadPollPhase
)

// UploadPhaseFor returns the phase a fault belongs to given the loop's pending
// UPID: a loop that holds one is polling, and one that doesn't is judging the
// POST.
func UploadPhaseFor(pendingUPID string) UploadFaultPhase {
	if pendingUPID != "" {
		return UploadPollPhase
	}
	return UploadPostPhase
}

// UploadFallbackEligible is the one decision the pinned task poll and both
// upload loops (the config drive and the stemcell upload) share, so they can't
// drift apart. It reports whether err, seen at phase of a direct-to-node
// upload, should drop the pin and fall back to the configured endpoint.
//
// For UploadPollPhase it accepts a certificate failure, a dial failure, a
// routed handshake failure, and a connect-phase tagged fault, and it ignores
// the maybe-sent marker. For every other phase it answers
// IsRoutedUploadFallbackEligible, which narrows under the marker. The poll-phase
// answer is a superset of the POST-phase one.
func UploadFallbackEligible(err error, phase UploadFaultPhase) bool {
	if phase == UploadPollPhase {
		return IsTLSCertVerificationFailure(err) || IsDirectDialFailure(err) ||
			routedHandshakeFailure(err, false) || endpointpool.IsConnectPhase(err)
	}
	return IsRoutedUploadFallbackEligible(err)
}

// IsRoutedUploadFallbackEligible reports whether err, observed on a
// direct-to-node upload dial, should fall back to the configured (proxied)
// endpoint: the OR of IsTLSCertVerificationFailure, IsDirectDialFailure, and
// IsRoutedHandshakeFailure, plus an error that carries the endpoint wrapper's
// connect-phase tag, which proves the request never left; a marked error that
// also carries the tag is not counted, because a maybe-sent marker is never
// proof. The certificate and dial arms ignore the marker on purpose, because
// the spec lists them as proof that nothing was sent. It judges a fault from
// the upload's own POST; a fault from polling the
// finished upload's task goes through UploadFallbackEligible with
// UploadPollPhase. Upload call sites already gate on
// directHost != "" (equivalently, UploadHostFromContext(ctx) != "") before
// calling this, so it is deliberately not context-aware itself. Extracted
// as one call so each fallback site's own control flow — reopening the
// upload un-pinned, memoizing the route as failed — stays the readable part
// of the function.
func IsRoutedUploadFallbackEligible(err error) bool {
	return IsTLSCertVerificationFailure(err) || IsDirectDialFailure(err) || IsRoutedHandshakeFailure(err) ||
		(endpointpool.IsConnectPhase(err) && !endpointpool.IsMaybeSent(err))
}
