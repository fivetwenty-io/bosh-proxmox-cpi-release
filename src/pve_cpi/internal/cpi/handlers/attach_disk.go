package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
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
// Returns (v2): disk_hints object {"path": "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>"} per the BOSH CPI v2 spec.
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
//  5. Derive device path from diskID as a PVE-stable by-id symlink:
//     "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>".
//  6. Call agent.UpdateDiskHints so registry-mode agents receive the updated
//     persistent disk mapping. No-op for cloudinit and noagent modes.
//  7. Return disk_hints{"path": devicePath}.
//
// Device path convention:
//
//	virtio0 → /dev/vda  (system disk imported from the stemcell — not a persistent disk)
//	scsi<N> → /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>
//
// We DO NOT return a "/dev/sd<X>" hint. BOSH agent's
// mappedDevicePathResolver probes prefixes in the order
// [/dev/xvd, /dev/vd, /dev/sd] when the hint starts with "/dev/sd", and
// would resolve "/dev/sd<X>" to the virtio root disk "/dev/vd<X>" because
// /dev/vda exists. By returning a path that does not start with "/dev/sd",
// the resolver falls into its else-branch, finds the by-id symlink,
// follows it to whatever /dev/sd<X> the kernel actually assigned, and
// returns that. PVE virtio-scsi-pci sets the QEMU disk serial to
// "drive-scsi<N>", and udev creates the corresponding by-id symlink.
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
		vmCID, diskCID, err := attachDiskParseArgs(args)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; parse disk_cid → storage + volid.
		// --------------------------------------------------------------------
		node, vmid, err := attachDiskResolveNode(ctx, deps, vmCID, diskCID)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 3. Snapshot pre-flight guard.
		//
		// PVE permits attaching a disk to a VM that has existing snapshots, but the
		// newly attached disk is absent from all prior snapshots — on rollback the
		// disk disappears, causing silent data loss. Guard before any mutating PVE
		// call to surface the risk early with an actionable message.
		//
		// Policy (D2-C, D3-C):
		//   HasSnapshots error + RequireSnapshotCheckPass=true  → fail-closed (abort)
		//   HasSnapshots error + RequireSnapshotCheckPass=false → WARN + proceed
		//   Snapshots present + AllowDiskOpsWithSnapshots=false → Cloud error (hard fail)
		//   Snapshots present + AllowDiskOpsWithSnapshots=true  → WARN + proceed
		//   No snapshots                                        → proceed normally
		// --------------------------------------------------------------------
		if err := attachDiskSnapshotGuard(ctx, deps, vmCID, node, vmid, deps.Config, deps.Logger); err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 5. Attach disk via SDK at scsi1 or higher (NEVER scsi0).
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
			return nil, cpierrors.Wrap(pve.WrapError(prepErr), fmt.Sprintf("attach_disk: slot selection for VM %s disk %s", vmCID, diskCID))
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
			return nil, cpierrors.Wrap(wrapped, fmt.Sprintf("attach_disk: AttachDisk failed for VM %s disk %s", vmCID, diskCID))
		}

		// --------------------------------------------------------------------
		// 6+7. Confirm attachment (resolve diskID) and derive device path.
		// --------------------------------------------------------------------
		devicePath, err := attachDiskConfirmAndPath(ctx, deps, vmCID, node, vmid, diskCID, diskID, deps.Logger)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 8. Update agent disk hints (no-op for cloudinit/noagent modes).
		// --------------------------------------------------------------------
		if err := attachDiskUpdateHints(ctx, deps, vmCID, diskCID, vmid, []agent.DiskHint{
			{DiskCID: diskCID, DevicePath: devicePath},
		}, deps.Logger); err != nil {
			return nil, err
		}

		deps.Logger.Info("attach_disk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
			log.String("device_path", devicePath),
		)

		// --------------------------------------------------------------------
		// 9. Return disk_hints (v2 spec: object with "path" key).
		// --------------------------------------------------------------------
		return diskHints{Path: devicePath}, nil
	})
}

// attachDiskParseArgs unmarshals and validates the two positional attach_disk
// arguments. Returns (vmCID, diskCID, err).
//
// Failures:
//   - len(args) < 2           → Cloud error (missing args)
//   - args[0] not a JSON string → Wrap error
//   - vmCID == ""             → Cloud error
//   - args[1] not a JSON string → Wrap error
//   - diskCID == ""           → Cloud error
func attachDiskParseArgs(args []json.RawMessage) (vmCID, diskCID string, err error) {
	if len(args) < 2 {
		return "", "", cpierrors.Cloud("attach_disk: expected 2 arguments (vm_cid, disk_cid), got %d", len(args))
	}

	if err := json.Unmarshal(args[0], &vmCID); err != nil {
		return "", "", cpierrors.Wrap(err, "attach_disk: args[0] vm_cid must be a string")
	}
	if vmCID == "" {
		return "", "", cpierrors.Cloud("attach_disk: args[0] vm_cid must not be empty")
	}

	if err := json.Unmarshal(args[1], &diskCID); err != nil {
		return "", "", cpierrors.Wrap(err, "attach_disk: args[1] disk_cid must be a string")
	}
	if diskCID == "" {
		return "", "", cpierrors.Cloud("attach_disk: args[1] disk_cid must not be empty")
	}

	return vmCID, diskCID, nil
}

