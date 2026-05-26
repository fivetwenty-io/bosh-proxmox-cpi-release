package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// vmResources maps BOSH vm_resources hint fields.
// All values are integers: cpu in cores, ram in MiB, ephemeral_disk_size in MiB.
// Storage, when non-empty, overrides deps.Config.VMStorage for this call only (D6-B).
type vmResources struct {
	CPU               int    `json:"cpu"`
	RAM               int    `json:"ram"`
	EphemeralDiskSize int    `json:"ephemeral_disk_size"`
	Storage           string `json:"storage,omitempty"`
}

// vmCloudProperties is the cloud_properties map returned to the BOSH Director.
// The Director passes this as cloud_properties in subsequent create_vm calls.
type vmCloudProperties struct {
	Cores         int    `json:"cores"`
	Sockets       int    `json:"sockets"`
	Memory        int    `json:"memory"`
	VMDiskFormat  string `json:"vm_disk_format"`
	TargetNode    string `json:"target_node"`
	TargetStorage string `json:"target_storage"`
}

// clusterStatusNode is the decoded shape of each node entry returned by
// GET /cluster/status. Only fields used for node selection are captured;
// additional PVE fields are silently ignored. The "online" field is an
// integer in the PVE API (1 = online, 0 = offline).
type clusterStatusNode struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Online int64  `json:"online"` // 1 = online, 0 = offline
	Maxcpu int64  `json:"maxcpu"`
	Maxmem int64  `json:"maxmem"`
	Mem    int64  `json:"mem"` // current used memory in bytes
}

// storageStatusEntry is the decoded shape of each entry returned by
// GET /nodes/{node}/storage. Only fields needed for storage-first filtering
// are captured; the rest are silently ignored.
type storageStatusEntry struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Active  int    `json:"active"`  // 1 = active, 0 = inactive
	Enabled int    `json:"enabled"` // 1 = enabled, 0 = disabled
	Content string `json:"content"` // comma-separated content types, e.g. "images,rootdir"
}

// nodeHasStorage returns true when the per-node storage list returned by
// ListStorage includes an entry for storageName that is active (active==1)
// and declares the "images" content type.
//
// It does NOT return an error; the caller is responsible for handling ListStorage
// errors before calling this function. The response must be non-nil.
func nodeHasStorage(resp *nodes.ListStorageResponse, storageName string) bool {
	if resp == nil {
		return false // defense-in-depth: treat a nil response as no storage
	}
	for _, raw := range *resp {
		var entry storageStatusEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue // skip unparseable entries; conservative
		}
		if entry.Storage != storageName {
			continue
		}
		if entry.Active != 1 {
			return false
		}
		for _, ct := range strings.Split(entry.Content, ",") {
			if strings.TrimSpace(ct) == "images" {
				return true
			}
		}
		return false // storage found but does not support images
	}
	return false // storage not listed on this node
}

