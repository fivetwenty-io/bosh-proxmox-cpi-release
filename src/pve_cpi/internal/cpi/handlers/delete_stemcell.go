package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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

			return nil, destroyTemplateVM(ctx, deps, node, vmid, cidStr)
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
