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
