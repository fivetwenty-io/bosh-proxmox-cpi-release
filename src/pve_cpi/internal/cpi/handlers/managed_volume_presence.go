package handlers

import (
	"context"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func managedVolumePresent(ctx context.Context, deps Deps, node, volume string) (bool, error) {
	return pve.ObserveStorageVolumePresence(ctx, deps.PVE, node, volume)
}
