package endpointpool

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// tetherLab is a pool over addresses nothing listens on, for tests that drive
// the tether table directly and never send a request.
type tetherLab struct {
	p   *Pool
	obs *log.Observer
}

func newTetherLab(t *testing.T, addrs ...string) *tetherLab {
	t.Helper()
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	eps := make([]Endpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, configured(a))
	}
	return &tetherLab{p: newPool(t, Config{Base: baseFor(nil, nil), Logger: logger, Endpoints: eps}), obs: obs}
}

// hold returns a fresh hold for a mutation on ctx.
func (l *tetherLab) hold(t *testing.T, ctx context.Context, method, route string) *tetherHold {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "https://pve1:8006"+APIPrefix+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newTetherHold(req)
}

// target returns the target naming the pool's endpoint at addr.
func (l *tetherLab) target(t *testing.T, addr string) tetherTarget {
	t.Helper()
	return targetOf(endpointOf(t, l.p, addr))
}

// mustClaim claims h on planned and fails the test on a refusal. It returns
// the address the request goes to and whether it follows an existing tether.
func (l *tetherLab) mustClaim(t *testing.T, h *tetherHold, planned tetherTarget) (string, bool) {
	t.Helper()
	if err := l.p.claim(h, planned); err != nil {
		t.Fatalf("claim %s %s: %v", h.method, h.route, err)
	}
	return h.target.addr, h.following
}

// tethersFor returns the tethers in the table for key.
func (l *tetherLab) tethersFor(key tetherKey) []*tether {
	l.p.tethers.mu.Lock()
	defer l.p.tethers.mu.Unlock()
	return append([]*tether(nil), l.p.tethers.byKey[key]...)
}

// lines returns the tether and conflict lines logged at level.
func (l *tetherLab) lines(level slog.Level) []log.Entry {
	var out []log.Entry
	for _, e := range l.obs.All() {
		if e.Level == level && strings.HasPrefix(e.Message, "pve endpoints: ") &&
			strings.Contains(e.Message, " may have reached ") {
			out = append(out, e)
		}
	}
	return out
}

const (
	tetherA = "pve1:8006"
	tetherB = "pve2:8006"
	tetherC = "pve3:8006"
)

// A reservation that the only request holding it releases after a proof that
// nothing was sent is removed, so that request may hop, and the next mutation
// with the key reserves afresh on whatever endpoint it is about to try.
func TestTetherReleasedReservationLetsTheNextReserveAfresh(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	h1 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, h1, l.target(t, tetherA)); addr != tetherA || following {
		t.Fatalf("first claim went to %s, following=%v", addr, following)
	}
	if !l.p.release(h1) {
		t.Fatal("the only holder of a reservation that was never fixed may hop")
	}
	if n := len(l.tethersFor(h1.key)); n != 0 {
		t.Fatalf("%d tethers left after the release, want 0", n)
	}
	h2 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, h2, l.target(t, tetherB)); addr != tetherB || following {
		t.Fatalf("the next claim went to %s, following=%v, want a fresh reservation on B", addr, following)
	}
}

// A second mutation with a key the first one holds follows the reservation at
// once, whatever endpoint it was about to try. A reservation that another
// request still holds is not removed, and neither holder may hop.
func TestTetherReservationHeldByAnotherIsNotRemoved(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	h1 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, h1, l.target(t, tetherA))
	h2 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, h2, l.target(t, tetherB)); addr != tetherA || !following {
		t.Fatalf("the second claim went to %s, following=%v, want it to follow A", addr, following)
	}

	if l.p.release(h1) {
		t.Fatal("a reservation another request holds must not let its releaser hop")
	}
	if n := len(l.tethersFor(h1.key)); n != 1 {
		t.Fatalf("%d tethers after the first release, want 1", n)
	}
	h3 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, h3, l.target(t, tetherB)); addr != tetherA || !following {
		t.Fatalf("the third claim went to %s, following=%v", addr, following)
	}
	if l.p.release(h2) || l.p.release(h3) {
		t.Fatal("a request that followed a tether never hops")
	}
	if n := len(l.tethersFor(h1.key)); n != 0 {
		t.Fatalf("%d tethers after every holder released, want 0", n)
	}
}

