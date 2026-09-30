package allocationjournal

import (
	"context"
	"errors"
	"testing"
)

// lockStep is a planned sentinel create in the shape a disk lifecycle writes
// one, naming the disk's volume and the VM the disk was going to.
func lockStep(id string, attempt int) Step {
	return Step{ID: id, Attempt: attempt, Kind: "lifecycle_attach_disk_Pool_CreatePool", Target: Target{Node: "node-a", Storage: "nfs-a", Backing: "nfs:server/export", VMID: 777, IntendedVolume: "nfs-a:123/vm-123-disk-1.raw"}, State: Planned}
}

// settle returns r with the step at index settled the way the lock step rule
// settles one, observed and holding its intended volume.
func settle(r Record, index int) Record {
	r.Steps[index].State = Observed
	r.Steps[index].VolIDs = append(r.Steps[index].VolIDs, r.Steps[index].Target.IntendedVolume)
	return r
}

// crashedLockStepRecord writes a record whose request stopped right after it
// planned a lock step, so the record keeps the state it had, which is planned
// here, and then reopens it the way every later request does.
func crashedLockStepRecord(t *testing.T, write func(*testing.T, *Handle)) *Handle {
	t.Helper()
	j, _ := fixture(t)
	h := retryVM(t, j)
	write(t, h)
	id := h.Record().ID
	closeHandle(t, h)
	resumed, err := j.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resumed.Close(); err != nil {
			t.Error(err)
		}
	})
	if state := resumed.Record().State; state != Planned {
		t.Fatalf("the crashed record is %s, want %s", state, Planned)
	}
	return resumed
}

func plannedLockStep(t *testing.T, h *Handle) {
	t.Helper()
	r := h.Record()
	r.Steps = append(r.Steps, lockStep("lock", 0))
	save(t, h, r)
}

// TestResumedHandleAcceptsASettlementWrite covers a record that a crash left
// in planned with one planned lock step. A resumed handle accepts the write
// that settles the step, and the record's state does not change.
func TestResumedHandleAcceptsASettlementWrite(t *testing.T) {
	h := crashedLockStepRecord(t, plannedLockStep)
	if err := h.Save(settle(h.Record(), 0)); err != nil {
		t.Fatalf("the resumed handle refused the settlement: %v", err)
	}
	r := h.Record()
	if r.State != Planned || r.Reason != "" || len(r.Verifications) != 0 {
		t.Fatalf("the settlement changed the record to %s (%q) with %d verifications", r.State, r.Reason, len(r.Verifications))
	}
	if s := r.Steps[0]; s.State != Observed || len(s.VolIDs) != 1 || s.VolIDs[0] != s.Target.IntendedVolume {
		t.Fatalf("the settled step is %+v", s)
	}
}

// TestResumedHandleRefusesMoreThanASettlement keeps the gate closed to every
// write that does more than settle. Each variant is refused, and the plain
// settlement that follows it on the same handle is accepted, so the refusal
// comes from the write's shape and not from the handle.
func TestResumedHandleRefusesMoreThanASettlement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, *Handle)
		index int
		more  func(Record) Record
	}{
		{name: "a state change", write: plannedLockStep, more: func(r Record) Record {
			r = settle(r, 0)
			r.State = Observed
			return r
		}},
		{name: "an appended step", write: plannedLockStep, more: func(r Record) Record {
			r = settle(r, 0)
			r.Steps = append(r.Steps, lockStep("another", 0))
			return r
		}},
		{name: "a submitted step", write: plannedLockStep, more: func(r Record) Record {
			r.Steps[0].State = Submitted
			return r
		}},
		{name: "a closed attempt's step", write: func(t *testing.T, h *Handle) {
			plannedLockStep(t, h)
			proof := retryProof("absence")
			proof.OutcomesKnown = false
			proof.NoSubmissionVerified = true
			if err := h.BeginAttempt(retryPlan(retryIntent()), proof); err != nil {
				t.Fatal(err)
			}
			r := h.Record()
			r.Steps = append(r.Steps, lockStep("retry-lock", 1))
			save(t, h, r)
		}, index: 1, more: func(r Record) Record {
			return settle(r, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := crashedLockStepRecord(t, tc.write)
			err := h.Save(tc.more(h.Record()))
			if !errors.Is(err, ErrReconciliationRequired) && !errors.Is(err, ErrConflict) {
				t.Fatalf("the resumed handle accepted %s: %v", tc.name, err)
			}
			if err := h.Save(settle(h.Record(), tc.index)); err != nil {
				t.Fatalf("the plain settlement after refusing %s was refused too: %v", tc.name, err)
			}
		})
	}
}
