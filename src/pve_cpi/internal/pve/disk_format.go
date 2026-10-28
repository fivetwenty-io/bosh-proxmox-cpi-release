// Disk image format selection from the target storage's type. This is the one
// classifier every format decision uses: root disks (import and full clone),
// ephemeral disks, and persistent disks. The volume-naming, clone, and TRIM
// helpers (StorageUsesFileVolumes, IsBlockNativeStorage, IsTrimCapable) keep
// their own lists because they answer different questions. In particular
// cephfs is file storage here, but StorageUsesFileVolumes leaves it out
// because PVE does not store VM images on cephfs, so no volume name changes.
package pve

import (
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// Disk image formats the CPI requests from PVE.
const (
	DiskFormatRaw   = "raw"
	DiskFormatQCOW2 = "qcow2"
	DiskFormatVMDK  = "vmdk"
)

// Storage types that appear only in format classification. The other plugin
// names live in storage_types.go.
const (
	StorageTypeZFSOverISCSI = "zfs"
	StorageTypeISCSI        = "iscsi"
	StorageTypeISCSIDirect  = "iscsidirect"
)

// StorageFormatClass says which disk image formats a PVE storage type can
// hold.
type StorageFormatClass int

const (
	// StorageFormatUnknown is an empty type, a failed lookup, or a plugin the
	// CPI does not classify. Callers keep their previous behavior and warn.
	StorageFormatUnknown StorageFormatClass = iota
	// StorageFormatFile is a filesystem-backed plugin that holds raw, qcow2,
	// and vmdk image files.
	StorageFormatFile
	// StorageFormatBlock is a block-backed plugin that holds raw volumes.
	StorageFormatBlock
)

// String names the class for log fields.
func (c StorageFormatClass) String() string {
	switch c {
	case StorageFormatFile:
		return "file"
	case StorageFormatBlock:
		return "block"
	default:
		return "unknown"
	}
}

// ClassifyStorageFormat returns the format class of a PVE storage type.
// dir, nfs, cifs, glusterfs, cephfs, and btrfs are file storage. lvm,
// lvmthin, zfspool, zfs (ZFS over iSCSI), rbd, iscsi, and iscsidirect are
// block storage. Anything else, including pbs and an empty type, is unknown.
// Matching ignores case and surrounding whitespace.
func ClassifyStorageFormat(storageType string) StorageFormatClass {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case StorageTypeDir, StorageTypeNFS, StorageTypeCIFS, StorageTypeGlusterFS, StorageTypeCephFS, StorageTypeBTRFS:
		return StorageFormatFile
	case StorageTypeLVM, StorageTypeLVMThin, StorageTypeZFSPool, StorageTypeZFSOverISCSI, StorageTypeRBD,
		StorageTypeISCSI, StorageTypeISCSIDirect:
		return StorageFormatBlock
	}
	return StorageFormatUnknown
}

// DiskFormatResolution is the outcome of ResolveDiskFormat.
type DiskFormatResolution struct {
	// Format is the format to request from PVE and to record.
	Format string
	// Requested is the trimmed explicit format, or "" when the request left
	// the choice to the storage type.
	Requested string
	// Class is the storage type's format class.
	Class StorageFormatClass
	// Auto is true when the request carried no explicit format.
	Auto bool
	// Coerced is true when an explicit format the block storage cannot hold
	// was replaced with raw.
	Coerced bool
	// Fallback is true when the request was automatic but the storage type was
	// unknown, so Format is the qcow2 the CPI used before it looked at types.
	Fallback bool
}

// ResolveDiskFormat picks the disk format for a volume on storage of the given
// type. requested is the first explicit format the caller's layers produced;
// an empty or "auto" value (config.IsAutoDiskFormat) leaves the choice to the
// type.
//
// When the choice is automatic, file storage gets qcow2 and block storage gets
// raw. An unknown type gets qcow2, which is what every path used before the
// type was consulted, and Fallback is set so the caller can warn.
//
// An explicit format passes through on file storage and on an unknown type. On
// block storage raw passes through, and so does qcow2 on thick lvm, because
// PVE 9 can hold qcow2 there when the storage enables
// snapshot-as-volume-chain. An explicit qcow2 or vmdk anywhere else on block
// storage becomes raw with Coerced set. Any other explicit string passes
// through untouched, so PVE rejects it exactly as it did before.
func ResolveDiskFormat(storageType, requested string) DiskFormatResolution {
	class := ClassifyStorageFormat(storageType)
	if config.IsAutoDiskFormat(requested) {
		res := DiskFormatResolution{Class: class, Auto: true}
		switch class {
		case StorageFormatBlock:
			res.Format = DiskFormatRaw
		case StorageFormatFile:
			res.Format = DiskFormatQCOW2
		default:
			res.Format = DiskFormatQCOW2
			res.Fallback = true
		}
		return res
	}
	explicit := strings.TrimSpace(requested)
	res := DiskFormatResolution{Format: explicit, Requested: explicit, Class: class}
	if class != StorageFormatBlock {
		return res
	}
	switch explicit {
	case DiskFormatQCOW2:
		if strings.EqualFold(strings.TrimSpace(storageType), StorageTypeLVM) {
			return res
		}
		res.Format, res.Coerced = DiskFormatRaw, true
	case DiskFormatVMDK:
		res.Format, res.Coerced = DiskFormatRaw, true
	}
	return res
}
