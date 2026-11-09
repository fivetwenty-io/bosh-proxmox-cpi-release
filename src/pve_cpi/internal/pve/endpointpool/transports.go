package endpointpool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// defaultHeaderDeadline is the response-header deadline on an endpoint's
// idempotent clone in a pool of more than one endpoint. Go starts the timer
// only after the request is fully written, so it measures how long the node
// takes to start answering.
const defaultHeaderDeadline = 60 * time.Second

// Connect caps for a cold-start candidate's clones. A candidate always uses
// them, because no request goes to a candidate today and no behavior of today's
// needs keeping there.
const (
	candidateDialCap      = 5 * time.Second
	candidateHandshakeCap = 10 * time.Second
)

// connectBoundKey carries, on a request's context, the latest moment a
// connection dialed for the request may finish being set up.
type connectBoundKey struct{}

// connectContext returns ctx carrying the send-by deadline that guards the
// request with the given method and URL path as the bound on any connection
// the request's dial makes. Go keeps a request context's values on the dial
// context and drops its cancellation, so the bound has to travel as a value,
// and the pool's dialer applies it as the dial and handshake deadline.
//
// When no marker guards the request, a bound that an earlier hop left on ctx
// is cleared, so a readback on the same context is never held to another
// request's deadline.
func connectContext(ctx context.Context, method, path string) context.Context {
	deadline, ok := SendBy(ctx, method, path)
	if ok {
		return context.WithValue(ctx, connectBoundKey{}, deadline)
	}
	if _, had := ctx.Value(connectBoundKey{}).(time.Time); had {
		return context.WithValue(ctx, connectBoundKey{}, time.Time{})
	}
	return ctx
}

// connectBound returns the bound connectContext put on ctx, or the zero time
// when there is none.
func connectBound(ctx context.Context) time.Time {
	t, _ := ctx.Value(connectBoundKey{}).(time.Time)
	return t
}

// handshakeTimeoutError is the error a handshake gets when the base transport's
// TLSHandshakeTimeout expires. It matches the text and the net.Error behavior
// of the error Go's own transport returns, so a timed-out handshake reads and
// classifies as it did before the pool owned the dial.
type handshakeTimeoutError struct{}

func (handshakeTimeoutError) Error() string   { return "net/http: TLS handshake timeout" }
func (handshakeTimeoutError) Timeout() bool   { return true }
func (handshakeTimeoutError) Temporary() bool { return true }

// errWrongAddress is the cause when an owned transport is asked to dial an
// address other than the one it was built for. The pool never routes a
// request that way, so this guards against a defect sending a request, and
// its credential, through another endpoint's verifier.
var errWrongAddress = errors.New("owned transport asked to dial another address")

// transportSpec describes the clones to build for one address.
type transportSpec struct {
	// addr is the canonical host:port the clones dial.
	addr string
	// verify, when set, replaces Go's certificate verification with the
	// pinned callback of a discovered endpoint or a candidate.
	verify func(tls.ConnectionState) error
	// headerDeadline, when positive, is the ResponseHeaderTimeout of the
	// idempotent clone. When it is zero, one clone serves every request.
	headerDeadline time.Duration
	// dialCap and handshakeCap, when positive, bound the TCP dial and the TLS
	// handshake on top of the base transport's own settings.
	dialCap      time.Duration
	handshakeCap time.Duration
}

// ownedTransports are the clones of the base transport the pool owns for one
// address. The idempotent clone carries the response-header deadline and the
// plain clone carries none. When the pool has no header deadline they are the
// same clone.
type ownedTransports struct {
	addr       string
	idempotent *http.Transport
	plain      *http.Transport
}

// pick returns the clone with the header deadline when withDeadline is true,
// and the clone without one otherwise.
func (o *ownedTransports) pick(withDeadline bool) *http.Transport {
	if withDeadline {
		return o.idempotent
	}
	return o.plain
}

// closeIdle closes the idle connections of both clones.
func (o *ownedTransports) closeIdle() {
	o.plain.CloseIdleConnections()
	if o.idempotent != o.plain {
		o.idempotent.CloseIdleConnections()
	}
}

