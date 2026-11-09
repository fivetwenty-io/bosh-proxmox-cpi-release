package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// sendByCall is one pool call as the send-by wire saw it.
type sendByCall struct {
	method, route string
	// deadline and guarded say whether a send-by marker applied to this
	// call, and which deadline it carried.
	deadline time.Time
	guarded  bool
	// steal says whether the call's context carried the steal's marker at
	// all, which a read on the same context ignores.
	steal   bool
	refused bool
}

// sendByWire stands in for the failover transport's send-by check in front of
// a pool service. It checks every call the way the transport checks a request,
// with the method and the SDK-prefixed path the call sends, against the time
// now returns. A refused call never reaches the pool service and fails with
// endpointpool.ErrSendByPassed wrapped the way the SDK wraps a transport
// failure. Every call is recorded with the marker it carried.
type sendByWire struct {
	PoolService
	now func() time.Time
	// afterDelete, when set, runs once a delete has reached the pool service.
	afterDelete func()

	mu    sync.Mutex
	calls []sendByCall
}

// sdkSendByRefusal is the error the SDK surfaces when its transport refuses a
// request for its send-by deadline: the retry loop's wrap around a *url.Error.
func sdkSendByRefusal(method, path string, cause error) error {
	op := strings.ToUpper(method[:1]) + strings.ToLower(method[1:])
	return fmt.Errorf("request failed after 1 attempt(s): %w",
		&url.Error{Op: op, URL: "https://pve1:8006" + path, Err: cause})
}

func (w *sendByWire) admit(ctx context.Context, method, route string) error {
	path := endpointpool.APIPrefix + route
	deadline, guarded := endpointpool.SendBy(ctx, method, path)
	_, steal := endpointpool.SendBy(ctx, http.MethodDelete, path)
	refusal := endpointpool.CheckSendBy(ctx, w.now(), method, path)
	w.mu.Lock()
	w.calls = append(w.calls, sendByCall{
		method: method, route: route, deadline: deadline, guarded: guarded, steal: steal, refused: refusal != nil,
	})
	w.mu.Unlock()
	if refusal != nil {
		return sdkSendByRefusal(method, path, refusal)
	}
	return nil
}

func (w *sendByWire) CreatePool(ctx context.Context, id, comment string) error {
	if err := w.admit(ctx, http.MethodPost, "/pools"); err != nil {
		return err
	}
	return w.PoolService.CreatePool(ctx, id, comment)
}

func (w *sendByWire) DeletePool(ctx context.Context, id string) error {
	if err := w.admit(ctx, http.MethodDelete, "/pools"); err != nil {
		return err
	}
	err := w.PoolService.DeletePool(ctx, id)
	if w.afterDelete != nil {
		w.afterDelete()
	}
	return err
}

func (w *sendByWire) GetPoolComment(ctx context.Context, id string) (string, bool, error) {
	if err := w.admit(ctx, http.MethodGet, "/pools/"+id); err != nil {
		return "", false, err
	}
	return w.PoolService.GetPoolComment(ctx, id)
}

func (w *sendByWire) recorded() []sendByCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sendByCall(nil), w.calls...)
}

// deletes returns the recorded deletes.
func (w *sendByWire) deletes() []sendByCall {
	var out []sendByCall
	for _, call := range w.recorded() {
		if call.method == http.MethodDelete {
			out = append(out, call)
		}
	}
	return out
}

// assertNoMarker fails the test when any recorded call carried a send-by
// marker or was refused for one.
func assertNoMarker(t *testing.T, w *sendByWire) {
	t.Helper()
	for _, call := range w.recorded() {
		if call.guarded || call.steal || call.refused {
			t.Fatalf("%s %s carried a send-by marker (refused=%v); calls %+v", call.method, call.route, call.refused, w.recorded())
		}
	}
}

// assertStealDeadline fails the test unless deadline is what a steal's marker
// set between the real times before and after carries, with left of the budget
// remaining on the lock clock. The deadline is in real time whatever the lock
// clock reads.
func assertStealDeadline(t *testing.T, deadline, before, after time.Time, left time.Duration) {
	t.Helper()
	if deadline.Before(before.Add(left)) || deadline.After(after.Add(left)) {
		t.Fatalf("the send-by deadline is %v, want real time plus %s, between %v and %v",
			deadline, left, before.Add(left), after.Add(left))
	}
}

