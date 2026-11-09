package endpointpool

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// tetherTarget is the address a tether names. It is one of the pool's
// endpoints, or a routed host outside the pool that a caller chose with
// WithHost, such as a direct-to-node upload.
type tetherTarget struct {
	// addr is the canonical host:port, which every comparison uses.
	addr string
	// urlHost is the host:port as the request that reserved the tether wrote
	// it. A request that follows the tether to a routed host outside the pool
	// carries it in its URL, so the Host header and SNI keep that spelling.
	urlHost string
}

// targetOf returns the target that names ep.
func targetOf(ep *endpoint) tetherTarget {
	return tetherTarget{addr: ep.addr, urlHost: ep.urlHost}
}

// tether ties the mutations with one key, made under one affinity scope, to
// one target. It starts as a reservation, which the request that made it and
// every request that followed it hold while they are in flight, and it becomes
// fixed when any of them ends with an outcome other than sentNever. Every
// field is guarded by tetherTable.mu.
type tether struct {
	key    tetherKey
	scope  *Scope
	target tetherTarget
	// holders counts the requests in flight that hold the tether.
	holders int
	// fixed says a request holding the tether may have been sent, so the
	// tether stays on its target until the call that made it returns, or for
	// the process when it outlived that call.
	fixed bool
	// logged says the Debug line for the fixed tether has been written.
	logged bool
	// conflicted says a request holding the tether ended maybe sent on
	// another address, which only a defect can cause. Every later mutation
	// that sees the tether is refused with ErrTetherConflict.
	conflicted bool
	// outlived says the call that made the tether returned while a request
	// still held it, or before the request that made it claimed. No scope's
	// release drops it. It is removed when its last holder settles sentNever
	// and it was never fixed, and otherwise it stays for the process, so a
	// late replay on that call's context still goes only to its target.
	outlived bool
	// splitLogged says the Error line for a request refused because it saw
	// this tether beside one on another address has been written.
	splitLogged bool
}

// tetherTable holds every tether of one pool, reserved or fixed, under one
// lock. A tether made through one pool never steers a request that another
// pool handles. The zero value is ready to use. Its lock may be held while a
// scope's lock or scopelessTables.mu is taken, and never the other way round.
type tetherTable struct {
	mu    sync.Mutex
	byKey map[tetherKey][]*tether
	// watched holds the scopes whose release already drops their tethers,
	// so each scope registers one release function per pool.
	watched map[*Scope]struct{}
	// registered says the table is in scopelessTables, because it holds a
	// tether made without a scope that no call's release has dropped yet.
	registered bool
}

// scopelessTables holds every tether table with a tether made without a
// scope that may still be dropped, so the release of a call's scope reaches it
// even when none of the call's own requests went through its pool. A table
// leaves the set at the first release after it joined.
var scopelessTables struct {
	mu  sync.Mutex
	set map[*tetherTable]struct{}
}

// tetherHold is one request's hold on the tethers of its key. A request
// claims it before the pool hands the request to any transport, and settles
// it when the request ends. A walk that hops releases it after a hop that
// proved nothing was sent and claims it again for the next hop. It is used
// by one goroutine.
type tetherHold struct {
	key   tetherKey
	scope *Scope
	// method and route are the request's own, for the log lines.
	method string
	route  string
	// held lists the tethers the request holds, which all name target.
	held   []*tether
	target tetherTarget
	// following says the key already had a tether when the request claimed
	// it, so the request goes only to that tether's target and never hops.
	following bool
}

// newTetherHold returns the hold for a mutation, with nothing claimed yet.
func newTetherHold(req *http.Request) *tetherHold {
	return &tetherHold{
		key:    tetherKeyOf(req),
		scope:  ScopeFromContext(req.Context()),
		method: methodOf(req),
		route:  keyRoute(req.URL.Path),
	}
}

