// Package handlers contains the 22 BOSH CPI v2 method implementations.
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
//  2. Decode metadata map.
//  3. Build description string: sorted "key: value\n" lines (matches Perl reference).
//  4. Build tags string: extract director, deployment, job; emit as "<key>--<value>"
//     entries (PVE tags allow only alphanumerics and "-"); join with ";"; truncate
//     to 50 chars at a tag boundary.
//  5. Call nodes.UpdateQemuConfig with description + tags. Overwrites existing values.
//  6. 404 → return VMNotFound.
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

		node := deps.Config.Node
		logger := deps.Logger.With(
			log.String("method", "set_vm_metadata"),
			log.String("vm_cid", vmCID),
			log.Int("vmid", vmid),
		)

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

		logger.Debug("set_vm_metadata: updating VM config",
			log.String("description_len", fmt.Sprintf("%d", len(description))),
			log.String("tags", tags),
		)

		// --- update VM config ---
		updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID, &sdknodes.UpdateQemuConfigParams{
			Description: &description,
			Tags:        &tags,
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
		sb.WriteString(fmt.Sprintf("%v", metadata[k]))
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildBoshManagedTags extracts director, deployment, and job from metadata
// and returns sanitized "<key>--<value>" entries. PVE tag values accept only
// [A-Za-z0-9-]; any other byte in the value is replaced with "-". Keys whose
// metadata value is missing, nil, or sanitizes to empty are skipped.
func buildBoshManagedTags(metadata map[string]any) []string {
	var parts []string
	for _, key := range []string{"director", "deployment", "job"} {
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
	return parts
}
