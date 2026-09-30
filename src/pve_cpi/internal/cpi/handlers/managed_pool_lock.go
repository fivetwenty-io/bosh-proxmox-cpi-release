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
// owners all carry the pid and a per-process sequence, so two acquirers never
// write the same claim. A new lock owner must keep that true, or a waiter that shares a
// holder's claim reads the holder's sentinel as its own create and poisons its
// allocation.
func (t *managedPoolService) lockCreateRefused(ctx context.Context, poolID, comment string, err error) bool {
	if !isManagedLockPool(poolID) || !exactPoolAlreadyExists(err, poolID) {
		return false
	}
	reader, ok := t.PoolService.(pve.RawPoolCommentReader)
	if !ok {
		return false
	}
	held, readErr := reader.ReadPoolComment(ctx, poolID)
	if readErr != nil {
		return exactPoolReadMissing(readErr, poolID)
	}
	return held != comment
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