// claim takes the tether for h's key under the table's lock, before the
// request is handed to any transport. h must hold nothing.
//
//   - When a tether the request can see is conflicted, the request is
//     refused with an error that wraps ErrTetherConflict.
//   - When the request sees no tether, it reserves one on planned, the
//     address it is about to try, and holds it. The first mutation with a key
//     wins.
//   - When it sees tethers, reserved or fixed, it holds them all and goes to
//     the target they name, without waiting for the requests that hold them,
//     so two requests in flight with one key share one endpoint.
//   - When the tethers it sees name more than one address, which happens only
//     to a request without a scope, or one whose call already returned, that
//     sees the tethers two calls made on different endpoints, it is refused
//     with an error that wraps ErrTetherConflict, and the pool logs it at
//     Error once per key. Either address could be the one its call sent to,
//     so neither is safe.
//
// Scope.sees decides which tethers a request can see, and a request whose
// scope was already released also sees every tether that outlived the call
// that made it, so late requests with one key share one tether. A new reservation made under
// a scope that is not yet released registers the release that drops the
// scope's tethers, once per scope, outside the table's lock, as
// Scope.OnRelease asks. One made under a released scope has outlived its call
// already, and one made without a scope puts the table in scopelessTables.
func (p *Pool) claim(h *tetherHold, planned tetherTarget) error {
	t := &p.tethers
	t.mu.Lock()
	late := h.scope.Released()
	visible := t.visibleLocked(h.key, h.scope, late)
	for _, x := range visible {
		if x.conflicted {
			t.mu.Unlock()
			return fmt.Errorf("endpoint pool: refusing %s %s: %w", h.method, h.route, ErrTetherConflict)
		}
	}
	if len(visible) > 0 {
		target := visible[0].target
		for _, x := range visible[1:] {
			if x.target.addr != target.addr {
				first := markSplitLocked(visible)
				t.mu.Unlock()
				if first {
					p.logSplit(h, target.addr, x.target.addr)
				}
				return fmt.Errorf("endpoint pool: refusing %s %s, whose tether key is tethered to both %s and %s "+
					"by different calls: %w", h.method, h.route, target.addr, x.target.addr, ErrTetherConflict)
			}
		}
		for _, x := range visible {
			x.holders++
		}
		h.held, h.target, h.following = visible, target, true
		t.mu.Unlock()
		return nil
	}
	x := &tether{key: h.key, scope: h.scope, target: planned, holders: 1, outlived: late}
	if t.byKey == nil {
		t.byKey = make(map[tetherKey][]*tether)
	}
	t.byKey[h.key] = append(t.byKey[h.key], x)
	h.held, h.target, h.following = []*tether{x}, planned, false
	watch := false
	switch {
	case h.scope == nil:
		t.registerLocked()
	case !late:
		if _, ok := t.watched[h.scope]; !ok {
			if t.watched == nil {
				t.watched = make(map[*Scope]struct{})
			}
			t.watched[h.scope] = struct{}{}
			watch = true
		}
	}
	t.mu.Unlock()
	if watch {
		p.watchScope(h.scope)
	}
	return nil
}

// watchScope registers the release that drops s's tethers from this pool.
// When s was released after claim found it live, its release has already run
// without this pool, so the pool drops s's tethers itself, as that release
// would have. The tether the claim made is still held, so it outlives the call
// rather than being dropped under its request.
func (p *Pool) watchScope(s *Scope) {
	if !s.OnRelease(func() { p.tethers.dropScope(s) }) {
		p.tethers.dropScope(s)
	}
}

// release drops h's hold after a hop or a request that proved nothing was
// sent, and reports whether the request may hop and claim again on its next
// endpoint. A tether that no other request holds and that was never fixed is
// removed, because nothing with its key has left the machine. A request that
// followed a tether never hops, and neither does one whose tether another
// request still holds or has fixed: it returns its error, and the next
// attempt meets whatever the tether has become by then. A tether that outlived
// its call is removed by the same rule.
func (p *Pool) release(h *tetherHold) bool {
	t := &p.tethers
	t.mu.Lock()
	defer t.mu.Unlock()
	remaining := false
	for _, x := range h.held {
		x.holders--
		if x.holders <= 0 && !x.fixed && !x.conflicted {
			t.removeLocked(x)
			continue
		}
		remaining = true
	}
	mayHop := !remaining && !h.following
	h.held, h.following = nil, false
	return mayHop
}

