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
type vmidLockBody struct {
	operation   string
	singleCalls int
	loops       []vmidLockRetryLoop
}

// budget is the longest the body can run: every call, single or retried,
// takes its full allowance, and every retry loop sleeps its longest backoff.
func (b vmidLockBody) budget() time.Duration {
	total := time.Duration(b.singleCalls) * vmidLockCallAllowance
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
// set_vm_metadata, in order: the read-modify-write reads the config and writes
// it back, and the pool reconcile then reads the config, looks up the VM's pool
// (FindVMPoolViaCluster retries on the transient budget), ensures the target
// pool exists (EnsurePoolExists passes zero, so it retries on
// DefaultStorageLockMaxAttempts), moves the VM (cleanupSweepMaxAttempts), and
// reads and writes the config again to record the pool. A managed pool service
// ensures the pool in at most three calls with no retry, so the unmanaged path
// is the longer one and is the one counted.
//
// The other bodies are fixed read-modify-writes: set_disk_metadata reads and
// writes the config twice, and the stemcell reference updates, delete_vm's
// deleting tag, and create_vm's failure tag each read and write it once. The
// stemcell template destroy runs after the lock is released, so it is not
// counted.
func vmidLockBodies() []vmidLockBody {
	return []vmidLockBody{
		{
			operation:   "set_vm_metadata",
			singleCalls: 5,
			loops: []vmidLockRetryLoop{
				{site: "pool lookup", attempts: pve.TransientMaxAttempts(), sleep: pve.RetryOnTransientSleepBudget},
				{site: "pool ensure", attempts: pve.DefaultStorageLockMaxAttempts, sleep: pve.RetryOnTransientOrLockSleepBudget},
				{site: "pool move", attempts: cleanupSweepMaxAttempts, sleep: pve.RetryOnTransientOrLockSleepBudget},
			},
		},
		{operation: "set_disk_metadata", singleCalls: 4},
		{operation: "stemcell director reference", singleCalls: 2},
		{operation: "delete_vm deleting tag", singleCalls: 2},
		{operation: "create_vm failure tag", singleCalls: 2},
	}
}

// vmidLockBodyBudget is the longest any body in vmidLockBodies can run under
// the backoff curves configured now.
func vmidLockBodyBudget() time.Duration {
	var longest time.Duration
	for _, body := range vmidLockBodies() {
		longest = max(longest, body.budget())
	}
	return longest
}

// vmidLockTTLNow is the TTL withVMIDLock claims with. It is vmidLockTTL unless
// the operator's retry configuration lengthens a backoff curve or the transient
// attempt budget far enough that the slowest body plus the margin no longer
// fits, and then it is that sum instead.
func vmidLockTTLNow() time.Duration {
	return max(vmidLockTTL, vmidLockBodyBudget()+vmidLockTTLMargin)
}
