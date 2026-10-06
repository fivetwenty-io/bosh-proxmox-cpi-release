package handlers

import (
	"context"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"sort"
	"time"
)

// managedDiskLifecycle owns one allocation lock for an entire disk operation.
// Its PVE observations always use original request dependencies.
type managedDiskLifecycle struct {
	// external records legacy persistent-disk preservation under a held VM
	// allocation. Such evidence never establishes ownership by that VM.
	external        bool
	ownedRetention  bool
	retainedBytes   uint64
	externalNode    string
	externalBacking string
	requestContext  context.Context
	guard           *ManagedAllocationGuard
	deps            Deps
	disk            resolvedDisk
	journal         *aj.Journal
	handle          *aj.Handle
	session         *storageLifecycle
	// diskMutationAdmitted records that, during this operation, the guard
	// admitted a mutation other than a bosh-lock- sentinel create or delete, a
	// write of the drive-option overlay note alone (see
	// lifecycleOverlayOnlyConfigWrite), or a pinned removal of notes from a VM
	// that doesn't name the disk (see lifecycleMutationTouchesDisk).
	diskMutationAdmitted bool
	// pendingDeleteReverted records that the guard observed the revert of a
	// slot delete PVE could only record as pending, after it had settled that
	// delete as not applied, during this operation (see cleanPendingDelete).
	pendingDeleteReverted bool
	// holders is the guard's hook state, which records the holders this
	// operation created. createdHolder answers from it.
	holders *managedDiskLifecycleGuard
}

func acquireManagedDiskLifecycle(ctx context.Context, deps Deps, rd resolvedDisk, operation string) (*managedDiskLifecycle, error) {
	if rd.allocation == nil {
		return nil, nil
	}
	if rd.allocation.terminalAbsent || rd.allocation.absent {
		return nil, fmt.Errorf("terminal managed disk cannot be mutated")
	}
	node := rd.allocation.provenance.Node
	journal, err := openStorageAllocationJournal(ctx, deps, []string{node})
	if err != nil {
		return nil, err
	}
	handle, err := journal.Acquire(ctx, rd.allocation.record.ID)
	if err != nil {
		return nil, errors.Join(err, journal.Close())
	}
	fail := func(err error) (*managedDiskLifecycle, error) {
		return nil, errors.Join(err, handle.Close(), journal.Close())
	}
	// The earlier read-only lookup did not acquire generation ownership. Repeat
	// the complete targeted identity read after taking the allocation lock.
	current, err := resolveDiskForOp(ctx, deps, operation, rd.diskCID, rd.birth, rd.meta)
	if err != nil {
		return fail(err)
	}
	if current.allocation == nil || current.allocation.record.ID != handle.Record().ID {
		return fail(fmt.Errorf("managed disk identity changed before lifecycle admission"))
	}
	proof, err := managedDiskOwnershipProof(current)
	if err != nil {
		return fail(err)
	}
	// A lock step an earlier request left planned is settled by readback
	// before readmission judges the record, so a retry is not refused for a
	// sentinel that never held anything of ours.
	gaps, err := settlePlannedLockSteps(ctx, deps, handle)
	if err != nil {
		return fail(err)
	}
	if text := unsettledStepText(handle.Record(), gaps, func(step aj.Step) bool { return step.Attempt != handle.Record().ActiveAttempt() }); text != "" && len(gaps) > 0 {
		return fail(protectionPendingOr(handle.Record(), gaps, storageRefusal("lifecycle has unresolved mutation evidence; "+text)))
	}
	session, err := beginStorageLifecycle(handle, operation, proof)
	if err != nil {
		return fail(err)
	}
	return &managedDiskLifecycle{requestContext: ctx, deps: deps, disk: current, journal: journal, handle: handle, session: session}, nil
}

