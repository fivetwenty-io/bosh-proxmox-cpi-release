package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// errRebalanceBusy is a lock another call holds, which skips the VM.
var errRebalanceBusy = errors.New("another call holds the VM")

// rebalanceSkip is a check that leaves the VM where it is, with the reason.
type rebalanceSkip struct{ reason string }

func (e *rebalanceSkip) Error() string { return e.reason }

// rebalanceHalt is a lock no VM can take, which stops the run.
type rebalanceHalt struct{ reason string }

func (e *rebalanceHalt) Error() string { return e.reason }

// rebalanceUnmarkAttempts is how many times the run reads the resource and
// writes failback and the dropped mark when PVE refuses the write because
// resources.cfg changed since the read.
const rebalanceUnmarkAttempts = 4

// makeMove runs one move in the order of section 10 of the design, and records
// it in the report. A non-zero code stops the run.
func (r *rebalanceRun) makeMove(ctx context.Context, view *rebalanceView, m harebalance.Move) (int, string) {
	start := r.io.now()
	rec := rebalanceMoveOf(&m)
	logger := r.logger.With(log.Int(metadataKeyVMID, m.VMID), log.String("from", m.From), log.String("to", m.To))
	code, why := r.moveSteps(ctx, view, m, &rec, logger)
	rec.Seconds = rebalanceSeconds(r.io.now().Sub(start))
	r.report.Moves = append(r.report.Moves, rec)
	r.noteMoveDiffers()
	switch rec.Outcome {
	case RebalanceRefused:
		r.excluded[m.VMID] = "this run tried to move it to " + m.To + " and left it where it was: " + rec.Error
		logger.Info(rebalanceOp+": the move did not start, and the VM stays where it was",
			log.String("reason", rec.Error))
	case RebalanceMoved:
		logger.Info(rebalanceOp+": moved the VM", log.String("phase", string(m.Phase)))
	default:
		logger.Warn(rebalanceOp+": the move did not end clean", log.String("outcome", string(rec.Outcome)),
			log.String("reason", rec.Error))
	}
	return code, why
}

// moveSteps runs the steps of one move.
func (r *rebalanceRun) moveSteps(ctx context.Context, view *rebalanceView, m harebalance.Move, rec *RebalanceMove,
	logger *log.Logger,
) (int, string) {
	plan := view.plans[m.VMID]
	if plan == nil {
		rec.Outcome, rec.Error = RebalanceRefused, "the run holds no plan for the VM"
		return RebalanceExitOK, ""
	}
	// Step 1, under the VM's journal record and claim: read again, confirm
	// the plan's inputs, and turn failback off with the mark in one write.
	err := r.underRecordAndClaim(ctx, plan.facts, func(ctx context.Context) error {
		return r.hold(ctx, plan, m)
	})
	if code, why, done := r.preMoveFailure(err, rec); done {
		return code, why
	}

	// Steps 2 and 3: migrate with no claim, and wait for the VM to settle on
	// the target.
	loc, err := r.io.migrate(ctx, m.VMID, m.From, m.To, func(ctx context.Context) error {
		return r.confirmMark(ctx, m.VMID, m.To)
	})
	if err != nil {
		return r.migrateFailure(ctx, view, plan, m, rec, err, logger)
	}
	if loc.Node != m.To {
		rec.Outcome, rec.Error = RebalanceLeftPending, "the VM settled on "+loc.Node+", not on "+m.To
		return RebalanceExitError, fmt.Sprintf("VM %d settled on %s and keeps the rebalance mark; run the "+
			"command again to finish it", m.VMID, loc.Node)
	}

	// Steps 4 and 5, under the record and claim: rewrite the pin when the
	// move calls for it, restore failback, and drop the mark.
	change, rewrite := plan.rewrites[m.To]
	err = r.underRecordAndClaim(ctx, plan.facts, func(ctx context.Context) error {
		if rewrite {
			if err := r.writePin(ctx, view.profile, plan, change); err != nil {
				return err
			}
			rec.PinRewritten = true
		}
		return r.release(ctx, view.profile, plan.facts, m.To, false)
	})
	if err != nil {
		rec.Outcome, rec.Error = RebalanceLeftPending, "the VM reached "+m.To+", and "+err.Error()
		return RebalanceExitError, fmt.Sprintf("VM %d reached %s and keeps the rebalance mark, so failback "+
			"stays off; run the command again to finish it", m.VMID, m.To)
	}

	// Step 6: after two CRM rounds, the VM must still be on the target.
	if err := r.io.sleep(ctx, rebalanceConfirmWait); err != nil {
		rec.Outcome = RebalanceMoved
		return RebalanceExitOK, ""
	}
	node, err := r.io.locate(ctx, m.VMID)
	switch {
	case err != nil:
		rec.Outcome, rec.Error = RebalanceMoved, "the check that the VM stayed failed: "+err.Error()
		return RebalanceExitError, "the check that VM " + strconv.Itoa(m.VMID) + " stayed on " + m.To +
			" failed: " + err.Error()
	case node != m.To:
		rec.Outcome, rec.Error = RebalanceFailed, "the VM moved again, to "+node
		return RebalanceExitError, fmt.Sprintf("VM %d moved again to %s after its move to %s, so something "+
			"other than this command is moving VMs", m.VMID, node, m.To)
	}
	rec.Outcome = RebalanceMoved
	return RebalanceExitOK, ""
}

