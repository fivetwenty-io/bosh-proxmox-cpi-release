package handlers

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// stemcellSHA8Value matches an sha8 as create_stemcell writes it: the first
// eight hex digits of the stemcell image's sha256.
var stemcellSHA8Value = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)

// provenanceStringKeys are the description keys every CPI generation that
// wrote provenance JSON records as strings, with or without a value. The
// stemcellProvenance struct has never marked them omitempty, so a
// description that lacks one was not written by create_stemcell.
var provenanceStringKeys = []string{metadataKeyName, jsonKeyVersion, "sha8", "created"}

// legacyRefsKey is the retired ref-count key that a CPI writing minimal notes
// (provenance off, before path-identity stemcells) always set to the
// template's own stemcell CID, "template:<vmid>".
const legacyRefsKey = "stemcell_refs"

// provenanceKindKey and provenanceCIDKey are the path-identity description
// keys that only a CPI appending the sha8 to template names writes.
const (
	provenanceKindKey = "kind"
	provenanceCIDKey  = "cid"
)

// untrackedIdentityBlock returns why untracked template t is not proven to be
// a stemcell cache template that create_stemcell wrote, or "". The proof
// needs both signals:
//
//   - The name follows stemcell cache naming, bosh-stemcell-<name>-<version>
//     with or without a -<sha8> suffix.
//   - The description is provenance that create_stemcell wrote, and it
//     yields one sha8 that every other sha8 signal agrees with.
//
// Every CPI that wrote provenance also tagged the template
// bosh-stemcell-sha-<sha8> and took the description's sha8 from that tag, so
// the tag is required, and every such tag must name the same sha8.
//
// Two provenance forms qualify. Full provenance records an sha8, which must
// equal the tag's, together with a stemcell name and version, and the
// template's name must be the one create_stemcell builds from them: with the
// sha8 suffix, or without it for provenance that predates path identity,
// which carries no kind or CID. Minimal notes, which a CPI with provenance
// turned off wrote before path-identity stemcells, hold exactly name,
// version, sha8, and created, all empty, and stemcell_refs, whose every
// entry is the template's own stemcell CID, template:<vmid>. They record no
// name or version to rebuild the name from, so the name's last segment must
// equal the tag's sha8 or not look like one.
func untrackedIdentityBlock(t *StemcellRefsTemplate) string {
	if !legacyStemcellTemplateName.MatchString(t.Name) {
		return fmt.Sprintf("its name %q does not follow stemcell cache naming (bosh-stemcell-<name>-<version>), "+
			"so --untracked does not treat it as a stemcell cache template", t.Name)
	}
	p, reason := untrackedProvenanceOf(t.description, t.VMID)
	if reason != "" {
		return reason
	}
	tagSHA, reason := untrackedTagSHA8(t.Tags)
	if reason != "" {
		return reason
	}
	if tagSHA == "" {
		return "it carries no " + stemcellSHATagPrefix + " tag, so nothing corroborates the sha8 of its " +
			"provenance; every CPI that wrote provenance also wrote that tag"
	}
	if p.form == untrackedProvenanceMinimal {
		return untrackedNameSuffixBlock(t.Name, tagSHA)
	}
	if !strings.EqualFold(p.fields["sha8"], tagSHA) {
		return fmt.Sprintf("its description sha8 %s does not match its %s%s tag, so its identity is "+
			"contradictory", p.fields["sha8"], stemcellSHATagPrefix, tagSHA)
	}
	return untrackedFullNameBlock(t.Name, tagSHA, p.fields)
}

// untrackedProvenanceForm is the shape of provenance an untracked template's
// description holds.
type untrackedProvenanceForm int

const (
	// untrackedProvenanceFull records sha8, name, and version.
	untrackedProvenanceFull untrackedProvenanceForm = iota + 1
	// untrackedProvenanceMinimal is an older CPI's minimal notes.
	untrackedProvenanceMinimal
)

// minimalNotesKeys are the only keys an older CPI's minimal notes hold.
var minimalNotesKeys = []string{metadataKeyName, jsonKeyVersion, "sha8", "created", legacyRefsKey}

// untrackedProvenance is what an untracked template's description holds:
// its form, its string fields (trimmed), which also carry kind and cid, and,
// for minimal notes, the entries of stemcell_refs.
type untrackedProvenance struct {
	form       untrackedProvenanceForm
	fields     map[string]string
	legacyRefs []string
}

// untrackedProvenanceOf parses desc, the description of template vmid, as
// provenance, or returns the reason it is not provenance that
// create_stemcell wrote.
func untrackedProvenanceOf(desc string, vmid int64) (untrackedProvenance, string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(desc), &raw); err != nil {
		return untrackedProvenance{}, "its description is not a JSON object, so it is not stemcell provenance"
	}
	fields := map[string]string{}
	var missing []string
	for _, k := range provenanceStringKeys {
		var s string
		v, ok := raw[k]
		if !ok || json.Unmarshal(v, &s) != nil {
			missing = append(missing, k)
			continue
		}
		fields[k] = strings.TrimSpace(s)
	}
	if len(missing) > 0 {
		return untrackedProvenance{}, fmt.Sprintf("its description is not stemcell provenance: it lacks the "+
			"string field(s) %s that every create_stemcell provenance write records", strings.Join(missing, ", "))
	}
	for _, k := range []string{provenanceKindKey, provenanceCIDKey} {
		var s string
		if v, ok := raw[k]; ok && json.Unmarshal(v, &s) == nil {
			fields[k] = strings.TrimSpace(s)
		}
	}
	switch {
	case fields["sha8"] != "":
		if fields[metadataKeyName] == "" || fields[jsonKeyVersion] == "" {
			return untrackedProvenance{}, "its description records an sha8 but no stemcell name or version, " +
				"so it is not provenance create_stemcell wrote"
		}
		if !stemcellSHA8Value.MatchString(fields["sha8"]) {
			return untrackedProvenance{}, fmt.Sprintf("its description sha8 %q is not eight hex digits, so it "+
				"is not provenance create_stemcell wrote", fields["sha8"])
		}
		return untrackedProvenance{form: untrackedProvenanceFull, fields: fields}, ""
	case fields[metadataKeyName] == "" && fields[jsonKeyVersion] == "" && fields["created"] == "":
		refs, reason := minimalNotesRefs(raw, vmid)
		if reason != "" {
			return untrackedProvenance{}, reason
		}
		return untrackedProvenance{form: untrackedProvenanceMinimal, fields: fields, legacyRefs: refs}, ""
	}
	return untrackedProvenance{}, "its description provenance records no sha8 and is not the minimal notes of " +
		"an older CPI, so nothing in it names its stemcell"
}

