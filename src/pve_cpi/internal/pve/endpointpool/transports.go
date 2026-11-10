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

// errPlaintextRefused is the cause when an owned transport that verifies
// certificates with its own callback, which a discovered endpoint and a
// cold-start candidate have, is asked for a plain http connection. Such a
// connection would carry the request, and its credential, without any
// certificate check, so no address reaches it without its pin.
var errPlaintextRefused = errors.New("owned transport refuses plain http to an endpoint whose certificate it checks")

// blamesEndpoint reports whether err is a connect-phase failure that counts
// against the endpoint it names, so the walk may demote the endpoint and mark
// it down. It is false for nil, for an error without the connect-phase tag,
// and for the refusals the pool makes itself:
//
//   - ErrSendByPassed, a send-by deadline that had passed before the dial
//     started or passed while the connection was being set up, which is the
//     caller's own deadline and says nothing about the endpoint.
//   - errWrongAddress, an owned transport asked to dial another address, which
//     is a routing defect in the pool.
//   - errPlaintextRefused, a plain http connection to an endpoint whose
//     certificate the pool checks itself, which is a routing defect too.
//   - errIneligible, a request the pool would not send to a discovered
//     endpoint that already failed its pin, which that failure already
//     counted against the endpoint.
//   - errDropped, a request the pool would not send to a discovered endpoint
//     that DropDiscovered took out, which blames nothing on the endpoint.
//
// Each of those refusals still carries the connect-phase tag, because no byte
// of the request left the machine, so IsConnectPhase and provesNeverSent treat
// them as they treat any connect-phase failure.
func blamesEndpoint(err error) bool {
	if !IsConnectPhase(err) {
		return false
	}
	return !errors.Is(err, ErrSendByPassed) && !errors.Is(err, errWrongAddress) &&
		!errors.Is(err, errPlaintextRefused) && !errors.Is(err, errIneligible) &&
		!errors.Is(err, errDropped)
}

// provesNeverSent reports whether err, the final error a request got from an
// owned transport, proves that no attempt at the request was ever written. It
// needs the connect-phase tag, and it needs a request for which neither Go's
// transport nor the http.Client could have written an earlier attempt before
// the one that failed to connect. Two things can write such an attempt.
//
//   - Go's transport replays a request it counts as replayable after a written
//     attempt dies on a reused connection, and the replay's dial is what
//     carries the tag. Request.isReplayable counts GET, HEAD, OPTIONS, and
//     TRACE, and any request that carries an Idempotency-Key or
//     X-Idempotency-Key header, when its body is empty or GetBody can rebuild
//     it.
//   - The http.Client sends each leg of a redirect through the transport after
//     the previous leg was written and answered, and it sets Response on every
//     leg after the first.
//
// An error marked with MaybeSentError is never proof, whatever it wraps. A
// false result for a request that failed in the connect phase means that
// the request may have been sent, so a mutation must be treated as sent. A GET
// is always false, because Go may have replayed it, which the walk accepts for
// an idempotent request.
func provesNeverSent(req *http.Request, err error) bool {
	if req == nil || !IsConnectPhase(err) || IsMaybeSent(err) {
		return false
	}
	return req.Response == nil && !goMayReplay(req)
}

// goMayReplay reports whether Go's transport counts req as replayable after a
// written attempt, mirroring Request.isReplayable. Header presence is checked
// on the canonical keys, as Go checks it.
func goMayReplay(req *http.Request) bool {
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	_, key := req.Header["Idempotency-Key"]
	_, xkey := req.Header["X-Idempotency-Key"]
	return key || xkey
}

// transportSpec describes the clones to build for one address.
type transportSpec struct {
	// addr is the canonical host:port the clones dial.
	addr string
	// serverName is the host as the operator, discovery, or a routed URL
	// spelled it, which the clones send as SNI by default. Empty means the
	// host of addr. A dial for another spelling of the same address sends
	// that spelling, as Go's own transport does.
	serverName string
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
	if spec.serverName != "" {
		host = spec.serverName
	}
	var cfg *tls.Config
	if base.TLSClientConfig != nil {
		cfg = base.TLSClientConfig.Clone()
	} else {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// The SDK never sets ServerName, so net/http would derive it from the URL
	// host as written. The pool's dialer does the TLS itself, so it sets the
	// name from the host it dials, in the spelling it was given, which drives
	// SNI and the name check alike.
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
		tlsOnly:          spec.verify != nil,
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
// has a connection, so the attempt that gets a tagged error was never sent.
// Whether an earlier attempt of the same request was is what provesNeverSent
// decides.
type connectDialer struct {
	addr string
	// tlsOnly refuses plain http connections, for a clone whose certificate
	// check is the pool's own callback.
	tlsOnly          bool
	dial             func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig        *tls.Config
	handshakeTimeout time.Duration
	dialCap          time.Duration
	handshakeCap     time.Duration
}

// dialPlain is the clone's DialContext, which net/http uses for an http URL.
// A clone with its own certificate callback refuses it after the address
// check, so a discovered endpoint or a candidate is never reached without its
// certificate being checked.
func (d *connectDialer) dialPlain(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel, bound, err := d.begin(ctx, addr)
	if err != nil {
		return nil, err
	}
	if d.tlsOnly {
		cancel()
		return nil, &ConnectPhaseError{
			Phase:    PhaseDial,
			Endpoint: d.addr,
			Err:      fmt.Errorf("%w: %s", errPlaintextRefused, addr),
		}
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
	conn := tls.Client(raw, d.configFor(addr))
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

// configFor returns the TLS config for a dial to addr, which begin has already
// checked is this dialer's address. When the dial's host is spelled other than
// the config's ServerName, such as a routed URL that capitalizes the name, the
// config is copied with that spelling, so SNI goes out as Go's own transport
// would send it.
func (d *connectDialer) configFor(addr string) *tls.Config {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == d.tlsConfig.ServerName {
		return d.tlsConfig
	}
	cfg := d.tlsConfig.Clone()
	cfg.ServerName = host
	return cfg
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
// endpoint, so the error also wraps ErrSendByPassed. blamesEndpoint is false
// for such an error, so a deadline the caller set never demotes a healthy
// endpoint or marks it down.
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