// newOwnedTransports builds the clones for spec. The base must already have
// passed checkBase.
func newOwnedTransports(base *http.Transport, spec transportSpec) (*ownedTransports, error) {
	plain, err := newClone(base, spec, 0)
	if err != nil {
		return nil, err
	}
	o := &ownedTransports{addr: spec.addr, idempotent: plain, plain: plain}
	if spec.headerDeadline > 0 {
		if o.idempotent, err = newClone(base, spec, spec.headerDeadline); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// newClone clones base for one address. The clone dials through its own
// connectDialer, speaks only HTTP/1.1, and carries headerDeadline as its
// ResponseHeaderTimeout.
func newClone(base *http.Transport, spec transportSpec, headerDeadline time.Duration) (*http.Transport, error) {
	host, _, err := net.SplitHostPort(spec.addr)
	if err != nil {
		return nil, fmt.Errorf("endpoint pool: address %q: %w", spec.addr, err)
	}
	var cfg *tls.Config
	if base.TLSClientConfig != nil {
		cfg = base.TLSClientConfig.Clone()
	} else {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// The SDK never sets ServerName, so net/http would derive it from the URL
	// host. The pool's dialer does the TLS itself, so it sets the name from
	// the host it dials, which drives SNI and the name check alike.
	cfg.ServerName = host
	cfg.NextProtos = slices.DeleteFunc(slices.Clone(cfg.NextProtos), func(p string) bool { return p == h2Proto })
	if spec.verify != nil {
		// #nosec G402 -- Go's built-in verification is replaced, not skipped:
		// VerifyConnection runs on every handshake, resumptions included, and
		// enforces the chain, pin, and name rules before any request byte.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = spec.verify
		cfg.ClientSessionCache = nil
		cfg.SessionTicketsDisabled = true
	}

	d := &connectDialer{
		addr:             spec.addr,
		tlsConfig:        cfg,
		handshakeTimeout: base.TLSHandshakeTimeout,
		dialCap:          spec.dialCap,
		handshakeCap:     spec.handshakeCap,
	}
	if base.DialContext != nil {
		d.dial = base.DialContext
	} else {
		// The zero dialer is what net/http uses when DialContext is nil.
		d.dial = (&net.Dialer{}).DialContext
	}

	t := base.Clone()
	t.DialContext = d.dialPlain
	t.DialTLSContext = d.dialTLS
	t.TLSClientConfig = cfg
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	t.Protocols = nil
	t.ResponseHeaderTimeout = headerDeadline
	if err := assertHTTP1(t); err != nil {
		return nil, err
	}
	return t, nil
}

// connectDialer dials one address for one owned clone and tags every failure
// with the connect phase it happened in. Go writes no request byte before it
// has a connection, so a final error that carries the tag proves the request
// was never sent.
type connectDialer struct {
	addr             string
	dial             func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig        *tls.Config
	handshakeTimeout time.Duration
	dialCap          time.Duration
	handshakeCap     time.Duration
}

// dialPlain is the clone's DialContext, which net/http uses for an http URL.
func (d *connectDialer) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel, bound, err := d.begin(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer cancel()
	conn, err := d.dialTCP(ctx, network, addr)
	if err != nil {
		return nil, d.fail(dialPhase(err), err, bound)
	}
	return conn, nil
}

// dialTLS is the clone's DialTLSContext. It dials with the base transport's
// dialer and performs the handshake with the clone's TLS config and the base
// transport's handshake timeout, so the connection is set up exactly as Go's
// own transport would set it up, with every failure tagged.
func (d *connectDialer) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel, bound, err := d.begin(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer cancel()
	raw, err := d.dialTCP(ctx, network, addr)
	if err != nil {
		return nil, d.fail(dialPhase(err), err, bound)
	}
	conn := tls.Client(raw, d.tlsConfig)
	hctx, hcancel, timed := d.handshakeContext(ctx)
	defer hcancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		if timed && ctx.Err() == nil && hctx.Err() != nil {
			err = handshakeTimeoutError{}
		}
		return nil, d.fail(handshakePhase(err), err, bound)
	}
	return conn, nil
}

// begin checks the address and the send-by bound and returns the context the
// connection is set up under. A bound that has already passed refuses the
// dial outright, so no connection, and no request, is started for it.
func (d *connectDialer) begin(ctx context.Context, addr string) (context.Context, context.CancelFunc,
	time.Time, error,
) {
	bound := connectBound(ctx)
	if got, err := canonicalAddress(addr); err != nil || got != d.addr {
		return nil, nil, bound, &ConnectPhaseError{
			Phase:    PhaseDial,
			Endpoint: d.addr,
			Err:      fmt.Errorf("%w: %s", errWrongAddress, addr),
		}
	}
	if bound.IsZero() {
		ctx, cancel := context.WithCancel(ctx)
		return ctx, cancel, bound, nil
	}
	if !time.Now().Before(bound) {
		return nil, nil, bound, &ConnectPhaseError{Phase: PhaseDial, Endpoint: d.addr, Err: ErrSendByPassed}
	}
	ctx, cancel := context.WithDeadline(ctx, bound)
	return ctx, cancel, bound, nil
}

// dialTCP runs the base dialer, under the dial cap when one is set.
func (d *connectDialer) dialTCP(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.dialCap > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.dialCap)
		defer cancel()
	}
	return d.dial(ctx, network, addr)
}