// managedDiskOwnershipProof certifies targeted live ownership only. It never
// certifies absence of historical artifacts or cluster-wide scan completeness.
func managedDiskOwnershipProof(rd resolvedDisk) (aj.Verification, error) {
	if rd.allocation == nil || rd.allocation.absent || rd.allocation.terminalAbsent {
		return aj.Verification{}, fmt.Errorf("managed ownership proof requires validated identity")
	}
	id, body, err := aj.VerificationEvidence(map[string]any{
		"observed_at": time.Now().UTC(), "scope": "targeted_live_disk_ownership",
		"allocation_id": rd.allocation.record.ID, "namespace": rd.allocation.record.Namespace,
		"volume": rd.volid, resourceTypeNode: rd.allocation.provenance.Node, "backing": rd.allocation.provenance.Backing,
	})
	if err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{EvidenceID: id, EvidenceJSON: body, Complete: true, OwnershipVerified: true}, nil
}

// finish records success only after independent current ownership or complete
// historical absence evidence. An operation error leaves reconciliation visible.
func (m *managedDiskLifecycle) finish(ctx context.Context, operationErr error, deleted bool) error {
	if m == nil {
		return operationErr
	}
	cleanTimeout := m.cleanLockTimeout(operationErr)
	cleanTail := !cleanTimeout && m.cleanTailRefusal(operationErr)
	cleanPending := !cleanTimeout && !cleanTail && m.cleanPendingDelete(operationErr)
	if m.guard != nil {
		operationErr = errors.Join(operationErr, m.guard.Err())
	}
	var finalErr error
	switch {
	case cleanPending:
		// A pending change on the slot stopped the operation, and either the
		// guard observed our delete settling as not applied and its revert, or
		// nothing was sent to the slot, so the disk is where the operation
		// found it. The allocation goes back to the Director the
		// way a success returns it, and the refusal goes back unchanged.
		finalErr = m.completeOwned(ctx, false)
		if finalErr != nil {
			finalErr = errors.Join(finalErr, m.session.Uncertain("completion audit after a reverted pending delete failed"))
		}
	case cleanTimeout:
		// The wait ran out before this operation changed anything it cannot
		// account for, so the allocation is returned to the Director exactly as
		// a success would return it, and the retriable timeout goes back for
		// the Director to retry. When the request's context has already
		// ended, the completion runs on a detached, bounded context instead,
		// because every read it makes would otherwise fail at once. That path
		// is only ever taken for a clean exit, never after an operation error.
		completionCtx := ctx
		if ctx.Err() != nil {
			detached, cancel := detachedContext(ctx, pve.ClusterLockCompletionAllowance)
			defer cancel()
			completionCtx = detached
		}
		finalErr = m.completeOwned(completionCtx, false)
		if finalErr != nil {
			finalErr = errors.Join(finalErr, m.session.Uncertain("completion audit after a lock timeout failed"))
		}
	case cleanTail:
		// The detach tail stopped before it changed the source VM, and
		// nothing else in this operation touched the disk. So the
		// allocation returns to the Director the way a success returns
		// it. The tail's error also goes back for the Director to retry.
		finalErr = m.completeOwned(ctx, false)
		if finalErr != nil {
			finalErr = errors.Join(finalErr, m.session.Uncertain("completion audit after a detach tail refusal failed"))
		}
	case operationErr != nil:
		finalErr = m.session.Uncertain("operation did not complete")
	default:
		finalErr = m.completeOwned(ctx, deleted)
		if finalErr != nil {
			finalErr = errors.Join(finalErr, m.session.Uncertain("completion audit failed"))
		}
	}
	closeErr := errors.Join(m.handle.Close(), m.journal.Close())
	result := errors.Join(operationErr, finalErr, closeErr)
	returned := (cleanTimeout || cleanTail || cleanPending) && finalErr == nil && closeErr == nil
	if result != nil && !returned {
		m.deps.recordStorageReconciliation(ctx, "required")
	}
	if returned && cleanTimeout {
		return &diskReturnedAfterLockTimeout{err: result}
	}
	if returned && cleanTail {
		return &diskReturnedAfterTailRefusal{err: result}
	}
	return result
}

