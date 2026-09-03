package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// ============================================================================
// Director-UUID ref API (D3).
//
// This section tracks references by BOSH director UUID (a SET, stored as
// stemcellProvenance.DirectorRefs) — the correct unit of refcounting once a
// single path CID can be created by, and deleted by, multiple directors
// sharing the same cluster.
// ============================================================================

// unknownDirectorRef is the shared ref value used when a caller's request
// context carries no director UUID (jsonrpc.Context.DirectorUUID == "").
// Every such caller collapses into this ONE ref: the CPI cannot distinguish
// between two different UUID-less callers, so treating them as a single
// reference is the safe (never-under-count) choice — a real destroy still
// requires every caller lacking a UUID to have deregistered.
const unknownDirectorRef = "unknown-director"

// stemcellDestroyPendingTag marks a template VM whose last-ref destroy was
// attempted and failed. A later deregisterStemcellDirectorRef call sees this
// tag and resumes the destroy directly instead of re-running refs logic
// against a template whose provenance may already report an empty ref set.
const stemcellDestroyPendingTag = "bosh-destroy-pending"

// ErrStemcellTemplateGone is returned by registerStemcellDirectorRef when the
// target template VM no longer exists (PVE Config returns not-found). The
// caller's cache-template lookup raced a concurrent destroy; the correct
// response is to rebuild the cache template and retry, not to treat this as a
// generic failure.
var ErrStemcellTemplateGone = errors.New("stemcell_refs: cache template no longer exists")

// registerStemcellDirectorRef adds directorUUID to the template's DirectorRefs
// set, stamps the corresponding "director--<uuid>" VM tag, and clears a stale
// stemcellDestroyPendingTag if present (a re-registration cancels a previously
// failed destroy attempt — the template is alive again).
//
// Failures here are HARD errors: the director-UUID ref set is the sole
// source of truth for whether this cache template may be destroyed, so a
// silently-dropped registration would let a later delete_stemcell from a
// DIFFERENT director destroy a template this caller still depends on.
//
// The read-modify-write runs under the per-VMID cluster lock (withVMIDLock)
// so a concurrent register/deregister on the same template cannot interleave.
//
// Return values:
//   - nil: directorUUID (or unknownDirectorRef, see below) is now present in
//     DirectorRefs, the "director--<uuid>" tag is present, and
//     stemcellDestroyPendingTag is absent. A no-op call (all three already
//     true) still returns nil without issuing an API write.
//   - ErrStemcellTemplateGone: the template VM no longer exists.
//   - any other error: PVE API failure (read, merge, or write), wrapped with
//     context; also returned when withVMIDLock cannot acquire the lock.
//
// directorUUID == "" is accepted: the caller could not determine the BOSH
// director UUID for this request (jsonrpc.Context.DirectorUUID unset — a
// tool, script, or CPI API version that predates director-UUID context). The
// registration proceeds under the shared unknownDirectorRef sentinel and a
// Warn is logged so operators can see how many "callers with no identity" are
// sharing that one ref slot.
func registerStemcellDirectorRef(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	directorUUID string,
) error {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return cpierrors.Cloud("registerStemcellDirectorRef: PVE client unavailable")
	}
	if node == "" {
		return cpierrors.Cloud("registerStemcellDirectorRef: node must not be empty")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("registerStemcellDirectorRef: vmid must be a positive integer, got %d", vmid)
	}

	ref := resolveDirectorRef(directorUUID, logger, "registerStemcellDirectorRef", vmid, node)

	pools := deps.PVE.Pools()
	vmidInt := int(vmid) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	lockOwner := fmt.Sprintf("stemcell/director_ref/register/%d", vmid)

	return withVMIDLock(ctx, pools, vmidInt, lockOwner, logger, func() error {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmidInt)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) {
				return ErrStemcellTemplateGone
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("registerStemcellDirectorRef: read config vmid %d node %q", vmid, node))
		}

		description := stringConfigField(cfg, "description")
		currentTags := parseTagsField(stringConfigField(cfg, jsonKeyTags))

		prov, _ := parseStemcellProvenanceFromDescription(description)
		refAdded := !stringSliceContains(prov.DirectorRefs, ref)
		if refAdded {
			prov.DirectorRefs = append(prov.DirectorRefs, ref)
		}

		// Always attempt the merge (even when refAdded is false) so a
		// template whose description cannot be parsed as a JSON object fails
		// registration loudly rather than silently understating refs.
		newDesc, mergeErr := mergeProvenanceIntoDescription(description, prov)
		if mergeErr != nil {
			return cpierrors.Wrap(mergeErr,
				fmt.Sprintf("registerStemcellDirectorRef: merge description vmid %d node %q", vmid, node))
		}

		directorTag := "director--" + sanitizeTagValue(ref)
		newTags, tagAdded := stringSliceEnsure(currentTags, directorTag)
		newTags, pendingRemoved := stringSliceRemove(newTags, stemcellDestroyPendingTag)

		if !refAdded && !tagAdded && !pendingRemoved {
			// Fully idempotent: nothing to write.
			return nil
		}

		params := &sdknodes.UpdateQemuConfigParams{}
		if refAdded {
			params.Description = &newDesc
		}
		if tagAdded || pendingRemoved {
			joined := strings.Join(newTags, pveTagJoinSep)
			params.Tags = &joined
		}

		vmidStr := strconv.FormatInt(vmid, 10)
		if updErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr, params); updErr != nil {
			return cpierrors.Wrap(pve.WrapError(updErr),
				fmt.Sprintf("registerStemcellDirectorRef: write config vmid %d node %q", vmid, node))
		}
		return nil
	})
}

