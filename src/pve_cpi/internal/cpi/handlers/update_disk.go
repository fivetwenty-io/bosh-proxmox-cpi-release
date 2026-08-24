package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// HandleUpdateDisk returns a Handler for the BOSH CPI update_disk method.
// This is a PVE CPI extension (not part of the core BOSH CPI v2 spec).
//
// Arguments (positional JSON array):
//
//	[0] disk_cid     string         — disk CID in the "pvd-"/"pvz-" envelope
//	                                  form emitted by create_disk
//	[1] update_spec  map[string]any — update specification (see below)
//
// update_spec recognized fields:
//
//	"size"     int    — new total disk size in MiB (triggers additive resize)
//	"iothread" bool   — enable/disable dedicated I/O thread
//	"cache"    string — PVE cache mode: "none", "writethrough", "writeback",
//	                    "unsafe", "directsync" (empty → remove cache option)
//	"discard"  string — PVE discard mode: "ignore" or "on" (empty → remove
//	                    discard option)
//	"ssd"      bool   — mark as SSD emulation
//	"backup"   bool   — include disk in vzdump backups
//	"mbps_rd"  int    — read throughput limit in MB/s (0 → remove limit)
//	"mbps_wr"  int    — write throughput limit in MB/s (0 → remove limit)
//	"iops_rd"  int    — read IOPS limit (0 → remove limit)
//	"iops_wr"  int    — write IOPS limit (0 → remove limit)
//
// Returns: null on success (BOSH void method).
//
// Logic (full update semantics):
//  1. Parse disk_cid, resolve it through the identity seam, and locate the
//     holder (a workload VM or a parker). Free-floating disk → DetachedDisk
//     CloudError (no carrier exists for the update).
//  2. If update_spec has "size", route through resize logic (delta computation).
//  3. Record the option changes as the disk's override map — on the holder
//     VM's bosh_disk_opt_overlays sentinel while attached, in the parker's
//     provenance entry while parked — FAIL-CLOSED, before the drive string is
//     touched. The record is what makes the update survive a detach/attach
//     cycle, whose attach otherwise rebuilds the drive string from config and
//     CID options alone.
//  4. Attached: read the current drive option string, merge the new options
//     over it (preserving unspecified options and the identity serial), and
//     PUT it back via AttachDisk with the explicit DiskID.
//     Parked: skip the in-place rewrite entirely — the parker slot's option
//     string is CPI-owned (volid plus serial only) and the next attach bakes
//     the merged string; the update takes effect then.
//
// Empty update_spec (all absent or zero-value fields): returns nil (no-op).
func HandleUpdateDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 2 {
			return nil, cpierrors.Cloud("update_disk: expected 2 arguments (disk_cid, update_spec), got %d", len(args))
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil {
			return nil, cpierrors.Wrap(err, "update_disk: args[0] disk_cid must be a string")
		}
		if diskCID == "" {
			return nil, cpierrors.Cloud("update_disk: args[0] disk_cid must not be empty")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "update_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}
		// Resolve to the volume's current name (identity seam): the holder
		// scan, slot resolver, and option RMW below all read the VM config,
		// which carries the post-reassignment name for stable-ID disks. The
		// RMW merges from the live option string, so the identity serial is
		// preserved without special handling.
		rd, resolveErr := resolveDiskForOp(ctx, deps, "update_disk", diskCID, bareDiskCID, meta)
		if resolveErr != nil {
			return nil, resolveErr
		}

		// update_spec may be null or missing; treat as empty map.
		var updateSpec map[string]any
		if args[1] != nil {
			if err := json.Unmarshal(args[1], &updateSpec); err != nil {
				return nil, cpierrors.Wrap(err, "update_disk: args[1] update_spec must be an object")
			}
		}

		// ----------------------------------------------------------------
		// 2. Validate disk_cid format. The holder scan and diskID resolver
		//    match against the full canonical "<storage>:<volume>" form that
		//    PVE stores in the VM config (e.g. "data:vm-9559-disk-0,size=2G"),
		//    so pass the resolved volid through — NOT the storage-stripped
		//    volume part.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(rd.volid); err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate the holder, parker-aware. The pre-override behavior wrote
		//    updated options onto whatever VM held the volume — including a
		//    parker, whose drive string the next unpark discards. Classifying
		//    the holder routes a parked disk to the record-only path instead.
		// ----------------------------------------------------------------
		holder, holderErr := updateDiskHolder(ctx, deps, diskCID, &rd)
		if holderErr != nil {
			return nil, holderErr
		}
		if !holder.Found {
			return nil, cpierrors.DetachedDisk(
				"update_disk: detached disk cannot be updated — disk %q not attached to any VM", diskCID,
			)
		}
		if strandedErr := strandedParkerRefusal(deps, "update_disk", diskCID, holder); strandedErr != nil {
			return nil, strandedErr
		}
		if holder.IsParker {
			return nil, updateParkedDisk(ctx, deps, diskCID, rd, holder, updateSpec)
		}
		return nil, updateAttachedDisk(ctx, deps, diskCID, rd, holder, updateSpec)
	})
}

