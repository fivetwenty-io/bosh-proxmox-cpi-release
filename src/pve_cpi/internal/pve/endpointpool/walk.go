package endpointpool

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// errNilRequest and errNoURL refuse a request the pool cannot route.
var (
	errNilRequest = errors.New("endpoint pool: nil request")
	errNoURL      = errors.New("endpoint pool: request has no URL")
)

// errNilPool refuses a request sent through a nil *Pool.
var errNilPool = errors.New("endpoint pool: request sent through a nil pool")

// snapshotTries is how many times a walk reads the active endpoint and the
// walk order before it gives up on reading both under one generation.
const snapshotTries = 3

// RoundTrip sends req through the pool. It implements http.RoundTripper.
//
// It first refuses a request whose send-by deadline has passed, with an error
// that wraps ErrSendByPassed, before any dial. A mutation then claims its
// key's tether, before it is handed to any transport. A mutation whose key
// is conflicted is refused with an error that wraps ErrTetherConflict, and
// one whose key already has a tether goes only to that tether's endpoint and
// never walks, whatever kind it would otherwise be. Every other request is
// sorted into one of three kinds:
//
//   - A request whose URL host is not the primary endpoint's is routed. A
//     caller chose that host, so the request goes once through the owned
//     clone for it, and every error it gets that does not prove it was never
//     sent is marked with MaybeSentError.
//   - The ticket login, POST /access/ticket, and a request that carries the
//     configured-only marker walk the configured endpoints only, so a
//     password never reaches a discovered address.
//   - Every other request walks the whole pool from the active endpoint.
//
// A walk clones the request for each hop with req.Clone, puts the endpoint's
// address in the clone's URL.Host, leaves its Host empty, and rebuilds its
// body from GetBody after the first hop. What each hop's outcome does is the
// outcome table in classify, and how far a walk goes is set by bounds. The
// response carries the caller's request in its Request field.
//
// A mutation that reserved its tether releases it when its request ends with
// a proof that nothing was sent, and fixes it on the endpoint it went to on
// every other outcome, a status of any kind included. A walk that hops after
// such a proof claims the tether again for its next endpoint, unless another
// request still holds the reservation, in which case it returns its error
// without hopping.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.roundTrip(req, defaultBounds())
}

// roundTrip is RoundTrip under the given bounds. It logs a send-by refusal
// once, whichever path refused the request, and tells the first-answer hook
// when a configured endpoint answered.
func (p *Pool) roundTrip(req *http.Request, b bounds) (*http.Response, error) {
	resp, oc, err := p.dispatch(req, b)
	if oc.sendBy {
		p.logSendBy(methodOf(req), req.URL.Path)
	}
	if err == nil {
		p.noteAnswer(resp, oc)
	}
	return resp, err
}

// unsent is the outcome of a request the pool refuses before it hands the
// request to any transport, which therefore never left the machine.
var unsent = outcome{action: actReturn, sent: sentNever, class: classRequest}

// dispatch sends req and returns what the caller gets, with the outcome of the
// hop that ended the request. Every kind of request ends here with an outcome,
// so a mutation's tether is released or fixed in one place from its sent
// state.
func (p *Pool) dispatch(req *http.Request, b bounds) (*http.Response, outcome, error) {
	if req == nil {
		return nil, unsent, errNilRequest
	}
	if p == nil {
		closeRequestBody(req)
		return nil, unsent, errNilPool
	}
	if req.URL == nil {
		closeRequestBody(req)
		return nil, unsent, errNoURL
	}
	method := methodOf(req)
	addr, addrErr := urlAddress(req.URL)
	if err := CheckSendBy(req.Context(), time.Now(), method, req.URL.Path); err != nil {
		closeRequestBody(req)
		endpoint := addr
		if addrErr != nil {
			endpoint = req.URL.Host
		}
		oc := unsent
		oc.sendBy, oc.class = true, classSendBy
		return nil, oc, &ConnectPhaseError{Phase: PhaseDial, Endpoint: endpoint, Err: err}
	}
	if addrErr != nil {
		closeRequestBody(req)
		return nil, unsent, addrErr
	}
	configuredOnly := IsConfiguredOnly(req.Context()) || isLogin(req)
	routed := addr != p.endpoints[0].addr
	if classOf(req) == classMutation {
		h := newTetherHold(req)
		resp, oc, err := p.dispatchMutation(req, h, addr, routed, configuredOnly, b)
		p.settle(h, oc)
		return resp, oc, err
	}
	if routed {
		return p.sendRouted(req, tetherTarget{addr: addr, urlHost: req.URL.Host}, nil, configuredOnly)
	}
	return p.newWalker(req, configuredOnly, b).run()
}

