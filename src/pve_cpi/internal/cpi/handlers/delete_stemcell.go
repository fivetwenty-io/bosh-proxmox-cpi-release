package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// HandleDeleteStemcell returns a Handler for the BOSH CPI delete_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] stemcell_cid string — the path-identity CID returned by create_stemcell.
//	    Accepted forms (pve.ParseStemcellPathCID):
//	      ":light:<storage>:import/<filename>"  operator-managed qcow2
//	      ":heavy:<storage>:import/<filename>"  CPI-uploaded qcow2
//	    Every retired grammar (bare "<storage>:import/...", "light:...",
//	    "template:<vmid>", bare integers) is REJECTED as a hard parse error —
//	    this is a pre-release cutover with no backward compatibility; those
//	    CIDs cannot exist post-deploy.
//
// Returns: null / void on success.
//
// Reference model (D3/D10): a stemcell's PVE-side identity is the qcow2 file
// named by the CID. The template VM that clones from it is a per-cluster
// CACHE, keyed by the qcow2's content sha8 tag ("bosh-stemcell-sha-<sha8>"),
// discovered cluster-wide (not node-scoped) so create-side dedup and
// delete-side teardown always agree on what cache templates exist. Multiple
// BOSH directors sharing this cluster can each hold a live reference to the
// same cache template; the reference set is a director-UUID SET stored in the
// template's description JSON (stemcellProvenance.DirectorRefs, see
// stemcell_refs.go's registerStemcellDirectorRef / deregisterStemcellDirectorRef).
//
// Policy table (D10):
//
//	kind    cache template (primary + replicas)      qcow2 file
//	------  ---------------------------------------   ------------------------
//	light   destroyed when THIS director's removal    NEVER deleted — the
//	        empties the ref set (last ref in cluster)  operator owns the file
//	heavy   destroyed when THIS director's removal     deleted when the cache
//	        empties the ref set (last ref in cluster)  template destroy above
//	                                                    completes (same call)
//	(unparseable CID)  —                                hard cloud error
//
// Deletion order per call (steps, see the numbered comments in the code
// below):
//
//  1. Parse the path CID → (kind, storage, volumePath). Parse failure is a
//     hard, non-retriable cloud error — no legacy fallback exists.
//  2. Extract the content sha8 from the CID's filename tail
//     ("-<8hex>.qcow2"). Unextractable → Warn and treat as "no cache
//     template found" (falls through to step 6).
//  3. Cluster-scoped lookup of every cache template carrying
//     "bosh-stemcell-sha-<sha8>" (pve.FindTemplatesBySHATagCluster). Zero
//     matches → an authoritative per-node sweep of every cluster node
//     (findStemcellTemplatesAllNodesBestEffort), since a cluster-index miss
//     can be lag rather than absence. Zero matches from both → step 6
//     (idempotent retry / already-destroyed convergence).
//  4. One or more matches (from either the cluster lookup or the per-node
//     sweep): the lowest-VMID entry that is NOT a per-node
//     replica (pve.TemplateRef.IsReplica) is the ref-anchor; every other
//     match (replica or not) is swept alongside it. Anchoring must skip
//     replicas — a replica's provenance ref set is a fossil of its own
//     creation (see the replication feature in create_stemcell.go), and a
//     replica can carry a LOWER VMID than the actual anchor when it was
//     built after the anchor by a different director, so "lowest VMID" alone
//     is not a safe anchor rule. If every match is a replica there is no
//     anchor to deregister against — treated as step 6 (no-template
//     semantics; see selectStemcellAnchor). Otherwise
//     deregisterStemcellDirectorRef removes this request's director UUID
//     from the anchor's ref set under a per-VMID cluster lock (held only for
//     the fast read-modify-write); when that was the LAST ref, destroy runs
//     after the lock releases, still BEFORE the ref set is ever rewritten as
//     empty (the trapdoor fix — a failed destroy leaves the template's
//     provenance untouched, retried next call via the bosh-destroy-pending
//     marker). The destroy closure destroys the anchor first, then sweeps
//     every other match best-effort.
//  5. Destroy outcome:
//     - refs remain (destroyed=false, err=nil): Info log, return success. The
//     qcow2 file is untouched for BOTH kinds.
//     - destroy failed and PVE reports the template's base volume is still
//     in use by a linked clone (pve.IsBaseVolumeInUse): a clear,
//     non-retriable cloud error naming the template VMID/node and
//     instructing the operator to delete or migrate the dependent VM(s);
//     a bosh-destroy-pending marker is already stamped, so a retry
//     resumes the destroy directly.
//     - any other destroy/deregister error: wrapped and returned, retriable
//     classification preserved from pve.WrapError.
//     - destroyed=true: continue to step 6 (kind-specific qcow2 lifecycle).
//  6. qcow2 file lifecycle, evaluated for BOTH "templates found and
//     destroyed" and "no templates found" outcomes (the second case is the
//     idempotent-retry / already-destroyed convergence path — a heavy
//     stemcell whose cache template is already gone still needs its qcow2
//     removed if a prior call died between destroy and file delete):
//     - light: never deleted. Info log only.
//     - heavy: DeleteVolumeIfExists on the primary's node (hard error on
//     failure — the director retries delete_stemcell, and the whole call
//     is idempotent, so a hard error here is how retry convergence
//     happens). Every replica's node gets a best-effort delete of the
//     same storage:volumePath (Warn on failure, never fails the call —
//     each replica's local-storage qcow2 copy is a best-effort cleanup).
//  7. Opt-in orphan prune (deps.Config.StemcellOrphanPruneEnabled()), run at
//     the end of BOTH the templates-found-and-destroyed branch and the
//     no-cache-template branch (step 6): cluster-wide sweep for template VMs
//     tagged both stemcellMarkerTag and "director--<this request's director
//     UUID>" — catches templates this director created but never released
//     (e.g. a create_stemcell that crashed after registering a ref for a
//     template it then abandoned). Director UUID comes from THIS request's
//     context (jsonrpc.Context), never from config — an empty UUID skips the
//     prune with a Warn rather than falling back to any shared sentinel (an
//     unscoped prune could destroy another caller's abandoned template). Each
//     candidate's own provenance director_refs is read before it is
//     destroyed — a candidate is pruned only when that set is empty or
//     contains exactly this director's UUID, and never when its VMID is one
//     this call already handled above (excludeVMIDs); the marker tag alone
//     is not sufficient (nothing ever removes it on deregistration) and an
//     unfiltered destroy can hit a template this director still actively
//     references for a DIFFERENT stemcell.
func HandleDeleteStemcell(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// Step 1: parse arg 0 as a path-identity stemcell CID.
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_stemcell: missing required argument stemcell_cid")
		}
		var cidStr string
		if err := json.Unmarshal(args[0], &cidStr); err != nil || cidStr == "" {
			return nil, cpierrors.Cloud("delete_stemcell: stemcell_cid must be a non-empty string")
		}

		kind, storage, volumePath, parseErr := pve.ParseStemcellPathCID(cidStr)
		if parseErr != nil {
			return nil, cpierrors.Cloud("delete_stemcell: invalid stemcell CID %q: %s", cidStr, parseErr.Error())
		}

		logger := deps.Log(ctx).With(
			log.String("stemcell_cid", cidStr),
			log.String("kind", string(kind)),
			log.String("storage", storage),
			log.String("volume_path", volumePath),
		)

		// ----------------------------------------------------------------
		// Step 2: extract sha8 from the CID's filename tail.
		// ----------------------------------------------------------------
		sha8, sha8OK := extractSHA8FromPathCIDVolume(volumePath)
		if !sha8OK {
			logger.Warn("delete_stemcell: sha8 unextractable from CID filename; skipping cache-template resolution")
		}

		// ----------------------------------------------------------------
		// Step 3: cluster-scoped cache template lookup.
		// ----------------------------------------------------------------
		var refs []pve.TemplateRef
		if sha8OK {
			refs, err = pve.FindTemplatesBySHATagCluster(ctx, deps.PVE, sha8)
			if err != nil {
				return nil, cpierrors.Wrap(pve.WrapError(err), "delete_stemcell: cluster template lookup")
			}
		}

		// A cluster-scoped miss can be /cluster/resources index lag rather
		// than a true absence: a template create_stemcell froze moments ago is
		// invisible there for the lag window (observed at minutes on loaded
		// clusters), and taking the no-template branch then deletes the qcow2
		// while orphaning the live template. Before concluding no-template,
		// read every node's guest listing authoritatively (per-node listings,
		// no index involved). Sweeping all nodes rather than guessing the
		// template's home matters: create_stemcell can build on a node other
		// than the configured one (owning-node retarget of a node-restricted
		// staging pool, a cloud_properties node pin, or adoption of an
		// existing template found elsewhere), and delete_stemcell's
		// destructive branches must see replicas on every node so this
		// with-template path stays equivalent to the cluster-listing one.
		if sha8OK && len(refs) == 0 {
			if nodeRefs := findStemcellTemplatesAllNodesBestEffort(ctx, deps, logger, sha8); len(nodeRefs) > 0 {
				logger.Info("delete_stemcell: cache template found by authoritative per-node "+
					"listings after a cluster-index miss",
					log.String("sha8", sha8),
					log.Int("match_count", len(nodeRefs)),
				)
				refs = nodeRefs
			}
		}

		if len(refs) == 0 {
			// Step 6 (no-template branch): already destroyed, or a retry
			// after a previous call died mid-way. Idempotent convergence.
			return handleDeleteStemcellNoTemplate(ctx, deps, logger, kind, storage, volumePath, cidStr, reqCtx.DirectorUUID)
		}

		// Step 4 anchor selection: the lowest-VMID NON-replica match is the
		// ref-anchor; every other match (replica or not) is swept alongside
		// it. See selectStemcellAnchor and the doc comment above for why a
		// replica must never be the anchor.
		anchor, sweep, ok := selectStemcellAnchor(refs)
		if !ok {
			logger.Warn("delete_stemcell: every cache template matching this content sha8 is a per-node replica; "+
				"none carries a live director-ref anchor — falling through to no-template semantics",
				log.Int("candidate_count", len(refs)),
			)
			return handleDeleteStemcellNoTemplate(ctx, deps, logger, kind, storage, volumePath, cidStr, reqCtx.DirectorUUID)
		}

		// Steps 4-7 (templates-found branch): deregister the director ref,
		// destroy the anchor + swept templates when that was the last ref,
		// run the kind-specific qcow2 cleanup, and finish with the opt-in
		// orphan prune.
		return handleDeleteStemcellWithTemplate(
			ctx, deps, logger, kind, storage, volumePath, cidStr, anchor, sweep, reqCtx.DirectorUUID)
	})
}

