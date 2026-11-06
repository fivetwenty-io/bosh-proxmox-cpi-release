package placement

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// ClusterClient is the subset of the PVE cluster API used by GatherNodeFacts.
// It is satisfied by the full pve.Client.Cluster() return value.
type ClusterClient interface {
	ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error)
	ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
	ListHaStatusCurrent(ctx context.Context) (*cluster.ListHaStatusCurrentResponse, error)
	ListHaStatusManagerStatus(ctx context.Context) (*cluster.ListHaStatusManagerStatusResponse, error)
}

// NodesClient is the subset of the PVE nodes API used by GatherNodeFacts.
// It is satisfied by the full pve.Client.Nodes() return value.
type NodesClient interface {
	ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
	ListTime(ctx context.Context, node string) (*nodes.ListTimeResponse, error)
}

// clusterStatusItem carries membership from GET /cluster/status. Resource
// counters come from GET /cluster/resources; optional status counters remain
// compatible with older API fixtures and are overridden by resource observations.
// The scalar fields use the SDK's tolerant types because PVE renders "online"
// as the integer 1/0 and has returned the counters as strings on some
// versions; a plain int64 or float64 would reject the row and silently drop
// the node from placement.
type clusterStatusItem struct {
	Type   string             `json:"type"`
	Name   string             `json:"name"`
	Online sdkclient.PVEBool  `json:"online"`
	Maxcpu sdkclient.PVEInt   `json:"maxcpu"`
	Maxmem sdkclient.PVEInt   `json:"maxmem"`
	Mem    sdkclient.PVEInt   `json:"mem"`
	CPU    sdkclient.PVEFloat `json:"cpu"` // current utilisation fraction [0,1]
}

// NodeResource is the typed shape for each entry in GET /cluster/resources
// that the placement scorer cares about. Unknown fields are silently ignored.
// Exported so callers with an existing []json.RawMessage can decode without
// re-issuing the API call.
type NodeResource struct {
	Type   string             `json:"type"`
	Node   string             `json:"node"`
	Name   string             `json:"name"`   // VM name; used for group tag matching
	Tags   string             `json:"tags"`   // space-separated PVE tags
	Maxmem sdkclient.PVEInt   `json:"maxmem"` // configured guest memory or total node memory
	Maxcpu sdkclient.PVEInt   `json:"maxcpu"`
	Mem    sdkclient.PVEInt   `json:"mem"`
	CPU    sdkclient.PVEFloat `json:"cpu"`
}

// clusterResourceItem is an internal alias kept for backward compatibility
// within this package.
type clusterResourceItem = NodeResource

// storageEntry is the typed shape of each entry in GET /nodes/{node}/storage.
type storageEntry struct {
	Storage string            `json:"storage"`
	Active  sdkclient.PVEBool `json:"active"`  // PVE renders this as 1/0
	Enabled sdkclient.PVEBool `json:"enabled"` // PVE renders this as 1/0
	Content string            `json:"content"` // comma-separated content types
	Avail   sdkclient.PVEInt  `json:"avail"`   // available bytes
	Total   sdkclient.PVEInt  `json:"total"`   // total bytes
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

	// ExcludeMaintenanceNodes, when true, causes GatherNodeFacts to ask
	// pve.NodeHealth about every node and mark each node that is not placeable
	// as InMaintenance=true. That covers a node in maintenance, a fenced or
	// unknown node, and every node when the entry node is not quorate. The
	// per-node operator tag list (MaintenanceNodeTags) is an input to the same
	// answer. A source that cannot be read is non-fatal: pve.NodeHealth falls
	// back to the sources it can read, down to cluster status alone.
	ExcludeMaintenanceNodes bool

	// MaintenanceNodeTags is the list of PVE node tags that indicate a node is
	// in maintenance. A node carrying any of these tags has InMaintenance=true
	// regardless of HA status. Empty list disables tag-based detection.
	// The tags field on cluster status items is checked; not per-QEMU resource tags.
	MaintenanceNodeTags []string
}

