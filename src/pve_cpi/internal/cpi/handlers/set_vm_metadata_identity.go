package handlers

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// metadataKeyID is the Director's instance UUID key in set_vm_metadata. bosh
// create-env sends no such key, and its instance is named by the index that
// trails metadata["name"] instead.
const metadataKeyID = "id"

// instanceIdentityTagValue matches the trailing part of a "<job>--<id>" name
// tag that can hold an instance identity, which is a Director instance UUID or
// a create-env index. Anything else after a job prefix is an operator tag that
// happens to share the job's name, and we do not read it as evidence.
var instanceIdentityTagValue = regexp.MustCompile(
	`^(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9]+)$`)

// instanceUUIDValue matches a lowercased instance UUID, the shape a Director's
// instance identity always has.
var instanceUUIDValue = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// instanceIndexValue matches a create-env index, the shape bosh create-env's
// instance identity always has.
var instanceIndexValue = regexp.MustCompile(`^\d+$`)

// backfillExcludedTags mark VMs that are not instance VMs, namely stemcell
// templates and disk parkers. They carry ownershipTag and record no instance,
// and several Directors may share them, so set_vm_metadata never backfills an
// owner tag onto one.
var backfillExcludedTags = []string{stemcellMarkerTag, stemcellCacheTag, pve.ParkerTag}

// vmMetadataIdentity is what set_vm_metadata learns by comparing the instance
// the Director's metadata names with the instance the VM already records.
type vmMetadataIdentity struct {
	// incoming is the normalized identity the metadata names, or "" when the
	// metadata carries none and the comparison is skipped.
	incoming string
	// stored holds every normalized identity the VM records, sorted and
	// deduplicated. It is empty for a VM that has never had metadata set.
	stored []string
	// compared holds the values in stored that can be compared with
	// incoming. An index that only a tag records is compared only with an
	// incoming index, so a digit-valued operator tag cannot refuse a
	// Director's UUID. An index the VM's Notes record stays comparable.
	compared []string
	// uncomparable is true when the VM records identity evidence and none of
	// it can be compared with incoming. That covers tag-only index evidence
	// against a UUID, and a UUID-shaped "<x>--<uuid>" tag that no job record
	// ties to the VM's own name tag. Such a VM is neither refused nor
	// backfilled.
	uncomparable bool
	// orphans holds the normalized UUIDs of "<x>--<uuid>" tags that no job
	// record ties to the VM's own name tag.
	orphans []string
	// foreignOrphan is true when one of orphans differs from incoming, which
	// means the VM may carry another instance's name tag. It blocks the
	// owner tag backfill even when other evidence matched.
	foreignOrphan bool
	// matched is true when incoming is one of compared.
	matched bool
}

// checkVMMetadataIdentity compares the instance the Director's metadata names
// against the instance the VM records, and refuses the call when the two
// differ. A bare CID carries nothing that ties it to one VM, so after a
// cpi_config or AZ repoint it can reach a VM that belongs to a different
// instance, and renaming and retagging that VM is the write that does the
// damage. The refusal is non-retriable, because retrying cannot change what
// either side records.
//
// The comparison uses the instance UUID, or the create-env index, and never
// the job part of the name, because renaming an instance group changes the job
// and keeps the UUID. A VM matches when the incoming identity equals any value
// it records, so one stale or hand-edited record never refuses the VM's own
// instance. The check is skipped when the metadata names no instance at all.
//
// An index that only a tag records is evidence only against an incoming
// index, because an operator tag such as "web--7" looks like a create-env name
// tag, and a UUID request must not be refused over it. An index the Notes
// record, from a "name: <job>/<index>" or "id:" line, is written only by a
// metadata sync, so it stays comparable, and a Director's UUID that reaches a
// create-env VM is refused. A UUID value is compared with any incoming
// identity, so create-env metadata that reaches a Director's VM is refused too.
func checkVMMetadataIdentity(vmCID string, metadata map[string]any, existingTags []string,
	existingDesc string) (vmMetadataIdentity, error) {
	ev := storedInstanceEvidence(existingDesc, existingTags)
	id := vmMetadataIdentity{
		incoming: incomingInstanceIdentity(metadata),
		stored:   ev.values,
		orphans:  ev.orphans,
	}
	if id.incoming == "" {
		return id, nil
	}
	for _, o := range id.orphans {
		if o != id.incoming {
			id.foreignOrphan = true
		}
	}
	incomingIsIndex := instanceIndexValue.MatchString(id.incoming)
	for _, s := range id.stored {
		if ev.tagOnly[s] && instanceIndexValue.MatchString(s) && !incomingIsIndex {
			continue
		}
		id.compared = append(id.compared, s)
	}
	if len(id.compared) == 0 {
		id.uncomparable = len(id.stored) > 0 || len(id.orphans) > 0
		return id, nil
	}
	for _, s := range id.compared {
		if s == id.incoming {
			id.matched = true
			return id, nil
		}
	}
	return id, cpierrors.Cloud(
		"set_vm_metadata: VM %s records instance %s, but the Director's metadata names instance %s; "+
			"the VM was left unchanged because this CID may point at a different instance's VM, "+
			"for example after a cpi_config or AZ change",
		vmCID, strings.Join(id.compared, ","), id.incoming)
}