// attachDiskResolveNode parses vmCID to a VMID, parses diskCID to its storage
// component, resolves the storage backend, and determines which cluster node
// holds the disk. For local backends it also verifies VM/disk co-location.
//
// Returns (node, vmid, err).
//
// Failures:
//   - vmCID not a positive integer        → VMNotFound
//   - diskCID not in "<storage>:<volid>"  → DiskNotFound
//   - backend resolution error            → Wrap(Cloud)
//   - NodeForExisting: not-found          → DiskNotFound
//   - NodeForExisting: other error        → Wrap
//   - local backend + node mismatch       → Cloud error
func attachDiskResolveNode(ctx context.Context, deps Deps, vmCID, diskCID string) (node string, vmid int, err error) {
	vmid, err = strconv.Atoi(vmCID)
	if err != nil || vmid <= 0 {
		return "", 0, cpierrors.VMNotFound(vmCID)
	}

	storage, _, err := pve.ParseDiskCID(diskCID)
	if err != nil {
		return "", 0, cpierrors.DiskNotFound(diskCID)
	}

	// Resolve disk's owning node via the backend abstraction. For shared
	// backends this is the configured default; for local backends a
	// cluster scan locates the node that holds the volume. attach_disk
	// then runs the QEMU config PUT against that node. PVE's storage
	// content endpoint wants the canonical "<storage>:<volname>" form,
	// which is the disk_cid as-is.
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return "", 0, cpierrors.Wrap(err, fmt.Sprintf("attach_disk: backend resolution failed for storage %q", storage))
	}
	node, err = backend.NodeForExisting(ctx, diskCID)
	if err != nil {
		if pve.IsNotFound(err) {
			return "", 0, cpierrors.DiskNotFound(diskCID)
		}
		return "", 0, cpierrors.Wrap(err, "attach_disk: node lookup failed")
	}

	// For local backends, the disk and VM MUST live on the same node — the
	// SDK call would otherwise PUT a config update on a node that cannot
	// see the volume, producing a confusing storage error. Verify
	// co-location explicitly and surface a clear message when violated.
	if backend.Kind() == pve.BackendLocal {
		if vmNode, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid); lookupErr != nil {
			return "", 0, cpierrors.Wrap(lookupErr, fmt.Sprintf("attach_disk: lookup VM %s node failed", vmCID))
		} else if found && vmNode != "" && vmNode != node {
			return "", 0, cpierrors.Cloud(
				"attach_disk: local-backend disk %s lives on node %s but VM %s runs on node %s — local-storage disks cannot cross nodes",
				diskCID, node, vmCID, vmNode,
			)
		}
	}

	return node, vmid, nil
}

// attachDiskSnapshotGuard runs the snapshot pre-flight check. See the policy
// comment in HandleAttachDisk step 3 for full semantics.
//
// Failures:
//   - HasSnapshots error + cfg.RequireSnapshotCheckPass  → Wrap error (fail-closed)
//   - HasSnapshots error + !cfg.RequireSnapshotCheckPass → nil (WARN + proceed)
//   - snapshots present + !cfg.AllowDiskOpsWithSnapshots → Cloud error (hard fail)
//   - snapshots present + cfg.AllowDiskOpsWithSnapshots  → nil (WARN + proceed)
//   - no snapshots                                       → nil
func attachDiskSnapshotGuard(ctx context.Context, deps Deps, vmCID, node string, vmid int, cfg *config.CPIConfig, logger *log.Logger) error {
	snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if snapErr != nil {
		if cfg.RequireSnapshotCheckPass {
			return cpierrors.Wrap(snapErr,
				"attach_disk: snapshot pre-flight check failed and require_snapshot_check_pass is set",
			)
		}
		logger.Warn("attach_disk: snapshot pre-flight check failed — proceeding (fail-open)",
			log.String("node", node),
			log.Int("vmid", vmid),
			log.Err(snapErr),
		)
		return nil
	}
	if len(snapNames) > 0 {
		if cfg.AllowDiskOpsWithSnapshots {
			logger.Warn("attach_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
				log.String("vm_cid", vmCID),
				log.String("node", node),
				log.String("snapshots", strings.Join(snapNames, ", ")),
			)
			return nil
		}
		return cpierrors.Cloud(
			"attach_disk: VM %s (node %s) has %d snapshot(s) [%s]: attaching a persistent disk while"+
				" snapshots exist makes the disk invisible in all prior snapshot rollbacks."+
				" Delete all snapshots before attaching persistent disks, or set"+
				" pve.allow_disk_ops_with_snapshots=true in CPI config to bypass this guard.",
			vmCID, node, len(snapNames), strings.Join(snapNames, ", "),
		)
	}
	return nil
}

