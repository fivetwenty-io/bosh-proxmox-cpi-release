package handlers

import (
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// vmidLockCallAllowance is the time the lock budget gives one PVE API call. It
// matches lockReleaseTimeout, which is this package's existing allowance for a
// single call. Nothing enforces it per call, because the transport's only
// timeout is the half-hour one that stemcell uploads need. A PVE that holds one
// request longer than this is wedged, and no TTL covers a wedged cluster.
const vmidLockCallAllowance = lockReleaseTimeout

// vmidLockTTLMargin is added on top of the slowest body's budget. It covers
// the deferred release, which runs while the claim still stands, and clock
// skew between the CPI hosts that compare the claim's expiry with their own
// clocks.
const vmidLockTTLMargin = lockReleaseTimeout + 90*time.Second

// poolSentinelBudget is the time budget of the pool record write that
// set_vm_metadata makes under the lock after the pool move. The record is
// advisory, because the next reconcile writes it again, so the write rides the
// config mutation protocol for a few seconds only, and a lock or a move that
// outlasts that skips the record instead of holding the call and the lock open.
const poolSentinelBudget = 10 * time.Second

// vmConfigMutateOverrunCalls counts the PVE calls one MutateVMConfig can still
// make after its time budget ends: the send that was already under way, and the
// read of the config that checks whether a lost answer landed, which needs the
// VM's location and then the config.
const vmConfigMutateOverrunCalls = 4

// setVMMetadataMutateBudget is the time budget of the metadata write's config
// mutation, which is what MutateVMConfig would take without a context
// deadline, twice the motion wait. The handler passes it as the mutation's
// Budget so a longer context deadline cannot lengthen the hold on the lock.
func setVMMetadataMutateBudget(motionWait time.Duration) time.Duration {
	return 2 * motionWait
}

// setVMMetadataMutationTime is the most time set_vm_metadata spends in its two
// config mutations under the lock, beyond the calls the body already counts:
// the metadata write's budget, the pool record's budget, and the calls each
// can still make once its budget ends. A body that waits for no motion has
// motionWait zero and spends none.
func setVMMetadataMutationTime(motionWait time.Duration) time.Duration {
	if motionWait <= 0 {
		return 0
	}
	overrun := time.Duration(2*vmConfigMutateOverrunCalls) * vmidLockCallAllowance
	return setVMMetadataMutateBudget(motionWait) + poolSentinelBudget + overrun
}

// setDiskMetadataMutationTime is the most time set_disk_metadata spends in its
// two config mutations under the lock, beyond the calls the body already
// counts: each mutation's budget, and the calls each can still make once its
// budget ends. A body that waits for no motion has motionWait zero and spends
// none.
func setDiskMetadataMutationTime(motionWait time.Duration) time.Duration {
	if motionWait <= 0 {
		return 0
	}
	overrun := time.Duration(2*vmConfigMutateOverrunCalls) * vmidLockCallAllowance
	return 2*setVMMetadataMutateBudget(motionWait) + overrun
}

// vmidLockRetryLoop is one retry loop that runs under the VMID lock: how many
// attempts it makes, and the helper that gives the longest total time the
// loop's retry helper can sleep between that many attempts.
type vmidLockRetryLoop struct {
	site     string
	attempts int
	sleep    func(maxAttempts int) time.Duration
}

// vmidLockBody is the PVE traffic of one operation that runs under the VMID
// lock. singleCalls counts the calls made once, without a retry loop.
// bounded is the total deadline of the steps whose call count depends on the
// cluster, such as one config read per guest; each such step runs under a
// context with that deadline and fails closed when it expires.
type vmidLockBody struct {
	operation   string
	singleCalls int
	loops       []vmidLockRetryLoop
	bounded     time.Duration
}

// budget is the longest the body can run: every call, single or retried,
// takes its full allowance, every retry loop sleeps its longest backoff, and
// every bounded step runs to its deadline.
func (b vmidLockBody) budget() time.Duration {
	total := time.Duration(b.singleCalls)*vmidLockCallAllowance + b.bounded
	for _, loop := range b.loops {
		total += time.Duration(loop.attempts)*vmidLockCallAllowance + loop.sleep(loop.attempts)
	}
	return total
}

// vmidLockBodies lists every operation that runs under withVMIDLock, with the
// PVE calls it makes there. Attempt counts come from the constants the call
// sites pass, or from the default the retry helper resolves when they pass
// zero, so a change to any of them moves the budget with it. A new call or
// retry loop added under the lock must be added here as well.
//
// set_vm_metadata, in order: the read-modify-write runs as a config mutation
// that may wait out a migration, so its time is counted as bounded at its
// Budget (setVMMetadataMutateBudget), and it reads the config and writes it back.
// The pool reconcile then reads the config, looks up the VM's pool
// (FindVMPoolViaCluster retries on the transient budget), ensures the target
// pool exists (EnsurePoolExists passes zero, so it retries on
// DefaultStorageLockMaxAttempts), moves the VM (cleanupSweepMaxAttempts), and
// reads and writes the config again to record the pool, as a second config
// mutation whose Budget is poolSentinelBudget. Each mutation can also make
// vmConfigMutateOverrunCalls calls after its budget ends. A managed pool service
// ensures the pool in at most three calls with no retry, so the unmanaged path
// is the longer one and is the one counted.
//
// set_disk_metadata reads and writes the config twice, as two config mutations
// that may wait out a migration, so their time is counted as bounded at their
// Budgets (setDiskMetadataMutationTime).
//
// The other bodies are fixed read-modify-writes: the stemcell reference
// updates, delete_vm's deleting tag, and create_vm's failure tag each read and
// write it once. The stemcell template destroy runs after the lock is released,
// so it is not counted.
//
// The stemcell-refs untracked destroy reads the config and the status, runs
// the linked-clone rescan, reads the config again to compare its digest,
// and writes the destroy-pending stamp. The rescan
// lists every guest and reads each config, so its call count grows with the
// cluster; untrackedCloneRecheck runs it under untrackedCloneRescanTimeout,
// which is counted as a bounded step.
//
// motionWait is pve.ha.motion_wait_sec, which sets how long set_vm_metadata
// and set_disk_metadata can wait out a migration under the lock. Zero means the
// holder runs no config mutation, which is what vmidLockTTLNow asks for. The
// other bodies wait for no motion, so they ignore it.
func vmidLockBodies(motionWait time.Duration) []vmidLockBody {
	return []vmidLockBody{
		{
			operation:   "set_vm_metadata",
			singleCalls: 5,
			bounded:     setVMMetadataMutationTime(motionWait),
			loops: []vmidLockRetryLoop{
				{site: "pool lookup", attempts: pve.TransientMaxAttempts(), sleep: pve.RetryOnTransientSleepBudget},
				{site: "pool ensure", attempts: pve.DefaultStorageLockMaxAttempts, sleep: pve.RetryOnTransientOrLockSleepBudget},
				{site: "pool move", attempts: cleanupSweepMaxAttempts, sleep: pve.RetryOnTransientOrLockSleepBudget},
			},
		},
		{operation: "set_disk_metadata", singleCalls: 4, bounded: setDiskMetadataMutationTime(motionWait)},
		{operation: "stemcell director reference", singleCalls: 2},
		{operation: "delete_vm deleting tag", singleCalls: 2},
		{operation: "create_vm failure tag", singleCalls: 2},
		{operation: "stemcell-refs untracked destroy", singleCalls: 4, bounded: untrackedCloneRescanTimeout},
	}
}

// vmidLockBodyBudget is the longest any body in vmidLockBodies can run under
// the backoff curves configured now, with set_vm_metadata waiting out a
// migration for motionWait.
func vmidLockBodyBudget(motionWait time.Duration) time.Duration {
	var longest time.Duration
	for _, body := range vmidLockBodies(motionWait) {
		longest = max(longest, body.budget())
	}
	return longest
}

// vmidLockTTLFor is the TTL a holder claims with when its body can wait out a
// migration for motionWait. It is vmidLockTTL unless the operator's retry
// configuration or the motion wait lengthens the slowest body far enough that
// the body plus the margin no longer fits, and then it is that sum instead.
func vmidLockTTLFor(motionWait time.Duration) time.Duration {
	return max(vmidLockTTL, vmidLockBodyBudget(motionWait)+vmidLockTTLMargin)
}

// vmidLockTTLNow is the TTL withVMIDLock claims with. It is for the holders
// that wait for no migration under the lock, so it counts none. set_vm_metadata
// and set_disk_metadata wait, and claim with vmidLockTTLFor(their motion wait)
// through withVMIDLockFor.
func vmidLockTTLNow() time.Duration {
	return vmidLockTTLFor(0)
}
