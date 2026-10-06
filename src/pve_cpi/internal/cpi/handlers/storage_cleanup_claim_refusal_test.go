package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestStorageCleanupDiagnostic_NamesAClaimGivenUpBeforeTheMove checks that a
// cleanup whose parker lock acquire gave up a late claim says the lock was
// not taken, rather than that a wait ran out, and that a wait that did run
// out keeps its own text.
func TestStorageCleanupDiagnostic_NamesAClaimGivenUpBeforeTheMove(t *testing.T) {
	refused := &diskReturnedAfterLockTimeout{err: cpierrors.WrapAs(
		fmt.Errorf("attach: %w", pve.ErrClusterLockClaimTooShort),
		cpierrors.TypeRetriableCloud, "AcquireClusterLock: confirmed lock too late to use it")}
	got := StorageAllocationDecisionFailure(&storageCleanupStageError{stage: "resource_cleanup", cause: refused})
	if !strings.Contains(got, "a parker lock was not taken before the disk moved") {
		t.Fatalf("diagnostic = %q, want it to say the lock was not taken", got)
	}
	if strings.Contains(got, "ran out") {
		t.Fatalf("diagnostic = %q says a wait ran out, which is not what happened", got)
	}
	timedOut := &diskReturnedAfterLockTimeout{err: cpierrors.WrapAs(fmt.Errorf("attach: %w", pve.ErrClusterLockTimeout),
		cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")}
	if got := StorageAllocationDecisionFailure(timedOut); !strings.Contains(got, "a parker lock wait ran out") {
		t.Fatalf("diagnostic for a wait that ran out = %q", got)
	}
}

// testLockClock is a clock for the parker lock's acquire that moves only when
// the acquire sleeps. It starts at the wall clock's current whole second, so a
// production-sized TTL and wait run without being spent in wall-clock time.
type testLockClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newTestLockClock() *testLockClock {
	return &testLockClock{cur: time.Now().Truncate(time.Second)}
}

func (c *testLockClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *testLockClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
	return nil
}

// on returns ctx with the parker lock's acquires running on c.
func (c *testLockClock) on(ctx context.Context) context.Context {
	return pve.WithParkerLockClockForTest(ctx, c.now, c.sleep)
}

// failConfirmingReadsUntilLeft makes the parker lock's confirming reads fail
// after each sentinel create until clk shows only left before the claim's
// recorded expiry. The read on the acquire's way out, and every read once
// that point is reached, answer.
func failConfirmingReadsUntilLeft(t *testing.T, locks *lockContention, clk *testLockClock, left time.Duration) {
	t.Helper()
	var answersFrom time.Time
	locks.afterCreate = func(pool string) {
		expiry, ok := claimExpiryOf(locks.pools[pool])
		if !ok {
			t.Errorf("sentinel %q carries no expiry: %q", pool, locks.pools[pool])
			return
		}
		answersFrom = expiry.Add(-left)
	}
	locks.plainRead = func(context.Context, string) error {
		if answersFrom.IsZero() || readOnTheWayOut() || !clk.now().Before(answersFrom) {
			return nil
		}
		return errors.New("connection reset by peer")
	}
}

// claimExpiryOf returns the expiry a sentinel's claim records.
func claimExpiryOf(comment string) (time.Time, bool) {
	for field := range strings.FieldsSeq(comment) {
		if v, ok := strings.CutPrefix(field, "exp="); ok {
			unix, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(unix, 0), true
		}
	}
	return time.Time{}, false
}

// TestManagedAttachRefusedLockClaimReturnsTheAllocation covers an attach whose
// parker lock create landed but whose confirming reads failed until too
// little of its claim was left for the protection window. It runs at the
// production TTL and wait on a test clock. The acquire refuses the claim as
// confirmed too late, so the attach never entered the window and nothing the
// allocation owns changed. The allocation goes back to ready_to_return, the
// error is retriable, and the retry completes.
//
// A claim confirmed with a minute left is outside the release margin, so the
// acquire deletes it. The request has no time limit, and a wait of one TTL
// never leaves a fresh claim the window's reserve, so it does not take a
// second claim. A claim confirmed with two seconds left is inside the release
// margin, so it is left standing for its TTL steal, which the retry makes.
func TestManagedAttachRefusedLockClaimReturnsTheAllocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		left      time.Duration
		sentinels int
	}{
		{name: "claim released", left: time.Minute, sentinels: 0},
		{name: "claim left standing", left: 2 * time.Second, sentinels: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			locks := newLockContention(t)
			disk := newParkedFlowDisk(t, locks)
			moves := disk.client.moves
			locks.reset()
			clk := newTestLockClock()
			failConfirmingReadsUntilLeft(t, locks, clk, tc.left)
			ctx := clk.on(t.Context())

			err := disk.attach(ctx)
			if !errors.Is(err, pve.ErrClusterLockClaimTooShort) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("want the retriable refused claim, got %v", err)
			}
			if !isDiskReturnedAfterLockTimeout(err) {
				t.Fatalf("the refused claim did not hand the disk back: %v", err)
			}
			if disk.client.moves != moves {
				t.Fatal("the attach moved the disk without holding the parker lock")
			}
			assertReturnedRecord(t, "refused-claim", disk.record(t))
			locks.mu.Lock()
			creates, sentinels := locks.creates, len(locks.pools)
			locks.mu.Unlock()
			if creates != 1 {
				t.Fatalf("sentinel creates = %d, want the one refused claim and no fresh claim", creates)
			}
			if sentinels != tc.sentinels {
				t.Fatalf("sentinels = %d after the refusal, want %d", sentinels, tc.sentinels)
			}

			locks.mu.Lock()
			locks.afterCreate, locks.plainRead = nil, nil
			locks.mu.Unlock()
			if err := disk.attach(ctx); err != nil {
				t.Fatalf("the retry after the refused claim failed: %v", err)
			}
			assertReturnedRecord(t, "retried", disk.record(t))
			if sentinels := sentinelCount(locks); sentinels != 0 {
				t.Fatalf("sentinels = %d after the retry, want its claim released", sentinels)
			}
		})
	}
}
