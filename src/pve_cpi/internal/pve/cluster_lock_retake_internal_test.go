package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRetake_SkipsAFreshClaimThatCannotFitAfterItsCreate gives up a claim
// whose TTL is exactly the claim reserve plus one claim attempt. Every PVE
// call takes one round trip on the test clock, as it does on a cluster. A
// claim retaken at once would record its expiry in whole seconds before its
// create and be confirmed one attempt later, with less than the reserve left,
// so it would be refused again. The acquire must return the refusal after the
// first claim rather than create claim after claim until its wait runs out.
func TestRetake_SkipsAFreshClaimThatCannotFitAfterItsCreate(t *testing.T) {
	const reserve = 50 * time.Second
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	roundTrip := func() { _ = clk.sleep(context.Background(), clusterLockRoundTrip) }
	f.createFn = func(string, string) error {
		roundTrip()
		return nil
	}
	answersFrom := time.Unix(1002, 0)
	f.getFn = func(poolID string) (string, bool, error, bool) {
		roundTrip()
		if _, exists := f.pools[poolID]; exists && clk.now().Before(answersFrom) {
			return "", false, errors.New("503 pmxcfs read timeout"), true
		}
		return "", false, nil, false
	}
	settings := clusterLockSettings{claimReserve: reserve}
	ttl := reserve + settings.claimAttemptDuration()

	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", ttl, 5*time.Minute, clk,
		WithClaimReserve(reserve))
	if h != nil || !errors.Is(err, ErrClusterLockClaimTooShort) {
		t.Fatalf("want ErrClusterLockClaimTooShort and no handle, got %v", err)
	}
	if f.createN != 1 || f.deleteN != 1 {
		t.Fatalf("creates = %d and deletes = %d, want the one late claim deleted and no fresh claim",
			f.createN, f.deleteN)
	}
}

// TestRetakeShortClaim_Rules checks each test retakeShortClaim applies to a
// refused claim. The settings take no grace, so one claim attempt is a
// create and one read, which is one second, and the claim reserve is 50
// seconds.
func TestRetakeShortClaim_Rules(t *testing.T) {
	const reserve = 50 * time.Second
	settings := clusterLockSettings{claimReserve: reserve}
	if attempt := settings.claimAttemptDuration(); attempt != time.Second {
		t.Fatalf("claim attempt = %v, want 1s", attempt)
	}
	now := time.Unix(1000, 0)
	refused := errors.New("confirmed too late")
	cases := []struct {
		name     string
		start    time.Time
		ttl      time.Duration
		wait     time.Duration
		limit    time.Duration
		released bool
		want     bool
	}{
		{name: "the wait alone leaves the reserve after an attempt", ttl: 2 * time.Minute,
			wait: reserve + time.Second, released: true, want: true},
		{name: "the wait alone leaves less than the reserve after an attempt", ttl: 2 * time.Minute,
			wait: reserve + time.Second - time.Millisecond, released: true},
		{name: "the time limit leaves the reserve though the wait ends sooner", ttl: 2 * time.Minute,
			wait: 10 * time.Second, limit: reserve + time.Second + clusterLockContextMargin, released: true, want: true},
		{name: "the time limit leaves less than the reserve", ttl: 2 * time.Minute,
			wait: 10 * time.Second, limit: reserve + time.Second + clusterLockContextMargin - time.Millisecond,
			released: true},
		{name: "an attempt fits exactly before the wait ends", ttl: 2 * time.Minute,
			wait: time.Second, limit: time.Hour, released: true, want: true},
		{name: "an attempt would run past the end of the wait", ttl: 2 * time.Minute,
			wait: time.Second - time.Millisecond, limit: time.Hour, released: true},
		{name: "the refused claim was left standing", ttl: 2 * time.Minute,
			wait: time.Hour, limit: time.Hour},
		{name: "a fresh claim fits once its attempt is over", start: now.Add(300 * time.Millisecond),
			ttl: reserve + 2*time.Second, wait: time.Hour, released: true, want: true},
		{name: "a fresh claim no longer fits once its attempt is over and its expiry is truncated",
			start: now.Add(300 * time.Millisecond), ttl: reserve + time.Second, wait: time.Hour, released: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := now
			if !tc.start.IsZero() {
				start = tc.start
			}
			clk, _ := sleepLog(start)
			ctx := context.Background()
			if tc.limit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, start.Add(tc.limit))
				defer cancel()
			}
			refusal := &shortClaimRefusal{err: refused, released: tc.released}
			retake, err := retakeShortClaim(ctx, refusal, tc.ttl, start.Add(tc.wait), settings, clk)
			if retake != tc.want {
				t.Fatalf("retake = %v, want %v", retake, tc.want)
			}
			var wrapper *shortClaimRefusal
			switch {
			case retake && err != nil:
				t.Fatalf("a retake returned an error: %v", err)
			case !retake && (!errors.Is(err, refused) || errors.As(err, &wrapper)):
				t.Fatalf("a refusal that is not retaken must return its own error, not the wrapper, got %#v", err)
			}
		})
	}
}

// TestRetakeShortClaim_PassesOtherErrorsThrough checks that an error other
// than a short-claim refusal, and a nil error, come back unchanged.
func TestRetakeShortClaim_PassesOtherErrorsThrough(t *testing.T) {
	clk, _ := sleepLog(time.Unix(1000, 0))
	deadline := time.Unix(5000, 0)
	other := errors.New("read failed")
	for _, in := range []error{nil, other} {
		retake, err := retakeShortClaim(context.Background(), in, time.Minute, deadline, clusterLockSettings{}, clk)
		if retake || !errors.Is(err, in) {
			t.Fatalf("retakeShortClaim(%v) = %v, %v, want false and the same error", in, retake, err)
		}
	}
}
