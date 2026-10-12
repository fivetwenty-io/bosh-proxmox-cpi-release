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
var createTimeTagPrefixes = []string{stemcellTagPrefix, ownerTagPrefix, cidStampTagPrefix, clusterStampTagPrefix}

// ownerTagPrefix names the Director that created a VM. create_vm stamps
// "bosh-owner--<sanitized director_uuid>" right after ownershipTag, and
// set_vm_metadata stamps it on a VM that predates the tag once the VM's stored
// instance identity agrees with the Director's metadata. We cannot reuse the
// "director--" tag for this, because set_vm_metadata strips and rebuilds that
// prefix from the Director's name on every sync.
const ownerTagPrefix = "bosh-owner--"

// cidStampTagPrefix and clusterStampTagPrefix are reserved for the tags that
// tie a VM to the CID it was returned under and to the cluster that holds it.
// No operator tag, cloud property, or instance name may write either one, so
// they sit in createTimeTagPrefixes ahead of the code that stamps them.
const (
	cidStampTagPrefix     = "bosh-cid--"
	clusterStampTagPrefix = "bosh-cluster--"
)

// headTagPrefixes are the create-time tags that ownership decisions read. We
// keep them at the head of a VM's tag list, right behind ownershipTag, because
// mergeTagList truncates from the tail and an ownership record that falls off
// a long list cannot be told apart from one that was never written.
var headTagPrefixes = []string{ownerTagPrefix, cidStampTagPrefix, clusterStampTagPrefix}

// ownerTagFor renders the owner tag for a Director UUID, lowercased so every
// writer and reader sees one canonical form whatever case the request used. It
// returns "" when the UUID is empty or sanitizes to nothing, and callers then
// stamp no owner tag.
func ownerTagFor(directorUUID string) string {
	s := strings.ToLower(sanitizeTagValue(directorUUID))
	if s == "" {
		return ""
	}
	return ownerTagPrefix + s
}

// hasOwnerTag reports whether entries already carry an owner tag of any value.
// The prefix comparison ignores case, the same way hasCPIOwnedPrefix does.
func hasOwnerTag(entries []string) bool {
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e), ownerTagPrefix) {
			return true
		}
	}
	return false
}

// hasCreateTimePrefix reports whether entry claims one of the
// createTimeTagPrefixes, ignoring case. It is the narrower half of
// hasCPIOwnedPrefix, for callers that write a reserved key themselves and only
// need to keep the create-time namespace out of their output.
func hasCreateTimePrefix(entry string) bool {
	lowered := strings.ToLower(entry)
	for _, p := range createTimeTagPrefixes {
		if strings.HasPrefix(lowered, p) {
			return true
		}
	}
	return false
}

// hoistHeadTags returns entries with ownershipTag first and every
// headTagPrefixes entry right behind it, in their original order, followed by
// everything else in its original order. PVE may store tags sorted, so the
// order we read back is not the order create_vm wrote, and this puts the
// ownership record back where truncation cannot reach it.
func hoistHeadTags(entries []string) []string {
	if len(entries) == 0 {
		return entries
	}
	head := make([]string, 0, 4)
	stamps := make([]string, 0, 4)
	rest := make([]string, 0, len(entries))
	for _, e := range entries {
		switch {
		case e == ownershipTag:
			head = append(head, e)
		case hasHeadTagPrefix(e):
			stamps = append(stamps, e)
		default:
			rest = append(rest, e)
		}
	}
	out := make([]string, 0, len(entries))
	out = append(out, head...)
	out = append(out, stamps...)
	return append(out, rest...)
}

func hasHeadTagPrefix(entry string) bool {
	lowered := strings.ToLower(entry)
	for _, p := range headTagPrefixes {
		if strings.HasPrefix(lowered, p) {
			return true
		}
	}
	return false
}

// hasCPIOwnedPrefix reports whether entry claims a prefix the CPI owns. The
// inherited reservedBoshTagPrefixes are matched exactly, which is the comparison
// they have always had. The create-time prefixes are matched without regard to
// case, because an operator key of "Stemcell" is plainly an attempt at the same
// namespace and there is no reason to let a shift key through a guard.
func hasCPIOwnedPrefix(entry string) bool {
	return hasReservedBoshPrefix(entry) || hasCreateTimePrefix(entry)
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
// that sanitize to the empty string are skipped, and so is any key whose entry
// would claim a CPI-owned prefix. A caller that has to tell the operator about
// a refused key should call buildCustomTagsReporting instead.
func buildCustomTags(custom map[string]string) []string {
	entries, _ := buildCustomTagsReporting(custom)
	return entries
}

// buildCustomTagsReporting builds the entries exactly the way buildCustomTags
// does, and it also hands back the original keys it refused because their
// entry would claim a CPI-owned prefix, sorted. Without the filter a cloud
// property tag of {bosh-owner: x} would render "bosh-owner--x" and name a
// Director that never created the VM, and {stemcell: x} would overwrite the
// record of which stemcell the guest booted from.
func buildCustomTagsReporting(custom map[string]string) ([]string, []string) {
	if len(custom) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(custom))
	for k := range custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(custom))
	var refused []string
	for _, k := range keys {
		sk := sanitizeTagValue(k)
		sv := sanitizeTagValue(custom[k])
		if sk == "" || sv == "" {
			continue
		}
		entry := sk + "--" + sv
		if hasCPIOwnedPrefix(entry) {
			refused = append(refused, k)
			continue
		}
		out = append(out, entry)
	}
	return out, refused
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
