package ha

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ReplicaPinShape reads a replicate pin. A strict, enabled pin that names one
// node and no partner has that node as its source. A pin that names two
// nodes, one of them the partner its token names, has the other as its
// source. Any other pin is not a replicate pin, and ok is false.
func ReplicaPinShape(pin *Rule) (source, partner string, ok bool) {
	if pin == nil || !pin.Strict || pin.Disabled {
		return "", "", false
	}
	nodes := pin.NodeNames()
	partner, widened := ReplicaPartner(pin.Comment)
	switch {
	case !widened && len(nodes) == 1:
		return nodes[0], "", true
	case widened && len(nodes) == 2 && slices.Contains(nodes, partner):
		for _, n := range nodes {
			if n != partner {
				return n, partner, true
			}
		}
	}
	return "", "", false
}

// MigrationEvidence is what the task lists of the two nodes show about how a
// VM came to run on its pin's partner.
type MigrationEvidence int

const (
	// MigrationUnread means a task list or a task log it needs could not be
	// read, so nothing is known.
	MigrationUnread MigrationEvidence = iota
	// MigrationNone means the lists were read and no migration explains the
	// move, which is what an HA recovery after a node failure leaves.
	MigrationNone
	// MigrationProved means a migration from the old source ended after the
	// pin's last change, the VM has not migrated away since, and no start
	// task shows that it started again after the migration.
	MigrationProved
	// MigrationRestarted means a migration from the old source explains the
	// move, but a start task shows that the VM started again after it. An HA
	// recovery starts the VM that way from its replica, so writes may have
	// been lost after the migration. A restart by hand or a reboot looks the
	// same, and the CPI can't tell them apart.
	MigrationRestarted
)

// MigrationLookback is how far before the pin's last change the task
// listings reach, so that a migration that started before the change and
// ended after it is listed.
const MigrationLookback = 24 * time.Hour

// PartnerTasks holds the task lists of a VM that runs on its pin's partner.
// FromOld and FromNode are the qmigrate tasks on the old source and on the
// node the VM runs on, and StartsOnOld and StartsOnNode are the qmstart tasks
// on the same two nodes. Logs holds what the log of each task read so far
// shows, by UPID.
type PartnerTasks struct {
	FromOld, FromNode         []pve.VMTask
	StartsOnOld, StartsOnNode []pve.VMTask
	Logs                      map[string]TaskLogKind
}

// TaskLogKind is what a task's log shows about the task.
type TaskLogKind int

const (
	// TaskLogUnread means the log was not read.
	TaskLogUnread TaskLogKind = iota
	// TaskLogIncomingStart is a qmstart task that received a live migration.
	TaskLogIncomingStart
	// TaskLogPlainStart is any other qmstart task, such as an HA recovery, a
	// reboot, a start by hand, or the start after an offline relocation.
	TaskLogPlainStart
	// TaskLogOnlineMigration is a qmigrate task that moved the VM while it
	// ran.
	TaskLogOnlineMigration
	// TaskLogOfflineMigration is a qmigrate task that ended in success and
	// moved the VM while it was stopped.
	TaskLogOfflineMigration
)

// The log lines that tell the tasks apart. PVE's vm_start prints
// incomingStartMark when it starts a VM to receive a live migration
// (qemu-server QemuServer.pm vm_start_nolock), and nothing else prints it. The
// source of a live migration logs onlineMigrationMark, with the VMID and the
// target node after it, when it starts the VM on the target
// (qemu-server QemuMigrate.pm phase2_start_local_cluster). An offline
// migration skips that phase.
const (
	incomingStartMark   = "migration listens on"
	onlineMigrationMark = "starting VM "
)

// taskEndMark matches the last line PVE writes to the log of a task that
// ended in success.
var taskEndMark = regexp.MustCompile(`^TASK (OK|WARNINGS: \d+)$`)

// ClassifyTaskLog reads the log of task, a qmstart or a qmigrate task of the
// VM, one string per line. A start is incoming when its log shows that it
// received a migration, and plain otherwise. A migration is online when its
// log shows that it started the VM on the target. It is offline only when its
// log ended in success without that line, because a log that has not ended
// may still print it, and such a log is an error.
func ClassifyTaskLog(task pve.VMTask, lines []string) (TaskLogKind, error) {
	switch task.Type {
	case "qmstart":
		for _, line := range lines {
			if strings.Contains(line, incomingStartMark) {
				return TaskLogIncomingStart, nil
			}
		}
		return TaskLogPlainStart, nil
	case "qmigrate":
		online := onlineMigrationMark + task.ID + " on remote node "
		for _, line := range lines {
			if strings.Contains(line, online) {
				return TaskLogOnlineMigration, nil
			}
		}
		if len(lines) > 0 && taskEndMark.MatchString(strings.TrimSpace(lines[len(lines)-1])) {
			return TaskLogOfflineMigration, nil
		}
		return TaskLogUnread, fmt.Errorf("the log of migration task %s has not ended in success", task.UPID)
	}
	return TaskLogUnread, fmt.Errorf("task %s is a %s task, not a start or a migration", task.UPID, task.Type)
}

