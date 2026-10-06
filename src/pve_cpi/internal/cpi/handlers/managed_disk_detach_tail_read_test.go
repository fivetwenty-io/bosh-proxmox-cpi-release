package handlers

// These tests cover a failed read of 777's config, or of the cluster's
// identity, around the detach tail's removal write. When the guard's read
// before that write fails while the request is live, nothing has gone out, so
// the call returns the disk unchanged. The same failure on any other write is
// refused unsent and leaves the guard usable, and a request whose context ended
// during that read returns the disk unchanged as well. A readback that
// fails after PVE accepted the write leaves the record for reconciliation,
// because nothing shows what the write changed.

import (
	"context"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// detachAdmitted reports whether the record's detach_disk lifecycle is
// admitted. The tail runs inside that lifecycle once it is.
func (s *strandedDisk) detachAdmitted(id string) bool {
	record, err := s.journal.Inspect(id)
	return err == nil && record.Reason == "lifecycle detach_disk admitted; completion pending"
}

// failGuardReadAfterTailRead makes the first read of 777's config after the
// tail's own read fail inside detach_disk's lifecycle, answering with whatever
// the failure function returns. That read belongs to the guard and comes just
// before the tail's removal would go out. The function returns a pointer to a
// count of the reads it failed.
func (s *strandedDisk) failGuardReadAfterTailRead(id string, w *tailWrites, failure func() error) *int {
	return s.failReadAfterTailRead(id, w, &w.configRead, failure)
}

// failGuardIdentityAfterTailRead is failGuardReadAfterTailRead for the guard's
// read of the cluster's identity, which the guard makes before it reads 777's
// config.
func (s *strandedDisk) failGuardIdentityAfterTailRead(id string, w *tailWrites, failure func() error) *int {
	return s.failReadAfterTailRead(id, w, &w.identityRead, failure)
}

// failReadAfterTailRead arms the seam that read points at once the tail has
// read 777 inside detach_disk's lifecycle, and the seam then fails its next
// read.
func (s *strandedDisk) failReadAfterTailRead(id string, w *tailWrites, read *func() error, failure func() error) *int {
	failed := 0
	armed := false
	w.afterSourceRead = func() {
		if failed == 0 && s.detachAdmitted(id) {
			armed = true
		}
	}
	*read = func() error {
		if !armed {
			return nil
		}
		armed = false
		failed++
		return failure()
	}
	return &failed
}

// TestDetachTailGuardReadFailureReturnsTheDisk starts from a latent record and
// reruns detach_disk after the disk is already parked. The guard's read of
// 777's config fails just before the tail's removal would go out. The guard
// sends nothing until that read answers, so the call comes back retriable with
// the record returned, the disk on its parker, and 777's entry in place. Once
// the read answers again, the next detach_disk removes the entry.
func TestDetachTailGuardReadFailureReturnsTheDisk(t *testing.T) {
	for name, failure := range map[string]error{
		"transport": errors.New("connection reset by peer"),
		"forbidden": pveAnswer(403, "Permission check failed (/vms/777, VM.Audit)"),
	} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildLatentTailDisk(t)
			parker := s.requireSingleHolder(t, true)
			steps := configSteps(t, s.journal, id)
			failed := s.failGuardReadAfterTailRead(id, w, func() error { return failure })

			err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
			requireRetriable(t, err, "detach_disk whose guard read of 777 failed")
			if *failed != 1 || w.conflicts != 0 || w.removals != 0 {
				t.Fatalf("failed reads=%d conflicts=%d removals=%d, want one failed read and no write sent to 777", *failed, w.conflicts, w.removals)
			}
			if after := configSteps(t, s.journal, id); after != steps {
				t.Fatalf("the refused detach_disk journaled %d config steps, want none", after-steps)
			}
			if vmid := s.requireSingleHolder(t, true); vmid != parker {
				t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
			}
			if !s.hasEntry(777) {
				t.Fatal("777's entry went missing although no write was sent")
			}
			s.requireRecord(t, id, aj.ReadyToReturn)

			w.afterSourceRead, w.configRead = nil, nil
			if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
				t.Fatalf("detach_disk once 777's config reads again: %v", err)
			}
			if w.removals != 1 {
				t.Fatalf("777's entry was removed %d times, want once", w.removals)
			}
			s.requireSourceClean(t)
			s.requireRecord(t, id, aj.ReadyToReturn)
		})
	}
}

