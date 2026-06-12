package handlers

import (
	"sort"
	"strings"
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
// stale director/deployment/job triple cannot accumulate.
var reservedBoshTagPrefixes = []string{"director--", "deployment--", "job--", "index--"}

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
// emitted. maxBytes <= 0 disables truncation.
func mergeTagList(existing []string, additions []string, maxBytes int) string {
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
		return ""
	}
	joined := strings.Join(parts, ";")
	if maxBytes <= 0 || len(joined) <= maxBytes {
		return joined
	}
	var truncated string
	for i, p := range parts {
		candidate := p
		if i > 0 {
			candidate = truncated + ";" + p
		}
		if len(candidate) > maxBytes {
			break
		}
		truncated = candidate
	}
	return truncated
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
// reservedBoshTagPrefixes. Used so set_vm_metadata can rebuild the
// director/deployment/job triple from fresh metadata without leaving stale
// values from a prior sync.
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
