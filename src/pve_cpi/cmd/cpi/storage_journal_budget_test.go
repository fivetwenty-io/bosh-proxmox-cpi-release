package main

import (
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fullParkerWait checks one parker lock wait against the lock code's clamp. A
// wait that starts at start must get its full ttl before the action's
// deadline less the margin the lock leaves, and it returns when that wait
// ends. These lines copy the clamp in clusterLockDeadline in
// pve/cluster_lock.go. A wait ends at its start plus its timeout, unless the
// request's deadline less the margin comes first. A change to that rule has
// to be matched here.
func fullParkerWait(t *testing.T, action, what string, start, deadline time.Time, ttl time.Duration) time.Time {
	t.Helper()
	waitEnd, limit := start.Add(ttl), deadline.Add(-pve.ClusterLockContextMargin)
	if limit.Before(waitEnd) {
		t.Fatalf("%s: %s is cut to %s by the deadline; it needs the full %s",
			action, what, limit.Sub(start).Round(time.Second), ttl)
	}
	return waitEnd
}

// actionDeadline returns the deadline action runs under.
func actionDeadline(t *testing.T, action string) time.Time {
	t.Helper()
	ctx, cancel := storageJournalContext(action)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("%s runs without a deadline", action)
	}
	return deadline
}

// TestStorageJournalCleanupBudgetOutlastsTheParkerWait checks the context
// cleanup runs under against the lock code's clamp. Cleanup first settles the
// record's planned steps, and the settler can wait out a whole holder on a
// parker's lock before it reads the parker. A wait that starts once the base
// work has used its whole budget must still get its full length. The window
// cleanup then waits for and runs, with the margin each wait leaves for its
// release and completion, must still get its full wait and end before the
// deadline. No sleeps: the clamp is a pure function of the context's
// deadline.
func TestStorageJournalCleanupBudgetOutlastsTheParkerWait(t *testing.T) {
	now := time.Now()
	deadline := actionDeadline(t, "cleanup")
	ttl := pve.ParkerProtectionLockTTLNow()
	start := now.Add(storageJournalBaseBudget)
	settled := fullParkerWait(t, "cleanup", "the settler's parker wait after the base work", start, deadline, ttl).
		Add(pve.ClusterLockContextMargin)
	waitEnd := fullParkerWait(t, "cleanup", "the window's parker wait after the settler's", settled, deadline, ttl)
	if end := waitEnd.Add(ttl).Add(pve.ClusterLockContextMargin); end.After(deadline) {
		t.Fatalf("the window after two full waits ends %s past the cleanup deadline", end.Sub(deadline).Round(time.Second))
	}
}

// TestStorageJournalSettlingBudgetsOutlastTheSettlersWait checks adopt and
// finalize-cleanup the same way. Both settle the record's planned steps, so
// the settler can wait out a whole holder on a parker's lock, and a wait that
// starts once the base work has used its whole budget must still get its full
// length, with the margin for the read, the release, and the completion
// before the deadline.
func TestStorageJournalSettlingBudgetsOutlastTheSettlersWait(t *testing.T) {
	for _, action := range []string{"adopt", "finalize-cleanup"} {
		now := time.Now()
		deadline := actionDeadline(t, action)
		ttl := pve.ParkerProtectionLockTTLNow()
		waitEnd := fullParkerWait(t, action, "the settler's parker wait after the base work", now.Add(storageJournalBaseBudget), deadline, ttl)
		if end := waitEnd.Add(pve.ClusterLockContextMargin); end.After(deadline) {
			t.Fatalf("%s: the settle after a full wait ends %s past the deadline", action, end.Sub(deadline).Round(time.Second))
		}
	}
}

// TestStorageJournalBudgetLeavesLockFreeActionsAlone keeps every action that
// never takes a parker's lock on the base budget.
func TestStorageJournalBudgetLeavesLockFreeActionsAlone(t *testing.T) {
	for _, action := range []string{"audit-enrollment", "initialize", "audit", "recover-authority", "recover-index", "resolve-missing-vm"} {
		if got := storageJournalBudget(action); got != storageJournalBaseBudget {
			t.Errorf("%s runs under %s, want the base %s", action, got, storageJournalBaseBudget)
		}
	}
}
