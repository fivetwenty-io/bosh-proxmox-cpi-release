package handlers

import (
	"context"
	"fmt"
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

// ensureMergedPin writes the VM's one merged node-affinity pin outside the
// staging order, which is how the tests drive the merge. The pin names
// every node of the hard admissible set and is strict when that set is smaller
// than the cluster's configured nodes. The placed node, the preferred nodes,
// and the rest get priorities 2, 1, and 0, flattened to one value while the VM
// is in a resource-affinity rule, because PVE refuses several priorities there.
//
// The writers of a VM's constraints run one after another (the AZ pin, then the
// PCI pin), so a strict pin already written for the VM is a hard constraint the
// new one must keep. Its nodes are intersected with the new hard set, and the
// result replaces the new hard set when it is non-empty and holds the placed
// node. An AZ pin of {a,b,c} and a PCI pin of {a} so give strict {a} in either
// order. A strict pin that shares no node with the new constraint, or that does
// not hold the placed node, is left from an earlier attempt and is replaced.
//
// The pin is written under the rules lock with a PUT that carries the read's
// digest, so it changes in place and never disappears between a delete and a
// create. A pin carrying another Director's marker is never touched. The PVE 9
// gate runs before the lock.
func ensureMergedPin(ctx context.Context, deps Deps, vmid int, req pinRequest, logger *log.Logger) error {
	ruleName := naRuleNameFor(vmid)
	ident := haIdentity{Owner: deps.RequestDirectorUUID}
	if ident.Owner == "" {
		// A pin the CPI cannot mark as its own is one it could never edit or
		// clean up safely, so a request without a director_uuid writes none.
		return fmt.Errorf("node-affinity: the request carries no director_uuid to mark rule %q with", ruleName)
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("node-affinity: %w", err)
	}
	return writeMergedPin(ctx, deps, profile, vmid, req, logger)
}

// writeMergedPin is ensureMergedPin after the gate, for a caller that already
// holds the gated profile.
func writeMergedPin(
	ctx context.Context, deps Deps, profile *ha.Profile, vmid int, req pinRequest, logger *log.Logger,
) error {
	_, err := writeMergedPinRule(ctx, deps, profile, vmid, req, logger)
	return err
}
