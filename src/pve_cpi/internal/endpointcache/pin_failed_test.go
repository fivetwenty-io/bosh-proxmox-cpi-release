package endpointcache

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// pinFailedStored returns a stored payload of one node whose discovery was
// five minutes before payloadNow and whose pin_failed_at is failedAt.
func pinFailedStored(failedAt time.Time) *Payload {
	p := payloadFor(mergeKey(), "pve2")
	p.Discovery.PinFailedAt = failedAt
	return p
}

func TestMergeRecordsThePinFailureInTheDiscoverySection(t *testing.T) {
	earlier := payloadNow.Add(-3 * time.Minute)
	later := payloadNow.Add(-time.Minute)
	cases := []struct {
		name   string
		stored *Payload
		change Change
		want   time.Time
	}{
		{"a pin failure on a section that holds none", pinFailedStored(time.Time{}),
			Change{PinFailedAt: earlier}, earlier},
		{"a later pin failure replaces the stored one", pinFailedStored(earlier), Change{PinFailedAt: later}, later},
		{"an earlier pin failure never replaces the stored one", pinFailedStored(later),
			Change{PinFailedAt: earlier}, later},
		{"no pin failure leaves the stored one", pinFailedStored(earlier), Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}},
		}, earlier},
		{"a change with a pin mark and no time sets nothing", pinFailedStored(time.Time{}), Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkPin}},
		}, time.Time{}},
		{"a time past the skew is ignored", pinFailedStored(earlier), Change{
			PinFailedAt: payloadNow.Add(FutureSkew + time.Second),
		}, earlier},
		{"a time inside the skew is kept", pinFailedStored(time.Time{}), Change{
			PinFailedAt: payloadNow.Add(FutureSkew),
		}, payloadNow.Add(FutureSkew)},
		{"a stored time past the skew gives way to a real one",
			pinFailedStored(payloadNow.Add(10 * time.Minute)), Change{PinFailedAt: earlier}, earlier},
		{"an attempt leaves the stored pin failure", pinFailedStored(earlier), Change{
			Attempt: failedAttempt(FailureNodesUnreadable),
		}, earlier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := mustMerge(t, tc.stored, tc.change)
			if next.Discovery == nil || !next.Discovery.PinFailedAt.Equal(tc.want) {
				t.Errorf("pin_failed_at %v, want %v", next.Discovery.PinFailedAt, tc.want)
			}
		})
	}
}

func TestMergeClearsThePinFailureOnlyForAResult(t *testing.T) {
	stored := pinFailedStored(payloadNow.Add(-time.Minute))
	result := mergeResult("pve2")
	next := mustMerge(t, stored, Change{Result: result})
	if !next.Discovery.PinFailedAt.IsZero() {
		t.Errorf("a result left pin_failed_at %v", next.Discovery.PinFailedAt)
	}
	// A result never carries a pin failure in, and the change's own time does
	// not survive it either.
	result = mergeResult("pve2")
	result.Discovery.PinFailedAt = payloadNow.Add(-time.Minute)
	next = mustMerge(t, nil, Change{Result: result, PinFailedAt: payloadNow.Add(-time.Minute)})
	if !next.Discovery.PinFailedAt.IsZero() {
		t.Errorf("a result carried pin_failed_at %v in", next.Discovery.PinFailedAt)
	}
}

func TestMergeRecordsNoPinFailureWithoutAUsableSection(t *testing.T) {
	failed := payloadNow.Add(-time.Minute)
	// A file with no discovery section is not given one, because a pin failure
	// changes nothing there. The refresh is due already.
	stored := &Payload{Key: mergeKey(), Cluster: ptr(payloadCluster(payloadNow))}
	next := mustMerge(t, stored, Change{PinFailedAt: failed})
	if next.Discovery != nil {
		t.Errorf("a pin failure created the section %+v", next.Discovery)
	}
	// A section dated ahead is hidden from a reader and stays as it is.
	ahead := payloadFor(mergeKey(), "pve2")
	ahead.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	ahead.Discovery.NextRefreshAfter = payloadNow.Add(2 * time.Hour)
	next = mustMerge(t, ahead, Change{PinFailedAt: failed, Marks: map[string]DownMark{
		mergeSeed: {DownAt: failed, Class: MarkDial},
	}})
	if next.Discovery == nil || !next.Discovery.PinFailedAt.IsZero() {
		t.Errorf("a section dated ahead took pin_failed_at: %+v", next.Discovery)
	}
}

