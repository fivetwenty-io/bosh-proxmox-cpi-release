package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
//  1. Parse disk_cid and locate the attached VM + diskID.
//     Detached disk → CloudError (cannot update options on detached disk).
//  2. If update_spec has "size", route through resize logic (delta computation).
//  3. Read current disk option string from VM config.
//  4. Parse current options into key-value map; preserve unspecified options.
//  5. Merge new options from update_spec into the map.
//  6. Reconstruct disk option string: "<volid>[,key=value,...]".
//  7. PUT updated config via AttachDisk (with explicit DiskID to avoid slot re-allocation).
//  8. Return nil.
//
// Empty update_spec (all absent or zero-value fields): returns nil (no-op).
//
//nolint:gocognit // Orchestration shell: parse args, locate VM, resolve diskID, merge options, resize, PUT config. Sequential phases with per-step error handling; cognitive floor set by the step count.
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
		bareDiskCID, _, decErr := decodeDiskCID(ctx, deps, "update_disk", diskCID)
		if decErr != nil {
			return nil, decErr
		}

		// update_spec may be null or missing; treat as empty map.
		var updateSpec map[string]any
		if args[1] != nil {
			if err := json.Unmarshal(args[1], &updateSpec); err != nil {
				return nil, cpierrors.Wrap(err, "update_disk: args[1] update_spec must be an object")
			}
		}

		// ----------------------------------------------------------------
		// 2. Validate disk_cid format. The locator and diskID resolver match
		//    against the full canonical "<storage>:<volume>" form that PVE
		//    stores in the VM config (e.g. "data:vm-9559-disk-0,size=2G"), so
		//    pass bareDiskCID through — NOT the storage-stripped volume part.
		// ----------------------------------------------------------------
		if _, _, err := pve.ParseDiskCID(bareDiskCID); err != nil {
			return nil, cpierrors.DiskNotFound(diskCID)
		}

		// ----------------------------------------------------------------
		// 3. Locate attached VM + its node, then resolve diskID.
		//    Detached disk → CloudError. FindVMByDiskVolid uses a cluster
		//    scan, so update_disk works across nodes in a cluster deploy.
		// ----------------------------------------------------------------
		vmid, node, vmErr := pve.FindVMByDiskVolid(ctx, deps.PVE, deps.Config.Node, bareDiskCID)
		if vmErr != nil {
			// Distinguish "disk not attached to any VM" (expected detached-disk path)
			// from transport/cluster-scan errors (which must propagate so the caller
			// can detect transient failures and retry).
			//
			// FindVMByDiskVolid wraps pve.ErrDiskNotAttachedToAnyVM via fmt.Errorf when
			// the cluster scan completes with no match. Transport/cluster errors are
			// distinct wrapped errors without that sentinel. errors.Is traverses the
			// chain, so both direct and double-wrapped forms are caught.
			// pve.IsNotFound handles SDK 404 shapes for callers that surface a 404
			// instead of ErrDiskNotAttachedToAnyVM.
			if pve.IsNotFound(vmErr) || errors.Is(vmErr, pve.ErrDiskNotAttachedToAnyVM) {
				return nil, cpierrors.DetachedDisk(
					"update_disk: detached disk cannot be updated — disk %q not attached to any VM", diskCID,
				)
			}
			return nil, cpierrors.Wrap(pve.WrapError(vmErr), fmt.Sprintf("update_disk: locate VM for disk %q", diskCID))
		}

		diskID, err := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, bareDiskCID)
		if err != nil {
			return nil, cpierrors.Wrap(err, fmt.Sprintf("update_disk: cannot resolve diskID for %s on VM %d", diskCID, vmid))
		}

		// ----------------------------------------------------------------
		// 4. Handle "size" field via resize logic.
		// ----------------------------------------------------------------
		if sizeRaw, hasSz := updateSpec["size"]; hasSz {
			newSizeMB, ok := toInt(sizeRaw)
			if !ok || newSizeMB <= 0 {
				return nil, cpierrors.Cloud("update_disk: update_spec.size must be a positive integer (MiB), got %v", sizeRaw)
			}
			if err := resizeDiskInternal(ctx, deps, node, vmid, diskID, diskCID, newSizeMB); err != nil {
				return nil, err
			}
		}

		// ----------------------------------------------------------------
		// 5. Build option key-value map from remaining update_spec fields.
		//    Only recognized option fields are applied; unknown fields are
		//    silently ignored to maintain forward compatibility.
		// ----------------------------------------------------------------
		newOpts := updateSpecToOptions(updateSpec)

		// No option changes (size-only update or truly empty spec).
		if len(newOpts) == 0 {
			return nil, nil
		}

		// ----------------------------------------------------------------
		// 6. Read current disk option string and parse existing options.
		// ----------------------------------------------------------------
		cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return nil, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("update_disk: read VM %d config", vmid))
		}

		diskOptStr, ok := cfg[diskID].(string)
		if !ok || diskOptStr == "" {
			return nil, cpierrors.Cloud("update_disk: disk %q not found in VM %d config after locate", diskID, vmid)
		}

		// Split option string into the bare volid and existing option pairs.
		bareVolid, existingOpts := splitDiskOptStr(diskOptStr)

		// ----------------------------------------------------------------
		// 7. Merge existing options with new options (new takes precedence).
		// ----------------------------------------------------------------
		merged := mergeDiskOptions(existingOpts, newOpts)

		// ----------------------------------------------------------------
		// 8. Reconstruct disk option string and PUT to VM config.
		// ----------------------------------------------------------------
		newOptStr := buildDiskOptStr(bareVolid, merged)

		// Use AttachDisk with opts.DiskID set to the current slot so the SDK
		// issues PUT /nodes/{node}/qemu/{vmid}/config with {diskID: newOptStr}.
		_, err = deps.PVE.QEMU().AttachDisk(ctx, node, vmid, newOptStr, "scsi", &qemu.AttachOpts{
			DiskID: diskID,
		})
		if err != nil {
			return nil, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("update_disk: update disk options for %s on VM %d", diskCID, vmid))
		}

		deps.Log(ctx).Info("update_disk",
			log.String("disk_cid", diskCID),
			log.Int("vmid", vmid),
			log.String("disk_id", diskID),
			log.String("new_opt_str", newOptStr),
		)

		return nil, nil
	})
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
	// ... got timeout". Retry the full submit+await pair on that signal.
	rerr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "update_disk_resize", 0, func() error {
		upid, e := deps.PVE.QEMU().ResizeDisk(ctx, node, vmid, diskID, deltaGiB)
		if e != nil {
			return e
		}
		if upid == "" {
			return nil
		}
		return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx))
	})
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