// Any outcome but a proof that nothing was sent fixes the tether, and a fixed
// tether outlives every holder and never moves.
func TestTetherFixedTetherNeverMoves(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	h1 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, h1, l.target(t, tetherA))
	l.p.settle(h1, outcome{sent: sentAnswered, addr: tetherA})
	if len(h1.held) != 0 {
		t.Fatal("settling must empty the hold")
	}

	h2 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, h2, l.target(t, tetherB)); addr != tetherA || !following {
		t.Fatalf("a claim after the fix went to %s, following=%v", addr, following)
	}
	l.p.settle(h2, outcome{sent: sentNever})
	tethers := l.tethersFor(h1.key)
	if len(tethers) != 1 || !tethers[0].fixed || tethers[0].target.addr != tetherA || tethers[0].holders != 0 {
		t.Fatalf("tethers = %+v, want one fixed on A with no holders", tethers)
	}
	h3 := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, h3, l.target(t, tetherB))
	l.p.settle(h3, outcome{sent: sentMaybe, addr: tetherA})
	if tethers := l.tethersFor(h1.key); len(tethers) != 1 || tethers[0].target.addr != tetherA {
		t.Fatalf("a later outcome on A moved the tether: %+v", tethers)
	}
	if n := len(l.lines(slog.LevelDebug)); n != 1 {
		t.Fatalf("Debug tether lines = %d, want 1 for the one tether", n)
	}
}

// A hold that holds nothing settles to nothing, whatever the outcome says.
func TestTetherSettleOfAnEmptyHold(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	h := l.hold(t, context.Background(), http.MethodPost, "/nodes/n1/qemu")
	l.p.settle(h, outcome{sent: sentMaybe, addr: tetherA})
	l.p.settle(h, outcome{sent: sentNever})
	if n := len(l.tethersFor(h.key)); n != 0 {
		t.Fatalf("an empty hold made %d tethers", n)
	}
	if n := len(l.lines(slog.LevelDebug)) + len(l.lines(slog.LevelError)); n != 0 {
		t.Fatalf("an empty hold logged %d tether lines", n)
	}
}

// The Debug line names the request and the endpoint once per tether, in the
// single-key form or, for a family, in the family form.
func TestTetherDebugLineForms(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	single := l.hold(t, context.Background(), http.MethodPost, "/nodes/n3/qemu/101/clone")
	l.mustClaim(t, single, l.target(t, tetherB))
	l.p.settle(single, outcome{sent: sentMaybe, addr: tetherB})
	fam := l.hold(t, context.Background(), http.MethodPut, "/cluster/ha/rules/cpi-aa-web")
	l.mustClaim(t, fam, l.target(t, tetherB))
	l.p.settle(fam, outcome{sent: sentAnswered, addr: tetherB})

	lines := l.lines(slog.LevelDebug)
	if len(lines) != 2 {
		t.Fatalf("Debug lines = %d, want 2", len(lines))
	}
	wantSingle := "pve endpoints: POST /nodes/n3/qemu/101/clone may have reached pve2:8006; retries stay on pve2:8006"
	if lines[0].Message != wantSingle {
		t.Errorf("single-key line = %q, want %q", lines[0].Message, wantSingle)
	}
	mustAttrs(t, lines[0], map[string]string{"method": "POST", "path": "/nodes/n3/qemu/101/clone",
		"endpoint": tetherB})
	if _, ok := lines[0].Attrs["family"]; ok {
		t.Error("the single-key line carries a family field")
	}
	wantFamily := "pve endpoints: PUT /cluster/ha/rules/cpi-aa-web may have reached pve2:8006; " +
		"ha mutations stay on pve2:8006"
	if lines[1].Message != wantFamily {
		t.Errorf("family line = %q, want %q", lines[1].Message, wantFamily)
	}
	mustAttrs(t, lines[1], map[string]string{"method": "PUT", "path": "/cluster/ha/rules/cpi-aa-web",
		"endpoint": tetherB, "family": "ha"})

	again := l.hold(t, context.Background(), http.MethodDelete, "/cluster/ha/rules/other")
	l.mustClaim(t, again, l.target(t, tetherA))
	l.p.settle(again, outcome{sent: sentAnswered, addr: tetherB})
	if n := len(l.lines(slog.LevelDebug)); n != 2 {
		t.Fatalf("Debug lines after refreshing the ha tether = %d, want 2", n)
	}
}

