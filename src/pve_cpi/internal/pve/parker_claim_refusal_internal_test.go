package pve

import (
	"context"
	"errors"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestParkerClaim_ConfirmedJustAboveTheWindowReserveIsRefused confirms a
// parker claim with two seconds more than the window's reserve left. That
// would leave the body two seconds, so the acquire refuses it and asks for a
// claim that also leaves the window a working body.
func TestParkerClaim_ConfirmedJustAboveTheWindowReserveIsRefused(t *testing.T) {
	t.Cleanup(SetClusterLockGraceForTest(0))
	ttl := parkerProtectionLockTTLNow()
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	expiry := claimExpiry(clk.now(), ttl)
	answersFrom := expiry.Add(-(parkerWindowReserveNow() + 2*time.Second))
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "vm-90000", "me", ttl, ttl, clk,
		parkerLockOptions(ttl)...)
	if h != nil {
		t.Fatalf("the acquire returned a handle with %v left, which leaves its body %v",
			h.expiry.Sub(clk.now()), h.expiry.Sub(clk.now())-parkerWindowReserveNow())
	}
	if !errors.Is(err, ErrClusterLockClaimTooShort) {
		t.Fatalf("want ErrClusterLockClaimTooShort, got %v", err)
	}
}

// TestConfirm_GivenUpClaimIsRetakenWhileTheWaitLasts gives up a claim that
// was confirmed too late, deletes it, and then takes the lock with a fresh
// claim, because the acquire's wait still has room for one.
func TestConfirm_GivenUpClaimIsRetakenWhileTheWaitLasts(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(20 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 5*time.Minute, clk,
		WithClaimReserve(50*time.Second))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if left := h.expiry.Sub(clk.now()); left <= 50*time.Second {
		t.Fatalf("the fresh claim has %v left, want more than the 50s reserve", left)
	}
	if f.createN != 2 || f.deleteN != 1 {
		t.Fatalf("creates = %d and deletes = %d, want the late claim deleted once and one fresh create",
			f.createN, f.deleteN)
	}
}

// TestConfirm_ClaimLeftStandingIsNotRetaken refuses a claim confirmed inside
// its release margin. The acquire cannot delete a claim that a steal may
// already have replaced, so it leaves it standing and does not create again,
// even though its wait has room for another claim.
func TestConfirm_ClaimLeftStandingIsNotRetaken(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(8 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", 10*time.Second, time.Minute, clk)
	if h != nil || !errors.Is(err, ErrClusterLockClaimTooShort) {
		t.Fatalf("want ErrClusterLockClaimTooShort and no handle, got %v", err)
	}
	if f.createN != 1 || f.deleteN != 0 {
		t.Fatalf("creates = %d and deletes = %d, want the one claim left standing", f.createN, f.deleteN)
	}
}

// stallParkerConfirm makes the confirming reads of a parker claim created at
// clk's current time fail until the claim has 5 seconds less than the
// window's claim reserve left, which is too late to hand it back.
func stallParkerConfirm(f *fakeLockPools, clk lockClock, ttl time.Duration) {
	need := parkerWindowReserveNow() + parkerMinimumWindowBody
	answersFrom := claimExpiry(clk.now(), ttl).Add(-(need - 5*time.Second))
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
}

// wallAlignedLockClock returns a test clock that starts at the wall clock's
// current whole second. A request deadline set on a real context then means
// the same to the acquire as it does in production, while the clock still
// moves only when the acquire sleeps.
func wallAlignedLockClock() (lockClock, time.Time) {
	start := time.Now().Truncate(time.Second)
	clk, _ := sleepLog(start)
	return clk, start
}

// TestParkerWindow_StalledConfirmAtProductionTTLNeverRunsTheBody runs a
// protection window at the production TTL, waiting a whole TTL for the lock,
// while the reads that confirm its claim fail until the claim can no longer
// hold the window's reserve and a working body. The window must not run, the
// error must say the claim was confirmed too late, and the claim must be
// deleted rather than left to block the parker. The acquire does not create
// a second claim in either case. A call without a time limit has only its
// wait, and a wait of one TTL never leaves a fresh claim its claim reserve. A
// call whose time limit ends 200 seconds after it began would have to use the
// fresh claim past that limit.
func TestParkerWindow_StalledConfirmAtProductionTTLNeverRunsTheBody(t *testing.T) {
	cases := []struct {
		name  string
		limit time.Duration
	}{
		{name: "no time limit"},
		{name: "time limit too close", limit: 200 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ttl := parkerProtectionLockTTLNow()
			f := newFakeLockPools()
			clk, start := wallAlignedLockClock()
			stallParkerConfirm(f, clk, ttl)
			ctx := t.Context()
			if tc.limit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, start.Add(tc.limit))
				defer cancel()
			}
			ctx = withTestParkerLockClock(WithParkerLockWait(ctx, ttl), clk)
			ran := 0
			err := withParkerProtectionLock(ctx, &parkerLockClient{pools: f}, nil, 90000, "transfer",
				func(context.Context) error {
					ran++
					return nil
				})
			if ran != 0 {
				t.Fatalf("the window body ran %d times on a claim confirmed too late to hold its reserve", ran)
			}
			if !errors.Is(err, ErrClusterLockClaimTooShort) {
				t.Fatalf("want ErrClusterLockClaimTooShort, got %v", err)
			}
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("the refusal must be retriable: %v", err)
			}
			if f.createN != 1 || f.deleteN != 1 {
				t.Fatalf("creates = %d and deletes = %d, want the one late claim deleted", f.createN, f.deleteN)
			}
			if _, standing := f.pools["bosh-lock-vm-90000"]; standing {
				t.Fatal("the refused claim was left standing on the parker")
			}
		})
	}
}

