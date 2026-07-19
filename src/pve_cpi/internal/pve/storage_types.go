// Storage type identifiers as recognized by Proxmox VE.
//
// Source-of-truth: PVE storage plugin names per `pvesh get /storage` / pveStorage(5).
package pve

import "strings"

// Storage type identifiers (Proxmox storage plugin names).
const (
	StorageTypeDir       = "dir"
	StorageTypeLVM       = "lvm"
	StorageTypeLVMThin   = "lvmthin"
	StorageTypeRBD       = "rbd"
	StorageTypeCephFS    = "cephfs"
	StorageTypeNFS       = "nfs"
	StorageTypeCIFS      = "cifs"
	StorageTypeGlusterFS = "glusterfs"
	StorageTypeZFSPool   = "zfspool"
	StorageTypePBS       = "pbs"
	StorageTypeBTRFS     = "btrfs"
)

// StorageUsesFileVolumes reports whether the given PVE storage type stores VM
// disk volumes as files on a filesystem (dir-style plugins). These plugins
// require the volume name passed to the content-allocation API to carry a
// format extension ("vm-<vmid>-disk-0.qcow2") and return volids in path form
// ("<storage>:<vmid>/vm-<vmid>-disk-0.qcow2"); allocation without the
// extension fails with "unable to parse volume filename". Block-native
// plugins (lvm, lvmthin, zfspool, rbd) take the opposite convention: bare
// names without extension.
//
// Matching is case-insensitive. Empty or unknown types return false — callers
// fall back to the bare (block-style) name, preserving prior behavior when a
// live type lookup fails.
func StorageUsesFileVolumes(storageType string) bool {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case StorageTypeDir, StorageTypeNFS, StorageTypeCIFS, StorageTypeGlusterFS, StorageTypeBTRFS:
		return true
	}
	return false
}
