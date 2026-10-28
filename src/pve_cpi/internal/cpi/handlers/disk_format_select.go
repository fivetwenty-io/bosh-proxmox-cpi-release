// disk_format_select.go gathers the disk format a request asks for and logs
// how pve.ResolveDiskFormat or pve.ResolveVolumeDiskFormat turned it into the
// format sent to PVE. Each disk path (root, ephemeral, persistent) resolves
// against the storage it actually lands on, after that storage is chosen.
package handlers

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// requestedDiskFormat returns the first explicit format the cloud-property
// layers carry under keys, and then the job-level pve.vm_disk_format (which a
// cpi-config entry may override). An empty or "auto" value at any rung counts
// as unset. The result is "" when nothing explicit was found, which means the
// storage type decides, and an explicit result is trimmed and lower-cased.
// fromCloudProps is true only when a cloud-property layer supplied the value.
func requestedDiskFormat(r *layeredResolver, cfg *config.CPIConfig, keys ...string) (format string, fromCloudProps bool) {
	if df, ok := r.DiskFormat(keys...); ok {
		return df, true
	}
	if cfg != nil && !config.IsAutoDiskFormat(cfg.VMDiskFormat) {
		return pve.NormalizeDiskFormat(cfg.VMDiskFormat), false
	}
	return "", false
}

// logDiskFormatResolution warns when a resolution departed from what the
// request asked for: an explicit format the block storage cannot hold was
// replaced with raw, or the storage type was unknown. op names the CPI method
// and disk role for the log line. formatSent says whether the caller sends the
// resolved format to PVE; when it does not, an unknown type means PVE applies
// the storage's own default, and the warning says so.
func logDiskFormatResolution(logger *log.Logger, op, storage, storageType string, res pve.DiskFormatResolution, formatSent bool) {
	if logger == nil {
		return
	}
	switch {
	case res.Coerced:
		logger.Warn(op+": the requested disk format cannot be stored on block storage, so the CPI uses raw instead",
			log.String("storage", storage),
			log.String("storage_type", storageType),
			log.String("requested_format", res.Requested),
			log.String("format", res.Format),
		)
	case res.Fallback && formatSent:
		logger.Warn(op+": the storage type is unknown or its lookup failed, so the disk format falls back to qcow2",
			log.String("storage", storage),
			log.String("storage_type", storageType),
			log.String("format", res.Format),
		)
	case res.Fallback:
		logger.Warn(op+": the storage type is unknown or its lookup failed, so the CPI sends no disk format and PVE applies the storage's default",
			log.String("storage", storage),
			log.String("storage_type", storageType),
		)
	}
}

// warnEphemeralFormatCoercion warns when an explicit ephemeral format other
// than raw lands on block storage, where pve.EphemeralVolumeFormat always
// allocates raw.
func warnEphemeralFormatCoercion(logger *log.Logger, op, storage, storageType, preferred string) {
	preferred = pve.NormalizeDiskFormat(preferred)
	if logger == nil || preferred == "" || preferred == diskFormatRaw ||
		pve.ClassifyStorageFormat(storageType) != pve.StorageFormatBlock {
		return
	}
	logger.Warn(op+": the requested ephemeral disk format cannot be stored on block storage, so the CPI uses raw instead",
		log.String("storage", storage),
		log.String("storage_type", storageType),
		log.String("requested_format", preferred),
		log.String("format", diskFormatRaw),
	)
}

