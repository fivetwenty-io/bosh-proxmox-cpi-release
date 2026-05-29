// Template-clone primitives: name construction, VM clone, template freeze, and template lookup.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// qemuListItem is the per-VM element returned by GET /nodes/{node}/qemu.
// ListQemuResponse is []json.RawMessage, so we unmarshal each element
// individually into this struct. Fields match the PVE API schema documented in
// ListQemuStatusCurrentResponse: Vmid is always present; Name, Tags, and
// Template are optional.
type qemuListItem struct {
	// Vmid is the unique VM ID; always present in list responses.
	Vmid int64 `json:"vmid"`
	// Name is the VM (host)name; absent for unnamed VMs.
	Name *string `json:"name,omitempty"`
	// Tags is the semicolon-delimited PVE tag string; absent when no tags set.
	Tags *string `json:"tags,omitempty"`
	// Template is true when the VM has been frozen as a PVE template.
	// PVE omits the field entirely for normal VMs (not false, just absent).
	Template *bool `json:"template,omitempty"`
}

// BuildTemplateName returns the canonical PVE VM name for a stemcell template.
//
// Format: "bosh-stemcell-<sanitized-name>-<sanitized-version>"
// Sanitization uses the same rules as BuildStemcellFilename: lowercase, replace
// characters outside [a-z0-9._] with "-", collapse consecutive "-" runs, trim
// leading/trailing "-". The combined result is capped at maxStemcellFilenameLen
// total characters; any trailing "-" introduced by truncation is stripped.
//
// The name is used as the idempotency lookup key for FindTemplateByName: two
// calls with the same (name, version) produce the same string.
func BuildTemplateName(name, version string) string {
	// PVE VM names must be valid DNS names (each label is [a-z0-9-]); '_' and
	// '.' are rejected. Use the DNS-strict sanitizer here rather than
	// sanitizeStemcellPart (which preserves '.'/'_' for volume/file names) —
	// e.g. stemcell "...ubuntu-jammy-go_agent" / version "1.1202" would
	// otherwise yield an underscore/dot the PVE qemu create rejects.
	sanitizedName := dnsSafeStemcellPart(name)
	sanitizedVersion := dnsSafeStemcellPart(version)

	base := fmt.Sprintf("bosh-stemcell-%s-%s", sanitizedName, sanitizedVersion)
	if len(base) > maxStemcellFilenameLen {
		base = base[:maxStemcellFilenameLen]
		base = strings.TrimRight(base, "-")
	}

	return base
}

// dnsSafeStemcellPart lowercases s and replaces every rune outside [a-z0-9-]
// with '-' (collapsing consecutive replacements), trimming leading/trailing
// '-'. Unlike sanitizeStemcellPart, '.' and '_' are NOT preserved: a PVE VM
// name must be a valid DNS name and the qemu create API rejects both.
func dnsSafeStemcellPart(s string) string {
	s = strings.ToLower(s)
	var buf []byte
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf = append(buf, byte(r))
			prevDash = false
		} else if !prevDash {
			buf = append(buf, '-')
			prevDash = true
		}
	}

	return strings.Trim(string(buf), "-")
}

// CloneQemuVM clones a PVE QEMU VM or template identified by templateVMID on
// the given node, passing params to the SDK CreateQemuClone call.
//
// The returned upid identifies the async PVE clone task; callers must await it
// via AwaitTask before using the new VM. upid is extracted from the
// json.RawMessage response via UPIDFromRaw; an empty UPID is not treated as an
// error here — it means PVE returned no task ID (e.g. an already-complete
// synchronous clone on some backends).
//
// Input validation:
//   - ctx nil → error
//   - c nil → error
//   - node empty → error
//   - templateVMID ≤ 0 → error
//   - params nil → error (Newid is required by the SDK and must be set by caller)
//
// All SDK errors are wrapped with context naming the node and source VMID.
func CloneQemuVM(ctx context.Context, c Client, node string, templateVMID int64, params *sdknodes.CreateQemuCloneParams) (upid string, err error) {
	if ctx == nil {
		return "", cpierrors.Cloud("CloneQemuVM: ctx must not be nil")
	}
	if c == nil {
		return "", cpierrors.Cloud("CloneQemuVM: client must not be nil")
	}
	if node == "" {
		return "", cpierrors.Cloud("CloneQemuVM: node must not be empty")
	}
	if templateVMID <= 0 {
		return "", cpierrors.Cloud("CloneQemuVM: templateVMID must be a positive integer, got %d", templateVMID)
	}
	if params == nil {
		return "", cpierrors.Cloud("CloneQemuVM: params must not be nil (Newid is required)")
	}

	vmidStr := strconv.FormatInt(templateVMID, 10)
	raw, err := c.Nodes().CreateQemuClone(ctx, node, vmidStr, params)
	if err != nil {
		return "", cpierrors.Wrap(err, fmt.Sprintf("CloneQemuVM: node %s vmid %d", node, templateVMID))
	}

	if raw == nil {
		return "", nil
	}

	upid, err = UPIDFromRaw(*raw)
	if err != nil {
		return "", cpierrors.Wrap(err, fmt.Sprintf("CloneQemuVM: extract UPID: node %s vmid %d", node, templateVMID))
	}

	return upid, nil
}

