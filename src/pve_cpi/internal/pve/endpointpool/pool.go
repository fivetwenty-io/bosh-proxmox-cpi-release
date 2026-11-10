package endpointpool

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// Kind says where an endpoint came from.
type Kind int

// The kinds of endpoint a pool holds.
const (
	// KindConfigured is an endpoint the operator listed. Its clones follow the
	// operator's TLS settings exactly, as pve.host does.
	KindConfigured Kind = iota + 1
	// KindDiscovered is an endpoint that discovery learned through a
	// configured endpoint. Its clones accept only a certificate that its
	// node's pin set, or under verify_ssl true its name, vouches for.
	KindDiscovered
)

// String returns "configured" or "discovered".
func (k Kind) String() string {
	switch k {
	case KindConfigured:
		return "configured"
	case KindDiscovered:
		return "discovered"
	default:
		return "kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Endpoint describes one endpoint handed to New.
type Endpoint struct {
	// Address is the endpoint's host:port. An IPv6 literal is written in
	// brackets.
	Address string
	// Kind says whether the operator configured the endpoint or discovery
	// found it.
	Kind Kind
	// Node is the name of the Proxmox node the endpoint serves, when it is
	// known, and empty otherwise. Discovery records it for every discovered
	// endpoint, and for a configured endpoint that /cluster/status reported as
	// local.
	Node string
	// Pins is a discovered endpoint's pin set, which must not be empty. A
	// configured endpoint carries no pins.
	Pins []Pin
	// Online says a quorate /cluster/status reported a discovered endpoint's
	// node online, which puts it ahead of the discovered endpoints without
	// that report. A configured endpoint keeps its configured place, so it
	// must leave Online false.
	Online bool
	// DownMarkedAt is when a cache down mark for the endpoint was written, or
	// zero when it carries none. A mark younger than two minutes moves the
	// endpoint to the end of the walk, and an older one has no effect. A mark
	// dated more than 30 seconds ahead of the pool's clock when the pool is
	// built is dropped, and it never takes effect.
	DownMarkedAt time.Time
}

// Config is what New needs to build a pool.
type Config struct {
	// Base is the transport the SDK built, with the operator's TLS settings,
	// dialer, and timeouts. The pool never sends on it. It clones it for every
	// endpoint and for every routed host.
	Base *http.Transport
	// Logger receives the pool's log lines. A nil Logger discards them.
	Logger *log.Logger
	// Endpoints lists the configured and discovered endpoints. The configured
	// endpoints keep their relative order and come first. The discovered ones
	// follow, those whose node a quorate /cluster/status reported online
	// first, each group in node-name order, and endpoints with the same node
	// name keep the order they were listed in. The first configured endpoint
	// is the primary, whose host:port the SDK puts on every request.
	Endpoints []Endpoint
	// MarkSink receives a down mark for an endpoint's first connect-phase
	// failure in this process, and for a discovered endpoint's pin failure.
	// A nil MarkSink records no marks, which is the pool of a process without
	// an endpoint cache. The pool calls it on the failed request's goroutine,
	// so it hands the write to a goroutine of its own and returns promptly.
	MarkSink MarkSink
	// FirstAnswer is told once, after the first request that a configured
	// endpoint answers. A nil FirstAnswer is told nothing. The pool calls it
	// on the answered request's goroutine, so it hands its work to a
	// goroutine of its own and returns promptly.
	FirstAnswer FirstAnswerHook

	// headerDeadline replaces the 60-second response-header deadline. Only
	// the package's tests set it. Zero means the default.
	headerDeadline time.Duration
	// clock replaces time.Now for the age of down marks and the time a new
	// mark records. Only the package's tests set it.
	clock func() time.Time
}

// endpoint is one endpoint of a pool. Everything but demoted, ineligible,
// dropped, marked, and lastMarkAt is fixed at construction.
type endpoint struct {
	// addr is the canonical host:port, the key every comparison uses.
	addr string
	// urlHost is the host:port as the caller wrote it. The walk puts it in a
	// request's URL.Host, so the Host header and SNI keep the spelling the SDK
	// sends today, and the clones send its host as SNI.
	urlHost    string
	kind       Kind
	node       string
	pins       int
	rank       int
	transports *ownedTransports
	// markedAt is when the cache down mark the endpoint was loaded with was
	// written, or zero.
	markedAt time.Time
	// demoted is guarded by Pool.mu.
	demoted bool
	// ineligible says a discovered endpoint failed its pin in this process,
	// or that DropDiscovered took it out, so no request is sent to it again.
	// It is guarded by Pool.mu.
	ineligible bool
	// dropped says DropDiscovered took the endpoint out, as opposed to a pin
	// failure, so a refused request says so. It is guarded by Pool.mu.
	dropped bool
	// marked says the endpoint has had its mark for this process, which is
	// its first connect-phase failure or its pin failure, and lastMarkAt is
	// the time that last mark records. Both are guarded by Pool.mu.
	marked     bool
	lastMarkAt time.Time
}

// Pool holds the ordered endpoints of one cluster, the active endpoint with
// its generation counter, the in-process demotions, and every transport it
// owns. It is safe for concurrent use.
//
// The state a pool keeps once, which is the ineligible endpoints, each
// endpoint's mark, the first answer, and the pin-mismatch and hook-panic
// lines, lasts as long as the pool. It assumes one pool per CPI call, and
// every CPI call is a new process, so "once per process" and "once per call"
// say the same. A process that kept a pool across calls would carry that
// state from one call into the next.
type Pool struct {
	logger    *log.Logger
	base      *http.Transport
	verifySSL bool

	// endpoints is in preference order, the configured endpoints first.
	endpoints []*endpoint
	byAddr    map[string]*endpoint

	// refreshDue is set when a discovered endpoint was accepted on its name
	// with a certificate outside its pin set, or failed its pin, so the next
	// discovery through a configured endpoint should record the new pin.
	refreshDue atomic.Bool

	// now is the clock for down marks.
	now func() time.Time
	// sink and firstAnswer are the cache's hooks, and either may be nil.
	sink        MarkSink
	firstAnswer FirstAnswerHook
	// answered says a configured endpoint has answered a request, so
	// firstAnswer has been told.
	answered atomic.Bool
	// sinkPanicked and hookPanicked say the sink or the hook has panicked
	// once already, so a later panic is logged at Debug.
	sinkPanicked atomic.Bool
	hookPanicked atomic.Bool
	// startAway is set when a down mark made the pool start on an endpoint
	// other than the first in preference order.
	startAway *StartAway

	mu sync.Mutex
	// active is where the next walk starts.
	active *endpoint
	// generation goes up each time active changes.
	generation uint64
	// routed holds the clones built on first use for a routed host that is
	// not one of the pool's endpoints.
	routed map[string]*ownedTransports

	// walk is what the pool's walks remember for their log lines.
	walk walkState
	// tethers holds every mutation tether this pool made, under its own lock.
	tethers tetherTable
}

// New builds a pool. It refuses a base transport that checkBase refuses, an
// empty endpoint list, a list without a configured endpoint, an address that
// is not a valid host:port, an address listed twice, an unknown kind, a
// configured endpoint with pins or with Online set, and a discovered endpoint
// without pins.
//
// The pool starts on the first endpoint of the walk order, so a process whose
// first configured endpoint carries a down mark younger than two minutes
// starts on the next endpoint without one, and StartedAway reports it. A down
// mark dated more than 30 seconds ahead of the pool's clock is dropped here
// and never takes effect.
//
// A pool of more than one endpoint gives each endpoint's idempotent clone the
// 60-second response-header deadline. A pool of one endpoint keeps today's
// behavior, so its one clone has no header deadline.
func New(cfg Config) (*Pool, error) {
	if err := checkBase(cfg.Base); err != nil {
		return nil, err
	}
	base := cfg.Base.Clone()
	logger := cfg.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}
	eps, err := orderEndpoints(cfg.Endpoints)
	if err != nil {
		return nil, err
	}

	now := cfg.clock
	if now == nil {
		now = time.Now
	}
	builtAt := now()
	p := &Pool{
		logger:      logger,
		base:        base,
		verifySSL:   base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify,
		endpoints:   make([]*endpoint, 0, len(eps)),
		byAddr:      make(map[string]*endpoint, len(eps)),
		routed:      make(map[string]*ownedTransports),
		now:         now,
		sink:        cfg.MarkSink,
		firstAnswer: cfg.FirstAnswer,
	}
	headerDeadline := time.Duration(0)
	if len(eps) > 1 {
		headerDeadline = defaultHeaderDeadline
		if cfg.headerDeadline > 0 {
			headerDeadline = cfg.headerDeadline
		}
	}
	for i, in := range eps {
		ep := &endpoint{
			addr: in.Address, urlHost: in.urlHost, kind: in.Kind, node: in.Node, pins: len(in.Pins), rank: i,
			markedAt: acceptedMark(in.DownMarkedAt, builtAt),
		}
		spec := transportSpec{addr: in.Address, serverName: hostOf(in.urlHost), headerDeadline: headerDeadline}
		if in.Kind == KindDiscovered {
			v, verr := p.discoveredVerifier(in.Endpoint)
			if verr != nil {
				return nil, verr
			}
			spec.verify = v.verifyConnection
		}
		if ep.transports, err = newOwnedTransports(base, spec); err != nil {
			return nil, err
		}
		p.endpoints = append(p.endpoints, ep)
		p.byAddr[ep.addr] = ep
	}
	p.startOnPreferred(builtAt)
	p.logger.Debug(p.describe())
	return p, nil
}

// orderedEndpoint is an endpoint that passed validation. Its Address is in
// canonical form, and urlHost keeps the address as the caller wrote it.
type orderedEndpoint struct {
	Endpoint
	urlHost string
}

// orderEndpoints validates the endpoints, canonicalizes their addresses, and
// returns the configured ones in their order followed by the discovered ones
// in the order sortDiscovered gives.
func orderEndpoints(in []Endpoint) ([]orderedEndpoint, error) {
	if len(in) == 0 {
		return nil, errors.New("endpoint pool: no endpoints")
	}
	configured := make([]orderedEndpoint, 0, len(in))
	var discovered []orderedEndpoint
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		addr, err := canonicalAddress(raw.Address)
		if err != nil {
			return nil, fmt.Errorf("endpoint pool: endpoint %q: %w", raw.Address, err)
		}
		if _, dup := seen[addr]; dup {
			return nil, fmt.Errorf("endpoint pool: endpoint %s is listed twice", addr)
		}
		seen[addr] = struct{}{}
		ep := orderedEndpoint{Endpoint: raw, urlHost: raw.Address}
		ep.Address = addr
		ep.Node = strings.TrimSpace(ep.Node)
		switch ep.Kind {
		case KindConfigured:
			if len(ep.Pins) > 0 {
				return nil, fmt.Errorf("endpoint pool: configured endpoint %s carries pins, and a configured "+
					"endpoint follows the operator's TLS settings", addr)
			}
			if ep.Online {
				return nil, fmt.Errorf("endpoint pool: configured endpoint %s is marked online, and a configured "+
					"endpoint keeps its configured place", addr)
			}
			configured = append(configured, ep)
		case KindDiscovered:
			if len(ep.Pins) == 0 {
				return nil, fmt.Errorf("endpoint pool: discovered endpoint %s has no pins", addr)
			}
			ep.Pins = append([]Pin(nil), ep.Pins...)
			discovered = append(discovered, ep)
		default:
			return nil, fmt.Errorf("endpoint pool: endpoint %s has unknown %s", addr, ep.Kind)
		}
	}
	if len(configured) == 0 {
		return nil, errors.New("endpoint pool: no configured endpoint")
	}
	sortDiscovered(discovered)
	return append(configured, discovered...), nil
}