// deregisterStemcellDirectorRef removes directorUUID from the template's
// DirectorRefs set and destroys the template when the set becomes empty. All
// steps run under the per-VMID cluster lock (withVMIDLock).
//
// destroy is called (at most once) to perform the actual template VM
// deletion; the caller supplies it (typically a closure over
// destroyTemplateVM) so this function stays testable without a real PVE
// client and so callers can vary destroy behavior (e.g. purge options)
// without this function needing to know about them.
//
// Lock envelope: destroy is a purge-destroy of a template VM plus
// AwaitTaskWithLogger — routinely tens of seconds on real storage, and the
// caller's closure typically also sweeps every replica in the same call.
// That is far longer than vmidLockTTL (30s), so destroy runs AFTER the
// per-VMID lock (withVMIDLock) is released — the lock is held only for the
// fast read-modify-write that decides whether this was the last ref. Ordering
// (the fix for the historical trapdoor bug where refs were cleared BEFORE a
// destroy that then failed, leaving an undeletable, ref-less template): when
// the removed ref is the LAST one, the ref set itself is NEVER separately
// rewritten as empty — the only write made inside the lock for that case is
// the stemcellDestroyPendingTag stamp, the crash-safety marker that lets a
// process death between lock release and destroy completion resume cleanly
// via branch (b) below instead of re-deriving "last ref" against a template
// whose destroy PVE may already have queued.
//
// Control flow (see the numbered steps in the function body):
//
//	(a) Config read 404 → template already gone. (true, nil, nil) — idempotent
//	    success; there is nothing left to deregister.
//	(b) Tags contain stemcellDestroyPendingTag → a previous call already
//	    decided last-ref (this call, or an earlier one) and stamped the
//	    marker before running (or completing) destroy. Skip refs logic
//	    entirely (the ref set on a doomed template is not trustworthy input)
//	    and resume destroy directly after the lock releases.
//	(c) Description is not parseable JSON → conservative: never destroy on
//	    unknown state. (false, nil, nil), Warn logged.
//	(d) directorUUID (or unknownDirectorRef) removed from DirectorRefs; other
//	    refs remain → merge-preserving write of the updated provenance.
//	    (false, remaining, nil). destroy is NOT called.
//	(e) The removed ref was the last one → stamp stemcellDestroyPendingTag
//	    (the description/refs are left untouched) and run destroy(ctx) once
//	    the lock releases. Success → (true, nil, nil). Failure → (false, nil,
//	    err); the pending tag is already stamped so a retry resumes via (b).
//
// directorUUID == "" is accepted with the same unknownDirectorRef collapse
// and Warn as registerStemcellDirectorRef.
func deregisterStemcellDirectorRef(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	directorUUID string,
	destroy func(context.Context) error,
) (destroyed bool, remaining []string, err error) {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return false, nil, cpierrors.Cloud("deregisterStemcellDirectorRef: PVE client unavailable")
	}
	if node == "" {
		return false, nil, cpierrors.Cloud("deregisterStemcellDirectorRef: node must not be empty")
	}
	if vmid <= 0 {
		return false, nil, cpierrors.Cloud("deregisterStemcellDirectorRef: vmid must be a positive integer, got %d", vmid)
	}
	if destroy == nil {
		return false, nil, cpierrors.Cloud("deregisterStemcellDirectorRef: destroy callback must not be nil")
	}

	ref := resolveDirectorRef(directorUUID, logger, "deregisterStemcellDirectorRef", vmid, node)

	pools := deps.PVE.Pools()
	vmidInt := int(vmid) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	lockOwner := fmt.Sprintf("stemcell/director_ref/deregister/%d", vmid)

	// needDestroy/resumed are decided inside the locked read-modify-write
	// below; destroy itself is called AFTER withVMIDLock returns (see the
	// lock-envelope doc comment above).
	var needDestroy, resumed bool

	lockErr := withVMIDLock(ctx, pools, vmidInt, lockOwner, logger, func() error {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmidInt)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) {
				// (a) Template already gone — idempotent success.
				destroyed = true
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("deregisterStemcellDirectorRef: read config vmid %d node %q", vmid, node))
		}

		description := stringConfigField(cfg, "description")
		currentTags := parseTagsField(stringConfigField(cfg, jsonKeyTags))

		if stringSliceContains(currentTags, stemcellDestroyPendingTag) {
			// (b) A previous call already stamped the pending marker before
			// destroy ran (or completed). The ref set on a template already
			// marked for destroy is not trustworthy input — do not touch it;
			// nothing more to write here, destroy resumes after unlock.
			resumed = true
			needDestroy = true
			return nil
		}

		prov, ok := parseStemcellProvenanceFromDescription(description)
		if !ok {
			// (c) Conservative rule: unparseable description, unknown ref
			// count. Never destroy on unknown state.
			warnStemcellDescriptionNotJSON(logger, "deregisterStemcellDirectorRef", vmid, node)
			return nil
		}

		// (d)/(e) Remove ref from the set.
		updatedRefs := removeStringFromSlice(prov.DirectorRefs, ref)

		if len(updatedRefs) > 0 {
			// (d) Other refs remain — persist the updated set, do not destroy.
			writeErr := persistRemainingDirectorRefs(ctx, deps, node, vmid, description, prov, updatedRefs)
			if writeErr != nil {
				return writeErr
			}
			remaining = updatedRefs
			return nil
		}

		// (e) Last ref removed. The description (DirectorRefs included) is
		// left exactly as read — never rewritten as empty. The only write
		// made here is the pending-destroy tag, the prerequisite for running
		// destroy safely once this lock releases; a failure to stamp it is a
		// hard error (unlike the historical best-effort stamp on the FAILURE
		// path) because it is the crash-safety net destroy is about to run
		// without, not a nice-to-have applied after the fact.
		if stampErr := stampDestroyPendingTagLocked(ctx, deps, node, vmid, currentTags); stampErr != nil {
			return stampErr
		}
		needDestroy = true
		return nil
	})

	if lockErr != nil {
		return false, nil, lockErr
	}
	if !needDestroy {
		return destroyed, remaining, nil
	}

	// Destroy runs OUTSIDE the per-VMID lock — see the lock-envelope doc
	// comment above for why. label distinguishes the two callers of destroy
	// in the wrapped error only; the underlying operation is identical.
	if destroyErr := destroy(ctx); destroyErr != nil {
		label := "destroy last-ref template"
		if resumed {
			label = "resume pending destroy"
		}
		return false, nil, cpierrors.Wrap(pve.WrapError(destroyErr),
			fmt.Sprintf("deregisterStemcellDirectorRef: %s vmid %d node %q", label, vmid, node))
	}
	return true, nil, nil
}

