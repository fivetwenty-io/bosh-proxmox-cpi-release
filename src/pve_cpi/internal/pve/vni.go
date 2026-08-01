package pve

import (
	"context"
	"encoding/json"
	mrand "math/rand/v2"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// globalVNIMu is the process-level mutex that serialises VNI allocation,
// mirroring globalVMIDMu. It prevents two goroutines within the same CPI
// process from reading the vnet list simultaneously and returning the same
// VNI. Cross-process races are handled by the caller's conflict handling
// (PVE rejects duplicate tags within a zone).
var globalVNIMu sync.Mutex

// vniZoneListWarnOnce guards the single warning emitted when the zone-level
// VNI exclusion listing fails. Fail-open: VNI allocation proceeds using only
// the vnet-tag exclusion set, exactly as it did before zone-level exclusion
// was added. One warning per CPI process is enough to alert the operator
// without flooding logs on every subsequent allocation while the underlying
// PVE fault persists — mirrors the localISOStorageWarnOnce once-per-process
// idiom used elsewhere in this codebase.
//
// Process-scoped, not cluster-scoped: with per-request context overrides one
// process can serve several clusters, and only the first to hit the failure
// warns. Accepted for this low-stakes diagnostic; contrast handlers'
// firewallMasterSwitchProbedClusters, keyed per cluster because its warning
// is enforcement-relevant. Tests reset it via export_test.go so the suite is
// repeat-safe under -count=N.
var vniZoneListWarnOnce sync.Once

// zoneReservedVNIFields decodes the numeric VNI-shaped fields a PVE SDN zone
// row may carry:
//
//   - vrf-vxlan: an EVPN zone's control-plane VNI (the VXLAN Network
//     Identifier PVE uses for the zone's own VRF, distinct from any vnet's
//     tag). PVE accepts a vnet tag that collides with this value — VNIs are
//     fabric-global, but PVE's per-zone tag-uniqueness check does not look
//     across zones — so an un-excluded collision silently cross-talks or
//     blackholes traffic on that VRF.
//   - tag: some zone types carry their own zone-level tag/VNI reservation
//     (schema varies by plugin type; this field is included defensively so a
//     zone-level tag, if PVE ever populates one, is excluded the same way).
//
// Both fields are optional and zone-type-dependent; PVE's zone row schema is
// sparse and varies by plugin type (see SDNZone's own doc comment), so
// decoding tolerates either or both being absent. Uses the same plain int64
// JSON typing as SDNVnet.Tag elsewhere in this file — PVE encodes these as
// JSON numbers, not the 0/1 integer-boolean convention pveBool exists for.
type zoneReservedVNIFields struct {
	VrfVxlan *int64 `json:"vrf-vxlan,omitempty"`
	Tag      *int64 `json:"tag,omitempty"`
}

// zoneReservedVNIs extracts the zone-level VNI(s) that must never be handed
// out to a new vnet from a single zone row's raw JSON. Returns nil for a zone
// that carries neither field (the common case — most zone types, including
// the CPI's own turnkey vxlan zone, reserve nothing at the zone level) or
// whose raw JSON fails to decode (malformed rows are skipped, not treated as
// fatal — one bad row must not abort exclusion for every other zone).
func zoneReservedVNIs(raw json.RawMessage) []int {
	if len(raw) == 0 {
		return nil
	}
	var fields zoneReservedVNIFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	var out []int
	if fields.VrfVxlan != nil && *fields.VrfVxlan != 0 {
		out = append(out, int(*fields.VrfVxlan))
	}
	if fields.Tag != nil && *fields.Tag != 0 {
		out = append(out, int(*fields.Tag))
	}
	return out
}

// listUsedVNIs returns the set of VNIs that must not be handed out to a new
// vnet: every in-use vnet tag (pending vnets included, so tags on
// staged-but-unapplied vnets are never double-allocated — untagged vnets with
// Tag == 0 are skipped, since 0 is not a valid VNI) PLUS every zone-level
// control VNI (see zoneReservedVNIFields) found across all SDN zones
// (pending zones included, for the same staged-but-unapplied reason).
//
// The zone listing is fail-open: a failure there logs a single Warn (once per
// CPI process — see vniZoneListWarnOnce) and allocation proceeds using only
// the vnet-tag exclusion set, i.e. the behavior before zone-level exclusion
// existed. A zone-listing hiccup must never block VM/network creation.
// logger may be nil (no Warn is attempted in that case, but the fail-open
// behavior still applies).
func listUsedVNIs(ctx context.Context, c Client, logger *log.Logger) (map[int]struct{}, error) {
	vnets, err := ListSDNVnets(ctx, c)
	if err != nil {
		return nil, cpierrors.Wrap(err, "vni: list vnets")
	}
	used := make(map[int]struct{}, len(vnets))
	for _, v := range vnets {
		if v.Tag != 0 {
			used[int(v.Tag)] = struct{}{}
		}
	}

	zones, zerr := ListSDNZones(ctx, c)
	if zerr != nil {
		vniZoneListWarnOnce.Do(func() {
			if logger != nil {
				logger.Warn("vni: could not list SDN zones to exclude zone-level control VNIs "+
					"(e.g. an EVPN zone's vrf-vxlan) from allocation; proceeding with vnet-tag "+
					"exclusion only — a zone control VNI inside the allocation band could be "+
					"re-allocated to a vnet",
					log.Err(zerr))
			}
		})
		return used, nil
	}
	for _, z := range zones {
		for _, vni := range zoneReservedVNIs(z.Raw) {
			used[vni] = struct{}{}
		}
	}
	return used, nil
}

// nextVNIInRange picks a random start offset within [start, end] and scans the
// full range exactly once (wrapping at end back to start), returning the first
// VNI not present in used. Randomising the entry point scatters concurrent CPI
// processes across the band, mirroring nextVMIDInRange.
func nextVNIInRange(used map[int]struct{}, start, end int) (int, error) {
	if end < start {
		return 0, cpierrors.Cloud("no free VNI in range [%d, %d]: invalid range (end < start)",
			start, end)
	}
	width := end - start + 1
	randomOffset := mrand.IntN(width) // #nosec G404 -- VNI collision-avoidance offset; non-cryptographic
	for i := 0; i < width; i++ {
		candidate := start + (randomOffset+i)%width
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return 0, cpierrors.Cloud(
		"no free VNI in range [%d, %d]: all %d IDs exhausted — widen sdn_vni_range_start/sdn_vni_range_end "+
			"or set cloud_properties.vnet_tag explicitly",
		start, end, width)
}

// NextVNI returns a free vnet tag (VNI) within [start, end], consulting the
// cluster-wide vnet list AND every SDN zone's zone-level control VNI (e.g. an
// EVPN zone's vrf-vxlan) for values already in use — see listUsedVNIs. The
// vnet/zone lists are fetched outside the process-level mutex so a slow PVE
// API call does not block other goroutines; the mutex is held only around the
// in-memory scan+select.
//
// Inputs and failure modes:
//   - ctx nil / c nil → *cpierrors.Error before any SDK call.
//   - vnet list failure → wrapped *cpierrors.Error (fatal: the vnet exclusion
//     set is load-bearing for correctness, unlike the zone list below).
//   - zone list failure → fail-open; logs a Warn once per process and
//     proceeds with vnet-only exclusion (see listUsedVNIs).
//   - band exhausted → *cpierrors.Error naming both config keys and the
//     cloud_properties.vnet_tag escape hatch.
//
// logger may be nil; it is only used for the fail-open zone-list Warn.
func NextVNI(ctx context.Context, c Client, start, end int, logger *log.Logger) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextVNI: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextVNI: client must not be nil")
	}
	used, err := listUsedVNIs(ctx, c, logger)
	if err != nil {
		return 0, err
	}
	globalVNIMu.Lock()
	defer globalVNIMu.Unlock()
	return nextVNIInRange(used, start, end)
}