// MakeTemplate converts the given VM into a PVE template by calling the
// CreateQemuTemplate endpoint. Disk=nil causes PVE to convert all disks
// (the correct default for stemcell templates).
//
// The returned upid identifies the async freeze task; callers must await it
// via AwaitTask before treating the VM as a usable template. An empty upid
// means PVE completed the conversion synchronously and returned no task ID.
//
// Input validation:
//   - ctx nil → error
//   - c nil → error
//   - node empty → error
//   - vmid ≤ 0 → error
//
// All SDK errors are wrapped with context naming the node and VMID.
func MakeTemplate(ctx context.Context, c Client, node string, vmid int64) (upid string, err error) {
	if ctx == nil {
		return "", cpierrors.Cloud("MakeTemplate: ctx must not be nil")
	}
	if c == nil {
		return "", cpierrors.Cloud("MakeTemplate: client must not be nil")
	}
	if node == "" {
		return "", cpierrors.Cloud("MakeTemplate: node must not be empty")
	}
	if vmid <= 0 {
		return "", cpierrors.Cloud("MakeTemplate: vmid must be a positive integer, got %d", vmid)
	}

	vmidStr := strconv.FormatInt(vmid, 10)
	// Disk=nil → convert all disks; this is the correct default for stemcell templates.
	raw, err := c.Nodes().CreateQemuTemplate(ctx, node, vmidStr, &sdknodes.CreateQemuTemplateParams{})
	if err != nil {
		return "", cpierrors.Wrap(err, fmt.Sprintf("MakeTemplate: node %s vmid %d", node, vmid))
	}

	if raw == nil {
		return "", nil
	}

	upid, err = UPIDFromRaw(*raw)
	if err != nil {
		return "", cpierrors.Wrap(err, fmt.Sprintf("MakeTemplate: extract UPID: node %s vmid %d", node, vmid))
	}

	return upid, nil
}

// FindTemplateByName returns the VMID of a template VM whose Name exactly
// matches name on node. It calls ListQemu and scans all results.
//
// Match criteria:
//   - The VM Name field equals name (exact, case-sensitive).
//   - If the Template field is present in the response, it must be true.
//     When Template is absent (PVE omits it for non-templates), the entry is
//     skipped — only items where Template is explicitly true are matched.
//     This avoids false positives from regular VMs whose name happens to match
//     a template name before the freeze task completes.
//
// On multiple matches (e.g. two templates with identical names due to manual
// PVE manipulation), the entry with the lowest VMID is returned. The
// ListQemuResponse order is not guaranteed, so all entries are scanned.
//
// Return values:
//   - (vmid, true, nil)  — exactly one match found; vmid is the lowest-VMID match.
//   - (0, false, nil)    — no match; nil list; or all elements failed to parse.
//   - (0, false, err)    — ListQemu API error; err is wrapped with context.
func FindTemplateByName(ctx context.Context, c Client, node, name string) (vmid int64, found bool, err error) {
	if ctx == nil {
		return 0, false, cpierrors.Cloud("FindTemplateByName: ctx must not be nil")
	}
	if c == nil {
		return 0, false, cpierrors.Cloud("FindTemplateByName: client must not be nil")
	}
	if node == "" {
		return 0, false, cpierrors.Cloud("FindTemplateByName: node must not be empty")
	}
	if name == "" {
		return 0, false, cpierrors.Cloud("FindTemplateByName: name must not be empty")
	}

	resp, err := c.Nodes().ListQemu(ctx, node, nil)
	if err != nil {
		return 0, false, cpierrors.Wrap(err, fmt.Sprintf("FindTemplateByName: node %s name %q", node, name))
	}
	if resp == nil || len(*resp) == 0 {
		return 0, false, nil
	}

	var bestVMID int64
	for _, raw := range *resp {
		var item qemuListItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Malformed element — skip; do not fail the whole scan.
			continue
		}
		// Only match confirmed templates.
		if item.Template == nil || !*item.Template {
			continue
		}
		if item.Name == nil || *item.Name != name {
			continue
		}
		// Match found. Keep the lowest VMID for determinism on multi-match.
		if bestVMID == 0 || item.Vmid < bestVMID {
			bestVMID = item.Vmid
		}
	}

	if bestVMID == 0 {
		return 0, false, nil
	}
	return bestVMID, true, nil
}

