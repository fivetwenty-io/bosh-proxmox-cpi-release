package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// crsModeDynamic is the PVE cluster-resource-scheduler "ha" mode that enables
// the Dynamic Load Balancer; set via /cluster/options crs=ha=dynamic.
const crsModeDynamic = "dynamic"

// ensureDLBMembership checks the cluster's CRS mode for a VM whose HA decision
// registers it in the DLB AZ. The decision already settled whether the VM may
// join the PVE 9.2+ Dynamic Load Balancer: the version floor, a cluster with
// two or more placeable nodes, and shared storage for every volume. Staging
// already registered the resource with auto-rebalance, so what remains is the
// CRS check, which logs a WARN with the corrective pvesh command when the
// cluster's crs is not dynamic. The CPI never writes the CRS mode. A VM outside
// the DLB AZ, or one the decision does not register, needs no check. A failed
// read is returned for the caller to log.
func ensureDLBMembership(ctx context.Context, deps Deps, d *ha.VMDecision, logger *log.Logger) error {
	if !d.Registers() || !d.DLB {
		return nil
	}
	if err := ensureDLBClusterCRS(ctx, deps.PVE.Cluster(), logger); err != nil {
		return fmt.Errorf("DLB: cluster CRS check: %w", err)
	}
	return nil
}

// ensureDLBClusterCRS reads /cluster/options and emits a WARN when the crs
// setting is not dynamic. It never writes the setting, because the CRS mode
// affects every HA guest in the cluster and belongs to the operator. A crs
// value the CPI cannot decode is an error: the corrective command below merges
// into the decoded sub-options, and merging from an empty base would drop every
// sub-option the operator configured.
func ensureDLBClusterCRS(ctx context.Context, svc cluster.Service, logger *log.Logger) error {
	opts, listErr := svc.ListOptions(ctx)
	if listErr != nil {
		return fmt.Errorf("read cluster options: %w", listErr)
	}

	crsMap := make(map[string]string)
	if opts != nil {
		parsed, parseErr := parseCRSRaw(opts.Crs)
		if parseErr != nil {
			return fmt.Errorf("decode cluster crs %s: %w", strings.TrimSpace(string(opts.Crs)), parseErr)
		}
		crsMap = parsed
	}
	isDynamic := crsHasDynamic(crsMap)

	// Warn if not dynamic, and hand the operator the merged setting so the
	// corrective command keeps whatever else they configured; pvesh set --crs
	// replaces the whole property string.
	if !isDynamic {
		logger.Warn("DLB: cluster crs is not dynamic; DLB rebalancing is inactive",
			log.String("current_crs", formatCRS(crsMap)),
			log.String("fix", "pvesh set /cluster/options --crs "+formatCRS(withRequiredCRS(crsMap))))
	}
	return nil
}

// withRequiredCRS returns a copy of m with the three sub-options DLB needs set,
// leaving every other sub-option the operator configured in place.
func withRequiredCRS(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+3)
	for k, v := range m {
		out[k] = v
	}
	out["ha"] = crsModeDynamic
	out["ha-rebalance-on-start"] = "1"
	out["ha-auto-rebalance"] = "1"
	return out
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

// parseCRSRaw parses the crs field of GET /cluster/options into the same
// key→value map parseCRS produces. PVE returns the parsed datacenter.cfg, so
// crs normally arrives as an object keyed by sub-option, with the boolean
// flags as unquoted integers; a bare property string is still accepted for
// the shape a hand-written configuration can produce. An absent or null field
// is an empty map. Any other shape, or a sub-option whose value is not a JSON
// scalar, is an error rather than an empty map, so a caller never merges into
// an empty base and writes the operator's other sub-options away.
func parseCRSRaw(raw json.RawMessage) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return make(map[string]string), nil
	}

	var asString string
	if err := json.Unmarshal([]byte(trimmed), &asString); err == nil {
		return parseCRS(asString), nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return nil, fmt.Errorf("crs is neither a property string nor an object of sub-options: %w", err)
	}
	out := make(map[string]string, len(fields))
	for key, val := range fields {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		scalar, err := crsScalar(val)
		if err != nil {
			return nil, fmt.Errorf("crs sub-option %q: %w", key, err)
		}
		out[key] = scalar
	}
	return out, nil
}

// crsScalar renders one crs sub-option as the string the map holds. PVE sends
// the boolean flags as unquoted 1/0, so a JSON number or bool becomes the
// digit crsHasDynamic, the flag comparisons, and formatCRS expect. Strings are
// trimmed the way parseCRS trims property-string values. Anything that is not
// a JSON scalar is an error rather than its raw JSON text, so an object or
// array can never be spliced into the property string the CPI writes back.
func crsScalar(val json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(val))
	switch trimmed {
	case "", "null":
		return "", nil
	case "true":
		return "1", nil
	case "false":
		return "0", nil
	}
	var asString string
	if err := json.Unmarshal([]byte(trimmed), &asString); err == nil {
		return strings.TrimSpace(asString), nil
	}
	var asNumber json.Number
	if err := json.Unmarshal([]byte(trimmed), &asNumber); err == nil {
		return asNumber.String(), nil
	}
	return "", fmt.Errorf("value %s is not a JSON scalar", trimmed)
}

// parseCRS parses a PVE crs option string into a key→value map.
// PVE format: "ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1"
// Each token is "key=value"; a token without "=" is stored with an empty
// string value so the read side keeps it, and formatCRS drops it again on the
// way out. An empty input string returns an empty (non-nil) map.
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

// formatCRS serializes a CRS map back to the PVE "key=value,..." property
// string with keys in deterministic (sorted) order so the output is stable
// across round-trips. A key with an empty value is dropped: the crs format
// declares no default key, so PVE rejects a bare token in a write, and a
// value-less key is only ever a read-side artifact of parseCRS.
func formatCRS(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
