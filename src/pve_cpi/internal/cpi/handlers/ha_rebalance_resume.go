package handlers

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The actions the resume phase takes on a VM that carries a rebalance mark.
const (
	// rebalanceResumeFinish is a VM on the node its mark names: the run
	// writes its pin when the pin needs it, restores failback, and drops the
	// mark.
	rebalanceResumeFinish = "finish"
	// rebalanceResumeAbort is a VM elsewhere that PVE HA is not moving: the
	// run restores failback and drops the mark, and the VM stays.
	rebalanceResumeAbort = "abort"
	// rebalanceResumeWait is a VM that PVE HA is still moving, which a later
	// run handles once it settles.
	rebalanceResumeWait = "wait"
)

// resume handles the VMs that carry the rebalance mark of an earlier run that
// stopped part way (section 11 of the design). A dry run lists them with the
// action an applied run takes. An applied run finishes or aborts each one, and
// any VM it can't clear stops the run with exit 1 before it plans.
func (r *rebalanceRun) resume(ctx context.Context, view *rebalanceView) (int, string) {
	var left []string
	for _, f := range view.marked() {
		target, _ := ha.RebalancePending(f.res.Comment)
		rec := RebalanceResumed{VMID: f.vmid, Node: f.node, Target: target, Outcome: RebalancePlanned}
		switch state := view.snap.states[haResourceSid(f.vmid)]; {
		case state == "migrate" || state == "relocate":
			rec.Action = rebalanceResumeWait
		case f.node == target:
			rec.Action = rebalanceResumeFinish
		default:
			rec.Action = rebalanceResumeAbort
		}
		if r.opts.Apply {
			r.resumeOne(ctx, view, f, target, &rec)
			if rec.Outcome != RebalanceFinished && rec.Outcome != RebalanceAborted {
				left = append(left, strconv.Itoa(f.vmid))
			}
		}
		r.report.Resumed = append(r.report.Resumed, rec)
	}
	if len(left) > 0 {
		return RebalanceExitError, fmt.Sprintf("the rebalance marks of VM %s could not be cleared, so no new move "+
			"starts; run the command again once the cause is gone", strings.Join(left, ", "))
	}
	return RebalanceExitOK, ""
}

// resumeOne finishes or aborts one marked VM and records the outcome.
func (r *rebalanceRun) resumeOne(ctx context.Context, view *rebalanceView, f *rebalanceVMFacts, target string,
	rec *RebalanceResumed,
) {
	logger := r.logger.With(log.Int(metadataKeyVMID, f.vmid), log.String("node", f.node),
		log.String("target", target), log.String("action", rec.Action))
	var err error
	switch rec.Action {
	case rebalanceResumeWait:
		rec.Outcome, rec.Error = RebalanceLeftPending, "PVE HA is still moving the VM"
		return
	case rebalanceResumeFinish:
		pin, change, refused := r.resumeRewrite(view, f, target)
		err = r.underRecordAndClaim(ctx, f, func(ctx context.Context) error {
			if change != nil {
				if err := r.writePin(ctx, view.profile, &rebalanceVMPlan{facts: f, pin: pin}, *change); err != nil {
					return err
				}
			}
			return r.release(ctx, view.profile, f, target, refused != "")
		})
		if err == nil {
			rec.Outcome = RebalanceFinished
			if refused != "" {
				rec.Error = refused + ", so the run dropped the mark and left failback off"
				r.warn(fmt.Sprintf("VM %d finished its move to %s, but %s, so its failback stays off until the "+
					"CPI writes its pin", f.vmid, target, refused))
			}
		}
	default:
		err = r.underRecordAndClaim(ctx, f, func(ctx context.Context) error {
			return r.release(ctx, view.profile, f, target, false)
		})
		if err == nil {
			rec.Outcome = RebalanceAborted
		}
	}
	if err != nil {
		rec.Outcome, rec.Error = RebalanceLeftPending, err.Error()
		logger.Warn(rebalanceOp+": the VM keeps the rebalance mark of an earlier run", log.Err(err))
		return
	}
	logger.Info(rebalanceOp+": cleared the rebalance mark of an earlier run", log.String("outcome",
		string(rec.Outcome)))
}

// resumeRewrite returns the VM's pin and the pin a finished move to target
// writes, nil when the pin stays, or why no pin can be written there.
func (r *rebalanceRun) resumeRewrite(view *rebalanceView, f *rebalanceVMFacts, target string) (
	*ha.Rule, *rebalancePinChange, string,
) {
	pin := view.snap.rules.Find(naRuleNameFor(f.vmid))
	if pin == nil || pin.Type != ha.RuleTypeNodeAffinity || pin.Disabled ||
		haOwnershipOf(pin.Comment, r.owner) != haOwnedByUs {
		return nil, nil, "the VM has no enabled pin of this Director"
	}
	placed, flat := pinPlaced(pin)
	switch {
	case flat || placed == target:
		return pin, nil, ""
	case !slices.Contains(pin.NodeNames(), target):
		return pin, nil, "the VM's pin does not name " + target
	}
	b := rebalanceModelBuilder{deps: r.deps, owner: r.owner, profile: view.profile, snap: view.snap,
		now: r.io.now()}
	az, azNodes := rebalanceAZ(r.deps, pin)
	change, refused := b.rewrite(f, pin, target, az, azNodes)
	return pin, change, refused
}
