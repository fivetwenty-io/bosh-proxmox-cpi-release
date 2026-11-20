package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// A rebalance holds failback off while it moves a VM, and the reconcile of
// any CPI call leaves the flag of such a resource alone, so a set_vm_metadata
// during the move can't raise failback under the old pin and pull the VM back.
func TestReconcileVMFailback_LeavesAResourceARebalanceMoves(t *testing.T) {
	for _, have := range []bool{false, true} {
		r := newFailbackRig(t, failbackFlag(have))
		r.cluster.comment = ha.WithRebalanceMark(haIdentity{Owner: aaDirector}.marker(), "pve02")
		r.reconcile()
		r.requireWrites(t)
	}
}

// The rebalance itself decides the flag the VM gets once its move is done. It
// names the node the mark names, so the mark doesn't count as pending, and
// the decision is the one the VM would get without the mark.
func TestWantVMFailbackForRebalance_DecidesPastItsOwnMark(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.cluster.comment = ha.WithRebalanceMark(haIdentity{Owner: aaDirector}.marker(), "pve01")
	w, err := wantVMFailbackForRebalance(context.Background(), r.deps, r.cluster, nil, 100, r.loc, "pve01")
	if err != nil || !w.decided || !w.want {
		t.Fatalf("want %+v err %v, want failback on", w, err)
	}

	// A mark that names another node belongs to another move.
	w, err = wantVMFailbackForRebalance(context.Background(), r.deps, r.cluster, nil, 100, r.loc, "pve03")
	if err != nil || w.decided {
		t.Fatalf("want %+v err %v, want undecided", w, err)
	}

	// A fresh failback hold still holds the flag off.
	held := ha.WithFailbackHold(r.cluster.comment, time.Now())
	r.cluster.comment = held
	w, err = wantVMFailbackForRebalance(context.Background(), r.deps, r.cluster, nil, 100, r.loc, "pve01")
	if err != nil || w.decided {
		t.Fatalf("want %+v err %v, want undecided under a fresh hold", w, err)
	}
}
