// Template-clone primitives: name construction, VM clone, template freeze, and template lookup.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
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
	//
	// The PVE (Perl-backed) API serialises this boolean as the JSON number 1,
	// not the JSON literal true, so the field is decoded through pveBool rather
	// than *bool — a *bool decode fails on the integer and would silently drop
	// every template from the scan, defeating stemcell-template deduplication.
	Template *pveBool `json:"template,omitempty"`
}

// pveBool decodes a PVE API boolean. PVE renders booleans as the JSON numbers
// 1 and 0 (its API is Perl-backed), but some endpoints — and most fixtures —
// use the JSON literals true/false, and a handful return the strings "1"/"0".
// All three encodings, plus null, are accepted; anything else is an error so a
// genuinely malformed field still surfaces.
type pveBool bool

// UnmarshalJSON accepts 1/0, true/false, "1"/"0"/"true"/"false", and null.
func (b *pveBool) UnmarshalJSON(data []byte) error {
	switch s := strings.Trim(strings.TrimSpace(string(data)), `"`); s {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("pveBool: cannot decode %q as a PVE boolean", s)
	}
	return nil
}

// BuildTemplateName returns the canonical PVE VM name for a stemcell template.
//
// Format: "bosh-stemcell-<sanitized-name>-<sanitized-version>"
// Sanitization uses the same rules as BuildStemcellFilename: lowercase, replace
// characters outside [a-z0-9._] with "-", collapse consecutive "-" runs, trim
// leading/trailing "-". The combined result is capped at maxStemcellFilenameLen
// total characters; any trailing "-" introduced by truncation is stripped.
//
// The name is used as the idempotency lookup key for FindTemplateByNameCluster:
// two
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
			buf = append(buf, byte(r)) // #nosec G115 -- r is range-checked to a-z/0-9 above, always fits in a byte
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
		return cpierrors.Wrap(WrapError(err), fmt.Sprintf("AssignVMToPool: pool %q vmid %d", poolID, vmid))
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

// replicaNodeTag returns the canonical per-node replica tag for node.
// Format: "bosh-stemcell-node-<sanitized-node>" where sanitized-node has
// characters outside [a-z0-9-] replaced with "-" (mirrors dnsSafeStemcellPart).
func replicaNodeTag(node string) string {
	safe := dnsSafeStemcellPart(node)
	return "bosh-stemcell-node-" + safe
}

// ReplicaNodeTagForNode is the exported form of replicaNodeTag, used by handler
// code in internal/cpi/handlers when composing the combined tag string for a
// replica template VM. Both this function and replicaNodeTag produce the same
// output; the unexported form is used internally within this package.
func ReplicaNodeTagForNode(node string) string {
	return replicaNodeTag(node)
}

