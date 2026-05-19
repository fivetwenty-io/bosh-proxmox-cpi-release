// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleDeleteStemcell returns a Handler for the BOSH CPI delete_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] stemcell_cid string — the CID returned by create_stemcell.
//	    Format: "<storage>:import/<filename>" (e.g. "local:import/bosh-stemcell-ubuntu-jammy-1.0-abc12345.qcow2").
//	    Legacy integer-only CIDs (e.g. "5042") are rejected.
//
// Returns: null / void on success.
//
// Flow:
//
//  1. Parse args[0] as stemcell_cid string.
//  2. Call pve.ParseStemcellCID to extract storage and volumePath ("import/<filename>").
//  3. Delete the qcow2 volume via Storage().DeleteVolumeIfExists — log warning if absent.
//     Volume notes attached to the qcow2 are removed transitively with the volume.
//  4. Return nil (success).
//
// Idempotency: absent volumes are treated as success (warning logged, not an error).
// The BOSH Director expects delete_stemcell to be idempotent.
//
// Error cases returned as *cpierrors.Error (TypeCloud, not retriable):
//   - Missing or non-string stemcell_cid.
//   - stemcell_cid fails pve.ParseStemcellCID (bad format or legacy integer CID).
//   - config.Node is empty.
//   - PVE Storage API failure other than not-found.
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
		qcow2Existed, qcow2Err := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath)
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
