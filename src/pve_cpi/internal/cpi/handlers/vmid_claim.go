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
// The claim is advisory for the work and mandatory for the sweep. When PVE
// refuses it for lack of permission, a create, a delete, or a rollback runs
// unclaimed, as the parker and disk-transfer locks run unserialized, and the
// sweep destroys nothing, because without the claim it cannot prove any VM
// abandoned.
//
// The set lives in the request's context. A nested claim on a VMID the set
// already holds, such as the rollback inside the create that claimed the
// VMID, or delete_vm's deleting-tag write inside its own claim, runs in the
// outer claim's window instead of waiting on it.

// vmidClaimReleaseAttempts bounds how many times a release sends the delete of
// a claim's sentinel when PVE fails it. vmidClaimReleaseBackoff is the pause
// before the second attempt, and each later pause doubles. Every attempt
// shares one lockReleaseTimeout budget, so a PVE that stays down cannot hold
// the request past it.
const (
	vmidClaimReleaseAttempts = 3
	vmidClaimReleaseBackoff  = 250 * time.Millisecond
)

// errVMIDClaimHeld marks a VMID claim that another process holds. A create
// treats it like a VMID conflict and moves to a fresh VMID.
var errVMIDClaimHeld = errors.New("the VMID is claimed by another process")

// vmidClaimHeldError is a claim another process holds. Its text is the lock
// error's alone, which names the holder, and it matches errVMIDClaimHeld as
// well as everything the lock error matches.
type vmidClaimHeldError struct{ err error }

func (e vmidClaimHeldError) Error() string   { return e.err.Error() }
func (e vmidClaimHeldError) Unwrap() []error { return []error{e.err, errVMIDClaimHeld} }

// vmidClaimMode says how withVMIDClaim takes a claim and what it does when PVE
// refuses the claim for lack of permission.
type vmidClaimMode int

const (
	// claimForDisposal waits up to vmidLockTimeout for a live holder, and it
	// runs the work unclaimed when PVE refuses the claim for lack of
	// permission, the way the parker and disk-transfer locks run unserialized.
	// delete_vm and the create_vm rollback take their claims this way.
	claimForDisposal vmidClaimMode = iota
	// claimForSweep tries once and never runs the work unclaimed. The claim is
	// the sweep's only proof that a VM is abandoned, so without it the sweep
	// destroys nothing.
	claimForSweep
)

type vmidClaimSetKey struct{}

// vmidClaimEntry is one claim the set holds. windowEnd is when work under the
// claim must stop, so the release still fits before the claim's expiry.
type vmidClaimEntry struct {
	handle    *pve.ClusterLockHandle
	windowEnd time.Time
}

// vmidClaimSet records the VMID claims one request holds. refusalLogged says
// whether the request has already warned that PVE refused it a claim for lack
// of permission, so that warning is logged once per request.
type vmidClaimSet struct {
	mu            sync.Mutex
	held          map[int]*vmidClaimEntry
	refusalLogged bool
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

// acquire claims vmid for this request with ttl. With wait false the acquire
// tries once, and with wait true it waits up to vmidLockTimeout. A holder that
// stays live returns a vmidClaimHeldError, and after a wait that error names
// the holder. A VMID the set still holds, because an earlier release of it
// failed, returns a vmidClaimHeldError too, so a create moves to a fresh VMID.
// Every other lock failure, a permission refusal included, is returned as it
// is, and the caller decides whether to run unclaimed (see
// proceedUnclaimedAfter). A nil pool service claims nothing and returns nil:
// there is no lock to take, and the sweep, which refuses to run without one,
// never races the work.
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
		return vmidClaimHeldError{err: cpierrors.Retriable(
			"VMID claim: this request still claims VM %d, because releasing that claim failed", vmid)}
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
			return vmidClaimHeldError{err: err}
		case errors.Is(err, pve.ErrClusterLockTimeout):
			return vmidClaimHeldError{err: vmidLockHeldError(ctx, pools, lockName, err)}
		}
		return err
	}
	entry := &vmidClaimEntry{handle: handle, windowEnd: handle.WindowDeadline(vmidLockTTLMargin)}
	s.mu.Lock()
	s.held[vmid] = entry
	s.mu.Unlock()
	return nil
}