// HandleCalculateVMCloudProperties maps BOSH resource hints to PVE
// cloud_properties by querying the PVE cluster for available node capacity.
//
// Input: args[0] = { "cpu": int, "ram": int_MiB, "ephemeral_disk_size": int_MiB,
//
//	"storage": string (optional, overrides config vm_storage) }
//
// Algorithm (storage-first, D5-C):
//  1. Decode vm_resources from args[0].
//  2. Resolve effectiveStorage: res.Storage when non-empty, else Config.VMStorage.
//  3. Call cluster.ListStatus to retrieve all cluster members.
//  4. For each online node, call Nodes().ListStorage to check whether
//     effectiveStorage is active and images-capable on that node.
//     - ListStorage error → WARN, exclude node (fail-safe).
//     - Storage not active or not images-capable → WARN, exclude node.
//  5. Among storage-OK nodes, apply CPU fit (maxcpu >= cpu) and RAM fit
//     (maxmem-mem >= ram_bytes); rank by max free bytes.
//  6. No node passes → NotSupported listing CPU/RAM-qualifying nodes that
//     failed the storage check.
//  7. Return cloud_properties: cores, sockets, memory, vm_disk_format,
//     target_node (winner), target_storage (effectiveStorage).
//
// Errors:
//   - args[0] missing or malformed → CloudError.
//   - cluster.ListStatus API error → CloudError wrapping SDK error.
//   - No qualifying node → NotSupported.
func HandleCalculateVMCloudProperties(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// Decode input.
		if len(args) == 0 {
			return nil, cpierrors.Cloud("calculate_vm_cloud_properties: missing required argument vm_resources")
		}
		var res vmResources
		if err := json.Unmarshal(args[0], &res); err != nil {
			return nil, cpierrors.Cloud("calculate_vm_cloud_properties: invalid vm_resources argument: %s", err.Error())
		}
		if res.CPU <= 0 {
			return nil, cpierrors.Cloud("calculate_vm_cloud_properties: cpu must be > 0, got %d", res.CPU)
		}
		if res.RAM <= 0 {
			return nil, cpierrors.Cloud("calculate_vm_cloud_properties: ram must be > 0, got %d", res.RAM)
		}

		// Resolve effective storage (D6-B per-request override).
		effectiveStorage := deps.Config.VMStorage
		if res.Storage != "" {
			effectiveStorage = res.Storage
			deps.Logger.Debug(
				"calculate_vm_cloud_properties: using per-request storage override",
				log.String("storage", effectiveStorage),
				log.String("config_default", deps.Config.VMStorage),
			)
		}

		ramBytes := int64(res.RAM) * 1024 * 1024 // MiB → bytes

		// Fetch cluster node list.
		statusResp, err := deps.PVE.Cluster().ListStatus(ctx)
		if err != nil {
			return nil, cpierrors.Wrap(err, "calculate_vm_cloud_properties: cluster status fetch failed")
		}

		// Storage-first pipeline (D5-C):
		// Phase 1 — collect online nodes from cluster status.
		// Phase 2 — per-node storage check via ListStorage.
		// Phase 3 — CPU+RAM fit among storage-OK nodes, rank by free bytes.

		type candidateNode struct {
			name      string
			freeBytes int64
		}

		// storageOKNodes: nodes that passed the storage check.
		// cpuRAMNodes: nodes that pass CPU+RAM but failed storage (for error message).
		var storageOKNodes []candidateNode
		var cpuRAMFailedStorageNodes []string

		for i, raw := range *statusResp {
			var item clusterStatusNode
			if parseErr := json.Unmarshal(raw, &item); parseErr != nil {
				// Skip items whose schema is incompatible (e.g., quorum-info entries).
				deps.Logger.Debug("calculate_vm_cloud_properties: skip item",
					log.Int("idx", i),
					log.Err(parseErr),
				)
				continue
			}
			if item.Type != "node" {
				continue
			}
			if item.Online == 0 {
				continue // node offline
			}

			// Phase 2: storage check — first-class filter step.
			storageResp, storErr := deps.PVE.Nodes().ListStorage(ctx, item.Name, &nodes.ListStorageParams{
				Storage: &effectiveStorage,
			})
			if storErr != nil {
				deps.Logger.Warn(
					"calculate_vm_cloud_properties: ListStorage failed — excluding node",
					log.String("node", item.Name),
					log.String("storage", effectiveStorage),
					log.Err(storErr),
				)
				// Track for the NotSupported message if it would have fit CPU+RAM,
				// so an unreachable-storage node is reported like an inactive one.
				freeBytes := item.Maxmem - item.Mem
				if item.Maxcpu >= int64(res.CPU) && freeBytes >= ramBytes {
					cpuRAMFailedStorageNodes = append(cpuRAMFailedStorageNodes, item.Name)
				}
				continue // fail-safe: never pick node with unknown storage status
			}
			if !nodeHasStorage(storageResp, effectiveStorage) {
				deps.Logger.Warn(
					fmt.Sprintf(
						"calculate_vm_cloud_properties: storage %q not active/images-capable on node %q — excluding from candidates",
						effectiveStorage, item.Name,
					),
				)
				// Still track it for the error message if it would have fit CPU+RAM.
				freeBytes := item.Maxmem - item.Mem
				if item.Maxcpu >= int64(res.CPU) && freeBytes >= ramBytes {
					cpuRAMFailedStorageNodes = append(cpuRAMFailedStorageNodes, item.Name)
				}
				continue
			}

			// Phase 3: CPU + RAM fit among storage-OK nodes.
			freeBytes := item.Maxmem - item.Mem
			if item.Maxcpu < int64(res.CPU) {
				continue
			}
			if freeBytes < ramBytes {
				continue
			}

			storageOKNodes = append(storageOKNodes, candidateNode{
				name:      item.Name,
				freeBytes: freeBytes,
			})
		}

		// Pick winner: max free bytes among storage-OK, CPU+RAM-qualifying nodes.
		bestNode := ""
		bestFreeBytes := int64(-1)
		for _, c := range storageOKNodes {
			if c.freeBytes > bestFreeBytes {
				bestFreeBytes = c.freeBytes
				bestNode = c.name
			}
		}

		if bestNode == "" {
			msg := fmt.Sprintf(
				"no node satisfies requirements: cpu=%d ram=%d MiB with storage %q active and images-capable."+
					" RAM/CPU-qualifying nodes that failed storage check: [%s]."+
					" Remediation: ensure storage %q is enabled with content type \"images\" on at least one node,"+
					" or set pve.vm_storage / per-request storage to a storage available cluster-wide.",
				res.CPU, res.RAM, effectiveStorage,
				strings.Join(cpuRAMFailedStorageNodes, ", "),
				effectiveStorage,
			)
			return nil, cpierrors.NotSupported("calculate_vm_cloud_properties", msg)
		}

		props := vmCloudProperties{
			Cores:         res.CPU,
			Sockets:       1,
			Memory:        res.RAM,
			VMDiskFormat:  deps.Config.VMDiskFormat,
			TargetNode:    bestNode,
			TargetStorage: effectiveStorage,
		}

		return props, nil
	})
}
