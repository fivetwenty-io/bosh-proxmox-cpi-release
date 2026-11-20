package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rebalanceOp prefixes the logs of pve-ops ha rebalance.
const rebalanceOp = "ha rebalance"

// The defaults and bounds of pve-ops ha rebalance.
const (
	RebalanceDefaultMaxMoves      = 10
	RebalanceDefaultSettleTimeout = 10 * time.Minute
	RebalanceDefaultMaxDuration   = 60 * time.Minute
	RebalanceDefaultStart         = 0.15
	RebalanceDefaultStop          = 0.05
	// RebalanceMinSettleTimeout is the shortest settle timeout the command
	// takes.
	RebalanceMinSettleTimeout = time.Minute
)

// The exit codes a rebalance report carries.
const (
	RebalanceExitOK    = 0
	RebalanceExitError = 1
	RebalanceExitGate  = 3
)

// RebalanceRunLockName is the logical name of the run lock, whose sentinel
// pool is bosh-lock-ha-rebalance.
const RebalanceRunLockName = "ha-rebalance"

// rebalanceRunLockMargin is what the run lock's lease adds to the run's
// longest time, because a sentinel pool has no renewal.
const rebalanceRunLockMargin = 5 * time.Minute

// rebalanceWriteTimeout bounds each write once it starts. Every write runs on
// a context detached from the run's, so a signal never leaves a write's
// outcome unknown.
const rebalanceWriteTimeout = 10 * time.Second

// rebalanceConfirmWait is the wait before the check that the VM stayed on its
// target, which covers two rounds of the CRM.
const rebalanceConfirmWait = 20 * time.Second

// rebalanceSettlePoll is the pause between the reads of the settle wait.
const rebalanceSettlePoll = 5 * time.Second

// RebalanceOptions shape a run of pve-ops ha rebalance.
type RebalanceOptions struct {
	// Apply makes the moves. Without it the run reads the cluster, takes no
	// lock, and writes nothing.
	Apply bool
	// MaxMoves caps the moves of one run.
	MaxMoves int
	// SettleTimeout bounds the wait for each move to settle, and for the
	// cluster to settle before each move.
	SettleTimeout time.Duration
	// MaxDuration bounds the run. No move starts after it passes.
	MaxDuration time.Duration
	// StartThreshold and StopThreshold are the hysteresis of memory evening.
	StartThreshold, StopThreshold float64
	// RestoreOnly skips memory evening.
	RestoreOnly bool
}

// RebalanceOutcome is how a move or a resumed move ended.
type RebalanceOutcome string

// The outcomes of a move.
const (
	// RebalancePlanned is a move a dry run plans, or one an applied run
	// planned and did not reach.
	RebalancePlanned RebalanceOutcome = "planned"
	// RebalanceMoved is a move that landed and ended clean.
	RebalanceMoved RebalanceOutcome = "moved"
	// RebalanceRefused is a move that did not start, or that PVE refused,
	// and that left the VM where it was with its failback restored.
	RebalanceRefused RebalanceOutcome = "refused"
	// RebalanceFailed is a move whose migration failed, or that landed and
	// moved again.
	RebalanceFailed RebalanceOutcome = "failed"
	// RebalanceLeftPending is a move that a rerun has to finish, because the
	// VM keeps the rebalance mark.
	RebalanceLeftPending RebalanceOutcome = "left_pending"
	// RebalanceAborted is a move of an earlier run that the resume phase
	// undid, because the VM never left its node.
	RebalanceAborted RebalanceOutcome = "aborted"
	// RebalanceFinished is a move of an earlier run that the resume phase
	// finished, because the VM had landed on its target.
	RebalanceFinished RebalanceOutcome = "finished"
)

// The names of the gates of a rebalance.
const (
	RebalanceGateProfile  = "profile"
	RebalanceGateQuorum   = "quorum"
	RebalanceGateNodes    = "nodes"
	RebalanceGateManager  = "ha_manager"
	RebalanceGateSettled  = "settled"
	RebalanceGateJournal  = "journal"
	RebalanceGateActivity = "cpi_activity"
	RebalanceGateRunLock  = "run_lock"
)

// RebalanceGate is one gate as the run last checked it.
type RebalanceGate struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// RebalanceNode is a node's committed memory before and after the plan.
type RebalanceNode struct {
	Node            string  `json:"node"`
	CommittedBefore float64 `json:"committed_before"`
	CommittedAfter  float64 `json:"committed_after"`
	GuestsBefore    int     `json:"guests_before"`
	GuestsAfter     int     `json:"guests_after"`
}

