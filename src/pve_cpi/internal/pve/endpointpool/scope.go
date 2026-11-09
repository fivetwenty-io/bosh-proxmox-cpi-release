package endpointpool

import (
	"context"
	"sync"
)

type scopeKey struct{}

// Scope is the affinity scope of one CPI call. The dispatcher, and each
// operator command, puts one on the call's context. A pool ties each tether it
// makes to the scope on the request's context, and drops those tethers when the
// scope is released, so a tether lasts as long as the call that made it. A
// tether made on a context without a scope lasts for the process.
//
// A Scope is compared by identity, so it must come from WithAffinityScope and
// be passed around as the pointer that function put on the context.
type Scope struct {
	mu       sync.Mutex
	released bool
	fns      []func()
}

// WithAffinityScope returns a context that carries a new scope, and a release
// function. Release runs every function registered with OnRelease, in
// registration order, once, and later calls to it do nothing. The caller calls
// release when the call ends.
//
// When ctx already carries a scope, the new one replaces it for requests on the
// returned context and does not release the outer one.
func WithAffinityScope(ctx context.Context) (context.Context, func()) {
	s := &Scope{}
	return context.WithValue(ctx, scopeKey{}, s), s.release
}

// ScopeFromContext returns the scope on the context, or nil when the call has
// none.
func ScopeFromContext(ctx context.Context) *Scope {
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// OnRelease registers fn to run once when the scope is released, so a pool can
// drop the scope's tethers without the dispatcher knowing about pools. It
// returns false and does not keep fn when the scope has already been released,
// and the caller then handles the late registration itself. fn runs in the
// goroutine that calls the release function and without the scope's lock, and
// it must not block on locks held by a caller of OnRelease. A nil receiver or a
// nil fn registers nothing and returns false.
func (s *Scope) OnRelease(fn func()) bool {
	if s == nil || fn == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return false
	}
	s.fns = append(s.fns, fn)
	return true
}

// Released reports whether the scope has been released. A nil scope reports
// false.
func (s *Scope) Released() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

// sees reports whether a request whose context carries scope s sees a tether
// made under scope made. A request with a scope sees the tethers its own scope
// made and the tethers made without a scope. A request without a scope sees
// every tether, so a mutation sent on a context that a rollback or parker path
// built from context.Background() still meets a tether its call made. Two
// calls with different scopes never see each other's tethers.
func (s *Scope) sees(made *Scope) bool {
	return s == nil || made == nil || made == s
}

// release marks the scope released and runs the registered functions once. A
// function that panics does not keep the rest from running, and its panic is
// dropped. The dispatcher calls release from a defer after a call's work has
// finished, and the registered functions only drop tethers, so a faulty one
// must not crash the CPI at that point.
func (s *Scope) release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	fns := s.fns
	s.fns = nil
	s.mu.Unlock()

	for _, fn := range fns {
		func() {
			defer func() { _ = recover() }()
			fn()
		}()
	}
}