// TestSteal_SendByPassedAbandonsTheSteal refuses the steal's delete for its
// send-by deadline. Nothing reached PVE, so the steal is abandoned like one
// whose claim changed. It returns a *lockStealNotSent that wraps the refusal,
// rather than reaching holderUnknown and ErrClusterLockStateUnknown, the
// expired claim still stands, and no create follows.
func TestSteal_SendByPassedAbandonsTheSteal(t *testing.T) {
	start := time.Unix(10_000, 0)
	crashed := encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	f := newFakeLockPools()
	f.pools[stealPool] = crashed
	clk := fixedClock(start, time.Second)
	// The delete arrives no earlier than its deadline, which counts as passed.
	wire := &sendByWire{PoolService: f, now: func() time.Time { return time.Now().Add(clusterLockStealBudget) }}

	before := time.Now()
	h, err := tryStealExpired(context.Background(), wire, stealPool, "S@h/2-b-1", time.Minute,
		start.Add(time.Minute), clusterLockSettings{}, clk)
	after := time.Now()
	var notSent *lockStealNotSent
	if h != nil || !errors.As(err, &notSent) || !errors.Is(err, endpointpool.ErrSendByPassed) {
		t.Fatalf("tryStealExpired = (%v, %v), want the refused steal for a delete refused by its send-by deadline", h, err)
	}
	if errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("a refused steal delete left the holder unknown: %v", err)
	}
	deletes := wire.deletes()
	if len(deletes) != 1 || !deletes[0].refused {
		t.Fatalf("want one refused delete, got %+v", deletes)
	}
	assertStealDeadline(t, deletes[0].deadline, before, after, clusterLockStealBudget)
	if f.deleteN != 0 || f.createN != 0 {
		t.Fatalf("the refused steal reached the store: deletes=%d creates=%d", f.deleteN, f.createN)
	}
	if got := f.pools[stealPool]; got != crashed {
		t.Fatalf("the expired claim was disturbed: %q", got)
	}
}

// TestSteal_SendByMarksOnlyTheDelete runs a whole steal of an expired claim
// under the create grace and then releases on a detached context. Only the
// steal's delete carries the send-by marker, with a deadline of the re-read's
// start plus the steal budget. The reads before it, the recreate, the
// confirming reads, and the release carry none, so none of them can be refused
// once the clock has moved past that deadline.
func TestSteal_SendByMarksOnlyTheDelete(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	f := newFakeLockPools()
	f.pools[stealPool] = encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	clock := &sharedClock{t: start}
	clk := clock.lockClock()
	var late time.Duration
	wire := &sendByWire{PoolService: f, now: func() time.Time { return time.Now().Add(late) }}
	// Once the steal's delete is in, every later call runs long after its
	// deadline.
	wire.afterDelete = func() { late = time.Hour }

	ctx := context.Background()
	before := time.Now()
	h, err := acquireClusterLockWithClock(ctx, wire, "vm-90000", "S@h/2-b-1", 3*time.Minute, time.Minute, clk, WithCreateGrace())
	after := time.Now()
	if err != nil || h == nil {
		t.Fatalf("steal under grace: handle %v, err %v", h, err)
	}
	wire.afterDelete = nil
	if err := h.Release(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("release on a detached context: %v", err)
	}

	calls := wire.recorded()
	var marked []sendByCall
	for _, call := range calls {
		if call.refused {
			t.Fatalf("%s %s was refused; calls %+v", call.method, call.route, calls)
		}
		if call.guarded || call.steal {
			marked = append(marked, call)
		}
	}
	if len(marked) != 1 || marked[0].method != http.MethodDelete {
		t.Fatalf("want only the steal's delete marked, got %+v of %+v", marked, calls)
	}
	assertStealDeadline(t, marked[0].deadline, before, after, clusterLockStealBudget)
	deletes := wire.deletes()
	if len(deletes) != 2 || deletes[1].guarded {
		t.Fatalf("want the steal's delete and an unmarked release, got %+v", deletes)
	}
	if _, held := f.pools[stealPool]; held {
		t.Fatal("the release did not delete the sentinel")
	}
}

