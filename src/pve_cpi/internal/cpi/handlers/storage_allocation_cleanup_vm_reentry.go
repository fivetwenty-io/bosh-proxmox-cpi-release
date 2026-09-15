package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// Admission never resubmits an unknown cleanup request. Its exact effect must
// already be visible before this invocation can continue with other disposal.
func cleanupPendingVMDeletion(step aj.Step, record aj.Record) bool {
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.Target.External || len(step.Charges) != 0 || len(step.VolIDs) != 0 || step.Target.Node == "" {
		return false
	}
	if step.State != aj.Planned && step.State != aj.Submitted {
		return false
	}
	if step.State == aj.Planned && step.UPID != "" {
		return false
	}
	kind := ""
	switch step.Kind {
	case "vm.delete.stop":
		kind = "qmstop"
	case "vm.delete.destroy":
		kind = "qmdestroy"
	default:
		return false
	}
	if step.Target.VMID <= 0 || step.Target.Storage != "" || step.Target.Backing != "" || step.Target.IntendedVolume != "" || len(step.Parameters) != 0 {
		return false
	}
	linked := false
	for index := range record.Steps {
		prior := &record.Steps[index]
		if prior.ID == step.ID {
			break
		}
		if prior.Attempt == step.Attempt && !prior.Target.External && prior.Target.VMID == step.Target.VMID && prior.Target.Node == step.Target.Node && !strings.HasPrefix(prior.Kind, "vm.delete.") {
			linked = true
		}
	}
	if !linked {
		return false
	}
	if step.State == aj.Submitted {
		parts := strings.Split(step.UPID, ":")
		if len(parts) != 9 || parts[1] != step.Target.Node || parts[5] != kind || parts[6] != strconv.Itoa(step.Target.VMID) {
			return false
		}
	}
	return true
}

func observeCleanupVMDeletion(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, step aj.Step) error {
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return err
	}
	if step.Kind == "vm.delete.destroy" {
		// No numeric-ID adoption: both guest classes must be absent. Remaining exact
		// journal-owned volumes are checked separately by the disposal observers.
		return observePoolOnlyGuestsAbsent(ctx, deps, nodes, step.Target.VMID)
	}
	observed, err := observeManagedVMRecord(ctx, deps, journal, record)
	if err != nil {
		return err
	}
	if observed.Node != step.Target.Node || observed.VMID != step.Target.VMID {
		return fmt.Errorf("pending stop lacks exact VM ownership")
	}
	status, err := deps.PVE.QEMU().Status(ctx, observed.Node, observed.VMID)
	if err != nil || status["status"] != "stopped" {
		return fmt.Errorf("pending stop completion is not independently observed")
	}
	return nil
}
