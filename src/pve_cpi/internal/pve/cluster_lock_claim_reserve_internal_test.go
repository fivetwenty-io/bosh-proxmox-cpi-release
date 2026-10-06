package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestConfirm_LateConfirmationShortOfTheClaimReserveIsGivenUp covers a
// confirmation that lands well before the claim's release margin but with
// less time left than the caller's work needs. The acquire refuses the
// handle, deletes its own claim, because a steal cannot have replaced a claim
// that far from expiry, and returns a retriable error that names the cause.
func TestConfirm_LateConfirmationShortOfTheClaimReserveIsGivenUp(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(20 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, time.Minute, clk,
		WithClaimReserve(50*time.Second))
	if h != nil {
		t.Fatalf("the acquire returned a handle with %v left on its claim, under the 50s reserve", h.expiry.Sub(clk.now()))
	}
	if !errors.Is(err, ErrClusterLockClaimTooShort) {
		t.Fatalf("want ErrClusterLockClaimTooShort, got %v", err)
	}
	if errors.Is(err, ErrClusterLockStateUnknown) || errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("a confirmed claim was reported as unknown or timed out: %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable refusal, got %v", err)
	}
	for _, want := range []string{`confirmed lock "bosh-lock-web" too late to use it`, "less than the 50s", "a retry takes the lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal %q does not contain %q", err, want)
		}
	}
	if f.deleteN != 1 {
		t.Fatalf("deletes = %d, want our claim given up with one delete", f.deleteN)
	}
	if _, ok := f.pools["bosh-lock-web"]; ok {
		t.Fatalf("the sentinel %q is still standing after the claim was given up", f.pools["bosh-lock-web"])
	}
}

// TestConfirm_ConfirmationWithTheReserveLeftKeepsTheClaim is the passing side
// of the reserve check, where a slow confirmation still leaves more than the
// reserve on the claim.
func TestConfirm_ConfirmationWithTheReserveLeftKeepsTheClaim(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(5 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, time.Minute, clk,
		WithClaimReserve(50*time.Second))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if left := h.expiry.Sub(clk.now()); left <= 50*time.Second {
		t.Fatalf("the handle has %v left, want more than the 50s reserve", left)
	}
	if f.deleteN != 0 {
		t.Fatalf("deletes = %d, want the claim kept", f.deleteN)
	}
}

// TestConfirm_LateConfirmationInsideReleaseMarginNamesTheCause pins the
// sentinel on the release-margin refusal, which handlers classify as a lock
// wait that never entered the lock.
func TestConfirm_LateConfirmationInsideReleaseMarginNamesTheCause(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(8 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	_, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", 10*time.Second, 10*time.Second, clk)
	if !errors.Is(err, ErrClusterLockClaimTooShort) {
		t.Fatalf("want ErrClusterLockClaimTooShort, got %v", err)
	}
}

func TestWithClaimReserve_IgnoresNonPositiveValues(t *testing.T) {
	for _, reserve := range []time.Duration{0, -time.Second} {
		var s clusterLockSettings
		WithClaimReserve(reserve)(&s)
		if s.claimReserve != 0 {
			t.Fatalf("WithClaimReserve(%v) set the reserve to %v", reserve, s.claimReserve)
		}
		if got := s.claimNeed(); got != s.releaseMargin() {
			t.Fatalf("claimNeed with no reserve = %v, want the release margin %v", got, s.releaseMargin())
		}
	}
}

// TestParkerLockOptions_ReserveOnlyWhenTheTTLHoldsIt checks that the parker
// window asks the confirmation for its whole reserve and a minimum body at a
// real TTL, and that a test-sized TTL, which the window floor already
// handles, keeps only the release margin.
func TestParkerLockOptions_ReserveOnlyWhenTheTTLHoldsIt(t *testing.T) {
	settingsFor := func(ttl time.Duration) clusterLockSettings {
		var s clusterLockSettings
		for _, opt := range parkerLockOptions(ttl) {
			opt(&s)
		}
		return s
	}
	full := settingsFor(parkerProtectionLockTTLNow())
	if !full.grace {
		t.Fatal("the parker window acquire lost its create grace")
	}
	if want := parkerWindowReserveNow() + parkerMinimumWindowBody; full.claimReserve != want {
		t.Fatalf("claim reserve = %v, want the window reserve and a minimum body, %v", full.claimReserve, want)
	}
	small := settingsFor(10 * time.Second)
	if !small.grace || small.claimReserve != 0 {
		t.Fatalf("a 10s TTL got grace=%v reserve=%v, want grace and no reserve", small.grace, small.claimReserve)
	}
}
