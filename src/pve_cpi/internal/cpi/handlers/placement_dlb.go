package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// crsModeDynamic is the PVE cluster-resource-scheduler "ha" mode that enables
// the Dynamic Load Balancer; set via /cluster/options crs=ha=dynamic.
const crsModeDynamic = "dynamic"

// ensureDLBMembership registers an eligible VM as a PVE HA resource with
// auto-rebalance enabled so the PVE 9.2+ Dynamic Load Balancer places and
// continuously rebalances it. It is best-effort: every guard failure or PVE
// API error is returned for the caller to log as a warning — it never blocks
// create_vm.
//
// Guards evaluated in order:
//  1. PVE version >= 9.2 (nodes.ListVersion); if older or undeterminable as
//     older → log debug and return nil (skip).
//  2. Cluster has >= 2 nodes (ListConfigNodes); single-node = DLB is inert.
//  3. Shared-storage guard when DLBRequireSharedStorage() == true: if the VM
//     storage pool, disk pool, or ConfigDrive ISO pool is local → skip
//     registration. If undeterminable → proceed (fail-open) with a debug log.
//  4. Register: CreateHaResources with State=started and AutoRebalance=true.
//     If already registered → UpdateHaResources to ensure the flags are set.
//  5. CRS: when DLBManageClusterCRS() == true, ensure the cluster crs option is
//     set to ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1 via
//     UpdateOptions. When manage is false and crs is not dynamic, log a
//     one-time WARN with the corrective pvesh command.
func ensureDLBMembership(ctx context.Context, deps Deps, vmid int, az string, logger *log.Logger) error {
	logger.Debug("DLB: evaluating membership", log.Int("vmid", vmid), log.String("az", az))

	// Guard 1: version check.
	nodeName := ""
	if deps.Config != nil {
		nodeName = deps.Config.Node
	}
	if nodeName == "" {
		// Fall back to first node from cluster config.
		if nodeName2, err := firstClusterNode(ctx, deps); err == nil {
			nodeName = nodeName2
		}
	}
	if nodeName == "" {
		logger.Debug("DLB: no node name available for version check, skipping DLB")
		return nil
	}
	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		logger.Debug("DLB: nodes service unavailable, skipping DLB")
		return nil
	}
	verResp, verErr := deps.PVE.Nodes().ListVersion(ctx, nodeName)
	if verErr != nil {
		return fmt.Errorf("DLB: version check for node %q: %w", nodeName, verErr)
	}
	if verResp == nil {
		return fmt.Errorf("DLB: nil version response for node %q", nodeName)
	}
	if !pveVersionAtLeast(verResp.Version, 9, 2) {
		logger.Debug("DLB: PVE < 9.2, skipping",
			log.String("node", nodeName), log.String("version", verResp.Version))
		return nil
	}

	// Guard 2: cluster must have >= 2 nodes.
	nodeCount, countErr := clusterNodeCount(ctx, deps)
	if countErr != nil {
		return fmt.Errorf("DLB: cluster node count: %w", countErr)
	}
	if nodeCount < 2 {
		logger.Debug("DLB: single-node cluster, DLB is inert")
		return nil
	}

	// Guard 3: shared-storage requirement. A VM whose root pool, persistent
	// disk pool, OR config-drive ISO pool resides on node-local storage cannot
	// be live-migrated (the ISO pool holds the scsi30 CD-ROM that lives for the
	// VM's whole life, not only at boot — see the ISOStorage field doc in
	// internal/config/config.go), so any one of them being local disqualifies
	// the VM from DLB registration. Check all distinct, non-empty pools;
	// undeterminable shared-ness is fail-open (proceed).
	if deps.Config != nil && deps.Config.DLBRequireSharedStorage() {
		checked := map[string]struct{}{}
		for _, stg := range []string{deps.Config.VMStorage, deps.Config.DiskStorage, deps.Config.ISOStorage} {
			if stg == "" {
				continue
			}
			if _, dup := checked[stg]; dup {
				continue
			}
			checked[stg] = struct{}{}
			shared, knownErr := dlbStorageIsShared(ctx, deps, stg)
			if knownErr != nil {
				// Undeterminable → fail-open, proceed with debug log.
				logger.Debug("DLB: storage shared-ness undeterminable, proceeding fail-open",
					log.String("storage", stg), log.Err(knownErr))
				continue
			}
			if !shared {
				logger.Debug("DLB: local storage detected, skip HA registration",
					log.String("storage", stg))
				return nil
			}
		}
	}

	// Guard 4: register or update the HA resource.
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	stateVal := "started"
	arVal := true
	if createErr := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{
		Sid:           sid,
		State:         &stateVal,
		AutoRebalance: &arVal,
	}); createErr != nil {
		if isHaAlreadyExists(createErr) {
			// Resource exists (possibly from anti-affinity path); update flags.
			if updErr := svc.UpdateHaResources(ctx, sid, &cluster.UpdateHaResourcesParams{
				State:         &stateVal,
				AutoRebalance: &arVal,
			}); updErr != nil {
				return fmt.Errorf("DLB: update HA resource %q flags: %w", sid, updErr)
			}
		} else {
			return fmt.Errorf("DLB: register HA resource %q: %w", sid, createErr)
		}
	}

	// Guard 5: CRS option.
	if err := ensureDLBClusterCRS(ctx, deps, svc, logger); err != nil {
		return fmt.Errorf("DLB: cluster CRS check: %w", err)
	}

	return nil
}

