package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// boshSentinelPattern matches the BOSH disk-metadata sentinel comment block inside a
// VM description: <!--BOSH:{...}--> where {...} is the JSON payload.
var boshSentinelPattern = regexp.MustCompile(`<!--BOSH:(.*?)-->`)

// boshDescriptionPayload is the JSON structure stashed inside the sentinel comment.
type boshDescriptionPayload struct {
	BoshDiskMetadata map[string]map[string]any `json:"bosh_disk_metadata"`
}

// attachedVM records the node+vmid pair that hosts a disk.
type attachedVM struct {
	node string
	vmid int
}

// HandleSetDiskMetadata returns a Handler for the BOSH CPI "set_disk_metadata" method.
//
// BOSH passes metadata key-value pairs (e.g., instance_id, deployment, director) that
// describe the lifecycle context of a persistent disk. Because PVE storage volumes have
// no native metadata API, this handler stashes the metadata as JSON in the description
// field of the VM the disk is currently attached to. The sentinel format is:
//
//	<!--BOSH:{"bosh_disk_metadata":{"<disk_cid>":{...metadata...}}}-->
//
// Any non-BOSH content in the description is preserved before the sentinel block.
//
// Arguments:
//   - args[0]: disk_cid (string) — "<storage>:<volume>", parsed via pve.ParseDiskCID.
//   - args[1]: metadata (map[string]any) — arbitrary key-value pairs from the Director.
//
// Logic:
//  1. Validate and parse both arguments.
//  2. Scan all cluster nodes for VMs; for each VM call qemu.Config and check whether
//     any disk slot's volid contains disk_cid. Collect (node, vmid) matches.
//  3. If 0 matches: log WARN "disk not attached, metadata not persisted"; return nil.
//  4. If 2+ matches: return CloudError "ambiguous disk attachment".
//  5. If exactly 1 match: read current VM description, parse/merge the sentinel block,
//     call nodes.UpdateQemuConfig with the updated description.
//
// Errors:
//   - Missing or invalid args → CloudError.
//   - SDK API errors → CloudError wrapping the SDK error.
//   - Ambiguous attachment → CloudError.
func HandleSetDiskMetadata(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// --- decode args ---
		if len(args) < 2 {
			return nil, cpierrors.Cloud(
				"set_disk_metadata: requires 2 arguments (disk_cid, metadata), got %d", len(args),
			)
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil || diskCID == "" {
			return nil, cpierrors.Cloud("set_disk_metadata: disk_cid must be a non-empty string")
		}

		var metadata map[string]any
		if err := json.Unmarshal(args[1], &metadata); err != nil {
			return nil, cpierrors.Cloud("set_disk_metadata: metadata must be a JSON object: %s", err.Error())
		}

		// Validate disk_cid structure (storage:volume).
		if _, _, err := pve.ParseDiskCID(diskCID); err != nil {
			return nil, err
		}

		// --- scan VMs for disk ---
		matches, err := findVMsHostingDisk(ctx, deps, diskCID)
		if err != nil {
			return nil, err
		}

		switch len(matches) {
		case 0:
			deps.Logger.Warn("set_disk_metadata: disk not attached; metadata not persisted",
				log.String("disk_cid", diskCID))
			return nil, nil

		case 1:
			return nil, persistMetadata(ctx, deps, matches[0], diskCID, metadata)

		default:
			return nil, cpierrors.Cloud(
				"set_disk_metadata: ambiguous disk attachment — disk %q found on %d VMs",
				diskCID, len(matches),
			)
		}
	})
}

