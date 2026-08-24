package placement

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// ClusterClient is the subset of the PVE cluster API used by GatherNodeFacts.
// It is satisfied by the full pve.Client.Cluster() return value.
type ClusterClient interface {
	ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error)
	ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
	ListHaStatusCurrent(ctx context.Context) (*cluster.ListHaStatusCurrentResponse, error)
}

// NodesClient is the subset of the PVE nodes API used by GatherNodeFacts.
// It is satisfied by the full pve.Client.Nodes() return value.
type NodesClient interface {
	ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
}

// clusterStatusItem is the typed shape of each entry in GET /cluster/status.
// The "online" field is an integer in the PVE API (1 = online, 0 = offline).
type clusterStatusItem struct {
	Type   string  `json:"type"`
	Name   string  `json:"name"`
	Online int64   `json:"online"`
	Maxcpu int64   `json:"maxcpu"`
	Maxmem int64   `json:"maxmem"`
	Mem    int64   `json:"mem"`
	CPU    float64 `json:"cpu"` // current utilisation fraction [0,1]
}

// NodeResource is the typed shape for each entry in GET /cluster/resources
// that the placement scorer cares about. Unknown fields are silently ignored.
// Exported so callers with an existing []json.RawMessage can decode without
// re-issuing the API call.
type NodeResource struct {
	Type   string `json:"type"`
	Node   string `json:"node"`
	Name   string `json:"name"`   // VM name; used for group tag matching
	Tags   string `json:"tags"`   // space-separated PVE tags
	Maxmem int64  `json:"maxmem"` // configured (reserved) memory in bytes; 0 when absent
}

// clusterResourceItem is an internal alias kept for backward compatibility
// within this package.
type clusterResourceItem = NodeResource

// haStatusEntry is the best-effort typed shape of one entry in
// GET /cluster/ha/status/current. PVE returns an array of mixed-type objects;
// the fields used here are the ones PVE 7+ documents. Unknown fields are
// silently ignored via json.Unmarshal.
type haStatusEntry struct {
	Type  string `json:"type"`  // "manager_status", "resources", etc.
	Node  string `json:"node"`  // node name; present on type=manager_status
	State string `json:"state"` // "maintenance", "error", "fence", "online", etc.
}

// storageEntry is the typed shape of each entry in GET /nodes/{node}/storage.
type storageEntry struct {
	Storage string `json:"storage"`
	Active  int    `json:"active"`  // 1 = active
	Enabled int    `json:"enabled"` // 1 = enabled
	Content string `json:"content"` // comma-separated content types
	Avail   int64  `json:"avail"`   // available bytes
	Total   int64  `json:"total"`   // total bytes
}

// GatherOptions holds optional parameters for GatherNodeFacts.
type GatherOptions struct {
	// StorageName restricts the storage availability check to a specific pool.
	// When empty, storage facts (FreeStorageBytes, TotalStorageBytes) are zero.
	StorageName string

	// GroupTag is the exact PVE tag that marks membership in the BOSH instance
	// group being placed (e.g. "job--diego-cell"). GatherNodeFacts counts, per
	// node, how many existing QEMU guests carry this tag and records the total
	// in SameGroupCount, which the scorer's anti-affinity axis penalizes. The
	// caller is responsible for forming the tag with the same sanitization the
	// CPI uses when it stamps tags (see set_vm_metadata). When empty,
	// SameGroupCount remains 0 for all nodes.
	GroupTag string

	// ExcludeMaintenanceNodes, when true, causes GatherNodeFacts to query
	// ListHaStatusCurrent and mark nodes that are in a maintenance or error HA
	// state as InMaintenance=true. The per-node operator tag list
	// (MaintenanceNodeTags) is also checked. Errors from ListHaStatusCurrent
	// are non-fatal: affected nodes are left with InMaintenance=false (fail-open).
	ExcludeMaintenanceNodes bool

	// MaintenanceNodeTags is the list of PVE node tags that indicate a node is
	// in maintenance. A node carrying any of these tags has InMaintenance=true
	// regardless of HA status. Empty list disables tag-based detection.
	// The tags field on cluster status items is checked; not per-QEMU resource tags.
	MaintenanceNodeTags []string
}