// ResolveTemplateVMIDForNode returns the VMID of a stemcell template residing
// on node that matches sha8. It accepts both the primary template (on the
// canonical template node) and per-node replicas tagged with
// "bosh-stemcell-node-<node>".
//
// Match criteria (both candidate types accepted; lowest VMID wins on tie).
// Every candidate must first pass the generation gate — carry
// "bosh-stemcell-cache" or a "director--<uuid>" ref tag — so a template left
// by a previous CPI generation is never returned (see stemcell_generation.go):
//  1. Template carries "bosh-stemcell-sha-<sha8>" AND "bosh-stemcell-node-<node>". → replica.
//  2. Template carries "bosh-stemcell-sha-<sha8>" AND no "bosh-stemcell-node-" tag. → primary.
//
// Return values:
//   - (vmid, true, nil)  — match found on node.
//   - (0, false, nil)    — no match; sha8 empty; nil/empty list.
//   - (0, false, err)    — ListQemu API error.
//
// The placement scorer consumes this helper to check whether a per-node replica
// exists before choosing a target node for clone.
func ResolveTemplateVMIDForNode(ctx context.Context, c Client, node, sha8 string) (vmid int, found bool, err error) {
	if ctx == nil {
		return 0, false, cpierrors.Cloud("ResolveTemplateVMIDForNode: ctx must not be nil")
	}
	if c == nil {
		return 0, false, cpierrors.Cloud("ResolveTemplateVMIDForNode: client must not be nil")
	}
	if node == "" {
		return 0, false, cpierrors.Cloud("ResolveTemplateVMIDForNode: node must not be empty")
	}
	if sha8 == "" {
		return 0, false, nil
	}

	shaTag := "bosh-stemcell-sha-" + sha8
	nodeTag := replicaNodeTag(node)

	resp, listErr := c.Nodes().ListQemu(ctx, node, nil)
	if listErr != nil {
		return 0, false, cpierrors.Wrap(listErr,
			fmt.Sprintf("ResolveTemplateVMIDForNode: node %s sha8 %q", node, sha8))
	}
	if resp == nil || len(*resp) == 0 {
		return 0, false, nil
	}

	var bestVMID int64
	for i, raw := range *resp {
		var item qemuListItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Malformed element — skip; do not fail the whole scan. Logged at
			// Debug so schema drift leaves a diagnostic trail instead of
			// silently reporting "template not found" a layer up.
			log.FromContext(ctx).Debug("template: skipping malformed qemu list entry",
				log.String("node", node),
				log.Int("index", i),
				log.Err(jsonErr),
			)
			continue
		}
		if item.Template == nil || !*item.Template {
			continue
		}
		if item.Tags == nil {
			continue
		}
		tokens := splitPVETags(*item.Tags)
		if !hasStemcellGenerationMarker(tokens) {
			// Same generation gate the cluster-scoped scan applies: a
			// pre-generation template carries the sha tag but none of this
			// CPI's markers and must stay invisible here too, or the
			// placement scorer would treat it as a usable clone source and
			// pull it back into the adopt/destroy paths.
			continue
		}

		hasSHA := false
		hasNodeTag := false
		hasAnyNodeTag := false
		for _, tok := range tokens {
			if tok == shaTag {
				hasSHA = true
			}
			if tok == nodeTag {
				hasNodeTag = true
			}
			if strings.HasPrefix(tok, "bosh-stemcell-node-") {
				hasAnyNodeTag = true
			}
		}
		if !hasSHA {
			continue
		}
		// Accept: replica with this node's tag, OR primary with no node tag.
		if !hasNodeTag && hasAnyNodeTag {
			continue
		}
		if bestVMID == 0 || item.Vmid < bestVMID {
			bestVMID = item.Vmid
		}
	}

	if bestVMID == 0 {
		return 0, false, nil
	}
	return int(bestVMID), true, nil
}

// ---- Cluster-scoped template cache lookup ----
//
// A node-scoped ListQemu lookup would mean create-side dedup on node A and a
// destroy-side sweep initiated from node B see different worlds. The
// functions below scan Cluster().ListResources instead, so create-side lookup
// and destroy-side sweep always agree on which cache templates exist,
// clusterwide, regardless of which node the caller is talking to.

// clusterResourceTypeQemu is the "type" discriminator for QEMU guest rows in
// a GET /cluster/resources response (as opposed to "lxc", "node", "storage",
// "pool", "sdn", etc.).
const clusterResourceTypeQemu = "qemu"

// TemplateRef identifies a stemcell-cache template VM discovered by a
// cluster-scoped scan (FindTemplatesBySHATagCluster / FindTemplateByNameCluster).
type TemplateRef struct {
	// VMID is the template's PVE VM ID.
	VMID int64
	// Node is the PVE node currently hosting the template.
	Node string
	// Name is the template's PVE VM name.
	Name string
	// Tags is the raw (semicolon-delimited) PVE tags string, returned as-is
	// so callers needing further tag inspection (e.g. per-node replica tags)
	// do not have to re-fetch the resource list.
	Tags string
}

