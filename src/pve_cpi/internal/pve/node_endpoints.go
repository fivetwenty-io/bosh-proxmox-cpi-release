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

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
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
// must never fail the upload.
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

// discoveredMap returns the memoized /cluster/status name-to-IP map, fetching
// it on first use. The outcome (including a failed fetch, memoized as nil) is
// kept for the process lifetime.
func (r *NodeEndpointResolver) discoveredMap(ctx context.Context) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.discoveryDone {
		return r.discovered
	}
	r.discoveryDone = true
	m, err := ClusterNodeAddressMap(ctx, r.c)
	if err != nil {
		r.logger.Debug("node endpoints: cluster status discovery failed; uploads take the proxied path",
			log.Err(err))
		return nil
	}
	r.discovered = m
	return m
}

// hostPartEquals reports whether the host part of addr (which may carry a
// :port suffix) equals host. Comparison is textual only; a hostname never
// equals an IP here, which at worst pins an upload to the endpoint node's own
// address, a harmless no-hop dial.
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