// managedLockWaitContext lets a journal-managed operation wait out a whole
// parker window that another request holds. The default wait is 15 seconds,
// while a holder's window may run for most of the lock's TTL, which is 235
// seconds on the shipped retry curves and longer when pve.retry lengthens
// them. The window ends early enough before the claim's recorded expiry to
// leave time for the sweep, the protection restore, and the release. A waiter
// that gives up early fails its Director task even though nothing went wrong.
// Waiting a full TTL is the shortest wait that outlasts any single holder,
// live or crashed, because a claim that outlives its TTL is stolen. A queue of
// several holders can still outlast it, and that timeout is settled cleanly by
// cleanLockTimeout.
//
// The wait is the lock's TTL under the retry curves configured now, from
// pve.ParkerProtectionLockTTLNow, unless the context carries a shorter one under
// managedLockWaitKey, which only tests set. The wait is a context value, and it
// must never become a context deadline. failConfirmingReads in the tests tells
// the acquire's confirming reads from the reads on its way out only by whether
// their context has a deadline, so a request context that gained one would
// make every confirming read answer, and the tests that drive the unknown lock
// state would no longer reach it.
func managedLockWaitContext(ctx context.Context) context.Context {
	wait := pve.ParkerProtectionLockTTLNow()
	if d, ok := ctx.Value(managedLockWaitKey{}).(time.Duration); ok && d > 0 {
		wait = d
	}
	return pve.WithParkerLockWait(ctx, wait)
}

// managedLockWaitKey carries a test's shorter managed lock wait on the request
// context. Only tests set it, so production always waits the lock's TTL.
type managedLockWaitKey struct{}

// diskReturnedAfterLockTimeout marks a disk operation that failed only because
// a cluster lock wait ran out, after which finish returned the disk's
// allocation unchanged. A caller that holds its own allocation around the disk
// operation, such as create_vm's pre-attach or delete_vm's disk preservation,
// reads the marker to tell a clean wait-out from an uncertain outcome. The
// marker wraps the original error, so its CPI type and the timeout sentinel
// stay visible.
type diskReturnedAfterLockTimeout struct{ err error }

func (e *diskReturnedAfterLockTimeout) Error() string { return e.err.Error() }
func (e *diskReturnedAfterLockTimeout) Unwrap() error { return e.err }

// isDiskReturnedAfterLockTimeout reports whether err carries that marker.
func isDiskReturnedAfterLockTimeout(err error) bool {
	var returned *diskReturnedAfterLockTimeout
	return errors.As(err, &returned)
}

// diskReturnedAfterTailRefusal marks a disk operation that failed only because
// the detach tail left the source VM as it found it, after which finish
// returned the disk's allocation unchanged. create_vm's pre-attach reads it
// the way it reads diskReturnedAfterLockTimeout. The marker wraps the original
// error, so its CPI type stays visible.
type diskReturnedAfterTailRefusal struct{ err error }

func (e *diskReturnedAfterTailRefusal) Error() string { return e.err.Error() }
func (e *diskReturnedAfterTailRefusal) Unwrap() error { return e.err }

// isDiskReturnedUnchanged reports whether err carries either marker, so the
// disk operation returned the disk's allocation unchanged.
func isDiskReturnedUnchanged(err error) bool {
	var refused *diskReturnedAfterTailRefusal
	return isDiskReturnedAfterLockTimeout(err) || errors.As(err, &refused)
}

// completeOwned closes the session with fresh evidence of the disk's current
// disposition: its absence after a delete, and its ownership otherwise.
func (m *managedDiskLifecycle) completeOwned(ctx context.Context, deleted bool) error {
	// A protection restore that failed without an answer from PVE and then
	// landed on a retry leaves the first attempt's step planned. Settle such
	// steps by reading the parker back before completion judges the record,
	// through the same settler every readmission runs.
	gaps, err := settlePlannedProtectionSteps(ctx, m.deps, m.handle, nil, nil)
	if err != nil {
		return err
	}
	if len(gaps) > 0 {
		record := m.handle.Record()
		return storageRefusal("lifecycle has unresolved mutation evidence; " + unsettledStepText(record, gaps, func(step aj.Step) bool { return step.Attempt != record.ActiveAttempt() }))
	}
	var proof aj.Verification
	if deleted {
		proof, err = m.absentDeletionProof(ctx)
	} else {
		var current resolvedDisk
		current, err = resolveDiskForOp(ctx, m.deps, "lifecycle_complete", m.disk.diskCID, m.disk.birth, m.disk.meta)
		if err == nil {
			proof, err = managedDiskOwnershipProof(current)
		}
	}
	if err != nil {
		return err
	}
	return m.session.Finish(proof, deleted)
}

