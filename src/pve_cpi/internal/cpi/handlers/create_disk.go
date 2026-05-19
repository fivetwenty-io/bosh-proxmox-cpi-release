// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"

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
			return nil, fmt.Errorf("create_disk: expected at least 2 arguments (size_mb, cloud_properties), got %d", len(args))
		}

		var sizeMB int
		if err := json.Unmarshal(args[0], &sizeMB); err != nil {
			return nil, fmt.Errorf("create_disk: args[0] size_mb must be an integer: %w", err)
		}
		if sizeMB <= 0 {
			return nil, fmt.Errorf("create_disk: size_mb must be > 0, got %d", sizeMB)
		}

		var cloudProps createDiskCloudProperties
		// args[1] may be null, {}, or a populated object.
		if err := json.Unmarshal(args[1], &cloudProps); err != nil {
			return nil, fmt.Errorf("create_disk: args[1] cloud_properties must be an object: %w", err)
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
			return nil, fmt.Errorf(
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
			return nil, fmt.Errorf("create_disk: backend resolution failed for storage %q: %w", storage, err)
		}
		node, err := backend.NodeForCreate(ctx, vmCID, cloudProps.Node)
		if err != nil {
			return nil, fmt.Errorf("create_disk: %w", err)
		}

		// ----------------------------------------------------------------
		// 3. Allocate a synthetic disk VMID from [9000, 9999].
		//
		// vmCID is intentionally NOT used for naming: the owning VM already
		// has its system disk named "vm-{vmcid}-disk-0", and lvmthin/zfspool
		// volume names must be unique per storage. Allocating a fresh
		// synthetic VMID keeps every BOSH persistent disk in its own
		// namespace ("vm-9xxx-disk-0").
		// ----------------------------------------------------------------
		_ = vmCID
		// Pass node+storage so NextDiskVMID also unions volume names from
		// the disk_storage. Without this, orphan volumes from prior failed
		// runs ("vm-9000-disk-0" with no matching VM) are invisible and
		// the same VMID gets handed out again, colliding on lvcreate.
		namingVMID, err := pve.NextDiskVMID(ctx, deps.PVE, node, storage)
		if err != nil {
			return nil, fmt.Errorf("create_disk: failed to allocate disk VMID: %w", err)
		}

		// ----------------------------------------------------------------
		// 4. Compute size in GiB (ceiling division).
		// ----------------------------------------------------------------
		sizeGiB := (sizeMB + 1023) / 1024
		if sizeGiB <= 0 {
			sizeGiB = 1
		}

		// ----------------------------------------------------------------
		// 5. Compose volume name and call CreateVolume.
		//
		// Volume name must follow PVE's vm-{vmid}-disk-{N} convention.
		// lvmthin/zfspool storages enforce this strictly — names like
		// "bosh-disk-X" are rejected. For BOSH persistent disks each volume
		// owns its own synthetic VMID (allocated by NextDiskVMID), so disk
		// index is always 0.
		// ----------------------------------------------------------------
		volName := fmt.Sprintf("vm-%d-disk-0", namingVMID)

		// Block storages (lvm/lvmthin/zfspool) reject qcow2 and only accept
		// raw. The CPI's default disk_format is qcow2 which works for file
		// storages (dir/nfs/cifs). If the operator did not explicitly set a
		// disk_format in cloud_properties, omit the format param so PVE
		// auto-selects the right default for the target storage type.
		formatArg := ""
		if cloudProps.DiskFormat != "" {
			formatArg = format
		}

		// The canonical PVE volid form for a block-storage VM disk volume.
		// Used both as the delete target on rollback and as the source for
		// the BOSH disk_cid when PVE returns an empty volid on create.
		canonicalVolID := fmt.Sprintf("%s:%s", storage, volName)

		volid, err := deps.PVE.Storage().CreateVolume(ctx, node, storage, sizeGiB, formatArg, namingVMID, volName)
		if err != nil {
			// CreateVolume may fail after PVE has partially committed the
			// volume (network drop mid-task, storage daemon timeout, etc.).
			// Best-effort: check if the volume now exists and delete it so
			// the next retry sees a clean slate.
			rollbackCtx := contextWithoutCancel(ctx)
			if exists, exErr := deps.PVE.Storage().Exists(rollbackCtx, node, storage, canonicalVolID); exErr == nil && exists {
				if delErr := deps.PVE.Storage().DeleteVolume(rollbackCtx, node, storage, canonicalVolID); delErr != nil {
					deps.Logger.Warn("create_disk: orphan volume cleanup after CreateVolume error failed",
						log.String("volid", canonicalVolID),
						log.Err(delErr),
					)
				} else {
					deps.Logger.Info("create_disk: removed orphan volume after CreateVolume error",
						log.String("volid", canonicalVolID),
					)
				}
			}
			return nil, fmt.Errorf("create_disk: CreateVolume failed on node %q storage %q: %w", node, storage, err)
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
			if delErr := deps.PVE.Storage().DeleteVolume(rollbackCtx, node, storage, canonicalVolID); delErr != nil {
				deps.Logger.Warn("create_disk: rollback DeleteVolume failed",
					log.String("volid", canonicalVolID),
					log.Err(delErr),
				)
				return
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
