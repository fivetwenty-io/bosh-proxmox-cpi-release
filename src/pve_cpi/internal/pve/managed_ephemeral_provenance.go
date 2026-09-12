package pve

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var managedEphemeralVolume = regexp.MustCompile(`^vm-([1-9][0-9]*)-bosh-([0-9a-f]{16})-ephemeral-([0-9a-f-]{36})(\.(raw|qcow2|vmdk))?$`)

// ManagedEphemeralVolumeName carries the full VM allocation UUID at the storage
// API boundary. It returns the plugin filename and its canonical volume suffix.
func ManagedEphemeralVolumeName(storageType, preferred string, vmid int, namespace, allocationID string) (string, string, error) {
	format, err := EphemeralVolumeFormat(storageType, preferred)
	if err != nil {
		return "", "", err
	}
	if vmid <= 0 || strings.TrimSpace(namespace) == "" || !allocationUUID.MatchString(allocationID) {
		return "", "", fmt.Errorf("invalid ephemeral allocation identity")
	}
	name := fmt.Sprintf("vm-%d-bosh-%s-ephemeral-%s", vmid, AllocationNamespaceLocator(namespace), allocationID)
	if StorageUsesFileVolumes(storageType) {
		name += "." + format
		return name, strconv.Itoa(vmid) + "/" + name, nil
	}
	return name, name, nil
}

// ParseManagedEphemeralVolumeID recognizes only canonical complete provenance;
// the namespace locator is not a substitute for the full enrolled namespace.
func ParseManagedEphemeralVolumeID(volid string) (namespaceLocator, allocationID string, ok bool) {
	storage, volume, err := ParseDiskCID(volid)
	if err != nil || storage == "" || strings.ContainsAny(storage, "/\\") || path.Clean(volume) != volume {
		return "", "", false
	}
	parts := strings.Split(volume, "/")
	if len(parts) != 1 && len(parts) != 2 {
		return "", "", false
	}
	match := managedEphemeralVolume.FindStringSubmatch(parts[len(parts)-1])
	if len(match) != 6 || !allocationUUID.MatchString(match[3]) {
		return "", "", false
	}
	if _, err := strconv.Atoi(match[1]); err != nil {
		return "", "", false
	}
	if len(parts) == 2 {
		if parts[0] != match[1] || match[4] == "" {
			return "", "", false
		}
	} else if match[4] != "" {
		return "", "", false
	}
	return match[2], match[3], true
}
