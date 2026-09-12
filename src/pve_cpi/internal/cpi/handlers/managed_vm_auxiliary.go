package handlers

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// observeRootAuxiliary proves every clone-created disk carried with the root.
// Source disks and inherited CD-ROM references never become owned artifacts.
func (m *managedVMAllocation) observeRootAuxiliary(ctx context.Context, cfg map[string]any, target StoragePlanTarget) ([]string, error) {
	if target.Source == nil {
		return nil, fmt.Errorf("root source facts absent")
	}
	var volumes []string
	var bytes uint64

	seen := map[string]bool{}
	for _, source := range target.Source.AuxiliaryVolumes {
		device := source.Device
		if device == "" || device == m.shape.rootDiskKey || !managedVMVolumeDevice(device) || seen[device] || source.VirtualBytes == 0 {
			return nil, fmt.Errorf("frozen auxiliary device facts invalid")
		}
		seen[device] = true
		drive, ok := pve.ConfigString(cfg, device)
		if !ok || strings.Contains(drive, "media=cdrom") {
			return nil, fmt.Errorf("cloned auxiliary device missing or wrong media")
		}
		volume := strings.Split(drive, ",")[0]
		if source.VolumeID == volume {
			return nil, fmt.Errorf("clone still references source-owned auxiliary disk")
		}
		size, err := m.observeTargetVolume(ctx, target, volume, source.VirtualBytes)
		if err != nil {
			return nil, err
		}
		if size != source.VirtualBytes {
			return nil, fmt.Errorf("cloned auxiliary virtual size changed")
		}
		rounded, err := inv.RoundBytes(size, 1<<20)
		if err != nil || bytes > math.MaxUint64-rounded {
			return nil, fmt.Errorf("auxiliary allocation size overflow")
		}
		bytes += rounded
		volumes = append(volumes, volume)
	}

	if len(volumes) != len(target.Source.AuxiliaryVolumes) || bytes != target.Source.AuxiliaryBytes {
		return nil, fmt.Errorf("clone auxiliary layout differs from frozen source")
	}
	slices.Sort(volumes)
	return volumes, nil
}