// A request that ends maybe sent on an address other than its tether's, which
// only a defect can cause, marks the key conflicted and logs once at Error.
// Every later mutation with the key is refused with ErrTetherConflict, another
// key is untouched, and another call's scope does not see the conflict.
func TestTetherConflictRefusesTheKey(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	ctx, release := WithAffinityScope(context.Background())
	h := l.hold(t, ctx, http.MethodPost, "/nodes/n3/qemu")
	l.mustClaim(t, h, l.target(t, tetherA))
	// The test hook: settle with an outcome on B for a hold that names A.
	l.p.settle(h, outcome{sent: sentMaybe, addr: tetherB})

	for range 3 {
		later := l.hold(t, ctx, http.MethodPost, "/nodes/n3/qemu?vmid=102")
		err := l.p.claim(later, l.target(t, tetherA))
		if !errors.Is(err, ErrTetherConflict) {
			t.Fatalf("claim after the conflict: %v, want ErrTetherConflict", err)
		}
		if len(later.held) != 0 {
			t.Fatal("a refused claim holds tethers")
		}
	}
	bare := l.hold(t, context.Background(), http.MethodPost, "/nodes/n3/qemu")
	if err := l.p.claim(bare, l.target(t, tetherB)); !errors.Is(err, ErrTetherConflict) {
		t.Fatalf("a request without a scope sees the call's conflict: %v", err)
	}

	errs := l.lines(slog.LevelError)
	if len(errs) != 1 {
		t.Fatalf("Error lines = %d, want 1", len(errs))
	}
	want := "pve endpoints: POST /nodes/n3/qemu may have reached both pve1:8006 and pve2:8006; " +
		"refusing further mutations with this key for this call"
	if errs[0].Message != want {
		t.Errorf("conflict line = %q, want %q", errs[0].Message, want)
	}
	mustAttrs(t, errs[0], map[string]string{"endpoint": tetherA, "sent": tetherB, "method": "POST"})

	other := l.hold(t, ctx, http.MethodPost, "/nodes/n3/qemu/101/clone")
	if addr, following := l.mustClaim(t, other, l.target(t, tetherB)); addr != tetherB || following {
		t.Fatalf("another key went to %s, following=%v", addr, following)
	}
	otherCall, releaseOther := WithAffinityScope(context.Background())
	defer releaseOther()
	fresh := l.hold(t, otherCall, http.MethodPost, "/nodes/n3/qemu")
	if addr, following := l.mustClaim(t, fresh, l.target(t, tetherB)); addr != tetherB || following {
		t.Fatalf("another call's claim went to %s, following=%v", addr, following)
	}
	l.p.settle(fresh, outcome{sent: sentNever})

	release()
	after := l.hold(t, context.Background(), http.MethodPost, "/nodes/n3/qemu")
	if addr, following := l.mustClaim(t, after, l.target(t, tetherB)); addr != tetherB || following {
		t.Fatalf("after the call returned, a claim went to %s, following=%v", addr, following)
	}
}

// The family form of the conflict line names the family.
func TestTetherConflictLineForAFamily(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	h := l.hold(t, context.Background(), http.MethodPut, "/cluster/ha/rules/x")
	l.mustClaim(t, h, l.target(t, tetherA))
	l.p.settle(h, outcome{sent: sentAnswered, addr: ""})
	errs := l.lines(slog.LevelError)
	want := "pve endpoints: PUT /cluster/ha/rules/x may have reached both pve1:8006 and an unknown endpoint; " +
		"refusing further ha mutations for this call"
	if len(errs) != 1 || errs[0].Message != want {
		t.Fatalf("Error lines = %+v, want one %q", errs, want)
	}
	mustAttrs(t, errs[0], map[string]string{"family": "ha"})
}

