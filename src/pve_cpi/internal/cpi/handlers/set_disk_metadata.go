package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// Top-level sentinel JSON keys owned by this handler inside the shared
// <!--BOSH:{...}--> description block (codec: pve.ParseSentinel /
// pve.RenderSentinel). Other keys in the same block (bosh_attached_disks,
// bosh_parked_disks) belong to other writers and pass through raw.
const (
	sentinelKeyDiskMetadata = "bosh_disk_metadata"
	sentinelKeyDiskTags     = "bosh_disk_tags"
)

// attachedVM records the node+vmid pair that hosts a disk.
type attachedVM struct {
	node string
	vmid int
}

// HandleSetDiskMetadata returns a Handler for the BOSH CPI "set_disk_metadata" method.
//
// BOSH passes metadata key-value pairs (e.g., instance_id, deployment, director) that
// describe the lifecycle context of a persistent disk. Because PVE storage volumes have
// no native metadata API, this handler stashes the metadata as JSON in the description
// field of the VM the disk is currently attached to. The sentinel format is:
//
//	<!--BOSH:{"bosh_disk_metadata":{"<disk_cid>":{...metadata...}}}-->
//
// Any non-BOSH content in the description is preserved before the sentinel block.
//
// Arguments:
//   - args[0]: disk_cid (string) — the "pvd-"/"pvz-" envelope form emitted by
//     create_disk; decoded via decodeDiskCID, then parsed via pve.ParseDiskCID.
//   - args[1]: metadata (map[string]any) — arbitrary key-value pairs from the Director.
//
// Logic:
//  1. Validate and parse both arguments.
//  2. Scan all cluster nodes for VMs; for each VM call qemu.Config and check whether
//     any disk slot's volid contains disk_cid. Collect (node, vmid) matches.
//  3. If 0 matches: log WARN "disk not attached, metadata not persisted"; return nil.
//  4. If 2+ matches: return CloudError "ambiguous disk attachment".
//  5. If exactly 1 match: read current VM description, parse/merge the sentinel block,
//     call nodes.UpdateQemuConfig with the updated description.
//
// Errors:
//   - Missing or invalid args → CloudError.
//   - SDK API errors → CloudError wrapping the SDK error.
//   - Ambiguous attachment → CloudError.
func HandleSetDiskMetadata(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// --- decode args ---
		if len(args) < 2 {
			return nil, cpierrors.Cloud(
				"set_disk_metadata: requires 2 arguments (disk_cid, metadata), got %d", len(args),
			)
		}

		var diskCID string
		if err := json.Unmarshal(args[0], &diskCID); err != nil || diskCID == "" {
			return nil, cpierrors.Cloud("set_disk_metadata: disk_cid must be a non-empty string")
		}
		// Strip optional metadata suffix before any PVE API or storage lookup.
		bareDiskCID, decodedMeta, decErr := decodeDiskCID(ctx, deps, "set_disk_metadata", diskCID)
		if decErr != nil {
			return nil, decErr
		}
		// Resolve to the volume's current name (identity seam): the hosting
		// scan matches VM config entries, which carry the post-reassignment
		// name for stable-ID disks. The metadata sentinel is keyed by
		// rd.sentinelKey() — the stable ID when the disk has one — so the
		// record survives future renames.
		rd, resolveErr := resolveDiskForOp(ctx, deps, "set_disk_metadata", diskCID, bareDiskCID, decodedMeta)
		if resolveErr != nil {
			return nil, resolveErr
		}
		bareDiskCID = rd.volid

		var metadata map[string]any
		if err := json.Unmarshal(args[1], &metadata); err != nil {
			return nil, cpierrors.Cloud("set_disk_metadata: metadata must be a JSON object: %s", err.Error())
		}

		// Validate disk_cid structure (storage:volume).
		if _, _, err := pve.ParseDiskCID(bareDiskCID); err != nil {
			return nil, err
		}

		// Extract operator-supplied disk tags out of metadata so they don't
		// also land in the regular sentinel "bosh_disk_metadata" payload.
		// Accept map[string]string and map[string]any (BOSH JSON deserialises
		// nested objects as map[string]any).
		var diskTags map[string]string
		if raw, ok := metadata[jsonKeyTags]; ok {
			diskTags = coerceTagMap(raw)
			delete(metadata, jsonKeyTags)
		}

		// --- scan VMs for disk ---
		matches, err := findVMsHostingDisk(ctx, deps, bareDiskCID)
		if err != nil {
			return nil, err
		}

		switch len(matches) {
		case 0:
			deps.Log(ctx).Warn("set_disk_metadata: disk not attached; metadata not persisted",
				log.String("disk_cid", diskCID))
			return nil, nil

		case 1:
			// Wrap both persistMetadata and applyCustomTagsToVM under a single
			// per-VMID cluster lock. This prevents a concurrent set_vm_metadata
			// (or another set_disk_metadata) from interleaving its own Config
			// read between our read and write, which would silently lose one
			// writer's changes. Both calls share the same VMID so a single lock
			// acquisition covers the full case-1 RMW sequence atomically.
			//
			// Pool service absent (nil) → retriable error so the director re-drives.
			vmid := matches[0].vmid
			lockOwner := fmt.Sprintf("set_disk_metadata/%d", vmid)
			lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmid, lockOwner, deps.Log(ctx), func() error {
				if err := persistMetadata(ctx, deps, matches[0], rd.sentinelKey(), metadata); err != nil {
					return err
				}
				if len(diskTags) > 0 {
					if err := applyCustomTagsToVM(ctx, deps, matches[0].node, vmid, diskTags, diskCID); err != nil {
						return err
					}
				}
				return nil
			})
			if lockErr != nil {
				return nil, lockErr
			}
			return nil, nil

		default:
			return nil, cpierrors.Cloud(
				"set_disk_metadata: ambiguous disk attachment — disk %q found on %d VMs",
				diskCID, len(matches),
			)
		}
	})
}