// TestSteal_SendByRefusalWaitsAPollAndStealsAgain has the first steal's delete
// refused for its deadline. The acquire creates nothing on that steal, waits a
// poll, reads the claim afresh, and steals it on the next attempt, whose
// delete goes out in time.
func TestSteal_SendByRefusalWaitsAPollAndStealsAgain(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	f := newFakeLockPools()
	f.pools[stealPool] = encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	clock := &sharedClock{t: start}
	clk := clock.lockClock()
	wire := &sendByWire{PoolService: f}
	wire.now = func() time.Time {
		if len(wire.deletes()) == 0 {
			// The first steal's delete arrives a second after its budget.
			return time.Now().Add(clusterLockStealBudget + time.Second)
		}
		return time.Now()
	}
	clock.onSleep = func(string, time.Duration) {
		wire.mu.Lock()
		wire.calls = append(wire.calls, sendByCall{method: "sleep"})
		wire.mu.Unlock()
	}

	h, err := acquireClusterLockWithClock(context.Background(), wire, "vm-90000", "S@h/2-b-1",
		3*time.Minute, time.Minute, clk, WithCreateGrace())
	if err != nil || h == nil {
		t.Fatalf("the acquire did not steal on its next attempt: handle %v, err %v", h, err)
	}
	deletes := wire.deletes()
	if len(deletes) != 2 || !deletes[0].refused || deletes[1].refused {
		t.Fatalf("want a refused steal delete and then one that went out, got %+v", deletes)
	}
	calls := wire.recorded()
	for i, call := range calls {
		if call.method != http.MethodDelete || !call.refused {
			continue
		}
		if i+1 >= len(calls) || calls[i+1].method != "sleep" {
			t.Fatalf("the refused steal did not wait a poll before its next call; calls %+v", calls)
		}
		break
	}
	if !strings.Contains(f.pools[stealPool], "owner=S@h/2-b-1 ") {
		t.Fatalf("the second steal did not take the lock: %q", f.pools[stealPool])
	}
}

// TestSteal_EverySendByRefusalEndsInATimeout keeps refusing the steal's
// delete. Each attempt is abandoned and the claim left alone, so the acquire
// runs out its wait with ErrClusterLockTimeout and never reports the holder
// as unknown. The timeout carries the last refusal and says the expired
// holder's steal could not be sent, rather than reading like a live holder.
func TestSteal_EverySendByRefusalEndsInATimeout(t *testing.T) {
	start := time.Unix(10_000, 0)
	crashed := encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	f := newFakeLockPools()
	f.pools[stealPool] = crashed
	clk := fixedClock(start, time.Second)
	wire := &sendByWire{PoolService: f, now: func() time.Time { return time.Now().Add(time.Hour) }}

	h, err := acquireClusterLockWithClock(context.Background(), wire, "vm-90000", "S@h/2-b-1", time.Minute, 5*time.Second, clk)
	if h != nil {
		t.Fatal("an acquire whose every steal delete was refused took the lock")
	}
	if !errors.Is(err, ErrClusterLockTimeout) || errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want ErrClusterLockTimeout without ErrClusterLockStateUnknown, got %v", err)
	}
	if !errors.Is(err, endpointpool.ErrSendByPassed) || errors.Is(err, ErrClusterLockHeld) {
		t.Fatalf("want the timeout to carry the steal's refusal and not ErrClusterLockHeld, got %v", err)
	}
	if !strings.Contains(err.Error(), "the expired holder's steal could not be sent before its send-by deadline") ||
		strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the timeout does not say the expired holder's steal could not be sent: %v", err)
	}
	deletes := wire.deletes()
	if len(deletes) < 2 {
		t.Fatalf("want the steal retried on every poll, got %+v", deletes)
	}
	for _, d := range deletes {
		if !d.refused {
			t.Fatalf("a delete went out past its deadline: %+v", deletes)
		}
	}
	if f.deleteN != 0 || f.pools[stealPool] != crashed {
		t.Fatalf("a refused delete reached the store: deletes=%d claim=%q", f.deleteN, f.pools[stealPool])
	}
}

