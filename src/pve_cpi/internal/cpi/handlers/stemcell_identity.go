// Stemcell identity on a workload VM: the "stemcell--<name>-<version>" tag and
// the bosh_stemcell description sentinel that together say which stemcell a
// guest was booted from. create_vm is the only call that knows, so it stamps
// both; see internal/pve/vm_stemcell.go for the sentinel codec.
package handlers

import (
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// stemcellTagPrefix is the tag key naming the stemcell a VM booted from, for
// example "stemcell--bosh-openstack-kvm-ubuntu-noble-1-585".
//
// It is deliberately NOT in reservedBoshTagPrefixes. Those prefixes are the
// keys set_vm_metadata rebuilds from the Director's metadata map on every
// sync, and the Director never sends the stemcell. A reserved prefix here
// would mean the first set_vm_metadata call strips the tag create_vm wrote and
// puts nothing back. Leaving it unreserved makes it behave like ownershipTag:
// written once at create time, preserved across every later metadata update,
// and replaced only when the VM itself is recreated from a new stemcell.
const stemcellTagPrefix = "stemcell--"

// stemcellFilenamePrefix is the fixed prefix pve.BuildStemcellFilename puts in
// front of every stemcell qcow2 name.
const stemcellFilenamePrefix = "bosh-stemcell-"

// stemcellLabelFromFilename recovers the "<name>-<version>" identity from a
// stemcell qcow2 filename produced by pve.BuildStemcellFilename, whose shape is
// "bosh-stemcell-<name>-<version>-<sha8>.qcow2". For the noble stemcell that
// is "bosh-openstack-kvm-ubuntu-noble-1.585".
//
// The filename is the one identity source available on every create_vm path.
// The cache template carries the same name and version in its provenance JSON,
// but create_vm falls back to a direct qcow2 import whenever the template is
// missing, and a create-env Director frequently takes that path, so reading the
// template would leave exactly the guests we most want labelled unlabelled.
//
// Name and version may both contain hyphens, so the sha8 is peeled off by
// reusing extractSHA8FromFilename's validation rather than by counting
// separators. Returns "" when the filename does not match the expected shape,
// which the callers treat as "stamp nothing" — an unlabelled VM is the state
// every release before this one produced.
//
// The label is also held to the character set pve.BuildStemcellFilename can
// produce. Only the "import/" prefix of a stemcell CID's volume path is
// validated, so for a ":light:" stemcell the filename is whatever the operator
// named the qcow2, and it reaches us as arbitrary bytes. A label carrying a
// newline would break the "key: value" shape of the readable notes, and one
// carrying "[bosh_storage_allocation]" or "<!--BOSH:" would be written into the
// same description field that holds the allocation marker and the sentinel,
// where set_vm_metadata would then reject every later metadata sync for that
// VM as ambiguous provenance. A filename outside this alphabet did not come
// from our builder, so declining to derive a label from it is the right answer.
func stemcellLabelFromFilename(filename string) string {
	if _, ok := extractSHA8FromFilename(filename); !ok {
		return ""
	}
	base := filename[:len(filename)-len(".qcow2")]
	// extractSHA8FromFilename proved the final "-" exists and the segment
	// after it is the sha8, so cutting there leaves "bosh-stemcell-<name>-<version>".
	base = base[:strings.LastIndexByte(base, '-')]
	if !strings.HasPrefix(base, stemcellFilenamePrefix) {
		return ""
	}
	label := strings.Trim(strings.TrimPrefix(base, stemcellFilenamePrefix), "-")
	if !isStemcellLabelSafe(label) {
		return ""
	}
	return label
}

// maxStemcellLabelLen caps the derived label. pve.BuildStemcellFilename allows a
// 200-byte base, so a label can otherwise run to about 186 characters, and the
// tag built from it would take more than half a guest's 350-byte tag budget away
// from the advertised-route and identity tags that delete_vm and the parker
// lookup depend on. A real stemcell label is around 37 characters, so a label
// past this bound did not come from a stemcell we would recognise anyway.
const maxStemcellLabelLen = 96

// isStemcellLabelSafe reports whether every byte of label is one
// pve.BuildStemcellFilename's sanitizer can emit, plus the uppercase letters an
// operator-placed qcow2 may legitimately carry, and whether the whole label fits
// maxStemcellLabelLen. Nothing in this set can open a description sentinel, an
// allocation marker, or a new line.
func isStemcellLabelSafe(label string) bool {
	if label == "" || len(label) > maxStemcellLabelLen {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

// stemcellIdentityTag builds the "stemcell--<name>-<version>" tag for a parsed
// stemcell CID, or "" when the filename yields no label.
//
// The label goes through sanitizeTagValue, which keeps the CPI to one tag
// alphabet ([A-Za-z0-9-]) across every tag it writes, so a dotted version
// reaches PVE as "1-585" rather than "1.585". The exact version survives in the
// bosh_stemcell sentinel's label field, which is the value to read when an
// exact match matters.
func stemcellIdentityTag(parsed *createVMParsedArgs) string {
	if parsed == nil {
		return ""
	}
	label := sanitizeTagValue(stemcellLabelFromFilename(parsed.stemcellFilename))
	if label == "" {
		return ""
	}
	return stemcellTagPrefix + label
}

// vmStemcellRecord builds the bosh_stemcell sentinel record for a parsed
// stemcell CID. Returns nil when the CID yields nothing worth recording.
func vmStemcellRecord(parsed *createVMParsedArgs) *pve.VMStemcell {
	if parsed == nil || parsed.stemcellCID == "" {
		return nil
	}
	sha8, _ := extractSHA8FromParsed(parsed)
	return &pve.VMStemcell{
		Label: stemcellLabelFromFilename(parsed.stemcellFilename),
		CID:   parsed.stemcellCID,
		Kind:  string(parsed.stemcellKind),
		SHA8:  sha8,
	}
}

// stemcellNoteKey is the line key the readable notes carry the stemcell under,
// matching the "<key>: <value>" shape buildDescription emits for every
// Director-supplied metadata key.
const stemcellNoteKey = "stemcell"

// withStemcellNote appends a "stemcell: <name>-<version>" line to a freshly
// built description, reading the label from the bosh_stemcell sentinel on the
// VM's existing description.
//
// The line is appended rather than sorted into place because a metadata value
// containing a newline would let a re-sort interleave its fragments with
// unrelated keys. A description that already carries a "stemcell: " line is
// returned untouched, so a Director that sends its own stemcell key wins and
// no VM ends up with the fact stated twice.
func withStemcellNote(description, existingDesc string) string {
	for _, line := range strings.Split(description, "\n") {
		if strings.HasPrefix(line, stemcellNoteKey+": ") {
			return description
		}
	}
	rec, ok := pve.GetVMStemcell(existingDesc)
	// The label is re-checked on the way out, not only on the way in: the
	// description is a field any holder of VM.Config can edit by hand, so the
	// record we read back is not necessarily the one create_vm wrote.
	if !ok || !isStemcellLabelSafe(rec.Label) {
		return description
	}
	return description + stemcellNoteKey + ": " + rec.Label + "\n"
}
