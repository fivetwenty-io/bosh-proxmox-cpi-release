package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rebalanceView is one fresh read of the cluster, the gates it passes, and the
// model the planner takes.
type rebalanceView struct {
	profile    *ha.Profile
	snap       *rebalanceSnapshot
	gates      []RebalanceGate
	model      harebalance.Model
	plans      map[int]*rebalanceVMPlan
	dlbMembers int
}

// marked returns this Director's VMs whose resource carries a rebalance mark,
// in VMID order.
func (v *rebalanceView) marked() []*rebalanceVMFacts {
	var out []*rebalanceVMFacts
	for i := range v.snap.vms {
		f := &v.snap.vms[i]
		if f.res == nil {
			continue
		}
		if _, ok := ha.RebalancePending(f.res.Comment); ok {
			out = append(out, f)
		}
	}
	return out
}

// rebalanceActivity is what PVE shows of CPI work in flight.
type rebalanceActivity struct {
	// locks lists the live bosh-lock-* sentinel pools other than the run
	// lock, and runLock says whether a live run lock exists that this run
	// does not hold.
	locks   []string
	runLock bool
	// tasks lists the running tasks of the CPI's token user.
	tasks []string
}

// rebalanceJournal is the allocation journal as the gate read it.
type rebalanceJournal struct {
	// configured is false when the configuration names no journal.
	configured bool
	// err is why a configured journal could not be read.
	err error
	// open lists the records in an open state.
	open []aj.Record
}

// rebalanceOpenStates are the journal states that mean a CPI call is at work
// or left work for a reconcile.
var rebalanceOpenStates = []aj.State{aj.Planned, aj.Submitted, aj.Observed, aj.ReconciliationRequired}

// observe waits for the cluster to settle when the run applies, then reads
// the cluster afresh and checks every gate.
func (r *rebalanceRun) observe(ctx context.Context) (*rebalanceView, error) {
	if r.opts.Apply {
		if err := r.waitSettled(ctx); err != nil {
			return nil, err
		}
	}
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
	view := &rebalanceView{profile: profile, snap: snap}
	view.gates = rebalanceGates(r.deps.Config, profile, snap, act, journal, r.opts.Apply)
	if r.opts.Apply && !journal.configured {
		r.warn("the configuration names no allocation journal, so the run can't see the Director's own records; " +
			"the Director's own CPI must have no journal either, or this run may move a VM under a CPI call")
	}
	b := rebalanceModelBuilder{deps: r.deps, owner: r.owner, profile: profile, snap: snap, excluded: r.excluded,
		now: r.io.now()}
	view.model, view.plans, view.dlbMembers = b.build()
	return view, nil
}

// waitSettled waits up to the settle timeout until no HA service is migrating
// or relocating. A wait that runs out returns nil, and the settled gate then
// fails from the fresh read.
func (r *rebalanceRun) waitSettled(ctx context.Context) error {
	deadline := r.io.now().Add(r.opts.SettleTimeout)
	for {
		states, err := r.io.serviceStates(ctx)
		if err != nil {
			return fmt.Errorf("read the HA manager status: %w", err)
		}
		if len(movingServices(states)) == 0 || !r.io.now().Before(deadline) {
			return nil
		}
		if err := r.io.sleep(ctx, rebalanceSettlePoll); err != nil {
			return fmt.Errorf("the wait for the cluster to settle ended: %w", err)
		}
	}
}

// movingServices lists the HA services in migrate or relocate, sorted.
func movingServices(states map[string]string) []string {
	var out []string
	for sid, st := range states {
		if st == "migrate" || st == "relocate" {
			out = append(out, sid)
		}
	}
	slices.Sort(out)
	return out
}

// rebalanceGates checks every gate of section 6 against one fresh read. It is
// pure, so each gate's refusal can be tested on its own.
func rebalanceGates(cfg *config.CPIConfig, profile *ha.Profile, snap *rebalanceSnapshot, act rebalanceActivity,
	journal rebalanceJournal, apply bool,
) []RebalanceGate {
	gates := []RebalanceGate{
		profileGate(cfg, profile),
		quorumGate(profile),
		nodesGate(profile),
		managerGate(profile),
		settledGate(snap.states),
		journalGate(journal, apply),
		activityGate(act),
		runLockGate(act, apply),
	}
	return gates
}

// runLockGate is the run lock gate: an applied run holds the lock, and a dry
// run refuses while another run holds it.
func runLockGate(act rebalanceActivity, apply bool) RebalanceGate {
	if apply {
		return RebalanceGate{Name: RebalanceGateRunLock, OK: true, Detail: "this run holds the run lock"}
	}
	g := RebalanceGate{Name: RebalanceGateRunLock, OK: !act.runLock, Detail: "no other rebalance holds the run lock"}
	if act.runLock {
		g.Detail = "another rebalance holds the run lock bosh-lock-ha-rebalance"
	}
	return g
}

