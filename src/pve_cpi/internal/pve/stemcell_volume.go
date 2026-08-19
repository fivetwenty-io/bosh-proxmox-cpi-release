package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
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

// ---- Path-identity stemcell CIDs ----
//
// CID family overview — every identifier the CPI emits or accepts:
//
//	Stemcell  :light:<storage>:import/<file>   operator-managed qcow2; the CPI never deletes the file
//	Stemcell  :heavy:<storage>:import/<file>   CPI-uploaded qcow2; deleted at the last director ref within a cluster
//	Disk      pvd-<base64url(json)>            persistent-disk envelope (internal/pve/disk.go)
//	Disk      pvz-<base64url(gzip(json))>      compressed persistent-disk envelope
//	VM        <vmid>                           bare integer
//	Snapshot  <vmid>:<name>                    VMID plus PVE snapshot name
//
// The leading ':' is the stemcell discriminator: a PVE storage identifier
// cannot begin with ':', so no raw "<storage>:<path>" volid can ever collide
// with a path-identity stemcell CID — including a storage pool literally
// named "light" or "heavy".

// StemcellKind discriminates the two path-identity stemcell CID families.
type StemcellKind string

const (
	// StemcellKindLight marks an operator-managed qcow2. The operator placed
	// the file and owns its lifecycle; delete_stemcell drops references and
	// cache templates but never removes the file.
	StemcellKindLight StemcellKind = "light"
	// StemcellKindHeavy marks a CPI-uploaded qcow2. The CPI owns the file and
	// deletes it when the last registered director reference within the
	// cluster is dropped.
	StemcellKindHeavy StemcellKind = "heavy"
)

const (
	lightStemcellCIDPrefix = ":light:"
	heavyStemcellCIDPrefix = ":heavy:"
)

// BuildLightStemcellCID composes a light path-identity stemcell CID.
//
// Format: ":light:<storage>:import/<filename>"
func BuildLightStemcellCID(storage, qcow2Filename string) string {
	return lightStemcellCIDPrefix + BuildStemcellCID(storage, qcow2Filename)
}

// BuildHeavyStemcellCID composes a heavy path-identity stemcell CID.
//
// Format: ":heavy:<storage>:import/<filename>"
func BuildHeavyStemcellCID(storage, qcow2Filename string) string {
	return heavyStemcellCIDPrefix + BuildStemcellCID(storage, qcow2Filename)
}

// IsStemcellPathCID reports whether cid begins with the path-identity
// discriminator ':'. It performs no further validation; use
// ParseStemcellPathCID to validate and decompose.
func IsStemcellPathCID(cid string) bool {
	return strings.HasPrefix(cid, ":")
}

// ParseStemcellPathCID validates and decomposes a path-identity stemcell CID.
//
// Accepted forms:
//
//	":light:<storage>:import/<filename>"
//	":heavy:<storage>:import/<filename>"
//
// volumePath is the "import/<filename>" tail used as the PVE volume path.
//
// Errors:
//   - empty CID
//   - missing leading ':' (includes every retired grammar: "light:...",
//     "template:<vmid>", bare "<storage>:import/...", bare integers)
//   - unknown kind segment (anything other than "light"/"heavy")
//   - doubled prefix (":light::heavy:...") — the payload after the kind must
//     itself parse as "<storage>:import/<filename>", and a storage name
//     cannot be empty or contain ':'
//   - payload whose path component does not start with "import/"
func ParseStemcellPathCID(cid string) (kind StemcellKind, storage, volumePath string, err error) {
	if cid == "" {
		return "", "", "", fmt.Errorf("ParseStemcellPathCID: empty CID")
	}
	if !strings.HasPrefix(cid, ":") {
		return "", "", "", fmt.Errorf("ParseStemcellPathCID: CID %q missing leading ':' — expected \":light:<storage>:import/<file>\" or \":heavy:<storage>:import/<file>\"", cid)
	}

	var rest string
	switch {
	case strings.HasPrefix(cid, lightStemcellCIDPrefix):
		kind = StemcellKindLight
		rest = cid[len(lightStemcellCIDPrefix):]
	case strings.HasPrefix(cid, heavyStemcellCIDPrefix):
		kind = StemcellKindHeavy
		rest = cid[len(heavyStemcellCIDPrefix):]
	default:
		return "", "", "", fmt.Errorf("ParseStemcellPathCID: CID %q has unknown kind — expected \":light:\" or \":heavy:\" prefix", cid)
	}

	if strings.HasPrefix(rest, ":") {
		return "", "", "", fmt.Errorf("ParseStemcellPathCID: CID %q has an empty storage segment (doubled prefix?)", cid)
	}

	storage, volumePath, err = ParseStemcellCID(rest)
	if err != nil {
		return "", "", "", fmt.Errorf("ParseStemcellPathCID: CID %q payload invalid: %w", cid, err)
	}

	return kind, storage, volumePath, nil
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
	for i, raw := range *resp {
		var item storageContentItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Skip malformed entries: one bad element must not fail the whole
			// scan. Logged at Debug so schema drift in a genuinely malformed
			// PVE response leaves a diagnostic trail instead of silently
			// reporting "stemcell not found" a layer up.
			log.FromContext(ctx).Debug("stemcell_volume: skipping malformed storage content entry",
				log.String("node", node),
				log.String("storage", storage),
				log.Int("index", i),
				log.Err(jsonErr),
			)
			continue
		}
		if strings.HasSuffix(item.VolID, want) {
			return item.VolID, nil
		}
	}

	return "", nil
}