// updateDiskHolder resolves who currently holds the disk. A stable-ID disk's
// holder came out of the identity resolution (after converging an interrupted
// transfer, since this is a mutating handler); a legacy disk pays the same
// cluster scan the other disk handlers do.
func updateDiskHolder(ctx context.Context, deps Deps, diskCID string, rd *resolvedDisk) (pve.DiskHolder, error) {
	if rd.stableID != "" {
		refreshed, err := resumeTransferIfNeeded(ctx, deps, "update_disk", *rd)
		if err != nil {
			return pve.DiskHolder{}, err
		}
		*rd = refreshed
		if rd.holder != nil {
			return *rd.holder, nil
		}
		return pve.DiskHolder{}, nil
	}
	holder, err := pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), rd.volid, parkerReadConfigFor(deps))
	if err != nil {
		return pve.DiskHolder{}, wrapHolderScanError(err, fmt.Sprintf("update_disk: locate VM for disk %q", diskCID))
	}
	return holder, nil
}

// updateSpecSizeMB extracts and validates the optional "size" field. Returns
// (size, present, err).
func updateSpecSizeMB(updateSpec map[string]any) (int, bool, error) {
	sizeRaw, hasSz := updateSpec["size"]
	if !hasSz {
		return 0, false, nil
	}
	newSizeMB, ok := toInt(sizeRaw)
	if !ok || newSizeMB <= 0 {
		return 0, false, cpierrors.Cloud("update_disk: update_spec.size must be a positive integer (MiB), got %v", sizeRaw)
	}
	return newSizeMB, true, nil
}

// updateAttachedDisk applies an update to a disk held by a workload VM:
// resize, then the fail-closed override record, then the in-place drive
// rewrite. The record is written before the drive so a failure between the
// two converges forward — the recorded override is re-applied at the next
// attach and a Director retry re-runs the rewrite — whereas the reverse order
// would leave a drive updated with no record, the exact silent-revert state
// this method exists to prevent.
func updateAttachedDisk(ctx context.Context, deps Deps, diskCID string, rd resolvedDisk, holder pve.DiskHolder, updateSpec map[string]any) error {
	node, vmid := holder.Node, holder.VMID

	diskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, rd.volid)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("update_disk: cannot resolve diskID for %s on VM %d", diskCID, vmid))
	}

	if newSizeMB, hasSz, sizeErr := updateSpecSizeMB(updateSpec); sizeErr != nil {
		return sizeErr
	} else if hasSz {
		if err := resizeDiskInternal(ctx, deps, node, vmid, diskID, diskCID, newSizeMB); err != nil {
			return err
		}
	}

	// Build option key-value map from the remaining update_spec fields. Only
	// recognized option fields are applied; unknown fields are silently
	// ignored to maintain forward compatibility. An empty-string value is a
	// deletion, of both the live drive key and any recorded override for it.
	newOpts := updateSpecToOptions(updateSpec)
	if len(newOpts) == 0 {
		return nil // size-only update or truly empty spec
	}

	// Record the override map first, fail-closed: if the record cannot be
	// written the drive is left untouched and the error is returned — the
	// record carries the update's durability, so it cannot inherit the
	// best-effort contract of the neighboring provenance writers.
	if _, ovErr := pve.ApplyVMDiskOptOverlay(ctx, deps.PVE, node, vmid, rd.sentinelKey(),
		[]string{rd.volid, rd.birth}, newOpts); ovErr != nil {
		return retriableUnlessPermanent(ovErr,
			fmt.Sprintf("update_disk: record option overrides for disk %s (fail-closed: the drive was not modified)", diskCID))
	}

	// Read the current drive option string and merge the new options over it.
	// The merge starts from the live string, so unspecified options — the
	// identity serial included — are preserved.
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("update_disk: read VM %d config", vmid))
	}
	diskOptStr, ok := cfg[diskID].(string)
	if !ok || diskOptStr == "" {
		return cpierrors.Cloud("update_disk: disk %q not found in VM %d config after locate", diskID, vmid)
	}
	bareVolid, existingOpts := splitDiskOptStr(diskOptStr)
	newOptStr := buildDiskOptStr(bareVolid, mergeDiskOptions(existingOpts, newOpts))

	// Use AttachDisk with opts.DiskID set to the current slot so the SDK
	// issues PUT /nodes/{node}/qemu/{vmid}/config with {diskID: newOptStr}.
	_, err = deps.PVE.QEMU().AttachDisk(ctx, node, vmid, newOptStr, "scsi", &qemu.AttachOpts{
		DiskID: diskID,
	})
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("update_disk: update disk options for %s on VM %d", diskCID, vmid))
	}

	deps.Log(ctx).Info("update_disk",
		log.String("disk_cid", diskCID),
		log.Int("vmid", vmid),
		log.String("disk_id", diskID),
		log.String("new_opt_str", newOptStr),
	)
	return nil
}

