package endpointpool

import (
	"errors"
	"net"
	"strings"
)

// ErrSendByPassed is returned when a request carries a send-by deadline that
// matches it and the deadline has passed before any byte could be written. No
// byte left the machine, so the request is not tethered. Callers match it with
// errors.Is, which sees it through the SDK's %w wrapping.
var ErrSendByPassed = errors.New("send-by deadline passed before the request could be sent")

// ErrTetherConflict is returned, before dispatch, for a mutation whose tether
// key was marked conflicted for the rest of the call. A key is conflicted when
// two endpoints each hold a request for it that may have been sent. The error
// is not transient, so no retry helper replays the mutation.
var ErrTetherConflict = errors.New("mutation refused because its tether key is conflicted on two endpoints")

// Phase names the connect-phase step that failed.
type Phase string

// The phases a ConnectPhaseError records.
const (
	// PhaseDial is a failed or timed-out TCP dial.
	PhaseDial Phase = "dial"
	// PhaseDNS is a failed name lookup.
	PhaseDNS Phase = "dns"
	// PhaseTLSHandshake is a TLS handshake that failed or timed out.
	PhaseTLSHandshake Phase = "tls_handshake"
	// PhaseTLSVerify is a handshake that failed certificate verification.
	PhaseTLSVerify Phase = "tls_verify"
	// PhasePin is a discovered endpoint whose certificate failed its pin.
	PhasePin Phase = "pin"
)

// Valid reports whether p is one of the defined phases.
func (p Phase) Valid() bool {
	switch p {
	case PhaseDial, PhaseDNS, PhaseTLSHandshake, PhaseTLSVerify, PhasePin:
		return true
	default:
		return false
	}
}

// ConnectPhaseError wraps a failure that happened while the pool's own dial or
// TLS code was establishing a connection. Go writes no request byte before it
// has a connection, so a final error that unwraps to a ConnectPhaseError
// proves the request was never sent.
type ConnectPhaseError struct {
	// Phase is the step that failed.
	Phase Phase
	// Endpoint is the host:port the connection was for. It may be empty.
	Endpoint string
	// Err is the underlying failure.
	Err error
}

func (e *ConnectPhaseError) Error() string {
	var b strings.Builder
	b.WriteString("connect phase ")
	b.WriteString(string(e.Phase))
	if e.Endpoint != "" {
		b.WriteString(" failed for ")
		b.WriteString(e.Endpoint)
	} else {
		b.WriteString(" failed")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying failure.
func (e *ConnectPhaseError) Unwrap() error { return e.Err }

// AsConnectPhase returns the ConnectPhaseError in err's chain, if any.
func AsConnectPhase(err error) (*ConnectPhaseError, bool) {
	var cpe *ConnectPhaseError
	if errors.As(err, &cpe) {
		return cpe, true
	}
	return nil, false
}

// IsConnectPhase reports whether err carries a ConnectPhaseError, which proves
// the request was never sent.
func IsConnectPhase(err error) bool {
	_, ok := AsConnectPhase(err)
	return ok
}

// MaybeSentError marks an error from a request that may have reached the
// endpoint, such as a reset or a drop after the connection was made. Its text
// is the wrapped error's text unchanged, so code that matches on the message
// and logs that print it behave as they did before the marker.
type MaybeSentError struct {
	// Err is the transport error.
	Err error
}

func (e *MaybeSentError) Error() string {
	if e.Err == nil {
		return "request may have been sent"
	}
	return e.Err.Error()
}

// Unwrap returns the transport error.
func (e *MaybeSentError) Unwrap() error { return e.Err }

// IsMaybeSent reports whether err carries a MaybeSentError.
func IsMaybeSent(err error) bool {
	var mse *MaybeSentError
	return errors.As(err, &mse)
}

// EndpointFailure is one hop of a walk that did not get an answer.
type EndpointFailure struct {
	// Endpoint is the host:port the hop tried.
	Endpoint string
	// Err is why the hop failed, either a connect-phase error or a header
	// deadline expiry.
	Err error
}

// EntryUnreachableError reports a walk in which every hop failed in the
// connect phase or on the header deadline. It lists each endpoint with its
// cause.
//
// It unwraps to one synthesized *net.OpError with Op "dial" whose own error
// joins the per-endpoint causes. The SDK stops at the first match of
// errors.As, so one dial error keeps its classification independent of the
// order of the causes, and the CPI's transient-transport check still sees a
// connection error. The exception is a walk in which every hop failed
// certificate verification. That one unwraps to the joined verification
// errors, so the SDK reports a certificate failure as it does today.
//
// Only the PhaseTLSVerify phase counts as a certificate verification failure
// here. A PhasePin failure is a discovered endpoint whose certificate
// changed outside its pin set, which makes a refresh due and heals on a later
// call, so a walk with a pin failure in it reads as a dial error and stays
// transient, whether the other hops failed verification, dialing, or pinning.
type EntryUnreachableError struct {
	// Attempts lists every hop in the order the walk tried it.
	Attempts []EndpointFailure
}

// NewEntryUnreachableError builds the error for a walk's failed hops. The
// slice is copied.
func NewEntryUnreachableError(attempts []EndpointFailure) *EntryUnreachableError {
	return &EntryUnreachableError{Attempts: append([]EndpointFailure(nil), attempts...)}
}

func (e *EntryUnreachableError) Error() string {
	if len(e.Attempts) == 0 {
		return "no Proxmox endpoint was eligible to try"
	}
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		cause := "unknown failure"
		if a.Err != nil {
			cause = a.Err.Error()
		}
		parts = append(parts, a.Endpoint+": "+cause)
	}
	return "no Proxmox endpoint answered (" + strings.Join(parts, "; ") + ")"
}

// Unwrap returns the synthesized dial error, or the joined verification errors
// when every hop failed certificate verification. It builds a fresh value on
// each call from the recorded attempts.
func (e *EntryUnreachableError) Unwrap() error {
	causes := make([]error, 0, len(e.Attempts))
	allVerify := len(e.Attempts) > 0
	for _, a := range e.Attempts {
		if a.Err == nil {
			allVerify = false
			continue
		}
		causes = append(causes, a.Err)
		if cpe, ok := AsConnectPhase(a.Err); !ok || cpe.Phase != PhaseTLSVerify {
			allVerify = false
		}
	}
	if allVerify {
		return errors.Join(causes...)
	}
	cause := errors.Join(causes...)
	if cause == nil {
		cause = errors.New("no endpoint was tried")
	}
	return &net.OpError{Op: "dial", Net: "tcp", Err: cause}
}

// Endpoints returns the endpoint of every hop, in the order tried.
func (e *EntryUnreachableError) Endpoints() []string {
	out := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		out = append(out, a.Endpoint)
	}
	return out
}
