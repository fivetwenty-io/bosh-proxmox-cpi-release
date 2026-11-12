package pve

import (
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
// An error the endpoint pool marked with endpointpool.MaybeSentError never
// counts, whatever it wraps, and neither does any other transport fault, a
// timeout, a cancelled context, or a gateway or relay status such as 596.
// Those leave the outcome unknown. nil reports false.
func IsMutationRefusedOrUnsent(err error) bool {
	if err == nil || endpointpool.IsMaybeSent(err) {
		return false
	}
	if _, answered := AnsweredStatus(err); answered {
		return true
	}
	return endpointpool.IsConnectPhase(err) || IsDirectDialFailure(err)
}
