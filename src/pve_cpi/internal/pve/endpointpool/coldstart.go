package endpointpool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// kindCandidate is the kind of a cold-start candidate's endpoint. It is never
// passed in Config.Endpoints. New builds such endpoints from Config.Candidates,
// and they are kept apart from the configured and discovered endpoints, so no
// walk order, routed request, or start of a pool ever names one until a probe
// accepts it.
const kindCandidate Kind = KindDiscovered + 1

// The limits of the cold start. A pool carries at most maxCandidates. The probe
// waits at most probeHeaderDeadline for the answer's headers, on every
// candidate hop including a walk's last, because the probe is not the request
// the walk carries. probeLimit bounds the whole probe, which is the dial, the
// handshake, and the header deadline, so a candidate costs at most 25 seconds
// before its request even when it sends the probe's headers and then stalls
// its body.
const (
	maxCandidates       = 64
	probeHeaderDeadline = 10 * time.Second
	probeLimit          = candidateDialCap + candidateHandshakeCap + probeHeaderDeadline
)

// ProbePath is the URL path of the membership probe, GET /cluster/status under
// the API prefix. The probe transport refuses every other path.
const ProbePath = APIPrefix + "/cluster/status"

// apiTokenScheme starts the Authorization header of a request that
// authenticates with a PVE API token. The cold start begins only for such a
// request, so a ticket cookie, a CSRF token, or a password never goes to an
// address the operator did not list.
const apiTokenScheme = "PVEAPIToken=" // #nosec G101 -- the header's scheme name, which carries no secret

// The discovery outcomes a cache file records, as Gate reads them. The empty
// outcome is a file without a discovery result, which is a missing file or one
// that holds only a cold_start hint.
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
// records, or empty when the file is missing or holds no discovery result.
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
	// cold start, which the probe sends as its own Authorization header. It
	// always starts with "PVEAPIToken=".
	Authorization string
	// Transport sends the probe. It accepts one GET of ProbePath over https at
	// the candidate's Address and refuses every other request, and any second
	// request, before any dial. The connection it makes is accepted only when
	// the candidate's certificate chains to the operator's CA and covers the
	// candidate's host, with no pin, so the token leaves only after both
	// checks pass. It waits at most 10 seconds for the answer's headers.
	Transport http.RoundTripper
}

// Prober decides whether a cold-start candidate is a member of the entry's
// cluster.
//
// Probe sends GET ProbePath through req.Transport and returns nil only when
// the answer proves membership, which is a 2xx whose local node is
// req.Candidate.Node and whose node list holds every valid name in the entry's
// az_map. It reads and closes the response body before it returns. Any error
// refuses the candidate for the rest of the process. ctx ends when the request
// that began the cold start ends, and after 25 seconds at most.
//
// The pool accepts a candidate only when Probe returns nil and the probe it
// sent through req.Transport got a 2xx, so a Prober that returns nil without
// probing, or after an error or another status, accepts nothing. The pool
// recovers a panic in Probe and refuses the candidate. A Prober logs its own
// refusals, and the pool calls it on the walk's goroutine, outside every lock
// the pool holds.
type Prober interface {
	Probe(ctx context.Context, req ProbeRequest) error
}

// candidate is one cold-start candidate of a pool. ep is its endpoint, whose
// clones carry no response-header deadline, so a request on an accepted
// candidate never has one. probe is the clone the membership probe goes
// through, with the probe's own header deadline. Both check the certificate
// with the candidate's callback and dial under the candidate's connect caps.
type candidate struct {
	ep    *endpoint
	probe *http.Transport
}

// coldStart is the cold start of one pool. It runs in at most one walk of the
// process, and every other walk that reaches it shares that run.
type coldStart struct {
	prober     Prober
	candidates []*candidate
	// onWait, when set, is called by a walk just before it waits for the
	// walk that runs the cold start. Only the package's tests set it.
	onWait func()
	// proberPanicked says the prober has panicked once already, so a later
	// panic is logged at Debug.
	proberPanicked atomic.Bool

	mu sync.Mutex
	// begun says a walk has claimed the cold start. It never goes back.
	begun bool
	// done is closed when the walk that claimed the cold start ends it, after
	// the accepted candidate, if any, is recorded on the pool.
	done chan struct{}
}