// fix ends h's hold for a request that may have been sent to sent, the
// canonical address the request's final hop went to. Each tether the request
// holds becomes fixed on its target, and a tether that was already fixed is
// refreshed in place, so a fixed tether never moves. The first time a tether
// is fixed, the pool logs it at Debug.
//
// When sent is not the address the request's tethers name, which the claim
// made before every hop rules out and only a defect can cause, the key is
// marked conflicted for as long as the tethers last, the pool logs it at Error
// once, and every later mutation with the key is refused with
// ErrTetherConflict before dispatch.
func (p *Pool) fix(h *tetherHold, sent string) {
	t := &p.tethers
	t.mu.Lock()
	mismatch := sent != h.target.addr
	newlyFixed, newlyConflicted := false, false
	for _, x := range h.held {
		x.holders--
		x.fixed = true
		if mismatch {
			if !x.conflicted {
				x.conflicted, newlyConflicted = true, true
			}
			continue
		}
		if !x.logged {
			x.logged, newlyFixed = true, true
		}
	}
	target := h.target
	h.held, h.following = nil, false
	t.mu.Unlock()

	switch {
	case newlyConflicted:
		p.logConflict(h, target.addr, sent)
	case newlyFixed:
		p.logTethered(h, target.addr)
	}
}

// settle ends h's hold from the outcome that ended the request. sentNever
// releases it, and every other outcome fixes the tether on the address the
// outcome names. A hold that holds nothing, such as one whose request was
// refused before dispatch or released between hops, settles to nothing.
func (p *Pool) settle(h *tetherHold, oc outcome) {
	if len(h.held) == 0 {
		return
	}
	if oc.sent == sentNever {
		p.release(h)
		return
	}
	p.fix(h, oc.addr)
}

// visibleLocked returns the tethers for key that a request under scope can
// see, in the order they were made. A late request, one whose scope was
// already released, also sees every tether that outlived the call that made
// it. t.mu must be held. The slice is new, so a hold may keep it.
func (t *tetherTable) visibleLocked(key tetherKey, scope *Scope, late bool) []*tether {
	var out []*tether
	for _, x := range t.byKey[key] {
		if scope.sees(x.scope) || late && x.outlived {
			out = append(out, x)
		}
	}
	return out
}

// markSplitLocked marks the tethers a refused request saw on two addresses,
// and reports whether any of them was not marked yet, so the refusal is logged
// once per key for as long as those tethers last. t.mu must be held.
func markSplitLocked(visible []*tether) bool {
	first := false
	for _, x := range visible {
		if !x.splitLogged {
			x.splitLogged, first = true, true
		}
	}
	return first
}

// registerLocked puts the table in scopelessTables once. t.mu must be held.
func (t *tetherTable) registerLocked() {
	if t.registered {
		return
	}
	t.registered = true
	scopelessTables.mu.Lock()
	defer scopelessTables.mu.Unlock()
	if scopelessTables.set == nil {
		scopelessTables.set = make(map[*tetherTable]struct{})
	}
	scopelessTables.set[t] = struct{}{}
}

// removeLocked takes x out of the table. t.mu must be held.
func (t *tetherTable) removeLocked(x *tether) {
	list := t.byKey[x.key]
	kept := make([]*tether, 0, len(list))
	for _, y := range list {
		if y != x {
			kept = append(kept, y)
		}
	}
	if len(kept) == 0 {
		delete(t.byKey, x.key)
		return
	}
	t.byKey[x.key] = kept
}

// dropScope ends the call that owns s for the tethers made under s, as
// endCallLocked does, and stops watching s.
func (t *tetherTable) dropScope(s *Scope) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endCallLocked(func(x *tether) bool { return x.scope == s })
	delete(t.watched, s)
}

