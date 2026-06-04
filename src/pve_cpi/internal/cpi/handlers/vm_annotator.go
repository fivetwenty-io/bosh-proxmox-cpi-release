package handlers

import (
	"context"
	"strconv"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// VMAnnotator writes operator-facing Notes onto a created VM. It satisfies the
// hooks.VMAnnotator interface structurally — this package does not import
// internal/cpi/hooks, which avoids an import cycle (hooks is wired around
// handlers, not the reverse). cmd/cpi/main.go constructs one and passes it into
// the hook Deps.
//
// The write is intentionally a plain Description set, not a read-modify-merge:
// create_vm leaves the description empty, and set_vm_metadata later overwrites
// it with the full BOSH metadata block. The notes_audit annotation therefore
// gives PVE-UI visibility only in the window between create and set_metadata,
// which is exactly its purpose. It is best-effort; the caller (the hook) logs
// and swallows any returned error.
type VMAnnotator struct {
	pveClient pve.Client
	logger    *log.Logger
}

// NewVMAnnotator builds a VMAnnotator from handler Deps.
func NewVMAnnotator(deps Deps) *VMAnnotator {
	return &VMAnnotator{pveClient: deps.PVE, logger: deps.Logger}
}

// AnnotateNotes resolves the VM's node from its CID and sets the guest Notes
// (PVE description) to notes. Returns an error when the node cannot be located
// or the PVE update fails.
func (a *VMAnnotator) AnnotateNotes(ctx context.Context, vmid int, notes string) error {
	node, ok, err := pve.FindVMNodeViaCluster(ctx, a.pveClient, vmid)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return a.pveClient.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid),
		&sdknodes.UpdateQemuConfigParams{Description: &notes})
}