// updateParkedDisk applies an update to a disk held by a parker: resize acts
// on the volume directly through the parker slot, and option changes are
// recorded in the parker's provenance entry only. The parker slot's drive
// string stays CPI-owned (volid plus serial) — the next attach bakes the
// merged string, which is when the option update takes effect.
func updateParkedDisk(ctx context.Context, deps Deps, diskCID string, rd resolvedDisk, holder pve.DiskHolder, updateSpec map[string]any) error {
	if newSizeMB, hasSz, sizeErr := updateSpecSizeMB(updateSpec); sizeErr != nil {
		return sizeErr
	} else if hasSz {
		if holder.Slot == "" {
			return cpierrors.Retriable(
				"update_disk: disk %s confirmed on parker vmid %d but slot not found in config (possible race)",
				diskCID, holder.VMID)
		}
		if err := resizeDiskInternal(ctx, deps, holder.Node, holder.VMID, holder.Slot, diskCID, newSizeMB); err != nil {
			return err
		}
	}

	newOpts := updateSpecToOptions(updateSpec)
	if len(newOpts) == 0 {
		return nil
	}
	merged, ovErr := pve.ApplyParkerDiskOverlay(ctx, deps.PVE, holder.Node, holder.VMID,
		rd.volid, rd.stableID, rd.diskCID, newOpts, parkerReadConfigFor(deps))
	if ovErr != nil {
		return retriableUnlessPermanent(ovErr,
			fmt.Sprintf("update_disk: record option overrides for parked disk %s (fail-closed: nothing was changed)", diskCID))
	}
	deps.Log(ctx).Info("update_disk: disk is parked; option overrides recorded and applied at the next attach",
		log.String("disk_cid", diskCID),
		log.Int("parker_vmid", holder.VMID),
		log.Int("override_keys", len(merged)),
	)
	return nil
}

// resizeDiskInternal performs the additive GiB resize for a disk already located
// at diskID on vmid. It mirrors the resize_disk logic without re-locating the VM.
func resizeDiskInternal(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	diskID string,
	diskCID string,
	newSizeMB int,
) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("update_disk: read VM %d config for resize", vmid))
	}

	diskOptStr, ok := cfg[diskID].(string)
	if !ok || diskOptStr == "" {
		return cpierrors.Cloud("update_disk: disk %q not in VM %d config during resize", diskID, vmid)
	}

	currentGiB, parseErr := parseDiskSizeGiB(diskOptStr)
	if parseErr != nil {
		return cpierrors.Wrap(parseErr, fmt.Sprintf("update_disk: cannot determine current size for %s", diskCID))
	}

	newGiB := (newSizeMB + 1023) / 1024
	deltaGiB := newGiB - currentGiB

	if deltaGiB < 0 {
		return cpierrors.NotSupported(
			"update_disk: resize",
			fmt.Sprintf("shrink not supported (current %d GiB, requested %d GiB via %d MiB)", currentGiB, newGiB, newSizeMB),
		)
	}

	if deltaGiB == 0 {
		return nil // no-op
	}

	// Wrap submit+await in RetryOnTransientOrLock: PVE holds a per-storage
	// lockfile during resize; concurrent resizes fail with "can't lock file
	// ... got timeout". Retry the full submit+await pair on that signal. The
	// retry recomputes the remaining delta from the live config so a
	// committed-then-dropped attempt is not replayed on top of itself.
	rerr := resizeDiskConverging(ctx, deps, deps.Log(ctx), "update_disk_resize", node, vmid, diskID, currentGiB, newGiB, 0)
	if rerr != nil {
		return cpierrors.Wrap(pve.WrapError(rerr), fmt.Sprintf("update_disk: ResizeDisk failed for %s (+%dG)", diskCID, deltaGiB))
	}
	return nil
}

