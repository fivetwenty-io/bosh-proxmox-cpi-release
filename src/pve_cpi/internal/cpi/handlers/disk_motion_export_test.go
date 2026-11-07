package handlers

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ForeignHolderErrorForTest exposes the refusal attach_disk and create_vm give
// a disk that another VM holds, so a test outside the package can drive it
// against the HA scene.
func ForeignHolderErrorForTest(
	ctx context.Context, deps Deps, op, diskCID string, targetVMID int, holder pve.DiskHolder,
) error {
	return foreignHolderError(ctx, deps, op, diskCID, targetVMID, holder)
}

// SettleTransferIdentitiesForTest settles the VMs of an interrupted transfer
// and returns the parker node the resume would use, so a test outside the
// package can drive the settle without building a resolved disk.
func SettleTransferIdentitiesForTest(
	ctx context.Context, deps Deps, op string, intent *pve.DiskTransferIntent,
) (string, error) {
	rd, err := settleTransferIdentities(ctx, deps, op, resolvedDisk{diskCID: "test-disk", intent: intent})
	if err != nil {
		return "", err
	}
	return rd.intent.ParkerNode, nil
}
