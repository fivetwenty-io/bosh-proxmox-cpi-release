package pve

import (
	"fmt"
	"strconv"
	"strings"
)

// ephemeralVolumeInfix is the infix of the name EphemeralVolumeName gives a
// VM's own ephemeral volume, "vm-<vmid>-ephemeral-<n>". The name differs from
// "vm-<n>-disk-<n>" (persistent disks), so EmbeddedDiskVMID does not match it.
const ephemeralVolumeInfix = "-ephemeral-"

// EphemeralVolumeFormat keeps file formats and selects raw for block volumes.
func EphemeralVolumeFormat(storageType, preferred string) (string, error) {
	switch preferred {
	case "raw", "qcow2", "vmdk":
	default:
		return "", fmt.Errorf("ephemeral volume format is unsupported")
	}
	if StorageUsesFileVolumes(storageType) {
		return preferred, nil
	}
	if IsBlockNativeStorage(storageType) {
		return "raw", nil
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