// RebalanceResumed is a VM that carried the rebalance mark of an earlier run.
type RebalanceResumed struct {
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Target string `json:"target"`
	// Action is finish, abort, or wait.
	Action  string           `json:"action"`
	Outcome RebalanceOutcome `json:"outcome"`
	Error   string           `json:"error"`
}

// RebalanceMove is one move as the run planned or made it.
type RebalanceMove struct {
	VMID         int               `json:"vmid"`
	Name         string            `json:"name"`
	From         string            `json:"from"`
	To           string            `json:"to"`
	Phase        harebalance.Phase `json:"phase"`
	Reason       string            `json:"reason"`
	PinRewritten bool              `json:"pin_rewritten"`
	Outcome      RebalanceOutcome  `json:"outcome"`
	Error        string            `json:"error"`
	// Seconds is how long an applied move took.
	Seconds float64 `json:"seconds,omitempty"`
}

// RebalanceSkip is a VM of this Director the plan leaves where it is.
type RebalanceSkip struct {
	VMID   int                  `json:"vmid"`
	Name   string               `json:"name,omitempty"`
	Node   string               `json:"node"`
	Kind   harebalance.SkipKind `json:"kind"`
	Reason string               `json:"reason"`
}

// RebalanceReport is what a run did.
type RebalanceReport struct {
	// Cluster is the name /cluster/status gives the cluster, empty until the
	// run read it.
	Cluster  string             `json:"cluster"`
	Applied  bool               `json:"applied"`
	Gates    []RebalanceGate    `json:"gates"`
	Warnings []string           `json:"warnings"`
	Nodes    []RebalanceNode    `json:"nodes"`
	Resumed  []RebalanceResumed `json:"resumed"`
	// Planned is the first plan of an applied run, and Moves the moves it
	// made. A dry run's Moves are its plan, and Planned is empty.
	Planned      []RebalanceMove `json:"planned,omitempty"`
	Moves        []RebalanceMove `json:"moves"`
	Skipped      []RebalanceSkip `json:"skipped"`
	SpreadBefore float64         `json:"spread_before"`
	SpreadAfter  float64         `json:"spread_after"`
	Deferred     int             `json:"deferred"`
	// DiffersFromPlan says the moves an applied run made are not the ones it
	// planned first.
	DiffersFromPlan bool   `json:"differs_from_plan"`
	Halted          string `json:"halted"`
	ExitCode        int    `json:"exit_code"`
}

// Count returns how many moves have outcome o.
func (r *RebalanceReport) Count(o RebalanceOutcome) int {
	n := 0
	for i := range r.Moves {
		if r.Moves[i].Outcome == o {
			n++
		}
	}
	return n
}

// GatesOK reports whether every gate passed.
func (r *RebalanceReport) GatesOK() bool {
	for i := range r.Gates {
		if !r.Gates[i].OK {
			return false
		}
	}
	return true
}

// RebalanceHA plans, and with opts.Apply makes, the live migrations that
// bring this Director's HA VMs back into a good spread. deps.RequestDirectorUUID
// names the Director. The run checks its gates before the first move and again
// from fresh reads before every move, makes one move at a time, and plans
// again after each one. The error is for a run that could not start, and the
// report's ExitCode says how a run that started ended.
func RebalanceHA(ctx context.Context, deps Deps, opts RebalanceOptions) (RebalanceReport, error) {
	deps.RequestDirectorUUID = strings.TrimSpace(deps.RequestDirectorUUID)
	switch {
	case deps.Config == nil:
		return RebalanceReport{}, errors.New("ha rebalance: no CPI configuration")
	case deps.RequestDirectorUUID == "":
		return RebalanceReport{}, errors.New("ha rebalance: no director UUID names the Director whose VMs to move")
	}
	client := unguardedPVE(deps.PVE)
	if client == nil || client.Cluster() == nil || client.QEMU() == nil || client.Nodes() == nil {
		return RebalanceReport{}, errors.New("ha rebalance: no PVE client")
	}
	deps.PVE = client
	opts = opts.withDefaults()
	r := newRebalanceRun(deps, opts, newPVERebalanceIO(deps, opts))
	return r.run(ctx), nil
}

// withDefaults fills the options a caller left zero.
func (o RebalanceOptions) withDefaults() RebalanceOptions {
	if o.MaxMoves <= 0 {
		o.MaxMoves = RebalanceDefaultMaxMoves
	}
	if o.SettleTimeout <= 0 {
		o.SettleTimeout = RebalanceDefaultSettleTimeout
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = RebalanceDefaultMaxDuration
	}
	if o.StartThreshold <= 0 && o.StopThreshold <= 0 {
		o.StartThreshold, o.StopThreshold = RebalanceDefaultStart, RebalanceDefaultStop
	}
	return o
}

