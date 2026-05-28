package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// maxStemcellFilenameLen caps the total length of a generated stemcell filename
// (excluding the ".qcow2" suffix). Keeps the full path short enough for all
// Linux filesystems (NAME_MAX = 255).
const maxStemcellFilenameLen = 200

// BuildStemcellFilename returns the canonical qcow2 filename for a stemcell.
//
// Format: bosh-stemcell-<sanitized-name>-<sanitized-version>-<sha8>.qcow2
// where sha8 is the first 8 hex characters of sha256hex and sanitization
// lowercases the input, replaces any character outside [a-z0-9._] with "-",
// collapses consecutive "-" runs to a single "-", and trims the combined
// base (name + version, without sha8 or extension) to maxStemcellFilenameLen
// total characters.
//
// sha256hex must be at least 8 hex characters; the function uses the first 8.
// Shorter or empty values produce an "unknown" sha8 placeholder ("00000000").
func BuildStemcellFilename(name, version, sha256hex string) string {
	sanitizedName := sanitizeStemcellPart(name)
	sanitizedVersion := sanitizeStemcellPart(version)

	sha8 := "00000000"
	if len(sha256hex) >= 8 {
		sha8 = strings.ToLower(sha256hex[:8])
	}

	// Base without the sha8 suffix and extension: "bosh-stemcell-<name>-<version>"
	base := fmt.Sprintf("bosh-stemcell-%s-%s", sanitizedName, sanitizedVersion)
	if len(base) > maxStemcellFilenameLen {
		base = base[:maxStemcellFilenameLen]
		// Trim trailing "-" left by the truncation.
		base = strings.TrimRight(base, "-")
	}

	return fmt.Sprintf("%s-%s.qcow2", base, sha8)
}

// sanitizeStemcellPart lowercases s, replaces characters outside [a-z0-9._]
// with "-", and collapses consecutive "-" runs to a single "-".
//
// Iteration uses range over the string so multi-byte UTF-8 runes are processed
// as a unit: a single non-ASCII rune produces exactly one "-" rather than one
// "-" per byte, keeping output length predictable for multi-byte inputs.
func sanitizeStemcellPart(s string) string {
	s = strings.ToLower(s)
	var buf []byte
	prevDash := false
	for _, r := range s {
		if isAllowedStemcellRune(r) {
			// #nosec G115 -- isAllowedStemcellRune restricts r to ASCII [a-z0-9._]; rune-to-byte truncation is safe.
			buf = append(buf, byte(r))
			prevDash = false
		} else {
			if !prevDash {
				buf = append(buf, '-')
			}
			prevDash = true
		}
	}
	// Trim leading/trailing dashes produced by replacement.
	result := strings.Trim(string(buf), "-")
	return result
}

// isAllowedStemcellRune reports whether r is in the ASCII subset [a-z0-9._].
// Non-ASCII runes (any rune > 127) always return false so the caller replaces
// them with a single '-'. This intentionally excludes Unicode lowercase letters
// such as 'é' — only the 26 ASCII letters, digits 0-9, '.', and '_' are valid.
func isAllowedStemcellRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_'
}

// BuildStemcellCID composes the BOSH stemcell CID from the storage pool name
// and the qcow2 filename.
//
// Format: "<storage>:import/<filename>"
func BuildStemcellCID(storage, qcow2Filename string) string {
	return fmt.Sprintf("%s:import/%s", storage, qcow2Filename)
}

// ParseStemcellCID splits a stemcell CID into (storage, volumePath).
//
// Expected format: "<storage>:import/<filename>"
// volumePath is the full "import/<filename>" path used as the PVE volume identifier.
//
// Returns an error for:
//   - empty string
//   - integer-only strings (legacy VMID format such as "5042")
//   - strings without ":"
//   - strings where the path component does not start with "import/"
func ParseStemcellCID(cid string) (storage string, volumePath string, err error) {
	if cid == "" {
		return "", "", fmt.Errorf("ParseStemcellCID: empty CID")
	}

	// Reject legacy integer-only CIDs (e.g. "5042").
	if isAllDigits(cid) {
		return "", "", fmt.Errorf("ParseStemcellCID: legacy integer CID %q not supported in direct-qcow mode; clear the stemcell entry from BOSH state and re-upload to regenerate the CID", cid)
	}

	idx := strings.IndexByte(cid, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("ParseStemcellCID: CID %q missing ':' separator", cid)
	}

	storage = cid[:idx]
	volumePath = cid[idx+1:]

	if !strings.HasPrefix(volumePath, "import/") {
		return "", "", fmt.Errorf("ParseStemcellCID: CID %q volume path %q does not start with \"import/\"", cid, volumePath)
	}

	return storage, volumePath, nil
}

// IsLegacyIntegerStemcellCID reports whether cid is the obsolete integer-only
// stemcell CID format (a bare VMID such as "5042") that the template-clone CPI
// design used before the direct-qcow rewrite. Used by delete_stemcell to
// treat legacy rows as no-op deletes so operators can scrub them from the
// director without sql surgery.
func IsLegacyIntegerStemcellCID(cid string) bool {
	return cid != "" && isAllDigits(cid)
}

