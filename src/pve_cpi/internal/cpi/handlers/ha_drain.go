package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The defaults of pve-ops ha drain and pve-ops ha undrain.
const (
	// DrainDefaultSettleTimeout bounds each wait of a drain. A node can hold
	// many VMs, and PVE HA moves them a few at a time.
	DrainDefaultSettleTimeout = 30 * time.Minute
	// UndrainDefaultSettleTimeout bounds the wait for the VMs an undrain
	// starts, and it is the settle timeout of the rebalance that follows.
	UndrainDefaultSettleTimeout = 10 * time.Minute
)

// RebalanceGateMaintenance is the gate of a drain or an undrain that reads
// the named node's HA maintenance.
const RebalanceGateMaintenance = "maintenance"

// DrainGateFates is the gate of a drain that passes once the drain could
// decide the fate of every VM on the node.
const DrainGateFates = "fates"

// The commands a DrainReport records.
const (
	DrainCommand   = "drain"
	UndrainCommand = "undrain"
)

// DrainOutcome is what a drain or an undrain did to one VM.
type DrainOutcome string

// The outcomes of a VM in a drain or an undrain.
const (
	// DrainOutcomePlanned is a stop or a start a dry run plans, or one an
	// applied run did not reach.
	DrainOutcomePlanned DrainOutcome = "planned"
	// DrainOutcomeStopped is a VM the drain stopped through PVE HA.
	DrainOutcomeStopped DrainOutcome = haRequestStopped
	// DrainOutcomeStoppedEarlier is a VM an earlier drain of the node stopped.
	DrainOutcomeStoppedEarlier DrainOutcome = "stopped_earlier"
	// DrainOutcomeStarted is a VM the undrain started through PVE HA.
	DrainOutcomeStarted DrainOutcome = haRawStateStarted
	// DrainOutcomeSkipped is a VM whose HA resource changed so that the stop
	// or the start no longer applies, such as a VM that left the node.
	DrainOutcomeSkipped DrainOutcome = "skipped"
	// DrainOutcomeBusy is a VM whose journal record or VMID claim another
	// call held, so a rerun has to stop or start it.
	DrainOutcomeBusy DrainOutcome = "busy"
	// DrainOutcomeFailed is a VM whose write failed, that could leave and
	// stayed, or that the HA manager reports in error.
	DrainOutcomeFailed DrainOutcome = "failed"
	// DrainOutcomeLeftPending is a VM whose stop or start PVE HA had not
	// finished when the settle timeout ran out.
	DrainOutcomeLeftPending DrainOutcome = "left_pending"
)

// DrainOptions shape a run of pve-ops ha drain.
type DrainOptions struct {
	// Node is the node in maintenance.
	Node string
	// Apply stops the VMs that can't leave. Without it the run reads the
	// cluster, takes no lock, and writes nothing.
	Apply bool
	// SettleTimeout bounds the wait for PVE HA to move the VMs that can
	// leave, and the wait for the stops.
	SettleTimeout time.Duration
}

// UndrainOptions shape a run of pve-ops ha undrain.
type UndrainOptions struct {
	// Node is the node whose maintenance ended.
	Node string
	// Apply starts the VMs a drain stopped and runs the rebalance. Without it
	// the run reads the cluster, takes no lock, and writes nothing.
	Apply bool
	// SettleTimeout bounds the wait for the started VMs, and it becomes the
	// rebalance's settle timeout.
	SettleTimeout time.Duration
	// NoRebalance skips the rebalance.
	NoRebalance bool
	// Rebalance carries the rebalance's move cap, duration, thresholds, and
	// restore-only choice. Its Apply and SettleTimeout are set from the
	// undrain's.
	Rebalance RebalanceOptions
}

