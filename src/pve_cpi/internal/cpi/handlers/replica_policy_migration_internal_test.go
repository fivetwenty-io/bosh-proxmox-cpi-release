package handlers

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The logs of the start and migration tasks of VM 100, as PVE writes them. A
// start that received a live migration logs where it listens for it, and only
// a live migration logs the start of the VM on the target.
var (
	incomingStart = []string{"migration listens on unix:/run/qemu-server/100.migrate", "TASK OK"}
	plainStart    = []string{"TASK OK"}
	liveMove      = []string{"starting migration of VM 100 to node 'pve02' (10.0.0.2)",
		"starting VM 100 on remote node 'pve02'", "migration finished successfully (duration 00:00:06)", "TASK OK"}
	offlineMove = []string{"starting migration of VM 100 to node 'pve02' (10.0.0.2)",
		"migration finished successfully (duration 00:00:03)", "TASK OK"}
)

// movedReplicaRig is VM 100 under its widened pin, pve01 the source and pve02
// the partner, with the pin's last change two hours ago. The VM runs on pve02
// now, after a migration from pve01 that ended an hour ago, and PVE runs job
// 100-0 from pve02 back to pve01, the state a planned live migration leaves.
// The last sync from pve02 covers the root volume on pve01.
func movedReplicaRig(t *testing.T) (*replicaRig, time.Time) {
	t.Helper()
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01,pve02", func(c string) string {
		return ha.WithReplicaPartner(ha.WithReplicaChanged(c, changed), "pve02")
	})
	r.repl.source = "pve02"
	r.repl.addJob("100-0", "pve02", "pve01", renderReplicaJobMarker(aaDirector, 1))
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	r.tasks.migrated("pve01", changed.Add(time.Hour-time.Minute), changed.Add(time.Hour), "OK")
	return r, changed
}

// reconcileMoved runs the reconcile of a VM that runs on pve02, the partner,
// with the narrowing barrier of failoverView, and returns the warnings logged.
func reconcileMoved(t *testing.T, r *replicaRig) ([]string, *stagingLog) {
	t.Helper()
	logger, observed := log.NewObservedLogger(log.LevelInfo)
	set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: r.stub.comment}
	err = reconcileOnPartner(crhCtx(), r.deps, logger, "set_vm_metadata", "100", r.repl, failoverView(res),
		r.profile, 100, "pve02", set.Find("bosh-na-100"), "pve01")
	if err != nil {
		t.Fatalf("reconcile on the partner: %v", err)
	}
	var warnings []string
	for _, e := range observed.All() {
		if e.Level >= log.LevelWarn {
			warnings = append(warnings, e.Message)
		}
	}
	return warnings, sl
}

func TestReconcileVMReplica_PlannedMigrationKeepsBothNodesAndMovesThePartner(t *testing.T) {
	r, changed := movedReplicaRig(t)
	logger, observed := log.NewObservedLogger(log.LevelInfo)
	loc := pve.VMLocation{Node: "pve02", Found: true, Tags: "bosh-cpi;" + haReplicaTag}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	pin := set.Find("bosh-na-100")
	if pin == nil || !slices.Equal(pin.NodeNames(), []string{"pve01", "pve02"}) {
		t.Fatalf("pin %+v; a planned migration keeps both nodes", pin)
	}
	if p, ok := ha.ReplicaPartner(pin.Comment); !ok || p != "pve01" {
		t.Fatalf("pin comment %q; the partner token names the old source", pin.Comment)
	}
	if got, ok := ha.ReplicaChanged(pin.Comment); !ok || !got.Equal(changed) {
		t.Fatalf("pin comment %q; the time of the last change stays", pin.Comment)
	}
	if source, partner, ok := ha.ReplicaPinShape(pin); !ok || source != "pve02" || partner != "pve01" {
		t.Fatalf("pin shape %q %q %v, want pve02 the source and pve01 the partner", source, partner, ok)
	}
	if !r.repl.called("status:pve02/100-0") || !r.repl.called("volumes:pve01/local-zfs") {
		t.Fatalf("calls %v; the sync is read on the new source and the copy on the old source", r.repl.calls)
	}
	if r.repl.called("schedule") {
		t.Fatalf("calls %v; a planned migration requests no sync", r.repl.calls)
	}
	for _, e := range observed.All() {
		if e.Level >= log.LevelWarn {
			t.Fatalf("warning %q; a planned migration loses no writes", e.Message)
		}
	}
}

