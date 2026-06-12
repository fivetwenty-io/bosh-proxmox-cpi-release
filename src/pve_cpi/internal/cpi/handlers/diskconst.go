// PVE disk-format and OS-type string constants shared across the create_vm,
// create_disk, and create_stemcell handlers.

package handlers

const (
	// diskFormatQCOW2 is the QCOW2 disk image format identifier used by PVE.
	diskFormatQCOW2 = "qcow2"
	// diskFormatRaw is the raw disk image format identifier used by PVE.
	diskFormatRaw = "raw"

	// osTypeLinux26 is the PVE OS type for modern Linux kernels (2.6+/3.x/4.x/5.x/6.x).
	osTypeLinux26 = "l26"
	// osTypeLinux24 is the PVE OS type for Linux 2.4 kernels.
	osTypeLinux24 = "l24"
	// osTypeWindows is the PVE OS type for Windows 10/11/2016+ guests.
	osTypeWindows = "win10"

	// diskOptCache is the PVE per-disk cache-mode option key.
	diskOptCache = "cache"
	// diskOptIothread is the PVE per-disk iothread toggle option key.
	diskOptIothread = "iothread"
	// diskOptSSD is the PVE per-disk SSD-emulation toggle option key.
	diskOptSSD = "ssd"
	// diskOptRetainOnDelete is the CID opts key that flags a disk as operator-retained.
	// Written by create_disk when cloud_properties.retain_on_delete: true; read by
	// delete_vm to emit audit provenance in the WARN log for the foreign-disk guard path.
	diskOptRetainOnDelete = "retain_on_delete"
	// tagRetainEphemeral is the PVE VM tag stamped by create_vm when
	// cloud_properties.retain_ephemeral_on_delete: true. The tag survives
	// set_vm_metadata's tag RMW (not in reservedBoshTagPrefixes) and is read by
	// delete_vm on both paths to trigger ephemeral-disk unlink before destroy.
	tagRetainEphemeral = "bosh-retain-ephemeral"
)