// discoveredVerifier builds the pinned callback for a discovered endpoint from
// the operator's TLS settings on the base transport.
func (p *Pool) discoveredVerifier(ep Endpoint) (*verifier, error) {
	host := hostOf(ep.Address)
	cfg := p.base.TLSClientConfig
	if cfg == nil {
		return newDiscoveredVerifier(host, p.verifySSL, nil, nil, ep.Pins, p.markRefreshDue)
	}
	return newDiscoveredVerifier(host, p.verifySSL, cfg.RootCAs, cfg.Time, ep.Pins, p.markRefreshDue)
}

// describe returns the pool-built line, which lists every endpoint in
// preference order with its kind and names the endpoint the pool starts on.
func (p *Pool) describe() string {
	parts := make([]string, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		parts = append(parts, ep.addr+" "+ep.kind.String())
	}
	return fmt.Sprintf("pve endpoints: pool [%s], starting on %s", strings.Join(parts, ", "), p.active.addr)
}

// Len returns the number of configured and discovered endpoints.
func (p *Pool) Len() int { return len(p.endpoints) }

// HeaderDeadline returns the response-header deadline the pool gives an
// idempotent request, which is 60 seconds for a pool of more than one endpoint
// and zero for a pool of one.
func (p *Pool) HeaderDeadline() time.Duration {
	return p.endpoints[0].transports.idempotent.ResponseHeaderTimeout
}

