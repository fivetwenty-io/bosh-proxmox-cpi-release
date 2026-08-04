// Generation eligibility for stemcell cache templates.
//
// The "bosh-stemcell-sha-<sha8>" tag is a CONTENT identity, not an ownership
// claim: any CPI generation that ever cached a stemcell on this cluster wrote
// one, and two generations caching the same BOSH stemcell write the identical
// tag. Matching on the sha tag alone therefore makes this CPI adopt a template
// built by a PREVIOUS CPI generation — register a director ref against it and,
// because that generation registered none of its own, destroy it on the first
// delete_stemcell, breaking the older director that still clones from it.
//
// Eligibility is settled by the two markers only this generation writes:
//
//   - stemcellCacheTagValue ("bosh-stemcell-cache") — stamped unconditionally
//     on every cache template (and replica) this CPI builds, so it identifies
//     a template built by this generation.
//   - directorRefTagPrefix ("director--<uuid>") — stamped when a director
//     registers a reference, so it identifies a template this generation has
//     already adopted, whatever built it. Keeping this arm visible is what
//     lets an already-adopted template stay refcountable and eventually
//     destroyable rather than becoming an unreachable leak.
//
// A template carrying neither marker is invisible to every sha8-keyed lookup
// and sweep: never adopted, never swept, never destroyed. The CPI builds its
// own cache alongside it, at the cost of one duplicated template per stemcell
// on a cluster mid-upgrade.
package pve

import "strings"

const (
	// stemcellCacheTagValue is the cache marker this CPI stamps on every
	// stemcell cache template it builds. Mirrors the handlers package's
	// stemcellCacheTag, duplicated here because internal/pve must not import
	// internal/cpi/handlers (the dependency runs the other way).
	stemcellCacheTagValue = "bosh-stemcell-cache"

	// directorRefTagPrefix is the prefix of the per-director reference tag
	// ("director--<sanitized uuid>") written by registerStemcellDirectorRef.
	directorRefTagPrefix = "director--"
)

// HasStemcellGenerationMarker reports whether the given PVE tag tokens carry
// at least one marker proving the template belongs to this CPI generation:
// the cache tag, or a non-empty director reference tag. See the package
// comment above for why sha-tag identity alone is not sufficient.
//
// Exported for cmd/pve-cid, whose stemcell inventory must classify the same
// templates this package's lookups filter on. The operator tool deliberately
// LISTS previous-generation templates that the CPI ignores — an inventory
// exists to surface leftovers — so it needs the predicate as a label rather
// than only as a filter. Sharing this one implementation keeps the tool's
// notion of "this generation" from drifting away from the CPI's.
func HasStemcellGenerationMarker(tokens []string) bool {
	for _, tok := range tokens {
		if tok == stemcellCacheTagValue {
			return true
		}
		if strings.HasPrefix(tok, directorRefTagPrefix) && len(tok) > len(directorRefTagPrefix) {
			return true
		}
	}
	return false
}