// proceedUnclaimedAfter reports whether err is PVE refusing the claim on vmid
// for lack of permission, after which a create, a delete, or a rollback runs
// without the claim, the way the parker and disk-transfer locks run
// unserialized. Only a 403 on the sentinel's create counts (see
// pve.IsClusterLockPermissionRefused), because that proves no sentinel was
// created and that the lock is unavailable to this token. Every other
// failure leaves the holder unknown or names a live one, so it is not a
// reason to proceed. The first refusal in a request is logged at Warn.
func (s *vmidClaimSet) proceedUnclaimedAfter(err error, vmid int, logger *log.Logger) bool {
	if !pve.IsClusterLockPermissionRefused(err) {
		return false
	}
	s.mu.Lock()
	first := !s.refusalLogged
	s.refusalLogged = true
	s.mu.Unlock()
	if first && logger != nil {
		logger.Warn("VMID claim: PVE refused the claim for lack of permission, so create_vm, delete_vm, and the "+
			"create_vm rollback run without it, and the deferred-teardown sweep destroys nothing; grant "+
			"Pool.Allocate and Pool.Audit on /pool so the CPI can create bosh-lock-vm-<vmid>",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
	}
	return true
}

// release gives up the claim on vmid (see releaseVMIDClaimHandle). A claim
// whose release still fails stays in the set, so releaseAll tries it again
// and a later acquire of the VMID in this request sees it as held. If every
// attempt fails, the claim lapses at its expiry.
func (s *vmidClaimSet) release(ctx context.Context, vmid int, logger *log.Logger) {
	s.mu.Lock()
	entry, ok := s.held[vmid]
	if ok && entry.handle == nil {
		delete(s.held, vmid)
	}
	s.mu.Unlock()
	if !ok || entry.handle == nil {
		return
	}
	if err := releaseVMIDClaimHandle(ctx, entry.handle); err != nil {
		if logger != nil {
			logger.Warn("VMID claim: release failed (non-fatal); the claim lapses at its expiry unless a later "+
				"release in this request succeeds", log.Int(metadataKeyVMID, vmid),
				log.Int("attempts", vmidClaimReleaseAttempts), log.Err(err))
		}
		return
	}
	s.mu.Lock()
	if s.held[vmid] == entry {
		delete(s.held, vmid)
	}
	s.mu.Unlock()
}

// releaseVMIDClaimHandle deletes a claim's sentinel. It runs on a context
// detached from the request and bounded at lockReleaseTimeout, so a cancelled
// request still deletes its sentinel. A failed delete is sent again up to
// vmidClaimReleaseAttempts times in all, pausing vmidClaimReleaseBackoff and
// then twice that, inside the same bound. Release leaves a handle whose delete
// failed unreleased, so each attempt sends the delete again, and it stops
// short of deleting a claim close enough to its expiry that a steal could
// land first. The last attempt's error is returned.
func releaseVMIDClaimHandle(ctx context.Context, handle *pve.ClusterLockHandle) error {
	relCtx, cancel := detachedContext(ctx, lockReleaseTimeout)
	defer cancel()
	pause := vmidClaimReleaseBackoff
	for attempt := 1; ; attempt++ {
		err := handle.Release(relCtx)
		if err == nil || attempt >= vmidClaimReleaseAttempts {
			return err
		}
		if sleepOrDone(relCtx, pause) != nil {
			return err
		}
		pause *= 2
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

// createVMIDClaimBudget is how long a legacy create_vm's own work may run,
// which is the create operation timeout, pve.operation_timeout.create_sec.
// With the envelope on, the dispatcher already bounds the request at that
// figure. With it off, bindCreateVMIDClaims bounds the create at it, so a
// claim a crashed create leaves behind lapses about as soon as the sweep's
// settling age would let the sweep look at the VM.
func createVMIDClaimBudget(deps Deps) time.Duration {
	return time.Duration(deps.Config.OperationTimeoutCreateSec()) * time.Second
}

// createClaimTTL is the TTL of a create's claim. The create's own work ends
// at ctx's deadline, which bindCreateVMIDClaims always sets, and the TTL
// leaves the rollback its whole budget after that, plus the lock margin. A ctx
// with no deadline gets budget from now in its place.
func createClaimTTL(ctx context.Context, budget time.Duration) time.Duration {
	bodyEnd, ok := ctx.Deadline()
	if !ok {
		bodyEnd = time.Now().Add(budget)
	}
	return max(vmidLockTTLNow(), time.Until(bodyEnd)+rollbackCleanupTimeout+vmidLockTTLMargin)
}

// bindCreateVMIDClaims prepares a legacy create_vm to claim its VMIDs. It
// returns ctx carrying a claim set and, when ctx has no deadline, bounded by
// budget, so the claim's TTL always covers the create and its rollback. The
// returned function releases every claim the create took, ends the bound, and
// returns the create's error. A create that the bound cut off, while the
// request itself was still live, gets a retriable error, because the
// dispatcher only rewrites a deadline it set itself. The create defers it
// before it arms the rollback, so the rollback runs while the claim is still
// held.
func bindCreateVMIDClaims(
	ctx context.Context, budget time.Duration, logger *log.Logger,
) (context.Context, func(error) error) {
	parent := ctx
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, budget)
	}
	bounded := ctx
	ctx, set := withVMIDClaimSet(ctx)
	return ctx, func(err error) error {
		set.releaseAll(ctx, logger)
		cutOff := bounded != parent && errors.Is(bounded.Err(), context.DeadlineExceeded) && parent.Err() == nil
		cancel()
		if err != nil && cutOff && !cpierrors.IsDefinite(err) {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"create_vm: the create ran past pve.operation_timeout.create_sec (%s), which bounds it so that its "+
					"VMID claim expires; the new VM was rolled back, and a retry starts over", budget))
		}
		return err
	}
}