// directorRefOrSentinel returns directorUUID unchanged when non-empty, or the
// shared unknownDirectorRef sentinel when it is empty. This is the single
// substitution rule every DirectorRefs producer/consumer must share: a
// template's ref set may be seeded (buildStemcellProvenanceNotesPath, at
// template-create time) or mutated (registerStemcellDirectorRef,
// deregisterStemcellDirectorRef) from different call sites, and a seed that
// stores the raw empty string instead of this sentinel can never be removed
// by a later deregister call resolving the same UUID-less caller to
// unknownDirectorRef — the "" entry would linger in DirectorRefs forever,
// keeping the template permanently un-destroyable.
func directorRefOrSentinel(directorUUID string) string {
	if directorUUID != "" {
		return directorUUID
	}
	return unknownDirectorRef
}

// resolveDirectorRef is directorRefOrSentinel plus a Warn (naming the caller)
// logged only when the substitution actually happened — used by
// registerStemcellDirectorRef and deregisterStemcellDirectorRef, which both
// have a vmid/node to log against. Callers seeding provenance at
// template-create time (no vmid yet) use directorRefOrSentinel directly.
func resolveDirectorRef(directorUUID string, logger *log.Logger, caller string, vmid int64, node string) string {
	ref := directorRefOrSentinel(directorUUID)
	if directorUUID == "" && logger != nil {
		logger.Warn(caller+": request context carried no director UUID; collapsing into shared sentinel ref",
			log.String("sentinel_ref", unknownDirectorRef),
			log.Int64("vmid", vmid),
			log.String("node", node),
		)
	}
	return ref
}

