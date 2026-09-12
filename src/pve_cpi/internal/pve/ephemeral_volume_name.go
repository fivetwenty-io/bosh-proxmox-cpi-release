package pve

import (
	"fmt"
	"strconv"
)

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
