package pve

import (
	"context"
	"testing"
	"time"
)

func TestNetworkResolvePollFor_DefaultsToTheInterval(t *testing.T) {
	t.Parallel()
	if got := networkResolvePollFor(context.Background()); got != time.Second {
		t.Fatalf("poll on a bare context = %s, want 1s", got)
	}
}

func TestNetworkResolvePollFor_ReadsTheContextOverride(t *testing.T) {
	t.Parallel()
	ctx := WithNetworkResolvePollForTest(context.Background(), 7*time.Millisecond)
	if got := networkResolvePollFor(ctx); got != 7*time.Millisecond {
		t.Fatalf("poll with an override = %s, want 7ms", got)
	}
}

func TestWithNetworkResolvePollForTest_IgnoresNonPositive(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Millisecond} {
		base := context.Background()
		if ctx := WithNetworkResolvePollForTest(base, d); ctx != base {
			t.Errorf("a poll of %s changed the context", d)
		}
	}
}

// TestPollUntilResolved_SleepsTheContextPoll runs a check that never resolves
// under a context carrying a short poll. Every sleep between attempts is that
// poll, and a bare context keeps the production interval.
func TestPollUntilResolved_SleepsTheContextPoll(t *testing.T) {
	t.Parallel()
	never := func(context.Context) (bool, error) { return false, nil }
	for name, tc := range map[string]struct {
		ctx  context.Context
		want time.Duration
	}{
		"override":     {WithNetworkResolvePollForTest(context.Background(), 7*time.Millisecond), 7 * time.Millisecond},
		"bare context": {context.Background(), networkResolvePollInterval},
	} {
		var slept []time.Duration
		ok, err := pollUntilResolved(tc.ctx, 3, time.Hour, sleepRecordingClock(time.Unix(1000, 0), &slept), never)
		if ok || err != nil {
			t.Fatalf("%s: a check that never resolves returned ok=%t err=%v", name, ok, err)
		}
		if len(slept) != 3 {
			t.Fatalf("%s: slept %d times, want one sleep before each of the 3 retries", name, len(slept))
		}
		for _, d := range slept {
			if d != tc.want {
				t.Fatalf("%s: slept %s, want %s", name, d, tc.want)
			}
		}
	}
}
