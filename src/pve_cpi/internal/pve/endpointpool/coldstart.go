package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// kindCandidate is the kind of a cold-start candidate's endpoint. It is never
// passed in Config.Endpoints. New builds such endpoints from Config.Candidates,
// and they are kept apart from the configured and discovered endpoints, so no
// walk order, routed request, or start of a pool ever names one. A walk
// reaches a candidate only through the cold start, which it enters only when
// every configured endpoint failed in the connect phase in that walk.
const kindCandidate Kind = KindDiscovered + 1

// The limits of the cold start. A pool carries at most maxCandidates. The probe
// waits at most probeHeaderDeadline for the answer's headers, on every
// candidate hop including a walk's last, because the probe is not the request
// the walk carries. probeLimit bounds the whole probe, which is the dial, the
// handshake, the header deadline, and the reading of the answer's body, so a
// candidate costs at most 25 seconds before its request even when it sends the
// probe's headers and then stalls its body.
const (
	maxCandidates       = 64
	probeHeaderDeadline = 10 * time.Second
	probeLimit          = candidateDialCap + candidateHandshakeCap + probeHeaderDeadline
)

// ProbeLimit is the time one candidate's probe may take, and so the least time
// a request's context must have left for the cold start to run on it. A caller
// that must keep the cold start available to a request checks its context
// against this figure and not a copy of it. The handler tests read it too, so
// they hold the same figure the pool does.
const ProbeLimit = probeLimit

// ProbePath is the URL path of the membership probe, GET /cluster/status under
// the API prefix. The probe transport refuses every other path.
const ProbePath = APIPrefix + "/cluster/status"

// apiTokenScheme starts the Authorization header of a request that
// authenticates with a PVE API token. The cold start begins only for such a
// request, so a ticket cookie, a CSRF token, or a password never goes to an
// address the operator did not list.
const apiTokenScheme = "PVEAPIToken=" // #nosec G101 -- the header's scheme name, which carries no secret

// The discovery outcomes a cache file records, as Gate reads them. They are
// the values of the cache's DiscoveryOutcome, and a test in this package reads
// that enumeration from the cache's source, so a value added there fails the
// test until Gate decides it. The empty outcome is a file without a discovery
// result the pool may use, which is a missing file, a file that fails
// validation on read, or one that holds only a cold_start hint.
const (
	outcomeNone         = ""
	outcomeOK           = "ok"
	outcomeNoStatus     = "no_status"
	outcomeNoCandidates = "no_candidates"
	outcomeStandalone   = "standalone"
	outcomePublicSuffix = "public_suffix"
)

// Gate reports whether a pool carries cold-start candidates.
//
// azMapNodes is the config's answer, the node names of the entry's
// placement.az_map, which the caller passes empty when any config condition
// for the cold start fails: a discovery mode other than on, verify_ssl false,
// no pve.ca_cert, password auth, or an az_map the entry does not own.
// usableDiscovered is the number of discovered entries the key's cache file
// holds that pass validation on read, the node-name helper, and the 24-hour
// bound, and that the pool loads. outcome is the discovery outcome the file
// records. The caller passes the empty outcome when the file is missing, when
// it holds no discovery result, and when it fails validation on read, because
// nothing in a file that fails validation may be trusted, so such a file
// counts as missing and its count is 0.
//
// Candidates are carried only when azMapNodes names at least one node, the
// file holds no usable discovered entry, and the outcome is not standalone,
// which says a configured endpoint reported that the host has no peers. A
// missing file, the outcomes no_candidates and public_suffix, a file that
// holds only a hint, and a file whose nodes are all older than 24 hours each
// leave the cold start possible. A negative count or an outcome outside the
// cache's enumeration fails closed and carries none.
func Gate(azMapNodes []string, usableDiscovered int, outcome string) bool {
	if usableDiscovered != 0 {
		return false
	}
	switch outcome {
	case outcomeNone, outcomeOK, outcomeNoStatus, outcomeNoCandidates, outcomePublicSuffix:
	default:
		return false
	}
	return slices.ContainsFunc(azMapNodes, func(n string) bool { return strings.TrimSpace(n) != "" })
}

// Candidate is a node the entry's placement.az_map names, offered to a pool as
// a cold-start candidate.
type Candidate struct {
	// Node is the az_map name the candidate came from. A probe accepts the
	// candidate only when the node reports this name as its local node.
	Node string
	// Address is the host:port derived from Node with the seed's port. The
	// candidate's certificate must cover its host.
	Address string
}

// ProbeRequest is what a pool hands its Prober for one candidate.
type ProbeRequest struct {
	// Candidate is the candidate to probe, with Address as the pool was given
	// it.
	Candidate Candidate
	// Authorization is the API token header of the request that began the
	// cold start, which the probe sends as its only Authorization header. It
	// always starts with "PVEAPIToken=".
	Authorization string
	// Transport sends the probe. It accepts one GET of ProbePath over https at
	// the candidate's Address, with Authorization as its only Authorization
	// header and with no cookie and no CSRF token, and it refuses every other
	// request, any second request, and any request after Probe returned,
	// before any dial. The connection it makes is accepted only when the
	// candidate's certificate chains to the operator's CA and covers the
	// candidate's host, with no pin, so the token leaves only after both
	// checks pass. It waits at most 10 seconds for the answer's headers, and
	// the probe's own time limit applies to the request and its body whatever
	// context the request carries.
	Transport http.RoundTripper
}