// IsReplica reports whether this template carries a per-node replica tag
// ("bosh-stemcell-node-<node>"). Replicas are clone-speed caches: they never
// hold director references (their provenance ref set is a fossil of their
// creation), so ref-anchor selection — the create-side dedup register and
// delete_stemcell's deregister target — must skip them. Anchoring on a
// replica would consult the wrong ref set and can either destroy a template
// other directors still reference or turn delete_stemcell into a no-op.
func (r TemplateRef) IsReplica() bool {
	for _, tok := range splitPVETags(r.Tags) {
		if strings.HasPrefix(tok, "bosh-stemcell-node-") {
			return true
		}
	}
	return false
}

// clusterQemuResourceItem is the subset of fields needed from each element of
// a GET /cluster/resources response to identify stemcell-cache template VMs.
type clusterQemuResourceItem struct {
	// Type discriminates resource kind; only clusterResourceTypeQemu ("qemu")
	// rows are VM guests — "lxc", "node", "storage" etc. are skipped.
	Type string `json:"type"`
	// Vmid is the unique VM ID; 0 for non-VM resource rows.
	Vmid int64 `json:"vmid"`
	// Node is the hosting PVE node.
	Node string `json:"node"`
	// Name is the VM (host)name; absent for unnamed VMs.
	Name string `json:"name"`
	// Tags is the semicolon-delimited PVE tag string; absent when no tags set.
	Tags string `json:"tags"`
	// Template is true when the VM has been frozen as a PVE template. Decoded
	// through pveBool for the same reason as qemuListItem.Template above: PVE
	// serialises this boolean as 1/0, not true/false.
	Template *pveBool `json:"template,omitempty"`
}

