package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestClosingContextBudget(t *testing.T) {
	t.Parallel()
	const budget = pve.ClusterLockCompletionAllowance
	base := context.WithValue(t.Context(), finishingTestKey{}, "kept")

	t.Run("live request without a deadline runs unchanged", func(t *testing.T) {
		t.Parallel()
		got, cancel := closingContext(base)
		defer cancel()
		if got != base {
			t.Fatal("an unbounded live request was replaced")
		}
	})
	t.Run("stop signal without a deadline gets the budget", func(t *testing.T) {
		t.Parallel()
		parent, stop := context.WithCancel(base)
		stop()
		got, cancel := closingContext(parent)
		defer cancel()
		assertFinishingWindow(t, got, budget-time.Second, budget)
	})
	t.Run("ended request gets the budget", func(t *testing.T) {
		t.Parallel()
		req := newEndingRequest(base)
		req.end()
		got, cancel := closingContext(req)
		defer cancel()
		assertFinishingWindow(t, got, budget-time.Second, budget)
	})
	t.Run("far deadline is kept in full and detached", func(t *testing.T) {
		t.Parallel()
		req := newEndingRequest(base)
		got, cancel := closingContext(req)
		defer cancel()
		assertFinishingWindow(t, got, 59*time.Minute, time.Hour)
		req.end()
		if got.Err() != nil {
			t.Fatalf("the request's end cut the closing work off: %v", got.Err())
		}
	})
}
