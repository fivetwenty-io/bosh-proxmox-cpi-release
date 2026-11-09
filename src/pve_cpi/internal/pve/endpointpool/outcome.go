package endpointpool

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// reqClass is how the outcome table treats a request's method.
type reqClass int

const (
	// classIdempotent is GET, HEAD, or OPTIONS, the SDK's idempotent set.
	classIdempotent reqClass = iota + 1
	// classLogin is POST /access/ticket, which creates no state on PVE and is
	// treated like a read.
	classLogin
	// classMutation is every other request. Only a connect-phase failure
	// that proves nothing was sent, or a refused send-by deadline, lets one
	// move.
	classMutation
)

// classOf returns the class of req. An empty method is GET, as net/http reads
// it.
func classOf(req *http.Request) reqClass {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		return classIdempotent
	}
	if isLogin(req) {
		return classLogin
	}
	return classMutation
}

// action is what the walk does after one hop.
type action int

const (
	// actAnswer returns the response and makes the endpoint active if the
	// walk started under the current generation.
	actAnswer action = iota + 1
	// actReturn returns the response or the error as it is.
	actReturn
	// actHop tries the next endpoint when the bounds allow one.
	actHop
)

// sentState says what one hop, or the request it ended, proves about whether
// the request reached the endpoint. It is the seam a mutation's tether
// follows: sentNever releases the request's hold on its reservation, and every
// other state fixes the tether on the endpoint.
//
// sentNever rests on the only two proofs that no byte left the machine. One
// is a failure that carries the connect-phase tag, which for a mutation also
// needs provesNeverSent, because Go may have replayed a request it counts as
// replayable after an earlier attempt was written. A caller cancellation
// before a connection is a proof only when it carries the tag. The other is a
// refused send-by deadline, which carries the tag too, under the same
// provesNeverSent rule for a mutation. Nothing else is a proof. An untagged
// error before a connection leaves a mutation sentMaybe, even though httptrace
// saw no connection, because the walk does not trust a trace to prove that
// nothing was sent.
//
// Two cases send nothing at all, so they need no proof. A request the pool
// refuses before it hands the request to any transport is sentNever, and a
// hop whose body GetBody failed to rebuild keeps the sent state of the hop
// before it.
type sentState int

const (
	// sentNever means one of the two proofs holds, so no byte of the request
	// left the machine.
	sentNever sentState = iota + 1
	// sentMaybe means the request may have reached the endpoint.
	sentMaybe
	// sentAnswered means the endpoint answered with a status.
	sentAnswered
)

// The classes a failure is logged and recorded with, beside the connect
// phases, which use their Phase value.
const (
	classSendBy         = "send_by"
	classCancelled      = "cancelled"
	classRequest        = "request"
	classHeaderDeadline = "header_deadline"
	classTransport      = "transport"
	classStatus         = "status"
)

// The statuses with a row of their own in the outcome table. 595 and 596 are
// pveproxy's: 595 means it could not open the connection to the next hop, and
// 596 means that connection broke after the request may have reached it.
const (
	statusNoNextHop     = 595
	statusBrokenBackend = 596
	statusLastProxy     = 599
)

// attempt is everything the outcome table needs to know about one hop.
type attempt struct {
	class reqClass
	// kind is the endpoint's kind, or zero for a routed host outside the
	// pool.
	kind Kind
	err  error
	// status is the response's status code when err is nil.
	status int
	// attributable says whether a stall or a 502, 503, or 595 on the path
	// can be blamed on the endpoint.
	attributable bool
	// withDeadline says whether the hop used the clone with the
	// response-header deadline.
	withDeadline bool
	// callerDone says whether the request's own context had ended when the
	// hop returned.
	callerDone bool
	// gotConn says whether the transport ever handed the hop a connection.
	gotConn bool
	// provenUnsent is provesNeverSent for the hop's request and error.
	provenUnsent bool
	// statusHopped says whether the request has already spent its one
	// status hop, in this round trip or in an earlier round trip of the same
	// request.
	statusHopped bool
}