// warnStemcellDescriptionNotJSON logs the conservative-rule Warn shared by
// deregisterStemcellDirectorRef's non-JSON-description branch.
func warnStemcellDescriptionNotJSON(logger *log.Logger, caller string, vmid int64, node string) {
	if logger != nil {
		logger.Warn(caller+": template description is not JSON (conservative: not destroying)",
			log.Int64("vmid", vmid),
			log.String("node", node),
		)
	}
}

// removeStringFromSlice returns a new slice containing every element of s
// except those equal to want, preserving order.
func removeStringFromSlice(s []string, want string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v != want {
			out = append(out, v)
		}
	}
	return out
}

// persistRemainingDirectorRefs writes prov (with DirectorRefs set to
// updatedRefs) back to the template's description via the merge-preserving
// codec. Used by deregisterStemcellDirectorRef's (d) branch: other refs
// remain, so the template survives with an updated ref set.
func persistRemainingDirectorRefs(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int64,
	description string,
	prov stemcellProvenance,
	updatedRefs []string,
) error {
	prov.DirectorRefs = updatedRefs
	newDesc, mergeErr := mergeProvenanceIntoDescription(description, prov)
	if mergeErr != nil {
		return cpierrors.Wrap(mergeErr,
			fmt.Sprintf("deregisterStemcellDirectorRef: merge description vmid %d node %q", vmid, node))
	}
	vmidStr := strconv.FormatInt(vmid, 10)
	if updErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr,
		&sdknodes.UpdateQemuConfigParams{Description: &newDesc}); updErr != nil {
		return cpierrors.Wrap(pve.WrapError(updErr),
			fmt.Sprintf("deregisterStemcellDirectorRef: write description vmid %d node %q", vmid, node))
	}
	return nil
}