func TestValidateDropsAPinFailureTimeAheadOfTheClock(t *testing.T) {
	key := mergeKey()
	for name, tc := range map[string]struct {
		failedAt time.Time
		want     time.Time
		drops    int
	}{
		"none":               {time.Time{}, time.Time{}, 0},
		"a recent time":      {payloadNow.Add(-time.Minute), payloadNow.Add(-time.Minute), 0},
		"inside the skew":    {payloadNow.Add(FutureSkew), payloadNow.Add(FutureSkew), 0},
		"past the skew":      {payloadNow.Add(FutureSkew + time.Second), time.Time{}, 1},
		"a day ahead":        {payloadNow.Add(24 * time.Hour), time.Time{}, 1},
		"before the section": {payloadNow.Add(-time.Hour), payloadNow.Add(-time.Hour), 0},
	} {
		t.Run(name, func(t *testing.T) {
			usable, drops, err := Validate(pinFailedStored(tc.failedAt), key, payloadNow)
			if err != nil || usable == nil || usable.Discovery == nil {
				t.Fatalf("Validate: %+v, %v", usable, err)
			}
			// Dropping the field keeps the file and the identity it stores.
			if usable.Cluster == nil || len(usable.Discovery.Nodes) != 1 {
				t.Errorf("the section lost its identity or nodes: %+v", usable)
			}
			if !usable.Discovery.PinFailedAt.Equal(tc.want) {
				t.Errorf("pin_failed_at %v, want %v", usable.Discovery.PinFailedAt, tc.want)
			}
			var future int
			for _, drop := range drops {
				if drop.Record == "pin_failed_at" && drop.Reason == DropFuture {
					future++
				}
			}
			if future != tc.drops {
				t.Errorf("drops %v, want %d for pin_failed_at", drops, tc.drops)
			}
		})
	}
}

func TestPinFailedAtJSONShape(t *testing.T) {
	zero, err := json.Marshal(payloadFor(mergeKey(), "pve2").Discovery)
	if err != nil || strings.Contains(string(zero), "pin_failed_at") {
		t.Errorf("a zero pin_failed_at is written: %s, %v", zero, err)
	}
	set, err := json.Marshal(pinFailedStored(payloadNow.Add(-time.Minute)).Discovery)
	if err != nil || !strings.Contains(string(set), `"pin_failed_at":"2026-11-10T11:59:00Z"`) {
		t.Errorf("pin_failed_at is not written in UTC: %s, %v", set, err)
	}
	var back DiscoverySection
	if err := json.Unmarshal(set, &back); err != nil || !back.PinFailedAt.Equal(payloadNow.Add(-time.Minute)) {
		t.Errorf("pin_failed_at does not round-trip: %+v, %v", back, err)
	}
}

func TestRefreshDueCountsAStoredPinFailure(t *testing.T) {
	key := mergeKey()
	at := payloadNow.Add(-11 * time.Minute)
	section := func(failedAt time.Time, nodes bool) *Payload {
		p := payloadFor(key, "pve2")
		p.Discovery.DiscoveredAt = at
		p.Discovery.NextRefreshAfter = payloadNow.Add(time.Hour)
		p.Discovery.PinFailedAt = failedAt
		if !nodes {
			p.Discovery.Nodes = []DiscoveredNode{}
			p.Discovery.Outcome = OutcomeStandalone
		}
		usable, _, err := Validate(p, key, payloadNow)
		if err != nil {
			t.Fatal(err)
		}
		return usable
	}
	cases := []struct {
		name string
		p    *Payload
		now  time.Time
		want bool
	}{
		{"a failure after the discovery, past the floor", section(at.Add(5*time.Minute), true), payloadNow, true},
		{"a failure after the discovery, within the floor", section(at.Add(time.Minute), true),
			at.Add(PinRefreshFloor), false},
		{"a failure at the discovery", section(at, true), payloadNow, false},
		{"a failure before the discovery", section(at.Add(-time.Minute), true), payloadNow, false},
		{"no failure recorded", section(time.Time{}, true), payloadNow, false},
		{"a failure on a section with no nodes", section(at.Add(5*time.Minute), false), payloadNow, false},
	}
	for _, tc := range cases {
		if got := RefreshDue(tc.p, tc.now, false); got != tc.want {
			t.Errorf("%s: RefreshDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStoreCloseIsSafeAgainstConcurrentUse(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, "secret-a", StoreOptions{})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 40 {
				_, _, _ = s.ReadPayload(payloadNow, nil)
				_, _ = s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
					return Change{Attempt: failedAttempt(FailureNodesEmpty)}, nil
				})
				if lock, held, err := s.TryLockDiscovery(context.Background()); err == nil && held {
					_ = lock.Close()
				}
				_ = s.Sweep(payloadNow)
			}
		}()
	}
	close(start)
	time.Sleep(2 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if _, _, err := s.ReadPayload(payloadNow, nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("ReadPayload on the closed store: %v, want a closed-store error", err)
	}
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{}, nil
	})
	if err == nil || res.Written {
		t.Errorf("UpdatePayload on the closed store: %+v, %v", res, err)
	}
}