// TestDetachTailGuardIdentityReadFailureReturnsTheDisk is the same rerun
// detach_disk as the test above, but the failed read is the guard's read of the
// cluster's identity, which the guard makes before it reads 777's config. The
// guard sends nothing until that read answers, so the call comes back
// retriable with the record returned, and 777's entry stays for the next call.
// Whatever class the read's cause had, the guard returns it as retriable.
func TestDetachTailGuardIdentityReadFailureReturnsTheDisk(t *testing.T) {
	for name, failure := range map[string]error{
		"transport": errors.New("connection reset by peer"),
		"forbidden": pveAnswer(403, "Permission check failed (/nodes/n1, Sys.Audit)"),
	} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildLatentTailDisk(t)
			parker := s.requireSingleHolder(t, true)
			steps := configSteps(t, s.journal, id)
			failed := s.failGuardIdentityAfterTailRead(id, w, func() error { return failure })

			err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
			requireRetriable(t, err, "detach_disk whose guard read of the cluster identity failed")
			if *failed != 1 || w.conflicts != 0 || w.removals != 0 {
				t.Fatalf("failed reads=%d conflicts=%d removals=%d, want one failed identity read and no write sent to 777", *failed, w.conflicts, w.removals)
			}
			if after := configSteps(t, s.journal, id); after != steps {
				t.Fatalf("the refused detach_disk journaled %d config steps, want none", after-steps)
			}
			if vmid := s.requireSingleHolder(t, true); vmid != parker {
				t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
			}
			if !s.hasEntry(777) {
				t.Fatal("777's entry went missing although no write was sent")
			}
			s.requireRecord(t, id, aj.ReadyToReturn)

			w.afterSourceRead, w.identityRead = nil, nil
			if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
				t.Fatalf("detach_disk once the cluster identity reads again: %v", err)
			}
			if w.removals != 1 {
				t.Fatalf("777's entry was removed %d times, want once", w.removals)
			}
			s.requireSourceClean(t)
			s.requireRecord(t, id, aj.ReadyToReturn)
		})
	}
}

