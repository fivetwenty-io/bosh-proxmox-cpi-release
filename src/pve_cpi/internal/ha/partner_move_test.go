package ha_test

import (
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func migrationTask(status string, start, end time.Time) pve.MigrationTask {
	task := pve.MigrationTask{Type: "qmigrate", ID: "100", Status: status, StartTime: start.Unix()}
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
	ok := func(end time.Time) pve.MigrationTask { return migrationTask("OK", end.Add(-time.Minute), end) }
	for _, tc := range []struct {
		name           string
		fromOld, onNow []pve.MigrationTask
		changedAt      time.Time
		want           ha.MigrationEvidence
	}{
		{name: "a migration from the old source after the change", fromOld: []pve.MigrationTask{ok(after)},
			changedAt: changed, want: ha.MigrationProved},
		{name: "no task, as an HA recovery leaves", changedAt: changed, want: ha.MigrationNone},
		{name: "a migration that ended before the change", fromOld: []pve.MigrationTask{ok(changed.Add(-time.Hour))},
			changedAt: changed, want: ha.MigrationNone},
		{name: "a migration within the clock-skew tolerance of the change",
			fromOld: []pve.MigrationTask{ok(changed.Add(time.Minute))}, changedAt: changed, want: ha.MigrationNone},
		{name: "a failed migration", fromOld: []pve.MigrationTask{migrationTask("migration aborted",
			after.Add(-time.Minute), after)}, changedAt: changed, want: ha.MigrationNone},
		{name: "a migration still running", fromOld: []pve.MigrationTask{migrationTask("RUNNING",
			after, time.Time{})}, changedAt: changed, want: ha.MigrationNone},
		{name: "the VM migrated away again after it arrived", fromOld: []pve.MigrationTask{ok(after)},
			onNow: []pve.MigrationTask{ok(after.Add(time.Hour))}, changedAt: changed, want: ha.MigrationNone},
		{name: "a migration away before the one that brought it", fromOld: []pve.MigrationTask{ok(after)},
			onNow: []pve.MigrationTask{ok(after.Add(-10 * time.Minute))}, changedAt: changed,
			want: ha.MigrationProved},
		{name: "no time of change", fromOld: []pve.MigrationTask{ok(after)}, want: ha.MigrationNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ha.MigrationEvidenceOf(tc.fromOld, tc.onNow, tc.changedAt); got != tc.want {
				t.Fatalf("evidence %v, want %v", got, tc.want)
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
				tc.want == ha.PartnerMoveUnproven
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
