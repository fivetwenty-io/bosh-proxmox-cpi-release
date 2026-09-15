package handlers

import (
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ownershipTag is the fixed CPI ownership marker stamped on every VM and
// stemcell template created by this CPI. Operators can filter by this tag in
// the PVE UI and scripts to distinguish CPI-managed guests from hand-made
// ones. The tag is NOT in reservedBoshTagPrefixes, so set_vm_metadata
// preserves it across every metadata update.
const ownershipTag = "bosh-cpi"

// reservedBoshTagPrefixes are the tag key prefixes the CPI owns and rewrites
// on every set_vm_metadata call. Entries with these prefixes are stripped
// from a stored PVE tag list before BOSH-managed values are re-applied, so a
// stale value cannot accumulate beside the current one. The set is open, and a
// key joins it whenever the CPI starts rebuilding that key on every sync, so
// please read the slice rather than any prose that enumerates it.
var reservedBoshTagPrefixes = []string{
	"director--",
	"deployment--",
	"instance-group--",
	"job--",
	"index--",
	// The identity tag that names the prefix a guest belongs to. The pve
	// package owns the literal, because it stamps the same tag on parker and
	// mover VMs, and an operator filtering the PVE UI on "vm-prefix--blue"
	// expects the workload VMs of that bloc and the parkers holding their
	// detached disks in one listing.
	pve.ParkerPrefixTagPrefix,
}

// createTimeTagPrefixes are the CPI-owned tag prefixes written once at create
// time and preserved from then on, as opposed to the reservedBoshTagPrefixes
// that set_vm_metadata rebuilds from the Director's metadata on every sync.
//
// The two lists answer different questions. reservedBoshTagPrefixes decides what
// set_vm_metadata strips before re-applying that metadata, so a key written once
// at create time has to stay out of it or the first sync would delete it and put
// nothing back. This list covers the other half of ownership: what an
// operator-supplied tag may not claim. A disk tag keyed "stemcell" would
// otherwise render "stemcell--<value>" and overwrite the record of which
// stemcell the guest actually booted from.
//
// Every entry is lowercase, because hasCPIOwnedPrefix lowercases before it
// compares.
var createTimeTagPrefixes = []string{stemcellTagPrefix}

// hasCPIOwnedPrefix reports whether entry claims a prefix the CPI owns. The
// inherited reservedBoshTagPrefixes are matched exactly, which is the comparison
// they have always had. The create-time prefixes are matched without regard to
// case, because an operator key of "Stemcell" is plainly an attempt at the same
// namespace and there is no reason to let a shift key through a guard.
func hasCPIOwnedPrefix(entry string) bool {
	if hasReservedBoshPrefix(entry) {
		return true
	}
	lowered := strings.ToLower(entry)
	for _, p := range createTimeTagPrefixes {
		if strings.HasPrefix(lowered, p) {
			return true
		}
	}
	return false
}

// jsonKeyTags is the PVE "tags" field key in qemu config/create payloads and
// cluster-resource rows, named once so the literal stays under the goconst
// occurrence cap.
const jsonKeyTags = "tags"

// resourceTypeQemu is the /cluster/resources row type for QEMU guests.
const resourceTypeQemu = "qemu"

// sanitizeTagValue replaces any byte outside [A-Za-z0-9-] with "-" so the
// result is a valid PVE tag value. Leading/trailing "-" are trimmed.
func sanitizeTagValue(s string) string {
	if s == "" {
		return ""
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-':
			b[i] = c
		default:
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-")
}

// buildCustomTags converts a user-supplied tag map to sanitized "key--value"
// entries, sorted by key for deterministic ordering. Empty values and keys
// that sanitize to the empty string are skipped.
func buildCustomTags(custom map[string]string) []string {
	if len(custom) == 0 {
		return nil
	}
	keys := make([]string, 0, len(custom))
	for k := range custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(custom))
	for _, k := range keys {
		sk := sanitizeTagValue(k)
		sv := sanitizeTagValue(custom[k])
		if sk == "" || sv == "" {
			continue
		}
		out = append(out, sk+"--"+sv)
	}
	return out
}

// mergeTagList concatenates existing and additions into a single PVE tag
// string joined by ";". Duplicate entries (exact string match) are dropped,
// preserving the first occurrence. The result is truncated at a tag boundary
// so the total byte length never exceeds maxBytes; partial entries are never
// emitted. maxBytes <= 0 disables truncation. A caller that wants to know
// which entries the cap left out should call mergeTagListReporting instead.
func mergeTagList(existing []string, additions []string, maxBytes int) string {
	merged, _ := mergeTagListReporting(existing, additions, maxBytes)
	return merged
}

// mergeTagListReporting merges the two lists exactly the way mergeTagList
// does, and it also hands back the entries the byte cap left out. The second
// return value holds those entries in the order we dropped them, already
// deduplicated, and it is nil when everything fits. We report them because we
// write the identity tag last, so it is the first entry to fall off a full
// list, and a guest that quietly loses that tag no longer names the parkers
// holding its detached disks.
func mergeTagListReporting(existing []string, additions []string, maxBytes int) (string, []string) {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	parts := make([]string, 0, len(existing)+len(additions))
	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		parts = append(parts, p)
	}
	for _, p := range existing {
		add(p)
	}
	for _, p := range additions {
		add(p)
	}
	if len(parts) == 0 {
		return "", nil
	}
	joined := strings.Join(parts, ";")
	if maxBytes <= 0 || len(joined) <= maxBytes {
		return joined, nil
	}
	var truncated string
	for i, p := range parts {
		candidate := p
		if i > 0 {
			candidate = truncated + ";" + p
		}
		if len(candidate) > maxBytes {
			// The first entry that does not fit ends the list, so every
			// entry from here on goes with it and we name them all.
			dropped := make([]string, len(parts)-i)
			copy(dropped, parts[i:])
			return truncated, dropped
		}
		truncated = candidate
	}
	return truncated, nil
}

