package handlers

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
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

// admitLifecycleCall leaves a parked disk's record the way a lifecycle call
// leaves it once the CPI admits the call. The record passes through
// reconciliation_required with the lifecycle's reason and lands in observed
// with fresh ownership evidence, as beginStorageLifecycle writes it. It returns
// the handle that did so, still held, because the guard plans its first step
// through that same handle.
func admitLifecycleCall(t *testing.T, disk *parkedFlowDisk) *aj.Handle {
	t.Helper()
	markRecordUncertain(t, disk, "lifecycle detach_disk admitted; completion pending")
	handle, err := disk.journal.Acquire(t.Context(), disk.id)
	if err != nil {
		t.Fatal(err)
	}
	id, payload, err := aj.VerificationEvidence(map[string]any{"operation": "test_lifecycle_admission", "state": aj.Observed})
	if err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.Observed
	record.Verifications = append(record.Verifications, aj.Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, OwnershipVerified: true})
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	return handle
}

// markRecordAdmitted leaves a parked disk's record observed after a lifecycle
// call admitted it and the CPI stopped before the call planned anything.
func markRecordAdmitted(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	handle := admitLifecycleCall(t, disk)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

// markRecordPlanned leaves a parked disk's record planned after a lifecycle
// call admitted it, planned its lock step for the parker, and the CPI stopped
// before the lock came back. storageMutationIntent writes the step and moves
// the record to planned in one write, as the guard does, so cleanup settles the
// step before it admits its own lifecycle.
func markRecordPlanned(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	handle := admitLifecycleCall(t, disk)
	birth := handle.Record().Steps[0].Target
	target := aj.Target{Node: birth.Node, Storage: birth.Storage, Backing: birth.Backing, VMID: disk.parker, IntendedVolume: birth.IntendedVolume}
	if _, err := storageMutationIntent(handle, "lifecycle_detach_disk_Pool_CreatePool", target, nil); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

// requireRecordRestored fails the test unless after is before apart from what
// the journal only appends to. Each step after the ones before held must be an
// observed lock step, a step before held may only have settled from planned to
// observed, and each verification after the ones before held must prove
// ownership. Every other field must be unchanged, so a restore that rewrote
// anything else is caught here.
func requireRecordRestored(t *testing.T, before, after aj.Record) {
	t.Helper()
	if len(after.Steps) < len(before.Steps) || len(after.Verifications) < len(before.Verifications) {
		t.Fatalf("the record lost evidence, steps %d to %d and verifications %d to %d",
			len(before.Steps), len(after.Steps), len(before.Verifications), len(after.Verifications))
	}
	for i := range before.Steps {
		was, now := before.Steps[i], after.Steps[i]
		if reflect.DeepEqual(was, now) {
			continue
		}
		if was.State != aj.Planned || now.State != aj.Observed || !isLockStep(was) {
			t.Fatalf("step %s changed from %+v to %+v", was.ID, was, now)
		}
		now.State, now.VolIDs = was.State, was.VolIDs
		if !reflect.DeepEqual(was, now) {
			t.Fatalf("step %s changed beyond settling from planned to observed, from %+v to %+v", was.ID, was, after.Steps[i])
		}
	}
	for i := len(before.Steps); i < len(after.Steps); i++ {
		step := &after.Steps[i]
		if step.State != aj.Observed || !isLockStep(*step) {
			t.Fatalf("cleanup added step %s (%s) in state %s, want only observed lock steps", step.ID, step.Kind, step.State)
		}
	}
	for i := range before.Verifications {
		if !reflect.DeepEqual(before.Verifications[i], after.Verifications[i]) {
			t.Fatalf("verification %d changed from %+v to %+v", i, before.Verifications[i], after.Verifications[i])
		}
	}
	for i, v := range after.Verifications[len(before.Verifications):] {
		if !v.OwnershipVerified {
			t.Fatalf("added verification %d does not prove ownership: %+v", len(before.Verifications)+i, v)
		}
	}
	was, now := before, after
	was.Steps, now.Steps, was.Verifications, now.Verifications = nil, nil, nil, nil
	was.UpdatedAt, now.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(was, now) {
		t.Fatalf("the timed-out cleanup changed the record beyond its evidence, from %+v to %+v", was, now)
	}
}

// contendedCleanup runs a plain explicit cleanup of the parked disk's record
// on ctx while another request holds the parker's lock past the managed wait.
func contendedCleanup(t *testing.T, ctx context.Context, disk *parkedFlowDisk, locks *lockContention) (time.Duration, error) {
	t.Helper()
	plantHeldParkerLock(locks, disk.parker)
	ctx = shortenManagedLockWait(ctx, testManagedLockWait)
	decision := StorageAllocationDecision{Action: "cleanup", AllocationID: disk.id, DecisionID: "contended-cleanup"}
	started := time.Now()
	_, err := CleanupStorageAllocation(ctx, attestedCleanupDeps(disk.deps), disk.journal, []string{"n1"}, decision)
	return time.Since(started), err
}

// TestExplicitCleanupLockTimeoutLeavesTheRecord runs explicit cleanup of a
// parked disk against a parker lock another request holds past the managed
// wait. Cleanup changed nothing before the wait, so it must leave the record
// as it was, in the state and with the reason it had, with every step observed
// and the disk still on its parker, and hand back the retriable timeout for us
// to rerun. Only the evidence the journal appends to may differ. Every disposition cleanup can start from with the disk on its parker
// and every step observed is covered: a returned record, an adopted one, one
// already needing reconciliation, and one a lifecycle call left observed or
// planned when the CPI stopped after admitting it.
func TestExplicitCleanupLockTimeoutLeavesTheRecord(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, disk *parkedFlowDisk){
		"ready_to_return": func(*testing.T, *parkedFlowDisk) {},
		"adopted": func(t *testing.T, disk *parkedFlowDisk) {
			next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
			if err != nil || next.State != aj.Adopted {
				t.Fatalf("setup adoption: %v (state %s)", err, next.State)
			}
		},
		"reconciliation_required": func(t *testing.T, disk *parkedFlowDisk) {
			markRecordUncertain(t, disk, "outcome requires reconciliation at an earlier operation")
		},
		"observed": func(t *testing.T, disk *parkedFlowDisk) { markRecordAdmitted(t, disk) },
		"planned":  func(t *testing.T, disk *parkedFlowDisk) { markRecordPlanned(t, disk) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			locks := newLockContention(t)
			disk := newParkedFlowDisk(t, locks)
			locks.reset()
			prepare(t, disk)
			before := disk.record(t)
			if before.State != aj.State(name) {
				t.Fatalf("setup left the record %s, want %s", before.State, name)
			}
			if planned := slices.ContainsFunc(before.Steps, func(step aj.Step) bool { return step.State == aj.Planned }); planned != (name == "planned") {
				t.Fatalf("setup left a planned step %v in a %s record", planned, name)
			}

			elapsed, err := contendedCleanup(t, t.Context(), disk, locks)
			if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("want the retriable lock timeout, got %v (%s)", err, StorageAllocationDecisionFailure(err))
			}
			if elapsed >= 10*time.Second {
				t.Fatalf("the wait took %s, so the managed wait was not applied", elapsed)
			}
			if elapsed < testManagedLockWait {
				t.Fatalf("the timeout came after %s, before the %s wait ran out", elapsed, testManagedLockWait)
			}
			after := disk.record(t)
			if after.State != before.State || after.Reason != before.Reason || after.CID != before.CID {
				t.Fatalf("the timed-out cleanup left the record %s (reason %q, CID %q), was %s (reason %q, CID %q)",
					after.State, after.Reason, after.CID, before.State, before.Reason, before.CID)
			}
			requireRecordRestored(t, before, after)
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

// adoptedDiskFailingWriteBack adopts a parked disk and returns it with a
// context whose journal refuses the write that puts the record back to adopted
// after cleanup's clean lock timeout. Cleanup's admission also writes the
// record while it is adopted, so only an adopted write that follows a write to
// ready_to_return fails.
func adoptedDiskFailingWriteBack(t *testing.T, locks *lockContention) (*parkedFlowDisk, aj.Record, context.Context, error) {
	t.Helper()
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	if next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk)); err != nil || next.State != aj.Adopted {
		t.Fatalf("setup adoption: %v (state %s)", err, next.State)
	}
	before := disk.record(t)
	crash := errors.New("the CPI stopped before the write back to adopted")
	var returned atomic.Bool
	ctx := aj.WithSaveFaultForTest(t.Context(), func(record aj.Record) error {
		switch {
		case record.State == aj.ReadyToReturn:
			returned.Store(true)
		case record.State == aj.Adopted && returned.Load():
			return crash
		}
		return nil
	})
	return disk, before, ctx, crash
}

// TestExplicitCleanupLockTimeoutAdoptedCrashBetweenWrites stops the CPI
// between the two writes that put an adopted record back after a clean lock
// timeout. The write back to adopted fails, which leaves the record on disk
// in ready_to_return with its CID and every step observed, never in
// reconciliation_required. A plain adopt then takes it back to adopted.
func TestExplicitCleanupLockTimeoutAdoptedCrashBetweenWrites(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk, before, ctx, crash := adoptedDiskFailingWriteBack(t, locks)

	if _, err := contendedCleanup(t, ctx, disk, locks); !errors.Is(err, crash) {
		t.Fatalf("want the failed write back to adopted inside the failure, got %v", err)
	}
	after := disk.record(t)
	if after.State != aj.ReadyToReturn || after.Reason != before.Reason || after.CID != before.CID {
		t.Fatalf("the interrupted restore left the record %s (reason %q, CID %q), want %s (reason %q, CID %q)",
			after.State, after.Reason, after.CID, aj.ReadyToReturn, before.Reason, before.CID)
	}
	for i := range after.Steps {
		if step := &after.Steps[i]; step.Attempt == after.ActiveAttempt() && step.State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
	}
	if holder := resolveFlowDisk(t, disk).holder; holder == nil || !holder.IsParker || holder.VMID != disk.parker {
		t.Fatalf("the disk is not on its parker after the interrupted restore: %+v", holder)
	}
	next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err != nil || next.State != aj.Adopted || next.CID != before.CID {
		t.Fatalf("adopt after the interrupted restore: %v (state %s, CID %q)", err, next.State, next.CID)
	}
}

// TestExplicitCleanupLockTimeoutAdoptedWriteBackFailureNamesTheState fails the
// write back to adopted without stopping the CPI. The record is in
// ready_to_return and needs no reconciliation, so the error must say which
// state the record is in and which commands the operator runs next, and it
// must not claim the allocation requires reconciliation. It must also still
// carry the lock timeout and the journal's own failure.
func TestExplicitCleanupLockTimeoutAdoptedWriteBackFailureNamesTheState(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk, _, ctx, crash := adoptedDiskFailingWriteBack(t, locks)

	_, err := contendedCleanup(t, ctx, disk, locks)
	if err == nil {
		t.Fatal("cleanup succeeded although the write back to adopted failed")
	}
	if !errors.Is(err, crash) || !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("the failure lost its causes, got %v", err)
	}
	if errors.Is(err, aj.ErrReconciliationRequired) {
		t.Fatalf("the failure sends the operator to reconciliation for a record that needs none: %v", err)
	}
	// The CLI prints StorageAllocationDecisionFailure, so that is the text an
	// operator reads.
	text := StorageAllocationDecisionFailure(err)
	if strings.Contains(text, "requires reconciliation") {
		t.Fatalf("the CLI text sends the operator to reconciliation for a record that needs none: %s", text)
	}
	for _, want := range []string{disk.id, "ready_to_return", "storage-journal cleanup", "storage-journal adopt"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the CLI text does not mention %q: %s", want, text)
		}
	}
	if record := disk.record(t); record.State != aj.ReadyToReturn {
		t.Fatalf("the record is %s, but the failure says ready_to_return", record.State)
	}
}

// TestExplicitCleanupMutationBeforeTheWaitStaysUncertain admits a guarded
// write to the parker before cleanup's lock wait, and then the wait runs out.
// The disk's holder has been touched, so the timeout is not clean and the
// record goes to reconciliation_required, as any other failure does.
func TestExplicitCleanupMutationBeforeTheWaitStaysUncertain(t *testing.T) {
	t.Parallel()
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