// DrainVM is one VM of this Director in a drain or an undrain.
type DrainVM struct {
	VMID       int    `json:"vmid"`
	Name       string `json:"name,omitempty"`
	Node       string `json:"node"`
	Deployment string `json:"deployment,omitempty"`
	// Fate is what a drain decided for the VM, empty in an undrain.
	Fate DrainFate `json:"fate,omitempty"`
	// Reason says why a VM stays, has no HA resource, or isn't started.
	Reason string `json:"reason,omitempty"`
	// Targets lists the nodes that can take a VM that leaves.
	Targets []string     `json:"targets,omitempty"`
	Outcome DrainOutcome `json:"outcome,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// DrainReport is what a drain or an undrain did.
type DrainReport struct {
	Command string `json:"command"`
	// Cluster is the name /cluster/status gives the cluster, empty until the
	// run read it.
	Cluster  string          `json:"cluster"`
	Node     string          `json:"node"`
	Applied  bool            `json:"applied"`
	Gates    []RebalanceGate `json:"gates"`
	Warnings []string        `json:"warnings"`
	// VMs lists this Director's VMs on the node for a drain, and the VMs
	// that carry the node's drain token for an undrain, in VMID order.
	VMs []DrainVM `json:"vms"`
	// Deployments lists the BOSH deployments of the VMs a drain stops or an
	// undrain starts, sorted.
	Deployments []string `json:"deployments"`
	// Reminder tells the operator what to do about resurrection, empty when
	// no VM stops or starts.
	Reminder string `json:"reminder"`
	// Rebalance is the report of the rebalance an applied undrain ran.
	Rebalance *RebalanceReport `json:"rebalance,omitempty"`
	Halted    string           `json:"halted"`
	ExitCode  int              `json:"exit_code"`
}

// GatesOK reports whether every gate passed.
func (r *DrainReport) GatesOK() bool {
	for i := range r.Gates {
		if !r.Gates[i].OK {
			return false
		}
	}
	return true
}

// Count returns how many VMs have outcome o.
func (r *DrainReport) Count(o DrainOutcome) int {
	n := 0
	for i := range r.VMs {
		if r.VMs[i].Outcome == o {
			n++
		}
	}
	return n
}

// DrainHA plans, and with opts.Apply carries out, the drain of a node in HA
// maintenance: it waits for PVE HA to move every VM of this Director that can
// leave, then stops through PVE HA each VM that can't, and records the stop
// with a drain token on the VM's HA resource. deps.RequestDirectorUUID names
// the Director. The error is for a run that could not start, and the report's
// ExitCode says how a run that started ended.
func DrainHA(ctx context.Context, deps Deps, opts DrainOptions) (DrainReport, error) {
	deps, err := drainDeps(deps, "ha drain", opts.Node)
	if err != nil {
		return DrainReport{}, err
	}
	if opts.SettleTimeout <= 0 {
		opts.SettleTimeout = DrainDefaultSettleTimeout
	}
	rio := newPVERebalanceIO(deps, RebalanceOptions{SettleTimeout: opts.SettleTimeout}.withDefaults())
	r := newDrainRun(deps, DrainCommand, opts.Node, opts.Apply, opts.SettleTimeout, rio)
	return r.drain(ctx), nil
}

// UndrainHA plans, and with opts.Apply carries out, the end of a node's
// drain: it starts through PVE HA each VM that carries the node's drain
// token, waits for them, and then runs the rebalance unless opts.NoRebalance
// is set. The error is for a run that could not start, and the report's
// ExitCode says how a run that started ended.
func UndrainHA(ctx context.Context, deps Deps, opts UndrainOptions) (DrainReport, error) {
	deps, err := drainDeps(deps, "ha undrain", opts.Node)
	if err != nil {
		return DrainReport{}, err
	}
	if opts.SettleTimeout <= 0 {
		opts.SettleTimeout = UndrainDefaultSettleTimeout
	}
	opts.Rebalance.Apply, opts.Rebalance.SettleTimeout = opts.Apply, opts.SettleTimeout
	opts.Rebalance = opts.Rebalance.withDefaults()
	r := newDrainRun(deps, UndrainCommand, opts.Node, opts.Apply, opts.SettleTimeout,
		newPVERebalanceIO(deps, opts.Rebalance))
	r.noRebalance, r.rebalance = opts.NoRebalance, opts.Rebalance
	return r.undrain(ctx), nil
}

// drainDeps checks what a drain or an undrain needs before it starts.
func drainDeps(deps Deps, op, node string) (Deps, error) {
	deps.RequestDirectorUUID = strings.TrimSpace(deps.RequestDirectorUUID)
	switch {
	case deps.Config == nil:
		return deps, errors.New(op + ": no CPI configuration")
	case deps.RequestDirectorUUID == "":
		return deps, errors.New(op + ": no director UUID names the Director whose VMs to act on")
	case strings.TrimSpace(node) == "":
		return deps, errors.New(op + ": no node named")
	}
	client := unguardedPVE(deps.PVE)
	if client == nil || client.Cluster() == nil || client.QEMU() == nil || client.Nodes() == nil {
		return deps, errors.New(op + ": no PVE client")
	}
	deps.PVE = client
	return deps, nil
}

// drainRun is one run of a drain or an undrain.
type drainRun struct {
	deps   Deps
	owner  string
	node   string
	apply  bool
	settle time.Duration
	io     rebalanceIO
	logger *log.Logger
	op     string

	report DrainReport

	// noRebalance and rebalance shape the rebalance an undrain runs.
	noRebalance bool
	rebalance   RebalanceOptions
}

func newDrainRun(deps Deps, command, node string, apply bool, settle time.Duration, rio rebalanceIO) *drainRun {
	return &drainRun{deps: deps, owner: deps.RequestDirectorUUID, node: node, apply: apply, settle: settle,
		io: rio, logger: deps.Log(context.Background()).With(log.String("node", node)), op: "ha " + command,
		report: DrainReport{Command: command, Node: node, Applied: apply, Gates: []RebalanceGate{},
			Warnings: []string{}, VMs: []DrainVM{}, Deployments: []string{}}}
}

// drainView is one fresh read of the cluster and the gates it passes. A
// drain's view also holds the fate of each VM on the node.
type drainView struct {
	profile *ha.Profile
	snap    *rebalanceSnapshot
	gates   []RebalanceGate
	fates   []drainVMFate
}

// halt ends the run with code and the reason.
func (r *drainRun) halt(code int, why string) DrainReport {
	r.report.ExitCode, r.report.Halted = code, why
	return r.report
}

// lock takes the run lock with lease ttl. It returns false, with the report
// halted, when the lock could not be taken.
func (r *drainRun) lock(ctx context.Context, ttl time.Duration) (func(), bool) {
	release, err := r.io.runLock(ctx, ttl)
	switch {
	case errors.Is(err, pve.ErrClusterLockHeld):
		r.report.Gates = append(r.report.Gates, RebalanceGate{Name: RebalanceGateRunLock,
			Detail: "another rebalance, drain, or undrain holds the run lock bosh-lock-ha-rebalance"})
		r.halt(RebalanceExitGate, RebalanceGateRunLock)
		return nil, false
	case err != nil:
		r.report.Gates = append(r.report.Gates, RebalanceGate{Name: RebalanceGateRunLock,
			Detail: "the run lock could not be taken: " + err.Error()})
		r.halt(RebalanceExitError, "the run lock could not be taken: "+err.Error())
		return nil, false
	}
	return release, true
}

// observe reads the cluster afresh and checks every gate, and for a drain it
// decides the fate of each VM on the node.
func (r *drainRun) observe(ctx context.Context) (*drainView, error) {
	profile, err := r.io.profile(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve the HA profile: %w", err)
	}
	snap, err := r.io.snapshot(ctx, profile)
	if err != nil {
		return nil, err
	}
	act, err := r.io.activity(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the CPI's locks and tasks: %w", err)
	}
	journal := r.io.journal(ctx)
	undrain := r.report.Command == UndrainCommand
	view := &drainView{profile: profile, snap: snap,
		gates: drainGates(r.deps, profile, act, journal, r.node, r.apply, undrain)}
	if !undrain {
		view.fates = decideDrainFates(r.deps.Config, profile, snap, r.node)
		view.gates = append(view.gates, drainFatesGate(view.fates, r.node))
	}
	if r.apply && !journal.configured {
		r.warn("the configuration names no allocation journal, so the run can't see the Director's own records; " +
			"the Director's own CPI must have no journal either, or this run may stop or start a VM under a CPI " +
			"call")
	}
	r.report.Gates = view.gates
	if profile != nil {
		r.report.Cluster = profile.ClusterName
	}
	return view, nil
}

// readFailed halts the run after a read failed.
func (r *drainRun) readFailed(ctx context.Context, err error) DrainReport {
	if ctx.Err() != nil {
		return r.halt(RebalanceExitError, "the run was interrupted; run it again to go on")
	}
	return r.halt(RebalanceExitError, "a read failed: "+err.Error())
}

// warn adds a warning once.
func (r *drainRun) warn(w string) {
	if !slices.Contains(r.report.Warnings, w) {
		r.report.Warnings = append(r.report.Warnings, w)
	}
}

// drainGates checks the gates of a drain or an undrain against one fresh
// read. They are the rebalance's gates with two changes: the nodes gate judges
// the named node by the maintenance gate instead, and the settled gate does
// not apply, because the commands wait for motion themselves.
func drainGates(deps Deps, profile *ha.Profile, act rebalanceActivity, journal rebalanceJournal, node string,
	apply, undrain bool,
) []RebalanceGate {
	return []RebalanceGate{
		profileGate(deps.Config, profile),
		quorumGate(profile),
		drainNodesGate(profile, node),
		maintenanceGate(profile, node, apply, undrain),
		managerGate(profile),
		journalGate(journal, apply),
		activityGate(act),
		runLockGate(act, apply),
	}
}

// drainFatesGate passes when the drain decided the fate of every VM on node.
// A VM it can't decide might leave or might not, so the drain refuses before
// it stops anything, and a rerun decides again from a fresh read.
func drainFatesGate(fates []drainVMFate, node string) RebalanceGate {
	g := RebalanceGate{Name: DrainGateFates}
	var undecided []string
	for i := range fates {
		if fates[i].Fate == DrainUndecided {
			undecided = append(undecided, fmt.Sprintf("VM %d, because %s", fates[i].VMID, fates[i].Reason))
		}
	}
	if len(undecided) > 0 {
		g.Detail = "the drain can't decide whether PVE HA can move " + rebalanceDetail(undecided) +
			"; run the drain again once the cause is fixed"
		return g
	}
	g.OK, g.Detail = true, "the drain decided the fate of every VM of this Director on node "+node
	return g
}

// drainNodesGate passes when node is an online node of the cluster and every
// other node is online, out of maintenance, and able to take work.
func drainNodesGate(profile *ha.Profile, node string) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateNodes}
	if profile == nil || len(profile.Nodes) == 0 || profile.Health == nil {
		g.Detail = "the HA profile lists no node health"
		return g
	}
	var bad []string
	found := false
	for i := range profile.Nodes {
		n := profile.Nodes[i]
		entry := healthEntry(profile.Health, n.Name)
		switch {
		case n.Name == node:
			found = true
			if !n.Online {
				bad = append(bad, "node "+n.Name+" is offline")
			}
		case !n.Online:
			bad = append(bad, "node "+n.Name+" is offline")
		case entry == nil:
			bad = append(bad, "node "+n.Name+" has no health entry")
		case entry.Maintenance:
			bad = append(bad, "node "+n.Name+" is in maintenance too, and a rolling maintenance takes one node "+
				"at a time")
		case entry.DeadOrFenced || !entry.Placeable:
			bad = append(bad, "node "+n.Name+" can't take work now")
		}
	}
	if !found {
		bad = append(bad, "node "+node+" is not a node of this cluster")
	}
	if len(bad) > 0 {
		g.Detail = rebalanceDetail(bad)
		return g
	}
	g.OK = true
	g.Detail = fmt.Sprintf("node %s is online, and the other %d nodes are online, out of maintenance, and able "+
		"to take work", node, len(profile.Nodes)-1)
	return g
}

// pveMaintenance reports whether the HA manager or the node's LRM reports the
// node in maintenance. An operator maintenance tag alone does not count,
// because PVE HA moves nothing for a tag.
func pveMaintenance(e *pve.NodeHealthEntry) bool {
	return e.CRMState == "maintenance" || e.LRMMode == "maintenance" || e.LRMState == "maintenance"
}

// maintenanceGate reads the named node's maintenance. A drain needs the node
// in PVE HA maintenance, and an undrain needs it out of PVE HA maintenance and
// free of an operator maintenance tag. A dry run reports the node's state and
// passes, so the operator can read the plan first.
func maintenanceGate(profile *ha.Profile, node string, apply, undrain bool) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateMaintenance}
	var e *pve.NodeHealthEntry
	if profile != nil {
		e = healthEntry(profile.Health, node)
	}
	if e == nil {
		g.Detail = "node " + node + " has no health entry"
		return g
	}
	inPVE := pveMaintenance(e)
	switch {
	case !undrain && inPVE:
		g.OK, g.Detail = true, "the HA manager reports node "+node+" in maintenance"
		return g
	case !undrain:
		g.Detail = "the HA manager doesn't report node " + node + " in maintenance; run `ha-manager crm-command " +
			"node-maintenance enable " + node + "` on a PVE node first"
		if e.Maintenance {
			g.Detail += ", because an operator maintenance tag alone moves nothing"
		}
	case inPVE:
		g.Detail = "node " + node + " is still in HA maintenance; run `ha-manager crm-command node-maintenance " +
			"disable " + node + "` on a PVE node first"
	case e.Maintenance:
		g.Detail = "node " + node + " carries an operator maintenance tag, so the CPI places nothing on it; " +
			"remove the tag first"
	default:
		g.OK, g.Detail = true, "node "+node+" is out of maintenance"
		return g
	}
	if !apply {
		// A dry run reports the node's state and passes.
		g.OK, g.Detail = true, "for --apply, "+g.Detail
	}
	return g
}

