package ha_test

import (
	"context"
	"errors"
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

// TestMigrationEvidenceOfStarts judges the start tasks of a VM whose move a
// migration from the old source explains. The migration ran on pve01 from an
// hour and a minute after the pin's last change to an hour after it.
func TestMigrationEvidenceOfStarts(t *testing.T) {
	changed := time.Unix(1_700_000_000, 0)
	begun, ended := changed.Add(time.Hour), changed.Add(time.Hour+time.Minute)
	started := func(at time.Time) pve.VMTask { return startTask("OK", at, at.Add(5*time.Second)) }
	for _, tc := range []struct {
		name                      string
		startsOnOld, startsOnNode []pve.VMTask
		want                      ha.MigrationEvidence
	}{
		{name: "a live migration with no start listed", want: ha.MigrationProved},
		{name: "the incoming start of a live migration on the target",
			startsOnNode: []pve.VMTask{started(begun.Add(2 * time.Second))}, want: ha.MigrationProved},
		{name: "the target's clock a few minutes ahead of the source's",
			startsOnNode: []pve.VMTask{started(begun.Add(4 * time.Minute))}, want: ha.MigrationProved},
		{name: "the start that brought the VM to the old source before the migration",
			startsOnOld: []pve.VMTask{started(begun.Add(-30 * time.Minute))}, want: ha.MigrationProved},
		{name: "a start on the target from an earlier stay there",
			startsOnNode: []pve.VMTask{started(begun.Add(-time.Hour)), started(begun.Add(2 * time.Second))},
			want:         ha.MigrationProved},
		{name: "an HA recovery back to the old source and then to the target again",
			startsOnOld:  []pve.VMTask{started(ended.Add(10 * time.Minute))},
			startsOnNode: []pve.VMTask{started(begun.Add(2 * time.Second)), started(ended.Add(20 * time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "the same sequence with the old source's start lost in its crash",
			startsOnNode: []pve.VMTask{started(begun.Add(2 * time.Second)), started(ended.Add(20 * time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "the same sequence on a target that lists no incoming start",
			startsOnNode: []pve.VMTask{started(ended.Add(20 * time.Minute))}, want: ha.MigrationRestarted},
		{name: "a recovery start on the old source alone",
			startsOnOld: []pve.VMTask{started(ended.Add(10 * time.Minute))}, want: ha.MigrationRestarted},
		{name: "a second start soon after the incoming one",
			startsOnNode: []pve.VMTask{started(begun.Add(2 * time.Second)), started(ended.Add(time.Minute))},
			want:         ha.MigrationRestarted},
		{name: "a start that failed", startsOnNode: []pve.VMTask{startTask("start failed", ended.Add(time.Hour),
			ended.Add(time.Hour+time.Second))}, want: ha.MigrationRestarted},
		{name: "a start still running", startsOnNode: []pve.VMTask{startTask("RUNNING", ended.Add(time.Hour),
			time.Time{})}, want: ha.MigrationRestarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := ha.PartnerTasks{FromOld: []pve.VMTask{migrationTask("OK", begun, ended)},
				StartsOnOld: tc.startsOnOld, StartsOnNode: tc.startsOnNode}
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
	calls              []string
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
	migrated := map[string][]pve.VMTask{"pve01": {migrationTask("OK", begun, ended)}}
	all := []string{"migrations:pve01", "migrations:pve02", "starts:pve01", "starts:pve02"}
	for _, tc := range []struct {
		name       string
		migrations map[string][]pve.VMTask
		starts     map[string][]pve.VMTask
		errs       map[string]error
		want       ha.MigrationEvidence
		calls      []string
		cause      string
	}{
		{name: "a planned migration", migrations: migrated, want: ha.MigrationProved, calls: all},
		{name: "a recovery after the migration", migrations: migrated,
			starts: map[string][]pve.VMTask{"pve02": {startTask("OK", ended.Add(time.Hour), ended.Add(time.Hour))}},
			want:   ha.MigrationRestarted, calls: all},
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
			reader := &partnerTaskReader{migrations: tc.migrations, starts: tc.starts, errs: tc.errs}
			got, err := ha.ReadMigrationEvidence(t.Context(), reader, 100, "pve02", "pve01", changed)
			if got != tc.want {
				t.Fatalf("evidence %v, want %v", got, tc.want)
			}
			if !slices.Equal(reader.calls, tc.calls) {
				t.Fatalf("listings %v, want %v", reader.calls, tc.calls)
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