// cleanLockTimeout reports whether an operation failed only because a cluster
// lock wait ran out, before it changed the disk. That takes four things. The
// failure carries pve.ErrClusterLockTimeout. The guard was never poisoned, and
// it admitted only sentinel creates and deletes, writes of the drive-option
// overlay note, and pinned note removals from a VM that doesn't name the disk.
// Every step the operation journaled has been observed. A timeout is positive
// evidence that another request held the lock throughout, so this request
// never entered the window it was waiting for. An operation that moved or
// migrated the disk before it waited has changed it, even when every step
// settled, so it still goes uncertain.
//
// A request whose context ended counts the same way. pve.ErrClusterLockInterrupted
// is a lock wait that a cancelled request cut short, and errManagedRequestEnded
// is a mutation the guard refused on an ended request before it reached PVE.
// Neither changed anything, so the same conditions decide.
//
// pve.ErrClusterLockStateUnknown counts the same way. The acquire could not
// tell who holds the lock, because no read of its new sentinel answered before
// the deadline, its create ended without an answer, or PVE said the sentinel
// exists and a read or steal that would judge its holder failed. In each case
// it never entered the window either. When the read on its way out proved the
// sentinel ours and the guarded delete answered, both steps are observed and
// the record is returned like any clean timeout. When that delete did not
// answer, its step stays planned and the guard is poisoned, so the other
// conditions send the record to reconciliation, and the next call's readback
// settles the step.
//
// pve.ErrClusterLockClaimTooShort counts the same way. The acquire confirmed
// its claim too late to use it and gave it up without entering the window,
// and on its way out it reads and deletes its sentinel exactly as the
// unknown state does, so the same steps decide.
func (m *managedDiskLifecycle) cleanLockTimeout(operationErr error) bool {
	if operationErr == nil {
		return false
	}
	if !isLockWaitWithoutEntry(operationErr) {
		return false
	}
	if m.guard == nil || m.guard.Err() != nil || m.handle == nil || m.diskMutationAdmitted {
		return false
	}
	return storageLifecycleSettled(m.handle.Record()) == nil
}

// isLockWaitWithoutEntry reports whether err is a lock acquire that gave up
// without entering the work its lock guards. That is a wait that ran out
// (pve.ErrClusterLockTimeout), an acquire that could not tell who holds the
// lock (pve.ErrClusterLockStateUnknown), one that confirmed its claim too late
// to use it (pve.ErrClusterLockClaimTooShort), a wait a cancelled request cut
// short (pve.ErrClusterLockInterrupted), and a mutation the guard refused on
// an ended request (errManagedRequestEnded). It is only the first test a clean
// timeout passes. The callers also judge the guard and the journaled steps.
func isLockWaitWithoutEntry(err error) bool {
	return errors.Is(err, pve.ErrClusterLockTimeout) || errors.Is(err, pve.ErrClusterLockStateUnknown) ||
		errors.Is(err, pve.ErrClusterLockClaimTooShort) || errors.Is(err, pve.ErrClusterLockInterrupted) ||
		errors.Is(err, errManagedRequestEnded)
}

// cleanTailRefusal reports whether an operation failed only because the
// detach tail left the source VM as it found it, which detachTailNotSent marks.
// The same conditions as cleanLockTimeout apply. The guard must never have
// been poisoned, it must never have admitted a disk mutation during the
// operation, and every step the operation journaled must be observed. So a
// tail that runs after a move in the same call never counts.
func (m *managedDiskLifecycle) cleanTailRefusal(operationErr error) bool {
	if !isDetachTailNotSent(operationErr) {
		return false
	}
	if m.guard == nil || m.guard.Err() != nil || m.handle == nil || m.diskMutationAdmitted {
		return false
	}
	return storageLifecycleSettled(m.handle.Record()) == nil
}

