package handlers

import (
	"context"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// VMAnnotator writes operator-facing Notes onto a created VM. It satisfies the
// hooks.VMAnnotator interface structurally — this package does not import
// internal/cpi/hooks, which avoids an import cycle (hooks is wired around
// handlers, not the reverse). cmd/cpi/main.go constructs one and passes it into
// the hook Deps.
//
// The write replaces the human-readable description text but merge-preserves
// any existing <!--BOSH:{...}--> sentinel block: the notes_audit hook fires
// After() create_vm, which by then has written the bosh_pool provenance
// sentinel, and a plain Description set would erase it. set_vm_metadata later
// overwrites the text again with the full BOSH metadata block (also sentinel-
// preserving), so the notes_audit annotation gives PVE-UI visibility only in
// the window between create and set_metadata, which is exactly its purpose.
// It is best-effort; the caller (the hook) logs and swallows any returned
// error.
type VMAnnotator struct {
	pveClient pve.Client
	logger    *log.Logger
}

// NewVMAnnotator builds a VMAnnotator from handler Deps.
func NewVMAnnotator(deps Deps) *VMAnnotator {
	return &VMAnnotator{pveClient: deps.PVE, logger: deps.Logger}
}

// AnnotateNotes resolves the VM's node from its CID and sets the guest Notes
// (PVE description) to notes, re-rendering any existing sentinel keys on top
// of the new text (mirroring setVMMetadataRMW) so create-time provenance
// such as bosh_pool survives. A failed config read falls back to the plain
// set — losing the sentinel there degrades pool reconciliation for this VM,
// not correctness, and the annotation itself is already best-effort. Returns
// an error when the node cannot be located or the PVE update fails.
func (a *VMAnnotator) AnnotateNotes(ctx context.Context, vmid int, notes string) error {
	node, ok, err := pve.FindVMNodeViaCluster(ctx, a.pveClient, vmid)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	desc := notes
	if vmCfg, cfgErr := a.pveClient.QEMU().Config(ctx, node, vmid); cfgErr == nil {
		if _, raw := pve.ParseSentinel(pve.DescriptionFromConfig(vmCfg)); len(raw) > 0 {
			if merged, renderErr := pve.RenderSentinel(strings.TrimSpace(notes), raw); renderErr == nil {
				desc = merged
			}
		}
	} else if a.logger != nil {
		a.logger.Warn("notes annotation: could not read current VM config; existing description sentinel will not be preserved",
			log.Int("vmid", vmid), log.Err(cfgErr))
	}

	return a.pveClient.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid),
		&sdknodes.UpdateQemuConfigParams{Description: &desc})
}