// drainVMOf is the report entry of one fate.
func drainVMOf(f *drainVMFate, node string) DrainVM {
	vm := DrainVM{VMID: f.VMID, Name: f.Name, Node: node, Deployment: f.Deployment, Fate: f.Fate, Reason: f.Reason,
		Targets: f.Targets}
	switch {
	case f.Fate == DrainStays:
		vm.Outcome = DrainOutcomePlanned
	case f.Fate == DrainNotStarted && f.facts != nil && f.facts.res != nil:
		if by, ok := ha.DrainStopped(f.facts.res.Comment); ok && by == node {
			vm.Outcome = DrainOutcomeStoppedEarlier
		}
	}
	return vm
}

// recordFates replaces the report's VMs with the fates of view.
func (r *drainRun) recordFates(view *drainView) {
	r.report.VMs = make([]DrainVM, 0, len(view.fates))
	for i := range view.fates {
		r.report.VMs = append(r.report.VMs, drainVMOf(&view.fates[i], r.node))
	}
}

// reportVM returns the report entry of vmid, or nil.
func (r *drainRun) reportVM(vmid int) *DrainVM {
	for i := range r.report.VMs {
		if r.report.VMs[i].VMID == vmid {
			return &r.report.VMs[i]
		}
	}
	return nil
}