// GatherNodeFacts assembles NodeFacts for every node reported by the PVE cluster.
// It makes three API calls (plus an optional fourth):
//  1. ListStatus — node online/offline, CPU, memory, tags.
//  2. ListResources — guest count, BOSH group tag, and committed (reserved)
//     memory per node (non-fatal on error).
//  3. Per-node ListStorage — available storage bytes (non-fatal on error per node).
//  4. ListHaStatusCurrent — HA maintenance state per node (non-fatal, only when
//     opts.ExcludeMaintenanceNodes is true).
//
// A ListResources error is non-fatal: GatherNodeFacts logs a warning and continues
// with GuestCount=0, SameGroupCount=0, and CommittedMemBytes=0 for all nodes.
//
// A per-node ListStorage error is non-fatal: that node's FreeStorageBytes and
// TotalStorageBytes remain 0, which causes the Storage scoring axis to be skipped
// for that node.
//
// A ListHaStatusCurrent error is non-fatal: all nodes are left with
// InMaintenance=false (fail-open) so a transient HA-API outage never blocks VM
// creation.
//
// A ListStatus error is fatal — it is the primary data source.
func GatherNodeFacts(
	ctx context.Context,
	clusterClient ClusterClient,
	nodesClient NodesClient,
	logger *log.Logger,
	opts GatherOptions,
) ([]NodeFacts, error) {
	// Phase 1: cluster status (node online/CPU/mem/tags).
	statusResp, err := clusterClient.ListStatus(ctx)
	if err != nil {
		return nil, err
	}

	// Parse node entries from status response.
	type nodeStatusItem struct {
		clusterStatusItem
		Tags string `json:"tags"` // space/semicolon-delimited PVE node tags
	}
	var nodeItems []clusterStatusItem
	nodeTagMap := make(map[string]string) // node name → raw tags string
	for i, raw := range *statusResp {
		var item nodeStatusItem
		if parseErr := json.Unmarshal(raw, &item); parseErr != nil {
			logger.Debug("placement: skip non-decodable cluster status entry",
				log.Int("idx", i), log.Err(parseErr))
			continue
		}
		if item.Type != "node" {
			continue
		}
		nodeItems = append(nodeItems, item.clusterStatusItem)
		if item.Tags != "" {
			nodeTagMap[item.Name] = item.Tags
		}
	}

	// Phase 1b: HA maintenance state (non-fatal, only when requested).
	var maintenanceNodes map[string]bool
	if opts.ExcludeMaintenanceNodes {
		maintenanceNodes = gatherHAMaintenanceNodes(ctx, clusterClient, logger)
	}

	// Phase 2: ListResources for guest count, group tags, and committed memory
	// (non-fatal).
	guestCounts, sameGroupCounts, committedMem := gatherGuestCounts(ctx, clusterClient, logger, opts.GroupTag)

	// Phase 3: per-node storage query (non-fatal per node).
	storageAvail, storageTotal := gatherStorageFacts(ctx, nodesClient, logger, nodeItems, opts.StorageName)

	// Assemble NodeFacts.
	facts := make([]NodeFacts, 0, len(nodeItems))
	for _, item := range nodeItems {
		nodeName := item.Name

		// Determine maintenance status: HA state OR operator node tag (union).
		inMaintenance := false
		if opts.ExcludeMaintenanceNodes {
			if maintenanceNodes[nodeName] {
				inMaintenance = true
			}
			if !inMaintenance && len(opts.MaintenanceNodeTags) > 0 {
				nodeTags := nodeTagMap[nodeName]
				for _, wantTag := range opts.MaintenanceNodeTags {
					if hasTag(nodeTags, wantTag) {
						inMaintenance = true
						break
					}
				}
			}
		}

		facts = append(facts, NodeFacts{
			Node:              nodeName,
			Online:            item.Online == 1,
			InMaintenance:     inMaintenance,
			FreeMemBytes:      item.Maxmem - item.Mem,
			TotalMemBytes:     item.Maxmem,
			CommittedMemBytes: committedMem[nodeName],
			FreeStorageBytes:  storageAvail[nodeName],
			TotalStorageBytes: storageTotal[nodeName],
			CPUUsed:           item.CPU,
			MaxCPU:            item.Maxcpu,
			GuestCount:        guestCounts[nodeName],
			SameGroupCount:    sameGroupCounts[nodeName],
		})
	}

	return facts, nil
}

// gatherHAMaintenanceNodes calls ListHaStatusCurrent and returns the set of
// node names currently in a maintenance or error HA state. Errors are
// non-fatal: on any failure the returned map is empty (fail-open) and a
// warning is logged. This mirrors the storage-facts fail-open pattern.
func gatherHAMaintenanceNodes(
	ctx context.Context,
	clusterClient ClusterClient,
	logger *log.Logger,
) map[string]bool {
	result := make(map[string]bool)

	resp, err := clusterClient.ListHaStatusCurrent(ctx)
	if err != nil {
		logger.Warn("placement: ListHaStatusCurrent failed — maintenance detection via HA disabled (fail-open)",
			log.Err(err))
		return result
	}
	if resp == nil {
		return result
	}

	for _, raw := range *resp {
		var entry haStatusEntry
		if parseErr := json.Unmarshal(raw, &entry); parseErr != nil {
			// Best-effort: skip malformed entries, never fail-hard.
			continue
		}
		// PVE HA status entries include both manager_status and resource entries.
		// The manager_status type carries the per-node state field.
		// We conservatively accept any entry that has a node name and a known
		// degraded-state value, regardless of the type field.
		if entry.Node == "" {
			continue
		}
		switch entry.State {
		case "maintenance", "error", "fence", "recovery":
			result[entry.Node] = true
		}
	}

	return result
}