// attachDiskConfirmAndPath resolves the canonical diskID from the current VM
// config (step 6) then derives the PVE-stable device path (step 7).
//
// If ResolveDiskID fails after a successful AttachDisk, the function falls back
// to the diskID returned by AttachDisk and logs a warning — the device path is
// still valid because devicePathByID is a pure function of the slot index.
//
// Failures:
//   - devicePathByID error (non-scsi diskID) → Wrap error
func attachDiskConfirmAndPath(ctx context.Context, deps Deps, vmCID, node string, vmid int, diskCID, fallbackDiskID string, logger *log.Logger) (devicePath string, err error) {
	// --------------------------------------------------------------------
	// 6. Confirm attachment by resolving diskID from current VM config.
	//    This guards against edge cases where AttachDisk returns stale data.
	// --------------------------------------------------------------------
	resolvedDiskID, resolveErr := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, diskCID)
	if resolveErr != nil {
		// ResolveDiskID failure after a successful AttachDisk is unexpected.
		// Use the diskID returned by AttachDisk as fallback; log the anomaly.
		logger.Warn("attach_disk: ResolveDiskID failed after successful attach; using diskID from AttachDisk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("fallback_disk_id", fallbackDiskID),
			log.Err(resolveErr),
		)
		resolvedDiskID = fallbackDiskID
	}

	// --------------------------------------------------------------------
	// 7. Derive device path from diskID.
	//
	// We use a PVE-stable by-id symlink rather than a "/dev/sd<X>" hint
	// because BOSH agent's mappedDevicePathResolver substitutes /dev/sd
	// prefixes with /dev/vd before falling back to /dev/sd (see
	// infrastructure/devicepathresolver/mapped_device_path_resolver.go).
	// A virtio root disk (/dev/vda) on this VM makes "/dev/sda" resolve
	// to the root disk; the agent then runs persistent-disk partitioning
	// against /dev/vda and fails with
	//     "Persistent disks with many partitions are not supported.
	//      Expected 1, got 4."
	//
	// PVE configures virtio-scsi-pci disks with QEMU disk serial
	// "drive-scsi<N>" (where N is the slot index), and udev creates the
	// symlink "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>"
	// pointing to whatever /dev/sd<X> letter the guest kernel assigned.
	// Because this path does not start with "/dev/sd", the agent's
	// resolver skips its substitution table, follows the symlink, and
	// returns the correct device.
	// --------------------------------------------------------------------
	devicePath, devErr := devicePathByID(resolvedDiskID)
	if devErr != nil {
		return "", cpierrors.Wrap(devErr, fmt.Sprintf("attach_disk: cannot compute device path for diskID %q", resolvedDiskID))
	}

	return devicePath, nil
}

// attachDiskUpdateHints calls agent.UpdateDiskHints and wraps any error with
// context. No-op for cloudinit and noagent modes (the Agent implementation
// itself is a no-op for those modes).
//
// Failures:
//   - agent.UpdateDiskHints error → Wrap error
func attachDiskUpdateHints(ctx context.Context, deps Deps, vmCID, diskCID string, vmid int, hints []agent.DiskHint, _ *log.Logger) error {
	if err := deps.Agent.UpdateDiskHints(ctx, vmid, hints); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("attach_disk: UpdateDiskHints failed for VM %s disk %s", vmCID, diskCID))
	}
	return nil
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

	if existing, ok := pve.FindDiskIDByVolID(qemu.ParseDisks(cfg), volid); ok {
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
			return "", cpierrors.Wrap(detachErr, "attach_disk: detach legacy scsi0")
		}
		// Re-read config so NextFreeSCSIIndexAtLeast sees scsi0 as free.
		cfg, err = deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return "", cpierrors.Wrap(err, "attach_disk: re-read config after scsi0 detach")
		}
	}

	idx := nextFreeSCSIIndexAtLeast(cfg, 1)
	return fmt.Sprintf("scsi%d", idx), nil
}

// devicePathByID returns the PVE-stable udev by-id symlink path for the
// disk attached at diskID. PVE configures virtio-scsi-pci disks with the
// QEMU disk serial "drive-scsi<N>" (where N is the slot index from the
// "scsiN" key), and udev's 60-persistent-storage.rules creates a symlink
//
//	/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>
//
// pointing to whatever /dev/sd<X> the guest kernel assigned. Returning
// this path (instead of a guessed /dev/sd<X>) bypasses BOSH agent's
// mappedDevicePathResolver substitution that would otherwise collide with
// the virtio root disk.
//
// Returns an error if diskID is not a scsi slot.
func devicePathByID(diskID string) (string, error) {
	var idx int
	if _, err := fmt.Sscanf(diskID, "scsi%d", &idx); err != nil {
		return "", cpierrors.Cloud("attach_disk: diskID %q is not a scsi slot", diskID)
	}
	return fmt.Sprintf("/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi%d", idx), nil
}

// nextFreeSCSIIndexAtLeast returns the lowest scsi slot index >= floor that is
// not present in cfg. Mirrors qemu.NextIndexForBus semantics but with a
// configurable floor so the caller can reserve low-numbered slots.
func nextFreeSCSIIndexAtLeast(cfg map[string]any, floor int) int {
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
