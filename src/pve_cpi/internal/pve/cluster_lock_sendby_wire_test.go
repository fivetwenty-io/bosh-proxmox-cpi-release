package pve

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file send the steal's delete through a client built by
// newClient, over two loopback servers, so a refusal for the send-by deadline
// travels the whole production chain the lock code reads it from: the pool, the
// HTTP client, the SDK's retry loop, and the CPI's pool service. The fake wire
// in cluster_lock_sendby_test.go imitates the SDK's error shape, so it cannot
// tell when an SDK change stops the refusal from reaching errors.Is.

// stealLab is two servers that hold one expired lock sentinel. The lock clock
// reads base until the steal's re-read reaches a server, and base plus the
// steal budget from then on. The delete is then the budget's last instant, and
// its deadline, real time plus what is left of the budget, is already the
// present. No test waits for it.
type stealLab struct {
	t      *testing.T
	base   time.Time
	a, b   *pvetest.Server
	late   atomic.Bool
	reads  atomic.Int32
	client Client
}

func newStealLab(t *testing.T, createConflicts bool) *stealLab {
	t.Helper()
	l := &stealLab{t: t, base: time.Unix(10_000, 0)}
	crashed := encodeLockComment("crashed@h/1-a-1", l.base.Add(-time.Second))
	lab := newTetherLab(t)
	routes := func() *pvetest.Routes {
		r := pvetest.NewRoutes()
		r.HandleFunc(http.MethodGet, "/pools/", func(w http.ResponseWriter, _ *http.Request) {
			// The second read is the steal's re-read. The claim is judged
			// expired on the first, and the clock passes the budget with the
			// second.
			if l.reads.Add(1) >= 2 {
				l.late.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			body := map[string]any{"comment": crashed, "members": []any{}}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": body})
		})
		if createConflicts {
			r.Fail(http.MethodPost, "/pools", http.StatusConflict, "pool 'bosh-lock-vm-90000' already exists")
		}
		return r
	}
	l.a, l.b = lab.live(t, routes()), lab.live(t, routes())
	l.client = lab.client(t, l.a, l.b)
	return l
}

// clock is the lock clock of the test.
func (l *stealLab) clock() lockClock {
	return lockClock{
		now: func() time.Time {
			if l.late.Load() {
				return l.base.Add(clusterLockStealBudget)
			}
			return l.base
		},
		sleep: func(context.Context, time.Duration) error { return nil },
	}
}

// noDeletes fails the test when either server read a DELETE, which is the
// proof that the refused steal sent nothing.
func (l *stealLab) noDeletes() {
	l.t.Helper()
	for name, s := range map[string]*pvetest.Server{"A": l.a, "B": l.b} {
		if n := seenCount(s, http.MethodDelete, "/pools"); n != 0 {
			l.t.Errorf("%s read %d DELETE /pools, want none, because the steal was refused before any dial", name, n)
		}
	}
}

func TestStealRefusedForItsSendByDeadlineThroughTheProductionClient(t *testing.T) {
	lab := newStealLab(t, false)

	h, err := tryStealExpired(context.Background(), lab.client.Pools(), stealPool, "S@h/2-b-1", time.Minute,
		lab.base.Add(time.Minute), clusterLockSettings{}, lab.clock())

	var notSent *lockStealNotSent
	if h != nil || !errors.As(err, &notSent) {
		t.Fatalf("tryStealExpired = (%v, %v), want a *lockStealNotSent", h, err)
	}
	if !errors.Is(err, endpointpool.ErrSendByPassed) {
		t.Fatalf("the refused steal lost endpointpool.ErrSendByPassed on its way through the SDK: %v", err)
	}
	if errors.Is(err, ErrClusterLockStateUnknown) {
		t.Errorf("a steal refused before it was sent left the holder unknown: %v", err)
	}
	if IsTransientTransport(err) {
		t.Errorf("a refusal for the send-by deadline is transient, so a retry helper would replay it: %v", err)
	}
	if got := lab.reads.Load(); got != 2 {
		t.Errorf("the servers read the pool %d times, want the read and the re-read", got)
	}
	lab.noDeletes()
	requireNoMutations(t, "A", lab.a)
	requireNoMutations(t, "B", lab.b)
}

// TestAcquireRefusedStealGivesUpNamingTheExpiredHolder runs the whole acquire
// loop of a create_vm claim, which does not wait, against the same refusal. It
// ends with the held mark and the timeout sentinel, names the expired holder it
// could not replace, and sends no delete.
func TestAcquireRefusedStealGivesUpNamingTheExpiredHolder(t *testing.T) {
	lab := newStealLab(t, true)

	h, err := acquireClusterLockWithClock(context.Background(), lab.client.Pools(), "vm-90000", "S@h/2-b-1",
		time.Minute, time.Minute, lab.clock(), WithoutWait())

	if h != nil || err == nil {
		t.Fatalf("acquire = (%v, %v), want an error for a holder it could not replace", h, err)
	}
	for _, want := range []error{ErrClusterLockTimeout, ErrClusterLockHeld, endpointpool.ErrSendByPassed} {
		if !errors.Is(err, want) {
			t.Errorf("error = %v, want it to wrap %v", err, want)
		}
	}
	if !strings.Contains(err.Error(), "expired holder this acquire could not replace") {
		t.Errorf("error = %v, want it to say the holder expired", err)
	}
	if errors.Is(err, ErrClusterLockStateUnknown) {
		t.Errorf("a refused steal left the holder unknown: %v", err)
	}
	lab.noDeletes()
	if n := seenCount(lab.a, http.MethodPost, "/pools") + seenCount(lab.b, http.MethodPost, "/pools"); n != 1 {
		t.Errorf("the servers read %d POST /pools, want the acquire's one create", n)
	}
}

// TestSendByPassedDuringTheDialIsNotTransient lets the dial run until the
// send-by bound cuts it off. The pool's error then wraps both the refusal and
// the dial failure, which the SDK reports as a ConnectionError. It must not
// count as transient, because the lock code never retries a refusal and a
// retry wrapper added to a steal would replay it through its backoff.
func TestSendByPassedDuringTheDialIsNotTransient(t *testing.T) {
	lab := newTetherLab(t)
	a, b := lab.live(t, cachePoolRoutes()), lab.live(t, cachePoolRoutes())
	// The dial returns when the bound the pool put on its context passes, so
	// the refusal arrives from fail and not from the check before the dial.
	setClientDialForTest(t, func(ctx context.Context, network, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
		case <-t.Context().Done():
			return nil, &net.OpError{Op: "dial", Net: network, Err: t.Context().Err()}
		}
	})
	c := lab.client(t, a, b)
	bound := time.Now().Add(20 * time.Millisecond)
	ctx := endpointpool.WithSendBy(context.Background(), bound, http.MethodGet, "/pools/p1")

	_, _, err := c.Pools().GetPoolComment(ctx, "p1")

	if !errors.Is(err, endpointpool.ErrSendByPassed) {
		t.Fatalf("error = %v, want endpointpool.ErrSendByPassed", err)
	}
	if IsTransientTransport(err) {
		t.Errorf("a send-by refusal during the dial is transient: %v", err)
	}
	for name, s := range map[string]*pvetest.Server{"A": a, "B": b} {
		if n := len(s.Seen()); n != 0 {
			t.Errorf("%s read %d requests, want none", name, n)
		}
	}
}
