// disk_format_select.go gathers the disk format a request asks for and logs
// how pve.ResolveDiskFormat turned it into the format sent to PVE. Each disk
// path (root, ephemeral, persistent) resolves against the storage it actually
// lands on, after that storage is chosen.
package handlers

import (
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// requestedDiskFormat returns the first explicit format the cloud-property
// layers carry under keys, and then the job-level pve.vm_disk_format (which a
// cpi-config entry may override). An empty or "auto" value at any rung counts
// as unset. The result is "" when nothing explicit was found, which means the
// storage type decides. fromCloudProps is true only when a cloud-property
// layer supplied the value.
func requestedDiskFormat(r *layeredResolver, cfg *config.CPIConfig, keys ...string) (format string, fromCloudProps bool) {
	if df, ok := r.DiskFormat(keys...); ok {
		return df, true
	}
	if cfg != nil && !config.IsAutoDiskFormat(cfg.VMDiskFormat) {
		return cfg.VMDiskFormat, false
	}
	return "", false
}

// logDiskFormatResolution warns when a resolution departed from what the
// request asked for: an explicit format the block storage cannot hold was
// replaced with raw, or the storage type was unknown so the format fell back
// to qcow2. op names the CPI method and disk role for the log line.
func logDiskFormatResolution(logger *log.Logger, op, storage, storageType string, res pve.DiskFormatResolution) {
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
	case res.Fallback:
		logger.Warn(op+": the storage type is unknown or its lookup failed, so the disk format falls back to qcow2",
			log.String("storage", storage),
			log.String("storage_type", storageType),
			log.String("format", res.Format),
		)
	}
}

// warnEphemeralFormatCoercion warns when an explicit ephemeral format other
// than raw lands on block storage, where pve.EphemeralVolumeFormat always
// allocates raw.
func warnEphemeralFormatCoercion(logger *log.Logger, op, storage, storageType, preferred string) {
	if logger == nil || config.IsAutoDiskFormat(preferred) || preferred == diskFormatRaw ||
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

// managedEphemeralDiskFormat returns the ephemeral format preference a frozen
// VM plan carries. The plan stores the root disk's resolved format, and a plan
// written before ephemeral formats were resolved separately uses that for the
// ephemeral disk too. So we return rootFormat whenever it yields the same
// ephemeral format as requested does, which keeps such plans unchanged, and
// the concrete ephemeral format only when the two differ. When the ephemeral
// format cannot be resolved at all (an unknown type or an unsupported explicit
// value), rootFormat is returned and the creation fails as it did before.
func managedEphemeralDiskFormat(ephemeralType, requested, rootFormat string) string {
	want, err := pve.EphemeralVolumeFormat(ephemeralType, requested)
	if err != nil {
		return rootFormat
	}
	if have, haveErr := pve.EphemeralVolumeFormat(ephemeralType, rootFormat); haveErr == nil && have == want {
		return rootFormat
	}
	return want
}

// finalizeCreateDiskFormat decides what create_disk sends to PVE once the
// pool's type is known. It returns the format parameter ("" means PVE picks),
// the volume-name extension, and the resolution to log and record.
//
// On file storage the resolved format is always sent, and the volume name
// carries it as its extension, because file plugins reject bare names.
//
// On block storage a format set in cloud_properties is sent, with qcow2 or
// vmdk replaced by raw (thick lvm keeps an explicit qcow2). A format that
// came only from the job-level pve.vm_disk_format was never sent to block
// storage, and PVE allocated raw, so that case still sends nothing and
// resolves to raw, with a warning when the job asked for something else.
//
// On an unknown type the old behavior holds: a cloud_properties format is
// sent as is, nothing else is sent, and the name stays bare.
func finalizeCreateDiskFormat(storageType, requested string, fromCloudProps bool) (formatArg, volExt string, res pve.DiskFormatResolution) {
	res = pve.ResolveDiskFormat(storageType, requested)
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