// rebalanceRun is one run of the command.
type rebalanceRun struct {
	deps   Deps
	owner  string
	opts   RebalanceOptions
	io     rebalanceIO
	logger *log.Logger

	report RebalanceReport
	// excluded holds the VMs a refusal or a busy lock took out of this run,
	// with the reason, so a later plan does not choose them again.
	excluded map[int]string
	// spreading carries the hysteresis of memory evening from one plan to the
	// next.
	spreading bool
	started   time.Time
	// planned is set once the report holds the first plan.
	planned bool
	// runLockHeld says the caller already holds the run lock, as an undrain
	// does when it hands off to the rebalance, so the run takes no lock.
	runLockHeld bool
}

func newRebalanceRun(deps Deps, opts RebalanceOptions, rio rebalanceIO) *rebalanceRun {
	return &rebalanceRun{deps: deps, owner: deps.RequestDirectorUUID, opts: opts, io: rio,
		logger: deps.Log(context.Background()), excluded: map[int]string{},
		report: RebalanceReport{Applied: opts.Apply, Gates: []RebalanceGate{}, Warnings: []string{},
			Nodes: []RebalanceNode{}, Resumed: []RebalanceResumed{}, Moves: []RebalanceMove{},
			Skipped: []RebalanceSkip{}}}
}

// runLockTTL is the lease of the run lock, which covers the longest run and
// the move that may still run at its end.
func (r *rebalanceRun) runLockTTL() time.Duration {
	return r.opts.MaxDuration + r.opts.SettleTimeout + rebalanceRunLockMargin
}

// halt ends the run with code and the reason.
func (r *rebalanceRun) halt(code int, why string) RebalanceReport {
	r.report.ExitCode, r.report.Halted = code, why
	return r.report
}

func (r *rebalanceRun) run(ctx context.Context) RebalanceReport {
	r.started = r.io.now()
	if r.opts.Apply && !r.runLockHeld {
		release, err := r.io.runLock(ctx, r.runLockTTL())
		switch {
		case errors.Is(err, pve.ErrClusterLockHeld):
			r.setGate(RebalanceGateRunLock, false, "another rebalance holds the run lock bosh-lock-ha-rebalance")
			return r.halt(RebalanceExitGate, RebalanceGateRunLock)
		case err != nil:
			r.setGate(RebalanceGateRunLock, false, "the run lock could not be taken: "+err.Error())
			return r.halt(RebalanceExitError, "the run lock could not be taken: "+err.Error())
		}
		defer release()
	}
	resumed := false
	for moves := 0; ; {
		view, err := r.observe(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return r.halt(RebalanceExitError, "the run was interrupted before its next move; run it again to go on")
		case err != nil:
			return r.halt(RebalanceExitError, "a read failed: "+err.Error())
		}
		r.report.Gates = view.gates
		if view.profile != nil {
			r.report.Cluster = view.profile.ClusterName
		}
		if !r.report.GatesOK() {
			if !r.opts.Apply {
				// A dry run still shows what an applied run would do once
				// the gate clears, and it writes nothing.
				r.resume(ctx, view)
				r.recordPlan(view)
			}
			return r.halt(RebalanceExitGate, firstFailedGate(view.gates))
		}
		if !resumed {
			resumed = true
			code, why := r.resume(ctx, view)
			if code != RebalanceExitOK {
				return r.halt(code, why)
			}
			if r.opts.Apply && len(view.marked()) > 0 {
				continue
			}
		}
		plan := r.recordPlan(view)
		if !r.opts.Apply || len(plan.Moves) == 0 {
			return r.report
		}
		if moves >= r.opts.MaxMoves || !r.io.now().Before(r.started.Add(r.opts.MaxDuration)) {
			r.report.Deferred = len(plan.Moves)
			return r.report
		}
		r.spreading = plan.Spreading
		moves++
		m := plan.Moves[0]
		code, why := r.makeMove(ctx, view, m)
		if code != RebalanceExitOK {
			return r.halt(code, why)
		}
		if ctx.Err() != nil {
			return r.halt(RebalanceExitError, "the run was interrupted after a move ended; run it again to go on")
		}
	}
}

