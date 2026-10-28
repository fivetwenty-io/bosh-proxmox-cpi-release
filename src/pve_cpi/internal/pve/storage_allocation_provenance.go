package pve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var allocationUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var allocationVolume = regexp.MustCompile(`^vm-([1-9][0-9]*)-bosh-([0-9a-f]{16})-alloc-([0-9a-f-]{36})(\.(raw|qcow2|vmdk))?$`)

// AllocationNamespaceLocator is an audit locator, never an ownership authority.
// The full namespace, UUID, enrollment and exact physical target must agree.
func AllocationNamespaceLocator(namespace string) string {
	sum := sha256.Sum256([]byte(namespace))
	return hex.EncodeToString(sum[:8])
}

// AllocationVolumeName embeds complete UUID provenance at the allocation API
// boundary, including when the API returns no response to its caller.
func AllocationVolumeName(vmid int, namespace, allocationID, format string) (string, error) {
	if vmid <= 0 || strings.TrimSpace(namespace) == "" || !allocationUUID.MatchString(allocationID) {
		return "", fmt.Errorf("invalid storage allocation identity")
	}
	if format != "raw" && format != "qcow2" && format != "vmdk" {
		return "", fmt.Errorf("unsupported allocation image format %q", format)
	}
	return fmt.Sprintf("vm-%d-bosh-%s-alloc-%s.%s", vmid, AllocationNamespaceLocator(namespace), allocationID, format), nil
}

// AllocationVolume returns the name to pass to PVE's content-allocation API
// and the canonical volid PVE gives the volume, for a persistent allocation
// on storage of the given type. Block storage takes a bare name, with no
// format extension, and returns "<storage>:<name>". Every other type keeps the
// file form AllocationVolumeName produces, "<storage>:<vmid>/<name>.<format>",
// which is what managed persistent disks have always used.
func AllocationVolume(storageType, storageID string, vmid int, namespace, allocationID, format string) (name, volid string, err error) {
	name, err = AllocationVolumeName(vmid, namespace, allocationID, format)
	if err != nil {
		return "", "", err
	}
	if ClassifyStorageFormat(storageType) == StorageFormatBlock {
		name = strings.TrimSuffix(name, "."+format)
		return name, storageID + ":" + name, nil
	}
	return name, fmt.Sprintf("%s:%d/%s", storageID, vmid, name), nil
}

// ParseAllocationVolumeID accepts canonical allocation volids only, in the
// file form "<storage>:<vmid>/<name>.<format>" or the block form
// "<storage>:<name>" with no extension. Malformed paths, mismatched VMID
// directories, a file name without its extension, a block name with one, and
// mere substring matches are rejected.
func ParseAllocationVolumeID(volid string) (namespaceLocator, allocationID string, ok bool) {
	storage, volume, err := ParseDiskCID(volid)
	if err != nil || storage == "" || strings.ContainsAny(storage, "/\\") || path.Clean(volume) != volume {
		return "", "", false
	}
	parts := strings.Split(volume, "/")
	if len(parts) != 1 && len(parts) != 2 {
		return "", "", false
	}
	m := allocationVolume.FindStringSubmatch(parts[len(parts)-1])
	if len(m) != 6 || !allocationUUID.MatchString(m[3]) {
		return "", "", false
	}
	if len(parts) == 2 && (m[1] != parts[0] || m[4] == "") || len(parts) == 1 && m[4] != "" {
		return "", "", false
	}
	if _, err := strconv.Atoi(m[1]); err != nil {
		return "", "", false
	}
	return m[2], m[3], true
}