// coerceTagMap accepts an arbitrary JSON value supplied under metadata[jsonKeyTags]
// and returns it as map[string]string. Non-string values are stringified via
// fmt.Sprint to keep the path forgiving for callers that supply numeric tag
// values. Returns nil if the input is not a JSON object.
func coerceTagMap(v any) map[string]string {
	switch m := v.(type) {
	case map[string]string:
		out := make(map[string]string, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, val := range m {
			out[k] = fmt.Sprint(val)
		}
		return out
	default:
		return nil
	}
}

// findVMsHostingDisk scans every cluster guest via authoritative per-node
// listings and returns every (node, vmid) pair whose QEMU config contains
// diskCID. Uses exact
// volid equality (with option-string tolerance via pve.DiskOptStrContainsVolid)
// to prevent false matches on diskCIDs that are substrings of other volids.
//
// VMs carrying the bosh-parker tag are silently skipped. A disk parked on a
// parker VM produces 0 matches, which flows into the existing warn+nil path
// ("disk not attached; metadata not persisted"). This is correct: metadata
// for a parked disk is irrelevant until the disk is attached to a real VM.
//
// The tag is tested twice and costs no extra API call either time. First on
// the listing row, which the scan already has. Then on the VM config,
// which the scan reads for every VM anyway: an empty tags field on the item
// cannot tell an untagged VM from a PVE that does not populate the field, and
// deciding on it alone would treat a parker as an ordinary holder.
//
// Classification is by the bosh-parker tag alone, with no band test. A band
// test can only narrow this one, and it narrows it in the wrong place: an
// operator who dropped the parker band while parkers still stood would have
// this treat one as an ordinary holder and merge deployment metadata into its
// description, which is where the provenance record for every disk it holds
// lives.
//
// Transport errors from the enumeration propagate as wrapped retriable
// errors.
// Per-VM Config errors are skipped only when they are not-found (the VM was
// deleted concurrently or its config is gone); any other Config error is
// classified by pve.WrapConfigReadError, so a transient fault mid-scan is
// retriable -- it must not produce a false 0-match (silent metadata loss) or a
// false 1-match (masked multi-attach) -- while a denied VM.Audit grant stays
// permanent and names itself. The scan never short-circuits on the first match:
// visiting every VM is what makes ambiguity detection possible.
func findVMsHostingDisk(ctx context.Context, deps Deps, diskCID string) ([]attachedVM, error) {
	// The candidate list comes from authoritative per-node listings
	// (ListGuestsAuthoritative), not the /cluster/resources index: the index
	// lags by minutes, so a young holder would be invisible to it, producing
	// the silent 0-match (metadata loss) or masked multi-attach the contract
	// above forbids. The enumeration classifies its own failures (retriable
	// for a partial fleet), preserving the retriable-propagation contract.
	guests, listErr := pve.ListGuestsAuthoritative(ctx, deps.PVE, deps.Log(ctx))
	if listErr != nil {
		return nil, cpierrors.Wrap(listErr, "set_disk_metadata: enumerate cluster guests")
	}

	var matches []attachedVM

	for _, g := range guests {
		vmNode := g.Node
		vmid := g.VMID

		// Skip parker VMs: a parked disk on a parker VM should not be treated
		// as "attached to a real VM". Uses only data from the listing row, no
		// extra API calls.
		//
		// The tag alone decides. IsParkerVM is the tag test AND a band test, so
		// it can only ever be a subset of this one, and the band half goes quiet
		// exactly when it matters: an operator who dropped the band while
		// parkers still stood would otherwise have this merge deployment
		// metadata into a parker's description — the field carrying the
		// provenance sentinel for every disk it holds.
		if pve.TagsMarkParker(g.Tags) {
			continue
		}

		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, vmNode, vmid)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) {
				// VM deleted concurrently or config gone: cannot host the disk.
				continue
			}
			return nil, cpierrors.Wrap(pve.WrapConfigReadError(cfgErr),
				fmt.Sprintf("set_disk_metadata: Config error for vm %d on node %s", vmid, vmNode))
		}

		// Second parker test, on the config just read. An empty tags field on
		// the cluster-resources row proves nothing -- it cannot tell an untagged
		// VM from a PVE that does not populate the field -- and treating a
		// parker as a real holder here writes deployment metadata into the
		// description that carries its provenance sentinel. delete_vm makes the
		// same fallback for the same reason; here the config is already in hand,
		// so it costs nothing.
		if cfgTags, _ := cfg["tags"].(string); pve.TagsMarkParker(cfgTags) {
			continue
		}

		// Use exact volid matching with option-string tolerance: a config value of
		// "local-lvm:vm-100-disk-0,size=10G" must match diskCID "local-lvm:vm-100-disk-0"
		// but must NOT match diskCID "local-lvm:vm-100-disk".
		if pve.DiskOptStrContainsVolid(qemu.ParseDisks(cfg), diskCID) {
			matches = append(matches, attachedVM{node: vmNode, vmid: vmid})
		}
	}

	return matches, nil
}