func profileGate(cfg *config.CPIConfig, profile *ha.Profile) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateProfile}
	mode := cfg.HAModeValue()
	switch {
	case mode != config.HAModeAuto && mode != config.HAModeManaged:
		g.Detail = "pve.ha.mode is " + mode + ", and the rebalance runs only in auto or managed mode"
	case !profile.WritesAllowed():
		g.Detail = "the HA profile does not let the CPI write HA state"
		if profile != nil && profile.Reason != "" {
			g.Detail += " (" + profile.Reason + ")"
		}
	default:
		g.OK, g.Detail = true, "pve.ha.mode is "+mode+", and the HA profile lets the CPI write HA state"
	}
	return g
}

func quorumGate(profile *ha.Profile) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateQuorum}
	if profile == nil || !profile.Quorate || profile.Health == nil || !profile.Health.Quorate {
		g.Detail = "the cluster does not report quorum"
		return g
	}
	g.OK, g.Detail = true, "quorum is held"
	return g
}

func nodesGate(profile *ha.Profile) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateNodes}
	if profile == nil || len(profile.Nodes) == 0 || profile.Health == nil {
		g.Detail = "the HA profile lists no node health"
		return g
	}
	var bad []string
	for i := range profile.Nodes {
		n := profile.Nodes[i]
		entry := healthEntry(profile.Health, n.Name)
		switch {
		case !n.Online:
			bad = append(bad, "node "+n.Name+" is offline")
		case entry == nil:
			bad = append(bad, "node "+n.Name+" has no health entry")
		case entry.Maintenance:
			bad = append(bad, "node "+n.Name+" is in maintenance, so the rolling patch hasn't finished")
		case entry.DeadOrFenced || !entry.Placeable:
			bad = append(bad, "node "+n.Name+" can't take work now")
		}
	}
	if len(bad) > 0 {
		g.Detail = rebalanceDetail(bad)
		return g
	}
	g.OK = true
	g.Detail = fmt.Sprintf("all %d nodes are online, and none is in maintenance", len(profile.Nodes))
	return g
}

// healthEntry returns the health entry of node, or nil.
func healthEntry(h *pve.NodeHealthReport, node string) *pve.NodeHealthEntry {
	if h == nil {
		return nil
	}
	for i := range h.Nodes {
		if h.Nodes[i].Name == node {
			return &h.Nodes[i]
		}
	}
	return nil
}

func managerGate(profile *ha.Profile) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateManager}
	switch {
	case profile == nil || profile.CRMMaster == "":
		g.Detail = "the HA manager has no master"
	case profile.Health == nil || profile.Health.HA != pve.HAViewManagerStatus:
		g.Detail = "the HA manager's status is not fresh"
	default:
		g.OK, g.Detail = true, "the HA manager's master is "+profile.CRMMaster+", and its status is fresh"
	}
	return g
}

func settledGate(states map[string]string) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateSettled}
	if moving := movingServices(states); len(moving) > 0 {
		g.Detail = "PVE HA is still moving " + strings.Join(moving, ", ")
		return g
	}
	g.OK, g.Detail = true, "no HA service is migrating or relocating"
	return g
}

func journalGate(j rebalanceJournal, apply bool) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateJournal}
	switch {
	case !j.configured:
		g.OK, g.Detail = true, "no allocation journal is configured"
	case j.err != nil && apply:
		g.Detail = "the allocation journal can't be read here (" + j.err.Error() + "), so --apply must run as " +
			"vcap on the Director"
	case j.err != nil:
		g.OK, g.Detail = true, "unchecked, because the allocation journal can't be read here ("+j.err.Error()+")"
	case len(j.open) > 0:
		ids := make([]string, 0, len(j.open))
		for i := range j.open {
			ids = append(ids, fmt.Sprintf("%s (%s)", j.open[i].ID, j.open[i].State))
		}
		g.Detail = "the allocation journal holds open records: " + strings.Join(ids, ", ")
	default:
		g.OK, g.Detail = true, "the allocation journal holds no open record"
	}
	return g
}

func activityGate(act rebalanceActivity) RebalanceGate {
	g := RebalanceGate{Name: RebalanceGateActivity}
	var busy []string
	if len(act.locks) > 0 {
		busy = append(busy, "the CPI locks "+strings.Join(act.locks, ", ")+" are held")
	}
	if len(act.tasks) > 0 {
		busy = append(busy, "the CPI's token user runs the tasks "+strings.Join(act.tasks, ", "))
	}
	if len(busy) > 0 {
		g.Detail = rebalanceDetail(busy)
		return g
	}
	g.OK, g.Detail = true, "no CPI task or lock is active"
	return g
}

// openJournalRecords returns the records in an open state.
func openJournalRecords(records []aj.Record) []aj.Record {
	var open []aj.Record
	for i := range records {
		if slices.Contains(rebalanceOpenStates, records[i].State) {
			open = append(open, records[i])
		}
	}
	return open
}

// liveSentinel reports whether a sentinel pool's comment records an expiry
// still ahead of now. A comment with no expiry reads as live, because the
// gate fails closed.
func liveSentinel(comment string, now time.Time) bool {
	exp, ok := pve.ClusterLockExpiry(comment)
	return !ok || exp.After(now)
}
