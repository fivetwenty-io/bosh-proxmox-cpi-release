package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// maxTagLength is the maximum byte length for the joined PVE tags field.
// PVE has no hard cap on the joined tag list, but we bound it so unbounded
// metadata can't bloat the QEMU config. The "<key>--<value>" form means a
// single director/deployment/job triple can easily exceed 50 bytes, so the
// cap is set to comfortably hold three full BOSH UUIDs with prefixes.
const maxTagLength = 255

// HandleSetVMMetadata returns a handler for the set_vm_metadata CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//   - args[1]: metadata (object) — arbitrary key/value map; BOSH typically sends
//     director, deployment, job, index, id.
//
// Logic:
//  1. Parse vm_cid → vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     Not-found → VMNotFound. Transport error → propagate.
//  3. Decode metadata map.
//  4. Build description string: sorted "key: value\n" lines (matches Perl reference).
//  5. Build tags string: extract director, deployment, job; emit as "<key>--<value>"
//     entries (PVE tags allow only alphanumerics and "-"). Also extract the BOSH
//     instance name ("<job>/<id>") and emit "<job>--<id>" (PVE tags reject "/").
//     Join with ";"; truncate to maxTagLength bytes at a tag boundary.
//     5a. Derive a DNS-label VM name as "<job>-<index>" (e.g. "diego-cell-0",
//     "bosh-0") so the PVE UI shows the human-readable BOSH instance
//     identifier instead of the placeholder "vm-<vmid>" written at create_vm
//     time. The UUID-bearing metadata["name"] ("<job>/<id>") is used only as
//     a fallback when job or index is missing.
//  6. Call nodes.UpdateQemuConfig with description + tags. Overwrites existing values.
//  7. 404 → return VMNotFound.
//
// Empty metadata is valid; description and tags will be empty strings.
// Returns nil result on success.
func HandleSetVMMetadata(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// --- argument extraction ---
		if len(args) < 1 {
			return nil, cpierrors.Cloud("set_vm_metadata: missing required argument vm_cid")
		}
		var vmCID string
		if err := json.Unmarshal(args[0], &vmCID); err != nil {
			return nil, cpierrors.Cloud("set_vm_metadata: vm_cid must be a string: %s", err.Error())
		}
		if vmCID == "" {
			return nil, cpierrors.Cloud("set_vm_metadata: vm_cid must not be empty")
		}

		vmid, err := strconv.Atoi(vmCID)
		if err != nil {
			return nil, cpierrors.Cloud("set_vm_metadata: vm_cid %q is not a valid integer VMID: %s", vmCID, err.Error())
		}
		if vmid <= 0 {
			return nil, cpierrors.Cloud("set_vm_metadata: vm_cid %q must be a positive integer", vmCID)
		}

		// Metadata argument: may be absent (treat as empty map) or null.
		metadata := make(map[string]any)
		if len(args) >= 2 && len(args[1]) > 0 && string(args[1]) != "null" {
			if err := json.Unmarshal(args[1], &metadata); err != nil {
				return nil, cpierrors.Cloud("set_vm_metadata: metadata must be a JSON object: %s", err.Error())
			}
		}

		logger := deps.Logger.With(
			log.String("method", "set_vm_metadata"),
			log.String("vm_cid", vmCID),
			log.Int("vmid", vmid),
		)

		// --- locate VM via cluster scan ---
		// Queries /cluster/resources for the authoritative node, correct even
		// after an HA failover. Not-found → VMNotFound. Transport error → propagate.
		logger.Debug("set_vm_metadata: locating VM via cluster scan")
		node, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(pve.WrapError(lookupErr), fmt.Sprintf("set_vm_metadata: locate VM %s", vmCID))
		}
		if !found || node == "" {
			return nil, cpierrors.VMNotFound(vmCID)
		}
		logger.Debug("set_vm_metadata: VM located", log.String("node", node))

		// --- build description ---
		// Sorted "key: value\n" lines matching Perl reference behavior.
		description := buildDescription(metadata)

		// --- merge tags ---
		// Read the existing tags string so operator-supplied custom tags
		// (env--prod, owner--cpi-test, ...) survive. The director/deployment/
		// job triple is rebuilt from metadata each call so stale values from
		// a prior sync cannot accumulate.
		var existingTags []string
		if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
			if v, ok := cfg["tags"]; ok {
				if s, ok := v.(string); ok {
					existingTags = parseTagsField(s)
				}
			}
		} else if pve.IsNotFound(cfgErr) {
			return nil, cpierrors.VMNotFound(vmCID)
		} else {
			logger.Warn("set_vm_metadata: could not read current VM config; existing tags will not be preserved",
				log.Err(cfgErr),
			)
		}

		preserved := stripReservedBoshTags(existingTags)
		boshEntries := buildBoshManagedTags(metadata)
		tags := mergeTagList(preserved, boshEntries, maxTagLength)

		// --- derive PVE VM name from prefix + deployment + job + index ---
		// PVE's name field is a DNS label (alnum + "-", ≤ 63 bytes). The CPI
		// stamps the name in "<prefix>-<deployment>-<job>-<index>" form
		// ("cpi-cf-api-0", "cf-api-0" when prefix is empty), so deployments
		// sharing a PVE cluster are filterable in the UI. When job, index, or
		// deployment is absent we fall back to the full BOSH instance name
		// ("<job>/<id>"); when no source yields a usable label the existing
		// PVE name is left untouched.
		var vmName *string
		if s := buildVMName(metadata, deps.Config.VMPrefix); s != "" {
			vmName = &s
		}

		logger.Debug("set_vm_metadata: updating VM config",
			log.String("description_len", fmt.Sprintf("%d", len(description))),
			log.String("tags", tags),
			log.String("name", derefStr(vmName)),
		)

		// --- update VM config ---
		updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID, &sdknodes.UpdateQemuConfigParams{
			Description: &description,
			Tags:        &tags,
			Name:        vmName,
		})
		if updateErr != nil {
			if pve.IsNotFound(updateErr) {
				return nil, cpierrors.VMNotFound(vmCID)
			}
			return nil, cpierrors.Wrap(pve.WrapError(updateErr), fmt.Sprintf("set_vm_metadata: update config for VM %s", vmCID))
		}

		logger.Info("set_vm_metadata: VM metadata updated")
		return nil, nil
	})
}