// persistMetadata reads the current VM description, merges the disk metadata into
// the BOSH sentinel block, and writes the updated description back via UpdateQemuConfig.
func persistMetadata(ctx context.Context, deps Deps, vm attachedVM, diskCID string, metadata map[string]any) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, vm.node, vm.vmid)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("set_disk_metadata: fetch VM config (node=%s vmid=%d)", vm.node, vm.vmid))
	}

	// Extract current description.
	currentDesc := ""
	if v, ok := cfg["description"]; ok {
		if s, ok := v.(string); ok {
			currentDesc = s
		}
	}

	// Parse the shared sentinel block, touching only this handler's key.
	// Foreign top-level keys (bosh_attached_disks from attach_disk,
	// bosh_parked_disks) pass through raw — dropping them makes a later
	// get_disks fall back to bare volids.
	nonBoshDesc, raw := pve.ParseSentinel(currentDesc)

	diskMeta := make(map[string]map[string]any)
	if b, ok := raw[sentinelKeyDiskMetadata]; ok {
		_ = json.Unmarshal(b, &diskMeta) // corrupted key → rebuilt from scratch
	}
	diskMeta[diskCID] = metadata

	metaJSON, err := json.Marshal(diskMeta)
	if err != nil {
		return cpierrors.Cloud("set_disk_metadata: marshal metadata payload: %s", err.Error())
	}
	raw[sentinelKeyDiskMetadata] = json.RawMessage(metaJSON)

	newDesc, err := pve.RenderSentinel(nonBoshDesc, raw)
	if err != nil {
		return cpierrors.Cloud("set_disk_metadata: render description sentinel: %s", err.Error())
	}

	// Write back via nodes.UpdateQemuConfig (description field).
	vmidStr := fmt.Sprintf("%d", vm.vmid)
	updateParams := &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	}
	if updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, vm.node, vmidStr, updateParams); updateErr != nil {
		return cpierrors.Wrap(pve.WrapError(updateErr),
			fmt.Sprintf("set_disk_metadata: UpdateQemuConfig (node=%s vmid=%d)", vm.node, vm.vmid),
		)
	}

	return nil
}