// findVMsHostingDisk iterates all nodes and their VMs, returning every (node, vmid)
// pair whose QEMU config contains a disk slot whose volid contains diskCID.
func findVMsHostingDisk(ctx context.Context, deps Deps, diskCID string) ([]attachedVM, error) {
	// List nodes via cluster status to discover node names.
	statusResp, err := deps.PVE.Cluster().ListStatus(ctx)
	if err != nil {
		return nil, cpierrors.Wrap(err, "set_disk_metadata: cluster status fetch failed")
	}

	var matches []attachedVM

	for _, raw := range *statusResp {
		var item struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Type != "node" || item.Name == "" {
			continue
		}
		nodeName := item.Name

		// List VMs on this node.
		vmList, listErr := deps.PVE.Nodes().ListQemu(ctx, nodeName, nil)
		if listErr != nil {
			// Non-fatal: node may be temporarily unavailable.
			deps.Logger.Warn("set_disk_metadata: cannot list VMs on node",
				log.String("node", nodeName), log.Err(listErr))
			continue
		}
		if vmList == nil {
			continue
		}

		for _, vmRaw := range *vmList {
			var vmEntry struct {
				Vmid int64 `json:"vmid"`
			}
			if err := json.Unmarshal(vmRaw, &vmEntry); err != nil || vmEntry.Vmid <= 0 {
				continue
			}
			vmid := int(vmEntry.Vmid)

			cfg, cfgErr := deps.PVE.QEMU().Config(ctx, nodeName, vmid)
			if cfgErr != nil {
				deps.Logger.Warn("set_disk_metadata: cannot fetch VM config",
					log.String("node", nodeName), log.Int("vmid", vmid), log.Err(cfgErr))
				continue
			}

			// Check whether any disk slot contains the disk CID as a substring of
			// the volid. qemu.ParseDisks returns diskID -> volid map.
			disks := qemu.ParseDisks(cfg)
			for _, volid := range disks {
				if strings.Contains(volid, diskCID) {
					matches = append(matches, attachedVM{node: nodeName, vmid: vmid})
					break
				}
			}
		}
	}

	return matches, nil
}

// persistMetadata reads the current VM description, merges the disk metadata into
// the BOSH sentinel block, and writes the updated description back via UpdateQemuConfig.
func persistMetadata(ctx context.Context, deps Deps, vm attachedVM, diskCID string, metadata map[string]any) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, vm.node, vm.vmid)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("set_disk_metadata: fetch VM config (node=%s vmid=%d)", vm.node, vm.vmid))
	}

	// Extract current description.
	currentDesc := ""
	if v, ok := cfg["description"]; ok {
		if s, ok := v.(string); ok {
			currentDesc = s
		}
	}

	// Parse existing sentinel block.
	var payload boshDescriptionPayload
	payload.BoshDiskMetadata = make(map[string]map[string]any)

	nonBoshDesc := currentDesc
	if m := boshSentinelPattern.FindStringSubmatchIndex(currentDesc); m != nil {
		jsonStr := currentDesc[m[2]:m[3]]
		if parseErr := json.Unmarshal([]byte(jsonStr), &payload); parseErr != nil {
			// Corrupted sentinel — start fresh but preserve non-bosh text.
			payload.BoshDiskMetadata = make(map[string]map[string]any)
		}
		// Non-bosh content is everything before the sentinel.
		nonBoshDesc = strings.TrimSpace(currentDesc[:m[0]])
	}

	// Merge metadata for this disk CID.
	payload.BoshDiskMetadata[diskCID] = metadata

	// Serialise updated sentinel.
	sentinelJSON, err := json.Marshal(payload)
	if err != nil {
		return cpierrors.Cloud("set_disk_metadata: marshal metadata payload: %s", err.Error())
	}

	newDesc := fmt.Sprintf("<!--BOSH:%s-->", string(sentinelJSON))
	if nonBoshDesc != "" {
		newDesc = nonBoshDesc + "\n" + newDesc
	}

	// Write back via nodes.UpdateQemuConfig (description field).
	vmidStr := fmt.Sprintf("%d", vm.vmid)
	updateParams := &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	}
	if updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, vm.node, vmidStr, updateParams); updateErr != nil {
		return cpierrors.Wrap(updateErr,
			fmt.Sprintf("set_disk_metadata: UpdateQemuConfig (node=%s vmid=%d)", vm.node, vm.vmid),
		)
	}

	return nil
}