// preMoveFailure maps an error of step 1. done is true when the move ends.
func (r *rebalanceRun) preMoveFailure(err error, rec *RebalanceMove) (int, string, bool) {
	var skip *rebalanceSkip
	var halt *rebalanceHalt
	switch {
	case err == nil:
		return 0, "", false
	case errors.As(err, &skip):
		rec.Outcome, rec.Error = RebalanceRefused, skip.reason
		return RebalanceExitOK, "", true
	case errors.Is(err, errRebalanceBusy):
		rec.Outcome, rec.Error = RebalanceRefused, err.Error()
		return RebalanceExitOK, "", true
	case errors.As(err, &halt):
		rec.Outcome, rec.Error = RebalanceRefused, halt.reason
		return RebalanceExitError, halt.reason, true
	}
	rec.Outcome, rec.Error = RebalanceRefused, err.Error()
	return RebalanceExitError, "the failback hold of VM " + strconv.Itoa(rec.VMID) + " failed: " + err.Error(), true
}

// hold is step 1. It reads the VM's config and HA resource and the pin again,
// confirms they are the ones the plan was built from, and turns failback off
// with the mark in one write against the digest of the read.
func (r *rebalanceRun) hold(ctx context.Context, plan *rebalanceVMPlan, m harebalance.Move) error {
	cfg, res, pin, err := r.io.reread(ctx, m.VMID, m.From)
	if err != nil {
		return err
	}
	if why := r.inputsChanged(plan, cfg, res, pin); why != "" {
		return &rebalanceSkip{reason: why}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var ok bool
	err = detached(ctx, func(ctx context.Context) error {
		var err error
		ok, err = r.io.mark(ctx, m.VMID, res, m.To)
		return err
	})
	switch {
	case err != nil:
		return err
	case !ok:
		return &rebalanceSkip{reason: "another writer changed the HA resources between the read and the " +
			"failback hold, so nothing moved"}
	}
	return nil
}

// inputsChanged returns why the VM's reads no longer match the plan, or "".
func (r *rebalanceRun) inputsChanged(plan *rebalanceVMPlan, cfg map[string]any,
	res *sdkcluster.GetHaResourcesResponse, pin *ha.Rule,
) string {
	switch {
	case cfg == nil:
		return "the VM's config is not on its node any more"
	case res == nil || res.Comment == nil || haOwnershipOf(*res.Comment, r.owner) != haOwnedByUs:
		return "the VM's HA resource is no longer this Director's"
	case pve.HARequestedState(res.State) != "started":
		return "the VM's HA resource no longer requests started"
	case ha.NarrowPending(res.State, res.Comment):
		return "the narrowing barrier now holds the VM's HA resource"
	case pin == nil || !ha.SameRule(plan.pin, pin):
		return "the VM's pin changed since the plan"
	}
	if node, ok := ha.RebalancePending(res.Comment); ok {
		return "the VM already carries a rebalance mark that names " + node
	}
	if lock, _ := pve.ConfigString(cfg, "lock"); strings.TrimSpace(lock) != "" {
		return "the VM now holds the PVE lock " + lock
	}
	if failbackBasis(cfg, plan.facts.node) != failbackBasis(plan.facts.cfg, plan.facts.node) {
		return "the VM's volumes changed since the plan"
	}
	return ""
}

// confirmMark is the check right before the migrate request: the resource
// still carries the mark that names the target, with failback off.
func (r *rebalanceRun) confirmMark(ctx context.Context, vmid int, to string) error {
	res, err := r.io.readResource(ctx, vmid)
	if err != nil {
		return err
	}
	if res == nil {
		return errors.New("the VM's HA resource is gone")
	}
	if node, ok := ha.RebalancePending(res.Comment); !ok || node != to {
		return errors.New("the VM's HA resource lost the rebalance mark that names " + to)
	}
	if res.Failback == nil || res.Failback.Bool() {
		return errors.New("the VM's failback is on again")
	}
	return nil
}

// migrateFailure handles a migration that did not bring the VM to the target
// (section 11 of the design).
func (r *rebalanceRun) migrateFailure(ctx context.Context, view *rebalanceView, plan *rebalanceVMPlan,
	m harebalance.Move, rec *RebalanceMove, err error, logger *log.Logger,
) (int, string) {
	var me *ha.MoveError
	failure := ha.MoveIncomplete
	if errors.As(err, &me) {
		failure = me.Failure
	}
	rec.Error = err.Error()
	if failure == ha.MoveIncomplete {
		// The migration may still run, or it may have ended on the source. A
		// VM that is still moving, or that the run can't read, keeps the mark
		// for a rerun to finish.
		node, lerr := r.io.locate(context.WithoutCancel(ctx), m.VMID)
		state := ""
		if lerr == nil {
			state = r.serviceState(ctx, m.VMID)
		}
		switch {
		case lerr != nil || ctx.Err() != nil || state == "migrate" || state == "relocate":
			rec.Outcome = RebalanceLeftPending
			return RebalanceExitError, fmt.Sprintf("VM %d was left part way to %s (%s); run the command again "+
				"to finish it", m.VMID, m.To, err.Error())
		case node == m.To:
			rec.Outcome = RebalanceLeftPending
			return RebalanceExitError, fmt.Sprintf("VM %d reached %s after the wait ran out and keeps the "+
				"rebalance mark; run the command again to finish it", m.VMID, m.To)
		}
	}
	// The VM is still on the source: restore failback and drop the mark.
	aerr := r.underRecordAndClaim(context.WithoutCancel(ctx), plan.facts, func(ctx context.Context) error {
		return r.release(ctx, view.profile, plan.facts, m.To, false)
	})
	if aerr != nil {
		rec.Outcome = RebalanceLeftPending
		rec.Error += "; and the mark could not be dropped: " + aerr.Error()
		logger.Warn(rebalanceOp+": the migration did not happen, and the VM keeps failback off and the mark",
			log.Err(aerr))
		return RebalanceExitError, fmt.Sprintf("VM %d did not move, and its failback could not be restored, so "+
			"it keeps the rebalance mark; run the command again to finish it", m.VMID)
	}
	if failure == ha.MoveRefused {
		rec.Outcome = RebalanceRefused
		return RebalanceExitOK, ""
	}
	rec.Outcome = RebalanceFailed
	return RebalanceExitError, fmt.Sprintf("the migration of VM %d to %s failed (%s), which points to a "+
		"problem with the cluster", m.VMID, m.To, err.Error())
}

// serviceState returns the HA manager's state of the VM, or "" when it
// can't be read.
func (r *rebalanceRun) serviceState(ctx context.Context, vmid int) string {
	states, err := r.io.serviceStates(context.WithoutCancel(ctx))
	if err != nil {
		return ""
	}
	return states[haResourceSid(vmid)]
}

// writePin is step 4. It writes the pin the plan built for the target, in
// place of the pin the plan read.
func (r *rebalanceRun) writePin(ctx context.Context, profile *ha.Profile, plan *rebalanceVMPlan,
	change rebalancePinChange,
) error {
	if err := r.claimWindowOpen(ctx, plan.facts.vmid); err != nil {
		return err
	}
	err := detached(ctx, func(ctx context.Context) error {
		_, err := r.io.writePin(ctx, profile, change.d, plan.facts.vmid, plan.pin, change.pin)
		return err
	})
	if errors.Is(err, errPinChanged) {
		return errors.New("its pin changed while it moved, so the pin was not rewritten")
	}
	if err != nil {
		return fmt.Errorf("the pin write failed: %w", err)
	}
	return nil
}

// release is step 5, and the abort of a move. It decides the failback the VM
// gets, which counts the mark that names to as the run's own, and writes it
// with the mark dropped, against the digest of a fresh read. An undecided
// failback stays off, keepOff keeps it off whatever the decision, and so does
// a failback hold that is fresh in the read the write names.
func (r *rebalanceRun) release(ctx context.Context, profile *ha.Profile, f *rebalanceVMFacts, to string,
	keepOff bool,
) error {
	for range rebalanceUnmarkAttempts {
		node, err := r.io.locate(ctx, f.vmid)
		if err != nil {
			return fmt.Errorf("locate the VM: %w", err)
		}
		want, err := r.io.wantFailback(ctx, profile, f.vmid, pve.VMLocation{Node: node, Tags: f.tags,
			Found: true, Type: resourceTypeQemu}, to)
		if err != nil {
			return fmt.Errorf("decide the VM's failback: %w", err)
		}
		res, err := r.io.readResource(ctx, f.vmid)
		if err != nil {
			return err
		}
		if res == nil || res.Comment == nil {
			return errors.New("the VM's HA resource is gone")
		}
		if marked, ok := ha.RebalancePending(res.Comment); !ok || marked != to {
			return errors.New("the VM's HA resource no longer carries the mark that names " + to)
		}
		if err := r.claimWindowOpen(ctx, f.vmid); err != nil {
			return err
		}
		failback := !keepOff && want.decided && want.want
		if unmarked, _ := ha.WithoutRebalanceMark(*res.Comment); !ha.FailbackAllowed(failback, unmarked,
			r.io.now()) {
			failback = false
		}
		var ok bool
		err = detached(ctx, func(ctx context.Context) error {
			var err error
			ok, err = r.io.unmark(ctx, f.vmid, res, failback)
			return err
		})
		if err != nil {
			return fmt.Errorf("the failback restore failed: %w", err)
		}
		if ok {
			return nil
		}
	}
	return errors.New("other writers changed the HA resources on every try of the failback restore")
}

// claimWindowOpen fails when the run's claim on the VM ran short, so no write
// starts after the claim's window.
func (r *rebalanceRun) claimWindowOpen(ctx context.Context, vmid int) error {
	if end, ok := heldVMIDClaimWindow(ctx, vmid); ok && !r.io.now().Before(end) {
		return errors.New("the run's claim on the VM ran short before the write")
	}
	return nil
}

// underRecordAndClaim runs fn with the VM's journal record and its VMID claim
// held, each tried once.
func (r *rebalanceRun) underRecordAndClaim(ctx context.Context, f *rebalanceVMFacts,
	fn func(context.Context) error,
) error {
	return r.io.withRecord(ctx, f.vmid, f.cfg, func() error {
		return r.io.withClaim(ctx, f.vmid, fn)
	})
}

// detached runs one write on a context detached from ctx and bounded at
// rebalanceWriteTimeout, so a signal never leaves the write's outcome unknown.
// The values of ctx, such as the claim set, stay on it.
func detached(ctx context.Context, fn func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebalanceWriteTimeout)
	defer cancel()
	return fn(wctx)
}
