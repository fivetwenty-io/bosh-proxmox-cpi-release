package handlers

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// setVMMetadataLockBudget works out, from the constants the retry loops use,
// the longest set_vm_metadata can hold the VMID lock: five single calls, the
// pool lookup on the transient attempt budget, the pool ensure on
// DefaultStorageLockMaxAttempts, and the pool move on cleanupSweepMaxAttempts,
// with every attempt taking the per-call allowance and every loop sleeping its
// longest backoff.
func setVMMetadataLockBudget() time.Duration {
	lookup := pve.TransientMaxAttempts()
	ensure := pve.DefaultStorageLockMaxAttempts
	move := cleanupSweepMaxAttempts
	calls := 5 + lookup + ensure + move
	return time.Duration(calls)*vmidLockCallAllowance +
		pve.RetryOnTransientSleepBudget(lookup) +
		pve.RetryOnTransientOrLockSleepBudget(ensure) +
		pve.RetryOnTransientOrLockSleepBudget(move)
}

// TestVMIDLockTTLCoversSetVMMetadataRetryBudget fails when a change to an
// attempt count, a backoff curve, or the per-call allowance pushes
// set_vm_metadata's worst case past the TTL, instead of letting another
// process steal a lock that is still in use.
func TestVMIDLockTTLCoversSetVMMetadataRetryBudget(t *testing.T) {
	budget := setVMMetadataLockBudget()
	t.Logf("set_vm_metadata worst case %s, margin %s, TTL %s", budget, vmidLockTTLMargin, vmidLockTTL)
	if vmidLockTTL < budget+vmidLockTTLMargin {
		t.Fatalf("vmidLockTTL = %s, but set_vm_metadata can hold the lock for %s plus a %s margin; raise the TTL",
			vmidLockTTL, budget, vmidLockTTLMargin)
	}
	// The pool move alone, the retry loop this TTL was first raised for,
	// already runs past the old 30-second TTL at its worst.
	move := time.Duration(cleanupSweepMaxAttempts)*vmidLockCallAllowance + pve.RetryOnTransientOrLockSleepBudget(cleanupSweepMaxAttempts)
	if move <= 30*time.Second || vmidLockTTL <= move {
		t.Fatalf("pool move worst case %s should exceed the old 30s TTL and stay under vmidLockTTL %s", move, vmidLockTTL)
	}
}

// TestVMIDLockBodiesTableMatchesTheDerivation ties the budget table the lock
// uses at run time to the derivation above, and checks that set_vm_metadata
// is the slowest body, so covering it covers the others.
func TestVMIDLockBodiesTableMatchesTheDerivation(t *testing.T) {
	bodies := vmidLockBodies()
	var setVM *vmidLockBody
	for i := range bodies {
		if bodies[i].operation == "set_vm_metadata" {
			setVM = &bodies[i]
		}
	}
	if setVM == nil {
		t.Fatal("set_vm_metadata is missing from vmidLockBodies")
	}
	if got, want := setVM.budget(), setVMMetadataLockBudget(); got != want {
		t.Fatalf("table budget for set_vm_metadata = %s, derivation = %s", got, want)
	}
	for _, body := range bodies {
		if body.budget() > setVM.budget() {
			t.Fatalf("%s budget %s exceeds set_vm_metadata's %s", body.operation, body.budget(), setVM.budget())
		}
	}
	if got := vmidLockBodyBudget(); got != setVM.budget() {
		t.Fatalf("vmidLockBodyBudget() = %s, want set_vm_metadata's %s", got, setVM.budget())
	}
	if got := vmidLockTTLNow(); got != vmidLockTTL {
		t.Fatalf("vmidLockTTLNow() = %s at the shipped curves, want vmidLockTTL %s", got, vmidLockTTL)
	}
}

// TestVMIDLockTTLNowFollowsAnOperatorsLongerCurve checks that a longer
// operator-configured backoff cap lengthens the TTL the lock claims with,
// since the constant only covers the shipped curves.
func TestVMIDLockTTLNowFollowsAnOperatorsLongerCurve(t *testing.T) {
	restore := pve.SetPushbackBackoffForTest(5000, int((15 * time.Minute).Milliseconds()))
	defer restore()
	budget := setVMMetadataLockBudget()
	if budget+vmidLockTTLMargin <= vmidLockTTL {
		t.Fatalf("test curve too short to exceed the constant: budget %s", budget)
	}
	if got := vmidLockTTLNow(); got < budget+vmidLockTTLMargin {
		t.Fatalf("vmidLockTTLNow() = %s, want at least %s", got, budget+vmidLockTTLMargin)
	}
}

// TestWithVMIDLockClaimsTheFullTTL reads the claim withVMIDLock writes and
// checks that its expiry sits a full TTL after acquisition.
func TestWithVMIDLockClaimsTheFullTTL(t *testing.T) {
	pools := newVMIDLockPools(nil)
	before := time.Now()
	var comment string
	if err := withVMIDLock(t.Context(), pools, 5151, "set_vm_metadata/5151", nil, func() error {
		comment = pools.pools["bosh-lock-vm-5151"]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var exp time.Time
	for _, field := range strings.Fields(comment) {
		if v, ok := strings.CutPrefix(field, "exp="); ok {
			secs, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("claim %q has an unreadable expiry: %v", comment, err)
			}
			exp = time.Unix(secs, 0)
		}
	}
	if exp.IsZero() {
		t.Fatalf("claim %q carries no expiry", comment)
	}
	// The claim records whole seconds, so allow one second of truncation.
	if held := exp.Sub(before); held < vmidLockTTL-time.Second {
		t.Fatalf("claim expires %s after acquisition, want the full TTL %s", held, vmidLockTTL)
	}
}