// updateSpecToOptions converts the recognized option fields from update_spec into
// a map of PVE disk option key → value strings. Only non-zero/non-false values
// generate option entries; zero/false values remove the option from the merged set
// by explicitly mapping to an empty string sentinel (handled by buildDiskOptStr).
//
// Recognized option fields:
//
//	iothread bool   → "iothread" = "1" or "0"
//	cache    string → "cache" = <value> (empty → removes key)
//	discard  string → "discard" = <value> (empty → removes key)
//	ssd      bool   → "ssd" = "1" or "0"
//	backup   bool   → "backup" = "1" or "0"
//	mbps_rd  int    → "mbps_rd" = "<n>" (0 → removes key)
//	mbps_wr  int    → "mbps_wr" = "<n>" (0 → removes key)
//	iops_rd  int    → "iops_rd" = "<n>" (0 → removes key)
//	iops_wr  int    → "iops_wr" = "<n>" (0 → removes key)
func updateSpecToOptions(spec map[string]any) map[string]string {
	opts := make(map[string]string)

	setBool := func(key string, raw any) {
		if v, ok := raw.(bool); ok {
			if v {
				opts[key] = "1"
			} else {
				opts[key] = "0"
			}
		}
	}

	setInt := func(key string, raw any) {
		if n, ok := toInt(raw); ok {
			if n > 0 {
				opts[key] = strconv.Itoa(n)
			} else {
				opts[key] = "" // zero → remove
			}
		}
	}

	setString := func(key string, raw any) {
		if s, ok := raw.(string); ok {
			opts[key] = s // empty string → remove
		}
	}

	if v, ok := spec["iothread"]; ok {
		setBool("iothread", v)
	}
	if v, ok := spec["cache"]; ok {
		setString("cache", v)
	}
	if v, ok := spec["discard"]; ok {
		setString("discard", v)
	}
	if v, ok := spec["ssd"]; ok {
		setBool("ssd", v)
	}
	if v, ok := spec["backup"]; ok {
		setBool("backup", v)
	}
	if v, ok := spec["mbps_rd"]; ok {
		setInt("mbps_rd", v)
	}
	if v, ok := spec["mbps_wr"]; ok {
		setInt("mbps_wr", v)
	}
	if v, ok := spec["iops_rd"]; ok {
		setInt("iops_rd", v)
	}
	if v, ok := spec["iops_wr"]; ok {
		setInt("iops_wr", v)
	}

	return opts
}

// splitDiskOptStr splits a PVE disk option string into the bare volid and a
// map of option key → value pairs. Input format: "<volid>[,key=value,...]".
func splitDiskOptStr(optStr string) (bareVolid string, opts map[string]string) {
	opts = make(map[string]string)
	parts := strings.Split(optStr, ",")
	if len(parts) == 0 {
		return optStr, opts
	}
	bareVolid = parts[0]
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, "=")
		if ok && k != "" {
			opts[k] = v
		}
	}
	return bareVolid, opts
}

// mergeDiskOptions merges new option entries into existing, returning a combined map.
// Entries in newOpts with empty-string value are removed from the result (deletion semantics).
// Entries in newOpts with non-empty value override or add to existing.
func mergeDiskOptions(existing, newOpts map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(newOpts))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range newOpts {
		if v == "" {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	return merged
}

// buildDiskOptStr reconstructs a PVE disk option string from the bare volid and
// option map. The output format is "<volid>[,key=value,...]". The "size" key is
// always placed first among options (after the volid) for readability; all other
// keys are appended in deterministic alphabetical order.
func buildDiskOptStr(bareVolid string, opts map[string]string) string {
	if len(opts) == 0 {
		return bareVolid
	}

	// Collect and sort keys; "size" first.
	keys := make([]string, 0, len(opts))
	hasSz := false
	for k := range opts {
		if k == "size" {
			hasSz = true
		} else {
			keys = append(keys, k)
		}
	}
	// Sort remaining keys alphabetically for deterministic output.
	sortStrings(keys)

	var sb strings.Builder
	sb.WriteString(bareVolid)
	if hasSz {
		sb.WriteByte(',')
		sb.WriteString("size=")
		sb.WriteString(opts["size"])
	}
	for _, k := range keys {
		sb.WriteByte(',')
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(opts[k])
	}
	return sb.String()
}

// sortStrings is a simple insertion sort for short slices of option keys.
// The number of disk options is typically < 10; allocating a sort package import
// is avoided by this in-place sort.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// toInt coerces a json-decoded any value to int.
// JSON numbers decode to float64 by default when using interface{} targets.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
