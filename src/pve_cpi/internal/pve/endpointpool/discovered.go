package endpointpool

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The limits on a cache down mark. A mark moves its endpoint to the end of the
// walk until markLifetime after it was written. A mark written further than
// markFutureAllowance in the future is ignored, because a forged or skewed
// time must not make a mark last longer than its lifetime.
const (
	markLifetime        = 2 * time.Minute
	markFutureAllowance = 30 * time.Second
)

// Mark is a down mark the pool hands its MarkSink.
type Mark struct {
	// Endpoint is the endpoint's canonical host:port.
	Endpoint string
	// Class is the connect phase that failed, which is one of PhaseDial,
	// PhaseDNS, PhaseTLSHandshake, PhaseTLSVerify, and PhasePin. These are the
	// values of the cache's mark class.
	Class Phase
	// At is when the failure happened.
	At time.Time
}

// MarkSink records the down marks a pool reports, so other processes can try
// the endpoint last for two minutes. The pool reports an endpoint's first
// connect-phase failure in this process, and a discovered endpoint's pin
// failure when an earlier failure of another class already took the first
// report, because a pin mark is what tells other processes that discovery is
// due. A failure the caller's own context caused, a refused send-by deadline,
// and a refusal the pool makes itself are never reported.
//
// The pool calls MarkDown on the request's goroutine, outside every lock the
// pool holds, and does not wait for the write. MarkDown must return without
// waiting for it: the implementation owns the goroutine that writes the mark,
// and it registers that goroutine with the process's worker registry, so the
// exit drain covers it. MarkDown may call the pool's own methods.
type MarkSink interface {
	MarkDown(m Mark)
}

// FirstAnswerHook is told when a configured endpoint first answers a request
// sent through the pool, which is when the discovery worker may start. It is
// told once per pool, never for an answer from a discovered endpoint, and
// never for a 502, 503, or 595 to 599, which say the endpoint could not serve
// the request.
//
// The pool calls FirstAnswer on the request's goroutine, outside every lock
// the pool holds, before it returns the response. FirstAnswer must return
// without waiting for any work it starts: the implementation owns that work's
// goroutine and registers it with the process's worker registry. It may call
// the pool's own methods.
type FirstAnswerHook interface {
	// FirstAnswer is passed the canonical host:port of the configured
	// endpoint that answered.
	FirstAnswer(endpoint string)
}

// StartAway describes a pool that started on an endpoint other than the
// first in preference order, because the first carried a down mark younger
// than two minutes when the pool was built.
type StartAway struct {
	// First is the first endpoint in preference order, which carried the
	// mark.
	First string
	// Start is the endpoint the pool started on.
	Start string
	// MarkedAt is when First's mark was written.
	MarkedAt time.Time
}

// errIneligible is the cause when the pool refuses, before any dial, to send a
// request to a discovered endpoint that failed its pin earlier in the process.
// It wraps ErrPinMismatch, so the refusal reads as a certificate failure, as
// the pin failure that made the endpoint ineligible did.
var errIneligible = fmt.Errorf("%w: it failed its pin earlier in this call, so the pool skips it until "+
	"discovery refreshes", ErrPinMismatch)

// sortDiscovered puts the discovered endpoints whose node a quorate
// /cluster/status reported online ahead of the rest, each group in node-name
// order. Endpoints with the same node name keep the order they came in.
func sortDiscovered(eps []orderedEndpoint) {
	slices.SortStableFunc(eps, func(a, b orderedEndpoint) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Node, b.Node)
	})
}

// markInEffect reports whether a down mark written at at still moves its
// endpoint to the end of the walk at now. A zero at is no mark.
func markInEffect(at, now time.Time) bool {
	if at.IsZero() {
		return false
	}
	age := now.Sub(at)
	return age < markLifetime && age >= -markFutureAllowance
}

// eligibleLocked reports whether a walk may try ep. A discovered endpoint that
// failed its pin is never eligible, and a configured-only walk takes only the
// configured endpoints. Pool.mu must be held.
func eligibleLocked(ep *endpoint, configuredOnly bool) bool {
	return !ep.ineligible && (!configuredOnly || ep.kind == KindConfigured)
}

// atBackLocked reports whether ep goes to the end of the walk, because it is
// demoted in this process or carries a down mark still in effect at now.
// Pool.mu must be held.
func atBackLocked(ep *endpoint, now time.Time) bool {
	return ep.demoted || markInEffect(ep.markedAt, now)
}

// preferredLocked returns the first endpoint other than skip in walk order at
// now, without a lead: the first eligible one that is not at the back, or the
// first eligible one at the back when every one is. It returns nil when no
// endpoint but skip is eligible. Pool.mu must be held.
func preferredLocked(eps []*endpoint, now time.Time, skip *endpoint) *endpoint {
	for _, back := range []bool{false, true} {
		for _, ep := range eps {
			if ep != skip && eligibleLocked(ep, false) && atBackLocked(ep, now) == back {
				return ep
			}
		}
	}
	return nil
}

// startOnPreferred makes the first endpoint in walk order active, which New
// calls once, and records a start away from the first endpoint in preference
// order. Nothing is demoted or ineligible yet, so only a mark moves the start,
// and a configured endpoint is always eligible, so there is always one.
func (p *Pool) startOnPreferred() {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = preferredLocked(p.endpoints, now, nil)
	if first := p.endpoints[0]; p.active != first {
		p.startAway = &StartAway{First: first.addr, Start: p.active.addr, MarkedAt: first.markedAt}
	}
}