// taskLogOf returns what the log of a task shows, reading it when needed.
type taskLogOf func(task pve.VMTask) (TaskLogKind, error)

// ProvingMigration returns the qmigrate task that explains how a VM came to
// run on the partner its pin names, from fromOld, the old source's list, and
// onNode, the list of the node the VM runs on now. A planned migration,
// whether by qm migrate or by an HA migrate or relocate, leaves a qmigrate
// task on the node it leaves, and an HA recovery leaves none. The task is the
// latest one on the old source that succeeded and ended clearly after
// changedAt, by more than the clock-skew tolerance. ok is false when there is
// no such task, or when a task on the node the VM runs on succeeded after it,
// which would mean the VM left that node again and came back some other way.
// The two tasks' end times come from two node clocks, so a migration away
// counts when it ended later than the arrival less the clock-skew tolerance.
// That errs toward no proof, so two migrations closer together than the
// tolerance read as a move that no migration explains.
func ProvingMigration(fromOld, onNode []pve.VMTask, changedAt time.Time) (pve.VMTask, bool) {
	if changedAt.IsZero() {
		return pve.VMTask{}, false
	}
	earliest := changedAt.Add(aj.ReplicaClockSkewTolerance)
	var arrived pve.VMTask
	for _, task := range fromOld {
		if task.Succeeded() && task.End().After(earliest) && task.End().After(arrived.End()) {
			arrived = task
		}
	}
	if arrived.End().IsZero() {
		return pve.VMTask{}, false
	}
	departed := arrived.End().Add(-aj.ReplicaClockSkewTolerance)
	for _, task := range onNode {
		if task.Succeeded() && task.End().After(departed) {
			return pve.VMTask{}, false
		}
	}
	return arrived, true
}

// StartedAfter reports whether a start task shows that the VM started again
// after migration, the qmigrate task on the old source that brought it to the
// node it runs on, reading the logs of the tasks it needs through logOf.
// Every start counts, whether it is running, failed, or succeeded, because an
// HA recovery may take more than one attempt.
//
// The old source's tasks share migration's clock, so any start there at or
// after the migration ended means the VM came back to the old source, by an
// HA recovery or otherwise, before it reached the node it runs on now.
//
// The node the VM runs on has its own clock, so a start there that began
// clearly before the migration did, by more than the clock-skew tolerance,
// belongs to an earlier stay on that node. Any later start counts unless its
// log shows that it received a live migration. The target of a live
// migration lists that start of its own, the qm start --migratedfrom that
// received the VM, and a failed earlier attempt lists one too, so no number
// of them counts. The one other start allowed is the first plain start inside
// the window of an offline migration, widened by the tolerance on each side,
// because an HA relocation stops the VM, migrates it offline, and starts it on
// the target. A log that can't be read is an error, because the start it
// belongs to could be either.
func StartedAfter(migration pve.VMTask, onOld, onNode []pve.VMTask, logOf taskLogOf) (bool, error) {
	for _, task := range onOld {
		if task.StartTime >= migration.EndTime {
			return true, nil
		}
	}
	tolerance := int64(aj.ReplicaClockSkewTolerance / time.Second)
	earliest, latest := migration.StartTime-tolerance, migration.EndTime+tolerance
	starts := slices.Clone(onNode)
	slices.SortStableFunc(starts, func(a, b pve.VMTask) int { return cmp.Compare(a.StartTime, b.StartTime) })
	relocated := false
	for _, task := range starts {
		if task.StartTime <= earliest {
			continue
		}
		kind, err := logOf(task)
		switch {
		case err != nil:
			return false, err
		case kind == TaskLogIncomingStart:
			continue
		case kind != TaskLogPlainStart:
			return false, fmt.Errorf("the log of start task %s reads as no start", task.UPID)
		case relocated || task.StartTime > latest:
			return true, nil
		}
		moved, err := logOf(migration)
		if err != nil {
			return false, err
		}
		if moved != TaskLogOfflineMigration {
			return true, nil
		}
		relocated = true
	}
	return false, nil
}

