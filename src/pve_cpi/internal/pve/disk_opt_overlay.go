// Durable drive-option overrides: operator updates made through update_disk
// are recorded per disk and merged into the drive string at every attach as
// the rightmost layer (global config < CID-recorded opts < overrides, with an
// empty-string value deleting the key). Without a record, a detach/attach
// cycle rebuilds the drive string from config and CID options alone and every
// operator update silently reverts.
//
// The record lives wherever the disk lives. While attached, it is the
// bosh_disk_opt_overlays sentinel key on the holder VM's description — a
// distinct top-level key beside bosh_attached_disks and bosh_parked_disks,
// sharing the codec in sentinel.go. While parked, it is the opts field of the
// parker's bosh_parked_disks provenance entry, so it rides the same record a
// transfer already carries. Entries are keyed the same way as
// bosh_attached_disks: stable ID for disks that have one, bare volid for
// legacy disks; readers accept both generations.
//
// The serial key never enters an overlay: it is the disk's identity
// (disk_stable_id.go), owned by the attach boundaries, and every write and
// read here strips it.
package pve

import (
	"context"
	"encoding/json"
	"fmt"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// diskOptOverlaysSentinelKey is the top-level sentinel JSON key holding the
// per-disk drive-option override maps on a workload VM's description.
const diskOptOverlaysSentinelKey = "bosh_disk_opt_overlays"

// parseDiskOptOverlaysSentinel extracts the nonBOSH prefix and the current
// bosh_disk_opt_overlays map from a VM description. Corrupted JSON for our
// key -> fresh empty map (rebuilt on next write; nonBOSH text and unrelated
// keys are preserved either way).
func parseDiskOptOverlaysSentinel(desc string) (nonBOSH string, overlays map[string]map[string]string, raw map[string]json.RawMessage) {
	nonBOSH, raw = ParseSentinel(desc)
	overlays = make(map[string]map[string]string)

	if rawOverlays, ok := raw[diskOptOverlaysSentinelKey]; ok {
		_ = json.Unmarshal(rawOverlays, &overlays) // best-effort; corruption → empty map
		delete(raw, diskOptOverlaysSentinelKey)
	}
	return
}

// renderDiskOptOverlaysSentinel is the write-side counterpart: rebuilds the
// description from the nonBOSH prefix, the updated overlay map, and the raw
// remainder of other codec keys.
func renderDiskOptOverlaysSentinel(nonBOSH string, overlays map[string]map[string]string, raw map[string]json.RawMessage) (string, error) {
	if len(overlays) == 0 && len(raw) == 0 {
		return nonBOSH, nil
	}

	merged := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		merged[k] = v
	}
	if len(overlays) > 0 {
		b, err := json.Marshal(overlays)
		if err != nil {
			return "", err
		}
		merged[diskOptOverlaysSentinelKey] = json.RawMessage(b)
	}

	return RenderSentinel(nonBOSH, merged)
}

// sanitizeDiskOptOverlay returns a copy of m with the serial key removed.
// The serial is the disk's identity and must never be settable through an
// option override — a tampered or corrupted record would otherwise rewrite
// it at the next attach. Returns nil when nothing remains.
func sanitizeDiskOptOverlay(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k == "serial" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DiskOptOverlayFromDesc returns the drive-option override map recorded for a
// disk on a VM description, looked up under the given keys in order (callers
// pass the stable ID first, volids as legacy fallbacks). Absent sentinel,
// absent entry, or corrupted JSON all yield nil.
func DiskOptOverlayFromDesc(desc string, keys ...string) map[string]string {
	_, overlays, _ := parseDiskOptOverlaysSentinel(desc)
	for _, k := range keys {
		if k == "" {
			continue
		}
		if entry, ok := overlays[k]; ok {
			return sanitizeDiskOptOverlay(entry)
		}
	}
	return nil
}

// GetVMDiskOptOverlay reads a VM's config and returns the override map
// recorded for the disk identified by keys. A read failure is an error, not
// an empty answer: the callers copy the result onto a park or transfer, and
// concluding "no overrides" from a read that never arrived is how an
// operator's update silently reverts.
func GetVMDiskOptOverlay(ctx context.Context, c Client, node string, vmid int, keys ...string) (map[string]string, error) {
	if c == nil || node == "" || vmid <= 0 {
		return nil, cpierrors.Cloud("GetVMDiskOptOverlay: client, node, and vmid are all required")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return nil, cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("disk option overrides: config fetch for vmid %d", vmid))
	}
	return DiskOptOverlayFromDesc(DescriptionFromConfig(vmCfg), keys...), nil
}