// A request with a scope sees its own scope's tethers and the ones made
// without a scope, and a request without one sees every tether. Releasing a
// scope drops its tethers, and a tether made without a scope stays.
func TestTetherVisibility(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB, tetherC)
	call1, release1 := WithAffinityScope(context.Background())
	call2, release2 := WithAffinityScope(context.Background())
	defer release2()
	const route = "/nodes/n1/qemu"

	h1 := l.hold(t, call1, http.MethodPost, route)
	l.mustClaim(t, h1, l.target(t, tetherA))
	l.p.settle(h1, outcome{sent: sentMaybe, addr: tetherA})

	// Another call does not see call 1's tether.
	h2 := l.hold(t, call2, http.MethodPost, route)
	if addr, following := l.mustClaim(t, h2, l.target(t, tetherB)); addr != tetherB || following {
		t.Fatalf("call 2 went to %s, following=%v, want its own reservation on B", addr, following)
	}
	l.p.settle(h2, outcome{sent: sentMaybe, addr: tetherB})

	// A request without a scope sees both, which name different endpoints,
	// so it is refused rather than guess which call it belongs to.
	bare := l.hold(t, context.Background(), http.MethodPost, route)
	err := l.p.claim(bare, l.target(t, tetherC))
	if !errors.Is(err, ErrTetherConflict) || !strings.Contains(err.Error(), "by different calls") {
		t.Fatalf("a request without a scope over two calls' tethers: %v", err)
	}

	// Once call 2 returns, a request without a scope follows call 1's tether.
	release2()
	bare = l.hold(t, context.Background(), http.MethodPost, route)
	if addr, following := l.mustClaim(t, bare, l.target(t, tetherC)); addr != tetherA || !following {
		t.Fatalf("after call 2 returned, a bare request went to %s, following=%v", addr, following)
	}
	l.p.settle(bare, outcome{sent: sentMaybe, addr: tetherA})

	release1()
	if n := len(l.tethersFor(h1.key)); n != 0 {
		t.Fatalf("%d tethers left after both calls returned, want 0", n)
	}

	// A tether made without a scope is seen by every call and outlives them.
	bare = l.hold(t, context.Background(), http.MethodPost, route)
	l.mustClaim(t, bare, l.target(t, tetherC))
	l.p.settle(bare, outcome{sent: sentMaybe, addr: tetherC})
	call3, release3 := WithAffinityScope(context.Background())
	h3 := l.hold(t, call3, http.MethodPost, route)
	if addr, following := l.mustClaim(t, h3, l.target(t, tetherA)); addr != tetherC || !following {
		t.Fatalf("call 3 went to %s, following=%v, want the scopeless tether on C", addr, following)
	}
	l.p.settle(h3, outcome{sent: sentMaybe, addr: tetherC})
	release3()
	if tethers := l.tethersFor(h1.key); len(tethers) != 1 || tethers[0].scope != nil {
		t.Fatalf("tethers after call 3 returned = %+v, want the scopeless one", tethers)
	}
	l.p.tethers.mu.Lock()
	watched := len(l.p.tethers.watched)
	l.p.tethers.mu.Unlock()
	if watched != 0 {
		t.Fatalf("%d released scopes still watched", watched)
	}
}

// A tether made under a scope that has already been released, which only a
// request still in flight after its call returned can meet, stays for the
// process, because no release will come to drop it.
func TestTetherUnderAReleasedScopeStays(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	ctx, release := WithAffinityScope(context.Background())
	release()
	h := l.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, h, l.target(t, tetherA))
	l.p.settle(h, outcome{sent: sentMaybe, addr: tetherA})
	release()
	late := l.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	if addr, following := l.mustClaim(t, late, l.target(t, tetherB)); addr != tetherA || !following {
		t.Fatalf("a late replay went to %s, following=%v, want the tether on A", addr, following)
	}
	l.p.settle(late, outcome{sent: sentNever})
}

// A scope's release drops its tethers while requests still hold them, and a
// hold whose tether was dropped while it was in flight settles without
// touching the table.
func TestTetherScopeReleaseWhileInFlight(t *testing.T) {
	l := newTetherLab(t, tetherA, tetherB)
	ctx, release := WithAffinityScope(context.Background())
	h := l.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, h, l.target(t, tetherA))
	follower := l.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	l.mustClaim(t, follower, l.target(t, tetherB))
	release()
	if !l.p.release(h) {
		t.Fatal("a hold whose tether was dropped holds nothing, so the request may hop")
	}
	l.p.fix(follower, tetherA)
	if n := len(l.tethersFor(h.key)); n != 0 {
		t.Fatalf("%d tethers after the release, want 0", n)
	}
	if n := len(l.lines(slog.LevelDebug)); n != 0 {
		t.Fatalf("a dropped tether logged %d Debug lines", n)
	}
}

// Two pools never see each other's tethers, even under one scope.
func TestTetherPoolsAreSeparate(t *testing.T) {
	one := newTetherLab(t, tetherA, tetherB)
	two := newTetherLab(t, tetherC, tetherA)
	ctx, release := WithAffinityScope(context.Background())
	defer release()
	h := one.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	one.mustClaim(t, h, one.target(t, tetherA))
	one.p.settle(h, outcome{sent: sentMaybe, addr: tetherA})
	other := two.hold(t, ctx, http.MethodPost, "/nodes/n1/qemu")
	if addr, following := two.mustClaim(t, other, two.target(t, tetherC)); addr != tetherC || following {
		t.Fatalf("the second pool's claim went to %s, following=%v", addr, following)
	}
}