// ownerTagBackfill returns the owner tag set_vm_metadata should add to a VM,
// or "" when it should add none. Only a VM that predates owner tags gets one,
// meaning a VM that carries ownershipTag and no owner tag, and only on
// evidence that the requesting Director's instance owns it: either the stored
// identity matched, or the VM records no identity at all, which is the state
// create_vm leaves before the Director's first set_vm_metadata. A VM whose
// stored identity cannot be compared, either because this call's metadata
// names no instance or because the evidence has a shape the incoming identity
// cannot be compared with, keeps its tags as they are. So does a stemcell
// template or a disk parker, which is not an instance VM at all.
func ownerTagBackfill(id vmMetadataIdentity, existingTags []string, directorUUID string,
	logger *log.Logger) string {
	if !hasOwnershipTag(existingTags) || hasOwnerTag(existingTags) {
		return ""
	}
	if marker := backfillExcludedTag(existingTags); marker != "" {
		logger.Info("set_vm_metadata: the VM is tagged as a stemcell template or a disk parker, "+
			"so no owner tag is backfilled",
			log.String("tag", marker))
		return ""
	}
	owner := ownerTagFor(directorUUID)
	if owner == "" {
		logger.Debug("set_vm_metadata: the request carries no director_uuid, so no owner tag is backfilled")
		return ""
	}
	if (id.matched && !id.foreignOrphan) || (len(id.stored) == 0 && len(id.orphans) == 0) {
		return owner
	}
	logger.Info("set_vm_metadata: the VM's stored identity can't be compared with the instance the metadata "+
		"names, so no owner tag is backfilled",
		log.String("stored_instance", strings.Join(id.stored, ",")),
		log.String("metadata_instance", id.incoming))
	return ""
}

// backfillExcludedTag returns the first entry that marks a stemcell template
// or a disk parker, matched without regard to case, or "" when none does.
func backfillExcludedTag(entries []string) string {
	for _, e := range entries {
		for _, marker := range backfillExcludedTags {
			if strings.EqualFold(e, marker) {
				return e
			}
		}
	}
	return ""
}

// hasOwnershipTag reports whether entries carry the fixed ownershipTag.
func hasOwnershipTag(entries []string) bool {
	for _, e := range entries {
		if e == ownershipTag {
			return true
		}
	}
	return false
}

// withOwnerTag returns preserved with owner added and the head tags hoisted to
// the front, so the backfilled tag sits next to ownershipTag where truncation
// cannot reach it.
func withOwnerTag(preserved []string, owner string) []string {
	out := make([]string, 0, len(preserved)+1)
	out = append(out, preserved...)
	out = append(out, owner)
	return hoistHeadTags(out)
}

// incomingInstanceIdentity returns the normalized identity a set_vm_metadata
// payload names. That is metadata["id"] for a Director, and otherwise the
// part of metadata["name"] after its last "/", which is the instance UUID for
// a Director and the index for bosh create-env. It returns "" when the
// metadata names neither.
func incomingInstanceIdentity(metadata map[string]any) string {
	if v, ok := metadata[metadataKeyID]; ok && v != nil {
		if s := normalizeInstanceIdentity(fmt.Sprintf("%v", v)); s != "" {
			return s
		}
	}
	if v, ok := metadata[metadataKeyName]; ok && v != nil {
		return normalizeInstanceIdentity(trailingNameComponent(fmt.Sprintf("%v", v)))
	}
	return ""
}