// dispatchMutation sends a mutation after it claims its key's tether in h,
// and dispatch settles h from the outcome. The claim names the address the
// request is about to try, which is the routed host for a routed request and
// the first endpoint of the walk otherwise. A conflicted key is refused here,
// before the request is handed to any transport. A key that already has a
// tether sends the request only to that tether's target, and a fresh
// reservation sends it the way its kind does.
func (p *Pool) dispatchMutation(req *http.Request, h *tetherHold, addr string, routed, configuredOnly bool, b bounds,
) (*http.Response, outcome, error) {
	planned := tetherTarget{addr: addr, urlHost: req.URL.Host}
	var w *walker
	if !routed {
		w = p.newWalker(req, configuredOnly, b)
		if w.limit <= 0 {
			return w.run()
		}
		planned = targetOf(w.order[0])
	}
	if err := p.claim(h, planned); err != nil {
		closeRequestBody(req)
		return nil, unsent, err
	}
	if h.following {
		return p.sendFollowing(req, h.target, configuredOnly, b)
	}
	if routed {
		return p.sendRouted(req, planned, nil, configuredOnly)
	}
	w.hold = h
	return w.run()
}

// sendFollowing sends a mutation whose key already had a tether, once, to the
// tether's target, without a walk. A target that is one of the pool's
// endpoints gets a walk of that one endpoint, so its outcome follows the
// outcome table as a walk's first hop does, and a connect-phase failure
// returns the walk's error without trying another endpoint. A routed host
// outside the pool gets the routed path. A configured-only request whose
// tether names an endpoint that is not configured is refused by the routed
// path before any dial, so a credential never reaches an address the
// operator did not list.
func (p *Pool) sendFollowing(req *http.Request, target tetherTarget, configuredOnly bool, b bounds,
) (*http.Response, outcome, error) {
	ep := p.byAddr[target.addr]
	if ep == nil || (configuredOnly && ep.kind != KindConfigured) {
		return p.sendRouted(req, target, nil, configuredOnly)
	}
	w := p.newWalker(req, configuredOnly, b)
	w.order, w.limit, w.tethered = []*endpoint{ep}, 1, true
	return w.run()
}

// sendRouted sends a request once to target, through the owned clone for its
// address, which is an endpoint's own clone when the address is one of the
// pool's endpoints. It uses the clone without a header deadline, because a
// routed request never hops. When target's spelling differs from the
// request's URL host, which is so for a mutation that follows its key's
// tether to a routed host, the clone carries target's spelling and an empty
// Host. body is the request body rebuilt for a hop after a walk's first, and
// nil otherwise.
//
// A configured-only request goes to a routed host only when that host is a
// configured endpoint, so a credential never reaches an address the operator
// did not list, and no request goes to a discovered endpoint that failed its
// pin. A failure that blamesEndpoint counts against one of the pool's
// endpoints demotes it and is recorded as a connect-phase failure, and nothing
// a routed request gets ever makes an endpoint active. When the request's own
// context ends during the dial, net/http returns the context's error, which
// carries no tag, so a cancelled request records nothing. The error is marked
// with MaybeSentError unless its outcome is sentNever, which takes a
// connect-phase failure of an idempotent request or the login, or of a
// mutation that provesNeverSent clears.
func (p *Pool) sendRouted(req *http.Request, target tetherTarget, body io.ReadCloser, configuredOnly bool,
) (*http.Response, outcome, error) {
	addr := target.addr
	ep := p.byAddr[addr]
	if configuredOnly && (ep == nil || ep.kind != KindConfigured) {
		closeSendBody(req, body)
		return nil, unsent, fmt.Errorf(
			"endpoint pool: refusing a configured-only request to %s, which is not a configured endpoint", addr)
	}
	if ep != nil && p.ineligibleNow(ep) {
		closeSendBody(req, body)
		oc := unsent
		oc.class = string(PhasePin)
		return nil, oc, refuseIneligible(addr)
	}
	o, err := p.routedTransportsAt(addr, (&url.URL{Host: target.urlHost}).Hostname())
	if err != nil {
		closeSendBody(req, body)
		return nil, unsent, err
	}
	clone := req.Clone(connectContext(req.Context(), methodOf(req), req.URL.Path))
	if target.urlHost != req.URL.Host {
		clone.URL.Host = target.urlHost
		clone.Host = ""
	}
	if body != nil {
		clone.Body = body
	}
	resp, err := o.pick(false).RoundTrip(clone)
	if err == nil && resp == nil {
		err = errNoResponse
	}
	oc := routedOutcome(classOf(req), clone, err)
	oc.addr = addr
	if err == nil {
		resp.Request = req
		return resp, oc, nil
	}
	if ep != nil && blamesEndpoint(err) {
		p.demote(ep)
		p.walk.note(ep, noteOf(nil, err, oc.class))
		if oc.class == string(PhaseTLSVerify) && ep.kind == KindConfigured {
			p.alertVerify(ep, err, false)
		}
		p.connectFailed(ep, Phase(oc.class), err)
	}
	if oc.defectAlert {
		p.alertDefect(addr, err, oc.class, false)
	}
	if oc.sent != sentNever {
		return nil, oc, &MaybeSentError{Err: err}
	}
	return nil, oc, err
}