// RefreshDue reports whether a discovered endpoint was accepted on its name
// with a certificate outside its pin set in this process, or failed its pin,
// so the next discovery through a configured endpoint should run and record
// the new pin. The pool itself starts no discovery for it. The first-answer
// hook is told once, so a refresh that becomes due after the first answer is
// left to whoever reads RefreshDue, and otherwise to the next process, which
// the pin mark makes refresh.
func (p *Pool) RefreshDue() bool { return p.refreshDue.Load() }

func (p *Pool) markRefreshDue() { p.refreshDue.Store(true) }

// CloseIdleConnections closes the idle connections of every transport the pool
// owns, the routed ones included.
func (p *Pool) CloseIdleConnections() {
	for _, ep := range p.endpoints {
		ep.transports.closeIdle()
	}
	p.mu.Lock()
	routed := make([]*ownedTransports, 0, len(p.routed))
	for _, o := range p.routed {
		routed = append(routed, o)
	}
	p.mu.Unlock()
	for _, o := range routed {
		o.closeIdle()
	}
}

// lookup returns the endpoint with the given address, which may be written in
// any spelling canonicalAddress accepts, or nil.
func (p *Pool) lookup(addr string) *endpoint {
	canon, err := canonicalAddress(addr)
	if err != nil {
		return nil
	}
	return p.byAddr[canon]
}

