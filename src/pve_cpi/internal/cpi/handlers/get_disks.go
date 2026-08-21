package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// systemDiskIDs is the set of disk slot names that are reserved for system use
// and must be excluded from the list of BOSH persistent disks. Specifically:
//
//   - scsi0, virtio0: system root disk (OS volume)
//   - ide2, scsi2 (when used for cloudinit): cloudinit drive
//
// The cloudinit drive is identified by its option string containing "media=cdrom"
// rather than by slot name, since the slot may vary. The system disk is always
// the first disk on the boot bus (index 0).
var systemDiskSlots = map[string]bool{
	diskKeyScsi0:   true,
	diskKeyVirtio0: true,
	"ide0":         true,
	"ide2":         true, // conventional cloudinit slot
}

// HandleGetDisks returns a Handler for the BOSH CPI get_disks method.
//
// Arguments (positional JSON array):
//
//	[0] vm_cid  string — VMID of the target VM (integer as string, e.g. "100")
//
// Returns: []string of disk_cid values, one per persistent disk attached to the VM.
//
// Logic:
//  1. Parse vm_cid to VMID int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     Not-found -> VMNotFound. Transport error -> propagate.
//  3. Fetch VM config via SDK qemu.Config.
//  4. Parse all disk entries from config using SDK qemu.ParseDisks.
//  5. Filter out system disks (scsi0/virtio0) and cloudinit drives (ide2 or
//     any disk whose option string contains "media=cdrom").
//  6. For each remaining disk, extract the bare volid (the part before the first
//     comma in the option string). Return the recorded disk_cid from the VM's
//     description sentinel (pve.GetAttachedDiskCIDs) when attach_disk or
//     create_vm's persistent-disk attach recorded one for this volid;
//     otherwise re-encode the bare volid through pve.EncodeDiskCID (a
//     metadata-free pvd- envelope) so the fallback is a value every other
//     disk handler (attach_disk, detach_disk, delete_disk, ...) can decode.
//  7. Return the list. An empty list is a valid response when no persistent disks
//     are attached.
//
// VMNotFound: if the cluster scan does not find the VMID, or the Config API
// returns a 404, the handler returns a VMNotFound error.
func HandleGetDisks(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("get_disks: expected 1 argument (vm_cid), got 0")
		}

		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Wrap(err, "get_disks: args[0] vm_cid must be a string")
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("get_disks: args[0] vm_cid must not be empty")
		}

		vmid, err := strconv.Atoi(vmCID)
		if err != nil || vmid <= 0 {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		// ----------------------------------------------------------------
		// 2. Locate VM via cluster scan to get authoritative node.
		// ----------------------------------------------------------------
		node, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(lookupErr), "get_disks: locate VM "+vmCID)
		}
		if !found || node == "" {
			return nil, cpierrors.VMNotFound(vmCID)
		}

		deps.Log(ctx).Debug("get_disks: VM located via cluster scan",
			log.String("vm_cid", vmCID),
			log.String("node", node),
		)

		// ----------------------------------------------------------------
		// 3. Fetch VM config.
		// ----------------------------------------------------------------
		cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			if pve.IsNotFound(err) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(pve.WrapError(err), "get_disks: fetch config for VM "+vmCID)
		}

		// ----------------------------------------------------------------
		// 4. Parse disk entries from config and filter to persistent disks.
		//
		// CID fidelity: the Director compares its stored disk_cid strings
		// against this list on cloudcheck (bosh.io get_disks contract). A
		// bare volid is not always what the Director stored — attach_disk
		// may have received an opaque envelope CID (metadata that cannot be
		// reconstructed from PVE state) — so recordedCIDs, read from the VM's
		// description sentinel, supplies the exact string when attach_disk
		// or create_vm's persistent-disk attach recorded one. Absent entry
		// (disk attached by a pre-envelope CPI release, or the sentinel
		// write failed) falls back to the bare volid re-encoded through
		// pve.EncodeDiskCID (metadata-free): the codec rejects a raw
		// "<storage>:<volid>" string everywhere else (attach_disk,
		// detach_disk, delete_disk, ...), so a fallback CID must be a
		// well-formed envelope, not the pre-envelope-era bare form.
		// ----------------------------------------------------------------
		allDisks := qemu.ParseDisks(cfg)
		recordedCIDs := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg))
		diskCIDs := make([]string, 0, len(allDisks))
		recordedCount, fallbackCount := 0, 0

		for diskSlot, optStr := range allDisks {
			// Skip system disk slots by name.
			if systemDiskSlots[diskSlot] {
				continue
			}
			// Skip cloudinit / cdrom drives.
			if isCloudinitDrive(optStr) {
				continue
			}

			// Extract bare volid: the portion before the first comma.
			bareVolid := bareVolidFromOptStr(optStr)
			if bareVolid == "" {
				deps.Log(ctx).Warn("get_disks: skipping disk with empty volid",
					log.String("vm_cid", vmCID),
					log.String("disk_slot", diskSlot),
					log.String("opt_str", optStr),
				)
				continue
			}

			if recordedCID := recordedCIDForDrive(recordedCIDs, optStr, bareVolid); recordedCID != "" {
				diskCIDs = append(diskCIDs, recordedCID)
				recordedCount++
			} else {
				// No sentinel entry for this volid: wrap it in a metadata-free
				// pvd- envelope rather than returning the raw "<storage>:<volid>"
				// string, which every other disk handler now hard-rejects.
				// EncodeDiskCID only fails on an empty bareCID, which cannot
				// happen here (checked above), but the error is still handled
				// rather than ignored to honor the function's documented contract.
				encoded, encErr := pve.EncodeDiskCID(bareVolid, nil)
				if encErr != nil {
					return nil, cpierrors.Wrap(encErr, fmt.Sprintf("get_disks: encode fallback CID for VM %s disk %s", vmCID, bareVolid))
				}
				diskCIDs = append(diskCIDs, encoded)
				fallbackCount++
			}
		}

		deps.Log(ctx).Debug("get_disks: CID resolution",
			log.String("vm_cid", vmCID),
			log.Int("recorded_cid_count", recordedCount),
			log.Int("bare_volid_fallback_count", fallbackCount),
		)

		deps.Log(ctx).Info("get_disks",
			log.String("vm_cid", vmCID),
			log.Int("disk_count", len(diskCIDs)),
		)

		return diskCIDs, nil
	})
}