// stampDestroyPendingTagLocked stamps stemcellDestroyPendingTag onto the
// template's tags. Called from inside deregisterStemcellDirectorRef's lock
// for branch (e) (the removed ref was the last one), BEFORE destroy runs —
// destroy itself executes after the lock releases (see the lock-envelope doc
// comment on deregisterStemcellDirectorRef), so this stamp is the
// crash-safety marker that lets a process death between lock release and
// destroy completion resume via branch (b) instead of re-deriving "last ref"
// against a template whose destroy PVE may already have queued.
//
// A no-op (no write) when the tag is already present. Any write failure is
// returned as a hard error: an un-stamped template about to be destroyed
// outside the lock has no crash-safety net, so the caller must not proceed
// to destroy.
func stampDestroyPendingTagLocked(ctx context.Context, deps Deps, node string, vmid int64, currentTags []string) error {
	newTags, tagAdded := stringSliceEnsure(currentTags, stemcellDestroyPendingTag)
	if !tagAdded {
		return nil
	}
	joined := strings.Join(newTags, pveTagJoinSep)
	vmidStr := strconv.FormatInt(vmid, 10)
	if tagErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr,
		&sdknodes.UpdateQemuConfigParams{Tags: &joined}); tagErr != nil {
		return cpierrors.Wrap(pve.WrapError(tagErr),
			fmt.Sprintf("deregisterStemcellDirectorRef: stamp pending-destroy tag vmid %d node %q", vmid, node))
	}
	return nil
}

// pveTagJoinSep is the separator used when re-serializing a PVE tags slice
// back into the stored string form. ";" is PVE's canonical separator
// (parseTagsField/splitPVETags accept "," too, normalizing it to ";").
const pveTagJoinSep = ";"

// stringConfigField reads key from a QEMU config map (as returned by
// QEMU().Config) as a string via pve.ConfigString (tolerant of PVE
// rendering a spec-string field as a JSON number or bool), returning ""
// when the key is absent or holds a non-scalar value.
func stringConfigField(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	s, _ := pve.ConfigString(cfg, key)
	return s
}

// stringSliceContains reports whether want is present in s (exact match).
func stringSliceContains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// stringSliceEnsure appends tag to tags when not already present, returning
// the (possibly unchanged) slice and whether an addition was made. The input
// slice is never mutated in place — a fresh backing array is used when an
// addition occurs, so callers holding a reference to the original slice are
// unaffected.
func stringSliceEnsure(tags []string, tag string) (result []string, added bool) {
	if stringSliceContains(tags, tag) {
		return tags, false
	}
	out := make([]string, len(tags), len(tags)+1)
	copy(out, tags)
	out = append(out, tag)
	return out, true
}

// stringSliceRemove drops every occurrence of tag from tags, returning the
// filtered slice and whether anything was removed.
func stringSliceRemove(tags []string, tag string) (result []string, removed bool) {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t == tag {
			removed = true
			continue
		}
		out = append(out, t)
	}
	return out, removed
}

// provenanceOwnedJSONKeys lists every top-level JSON key the stemcellProvenance
// struct can serialize (its full `json:"..."` tag set, options stripped).
// mergeProvenanceIntoDescription uses this list to delete stale owned keys
// from the foreign-preserved base map before overlaying the freshly
// marshaled provenance — otherwise a field that now serializes as absent
// (e.g. an emptied DirectorRefs, which carries omitempty) would leave its old
// value behind from the prior write instead of actually clearing.
//
// "stemcell_refs" and "director_id" are NOT current stemcellProvenance
// fields — they are retired JSON keys from an earlier CID-CSV-keyed refcount
// design. They stay listed so mergeProvenanceIntoDescription still scrubs
// them out of any description written by that earlier design, or hand-edited
// by an operator carrying old habits, rather than letting a stale value
// persist forever alongside the current director-UUID-keyed fields.
var provenanceOwnedJSONKeys = []string{
	metadataKeyName, jsonKeyVersion, "os_type", "disk_format", "sha8", "source",
	"director_id", "created", jsonKeySHA256, "kind", "cid", "created_by",
	"director_refs", "stemcell_refs", "director_tags",
}