// StartedAway reports whether the pool started on an endpoint other than the
// first in preference order, because the first carried a down mark younger
// than two minutes, and describes the start when it did.
func (p *Pool) StartedAway() (StartAway, bool) {
	if p.startAway == nil {
		return StartAway{}, false
	}
	return *p.startAway, true
}

// ineligibleNow reports whether ep failed its pin in this process.
func (p *Pool) ineligibleNow(ep *endpoint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ep.ineligible
}

// refuseIneligible returns the error for a request the pool will not send to
// ep, a discovered endpoint that failed its pin, before any dial. It carries
// the connect-phase tag with PhasePin, because nothing was sent and the
// endpoint is out for a certificate failure.
func refuseIneligible(addr string) error {
	return &ConnectPhaseError{Phase: PhasePin, Endpoint: addr, Err: errIneligible}
}

// ineligibleHop is hop i's result when the walk will not send it to ep, a
// discovered endpoint that failed its pin after the walk read its order. The
// request was handed to no transport, so it is proven unsent, it hops as a
// pin failure does, and it has no effect on ep, which the pin failure already
// demoted and reported.
func ineligibleHop(ep *endpoint) hopResult {
	return hopResult{
		ep:  ep,
		err: refuseIneligible(ep.addr),
		oc:  outcome{action: actHop, sent: sentNever, unreachable: true, class: string(PhasePin)},
	}
}

// connectFailed applies what the pool records beyond a demotion when a
// request to ep failed in the connect phase for a reason the endpoint caused,
// in the given phase. A discovered endpoint that failed its pin becomes
// ineligible for the rest of the process, makes a refresh due, and is logged
// once at Warn. The first such failure of each endpoint is handed to the mark
// sink, and so is a pin failure that follows a failure of another class. The
// sink is called outside every lock the pool holds.
func (p *Pool) connectFailed(ep *endpoint, phase Phase, err error) {
	first := ep.marked.CompareAndSwap(false, true)
	pinOut := phase == PhasePin && ep.kind == KindDiscovered && p.makeIneligible(ep, err)
	if (first || pinOut) && p.sink != nil {
		p.sink.MarkDown(Mark{Endpoint: ep.addr, Class: phase, At: p.now()})
	}
}

// makeIneligible takes a discovered endpoint that failed its pin out of every
// later walk and reports whether this call did so. It also demotes the
// endpoint, so a walk that read its order before this call still tries it
// last, makes a refresh due, closes the endpoint's idle connections, and logs
// the pin-mismatch line once.
func (p *Pool) makeIneligible(ep *endpoint, err error) bool {
	p.mu.Lock()
	if ep.ineligible {
		p.mu.Unlock()
		return false
	}
	ep.ineligible, ep.demoted = true, true
	p.mu.Unlock()
	p.markRefreshDue()
	ep.transports.closeIdle()
	p.logPinMismatch(ep, err)
	return true
}

// logPinMismatch logs at Warn that a discovered endpoint failed its pin and is
// skipped for the rest of the call. A leaf outside the pin set gets the
// observability table's wording, and a leaf refused before its pins were
// checked, such as one whose chain failed under verify_ssl true, says so with
// its cause.
func (p *Pool) logPinMismatch(ep *endpoint, err error) {
	node := "its node"
	if ep.node != "" {
		node = "node " + ep.node
	}
	cause := log.ScrubMessage(noteOf(nil, err, string(PhasePin)).text)
	msg := fmt.Sprintf("pve endpoint %s: certificate is not one of the %d pinned for %s; "+
		"skipping it until discovery refreshes", ep.addr, ep.pins, node)
	if !isNotPinned(err) {
		msg = fmt.Sprintf("pve endpoint %s: certificate refused (%s), so it was not checked against the %d pinned "+
			"for %s; skipping it until discovery refreshes", ep.addr, cause, ep.pins, node)
	}
	p.logger.Warn(msg,
		log.String("endpoint", ep.addr),
		log.String("node", ep.node),
		log.Int("pins", ep.pins),
		log.String("class", string(PhasePin)),
		log.String("error", cause),
	)
}

// noteAnswer tells the first-answer hook, once per pool, that a configured
// endpoint answered a request. oc is the outcome of the request that got
// resp, and its addr names the endpoint the response came from.
func (p *Pool) noteAnswer(resp *http.Response, oc outcome) {
	if p.firstAnswer == nil || resp == nil || p.answered.Load() {
		return
	}
	ep := p.byAddr[oc.addr]
	if ep == nil || ep.kind != KindConfigured || !isAnswerStatus(resp.StatusCode) {
		return
	}
	if p.answered.CompareAndSwap(false, true) {
		p.firstAnswer.FirstAnswer(ep.addr)
	}
}

// isAnswerStatus reports whether a status shows that the endpoint served the
// request, which is every status the outcome table counts as an answer for a
// read. A 502, 503, or 595 to 599 says pveproxy could not serve it.
func isAnswerStatus(status int) bool {
	return classifyStatus(attempt{class: classIdempotent, status: status}).action == actAnswer
}