// recordPlan plans from view and records the plan in the report. The first
// plan of a run fills the plan fields, and every plan updates the loads and
// the skipped VMs. It returns the plan with no cap on its moves.
func (r *rebalanceRun) recordPlan(view *rebalanceView) harebalance.Plan {
	model := view.model
	plan := harebalance.Build(model, harebalance.Options{StartThreshold: r.opts.StartThreshold,
		StopThreshold: r.opts.StopThreshold, RestoreOnly: r.opts.RestoreOnly, Spreading: r.spreading})
	r.report.Skipped = rebalanceSkips(plan.Skipped)
	if !r.planned {
		r.planned = true
		r.report.Nodes = rebalanceNodes(plan)
		r.report.SpreadBefore, r.report.SpreadAfter = plan.SpreadBefore, plan.SpreadAfter
		moves := plan.Moves
		if len(moves) > r.opts.MaxMoves {
			moves = moves[:r.opts.MaxMoves]
		}
		listed := make([]RebalanceMove, 0, len(moves))
		for i := range moves {
			listed = append(listed, rebalanceMoveOf(&moves[i]))
		}
		if r.opts.Apply {
			r.report.Planned = listed
		} else {
			r.report.Moves = listed
			r.report.Deferred = len(plan.Moves) - len(moves)
		}
		if view.dlbMembers > 0 && view.profile != nil && view.profile.CRS.AutoRebalance {
			r.warn("the cluster's CRS auto-rebalance is on, so CRS may move DLB members in response to these moves")
		}
	} else {
		r.report.SpreadAfter = plan.SpreadAfter
		after := plan.After
		for i := range r.report.Nodes {
			n := &r.report.Nodes[i]
			n.CommittedAfter, n.GuestsAfter = after[n.Node].Committed, after[n.Node].Guests
		}
	}
	return plan
}

// warn adds a warning once.
func (r *rebalanceRun) warn(w string) {
	for _, have := range r.report.Warnings {
		if have == w {
			return
		}
	}
	r.report.Warnings = append(r.report.Warnings, w)
}

// setGate records gate name, replacing an earlier record of it.
func (r *rebalanceRun) setGate(name string, ok bool, detail string) {
	for i := range r.report.Gates {
		if r.report.Gates[i].Name == name {
			r.report.Gates[i] = RebalanceGate{Name: name, OK: ok, Detail: detail}
			return
		}
	}
	r.report.Gates = append(r.report.Gates, RebalanceGate{Name: name, OK: ok, Detail: detail})
}

func firstFailedGate(gates []RebalanceGate) string {
	for i := range gates {
		if !gates[i].OK {
			return gates[i].Name
		}
	}
	return ""
}

func rebalanceMoveOf(m *harebalance.Move) RebalanceMove {
	return RebalanceMove{VMID: m.VMID, Name: m.Name, From: m.From, To: m.To, Phase: m.Phase, Reason: m.Reason,
		PinRewritten: m.PinRewritten, Outcome: RebalancePlanned}
}

func rebalanceSkips(skips []harebalance.Skip) []RebalanceSkip {
	out := make([]RebalanceSkip, 0, len(skips))
	for i := range skips {
		s := &skips[i]
		out = append(out, RebalanceSkip{VMID: s.VMID, Name: s.Name, Node: s.Node, Kind: s.Kind, Reason: s.Reason})
	}
	return out
}

func rebalanceNodes(plan harebalance.Plan) []RebalanceNode {
	out := make([]RebalanceNode, 0, len(plan.Before))
	for name, before := range plan.Before {
		after := plan.After[name]
		out = append(out, RebalanceNode{Node: name, CommittedBefore: before.Committed,
			CommittedAfter: after.Committed, GuestsBefore: before.Guests, GuestsAfter: after.Guests})
	}
	sortRebalanceNodes(out)
	return out
}

func sortRebalanceNodes(nodes []RebalanceNode) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodes[j].Node < nodes[j-1].Node; j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

// noteMoveDiffers marks the report when the moves made so far are not the
// prefix of the first plan.
func (r *rebalanceRun) noteMoveDiffers() {
	made := r.report.Moves
	for i := range made {
		if i >= len(r.report.Planned) || made[i].VMID != r.report.Planned[i].VMID ||
			made[i].To != r.report.Planned[i].To {
			r.report.DiffersFromPlan = true
			return
		}
	}
}

// rebalanceDetail joins a gate's findings into one detail.
func rebalanceDetail(parts []string) string {
	return strings.Join(parts, ", and ")
}

// rebalanceSeconds rounds d to tenths of a second.
func rebalanceSeconds(d time.Duration) float64 {
	return float64(d.Round(100*time.Millisecond)) / float64(time.Second)
}