// ensureDLBClusterCRS reads /cluster/options and either writes the required
// crs setting (when DLBManageClusterCRS is true) or emits a WARN when not
// dynamic and management is off.
func ensureDLBClusterCRS(ctx context.Context, deps Deps, svc cluster.Service, logger *log.Logger) error {
	raw, listErr := svc.ListOptions(ctx)
	if listErr != nil {
		return fmt.Errorf("read cluster options: %w", listErr)
	}

	// ListOptionsResponse is json.RawMessage; extract "crs" string field.
	crsStr := ""
	if raw != nil {
		var opts struct {
			Crs string `json:"crs"`
		}
		if jerr := json.Unmarshal(*raw, &opts); jerr == nil {
			crsStr = opts.Crs
		}
	}

	crsMap := parseCRS(crsStr)
	isDynamic := crsHasDynamic(crsMap)

	if deps.Config != nil && deps.Config.DLBManageClusterCRS() {
		if isDynamic &&
			crsMap["ha-rebalance-on-start"] == "1" &&
			crsMap["ha-auto-rebalance"] == "1" {
			// Already correct — nothing to write.
			return nil
		}
		// Merge the required keys into whatever is currently configured.
		crsMap["ha"] = crsModeDynamic
		crsMap["ha-rebalance-on-start"] = "1"
		crsMap["ha-auto-rebalance"] = "1"
		newCRS := formatCRS(crsMap)
		if updErr := svc.UpdateOptions(ctx, &cluster.UpdateOptionsParams{
			Crs: &newCRS,
		}); updErr != nil {
			return fmt.Errorf("set cluster crs=%q: %w", newCRS, updErr)
		}
		return nil
	}

	// manage_cluster_crs is false: read-only. Warn once if not dynamic.
	if !isDynamic {
		logger.Warn("DLB: cluster crs is not dynamic; DLB rebalancing is inactive",
			log.String("current_crs", crsStr),
			log.String("fix", "pvesh set /cluster/options --crs ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1"))
	}
	return nil
}

// firstClusterNode returns the name of the first node from ListConfigNodes,
// used when deps.Config.Node is empty.
func firstClusterNode(ctx context.Context, deps Deps) (string, error) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		return "", fmt.Errorf("cluster service unavailable")
	}
	resp, err := deps.PVE.Cluster().ListConfigNodes(ctx)
	if err != nil || resp == nil || len(*resp) == 0 {
		return "", fmt.Errorf("no cluster nodes available")
	}
	var n struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	if jerr := json.Unmarshal((*resp)[0], &n); jerr != nil {
		return "", jerr
	}
	if n.Node != "" {
		return n.Node, nil
	}
	return n.Name, nil
}

