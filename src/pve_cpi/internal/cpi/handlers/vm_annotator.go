package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
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
// such as bosh_pool survives. For a bare CID, a failed config read falls back
// to the plain set — losing the sentinel there degrades pool reconciliation
// for this VM, not correctness, and the annotation itself is already
// best-effort.
//
// A stamped CID is written only to the VM it names. The config read is then
// required, and the VM's cluster and token tags must agree with the CID, so a
// VMID another create took over in the meantime keeps its own Notes. A failed
// read or a disagreeing tag skips the write and returns an error naming the
// reason, which the hook logs. Returns an error when the CID does not parse,
// the node cannot be located, or the PVE update fails.
func (a *VMAnnotator) AnnotateNotes(ctx context.Context, vmCID string, notes string) error {
	cid, err := vmcid.Parse(vmCID)
	if err != nil {
		return err
	}
	vmid := cid.VMID
	// Authoritative lookup: the annotator often runs right after create_vm,
	// inside the cluster index's lag window, and a stale-index miss here
	// silently skips the annotation for a VM that exists. The per-node probes
	// on the miss path find it; a proven absence stays a silent no-op.
	loc, err := pve.FindVMAuthoritative(ctx, a.pveClient, vmid)
	if err != nil {
		return err
	}
	if !loc.Found || loc.Node == "" {
		return nil
	}
	node := loc.Node

	desc := notes
	vmCfg, cfgErr := a.pveClient.QEMU().Config(ctx, node, vmid)
	if cid.Stamped() {
		if cfgErr != nil {
			return fmt.Errorf("skipping the Notes write for CID %s, because VM %d's config could not be read to check "+
				"its token: %w", vmCID, vmid, cfgErr)
		}
		if reason := annotationStampMismatch(cid, vmCfg); reason != "" {
			return fmt.Errorf("skipping the Notes write for CID %s, because %s", vmCID, reason)
		}
	}
	if cfgErr == nil {
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

// annotationStampMismatch returns why a VM config does not belong to a stamped
// CID, or "" when its tags agree with the CID. The VM must carry the CID's
// token and no other, and any cluster tag it carries must name the CID's
// cluster.
func annotationStampMismatch(cid vmcid.VMCID, vmCfg map[string]any) string {
	tagsRaw, _ := pve.ConfigString(vmCfg, jsonKeyTags)
	tags := lowerTags(parseTagsField(tagsRaw))
	if clusters := tagValues(tags, clusterStampTagPrefix); len(clusters) > 0 && !allEqual(clusters, cid.Cluster) {
		return fmt.Sprintf("the VM at VMID %d records cluster %s, and the CID names cluster %s",
			cid.VMID, strings.Join(clusters, " and "), cid.Cluster)
	}
	tokens := tagValues(tags, cidStampTagPrefix)
	if len(tokens) == 0 || !allEqual(tokens, cid.Token) {
		return fmt.Sprintf("the VM at VMID %d carries token %s, and the CID carries token %s",
			cid.VMID, orNone(strings.Join(tokens, " and ")), cid.Token)
	}
	return ""
}