// claim reports whether the caller is the walk that runs the cold start, and
// returns the channel that is closed when the run ends.
func (cs *coldStart) claim() (bool, <-chan struct{}) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.begun {
		return false, cs.done
	}
	cs.begun = true
	return true, cs.done
}

// buildColdStart validates cfg's candidates and builds their clones. A pool
// with no candidates gets no cold start. Candidates need a Prober, verify_ssl
// true with the operator's CA, which is pve.ca_cert, as the trust anchor, and a
// pool without discovered endpoints, because the cold start exists only for a
// cache that holds no usable discovered entry. Each candidate needs a node
// name with no surrounding space and a valid host:port that is neither a
// configured endpoint nor another candidate, and no two share a node.
func (p *Pool) buildColdStart(cfg Config) error {
	if len(cfg.Candidates) == 0 {
		return nil
	}
	if cfg.Prober == nil {
		return errors.New("endpoint pool: cold-start candidates need a prober")
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
	cs := &coldStart{prober: cfg.Prober, onWait: cfg.onColdStartWait, done: make(chan struct{})}
	addrs := make(map[string]struct{}, len(cfg.Candidates))
	nodes := make(map[string]struct{}, len(cfg.Candidates))
	for _, in := range cfg.Candidates {
		c, err := p.newCandidate(in, addrs, nodes)
		if err != nil {
			return err
		}
		cs.candidates = append(cs.candidates, c)
	}
	p.cold = cs
	return nil
}

// newCandidate validates one candidate against the pool and the candidates
// before it, recorded in addrs and nodes, and builds its clones.
func (p *Pool) newCandidate(in Candidate, addrs, nodes map[string]struct{}) (*candidate, error) {
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
		addr:         addr,
		serverName:   hostOf(in.Address),
		verify:       v.verifyConnection,
		dialCap:      candidateDialCap,
		handshakeCap: candidateHandshakeCap,
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep := p.coldAccepted; ep != nil && ep.addr == addr {
		return ep
	}
	return nil
}

// acceptedFor returns the accepted candidate for a walk whose order is order,
// or nil when no candidate was accepted or the walk already has it.
func (p *Pool) acceptedFor(order []*endpoint) *endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	ep := p.coldAccepted
	if ep == nil || ep.ineligible || slices.Contains(order, ep) {
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
// goes on to the cold start. That needs a pool that carries candidates and a
// walk that has not reached the cold start yet, that walks the whole pool, and
// that follows no tether, so the login, a discovery request, and a tethered
// mutation never do. The run loop already halts a tethered walk before it
// asks, and the check here keeps the rule if that order changes. Every
// configured endpoint must have failed in the connect phase in this walk, by
// opensColdStart, and the hop cap and both budgets must still allow a hop,
// because a candidate's hop counts against them. The request must carry an
// API token and no ticket, and a body it can rebuild.
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
// nil with the reason the walk stops. The first walk to get here runs the cold
// start. A walk that gets here while it runs waits for it within its own walk
// budget, and a walk that gets here after it ended reads its result. Either
// way a walk sends only to the candidate that run accepted, so no candidate is
// probed twice and no second cold start begins.
func (w *walker) coldStart() (*endpoint, stopReason) {
	w.coldDone = true
	cs := w.p.cold
	owner, done := cs.claim()
	if owner {
		return w.runColdStart(cs)
	}
	if stop := w.awaitColdStart(cs, done); stop != stopNone {
		return nil, stop
	}
	ep := w.p.acceptedFor(w.order)
	switch {
	case ep == nil:
		return nil, stopExhausted
	case !w.budget.allowsHop():
		return nil, stopBudget
	default:
		return ep, stopNone
	}
}

// awaitColdStart waits for the walk that runs the cold start to end it. It
// stops waiting when the request's context ends or when the walk budget runs
// out, and reports which happened.
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
	select {
	case <-done:
		return stopNone
	case <-w.req.Context().Done():
		return stopCaller
	case <-timer.C:
		return stopBudget
	}
}

// runColdStart probes the candidates in order, as the one walk of the process
// that runs the cold start, and returns the first one accepted, or nil with the
// reason it stopped. Before each candidate it checks the hop cap, both
// budgets, the caller's context, and the send-by deadline. A probe's whole
// time counts as connect time. A refused candidate uses a hop and joins the
// walk's failures, and an accepted one uses the hop its request goes out on.
// The run is recorded as ended however it returns, so a walk waiting for it
// never waits past its own budget.
func (w *walker) runColdStart(cs *coldStart) (accepted *endpoint, stop stopReason) {
	defer func() { w.p.endColdStart(cs, accepted) }()
	w.p.logger.Debug("pve endpoints: no configured endpoint could connect, so this request tries the cold-start "+
		"candidates from placement.az_map", log.Int("candidates", len(cs.candidates)))
	auth := tokenAuthorization(w.req)
	tried := 0
	for _, c := range cs.candidates {
		if w.made >= w.bounds.hops {
			stop = stopExhausted
			break
		}
		if !w.budget.allowsHop() {
			stop = stopBudget
			break
		}
		if stop = w.requestStop(); stop != stopNone {
			break
		}
		tried++
		start := w.bounds.now()
		err := w.p.probe(w.req.Context(), cs, c, auth)
		w.budget.spend(w.bounds.now().Sub(start))
		if err == nil {
			return c.ep, stopNone
		}
		w.made++
		w.fails.add(c.ep.addr, refusedCandidate(c.ep, err), true)
	}
	if stop == stopNone {
		stop = stopExhausted
	}
	w.p.logger.Debug("pve endpoints: no cold-start candidate from placement.az_map was accepted, so no later "+
		"request in this call tries one", log.Int("tried", tried), log.Int("candidates", len(cs.candidates)))
	return nil, stop
}

// endColdStart records accepted, when there is one, as the pool's accepted
// candidate, which every later walk may try, and then ends the run, which
// wakes every walk waiting for it.
func (p *Pool) endColdStart(cs *coldStart, accepted *endpoint) {
	if accepted != nil {
		p.mu.Lock()
		p.coldAccepted = accepted
		p.mu.Unlock()
	}
	close(cs.done)
}

// refusedCandidate returns the failure a walk records for a candidate the cold
// start refused.
func refusedCandidate(ep *endpoint, err error) error {
	return fmt.Errorf("cold-start candidate for node %s from placement.az_map refused: %w", ep.node, err)
}

// errProberPanicked is the refusal when the prober panicked.
var errProberPanicked = errors.New("the cold-start prober panicked")

// probe asks the prober about c, with the probe transport for c and the
// request's API token, under probeLimit and the request's own context, and
// returns nil only when the candidate is accepted. The probe's context drops
// any send-by bound that was meant for the request, because the probe is not
// the request. A panic in the prober is recovered and refuses the candidate.
// The probe's response body is closed when the prober returns.
func (p *Pool) probe(ctx context.Context, cs *coldStart, c *candidate, auth string) (err error) {
	ctx, cancel := context.WithTimeout(connectContext(ctx, http.MethodGet, ProbePath), probeLimit)
	defer cancel()
	rt := &probeTransport{c: c}
	defer rt.closeResponse()
	panicked := true
	defer func() {
		if panicked {
			err = errProberPanicked
		}
	}()
	defer p.guardHook(&cs.proberPanicked, fmt.Sprintf("pve endpoint %s: the cold-start prober panicked, so "+
		"the candidate is refused", c.ep.addr), c.ep.addr, "")
	err = cs.prober.Probe(ctx, ProbeRequest{
		Candidate:     Candidate{Node: c.ep.node, Address: c.ep.urlHost},
		Authorization: auth,
		Transport:     rt,
	})
	panicked = false
	if err != nil {
		return err
	}
	return rt.proof()
}

// The refusals of the probe transport and of a probe that proved nothing.
var (
	errProbeRequest = errors.New("the cold-start probe transport refuses the request")
	errProbeReused  = errors.New("the cold-start probe transport sends one probe per candidate")
	errNotProbed    = errors.New("the prober accepted the candidate without probing it")
)

// probeTransport is the http.RoundTripper a prober sends one candidate's probe
// through. It sends one GET of ProbePath over https at the candidate's address
// and refuses everything else before any dial. It keeps the outcome, so the
// pool can check that the probe got a 2xx before it trusts the prober's
// acceptance.
type probeTransport struct {
	c *candidate

	mu   sync.Mutex
	used bool
	resp *http.Response
	err  error
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
	if t.used {
		t.mu.Unlock()
		return nil, errProbeReused
	}
	t.used = true
	t.mu.Unlock()
	resp, err := t.c.probe.RoundTrip(req)
	if err == nil && resp == nil {
		err = errNoResponse
	}
	t.mu.Lock()
	t.resp, t.err = resp, err
	t.mu.Unlock()
	return resp, err
}

// check refuses any request other than one GET of ProbePath, without a body,
// over https at the candidate's address.
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
	return nil
}