func TestSwapAfterMigrationFollowsTheJobInTheJournal(t *testing.T) {
	r, changed := movedReplicaRig(t)
	r.deps.Config.HA.Mode = haChoiceOf(config.HAModeManaged)
	handle := replicaHandle(t, changed)
	w := widenOf(r, handle)
	w.node = "pve02"
	set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	swapped, undecided, err := w.swapAfterMigration(crhCtx(), set.Find("bosh-na-100"), "pve01")
	if err != nil || !swapped || undecided != "" {
		t.Fatalf("swapped %v undecided %q err %v", swapped, undecided, err)
	}
	rep := handle.Record().Replication
	if rep.Job == nil || rep.Job.Source != "pve02" || rep.Job.Target != "pve01" || rep.Job.Generation != 2 {
		t.Fatalf("record job %+v, want pve02 to pve01 at generation 2", rep.Job)
	}
	if rep.Sync == nil || rep.Sync.Source != "pve02" || rep.Sync.Target != "pve01" {
		t.Fatalf("record sync %+v, want the verified sync from pve02 to pve01", rep.Sync)
	}
	if p, ok := ha.ReplicaPartner(r.pinComment()); !ok || p != "pve01" {
		t.Fatalf("pin comment %q", r.pinComment())
	}
}

func TestSwappedReplicaPinKeepsTheSourceAhead(t *testing.T) {
	pin := &ha.Rule{ID: "bosh-na-100", Strict: true, Nodes: map[string]int{"pve01": 5, "pve02": 2},
		Resources: []string{"vm:100"}, Comment: ha.WithReplicaPartner("m", "pve02")}
	got := swappedReplicaPin(pin, "pve02", "pve01")
	if got.Nodes["pve02"] != 5 || got.Nodes["pve01"] != 2 || pin.Nodes["pve01"] != 5 {
		t.Fatalf("swapped %v (input %v); the new source takes the highest priority and the input stays",
			got.Nodes, pin.Nodes)
	}
	if p, ok := ha.ReplicaPartner(got.Comment); !ok || p != "pve01" {
		t.Fatalf("swapped comment %q", got.Comment)
	}
}

func TestReconcileOnPartner_FailoverNarrowsWithTheLostWritesWarning(t *testing.T) {
	cases := map[string]func(r *replicaRig){
		"job gone": func(r *replicaRig) { r.repl.jobs = nil },
		"job not swapped yet": func(r *replicaRig) {
			r.repl.jobs[0].Source, r.repl.jobs[0].Target = "pve01", "pve02"
		},
		"job marked for removal": func(r *replicaRig) { r.repl.jobs[0].RemoveJob = "full" },
		"old source offline":     func(r *replicaRig) { r.profile.Nodes[0].Online = false },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := movedReplicaRig(t)
			setup(r)
			warnings, sl := reconcileMoved(t, r)
			if r.pinNodes() != "pve02" {
				t.Fatalf("pin nodes %q, want the new source alone", r.pinNodes())
			}
			if _, ok := ha.ReplicaPartner(r.pinComment()); ok {
				t.Fatalf("pin comment %q keeps the partner", r.pinComment())
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "after a failover") ||
				!strings.Contains(warnings[0], "are lost") {
				t.Fatalf("warnings %q; a failover warns about the lost writes", warnings)
			}
			if !slices.Equal(sl.events, []string{"request:ignored", "request:started"}) {
				t.Fatalf("events %v; the narrowing runs inside the barrier", sl.events)
			}
		})
	}
}