// selectStemcellAnchor picks the director-ref anchor and the sweep set (every
// other match to destroy alongside it) from a cluster-scoped sha8 lookup, in
// refs' ascending-VMID order (FindTemplatesBySHATagCluster's contract).
//
// Replicas (pve.TemplateRef.IsReplica) never hold live director references —
// each replica's provenance DirectorRefs is a fossil of its OWN creation
// (replicateOneNode/ensureReplicaTemplateVM never register a ref against a
// replica; only the primary create-side path does). A replica can also carry
// a VMID lower than the true anchor's — VMID allocation is a random offset
// within the template band, not creation order — so "lowest VMID" alone is
// not a safe anchor rule once more than one director/replica can exist for
// the same content sha8: anchoring on a replica would consult the wrong ref
// set and can either destroy a template a DIFFERENT director still
// references, or turn delete_stemcell into a silent no-op against a ref set
// that never contained the caller.
//
// Returns (anchor, sweep, true) when at least one non-replica match exists:
// anchor is the lowest-VMID such match; sweep is every OTHER match (replica
// or not) in refs, order preserved. Returns (zero, nil, false) when every
// match is a replica — there is no anchor to deregister against. The caller
// falls through to the no-cache-template semantics: nothing is destroyed
// here (a stray replica with no live director backing it is left for the
// orphan prune or manual operator cleanup, never destroyed on the strength
// of an unrelated director's delete_stemcell call).
func selectStemcellAnchor(refs []pve.TemplateRef) (anchor pve.TemplateRef, sweep []pve.TemplateRef, ok bool) {
	anchorIdx := -1
	for i, r := range refs {
		if !r.IsReplica() {
			anchorIdx = i
			break
		}
	}
	if anchorIdx == -1 {
		return pve.TemplateRef{}, nil, false
	}
	anchor = refs[anchorIdx]
	sweep = make([]pve.TemplateRef, 0, len(refs)-1)
	for i, r := range refs {
		if i == anchorIdx {
			continue
		}
		sweep = append(sweep, r)
	}
	return anchor, sweep, true
}