// Prober decides whether a cold-start candidate is a member of the entry's
// cluster.
//
// Probe sends GET ProbePath through req.Transport and returns nil only when
// the answer proves membership, which is a 2xx whose local node is
// req.Candidate.Node and whose node list holds every valid name in the entry's
// az_map. It reads and closes the response body before it returns. Any error
// refuses the candidate for the rest of the process, except an error after
// the request's own context ended before the candidate answered, which leaves
// the candidate to the next request that reaches the cold start. ctx ends when
// the request that probes ends, and after 25 seconds at most.
//
// The pool accepts a candidate only on a complete proof: Probe returned nil,
// the probe it sent through req.Transport got a 2xx before Probe returned, and
// Probe closed that response's body before it returned. A Prober that returns
// nil without probing, after an error or another status, while its probe is
// still in flight, or with the body still open accepts nothing. The pool
// recovers a panic in Probe and refuses the candidate. A Prober logs its own
// refusals, and the pool calls it on the walk's goroutine, outside every lock
// the pool holds.
type Prober interface {
	Probe(ctx context.Context, req ProbeRequest) error
}

// noProber reports whether pr is nil or a typed nil, such as a nil pointer in
// a non-nil interface, which a pool treats as no prober at all.
func noProber(pr Prober) bool {
	if pr == nil {
		return true
	}
	v := reflect.ValueOf(pr)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// candidate is one cold-start candidate of a pool. ep is its endpoint, whose
// idempotent clone carries the pool's response-header deadline and whose plain
// clone carries none. A request on a candidate that is its walk's final hop
// uses the plain clone, as every final hop does. probe is the clone the
// membership probe goes through, with the probe's own header deadline. All of
// them check the certificate with the candidate's callback and dial under the
// candidate's connect caps.
type candidate struct {
	ep    *endpoint
	probe *http.Transport
}

// coldLimits are the time limits of one pool's cold start. New fills them
// with the package's defaults, which the package's tests may replace.
type coldLimits struct {
	// probe bounds one whole probe, and a request with less than this left
	// before its context's deadline never starts the cold start.
	probe time.Duration
	// dialCap and handshakeCap bound a candidate's TCP dial and TLS
	// handshake.
	dialCap      time.Duration
	handshakeCap time.Duration
	// headerDeadline is the response-header deadline of a candidate's
	// idempotent clone.
	headerDeadline time.Duration
}

// coldStart is the cold start of one pool. A process runs one cold start. One
// walk at a time probes its candidates, and every other walk that reaches it
// meanwhile waits for that walk. When the walk that probes them stops because
// its own request was cancelled, its send-by deadline passed, or its context
// left less than one probe may take, the candidates it never probed pass to
// the next walk that reaches the cold start, which goes on where it stopped.
// A candidate whose probe that request's own context cut before it answered
// counts as never probed. Every other stop, and an accepted candidate, ends
// the cold start for the process.
type coldStart struct {
	prober     Prober
	candidates []*candidate
	limits     coldLimits
	// onWait, when set, is called by a walk just before it waits for the
	// walk that runs the cold start. Only the package's tests set it.
	onWait func()
	// proberPanicked says the prober has panicked once already, so a later
	// panic is logged at Debug.
	proberPanicked atomic.Bool

	mu sync.Mutex
	// begun says a walk has claimed the cold start. It never goes back.
	begun bool
	// running says a walk is probing candidates now.
	running bool
	// next is the first candidate no walk has probed to an answer. A
	// candidate before it was refused, so no candidate that answered is
	// probed twice.
	next int
	// over says the cold start has ended for the process, with accepted as
	// its result, which is nil when no candidate was accepted.
	over     bool
	accepted *endpoint
	// done is closed when the walk probing the candidates stops. A stop that
	// passes the rest on replaces it, so the walks that reach the cold start
	// later wait on the next run.
	done chan struct{}
}

// coldRole is what claim tells a walk to do at the cold start.
type coldRole int

const (
	// coldRun means the walk probes the candidates from coldTurn.start.
	coldRun coldRole = iota
	// coldWait means another walk is probing them, and the walk waits for
	// coldTurn.done before it asks again.
	coldWait
	// coldRead means the cold start is over, with coldTurn.accepted as its
	// result.
	coldRead
	// coldBarred means no walk is probing them and this walk may not.
	coldBarred
)

// coldTurn is claim's answer.
type coldTurn struct {
	role     coldRole
	start    int
	done     <-chan struct{}
	accepted *endpoint
}

// claim tells the caller what to do at the cold start. mayRun says whether the
// caller may probe candidates itself. The state and the result are read under
// the lock finish writes them under, so a walk woken by done always sees the
// result of the run that closed it.
func (cs *coldStart) claim(mayRun bool) coldTurn {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	switch {
	case cs.over:
		return coldTurn{role: coldRead, accepted: cs.accepted}
	case cs.running:
		return coldTurn{role: coldWait, done: cs.done}
	case !mayRun:
		return coldTurn{role: coldBarred}
	}
	cs.begun, cs.running = true, true
	return coldTurn{role: coldRun, start: cs.next}
}

// finish records the end of a run that stopped before candidate next, which
// accepted accepted, or nil, and wakes every walk waiting for it. handOn says
// the run stopped for its own request, so any candidate left passes to the
// next walk. It reports whether the cold start is over for the process.
func (cs *coldStart) finish(accepted *endpoint, next int, handOn bool) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.running, cs.next = false, next
	switch {
	case accepted != nil:
		cs.accepted, cs.over = accepted, true
	case !handOn || next >= len(cs.candidates):
		cs.over = true
	}
	close(cs.done)
	if !cs.over {
		cs.done = make(chan struct{})
	}
	return cs.over
}

// acceptedEndpoint returns the candidate the cold start accepted, or nil.
func (cs *coldStart) acceptedEndpoint() *endpoint {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.accepted
}