// TestParkerWindow_StalledConfirmAtProductionTTLIsRetakenInsideTheCallsTimeLimit
// runs the managed path at production settings. The wait and the TTL are both
// the shipped 235 seconds, and the call runs under the 600-second time limit
// pve.operation_timeout gives attach_disk. The reads that confirm the first
// claim fail until it can no longer hold the window's reserve and a working
// body. The acquire deletes that claim, takes a fresh one at once because the
// call's time limit leaves it the claim reserve, and runs the window body
// exactly once on the fresh claim.
func TestParkerWindow_StalledConfirmAtProductionTTLIsRetakenInsideTheCallsTimeLimit(t *testing.T) {
	ttl := parkerProtectionLockTTLNow()
	if ttl != 235*time.Second {
		t.Fatalf("the parker lock TTL on the shipped curves is %v, want 235s", ttl)
	}
	f := newFakeLockPools()
	clk, start := wallAlignedLockClock()
	stallParkerConfirm(f, clk, ttl)
	ctx, cancel := context.WithDeadline(t.Context(), start.Add(600*time.Second))
	defer cancel()
	ctx = withTestParkerLockClock(WithParkerLockWait(ctx, ttl), clk)
	ran := 0
	var left time.Duration
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: f}, nil, 90000, "transfer",
		func(context.Context) error {
			ran++
			if exp, ok := decodeLockExpiry(f.pools["bosh-lock-vm-90000"]); ok {
				left = exp.Sub(clk.now())
			}
			return nil
		})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if ran != 1 {
		t.Fatalf("the window body ran %d times, want once on the fresh claim", ran)
	}
	if f.createN != 2 || f.deleteN != 2 {
		t.Fatalf("creates = %d and deletes = %d, want the late claim given up, one fresh claim, and its release",
			f.createN, f.deleteN)
	}
	if need := parkerWindowReserveNow() + parkerMinimumWindowBody; left < need {
		t.Fatalf("the body ran on a claim with %v left, less than the %v the window needs", left, need)
	}
	if _, standing := f.pools["bosh-lock-vm-90000"]; standing {
		t.Fatal("the fresh claim was not released")
	}
}

// TestParkerWindow_StalledConfirmIsRetakenWhenTheWaitHasRoom stalls the
// confirming reads the same way, under a wait of two TTLs. The acquire gives
// up and deletes the late claim, takes a fresh one at once, and runs the
// window body exactly once on it, with the window's whole claim reserve left.
func TestParkerWindow_StalledConfirmIsRetakenWhenTheWaitHasRoom(t *testing.T) {
	ttl := parkerProtectionLockTTLNow()
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	stallParkerConfirm(f, clk, ttl)
	ctx := withTestParkerLockClock(WithParkerLockWait(context.Background(), 2*ttl), clk)
	ran := 0
	var left time.Duration
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: f}, nil, 90000, "transfer",
		func(context.Context) error {
			ran++
			if exp, ok := decodeLockExpiry(f.pools["bosh-lock-vm-90000"]); ok {
				left = exp.Sub(clk.now())
			}
			return nil
		})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if ran != 1 {
		t.Fatalf("the window body ran %d times, want once on the fresh claim", ran)
	}
	if f.createN != 2 {
		t.Fatalf("creates = %d, want the late claim and one fresh claim", f.createN)
	}
	if f.deleteN != 2 {
		t.Fatalf("deletes = %d, want the late claim given up and the fresh claim released", f.deleteN)
	}
	if need := parkerWindowReserveNow() + parkerMinimumWindowBody; left < need {
		t.Fatalf("the body ran on a claim with %v left, less than the %v the window needs", left, need)
	}
}
