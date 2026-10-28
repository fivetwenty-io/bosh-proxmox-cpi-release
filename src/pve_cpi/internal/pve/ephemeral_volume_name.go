package pve

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// ephemeralVolumeInfix is the infix of the name EphemeralVolumeName gives a
// VM's own ephemeral volume, "vm-<vmid>-ephemeral-<n>". The name differs from
// "vm-<n>-disk-<n>" (persistent disks), so EmbeddedDiskVMID does not match it.
const ephemeralVolumeInfix = "-ephemeral-"

// EphemeralVolumeFormat keeps file formats and selects raw for block volumes.
// An empty or "auto" preference picks from the storage type through
// ResolveDiskFormat, which gives qcow2 on file storage and raw on block
// storage. An explicit qcow2 or vmdk on block storage, thick lvm included,
// becomes raw, as it always has for ephemeral volumes; callers that know the
// preference was explicit log that coercion. A type that is neither a
// file-volume type (StorageUsesFileVolumes) nor block storage is an error
// whatever the preference, because the volume name depends on it.
func EphemeralVolumeFormat(storageType, preferred string) (string, error) {
	if !config.IsAutoDiskFormat(preferred) {
		switch preferred {
		case DiskFormatRaw, DiskFormatQCOW2, DiskFormatVMDK:
		default:
			return "", fmt.Errorf("ephemeral volume format is unsupported")
		}
	}
	if StorageUsesFileVolumes(storageType) {
		return ResolveDiskFormat(storageType, preferred).Format, nil
	}
	if ClassifyStorageFormat(storageType) == StorageFormatBlock {
		return DiskFormatRaw, nil
	}
	return "", fmt.Errorf("ephemeral storage type is unavailable or unsupported")
}

// EphemeralVolumeName returns the API filename and canonical volume suffix.
// File plugins require a format extension and place images below their VMID.
func EphemeralVolumeName(storageType, preferred string, vmid int) (string, string, error) {
	if vmid <= 0 {
		return "", "", fmt.Errorf("ephemeral volume requires a positive VMID")
	}
	format, err := EphemeralVolumeFormat(storageType, preferred)
	if err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("vm-%d-ephemeral-0", vmid)
	if StorageUsesFileVolumes(storageType) {
		name += "." + format
		return name, strconv.Itoa(vmid) + "/" + name, nil
	}
	return name, name, nil
}

// IsOwnEphemeralVolume reports whether volid names vmid's own ephemeral
// volume, in the block form "<storage>:vm-<vmid>-ephemeral-<n>" or the file
// form "<storage>:<vmid>/vm-<vmid>-ephemeral-<n>.<format>" that
// EphemeralVolumeName produces. The option string a drive entry carries after
// the volid is ignored. The check anchors on "vm-" + VMID + "-ephemeral-" in
// the base name, so a foreign VMID that shares a prefix does not match.
func IsOwnEphemeralVolume(volid string, vmid int) bool {
	name, ok := volumeBaseName(volid)
	return ok && strings.HasPrefix(name, fmt.Sprintf("vm-%d%s", vmid, ephemeralVolumeInfix))
}

// volumeBaseName returns the name a volid gives its volume once the option
// string, the storage prefix, and any directory are removed, so both
// "a:777/vm-777-ephemeral-0.raw,size=5G" and "a:vm-777-ephemeral-0" give a
// name that starts with "vm-777-ephemeral-". The bool is false for a value
// that names no storage, such as "none", a bare name, or a device path.
func volumeBaseName(volid string) (string, bool) {
	bare, _, _ := strings.Cut(volid, ",")
	storage, name, ok := strings.Cut(bare, ":")
	if !ok || storage == "" || name == "" {
		return "", false
	}
	return name[strings.LastIndex(name, "/")+1:], true
}