func TestReconcileOnPartner_ReversedJobWithNoVerifiedSyncNarrows(t *testing.T) {
	cases := map[string]struct {
		setup  func(r *replicaRig, changed time.Time)
		reason string
	}{
		"no sync from the new source yet": {
			setup:  func(r *replicaRig, _ time.Time) { r.repl.status = ha.ReplicaSyncStatus{} },
			reason: "has not synced yet",
		},
		"sync failing": {
			setup: func(r *replicaRig, changed time.Time) {
				r.repl.status = ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour), FailCount: 2}
			},
			reason: "failed 2 time(s)",
		},
		"copy missing on the old source": {
			setup:  func(r *replicaRig, _ time.Time) { r.repl.listed = map[string]bool{} },
			reason: "is not on node pve01 yet",
		},
		"old source unreachable": {
			setup:  func(r *replicaRig, _ time.Time) { r.repl.volumesErr = errors.New("no route to host") },
			reason: "no route to host",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, changed := movedReplicaRig(t)
			tc.setup(r, changed)
			warnings, _ := reconcileMoved(t, r)
			if r.pinNodes() != "pve02" {
				t.Fatalf("pin nodes %q; a move that is not proved a planned migration narrows", r.pinNodes())
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "cannot tell a planned migration from a "+
				"failover") {
				t.Fatalf("warnings %q; the narrowing says the move is undecided", warnings)
			}
			if !slices.Contains(r.repl.calls, "schedule:pve02/100-0") {
				t.Fatalf("calls %v; the narrowing requests a sync from the new source", r.repl.calls)
			}
		})
	}
}

// TestReconcileVMReplica_UndecidedMoveWidensBackOnAVerifiedSync runs the
// narrowing of an undecided move, then the next call after a sync from the
// new source: the pin names the old source again as the partner.
func TestReconcileVMReplica_UndecidedMoveWidensBackOnAVerifiedSync(t *testing.T) {
	r, changed := movedReplicaRig(t)
	r.repl.status = ha.ReplicaSyncStatus{}
	if _, _ = reconcileMoved(t, r); r.pinNodes() != "pve02" {
		t.Fatalf("pin nodes %q after the narrowing", r.pinNodes())
	}
	narrowedAt, ok := ha.ReplicaChanged(r.pinComment())
	if !ok || !narrowedAt.After(changed) {
		t.Fatalf("pin comment %q; the narrowing records its time", r.pinComment())
	}
	r.repl.status = verifiedSync(narrowedAt)
	loc := pve.VMLocation{Node: "pve02", Found: true, Tags: haReplicaTag}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.pinNodes() != "pve01,pve02" && r.pinNodes() != "pve02,pve01" {
		t.Fatalf("pin nodes %q, want the old source back", r.pinNodes())
	}
	if p, ok := ha.ReplicaPartner(r.pinComment()); !ok || p != "pve01" {
		t.Fatalf("pin comment %q", r.pinComment())
	}
}