// buildColdStart validates cfg's candidates and builds their clones. A pool
// with no candidates gets no cold start, and so does one whose Prober is nil
// or a typed nil, which is logged at Warn because only a defect in the caller
// passes candidates without a prober. Candidates need verify_ssl true with the
// operator's CA, which is pve.ca_cert, as the trust anchor, and a pool without
// discovered endpoints, because the cold start exists only for a cache that
// holds no usable discovered entry. Each candidate needs a node name with no
// surrounding space and a valid host:port that is neither a configured
// endpoint nor another candidate, and no two share a node.
func (p *Pool) buildColdStart(cfg Config) error {
	if len(cfg.Candidates) == 0 {
		return nil
	}
	if noProber(cfg.Prober) {
		p.logger.Warn("pve endpoints: "+strconv.Itoa(len(cfg.Candidates))+" cold-start candidates from "+
			"placement.az_map came without a prober, so the pool carries none",
			log.Int("candidates", len(cfg.Candidates)))
		return nil
	}
	if len(cfg.Candidates) > maxCandidates {
		return fmt.Errorf("endpoint pool: %d cold-start candidates, at most %d are allowed",
			len(cfg.Candidates), maxCandidates)
	}
	tlsCfg := p.base.TLSClientConfig
	if !p.verifySSL || tlsCfg == nil || tlsCfg.RootCAs == nil {
		return errors.New("endpoint pool: cold-start candidates need verify_ssl true and the operator's CA, " +
			"so a candidate must chain to pve.ca_cert")
	}
	for _, ep := range p.endpoints {
		if ep.kind == KindDiscovered {
			return fmt.Errorf("endpoint pool: cold-start candidates with discovered endpoint %s, and the cold "+
				"start runs only when the cache holds no usable discovered entry", ep.addr)
		}
	}
	cs := &coldStart{
		prober: cfg.Prober,
		limits: coldLimits{
			probe:          orDefault(cfg.probeLimit, probeLimit),
			dialCap:        orDefault(cfg.candidateDialCap, candidateDialCap),
			handshakeCap:   orDefault(cfg.candidateHandshakeCap, candidateHandshakeCap),
			headerDeadline: orDefault(cfg.headerDeadline, defaultHeaderDeadline),
		},
		onWait: cfg.onColdStartWait,
		done:   make(chan struct{}),
	}
	addrs := make(map[string]struct{}, len(cfg.Candidates))
	nodes := make(map[string]struct{}, len(cfg.Candidates))
	for _, in := range cfg.Candidates {
		c, err := p.newCandidate(in, cs.limits, addrs, nodes)
		if err != nil {
			return err
		}
		cs.candidates = append(cs.candidates, c)
	}
	p.cold = cs
	return nil
}

// orDefault returns d when it is positive, and def otherwise.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// newCandidate validates one candidate against the pool and the candidates
// before it, recorded in addrs and nodes, and builds its clones under lim.
func (p *Pool) newCandidate(in Candidate, lim coldLimits, addrs, nodes map[string]struct{}) (*candidate, error) {
	if in.Node == "" || strings.TrimSpace(in.Node) != in.Node {
		return nil, fmt.Errorf("endpoint pool: cold-start candidate %q has node name %q, which is empty or has "+
			"surrounding space", in.Address, in.Node)
	}
	addr, err := canonicalAddress(in.Address)
	if err != nil {
		return nil, fmt.Errorf("endpoint pool: cold-start candidate %q: %w", in.Address, err)
	}
	if p.byAddr[addr] != nil {
		return nil, fmt.Errorf("endpoint pool: cold-start candidate %s is a configured endpoint", addr)
	}
	if _, dup := addrs[addr]; dup {
		return nil, fmt.Errorf("endpoint pool: cold-start candidate %s is listed twice", addr)
	}
	if _, dup := nodes[in.Node]; dup {
		return nil, fmt.Errorf("endpoint pool: cold-start candidate node %s is listed twice", in.Node)
	}
	addrs[addr], nodes[in.Node] = struct{}{}, struct{}{}
	tlsCfg := p.base.TLSClientConfig
	v, err := newCandidateVerifier(hostOf(addr), p.verifySSL, tlsCfg.RootCAs, tlsCfg.Time)
	if err != nil {
		return nil, err
	}
	spec := transportSpec{
		addr:           addr,
		serverName:     hostOf(in.Address),
		verify:         v.verifyConnection,
		headerDeadline: lim.headerDeadline,
		dialCap:        lim.dialCap,
		handshakeCap:   lim.handshakeCap,
	}
	o, err := newOwnedTransports(p.base, spec)
	if err != nil {
		return nil, err
	}
	probe, err := newClone(p.base, spec, probeHeaderDeadline)
	if err != nil {
		return nil, err
	}
	ep := &endpoint{addr: addr, urlHost: in.Address, kind: kindCandidate, node: in.Node, transports: o, rank: -1}
	return &candidate{ep: ep, probe: probe}, nil
}

// candidateCount returns how many cold-start candidates the pool carries.
func (p *Pool) candidateCount() int {
	if p.cold == nil {
		return 0
	}
	return len(p.cold.candidates)
}

// closeCandidates closes the idle connections of every candidate's clones.
func (p *Pool) closeCandidates() {
	if p.cold == nil {
		return
	}
	for _, c := range p.cold.candidates {
		c.ep.transports.closeIdle()
		c.probe.CloseIdleConnections()
	}
}

