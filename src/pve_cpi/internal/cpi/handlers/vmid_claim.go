package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A VMID claim is the per-VMID lock (see withVMIDLock) held across a whole
// create or disposal rather than one read-modify-write. A legacy create_vm
// claims each candidate VMID before it sends the clone or import, and keeps
// the claim until its rollback, if any, has finished. delete_vm's legacy path,
// the create_vm rollback, and the deferred-teardown sweep each claim the VMID
// they dispose of. So a VM that carries a birth tag or bosh-deleting while its
// VMID claim is live still has a process working on it, and the sweep, which
// tries the claim once and skips on a live holder, never races that process.
//
// The claim is the sweep's proof that a create was abandoned, so it must not
// outlive the work it covers. Its TTL is sized from the request's deadline,
// and the work under it runs under a context that ends before the claim's
// recorded expiry, so a claim that has expired, and that the sweep may steal,
// belongs to a process that has stopped working on the VM.
//
// The set lives in the request's context. A nested claim on a VMID the set
// already holds, such as the rollback inside the create that claimed the
// VMID, or delete_vm's deleting-tag write inside its own claim, runs in the
// outer claim's window instead of waiting on it.

// createVMIDClaimBodyBudget bounds a create that runs without a request
// deadline, which is what create_vm gets when the operation-timeout envelope
// is off. The claim needs some bound to prove a create abandoned, and this one
// is far past any create that is still making progress. bindCreateVMIDClaims
// installs it as the create's deadline.
const createVMIDClaimBodyBudget = 6 * time.Hour

// errVMIDClaimHeld marks a VMID claim that another process holds. A create
// treats it like a VMID conflict and moves to a fresh VMID.
var errVMIDClaimHeld = errors.New("the VMID is claimed by another process")

type vmidClaimSetKey struct{}

// vmidClaimEntry is one claim the set holds. windowEnd is when work under the
// claim must stop, so the release still fits before the claim's expiry.
type vmidClaimEntry struct {
	handle    *pve.ClusterLockHandle
	windowEnd time.Time
}

// vmidClaimSet records the VMID claims one request holds.
type vmidClaimSet struct {
	mu   sync.Mutex
	held map[int]*vmidClaimEntry
}

// vmidClaimsFrom returns the claim set ctx carries, or nil.
func vmidClaimsFrom(ctx context.Context) *vmidClaimSet {
	if ctx == nil {
		return nil
	}
	set, _ := ctx.Value(vmidClaimSetKey{}).(*vmidClaimSet)
	return set
}

// withVMIDClaimSet returns ctx carrying a claim set, reusing the one ctx
// already carries.
func withVMIDClaimSet(ctx context.Context) (context.Context, *vmidClaimSet) {
	if set := vmidClaimsFrom(ctx); set != nil {
		return ctx, set
	}
	set := &vmidClaimSet{held: map[int]*vmidClaimEntry{}}
	return context.WithValue(ctx, vmidClaimSetKey{}, set), set
}