// MigrationEvidenceOf judges the task lists of a VM that runs on the partner
// its pin names (see ProvingMigration and StartedAfter), with the logs that
// tasks.Logs holds. Without a proving migration it is MigrationNone, and the
// start tasks don't matter. A log that StartedAfter needs and tasks.Logs
// lacks makes it MigrationUnread.
func MigrationEvidenceOf(tasks PartnerTasks, changedAt time.Time) MigrationEvidence {
	evidence, _ := migrationEvidence(tasks, changedAt, func(task pve.VMTask) (TaskLogKind, error) {
		if kind, ok := tasks.Logs[task.UPID]; ok && kind != TaskLogUnread {
			return kind, nil
		}
		return TaskLogUnread, fmt.Errorf("the log of task %s was not read", task.UPID)
	})
	return evidence
}

func migrationEvidence(tasks PartnerTasks, changedAt time.Time, logOf taskLogOf) (MigrationEvidence, error) {
	migration, ok := ProvingMigration(tasks.FromOld, tasks.FromNode, changedAt)
	if !ok {
		return MigrationNone, nil
	}
	restarted, err := StartedAfter(migration, tasks.StartsOnOld, tasks.StartsOnNode, logOf)
	switch {
	case err != nil:
		return MigrationUnread, err
	case restarted:
		return MigrationRestarted, nil
	}
	return MigrationProved, nil
}

// ReadMigrationEvidence reads the task lists of VM vmid, which runs on node,
// the partner its pin names, with oldSource the node the pin names as its
// source, and judges them (see MigrationEvidenceOf). It reads the qmigrate
// tasks first and the qmstart tasks only when a migration explains the move,
// because without one the starts change nothing. It reads a task's log only
// when StartedAfter needs it, and each log at most once. A listing or a log
// read that fails is MigrationUnread with its cause, which names the node and
// the read.
func ReadMigrationEvidence(
	ctx context.Context, reader pve.MigrationTaskReader, vmid int, node, oldSource string, changedAt time.Time,
) (MigrationEvidence, error) {
	since := changedAt.Add(-MigrationLookback)
	var tasks PartnerTasks
	var err error
	if tasks.FromOld, err = reader.MigrationTasks(ctx, oldSource, vmid, since); err != nil {
		return MigrationUnread, fmt.Errorf("list the migration tasks on node %s: %w", oldSource, err)
	}
	if tasks.FromNode, err = reader.MigrationTasks(ctx, node, vmid, since); err != nil {
		return MigrationUnread, fmt.Errorf("list the migration tasks on node %s: %w", node, err)
	}
	if _, ok := ProvingMigration(tasks.FromOld, tasks.FromNode, changedAt); !ok {
		return MigrationNone, nil
	}
	if tasks.StartsOnOld, err = reader.StartTasks(ctx, oldSource, vmid, since); err != nil {
		return MigrationUnread, fmt.Errorf("list the start tasks on node %s: %w", oldSource, err)
	}
	if tasks.StartsOnNode, err = reader.StartTasks(ctx, node, vmid, since); err != nil {
		return MigrationUnread, fmt.Errorf("list the start tasks on node %s: %w", node, err)
	}
	// StartedAfter reads the logs of the starts on node and of the migration
	// that left the old source, and nothing else.
	nodeOf := map[string]string{}
	for _, task := range tasks.FromOld {
		nodeOf[task.UPID] = oldSource
	}
	for _, task := range tasks.StartsOnNode {
		nodeOf[task.UPID] = node
	}
	tasks.Logs = map[string]TaskLogKind{}
	return migrationEvidence(tasks, changedAt, func(task pve.VMTask) (TaskLogKind, error) {
		if kind, ok := tasks.Logs[task.UPID]; ok {
			return kind, nil
		}
		at, ok := nodeOf[task.UPID]
		if !ok || task.UPID == "" {
			return TaskLogUnread, fmt.Errorf("task %q is not on a listed node", task.UPID)
		}
		lines, err := reader.TaskLog(ctx, at, task.UPID)
		if err != nil {
			return TaskLogUnread, fmt.Errorf("read the log of task %s on node %s: %w", task.UPID, at, err)
		}
		kind, err := ClassifyTaskLog(task, lines)
		if err != nil {
			return TaskLogUnread, fmt.Errorf("read the log of task %s on node %s: %w", task.UPID, at, err)
		}
		tasks.Logs[task.UPID] = kind
		return kind, nil
	})
}