// memberAt returns the pool endpoint with the canonical address addr, which is
// a configured or discovered endpoint or the accepted candidate, or nil. A
// candidate no probe accepted is never a member, so a tether on its address
// is followed the way one on a routed host outside the pool is.
func (p *Pool) memberAt(addr string) *endpoint {
	if ep := p.byAddr[addr]; ep != nil {
		return ep
	}
	if p.cold == nil {
		return nil
	}
	if ep := p.cold.acceptedEndpoint(); ep != nil && ep.addr == addr {
		return ep
	}
	return nil
}

// acceptedFor returns ep, the candidate the cold start accepted, for a walk
// whose order is order, or nil when there is none, when it failed TLS
// verification since, or when the walk already has it.
func (p *Pool) acceptedFor(ep *endpoint, order []*endpoint) *endpoint {
	if ep == nil || slices.Contains(order, ep) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep.ineligible {
		return nil
	}
	return ep
}

// tokenAuthorization returns the Authorization header of req when req
// authenticates with exactly one PVE API token and carries no ticket cookie
// and no CSRF token, and the empty string otherwise. Only such a request may
// begin the cold start or wait for one.
func tokenAuthorization(req *http.Request) string {
	if len(req.Header.Values("Cookie")) > 0 || len(req.Header.Values("CSRFPreventionToken")) > 0 {
		return ""
	}
	vals := req.Header.Values("Authorization")
	if len(vals) != 1 || len(vals[0]) <= len(apiTokenScheme) || !strings.HasPrefix(vals[0], apiTokenScheme) {
		return ""
	}
	return vals[0]
}

// opensColdStart reports whether hop h, to a configured endpoint, failed in a
// way that lets the walk go on to the cold start. Only a dial, DNS, or
// handshake failure that the endpoint caused does. A TLS verification failure
// can mean interception, and a header-deadline expiry, a status, or any other
// failure after a connection is not a connect-phase failure, so each of those
// bars the cold start for the rest of the walk.
func opensColdStart(h hopResult) bool {
	if h.err == nil || h.oc.statusHop || !h.oc.unreachable || !blamesEndpoint(h.err) {
		return false
	}
	switch Phase(h.oc.class) {
	case PhaseDial, PhaseDNS, PhaseTLSHandshake:
		return true
	default:
		return false
	}
}

// noteColdStart counts hop h, which the walk moves on from, when it is a
// configured endpoint's dial, DNS, or handshake failure. Any other hop to a
// configured endpoint, such as a verification failure, a header-deadline
// expiry, or a status hop, leaves that endpoint uncounted, so the walk never
// reaches the cold start.
func (w *walker) noteColdStart(h hopResult) {
	if h.ep.kind == KindConfigured && opensColdStart(h) {
		w.coldFailed++
	}
}

// coldStartDue reports whether the walk, whose order ran out before hop next,
// goes on to the cold start. Every walk that reaches a candidate goes through
// here, the walks after a candidate was accepted included, so an accepted
// candidate stays behind the same conditions as the cold start. That needs a
// pool that carries candidates and a walk that has not reached the cold start
// yet, that walks the whole pool, and that follows no tether, so the login, a
// discovery request, and a mutation tethered to a configured endpoint never
// do. A mutation tethered to the accepted candidate goes there through
// sendFollowing or reclaim, under followCandidate, and never through here. The run loop already
// halts a tethered walk before it asks, and the check here keeps the rule if
// that order changes. Every configured endpoint must have failed in the
// connect phase in this walk, by opensColdStart, and the hop cap and both
// budgets must still allow a hop, because a candidate's hop counts against
// them. The request must carry an API token and no ticket, and a body it can
// rebuild.
func (w *walker) coldStartDue(next int) bool {
	switch {
	case w.p.cold == nil, w.coldDone, w.configuredOnly, w.tethered:
		return false
	case w.coldFailed < w.p.configured:
		return false
	case next < len(w.order), w.made >= w.bounds.hops, !w.budget.allowsHop():
		return false
	case hasBody(w.req) && w.req.GetBody == nil:
		return false
	default:
		return tokenAuthorization(w.req) != ""
	}
}

// enterColdStart takes the walk through the cold start after hop h, which the
// walk moves on from, and returns nil when an accepted candidate was put at the
// end of the walk's order for hop next. Otherwise it returns what the walk
// ends with: the caller's context error, the send-by refusal, or the walk's
// own failures, which list the candidates it refused.
func (w *walker) enterColdStart(h hopResult, next int) *walkEnd {
	w.proceed(h)
	ep, stop := w.coldStart()
	if ep != nil {
		w.order = append(w.order, ep)
		stop = w.requestStop()
	}
	if stop == stopNone {
		return nil
	}
	resp, oc, err := w.end(h, stop, next, nil)
	return &walkEnd{resp: resp, oc: oc, err: err}
}

// coldStart returns the accepted candidate this walk sends its request to, or
// nil with the reason the walk stops. A walk that may probe and finds no walk
// probing takes the candidates no walk has probed yet. A walk that finds one
// probing waits for it within its own budgets and then asks again, so it
// takes over the candidates that walk passed on, if any. A walk that gets
// here after the cold start ended reads its result. Either way a walk sends
// only to the candidate a probe accepted, no candidate that answered is probed
// twice, and no second cold start begins.
func (w *walker) coldStart() (*endpoint, stopReason) {
	w.coldDone = true
	cs := w.p.cold
	for {
		mayRun, why := w.mayRunColdStart(cs.limits.probe)
		turn := cs.claim(mayRun)
		switch turn.role {
		case coldRun:
			return w.runColdStart(cs, turn.start)
		case coldRead:
			return w.acceptedHop(turn.accepted)
		case coldBarred:
			route := apiRelative(w.req.URL.Path)
			w.p.logger.Debug("pve endpoints: no configured endpoint could connect, and "+w.method+" "+route+
				" does not start the cold start from placement.az_map, because "+why,
				log.String("method", w.method),
				log.String("path", route),
			)
			return nil, stopExhausted
		}
		if stop := w.awaitColdStart(cs, turn.done); stop != stopNone {
			return nil, stop
		}
	}
}

