package placement

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// ClusterClient is the subset of the PVE cluster API used by GatherNodeFacts.
// It is satisfied by the full pve.Client.Cluster() return value.
type ClusterClient interface {
	ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error)
	ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
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
	Type string `json:"type"`
	Node string `json:"node"`
	Name string `json:"name"` // VM name; used for group tag matching
	Tags string `json:"tags"` // space-separated PVE tags
}

// clusterResourceItem is an internal alias kept for backward compatibility
// within this package.
type clusterResourceItem = NodeResource

// storageEntry is the typed shape of each entry in GET /nodes/{node}/storage.
type storageEntry struct {
	Storage  string `json:"storage"`
	Active   int    `json:"active"`   // 1 = active
	Enabled  int    `json:"enabled"`  // 1 = enabled
	Content  string `json:"content"`  // comma-separated content types
	Avail    int64  `json:"avail"`    // available bytes
	Total    int64  `json:"total"`    // total bytes
}

// GatherOptions holds optional parameters for GatherNodeFacts.
type GatherOptions struct {
	// StorageName restricts the storage availability check to a specific pool.
	// When empty, storage facts (FreeStorageBytes, TotalStorageBytes) are zero.
	StorageName string

	// BOSHGroup is the BOSH instance group name used to compute SameGroupCount
	// (the number of existing VMs on each node from the same group). When empty,
	// SameGroupCount remains 0 for all nodes.
	BOSHGroup string
}

// GatherNodeFacts assembles NodeFacts for every node reported by the PVE cluster.
// It makes three API calls:
//  1. ListStatus — node online/offline, CPU, memory.
//  2. ListResources — guest count and BOSH group tag per node (non-fatal on error).
//  3. Per-node ListStorage — available storage bytes (non-fatal on error per node).
//
// A ListResources error is non-fatal: GatherNodeFacts logs a warning and continues
// with GuestCount=0 and SameGroupCount=0 for all nodes.
//
// A per-node ListStorage error is non-fatal: that node's FreeStorageBytes and
// TotalStorageBytes remain 0, which causes the Storage scoring axis to be skipped
// for that node.
//
// A ListStatus error is fatal — it is the primary data source.
func GatherNodeFacts(
	ctx context.Context,
	clusterClient ClusterClient,
	nodesClient NodesClient,
	logger *log.Logger,
	opts GatherOptions,
) ([]NodeFacts, error) {
	// Phase 1: cluster status (node online/CPU/mem).
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

	// Phase 2: ListResources for guest count and group tags (non-fatal).
	guestCounts, sameGroupCounts := gatherGuestCounts(ctx, clusterClient, logger, opts.BOSHGroup)

	// Phase 3: per-node storage query (non-fatal per node).
	storageAvail, storageTotal := gatherStorageFacts(ctx, nodesClient, logger, nodeItems, opts.StorageName)

	// Assemble NodeFacts.
	facts := make([]NodeFacts, 0, len(nodeItems))
	for _, item := range nodeItems {
		nodeName := item.Name
		facts = append(facts, NodeFacts{
			Node:              nodeName,
			Online:            item.Online == 1,
			FreeMemBytes:      item.Maxmem - item.Mem,
			TotalMemBytes:     item.Maxmem,
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

// gatherGuestCounts calls ListResources and tallies per-node QEMU guest
// counts and same-group counts. Errors are non-fatal: on failure both maps
// are returned empty (all nodes get GuestCount=0 / SameGroupCount=0).
func gatherGuestCounts(
	ctx context.Context,
	clusterClient ClusterClient,
	logger *log.Logger,
	boshGroup string,
) (map[string]int, map[string]int) {
	guestCounts := make(map[string]int)
	sameGroupCounts := make(map[string]int)

	resResp, resErr := clusterClient.ListResources(ctx, &cluster.ListResourcesParams{})
	if resErr != nil {
		logger.Warn("placement: ListResources failed — GuestCount=0 for all nodes",
			log.Err(resErr))
		return guestCounts, sameGroupCounts
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
		if boshGroup != "" && matchesBOSHGroup(ri.Tags, boshGroup) {
			sameGroupCounts[ri.Node]++
		}
	}
	return guestCounts, sameGroupCounts
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

// ParseNodeResources decodes a cluster ListResources response slice into typed
// NodeResource values. Entries that fail JSON decode are skipped without error;
// the caller receives a best-effort slice of decodable entries.
func ParseNodeResources(raw []json.RawMessage) ([]NodeResource, error) {
	out := make([]NodeResource, 0, len(raw))
	for _, r := range raw {
		var item NodeResource
		if err := json.Unmarshal(r, &item); err != nil {
			continue // skip malformed entries
		}
		out = append(out, item)
	}
	return out, nil
}

// matchesBOSHGroup returns true when the space-separated PVE tags string
// contains a tag that matches the expected BOSH group name. PVE stores BOSH
// group tags in the format "bosh.<group>" where <group> is sanitized.
// The match first checks the exact group name, then the prefixed form.
func matchesBOSHGroup(tags, group string) bool {
	if tags == "" || group == "" {
		return false
	}
	prefixed := "bosh." + sanitizeTagValue(group)
	sanitizedGroup := sanitizeTagValue(group)
	for _, tag := range strings.Fields(tags) {
		tag = strings.TrimSpace(tag)
		if tag == sanitizedGroup || tag == prefixed || tag == group {
			return true
		}
	}
	return false
}

// sanitizeTagValue maps a BOSH group name to a PVE-safe tag value.
// PVE tag names allow [a-z0-9_-]; uppercase is lowercased, other chars become "-".
func sanitizeTagValue(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32) // lowercase
		case c >= 'a' && c <= 'z':
			out = append(out, c)
		case c >= '0' && c <= '9':
			out = append(out, c)
		case c == '_' || c == '-' || c == '.':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
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