// handleDeleteStemcellWithTemplate implements steps 4-7 of HandleDeleteStemcell
// for the branch where selectStemcellAnchor (step 4) found a director-ref
// anchor among the cluster-scoped lookup's (step 3) matches.
//
// anchor is the ref-anchor (lowest-VMID non-replica match); replicas is every
// other match to sweep alongside it once anchor's ref set empties (the name
// is kept from the pre-anchor-selection shape even though a member can, in principle, be a
// second non-replica match from an anomalous cluster state — it is still
// swept the same best-effort way a true replica is).
func handleDeleteStemcellWithTemplate(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	kind pve.StemcellKind,
	storage, volumePath, cidStr string,
	anchor pve.TemplateRef,
	replicas []pve.TemplateRef,
	directorUUID string,
) (any, error) {
	// ----------------------------------------------------------------
	// Step 4: anchor + sweep destroy, gated by the director-UUID ref set.
	// ----------------------------------------------------------------
	destroyFn := func(ctx context.Context) error {
		if delErr := destroyTemplateVM(ctx, deps, anchor.Node, anchor.VMID, cidStr); delErr != nil {
			return delErr
		}
		for _, r := range replicas {
			if delErr := destroyTemplateVM(ctx, deps, r.Node, r.VMID, cidStr); delErr != nil {
				logger.Warn("delete_stemcell: replica template destroy failed (best-effort, continuing)",
					log.String("node", r.Node),
					log.Int64("vmid", r.VMID),
					log.Err(delErr),
				)
			}
		}
		return nil
	}

	destroyed, remaining, refErr := deregisterStemcellDirectorRef(
		ctx, deps, logger, anchor.Node, anchor.VMID, directorUUID, destroyFn)
	if refErr != nil {
		// Step 5, IsBaseVolumeInUse branch: actionable, non-retriable
		// error. A bosh-destroy-pending marker was already stamped by
		// deregisterStemcellDirectorRef, so a retry resumes the destroy
		// directly instead of re-deriving "last ref".
		if pve.IsBaseVolumeInUse(refErr) {
			return nil, cpierrors.Cloud(
				"delete_stemcell: template VM %d (node %q) still backs one or more linked-clone VMs; "+
					"delete or migrate those VMs, then retry delete_stemcell (the destroy is marked pending and will resume): %s",
				anchor.VMID, anchor.Node, refErr.Error())
		}
		return nil, cpierrors.Wrap(refErr,
			fmt.Sprintf("delete_stemcell: deregister director ref for template VM %d node %q", anchor.VMID, anchor.Node))
	}
	if !destroyed {
		logger.Info("delete_stemcell: director refs remain; template cache preserved",
			log.Int("remaining_refs", len(remaining)),
		)
		return nil, nil
	}

	// ----------------------------------------------------------------
	// Step 6 (templates-found-and-destroyed branch): kind-specific
	// qcow2 lifecycle.
	// ----------------------------------------------------------------
	if kind == pve.StemcellKindLight {
		logger.Info("delete_stemcell: light stemcell — operator-managed qcow2 left in place")
	} else {
		if delErr := deleteHeavyQcow2Primary(ctx, deps, logger, anchor.Node, storage, volumePath); delErr != nil {
			return nil, delErr
		}
		swept := map[string]bool{anchor.Node: true}
		for _, r := range replicas {
			deleteHeavyQcow2ReplicaBestEffort(ctx, deps, logger, r.Node, storage, volumePath)
			swept[r.Node] = true
		}
		// The replica list is empty whenever stemcell_replicate_local is off,
		// yet stray per-node copies can still exist on a node-local staging
		// pool: a second CPI entry pinned to another node uploaded its own
		// copy before cluster-scoped sha-tag dedup converged on one template,
		// or the template VM migrated off its staging node, dragging
		// anchor.Node away from where the qcow2 actually lives. Sweep every
		// node not already covered above when the storage is not positively
		// shared, the same last-resort convergence the no-template branch
		// applies.
		sweepHeavyQcow2OtherNodesBestEffort(ctx, deps, logger, storage, volumePath, swept)
	}

	// ----------------------------------------------------------------
	// Step 7: opt-in orphan prune, director-scoped from THIS request's
	// context. excludeVMIDs keeps the prune from re-evaluating the
	// template(s) this call just destroyed above.
	// ----------------------------------------------------------------
	if deps.Config.StemcellOrphanPruneEnabled() {
		excludeVMIDs := make(map[int64]bool, len(replicas)+1)
		excludeVMIDs[anchor.VMID] = true
		for _, r := range replicas {
			excludeVMIDs[r.VMID] = true
		}
		pruneOrphanStemcellTemplates(ctx, deps, cidStr, directorUUID, excludeVMIDs)
	}

	return nil, nil
}

