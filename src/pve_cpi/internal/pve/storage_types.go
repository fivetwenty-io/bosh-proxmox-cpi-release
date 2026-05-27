// Storage type identifiers as recognized by Proxmox VE.
//
// Source-of-truth: PVE storage plugin names per `pvesh get /storage` / pveStorage(5).
package pve

// Storage type identifiers (Proxmox storage plugin names).
const (
	StorageTypeLVM       = "lvm"
	StorageTypeLVMThin   = "lvmthin"
	StorageTypeRBD       = "rbd"
	StorageTypeCephFS    = "cephfs"
	StorageTypeNFS       = "nfs"
	StorageTypeCIFS      = "cifs"
	StorageTypeGlusterFS = "glusterfs"
	StorageTypeZFSPool   = "zfspool"
	StorageTypePBS       = "pbs"
)
