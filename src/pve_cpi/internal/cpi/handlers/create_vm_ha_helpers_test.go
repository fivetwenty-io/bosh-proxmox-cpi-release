package handlers

import (
	"context"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// testPinStorage is the storage the pins the writer tests write record.
const testPinStorage = "local-lvm"

// testRegisteredDecision returns the decision create_vm would hold for a VM
// the eligibility decision registers on node, with the AZ constraint of cp the
// way DecideVM derives it: a strict pin_az makes the AZ's nodes hard, and
// otherwise they are preferred. Its admissible set is the hard set, or the
// placed node when there is none. It is nil under a mode that writes no HA,
// because create_vm then makes no decision.
func testRegisteredDecision(deps Deps, vmid int, cp createVMCloudProps, node string) *ha.VMDecision {
	if !deps.Config.HAWritesEnabled() {
		return nil
	}
	d := &ha.VMDecision{
		VMID: vmid, SID: haResourceSid(vmid), Node: node, Outcome: ha.OutcomeRegister, Policy: ha.PolicyShared,
		Placed: node, DLB: vmInDLBAZ(cp, deps.Config),
	}
	if deps.Config.HAPinAZValue() != config.HARuleOff && node != "" {
		if az := pinAZForNode(cp, deps.Config, node); az != "" {
			if nodes, ok := deps.Config.AZCandidates(az); ok {
				if deps.Config.HAPinAZStrictValue() {
					d.Hard = nodes
				} else {
					d.Preferred = nodes
				}
			}
		}
	}
	d.Admissible = d.Hard
	if len(d.Admissible) == 0 && node != "" {
		d.Admissible = []string{node}
	}
	return d
}

// azPinApply runs the AZ pin writer the way create_vm's confinement step does
// for a registered VM: an error propagateHARuleError names is returned, and
// any other is logged.
func azPinApply(
	ctx context.Context, deps Deps, vmid int, cp createVMCloudProps, node string, logger *log.Logger,
) error {
	_, err := applyAZNodeAffinityPin(ctx, deps, testRegisteredDecision(deps, vmid, cp, node),
		[]string{testPinStorage}, cp, node, logger)
	if err != nil && !propagateHARuleError(deps, err) {
		logger.Warn("test: node-affinity pin not applied", log.Err(err))
		return nil
	}
	return err
}

// aaApply runs the anti-affinity writer the way create_vm's confinement step
// does for a registered VM, with the error handling of azPinApply.
func aaApply(ctx context.Context, deps Deps, vmid int, env map[string]any, logger *log.Logger) error {
	_, err := applyAntiAffinityMembership(ctx, deps, testRegisteredDecision(deps, vmid, createVMCloudProps{}, ""),
		env, logger)
	if err != nil && !propagateHARuleError(deps, err) {
		logger.Warn("test: anti-affinity membership not applied", log.Err(err))
		return nil
	}
	return err
}

// ensureNodeAffinityPin writes the node constraint azNodes into the VM's merged
// pin with no eligibility decision. strict makes azNodes a hard constraint and
// otherwise they are preferred, and a single node is also the placed node. The
// pin records testPinStorage. An empty azNodes writes nothing.
func ensureNodeAffinityPin(
	ctx context.Context, deps Deps, vmid int, azNodes []string, strict bool, logger *log.Logger,
) error {
	nodesCSV := sortedNodesCSV(azNodes)
	if nodesCSV == "" {
		return nil
	}
	nodes := strings.Split(nodesCSV, ",")
	req := pinRequest{Storages: []string{testPinStorage}}
	if strict {
		req.Hard = nodes
	} else {
		req.Preferred = nodes
	}
	if len(nodes) == 1 {
		req.Placed = nodes[0]
	}
	return ensureMergedPin(ctx, deps, vmid, req, logger)
}