// handleDeleteStemcellNoTemplate implements step 6's no-cache-template
// branch: either the sha8 could not be extracted from the CID (step 2), both
// the cluster-scoped lookup and the authoritative per-node sweep found zero
// matches (already destroyed, or a retry after a previous call died between
// template destroy and qcow2 delete), or
// every match was a per-node replica with no anchor to deregister against
// (selectStemcellAnchor). All cases converge to the same kind-specific qcow2
// handling, followed by the same opt-in orphan prune step 6/7 the
// templates-found branch runs.
//
// The node used for the primary qcow2 delete falls back to
// deps.Config.StemcellTemplateNode, then deps.Config.Node — there is no
// template VM left to read a node from.
//
// cidStr and directorUUID are threaded through only for the orphan prune
// (step 7) — no director-ref deregistration happens on this branch (there is
// no template to hold a ref set).
func handleDeleteStemcellNoTemplate(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	kind pve.StemcellKind,
	storage, volumePath, cidStr, directorUUID string,
) (any, error) {
	if kind == pve.StemcellKindLight {
		logger.Info("delete_stemcell: light stemcell — no cache template found; nothing to do (operator-managed qcow2)")
		return nil, nil
	}

	node := deps.Config.StemcellTemplateNode
	if node == "" {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("delete_stemcell: config.node must not be empty")
	}

	logger.Info("delete_stemcell: heavy stemcell — no cache template found; attempting idempotent qcow2 delete for retry convergence",
		log.String("node", node),
	)
	if delErr := deleteHeavyQcow2Primary(ctx, deps, logger, node, storage, volumePath); delErr != nil {
		return nil, delErr
	}

	// With stemcell_replicate_local, every cluster node held its own copy of
	// this qcow2 under the same storage:import/<file> path — the primary
	// delete above only removed node's copy. Without a cache template to
	// read the replica node list from (this IS the no-template branch), the
	// only signal available is the storage's own shared-ness: when it is NOT
	// positively known to be shared, best-effort delete the same volume on
	// every OTHER cluster node too. On genuinely shared storage the primary
	// delete already removed the cluster's only copy, and on a single-node
	// cluster the sweep is a same-node no-op.
	sweepHeavyQcow2OtherNodesBestEffort(ctx, deps, logger, storage, volumePath, map[string]bool{node: true})

	if deps.Config.StemcellOrphanPruneEnabled() {
		// No template was destroyed on this branch — nothing to exclude.
		pruneOrphanStemcellTemplates(ctx, deps, cidStr, directorUUID, nil)
	}

	return nil, nil
}

