package endpointpool

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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

// The SDK builds every request URL from a base that ends in /api2/json, so a
// transport sees "/api2/json/pools" for the route the marker names "/pools".
func TestSendByMatchesTheSDKPrefixedPath(t *testing.T) {
	deadline := time.Unix(1000, 0)
	ctx := WithSendBy(context.Background(), deadline, "DELETE", "/pools")

	if got, ok := SendBy(ctx, "DELETE", APIPrefix+"/pools"); !ok || !got.Equal(deadline) {
		t.Errorf("SendBy on the prefixed path = %v, %v; want the marker's deadline", got, ok)
	}
	if err := CheckSendBy(ctx, deadline, "DELETE", "/api2/json/pools"); !errors.Is(err, ErrSendByPassed) {
		t.Errorf("CheckSendBy on the prefixed path at the deadline = %v, want ErrSendByPassed", err)
	}
	for _, other := range [][2]string{
		{"GET", "/api2/json/pools"},
		{"DELETE", "/api2/json/pools/x"},
		{"DELETE", "/api2/json/nodes/pools"},
		{"DELETE", "/api2/jsonpools"},
		{"DELETE", "/api2/json"},
	} {
		if err := CheckSendBy(ctx, deadline.Add(time.Hour), other[0], other[1]); err != nil {
			t.Errorf("%s %s should be unguarded, got %v", other[0], other[1], err)
		}
	}

	// A marker written with the prefix guards the same request.
	prefixed := WithSendBy(context.Background(), deadline, "DELETE", "/api2/json/pools")
	if _, ok := SendBy(prefixed, "DELETE", "/api2/json/pools"); !ok {
		t.Error("a prefixed marker should match the prefixed request path")
	}
	if _, ok := SendBy(prefixed, "DELETE", "/pools"); !ok {
		t.Error("a prefixed marker should match the API-relative path")
	}
}

// A caller that refuses a request itself writes the line the pool writes, from
// the SDK-prefixed path the request would have gone to, and a nil logger
// writes nothing and does not panic.
func TestLogSendByRefusalWritesThePoolsLine(t *testing.T) {
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	LogSendByRefusal(logger, "DELETE", APIPrefix+"/pools")
	LogSendByRefusal(nil, "DELETE", APIPrefix+"/pools")

	lines := obs.All()
	if len(lines) != 1 || lines[0].Level != slog.LevelDebug ||
		lines[0].Message != "pve endpoints: DELETE /pools not sent, send-by deadline passed" {
		t.Fatalf("lines = %+v, want the pool's one Debug line", lines)
	}
	mustAttrs(t, lines[0], map[string]string{"method": "DELETE", "path": "/pools"})
}