// TestReconcileOnPartner_VerifiedSyncWithoutAMigrationWarnsOfLostWrites runs
// the reconcile of a VM whose reversed sync is verified, with migration tasks
// that do not prove a planned migration. The pin keeps both nodes, because
// the old source's copy is current again, and the CPI warns that the move may
// have lost writes instead of calling it a planned migration.
func TestReconcileOnPartner_VerifiedSyncWithoutAMigrationWarnsOfLostWrites(t *testing.T) {
	cases := map[string]struct {
		setup func(r *replicaRig, changed time.Time)
		warn  string
	}{
		"no migration task, as an HA recovery leaves": {
			setup: func(r *replicaRig, _ time.Time) { r.tasks.migrations = map[string][]pve.VMTask{} },
			warn:  "after a failover, since no migration task on the old source explains the move",
		},
		"a migration that ended before the pin's last change": {
			setup: func(r *replicaRig, changed time.Time) {
				r.tasks.migrations = map[string][]pve.VMTask{}
				r.tasks.migrated("pve01", changed.Add(-2*time.Minute), changed.Add(-time.Minute), "OK")
			},
			warn: "after a failover, since no migration task on the old source explains the move",
		},
		"a migration that failed": {
			setup: func(r *replicaRig, changed time.Time) {
				r.tasks.migrations = map[string][]pve.VMTask{}
				r.tasks.migrated("pve01", changed.Add(time.Hour-time.Minute), changed.Add(time.Hour),
					"migration aborted")
			},
			warn: "after a failover, since no migration task on the old source explains the move",
		},
		"the VM migrated away and back, then failed over": {
			setup: func(r *replicaRig, changed time.Time) {
				r.tasks.migrated("pve02", changed.Add(time.Hour+time.Minute), changed.Add(time.Hour+2*time.Minute),
					"OK")
			},
			warn: "after a failover, since no migration task on the old source explains the move",
		},
		"the old source's tasks cannot be read": {
			setup: func(r *replicaRig, _ time.Time) { r.tasks.err["pve01"] = errors.New("needs Sys.Audit") },
			warn:  "could not read the task lists or task logs that would prove a planned migration",
		},
		"the new source's tasks cannot be read": {
			setup: func(r *replicaRig, _ time.Time) { r.tasks.err["pve02"] = errors.New("needs Sys.Audit") },
			warn:  "could not read the task lists or task logs that would prove a planned migration",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, changed := movedReplicaRig(t)
			tc.setup(r, changed)
			warnings, sl := reconcileMoved(t, r)
			set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
			if err != nil {
				t.Fatal(err)
			}
			pin := set.Find("bosh-na-100")
			if source, partner, ok := ha.ReplicaPinShape(pin); !ok || source != "pve02" || partner != "pve01" {
				t.Fatalf("pin shape %q %q %v; the verified sync keeps both nodes and moves the partner", source,
					partner, ok)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.warn) ||
				!strings.Contains(warnings[0], "were lost") || strings.Contains(warnings[0], "by a planned migration") {
				t.Fatalf("warnings %q; a move no migration task proves warns of lost writes", warnings)
			}
			if len(sl.events) != 0 || r.repl.called("schedule") {
				t.Fatalf("events %v calls %v; a pin that keeps both nodes needs no barrier and no sync", sl.events,
					r.repl.calls)
			}
		})
	}
}

// TestReconcileOnPartner_ReadsTheMigrationTasksOfBothNodes checks that the
// planned migration is proved from the old source's task list and checked
// against the new source's.
func TestReconcileOnPartner_ReadsTheMigrationTasksOfBothNodes(t *testing.T) {
	r, _ := movedReplicaRig(t)
	if warnings, _ := reconcileMoved(t, r); len(warnings) != 0 {
		t.Fatalf("warnings %q; a planned migration loses no writes", warnings)
	}
	want := []string{"migrations:pve01", "migrations:pve02", "starts:pve01", "starts:pve02"}
	if !slices.Equal(r.tasks.calls, want) {
		t.Fatalf("task listings %v, want the migrations and then the starts of the old source and the new one",
			r.tasks.calls)
	}
}