func TestMergeAttemptReplacesALaterStoredRefreshTime(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2")
	stored.Discovery.NextRefreshAfter = payloadNow.Add(20 * time.Hour)
	attempt := failedAttempt(FailureReadsFailed)
	attempt.At = payloadNow
	attempt.NextRefreshAfter = payloadNow.Add(time.Hour)
	next := mustMerge(t, stored, Change{Attempt: attempt})
	if want := payloadNow.Add(time.Hour); !next.Discovery.NextRefreshAfter.Equal(want) {
		t.Errorf("next_refresh_after %v, want the attempt's %v", next.Discovery.NextRefreshAfter, want)
	}
}

// A writer whose clock runs ahead can store a pin_failed_at that a reader on
// the true clock drops. The next write clears it, so it cannot wake up later
// as a failure that a failed attempt in between already answered.
func TestMergeClearsAPinFailureAheadOfTheWritersClock(t *testing.T) {
	key := mergeKey()
	ahead := payloadNow.Add(time.Hour)
	attemptAt := payloadNow.Add(11 * time.Minute)
	attempt := &DiscoveryAttempt{
		At: attemptAt, NextRefreshAfter: attemptAt.Add(RefreshInterval), Failure: FailureReadsFailed,
	}
	t.Run("an attempt clears it and no extra refresh follows", func(t *testing.T) {
		next, _, err := Merge(pinFailedStored(ahead), key, Change{Attempt: attempt}, attemptAt)
		if err != nil {
			t.Fatal(err)
		}
		if !next.Discovery.PinFailedAt.IsZero() {
			t.Fatalf("pin_failed_at %v survived the write", next.Discovery.PinFailedAt)
		}
		for _, off := range []time.Duration{30 * time.Minute, 61 * time.Minute} {
			now := payloadNow.Add(off)
			usable, _, err := Validate(next, key, now)
			if err != nil || usable == nil || usable.Discovery == nil {
				t.Fatalf("Validate at +%v: %+v, %v", off, usable, err)
			}
			if RefreshDue(usable, now, false) {
				t.Errorf("a refresh is due at +%v although the attempt answered the failure", off)
			}
		}
	})
	t.Run("a change with only a mark clears it", func(t *testing.T) {
		next := mustMerge(t, pinFailedStored(ahead), Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}},
		})
		if !next.Discovery.PinFailedAt.IsZero() {
			t.Errorf("pin_failed_at %v survived the write", next.Discovery.PinFailedAt)
		}
	})
	t.Run("a time inside the skew stays", func(t *testing.T) {
		inside := payloadNow.Add(FutureSkew)
		next := mustMerge(t, pinFailedStored(inside), Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}},
		})
		if !next.Discovery.PinFailedAt.Equal(inside) {
			t.Errorf("pin_failed_at %v, want the stored %v", next.Discovery.PinFailedAt, inside)
		}
	})
	t.Run("a section dated inside the skew is visible, so the clear reaches it", func(t *testing.T) {
		stored := pinFailedStored(ahead)
		stored.Discovery.DiscoveredAt = payloadNow.Add(10 * time.Second)
		stored.Discovery.NextRefreshAfter = stored.Discovery.DiscoveredAt.Add(RefreshInterval)
		next := mustMerge(t, stored, Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}},
		})
		if !next.Discovery.PinFailedAt.IsZero() {
			t.Errorf("pin_failed_at %v survived in a section a reader still loads", next.Discovery.PinFailedAt)
		}
	})
	t.Run("a section dated ahead is hidden and stays as it is", func(t *testing.T) {
		stored := pinFailedStored(ahead)
		stored.Discovery.DiscoveredAt = payloadNow.Add(30 * time.Minute)
		stored.Discovery.NextRefreshAfter = payloadNow.Add(90 * time.Minute)
		next := mustMerge(t, stored, Change{
			Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}},
		})
		if !next.Discovery.PinFailedAt.Equal(ahead) {
			t.Errorf("pin_failed_at %v, want the hidden section's %v", next.Discovery.PinFailedAt, ahead)
		}
	})
	t.Run("a real failure is recorded in its place", func(t *testing.T) {
		failed := payloadNow.Add(-time.Minute)
		next := mustMerge(t, pinFailedStored(ahead), Change{PinFailedAt: failed})
		if !next.Discovery.PinFailedAt.Equal(failed) {
			t.Errorf("pin_failed_at %v, want %v", next.Discovery.PinFailedAt, failed)
		}
	})
}
