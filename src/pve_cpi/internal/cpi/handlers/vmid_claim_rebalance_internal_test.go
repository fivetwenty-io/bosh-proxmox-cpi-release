package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// A rebalance claim tries once, has the short lease of a replica widening
// claim, and never runs its work unclaimed, so a VM call that queues behind
// it gets the VM inside its own wait even when the rebalance died holding it.
func TestWithVMIDClaim_RebalanceClaimHasTheShortLeaseAndTriesOnce(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	ran := false
	start := time.Now()
	err := withVMIDClaim(crhCtx(), deps, 100, "ha-rebalance/100", claimForRebalance, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			exp, ok := pools.expiry(100)
			if !ok || exp.After(start.Add(replicaWidenClaimTTL+time.Second)) {
				t.Errorf("the claim expires at %v, want a lease of at most %v", exp, replicaWidenClaimTTL)
			}
			return nil
		})
	if err != nil || !ran || pools.held(100) {
		t.Fatalf("ran %v err %v held %v", ran, err, pools.held(100))
	}

	pools.hold(100, "set_vm_metadata/100", time.Now().Add(time.Hour))
	ran = false
	err = withVMIDClaim(crhCtx(), deps, 100, "ha-rebalance/100", claimForRebalance, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			return nil
		})
	if ran || !isVMIDClaimHeld(err) {
		t.Fatalf("a live holder must refuse the claim at once: ran %v err %v", ran, err)
	}

	err = withVMIDClaim(crhCtx(), claimDeps(nil), 100, "ha-rebalance/100", claimForRebalance, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			return nil
		})
	if ran || !errors.Is(err, errVMIDClaimNoPools) {
		t.Fatalf("no pool service must refuse the work: ran %v err %v", ran, err)
	}
}