// TestReconcileOnPartner_LiveMigrationWithItsIncomingStartIsPlanned runs the
// reconcile of a VM whose live migration left the target's own start task,
// the qm start --migratedfrom that received the VM, inside the migration's
// window. That start belongs to the migration, so the move is still a planned
// migration that loses no writes.
func TestReconcileOnPartner_LiveMigrationWithItsIncomingStartIsPlanned(t *testing.T) {
	type move struct {
		offset            time.Duration
		startLog, moveLog []string
	}
	cases := map[string]move{
		"the target's clock agrees with the source's": {offset: 2 * time.Second, startLog: incomingStart},
		"the target's clock runs four minutes ahead":  {offset: 4 * time.Minute, startLog: incomingStart},
		"an offline relocation that HA started on the target": {offset: 62 * time.Second, startLog: plainStart,
			moveLog: offlineMove},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, changed := movedReplicaRig(t)
			begun := changed.Add(time.Hour - time.Minute)
			received := r.tasks.started("pve02", begun.Add(tc.offset), begun.Add(tc.offset+3*time.Second), "OK")
			r.tasks.logs[received] = tc.startLog
			if tc.moveLog != nil {
				r.tasks.logs[r.tasks.migrations["pve01"][0].UPID] = tc.moveLog
			}
			r.tasks.started("pve01", changed.Add(-time.Hour), changed.Add(-time.Hour+5*time.Second), "OK")
			logger, observed := log.NewObservedLogger(log.LevelInfo)
			set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
			if err != nil {
				t.Fatal(err)
			}
			res := &attachRunRes{log: &stagingLog{}, state: "started", comment: r.stub.comment}
			if err := reconcileOnPartner(crhCtx(), r.deps, logger, "set_vm_metadata", "100", r.repl,
				failoverView(res), r.profile, 100, "pve02", set.Find("bosh-na-100"), "pve01"); err != nil {
				t.Fatalf("reconcile on the partner: %v", err)
			}
			planned := false
			for _, e := range observed.All() {
				if e.Level >= log.LevelWarn {
					t.Fatalf("warning %q; a live migration's own start loses no writes", e.Message)
				}
				planned = planned || strings.Contains(e.Message, "by a planned migration")
			}
			if !planned {
				t.Fatal("the reconcile did not report a planned migration")
			}
			set, err = readHARuleSet(t.Context(), r.deps.PVE.Cluster())
			if err != nil {
				t.Fatal(err)
			}
			if source, partner, ok := ha.ReplicaPinShape(set.Find("bosh-na-100")); !ok || source != "pve02" ||
				partner != "pve01" {
				t.Fatalf("pin shape %q %q %v; a planned migration keeps both nodes and moves the partner", source,
					partner, ok)
			}
		})
	}
}