// SetVMDiskOptOverlay records an override map for a disk on a VM's
// description under key, removing any stale entries under extraKeys (the
// other keying generation) in the same write. An empty overlay removes the
// entry. Fail-closed: the record carries correctness weight, so a failure is
// returned rather than logged — unlike the neighboring best-effort CID
// writers.
//
// Read-modify-write on the description, like every sentinel writer here; two
// concurrent writers on one VM can lose one whole update (the same accepted
// lost-update race the neighboring writers document).
func SetVMDiskOptOverlay(ctx context.Context, c Client, node string, vmid int, key string, overlay map[string]string, extraKeys ...string) error {
	if c == nil || node == "" || vmid <= 0 || key == "" {
		return cpierrors.Cloud("SetVMDiskOptOverlay: client, node, vmid, and key are all required")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("disk option overrides: config fetch for vmid %d", vmid))
	}

	nonBOSH, overlays, rawOther := parseDiskOptOverlaysSentinel(DescriptionFromConfig(vmCfg))
	for _, k := range extraKeys {
		if k != "" && k != key {
			delete(overlays, k)
		}
	}
	if clean := sanitizeDiskOptOverlay(overlay); clean != nil {
		overlays[key] = clean
	} else {
		delete(overlays, key)
	}

	newDesc, marshalErr := renderDiskOptOverlaysSentinel(nonBOSH, overlays, rawOther)
	if marshalErr != nil {
		return cpierrors.Wrap(marshalErr, "disk option overrides: marshal")
	}
	return writeVMDescription(ctx, c, node, vmid, newDesc)
}

// ApplyVMDiskOptOverlay merges update entries into the override map recorded
// for a disk on a VM's description and writes the result back, in one
// read-modify-write. Empty-string update values are kept: they are deletion
// markers for the final merged drive string, not entries to drop. Entries
// found under altKeys (the other keying generation) seed the merge and are
// consolidated under key. Returns the merged map. Fail-closed like
// SetVMDiskOptOverlay, with the same accepted concurrent-writer race.
func ApplyVMDiskOptOverlay(ctx context.Context, c Client, node string, vmid int, key string, altKeys []string, updates map[string]string) (map[string]string, error) {
	if c == nil || node == "" || vmid <= 0 || key == "" {
		return nil, cpierrors.Cloud("ApplyVMDiskOptOverlay: client, node, vmid, and key are all required")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return nil, cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("disk option overrides: config fetch for vmid %d", vmid))
	}

	nonBOSH, overlays, rawOther := parseDiskOptOverlaysSentinel(DescriptionFromConfig(vmCfg))
	existing := overlays[key]
	for _, k := range altKeys {
		if k == "" || k == key {
			continue
		}
		if existing == nil {
			existing = overlays[k]
		}
		delete(overlays, k)
	}
	merged := mergeOverlayUpdates(existing, updates)
	if merged != nil {
		overlays[key] = merged
	} else {
		delete(overlays, key)
	}

	newDesc, marshalErr := renderDiskOptOverlaysSentinel(nonBOSH, overlays, rawOther)
	if marshalErr != nil {
		return nil, cpierrors.Wrap(marshalErr, "disk option overrides: marshal")
	}
	if writeErr := writeVMDescription(ctx, c, node, vmid, newDesc); writeErr != nil {
		return nil, writeErr
	}
	return merged, nil
}

// mergeOverlayUpdates layers updates over an existing override map, keeping
// empty-string values as deletion markers, and strips the serial key.
func mergeOverlayUpdates(existing, updates map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(updates))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}
	delete(merged, "serial")
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// RemoveVMDiskOptOverlay removes the override entries stored under any of the
// given keys from a VM's description. Best-effort, mirroring
// RemoveAttachedDiskCID: by the time this runs the overrides have already
// been copied to their next carrier (or deliberately dropped), so a failure
// costs a stale entry on a VM the disk has left, not correctness.
func RemoveVMDiskOptOverlay(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, keys ...string) {
	live := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			live = append(live, k)
		}
	}
	if c == nil || node == "" || vmid <= 0 || len(live) == 0 {
		return
	}

	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		warnOverlayNotRemoved(logger, node, vmid, live[0], "config fetch failed", err)
		return
	}

	nonBOSH, overlays, rawOther := parseDiskOptOverlaysSentinel(DescriptionFromConfig(vmCfg))
	removed := false
	for _, k := range live {
		if _, exists := overlays[k]; exists {
			delete(overlays, k)
			removed = true
		}
	}
	if !removed {
		return
	}

	newDesc, marshalErr := renderDiskOptOverlaysSentinel(nonBOSH, overlays, rawOther)
	if marshalErr != nil {
		warnOverlayNotRemoved(logger, node, vmid, live[0], "marshal failed", marshalErr)
		return
	}
	if writeErr := writeVMDescription(ctx, c, node, vmid, newDesc); writeErr != nil {
		warnOverlayNotRemoved(logger, node, vmid, live[0], "UpdateQemuConfig failed", writeErr)
	}
}