// parseTagsField splits a stored PVE tags string back into entries. PVE
// accepts both ";" and "," as separators in the stored value; both are
// honored. Empty entries are dropped.
func parseTagsField(s string) []string {
	if s == "" {
		return nil
	}
	// Normalise comma separator to semicolon so a config that round-tripped
	// through PVE's parser is handled consistently.
	s = strings.ReplaceAll(s, ",", ";")
	raw := strings.Split(s, ";")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// stripReservedBoshTags drops entries whose prefix matches any of
// reservedBoshTagPrefixes. Used so set_vm_metadata can rebuild every
// CPI-owned key from fresh inputs without leaving a stale value from a prior
// sync beside the new one. The keys are the BOSH metadata keys plus the
// vm-prefix-- identity tag, and reservedBoshTagPrefixes is the list that
// decides.
func stripReservedBoshTags(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if hasReservedBoshPrefix(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func hasReservedBoshPrefix(entry string) bool {
	for _, p := range reservedBoshTagPrefixes {
		if strings.HasPrefix(entry, p) {
			return true
		}
	}
	return false
}

// maxPVEVMNameLength is the maximum byte length accepted by PVE for a VM's
// "name" config field. The PVE schema documents the field as a DNS name
// (RFC 1035 single label), which caps total length at 63 octets.
const maxPVEVMNameLength = 63

// sanitizeVMName converts a BOSH instance name (e.g. "diego-cell/2844c990-...")
// into a PVE-compatible VM name. PVE's name field is a DNS label
// ([A-Za-z0-9-], must start/end with alphanumeric, ≤ 63 bytes). Every byte
// outside [A-Za-z0-9] is rewritten to "-", consecutive dashes are collapsed,
// and leading/trailing dashes are trimmed. If the result would exceed 63
// bytes it is truncated to 63 and re-trimmed. Returns "" if the input
// collapses to an empty/invalid label.
func sanitizeVMName(s string) string {
	if s == "" {
		return ""
	}
	b := make([]byte, 0, len(s))
	prevDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlnum := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
		switch {
		case isAlnum:
			b = append(b, c)
			prevDash = false
		case c == '-':
			if !prevDash {
				b = append(b, '-')
				prevDash = true
			}
		default:
			if !prevDash {
				b = append(b, '-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(string(b), "-")
	if len(out) > maxPVEVMNameLength {
		out = strings.TrimRight(out[:maxPVEVMNameLength], "-")
	}
	return out
}