// deleteHeavyQcow2Primary deletes the qcow2 volume on node via
// Storage().DeleteVolumeIfExists, retrying transient/lock errors. This is the
// hard-error path (unlike deleteHeavyQcow2ReplicaBestEffort): a failure here
// is returned to the caller so the BOSH Director retries delete_stemcell,
// which is idempotent — a repeat call either finds the volume already gone
// (existed=false, no error) or retries the same delete.
func deleteHeavyQcow2Primary(ctx context.Context, deps Deps, logger *log.Logger, node, storage, volumePath string) error {
	var existed bool
	err := pve.RetryOnTransientOrLock(ctx, logger, "delete_stemcell.delete_qcow2", 0, func() error {
		var innerErr error
		existed, innerErr = deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath)
		return innerErr
	})
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("delete_stemcell: delete qcow2 volume %q on storage %q node %q", volumePath, storage, node))
	}
	if !existed {
		logger.Warn("delete_stemcell: qcow2 volume not found on primary node (already deleted or never existed)",
			log.String("node", node),
			log.String("storage", storage),
			log.String("volume", volumePath),
		)
		return nil
	}
	logger.Info("delete_stemcell: qcow2 volume deleted on primary node",
		log.String("node", node),
	)
	return nil
}

// findStemcellTemplatesAllNodesBestEffort probes every cluster node's
// authoritative guest listing for generation-gated cache templates carrying
// the sha tag, merged in ascending-VMID order (the anchor-selection contract).
// It backstops a cluster-scoped lookup that returned nothing: per-node
// listings read node-local guest configs and do not lag the way
// /cluster/resources does. Best effort throughout: node-enumeration failure
// degrades to probing the template's presumed home node, and a single node's
// probe failure warns and continues with the remaining nodes, since the
// caller treats an empty answer as the (idempotent) no-template branch.
func findStemcellTemplatesAllNodesBestEffort(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	sha8 string,
) []pve.TemplateRef {
	nodes, listErr := listClusterNodes(ctx, deps)
	if listErr != nil || len(nodes) == 0 {
		logger.Warn("delete_stemcell: cluster node enumeration returned no nodes; "+
			"probing the template's presumed home node only",
			log.Err(listErr),
		)
		if home := templateHomeNode(deps); home != "" {
			nodes = []string{home}
		}
	}
	var all []pve.TemplateRef
	for _, node := range nodes {
		nodeRefs, probeErr := pve.FindTemplatesBySHATagNode(ctx, deps.PVE, node, sha8)
		if probeErr != nil {
			logger.Warn("delete_stemcell: authoritative per-node template probe failed; "+
				"continuing with the remaining nodes",
				log.String("node", node),
				log.String("sha8", sha8),
				log.Err(probeErr),
			)
			continue
		}
		all = append(all, nodeRefs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].VMID < all[j].VMID })
	return all
}

