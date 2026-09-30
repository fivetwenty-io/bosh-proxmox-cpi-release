package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A cluster lock is a bosh-lock- sentinel pool, and contention on it is normal.
// A second acquirer's create is refused because the holder's sentinel exists,
// and a release or a steal can find the sentinel already gone. Both refusals
// happen under PVE's user.cfg lock before anything changes, so under a guard
// they are settled as observed non-mutations and handed back unchanged. The
// lock code then classifies them the way it always has, as "held, wait" and
// "already released". Any other outcome stays uncertain and fails closed.

// managedLockPoolRejection is the After result for a sentinel mutation PVE
// refused before changing anything.
type managedLockPoolRejection struct{ poolID, method string }

// managedLockPoolDeletion is the After result for a sentinel delete that PVE
// accepted. It carries the claim the sentinel held just before the delete, so
// a readback that finds a successor's sentinel can tell it apart from ours.
type managedLockPoolDeletion struct {
	poolID, comment string
	known           bool
}

func isManagedLockPool(poolID string) bool {
	return strings.HasPrefix(poolID, reservedPoolLockPrefix)
}

// lockCreateRefused reports whether a failed sentinel create is proven to have
// changed nothing. The exact duplicate verdict is the proof, and the readback
// rules out one case it cannot cover. A create whose committed response was
// lost and whose replay drew the duplicate verdict leaves our own claim in the
// sentinel, and that create did happen.
func (t *managedPoolService) lockCreateRefused(ctx context.Context, poolID, comment string, err error) bool {
	if !isManagedLockPool(poolID) || !exactPoolAlreadyExists(err, poolID) {
		return false
	}
	held, found, readErr := t.GetPoolComment(ctx, poolID)
	return readErr == nil && (!found || held != comment)
}

// lockDeletionClaim reads the claim a sentinel holds before it is deleted. A
// claim that cannot be read is recorded as unknown, which makes the readback
// after the delete strict again.
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
		want, _ := call.Args["comment"].(string)
		if !found || comment != want {
			return fmt.Errorf("%s lock readback mismatch", label)
		}
		return nil
	}
	if !found {
		return nil
	}
	// A waiter polls for exactly this moment, so its sentinel can land between
	// our delete and this read. A different claim in the sentinel is a new
	// sentinel, which proves ours is gone.
	deleted, ok := result.(managedLockPoolDeletion)
	if ok && deleted.known && deleted.poolID == pool && deleted.comment != comment {
		return nil
	}
	return fmt.Errorf("%s lock deletion not observed", label)
}