// TestDetachTailGuardReadFailureOnOtherWriteLeavesTheGuardUsable checks a
// description-only write to 777 that isn't the detach tail's removal and that
// meets a failed read of 777's config before it goes out. Nothing is sent, so
// the write comes back retriable and marked as not attempted, and the guard
// stays usable. The refusal is the guard's general one and not the tail's, and
// the read's cause stays in the text only. Nothing else in the operation
// touched the disk, so finishing the lifecycle returns the allocation
// unchanged rather than leaving it for reconciliation.
func TestDetachTailGuardReadFailureOnOtherWriteLeavesTheGuardUsable(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	rd, err := resolveDeleteDiskCID(context.Background(), s.deps, s.cid)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(context.Background(), s.deps, rd, "delete_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("admission: %v", err)
	}
	description := pve.DescriptionFromConfig(s.client.state.configs[777]) + "\nedited by hand"
	digest, _ := pve.ConfigString(s.client.state.configs[777], "digest")
	w.configRead = func() error { return errors.New("connection reset by peer") }

	writeErr := local.PVE.Nodes().UpdateQemuConfig(context.Background(), "n1", "777", &nodes.UpdateQemuConfigParams{Description: &description, Digest: &digest})
	guardErr := lifecycle.guard.Err()
	w.configRead = nil
	finished := lifecycle.finish(context.Background(), writeErr, false)
	if writeErr == nil || guardErr != nil {
		t.Fatalf("write err=%v guard err=%v, want the write refused and the guard left usable", writeErr, guardErr)
	}
	requireRetriable(t, writeErr, "description write whose guard read of 777's config failed")
	if !errors.Is(writeErr, errManagedAdmissionReadFailed) || !errors.Is(writeErr, pve.ErrMutationNotAttempted) || errors.Is(writeErr, errManagedTailReadFailed) {
		t.Fatalf("write err = %v, want the guard's admission read refusal, marked as not attempted", writeErr)
	}
	if text := writeErr.Error(); !strings.Contains(text, "nothing was sent; retry the operation") || !strings.Contains(text, "connection reset by peer") {
		t.Fatalf("write err = %q, want it to say nothing was sent, to retry, and why the read failed", text)
	}
	if w.conflicts != 0 || w.removals != 0 || pve.DescriptionFromConfig(s.client.state.configs[777]) == description {
		t.Fatalf("conflicts=%d removals=%d, want nothing sent to 777 by this description-only write, which isn't the tail's removal", w.conflicts, w.removals)
	}
	if !isDiskReturnedUnchanged(finished) {
		t.Fatalf("finish = %v, want the allocation returned unchanged after a refusal that sent nothing", finished)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailGuardReadEndedContextReturnsTheDisk starts from a latent record
// and reruns detach_disk, and the request ends while the guard reads 777's
// config, just before the tail's removal would go out. Nothing is sent, so the
// call comes back with the ordinary retriable deadline error, the record is
// returned unchanged, and 777's entry stays for the next call.
func TestDetachTailGuardReadEndedContextReturnsTheDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := s.failGuardReadAfterTailRead(id, w, func() error {
		cancel()
		return ctx.Err()
	})

	err := detachDiskAt(t, ctx, s.deps, "777", s.cid)
	if err == nil {
		t.Fatal("detach_disk succeeded although its request ended during the guard's read")
	}
	if *failed != 1 || w.conflicts != 0 || w.removals != 0 {
		t.Fatalf("failed reads=%d conflicts=%d removals=%d, want one failed read and no write sent to 777", *failed, w.conflicts, w.removals)
	}
	requireRetriable(t, err, "detach_disk whose request ended during the guard's read")
	if !errors.Is(err, errManagedRequestEnded) || !errors.Is(err, context.Canceled) || !isDiskReturnedUnchanged(err) {
		t.Fatalf("err = %v, want the ended request returned unchanged", err)
	}
	if isDetachTailNotSent(err) {
		t.Fatalf("err = %v, want the ended request's refusal and not the tail's own not-sent refusal", err)
	}
	if strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("err = %v, want the ordinary deadline error with no reconciliation", err)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although no write was sent")
	}

	w.afterSourceRead, w.configRead = nil, nil
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk once the request runs to the end: %v", err)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailReadbackFailureAfterWriteStaysForReconciliation starts from a
// latent record and runs delete_disk, which sends the tail's removal. PVE
// applies the removal, and the readback of 777 then gets a 403. The 403 answers
// the read and not the write, so it can't show that the write changed nothing.
// The error carries neither marker, the record is left for reconciliation, and
// the volume stays.
func TestDetachTailReadbackFailureAfterWriteStaysForReconciliation(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	written, failed := false, 0
	ctx := withTailHook(context.Background(), func(event tailEvent) {
		if event == tailWritten {
			written = true
		}
	})
	w.configRead = func() error {
		if !written || failed > 0 {
			return nil
		}
		failed++
		return pveAnswer(403, "Permission check failed (/vms/777, VM.Audit)")
	}

	err := deleteDiskAt(t, ctx, s.deps, s.cid)
	if err == nil {
		t.Fatal("delete_disk succeeded although the readback of its removal failed")
	}
	if failed != 1 || w.removals != 1 {
		t.Fatalf("failed readbacks=%d removals=%d, want the write applied and its readback refused once", failed, w.removals)
	}
	if isDetachTailNotSent(err) || isDiskReturnedUnchanged(err) {
		t.Fatalf("err = %v, want a failed readback after an accepted write left unmarked", err)
	}
	s.requireRecord(t, id, aj.ReconciliationRequired)
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
	}
}

// TestDetachTailReadbackFailureAfterWriteHealsOnRerun runs the same failure
// as the test above and then runs delete_disk again. The first call fails
// with a retriable error, which is the class the Director retries on, and the
// record waits in reconciliation_required. Nothing else changes between the
// two calls. The rerun's admission takes the record, because every step the
// first call journaled is observed. The tail then reads 777, finds its entry
// already gone, and sends no second removal, and the completion audit lets the
// delete finish. So the record ends deleted and the volume is gone.
func TestDetachTailReadbackFailureAfterWriteHealsOnRerun(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	written, failed := false, 0
	ctx := withTailHook(context.Background(), func(event tailEvent) {
		if event == tailWritten {
			written = true
		}
	})
	w.configRead = func() error {
		if !written || failed > 0 {
			return nil
		}
		failed++
		return pveAnswer(403, "Permission check failed (/vms/777, VM.Audit)")
	}

	err := deleteDiskAt(t, ctx, s.deps, s.cid)
	requireRetriable(t, err, "delete_disk whose readback failed after an accepted write")
	if failed != 1 || w.removals != 1 {
		t.Fatalf("failed readbacks=%d removals=%d, want the write applied and its readback refused once", failed, w.removals)
	}
	s.requireRecord(t, id, aj.ReconciliationRequired)
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
	}

	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("the plain delete_disk rerun: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	if s.client.state.volumes[s.stranded] != nil {
		t.Fatalf("%s survived the rerun", s.stranded)
	}
	s.requireSourceClean(t)
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want the rerun to send no second removal", w.removals)
	}
}