// routedOutcome is the outcome of a routed request of the given class, which
// sent clone and got err. A routed request never hops and never promotes, so
// the outcome only says what was sent, whether a send-by deadline refused it,
// and whether the pool refused it for a defect. It is sentNever only on a
// connect-phase failure, which for a mutation also needs provesNeverSent. A
// refused send-by carries the tag and follows the same rule, so a mutation Go
// may have replayed is maybe sent.
func routedOutcome(class reqClass, clone *http.Request, err error) outcome {
	if err == nil {
		return outcome{action: actReturn, sent: sentAnswered}
	}
	oc := outcome{action: actReturn, sent: sentMaybe, class: classTransport}
	cpe, tagged := AsConnectPhase(err)
	if !tagged {
		return oc
	}
	oc.class = string(cpe.Phase)
	if class != classMutation || provesNeverSent(clone, err) {
		oc.sent = sentNever
	}
	oc.defectAlert = isPoolRefusal(err)
	if errors.Is(err, ErrSendByPassed) {
		oc.sendBy, oc.class = true, classSendBy
	}
	return oc
}

// errNoResponse stands in for the error when a transport returns neither a
// response nor an error, which http.Transport never does.
var errNoResponse = errors.New("endpoint pool: transport returned neither a response nor an error")

// walker is one walk over the pool for one request. It is used by one
// goroutine.
type walker struct {
	p      *Pool
	req    *http.Request
	method string
	class  reqClass
	// configuredOnly says the walk covers the configured endpoints only.
	configuredOnly bool
	bounds         bounds
	budget         *budget
	order          []*endpoint
	// limit is how many hops the walk may make at most.
	limit int
	// gen is the generation the walk started under, and from the endpoint
	// that was active then, or nil when the walk could not read both under
	// one generation.
	gen  uint64
	from *endpoint
	// statusHopped says the request has spent its one status hop, in this
	// walk or in an earlier round trip of the same request, and held is the
	// response this walk's status hop moved away from.
	statusHopped bool
	held         *heldResponse
	fails        failures
	// hold is a mutation's hold on its key's tethers, which the walk
	// releases after a hop that proved nothing was sent and claims again for
	// the next hop. It is nil for a read, for the login, and for a walk that
	// follows a tether, whose hold dispatch settles.
	hold *tetherHold
	// tethered says the request goes only to the endpoint its key's tether
	// names, so the walk never hops.
	tethered bool
}

// walkEnd is what a walk returns when it ends before a hop.
type walkEnd struct {
	resp *http.Response
	oc   outcome
	err  error
}

// hopResult is one hop and the outcome table's verdict on it.
type hopResult struct {
	ep   *endpoint
	resp *http.Response
	err  error
	oc   outcome
}

// stopReason is why a walk starts no further hop.
type stopReason int

