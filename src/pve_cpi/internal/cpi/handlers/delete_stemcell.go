package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// HandleDeleteStemcell returns a Handler for the BOSH CPI delete_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] stemcell_cid string — the CID returned by create_stemcell.
//	    Supported formats:
//	      - "template:<vmid>"                        (new template CID, e.g. "template:6042")
//	      - "<storage>:import/<filename>"            (volume CID, e.g. "local:import/bosh-stemcell-ubuntu-jammy-1.0-abc12345.qcow2")
//	      - "light:<storage>:import/<filename>"      (pre-upgrade light CID, no-op)
//	      - "<integer>"                              (pre-upgrade legacy integer CID, no-op)
//
// Returns: null / void on success.
//
// CID routing (evaluated in order):
//
//  1. "template:<vmid>" → destroy the template VM with purge (removes all disks).
//     Idempotent: VM already absent is treated as success.
//  2. "light:..." → no-op (operator-managed volume; CPI never deletes it).
//  3. Integer-only (e.g. "5042") → no-op (pre-upgrade legacy CID scrub).
//  4. "<storage>:import/<filename>" → delete the qcow2 volume via Storage().DeleteVolumeIfExists.
//
// Idempotency: absent resources (VM or volume) are treated as success (warning logged).
// The BOSH Director expects delete_stemcell to be idempotent.
//
// Error cases returned as *cpierrors.Error (TypeCloud, not retriable):
//   - Missing or non-string stemcell_cid.
//   - template CID with invalid VMID.
//   - stemcell_cid fails pve.ParseStemcellCID (bad format).
//   - config.Node is empty (for volume and template paths).
//   - PVE API failure other than not-found.
func HandleDeleteStemcell(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// ----------------------------------------------------------------
		// Arg 0: stemcell_cid
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_stemcell: missing required argument stemcell_cid")
		}
		var cidStr string
		if err := json.Unmarshal(args[0], &cidStr); err != nil || cidStr == "" {
			return nil, cpierrors.Cloud("delete_stemcell: stemcell_cid must be a non-empty string")
		}

		// ----------------------------------------------------------------
		// Template stemcell CIDs ("template:<vmid>") identify a PVE template
		// VM created by create_stemcell. Destroy it with purge=true so all
		// disks are removed. Idempotent: if the VM is already gone, return
		// success without error.
		//
		// Node resolution: StemcellTemplateNode if set (matches create_stemcell
		// placement), otherwise config.Node. This mirrors the convention used
		// by create_stemcell for ensureTemplateVM.
		// ----------------------------------------------------------------
		if pve.IsTemplateStemcellCID(cidStr) {
			vmid, parseErr := pve.ParseTemplateStemcellCID(cidStr)
			if parseErr != nil {
				return nil, cpierrors.Cloud("delete_stemcell: invalid template stemcell CID %q: %s", cidStr, parseErr.Error())
			}

			node := deps.Config.StemcellTemplateNode
			if node == "" {
				node = deps.Config.Node
			}
			if node == "" {
				return nil, cpierrors.Cloud("delete_stemcell: config.node must not be empty")
			}

			// Resolve sha8 from the primary BEFORE destroying it; the tag is
			// unreadable once the VM is gone.
			sha8 := resolveStemcellSHA8FromVMID(ctx, deps, node, vmid)

			// Destroy the primary template VM.
			if err := destroyTemplateVM(ctx, deps, node, vmid, cidStr); err != nil {
				return nil, err
			}

			// Always sweep all cluster nodes for template VMs that carry the
			// same sha8 tag. This subsumes the old StemcellReplicateLocal-gated
			// replica-cleanup path: replicas carry the sha tag and will be found
			// regardless of the replication-enabled flag. Best-effort: sweep
			// failures are logged at Warn and never fail the delete.
			if sha8 != "" {
				sweepStemcellByShaTag(ctx, deps, sha8, cidStr)
			} else {
				deps.Logger.Warn("delete_stemcell: sha8 unresolvable before primary destroy; cross-node sweep skipped",
					log.String("stemcell_cid", cidStr),
				)
			}

			// Opt-in orphan prune: remove all stemcell templates tagged with
			// this director's ID that are no longer linked. Best-effort; never
			// fails delete_stemcell.
			if deps.Config.StemcellOrphanPruneEnabled() {
				pruneOrphanStemcellTemplates(ctx, deps, cidStr)
			}

			return nil, nil
		}

		// ----------------------------------------------------------------
		// Light stemcell CIDs ("light:<storage>:import/<file>") are
		// operator-managed: both pre-uploaded mode and CPI-assisted-fetch
		// mode resolve to the same lifecycle policy — the CPI never
		// deletes the underlying volume. Operator removes via PVE-native
		// tooling (pvesm free) when ready.
		// ----------------------------------------------------------------
		if pve.IsLightStemcellCID(cidStr) {
			deps.Logger.Info("delete_stemcell: light stemcell CID, skipping delete (operator-managed)",
				log.String("stemcell_cid", cidStr),
			)
			return nil, nil
		}

		// ----------------------------------------------------------------
		// Legacy integer CIDs ("5042") were emitted by the obsolete
		// template-clone CPI design. The matching template VM and its
		// import qcow2 are no longer referenced by direct-qcow create_vm.
		// Treat the delete as a no-op so the director can scrub the stale
		// row from its stemcells table; any leftover template VM or
		// qcow2 file must be cleaned up out-of-band on the PVE host.
		// ----------------------------------------------------------------
		if pve.IsLegacyIntegerStemcellCID(cidStr) {
			deps.Logger.Warn("delete_stemcell: legacy integer CID accepted as no-op (re-upload to regenerate)",
				log.String("stemcell_cid", cidStr),
			)
			return nil, nil
		}

		// ----------------------------------------------------------------
		// Parse CID → storage + volumePath ("import/<filename>")
		// ----------------------------------------------------------------
		storage, volumePath, parseErr := pve.ParseStemcellCID(cidStr)
		if parseErr != nil {
			return nil, cpierrors.Cloud("delete_stemcell: invalid stemcell CID format: %s", parseErr.Error())
		}

		node := deps.Config.Node
		if node == "" {
			return nil, cpierrors.Cloud("delete_stemcell: config.node must not be empty")
		}

		deps.Logger.Info("delete_stemcell: deleting stemcell volume",
			log.String("stemcell_cid", cidStr),
			log.String("node", node),
			log.String("storage", storage),
			log.String("qcow2_volume", volumePath),
		)

		// ----------------------------------------------------------------
		// Delete qcow2 volume. Absent = warning only (idempotent).
		// Volume notes (set by create_stemcell) are removed transitively.
		// ----------------------------------------------------------------
		var qcow2Existed bool
		qcow2Err := pve.RetryOnTransientOrLock(ctx, deps.Logger, "delete_stemcell", 0, func() error {
			var innerErr error
			qcow2Existed, innerErr = deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath)
			return innerErr
		})
		if qcow2Err != nil {
			return nil, cpierrors.Cloud(
				"delete_stemcell: delete qcow2 volume %q on storage %q node %q: %s",
				volumePath, storage, node, qcow2Err.Error())
		}
		if !qcow2Existed {
			deps.Logger.Warn("delete_stemcell: qcow2 volume not found (already deleted or never existed)",
				log.String("volume", volumePath),
				log.String("storage", storage),
			)
		}

		deps.Logger.Info("delete_stemcell: stemcell volume deleted",
			log.String("stemcell_cid", cidStr),
			log.Bool("qcow2_existed", qcow2Existed),
		)
		return nil, nil
	})
}