// listClusterQemuTemplates fetches the full cluster resource list and returns
// the decoded, type/template-filtered QEMU template rows. Shared by
// FindTemplatesBySHATagCluster and FindTemplateByNameCluster so both apply
// identical filtering (type=="qemu", template==true, generation-compatible)
// before their respective name/tag match.
//
// Rows without a marker proving this CPI generation built or adopted them
// (hasStemcellGenerationMarker) are dropped here rather than in each caller,
// so no sha8- or name-keyed path can adopt, sweep, or destroy a template a
// previous CPI generation left on the cluster. See stemcell_generation.go.
func listClusterQemuTemplates(ctx context.Context, c Client, label string) ([]clusterQemuResourceItem, error) {
	resp, err := c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		return nil, cpierrors.Wrap(err, label)
	}
	if resp == nil || len(*resp) == 0 {
		return nil, nil
	}

	out := make([]clusterQemuResourceItem, 0, len(*resp))
	for i, raw := range *resp {
		var item clusterQemuResourceItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			// Malformed element — skip; do not fail the whole scan. Logged at
			// Debug so schema drift leaves a diagnostic trail instead of
			// silently reporting "template not found" a layer up.
			log.FromContext(ctx).Debug("template: skipping malformed cluster resource entry",
				log.Int("index", i),
				log.Err(jsonErr),
			)
			continue
		}
		if item.Type != clusterResourceTypeQemu || item.Vmid == 0 {
			// Excludes lxc containers, nodes, storages, pools, and any other
			// non-VM resource row.
			continue
		}
		if item.Template == nil || !*item.Template {
			// Excludes running VMs (not yet — or never — frozen as templates).
			continue
		}
		if !hasStemcellGenerationMarker(splitPVETags(item.Tags)) {
			// Excludes templates left by a previous CPI generation: they carry
			// the same content sha tag but none of this generation's refs, so
			// adopting one would destroy a live template on the first
			// last-ref delete_stemcell. Logged at Debug so an operator seeing
			// an unexpected duplicate cache can tell why.
			log.FromContext(ctx).Debug("template: skipping stemcell template with no current-generation marker",
				log.Int64("vmid", item.Vmid),
				log.String("node", item.Node),
				log.String("name", item.Name),
			)
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

// FindTemplatesBySHATagCluster scans the whole cluster (not a single node)
// for template VMs carrying the tag "bosh-stemcell-sha-<sha8>" as an exact
// tag token. Returns every match — a cache with per-node replicas can have
// more than one — sorted by VMID ascending for deterministic output.
//
// Only generation-compatible templates are returned: the sha tag identifies
// CONTENT, and a template built by a previous CPI generation carries the same
// tag for the same stemcell. listClusterQemuTemplates drops any row lacking
// this generation's cache or director-ref marker, so such a template is never
// adopted here and never reaches the delete_stemcell sweep that would destroy
// it out from under the older director still using it.
//
// Input validation:
//   - ctx nil → error
//   - c nil → error
//   - sha8 empty → (nil, nil) — not an error; callers that have not yet
//     resolved a sha8 simply see no matches.
//
// Return values:
//   - ([]TemplateRef, nil) — zero or more matches (nil slice means zero).
//   - (nil, err) — Cluster().ListResources API error, wrapped with context.
func FindTemplatesBySHATagCluster(ctx context.Context, c Client, sha8 string) ([]TemplateRef, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("FindTemplatesBySHATagCluster: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("FindTemplatesBySHATagCluster: client must not be nil")
	}
	if sha8 == "" {
		return nil, nil
	}

	wantedTag := "bosh-stemcell-sha-" + sha8

	items, err := listClusterQemuTemplates(ctx, c, fmt.Sprintf("FindTemplatesBySHATagCluster: sha8 %q", sha8))
	if err != nil {
		return nil, err
	}

	var out []TemplateRef
	for _, item := range items {
		matched := false
		for _, tok := range splitPVETags(item.Tags) {
			if tok == wantedTag {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		out = append(out, TemplateRef{VMID: item.Vmid, Node: item.Node, Name: item.Name, Tags: item.Tags})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}

// FindTemplateByNameCluster scans the whole cluster for template VMs whose
// Name exactly matches name. Returns every match (should be unique in
// practice, but manual PVE manipulation can produce duplicates), sorted by
// VMID ascending for deterministic output.
//
// Input validation:
//   - ctx nil → error
//   - c nil → error
//   - name empty → error
//
// Return values:
//   - ([]TemplateRef, nil) — zero or more matches (nil slice means zero).
//   - (nil, err) — Cluster().ListResources API error, wrapped with context.
func FindTemplateByNameCluster(ctx context.Context, c Client, name string) ([]TemplateRef, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("FindTemplateByNameCluster: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("FindTemplateByNameCluster: client must not be nil")
	}
	if name == "" {
		return nil, cpierrors.Cloud("FindTemplateByNameCluster: name must not be empty")
	}

	items, err := listClusterQemuTemplates(ctx, c, fmt.Sprintf("FindTemplateByNameCluster: name %q", name))
	if err != nil {
		return nil, err
	}

	var out []TemplateRef
	for _, item := range items {
		if item.Name != name {
			continue
		}
		out = append(out, TemplateRef{VMID: item.Vmid, Node: item.Node, Name: item.Name, Tags: item.Tags})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}

// BuildTemplateNameWithSHA returns the canonical PVE VM name for a
// stemcell-cache template, mirroring BuildTemplateName but appending
// "-<sha8>" AFTER length truncation — the same discipline
// BuildStemcellFilename applies to the qcow2 filename. Two stemcells whose
// sanitized name+version collide at the maxStemcellFilenameLen truncation
// boundary get distinct template names because the sha8 is appended after
// the cap, not folded into the truncated portion.
//
// sha8 is passed through dnsSafeStemcellPart (same DNS-safe sanitizer
// BuildTemplateName uses) so an unexpected non-hex value cannot introduce PVE
// name-invalid characters. An empty or all-invalid sha8 falls back to the
// same "00000000" placeholder BuildStemcellFilename uses for an unknown
// digest, keeping the two conventions aligned.
func BuildTemplateNameWithSHA(name, version, sha8 string) string {
	base := BuildTemplateName(name, version)
	safeSHA := dnsSafeStemcellPart(sha8)
	if safeSHA == "" {
		safeSHA = "00000000"
	}
	return base + "-" + safeSHA
}