// minimalNotesRefs holds raw, a description whose provenance string fields
// are all empty, to the minimal notes an older CPI wrote on template vmid,
// and returns the entries of its stemcell_refs, or the reason it is not
// those notes. The notes hold exactly minimalNotesKeys, and every
// stemcell_refs entry is template:<vmid>, the only value that CPI wrote
// there; an empty stemcell_refs is accepted.
func minimalNotesRefs(raw map[string]json.RawMessage, vmid int64) ([]string, string) {
	const notMinimal = "its description records no sha8, name, or version, so it qualifies only as the minimal " +
		"notes of an older CPI, but "
	var extra []string
	for k := range raw {
		if !slices.Contains(minimalNotesKeys, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Sprintf(notMinimal+"it has the extra field(s) %s, which those notes never carry",
			strings.Join(extra, ", "))
	}
	v, ok := raw[legacyRefsKey]
	if !ok {
		return nil, notMinimal + "it lacks the " + legacyRefsKey + " field that those notes always carry"
	}
	var csv string
	if json.Unmarshal(v, &csv) != nil {
		return nil, notMinimal + "its " + legacyRefsKey + " field is not a string"
	}
	own := fmt.Sprintf("template:%d", vmid)
	var refs []string
	for entry := range strings.SplitSeq(csv, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry != own {
			return nil, fmt.Sprintf(notMinimal+"its %s entry %q is not %s, the template's own stemcell CID, "+
				"which is the only value those notes record", legacyRefsKey, entry, own)
		}
		refs = append(refs, entry)
	}
	return refs, ""
}

// untrackedTagSHA8 returns the sha8 that tags' bosh-stemcell-sha- tags name,
// "" when there is none, or the reason those tags contradict each other or
// do not hold an sha8.
func untrackedTagSHA8(tags []string) (string, string) {
	seen := map[string]bool{}
	for _, tok := range tags {
		v, ok := strings.CutPrefix(tok, stemcellSHATagPrefix)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(v))] = true
	}
	if len(seen) == 0 {
		return "", ""
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	if len(values) > 1 {
		return "", fmt.Sprintf("it carries conflicting %s tags (%s), so its identity is contradictory",
			stemcellSHATagPrefix, strings.Join(values, ", "))
	}
	if !stemcellSHA8Value.MatchString(values[0]) {
		return "", fmt.Sprintf("its %s%s tag does not hold eight hex digits, so it names no stemcell",
			stemcellSHATagPrefix, values[0])
	}
	return values[0], ""
}

// untrackedFullNameBlock returns why name is not the template name
// create_stemcell builds from full provenance fields with sha8, or "". The
// name carries the sha8 suffix, or, for provenance with no path-identity
// kind or CID, which predates the suffix, it may lack it.
func untrackedFullNameBlock(name, sha8 string, fields map[string]string) string {
	stemcell, version := fields[metadataKeyName], fields[jsonKeyVersion]
	withSHA := pve.BuildTemplateNameWithSHA(stemcell, version, sha8)
	plain := pve.BuildTemplateName(stemcell, version)
	pathIdentity := fields[provenanceKindKey] != "" || fields[provenanceCIDKey] != ""
	switch {
	case name == withSHA:
		return ""
	case name == plain && !pathIdentity:
		return ""
	case name == plain:
		return fmt.Sprintf("its name %q has no sha8 suffix, but its provenance records a path-identity kind or "+
			"CID, which only a CPI that names the template %q writes", name, withSHA)
	}
	if reason := untrackedNameSuffixBlock(name, sha8); reason != "" {
		return reason
	}
	want := fmt.Sprintf("%q", withSHA)
	if !pathIdentity {
		want += fmt.Sprintf(" or %q", plain)
	}
	return fmt.Sprintf("its name %q does not match its provenance, stemcell %s version %s, for which "+
		"create_stemcell builds the name %s", name, stemcell, version, want)
}

// untrackedNameSuffixBlock returns why name's last segment contradicts sha8,
// or "": a last segment of eight hex digits must be sha8.
func untrackedNameSuffixBlock(name, sha8 string) string {
	suffix := name[strings.LastIndexByte(name, '-')+1:]
	if stemcellSHA8Value.MatchString(suffix) && !strings.EqualFold(suffix, sha8) {
		return fmt.Sprintf("its name suffix %s does not match sha8 %s, so its identity is contradictory",
			suffix, sha8)
	}
	return ""
}

// untrackedLegacyRefs returns the stemcell_refs entries of untracked
// template t's minimal notes, or nil when its description is not those notes.
func untrackedLegacyRefs(t *StemcellRefsTemplate) []string {
	p, reason := untrackedProvenanceOf(t.description, t.VMID)
	if reason != "" || p.form != untrackedProvenanceMinimal {
		return nil
	}
	return p.legacyRefs
}