// drain runs the steps of section 23.5 of the design.
func (r *drainRun) drain(ctx context.Context) DrainReport {
	if r.apply {
		release, ok := r.lock(ctx, 2*r.settle+rebalanceRunLockMargin)
		if !ok {
			return r.report
		}
		defer release()
	}
	view, err := r.observe(ctx)
	if err != nil {
		return r.readFailed(ctx, err)
	}
	r.recordFates(view)
	if !r.report.GatesOK() || !r.apply {
		r.remind(DrainCommand)
		if !r.report.GatesOK() {
			return r.halt(RebalanceExitGate, firstFailedGate(r.report.Gates))
		}
		return r.report
	}
	view, ok := r.waitForPVE(ctx, view)
	if !ok {
		r.remind(DrainCommand)
		return r.report
	}
	r.recordFates(view)
	if !r.report.GatesOK() {
		r.remind(DrainCommand)
		return r.halt(RebalanceExitGate, firstFailedGate(r.report.Gates))
	}
	code, why := r.stopStuck(ctx, view)
	if code == RebalanceExitOK || code == RebalanceExitError && why == "" {
		code2, why2 := r.waitFor(ctx, haRequestStopped, r.vmsWith(DrainOutcomeStopped, DrainOutcomeStoppedEarlier))
		if code == RebalanceExitOK {
			code, why = code2, why2
		}
	}
	r.remind(DrainCommand)
	if code != RebalanceExitOK {
		if why == "" {
			why = r.leftRunning()
		}
		return r.halt(code, why)
	}
	return r.report
}