const (
	stopNone stopReason = iota
	// stopExhausted means the hop cap or the eligible endpoints ran out.
	stopExhausted
	// stopBudget means the connect or the walk budget ran out.
	stopBudget
	// stopCaller means the request's context ended.
	stopCaller
	// stopSendBy means the request's send-by deadline passed.
	stopSendBy
	// stopBody means the request body cannot be rebuilt for another hop,
	// because it has no GetBody.
	stopBody
	// stopRebuild means GetBody failed to rebuild the body for the next hop.
	stopRebuild
	// stopTethered means the request is a mutation that may not hop: it
	// follows its key's tether, or another request still holds or has fixed
	// the reservation it released.
	stopTethered
)

// newWalker reads the active endpoint, the walk order, and the generation for
// a walk. The active endpoint and the order are read under separate locks, so
// it reads them again when a failover elsewhere moved the generation between
// the two reads. After snapshotTries it walks without knowing which endpoint
// was active, which a configured-only walk reads as a discovered one when it
// decides whether its answer may move the active endpoint.
//
// It also reads whether an earlier round trip of the same request already
// spent the request's status hop, so the SDK's retries of one request share
// one status hop.
func (p *Pool) newWalker(req *http.Request, configuredOnly bool, b bounds) *walker {
	w := &walker{
		p:              p,
		req:            req,
		method:         methodOf(req),
		class:          classOf(req),
		configuredOnly: configuredOnly,
		bounds:         b,
	}
	for range snapshotTries {
		from, before := p.current()
		order, gen := p.walkOrder(configuredOnly)
		w.order, w.gen = order, gen
		if gen == before {
			w.from = from
			break
		}
	}
	w.limit = b.hopLimit(len(w.order))
	w.budget = b.begin()
	w.statusHopped = p.walk.statusHopSpent(req.URL, b.now())
	return w
}

// run makes the walk's hops and returns what the caller gets, with the outcome
// of the hop that ended the request.
func (w *walker) run() (*http.Response, outcome, error) {
	defer w.rememberStatusHop()
	if w.limit <= 0 {
		closeRequestBody(w.req)
		return nil, unsent, NewEntryUnreachableError(nil)
	}
	var body io.ReadCloser
	for i := 0; ; i++ {
		if i > 0 && w.hold != nil {
			if end := w.reclaim(i, body); end != nil {
				return end.resp, end.oc, end.err
			}
		}
		h := w.try(i, w.order[i], body)
		if h.oc.action != actHop {
			return w.finish(h)
		}
		// Only a proof that nothing was sent hops a mutation, so its hold is
		// released here, and the request hops only when that removed the
		// reservation.
		if w.tethered || (w.hold != nil && !w.p.release(w.hold)) {
			return w.halt(h, stopTethered, i+1, nil)
		}
		if stop := w.stopBefore(i + 1); stop != stopNone {
			return w.halt(h, stop, i+1, nil)
		}
		var err error
		if body, err = w.nextBody(); err != nil {
			return w.halt(h, stopRebuild, i+1, err)
		}
		w.proceed(h)
	}
}

// reclaim claims the tether of a mutation for hop i, after the hop before it
// released the request's hold, and returns nil when hop i goes ahead. body is
// the request body rebuilt for hop i.
//
// A conflicted key ends the walk with the refusal, and nothing more is sent.
// When the key gained a tether since the release, the request follows it and
// never hops again: hop i goes to the tether's endpoint instead of the one the
// walk order names, and a tether on a routed host outside the pool, or on an
// endpoint a configured-only request may not reach, ends the walk through the
// routed path.
func (w *walker) reclaim(i int, body io.ReadCloser) *walkEnd {
	if err := w.p.claim(w.hold, targetOf(w.order[i])); err != nil {
		closeSendBody(w.req, body)
		return &walkEnd{oc: unsent, err: err}
	}
	if !w.hold.following {
		return nil
	}
	w.tethered = true
	if ep := w.p.byAddr[w.hold.target.addr]; ep != nil && (!w.configuredOnly || ep.kind == KindConfigured) {
		w.order[i] = ep
		return nil
	}
	resp, oc, err := w.p.sendRouted(w.req, w.hold.target, body, w.configuredOnly)
	return &walkEnd{resp: resp, oc: oc, err: err}
}