// IsLightStemcellCID reports whether cid is a light-stemcell CID.
//
// A light-stemcell CID has the form "light:<storage>:import/<filename>".
// Returns true iff cid starts with the literal prefix "light:" and has at
// least one character after that prefix. A bare "light:" string (no payload)
// returns false. Double-prefixed strings such as "light:light:..." return true
// because the outer prefix is present; StripLightPrefix removes exactly one layer.
func IsLightStemcellCID(cid string) bool {
	const prefix = "light:"
	return strings.HasPrefix(cid, prefix) && len(cid) > len(prefix)
}

// StripLightPrefix removes exactly one leading "light:" segment from cid.
// If cid does not satisfy IsLightStemcellCID, cid is returned unchanged.
// The function intentionally strips only one layer; callers must not double-strip.
func StripLightPrefix(cid string) string {
	if IsLightStemcellCID(cid) {
		return cid[len("light:"):]
	}
	return cid
}

// BuildLightStemcellCID returns the canonical light-stemcell CID for the given
// storage pool and qcow2 filename.
//
// Format: "light:<storage>:import/<filename>"
func BuildLightStemcellCID(storage, qcow2Filename string) string {
	return "light:" + BuildStemcellCID(storage, qcow2Filename)
}

// ParseLightStemcellCID splits a light-stemcell CID into (storage, volumePath).
//
// Expected format: "light:<storage>:import/<filename>"
// Returns an error when cid does not satisfy IsLightStemcellCID. All errors
// from the underlying ParseStemcellCID call are propagated as-is.
func ParseLightStemcellCID(cid string) (storage string, volumePath string, err error) {
	if !IsLightStemcellCID(cid) {
		return "", "", fmt.Errorf("ParseLightStemcellCID: CID %q missing \"light:\" prefix", cid)
	}
	return ParseStemcellCID(StripLightPrefix(cid))
}

// BuildTemplateStemcellCID encodes a template VMID as "template:<vmid>".
//
// Format: "template:<vmid>" (e.g. "template:6042").
// vmid must be a positive integer; callers are responsible for supplying a
// valid PVE VMID from the configured template range.
func BuildTemplateStemcellCID(vmid int64) string {
	return fmt.Sprintf("template:%d", vmid)
}

// IsTemplateStemcellCID reports whether cid starts with "template:" and has at
// least one character after the prefix that forms a valid positive integer.
//
// Returns false for:
//   - bare "template:" (empty remainder)
//   - "template:abc" (non-digit remainder)
//   - "template:-1" (leading minus — not all-digits)
//   - "template:6.5" (decimal point — not all-digits)
//   - "template:template:6042" (nested prefix — "template:6042" is not all-digits)
func IsTemplateStemcellCID(cid string) bool {
	const prefix = "template:"
	if !strings.HasPrefix(cid, prefix) {
		return false
	}
	remainder := cid[len(prefix):]
	return isAllDigits(remainder)
}

// ParseTemplateStemcellCID extracts the VMID from a "template:<vmid>" CID.
//
// Returns an error when:
//   - cid does not satisfy IsTemplateStemcellCID (missing prefix, empty remainder,
//     non-digit characters, negative sign, decimal point, or nested prefix)
//   - the VMID string overflows int64
//   - the parsed VMID is zero or negative (template VMIDs must be positive)
func ParseTemplateStemcellCID(cid string) (int64, error) {
	if !IsTemplateStemcellCID(cid) {
		return 0, fmt.Errorf("ParseTemplateStemcellCID: CID %q is not a valid template CID (expected \"template:<positive-integer>\")", cid)
	}
	const prefix = "template:"
	remainder := cid[len(prefix):]
	vmid, err := strconv.ParseInt(remainder, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ParseTemplateStemcellCID: CID %q VMID %q: %w", cid, remainder, err)
	}
	if vmid <= 0 {
		return 0, fmt.Errorf("ParseTemplateStemcellCID: CID %q VMID %d must be a positive integer", cid, vmid)
	}
	return vmid, nil
}

// isAllDigits reports whether s consists entirely of ASCII decimal digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// storageContentItem is the subset of fields needed from each item returned by
// GET /nodes/{node}/storage/{storage}/content.
type storageContentItem struct {
	VolID string `json:"volid"`
}

// stringPtr returns a pointer to s. Used to pass string literals to SDK param
// fields that take *string.
func stringPtr(s string) *string { return &s }

// FindStemcellByFilename scans the given storage pool for a content item
// whose volid ends with ":import/<qcow2Filename>". Returns the full volid
// (suitable as a stemcell CID) or an empty string if no match is found.
// API errors are returned as-is.
//
// The call targets content type "import" to limit the result set. All
// returned items are iterated; the first matching volid is returned.
func FindStemcellByFilename(ctx context.Context, client Client, node, storage, qcow2Filename string) (volid string, err error) {
	resp, err := client.Nodes().ListStorageContent(ctx, node, storage, &nodes.ListStorageContentParams{
		Content: stringPtr("import"),
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}

	want := ":import/" + qcow2Filename
	for _, raw := range *resp {
		var item storageContentItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Skip malformed entries.
			continue
		}
		if strings.HasSuffix(item.VolID, want) {
			return item.VolID, nil
		}
	}

	return "", nil
}