// managedPlanDiskFormat returns the DiskFormat a managed VM plan freezes. The
// plan records one format: the root disk's, which the full clone or import
// sends to PVE. Every release, this one included, derives the ephemeral
// disk's format from that value and the ephemeral pool's type
// (pve.EphemeralVolumeFormat), which keeps every plan this release writes
// readable, verifiable, and cleanable by an older CPI.
//
// The ephemeral pool must be a type every release can derive a managed
// ephemeral volume for, which is file storage or lvm, lvmthin, zfspool, or
// rbd. Any other type, iscsi, iscsidirect, and ZFS over iSCSI included, is
// refused here with a non-retriable error that names the pool and its type,
// before the plan is frozen or the VM is created. An unsupported request is
// refused the same way.
//
// rootFormat is the root disk's resolved format. When the root lands on block
// storage other than thick lvm, PVE stores the root as raw whatever format
// the clone or import asks for, so the value we send there only matters for
// the ephemeral disk. In that one case, when the ephemeral pool is file
// storage and wants a different format than rootFormat would give it, we
// return the format the ephemeral pool wants (qcow2 for an unset request, or
// the explicit format). That is the value earlier releases sent there too.
// Every other case returns rootFormat unchanged. On thick lvm an explicit
// qcow2 root reaches PVE unchanged, so the root wins and the ephemeral disk
// follows it.
func managedPlanDiskFormat(rootType, rootFormat, ephemeralStorage, ephemeralType, requested string) (string, error) {
	if !pve.StorageUsesFileVolumes(ephemeralType) && !pve.IsBlockNativeStorage(ephemeralType) {
		reason := "which a storage-set placement cannot use for an ephemeral disk"
		if !pve.StorageAllocatesImages(ephemeralType) {
			reason = "which cannot allocate disk images"
		}
		return "", refuseEphemeralStorage(ephemeralStorage, ephemeralType, reason)
	}
	want, err := pve.EphemeralVolumeFormat(ephemeralType, requested)
	if err != nil {
		return "", cpierrors.Cloud("create_vm: ephemeral disk format %q on storage %q (type %q) is unsupported; refused before the VM was created",
			requested, ephemeralStorage, ephemeralType)
	}
	if pve.ClassifyStorageFormat(rootType) != pve.StorageFormatBlock || pve.IsThickLVMStorage(rootType) {
		return rootFormat, nil
	}
	if have, haveErr := pve.EphemeralVolumeFormat(ephemeralType, rootFormat); haveErr == nil && have == want {
		return rootFormat, nil
	}
	return want, nil
}

// refuseEphemeralStorage is the non-retriable error create_vm returns, before
// any VM exists, for an ephemeral pool whose type cannot take the disk.
func refuseEphemeralStorage(storage, storageType, reason string) error {
	return cpierrors.Cloud("create_vm: ephemeral storage %q has type %q, %s; refused before the VM was created",
		storage, storageType, reason)
}

// checkUnmanagedEphemeralStorage refuses an iscsi or iscsidirect ephemeral
// pool on the unmanaged create_vm path before the clone, where PVE would
// otherwise refuse the volume only after the VM was cloned and configured. It
// reuses the root pool's type when the ephemeral disk shares that pool. A
// failed type lookup passes, so the deploy goes on as before and
// attachEphemeralDisk decides once the pool can be read.
func checkUnmanagedEphemeralStorage(ctx context.Context, deps Deps, storage, vmStorage, vmStorageType string) error {
	storageType := vmStorageType
	if storage != vmStorage || storageType == "" {
		storageType = lookupVMStorageType(ctx, deps, storage)
	}
	if storageType == "" || pve.StorageAllocatesImages(storageType) {
		return nil
	}
	return refuseEphemeralStorage(storage, storageType, "which cannot allocate disk images")
}

// finalizeCreateDiskFormat decides what create_disk sends to PVE once the
// pool's type is known. It returns the format parameter ("" means PVE picks),
// the volume-name extension, and the resolution to log and record. It
// resolves with pve.ResolveVolumeDiskFormat, so thick lvm coerces an explicit
// qcow2 to raw like every other block type.
//
// On file storage the resolved format is always sent, and the volume name
// carries it as its extension, because file plugins reject bare names.
//
// On block storage a format set in cloud_properties is sent after coercion to
// raw. A format that came only from the job-level pve.vm_disk_format, or from
// the storage type, was never sent to block storage, and PVE allocated raw,
// so that case still sends nothing and resolves to raw, with a warning when
// the job asked for something else.
//
// On an unknown type the old behavior holds: a cloud_properties format is
// sent as is, nothing else is sent, and the name stays bare.
func finalizeCreateDiskFormat(storageType, requested string, fromCloudProps bool) (formatArg, volExt string, res pve.DiskFormatResolution) {
	res = pve.ResolveVolumeDiskFormat(storageType, requested)
	if pve.StorageUsesFileVolumes(storageType) {
		return res.Format, "." + res.Format, res
	}
	if res.Class == pve.StorageFormatBlock {
		if fromCloudProps {
			return res.Format, "", res
		}
		if res.Format != pve.DiskFormatRaw {
			res.Format, res.Coerced = pve.DiskFormatRaw, true
		}
		return "", "", res
	}
	if fromCloudProps {
		return res.Format, "", res
	}
	return "", "", res
}
