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
var allocationVolume = regexp.MustCompile(`^vm-([1-9][0-9]*)-bosh-([0-9a-f]{16})-alloc-([0-9a-f-]{36})\.(raw|qcow2|vmdk)$`)

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

// ParseAllocationVolumeID accepts canonical file-backed volids only; malformed
// paths, mismatched VMID directories and mere substring matches are rejected.
func ParseAllocationVolumeID(volid string) (namespaceLocator, allocationID string, ok bool) {
	storage, volume, err := ParseDiskCID(volid)
	if err != nil || storage == "" || strings.ContainsAny(storage, "/\\") || path.Clean(volume) != volume {
		return "", "", false
	}
	parts := strings.Split(volume, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	m := allocationVolume.FindStringSubmatch(parts[1])
	if len(m) < 4 || m[1] != parts[0] || !allocationUUID.MatchString(m[3]) {
		return "", "", false
	}
	if _, err := strconv.Atoi(m[1]); err != nil {
		return "", "", false
	}
	return m[2], m[3], true
}
