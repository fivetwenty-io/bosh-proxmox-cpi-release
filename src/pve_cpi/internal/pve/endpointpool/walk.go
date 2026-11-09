package endpointpool

import (
	"errors"
	"fmt"
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
// that wraps ErrSendByPassed, before any dial. It then sorts the request into
// one of three kinds:
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
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	return p.roundTrip(req, defaultBounds())
}

// roundTrip is RoundTrip under the given bounds.
func (p *Pool) roundTrip(req *http.Request, b bounds) (*http.Response, error) {
	if req == nil {
		return nil, errNilRequest
	}
	if p == nil {
		closeRequestBody(req)
		return nil, errNilPool
	}
	if req.URL == nil {
		closeRequestBody(req)
		return nil, errNoURL
	}
	method := methodOf(req)
	if err := CheckSendBy(req.Context(), time.Now(), method, req.URL.Path); err != nil {
		closeRequestBody(req)
		p.logSendBy(method, req.URL.Path)
		return nil, &ConnectPhaseError{Phase: PhaseDial, Endpoint: req.URL.Host, Err: err}
	}
	addr, err := urlAddress(req.URL)
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}
	// A tethered mutation goes only to its tethered endpoint, so a tether
	// check belongs here, ahead of the routed and configured-only kinds it
	// overrides.
	configuredOnly := IsConfiguredOnly(req.Context()) || isLogin(req)
	if addr != p.endpoints[0].addr {
		return p.sendRouted(req, addr, configuredOnly)
	}
	return p.newWalker(req, configuredOnly, b).run()
}

// sendRouted sends a routed request once, through the owned clone for its
// host, which is an endpoint's own clone when the host is one of the pool's
// endpoints. It uses the clone without a header deadline, because a routed
// request never hops.
//
// A configured-only request goes to a routed host only when that host is a
// configured endpoint, so a credential never reaches an address the operator
// did not list. A failure that blamesEndpoint counts against one of the pool's
// endpoints demotes it, and nothing a routed request gets ever makes an
// endpoint active. The error is marked with MaybeSentError unless it is a
// connect-phase failure of an idempotent request, or of a mutation that
// provesNeverSent clears.
func (p *Pool) sendRouted(req *http.Request, addr string, configuredOnly bool) (*http.Response, error) {
	ep := p.byAddr[addr]
	if configuredOnly && (ep == nil || ep.kind != KindConfigured) {
		closeRequestBody(req)
		return nil, fmt.Errorf("endpoint pool: refusing a configured-only request to %s, which is not a configured endpoint",
			addr)
	}
	o, err := p.routedTransports(req.URL)
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}
	method := methodOf(req)
	clone := req.Clone(connectContext(req.Context(), method, req.URL.Path))
	resp, err := o.pick(false).RoundTrip(clone)
	if err == nil {
		if resp == nil {
			return nil, &MaybeSentError{Err: errNoResponse}
		}
		resp.Request = req
		return resp, nil
	}
	if errors.Is(err, ErrSendByPassed) {
		p.logSendBy(method, req.URL.Path)
		return nil, err
	}
	if ep != nil && blamesEndpoint(err) {
		cpe, _ := AsConnectPhase(err)
		p.demote(ep)
		p.walk.note(ep, noteOf(nil, err, string(cpe.Phase)))
		if cpe.Phase == PhaseTLSVerify && ep.kind == KindConfigured {
			p.alertVerify(ep, err, false)
		}
	}
	if !IsConnectPhase(err) || (classOf(req) == classMutation && !provesNeverSent(clone, err)) {
		return nil, &MaybeSentError{Err: err}
	}
	return nil, err
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
	// statusHopped says the request has spent its one status hop, and held
	// is the response that hop moved away from.
	statusHopped bool
	held         *heldResponse
	fails        failures
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
	// stopBody means the request body cannot be rebuilt for another hop.
	stopBody
)

// newWalker reads the active endpoint, the walk order, and the generation for
// a walk. The active endpoint and the order are read under separate locks, so
// it reads them again when a failover elsewhere moved the generation between
// the two reads. After snapshotTries it walks without knowing which endpoint
// was active, which only costs the failover line of a change it causes.
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
	return w
}

// run makes the walk's hops and returns what the caller gets.
func (w *walker) run() (*http.Response, error) {
	if w.limit <= 0 {
		closeRequestBody(w.req)
		return nil, NewEntryUnreachableError(nil)
	}
	for i := 0; ; i++ {
		h := w.try(i, w.order[i])
		if h.oc.action != actHop {
			return w.finish(h)
		}
		if stop := w.stopBefore(i + 1); stop != stopNone {
			return w.halt(h, stop, i+1)
		}
		w.proceed(h)
	}
}

