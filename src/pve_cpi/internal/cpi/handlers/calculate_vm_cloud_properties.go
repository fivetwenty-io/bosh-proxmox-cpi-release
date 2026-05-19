package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// vmResources maps BOSH vm_resources hint fields.
// All values are integers: cpu in cores, ram in MiB, ephemeral_disk_size in MiB.
type vmResources struct {
	CPU               int `json:"cpu"`
	RAM               int `json:"ram"`
	EphemeralDiskSize int `json:"ephemeral_disk_size"`
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

// HandleCalculateVMCloudProperties maps BOSH resource hints to PVE
// cloud_properties by querying the PVE cluster for available node capacity.
//
// Input: args[0] = { "cpu": int, "ram": int_MiB, "ephemeral_disk_size": int_MiB }
//
// Algorithm:
//  1. Decode vm_resources from args[0].
//  2. Call cluster.ListStatus to retrieve all cluster members.
//  3. Filter for entries with type="node" that are online and meet the CPU+RAM
//     requirements: node.maxcpu >= requested_cpu AND free_mem >= requested_ram_bytes.
//  4. Among qualifying nodes, select the one with the most free memory (maxmem-mem).
//  5. Return cloud_properties with cores, sockets, memory, vm_disk_format, target_node,
//     target_storage derived from deps.Config.
//  6. No qualifying node → NotSupported "no node satisfies requirements".
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

		ramBytes := int64(res.RAM) * 1024 * 1024 // MiB → bytes

		// Fetch cluster node list.
		statusResp, err := deps.PVE.Cluster().ListStatus(ctx)
		if err != nil {
			return nil, cpierrors.Wrap(err, "calculate_vm_cloud_properties: cluster status fetch failed")
		}

		// Parse node entries and select best fit.
		bestNode := ""
		bestFreeBytes := int64(-1)

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

			freeBytes := item.Maxmem - item.Mem
			if item.Maxcpu < int64(res.CPU) {
				continue
			}
			if freeBytes < ramBytes {
				continue
			}

			if freeBytes > bestFreeBytes {
				bestFreeBytes = freeBytes
				bestNode = item.Name
			}
		}

		if bestNode == "" {
			return nil, cpierrors.NotSupported(
				"calculate_vm_cloud_properties",
				fmt.Sprintf("no node satisfies requirements: cpu=%d ram=%d MiB", res.CPU, res.RAM),
			)
		}

		props := vmCloudProperties{
			Cores:         res.CPU,
			Sockets:       1,
			Memory:        res.RAM,
			VMDiskFormat:  deps.Config.VMDiskFormat,
			TargetNode:    bestNode,
			TargetStorage: deps.Config.VMStorage,
		}

		return props, nil
	})
}