// AssignVMToPool adds vmid to the named PVE resource pool by delegating to
// c.Pools().AddVM. Returns nil when poolID is empty (caller skips the call).
//
// Input validation:
//   - ctx nil → error
//   - c nil → error
//   - poolID empty → nil (no-op; caller should guard before calling)
//   - vmid ≤ 0 → error
//
// All API errors are wrapped with context naming the poolID and VMID.
func AssignVMToPool(ctx context.Context, c Client, poolID string, vmid int64) error {
	if poolID == "" {
		return nil
	}
	if ctx == nil {
		return cpierrors.Cloud("AssignVMToPool: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("AssignVMToPool: client must not be nil")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("AssignVMToPool: vmid must be a positive integer, got %d", vmid)
	}
	if err := c.Pools().AddVM(ctx, poolID, vmid); err != nil {
		return fmt.Errorf("AssignVMToPool: pool %q vmid %d: %w", poolID, vmid, err)
	}
	return nil
}

// pveTagSeparators lists the delimiters PVE uses in its stored tags string.
// PVE accepts ";" and "," as separators; ";" is canonical. Both are handled
// here by normalizing "," to ";" before splitting.
const pveTagSep = ";"

// splitPVETags splits a PVE tags string into individual tag tokens.
// PVE stores tags as a semicolon-delimited string (comma also accepted).
// Empty tokens after splitting are dropped.
func splitPVETags(tags string) []string {
	if tags == "" {
		return nil
	}
	// Normalize comma separator to semicolon.
	tags = strings.ReplaceAll(tags, ",", pveTagSep)
	parts := strings.Split(tags, pveTagSep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FindTemplateBySHATag returns the VMID of a template VM carrying the tag
// "bosh-stemcell-sha-<sha8>". It calls ListQemu and scans all results.
//
// Tag matching uses token-exact comparison: the Tags field is split on PVE's
// tag separator (";", with "," normalized), and the wanted tag must appear as
// a whole token. Naive substring matching is explicitly avoided to prevent
// prefix collisions (e.g. sha8 "abc12345" must NOT match a tag token
// "bosh-stemcell-sha-abc123456").
//
// Match criteria:
//   - The VM has Template == true.
//   - The Tags field contains "bosh-stemcell-sha-<sha8>" as an exact token.
//
// On multiple matches (should not occur in practice — each stemcell has a
// unique sha8), the entry with the lowest VMID is returned.
//
// Return values:
//   - (vmid, true, nil)  — match found.
//   - (0, false, nil)    — no match; nil/empty list; sha8 empty string.
//   - (0, false, err)    — ListQemu API error; err is wrapped with context.
func FindTemplateBySHATag(ctx context.Context, c Client, node, sha8 string) (vmid int64, found bool, err error) {
	if ctx == nil {
		return 0, false, cpierrors.Cloud("FindTemplateBySHATag: ctx must not be nil")
	}
	if c == nil {
		return 0, false, cpierrors.Cloud("FindTemplateBySHATag: client must not be nil")
	}
	if node == "" {
		return 0, false, cpierrors.Cloud("FindTemplateBySHATag: node must not be empty")
	}
	if sha8 == "" {
		return 0, false, nil
	}

	wantedTag := "bosh-stemcell-sha-" + sha8

	resp, err := c.Nodes().ListQemu(ctx, node, nil)
	if err != nil {
		return 0, false, cpierrors.Wrap(err, fmt.Sprintf("FindTemplateBySHATag: node %s sha8 %q", node, sha8))
	}
	if resp == nil || len(*resp) == 0 {
		return 0, false, nil
	}

	var bestVMID int64
	for _, raw := range *resp {
		var item qemuListItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Malformed element — skip.
			continue
		}
		// Only match confirmed templates.
		if item.Template == nil || !*item.Template {
			continue
		}
		if item.Tags == nil {
			continue
		}
		tokens := splitPVETags(*item.Tags)
		matched := false
		for _, tok := range tokens {
			if tok == wantedTag {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Match found. Keep the lowest VMID for determinism on multi-match.
		if bestVMID == 0 || item.Vmid < bestVMID {
			bestVMID = item.Vmid
		}
	}

	if bestVMID == 0 {
		return 0, false, nil
	}
	return bestVMID, true, nil
}
