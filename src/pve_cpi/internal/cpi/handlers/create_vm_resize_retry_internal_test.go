package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestResizeRootDisk_RetriesAConfigMissingAnswerFromAnUnsyncedClone pins the
// create_vm root-disk resize to its pmxcfs retry. The resize runs outside any
// VM-config mutation, so nothing follows the VM, and a config-missing 500 from
// a node that has not yet synced the just-cloned config must be retried with
// backoff instead of ending the create.
func TestResizeRootDisk_RetriesAConfigMissingAnswerFromAnUnsyncedClone(t *testing.T) {
	t.Parallel()

	var resizeCalls int
	var backoffs int
	client := &etClient{qemu: &etQEMU{
		resizeFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalls++
			if resizeCalls == 1 {
				return "", sdkerrors.ParseAPIError(500,
					[]byte(`{"message":"Configuration file 'nodes/node1/qemu-server/203.conf' does not exist"}`))
			}
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskKeyVirtio0: "local-lvm:vm-203-disk-0,size=5G"}, nil
		},
	}}
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "node1", rootDiskGiB: 10, maxAttempts: 3, rootDiskKey: diskKeyVirtio0}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration {
		backoffs++
		return 0
	})

	if err := resizeRootDisk(ctx, deps, log.NewNopLogger(), shape, 203); err != nil {
		t.Fatalf("resizeRootDisk returned error: %v; want the config-missing answer retried to success", err)
	}
	if resizeCalls != 2 {
		t.Errorf("ResizeDisk calls = %d; want 2 (one config-missing answer, one retry)", resizeCalls)
	}
	if backoffs == 0 {
		t.Error("the retry did not back off; the config-missing answer must ride the loop's backoff")
	}
}

// TestResizeDiskConverging_InsideAMutationEndsAtConfigMissing pins the other
// side. Inside pve.MutateVMConfig the same answer goes straight back to the
// mutation, which follows a VM that moved, so the loop must not retry it.
func TestResizeDiskConverging_InsideAMutationEndsAtConfigMissing(t *testing.T) {
	t.Parallel()

	var resizeCalls int
	client := &etClient{qemu: &etQEMU{
		resizeFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			resizeCalls++
			return "", sdkerrors.ParseAPIError(500,
				[]byte(`{"message":"Configuration file 'nodes/node1/qemu-server/203.conf' does not exist"}`))
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{diskKeyVirtio0: "local-lvm:vm-203-disk-0,size=5G"}, nil
		},
	}}
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })

	err := resizeDiskConverging(ctx, deps, log.NewNopLogger(), "resize_disk", "node1", 203, diskKeyVirtio0, 5, 10, 3, true)
	if !pve.IsPmxcfsConfigMissing(err) {
		t.Fatalf("err = %v; want the config-missing answer back for the mutation", err)
	}
	if resizeCalls != 1 {
		t.Errorf("ResizeDisk calls = %d; want 1 (the loop ends at the first config-missing answer)", resizeCalls)
	}
}