// rememberStatusHop records, when the request has spent its status hop, that
// it has, so the next round trip of the same request does not hop on a status
// again. A request that spent it in an earlier round trip refreshes the
// record, which keeps it for as long as the request's retries go on.
func (w *walker) rememberStatusHop() {
	if w.statusHopped {
		w.p.walk.spendStatusHop(w.req.URL, w.bounds.now())
	}
}

// try makes hop i, to ep. body is the request body rebuilt for a hop after
// the first, and nil for the first hop or a request without a body. A
// discovered endpoint that failed its pin after the walk read its order gets
// nothing, and the hop is refused before any dial.
func (w *walker) try(i int, ep *endpoint, body io.ReadCloser) hopResult {
	if w.p.ineligibleNow(ep) {
		closeSendBody(w.req, body)
		return ineligibleHop(ep)
	}
	ctx := w.req.Context()
	meter := newConnectMeter(w.bounds.now)
	clone := w.req.Clone(connectContext(httptrace.WithClientTrace(ctx, meter.trace()), w.method, w.req.URL.Path))
	clone.URL.Host = ep.urlHost
	clone.Host = ""
	if body != nil {
		clone.Body = body
	}
	attr := attributable(w.req.URL.Path, ep)
	withDeadline := w.wantsDeadline(i, attr)
	resp, err := ep.transports.pick(withDeadline).RoundTrip(clone)
	if err == nil && resp == nil {
		err = errNoResponse
	}
	spent, gotConn := meter.result()
	w.budget.spend(spent)
	a := attempt{
		class:        w.class,
		kind:         ep.kind,
		err:          err,
		attributable: attr,
		withDeadline: withDeadline,
		callerDone:   ctx.Err() != nil,
		gotConn:      gotConn,
		provenUnsent: err != nil && provesNeverSent(clone, err),
		statusHopped: w.statusHopped,
	}
	if err == nil {
		a.status = resp.StatusCode
	}
	return hopResult{ep: ep, resp: resp, err: err, oc: classify(a)}
}

// wantsDeadline reports whether hop i uses the clone with the response-header
// deadline. Only an idempotent request whose stall can be blamed on the
// endpoint gets it, and never on the walk's final hop, so the last try waits
// for a slow answer as a request to a single host does today.
func (w *walker) wantsDeadline(i int, attributable bool) bool {
	return w.class == classIdempotent && attributable && !finalHop(i, w.limit)
}

// errNoGetBodyResult stands in for the error when GetBody returns neither a
// body nor an error.
var errNoGetBodyResult = errors.New("GetBody returned neither a body nor an error")

// nextBody rebuilds the request body for the next hop, because the transport
// closed the one before. A request without a body needs none and gets nil.
// stopBefore has already refused a body without GetBody.
func (w *walker) nextBody() (io.ReadCloser, error) {
	if !hasBody(w.req) {
		return nil, nil
	}
	if w.req.GetBody == nil {
		return nil, errors.New("endpoint pool: request body cannot be rebuilt for another endpoint")
	}
	body, err := w.req.GetBody()
	if err == nil && body == nil {
		err = errNoGetBodyResult
	}
	if err != nil {
		return nil, fmt.Errorf("endpoint pool: rebuilding the request body for another endpoint: %w", err)
	}
	return body, nil
}

// stopBefore reports why hop next may not start, or stopNone when it may.
func (w *walker) stopBefore(next int) stopReason {
	switch {
	case next >= w.limit:
		return stopExhausted
	case !w.budget.allowsHop():
		return stopBudget
	case w.req.Context().Err() != nil:
		return stopCaller
	case CheckSendBy(w.req.Context(), time.Now(), w.method, w.req.URL.Path) != nil:
		return stopSendBy
	case hasBody(w.req) && w.req.GetBody == nil:
		return stopBody
	default:
		return stopNone
	}
}

// finish ends the walk on a hop whose outcome is to answer or to return.
func (w *walker) finish(h hopResult) (*http.Response, outcome, error) {
	w.record(h)
	w.alert(h, false)
	oc := h.oc
	oc.addr = h.ep.addr
	if h.err != nil {
		if oc.sent == sentMaybe && IsConnectPhase(h.err) {
			return nil, oc, &MaybeSentError{Err: h.err}
		}
		return nil, oc, h.err
	}
	if oc.action == actAnswer {
		w.promote(h.ep)
	}
	h.resp.Request = w.req
	return h.resp, oc, nil
}