// dlbStorageIsShared classifies whether the named storage is shared.
// Returns (true, nil) for shared, (false, nil) for local,
// (false, non-nil) when the classification cannot be determined.
// Fail-open callers treat a non-nil error as "proceed anyway".
//
// Classification is via pve.SharedViaBacking against the FULL /storage index
// (not just the one named entry): this closes a config-drift gap where an
// operator registers the same network mount under two storage IDs and only
// remembers to flag one of them "shared: 1" in storage.cfg — see the
// SharedViaBacking doc comment. Requires the full index (rather than a
// single-name StorageInfoCache.Get) precisely because the propagation needs
// to see every OTHER entry's BackingKey, which is why this stays a live
// listing rather than routing through the cache.
func dlbStorageIsShared(ctx context.Context, deps Deps, storage string) (bool, error) {
	if storage == "" {
		return false, fmt.Errorf("DLB storage guard: storage name is empty")
	}
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
		return false, fmt.Errorf("DLB storage guard: ClusterStorage unavailable")
	}
	resp, err := deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil {
		return false, fmt.Errorf("DLB storage guard: list cluster storage: %w", err)
	}
	if resp == nil {
		return false, fmt.Errorf("DLB storage guard: nil response from cluster storage list")
	}

	all := make([]pve.StorageInfo, 0, len(*resp))
	var target pve.StorageInfo
	found := false
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			continue
		}
		all = append(all, info)
		if info.Name == storage {
			target = info
			found = true
		}
	}
	if !found {
		return false, fmt.Errorf("DLB storage guard: storage %q not found in cluster storage list", storage)
	}
	return pve.SharedViaBacking(target, all), nil
}

// pveVersionAtLeast parses a PVE version string (e.g. "9.2-1", "9.2", "10.0")
// and reports whether it is >= major.minor. The version string format is:
//
//	"<major>.<minor>[-<build>]"  (e.g. "9.2-1", "9.1-3", "10.0", "8.4-2")
//
// Inputs and edge cases:
//   - empty string → false (version is unknown; skip conservatively)
//   - missing minor segment → false (not determinable as >=, skip conservatively)
//   - build suffix after "-" in the minor segment is stripped before comparison
//   - major > target_major → true regardless of minor
//   - major == target_major, minor >= target_minor → true
//   - major < target_major → false
func pveVersionAtLeast(version string, major, minor int) bool {
	if version == "" {
		return false
	}
	// Strip any trailing whitespace.
	version = strings.TrimSpace(version)
	// Split into major and the rest (e.g. "9" and "2-1" or "2").
	parts := strings.SplitN(version, ".", 2)
	if len(parts) < 2 {
		// No "." → cannot determine minor; skip conservatively.
		return false
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	// Strip build suffix from the minor segment: "2-1" → "2".
	minorPart := parts[1]
	if idx := strings.IndexByte(minorPart, '-'); idx >= 0 {
		minorPart = minorPart[:idx]
	}
	// Minor may itself contain a further "." (e.g. "2.3"); take only the first component.
	if idx := strings.IndexByte(minorPart, '.'); idx >= 0 {
		minorPart = minorPart[:idx]
	}
	gotMinor, err := strconv.Atoi(minorPart)
	if err != nil {
		return false
	}
	if gotMajor > major {
		return true
	}
	if gotMajor == major && gotMinor >= minor {
		return true
	}
	return false
}

// parseCRS parses a PVE crs option string into a key→value map.
// PVE format: "ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1"
// Each token is "key=value"; tokens without "=" are stored with an empty
// string value so they are preserved through a round-trip via formatCRS.
// An empty input string returns an empty (non-nil) map.
func parseCRS(s string) map[string]string {
	out := make(map[string]string)
	if s == "" {
		return out
	}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if idx := strings.IndexByte(tok, '='); idx >= 0 {
			key := strings.TrimSpace(tok[:idx])
			val := strings.TrimSpace(tok[idx+1:])
			if key != "" {
				out[key] = val
			}
		} else {
			// Token with no "=": store as key → "".
			out[tok] = ""
		}
	}
	return out
}

// crsHasDynamic reports whether the parsed CRS map has ha=dynamic.
func crsHasDynamic(m map[string]string) bool {
	return m["ha"] == crsModeDynamic
}

// formatCRS serializes a CRS map back to the PVE "key=value,..." string format
// with keys in deterministic (sorted) order so the output is stable across
// round-trips.
func formatCRS(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if v == "" {
			parts = append(parts, k)
		} else {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ",")
}