// cleanPendingDelete reports whether an operation failed only because of a
// pending change on the disk's slot, which leaves the disk where the operation
// found it. The failure's chain has to hold a *pve.DriveDeletePendingError
// whose reason isn't an unconfirmed revert, so no other failure rides along on
// the clean return. The guard must never have been poisoned, and every step
// the operation journaled must be observed. When the reason says our own
// delete stayed pending, the guard must also have observed the revert after
// it had settled the delete as not applied. A pending delete the resume found
// and left alone, and a slot whose pending value replaces its drive, need no
// revert, because nothing was sent to the slot. For those two the guard must
// also never have admitted a disk mutation during the operation, the same
// proof cleanLockTimeout asks for, so the clean return rests on what the guard
// saw rather than on the reason alone.
func (m *managedDiskLifecycle) cleanPendingDelete(operationErr error) bool {
	pending, ok := pve.IsDriveDeletePending(operationErr)
	if !ok || pending.Reason == pve.DriveDeletePendingRevertUnconfirmed {
		return false
	}
	sentNothing := pending.Reason == pve.DriveDeletePendingFound || pending.Reason == pve.DriveDeletePendingReplaced
	if sentNothing && m.diskMutationAdmitted {
		return false
	}
	if !sentNothing && !m.pendingDeleteReverted {
		return false
	}
	if m.guard == nil || m.guard.Err() != nil || m.handle == nil {
		return false
	}
	return storageLifecycleSettled(m.handle.Record()) == nil
}

func (m *managedDiskLifecycle) deletionProof(ctx context.Context) (aj.Verification, error) {
	report, err := m.deletionAudit(ctx)
	if err != nil {
		return aj.Verification{}, err
	}
	return m.judgeDeletion(ctx, report)
}

// absentDeletionProof is the form of deletionProof that delete_disk uses.
// Between the audit and judgeDeletion it removes this disk's own notes from
// each VM that carries them and holds the volume nowhere
// (removeAbsentDiskNotes), and it audits again when it has removed any, so the
// verdict rests on a fresh audit.
func (m *managedDiskLifecycle) absentDeletionProof(ctx context.Context) (aj.Verification, error) {
	report, err := m.deletionAudit(ctx)
	if err != nil {
		return aj.Verification{}, err
	}
	removed, unsettled, err := removeAbsentDiskNotes(ctx, m.deps, m.disk, m.handle.Record(), report)
	if err != nil {
		return aj.Verification{}, err
	}
	if removed {
		if report, err = m.deletionAudit(ctx); err != nil {
			return aj.Verification{}, err
		}
	}
	verification, err := m.judgeDeletion(ctx, report)
	if err != nil && unsettled != nil {
		return verification, fmt.Errorf("%w; every VM keeps the disk's notes while step %s (%s) of the record's active attempt is %s",
			err, unsettled.ID, unsettled.Kind, unsettled.State)
	}
	return verification, err
}

// deletionAudit runs the allocation audit over every node the record's steps
// name.
func (m *managedDiskLifecycle) deletionAudit(ctx context.Context) (StorageAllocationAudit, error) {
	nodes := map[string]bool{}
	record := m.handle.Record()
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.Node != "" {
			nodes[step.Target.Node] = true
		}
	}
	nodeList := make([]string, 0, len(nodes))
	for node := range nodes {
		nodeList = append(nodeList, node)
	}
	sort.Strings(nodeList)
	return AuditStorageAllocations(ctx, m.deps, m.journal, nodeList)
}

// judgeDeletion certifies the disk's absence from report, or refuses when the
// audit is gated or still finds anything of the allocation.
func (m *managedDiskLifecycle) judgeDeletion(ctx context.Context, report StorageAllocationAudit) (aj.Verification, error) {
	if err := storageAuditGateError(ctx, m.deps, "delete_disk", report, storageAuditGateAll); err != nil {
		return aj.Verification{}, err
	}
	for _, e := range report.Evidence {
		if e.AllocationID == m.handle.Record().ID {
			return aj.Verification{}, fmt.Errorf("managed disk artifact or provenance remains; reconciliation required")
		}
	}
	id, body, err := aj.VerificationEvidence(map[string]any{
		"started_at": report.StartedAt, "completed_at": report.CompletedAt,
		"complete": report.Complete, "vm_scan_complete": report.VMScanComplete,
		"allocation_id": m.handle.Record().ID, "evidence": report.Evidence,
		"issues": report.Issues, "conflicts": report.Conflicts,
	})
	if err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{EvidenceID: id, EvidenceJSON: body, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}, nil
}

