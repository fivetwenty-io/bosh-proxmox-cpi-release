package handlers

// Which config locks a destroy's retry loop waits out and which it hands to
// the skiplock and lock-clear recovery at once.

import (
	"errors"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func lock500(lock string) error {
	return sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to destroy VM 100: VM is locked (`+lock+`)\n"}`))
}

func TestLockForRecovery(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":               {nil, false},
		"lock in task text": {errors.New(lockedCloneMsg), true},
		"lock as a 500":     {lock500("migrate"), false},
		"cfs-lock timeout":  {crhLockErr(), false},
		"unrelated":         {errors.New("permission denied"), false},
	} {
		if got := lockForRecovery(tc.err); got != tc.want {
			t.Errorf("%s: lockForRecovery = %v; want %v", name, got, tc.want)
		}
	}
}

// A lock that PVE answers as a 500 is waited out on the retry loop's lock
// curve, so a short lock clears and the plain destroy lands without any
// skiplock.
func TestCleanupVM_DestroyLockAs500_ClearsBeforeSkiplock(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{lock500("backup"), nil}}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (locked 500 + plain retry), got %d", len(nodes.deleteCalls))
	}
	for i, call := range nodes.deleteCalls {
		if call.Skiplock != nil && *call.Skiplock {
			t.Errorf("DeleteQemu call %d carried skiplock; a lock that clears needs none", i)
		}
	}
	if nodes.updateCalls != 0 {
		t.Errorf("destroy succeeded; the VM must not be tagged as failed (got %d tag writes)", nodes.updateCalls)
	}
}

// delete_vm's destroy hands a task-text lock to the skiplock recovery on the
// first refusal instead of spending the loop's budget on it.
func TestDestroyVMWithRecovery_TaskTextLock_GoesStraightToSkiplock(t *testing.T) {
	nodes := &lrNodesStub{deleteErrs: []error{errors.New(lockedCloneMsg), nil}}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	_, gone, err := destroyVMWithRecovery(crhCtx(), deps, "pve01", "vm-cid", 100, false, log.NewNopLogger())
	if err != nil || gone {
		t.Fatalf("destroyVMWithRecovery = gone %v, err %v; want a clean destroy", gone, err)
	}
	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (locked + skiplock), got %d", len(nodes.deleteCalls))
	}
	if call := nodes.deleteCalls[1]; call.Skiplock == nil || !*call.Skiplock {
		t.Error("the second DeleteQemu call must carry skiplock=true")
	}
}
