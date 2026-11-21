package main

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haRebalanceLockPool is the sentinel pool of the rebalance run lock.
var haRebalanceLockPool = pve.ClusterLockPoolName(handlers.RebalanceRunLockName)

// haRebalancePlanMoves caps the moves of the doctor's dry run high enough
// that the plan lists every move, so the doctor counts every VM outside its AZ.
const haRebalancePlanMoves = 1 << 16

// rebalancePendingFindings reports each HA resource of this Director that
// carries a rebalance mark while no run of pve-ops ha rebalance holds the run
// lock. Such a mark is what a run that stopped part way leaves, and the mark
// keeps the resource's failback off until a run finishes or undoes the move.
// A live run lock means a run is moving the VM now, so the mark is expected.
func (c *haCollector) rebalancePendingFindings() {
	if c.off() || !c.resourcesOK {
		return
	}
	type marked struct{ sid, target, owner string }
	var pending []marked
	for _, sid := range slices.Sorted(maps.Keys(c.resources)) {
		res := c.resources[sid]
		owner, ok := haMarkerOwner(res.Comment)
		if !ok || c.foreign(res.Comment) {
			continue
		}
		if target, held := ha.RebalancePending(&res.Comment); held {
			if owner == "" {
				owner = c.owner
			}
			pending = append(pending, marked{sid: sid, target: target, owner: owner})
		}
	}
	if len(pending) == 0 {
		return
	}
	expiry, held, err := c.r.RebalanceRunLock(c.ctx)
	if err != nil {
		c.rep.unchecked("ha-rebalance-lock", "the rebalance run lock could not be read, so the rebalance marks on "+
			"%d HA resources are unchecked: %v", len(pending), err)
		return
	}
	if held && (expiry.IsZero() || expiry.After(c.now)) {
		return
	}
	for _, p := range pending {
		director := p.owner
		if director == "" {
			director = "<director-uuid>"
		}
		c.rep.problem(haKindRebalancePending, p.sid,
			"%s carries the rebalance mark of a move to %s, and no rebalance holds the run lock, so a run stopped "+
				"part way and left the VM's failback off. Run pve-ops ha rebalance --apply --director-uuid %s, "+
				"which finishes the move or undoes it before it plans anything new",
			p.sid, p.target, director)
	}
}

// spreadSkewedFindings advises a rebalance when the dry run of pve-ops ha
// rebalance would move a VM back into its AZ or pull apart two members of an
// anti-affinity rule that share a node, or when the spread of committed
// memory across the nodes passes the rebalance's start threshold. The advice
// never fails the doctor, and it appears only when the doctor's own reads and
// the dry run's reads succeeded, because a plan built on a failed read could
// advise a move the cluster doesn't need.
func (c *haCollector) spreadSkewedFindings() {
	if c.off() || c.owner == "" || !c.resourcesOK || !c.statesOK || !c.rulesOK || !c.guestsOK ||
		c.guestsPartial {
		return
	}
	rep, err := c.r.RebalancePlan(c.ctx, c.owner)
	if err != nil || rep.ExitCode == handlers.RebalanceExitError {
		return
	}
	for i := range rep.Gates {
		if rep.Gates[i].Name == handlers.RebalanceGateProfile && !rep.Gates[i].OK {
			return
		}
	}
	outside, separate := 0, 0
	for i := range rep.Moves {
		switch rep.Moves[i].Phase {
		case harebalance.PhaseRestore:
			outside++
		case harebalance.PhaseSeparate:
			separate++
		case harebalance.PhaseSpread:
		}
	}
	spread := int(math.Round(rep.SpreadBefore * 100))
	if outside == 0 && separate == 0 && rep.SpreadBefore <= handlers.RebalanceDefaultStart {
		return
	}
	subject := rep.Cluster
	if subject == "" {
		subject = "cluster"
	}
	vms := "HA VMs of this Director run outside their AZ"
	if outside == 1 {
		vms = "HA VM of this Director runs outside its AZ"
	}
	apart := ""
	if separate > 0 {
		noun, pron := "moves that pull", "them"
		if separate == 1 {
			noun, pron = "move that pulls", "it"
		}
		apart = fmt.Sprintf(" The dry run also has %d %s apart members of an anti-affinity rule sharing a node, "+
			"and a rebalance would make %s too.", separate, noun, pron)
	}
	c.rep.advise(haKindSpreadSkewed, subject,
		"%d %s, and the committed memory of the nodes spreads %d "+
			"points from the fullest node to the emptiest, against a start threshold of %d. Run pve-ops ha "+
			"rebalance --director-uuid %s to see the moves a rebalance would make, and add --apply to make them."+apart,
		outside, vms, spread, int(math.Round(handlers.RebalanceDefaultStart*100)), haOwnerValue(c.owner))
}