// outcome is the outcome table's verdict on one hop.
type outcome struct {
	action action
	// statusHop marks a hop on a 502, 503, or 595 from the entry, which is
	// the request's one status hop. Its demotion applies only when the walk
	// does hop.
	statusHop bool
	// demote says the endpoint is demoted in this process. For a status hop
	// it applies when the hop happens, and for every other outcome at once.
	demote bool
	sent   sentState
	// unreachable says the hop failed in the connect phase or on the header
	// deadline, which is what EntryUnreachableError reports.
	unreachable bool
	// sendBy says the request was refused for its send-by deadline.
	sendBy bool
	// verifyAlert says a configured endpoint failed TLS verification, which
	// is logged at Error because it can mean interception.
	verifyAlert bool
	// defectAlert says the pool refused to send through an owned transport,
	// for a wrong address or for plain http to an endpoint whose certificate
	// it checks. Only a defect in the pool can cause either, so it is logged
	// at Error.
	defectAlert bool
	// class names the failure for the log lines, and is empty for an answer.
	class string
}

// classify applies the outcome table to one hop.
func classify(a attempt) outcome {
	if a.err != nil {
		return classifyError(a)
	}
	return classifyStatus(a)
}

// classifyError applies the rows for a hop that ended with an error. The rows
// are checked in order, and the first that matches wins. A hop is sentNever
// only on a proof that sentState names, and every other error leaves it
// sentMaybe.
//
//   - A refused send-by deadline, from the pool's dialer or from the check in
//     front of it, refuses the request with no hop and no effect, since the
//     deadline is the caller's own. A mutation Go may have replayed may have
//     left on an earlier attempt, so provesNeverSent decides its sent state
//     here as in the connect-phase row.
//   - A caller whose context ended gets the error back with no hop. When the
//     hop never got a connection, which is the cancellation row, it has no
//     effect. Go returns the context's own error in place of the dial's when
//     the caller gives up during a dial, so the connection, not the tag,
//     decides the row, and only the tag proves the request unsent. After a
//     connection a mutation may have been sent, so it demotes the endpoint as
//     any transport error after the connection does. A read the caller gave up
//     on has no effect, because the caller's own deadline says nothing about
//     the endpoint.
//   - A connect-phase failure hops, and demotes the endpoint when
//     blamesEndpoint says the endpoint caused it. A mutation hops only when
//     provesNeverSent holds. Otherwise Go may have written an earlier attempt
//     of it, so it is maybe sent and stays. A refusal the pool made for a
//     wrong address or for plain http hops without blame and is logged at
//     Error.
//   - An error before any connection without the connect-phase tag comes from
//     the request itself, since every dial and handshake failure is tagged, so
//     it is returned with no effect. It proves nothing, so a mutation that
//     gets one is maybe sent.
//   - The header deadline on a hop that had one demotes and hops. Only an
//     idempotent request that is not on its final hop has one.
//   - Any other transport error demotes the endpoint. A read or the login
//     hops, and a mutation may have been sent, so it stays.
func classifyError(a attempt) outcome {
	cpe, tagged := AsConnectPhase(a.err)
	sent := sentMaybe
	if tagged && (a.class != classMutation || a.provenUnsent) {
		sent = sentNever
	}
	switch {
	case errors.Is(a.err, ErrSendByPassed):
		return outcome{action: actReturn, sent: sent, sendBy: true, class: classSendBy}
	case a.callerDone && !a.gotConn:
		return outcome{action: actReturn, sent: sent, class: classCancelled}
	case a.callerDone:
		return outcome{action: actReturn, sent: sentMaybe, demote: a.class == classMutation, class: classCancelled}
	case tagged:
		blamed := blamesEndpoint(a.err)
		oc := outcome{
			action:      actHop,
			demote:      blamed,
			sent:        sent,
			unreachable: true,
			verifyAlert: blamed && cpe.Phase == PhaseTLSVerify && a.kind == KindConfigured,
			defectAlert: isPoolRefusal(a.err),
			class:       string(cpe.Phase),
		}
		if sent != sentNever {
			oc.action, oc.unreachable = actReturn, false
		}
		return oc
	case !a.gotConn:
		return outcome{action: actReturn, sent: sentMaybe, class: classRequest}
	case a.withDeadline && isTimeout(a.err):
		return outcome{action: actHop, demote: true, sent: sentMaybe, unreachable: true, class: classHeaderDeadline}
	case a.class == classMutation:
		return outcome{action: actReturn, demote: true, sent: sentMaybe, class: classTransport}
	default:
		return outcome{action: actHop, demote: true, sent: sentMaybe, class: classTransport}
	}
}

// isPoolRefusal reports whether err is a refusal the pool made itself because
// a request reached the wrong owned transport, for a wrong address or for
// plain http to an endpoint whose certificate it checks.
func isPoolRefusal(err error) bool {
	return errors.Is(err, errWrongAddress) || errors.Is(err, errPlaintextRefused)
}