// sweepStemcellByShaTag lists all QEMU resources cluster-wide and destroys every
// template VM tagged with stemcellSHATagPrefix+sha8. It is called after the
// primary template is already deleted, so finding the primary VMID absent is
// normal and treated as success. All failures are logged at Warn; the function
// never returns an error. cidStr is used for log context only.
func sweepStemcellByShaTag(ctx context.Context, deps Deps, sha8, cidStr string) {
	logger := deps.Logger.With(
		log.String("method", "delete_stemcell.sha_sweep"),
		log.String("stemcell_cid", cidStr),
		log.String("sha8", sha8),
	)

	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		logger.Warn("delete_stemcell: cluster service unavailable; sha sweep skipped")
		return
	}

	resp, err := deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		logger.Warn("delete_stemcell: ListResources failed; sha sweep skipped", log.Err(err))
		return
	}
	if resp == nil {
		return
	}

	type clusterItem struct {
		Type     string `json:"type"`
		VMID     int64  `json:"vmid"`
		Node     string `json:"node"`
		Name     string `json:"name"`
		Tags     string `json:"tags"`
		Template *int   `json:"template"`
	}

	shaTag := stemcellSHATagPrefix + sha8
	for _, raw := range *resp {
		var item clusterItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type != "qemu" || item.VMID == 0 {
			continue
		}
		if item.Template == nil || *item.Template != 1 {
			continue
		}
		if !tagsContain(item.Tags, shaTag) {
			continue
		}
		nodeLogger := logger.With(
			log.String("node", item.Node),
			log.Int("vmid", int(item.VMID)),
			log.String("name", item.Name),
		)
		if delErr := destroyTemplateVM(ctx, deps, item.Node, item.VMID, cidStr); delErr != nil {
			nodeLogger.Warn("delete_stemcell: sha sweep: destroy failed (best-effort, continuing)", log.Err(delErr))
		} else {
			nodeLogger.Info("delete_stemcell: sha sweep: template destroyed")
		}
	}
}

