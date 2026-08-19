// Attached-disk CID provenance: records the exact disk_cid string the BOSH
// Director passed to attach_disk, keyed by the bare PVE volid, on the
// workload VM's description sentinel (distinct top-level key from
// bosh_parked_disks so the two codecs coexist — see sentinel.go).
//
// Why this exists: get_disks is a cloudcheck membership test — the Director
// compares its stored disk_cid strings against get_disks's return value.
// Since disk CIDs may be opaque envelopes (see disk.go's EncodeDiskCID)
// embedding metadata that cannot be reconstructed from PVE state alone,
// get_disks cannot derive the Director's exact CID from the volid at read
// time. attach_disk therefore records the verbatim CID here so a later
// get_disks call can echo it back byte-identical.
package pve

import (
	"context"
	"encoding/json"
	"fmt"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// attachedDisksSentinelKey is the top-level sentinel JSON key holding the
// bare-volid -> director-supplied-disk_cid map on a workload VM's description.
const attachedDisksSentinelKey = "bosh_attached_disks"

// parseAttachedDisksSentinel extracts the nonBOSH prefix and the current
// bosh_attached_disks map (bare volid -> director disk_cid) from a workload
// VM's description. Corrupted JSON for our key -> fresh empty map (sentinel
// rebuilt from scratch on next write; nonBOSH text and unrelated keys are
// preserved either way).
func parseAttachedDisksSentinel(desc string) (nonBOSH string, disks map[string]string, raw map[string]json.RawMessage) {
	nonBOSH, raw = ParseSentinel(desc)
	disks = make(map[string]string)

	if rawDisks, ok := raw[attachedDisksSentinelKey]; ok {
		_ = json.Unmarshal(rawDisks, &disks) // best-effort; corruption → empty map
		delete(raw, attachedDisksSentinelKey)
	}
	return
}

// renderAttachedDisksSentinel is parseAttachedDisksSentinel's write-side
// counterpart: builds the full description string from the nonBOSH prefix,
// the updated bosh_attached_disks map, and the raw remainder of other codec
// keys (e.g. bosh_parked_disks on a VM that happens to carry both — not
// expected in practice since parked disks live on a parker VM's own
// description, but the merge is symmetric with renderParkerSentinel so no
// key is ever silently dropped).
func renderAttachedDisksSentinel(nonBOSH string, disks map[string]string, raw map[string]json.RawMessage) (string, error) {
	if len(disks) == 0 && len(raw) == 0 {
		return nonBOSH, nil
	}

	merged := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		merged[k] = v
	}
	if len(disks) > 0 {
		b, err := json.Marshal(disks)
		if err != nil {
			return "", err
		}
		merged[attachedDisksSentinelKey] = json.RawMessage(b)
	}

	return RenderSentinel(nonBOSH, merged)
}

// GetAttachedDiskCIDs parses a workload VM's description and returns its
// bosh_attached_disks map (bare volid -> director-supplied disk_cid).
// Absent sentinel, absent key, or corrupted JSON all yield an empty
// (non-nil) map rather than an error — get_disks falls back to bare volids
// for every disk on any decode failure, matching pre-feature behavior.
func GetAttachedDiskCIDs(desc string) map[string]string {
	_, disks, _ := parseAttachedDisksSentinel(desc)
	return disks
}

// UpdateAttachedDiskCID merges {bareVolid: cidVerbatim} into the workload
// VM's description sentinel and writes it back via UpdateQemuConfig.
//
// Best-effort: any failure is logged at WARN and the function returns
// without error. attach_disk's success is never gated on this write — the
// recorded CID is advisory metadata consumed only by get_disks to answer
// cloud-check queries with a Director-matching CID; losing an update here
// degrades get_disks to the legacy bare-volid fallback for that one disk,
// not a correctness failure for attach_disk itself.
//
// A blank cidVerbatim is a no-op (nothing meaningful to record — callers
// should not normally pass one, but this guards against a stray call).
func UpdateAttachedDiskCID(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, bareVolid, cidVerbatim string) {
	if cidVerbatim == "" {
		return
	}
	if c == nil || node == "" || vmid <= 0 || bareVolid == "" {
		return
	}
	vmidStr := fmt.Sprintf("%d", vmid)

	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: config fetch failed — CID not recorded",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
		return
	}

	nonBOSH, disks, rawOther := parseAttachedDisksSentinel(DescriptionFromConfig(vmCfg))
	disks[bareVolid] = cidVerbatim

	newDesc, marshalErr := renderAttachedDisksSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: marshal failed — CID not recorded",
				log.Int("vmid", vmid),
				log.String("volid", bareVolid),
				log.Err(marshalErr),
			)
		}
		return
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return
	}
	if updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	}); updateErr != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: UpdateQemuConfig failed — CID not recorded",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(updateErr),
			)
		}
	}
}

// RemoveAttachedDiskCID removes the bareVolid entry from the workload VM's
// description sentinel. When no entry exists, no API call is made.
//
// Best-effort: any failure is logged at WARN and the function returns
// without error — detach_disk's success is never gated on this write.
func RemoveAttachedDiskCID(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, bareVolid string) {
	if c == nil || node == "" || vmid <= 0 || bareVolid == "" {
		return
	}
	vmidStr := fmt.Sprintf("%d", vmid)

	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: config fetch failed on remove — CID not removed",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
		return
	}

	nonBOSH, disks, rawOther := parseAttachedDisksSentinel(DescriptionFromConfig(vmCfg))

	// Absent entry — nothing to remove; skip the API call.
	if _, exists := disks[bareVolid]; !exists {
		return
	}
	delete(disks, bareVolid)

	newDesc, marshalErr := renderAttachedDisksSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: marshal failed on remove — CID not removed",
				log.Int("vmid", vmid),
				log.String("volid", bareVolid),
				log.Err(marshalErr),
			)
		}
		return
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return
	}
	if updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	}); updateErr != nil {
		if logger != nil {
			logger.Warn("attached-disk CID: UpdateQemuConfig failed on remove — CID not removed",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(updateErr),
			)
		}
	}
}