// runInVMIDClaimWindow runs fn under a context that ends at windowEnd. A
// window that has already ended runs nothing and returns a retriable error,
// because the claim may be stolen before fn's writes land. When the window
// ends while fn runs and the request itself is still live, fn's error is
// returned as retriable, because the dispatcher only rewrites a deadline it
// set itself, and without the envelope the cut would otherwise reach the
// Director as whatever the interrupted PVE call returned.
func runInVMIDClaimWindow(ctx context.Context, vmid int, windowEnd time.Time, fn func(context.Context) error) error {
	if !time.Now().Before(windowEnd) {
		return cpierrors.Retriable("VMID claim: the claim on VM %d has run out its window; retry so a fresh claim covers the work",
			vmid)
	}
	wctx, cancel := context.WithDeadline(ctx, windowEnd)
	defer cancel()
	err := fn(wctx)
	if err != nil && errors.Is(wctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil && !cpierrors.IsDefinite(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"VMID claim: the work on VM %d ran out the claim's window; retry so a fresh claim covers the rest", vmid))
	}
	return err
}

// withVMIDClaim runs fn while this request claims vmid. A claim the request
// already holds is reused, and fn runs in its window. Otherwise the claim is
// taken as mode says, fn runs in the new claim's window, and the claim is
// released when fn returns. A holder that stays live returns an error that
// carries errVMIDClaimHeld. Under claimForDisposal, a claim PVE refuses for
// lack of permission runs fn unclaimed on ctx (see proceedUnclaimedAfter).
// Under claimForSweep that refusal is returned, and fn does not run. A nil
// pool service runs fn without a claim (see vmidClaimSet.acquire).
func withVMIDClaim(
	ctx context.Context, deps Deps, vmid int, owner string, mode vmidClaimMode, logger *log.Logger,
	fn func(context.Context) error,
) error {
	if windowEnd, ok := heldVMIDClaimWindow(ctx, vmid); ok {
		return runInVMIDClaimWindow(ctx, vmid, windowEnd, fn)
	}
	if deps.PVE == nil || deps.PVE.Pools() == nil {
		return fn(ctx)
	}
	ctx, set := withVMIDClaimSet(ctx)
	if err := set.acquire(ctx, deps.PVE.Pools(), vmid, owner, disposalClaimTTL(ctx), mode != claimForSweep); err != nil {
		if mode == claimForDisposal && set.proceedUnclaimedAfter(err, vmid, logger) {
			return fn(ctx)
		}
		return err
	}
	defer set.release(ctx, vmid, logger)
	windowEnd, _ := heldVMIDClaimWindow(ctx, vmid)
	return runInVMIDClaimWindow(ctx, vmid, windowEnd, fn)
}

// claimCreateCandidate claims a create_vm candidate VMID before its clone or
// import, trying once. A candidate another process holds returns an error
// that carries errVMIDClaimHeld, which the allocation loop retries with a
// fresh VMID. A claim PVE refuses for lack of permission lets the candidate
// go ahead unclaimed (see proceedUnclaimedAfter).
func claimCreateCandidate(ctx context.Context, deps Deps, candidate int, logger *log.Logger) error {
	set := vmidClaimsFrom(ctx)
	if set == nil || deps.PVE == nil {
		return nil
	}
	owner := fmt.Sprintf("create_vm/%d", candidate)
	err := set.acquire(ctx, deps.PVE.Pools(), candidate, owner, createClaimTTL(ctx, createVMIDClaimBudget(deps)), false)
	if err != nil && set.proceedUnclaimedAfter(err, candidate, logger) {
		return nil
	}
	return err
}

// runCreateAttempt runs one create_vm allocation attempt on candidate under
// its VMID claim. The attempt's own cleanup of a failed candidate runs inside
// the claim, and the claim is released when the attempt fails. A successful
// attempt keeps the claim for the rest of the create.
func runCreateAttempt(ctx context.Context, deps Deps, candidate int, logger *log.Logger, attempt func() error) error {
	if err := claimCreateCandidate(ctx, deps, candidate, logger); err != nil {
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