// leftRunning says how many VMs the drain left running.
func (r *drainRun) leftRunning() string {
	n := r.report.Count(DrainOutcomeBusy) + r.report.Count(DrainOutcomeFailed)
	return fmt.Sprintf("the drain left %d VM%s that can't leave running; run it again to stop %s", n,
		haPlural(n), haThemOrIt(n))
}

// waitForPVE waits up to the settle timeout until no VM that can leave is on
// the node and nothing is migrating or relocating, apart from the VMs that
// can't leave. A VM whose fate a poll can't decide keeps the wait going too,
// and when it is still undecided at the timeout, the caller's fates gate ends
// the run. It reads the cluster and decides the fates again on each poll, and
// returns the last view. false means the run halted.
func (r *drainRun) waitForPVE(ctx context.Context, view *drainView) (*drainView, bool) {
	deadline := r.io.now().Add(r.settle)
	for {
		stuck := map[string]bool{}
		var leaving []*drainVMFate
		undecided := 0
		for i := range view.fates {
			f := &view.fates[i]
			switch f.Fate {
			case DrainStays:
				stuck[haResourceSid(f.VMID)] = true
			case DrainLeaves:
				leaving = append(leaving, f)
			case DrainUndecided:
				undecided++
			}
		}
		moving := slices.DeleteFunc(movingServices(view.snap.states), func(sid string) bool { return stuck[sid] })
		if len(leaving) == 0 && len(moving) == 0 && undecided == 0 {
			return view, true
		}
		if !r.io.now().Before(deadline) && len(leaving) == 0 && len(moving) == 0 {
			return view, true
		}
		if !r.io.now().Before(deadline) {
			r.recordFates(view)
			for _, f := range leaving {
				vm := r.reportVM(f.VMID)
				vm.Outcome = DrainOutcomeFailed
				vm.Error = "it could leave for " + strings.Join(f.Targets, " or ") + ", but it was still on node " +
					r.node + " when the settle timeout ran out, so something other than the HA rules holds it"
			}
			if len(leaving) > 0 {
				r.halt(RebalanceExitError, fmt.Sprintf("%d VM%s that could leave node %s stayed past the settle "+
					"timeout", len(leaving), haPlural(len(leaving)), r.node))
			} else {
				r.halt(RebalanceExitError, "PVE HA was still moving "+strings.Join(moving, ", ")+" when the settle "+
					"timeout ran out")
			}
			return nil, false
		}
		if err := r.io.sleep(ctx, rebalanceSettlePoll); err != nil {
			r.halt(RebalanceExitError, "the run was interrupted while PVE HA moved VMs off node "+r.node+
				"; run it again to go on")
			return nil, false
		}
		next, err := r.observe(ctx)
		if err != nil {
			r.readFailed(ctx, err)
			return nil, false
		}
		view = next
	}
}