// handshakeContext returns the context for the handshake, bounded by the
// shorter of the base transport's handshake timeout and the handshake cap. It
// reports whether either applies, so a timed-out handshake can be told from
// one the send-by bound cut off.
func (d *connectDialer) handshakeContext(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	limit := d.handshakeTimeout
	if d.handshakeCap > 0 && (limit <= 0 || d.handshakeCap < limit) {
		limit = d.handshakeCap
	}
	if limit <= 0 {
		ctx, cancel := context.WithCancel(ctx)
		return ctx, cancel, false
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	return ctx, cancel, true
}

// fail tags err with its phase. When the send-by bound has passed by the time
// the setup failed, the request could not have been sent in time on any
// endpoint, so the error also wraps ErrSendByPassed. The walk checks for that
// before it treats the failure as the endpoint's fault, so a deadline the
// caller set never demotes a healthy endpoint.
func (d *connectDialer) fail(phase Phase, err error, bound time.Time) error {
	if !bound.IsZero() && !time.Now().Before(bound) {
		err = fmt.Errorf("%w: %w", ErrSendByPassed, err)
	}
	return &ConnectPhaseError{Phase: phase, Endpoint: d.addr, Err: err}
}

// dialPhase classifies a failed TCP dial. A failed name lookup is PhaseDNS,
// and every other failure is PhaseDial.
func dialPhase(err error) Phase {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return PhaseDNS
	}
	return PhaseDial
}

// handshakePhase classifies a failed TLS handshake. A refusal from a
// discovered endpoint's pinned callback is PhasePin. A certificate that the
// chain or name check refused is PhaseTLSVerify. Everything else, such as a
// reset, a garbled record, or a timeout, is PhaseTLSHandshake.
func handshakePhase(err error) Phase {
	if errors.Is(err, ErrPinMismatch) {
		return PhasePin
	}
	var (
		verifyErr    *tls.CertificateVerificationError
		unknownAuth  x509.UnknownAuthorityError
		hostnameErr  x509.HostnameError
		invalidErr   x509.CertificateInvalidError
		systemRootsE x509.SystemRootsError
	)
	switch {
	case errors.As(err, &verifyErr),
		errors.As(err, &unknownAuth),
		errors.As(err, &hostnameErr),
		errors.As(err, &invalidErr),
		errors.As(err, &systemRootsE):
		return PhaseTLSVerify
	}
	return PhaseTLSHandshake
}

// canonicalAddress returns addr as a canonical host:port, with the host
// lowercased and an IP literal in its standard form, so two spellings of one
// endpoint compare equal. It refuses an address without a port, a port outside
// 1 to 65535, an empty host, and a host with characters no DNS name or IP
// literal has.
func canonicalAddress(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("address %q is not host:port: %w", addr, err)
	}
	return canonicalHostPort(host, port)
}

// canonicalHostPort joins a host and a port into a canonical address, with
// the checks canonicalAddress describes.
func canonicalHostPort(host, port string) (string, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("port %q is not a number from 1 to 65535", port)
	}
	if host == "" {
		return "", errors.New("the host is empty")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return net.JoinHostPort(ip.String(), port), nil
	}
	host = strings.ToLower(host)
	if len(host) > 253 {
		return "", fmt.Errorf("host %q is longer than 253 characters", host)
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' && r != '_' {
			return "", fmt.Errorf("host %q has a character a DNS name cannot have", host)
		}
	}
	return net.JoinHostPort(host, port), nil
}
