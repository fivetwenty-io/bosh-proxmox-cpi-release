package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"time"
)

type sendByKey struct{}

// sendByMarker is one send-by deadline and the request it guards. Markers
// chain, so a second WithSendBy on the same context for another request does
// not hide the first.
type sendByMarker struct {
	deadline time.Time
	method   string
	path     string
	parent   *sendByMarker
}

// APIPrefix is the path the SDK puts in front of every API route. A request's
// URL path carries it, so the failover transport sees "/api2/json/pools" for
// the route the spec writes as "/pools".
const APIPrefix = "/api2/json"

// apiRelative returns path without the leading APIPrefix, so a marker written
// as "/pools" and a request path of "/api2/json/pools" compare equal. A path
// that does not start with the prefix followed by a slash comes back as is.
func apiRelative(path string) string {
	if rest, ok := strings.CutPrefix(path, APIPrefix+"/"); ok {
		return "/" + rest
	}
	return path
}

func (m *sendByMarker) matches(method, path string) bool {
	return strings.EqualFold(m.method, method) && m.path == apiRelative(path)
}

// WithSendBy returns a context that carries a send-by deadline for the request
// with the given method and URL path. The failover transport checks the
// deadline immediately before it hands the request to a connection, on every
// hop and for a request that reuses a connection as for one that dials, and it
// uses the deadline to bound connection setup. A request that fails either
// check is refused with ErrSendByPassed and sends no byte. The deadline does not
// guard the write itself, so a stall between the last check and the write, such
// as a paused process, can send the request late. A caller that needs the
// request to start writing by a time sets the deadline earlier than that time
// by a margin that absorbs such a stall.
//
// The marker names the one request it guards, so a read on the same context,
// such as a readback after the guarded call, ignores it. The path is the
// API-relative route without the query string, such as "/pools". A leading
// APIPrefix on the marker's path or on the request's path is ignored, so the
// marker matches the "/api2/json/pools" path that a transport sees. A marker with an empty method or path never
// matches anything. When the context already carries markers they stay, and a
// request that matches several is held to the earliest deadline.
func WithSendBy(ctx context.Context, deadline time.Time, method, path string) context.Context {
	if method == "" || path == "" {
		return ctx
	}
	parent, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	return context.WithValue(ctx, sendByKey{}, &sendByMarker{
		deadline: deadline,
		method:   strings.ToUpper(method),
		path:     apiRelative(path),
		parent:   parent,
	})
}

// SendBy returns the send-by deadline that applies to the request with the
// given method and path, which may carry the APIPrefix. It reports false when the context carries no
// marker that matches the request. The method comparison ignores case.
func SendBy(ctx context.Context, method, path string) (time.Time, bool) {
	var (
		found    bool
		earliest time.Time
	)
	m, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	for ; m != nil; m = m.parent {
		if !m.matches(method, path) {
			continue
		}
		if !found || m.deadline.Before(earliest) {
			earliest = m.deadline
			found = true
		}
	}
	return earliest, found
}

// CheckSendBy returns ErrSendByPassed when the context carries a send-by
// deadline that matches the request and now is at or after it. A deadline that
// has been reached counts as passed, so the check fails closed. It
// returns nil for a request no marker guards.
func CheckSendBy(ctx context.Context, now time.Time, method, path string) error {
	deadline, ok := SendBy(ctx, method, path)
	if !ok {
		return nil
	}
	if !now.Before(deadline) {
		return ErrSendByPassed
	}
	return nil
}