// TestReconcileOnPartner_RecoveryAfterAPlannedMigrationWarnsOfLostWrites runs
// the reconcile of a VM that a planned migration took from pve01 to pve02,
// that an HA recovery then brought back to pve01, and that a second HA
// recovery took to pve02 again, with no CPI call in between. Each recovery
// starts the VM from its replica and leaves no migration task, so the
// migration task on pve01 is still the latest one. The starts after it show
// the recoveries, and the CPI keeps both nodes but warns that writes may have
// been lost instead of calling the move a planned migration.
func TestReconcileOnPartner_RecoveryAfterAPlannedMigrationWarnsOfLostWrites(t *testing.T) {
	type sequence func(r *replicaRig, ended time.Time)
	incoming := func(r *replicaRig, ended time.Time) {
		upid := r.tasks.started("pve02", ended.Add(-58*time.Second), ended.Add(-55*time.Second), "OK")
		r.tasks.logs[upid] = incomingStart
	}
	backToOld := func(r *replicaRig, ended time.Time) {
		upid := r.tasks.started("pve01", ended.Add(10*time.Minute), ended.Add(10*time.Minute+5*time.Second), "OK")
		r.tasks.logs[upid] = plainStart
	}
	toPartnerAgain := func(r *replicaRig, ended time.Time) {
		upid := r.tasks.started("pve02", ended.Add(20*time.Minute), ended.Add(20*time.Minute+5*time.Second), "OK")
		r.tasks.logs[upid] = plainStart
	}
	cases := map[string][]sequence{
		"both recoveries listed":                        {incoming, backToOld, toPartnerAgain},
		"the old source lost its start when it crashed": {incoming, toPartnerAgain},
		"a target that lists no incoming start":         {backToOld, toPartnerAgain},
		"the recovery to the partner alone":             {toPartnerAgain},
		"the recovery back to the old source alone":     {incoming, backToOld},
		"a second start soon after the incoming one ends": {incoming, func(r *replicaRig, ended time.Time) {
			upid := r.tasks.started("pve02", ended.Add(2*time.Minute), ended.Add(2*time.Minute+5*time.Second), "OK")
			r.tasks.logs[upid] = plainStart
			r.tasks.logs[r.tasks.migrations["pve01"][0].UPID] = liveMove
		}},
		"a recovery to the partner before a migration back and the migration": {incoming,
			func(r *replicaRig, ended time.Time) {
				upid := r.tasks.started("pve02", ended.Add(-40*time.Minute), ended.Add(-40*time.Minute+5*time.Second),
					"OK")
				r.tasks.logs[upid] = plainStart
			}},
		"a recovery start inside the migration's window": {func(r *replicaRig, ended time.Time) {
			upid := r.tasks.started("pve02", ended.Add(-30*time.Second), ended.Add(-25*time.Second), "OK")
			r.tasks.logs[upid] = plainStart
			r.tasks.logs[r.tasks.migrations["pve01"][0].UPID] = liveMove
		}},
		"a recovery start on the partner that failed": {incoming, func(r *replicaRig, ended time.Time) {
			upid := r.tasks.started("pve02", ended.Add(20*time.Minute), ended.Add(20*time.Minute+time.Second),
				"start failed")
			r.tasks.logs[upid] = []string{"start failed: QEMU exited with code 1", "TASK ERROR: start failed"}
		}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			r, changed := movedReplicaRig(t)
			for _, step := range steps {
				step(r, changed.Add(time.Hour))
			}
			warnings, sl := reconcileMoved(t, r)
			set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
			if err != nil {
				t.Fatal(err)
			}
			if source, partner, ok := ha.ReplicaPinShape(set.Find("bosh-na-100")); !ok || source != "pve02" ||
				partner != "pve01" {
				t.Fatalf("pin shape %q %q %v; the verified sync keeps both nodes and moves the partner", source,
					partner, ok)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "started outside a migration since the pin last changed") ||
				!strings.Contains(warnings[0], "were lost") || strings.Contains(warnings[0], "by a planned migration") {
				t.Fatalf("warnings %q; a start after the migration warns that writes may have been lost", warnings)
			}
			if len(sl.events) != 0 || r.repl.called("schedule") {
				t.Fatalf("events %v calls %v; a pin that keeps both nodes needs no barrier and no sync", sl.events,
					r.repl.calls)
			}
		})
	}
}

// TestReconcileOnPartner_UnreadableStartsLeaveTheMigrationUnproven runs the
// reconcile of a planned migration whose start tasks the CPI cannot read on
// one node. A start it can't see may be an HA recovery, so the CPI keeps both
// nodes and warns that it couldn't prove a planned migration.
func TestReconcileOnPartner_UnreadableStartsLeaveTheMigrationUnproven(t *testing.T) {
	for _, node := range []string{"pve01", "pve02"} {
		t.Run(node, func(t *testing.T) {
			r, _ := movedReplicaRig(t)
			r.tasks.startErr[node] = errors.New("needs Sys.Audit")
			warnings, _ := reconcileMoved(t, r)
			set, err := readHARuleSet(t.Context(), r.deps.PVE.Cluster())
			if err != nil {
				t.Fatal(err)
			}
			if source, partner, ok := ha.ReplicaPinShape(set.Find("bosh-na-100")); !ok || source != "pve02" ||
				partner != "pve01" {
				t.Fatalf("pin shape %q %q %v; the verified sync keeps both nodes and moves the partner", source,
					partner, ok)
			}
			if len(warnings) != 1 ||
				!strings.Contains(warnings[0], "could not read the task lists or task logs that would prove a planned migration") {
				t.Fatalf("warnings %q; an unreadable start listing leaves the migration unproven", warnings)
			}
		})
	}
}