// classifyStatus applies the rows for a hop that got a status. A mutation
// never moves on a status and never demotes or promotes its endpoint on one,
// because no status proves the mutation was not sent. For a read or the login:
//
//   - A 502, 503, or 595 that the entry caused hops once per request, and a
//     later one is returned as it is. One for another node is returned, since
//     every entry would give the same answer.
//   - A 596 is returned without a hop, because pveproxy's connection to the
//     backend broke after the request may have reached it. One that the entry
//     caused demotes the entry, by the same attribution as a 502, 503, or
//     595, and one for another node has no effect, because that node's broken
//     backend says nothing about the entry.
//   - A 597 to 599 is returned with no effect.
//   - Any other status, 2xx, 4xx, 500, and 504 among them, is an answer.
func classifyStatus(a attempt) outcome {
	if a.class == classMutation {
		return outcome{action: actReturn, sent: sentAnswered, class: classStatus}
	}
	switch s := a.status; {
	case s == http.StatusBadGateway || s == http.StatusServiceUnavailable || s == statusNoNextHop:
		if a.attributable && !a.statusHopped {
			return outcome{action: actHop, statusHop: true, demote: true, sent: sentAnswered, class: classStatus}
		}
		return outcome{action: actReturn, sent: sentAnswered, class: classStatus}
	case s == statusBrokenBackend:
		return outcome{action: actReturn, demote: a.attributable, sent: sentAnswered, class: classStatus}
	case s > statusBrokenBackend && s <= statusLastProxy:
		return outcome{action: actReturn, sent: sentAnswered, class: classStatus}
	default:
		return outcome{action: actAnswer, sent: sentAnswered}
	}
}

// isTimeout reports whether err is a net.Error that timed out. On an owned
// clone the only such error after a connection exists is the response-header
// deadline, since the caller's own deadline is checked first.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// failureNote is why an endpoint failed, for the log lines.
type failureNote struct {
	class string
	text  string
}

// noteOf describes the failure of one hop. A connect-phase failure is
// described by its cause, without the tag's own prefix, and a status by its
// status line.
func noteOf(resp *http.Response, err error, class string) failureNote {
	switch {
	case err != nil:
		if cpe, ok := AsConnectPhase(err); ok && cpe.Err != nil {
			return failureNote{class: class, text: cpe.Err.Error()}
		}
		return failureNote{class: class, text: err.Error()}
	case resp != nil:
		return failureNote{class: class, text: "HTTP " + resp.Status}
	default:
		return failureNote{class: class, text: "unknown failure"}
	}
}

// promoteAfter applies the generation guard to an answer from ep on a walk that
// started under generation gen. ep becomes active and loses its demotion only
// when gen is still current, so a slow answer from before a failover cannot
// make its endpoint active again. When that moves the active endpoint, the
// change is logged once at Warn, naming the endpoint that lost it with its last
// failure. The pool names that endpoint under its lock, so the line is right
// even when the walk could not read the active endpoint when it started.
func (p *Pool) promoteAfter(ep *endpoint, gen uint64) {
	if prev, ok := p.promote(ep, gen); ok && prev != nil {
		p.logFailover(prev, ep, true)
	}
}

// The limits on keeping a status-hop response so that a walk that gets no
// other answer can still return it.
const (
	heldBodyLimit = 64 << 10
	drainWait     = 5 * time.Second
)

// heldResponse is a 502, 503, or 595 answer that the walk hopped away from,
// with its body read into memory, so a walk that then gets no answer returns
// it as the last response.
type heldResponse struct {
	resp *http.Response
	body []byte
}

// holdResponse drains and closes resp's body, giving up after drainWait, and
// keeps the response when the body was read whole and fits heldBodyLimit. It
// returns nil when it could not keep it. PVE's error bodies are a few bytes,
// so in practice every status hop keeps its response.
func holdResponse(resp *http.Response) *heldResponse {
	timer := time.AfterFunc(drainWait, func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(io.LimitReader(resp.Body, heldBodyLimit+1))
	inTime := timer.Stop()
	_ = resp.Body.Close()
	if err != nil || !inTime || len(body) > heldBodyLimit {
		return nil
	}
	return &heldResponse{resp: resp, body: body}
}

// response returns the kept response with its body readable again and its
// Request set to the caller's request.
func (h *heldResponse) response(req *http.Request) *http.Response {
	r := h.resp
	r.Body = io.NopCloser(bytes.NewReader(h.body))
	r.ContentLength = int64(len(h.body))
	r.TransferEncoding = nil
	r.Request = req
	return r
}