// jsonKeySHA256 is the stemcellProvenance.SHA256 JSON tag, named once so the
// literal stays under the goconst occurrence cap (it already recurs across
// create_stemcell.go's sha256 logging call sites).
const jsonKeySHA256 = "sha256"

// jsonKeyVersion is the stemcellProvenance.Version JSON tag, named once so
// the literal stays under the goconst occurrence cap (it recurs across
// stemcell cloud-properties parsing and logging call sites package-wide).
const jsonKeyVersion = "version"

// mergeProvenanceIntoDescription writes prov into desc while preserving every
// top-level JSON key desc holds that stemcellProvenance does not own (an
// operator or another tool may have added arbitrary keys to the description
// JSON). This fixes the historical bug where a straight
// json.Marshal(stemcellProvenance) write clobbered any such foreign key.
//
// Algorithm (mirrors the pve.ParseSentinel/RenderSentinel key-preserving
// pattern used for VM/disk metadata sentinels, adapted for a description that
// is itself a bare JSON object rather than a "<!--BOSH:{...}-->"-wrapped one):
//  1. Unmarshal desc into a generic map. An empty desc is treated as an empty
//     object (nothing to preserve, not an error).
//  2. Marshal prov and unmarshal that into a second map — this is exactly the
//     key set stemcellProvenance would produce on its own, honoring omitempty.
//  3. Delete every key in provenanceOwnedJSONKeys from the base map (clears
//     stale owned values that no longer appear in step 2's map, e.g. a
//     DirectorRefs that emptied out).
//  4. Copy every key from step 2's map into the base map (installs the fresh
//     provenance values).
//  5. Marshal the merged map.
//
// Returns an error when desc is non-empty and not a JSON object (arrays,
// scalars, or malformed JSON) — callers apply their own conservative-vs-hard
// error policy for that case; this function only performs the merge.
func mergeProvenanceIntoDescription(desc string, prov stemcellProvenance) (string, error) {
	base := make(map[string]json.RawMessage)
	if trimmed := strings.TrimSpace(desc); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &base); err != nil {
			return "", fmt.Errorf("mergeProvenanceIntoDescription: description is not a JSON object: %w", err)
		}
	}

	provJSON, err := json.Marshal(prov)
	if err != nil {
		return "", fmt.Errorf("mergeProvenanceIntoDescription: marshal provenance: %w", err)
	}
	provMap := make(map[string]json.RawMessage)
	if err := json.Unmarshal(provJSON, &provMap); err != nil {
		return "", fmt.Errorf("mergeProvenanceIntoDescription: unmarshal marshaled provenance: %w", err)
	}

	for _, k := range provenanceOwnedJSONKeys {
		delete(base, k)
	}
	for k, v := range provMap {
		base[k] = v
	}

	merged, err := json.Marshal(base)
	if err != nil {
		return "", fmt.Errorf("mergeProvenanceIntoDescription: marshal merged description: %w", err)
	}
	return string(merged), nil
}

// templateHomeNode names the node create_stemcell most likely built the cache
// template on: stemcell_template_node when configured, the CPI's configured
// node otherwise. Best effort (an owning-node retarget of the stemcell
// storage, a cloud_properties node pin, or adoption of an existing template
// can all place it elsewhere), so callers use it only as a probe target,
// never as an exclusive answer: create_vm's settled re-check skips one
// authoritative probe on a wrong guess, and delete_stemcell falls back to it
// only when cluster node enumeration itself fails.
func templateHomeNode(deps Deps) string {
	if deps.Config == nil {
		return ""
	}
	if deps.Config.StemcellTemplateNode != "" {
		return deps.Config.StemcellTemplateNode
	}
	return deps.Config.Node
}
