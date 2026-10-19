package handlers

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
// Two provenance forms carry an sha8. Full provenance records it in its sha8
// field, together with a name and a version. Minimal notes, which a CPI with
// provenance turned off wrote before path-identity stemcells, leave name,
// version, sha8, and created empty and set stemcell_refs; that CPI recorded
// the sha8 only in the bosh-stemcell-sha- tag, so the tag supplies it. Every
// bosh-stemcell-sha- tag must name the same sha8, and full provenance's sha8
// must equal it. The name's last segment must equal the sha8, except on a
// template built before create_stemcell appended the sha8 to the name: that
// name ends in its version, so its last segment is accepted when it is not
// eight hex digits and the provenance carries no path-identity kind or CID,
// which only the suffixed generation writes.
func untrackedIdentityBlock(t *StemcellRefsTemplate) string {
	if !legacyStemcellTemplateName.MatchString(t.Name) {
		return fmt.Sprintf("its name %q does not follow stemcell cache naming (bosh-stemcell-<name>-<version>), "+
			"so --untracked does not treat it as a stemcell cache template", t.Name)
	}
	fields, form, reason := untrackedProvenanceFields(t.description)
	if reason != "" {
		return reason
	}
	tagSHA, reason := untrackedTagSHA8(t.Tags)
	if reason != "" {
		return reason
	}
	sha8 := fields["sha8"]
	switch form {
	case untrackedProvenanceFull:
		if tagSHA != "" && !strings.EqualFold(tagSHA, sha8) {
			return fmt.Sprintf("its description sha8 %s does not match its %s%s tag, so its identity is "+
				"contradictory", sha8, stemcellSHATagPrefix, tagSHA)
		}
	case untrackedProvenanceMinimal:
		if tagSHA == "" {
			return "its description is the minimal notes of an older CPI, which record no sha8, and it carries no " +
				stemcellSHATagPrefix + " tag, so nothing names its stemcell"
		}
		sha8 = tagSHA
	}
	return untrackedNameSuffixBlock(t.Name, sha8, fields)
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

// untrackedProvenanceFields parses desc as provenance and returns its string
// fields (trimmed) and its form, or the reason it is not provenance that
// create_stemcell wrote. The parsed fields also carry kind and cid.
func untrackedProvenanceFields(desc string) (map[string]string, untrackedProvenanceForm, string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(desc), &raw); err != nil {
		return nil, 0, "its description is not a JSON object, so it is not stemcell provenance"
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
		return nil, 0, fmt.Sprintf("its description is not stemcell provenance: it lacks the string field(s) %s "+
			"that every create_stemcell provenance write records", strings.Join(missing, ", "))
	}
	for _, k := range []string{provenanceKindKey, provenanceCIDKey, legacyRefsKey} {
		var s string
		if v, ok := raw[k]; ok && json.Unmarshal(v, &s) == nil {
			fields[k] = strings.TrimSpace(s)
		}
	}
	switch {
	case fields["sha8"] != "":
		if fields[metadataKeyName] == "" || fields[jsonKeyVersion] == "" {
			return nil, 0, "its description records an sha8 but no stemcell name or version, so it is not " +
				"provenance create_stemcell wrote"
		}
		if !stemcellSHA8Value.MatchString(fields["sha8"]) {
			return nil, 0, fmt.Sprintf("its description sha8 %q is not eight hex digits, so it is not provenance "+
				"create_stemcell wrote", fields["sha8"])
		}
		return fields, untrackedProvenanceFull, ""
	case fields[metadataKeyName] == "" && fields[jsonKeyVersion] == "" && fields["created"] == "" &&
		fields[legacyRefsKey] != "":
		return fields, untrackedProvenanceMinimal, ""
	}
	return nil, 0, "its description provenance records no sha8 and is not the minimal notes of an older CPI, " +
		"so nothing in it names its stemcell"
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

// untrackedNameSuffixBlock returns why name's last segment contradicts sha8,
// or "". fields are the description's provenance fields.
func untrackedNameSuffixBlock(name, sha8 string, fields map[string]string) string {
	suffix := name[strings.LastIndexByte(name, '-')+1:]
	switch {
	case strings.EqualFold(suffix, sha8):
		return ""
	case stemcellSHA8Value.MatchString(suffix):
		return fmt.Sprintf("its name suffix %s does not match sha8 %s, so its identity is contradictory",
			suffix, sha8)
	case fields[provenanceKindKey] != "" || fields[provenanceCIDKey] != "":
		return fmt.Sprintf("its name %q has no sha8 suffix, but its provenance records a path-identity kind or "+
			"CID, which only a CPI that appends the sha8 %s to the name writes", name, sha8)
	}
	return ""
}
