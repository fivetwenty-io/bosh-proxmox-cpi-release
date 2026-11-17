package ha_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func migrationTask(status string, start, end time.Time) pve.VMTask {
	task := pve.VMTask{Type: "qmigrate", ID: "100", Status: status, StartTime: start.Unix()}
	if !end.IsZero() {
		task.EndTime = end.Unix()
	}
	return task
}

// TestMigrationEvidenceOf judges the migration tasks of a VM that runs on its
// pin's partner.
func TestMigrationEvidenceOf(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	after := changed.Add(time.Hour)
	ok := func(end time.Time) pve.VMTask { return migrationTask("OK", end.Add(-time.Minute), end) }
	for _, tc := range []struct {
		name           string
		fromOld, onNow []pve.VMTask
		changedAt      time.Time
		want           ha.MigrationEvidence
	}{
		{name: "a migration from the old source after the change", fromOld: []pve.VMTask{ok(after)},
			changedAt: changed, want: ha.MigrationProved},
		{name: "no task, as an HA recovery leaves", changedAt: changed, want: ha.MigrationNone},
		{name: "a migration that ended before the change", fromOld: []pve.VMTask{ok(changed.Add(-time.Hour))},
			changedAt: changed, want: ha.MigrationNone},
		{name: "a migration within the clock-skew tolerance of the change",
			fromOld: []pve.VMTask{ok(changed.Add(time.Minute))}, changedAt: changed, want: ha.MigrationNone},
		{name: "a failed migration", fromOld: []pve.VMTask{migrationTask("migration aborted",
			after.Add(-time.Minute), after)}, changedAt: changed, want: ha.MigrationNone},
		{name: "a migration still running", fromOld: []pve.VMTask{migrationTask("RUNNING",
			after, time.Time{})}, changedAt: changed, want: ha.MigrationNone},
		{name: "the VM migrated away again after it arrived", fromOld: []pve.VMTask{ok(after)},
			onNow: []pve.VMTask{ok(after.Add(time.Hour))}, changedAt: changed, want: ha.MigrationNone},
		{name: "a migration away before the one that brought it", fromOld: []pve.VMTask{ok(after)},
			onNow: []pve.VMTask{ok(after.Add(-10 * time.Minute))}, changedAt: changed,
			want: ha.MigrationProved},
		{name: "no time of change", fromOld: []pve.VMTask{ok(after)}, want: ha.MigrationNone},
		{name: "a migration away within the clock-skew tolerance before the arrival",
			fromOld: []pve.VMTask{ok(after)}, onNow: []pve.VMTask{ok(after.Add(-4 * time.Minute))},
			changedAt: changed, want: ha.MigrationNone},
		{name: "a migration back on a node clock that runs behind the old source's",
			fromOld: []pve.VMTask{migrationTask("OK", after, after.Add(time.Minute))},
			onNow: []pve.VMTask{migrationTask("OK", after.Add(-80*time.Second),
				after.Add(-20*time.Second))}, changedAt: changed, want: ha.MigrationNone},
		{name: "a migration from the old source that ended with warnings",
			fromOld:   []pve.VMTask{migrationTask("WARNINGS: 2", after.Add(-time.Minute), after)},
			changedAt: changed, want: ha.MigrationProved},
		{name: "a migration away that ended with warnings", fromOld: []pve.VMTask{ok(after)},
			onNow:     []pve.VMTask{migrationTask("WARNINGS: 1", after.Add(time.Hour), after.Add(time.Hour+time.Minute))},
			changedAt: changed, want: ha.MigrationNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := ha.PartnerTasks{FromOld: tc.fromOld, FromNode: tc.onNow}
			if got := ha.MigrationEvidenceOf(tasks, tc.changedAt); got != tc.want {
				t.Fatalf("evidence %v, want %v", got, tc.want)
			}
		})
	}
}

func startTask(status string, start, end time.Time) pve.VMTask {
	task := migrationTask(status, start, end)
	task.Type = "qmstart"
	return task
}

// withUPID gives task a UPID of its own, which names its type and start time.
func withUPID(task pve.VMTask) pve.VMTask {
	task.UPID = fmt.Sprintf("UPID:pve:%08X:%s", task.StartTime, task.Type)
	return task
}

