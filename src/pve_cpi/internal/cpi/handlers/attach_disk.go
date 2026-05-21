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
//  2. Choose a scsi slot >= 1 (never scsi0 — see chooseSCSISlotSkippingZero
//     for rationale: BOSH agent's resolver resolves /dev/sda to /dev/vda when
//     a virtio root disk exists, so scsi0 attachments are silently shadowed
//     by the root disk). Call qemu.AttachDisk with the chosen DiskID; if the
//     volume is already attached at the chosen slot, the SDK is idempotent.
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
		// 3. Attach disk via SDK at scsi1 or higher (NEVER scsi0).
		//
		// The SDK's default slot selection picks the lowest free index — 0 for
		// a VM with no other scsi disks. That would yield /dev/sda inside the
		// guest, which collides with the BOSH agent's mappedDevicePathResolver:
		//
		//   The resolver strips the "/dev/sd" prefix and probes "/dev/xvd",
		//   "/dev/vd", "/dev/sd" in turn (see create_vm.go agent-Disks.System
		//   note). With a virtio0 root disk, /dev/vda exists, so the resolver
		//   returns /dev/vda for any "/dev/sda" hint — including the persistent
		//   disk hint. The agent then runs persistent-disk partitioning against
		//   the root disk and fails with:
		//
		//     "Persistent disks with many partitions are not supported.
		//      Expected 1, got 4."
		//
		// Reserving scsi0 forces persistent disks to scsi1+ (/dev/sdb+); the
		// resolver finds no /dev/vdb, falls through to /dev/sdb, and operates
		// on the correct disk.
		//
		// Bus is always "scsi" for persistent disks. PVE config disk values are
		// canonical "<storage>:<volname>" (e.g. "data:vm-9003-disk-0"); pass the
		// full disk_cid — a bare volname is rejected with
		// "scsi0.file: invalid format ...".
		// --------------------------------------------------------------------
		const bus = "scsi"

		desiredDiskID, prepErr := chooseSCSISlotSkippingZero(ctx, deps, node, vmid, diskCID)
		if prepErr != nil {
			if pve.IsNotFound(prepErr) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, fmt.Errorf("attach_disk: slot selection for VM %s disk %s: %w", vmCID, diskCID, pve.WrapError(prepErr))
		}

		var diskID string
		err = pve.RetryOnTransient(ctx, deps.Logger, "attach_disk", 0, func() error {
			var attachErr error
			diskID, attachErr = deps.PVE.QEMU().AttachDisk(ctx, node, vmid, diskCID, bus, &qemu.AttachOpts{
				DiskID: desiredDiskID,
			})
			return attachErr
		})
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

// chooseSCSISlotSkippingZero returns the diskID the persistent disk should be
// attached at. It guarantees scsi0 is never used so the BOSH agent's
// mappedDevicePathResolver does not silently resolve a "/dev/sda" hint to the
// virtio root disk (/dev/vda).
//
// Behavior:
//
//   - If volid is already present in the VM config at scsiN with N >= 1, that
//     diskID is reused (idempotent reattach).
//   - If volid is present at scsi0 (legacy from prior CPI versions), the
//     attachment is removed and a fresh scsi index >= 1 is chosen. Persistent
//     disks orphaned at scsi0 have, by construction, never been successfully
//     partitioned by the agent (the resolver always picked /dev/vda instead),
//     so detaching them loses no data.
//   - Otherwise the lowest free scsi index >= 1 is returned.
func chooseSCSISlotSkippingZero(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	volid string,
) (string, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", err
	}

	if existing, ok := qemu.FindDiskIDByVolID(cfg, volid); ok {
		if existing != "scsi0" {
			return existing, nil
		}
		// Legacy scsi0 attachment from a prior CPI version. Detach so the
		// reattach below lands on scsi1+. DetachDisk also sweeps the
		// resulting unusedN entry, leaving the config clean.
		deps.Logger.Warn("attach_disk: migrating legacy scsi0 attachment to scsi1+",
			log.Int("vmid", vmid),
			log.String("volid", volid),
		)
		if detachErr := deps.PVE.QEMU().DetachDisk(ctx, node, vmid, "scsi0"); detachErr != nil {
			return "", fmt.Errorf("detach legacy scsi0: %w", detachErr)
		}
		// Re-read config so NextFreeSCSIIndexAtLeast sees scsi0 as free.
		cfg, err = deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return "", fmt.Errorf("re-read config after scsi0 detach: %w", err)
		}
	}

	idx := nextFreeSCSIIndexAtLeast(cfg, 1)
	return fmt.Sprintf("scsi%d", idx), nil
}

// nextFreeSCSIIndexAtLeast returns the lowest scsi slot index >= floor that is
// not present in cfg. Mirrors qemu.NextIndexForBus semantics but with a
// configurable floor so the caller can reserve low-numbered slots.
func nextFreeSCSIIndexAtLeast(cfg map[string]interface{}, floor int) int {
	used := map[int]bool{}
	for k := range cfg {
		var idx int
		if _, scanErr := fmt.Sscanf(k, "scsi%d", &idx); scanErr == nil {
			used[idx] = true
		}
	}
	for i := floor; ; i++ {
		if !used[i] {
			return i
		}
	}
}