// try makes hop i, to ep.
func (w *walker) try(i int, ep *endpoint) hopResult {
	ctx := w.req.Context()
	meter := newConnectMeter(w.bounds.now)
	clone := w.req.Clone(connectContext(httptrace.WithClientTrace(ctx, meter.trace()), w.method, w.req.URL.Path))
	clone.URL.Host = ep.urlHost
	clone.Host = ""
	if i > 0 {
		if err := w.rebuildBody(clone); err != nil {
			return hopResult{ep: ep, err: err, oc: outcome{action: actReturn, sent: sentNever, class: classRequest}}
		}
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

// rebuildBody gives the clone of a hop after the first a fresh body, because
// the transport closed the one before. A request without a body needs none.
func (w *walker) rebuildBody(clone *http.Request) error {
	if !hasBody(w.req) {
		return nil
	}
	if w.req.GetBody == nil {
		return errors.New("endpoint pool: request body cannot be rebuilt for another endpoint")
	}
	body, err := w.req.GetBody()
	if err != nil {
		return fmt.Errorf("endpoint pool: rebuilding the request body for another endpoint: %w", err)
	}
	clone.Body = body
	return nil
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
func (w *walker) finish(h hopResult) (*http.Response, error) {
	w.record(h)
	if h.oc.verifyAlert {
		w.p.alertVerify(h.ep, h.err, false)
	}
	if h.err != nil {
		if h.oc.sendBy {
			w.p.logSendBy(w.method, w.req.URL.Path)
		}
		if h.oc.sent == sentMaybe && IsConnectPhase(h.err) {
			return nil, &MaybeSentError{Err: h.err}
		}
		return nil, h.err
	}
	if h.oc.action == actAnswer {
		w.promote(h.ep)
	}
	h.resp.Request = w.req
	return h.resp, nil
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
	if h.oc.verifyAlert {
		w.p.alertVerify(h.ep, h.err, true)
	}
	w.fails.add(h.ep.addr, h.err, h.oc.unreachable)
}

// halt ends the walk on a hop whose outcome is to hop, when stop says hop next
// may not start. A status hop that cannot proceed returns its response with no
// effect. Any other hop keeps its effects, and the walk returns the caller's
// context error, the send-by refusal, the response a status hop moved away
// from, or the walk's failures, in that order.
//
// A streamed upload cannot hop inside one request, so when its body cannot be
// rebuilt and the endpoint failed in the connect phase, the pool advances the
// active endpoint, and the upload's next attempt starts on another one.
func (w *walker) halt(h hopResult, stop stopReason, next int) (*http.Response, error) {
	if h.oc.statusHop {
		h.resp.Request = w.req
		return h.resp, nil
	}
	w.record(h)
	if h.oc.verifyAlert {
		w.p.alertVerify(h.ep, h.err, false)
	}
	w.fails.add(h.ep.addr, h.err, h.oc.unreachable)
	if hasBody(w.req) && w.req.GetBody == nil && blamesEndpoint(h.err) && w.p.advance(h.ep, w.gen) {
		to, _ := w.p.current()
		w.p.logFailover(h.ep, to, false)
	}
	switch stop {
	case stopCaller:
		return nil, w.req.Context().Err()
	case stopSendBy:
		w.p.logSendBy(w.method, w.req.URL.Path)
		return nil, &ConnectPhaseError{Phase: PhaseDial, Endpoint: w.order[next].addr, Err: ErrSendByPassed}
	}
	if w.held != nil {
		return w.held.response(w.req), nil
	}
	return nil, w.fails.err()
}

// record applies a hop's demotion and keeps its failure for the failover
// line.
func (w *walker) record(h hopResult) {
	if h.oc.demote {
		w.p.demote(h.ep)
	}
	if h.oc.demote || h.oc.unreachable {
		w.p.walk.note(h.ep, noteOf(h.resp, h.err, h.oc.class))
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
	w.p.promoteAfter(ep, w.from, w.gen)
}

// walkState is what the walks of one pool remember for its log lines: the last
// failure of each endpoint, for the failover line, and which configured
// endpoints have logged a verification failure. The zero value is ready to use.
type walkState struct {
	mu      sync.Mutex
	notes   map[*endpoint]failureNote
	alerted map[*endpoint]bool
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
