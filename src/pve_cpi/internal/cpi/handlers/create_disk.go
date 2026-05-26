// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// createDiskCloudProperties holds the recognized fields from the cloud_properties
// argument of create_disk. All fields are optional; missing fields fall back to
// CPI configuration defaults.
type createDiskCloudProperties struct {
	// Storage overrides deps.Config.DiskStorage for this disk.
	Storage string `json:"storage"`
	// DiskFormat overrides deps.Config.VMDiskFormat for this disk.
	DiskFormat string `json:"disk_format"`
	// Node pins the new disk to a specific PVE node. Required-on-local-backend
	// only when no vm_cid hint can resolve the owner node; for shared backends
	// this is just a placement preference.
	Node string `json:"node"`
	// Tags is an operator-supplied map applied to the tags field of the VM
	// the disk is attached to. PVE has no native disk-volume tag field, so
	// tags ride on the hosting VM plus a sentinel record in its description
	// (see applyCustomTagsToVM). Tags are deferred when create_disk has no
	// vm_cid hint; set_disk_metadata applies them on the next sync.
	Tags map[string]string `json:"tags"`
}

// HandleCreateDisk returns a Handler for the BOSH CPI create_disk method.
//
// Arguments (positional JSON array):
//
//	[0] size_mb        int     — requested disk size in MiB (must be > 0)
//	[1] cloud_props    object  — optional overrides: storage, disk_format
//	[2] vm_cid         string  — optional; when non-empty and parseable as int,
//	                             used as the VMID label in the volume name;
//	                             otherwise NextDiskVMID allocates a free VMID.
//
// Returns: disk_cid string of the form "<storage>:<volid>".
//
// Node selection: deps.Config.Node is used for all storage operations. PVE
// shared storage (e.g. ceph-rbd, NFS) is cluster-visible; local storage
// requires the disk and its VM to reside on the same node. Operators using
// local storage must ensure vm_cid and the new disk share the same node.
func HandleCreateDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 2 {
			return nil, cpierrors.Cloud("create_disk: expected at least 2 arguments (size_mb, cloud_properties), got %d", len(args))
		}

		var sizeMB int
		if err := json.Unmarshal(args[0], &sizeMB); err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: args[0] size_mb must be an integer")
		}
		if sizeMB <= 0 {
			return nil, cpierrors.Cloud("create_disk: size_mb must be > 0, got %d", sizeMB)
		}

		var cloudProps createDiskCloudProperties
		// args[1] may be null, {}, or a populated object.
		if err := json.Unmarshal(args[1], &cloudProps); err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: args[1] cloud_properties must be an object")
		}

		// args[2] (vm_cid) is optional; an absent or null third argument is fine.
		var vmCID string
		if len(args) >= 3 && args[2] != nil {
			// Accept both JSON string "\"100\"" and JSON null.
			// Ignore unmarshal errors for null/missing.
			_ = json.Unmarshal(args[2], &vmCID)
		}

		// ----------------------------------------------------------------
		// 2. Resolve storage, format, and node from config + cloud_props.
		// ----------------------------------------------------------------
		storage := deps.Config.DiskStorage
		if cloudProps.Storage != "" {
			storage = cloudProps.Storage
		}
		if storage == "" {
			return nil, cpierrors.Cloud(
				"create_disk: no storage configured (disk_storage empty and cloud_properties.storage not set)",
			)
		}

		format := deps.Config.VMDiskFormat
		if cloudProps.DiskFormat != "" {
			format = cloudProps.DiskFormat
		}
		if format == "" {
			format = "qcow2"
		}

		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: backend resolution failed for storage "+storage)
		}
		node, err := backend.NodeForCreate(ctx, vmCID, cloudProps.Node)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk")
		}

		// ----------------------------------------------------------------
		// 3. Compute size in GiB (ceiling division).
		// ----------------------------------------------------------------
		_ = vmCID
		sizeGiB := (sizeMB + 1023) / 1024
		if sizeGiB <= 0 {
			sizeGiB = 1
		}

		// Block storages (lvm/lvmthin/zfspool) reject qcow2 and only accept
		// raw. The CPI's default disk_format is qcow2 which works for file
		// storages (dir/nfs/cifs). If the operator did not explicitly set a
		// disk_format in cloud_properties, omit the format param so PVE
		// auto-selects the right default for the target storage type.
		formatArg := ""
		if cloudProps.DiskFormat != "" {
			formatArg = format
		}

		maxAttempts := deps.Config.VMIDAllocAttempts
		if maxAttempts <= 0 {
			maxAttempts = 5
		}
		// Lock retries scale with how busy the storage is, not with how many
		// VMID collisions we can tolerate. Use the package default unless an
		// operator overrides it explicitly.
		lockAttempts := deps.Config.VMIDAllocAttempts
		if lockAttempts <= 0 {
			lockAttempts = pve.DefaultStorageLockMaxAttempts
		}

		// ----------------------------------------------------------------
		// 4. Allocate a synthetic disk VMID + create the volume.
		//
		// AllocateDiskWithRetry re-runs NextDiskVMID(node, storage) every
		// attempt so the storage scan picks up orphan volumes from prior
		// failed iterations. On non-conflict CreateVolume failures the
		// callback removes any partially-committed volume before propagating.
		// ----------------------------------------------------------------
		var (
			namingVMID     int
			volid          string
			canonicalVolID string
		)

		namingVMID, err = pve.AllocateDiskWithRetry(ctx, deps.PVE, node, storage,
			func(candidate int) error {
				volName := fmt.Sprintf("vm-%d-disk-0", candidate)
				candidateCanonical := fmt.Sprintf("%s:%s", storage, volName)

				var v string
				cerr := pve.RetryOnTransientOrLock(ctx, deps.Logger, "create_disk", lockAttempts, func() error {
					var innerErr error
					v, innerErr = deps.PVE.Storage().CreateVolume(
						ctx, node, storage, sizeGiB, formatArg, candidate, volName,
					)
					return innerErr
				})
				if cerr != nil {
					if pve.IsVMIDConflict(cerr) {
						// Pure conflict (volume name already taken at storage
						// level). No partial commit possible; let the helper
						// pick a new VMID.
						deps.Logger.Info("create_disk: vmid conflict, retrying",
							log.Int("vmid_attempted", candidate),
							log.String("storage", storage),
						)
						return cerr
					}
					// Non-conflict CreateVolume failure: PVE may have
					// partially committed the volume (network drop mid-task,
					// storage daemon timeout). Best-effort: remove it before
					// propagating so retries (which won't run) and operator
					// re-runs see a clean slate.
					rollbackCtx := contextWithoutCancel(ctx)
					if exists, exErr := deps.PVE.Storage().Exists(rollbackCtx, node, storage, candidateCanonical); exErr == nil && exists {
						upid, delErr := deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, candidateCanonical)
						if delErr != nil {
							deps.Logger.Warn("create_disk: orphan volume cleanup after CreateVolume error failed",
								log.String("volid", candidateCanonical),
								log.Err(delErr),
							)
						} else if upid != "" {
							if werr := pve.AwaitTaskWithLogger(rollbackCtx, deps.PVE, node, upid, deps.Logger); werr != nil {
								deps.Logger.Warn("create_disk: orphan volume cleanup await failed",
									log.String("volid", candidateCanonical),
									log.String("upid", upid),
									log.Err(werr),
								)
							} else {
								deps.Logger.Info("create_disk: removed orphan volume after CreateVolume error",
									log.String("volid", candidateCanonical),
								)
							}
						} else {
							deps.Logger.Info("create_disk: removed orphan volume after CreateVolume error",
								log.String("volid", candidateCanonical),
							)
						}
					}
					return cerr
				}

				volid = v
				canonicalVolID = candidateCanonical
				return nil
			},
			pve.IsVMIDConflict,
			maxAttempts,
		)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: CreateVolume failed on node "+node+" storage "+storage)
		}

		// From here on, any failure path must roll back the just-created
		// volume so we never leak storage on partial failure. The flag is
		// flipped to true only on the success return below.
		success := false
		defer func() {
			if success {
				return
			}
			rollbackCtx := contextWithoutCancel(ctx)
			upid, delErr := deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, canonicalVolID)
			if delErr != nil {
				deps.Logger.Warn("create_disk: rollback DeleteVolume failed",
					log.String("volid", canonicalVolID),
					log.Err(delErr),
				)
				return
			}
			if upid != "" {
				if werr := pve.AwaitTaskWithLogger(rollbackCtx, deps.PVE, node, upid, deps.Logger); werr != nil {
					deps.Logger.Warn("create_disk: rollback DeleteVolume await failed",
						log.String("volid", canonicalVolID),
						log.String("upid", upid),
						log.Err(werr),
					)
					return
				}
			}
			deps.Logger.Info("create_disk: rolled back created volume after failure",
				log.String("volid", canonicalVolID),
			)
		}()

		// PVE returns the full volid in canonical "storage:name" form
		// ("local-lvm:vm-9001-disk-0") or an empty string when it echoes
		// the filename. Use the canonical form when empty so the disk_cid
		// is always well-formed. The volid is already a valid disk CID;
		// re-prefixing with storage would double it (e.g. "data:data:...").
		if volid == "" {
			volid = canonicalVolID
		}

		diskCID := volid
		deps.Logger.Info("create_disk",
			log.String("disk_cid", diskCID),
			log.Int("size_mb", sizeMB),
			log.Int("size_gib", sizeGiB),
			log.String("storage", storage),
			log.String("format", format),
			log.Int("naming_vmid", namingVMID),
		)

		// Apply operator-supplied tags to the attached VM, if any. PVE has
		// no native disk-volume tag field, so disk tags can only ride on the
		// hosting VM. When create_disk is called without a vm_cid hint we
		// can't attribute them yet — set_disk_metadata picks them up later.
		if len(cloudProps.Tags) > 0 {
			if vmCID == "" {
				deps.Logger.Warn(
					"create_disk: tags supplied but disk has no attached VM; tags deferred until set_disk_metadata is called",
					log.String("disk_cid", diskCID),
				)
			} else if vmid, parseErr := strconv.Atoi(vmCID); parseErr == nil && vmid > 0 {
				if applyErr := applyCustomTagsToVM(ctx, deps, node, vmid, cloudProps.Tags, diskCID); applyErr != nil {
					// Tagging is best-effort metadata; do not fail volume
					// creation when only the tag write fails. The next
					// set_disk_metadata call will re-apply.
					deps.Logger.Warn("create_disk: failed to apply tags to attached VM",
						log.String("disk_cid", diskCID),
						log.String("vm_cid", vmCID),
						log.Err(applyErr),
					)
				}
			} else {
				deps.Logger.Warn(
					"create_disk: tags supplied but vm_cid is not a valid integer; tags deferred",
					log.String("disk_cid", diskCID),
					log.String("vm_cid", vmCID),
				)
			}
		}

		success = true
		return diskCID, nil
	})
}

// contextWithoutCancel returns a context derived from parent that carries
// parent's values but is detached from its cancellation/deadline. Used so
// rollback I/O completes even when the caller cancelled the request.
func contextWithoutCancel(parent context.Context) context.Context {
	if parent == nil {
		return context.Background()
	}
	return context.WithoutCancel(parent)
}
