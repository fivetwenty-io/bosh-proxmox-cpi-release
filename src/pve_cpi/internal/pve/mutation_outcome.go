package pve

import (
	"errors"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// IsMutationRefusedOrUnsent reports whether err, the error a mutation's own
// request returned, proves that PVE did not act on the request. A caller that
// wants to escalate to a second, different mutation, such as a hard reset
// after a graceful reboot, may do so only on this proof, because a request
// that may have landed could have started a task that is still running.
//
// Three kinds of error count. PVE answered with a status that AnsweredStatus
// accepts, such as a 4xx, a 500, or pveproxy's 595 for a next hop it could not
// open. The endpoint pool tagged the failure as a connect-phase error, which
// proves no byte left the machine. Or the dial failed before any connection
// existed, which the SDK reports without the pool's tag when the pool is not
// installed. The SDK sends a mutation once and never retries it on its own, so
// that dial failure is the request's only attempt.
//
// A ticket login that failed around the request counts too, whatever the
// login's own error was (see isTicketLoginFailure). The SDK logs in before it
// sends the request, so a failed login means the request was never sent, and
// the forced login after a 401 means PVE refused the request. The login's
// error can carry a MaybeSentError from the login request, which says nothing
// about the mutation, so this check comes before that one.
//
// Otherwise an error the endpoint pool marked with endpointpool.MaybeSentError
// never counts, whatever it wraps, and neither does any other transport
// fault, a timeout, a cancelled context, or a gateway or relay status such as
// 596. Those leave the outcome unknown. nil reports false.
func IsMutationRefusedOrUnsent(err error) bool {
	if err == nil {
		return false
	}
	if isTicketLoginFailure(err) {
		return true
	}
	if endpointpool.IsMaybeSent(err) {
		return false
	}
	if _, answered := AnsweredStatus(err); answered {
		return true
	}
	return endpointpool.IsConnectPhase(err) || IsDirectDialFailure(err)
}

// ticketLoginFailurePrefixes are the texts the SDK puts in front of a ticket
// login error that ends a request. "auto-login failed: " wraps the login the
// SDK runs before it sends a request without a valid ticket.
// "failed to refresh authentication: " wraps the forced login after PVE
// answered the request with a 401, and the SDK returns it without sending the
// request again.
var ticketLoginFailurePrefixes = []string{"auto-login failed: ", "failed to refresh authentication: "}

// isTicketLoginFailure reports whether some error in err's chain starts with
// one of ticketLoginFailurePrefixes, which only the SDK's own wrapping of a
// failed ticket login does. The SDK has no typed marker for these errors, so
// the text is the only sign of them. It follows single wrapping only and
// stops at a joined error, whose text starts with its first part's text,
// because another of its parts could be a request that was sent.
func isTicketLoginFailure(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		// A MaybeSentError repeats its wrapped error's text, so its text says
		// nothing about a login, and the request may have been sent.
		//nolint:errorlint // this link only; errors.As would also see a MaybeSentError further down the chain
		if _, maybeSent := e.(*endpointpool.MaybeSentError); maybeSent {
			return false
		}
		if _, joined := e.(interface{ Unwrap() []error }); joined {
			return false
		}
		for _, prefix := range ticketLoginFailurePrefixes {
			if strings.HasPrefix(e.Error(), prefix) {
				return true
			}
		}
	}
	return false
}