// proceed applies the effects of a hop the walk moves on from.
func (w *walker) proceed(h hopResult) {
	if h.oc.statusHop {
		w.statusHopped = true
		w.held = holdResponse(h.resp)
		w.record(h)
		return
	}
	w.record(h)
	w.alert(h, true)
	w.fails.add(h.ep.addr, h.err, h.oc.unreachable)
}

// halt ends the walk on a hop whose outcome is to hop, when stop says hop next
// may not start. A mutation that may not hop because of its tether ends as a
// walk that ran out of endpoints does, with the walk's failures, so a
// tethered request whose endpoint fails in the connect phase returns that
// error. A status hop that cannot proceed returns its response with no
// effect. Any other hop keeps its effects, and the walk returns the caller's
// context error, the send-by refusal, the response a status hop moved away
// from, or the walk's failures, in that order. When GetBody failed, its error
// is joined to the walk's failures, so the caller still sees why every
// earlier hop failed.
//
// The hop that would have come next sent nothing, so the outcome the walk
// returns keeps h's sent state, which for a mutation is the connect-phase
// proof that let it hop.
//
// A streamed upload cannot hop inside one request, so when its body cannot be
// rebuilt and the endpoint failed in the connect phase, the pool advances the
// active endpoint, and the upload's next attempt starts on another one.
func (w *walker) halt(h hopResult, stop stopReason, next int, rebuildErr error) (*http.Response, outcome, error) {
	oc := outcome{action: actReturn, sent: h.oc.sent, class: h.oc.class, addr: h.ep.addr}
	if h.oc.statusHop {
		h.resp.Request = w.req
		return h.resp, oc, nil
	}
	w.record(h)
	w.alert(h, false)
	w.fails.add(h.ep.addr, h.err, h.oc.unreachable)
	if hasBody(w.req) && w.req.GetBody == nil && blamesEndpoint(h.err) {
		if to := w.p.advance(h.ep, w.gen); to != nil {
			w.p.logFailover(h.ep, to, false)
		}
	}
	switch stop {
	case stopCaller:
		oc.class = classCancelled
		return nil, oc, w.req.Context().Err()
	case stopSendBy:
		oc.sendBy, oc.class = true, classSendBy
		return nil, oc, &ConnectPhaseError{Phase: PhaseDial, Endpoint: w.order[next].addr, Err: ErrSendByPassed}
	}
	if w.held != nil {
		oc.sent = sentAnswered
		return w.held.response(w.req), oc, nil
	}
	if stop == stopRebuild {
		oc.class = classRequest
		return nil, oc, errors.Join(rebuildErr, w.fails.err())
	}
	return nil, oc, w.fails.err()
}

// alert logs the Error lines a hop's outcome calls for. hopping says whether
// the request moves on to another endpoint.
func (w *walker) alert(h hopResult, hopping bool) {
	if h.oc.verifyAlert {
		w.p.alertVerify(h.ep, h.err, hopping)
	}
	if h.oc.defectAlert {
		w.p.alertDefect(h.ep.addr, h.err, h.oc.class, hopping)
	}
}

// record applies a hop's demotion and keeps its failure for the failover
// line. A demotion for a connect-phase failure, which only a failure the
// endpoint caused gets, also records the failure for the mark sink and, for a
// pin failure, takes the endpoint out of later walks.
func (w *walker) record(h hopResult) {
	if h.oc.demote {
		w.p.demote(h.ep)
	}
	if h.oc.demote || h.oc.unreachable {
		w.p.walk.note(h.ep, noteOf(h.resp, h.err, h.oc.class))
	}
	if phase := Phase(h.oc.class); h.oc.demote && phase.Valid() {
		w.p.connectFailed(h.ep, phase, h.err)
	}
}

// promote applies an answer from ep under the generation guard. A
// configured-only walk that started while a discovered endpoint was active
// skipped that endpoint, so its answer says nothing against it and leaves the
// active endpoint where it is.
func (w *walker) promote(ep *endpoint) {
	if w.configuredOnly && (w.from == nil || w.from.kind != KindConfigured) {
		return
	}
	w.p.promoteAfter(ep, w.gen)
}