// The logs PVE writes for the tasks of VM 100, one string per line.
var (
	incomingStartLog = []string{"migration listens on unix:/run/qemu-server/100.migrate", "TASK OK"}
	plainStartLog    = []string{"TASK OK"}
	onlineMoveLog    = []string{"2026-11-16 04:37:17 starting migration of VM 100 to node 'pve02' (10.0.0.2)",
		"2026-11-16 04:37:18 starting VM 100 on remote node 'pve02'",
		"2026-11-16 04:37:23 migration finished successfully (duration 00:00:06)", "TASK OK"}
	offlineMoveLog = []string{"2026-11-16 04:37:17 starting migration of VM 100 to node 'pve02' (10.0.0.2)",
		"2026-11-16 04:37:20 migration finished successfully (duration 00:00:03)", "TASK OK"}
)

// TestClassifyTaskLog reads what a task's log shows.
func TestClassifyTaskLog(t *testing.T) {
	start := pve.VMTask{UPID: "UPID:s", Type: "qmstart", ID: "100"}
	move := pve.VMTask{UPID: "UPID:m", Type: "qmigrate", ID: "100"}
	for _, tc := range []struct {
		name    string
		task    pve.VMTask
		log     []string
		want    ha.TaskLogKind
		wantErr bool
	}{
		{name: "a start that received a live migration", task: start, log: incomingStartLog,
			want: ha.TaskLogIncomingStart},
		{name: "a start that received a migration with local disks", task: start,
			log:  []string{"storage migration listens on nbd:unix:/run/qemu-server/100_nbd.migrate:exportname=drive-scsi0"},
			want: ha.TaskLogIncomingStart},
		{name: "a start that received none", task: start, log: plainStartLog, want: ha.TaskLogPlainStart},
		{name: "a start still running", task: start, log: []string{"generating cloud-init ISO"},
			want: ha.TaskLogPlainStart},
		{name: "a live migration", task: move, log: onlineMoveLog, want: ha.TaskLogOnlineMigration},
		{name: "an offline migration", task: move, log: offlineMoveLog, want: ha.TaskLogOfflineMigration},
		{name: "an offline migration that warned", task: move,
			log: []string{"WARN: leftover cloud-init drive", "TASK WARNINGS: 1"}, want: ha.TaskLogOfflineMigration},
		{name: "a live migration of another VM", task: move,
			log: []string{"starting VM 1000 on remote node 'pve02'"}, wantErr: true},
		{name: "a migration whose log has not ended", task: move, log: offlineMoveLog[:2], wantErr: true},
		{name: "a migration that failed", task: move, log: []string{"TASK ERROR: migration aborted"},
			wantErr: true},
		{name: "another task type", task: pve.VMTask{UPID: "UPID:x", Type: "qmstop", ID: "100"},
			log: plainStartLog, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ha.ClassifyTaskLog(tc.task, tc.log)
			if (err != nil) != tc.wantErr || err == nil && got != tc.want {
				t.Fatalf("kind %v err %v, want %v and an error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestMigrationEvidenceOfStarts judges the start tasks of a VM whose move a
// migration from the old source explains. The migration ran on pve01 from an
// hour after the pin's last change for a minute, and its log reads online
// unless a row says otherwise. A row with an earlier move adds a migration on
// pve01 from ten minutes after the change for a minute, with that log.
func TestMigrationEvidenceOfStarts(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	begun, ended := changed.Add(time.Hour), changed.Add(time.Hour+time.Minute)
	type start struct {
		at  time.Time
		log ha.TaskLogKind
	}
	incoming := func(at time.Time) start { return start{at, ha.TaskLogIncomingStart} }
	plain := func(at time.Time) start { return start{at, ha.TaskLogPlainStart} }
	unread := func(at time.Time) start { return start{at, ha.TaskLogUnread} }
	for _, tc := range []struct {
		name         string
		moved        ha.TaskLogKind
		earlierMove  ha.TaskLogKind
		startsOnOld  []time.Time
		startsOnNode []start
		status       string
		want         ha.MigrationEvidence
	}{
		{name: "a live migration with no start listed", want: ha.MigrationProved},
		{name: "the incoming start of a live migration on the target",
			startsOnNode: []start{incoming(begun.Add(2 * time.Second))}, want: ha.MigrationProved},
		{name: "the target's clock a few minutes ahead of the source's",
			startsOnNode: []start{incoming(begun.Add(4 * time.Minute))}, want: ha.MigrationProved},
		{name: "the incoming starts of a failed attempt and of the migration",
			startsOnNode: []start{incoming(begun.Add(-2 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationProved},
		{name: "the start that brought the VM to the old source before the migration",
			startsOnOld: []time.Time{begun.Add(-30 * time.Minute)}, want: ha.MigrationProved},
		{name: "a start on the target before the pin's last change, whose log is not needed",
			startsOnNode: []start{unread(changed.Add(-10 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationProved},
		{name: "an earlier live stay on the target since the pin's last change",
			startsOnNode: []start{incoming(changed.Add(10 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationProved},
		{name: "an HA recovery to the target before a migration back and the migration",
			startsOnNode: []start{plain(changed.Add(20 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationRestarted},
		{name: "an HA recovery to the target just after the pin's last change",
			startsOnNode: []start{plain(changed.Add(time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationRestarted},
		{name: "an earlier offline relocation to the target since the pin's last change",
			earlierMove: ha.TaskLogOfflineMigration,
			startsOnNode: []start{plain(changed.Add(11*time.Minute + 10*time.Second)),
				incoming(begun.Add(2 * time.Second))},
			want: ha.MigrationProved},
		{name: "a second start after an earlier offline relocation", earlierMove: ha.TaskLogOfflineMigration,
			startsOnNode: []start{plain(changed.Add(11*time.Minute + 10*time.Second)),
				plain(changed.Add(13 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want: ha.MigrationRestarted},
		{name: "a start inside the window of an earlier live migration", earlierMove: ha.TaskLogOnlineMigration,
			startsOnNode: []start{plain(changed.Add(10*time.Minute + 30*time.Second)),
				incoming(begun.Add(2 * time.Second))},
			want: ha.MigrationRestarted},
		{name: "a start inside the window of an earlier migration whose log was not read",
			earlierMove: ha.TaskLogUnread,
			startsOnNode: []start{plain(changed.Add(10*time.Minute + 30*time.Second)),
				incoming(begun.Add(2 * time.Second))},
			want: ha.MigrationUnread},
		{name: "an earlier start on the target whose log was not read",
			startsOnNode: []start{unread(changed.Add(20 * time.Minute)), incoming(begun.Add(2 * time.Second))},
			want:         ha.MigrationUnread},
		{name: "an HA recovery back to the old source and then to the target again",
			startsOnOld:  []time.Time{ended.Add(10 * time.Minute)},
			startsOnNode: []start{incoming(begun.Add(2 * time.Second)), plain(ended.Add(20 * time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "the same sequence with the old source's start lost in its crash",
			startsOnNode: []start{incoming(begun.Add(2 * time.Second)), plain(ended.Add(20 * time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "a recovery start soon after the migration ended",
			startsOnNode: []start{incoming(begun.Add(2 * time.Second)), plain(ended.Add(time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "a recovery start soon after the migration on a target that lists no incoming start",
			startsOnNode: []start{plain(ended.Add(4 * time.Minute))}, want: ha.MigrationRestarted},
		{name: "a recovery start on the old source alone",
			startsOnOld: []time.Time{ended.Add(10 * time.Minute)}, want: ha.MigrationRestarted},
		{name: "the start after an offline relocation", moved: ha.TaskLogOfflineMigration,
			startsOnNode: []start{plain(ended.Add(10 * time.Second))}, want: ha.MigrationProved},
		{name: "a second start after an offline relocation", moved: ha.TaskLogOfflineMigration,
			startsOnNode: []start{plain(ended.Add(10 * time.Second)), plain(ended.Add(3 * time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "a start clearly after an offline relocation", moved: ha.TaskLogOfflineMigration,
			startsOnNode: []start{plain(ended.Add(time.Hour))}, want: ha.MigrationRestarted},
		{name: "a start that failed", status: "start failed", startsOnNode: []start{plain(ended.Add(time.Hour))},
			want: ha.MigrationRestarted},
		{name: "a start still running", status: "RUNNING", startsOnNode: []start{plain(ended.Add(time.Hour))},
			want: ha.MigrationRestarted},
		{name: "a start whose log was not read", startsOnNode: []start{unread(ended.Add(time.Minute))},
			want: ha.MigrationUnread},
		{name: "a start inside the window of a migration whose log was not read", moved: ha.TaskLogUnread,
			startsOnNode: []start{plain(ended.Add(time.Minute))}, want: ha.MigrationUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migration := withUPID(migrationTask("OK", begun, ended))
			moved := tc.moved
			if moved == ha.TaskLogUnread && !strings.Contains(tc.name, "not read") {
				moved = ha.TaskLogOnlineMigration
			}
			tasks := ha.PartnerTasks{FromOld: []pve.VMTask{migration},
				Logs: map[string]ha.TaskLogKind{migration.UPID: moved}}
			if tc.earlierMove != ha.TaskLogUnread || strings.Contains(tc.name, "earlier migration") {
				earlier := withUPID(migrationTask("OK", changed.Add(10*time.Minute), changed.Add(11*time.Minute)))
				tasks.FromOld = append(tasks.FromOld, earlier)
				tasks.Logs[earlier.UPID] = tc.earlierMove
			}
			for _, at := range tc.startsOnOld {
				tasks.StartsOnOld = append(tasks.StartsOnOld, withUPID(startTask("OK", at, at.Add(5*time.Second))))
			}
			for _, st := range tc.startsOnNode {
				status, end := "OK", st.at.Add(5*time.Second)
				if tc.status != "" {
					status = tc.status
				}
				if status == "RUNNING" {
					end = time.Time{}
				}
				task := withUPID(startTask(status, st.at, end))
				tasks.StartsOnNode = append(tasks.StartsOnNode, task)
				tasks.Logs[task.UPID] = st.log
			}
			if got := ha.MigrationEvidenceOf(tasks, changed); got != tc.want {
				t.Fatalf("evidence %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMigrationEvidenceOfALiveMigrationOnPVE92 judges the tasks PVE 9.2.28
// recorded for an HA live migration of VM 210032. The source logged its
// qmigrate task from 04:37:17 to 04:37:23 UTC, and the target logged a qmstart
// task of its own from 04:37:18 to 04:37:19, inside the migration's window,
// both as root@pam and both with status OK. The target's start received the
// migration, so the move is a planned migration. A start at the same time that
// received none is a recovery or a restart, and it counts.
func TestMigrationEvidenceOfALiveMigrationOnPVE92(t *testing.T) {
	at := func(hms string) time.Time {
		v, err := time.Parse(time.DateTime, "2026-11-16 "+hms)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	migration := pve.VMTask{UPID: "UPID:az2-2:00001000:00000000:6919B4AD:qmigrate:210032:root@pam:",
		Type: "qmigrate", ID: "210032", Status: "OK", StartTime: at("04:37:17").Unix(),
		EndTime: at("04:37:23").Unix()}
	incoming := pve.VMTask{UPID: "UPID:az2-1:00002000:00000000:6919B4AE:qmstart:210032:root@pam:",
		Type: "qmstart", ID: "210032", Status: "OK", StartTime: at("04:37:18").Unix(),
		EndTime: at("04:37:19").Unix()}
	changed := at("04:37:17").Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		start ha.TaskLogKind
		want  ha.MigrationEvidence
	}{
		{"the incoming start", ha.TaskLogIncomingStart, ha.MigrationProved},
		{"a plain start in its place", ha.TaskLogPlainStart, ha.MigrationRestarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := ha.PartnerTasks{FromOld: []pve.VMTask{migration}, StartsOnNode: []pve.VMTask{incoming},
				Logs: map[string]ha.TaskLogKind{migration.UPID: ha.TaskLogOnlineMigration, incoming.UPID: tc.start}}
			if got := ha.MigrationEvidenceOf(tasks, changed); got != tc.want {
				t.Fatalf("evidence %v, want %v", got, tc.want)
			}
		})
	}
}

// partnerTaskReader serves each node's migration and start tasks, fails a
// listing named in errs, and records every listing it serves.
type partnerTaskReader struct {
	migrations, starts map[string][]pve.VMTask
	errs               map[string]error
	// logs holds each task's log by UPID, and a task without one fails its
	// log read.
	logs  map[string][]string
	calls []string
}

func (r *partnerTaskReader) TaskLog(_ context.Context, node, upid string) ([]string, error) {
	r.calls = append(r.calls, "log:"+upid)
	lines, ok := r.logs[upid]
	if !ok {
		return nil, errors.New("no log for task " + upid + " on " + node)
	}
	return lines, nil
}

func (r *partnerTaskReader) MigrationTasks(_ context.Context, node string, _ int, _ time.Time) ([]pve.VMTask, error) {
	r.calls = append(r.calls, "migrations:"+node)
	return r.migrations[node], r.errs["migrations:"+node]
}

func (r *partnerTaskReader) StartTasks(_ context.Context, node string, _ int, _ time.Time) ([]pve.VMTask, error) {
	r.calls = append(r.calls, "starts:"+node)
	return r.starts[node], r.errs["starts:"+node]
}

// TestReadMigrationEvidence reads the task lists of VM 100 on pve02, its pin's
// partner, with pve01 the old source.
func TestReadMigrationEvidence(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	begun, ended := changed.Add(time.Hour), changed.Add(time.Hour+time.Minute)
	migration := withUPID(migrationTask("OK", begun, ended))
	migrated := map[string][]pve.VMTask{"pve01": {migration}}
	incoming := withUPID(startTask("OK", begun.Add(2*time.Second), begun.Add(3*time.Second)))
	recovery := withUPID(startTask("OK", ended.Add(time.Minute), ended.Add(time.Minute+time.Second)))
	later := withUPID(startTask("OK", ended.Add(time.Hour), ended.Add(time.Hour+time.Second)))
	all := []string{"migrations:pve01", "migrations:pve02", "starts:pve01", "starts:pve02"}
	for _, tc := range []struct {
		name       string
		migrations map[string][]pve.VMTask
		starts     map[string][]pve.VMTask
		logs       map[string][]string
		errs       map[string]error
		want       ha.MigrationEvidence
		calls      []string
		cause      string
	}{
		{name: "a planned migration", migrations: migrated, want: ha.MigrationProved, calls: all},
		{name: "a planned migration whose target lists its incoming start", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {incoming}},
			logs:   map[string][]string{incoming.UPID: incomingStartLog},
			want:   ha.MigrationProved, calls: append(slices.Clone(all), "log:"+incoming.UPID)},
		{name: "a recovery after the migration", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {later}},
			logs:   map[string][]string{later.UPID: plainStartLog},
			want:   ha.MigrationRestarted, calls: append(slices.Clone(all), "log:"+later.UPID)},
		{name: "a recovery inside the window of a live migration", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {incoming, recovery}},
			logs: map[string][]string{incoming.UPID: incomingStartLog, recovery.UPID: plainStartLog,
				migration.UPID: onlineMoveLog},
			want: ha.MigrationRestarted,
			calls: append(slices.Clone(all), "log:"+incoming.UPID, "log:"+recovery.UPID,
				"log:"+migration.UPID)},
		{name: "the start after an offline relocation", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {recovery}},
			logs:   map[string][]string{recovery.UPID: plainStartLog, migration.UPID: offlineMoveLog},
			want:   ha.MigrationProved, calls: append(slices.Clone(all), "log:"+recovery.UPID, "log:"+migration.UPID)},
		{name: "a start whose log cannot be read", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {recovery}}, want: ha.MigrationUnread,
			calls: append(slices.Clone(all), "log:"+recovery.UPID),
			cause: "read the log of task " + recovery.UPID + " on node pve02: no log for task " + recovery.UPID +
				" on pve02"},
		{name: "no migration, so the starts are not read", want: ha.MigrationNone,
			calls: []string{"migrations:pve01", "migrations:pve02"}},
		{name: "the old source's starts cannot be read", migrations: migrated,
			errs: map[string]error{"starts:pve01": errors.New("needs Sys.Audit")}, want: ha.MigrationUnread,
			calls: all[:3], cause: "list the start tasks on node pve01: needs Sys.Audit"},
		{name: "the node's starts cannot be read", migrations: migrated,
			errs: map[string]error{"starts:pve02": errors.New("needs Sys.Audit")}, want: ha.MigrationUnread,
			calls: all, cause: "list the start tasks on node pve02: needs Sys.Audit"},
		{name: "the old source's migrations cannot be read",
			errs: map[string]error{"migrations:pve01": errors.New("needs Sys.Audit")}, want: ha.MigrationUnread,
			calls: all[:1], cause: "list the migration tasks on node pve01: needs Sys.Audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &partnerTaskReader{migrations: tc.migrations, starts: tc.starts, errs: tc.errs, logs: tc.logs}
			got, err := ha.ReadMigrationEvidence(t.Context(), reader, 100, "pve02", "pve01", changed)
			if got != tc.want {
				t.Fatalf("evidence %v, want %v (%v)", got, tc.want, err)
			}
			if !slices.Equal(reader.calls, tc.calls) {
				t.Fatalf("reads %v, want %v", reader.calls, tc.calls)
			}
			if tc.cause == "" && err != nil || tc.cause != "" && (err == nil || err.Error() != tc.cause) {
				t.Fatalf("cause %v, want %q", err, tc.cause)
			}
		})
	}
}

// TestJudgePartnerMove decides how a VM on its pin's partner is reconciled.
func TestJudgePartnerMove(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	base := func() ha.PartnerMove {
		return ha.PartnerMove{Node: "pve02", OldSource: "pve01", OldSourceOnline: true, ChangedAt: changed,
			Job: &ha.ReplicaJobState{ID: "100-0", Source: "pve02", Target: "pve01"}, Synced: true,
			Migration: ha.MigrationProved}
	}
	for _, tc := range []struct {
		name string
		edit func(m *ha.PartnerMove)
		want ha.PartnerMoveVerdict
	}{
		{name: "a planned migration", want: ha.PartnerMovePlanned},
		{name: "a failover whose reversed sync ran", edit: func(m *ha.PartnerMove) { m.Migration = ha.MigrationNone },
			want: ha.PartnerMoveRecovered},
		{name: "tasks unread", edit: func(m *ha.PartnerMove) { m.Migration = ha.MigrationUnread },
			want: ha.PartnerMoveUnproven},
		{name: "a start after the migration", edit: func(m *ha.PartnerMove) { m.Migration = ha.MigrationRestarted },
			want: ha.PartnerMoveRestarted},
		{name: "no sync yet", edit: func(m *ha.PartnerMove) { m.Synced = false }, want: ha.PartnerMoveUndecided},
		{name: "no time of change", edit: func(m *ha.PartnerMove) { m.ChangedAt = time.Time{} },
			want: ha.PartnerMoveUndecided},
		{name: "no job", edit: func(m *ha.PartnerMove) { m.Job = nil }, want: ha.PartnerMoveFailover},
		{name: "job disabled", edit: func(m *ha.PartnerMove) { m.Job.Disabled = true }, want: ha.PartnerMoveFailover},
		{name: "job marked for removal", edit: func(m *ha.PartnerMove) { m.Job.RemoveJob = "local" },
			want: ha.PartnerMoveFailover},
		{name: "job not swapped", edit: func(m *ha.PartnerMove) { m.Job.Source, m.Job.Target = "pve01", "pve02" },
			want: ha.PartnerMoveFailover},
		{name: "old source offline", edit: func(m *ha.PartnerMove) { m.OldSourceOnline = false },
			want: ha.PartnerMoveFailover},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			if tc.edit != nil {
				tc.edit(&m)
			}
			got := ha.JudgePartnerMove(m)
			if got != tc.want {
				t.Fatalf("verdict %v, want %v", got, tc.want)
			}
			keeps := tc.want == ha.PartnerMovePlanned || tc.want == ha.PartnerMoveRecovered ||
				tc.want == ha.PartnerMoveUnproven || tc.want == ha.PartnerMoveRestarted
			if got.KeepsBothNodes() != keeps {
				t.Fatalf("verdict %v keeps both nodes %v, want %v", got, got.KeepsBothNodes(), keeps)
			}
		})
	}
}

// TestSyncAfterChange reads the status half of the sync gate.
func TestSyncAfterChange(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name      string
		status    ha.ReplicaSyncStatus
		changedAt time.Time
		want      bool
		reason    string
	}{
		{name: "a sync after the change", status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour)},
			changedAt: changed, want: true},
		{name: "no time of change", status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour)},
			reason: "not recorded"},
		{name: "an error", status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour), Error: "boom"},
			changedAt: changed, reason: "boom"},
		{name: "failures", status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour), FailCount: 2},
			changedAt: changed, reason: "failed"},
		{name: "never synced", changedAt: changed, reason: "not synced yet"},
		{name: "a sync within the tolerance", status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Minute)},
			changedAt: changed, reason: "not clearly after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ha.SyncAfterChange(tc.status, tc.changedAt)
			if got != tc.want || !strings.Contains(reason, tc.reason) || got && reason != "" {
				t.Fatalf("SyncAfterChange = %v %q, want %v with %q", got, reason, tc.want, tc.reason)
			}
		})
	}
}
