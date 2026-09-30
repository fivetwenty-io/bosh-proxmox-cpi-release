package main

import (
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestStorageJournalCleanupBudgetOutlastsTheParkerWait checks the context
// cleanup runs under against the lock code's clamp. A parker lock wait that
// starts once the base work has used its whole budget must still get its full
// length, and the window cleanup then runs, with the margin the lock leaves
// for its release and completion, must end before the deadline. No sleeps: the
// clamp is a pure function of the context's deadline.
func TestStorageJournalCleanupBudgetOutlastsTheParkerWait(t *testing.T) {
	now := time.Now()
	ctx, cancel := storageJournalContext("cleanup")
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanup runs without a deadline")
	}
	start := now.Add(storageJournalBaseBudget)
	// These two lines copy the clamp in clusterLockDeadline in
	// pve/cluster_lock.go. A wait ends at its start plus its timeout, unless
	// the request's deadline less the margin comes first. A change to that
	// rule has to be matched here.
	waitEnd, limit := start.Add(pve.ParkerProtectionLockTTL), deadline.Add(-pve.ClusterLockContextMargin)
	if limit.Before(waitEnd) {
		t.Fatalf("a parker wait started after %s of work is cut to %s by the cleanup deadline; it needs the full %s",
			storageJournalBaseBudget, limit.Sub(start).Round(time.Second), pve.ParkerProtectionLockTTL)
	}
	if end := waitEnd.Add(pve.ParkerProtectionLockTTL).Add(pve.ClusterLockContextMargin); end.After(deadline) {
		t.Fatalf("the window after a full wait ends %s past the cleanup deadline", end.Sub(deadline).Round(time.Second))
	}
}

// TestStorageJournalBudgetLeavesLockFreeActionsAlone keeps every action that
// never takes a parker's lock on the base budget.
func TestStorageJournalBudgetLeavesLockFreeActionsAlone(t *testing.T) {
	for _, action := range []string{"audit-enrollment", "initialize", "audit", "recover-authority", "recover-index", "resolve-missing-vm", "adopt", "finalize-cleanup"} {
		if got := storageJournalBudget(action); got != storageJournalBaseBudget {
			t.Errorf("%s runs under %s, want the base %s", action, got, storageJournalBaseBudget)
		}
	}
}
