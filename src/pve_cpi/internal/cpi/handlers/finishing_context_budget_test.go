package handlers

import (
	"context"
	"testing"
	"time"
)

// finishingTestKey carries a request value the finishing context must keep.
type finishingTestKey struct{}

func TestFinishingContextBudget(t *testing.T) {
	t.Parallel()
	const budget = 5 * time.Second
	base := context.WithValue(t.Context(), finishingTestKey{}, "kept")

	t.Run("live request without a deadline runs unchanged", func(t *testing.T) {
		t.Parallel()
		got, cancel := finishingContext(base, budget)
		defer cancel()
		if got != base {
			t.Fatal("an unbounded live request was replaced")
		}
	})
	t.Run("ended request gets the budget", func(t *testing.T) {
		t.Parallel()
		req := newEndingRequest(base)
		req.end()
		got, cancel := finishingContext(req, budget)
		defer cancel()
		assertFinishingWindow(t, got, budget-time.Second, budget)
	})
	t.Run("cancelled request without a deadline runs unchanged", func(t *testing.T) {
		t.Parallel()
		parent, stop := context.WithCancel(base)
		stop()
		got, cancel := finishingContext(parent, budget)
		defer cancel()
		if got != parent {
			t.Fatal("a stop signal on a request without a deadline was outlived")
		}
	})

	t.Run("near deadline is extended to the budget", func(t *testing.T) {
		t.Parallel()
		parent, stop := context.WithTimeout(base, time.Millisecond)
		defer stop()
		got, cancel := finishingContext(parent, budget)
		defer cancel()
		assertFinishingWindow(t, got, budget-time.Second, budget)
	})
	t.Run("far deadline is kept in full and detached", func(t *testing.T) {
		t.Parallel()
		req := newEndingRequest(base)
		got, cancel := finishingContext(req, budget)
		defer cancel()
		assertFinishingWindow(t, got, 59*time.Minute, time.Hour)
		req.end()
		if got.Err() != nil {
			t.Fatalf("the request's end cut the finishing work off: %v", got.Err())
		}
	})
	t.Run("nil parent gets the budget", func(t *testing.T) {
		t.Parallel()
		//lint:ignore SA1012 a nil parent is the input under test
		got, cancel := finishingContext(nil, budget) //nolint:staticcheck // SA1012: a nil parent is the input under test
		defer cancel()
		if got == nil {
			t.Fatal("no context for a nil parent")
		}
		if deadline, ok := got.Deadline(); !ok || time.Until(deadline) > budget {
			t.Fatalf("nil parent deadline = %v, %t", deadline, ok)
		}
	})
}

func assertFinishingWindow(t *testing.T, ctx context.Context, least, most time.Duration) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Fatalf("the finishing context is already done: %v", err)
	}
	if ctx.Value(finishingTestKey{}) != "kept" {
		t.Fatal("the finishing context lost the request's values")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the finishing context has no deadline")
	}
	if left := time.Until(deadline); left < least || left > most {
		t.Fatalf("the finishing context has %v left, want between %v and %v", left, least, most)
	}
}