// sweepHeavyQcow2OtherNodesBestEffort best-effort deletes storage:volumePath
// on every cluster node not already in skip, but only when storage is NOT
// positively classified as shared: on genuinely shared storage the primary
// delete already removed the cluster's only copy, and per-node sweeping would
// re-delete (harmlessly) or waste API calls. Unknown classification sweeps:
// convergence over economy, matching the fail-open stance of the no-template
// branch this helper was factored from. Node-enumeration failure warns and
// returns, since every delete here is best-effort by contract.
func sweepHeavyQcow2OtherNodesBestEffort(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	storage, volumePath string,
	skip map[string]bool,
) {
	if shared, known := stemcellStorageIsShared(ctx, deps, storage); known && shared {
		return
	}
	otherNodes, listErr := listClusterNodes(ctx, deps)
	if listErr != nil {
		logger.Warn("delete_stemcell: cannot enumerate cluster nodes for replica qcow2 cleanup (best-effort, continuing)",
			log.Err(listErr),
		)
		return
	}
	for _, n := range otherNodes {
		if skip[n] {
			continue
		}
		deleteHeavyQcow2ReplicaBestEffort(ctx, deps, logger, n, storage, volumePath)
	}
}

// deleteHeavyQcow2ReplicaBestEffort deletes the same storage:volumePath on a
// replica node. Replica qcow2 copies live on per-node LOCAL storage under the
// same storage ID + filename as the primary (the replication feature in
// create_stemcell.go). Failures are logged at Warn and never propagate — a
// stray replica-node copy is cleaned up by a subsequent
// pve.WithStorageScan-covered sweep or manual operator action; it must not
// block the overall delete_stemcell call from succeeding.
func deleteHeavyQcow2ReplicaBestEffort(ctx context.Context, deps Deps, logger *log.Logger, node, storage, volumePath string) {
	existed, err := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath)
	if err != nil {
		logger.Warn("delete_stemcell: replica qcow2 delete failed (best-effort, continuing)",
			log.String("node", node),
			log.String("storage", storage),
			log.String("volume", volumePath),
			log.Err(err),
		)
		return
	}
	if !existed {
		logger.Info("delete_stemcell: replica qcow2 not found (already deleted or never existed)",
			log.String("node", node),
		)
		return
	}
	logger.Info("delete_stemcell: replica qcow2 deleted",
		log.String("node", node),
	)
}

// extractSHA8FromPathCIDVolume extracts the sha8 digest from the filename
// embedded in a path-identity stemcell CID's volumePath ("import/<filename>",
// as returned by pve.ParseStemcellPathCID). Delegates to
// extractSHA8FromFilename (create_vm_disk.go) after stripping the "import/"
// prefix. Returns ("", false) when volumePath does not start with "import/"
// or the filename does not match the "...-<8hex>.qcow2" pattern.
func extractSHA8FromPathCIDVolume(volumePath string) (sha8 string, ok bool) {
	const importPrefix = "import/"
	if !strings.HasPrefix(volumePath, importPrefix) {
		return "", false
	}
	return extractSHA8FromFilename(volumePath[len(importPrefix):])
}