// heldVMIDClaimWindow reports when the work under the claim ctx holds on vmid
// must stop, and whether ctx holds one.
func heldVMIDClaimWindow(ctx context.Context, vmid int) (time.Time, bool) {
	set := vmidClaimsFrom(ctx)
	if set == nil {
		return time.Time{}, false
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	entry, ok := set.held[vmid]
	if !ok {
		return time.Time{}, false
	}
	return entry.windowEnd, true
}

// acquire claims vmid for this request with ttl. With wait false
// the acquire tries once, and a live holder returns errVMIDClaimHeld joined
// with the lock error. With wait true it waits up to vmidLockTimeout, and a
// holder that stays live returns vmidLockHeldError naming it, joined with
// errVMIDClaimHeld. A nil pool service claims nothing and returns nil: there
// is no lock to take, and the sweep, which refuses to run without one, never
// races the work.
func (s *vmidClaimSet) acquire(
	ctx context.Context, pools pve.PoolService, vmid int, owner string, ttl time.Duration, wait bool,
) error {
	if pools == nil {
		return nil
	}
	s.mu.Lock()
	_, already := s.held[vmid]
	s.mu.Unlock()
	if already {
		return cpierrors.Cloud("VMID claim: this request already claims VM %d", vmid)
	}
	lockName := fmt.Sprintf("vm-%d", vmid)
	var opts []pve.ClusterLockOption
	if !wait {
		opts = append(opts, pve.WithoutWait())
	}
	handle, err := pve.AcquireClusterLock(ctx, pools, lockName, pve.ProcessLockOwner(owner), ttl, vmidLockTimeout, opts...)
	if err != nil {
		switch {
		case errors.Is(err, pve.ErrClusterLockHeld):
			return errors.Join(errVMIDClaimHeld, err)
		case errors.Is(err, pve.ErrClusterLockTimeout):
			return errors.Join(errVMIDClaimHeld, vmidLockHeldError(ctx, pools, lockName, err))
		}
		return err
	}
	entry := &vmidClaimEntry{handle: handle, windowEnd: handle.WindowDeadline(vmidLockTTLMargin)}
	s.mu.Lock()
	s.held[vmid] = entry
	s.mu.Unlock()
	return nil
}

// release gives up the claim on vmid, on a detached, bounded context so a
// cancelled request still deletes its sentinel. A failed release is logged:
// the claim then lapses at its expiry.
func (s *vmidClaimSet) release(ctx context.Context, vmid int, logger *log.Logger) {
	s.mu.Lock()
	entry, ok := s.held[vmid]
	delete(s.held, vmid)
	s.mu.Unlock()
	if !ok || entry.handle == nil {
		return
	}
	relCtx, cancel := detachedContext(ctx, lockReleaseTimeout)
	defer cancel()
	if err := entry.handle.Release(relCtx); err != nil && logger != nil {
		logger.Warn("VMID claim: release failed (non-fatal); the claim lapses at its expiry",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
	}
}

// releaseAll gives up every claim the set holds.
func (s *vmidClaimSet) releaseAll(ctx context.Context, logger *log.Logger) {
	s.mu.Lock()
	vmids := make([]int, 0, len(s.held))
	for vmid := range s.held {
		vmids = append(vmids, vmid)
	}
	s.mu.Unlock()
	for _, vmid := range vmids {
		s.release(ctx, vmid, logger)
	}
}

// disposalClaimTTL is the TTL of a disposal's claim: the request's remaining
// time plus the lock margin, and never less than vmidLockTTLNow.
func disposalClaimTTL(ctx context.Context) time.Duration {
	ttl := vmidLockTTLNow()
	if dl, ok := ctx.Deadline(); ok {
		ttl = max(ttl, time.Until(dl)+vmidLockTTLMargin)
	}
	return ttl
}

// createClaimTTL is the TTL of a create's claim. The create's own work ends
// at ctx's deadline, which bindCreateVMIDClaims always sets, and the TTL
// leaves the rollback its whole budget after that, plus the lock margin. A ctx
// with no deadline gets createVMIDClaimBodyBudget in its place.
func createClaimTTL(ctx context.Context) time.Duration {
	bodyEnd, ok := ctx.Deadline()
	if !ok {
		bodyEnd = time.Now().Add(createVMIDClaimBodyBudget)
	}
	return max(vmidLockTTLNow(), time.Until(bodyEnd)+rollbackCleanupTimeout+vmidLockTTLMargin)
}

// bindCreateVMIDClaims prepares a legacy create_vm to claim its VMIDs. It
// returns ctx carrying a claim set and, when ctx has no deadline, bounded by
// createVMIDClaimBodyBudget, so the claim's TTL always covers the create and
// its rollback. The returned function releases every claim the create took and
// ends the bound. The create defers it before it arms the rollback, so the
// rollback runs while the claim is still held.
func bindCreateVMIDClaims(ctx context.Context, logger *log.Logger) (context.Context, func()) {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, createVMIDClaimBodyBudget)
	}
	ctx, set := withVMIDClaimSet(ctx)
	return ctx, func() {
		set.releaseAll(ctx, logger)
		cancel()
	}
}