// GatherNodeFacts assembles NodeFacts for every node reported by the PVE cluster.
// It observes the following APIs:
//  1. ListStatus provides node membership, online state, and tags.
//  2. ListResources provides node CPU and memory counters, guest counts, BOSH
//     group tags, and committed guest memory (non-fatal on error).
//  3. Per-node ListStorage provides available storage bytes (non-fatal on error).
//  4. pve.NodeHealth provides node placeability from the CRM and LRM state,
//     cluster status, and operator tags when opts.ExcludeMaintenanceNodes is
//     true (non-fatal on error).
//
// A ListResources error is non-fatal: GatherNodeFacts logs a warning and continues
// with GuestCount=0, SameGroupCount=0, and CommittedMemBytes=0 for all nodes.
//
// A per-node ListStorage error is non-fatal: that node's FreeStorageBytes and
// TotalStorageBytes remain 0, which causes the Storage scoring axis to be skipped
// for that node.
//
// A NodeHealth failure is non-fatal: all nodes are left with
// InMaintenance=false (fail-open) so a transient API outage never blocks VM
// creation. NodeHealth itself degrades past an unreadable HA source, so only
// a failed cluster status read reaches this path.
//
// A ListStatus error is fatal — it is the primary data source.
func GatherNodeFacts(
	ctx context.Context,
	clusterClient ClusterClient,
	nodesClient NodesClient,
	logger *log.Logger,
	opts GatherOptions,
) ([]NodeFacts, error) {
	// Phase 1: cluster membership (node online state and tags).
	statusResp, err := clusterClient.ListStatus(ctx)
	if err != nil {
		return nil, err
	}

	// Parse node entries from status response.
	var nodeItems []clusterStatusItem
	for i, raw := range *statusResp {
		var item clusterStatusItem
		if parseErr := json.Unmarshal(raw, &item); parseErr != nil {
			logger.Debug("placement: skip non-decodable cluster status entry",
				log.Int("idx", i), log.Err(parseErr))
			continue
		}
		if item.Type != "node" {
			continue
		}
		nodeItems = append(nodeItems, item)
	}

	// Phase 1b: node health (non-fatal, only when requested).
	var health *pve.NodeHealthReport
	if opts.ExcludeMaintenanceNodes {
		health = gatherNodeHealth(ctx, clusterClient, nodesClient, logger, opts.MaintenanceNodeTags)
	}

	// Phase 2: node compute counters, guest count, group tags, and committed
	// memory from ListResources (non-fatal).
	guestCounts, sameGroupCounts, committedMem, nodeResources := gatherResourceFacts(ctx, clusterClient, logger, opts.GroupTag)

	// Phase 3: per-node storage query (non-fatal per node).
	storageAvail, storageTotal := gatherStorageFacts(ctx, nodesClient, logger, nodeItems, opts.StorageName)

	// Assemble NodeFacts.
	facts := make([]NodeFacts, 0, len(nodeItems))
	for _, item := range nodeItems {
		nodeName := item.Name
		if resource, ok := nodeResources[nodeName]; ok {
			item.Maxcpu, item.Maxmem, item.Mem, item.CPU = resource.Maxcpu, resource.Maxmem, resource.Mem, resource.CPU
		}

		// A node that pve.NodeHealth does not call placeable is excluded. The
		// report already folds the operator node tags into its answer. A node
		// the report does not mention, or no report at all, leaves it included.
		inMaintenance := false
		if entry, ok := health.Node(nodeName); ok {
			inMaintenance = !entry.Placeable
		}

		// PVEFloat parses whatever string PVE sends, so a "nan" or "inf" cpu
		// value would reach the scorer as a non-finite number, poison the
		// score, and make the sort comparator non-transitive. Treat it as
		// unknown load, which is what a missing field already reads as.
		cpuUsed := item.CPU.Float()
		if math.IsNaN(cpuUsed) || math.IsInf(cpuUsed, 0) {
			cpuUsed = 0
		}

		facts = append(facts, NodeFacts{
			Node:              nodeName,
			Online:            item.Online.Bool(),
			InMaintenance:     inMaintenance,
			FreeMemBytes:      item.Maxmem.Int() - item.Mem.Int(),
			TotalMemBytes:     item.Maxmem.Int(),
			CommittedMemBytes: committedMem[nodeName],
			FreeStorageBytes:  storageAvail[nodeName],
			TotalStorageBytes: storageTotal[nodeName],
			CPUUsed:           cpuUsed,
			MaxCPU:            item.Maxcpu.Int(),
			GuestCount:        guestCounts[nodeName],
			SameGroupCount:    sameGroupCounts[nodeName],
		})
	}

	return facts, nil
}

