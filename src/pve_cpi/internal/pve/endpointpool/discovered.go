package endpointpool

import (
	"cmp"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The limits on a cache down mark. A mark moves its endpoint to the end of the
// walk until markLifetime after it was written. A mark dated further than
// markFutureAllowance ahead of the pool's clock when the pool is built is
// dropped, and it never takes effect, even once the clock passes its time,
// because a forged or skewed time must not make a mark last longer than its
// lifetime.
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
// The pool calls MarkDown synchronously, on the goroutine of the request that
// failed and outside every lock the pool holds, and that request waits until
// MarkDown returns. The pool starts no goroutine for the write. So each
// implementation hands the write to a goroutine of its own, registers that
// goroutine with the process's worker registry so the exit drain covers it,
// and returns promptly. MarkDown may call the pool's own methods. When
// MarkDown panics, the pool recovers, logs the panic, and carries on with the
// request, so a defect in the sink costs the mark and never the request.
//
// Every mark the pool hands over for one endpoint is dated after the one
// before it, and a pin mark is the last mark for its endpoint. A sink that
// keeps the newest mark for each endpoint, as the cache's merge does,
// therefore keeps the pin mark whatever order two writes finish in.
type MarkSink interface {
	MarkDown(m Mark)
}

// FirstAnswerHook is told when a configured endpoint first answers a request
// sent through the pool, which is when the discovery worker may start. It is
// told once per pool, never for an answer from a discovered endpoint, and
// never for a 502, 503, or 595 to 599, which say the endpoint could not serve
// the request.
//
// The pool calls FirstAnswer synchronously, on the goroutine of the request
// that got the answer and outside every lock the pool holds, and it returns
// the response only after FirstAnswer returns. The pool starts no goroutine
// for the hook. So each implementation hands the work it starts, such as
// discovery, to a goroutine of its own, registers that goroutine with the
// process's worker registry, and returns promptly. FirstAnswer may call the
// pool's own methods. When FirstAnswer panics, the pool recovers, logs the
// panic, and returns the response as if the hook had returned.
//
// The hook is told only once per pool, so a refresh that a pin failure makes
// due after the first answer starts no discovery in this process. The pin
// mark the pool writes for that failure is what makes the next process
// refresh.
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
// It wraps ErrPinMismatch, and refuseIneligible puts it inside a
// *tls.CertificateVerificationError, so the refusal reads as a certificate
// failure, as the pin failure that made the endpoint ineligible did.
var errIneligible = fmt.Errorf("%w: it failed its pin earlier in this call, so the pool skips it until "+
	"discovery refreshes", ErrPinMismatch)

// errDropped is the cause when the pool refuses, before any dial, to send a
// request to a discovered endpoint that DropDiscovered took out. Nothing
// failed a pin, so it does not wrap ErrPinMismatch, and refuseDropped puts it
// inside the same connect-phase tag and *tls.CertificateVerificationError as
// errIneligible, so the walk and the SDK treat the two refusals alike.
var errDropped = errors.New("discovery found that the cluster identity changed, so the pool stopped using its " +
	"discovered endpoints")

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

// acceptedMark returns the down mark an endpoint is loaded with when the pool
// is built at now. A mark dated more than markFutureAllowance ahead of now is
// dropped rather than clamped, so the result is zero and the mark never takes
// effect, even once the clock passes its time. Any other mark is kept as it
// is.
func acceptedMark(at, now time.Time) time.Time {
	if at.After(now.Add(markFutureAllowance)) {
		return time.Time{}
	}
	return at
}

// markInEffect reports whether a down mark written at at still moves its
// endpoint to the end of the walk at now. A zero at is no mark. A mark from
// up to markFutureAllowance ahead, which acceptedMark kept, is in effect from
// the start, and every mark expires markLifetime after its own time.
func markInEffect(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) < markLifetime
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

// startOnPreferred makes the first endpoint in walk order at now active,
// which New calls once, and records a start away from the first endpoint in
// preference order. Nothing is demoted or ineligible yet, so only a mark moves
// the start, and a configured endpoint is always eligible, so there is always
// one.
func (p *Pool) startOnPreferred(now time.Time) {
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

// DropDiscovered takes every discovered endpoint out of every later walk and
// routed request for the rest of the process, and it returns how many it took
// out. Discovery calls it when the configured endpoints report a cluster
// identity other than the one the discovered endpoints were pinned under, so
// none of them may be used again in this call.
//
// A dropped endpoint is ineligible, as a discovered endpoint that failed its
// pin is, so a walk that already read its order refuses the hop before any
// dial. The refusal names the drop and not a pin failure, because nothing
// failed its pin, and an endpoint that had already failed its pin keeps its
// pin refusal. A drop does not demote, mark, or log, and it makes no refresh
// due, because nothing failed and discovery itself asked for the drop. When
// the active endpoint is discovered, the first configured endpoint in walk
// order becomes active and the generation goes up, so a request in flight on
// the discovered endpoint cannot make it active again. The idle connections
// of every dropped endpoint are closed outside the pool's lock. A pool
// without discovered endpoints, and a second call, change nothing and return
// zero. It is safe to call concurrently with walks.
func (p *Pool) DropDiscovered() int {
	now := p.now()
	p.mu.Lock()
	var dropped []*endpoint
	for _, ep := range p.endpoints {
		if ep.kind == KindDiscovered && !ep.ineligible {
			ep.ineligible, ep.dropped = true, true
			dropped = append(dropped, ep)
		}
	}
	if p.active.kind == KindDiscovered {
		// A configured endpoint is always eligible, so there is always one.
		if next := preferredLocked(p.endpoints, now, nil); next != nil {
			p.active = next
			p.generation++
		}
	}
	p.mu.Unlock()
	for _, ep := range dropped {
		ep.transports.closeIdle()
	}
	return len(dropped)
}

// refusalFor returns the error for a request the pool will not send to ep,
// which is ineligible: the drop's refusal when DropDiscovered took ep out, and
// the pin failure's otherwise.
func (p *Pool) refusalFor(ep *endpoint) error {
	p.mu.Lock()
	dropped := ep.dropped
	p.mu.Unlock()
	if dropped {
		return refuseDropped(ep.addr)
	}
	return refuseIneligible(ep.addr)
}

// ineligibleNow reports whether ep failed its pin in this process.
func (p *Pool) ineligibleNow(ep *endpoint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ep.ineligible
}

// eligibleAmong counts the endpoints in eps that have not failed their pin in
// this process.
func (p *Pool) eligibleAmong(eps []*endpoint) int {
	_, n := p.eligibleFrom(nil, eps)
	return n
}

// eligibleFrom reports whether ep, the endpoint a hop is about to go to, has
// not failed its pin in this process, and counts the endpoints in eps, the
// order from that hop on, that have not. An accepted cold-start candidate is
// never counted, because candidates count toward neither the pool's size nor
// its final hop, so the configured endpoints keep the deadlines they would
// have without one. A nil ep is eligible. Both answers
// come from one acquisition of Pool.mu, so a pin failure of ep cannot fall
// between the refusal and the count the hop's deadline is decided from.
func (p *Pool) eligibleFrom(ep *endpoint, eps []*endpoint) (eligible bool, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range eps {
		if !e.ineligible && e.kind != kindCandidate {
			n++
		}
	}
	return ep == nil || !ep.ineligible, n
}

// refuseIneligible returns the error for a request the pool will not send to
// ep, a discovered endpoint that failed its pin, before any dial. It carries
// the connect-phase tag with PhasePin, because nothing was sent and the
// endpoint is out for a certificate failure. The cause is a
// *tls.CertificateVerificationError, which is the shape a real pin failure
// reaches the SDK in, so the SDK stops at once instead of retrying a request
// the pool will refuse again.
func refuseIneligible(addr string) error {
	return &ConnectPhaseError{Phase: PhasePin, Endpoint: addr, Err: &tls.CertificateVerificationError{Err: errIneligible}}
}

// refuseDropped returns the error for a request the pool will not send to addr,
// a discovered endpoint that DropDiscovered took out, before any dial. It has
// the shape refuseIneligible gives, with errDropped as the cause.
func refuseDropped(addr string) error {
	return &ConnectPhaseError{Phase: PhasePin, Endpoint: addr, Err: &tls.CertificateVerificationError{Err: errDropped}}
}

// ineligibleHop is hop i's result when the walk will not send it to ep, a
// discovered endpoint that failed its pin or was dropped after the walk read
// its order. The request was handed to no transport, so it is proven unsent,
// it hops as a pin failure does, and it has no effect on ep, which the pin
// failure already demoted and reported, or which the drop left alone. It does
// not count toward the hop cap.
func (p *Pool) ineligibleHop(ep *endpoint) hopResult {
	return hopResult{
		ep:  ep,
		err: p.refusalFor(ep),
		oc:  outcome{action: actHop, sent: sentNever, unreachable: true, class: string(PhasePin)},
	}
}

// connectFailed applies what the pool records beyond a demotion when a
// request to ep failed in the connect phase for a reason the endpoint caused,
// in the given phase. A discovered endpoint that failed its pin becomes
// ineligible for the rest of the process, makes a refresh due, is logged once
// at Warn, and gets a mark with the class pin. Any other failure gets a mark
// when it is the endpoint's first in this process. The sink is called outside
// every lock the pool holds.
func (p *Pool) connectFailed(ep *endpoint, phase Phase, err error) {
	var (
		m    Mark
		send bool
	)
	if phase == PhasePin && ep.kind == KindDiscovered {
		m, send = p.makeIneligible(ep, err)
	} else {
		m, send = p.takeFirstMark(ep, phase)
	}
	if send {
		p.markDown(m)
	}
}

// takeFirstMark decides whether a connect-phase failure of ep in the given
// phase gets a mark, and dates it. Only the endpoint's first failure in this
// process gets one, and an endpoint that already failed its pin gets none,
// because makeIneligible sets marked with the pin mark, which must be the last
// mark for the endpoint. The decision and the date are made under Pool.mu,
// together with the pin mark's, so a mark that loses the race to a pin mark is
// never sent, and one that wins it is dated before the pin mark.
func (p *Pool) takeFirstMark(ep *endpoint, phase Phase) (Mark, bool) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep.marked {
		return Mark{}, false
	}
	ep.marked = true
	return Mark{Endpoint: ep.addr, Class: phase, At: stampMarkLocked(ep, now)}, true
}

// stampMarkLocked returns the time a new mark for ep records, which is now
// unless an earlier mark for ep is dated at or after now. Then it is one
// nanosecond after that mark, so every mark for one endpoint is dated after
// the one before it, and the cache's merge, which keeps the newest mark,
// keeps the last one whatever order the writes finish in. The time carries no
// monotonic reading, so the comparison is the wall-clock one a written mark
// keeps. Pool.mu must be held.
func stampMarkLocked(ep *endpoint, now time.Time) time.Time {
	at := now.Round(0)
	if !ep.lastMarkAt.IsZero() && !at.After(ep.lastMarkAt) {
		at = ep.lastMarkAt.Add(time.Nanosecond)
	}
	ep.lastMarkAt = at
	return at
}

// makeIneligible takes a discovered endpoint that failed its pin out of every
// later walk, and it returns the endpoint's pin mark and true when this call
// did so. A later call returns false, so the pin mark is sent once. It also
// demotes the endpoint, makes a refresh due, closes the endpoint's idle
// connections, and logs the pin-mismatch line once.
//
// The demotion keeps the rule that every endpoint the pool has blamed in this
// process is demoted. A walk and a routed request demote a pin failure through
// its outcome, but a pin failure that finished after the send-by bound has an
// outcome that demotes nothing, so this demotion is its only one.
func (p *Pool) makeIneligible(ep *endpoint, err error) (Mark, bool) {
	now := p.now()
	p.mu.Lock()
	if ep.ineligible {
		p.mu.Unlock()
		return Mark{}, false
	}
	ep.ineligible, ep.demoted, ep.marked = true, true, true
	m := Mark{Endpoint: ep.addr, Class: PhasePin, At: stampMarkLocked(ep, now)}
	p.mu.Unlock()
	p.markRefreshDue()
	ep.transports.closeIdle()
	p.logPinMismatch(ep, err)
	return m, true
}

// pinFailureAfterSendBy counts a discovered endpoint's pin failure that
// finished after the request's send-by bound had passed. The dialer wraps such
// a failure in ErrSendByPassed, so the request reports the send-by refusal and
// its outcome demotes nothing. The certificate was still refused, so the
// endpoint gets every effect of a pin failure: it becomes ineligible, a refresh
// becomes due, the Warn line is logged, and a pin mark is written. The cause
// the line, the failure note, and the mark see is the certificate refusal
// without the send-by wrapping. Every other error is left alone.
func (p *Pool) pinFailureAfterSendBy(ep *endpoint, err error) {
	if ep == nil || ep.kind != KindDiscovered || !errors.Is(err, ErrSendByPassed) ||
		!errors.Is(err, ErrPinMismatch) {
		return
	}
	cpe, ok := AsConnectPhase(err)
	if !ok || cpe.Phase != PhasePin {
		return
	}
	cause := err
	var refused *tls.CertificateVerificationError
	if errors.As(err, &refused) {
		cause = &ConnectPhaseError{Phase: PhasePin, Endpoint: cpe.Endpoint, Err: refused}
	}
	p.walk.note(ep, noteOf(nil, cause, string(PhasePin)))
	p.connectFailed(ep, PhasePin, cause)
}

// markDown hands m to the mark sink, when the pool has one, through
// guardHook, so a panic in the sink costs the mark and never the request.
func (p *Pool) markDown(m Mark) {
	if p.sink == nil {
		return
	}
	defer p.guardHook(&p.sinkPanicked, fmt.Sprintf("pve endpoint %s: the down-mark sink panicked, so the %s "+
		"mark may not be recorded", m.Endpoint, m.Class), m.Endpoint, m.Class)
	p.sink.MarkDown(m)
}

// tellFirstAnswer tells the first-answer hook that the configured endpoint
// addr answered, through guardHook, so a panic in the hook never changes what
// the request returns.
func (p *Pool) tellFirstAnswer(addr string) {
	defer p.guardHook(&p.hookPanicked, fmt.Sprintf("pve endpoint %s: the first-answer hook panicked, so "+
		"discovery may not start in this call", addr), addr, "")
	p.firstAnswer.FirstAnswer(addr)
}

// guardHook is deferred around a call into the mark sink or the first-answer
// hook. It recovers a panic in the call and logs it with msg, the endpoint,
// the class when there is one, and the scrubbed panic value, and then the
// call returns normally. The first panic of each hook in the pool's life is
// logged at Warn and every later one at Debug, so a defect that panics on
// every call is reported once and every panic still leaves a line. A
// runtime.Goexit is not a panic, so it carries on unwinding.
func (p *Pool) guardHook(once *atomic.Bool, msg, endpoint string, class Phase) {
	r := recover()
	if r == nil {
		return
	}
	fields := []log.Field{
		log.String("endpoint", endpoint),
		log.String("panic", log.ScrubMessage(fmt.Sprint(r))),
	}
	if class != "" {
		fields = append(fields, log.String("class", string(class)))
	}
	if once.CompareAndSwap(false, true) {
		p.logger.Warn(msg, fields...)
		return
	}
	p.logger.Debug(msg, fields...)
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
		p.tellFirstAnswer(ep.addr)
	}
}

// isAnswerStatus reports whether a status shows that the endpoint served the
// request, which is every status the outcome table counts as an answer for a
// read. A 502, 503, or 595 to 599 says pveproxy could not serve it.
func isAnswerStatus(status int) bool {
	return classifyStatus(attempt{class: classIdempotent, status: status}).action == actAnswer
}
