package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestAcquireClusterLock_WithoutWaitGivesUpOnALiveHolder pins the try-once
// acquire: a live holder ends it after one look, with no poll sleep, and the
// error carries both ErrClusterLockHeld and ErrClusterLockTimeout.
func TestAcquireClusterLock_WithoutWaitGivesUpOnALiveHolder(t *testing.T) {
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("other-owner", time.Unix(100000, 0))
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	slept := 0
	inner := clk.sleep
	clk.sleep = func(ctx context.Context, d time.Duration) error {
		slept++
		return inner(ctx, d)
	}

	_, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk, WithoutWait())
	if err == nil {
		t.Fatal("want an error for a lock a live owner holds")
	}
	if !errors.Is(err, ErrClusterLockHeld) || !errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("want ErrClusterLockHeld joined with ErrClusterLockTimeout, got %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("the error must be retriable, got %v", err)
	}
	if slept != 0 {
		t.Errorf("a try-once acquire must not poll; it slept %d times", slept)
	}
	if got := f.pools["bosh-lock-web"]; !strings.Contains(got, "owner=other-owner") {
		t.Errorf("the live holder's claim must stay; got %q", got)
	}
}

// TestAcquireClusterLock_WithoutWaitStillStealsAnExpiredHolder shows the
// option changes only the wait: an expired claim is stolen as before.
func TestAcquireClusterLock_WithoutWaitStillStealsAnExpiredHolder(t *testing.T) {
	f := newFakeLockPools()
	f.pools["bosh-lock-web"] = encodeLockComment("dead-owner", time.Unix(500, 0))
	clk := fixedClock(time.Unix(1000, 0), time.Second)

	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk, WithoutWait())
	if err != nil {
		t.Fatalf("steal acquire: %v", err)
	}
	if h == nil || !strings.Contains(f.pools["bosh-lock-web"], "owner=me") {
		t.Fatalf("want the expired claim stolen; pool comment %q", f.pools["bosh-lock-web"])
	}
}

// TestAcquireClusterLock_WithoutWaitTakesAFreeLock shows a free lock is taken
// the ordinary way.
func TestAcquireClusterLock_WithoutWaitTakesAFreeLock(t *testing.T) {
	f := newFakeLockPools()
	clk := fixedClock(time.Unix(1000, 0), time.Second)

	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me",
		60*time.Second, 30*time.Second, clk, WithoutWait())
	if err != nil || h == nil {
		t.Fatalf("acquire of a free lock: handle %v, err %v", h, err)
	}
}