// PartnerMove holds what is known about a replicate VM that runs on the
// partner its pin names.
type PartnerMove struct {
	// Node is the node the VM runs on, the pin's partner.
	Node string
	// OldSource is the node the pin names as its source.
	OldSource string
	// Job is this Director's replication job of the VM, nil when it has none.
	Job *ReplicaJobState
	// OldSourceOnline reports whether the old source is an online member.
	OldSourceOnline bool
	// ChangedAt is the pin's last change, zero when the pin records none.
	ChangedAt time.Time
	// Synced reports whether a sync from Node to OldSource that started after
	// ChangedAt is verified. The CPI verifies it against the job's status and
	// the old source's volumes, and the doctor against the status alone (see
	// SyncAfterChange).
	Synced bool
	// Migration is what the task lists show. It counts only when Synced.
	Migration MigrationEvidence
}

// PartnerMoveVerdict is how a VM on its pin's partner is reconciled.
type PartnerMoveVerdict int

const (
	// PartnerMoveFailover means nothing kept the old source's copy current
	// from the VM's node: the job is gone, disabled, marked for removal, or
	// not reversed, or the old source is offline. The pin narrows to the
	// VM's node, and writes made on the old source after its last sync are
	// lost when the next sync runs.
	PartnerMoveFailover PartnerMoveVerdict = iota
	// PartnerMoveUndecided means the job runs back to an online old source
	// but no sync from the VM's node is verified yet, so a planned migration
	// cannot be told from a failover. The pin narrows, and the next verified
	// sync widens it back.
	PartnerMoveUndecided
	// PartnerMoveRecovered means a reversed sync is verified, so the old
	// source's copy matches the VM again and the pin keeps both nodes, but no
	// migration task explains the move. That is a failover whose first
	// reversed sync has run, and writes made on the old source after its last
	// sync before the failover were lost.
	PartnerMoveRecovered
	// PartnerMoveUnproven means a reversed sync is verified and the pin keeps
	// both nodes, but a task list could not be read, so the move may have
	// been a failover that lost writes.
	PartnerMoveUnproven
	// PartnerMoveRestarted means a reversed sync is verified and a migration
	// task explains the move, so the pin keeps both nodes, but the VM started
	// again after the migration. If that start was an HA recovery, writes made
	// after the last sync before it were lost.
	PartnerMoveRestarted
	// PartnerMovePlanned means a reversed sync is verified and a migration
	// task explains the move, which loses no writes. The pin keeps both nodes.
	PartnerMovePlanned
)

// KeepsBothNodes reports whether the verdict keeps both nodes in the pin.
func (v PartnerMoveVerdict) KeepsBothNodes() bool {
	switch v {
	case PartnerMoveRecovered, PartnerMoveUnproven, PartnerMoveRestarted, PartnerMovePlanned:
		return true
	}
	return false
}

// JudgePartnerMove decides how a VM that runs on its pin's partner got there,
// and so how its pin is reconciled. It is the one rule the CPI and the doctor
// both apply. A verdict that keeps both nodes needs a verified reversed sync,
// and only a migration task with no start of the VM after it makes it a
// planned migration.
func JudgePartnerMove(m PartnerMove) PartnerMoveVerdict {
	switch {
	case m.Job == nil, m.Job.Disabled, m.Job.RemoveJob != "":
		return PartnerMoveFailover
	case m.Node == "" || m.OldSource == "" || m.Job.Source != m.Node || m.Job.Target != m.OldSource:
		// A planned migration swaps the job's direction as it ends. A job
		// that PVE has not swapped is a failover that PVE's runner has not
		// caught up with yet.
		return PartnerMoveFailover
	case !m.OldSourceOnline:
		return PartnerMoveFailover
	case m.ChangedAt.IsZero() || !m.Synced:
		return PartnerMoveUndecided
	}
	switch m.Migration {
	case MigrationProved:
		return PartnerMovePlanned
	case MigrationNone:
		return PartnerMoveRecovered
	case MigrationRestarted:
		return PartnerMoveRestarted
	default:
		return PartnerMoveUnproven
	}
}

// SyncAfterChange reports whether a job's status on its source shows a
// successful sync that started after changedAt by more than the clock-skew
// tolerance, with no error and no failures since, and otherwise why not. It
// is the status half of the sync gate (see CheckReplicaSync).
func SyncAfterChange(st ReplicaSyncStatus, changedAt time.Time) (bool, string) {
	earliest := ReplicaSyncEarliest(changedAt)
	switch {
	case changedAt.IsZero():
		return false, "the time the VM's volume set changed is not recorded"
	case st.Error != "":
		return false, "the job reports an error: " + st.Error
	case st.FailCount != 0:
		return false, failedSince(st.FailCount)
	case st.LastSync.IsZero():
		return false, "the job has not synced yet"
	case !st.LastSync.After(earliest):
		return false, "the last sync started at " + st.LastSync.Format(time.RFC3339) +
			", which is not clearly after the volume set changed at " + changedAt.UTC().Format(time.RFC3339)
	}
	return true, ""
}
