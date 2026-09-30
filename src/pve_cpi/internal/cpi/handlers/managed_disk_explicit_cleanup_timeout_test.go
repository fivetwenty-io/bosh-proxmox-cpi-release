package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// markRecordUncertain puts a parked disk's record in reconciliation_required
// with reason, the state explicit cleanup is usually run against.
func markRecordUncertain(t *testing.T, disk *parkedFlowDisk, reason string) {
	t.Helper()
	handle, err := disk.journal.Acquire(t.Context(), disk.id)
	if err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = reason
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

// contendedCleanup runs a plain explicit cleanup of the parked disk's record
// on ctx while another request holds the parker's lock past the managed wait.
func contendedCleanup(t *testing.T, ctx context.Context, disk *parkedFlowDisk, locks *lockContention) (time.Duration, error) {
	t.Helper()
	plantHeldParkerLock(locks, disk.parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)
	decision := StorageAllocationDecision{Action: "cleanup", AllocationID: disk.id, DecisionID: "contended-cleanup"}
	started := time.Now()
	_, err := CleanupStorageAllocation(ctx, attestedCleanupDeps(disk.deps), disk.journal, []string{"n1"}, decision)
	return time.Since(started), err
}

// TestExplicitCleanupLockTimeoutLeavesTheRecord runs explicit cleanup of a
// parked disk against a parker lock another request holds past the managed
// wait. Cleanup changed nothing before the wait, so it must leave the record
// in the state and with the reason it had, with every step observed and the
// disk still on its parker, and hand back the retriable timeout for us to
// rerun. Both a returned record and one already needing reconciliation are
// covered.
func TestExplicitCleanupLockTimeoutLeavesTheRecord(t *testing.T) {
	for name, uncertain := range map[string]bool{"ready_to_return": false, "reconciliation_required": true} {
		t.Run(name, func(t *testing.T) {
			locks := newLockContention(t)
			disk := newParkedFlowDisk(t, locks)
			locks.reset()
			if uncertain {
				markRecordUncertain(t, disk, "outcome requires reconciliation at an earlier operation")
			}
			before := disk.record(t)

			elapsed, err := contendedCleanup(t, t.Context(), disk, locks)
			if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("want the retriable lock timeout, got %v (%s)", err, StorageAllocationDecisionFailure(err))
			}
			if elapsed >= 10*time.Second {
				t.Fatalf("the wait took %s, so the managed wait was not applied", elapsed)
			}
			after := disk.record(t)
			if after.State != before.State || after.Reason != before.Reason || after.CID != before.CID {
				t.Fatalf("the timed-out cleanup left the record %s (reason %q, CID %q), was %s (reason %q, CID %q)",
					after.State, after.Reason, after.CID, before.State, before.Reason, before.CID)
			}
			for i := range after.Steps {
				if step := &after.Steps[i]; step.Attempt == after.ActiveAttempt() && step.State != aj.Observed {
					t.Fatalf("step %s (%s) left %s", step.ID, step.Kind, step.State)
				}
			}
			if holder := resolveFlowDisk(t, disk).holder; holder == nil || !holder.IsParker || holder.VMID != disk.parker {
				t.Fatalf("the disk is not on its parker after the timed-out cleanup: %+v", holder)
			}
		})
	}
}

// TestExplicitCleanupMutationBeforeTheWaitStaysUncertain admits a guarded
// write to the parker before cleanup's lock wait, and then the wait runs out.
// The disk's holder has been touched, so the timeout is not clean and the
// record goes to reconciliation_required, as any other failure does.
func TestExplicitCleanupMutationBeforeTheWaitStaysUncertain(t *testing.T) {
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx := withExplicitCleanupBeforeUnpark(t.Context(), func(ctx context.Context, local Deps, _ resolvedDisk) {
		protect := true
		if err := local.PVE.Nodes().UpdateQemuConfig(ctx, "n1", itoa(disk.parker), &nodes.UpdateQemuConfigParams{Protection: &protect}); err != nil {
			t.Errorf("guarded parker write: %v", err)
		}
	})

	_, err := contendedCleanup(t, ctx, disk, locks)
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("want the lock timeout inside the failure, got %v", err)
	}
	record := disk.record(t)
	if record.State != aj.ReconciliationRequired || !strings.Contains(record.Reason, "explicit disk cleanup incomplete") {
		t.Fatalf("a cleanup that touched the disk before its wait left the record %s (reason %q)", record.State, record.Reason)
	}
}
