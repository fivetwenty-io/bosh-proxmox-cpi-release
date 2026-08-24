package stemcellfetch

import (
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// BuildFetchedFilename returns the canonical qcow2 filename for a CPI-fetched
// light stemcell. Delegates to pve.BuildStemcellFilename so the dedup scan
// against normal-upload volumes stays consistent.
//
// sha256hex must be at least 8 hex characters; the first 8 are embedded in the
// returned filename. Shorter or empty values produce the "00000000" placeholder.
func BuildFetchedFilename(name, version, sha256hex string) string {
	return pve.BuildStemcellFilename(name, version, sha256hex)
}

// FilenamePrefixForDedup returns the prefix "bosh-stemcell-<name>-<version>-"
// used by FindStemcellByFilename when scanning for any sha8 variant of the
// same (name, version) pair. Useful to short-circuit on an existing dedup hit
// before computing SHA-256.
//
// Sanitization is delegated to pve.BuildStemcellFilename via a placeholder sha8
// so the returned prefix never drifts from the canonical sanitizer.
//
// The placeholder suffix removed is "-00000000.qcow2" (15 chars). If the
// sanitized name+version collapses to empty the function returns whatever the
// builder produced (a degenerate prefix still safe to scan for).
func FilenamePrefixForDedup(name, version string) string {
	const placeholder = "00000000"
	// Drop "00000000.qcow2" (14 chars) to leave the trailing "-" that
	// separates the version field from the sha8 field in the canonical name.
	const dropSuffix = "00000000.qcow2" // 14 chars

	full := pve.BuildStemcellFilename(name, version, placeholder)
	if len(full) <= len(dropSuffix) {
		// Degenerate: sanitized name+version collapsed to nearly nothing;
		// return whatever was built — caller's prefix scan will be broader
		// than ideal but not incorrect.
		return full
	}
	return full[:len(full)-len(dropSuffix)]
}