// TestNoWaitAcquire_CarriesNoMarker covers the try-once acquire. Against a
// live holder it gives up after one look, and nothing it sends carries a
// marker, so a clock far past any deadline refuses nothing. Against an
// expired holder it steals as usual, and only the steal's delete is marked.
func TestNoWaitAcquire_CarriesNoMarker(t *testing.T) {
	t.Run("live holder", func(t *testing.T) {
		f := newFakeLockPools()
		f.pools["bosh-lock-web"] = encodeLockComment("other-owner", time.Unix(100_000, 0))
		clk := fixedClock(time.Unix(1000, 0), time.Second)
		wire := &sendByWire{PoolService: f, now: func() time.Time { return clk.now().Add(time.Hour) }}
		_, err := acquireClusterLockWithClock(context.Background(), wire, "web", "me", time.Minute, 30*time.Second, clk, WithoutWait())
		if !errors.Is(err, ErrClusterLockHeld) || !errors.Is(err, ErrClusterLockTimeout) {
			t.Fatalf("want ErrClusterLockHeld joined with ErrClusterLockTimeout, got %v", err)
		}
		assertNoMarker(t, wire)
	})
	t.Run("expired holder", func(t *testing.T) {
		f := newFakeLockPools()
		f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))
		clk := fixedClock(time.Unix(1000, 0), time.Second)
		wire := &sendByWire{PoolService: f, now: time.Now}
		h, err := acquireClusterLockWithClock(context.Background(), wire, "web", "me", time.Minute, 30*time.Second, clk, WithoutWait())
		if err != nil || h == nil {
			t.Fatalf("no-wait steal: handle %v, err %v", h, err)
		}
		for _, call := range wire.recorded() {
			if (call.guarded || call.steal) != (call.method == http.MethodDelete) || call.refused {
				t.Fatalf("want only the steal's delete marked and nothing refused, got %+v", wire.recorded())
			}
		}
	})
	t.Run("expired holder whose steal is refused", func(t *testing.T) {
		f := newFakeLockPools()
		dead := encodeLockComment("dead-owner", time.Unix(500, 0))
		f.pools["bosh-lock-web"] = dead
		clk := fixedClock(time.Unix(1000, 0), time.Second)
		wire := &sendByWire{PoolService: f, now: func() time.Time { return time.Now().Add(time.Hour) }}
		h, err := acquireClusterLockWithClock(context.Background(), wire, "web", "me", time.Minute, 30*time.Second, clk, WithoutWait())
		if h != nil {
			t.Fatal("a no-wait acquire whose steal was refused took the lock")
		}
		if !errors.Is(err, ErrClusterLockHeld) || !errors.Is(err, ErrClusterLockTimeout) ||
			!errors.Is(err, endpointpool.ErrSendByPassed) || errors.Is(err, ErrClusterLockStateUnknown) {
			t.Fatalf("want ErrClusterLockHeld and ErrClusterLockTimeout with the refusal, got %v", err)
		}
		text := err.Error()
		if !strings.Contains(text, "the expired holder's steal could not be sent before its send-by deadline") ||
			strings.Contains(text, ErrClusterLockHeld.Error()) || strings.Contains(text, "held by another process") {
			t.Fatalf("the no-wait error reads like a live holder: %v", err)
		}
		if deletes := wire.deletes(); len(deletes) != 1 || !deletes[0].refused {
			t.Fatalf("want one refused steal delete, got %+v", deletes)
		}
		if f.deleteN != 0 || f.createN != 1 || f.pools["bosh-lock-web"] != dead {
			t.Fatalf("the refused steal changed the store: deletes=%d creates=%d claim=%q",
				f.deleteN, f.createN, f.pools["bosh-lock-web"])
		}
	})
}

