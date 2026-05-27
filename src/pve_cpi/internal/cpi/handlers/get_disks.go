package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

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
	"scsi0":   true,
	"virtio0": true,
	"ide0":    true,
	"ide2":    true, // conventional cloudinit slot
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
//     comma in the option string) and format it as a disk_cid.
//  7. Return the list. An empty list is a valid response when no persistent disks
//     are attached.
//
// VMNotFound: if the cluster scan does not find the VMID, or the Config API
// returns a 404, the handler returns a VMNotFound error.
func HandleGetDisks(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
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

		deps.Logger.Debug("get_disks: VM located via cluster scan",
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
		// ----------------------------------------------------------------
		allDisks := qemu.ParseDisks(cfg)
		diskCIDs := make([]string, 0, len(allDisks))

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
				deps.Logger.Warn("get_disks: skipping disk with empty volid",
					log.String("vm_cid", vmCID),
					log.String("disk_slot", diskSlot),
					log.String("opt_str", optStr),
				)
				continue
			}

			diskCIDs = append(diskCIDs, bareVolid)
		}

		deps.Logger.Info("get_disks",
			log.String("vm_cid", vmCID),
			log.Int("disk_count", len(diskCIDs)),
		)

		return diskCIDs, nil
	})
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
