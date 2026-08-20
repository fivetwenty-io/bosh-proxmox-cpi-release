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
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// maxTagLength is the maximum byte length for the joined PVE tags field.
// PVE has no hard cap on the joined tag list, but we bound it so unbounded
// metadata can't bloat the QEMU config. The "<key>--<value>" form means a
// single director/deployment/job entry can easily exceed 50 bytes, so the
// cap is sized to comfortably hold the full BOSH-managed set (director,
// deployment, instance_group, job, index, and the UUID-bearing instance
// name) plus a few operator tags.
const maxTagLength = 350

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
//  5. Build tags string: extract director, deployment, instance_group, job, and
//     index; emit as "<key>--<value>" entries (PVE tags allow only alphanumerics
//     and "-", so instance_group becomes "instance-group--..."). Also extract the BOSH
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
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
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

		logger := deps.Log(ctx).With(
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

		// --- merge tags + update VM config (locked RMW) ---
		// Concurrent Director processes may call set_vm_metadata for the same
		// VMID at the same time (e.g. during a parallel apply). Without a lock
		// two processes each read the existing tags, each strip-and-rebuild, and
		// the last write silently discards the first writer's changes. The per-VMID
		// cluster lock serializes the tag read-modify-write across processes.
		//
		// The description and name writes are folded inside the same locked
		// UpdateQemuConfig call; they do not need separate RMW protection.
		//
		// Pool service absent (nil) → retriable error so the director re-drives.
		lockOwner := fmt.Sprintf("set_vm_metadata/%d", vmid)
		lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmid, lockOwner, logger, func() error {
			if rmwErr := setVMMetadataRMW(ctx, deps, node, vmid, vmCID, description, metadata, vmName, logger); rmwErr != nil {
				return rmwErr
			}
			// Reconcile the VM's resource-pool membership against the
			// director-level vm_pool_template, inside the same lock so a
			// concurrent set_vm_metadata cannot interleave a second
			// read-render-move. Warn-only: pool placement never fails the
			// metadata write the Director asked for.
			reconcileVMPoolMembership(ctx, deps, node, vmid, metadata, logger)
			return nil
		})
		if lockErr != nil {
			return nil, lockErr
		}

		logger.Info("set_vm_metadata: VM metadata updated")
		return nil, nil
	})
}

// setVMMetadataRMW is the tag read-modify-write body executed under the per-VMID
// cluster lock. It reads the current VM config (to preserve existing operator
// tags), merges BOSH-managed tags from metadata, and writes description + tags +
// name in a single UpdateQemuConfig call.
//
// Separated from HandleSetVMMetadata to keep cognitive complexity below the
// lint threshold; the caller (withVMIDLock closure) provides serialization.
func setVMMetadataRMW(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	vmCID string,
	description string,
	metadata map[string]any,
	vmName *string,
	logger *log.Logger,
) error {
	// Read existing tags inside the lock so no concurrent writer can interleave
	// between the read and the write. The same read supplies the current
	// description so its shared <!--BOSH:{...}--> sentinel block
	// (bosh_attached_disks from attach_disk, bosh_disk_metadata from
	// set_disk_metadata) is carried onto the rebuilt description — this
	// handler regenerates the human-readable text wholesale, and dropping
	// the sentinel would make a later get_disks fall back to bare volids.
	var existingTags []string
	var existingDesc string
	if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
		if v, ok := cfg[jsonKeyTags]; ok {
			if s, ok := v.(string); ok {
				existingTags = parseTagsField(s)
			}
		}
		if v, ok := cfg["description"]; ok {
			if s, ok := v.(string); ok {
				existingDesc = s
			}
		}
	} else if pve.IsNotFound(cfgErr) {
		return cpierrors.VMNotFound(vmCID)
	} else {
		logger.Warn("set_vm_metadata: could not read current VM config; existing tags and description sentinel will not be preserved",
			log.Err(cfgErr),
		)
	}

	if _, raw := pve.ParseSentinel(existingDesc); len(raw) > 0 {
		merged, renderErr := pve.RenderSentinel(strings.TrimSpace(description), raw)
		if renderErr != nil {
			logger.Warn("set_vm_metadata: could not re-render description sentinel; sentinel not preserved",
				log.Err(renderErr),
			)
		} else {
			description = merged
		}
	}

	preserved := stripReservedBoshTags(existingTags)
	boshEntries := buildBoshManagedTags(metadata)
	tags := mergeTagList(preserved, boshEntries, maxTagLength)

	logger.Debug("set_vm_metadata: updating VM config",
		log.String("description_len", fmt.Sprintf("%d", len(description))),
		log.String(jsonKeyTags, tags),
		log.String("name", derefStr(vmName)),
	)

	updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID, &sdknodes.UpdateQemuConfigParams{
		Description: &description,
		Tags:        &tags,
		Name:        vmName,
	})
	if updateErr != nil {
		if pve.IsNotFound(updateErr) {
			return cpierrors.VMNotFound(vmCID)
		}
		return cpierrors.Wrap(pve.WrapError(updateErr), fmt.Sprintf("set_vm_metadata: update config for VM %s", vmCID))
	}
	return nil
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

// buildBoshManagedTags extracts director, deployment, instance_group, job,
// index, and name from metadata and returns sanitized tag entries.
//
// The form is "<key>--<value>". PVE tag values accept only [A-Za-z0-9-]; any
// other byte in the key or value is replaced with "-", so metadata key
// "instance_group" is emitted as an "instance-group--<value>" tag.
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
	for _, key := range []string{"director", "deployment", "instance_group", "job", "index"} {
		v, ok := metadata[key]
		if !ok || v == nil {
			continue
		}
		s := sanitizeTagValue(fmt.Sprintf("%v", v))
		if s == "" {
			continue
		}
		parts = append(parts, sanitizeTagValue(key)+"--"+s)
	}
	if v, ok := metadata[metadataKeyName]; ok && v != nil {
		raw := strings.ReplaceAll(fmt.Sprintf("%v", v), "/", "--")
		if s := sanitizeTagValue(raw); s != "" {
			parts = append(parts, s)
		}
	}
	return parts
}