// dropScopeless ends the call for the tethers made without a scope, as
// endCallLocked does, and takes the table out of scopelessTables, because no
// tether it holds is left for a later release to drop.
func (t *tetherTable) dropScopeless() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endCallLocked(func(x *tether) bool { return x.scope == nil })
	if !t.registered {
		return
	}
	t.registered = false
	scopelessTables.mu.Lock()
	defer scopelessTables.mu.Unlock()
	delete(scopelessTables.set, t)
}

// endCallLocked handles the tethers that made selects, which have not yet
// outlived a call, when their call returns. A tether that no request holds is
// removed, reserved, fixed, or conflicted. A tether that a request still holds
// outlives the call, so its request still settles it and a replay of that
// request still meets it. t.mu must be held.
func (t *tetherTable) endCallLocked(made func(*tether) bool) {
	for key, list := range t.byKey {
		kept := make([]*tether, 0, len(list))
		for _, x := range list {
			if made(x) && !x.outlived {
				if x.holders <= 0 {
					continue
				}
				x.outlived = true
			}
			kept = append(kept, x)
		}
		if len(kept) == 0 {
			delete(t.byKey, key)
			continue
		}
		t.byKey[key] = kept
	}
}

// dropScopelessTethers runs dropScopeless on every table in scopelessTables.
// The release of a call's scope calls it, because a tether made without a
// scope ends with the call that made it. It takes each table's lock only after
// it lets go of scopelessTables.mu.
func dropScopelessTethers() {
	scopelessTables.mu.Lock()
	tables := make([]*tetherTable, 0, len(scopelessTables.set))
	for t := range scopelessTables.set {
		tables = append(tables, t)
	}
	scopelessTables.mu.Unlock()
	for _, t := range tables {
		t.dropScopeless()
	}
}

// logTethered logs, at Debug and once per tether, that a mutation may have
// reached addr and that its key now stays there. A family's line names the
// family, because every mutation in it stays.
func (p *Pool) logTethered(h *tetherHold, addr string) {
	stays := "retries stay on " + addr
	if h.key.family != "" {
		stays = string(h.key.family) + " mutations stay on " + addr
	}
	p.logger.Debug("pve endpoints: "+h.method+" "+h.route+" may have reached "+addr+"; "+stays,
		tetherFields(h, addr)...)
}

// logConflict logs, at Error and once per key for as long as its tethers
// last, that a mutation may have reached two addresses. Only a defect in the
// pool can cause it.
func (p *Pool) logConflict(h *tetherHold, tethered, sent string) {
	refusing := "refusing further mutations with this key for this call"
	if h.key.family != "" {
		refusing = "refusing further " + string(h.key.family) + " mutations for this call"
	}
	p.logger.Error("pve endpoints: "+h.method+" "+h.route+" may have reached both "+tethered+" and "+
		sentName(sent)+"; "+refusing,
		append(tetherFields(h, tethered), log.String("sent", sentName(sent)))...)
}

// logSplit logs, at Error and once per key for as long as the tethers it saw
// last, that a request was refused because the tethers it sees name two
// addresses, each of which a mutation from another call may have reached.
func (p *Pool) logSplit(h *tetherHold, first, second string) {
	refusing := "refusing further mutations with this key that see both"
	if h.key.family != "" {
		refusing = "refusing further " + string(h.key.family) + " mutations that see both"
	}
	p.logger.Error("pve endpoints: "+h.method+" "+h.route+" is tethered to both "+first+" and "+second+
		" by different calls; "+refusing,
		append(tetherFields(h, first), log.String("other", second))...)
}

// sentName names the address a maybe-sent request went to, which is empty
// only when a defect lost it.
func sentName(addr string) string {
	if addr == "" {
		return "an unknown endpoint"
	}
	return addr
}

// tetherFields returns the structured fields of a tether's log lines.
func tetherFields(h *tetherHold, addr string) []log.Field {
	fields := []log.Field{
		log.String("method", h.method),
		log.String("path", h.route),
		log.String("endpoint", addr),
	}
	if h.key.family != "" {
		fields = append(fields, log.String("family", string(h.key.family)))
	}
	return fields
}