// applyCustomTagsToVM merges operator-supplied disk tags onto the hosting VM.
//
// PVE has no native disk-volume tag field. For tags supplied on disk_types,
// the CPI writes them to the tags field of the VM the disk is attached to
// and also stashes them in the description sentinel under bosh_disk_tags so
// the per-disk attribution survives detach+attach cycles.
//
// Merge semantics: for each key in `tags`, any existing entry on the VM with
// the same key prefix ("<key>--") is replaced. Entries with unrelated keys
// are preserved. The BOSH-reserved director/deployment/job prefixes are also
// preserved here — set_vm_metadata owns those and rebuilds them on each sync.
func applyCustomTagsToVM(ctx context.Context, deps Deps, node string, vmid int, tags map[string]string, diskCID string) error {
	if len(tags) == 0 {
		return nil
	}

	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("set_disk_metadata: fetch VM config for tag apply (node=%s vmid=%d)", node, vmid),
		)
	}

	newEntries := buildCustomTags(tags)
	replacedPrefixes := make(map[string]struct{}, len(newEntries))
	for _, e := range newEntries {
		idx := strings.Index(e, "--")
		if idx <= 0 {
			continue
		}
		replacedPrefixes[e[:idx+2]] = struct{}{}
	}

	var existing []string
	if v, ok := cfg[jsonKeyTags]; ok {
		if s, ok := v.(string); ok {
			for _, e := range parseTagsField(s) {
				skip := false
				for prefix := range replacedPrefixes {
					if strings.HasPrefix(e, prefix) {
						skip = true
						break
					}
				}
				if !skip {
					existing = append(existing, e)
				}
			}
		}
	}

	mergedTags := mergeTagList(existing, newEntries, maxTagLength)

	currentDesc := ""
	if v, ok := cfg["description"]; ok {
		if s, ok := v.(string); ok {
			currentDesc = s
		}
	}

	// Same shared-sentinel discipline as persistMetadata: touch only the
	// bosh_disk_tags key, pass every other top-level key through raw.
	nonBoshDesc, raw := pve.ParseSentinel(currentDesc)

	diskTags := make(map[string]map[string]string)
	if b, ok := raw[sentinelKeyDiskTags]; ok {
		_ = json.Unmarshal(b, &diskTags) // corrupted key → rebuilt from scratch
	}
	tagCopy := make(map[string]string, len(tags))
	for k, v := range tags {
		tagCopy[k] = v
	}
	diskTags[diskCID] = tagCopy

	tagsJSON, err := json.Marshal(diskTags)
	if err != nil {
		return cpierrors.Cloud("set_disk_metadata: marshal tag payload: %s", err.Error())
	}
	raw[sentinelKeyDiskTags] = json.RawMessage(tagsJSON)

	newDesc, err := pve.RenderSentinel(nonBoshDesc, raw)
	if err != nil {
		return cpierrors.Cloud("set_disk_metadata: render description sentinel: %s", err.Error())
	}

	vmidStr := fmt.Sprintf("%d", vmid)
	updateParams := &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
		Tags:        &mergedTags,
	}
	if updateErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr, updateParams); updateErr != nil {
		return cpierrors.Wrap(pve.WrapError(updateErr),
			fmt.Sprintf("set_disk_metadata: UpdateQemuConfig tags (node=%s vmid=%d)", node, vmid),
		)
	}
	return nil
}