// wrapManagedDiskClient builds the client every managed disk operation runs
// on, which is the lifecycle decorator over the allocation guard's own
// decorator. Every path that needs that chain builds it here, so the chain has
// one definition and the tests that read it exercise the one production uses.
//
// unguardedPVE has to be able to walk back out of every decorator this
// function adds, because the parker pool sweep runs on the client underneath
// them all. A decorator added here therefore needs an unguardedClient method of
// its own and a case in TestUnguardedPVE_WalksOutOfEveryAllocationGuardDecorator.
func wrapManagedDiskClient(guard *ManagedAllocationGuard, m *managedDiskLifecycle) pve.Client {
	return &managedDiskLifecycleClient{Client: guard.Client(), lifecycle: m}
}

// managedDiskOperation returns request-local mutation services. Read-only
// handlers must continue using resolveDiskForOp without this acquisition path.
func managedDiskOperation(ctx context.Context, deps Deps, rd resolvedDisk, operation string) (Deps, *managedDiskLifecycle, error) {
	m, err := acquireManagedDiskLifecycle(ctx, deps, rd, operation)
	if err != nil || m == nil {
		return deps, m, err
	}
	guard, err := newManagedDiskLifecycleGuard(m)
	if err != nil {
		return deps, nil, m.finish(ctx, err, false)
	}
	m.guard = guard
	local := deps
	local.PVE = wrapManagedDiskClient(guard, m)
	if deps.Config != nil {
		copied := *deps.Config
		disabled := false
		copied.FastPathDelete = &disabled
		local.Config = &copied
	}
	return local, m, nil
}

func finalizeAbsentManagedDisk(ctx context.Context, deps Deps, rd resolvedDisk) error {
	if rd.allocation == nil || !rd.allocation.absent {
		return fmt.Errorf("absence finalization requires observed missing managed volume")
	}
	journal, err := openStorageAllocationJournal(ctx, deps, []string{rd.allocation.provenance.Node})
	if err != nil {
		return err
	}
	handle, err := journal.Acquire(ctx, rd.allocation.record.ID)
	if err != nil {
		return errors.Join(err, journal.Close())
	}
	m := &managedDiskLifecycle{requestContext: ctx, deps: deps, disk: rd, journal: journal, handle: handle, session: &storageLifecycle{handle: handle, operation: "delete_disk"}}
	// No new mutation is submitted. Steps the settlement dispatcher can prove
	// by readback, a lock sentinel or a parker's protection, are settled first,
	// exactly as a readmission settles them; unknown prior tasks still block
	// finalization.
	gaps, err := settlePlannedLockSteps(ctx, deps, handle)
	if err != nil {
		return errors.Join(err, handle.Close(), journal.Close())
	}
	if len(gaps) > 0 {
		record := handle.Record()
		text := "lifecycle has unresolved mutation evidence; " + unsettledStepText(record, gaps, func(step aj.Step) bool { return step.Attempt != record.ActiveAttempt() })
		refusal := storageRefusal(text)
		if hasMoveSettlementGap(gaps) {
			// A move step the settler left planned refuses as a Cloud error
			// that is not retried. That's the answer the Director got when
			// storageLifecycleSettled refused the step as a plain error, and
			// the typed error gives it on purpose.
			refusal = cpierrors.Cloud("%s", text)
		}
		return errors.Join(refusal, handle.Close(), journal.Close())
	}
	if err := storageLifecycleSettled(handle.Record()); err != nil {
		return errors.Join(err, handle.Close(), journal.Close())
	}
	return m.finish(ctx, nil, true)
}