// pruneOrphanStemcellTemplates removes template VMs cluster-wide that carry
// both stemcellMarkerTag and a "director--<directorID>" tag, indicating they
// belong to this director but were never cleaned up (e.g. failed deployments).
// A VM that is a linked-clone base is skipped with a warning. All failures are
// logged; the function never returns an error. cidStr is used for log context.
func pruneOrphanStemcellTemplates(ctx context.Context, deps Deps, cidStr string) {
	dryRun := deps.Config.StemcellOrphanPruneDryRun()
	dir := deps.Config.StemcellDirectorID()
	logger := deps.Logger.With(
		log.String("method", "delete_stemcell.orphan_prune"),
		log.String("stemcell_cid", cidStr),
		log.Bool("dry_run", dryRun),
	)

	if dir == "" {
		logger.Warn("delete_stemcell: orphan prune enabled but director_id unset; skipping")
		return
	}

	dirTag := "director--" + sanitizeTagValue(dir)

	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		logger.Warn("delete_stemcell: cluster service unavailable; orphan prune skipped")
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
		Type     string `json:"type"`
		VMID     int64  `json:"vmid"`
		Node     string `json:"node"`
		Name     string `json:"name"`
		Tags     string `json:"tags"`
		Template *int   `json:"template"`
	}

	for _, raw := range *resp {
		var item clusterItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type != "qemu" || item.VMID == 0 {
			continue
		}
		if item.Template == nil || *item.Template != 1 {
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

// resolveStemcellSHA8FromVMID reads tags from the QEMU VM list on node and
// extracts the "bosh-stemcell-sha-<sha8>" tag value for the given vmid.
// Returns "" when the VM is not found, has no tags, or carries no sha tag.
// This is a best-effort helper used by the cross-node sha sweep (sweepStemcellByShaTag)
// to scope replica deletion to the correct stemcell.
func resolveStemcellSHA8FromVMID(ctx context.Context, deps Deps, node string, vmid int64) string {
	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return ""
	}
	resp, err := deps.PVE.Nodes().ListQemu(ctx, node, nil)
	if err != nil || resp == nil || len(*resp) == 0 {
		return ""
	}
	type qemuItem struct {
		Vmid int64   `json:"vmid"`
		Tags *string `json:"tags,omitempty"`
	}
	const shaPrefix = "bosh-stemcell-sha-"
	for _, raw := range *resp {
		var item qemuItem
		if jerr := json.Unmarshal(raw, &item); jerr != nil || item.Vmid != vmid {
			continue
		}
		if item.Tags == nil {
			return ""
		}
		tags := strings.ReplaceAll(*item.Tags, ",", ";")
		for _, tok := range strings.Split(tags, ";") {
			tok = strings.TrimSpace(tok)
			if strings.HasPrefix(tok, shaPrefix) {
				return strings.TrimPrefix(tok, shaPrefix)
			}
		}
		return ""
	}
	return ""
}

// destroyTemplateVM deletes the template VM identified by vmid on node with
// purge=true (removes disks + unreferenced storage objects). Idempotent: a 404
// response from PVE means the VM is already gone; that is treated as success
// and a warning is logged. The UPID returned by DeleteQemu is awaited; a
// not-found or pmxcfs-config-missing error during the await is also treated as
// success. Any other error is returned as a cloud error.
func destroyTemplateVM(ctx context.Context, deps Deps, node string, vmid int64, cidStr string) error {
	vmCIDStr := strconv.FormatInt(vmid, 10)
	logger := deps.Logger.With(
		log.String("method", "delete_stemcell"),
		log.String("stemcell_cid", cidStr),
		log.String("node", node),
		log.Int("vmid", int(vmid)),
	)
	logger.Info("delete_stemcell: destroying template VM")

	purge := true
	destroyDisks := true
	var deleteResp *sdknodes.DeleteQemuResponse
	deleteErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_stemcell.destroy_template", 0, func() error {
		var innerErr error
		deleteResp, innerErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCIDStr, &sdknodes.DeleteQemuParams{
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