// current returns the active endpoint and the generation it became active in.
func (p *Pool) current() (*endpoint, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active, p.generation
}

// walkOrder returns the endpoints a walk tries, in order, and the current
// generation. The endpoints that are neither demoted nor carrying a down mark
// younger than two minutes come first in preference order, and the rest
// follow in theirs, so a demotion or a mark only reorders and never removes an
// endpoint. The active endpoint leads unless it is demoted. A discovered
// endpoint that failed its pin in this process is left out. With
// configuredOnly, only the configured endpoints are returned, which is the
// order for a ticket login and for a request with the configured-only marker.
func (p *Pool) walkOrder(configuredOnly bool) ([]*endpoint, uint64) {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	order := make([]*endpoint, 0, len(p.endpoints))
	var lead *endpoint
	if !p.active.demoted && eligibleLocked(p.active, configuredOnly) {
		lead = p.active
		order = append(order, lead)
	}
	for _, back := range []bool{false, true} {
		for _, ep := range p.endpoints {
			if ep == lead || !eligibleLocked(ep, configuredOnly) || atBackLocked(ep, now) != back {
				continue
			}
			order = append(order, ep)
		}
	}
	return order, p.generation
}

// demote moves ep behind every endpoint that is not demoted, for the rest of
// the process or until a request on it succeeds under the current generation.
func (p *Pool) demote(ep *endpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ep.demoted = true
}

// demotedNow reports whether ep is demoted in this process.
func (p *Pool) demotedNow(ep *endpoint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ep.demoted
}

// promote records a success on ep for an attempt that started under
// generation gen. When gen is still current, ep becomes active, its demotion
// is cleared, and the generation goes up if the active endpoint changed. A
// stale success changes nothing, so a slow request that started on an
// endpoint before a failover cannot make that endpoint active again, and
// neither does a success on a discovered endpoint that failed its pin while
// the request was in flight. It reports whether the success was applied and,
// when it moved the active endpoint, the endpoint that was active before,
// which is nil otherwise.
func (p *Pool) promote(ep *endpoint, gen uint64) (*endpoint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.generation || ep.ineligible {
		return nil, false
	}
	ep.demoted = false
	if p.active == ep {
		return nil, true
	}
	prev := p.active
	p.active = ep
	p.generation++
	return prev, true
}

// advance demotes from and, when from is still the active endpoint under
// generation gen, makes the next endpoint in walk order active and raises the
// generation. A streamed upload uses it after a connect-phase failure, because
// it cannot hop inside one request and its next attempt must start on another
// endpoint. It returns the endpoint it made active, or nil when the active
// endpoint did not change. A pool of one endpoint, or a stale generation,
// leaves the active endpoint where it is.
func (p *Pool) advance(from *endpoint, gen uint64) *endpoint {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	from.demoted = true
	if p.active != from || gen != p.generation {
		return nil
	}
	next := preferredLocked(p.endpoints, now, from)
	if next == nil {
		return nil
	}
	p.active = next
	p.generation++
	return next
}

// routedTransports returns the owned clones for a request whose URL host a
// caller chose with WithHost, such as a routed upload. A host that is one of
// the pool's endpoints uses that endpoint's clones, so a discovered address is
// never reached without its pin. Any other host gets one clone of the base
// transport, built on first use and owned for the rest of the process, with
// the connect-phase tag and the operator's TLS settings and no pin. That clone
// has no header deadline, because a routed request never hops and the
// deadline exists only to make a hop.
func (p *Pool) routedTransports(u *url.URL) (*ownedTransports, error) {
	if u == nil {
		return nil, errors.New("endpoint pool: routed request has no URL")
	}
	addr, err := urlAddress(u)
	if err != nil {
		return nil, err
	}
	return p.routedTransportsAt(addr, u.Hostname())
}

// routedTransportsAt returns the owned clones for the canonical address addr,
// as routedTransports does, building a clone outside the pool with serverName
// as its SNI on first use.
func (p *Pool) routedTransportsAt(addr, serverName string) (*ownedTransports, error) {
	if ep := p.byAddr[addr]; ep != nil {
		return ep.transports, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if o := p.routed[addr]; o != nil {
		return o, nil
	}
	o, err := newOwnedTransports(p.base, transportSpec{addr: addr, serverName: serverName})
	if err != nil {
		return nil, err
	}
	p.routed[addr] = o
	return o, nil
}

// hostOf returns the host of a host:port that has already passed
// canonicalAddress, so the split cannot fail.
func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