// gatherGuestCounts calls ListResources and tallies, per node: QEMU guest
// count, same-group count, and total committed (reserved) memory in bytes.
// Errors are non-fatal: on failure all three maps are returned empty (all
// nodes get GuestCount=0 / SameGroupCount=0 / CommittedMemBytes=0).
//
// Committed memory sums each qemu resource's Maxmem regardless of the guest's
// run state — cluster/resources reports Maxmem from the guest's configuration,
// not its runtime status, so a stopped guest (one BOSH is about to start)
// still counts toward the node's reservation. A guest whose Maxmem is missing
// from the API response decodes to the Go zero value (0) and is added as-is
// (fail-open: the guest still counts toward GuestCount/SameGroupCount above,
// it just contributes nothing to CommittedMemBytes).
func gatherGuestCounts(
	ctx context.Context,
	clusterClient ClusterClient,
	logger *log.Logger,
	groupTag string,
) (guestCounts map[string]int, sameGroupCounts map[string]int, committedMem map[string]int64) {
	guestCounts = make(map[string]int)
	sameGroupCounts = make(map[string]int)
	committedMem = make(map[string]int64)

	resResp, resErr := clusterClient.ListResources(ctx, &cluster.ListResourcesParams{})
	if resErr != nil {
		logger.Warn("placement: ListResources failed — GuestCount=0 for all nodes",
			log.Err(resErr))
		return guestCounts, sameGroupCounts, committedMem
	}
	for _, raw := range *resResp {
		var ri clusterResourceItem
		if parseErr := json.Unmarshal(raw, &ri); parseErr != nil {
			continue
		}
		if ri.Type != "qemu" {
			continue
		}
		if ri.Node == "" {
			continue
		}
		guestCounts[ri.Node]++
		if groupTag != "" && hasTag(ri.Tags, groupTag) {
			sameGroupCounts[ri.Node]++
		}
		committedMem[ri.Node] += ri.Maxmem
	}
	return guestCounts, sameGroupCounts, committedMem
}

// gatherStorageFacts queries per-node storage availability for storageName.
// When storageName is empty the returned maps are empty. Errors per node are
// non-fatal: that node's entries remain zero.
func gatherStorageFacts(
	ctx context.Context,
	nodesClient NodesClient,
	logger *log.Logger,
	nodeItems []clusterStatusItem,
	storageName string,
) (map[string]int64, map[string]int64) {
	storageAvail := make(map[string]int64)
	storageTotal := make(map[string]int64)
	if storageName == "" {
		return storageAvail, storageTotal
	}
	for _, item := range nodeItems {
		nodeName := item.Name
		storResp, storErr := nodesClient.ListStorage(ctx, nodeName, &nodes.ListStorageParams{
			Storage: &storageName,
		})
		if storErr != nil {
			logger.Warn("placement: ListStorage failed for node — storage facts unavailable",
				log.String("node", nodeName),
				log.String("storage", storageName),
				log.Err(storErr))
			continue
		}
		for _, raw := range *storResp {
			var se storageEntry
			if parseErr := json.Unmarshal(raw, &se); parseErr != nil {
				continue
			}
			if se.Storage != storageName {
				continue
			}
			if se.Active != 1 {
				continue
			}
			if !hasImagesContent(se.Content) {
				continue
			}
			storageAvail[nodeName] = se.Avail
			storageTotal[nodeName] = se.Total
			break
		}
	}
	return storageAvail, storageTotal
}

// hasTag returns true when the PVE tags string contains an exact match for
// want. PVE returns the per-resource tags field as a delimited list; different
// PVE versions and endpoints use ";", "," or whitespace as the separator, so
// the scan splits on all three. The caller supplies want already formed and
// sanitized to match the CPI's stored tag scheme (e.g. "job--diego-cell").
func hasTag(tags, want string) bool {
	if tags == "" || want == "" {
		return false
	}
	for _, tag := range strings.FieldsFunc(tags, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if tag == want {
			return true
		}
	}
	return false
}

// hasImagesContent returns true when the comma-separated content string contains
// the "images" content type.
func hasImagesContent(content string) bool {
	for _, ct := range strings.Split(content, ",") {
		if strings.TrimSpace(ct) == "images" {
			return true
		}
	}
	return false
}
