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
	// tether stays on its target until its scope is released, or for the
	// process when it has no scope.
	fixed bool
	// logged says the Debug line for the fixed tether has been written.
	logged bool
	// conflicted says a request holding the tether ended maybe sent on
	// another address, which only a defect can cause. Every later mutation
	// that sees the tether is refused with ErrTetherConflict.
	conflicted bool
	// gone says the tether is no longer in the table, because it was
	// released or its scope was.
	gone bool
}

// tetherTable holds every tether of one pool, reserved or fixed, under one
// lock. A tether made through one pool never steers a request that another
// pool handles. The zero value is ready to use.
type tetherTable struct {
	mu    sync.Mutex
	byKey map[tetherKey][]*tether
	// watched holds the scopes whose release already drops their tethers,
	// so each scope registers one release function per pool.
	watched map[*Scope]struct{}
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
//     to a request without a scope while two calls in one process each
//     tethered the key to a different endpoint, it is refused with an error
//     that wraps ErrTetherConflict. Either address could be the one its call
//     sent to, so neither is safe.
//
// Scope.sees decides which tethers a request can see. A new reservation made
// under a scope registers the release that drops the scope's tethers, once per
// scope, outside the table's lock, as Scope.OnRelease asks.
func (p *Pool) claim(h *tetherHold, planned tetherTarget) error {
	t := &p.tethers
	t.mu.Lock()
	visible := t.visibleLocked(h.key, h.scope)
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
				t.mu.Unlock()
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
	x := &tether{key: h.key, scope: h.scope, target: planned, holders: 1}
	if t.byKey == nil {
		t.byKey = make(map[tetherKey][]*tether)
	}
	t.byKey[h.key] = append(t.byKey[h.key], x)
	h.held, h.target, h.following = []*tether{x}, planned, false
	watch := false
	if h.scope != nil {
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
// When s was already released, which only a request still in flight after
// its call returned can meet, no release will ever come, so the tethers made
// under s stay for the process, as a tether without a scope does. Dropping
// them early would let a late replay on that context reach another endpoint,
// and keeping them only keeps more mutations on their endpoints.
func (p *Pool) watchScope(s *Scope) {
	_ = s.OnRelease(func() { p.tethers.dropScope(s) })
}

// release drops h's hold after a hop or a request that proved nothing was
// sent, and reports whether the request may hop and claim again on its next
// endpoint. A tether that no other request holds and that was never fixed is
// removed, because nothing with its key has left the machine. A request that
// followed a tether never hops, and neither does one whose tether another
// request still holds or has fixed: it returns its error, and the next
// attempt meets whatever the tether has become by then. A tether whose scope
// was released while the request was in flight no longer counts.
func (p *Pool) release(h *tetherHold) bool {
	t := &p.tethers
	t.mu.Lock()
	defer t.mu.Unlock()
	remaining := false
	for _, x := range h.held {
		x.holders--
		if x.gone {
			continue
		}
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
		if x.gone {
			continue
		}
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
// see, in the order they were made. t.mu must be held. The slice is new, so a
// hold may keep it.
func (t *tetherTable) visibleLocked(key tetherKey, scope *Scope) []*tether {
	var out []*tether
	for _, x := range t.byKey[key] {
		if scope.sees(x.scope) {
			out = append(out, x)
		}
	}
	return out
}

// removeLocked takes x out of the table. t.mu must be held.
func (t *tetherTable) removeLocked(x *tether) {
	x.gone = true
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

// dropScope removes every tether made under s, reserved, fixed, or
// conflicted, when the call that owns s returns.
func (t *tetherTable) dropScope(s *Scope) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, list := range t.byKey {
		kept := make([]*tether, 0, len(list))
		for _, x := range list {
			if x.scope == s {
				x.gone = true
				continue
			}
			kept = append(kept, x)
		}
		if len(kept) == 0 {
			delete(t.byKey, key)
			continue
		}
		t.byKey[key] = kept
	}
	delete(t.watched, s)
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
