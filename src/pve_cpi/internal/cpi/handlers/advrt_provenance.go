// advrt_provenance.go — provenance tags linking a VM to the SDN subnets its
// advertised_routes created, so delete_vm can clean them up.
//
// Why tags: set_vm_metadata overwrites the VM Description wholesale, so the
// description cannot carry provenance. Tags outside the reserved BOSH
// prefixes survive the set_vm_metadata read-modify-write, and the PVE tag
// charset ([A-Za-z0-9-]) forbids raw CIDRs — hence the hash encoding.
//
// Tag format: "advrt-<vnet>-<hash8>" where hash8 is the first 8 hex digits
// of FNV-1a-64 over "<vnet>/<cidr>". Vnet names are 1–8 chars of [a-z0-9]
// (no dashes), so the middle segment parses unambiguously. delete_vm never
// decodes the CIDR from the hash — it recomputes the hash for each existing
// subnet of the vnet and deletes on match, so IPv6 and every CIDR shape
// round-trip losslessly.
package handlers

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// advrtTagPrefix is the provenance tag namespace. Not in
// reservedBoshTagPrefixes — set_vm_metadata must preserve these tags.
const advrtTagPrefix = "advrt-"

// advrtHash8 returns the first 8 hex digits of FNV-1a-64 over "<vnet>/<cidr>".
func advrtHash8(vnet, cidr string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(vnet + "/" + cidr))
	return fmt.Sprintf("%016x", h.Sum64())[:8]
}

// advertisedRouteTag returns the provenance tag for one advertised route.
// Max length: 6 + 8 + 1 + 8 = 23 bytes (vnet names cap at 8 chars).
func advertisedRouteTag(vnet, cidr string) string {
	return advrtTagPrefix + vnet + "-" + advrtHash8(vnet, cidr)
}

// advertisedRouteTags returns the sorted, deduplicated provenance tags for
// all routes. Sorted so the tag string is deterministic across retries.
func advertisedRouteTags(routes []AdvertisedRoute) []string {
	if len(routes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(routes))
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		tag := advertisedRouteTag(r.VNet, r.Destination)
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// advrtTagRef is one parsed provenance tag: the full tag (for refcounting
// against other VMs) plus its vnet and hash components.
type advrtTagRef struct {
	tag   string
	vnet  string
	hash8 string
}

// parseAdvertisedRouteTags extracts the advrt provenance tags from PVE's
// semicolon-separated tag string. Malformed entries (wrong segment count,
// short hash) are skipped — fail-open, never an error.
func parseAdvertisedRouteTags(tags string) []advrtTagRef {
	if tags == "" {
		return nil
	}
	var out []advrtTagRef
	for _, t := range strings.Split(tags, ";") {
		t = strings.TrimSpace(t)
		if !strings.HasPrefix(t, advrtTagPrefix) {
			continue
		}
		rest := t[len(advrtTagPrefix):]
		// rest = "<vnet>-<hash8>"; vnet contains no dashes.
		idx := strings.LastIndex(rest, "-")
		if idx <= 0 || len(rest)-idx-1 != 8 {
			continue
		}
		out = append(out, advrtTagRef{tag: t, vnet: rest[:idx], hash8: rest[idx+1:]})
	}
	return out
}