// The limits on what a pool remembers of the status hops its requests spent.
// A record lasts statusHopTTL from the last round trip that used it, which
// covers the SDK's backoff between retries many times over, and the pool keeps
// at most statusHopCap of them. A record that expires or is evicted early only
// lets a later round trip of its request hop on a status once more, which is
// a status hop of a read or the login and never moves a mutation.
const (
	statusHopTTL = 10 * time.Minute
	statusHopCap = 1024
)

// walkState is what the walks of one pool remember across requests: the last
// failure of each endpoint, for the failover line, which configured endpoints
// have logged a verification failure, which addresses have logged a refusal
// the pool made for a defect, and which requests have spent their status hop.
// The zero value is ready to use.
//
// A status hop is remembered by the request's *url.URL. The SDK sends every
// retry of one request, including its re-authentication retry, as the same
// *http.Request, and the http.Client's per-attempt fork is a shallow copy that
// keeps the URL pointer, while its client timeout gives each attempt a context
// of its own that ends with the attempt. So the URL pointer is what stays the
// same across the round trips of one request, and the context is not. A layer
// that deep-copies the request before the pool, such as one calling req.Clone,
// gives each round trip a status hop of its own again, which is the cost of
// one extra status hop per round trip and nothing more.
type walkState struct {
	mu      sync.Mutex
	notes   map[*endpoint]failureNote
	alerted map[*endpoint]bool
	defects map[string]bool
	// statusHops maps a request's URL to the last time a round trip of it
	// used its spent status hop.
	statusHops map[*url.URL]time.Time
}

// note keeps n as ep's last failure.
func (s *walkState) note(ep *endpoint, n failureNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notes == nil {
		s.notes = make(map[*endpoint]failureNote)
	}
	s.notes[ep] = n
}

// noteFor returns ep's last failure.
func (s *walkState) noteFor(ep *endpoint) (failureNote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[ep]
	return n, ok
}

// firstAlert reports whether ep has not yet logged a verification failure, and
// records that it now has.
func (s *walkState) firstAlert(ep *endpoint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alerted[ep] {
		return false
	}
	if s.alerted == nil {
		s.alerted = make(map[*endpoint]bool)
	}
	s.alerted[ep] = true
	return true
}

// firstDefect reports whether addr has not yet logged a refusal the pool made
// for a defect, and records that it now has. It is keyed by address, because a
// routed host outside the pool has no endpoint of its own.
func (s *walkState) firstDefect(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.defects[addr] {
		return false
	}
	if s.defects == nil {
		s.defects = make(map[string]bool)
	}
	s.defects[addr] = true
	return true
}

// statusHopSpent reports whether a round trip of the request with URL u spent
// its status hop no longer than statusHopTTL before now. A record past that
// is dropped.
func (s *walkState) statusHopSpent(u *url.URL, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.statusHops[u]
	if !ok {
		return false
	}
	if now.Sub(last) >= statusHopTTL {
		delete(s.statusHops, u)
		return false
	}
	return true
}

// spendStatusHop records at now that the request with URL u has spent its
// status hop. When the pool already keeps statusHopCap records, it first drops
// the expired ones and then, if none expired, the one used longest ago.
func (s *walkState) spendStatusHop(u *url.URL, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusHops == nil {
		s.statusHops = make(map[*url.URL]time.Time)
	}
	if _, ok := s.statusHops[u]; !ok && len(s.statusHops) >= statusHopCap {
		s.evictStatusHopsLocked(now)
	}
	s.statusHops[u] = now
}

// evictStatusHopsLocked drops every expired status-hop record, or the oldest
// one when none has expired. s.mu must be held.
func (s *walkState) evictStatusHopsLocked(now time.Time) {
	var (
		oldest     *url.URL
		oldestTime time.Time
	)
	for u, last := range s.statusHops {
		if now.Sub(last) >= statusHopTTL {
			delete(s.statusHops, u)
			continue
		}
		if oldest == nil || last.Before(oldestTime) {
			oldest, oldestTime = u, last
		}
	}
	if len(s.statusHops) >= statusHopCap && oldest != nil {
		delete(s.statusHops, oldest)
	}
}

