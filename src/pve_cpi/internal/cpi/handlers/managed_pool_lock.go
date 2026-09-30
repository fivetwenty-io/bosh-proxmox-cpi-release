package handlers

import (
	"context"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A cluster lock is a bosh-lock- sentinel pool, and contention on it is normal.
// A second acquirer's create is refused because the holder's sentinel exists,
// and a release or a steal can find the sentinel already gone. PVE raises both
// refusals under its user.cfg lock before it writes anything. Under a guard,
// such a refusal is settled as an observed non-mutation and handed back
// unchanged, so the lock code classifies it the way it always has, as "held,
// wait" and "already released". A create refusal counts as proof only once a
// readback shows the sentinel does not hold our own claim. Any other outcome
// stays uncertain and fails closed.

// managedLockPoolRejection is the After result for a sentinel mutation PVE
// refused before changing anything.
type managedLockPoolRejection struct{ poolID, method string }

// managedLockPoolDeletion is the After result for a sentinel delete that PVE
// accepted. It carries the claim that a read just before the delete found in
// the sentinel. The claim lives only in memory and is not journaled. A readback
// that finds a different claim therefore sees a sentinel created after that
// read, which can only exist once the one we deleted is gone.
type managedLockPoolDeletion struct {
	poolID, comment string
	known           bool
}

func isManagedLockPool(poolID string) bool {
	return strings.HasPrefix(poolID, reservedPoolLockPrefix)
}

// lockCreateRefused reports whether a failed sentinel create is proven to have
// changed nothing. The exact duplicate verdict is the proof, and the readback
// rules out the one case it cannot cover. A create whose committed response was
// lost and whose replay drew the duplicate verdict would leave our own claim in
// the sentinel. The SDK never replays a POST today, so the readback is defense
// in depth.
//
// The readback must answer exactly. A clean read is compared with our claim,
// PVE's exact missing-pool verdict means the holder has since released, and
// anything else, including a loosely worded "does not exist", leaves the
// refusal unproven.
//
// Comparing claims relies on one invariant. Every owner token that reaches a
// sentinel names its process, because the parker, VMID, and anti-affinity
// owners all go through pve.ProcessLockOwner, which adds the host, the pid, a
// random per-process nonce, and a per-process sequence. So two acquirers never
// write the same claim, even on two Directors that share a hostname. A new lock owner must keep that true, or a waiter that shares a
// holder's claim reads the holder's sentinel as its own create and poisons its
// allocation.
//
// It also returns the claim the readback found, which is empty when the
// holder had already released.
func (t *managedPoolService) lockCreateRefused(ctx context.Context, poolID, comment string, err error) (string, bool) {
	if !isManagedLockPool(poolID) || !exactPoolAlreadyExists(err, poolID) {
		return "", false
	}
	reader, ok := t.PoolService.(pve.RawPoolCommentReader)
	if !ok {
		return "", false
	}
	held, readErr := reader.ReadPoolComment(ctx, poolID)
	if readErr != nil {
		return "", exactPoolReadMissing(readErr, poolID)
	}
	return held, held != comment
}

// lockRefusal is the claim that refused a guarded sentinel create and the
// refusal PVE returned for it.
type lockRefusal struct {
	claim string
	err   error
}

// rememberLockRefusal keeps the refusal a settled create drew, so the next
// poll of the same sentinel can recognize the same holder. A refusal that did
// not settle, or one whose holder had already released, is not kept.
func (g *ManagedAllocationGuard) rememberLockRefusal(poolID, claim string, refusal error) {
	if claim == "" || refusal == nil || g.poisoned != nil {
		delete(g.lockRefusals, poolID)
		return
	}
	if g.lockRefusals == nil {
		g.lockRefusals = map[string]lockRefusal{}
	}
	g.lockRefusals[poolID] = lockRefusal{claim: claim, err: refusal}
}

func (g *ManagedAllocationGuard) forgetLockRefusal(poolID string) {
	delete(g.lockRefusals, poolID)
}

// repeatLockRefusal answers a lock's poll without a mutation when nothing has
// changed since the last refusal. A waiter polls its sentinel every second or
// so for up to the whole wait, and each guarded create used to run a full
// admission and journal a step, which grew a long-lived disk record by about
// 300 bytes a poll. When the sentinel still holds the exact claim that refused
// the last create, a create now would draw the same refusal, so the guard
// hands that refusal back without calling PVE. No mutation is submitted, so
// there is nothing to admit or journal. Any other answer, including a
// different claim, a missing sentinel, or a failed read, lets the create go
// through the guard as usual, and an expired holder is still stolen through the
// guarded delete. A poisoned guard answers with its poison, as begin would.
func (t *managedPoolService) repeatLockRefusal(ctx context.Context, poolID string) (bool, error) {
	if !isManagedLockPool(poolID) {
		return false, nil
	}
	g := t.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.poisoned != nil {
		return true, g.poisoned
	}
	last, ok := g.lockRefusals[poolID]
	if !ok {
		return false, nil
	}
	reader, ok := t.PoolService.(pve.RawPoolCommentReader)
	if !ok {
		return false, nil
	}
	if held, err := reader.ReadPoolComment(ctx, poolID); err != nil || held != last.claim {
		delete(g.lockRefusals, poolID)
		return false, nil
	}
	return true, last.err
}

// lockDeletionClaim reads the claim a sentinel holds just before it is deleted.
// A claim that cannot be read is recorded as unknown, which keeps the readback
// after the delete strict, and the failed read is returned as well so that a
// delete which expects a particular claim can tell "we could not look" from
// "the claim is gone".
func (t *managedPoolService) lockDeletionClaim(ctx context.Context, poolID string) (managedLockPoolDeletion, error) {
	comment, found, err := t.GetPoolComment(ctx, poolID)
	return managedLockPoolDeletion{poolID: poolID, comment: comment, known: err == nil && found}, err
}

// expectedLockClaimRefusal decides whether a sentinel delete that carries an
// expected claim (pve.WithExpectedLockClaim) must be refused, and with what.
// PVE has no conditional delete, so the guard's read just before the delete is
// the last check. A refusal is returned before PVE is called, which proves the
// delete changed nothing. A delete that carries no expected claim is never
// refused here.
//
// A read that failed is not a changed claim. It comes back as a retriable
// error, so a Release keeps its handle unreleased and can delete on a retry,
// and a steal returns the error instead of quietly giving up.
//
// A claim that differs from the expected one, or a sentinel that is gone,
// comes back as pve.ErrLockClaimChanged, because deleting would remove a claim
// nobody judged. A vanished sentinel gets that answer too, not PVE's not-found,
// so a steal whose holder released in the meantime waits one poll before it
// creates instead of creating at once.
func expectedLockClaimRefusal(ctx context.Context, poolID string, claim managedLockPoolDeletion, readErr error) error {
	expected, ok := pve.ExpectedLockClaim(ctx)
	switch {
	case !ok:
		return nil
	case readErr != nil:
		return cpierrors.WrapAs(readErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("read lock sentinel %q before deleting it", poolID))
	case !claim.known || claim.comment != expected:
		return pve.ErrLockClaimChanged
	}
	return nil
}

// releasePoisonedSentinel releases our own lock sentinel after this guard was
// poisoned. begin refuses every write on a poisoned guard, and a release it
// refused left our claim standing for a whole TTL, so every request waiting on
// that parker stalled behind one uncertain request.
//
// The lock code marks a delete as releasing our own claim only when that claim
// is one it created and read back, and the guard deletes only when its own read
// right before the delete finds exactly that claim, through the same
// expectedLockClaimRefusal a guarded delete runs. A read that fails, or that
// finds any other claim or none, refuses the delete before PVE is called.
//
// Nothing is journaled. The guard is already poisoned, so its allocation
// needs reconciliation whatever happens here, and a sentinel is an empty lock
// marker that holds nothing the allocation owns.
//
// It does not take the guard's mutex. Nothing here reads or writes guard
// state: the read goes to the pool service, the refusal check is pure, and the
// delete is the pool service's own call. Holding the mutex across those two
// PVE calls would only stall every Err caller on this guard. The caller's
// poison check stays correct without it, because a poison is permanent.
func (t *managedPoolService) releasePoisonedSentinel(ctx context.Context, poolID string) error {
	claim, readErr := t.lockDeletionClaim(ctx, poolID)
	if refusal := expectedLockClaimRefusal(ctx, poolID, claim, readErr); refusal != nil {
		return refusal
	}
	return t.PoolService.DeletePool(ctx, poolID)
}

// observeLockPoolMutation proves a guarded sentinel mutation by readback. The
// label names the guard in the errors it returns.
func observeLockPoolMutation(ctx context.Context, pools pve.PoolService, call ManagedAllocationMutation, result any, label string) error {
	pool, _ := call.Args[managedArgumentPoolID].(string)
	if !isManagedLockPool(pool) || (call.Method != "CreatePool" && call.Method != "DeletePool") {
		return fmt.Errorf("%s lock mutation is not a sentinel create or delete", label)
	}
	if refused, ok := result.(managedLockPoolRejection); ok {
		if refused.poolID != pool || refused.method != call.Method {
			return fmt.Errorf("%s lock refusal names another mutation", label)
		}
		return nil
	}
	if call.Method == "CreatePool" {
		// PVE accepted the create, and that acceptance is the observation. A
		// read here could not change the answer, because a sentinel that holds
		// our claim, holds another one, or is already gone all mean the create
		// happened, and a stealer that displaced us afterwards leaves nothing
		// of ours behind. So no read is made, and a read that happens to fail
		// cannot turn an accepted create into an uncertain one. The acquire's
		// own confirming reads decide whether we hold the lock, and they retry
		// a failed read on the lock's poll cadence.
		return nil
	}
	comment, found, err := pools.GetPoolComment(ctx, pool)
	if err != nil {
		return fmt.Errorf("cannot verify %s lock mutation", label)
	}
	if !found {
		return nil
	}
	// A waiter polls for exactly this moment, so its sentinel can land between
	// our delete and this read. A claim that differs from the one read before
	// the delete belongs to a newer sentinel, so the release still counts as
	// an observed delete.
	deleted, ok := result.(managedLockPoolDeletion)
	if ok && deleted.known && deleted.poolID == pool && deleted.comment != comment {
		return nil
	}
	return fmt.Errorf("%s lock deletion not observed", label)
}