// warnOverlayNotRemoved is RemoveVMDiskOptOverlay's failure log line.
func warnOverlayNotRemoved(logger *log.Logger, node string, vmid int, key, stage string, err error) {
	if logger == nil {
		return
	}
	logger.Warn("disk option overrides: "+stage+" on remove — stale override entry left behind",
		log.Int("vmid", vmid),
		log.String("node", node),
		log.String("key", key),
		log.Err(err),
	)
}

// writeVMDescription writes a description via the nodes service. A nil nodes
// service (test stubs without injection) is skipped silently, matching every
// other sentinel writer in this package.
func writeVMDescription(ctx context.Context, c Client, node string, vmid int, desc string) error {
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return nil
	}
	if err := nodesSvc.UpdateQemuConfig(ctx, node, fmt.Sprintf("%d", vmid), &sdknodes.UpdateQemuConfigParams{
		Description: &desc,
	}); err != nil {
		return cpierrors.Wrap(WrapMutationError(err),
			fmt.Sprintf("disk option overrides: UpdateQemuConfig for vmid %d", vmid))
	}
	return nil
}

// matchParkerOverlayEntry finds the provenance entry naming a parked disk
// under either keying generation: the stable ID, the bare volid, or an entry
// whose recorded Volid names it. Returns the entry's key and whether one was
// found.
func matchParkerOverlayEntry(disks map[string]parkerProvEntry, bareVolid, stableID string) (string, bool) {
	if stableID != "" {
		if _, ok := disks[stableID]; ok {
			return stableID, true
		}
	}
	if _, ok := disks[bareVolid]; ok {
		return bareVolid, true
	}
	for key, entry := range disks {
		if entry.Volid == bareVolid {
			return key, true
		}
	}
	return "", false
}

// ReadParkerDiskOverlay returns the drive-option override map recorded in a
// parker's provenance entry for the disk currently held as bareVolid. A
// parker whose config is gone yields nil (nothing left to read there; the
// anchor semantics belong to the holder scan). Any other read failure is an
// error, for the same fail-closed reason as GetVMDiskOptOverlay.
func ReadParkerDiskOverlay(ctx context.Context, c Client, node string, parkerVMID int, bareVolid, stableID string) (map[string]string, error) {
	if c == nil || node == "" || parkerVMID <= 0 || bareVolid == "" {
		return nil, cpierrors.Cloud("ReadParkerDiskOverlay: client, node, parker VMID, and volid are all required")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if parkerConfigGone(err) {
			return nil, nil
		}
		return nil, cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("disk option overrides: config fetch for parker vmid %d", parkerVMID))
	}
	_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	key, found := matchParkerOverlayEntry(disks, bareVolid, stableID)
	if !found {
		return nil, nil
	}
	return sanitizeDiskOptOverlay(disks[key].Opts), nil
}

// ApplyParkerDiskOverlay merges update entries into the override map carried
// by a parker's provenance entry for a parked disk, creating the entry when
// the (best-effort) park never recorded one. Empty-string update values are
// kept as deletion markers, and the serial key is stripped. Returns the
// merged map. Fail-closed read-modify-write on the parker description, with
// the same accepted concurrent-writer race every provenance write here has.
func ApplyParkerDiskOverlay(ctx context.Context, c Client, node string, parkerVMID int, bareVolid, stableID, diskCID string, updates map[string]string, cfg ParkerConfig) (map[string]string, error) {
	if c == nil || node == "" || parkerVMID <= 0 || bareVolid == "" {
		return nil, cpierrors.Cloud("ApplyParkerDiskOverlay: client, node, parker VMID, and volid are all required")
	}
	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		return nil, cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("disk option overrides: config fetch for parker vmid %d", parkerVMID))
	}

	nonBOSH, disks, rawOther := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	key, found := matchParkerOverlayEntry(disks, bareVolid, stableID)
	var entry parkerProvEntry
	if found {
		entry = disks[key]
	} else {
		// The park's provenance write is best-effort and may have been lost;
		// the override cannot be, so a fresh entry carries it. The slot comes
		// from the live config, the disk's identity from the caller.
		slot, _ := FindDiskIDByVolID(qemu.ParseDisks(vmCfg), bareVolid)
		key = parkerProvKey(bareVolid, stableID)
		entry = buildParkerProvEntry(node, bareVolid, slot, cfg, ParkContext{DiskCID: diskCID, StableID: stableID})
	}
	merged := mergeOverlayUpdates(entry.Opts, updates)
	entry.Opts = merged
	disks[key] = entry

	newDesc, marshalErr := renderParkerSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		return nil, cpierrors.Wrap(marshalErr, "disk option overrides: marshal parker sentinel")
	}
	if writeErr := writeVMDescription(ctx, c, node, parkerVMID, newDesc); writeErr != nil {
		return nil, writeErr
	}
	return merged, nil
}
