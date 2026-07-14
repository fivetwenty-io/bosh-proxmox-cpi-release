// TRIM/discard capability classification for PVE storage backends, used by
// the disk_performance auto-resolution of discard/ssd (see
// internal/cpi/handlers/disk_performance.go).
package pve

import "strings"

// fileBackedStorageTypes lists PVE storage backends that store disk images as
// files on a filesystem, as opposed to block-native volumes. TRIM/discard
// passthrough on these backends depends on the disk image format: qcow2
// supports discard passthrough to the host filesystem; raw files on the same
// backend do not reliably reclaim space the same way.
var fileBackedStorageTypes = map[string]struct{}{
	StorageTypeDir:  {},
	StorageTypeNFS:  {},
	StorageTypeCIFS: {},
}

// trimCapableBlockTypes lists PVE storage backends that pass discard/TRIM
// through to the guest regardless of disk image format — thin-provisioned
// block-native backends where discard directly reclaims space at the storage
// layer. Thick LVM ("lvm") is deliberately excluded: it is not
// thin-provisioned, so TRIM has no space-reclamation benefit there.
var trimCapableBlockTypes = map[string]struct{}{
	StorageTypeLVMThin: {},
	StorageTypeZFSPool: {},
	StorageTypeRBD:     {},
}

// IsTrimCapable reports whether discard/TRIM passthrough is meaningful for a
// disk on the given PVE storage type with the given disk image format. Used
// to auto-resolve pve.disk_performance.discard and .ssd to "on" only where
// TRIM actually reclaims space, so thin pools do not grow monotonically as
// guest-deleted blocks accumulate.
//
// Two shapes are TRIM-capable:
//
//   - Thin-provisioned block-native backends (lvmthin, zfspool, rbd):
//     TRIM-capable regardless of format — these backends have no file
//     format at all (format is meaningless/ignored for the check).
//   - File-backed backends (dir, nfs, cifs): TRIM-capable only when the
//     disk image format is qcow2, which supports discard passthrough to
//     the host filesystem. A raw file on the same backend does not.
//
// Every other storage type (thick lvm, cephfs, glusterfs, pbs, unknown, or
// empty) is treated as NOT TRIM-capable — a conservative default so
// auto-resolution never enables discard/ssd on a backend where PVE might
// reject or silently ignore it.
//
// storageType and format are matched case-insensitively. Empty storageType
// (e.g. a failed live lookup) returns false — the fail-open default for the
// auto-resolution caller is simply "do not add the option", never an error.
func IsTrimCapable(storageType, format string) bool {
	st := strings.ToLower(strings.TrimSpace(storageType))
	if st == "" {
		return false
	}
	if _, ok := trimCapableBlockTypes[st]; ok {
		return true
	}
	if _, ok := fileBackedStorageTypes[st]; ok {
		return strings.EqualFold(strings.TrimSpace(format), "qcow2")
	}
	return false
}