// proof returns nil when the probe was sent and got a 2xx, and the reason the
// candidate is refused otherwise.
func (t *probeTransport) proof() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case !t.used:
		return errNotProbed
	case t.err != nil:
		return fmt.Errorf("the prober accepted a probe that failed: %w", t.err)
	case t.resp.StatusCode < http.StatusOK || t.resp.StatusCode >= http.StatusMultipleChoices:
		return fmt.Errorf("the prober accepted a probe answered with HTTP %s", t.resp.Status)
	default:
		return nil
	}
}

// closeResponse closes the probe's response body, which the prober should
// already have closed, so a prober that left it open never holds a
// connection.
func (t *probeTransport) closeResponse() {
	t.mu.Lock()
	resp := t.resp
	t.mu.Unlock()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// errCandidateRevoked is the cause when the pool refuses, before any dial, to
// send a request to an accepted cold-start candidate that later failed TLS
// verification in this process.
var errCandidateRevoked = errors.New("the cold-start candidate failed TLS verification earlier in this call, so " +
	"the pool no longer sends to it")

// revokedHop is the result of a hop the walk will not send to ep, an accepted
// cold-start candidate that failed TLS verification after the walk read its
// order. The request was handed to no transport, so it is proven unsent and
// hops as a verification failure would, and it has no effect on ep, which the
// failure already took out. It does not count toward the hop cap.
func revokedHop(ep *endpoint) hopResult {
	err := &ConnectPhaseError{
		Phase:    PhaseTLSVerify,
		Endpoint: ep.addr,
		Err:      &tls.CertificateVerificationError{Err: errCandidateRevoked},
	}
	return hopResult{
		ep:  ep,
		err: err,
		oc:  outcome{action: actHop, sent: sentNever, unreachable: true, class: string(PhaseTLSVerify)},
	}
}

// candidateFailed applies what the pool records beyond a demotion when a
// request to ep, the accepted cold-start candidate, failed in the connect
// phase for a reason the candidate caused. A candidate is no endpoint the
// cache knows, so it gets no down mark. A verification failure means the
// certificate no longer chains to the operator's CA or no longer covers the
// candidate's name, which can mean interception, so the candidate is taken out
// of every later walk, its idle connections are closed, and the failure is
// logged once at Error. Every other failure only demotes it.
func (p *Pool) candidateFailed(ep *endpoint, phase Phase, err error) {
	if phase != PhaseTLSVerify && phase != PhasePin {
		return
	}
	p.mu.Lock()
	if ep.ineligible {
		p.mu.Unlock()
		return
	}
	ep.ineligible, ep.demoted = true, true
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
