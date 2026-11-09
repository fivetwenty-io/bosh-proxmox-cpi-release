package endpointpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSendByMatchesOnlyItsMethodAndPath(t *testing.T) {
	deadline := time.Unix(1000, 0)
	ctx := WithSendBy(context.Background(), deadline, "DELETE", "/api2/json/pools")
	cases := []struct {
		name         string
		method, path string
		want         bool
	}{
		{"same request", "DELETE", "/api2/json/pools", true},
		{"method case ignored", "delete", "/api2/json/pools", true},
		{"readback GET", "GET", "/api2/json/pools", false},
		{"other path", "DELETE", "/api2/json/pools/x", false},
		{"path is exact", "DELETE", "/api2/json/POOLS", false},
		{"other method same path", "POST", "/api2/json/pools", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		got, ok := SendBy(ctx, tc.method, tc.path)
		if ok != tc.want {
			t.Errorf("%s: matched = %v, want %v", tc.name, ok, tc.want)
		}
		if ok && !got.Equal(deadline) {
			t.Errorf("%s: deadline = %v, want %v", tc.name, got, deadline)
		}
	}
}

func TestCheckSendBy(t *testing.T) {
	deadline := time.Unix(1000, 0)
	ctx := WithSendBy(context.Background(), deadline, "DELETE", "/pools")

	if err := CheckSendBy(ctx, deadline.Add(-time.Nanosecond), "DELETE", "/pools"); err != nil {
		t.Errorf("before the deadline: %v", err)
	}
	if err := CheckSendBy(ctx, deadline, "DELETE", "/pools"); !errors.Is(err, ErrSendByPassed) {
		t.Errorf("at the deadline: %v, want ErrSendByPassed", err)
	}
	if err := CheckSendBy(ctx, deadline.Add(time.Hour), "DELETE", "/pools"); !errors.Is(err, ErrSendByPassed) {
		t.Errorf("after the deadline: %v, want ErrSendByPassed", err)
	}
	// A passed deadline never refuses a request it does not guard.
	for _, other := range [][2]string{{"GET", "/pools"}, {"DELETE", "/other"}, {"POST", "/pools"}} {
		if err := CheckSendBy(ctx, deadline.Add(time.Hour), other[0], other[1]); err != nil {
			t.Errorf("%s %s should be unguarded, got %v", other[0], other[1], err)
		}
	}
}

func TestCheckSendByWithoutMarker(t *testing.T) {
	if err := CheckSendBy(context.Background(), time.Now(), "DELETE", "/pools"); err != nil {
		t.Errorf("no marker should never refuse: %v", err)
	}
	if _, ok := SendBy(context.Background(), "DELETE", "/pools"); ok {
		t.Error("no marker should not match")
	}
}

func TestSendByEarliestMatchWinsAndMarkersChain(t *testing.T) {
	early, late := time.Unix(1000, 0), time.Unix(2000, 0)
	ctx := WithSendBy(context.Background(), late, "DELETE", "/pools")
	ctx = WithSendBy(ctx, early, "DELETE", "/pools")
	ctx = WithSendBy(ctx, late, "PUT", "/other")

	if got, ok := SendBy(ctx, "DELETE", "/pools"); !ok || !got.Equal(early) {
		t.Errorf("DELETE /pools = %v, %v; want the earlier deadline", got, ok)
	}
	if got, ok := SendBy(ctx, "PUT", "/other"); !ok || !got.Equal(late) {
		t.Errorf("PUT /other = %v, %v; the inner marker must not hide the outer one", got, ok)
	}

	// Order of registration does not change the result.
	rev := WithSendBy(context.Background(), early, "DELETE", "/pools")
	rev = WithSendBy(rev, late, "DELETE", "/pools")
	if got, _ := SendBy(rev, "DELETE", "/pools"); !got.Equal(early) {
		t.Errorf("reversed order = %v, want the earlier deadline", got)
	}
}

func TestSendByEmptyMethodOrPathNeverMatches(t *testing.T) {
	base := context.Background()
	for _, ctx := range []context.Context{
		WithSendBy(base, time.Unix(1, 0), "", "/pools"),
		WithSendBy(base, time.Unix(1, 0), "DELETE", ""),
	} {
		if ctx != base {
			t.Error("an unusable marker should leave the context unchanged")
		}
		if err := CheckSendBy(ctx, time.Unix(100, 0), "DELETE", "/pools"); err != nil {
			t.Errorf("unusable marker refused a request: %v", err)
		}
		if err := CheckSendBy(ctx, time.Unix(100, 0), "", ""); err != nil {
			t.Errorf("unusable marker refused an empty request: %v", err)
		}
	}
}

func TestSendByDoesNotChangeContextCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := WithSendBy(parent, time.Now().Add(time.Hour), "DELETE", "/pools")
	cancel()
	if ctx.Err() == nil {
		t.Error("cancelling the parent should cancel the derived context")
	}
}