// stopStuck stops each VM of view that can't leave. It returns a non-zero code
// with a reason when a lock no VM can take, or a signal, stops the run, and a
// non-zero code with no reason when it stopped what it could and left VMs
// running.
func (r *drainRun) stopStuck(ctx context.Context, view *drainView) (int, string) {
	code := RebalanceExitOK
	for i := range view.fates {
		f := &view.fates[i]
		if f.Fate != DrainStays {
			continue
		}
		if ctx.Err() != nil {
			return RebalanceExitError, "the run was interrupted before it stopped every VM that can't leave; run " +
				"it again to go on"
		}
		vm := r.reportVM(f.VMID)
		err := r.underRecordAndClaim(ctx, f.facts, func(cctx context.Context) error {
			return r.writeState(cctx, f.VMID, true)
		})
		if halt, stop := r.classify(vm, err, DrainOutcomeStopped); stop {
			return RebalanceExitError, halt
		}
		if vm.Outcome == DrainOutcomeBusy || vm.Outcome == DrainOutcomeFailed {
			code = RebalanceExitError
		}
	}
	return code, ""
}

// classify records the outcome of one stop or start. It returns true, with
// the reason, when the error stops the run.
func (r *drainRun) classify(vm *DrainVM, err error, done DrainOutcome) (string, bool) {
	var skip *rebalanceSkip
	var halt *rebalanceHalt
	logger := r.logger.With(log.Int(metadataKeyVMID, vm.VMID))
	switch {
	case err == nil:
		vm.Outcome, vm.Error = done, ""
		logger.Info(r.op + ": the HA resource now requests " + map[DrainOutcome]string{
			DrainOutcomeStopped: haRequestStopped, DrainOutcomeStarted: haRawStateStarted}[done])
	case errors.As(err, &halt):
		vm.Outcome, vm.Error = DrainOutcomeFailed, halt.reason
		return halt.reason, true
	case errors.Is(err, errRebalanceBusy):
		vm.Outcome, vm.Error = DrainOutcomeBusy, err.Error()
	case errors.As(err, &skip):
		vm.Outcome, vm.Error = DrainOutcomeSkipped, skip.reason
	default:
		vm.Outcome, vm.Error = DrainOutcomeFailed, err.Error()
		logger.Warn(r.op+": the write failed", log.Err(err))
	}
	return "", false
}

// underRecordAndClaim runs fn with the VM's journal record and its VMID claim
// held, each tried once.
func (r *drainRun) underRecordAndClaim(ctx context.Context, f *rebalanceVMFacts,
	fn func(context.Context) error,
) error {
	return r.io.withRecord(ctx, f.vmid, f.cfg, func() error {
		return r.io.withClaim(ctx, f.vmid, fn)
	})
}

// writeState reads the VM's HA resource, checks that the stop or the start
// still applies, and writes it against the read's digest. A write PVE refuses
// because resources.cfg changed reads and decides again, up to
// ha.CommentWriteAttempts times.
func (r *drainRun) writeState(ctx context.Context, vmid int, stop bool) error {
	for range ha.CommentWriteAttempts {
		read, err := r.io.readResource(ctx, vmid)
		if err != nil {
			return err
		}
		if read == nil {
			return &rebalanceSkip{reason: "its HA resource is gone"}
		}
		if stop {
			if state := pve.HARequestedState(read.State); state != haRawStateStarted {
				return &rebalanceSkip{reason: "its HA resource now requests " + state}
			}
			at, err := r.io.locate(ctx, vmid)
			if err != nil {
				return fmt.Errorf("locate VM %d: %w", vmid, err)
			}
			if at != r.node {
				return &rebalanceSkip{reason: "it is now on node " + at}
			}
		} else if by, ok := ha.DrainStopped(read.Comment); !ok || by != r.node {
			return &rebalanceSkip{reason: "its HA resource no longer carries the drain token of node " + r.node}
		}
		if end, ok := heldVMIDClaimWindow(ctx, vmid); ok && !r.io.now().Before(end) {
			return errors.New("the run's claim on the VM ran short before the write")
		}
		var landed bool
		if err := detached(ctx, func(wctx context.Context) error {
			var werr error
			landed, werr = r.io.drainState(wctx, vmid, read, stop, r.node)
			return werr
		}); err != nil {
			return err
		}
		if landed {
			return nil
		}
	}
	return errors.New("other writers changed the HA resources on every try of the write")
}

