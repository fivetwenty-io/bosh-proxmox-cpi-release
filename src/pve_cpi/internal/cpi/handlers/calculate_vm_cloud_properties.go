package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// resourceTypeNode is the /cluster/resources item type identifying a PVE node.
const resourceTypeNode = "node"

// vmResources maps BOSH vm_resources hint fields.
// All values are integers: cpu in cores, ram in MiB, ephemeral_disk_size in MiB.
// Storage, when non-empty, overrides deps.Config.VMStorage for this call only.
type vmResources struct {
	CPU               int    `json:"cpu"`
	RAM               int    `json:"ram"`
	EphemeralDiskSize int    `json:"ephemeral_disk_size"`
	Storage           string `json:"storage,omitempty"`
}

// vmCloudProperties is the cloud_properties map returned to the BOSH Director.
// The Director passes this as cloud_properties in subsequent create_vm calls.
// EphemeralDiskSizeMB echoes the requested value from vm_resources (in MiB, matching
// the BOSH convention used throughout this codebase) under the key "ephemeral_disk_size_mb"
// so that create_vm's createVMCloudProps.EphemeralDiskSizeMB field receives it and
// resolveEphemeralShape creates the ephemeral disk. A zero value is omitted from JSON
// so pre-existing serialization is unchanged when the field is absent from the request.
type vmCloudProperties struct {
	Cores               int    `json:"cores"`
	Sockets             int    `json:"sockets"`
	Memory              int    `json:"memory"`
	VMDiskFormat        string `json:"vm_disk_format"`
	TargetNode          string `json:"target_node"`
	TargetStorage       string `json:"target_storage"`
	EphemeralDiskSizeMB int    `json:"ephemeral_disk_size_mb,omitempty"`
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
	Avail   int64  `json:"avail"`   // available bytes reported by PVE
	Total   int64  `json:"total"`   // total bytes reported by PVE
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

// cloudPropsCandidate holds a single node that passed the storage-first
// filter and CPU+RAM fit check, along with its free-byte count for ranking.
type cloudPropsCandidate struct {
	name      string
	freeBytes int64
}

// candidateNodesForCloudProps runs the storage-first node selection pipeline
// against the cluster status response and returns the set of nodes that pass
// all three filters (online, storage active+images-capable, CPU+RAM fit),
// ranked by descending free bytes.
//
// It also returns the list of nodes that would have qualified on CPU+RAM but
// failed the storage check (used in the NotSupported error message). Both
// slices may be nil when no nodes are present.
//
// Errors:
//   - cluster.ListStatus API failure → returned via pve.WrapError so 5xx is retriable.
//   - Per-node ListStorage errors → WARN, node excluded (fail-safe). Never fatal.
func candidateNodesForCloudProps(
	ctx context.Context,
	deps Deps,
	res vmResources,
	effectiveStorage string,
) (candidates []cloudPropsCandidate, cpuRAMFailedStorage []string, err error) {
	ramBytes := int64(res.RAM) * 1024 * 1024 // MiB → bytes

	statusResp, err := deps.PVE.Cluster().ListStatus(ctx)
	if err != nil {
		// Route through pve.WrapError so PVE 5xx and connection errors produce
		// a retriable error (TypeRetriableCloud) rather than a non-retriable
		// CloudError. This matches the retryability pattern used by all other
		// handlers (see internal/cpi/handlers/README.md rule #1).
		return nil, nil, cpierrors.Wrap(pve.WrapError(err), "calculate_vm_cloud_properties: cluster status fetch failed")
	}

	for i, raw := range *statusResp {
		var item clusterStatusNode
		if parseErr := json.Unmarshal(raw, &item); parseErr != nil {
			// Skip items whose schema is incompatible (e.g., quorum-info entries).
			deps.Log(ctx).Debug("calculate_vm_cloud_properties: skip item",
				log.Int("idx", i),
				log.Err(parseErr),
			)
			continue
		}
		if item.Type != resourceTypeNode {
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
			deps.Log(ctx).Warn(
				"calculate_vm_cloud_properties: ListStorage failed — excluding node",
				log.String("node", item.Name),
				log.String("storage", effectiveStorage),
				log.Err(storErr),
			)
			// Track for the NotSupported message if it would have fit CPU+RAM,
			// so an unreachable-storage node is reported like an inactive one.
			freeBytes := item.Maxmem - item.Mem
			if item.Maxcpu >= int64(res.CPU) && freeBytes >= ramBytes {
				cpuRAMFailedStorage = append(cpuRAMFailedStorage, item.Name)
			}
			continue // fail-safe: never pick node with unknown storage status
		}
		if !nodeHasStorage(storageResp, effectiveStorage) {
			deps.Log(ctx).Warn(
				fmt.Sprintf(
					"calculate_vm_cloud_properties: storage %q not active/images-capable on node %q — excluding from candidates",
					effectiveStorage, item.Name,
				),
			)
			// Still track it for the error message if it would have fit CPU+RAM.
			freeBytes := item.Maxmem - item.Mem
			if item.Maxcpu >= int64(res.CPU) && freeBytes >= ramBytes {
				cpuRAMFailedStorage = append(cpuRAMFailedStorage, item.Name)
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

		candidates = append(candidates, cloudPropsCandidate{
			name:      item.Name,
			freeBytes: freeBytes,
		})
	}

	return candidates, cpuRAMFailedStorage, nil
}

// pickBestNode delegates to the placement package to select the highest-scoring
// candidate by free memory. It preserves the original pickBestNode behavior
// (max free bytes wins) when called with mem-only weights.
//
// Returns an empty string when candidates is empty.
func pickBestNode(candidates []cloudPropsCandidate) string {
	if len(candidates) == 0 {
		return ""
	}

	// Build NodeFacts from cloudPropsCandidate values.
	// TotalMemBytes is estimated as freeBytes + 1 so the free-mem fraction
	// is still comparable across nodes. The relative order is identical to
	// the original max-freeBytes comparison because the weight and denominator
	// are constant across all candidates.
	// For absolute accuracy, set TotalMemBytes = Maxmem when refactoring to
	// GatherNodeFacts in create_vm (where Maxmem is available).
	facts := make([]placement.NodeFacts, len(candidates))
	for i, c := range candidates {
		facts[i] = placement.NodeFacts{
			Node:          c.name,
			Online:        true,
			FreeMemBytes:  c.freeBytes,
			TotalMemBytes: c.freeBytes + 1, // non-zero denominator
		}
	}

	w := placement.Weights{Mem: 1.0}
	scored := placement.Score(facts, w, nil)
	return placement.Pick(scored, nil)
}

// HandleCalculateVMCloudProperties maps BOSH resource hints to PVE
// cloud_properties by querying the PVE cluster for available node capacity.
//
// Input: args[0] = { "cpu": int, "ram": int_MiB, "ephemeral_disk_size": int_MiB,
//
//	"storage": string (optional, overrides config vm_storage) }
//
// Algorithm (storage-first):
//  1. Decode vm_resources from args[0].
//  2. Resolve effectiveStorage: res.Storage when non-empty, else Config.VMStorage.
//  3. Call candidateNodesForCloudProps to run the storage-first pipeline.
//  4. No node passes → NotSupported listing CPU/RAM-qualifying nodes that
//     failed the storage check.
//  5. Return cloud_properties: cores, sockets, memory, vm_disk_format,
//     target_node (winner), target_storage (effectiveStorage).
//
// Errors:
//   - args[0] missing or malformed → CloudError.
//   - cluster.ListStatus API error → retriable CloudError (5xx/conn) or non-retriable (4xx).
//   - No qualifying node → NotSupported.
func HandleCalculateVMCloudProperties(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
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

		// Resolve effective storage (per-request override when set).
		effectiveStorage := deps.Config.VMStorage
		if res.Storage != "" {
			effectiveStorage = res.Storage
			deps.Log(ctx).Debug(
				"calculate_vm_cloud_properties: using per-request storage override",
				log.String("storage", effectiveStorage),
				log.String("config_default", deps.Config.VMStorage),
			)
		}

		candidates, cpuRAMFailedStorageNodes, err := candidateNodesForCloudProps(ctx, deps, res, effectiveStorage)
		if err != nil {
			return nil, err
		}

		bestNode := pickBestNode(candidates)
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
			Cores:               res.CPU,
			Sockets:             1,
			Memory:              res.RAM,
			VMDiskFormat:        deps.Config.VMDiskFormat,
			TargetNode:          bestNode,
			TargetStorage:       effectiveStorage,
			EphemeralDiskSizeMB: res.EphemeralDiskSize,
		}

		return props, nil
	})
}
