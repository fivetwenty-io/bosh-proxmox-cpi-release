// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// diskHints is the v2 attach_disk return value. The Director uses the "path" key
// to tell the BOSH agent where the persistent disk is mounted.
//
// Shape: {"path": "/dev/sdX"}
//
// The Director passes disk_hints back to the agent via the agent API so the agent
// can populate /var/vcap/store correctly without requiring a registry lookup.
type diskHints struct {
	Path string `json:"path"`
}

// HandleAttachDisk returns a Handler for the BOSH CPI v2 attach_disk method.
//
// Arguments (positional JSON array):
//
//	[0] vm_cid   string — VMID of the target VM (integer as string, e.g. "100")
//	[1] disk_cid string — persistent disk CID in "<storage>:<volid>" form
//
// Returns (v2): disk_hints object {"path": "/dev/sdX"} per the BOSH CPI v2 spec.
//
// Logic:
//  1. Parse vm_cid to VMID int; parse disk_cid to storage+volid components.
//  2. Call qemu.AttachDisk — SDK resolves the next free scsi/virtio slot, sets
//     the disk in VM config via PUT /nodes/{node}/qemu/{vmid}/config, and returns
//     the assigned diskID (e.g., "scsi1"). If the volume is already attached the
//     SDK detects it and returns the existing diskID (idempotent by SDK design).
//  3. AttachDisk does not return a UPID (it is a synchronous config PUT, not an
//     async task), so no AwaitTask is needed.
//  4. Resolve the final diskID from the current VM config via pve.ResolveDiskID to
//     confirm the attachment and get the canonical slot name.
//  5. Derive device path from diskID using qemu.GuessDevicePath.
//  6. Call agent.UpdateDiskHints so registry-mode agents receive the updated
//     persistent disk mapping. No-op for cloudinit and noagent modes.
//  7. Return disk_hints{"path": devicePath}.
//
// Device path convention:
//
//	virtio0 → /dev/vda  (system disk imported from the stemcell — not a persistent disk)
//	scsi1   → /dev/sdb  (ephemeral / first persistent disk; scsi0 is unused)
//	scsi2   → /dev/sdc, scsi3 → /dev/sdd, …
//
// The SDK's GuessDevicePath maps bus+index to the above convention.
//
// Bus selection: "scsi" is always used for persistent disks regardless of
// VMDiskFormat. The scsi bus matches the virtio-scsi-single controller that
// create_vm configures. The system disk lives on virtio0 because the BOSH
// openstack-kvm stemcell's agent.json sets DevicePathResolutionType=virtio
// (resolves /dev/disk/by-id/virtio-* for the root device).
func HandleAttachDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// --------------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// --------------------------------------------------------------------
		if len(args) < 2 {
			return nil, fmt.Errorf("attach_disk: expected 2 arguments (vm_cid, disk_cid), got %d", len(args))
		}

		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, fmt.Errorf("attach_disk: args[0] vm_cid must be a string: %w", err)
		}
		if vmCID == "" {
			return nil, fmt.Errorf("attach_disk: args[0] vm_cid must not be empty")
		}

		var diskCID string
		if err := json.Unmarshal(args[1], &diskCID); err != nil {
			return nil, fmt.Errorf("attach_disk: args[1] disk_cid must be a string: %w", err)
		}
		if diskCID == "" {
			return nil, fmt.Errorf("attach_disk: args[1] disk_cid must not be empty")
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; parse disk_cid → storage + volid.
		// --------------------------------------------------------------------
		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		storage, _, err := pve.ParseDiskCID(diskCID)
		if err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// Resolve disk's owning node via the backend abstraction. For shared
		// backends this is the configured default; for local backends a
		// cluster scan locates the node that holds the volume. attach_disk
		// then runs the QEMU config PUT against that node. PVE's storage
		// content endpoint wants the canonical "<storage>:<volname>" form,
		// which is the disk_cid as-is.
		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, fmt.Errorf("attach_disk: backend resolution failed for storage %q: %w", storage, err)
		}
		node, err := backend.NodeForExisting(ctx, diskCID)
		if err != nil {
			if pve.IsNotFound(err) {
				return nil, cpierrors.DiskNotFound(diskCID)
			}
			return nil, fmt.Errorf("attach_disk: %w", err)
		}

		// For local backends, the disk and VM MUST live on the same node — the
		// SDK call would otherwise PUT a config update on a node that cannot
		// see the volume, producing a confusing storage error. Verify
		// co-location explicitly and surface a clear message when violated.
		if backend.Kind() == pve.BackendLocal {
			if vmNode, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid); lookupErr != nil {
				return nil, fmt.Errorf("attach_disk: lookup VM %s node failed: %w", vmCID, lookupErr)
			} else if found && vmNode != "" && vmNode != node {
				return nil, cpierrors.Cloud(
					"attach_disk: local-backend disk %s lives on node %s but VM %s runs on node %s — local-storage disks cannot cross nodes",
					diskCID, node, vmCID, vmNode,
				)
			}
		}

		// --------------------------------------------------------------------
		// 3. Attach disk via SDK. Bus is always "scsi" for persistent disks.
		//    The SDK auto-selects the next free slot index when opts.DiskID is
		//    empty. If the volid is already attached, the SDK returns the
		//    existing diskID without modifying the config (idempotent).
		// --------------------------------------------------------------------
		const bus = "scsi"

		// PVE config disk values are canonical "<storage>:<volname>"
		// (e.g. "data:vm-9003-disk-0"). Pass the full disk_cid; a bare
		// volname is rejected with "scsi0.file: invalid format ...".
		diskID, err := deps.PVE.QEMU().AttachDisk(ctx, node, vmid, diskCID, bus, nil)
		if err != nil {
			wrapped := pve.WrapError(err)
			if pve.IsNotFound(err) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, fmt.Errorf("attach_disk: AttachDisk failed for VM %s disk %s: %w", vmCID, diskCID, wrapped)
		}

		// --------------------------------------------------------------------
		// 4. Confirm attachment by resolving diskID from current VM config.
		//    This guards against edge cases where AttachDisk returns stale data.
		// --------------------------------------------------------------------
		resolvedDiskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, diskCID)
		if err != nil {
			// ResolveDiskID failure after a successful AttachDisk is unexpected.
			// Use the diskID returned by AttachDisk as fallback; log the anomaly.
			deps.Logger.Warn("attach_disk: ResolveDiskID failed after successful attach; using diskID from AttachDisk",
				log.String("vm_cid", vmCID),
				log.String("disk_cid", diskCID),
				log.String("fallback_disk_id", diskID),
				log.Err(err),
			)
			resolvedDiskID = diskID
		}

		// --------------------------------------------------------------------
		// 5. Derive device path from diskID.
		//    qemu.GuessDevicePath: scsiN → /dev/sd{a+N}, virtioN → /dev/vd{a+N}.
		// --------------------------------------------------------------------
		devicePath := qemu.GuessDevicePath(resolvedDiskID)
		if devicePath == "" {
			return nil, fmt.Errorf("attach_disk: cannot compute device path for diskID %q", resolvedDiskID)
		}

		// --------------------------------------------------------------------
		// 6. Update agent disk hints (no-op for cloudinit/noagent modes).
		// --------------------------------------------------------------------
		if err := deps.Agent.UpdateDiskHints(ctx, vmid, []agent.DiskHint{
			{DiskCID: diskCID, DevicePath: devicePath},
		}); err != nil {
			return nil, fmt.Errorf("attach_disk: UpdateDiskHints failed for VM %s disk %s: %w", vmCID, diskCID, err)
		}

		deps.Logger.Info("attach_disk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("disk_id", resolvedDiskID),
			log.String("device_path", devicePath),
		)

		// --------------------------------------------------------------------
		// 7. Return disk_hints (v2 spec: object with "path" key).
		// --------------------------------------------------------------------
		return diskHints{Path: devicePath}, nil
	})
}