// buildVMName derives the PVE VM name from BOSH metadata + the operator's
// configured VM prefix. The preferred form is
// "<prefix>-<deployment>-<job>-<index>" ("cpi-cf-api-0", "cf-api-0" when
// prefix is empty) so deployments sharing a PVE cluster are filterable in
// the UI. The prefix and deployment segments are dropped from the joined
// label when the corresponding input is empty, so a metadata payload with
// only job + index still yields "<job>-<index>" — keeping legacy single-
// deployment installs visible. When job or index is absent the BOSH
// instance name ("<job>/<id>" in metadata["name"]) is sanitized as a DNS
// label. Returns "" when no source yields a usable label, signalling the
// caller to leave the existing PVE name untouched.
func buildVMName(metadata map[string]any, prefix string) string {
	job, jobOK := metadata["job"]
	idx, idxOK := metadata["index"]
	if jobOK && job != nil && idxOK && idx != nil {
		parts := make([]string, 0, 4)
		if prefix != "" {
			parts = append(parts, prefix)
		}
		if v, ok := metadata["deployment"]; ok && v != nil {
			if s := fmt.Sprintf("%v", v); s != "" {
				parts = append(parts, s)
			}
		}
		parts = append(parts, fmt.Sprintf("%v", job), fmt.Sprintf("%v", idx))
		if s := sanitizeVMName(strings.Join(parts, "-")); s != "" {
			return s
		}
	}
	if v, ok := metadata[metadataKeyName]; ok && v != nil {
		raw := fmt.Sprintf("%v", v)
		if prefix != "" {
			raw = prefix + "-" + raw
		}
		if s := sanitizeVMName(raw); s != "" {
			return s
		}
	}
	return ""
}

// derefStr returns the pointed-to string, or "" if the pointer is nil. Used
// only for logging.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// buildDescription renders metadata as sorted "key: value\n" lines.
// Empty metadata produces an empty string.
func buildDescription(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(": ")
		fmt.Fprintf(&sb, "%v", metadata[k])
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildBoshManagedTags extracts director, deployment, job, and name from
// metadata and returns sanitized tag entries.
//
// For director/deployment/job the form is "<key>--<value>". PVE tag values
// accept only [A-Za-z0-9-]; any other byte in the value is replaced with "-".
//
// For the BOSH instance name (e.g. "diego-cell/2844c990-aef3-4de7-8bf3-..."),
// the "/" between job and id is rewritten to "--" so the tag round-trips as
// "<job>--<id>" (PVE tags reject "/"). The job/id pair is stable per VM CID,
// so re-running set_vm_metadata produces the same tag and mergeTagList dedups
// rather than accumulating.
//
// Keys whose metadata value is missing, nil, or sanitizes to empty are skipped.
func buildBoshManagedTags(metadata map[string]any) []string {
	var parts []string
	for _, key := range []string{"director", "deployment", "job", "index"} {
		v, ok := metadata[key]
		if !ok || v == nil {
			continue
		}
		s := sanitizeTagValue(fmt.Sprintf("%v", v))
		if s == "" {
			continue
		}
		parts = append(parts, key+"--"+s)
	}
	if v, ok := metadata[metadataKeyName]; ok && v != nil {
		raw := strings.ReplaceAll(fmt.Sprintf("%v", v), "/", "--")
		if s := sanitizeTagValue(raw); s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}