// storedInstanceEvidence collects every instance identity a VM records. The
// description supplies the "id: <uuid>" line that buildDescription writes and
// the trailing part of its "name: <job>/<id>" line. The tags supply the
// trailing part of the "<job>--<id>" tag that buildBoshManagedTags writes,
// found through the job and instance group names the VM also records. The
// values are normalized, sorted, and deduplicated.
//
// orphans are the UUIDs of "<x>--<uuid>" tags outside the CPI-owned prefixes
// that no job record ties to the VM's name tag. One may be another instance's
// name tag whose job records were edited away or truncated, or that names a
// different job than the VM's job records, so it shows that the VM records an
// identity even though we can't read whose.
func storedInstanceEvidence(desc string, tags []string) instanceEvidence {
	fromNotes := make(map[string]bool)
	fromTags := make(map[string]bool)
	addNotes := func(s string) {
		if s = normalizeInstanceIdentity(s); s != "" {
			fromNotes[s] = true
		}
	}
	add := func(s string) {
		if s = normalizeInstanceIdentity(s); s != "" {
			fromTags[s] = true
		}
	}

	lines := descriptionMetadataLines(desc)
	for _, v := range lines[metadataKeyID] {
		addNotes(v)
	}
	for _, v := range lines[metadataKeyName] {
		addNotes(trailingNameComponent(v))
	}

	// The name tag is "<sanitized job>--<id>", and the job is whatever the
	// VM's job and instance group records say it is. Those come from the
	// same sync that wrote the name tag, so a renamed instance group still
	// finds its own older name tag.
	jobs := make(map[string]struct{})
	for _, v := range lines["job"] {
		jobs[sanitizeTagValue(v)] = struct{}{}
	}
	for _, v := range lines["instance_group"] {
		jobs[sanitizeTagValue(v)] = struct{}{}
	}
	for _, t := range tags {
		for _, p := range []string{"job--", "instance-group--"} {
			if strings.HasPrefix(t, p) {
				jobs[t[len(p):]] = struct{}{}
			}
		}
	}
	tiedToJob := make(map[string]bool)
	for job := range jobs {
		prefix := job + "--"
		// A job whose tag prefix the CPI owns, such as one named "director",
		// shares its prefix with a CPI tag, so its name tag is ambiguous.
		if job == "" || hasCPIOwnedPrefix(prefix) {
			continue
		}
		for _, t := range tags {
			if v, ok := strings.CutPrefix(t, prefix); ok && instanceIdentityTagValue.MatchString(strings.ToLower(v)) {
				add(v)
				tiedToJob[t] = true
			}
		}
	}
	var ev instanceEvidence
	orphans := make(map[string]bool)
	for _, t := range tags {
		if tiedToJob[t] || hasCPIOwnedPrefix(t) {
			continue
		}
		if i := strings.LastIndex(t, "--"); i > 0 && instanceUUIDValue.MatchString(strings.ToLower(t[i+2:])) {
			orphans[strings.ToLower(t[i+2:])] = true
		}
	}
	ev.orphans = sortedKeys(orphans)

	all := make(map[string]bool, len(fromNotes)+len(fromTags))
	for s := range fromNotes {
		all[s] = true
	}
	for s := range fromTags {
		all[s] = true
		if !fromNotes[s] {
			if ev.tagOnly == nil {
				ev.tagOnly = make(map[string]bool)
			}
			ev.tagOnly[s] = true
		}
	}
	ev.values = sortedKeys(all)
	return ev
}

// instanceEvidence is the identity evidence a VM records, as
// storedInstanceEvidence reads it.
type instanceEvidence struct {
	// values holds every normalized identity, sorted and deduplicated, or nil.
	values []string
	// tagOnly marks the values that only a tag records, with no matching
	// line in the Notes.
	tagOnly map[string]bool
	// orphans holds the normalized UUIDs of untied "<x>--<uuid>" tags, or nil.
	orphans []string
}

// sortedKeys returns the keys of set in sorted order, or nil when it is empty.
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// descriptionMetadataLines reads the "key: value" lines buildDescription
// writes, and the legacy Perl CPI wrote the same way, for the keys the
// identity check needs. A key may repeat, so each holds every value found.
// Lines that are not in that form, such as the sentinel and the allocation
// marker, are skipped.
func descriptionMetadataLines(desc string) map[string][]string {
	out := make(map[string][]string)
	for _, line := range strings.Split(desc, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
		if !ok {
			continue
		}
		switch key {
		case metadataKeyID, metadataKeyName, "job", "instance_group":
			if value = strings.TrimSpace(value); value != "" {
				out[key] = append(out[key], value)
			}
		}
	}
	return out
}

// trailingNameComponent returns the part of a BOSH instance name after its
// last "/", or "" when the name has no "/" and so is not "<job>/<id>".
func trailingNameComponent(name string) string {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(name[i+1:])
}

// normalizeInstanceIdentity puts an identity into the form a tag stores it in,
// lowercased, so a description value and a tag value compare equal.
func normalizeInstanceIdentity(s string) string {
	return strings.ToLower(sanitizeTagValue(strings.TrimSpace(s)))
}