// logFailover logs the change of the active endpoint from one endpoint to
// another at Warn, naming the endpoint that lost it with its last failure.
// sent says the change came from a request that to answered, and otherwise it
// came from an upload that advanced the active endpoint for the next request.
func (p *Pool) logFailover(from, to *endpoint, sent bool) {
	n, ok := p.walk.noteFor(from)
	if !ok {
		n = failureNote{class: "unknown", text: "an earlier failure in this call"}
	}
	cause := log.ScrubMessage(n.text)
	tail := "request sent to " + to.addr + ", which stays active for this call"
	if !sent {
		tail = "next request goes to " + to.addr
	}
	p.logger.Warn("pve endpoint failover: "+from.addr+" unreachable ("+cause+"), "+tail,
		log.String("from", from.addr),
		log.String("to", to.addr),
		log.String("class", n.class),
		log.String("error", cause),
	)
}

// alertVerify logs, once per endpoint for the life of the pool, that a
// configured endpoint failed TLS verification. The operator's trust settings
// vouched for that endpoint, so the failure can mean interception. hopping
// says whether the request moves on to another endpoint.
func (p *Pool) alertVerify(ep *endpoint, err error, hopping bool) {
	if !p.walk.firstAlert(ep) {
		return
	}
	cause := log.ScrubMessage(noteOf(nil, err, string(PhaseTLSVerify)).text)
	next := "trying the next endpoint"
	if !hopping {
		next = "no other endpoint is tried for this request"
	}
	p.logger.Error("pve endpoint "+ep.addr+": TLS verification failed ("+cause+"); this can mean interception; "+next,
		log.String("endpoint", ep.addr),
		log.String("class", string(PhaseTLSVerify)),
		log.String("error", cause),
	)
}

// alertDefect logs, once per address for the life of the pool, that the pool
// refused to send a request through the owned transport for addr, because the
// transport was built for another address or because it checks its endpoint's
// certificate itself and was asked for plain http. Only a defect in the pool
// routes a request that way, so the line is an Error. hopping says whether the
// request moves on to another endpoint, and class is the refusal's phase.
func (p *Pool) alertDefect(addr string, err error, class string, hopping bool) {
	if !p.walk.firstDefect(addr) {
		return
	}
	cause := log.ScrubMessage(noteOf(nil, err, class).text)
	next := "trying the next endpoint"
	if !hopping {
		next = "no other endpoint is tried for this request"
	}
	p.logger.Error("pve endpoint "+addr+": the pool refused to send through its transport ("+cause+
		"); this is a defect in the endpoint pool; "+next,
		log.String("endpoint", addr),
		log.String("class", class),
		log.String("error", cause),
	)
}

// logSendBy logs, at Debug, a request refused because its send-by deadline
// passed. It is called once per refusal.
func (p *Pool) logSendBy(method, path string) {
	route := apiRelative(path)
	p.logger.Debug("pve endpoints: "+method+" "+route+" not sent, send-by deadline passed",
		log.String("method", method),
		log.String("path", route),
	)
}

// methodOf returns req's method, reading an empty one as GET, as net/http
// does.
func methodOf(req *http.Request) string {
	if req.Method == "" {
		return http.MethodGet
	}
	return req.Method
}

// hasBody reports whether req carries a body a hop would send.
func hasBody(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody
}

// closeRequestBody closes the body of a request the pool refuses before
// handing it to a transport, as an http.RoundTripper must.
func closeRequestBody(req *http.Request) {
	if hasBody(req) {
		_ = req.Body.Close()
	}
}

// closeSendBody closes the body a refused send would have carried: body when
// a walk rebuilt one for the hop, and the request's own body otherwise.
func closeSendBody(req *http.Request, body io.ReadCloser) {
	if body != nil {
		_ = body.Close()
		return
	}
	closeRequestBody(req)
}

// urlAddress returns the canonical host:port a URL addresses, with the
// scheme's default port when the URL names none.
func urlAddress(u *url.URL) (string, error) {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", fmt.Errorf("endpoint pool: request URL has unsupported scheme %q", u.Scheme)
		}
	}
	addr, err := canonicalHostPort(u.Hostname(), port)
	if err != nil {
		return "", fmt.Errorf("endpoint pool: request host %q: %w", u.Host, err)
	}
	return addr, nil
}