// pruneOrphanStemcellTemplates scans cluster-wide for candidate template VMs
// carrying both stemcellMarkerTag and "director--<sanitized directorUUID>" —
// but the tag pair alone is NOT sufficient to prune: nothing ever removes
// "director--<uuid>" on deregistration (it is a historical, write-only
// marker), so a template this director actively references for a DIFFERENT
// stemcell still carries it. Before destroying a candidate, its OWN
// provenance director_refs is read and checked
// (directorRefsAllowPrune): only a candidate whose live ref set is empty, or
// contains exactly this director's UUID and no one else's, is pruned. A
// candidate whose provenance cannot be read or parsed is skipped
// (conservative — matches deregisterStemcellDirectorRef's own
// unparseable-description rule). excludeVMIDs additionally skips every
// template this SAME delete_stemcell call already evaluated/destroyed above
// (nil/empty when nothing was destroyed, e.g. the no-cache-template branch).
//
// A VM that is a linked-clone base is skipped with a warning. All failures
// are logged; the function never returns an error. cidStr is used for log
// context only.
//
// directorUUID comes from the request context (jsonrpc.Context.DirectorUUID),
// never from config. Unlike registerStemcellDirectorRef/
// deregisterStemcellDirectorRef, an empty directorUUID does NOT collapse to
// the shared unknownDirectorRef sentinel here — a prune scoped to that shared
// sentinel could destroy another caller's abandoned template that happens to
// share the same "no identity" bucket. An empty directorUUID skips the prune
// entirely (Warn only).
func pruneOrphanStemcellTemplates(ctx context.Context, deps Deps, cidStr, directorUUID string, excludeVMIDs map[int64]bool) {
	dryRun := deps.Config.StemcellOrphanPruneDryRun()
	logger := deps.Log(ctx).With(
		log.String("operation", "delete_stemcell.orphan_prune"),
		log.String("stemcell_cid", cidStr),
		log.Bool("dry_run", dryRun),
	)

	if directorUUID == "" {
		logger.Warn("delete_stemcell: orphan prune enabled but request carried no director UUID; skipping")
		return
	}

	dirTag := "director--" + sanitizeTagValue(directorUUID)

	if deps.PVE == nil || deps.PVE.Cluster() == nil || deps.PVE.QEMU() == nil {
		logger.Warn("delete_stemcell: cluster or QEMU service unavailable; orphan prune skipped")
		return
	}

	resp, err := deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		logger.Warn("delete_stemcell: ListResources failed; orphan prune skipped", log.Err(err))
		return
	}
	if resp == nil {
		return
	}

	type clusterItem struct {
		Type string `json:"type"`
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Name string `json:"name"`
		Tags string `json:"tags"`
		// Template is decoded through clusterPruneBool, not *bool/*int — see
		// clusterPruneBool's doc comment for why a fixed numeric encoding
		// assumption silently drops or wrongly matches rows.
		Template *clusterPruneBool `json:"template"`
	}

	for _, raw := range *resp {
		var item clusterItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type != resourceTypeQemu || item.VMID == 0 {
			continue
		}
		if item.Template == nil || !bool(*item.Template) {
			continue
		}
		if excludeVMIDs[item.VMID] {
			continue
		}
		if !tagsContain(item.Tags, stemcellMarkerTag) {
			continue
		}
		if !tagsContain(item.Tags, dirTag) {
			continue
		}
		nodeLogger := logger.With(
			log.String("node", item.Node),
			log.Int("vmid", int(item.VMID)),
			log.String("name", item.Name),
		)

		vmidInt := int(item.VMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, item.Node, vmidInt)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) {
				nodeLogger.Info("delete_stemcell: orphan prune: candidate already gone (skipping)")
				continue
			}
			nodeLogger.Warn("delete_stemcell: orphan prune: cannot read candidate provenance; conservative — not pruning",
				log.Err(cfgErr),
			)
			continue
		}
		prov, ok := parseStemcellProvenanceFromDescription(stringConfigField(cfg, pveConfigKeyDescription))
		if !ok {
			nodeLogger.Warn("delete_stemcell: orphan prune: candidate description not parseable JSON; conservative — not pruning")
			continue
		}
		if !directorRefsAllowPrune(prov.DirectorRefs, directorUUID) {
			nodeLogger.Info("delete_stemcell: orphan prune: candidate carries live director refs beyond this director; skipping",
				log.Int("director_ref_count", len(prov.DirectorRefs)),
			)
			continue
		}

		if dryRun {
			nodeLogger.Info("delete_stemcell: orphan prune: would prune (dry-run)")
			continue
		}
		if delErr := destroyTemplateVM(ctx, deps, item.Node, item.VMID, cidStr); delErr != nil {
			if pve.IsBaseVolumeInUse(delErr) {
				nodeLogger.Warn("delete_stemcell: orphan prune: skip — referenced by linked clone")
			} else {
				nodeLogger.Warn("delete_stemcell: orphan prune: destroy failed (best-effort, continuing)", log.Err(delErr))
			}
		} else {
			nodeLogger.Info("delete_stemcell: orphan prune: template pruned")
		}
	}
}