// runInVMIDClaimWindow runs fn under a context that ends at windowEnd. A
// window that has already ended runs nothing and returns a retriable error,
// because the claim may be stolen before fn's writes land.
func runInVMIDClaimWindow(ctx context.Context, vmid int, windowEnd time.Time, fn func(context.Context) error) error {
	if !time.Now().Before(windowEnd) {
		return cpierrors.Retriable("VMID claim: the claim on VM %d has run out its window; retry so a fresh claim covers the work",
			vmid)
	}
	wctx, cancel := context.WithDeadline(ctx, windowEnd)
	defer cancel()
	return fn(wctx)
}

// withVMIDClaim runs fn while this request claims vmid. A claim the request
// already holds is reused, and fn runs in its window. Otherwise the claim is
// taken, waiting up to vmidLockTimeout when wait is set and trying once when
// it is not, fn runs in the new claim's window, and the claim is released when
// fn returns. A holder that stays live returns an error that carries
// errVMIDClaimHeld. A nil pool service runs fn without a claim (see
// vmidClaimSet.acquire).
func withVMIDClaim(
	ctx context.Context, deps Deps, vmid int, owner string, wait bool, logger *log.Logger,
	fn func(context.Context) error,
) error {
	if windowEnd, ok := heldVMIDClaimWindow(ctx, vmid); ok {
		return runInVMIDClaimWindow(ctx, vmid, windowEnd, fn)
	}
	if deps.PVE == nil || deps.PVE.Pools() == nil {
		return fn(ctx)
	}
	ctx, set := withVMIDClaimSet(ctx)
	if err := set.acquire(ctx, deps.PVE.Pools(), vmid, owner, disposalClaimTTL(ctx), wait); err != nil {
		return err
	}
	defer set.release(ctx, vmid, logger)
	windowEnd, _ := heldVMIDClaimWindow(ctx, vmid)
	return runInVMIDClaimWindow(ctx, vmid, windowEnd, fn)
}

// claimCreateCandidate claims a create_vm candidate VMID before its clone or
// import, trying once. A candidate another process holds returns an error
// that carries errVMIDClaimHeld, which the allocation loop retries with a
// fresh VMID.
func claimCreateCandidate(ctx context.Context, deps Deps, candidate int) error {
	set := vmidClaimsFrom(ctx)
	if set == nil || deps.PVE == nil {
		return nil
	}
	owner := fmt.Sprintf("create_vm/%d", candidate)
	return set.acquire(ctx, deps.PVE.Pools(), candidate, owner, createClaimTTL(ctx), false)
}

// runCreateAttempt runs one create_vm allocation attempt on candidate under
// its VMID claim. The attempt's own cleanup of a failed candidate runs inside
// the claim, and the claim is released when the attempt fails. A successful
// attempt keeps the claim for the rest of the create.
func runCreateAttempt(ctx context.Context, deps Deps, candidate int, logger *log.Logger, attempt func() error) error {
	if err := claimCreateCandidate(ctx, deps, candidate); err != nil {
		return err
	}
	err := attempt()
	if err != nil {
		releaseVMIDClaim(ctx, candidate, logger)
	}
	return err
}

// releaseVMIDClaim releases the claim ctx's claim set holds on vmid, if any.
func releaseVMIDClaim(ctx context.Context, vmid int, logger *log.Logger) {
	if set := vmidClaimsFrom(ctx); set != nil {
		set.release(ctx, vmid, logger)
	}
}

// isVMIDClaimHeld reports whether err says another process claims the VMID.
func isVMIDClaimHeld(err error) bool {
	return errors.Is(err, errVMIDClaimHeld)
}