// certFailureAfterSendBy counts a certificate refusal that finished after the
// request's send-by bound had passed. The dialer wraps every connect-phase
// failure that ends after the bound in ErrSendByPassed, so the request reports
// the send-by refusal, and its outcome demotes nothing, because the bound is
// the caller's own and a dial or handshake it cut short says nothing about the
// endpoint. A certificate refusal is different. The endpoint presented the
// certificate and the pool refused it, whenever the bound passed, so it gets
// every effect it gets without the bound:
//
//   - A discovered endpoint that failed its pin becomes ineligible, a refresh
//     becomes due, the Warn line is logged, and a pin mark is written.
//   - A configured endpoint that failed TLS verification is demoted, gets a
//     tls_verify mark when it is its first failure in this process, and is
//     logged at Error, once per endpoint.
//   - The accepted cold-start candidate that failed TLS verification is
//     demoted and revoked, which takes it out of every later walk, routed
//     request, and tether, and the revocation is logged once at Error. It gets
//     no mark, as without the bound.
//
// The cause the lines, the failure note, and the mark see is the certificate
// refusal without the send-by wrapping. Only the refusal phase each kind can
// meet counts, which is the pin for a discovered endpoint and TLS verification
// for the others, and only when the refusal would blame the endpoint without
// the bound. Every other error, a late dial or handshake failure among them,
// is left alone, and so is a refusal the request did not route through one of
// the pool's endpoints, which ep is nil for.
func (p *Pool) certFailureAfterSendBy(ep *endpoint, err error) {
	if ep == nil {
		return
	}
	cause := lateCertFailure(ep.kind, err)
	if cause == nil {
		return
	}
	p.demote(ep)
	p.walk.note(ep, noteOf(nil, cause, string(cause.Phase)))
	if ep.kind == kindCandidate {
		p.candidateFailed(ep, cause.Phase, cause)
		return
	}
	p.connectFailed(ep, cause.Phase, cause)
	if ep.kind == KindConfigured {
		p.alertVerify(ep, cause, false)
	}
}

// lateCertFailure returns the certificate refusal inside err, without the
// send-by wrapping, when err is a refusal that certFailureAfterSendBy counts
// against an endpoint of the given kind, and nil otherwise. err must wrap
// ErrSendByPassed and carry the refusal phase of the kind, and the refusal
// left once the send-by wrapping is taken off must be one blamesEndpoint
// counts, which rules out a refusal the pool made itself. A pin failure must
// also wrap ErrPinMismatch, which every pin failure of the verifier does.
func lateCertFailure(kind Kind, err error) *ConnectPhaseError {
	if !errors.Is(err, ErrSendByPassed) {
		return nil
	}
	cpe, ok := AsConnectPhase(err)
	if !ok || cpe.Phase != refusalPhase(kind) || (cpe.Phase == PhasePin && !errors.Is(err, ErrPinMismatch)) {
		return nil
	}
	inner := withoutSendBy(cpe.Err)
	if inner == nil {
		return nil
	}
	cause := &ConnectPhaseError{Phase: cpe.Phase, Endpoint: cpe.Endpoint, Err: inner}
	if !blamesEndpoint(cause) {
		return nil
	}
	return cause
}

// refusalPhase is the phase in which an endpoint of the given kind refuses a
// certificate. A discovered endpoint's verifier refuses one as a pin failure,
// while a configured endpoint's TLS config and a cold-start candidate's
// verifier refuse one as a verification failure.
func refusalPhase(kind Kind) Phase {
	if kind == KindDiscovered {
		return PhasePin
	}
	return PhaseTLSVerify
}

// withoutSendBy returns err without the ErrSendByPassed the dialer joined to
// it, or nil when nothing but the send-by refusal is left. The dialer wraps a
// late failure as fmt.Errorf("%w: %w", ErrSendByPassed, err), whose Unwrap
// returns both. Any other shape falls back to the certificate verification
// error inside err, which every certificate refusal of the pool's dialers
// carries.
func withoutSendBy(err error) error {
	if err == nil || err == ErrSendByPassed { //nolint:errorlint // the bare sentinel carries no cause
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint // only the outer join is unwrapped
		var rest []error
		for _, e := range joined.Unwrap() {
			if e != nil && e != ErrSendByPassed { //nolint:errorlint // dropping the sentinel itself
				rest = append(rest, e)
			}
		}
		if len(rest) == 1 {
			return rest[0]
		}
	}
	var refused *tls.CertificateVerificationError
	if errors.As(err, &refused) {
		return refused
	}
	return nil
}