// gatherNodeHealth asks pve.NodeHealth for the merged health of every node. A
// failure is non-fatal: the returned report is nil (fail-open) and a warning is
// logged. This mirrors the storage-facts fail-open pattern.
func gatherNodeHealth(
	ctx context.Context,
	clusterClient ClusterClient,
	nodesClient NodesClient,
	logger *log.Logger,
	maintenanceTags []string,
) *pve.NodeHealthReport {
	report, err := pve.NodeHealth(ctx, clusterClient, nodesClient, pve.NodeHealthOptions{
		MaintenanceTags: maintenanceTags,
	})
	if err != nil {
		logger.Warn("placement: node health unavailable — maintenance detection disabled (fail-open)",
			log.Err(err))
		return nil
	}
	if report.HA != pve.HAViewManagerStatus {
		logger.Debug("placement: node health did not use a fresh CRM view",
			log.String("view", string(report.HA)), log.String("reason", report.HAReason))
	}
	return report
}

// gatherResourceFacts reads node compute counters and tallies QEMU guest
// count, same-group count, and committed memory from one resource observation.
// Errors are non-fatal. On failure all maps are empty; nodes retain any
// available status counters and have no observed guest reservations.
//
// Committed memory sums each qemu resource's Maxmem regardless of the guest's
// run state — cluster/resources reports Maxmem from the guest's configuration,
// not its runtime status, so a stopped guest (one BOSH is about to start)
// still counts toward the node's reservation. A guest whose Maxmem is missing
// from the API response decodes to the Go zero value (0) and is added as-is
// (fail-open: the guest still counts toward GuestCount/SameGroupCount above,
// it just contributes nothing to CommittedMemBytes).
func gatherResourceFacts(
	ctx context.Context,
	clusterClient ClusterClient,
	logger *log.Logger,
	groupTag string,
) (guestCounts map[string]int, sameGroupCounts map[string]int, committedMem map[string]int64, nodeResources map[string]NodeResource) {
	guestCounts = make(map[string]int)
	sameGroupCounts = make(map[string]int)
	committedMem = make(map[string]int64)
	nodeResources = make(map[string]NodeResource)

	resResp, resErr := clusterClient.ListResources(ctx, &cluster.ListResourcesParams{})
	if resErr != nil {
		logger.Warn("placement: ListResources failed — GuestCount=0 for all nodes",
			log.Err(resErr))
		return guestCounts, sameGroupCounts, committedMem, nodeResources
	}
	if resResp == nil {
		return guestCounts, sameGroupCounts, committedMem, nodeResources
	}
	for _, raw := range *resResp {
		var ri clusterResourceItem
		if parseErr := json.Unmarshal(raw, &ri); parseErr != nil {
			continue
		}
		if ri.Type == "node" && ri.Node != "" {
			nodeResources[ri.Node] = ri
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
		committedMem[ri.Node] += ri.Maxmem.Int()
	}
	return guestCounts, sameGroupCounts, committedMem, nodeResources
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
			if !se.Active.Bool() {
				continue
			}
			if !hasImagesContent(se.Content) {
				continue
			}
			storageAvail[nodeName] = se.Avail.Int()
			storageTotal[nodeName] = se.Total.Int()
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
