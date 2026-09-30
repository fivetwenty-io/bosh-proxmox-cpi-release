package handlers

import (
	"context"
	"fmt"
	"strings"

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
// after the delete strict.
func (t *managedPoolService) lockDeletionClaim(ctx context.Context, poolID string) managedLockPoolDeletion {
	comment, found, err := t.GetPoolComment(ctx, poolID)
	return managedLockPoolDeletion{poolID: poolID, comment: comment, known: err == nil && found}
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
	comment, found, err := pools.GetPoolComment(ctx, pool)
	if err != nil {
		return fmt.Errorf("cannot verify %s lock mutation", label)
	}
	if call.Method == "CreatePool" {
		// PVE accepted the create, so it happened. A readback that finds our
		// claim confirms it. One that finds the sentinel gone, or holding
		// another claim, means a stealer displaced us after the create, which
		// is contention and not uncertainty: nothing of ours remains, and the
		// lock code's own verification decides whether we hold the lock.
		return nil
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
