package handlers

import (
	"context"
	"fmt"
	"math"
)

func managedVMGiB(bytes uint64) (int, error) {
	if bytes == 0 || bytes%(1<<30) != 0 {
		return 0, fmt.Errorf("VM disk size is not a positive whole GiB")
	}
	gib := bytes / (1 << 30)
	if gib > uint64(math.MaxInt) {
		return 0, fmt.Errorf("VM disk size exceeds platform integer range")
	}
	return int(gib), nil
}

func resolveAtomicVMEphemeral(ctx context.Context, deps Deps, cp createVMCloudProps, role *StorageRoleSelection) (int, string, error) {
	if cp.EphemeralDiskSizeMB <= 0 {
		return 0, "", nil
	}
	size := cp.EphemeralDiskSizeMB / 1024
	if cp.EphemeralDiskSizeMB%1024 != 0 {
		size++
	}
	switch role.Kind {
	case storageSelectorPool:
		return size, role.Value, nil
	case storageSelectorTier:
		if deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
			return 0, "", fmt.Errorf("atomic ephemeral tier observation unavailable")
		}
		pool, err := resolveStorageTier(ctx, deps.PVE.ClusterStorage(), deps.Config, role.Value, role.Encrypted)
		return size, pool, err
	default:
		return 0, "", fmt.Errorf("ephemeral set requires frozen allocation plan")
	}
}