// mayRunColdStart reports whether this walk may probe candidates itself, and
// why not when it may not. A request with a send-by deadline may not, and
// neither may one whose context leaves it less than limit, the time one probe
// may take. Either would stop the cold start early for a reason that is only
// its own, and burn candidates that a request with time to spare could have
// probed.
func (w *walker) mayRunColdStart(limit time.Duration) (bool, string) {
	ctx := w.req.Context()
	if _, ok := SendBy(ctx, w.method, w.req.URL.Path); ok {
		return false, "it carries a send-by deadline"
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < limit {
		return false, "its context has less than " + limit.String() + " left, the time one probe may take"
	}
	return true, ""
}

// acceptedHop returns ep, the candidate the cold start accepted, when the walk
// may still send to it, or nil with the reason the walk stops.
func (w *walker) acceptedHop(ep *endpoint) (*endpoint, stopReason) {
	switch {
	case w.p.acceptedFor(ep, w.order) == nil, w.made >= w.bounds.hops:
		return nil, stopExhausted
	case !w.budget.allowsHop():
		return nil, stopBudget
	default:
		return ep, stopNone
	}
}

// awaitColdStart waits for done, which the walk probing the candidates closes
// when it stops. It stops waiting when the request's context ends, when its
// send-by deadline passes, or when the walk budget runs out, and reports which
// happened.
func (w *walker) awaitColdStart(cs *coldStart, done <-chan struct{}) stopReason {
	select {
	case <-done:
		return stopNone
	default:
	}
	remaining := w.bounds.total - w.bounds.now().Sub(w.budget.start)
	if remaining <= 0 {
		return stopBudget
	}
	if cs.onWait != nil {
		cs.onWait()
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	var sendBy <-chan time.Time
	if deadline, ok := SendBy(w.req.Context(), w.method, w.req.URL.Path); ok {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		sendBy = t.C
	}
	select {
	case <-done:
		return stopNone
	case <-w.req.Context().Done():
		return stopCaller
	case <-sendBy:
		return stopSendBy
	case <-timer.C:
		return stopBudget
	}
}

// runColdStart probes the candidates in order from start, as the one walk
// probing them, and returns the first one accepted, or nil with the reason it
// stopped. Before each candidate it checks the hop cap, both budgets, the
// caller's context, the send-by deadline, and that the caller's context leaves
// at least the probe limit, so the request's own deadline never cuts a probe
// it starts. A probe's whole time counts as connect time. A refused candidate
// uses a hop and joins the walk's failures, and an accepted one uses the hop
// its request goes out on. A probe the request's own context cut before the
// candidate answered refuses nothing: the candidate counts as never probed,
// uses no hop, and joins no failures. The run is recorded as stopped however
// it returns, so a walk waiting for it never waits past its own budget. When
// it stops for the caller's context, its send-by deadline, or a context too
// short for a probe, the candidates it did not probe pass to the next walk.
func (w *walker) runColdStart(cs *coldStart, start int) (accepted *endpoint, stop stopReason) {
	next, probed := start, 0
	defer func() {
		over := cs.finish(accepted, next, handsOn(stop))
		if accepted == nil && stop != stopNone {
			w.p.logColdStartStop(stop, probed, len(cs.candidates)-next, over)
		}
	}()
	w.p.logger.Debug("pve endpoints: no configured endpoint could connect, so this request tries the cold-start "+
		"candidates from placement.az_map",
		log.Int("candidates", len(cs.candidates)-start),
		log.Int("refused_before", start),
	)
	auth := tokenAuthorization(w.req)
	for next < len(cs.candidates) {
		if w.made >= w.bounds.hops {
			return nil, stopExhausted
		}
		if !w.budget.allowsHop() {
			return nil, stopBudget
		}
		if s := w.requestStop(); s != stopNone {
			return nil, s
		}
		if ok, _ := w.mayRunColdStart(cs.limits.probe); !ok {
			return nil, stopShort
		}
		c := cs.candidates[next]
		begin := w.bounds.now()
		cut, err := w.p.probe(w.req.Context(), cs, c, auth)
		w.budget.spend(w.bounds.now().Sub(begin))
		if cut {
			return nil, stopCaller
		}
		next++
		probed++
		if err == nil {
			return c.ep, stopNone
		}
		w.made++
		w.fails.add(c.ep.addr, refusedCandidate(c.ep, err), true)
	}
	return nil, stopExhausted
}

// handsOn reports whether a run of the cold start that stopped for stop passes
// the candidates it did not probe to the next walk. Only a stop that is the
// request's own does: its context ended or left too little for a probe, or its
// send-by deadline passed.
func handsOn(stop stopReason) bool {
	return stop == stopCaller || stop == stopSendBy || stop == stopShort
}

// logColdStartStop logs, at Debug, that a run of the cold start accepted no
// candidate, with why it stopped, how many candidates it probed, and how many
// no walk has probed. over says whether the cold start is over for the
// process, or whether those candidates pass to the next walk.
func (p *Pool) logColdStartStop(stop stopReason, probed, unprobed int, over bool) {
	reason, why := coldStopReason(stop, unprobed)
	msg := "pve endpoints: the cold start from placement.az_map accepted no candidate in this request, because " + why
	switch {
	case unprobed == 0:
		msg += ", so no later request in this call tries one"
	case over:
		msg += ", with " + strconv.Itoa(unprobed) + " candidates never probed, which no later request in this " +
			"call tries"
	default:
		msg += ", with " + strconv.Itoa(unprobed) + " candidates never probed, which the next request that " +
			"reaches the cold start probes"
	}
	p.logger.Debug(msg,
		log.String("reason", reason),
		log.Int("probed", probed),
		log.Int("unprobed", unprobed),
	)
}

// coldStopReason returns the log field and the text for why a run of the cold
// start stopped with unprobed candidates left.
func coldStopReason(stop stopReason, unprobed int) (string, string) {
	switch {
	case stop == stopCaller:
		return "cancelled", "the request's context ended"
	case stop == stopSendBy:
		return "send_by", "the request's send-by deadline passed"
	case stop == stopShort:
		return "short", "the request's context had less than the time one probe may take left"
	case stop == stopBudget:
		return "budget", "the walk's time budget ran out"
	case unprobed > 0:
		return "hop_cap", "the walk reached its hop cap"
	default:
		return "refused", "every candidate was refused"
	}
}

// refusedCandidate returns the failure a walk records for a candidate the cold
// start refused.
func refusedCandidate(ep *endpoint, err error) error {
	return fmt.Errorf("cold-start candidate for node %s from placement.az_map refused: %w", ep.node, err)
}

// errProberPanicked is the refusal when the prober panicked.
var errProberPanicked = errors.New("the cold-start prober panicked")

// probe asks the prober about c, with the probe transport for c and the
// request's API token, under the probe limit and the request's own context,
// and returns nil only when the candidate is accepted. The probe's context
// drops any send-by bound that was meant for the request, because the probe
// is not the request. The candidate is accepted only when the prober returned
// nil and the probe transport, sealed the moment the prober returned, holds a
// complete proof. A panic in the prober, or any proof that is missing or
// incomplete, refuses the candidate. The probe's response body is closed and
// the probe clone's idle connections are closed once the prober returns.
//
// cut reports a probe that reqCtx, the request's own context, ended before
// the candidate answered. It is a probe whose prober returned an error other
// than a panic after reqCtx ended, while the probe's own limit had not passed,
// whose transport received no byte of a response, not even an informational
// 1xx, and whose transport failed, if it failed, only after the probe's own
// context ended. Such a candidate never answered, so the pool counts it as
// never probed rather than refused. A probe that got any response byte, or
// that failed while its context was alive, is never cut, so no candidate that
// answered is probed again.
func (p *Pool) probe(reqCtx context.Context, cs *coldStart, c *candidate, auth string) (cut bool, err error) {
	ctx, cancel := context.WithTimeoutCause(connectContext(reqCtx, http.MethodGet, ProbePath), cs.limits.probe,
		errProbeLimit)
	defer cancel()
	rt := newProbeTransport(ctx, c, auth)
	err = p.callProber(ctx, cs, c, ProbeRequest{
		Candidate:     Candidate{Node: c.ep.node, Address: c.ep.urlHost},
		Authorization: auth,
		Transport:     rt,
	})
	answered, proof := rt.seal()
	c.probe.CloseIdleConnections()
	if err != nil {
		cut = !answered && reqCtx.Err() != nil && !errors.Is(err, errProberPanicked) &&
			!errors.Is(context.Cause(ctx), errProbeLimit)
		return cut, err
	}
	if proof != nil {
		cause := log.ScrubMessage(proof.Error())
		p.logger.Debug("pve endpoint "+c.ep.addr+": the cold-start candidate for node "+c.ep.node+
			" from placement.az_map is refused, because "+cause,
			log.String("endpoint", c.ep.addr),
			log.String("node", c.ep.node),
			log.String("error", cause),
		)
		return false, proof
	}
	return false, nil
}

// errProbeLimit is the cause of a probe's context ending when the probe limit
// passed, which tells that end apart from the end of the request's own
// context.
var errProbeLimit = errors.New("the probe limit passed")

// callProber calls the prober and returns its answer, or errProberPanicked
// when it panicked. Only the prober's call runs under the recovery, so the
// pool's own checks never run in a region whose panic could be mistaken for
// an answer.
func (p *Pool) callProber(ctx context.Context, cs *coldStart, c *candidate, req ProbeRequest) (err error) {
	returned := false
	defer func() {
		if !returned {
			err = errProberPanicked
		}
	}()
	defer p.guardHook(&cs.proberPanicked, "pve endpoint "+c.ep.addr+": the cold-start prober panicked, so "+
		"the candidate is refused", c.ep.addr, "")
	err = cs.prober.Probe(ctx, req)
	returned = true
	return err
}

// The refusals of the probe transport and of a probe that proved nothing.
var (
	errProbeRequest  = errors.New("the cold-start probe transport refuses the request")
	errProbeReused   = errors.New("the cold-start probe transport sends one probe per candidate")
	errProbeSealed   = errors.New("the cold-start probe transport refuses a probe after the prober returned")
	errNotProbed     = errors.New("the prober accepted the candidate without probing it")
	errProbeInFlight = errors.New("the prober accepted the candidate before its probe was answered")
	errProbeBodyOpen = errors.New("the prober accepted the candidate without closing the probe's response body")
)

// probeTransport is the http.RoundTripper a prober sends one candidate's probe
// through. It sends one GET of ProbePath over https at the candidate's address,
// with the request's API token as its only credential, and refuses everything
// else before any dial. It sends the probe under ctx, the probe's own context,
// whatever context the prober put on the request, so the probe limit bounds
// the request and the reading of its body. It keeps the outcome, so the pool
// can check the proof before it trusts the prober's acceptance, and seal ends
// it when the prober returns.
type probeTransport struct {
	c    *candidate
	auth string
	ctx  context.Context

	// firstByte says the first byte of a response, a 1xx block included,
	// reached the transport.
	firstByte atomic.Bool

	mu     sync.Mutex
	used   bool
	sealed bool
	resp   *http.Response
	body   *probeBody
	err    error
	// errEnded says the probe's own context had ended when the transport
	// returned err, so the failure is the request's or the probe limit's and
	// not the candidate's.
	errEnded bool
}

// newProbeTransport returns the probe transport for c, which sends the probe
// under ctx with auth as its Authorization header.
func newProbeTransport(ctx context.Context, c *candidate, auth string) *probeTransport {
	return &probeTransport{c: c, auth: auth, ctx: ctx}
}

// RoundTrip sends the probe through the candidate's probe clone.
func (t *probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.check(req); err != nil {
		if req != nil {
			closeRequestBody(req)
		}
		return nil, err
	}
	t.mu.Lock()
	switch {
	case t.sealed:
		t.mu.Unlock()
		return nil, errProbeSealed
	case t.used:
		t.mu.Unlock()
		return nil, errProbeReused
	}
	t.used = true
	t.mu.Unlock()
	ctx, cancel := t.bind(req.Context())
	resp, err := t.c.probe.RoundTrip(req.WithContext(ctx))
	if err == nil && resp == nil {
		err = errNoResponse
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		cancel()
		t.err, t.errEnded = err, t.ended()
		return nil, err
	}
	if t.sealed {
		cancel()
		_ = resp.Body.Close()
		return nil, errProbeSealed
	}
	body := &probeBody{rc: resp.Body, t: t, cancel: cancel}
	resp.Body = body
	t.resp, t.body = resp, body
	return resp, nil
}

// bind returns the context the probe is sent under: reqCtx's values, the
// probe's deadline, an end when either reqCtx or the probe's own context
// ends, and a trace that records the first byte of a response, so a candidate
// that sent anything, even a 1xx block the transport consumes, counts as
// having answered. The cancel function releases it.
func (t *probeTransport) bind(reqCtx context.Context) (context.Context, context.CancelFunc) {
	ctx := httptrace.WithClientTrace(connectContext(reqCtx, http.MethodGet, ProbePath), &httptrace.ClientTrace{
		GotFirstResponseByte: func() { t.firstByte.Store(true) },
	})
	var cancel context.CancelFunc
	if deadline, ok := t.ctx.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	stop := context.AfterFunc(t.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// check refuses any request other than one GET of ProbePath, without a body,
// over https at the candidate's address, with the API token the pool handed
// the prober as its only Authorization header, and with no ticket cookie and
// no CSRF token. A refusal never names a header's value.
func (t *probeTransport) check(req *http.Request) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("%w: it has no URL", errProbeRequest)
	}
	if m := methodOf(req); m != http.MethodGet {
		return fmt.Errorf("%w: method %s, and the probe is a GET", errProbeRequest, m)
	}
	if hasBody(req) {
		return fmt.Errorf("%w: it has a body, and the probe has none", errProbeRequest)
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("%w: scheme %q, and the probe goes over https", errProbeRequest, req.URL.Scheme)
	}
	if addr, err := urlAddress(req.URL); err != nil || addr != t.c.ep.addr {
		return fmt.Errorf("%w: host %q, and the probe goes only to %s", errProbeRequest, req.URL.Host, t.c.ep.addr)
	}
	if req.URL.Path != ProbePath {
		return fmt.Errorf("%w: path %q, and the probe reads %s", errProbeRequest, req.URL.Path, ProbePath)
	}
	if vals := req.Header.Values("Authorization"); t.auth == "" || len(vals) != 1 || vals[0] != t.auth {
		return fmt.Errorf("%w: its Authorization header is not the one API token the pool handed the prober",
			errProbeRequest)
	}
	if len(req.Header.Values("Cookie")) > 0 || len(req.Header.Values("CSRFPreventionToken")) > 0 {
		return fmt.Errorf("%w: it carries a ticket cookie or a CSRF token", errProbeRequest)
	}
	return nil
}

// seal ends the probe transport when the prober returned. From then on it
// refuses any request, a probe still in flight gets no response, and reads of
// the response body fail. It returns nil only for a complete proof, which is
// a probe that was sent, got a 2xx answer before the prober returned, and
// whose body the prober closed before it returned, and the reason the
// candidate is refused otherwise. It closes the response body the prober left
// open. answered reports whether the candidate answered the probe in a way
// that counts against it: with any byte of a response, or with a failure
// while the probe's own context was alive, such as a refused dial, a failed
// TLS verification, or a dial that ran out its own cap.
func (t *probeTransport) seal() (answered bool, proof error) {
	t.mu.Lock()
	t.sealed = true
	proof = t.proofLocked()
	answered = t.firstByte.Load() || t.resp != nil || (t.err != nil && !t.errEnded)
	body := t.body
	t.mu.Unlock()
	if body != nil {
		_ = body.rc.Close()
		body.cancel()
	}
	return answered, proof
}

// ended reports whether the probe's own context has ended, counting a deadline
// that has passed before its timer fired, so a failure the probe's deadline
// caused is never read as the candidate's. Which error the transport returned
// says nothing here, because a context cancelled with a cause hands the
// transport that cause, and a dial that ran out its own cap returns an error
// that matches context.DeadlineExceeded.
func (t *probeTransport) ended() bool {
	if t.ctx.Err() != nil {
		return true
	}
	deadline, ok := t.ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

// proofLocked returns nil when the probe holds a complete proof, and the
// reason the candidate is refused otherwise. t.mu must be held.
func (t *probeTransport) proofLocked() error {
	switch {
	case !t.used:
		return errNotProbed
	case t.err != nil:
		return fmt.Errorf("the prober accepted a probe that failed: %w", t.err)
	case t.resp == nil || t.body == nil:
		return errProbeInFlight
	case t.resp.StatusCode < http.StatusOK || t.resp.StatusCode >= http.StatusMultipleChoices:
		return fmt.Errorf("the prober accepted a probe answered with HTTP %s", t.resp.Status)
	case !t.body.closed:
		return errProbeBodyOpen
	default:
		return nil
	}
}

// probeBody is the probe's response body as the prober reads it. It records
// whether the prober closed it before the transport was sealed, and refuses
// reads after the seal.
type probeBody struct {
	rc     io.ReadCloser
	t      *probeTransport
	cancel context.CancelFunc
	// closed says the prober closed the body before the seal. It is guarded
	// by t.mu.
	closed bool
}

// Read reads the body until the transport is sealed.
func (b *probeBody) Read(p []byte) (int, error) {
	b.t.mu.Lock()
	sealed := b.t.sealed
	b.t.mu.Unlock()
	if sealed {
		return 0, errProbeSealed
	}
	return b.rc.Read(p)
}

// Close closes the body and releases the probe's request context.
func (b *probeBody) Close() error {
	b.t.mu.Lock()
	if !b.t.sealed {
		b.closed = true
	}
	b.t.mu.Unlock()
	err := b.rc.Close()
	b.cancel()
	return err
}

// errCandidateRevoked is the cause when the pool refuses, before any dial, to
// send a request to an accepted cold-start candidate that later failed TLS
// verification in this process.
var errCandidateRevoked = errors.New("the cold-start candidate failed TLS verification earlier in this call, so " +
	"the pool no longer sends to it")

// refuseRevoked returns the error for a request the pool will not send to
// addr, an accepted cold-start candidate that failed TLS verification since.
// It carries the connect-phase tag with PhaseTLSVerify, because nothing was
// sent and the candidate is out for a certificate failure, and its cause is
// a *tls.CertificateVerificationError, so the SDK stops at once instead of
// retrying a request the pool will refuse again.
func refuseRevoked(addr string) error {
	return &ConnectPhaseError{
		Phase:    PhaseTLSVerify,
		Endpoint: addr,
		Err:      &tls.CertificateVerificationError{Err: errCandidateRevoked},
	}
}

// revokedHop is the result of a hop the walk will not send to ep, an accepted
// cold-start candidate that failed TLS verification after the walk took it.
// The request was handed to no transport, so it is proven unsent and hops as
// a verification failure would, and it has no effect on ep, which the failure
// already took out. It does not count toward the hop cap.
func revokedHop(ep *endpoint) hopResult {
	return hopResult{
		ep:  ep,
		err: refuseRevoked(ep.addr),
		oc:  outcome{action: actHop, sent: sentNever, unreachable: true, class: string(PhaseTLSVerify)},
	}
}

// followCandidate returns nil when req, a mutation whose key's tether names
// ep, the accepted cold-start candidate, may follow that tether to it, and
// the refusal otherwise. The tether keeps a mutation on the endpoint its key
// went to, and every dial to the candidate is verified, so the mutation
// follows it while the candidate has not failed TLS verification since it
// proved membership. It never goes there when the candidate has, when it is a
// login or discovery request, or when it carries no API token or a ticket,
// because a candidate gets only what the cold start would send it. A refused
// request may go nowhere else, so it is refused before any dial.
func (p *Pool) followCandidate(req *http.Request, ep *endpoint, configuredOnly bool) error {
	var why string
	switch {
	case p.ineligibleNow(ep):
		why = "which failed TLS verification after it proved membership, so the pool sends it nothing more"
	case configuredOnly:
		why = "and a login or discovery request never goes to a cold-start candidate"
	case tokenAuthorization(req) == "":
		why = "and only a request with one API token and no ticket goes to a cold-start candidate"
	default:
		return nil
	}
	return fmt.Errorf("endpoint pool: refusing %s %s, whose tether key is tethered to the cold-start candidate %s "+
		"for node %s, %s", methodOf(req), apiRelative(req.URL.Path), ep.addr, ep.node, why)
}

// candidateFailed applies what the pool records beyond a demotion, which the
// walk has already applied, when a request to ep, the accepted cold-start
// candidate, failed in the connect phase for a reason the candidate caused. A
// candidate is no endpoint the cache knows, so it gets no down mark. A
// verification failure means the certificate no longer chains to the
// operator's CA or no longer covers the candidate's name, which can mean
// interception, so the candidate is taken out of every later walk and routed
// request, its idle connections are closed, and the failure is logged once at
// Error, because the pool sent the operator's token to that host before. A
// candidate has no pins, so it never fails one. Every other failure only
// demotes it.
func (p *Pool) candidateFailed(ep *endpoint, phase Phase, err error) {
	if phase != PhaseTLSVerify {
		return
	}
	p.mu.Lock()
	if ep.ineligible {
		p.mu.Unlock()
		return
	}
	ep.ineligible = true
	p.mu.Unlock()
	ep.transports.closeIdle()
	cause := log.ScrubMessage(noteOf(nil, err, string(phase)).text)
	p.logger.Error("pve endpoint "+ep.addr+": the cold-start candidate for node "+ep.node+" from placement.az_map "+
		"failed TLS verification ("+cause+"); this can mean interception; the pool no longer sends to it",
		log.String("endpoint", ep.addr),
		log.String("node", ep.node),
		log.String("class", string(phase)),
		log.String("error", cause),
	)
}