// vmsWith returns the VMIDs of the report's VMs with one of the outcomes.
func (r *drainRun) vmsWith(outcomes ...DrainOutcome) []int {
	var out []int
	for i := range r.report.VMs {
		if slices.Contains(outcomes, r.report.VMs[i].Outcome) {
			out = append(out, r.report.VMs[i].VMID)
		}
	}
	return out
}

// waitFor waits up to the settle timeout until the HA manager reports each of
// vmids in state want. A wait for started also waits until no HA service is
// migrating or relocating, so the failback moves have ended too. A VM the
// HA manager reports in error fails, and one still short of want at the
// timeout is left pending.
func (r *drainRun) waitFor(ctx context.Context, want string, vmids []int) (int, string) {
	if len(vmids) == 0 {
		return RebalanceExitOK, ""
	}
	verb := map[string]string{haRequestStopped: "stop", haRawStateStarted: "start"}[want]
	interrupted := "the run was interrupted while it waited for PVE HA to " + verb + " the VMs; run it again to " +
		"go on"
	deadline := r.io.now().Add(r.settle)
	pending := slices.Clone(vmids)
	failed := 0
	for {
		states, err := r.io.serviceStates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return RebalanceExitError, interrupted
			}
			return RebalanceExitError, "a read failed: read the HA manager status: " + err.Error()
		}
		pending = slices.DeleteFunc(pending, func(vmid int) bool {
			switch states[haResourceSid(vmid)] {
			case want:
				return true
			case "error":
				vm := r.reportVM(vmid)
				vm.Outcome, vm.Error = DrainOutcomeFailed, "the HA manager reports it in error"
				failed++
				return true
			}
			return false
		})
		moving := want == haRawStateStarted && len(movingServices(states)) > 0
		if len(pending) == 0 && !moving {
			if failed > 0 {
				return RebalanceExitError, fmt.Sprintf("the HA manager reports %d VM%s in error", failed,
					haPlural(failed))
			}
			return RebalanceExitOK, ""
		}
		if !r.io.now().Before(deadline) {
			for _, vmid := range pending {
				vm := r.reportVM(vmid)
				vm.Outcome, vm.Error = DrainOutcomeLeftPending, "the HA manager reported it "+
					orNothing(states[haResourceSid(vmid)])+" when the settle timeout ran out"
			}
			if len(pending) == 0 {
				return RebalanceExitError, "PVE HA was still moving " + strings.Join(movingServices(states), ", ") +
					" when the settle timeout ran out"
			}
			return RebalanceExitError, fmt.Sprintf("%d VM%s didn't reach %s within the settle timeout; run the "+
				"command again to wait for %s", len(pending), haPlural(len(pending)), want, haThemOrIt(len(pending)))
		}
		if err := r.io.sleep(ctx, rebalanceSettlePoll); err != nil {
			return RebalanceExitError, interrupted
		}
	}
}

// remind fills the report's deployments and the resurrection reminder from
// the VMs a drain stops or an undrain starts.
func (r *drainRun) remind(command string) {
	var deployments []string
	unnamed := 0
	affected := 0
	for i := range r.report.VMs {
		vm := &r.report.VMs[i]
		switch {
		case command == DrainCommand && vm.Fate != DrainStays && vm.Outcome != DrainOutcomeStoppedEarlier:
			continue
		case command == UndrainCommand && vm.Outcome == DrainOutcomeSkipped:
			continue
		}
		affected++
		if vm.Deployment == "" {
			unnamed++
		} else if !slices.Contains(deployments, vm.Deployment) {
			deployments = append(deployments, vm.Deployment)
		}
	}
	slices.Sort(deployments)
	r.report.Deployments = append([]string{}, deployments...)
	if affected == 0 {
		r.report.Reminder = ""
		return
	}
	names := make([]string, 0, len(deployments)+1)
	for _, d := range deployments {
		names = append(names, "`"+d+"`")
	}
	if unnamed > 0 {
		names = append(names, fmt.Sprintf("the deployment%s of %d VM%s with no deployment tag", haPlural(unnamed),
			unnamed, haPlural(unnamed)))
	}
	who := haJoinAnd(names)
	if command == DrainCommand {
		r.report.Reminder = "Turn resurrection off for " + who + " until `pve-ops ha undrain " + r.node +
			" --apply` finishes, for example with `bosh update-resurrection off`, because the Health Monitor " +
			"sees the agents of the stopped VMs go quiet and would recreate them."
		return
	}
	r.report.Reminder = "Turn resurrection back on for " + who + " once the VMs run again, for example with " +
		"`bosh update-resurrection on`."
}