// TestWithLockStealSendBy_DeadlineIsInRealTime sets the steal's marker under
// lock clocks far in the past and far in the future, and under one that has
// run part of the budget. Each deadline is what is left of the budget on the
// lock clock, added to the real time, so a transport that compares with
// time.Now neither refuses it at once nor lets it through after the budget.
func TestWithLockStealSendBy_DeadlineIsInRealTime(t *testing.T) {
	path := endpointpool.APIPrefix + lockSentinelDeletePath
	for name, tc := range map[string]struct {
		start   time.Time
		elapsed time.Duration
	}{
		"far in the past":        {start: time.Unix(10_000, 0)},
		"far in the future":      {start: time.Now().Add(100 * 365 * 24 * time.Hour)},
		"part of the budget run": {start: time.Unix(10_000, 0), elapsed: 1500 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			at := tc.start.Add(tc.elapsed)
			clk := lockClock{now: func() time.Time { return at }}
			left := clusterLockStealBudget - tc.elapsed
			before := time.Now()
			ctx := withLockStealSendBy(context.Background(), clk, tc.start)
			after := time.Now()
			deadline, ok := endpointpool.SendBy(ctx, http.MethodDelete, path)
			if !ok {
				t.Fatal("the steal's marker does not name DELETE /pools")
			}
			assertStealDeadline(t, deadline, before, after, left)
			if err := endpointpool.CheckSendBy(ctx, time.Now(), http.MethodDelete, path); err != nil {
				t.Fatalf("a transport on real time refused the steal at once: %v", err)
			}
			if err := endpointpool.CheckSendBy(ctx, time.Now().Add(left), http.MethodDelete, path); !errors.Is(err, endpointpool.ErrSendByPassed) {
				t.Fatalf("a transport on real time sent the steal after its budget: %v", err)
			}
		})
	}
}

// TestCreateUnderGrace_CarriesNoMarker takes a free lock with the create grace
// and releases it on a detached context, with a clock far past any deadline.
// Neither the create, its confirming reads, nor the release carries a marker,
// so none is refused.
func TestCreateUnderGrace_CarriesNoMarker(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	f := newFakeLockPools()
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	wire := &sendByWire{PoolService: f, now: func() time.Time { return clk.now().Add(time.Hour) }}
	ctx := context.Background()
	h, err := acquireClusterLockWithClock(ctx, wire, "web", "me", time.Minute, 30*time.Second, clk, WithCreateGrace())
	if err != nil || h == nil {
		t.Fatalf("create under grace: handle %v, err %v", h, err)
	}
	if err := h.Release(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertNoMarker(t, wire)
	if len(wire.deletes()) != 1 || len(f.pools) != 0 {
		t.Fatalf("the release did not delete the sentinel: deletes %+v pools %v", wire.deletes(), f.pools)
	}
}

// TestAbandonAfterSteal_CarriesNoMarker has a steal's recreate fail with an
// outcome it cannot know, after PVE did create the sentinel. The acquire
// abandons the claim it proves is its own, on a detached context derived from
// the acquire's. That delete removes our own claim, so it carries no marker
// and is not refused although the steal's deadline has long passed.
func TestAbandonAfterSteal_CarriesNoMarker(t *testing.T) {
	start := time.Unix(10_000, 0)
	f := newFakeLockPools()
	f.pools[stealPool] = encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	f.createFn = func(id, comment string) error {
		if f.deleteN == 0 {
			// The first create meets the expired claim as usual.
			return nil
		}
		// The steal's recreate: PVE applied it, and its answer was lost.
		f.pools[id] = comment
		return errors.New("connection reset by peer")
	}
	clk := fixedClock(start, time.Second)
	var late time.Duration
	wire := &sendByWire{PoolService: f, now: func() time.Time { return clk.now().Add(late) }}
	wire.afterDelete = func() { late = time.Hour }

	h, err := acquireClusterLockWithClock(context.Background(), wire, "vm-90000", "S@h/2-b-1", time.Minute, time.Minute, clk)
	if h != nil || !errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want the unknown recreate to fail the acquire, got handle %v err %v", h, err)
	}
	deletes := wire.deletes()
	if len(deletes) != 2 || !deletes[0].guarded || deletes[1].guarded || deletes[1].steal || deletes[1].refused {
		t.Fatalf("want a marked steal delete and an unmarked abandon, got %+v", deletes)
	}
	if _, held := f.pools[stealPool]; held {
		t.Fatal("the abandon did not remove our claim")
	}
}