// directorRefsAllowPrune reports whether a candidate template's own
// provenance director_refs permits pruning it under directorUUID's scope: an
// empty set (no live reference at all) or a set containing EXACTLY
// directorUUID and no one else. Any other non-empty set means some director
// (this one alongside another, or a different one entirely) still actively
// references the template — pruning it would destroy a live cache another
// caller depends on.
func directorRefsAllowPrune(refs []string, directorUUID string) bool {
	if len(refs) == 0 {
		return true
	}
	return len(refs) == 1 && refs[0] == directorUUID
}

// clusterPruneBool decodes the "template" field of a GET /cluster/resources
// row for the orphan-prune scan. Mirrors internal/pve's pveBool decode
// (internal/pve/template.go) locally — that type is unexported in package
// pve and unreachable from here. PVE (Perl-backed) usually renders this
// boolean as the JSON numbers 1/0, but some endpoints and most fixtures use
// the JSON literals true/false, and a handful return "1"/"0" as strings. A
// *bool decode fails on the integer form and would silently drop every
// candidate from the scan (defeating the prune entirely); a *int decode
// fails on the boolean/string forms and does the same. All of 1/0,
// true/false, "1"/"0"/"true"/"false", and null are accepted here so the scan
// degrades gracefully across PVE API versions instead of going silently
// blind to one encoding.
type clusterPruneBool bool

// UnmarshalJSON accepts 1/0, true/false, "1"/"0"/"true"/"false", and null.
func (b *clusterPruneBool) UnmarshalJSON(data []byte) error {
	switch s := strings.Trim(strings.TrimSpace(string(data)), `"`); s {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("clusterPruneBool: cannot decode %q as a PVE boolean", s)
	}
	return nil
}

// destroyTemplateVM deletes the template VM identified by vmid on node with
// purge=true (removes config-referenced disks + config backups). Idempotent: a
// 404 response from PVE means the VM is already gone; that is treated as
// success and a warning is logged. The UPID returned by DeleteQemu is awaited;
// a not-found or pmxcfs-config-missing error during the await is also treated
// as success. Any other error is returned as a cloud error.
//
// DestroyUnreferencedDisks follows pve.destroy_unreferenced_disks (default
// false) for the same reason as delete_vm's three sites: on storage shared by
// a second cluster with an overlapping VMID band, PVE would free the OTHER
// cluster's VMID-matching volumes. The template's own disks are always
// config-referenced, so the default loses nothing.
func destroyTemplateVM(ctx context.Context, deps Deps, node string, vmid int64, cidStr string) error {
	logger := deps.Log(ctx).With(
		log.String("stemcell_cid", cidStr),
		log.String("node", node),
		log.Int("vmid", int(vmid)),
	)
	logger.Info("delete_stemcell: destroying template VM")

	purge := true
	destroyDisks := deps.Config.DestroyUnreferencedDisks
	var deleteResp *sdknodes.DeleteQemuResponse
	deleteErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_stemcell.destroy_template", 0, func() error {
		var innerErr error
		deleteResp, innerErr = deps.PVE.Nodes().DeleteQemu(ctx, node, fmt.Sprintf("%d", vmid), &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyDisks,
		})
		return innerErr
	})
	if deleteErr != nil {
		if pve.IsNotFound(deleteErr) {
			logger.Warn("delete_stemcell: template VM not found during destroy — already deleted, returning success",
				log.String("stemcell_cid", cidStr),
			)
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(deleteErr),
			fmt.Sprintf("delete_stemcell: destroy template VM %d node %q", vmid, node))
	}

	// Await the destroy task. An empty or null UPID means PVE completed
	// synchronously. Not-found or pmxcfs-config-missing during await means
	// the VM was already gone — treat as success.
	if deleteResp == nil {
		logger.Info("delete_stemcell: template VM destroyed (synchronous, no UPID)")
		return nil
	}
	deleteUPID, upidErr := pve.UPIDFromRaw(*deleteResp)
	if upidErr != nil {
		// Malformed UPID is unexpected but the delete already succeeded.
		logger.Warn("delete_stemcell: cannot parse UPID from template destroy response — skipping await",
			log.Err(upidErr),
		)
		return nil
	}
	if deleteUPID == "" {
		logger.Info("delete_stemcell: template VM destroyed (no UPID returned)")
		return nil
	}
	if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, deleteUPID, logger); awaitErr != nil {
		if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
			logger.Info("delete_stemcell: template VM config missing during destroy await — treating as already deleted")
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(awaitErr),
			fmt.Sprintf("delete_stemcell: await destroy task for template VM %d node %q", vmid, node))
	}

	logger.Info("delete_stemcell: template VM destroyed successfully")
	return nil
}