// undrain runs the steps of section 23.7 of the design.
func (r *drainRun) undrain(ctx context.Context) DrainReport {
	if r.apply {
		ttl := r.settle + rebalanceRunLockMargin
		if !r.noRebalance {
			ttl = r.settle + r.rebalance.MaxDuration + r.rebalance.SettleTimeout + rebalanceRunLockMargin
		}
		release, ok := r.lock(ctx, ttl)
		if !ok {
			return r.report
		}
		defer release()
	}
	view, err := r.observe(ctx)
	if err != nil {
		return r.readFailed(ctx, err)
	}
	tokened := r.tokened(view)
	r.report.VMs = make([]DrainVM, 0, len(tokened))
	for _, f := range tokened {
		r.report.VMs = append(r.report.VMs, DrainVM{VMID: f.vmid, Name: f.name, Node: f.node,
			Deployment: drainDeploymentOf(f.tags), Outcome: DrainOutcomePlanned})
	}
	r.remind(UndrainCommand)
	if !r.report.GatesOK() {
		return r.halt(RebalanceExitGate, firstFailedGate(r.report.Gates))
	}
	if !r.apply {
		if !r.noRebalance {
			r.warn("with --apply, the undrain runs pve-ops ha rebalance --apply once the VMs run again")
		}
		return r.report
	}
	code, why := RebalanceExitOK, ""
	for _, f := range tokened {
		if ctx.Err() != nil {
			return r.halt(RebalanceExitError, "the run was interrupted before it started every VM the drain "+
				"stopped; run it again to go on")
		}
		vm := r.reportVM(f.vmid)
		err := r.underRecordAndClaim(ctx, f, func(cctx context.Context) error {
			return r.writeState(cctx, f.vmid, false)
		})
		if halt, stop := r.classify(vm, err, DrainOutcomeStarted); stop {
			return r.halt(RebalanceExitError, halt)
		}
		if vm.Outcome == DrainOutcomeBusy || vm.Outcome == DrainOutcomeFailed {
			code = RebalanceExitError
		}
	}
	waitCode, waitWhy := r.waitFor(ctx, haRawStateStarted, r.vmsWith(DrainOutcomeStarted))
	if code == RebalanceExitOK {
		code, why = waitCode, waitWhy
	}
	r.remind(UndrainCommand)
	if code != RebalanceExitOK {
		if why == "" {
			n := r.report.Count(DrainOutcomeBusy) + r.report.Count(DrainOutcomeFailed)
			why = fmt.Sprintf("the undrain left %d VM%s stopped; run it again to start %s", n, haPlural(n),
				haThemOrIt(n))
		}
		if !r.noRebalance {
			why += ", and the rebalance didn't run"
		}
		return r.halt(code, why)
	}
	if r.noRebalance {
		return r.report
	}
	rr := newRebalanceRun(r.deps, r.rebalance, r.io)
	rr.runLockHeld = true
	rep := rr.run(ctx)
	r.report.Rebalance = &rep
	if rep.ExitCode != RebalanceExitOK {
		return r.halt(rep.ExitCode, "the rebalance ended: "+rep.Halted)
	}
	return r.report
}

// tokened returns this Director's VMs whose HA resource carries a drain token
// that names the node, wherever they now sit, in VMID order.
func (r *drainRun) tokened(view *drainView) []*rebalanceVMFacts {
	var out []*rebalanceVMFacts
	for i := range view.snap.vms {
		f := &view.snap.vms[i]
		if f.res == nil {
			continue
		}
		if by, ok := ha.DrainStopped(f.res.Comment); ok && by == r.node {
			out = append(out, f)
		}
	}
	return out
}

// haPlural returns "s" unless n is 1.
func haPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// haThemOrIt returns "it" for one and "them" otherwise.
func haThemOrIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// haJoinAnd joins names with commas and a final "and", Oxford style.
func haJoinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}
