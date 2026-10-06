package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestCreateVMSnapshotBlockAfterTheDeadlineStaysPermanent runs create_vm
// through the dispatcher with a short budget. The persistent disk's attach
// meets the snapshot that blocks its deferred park only once the request's
// deadline has passed, so the rollback that destroys the attempt's VM and
// closes the generation finishes after the deadline. The rollback settled the
// generation, and every retry would build a VM, meet the same snapshot, and
// destroy the VM again. So the Director must read the permanent snapshot
// refusal, not the retriable timeout the deadline would otherwise put in its
// place.
func TestCreateVMSnapshotBlockAfterTheDeadlineStaysPermanent(t *testing.T) {
	// Stays serial: it swaps the package variable attachExistingDiskForVM.
	locks := newLockContention(t)
	deps, client, journal, cid, _ := createVMDiskFixture(t, locks, false)
	locks.reset()
	blocked := cpierrors.SnapshotBlocked("create_vm.attach_disk: can't finish the deferred park of disk %s from VM 777; Delete snapshot pre-upgrade, then retry create_vm", cid)
	previous := attachExistingDiskForVM
	attachExistingDiskForVM = func(ctx context.Context, _ Deps, _ *aj.Handle, _ resolvedDisk, _ string, _ int) (string, error) {
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			t.Error("the request's deadline never reached the disk's attach")
		}
		return "", &diskReturnedAfterSnapshotRefusal{err: blocked}
	}
	t.Cleanup(func() { attachExistingDiskForVM = previous })

	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithMethodTimeouts(func(string) time.Duration { return 2 * time.Second }))
	handler := cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return createVM(ctx, deps, args)
	})
	if err := d.Register("create_vm", handler); err != nil {
		t.Fatal(err)
	}

	resp := d.Handle(t.Context(), &jsonrpc.Request{Method: "create_vm", Arguments: createVMArgs(t, cid), Context: jsonrpc.Context{RequestID: "snapshot-block-after-deadline"}})
	if resp.Error == nil {
		t.Fatal("create_vm succeeded while a snapshot blocks its disk")
	}
	if client.creates != 1 || client.destroys != 1 {
		t.Fatalf("create_vm built %d VM(s) and destroyed %d, want the attempt's VM built and rolled back", client.creates, client.destroys)
	}
	if _, found, err := journal.InspectVM("disk-agent"); err != nil || found {
		t.Fatalf("the rollback left a VM generation for the retry to resume: found=%t err=%v", found, err)
	}
	if resp.Error.OkToRetry || strings.Contains(resp.Error.Message, "exceeded its") || !strings.Contains(resp.Error.Message, "Delete snapshot pre-upgrade") {
		t.Fatalf("the Director would retry a create_vm the snapshot refuses every time: ok_to_retry=%t message=%q", resp.Error.OkToRetry, resp.Error.Message)
	}
}