// recordedCIDForDrive looks up the Director-supplied CID recorded for one
// drive entry, dual-keyed: stable-ID disks are recorded under the bpd- serial
// their drive entry carries (rename-proof), legacy disks under the bare
// volid. Empty when neither key has an entry.
func recordedCIDForDrive(recordedCIDs map[string]string, optStr, bareVolid string) string {
	if serial, hasSerial := pve.StableIDFromDriveOptStr(optStr); hasSerial {
		if cid := recordedCIDs[serial]; cid != "" {
			return cid
		}
	}
	return recordedCIDs[bareVolid]
}

// isCloudinitDrive reports whether a disk option string represents a cloudinit
// or cdrom drive, which must be excluded from the persistent disk list.
// PVE cloudinit drives contain "media=cdrom" in their option string.
func isCloudinitDrive(optStr string) bool {
	for _, part := range strings.Split(optStr, ",") {
		if strings.TrimSpace(part) == "media=cdrom" {
			return true
		}
	}
	return false
}

// bareVolidFromOptStr extracts the volid component from a PVE disk option string.
// The option string format is "<volid>[,key=value,...]". The volid is everything
// before the first comma.
func bareVolidFromOptStr(optStr string) string {
	if idx := strings.IndexByte(optStr, ','); idx >= 0 {
		return optStr[:idx]
	}
	return optStr
}
