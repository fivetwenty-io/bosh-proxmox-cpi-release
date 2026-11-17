package ha

import (
	"context"
	"fmt"
	"slices"
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
	// MigrationUnread means a task list could not be read, so nothing is
	// known.
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
// on the same two nodes.
type PartnerTasks struct {
	FromOld, FromNode         []pve.VMTask
	StartsOnOld, StartsOnNode []pve.VMTask
}

// ProvingMigration returns the qmigrate task that explains how a VM came to
// run on the partner its pin names, from fromOld, the old source's list, and
// onNode, the list of the node the VM runs on now. A planned migration,
// whether by qm migrate or by an HA migrate or relocate, leaves a qmigrate
// task on the node it leaves, and an HA recovery leaves none. The task is the
// latest one on the old source that succeeded and ended clearly after
// changedAt, by more than the clock-skew tolerance. ok is false when there is
// no such task, or when a task on the node the VM runs on succeeded after it,
// which would mean the VM left that node again and came back some other way.
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
	for _, task := range onNode {
		if task.Succeeded() && task.End().After(arrived.End()) {
			return pve.VMTask{}, false
		}
	}
	return arrived, true
}

// StartedAfter reports whether a start task shows that the VM started again
// after migration, the qmigrate task on the old source that brought it to the
// node it runs on. Every start counts, whether it is running, failed, or
// succeeded, because an HA recovery may take more than one attempt.
//
// The old source's tasks share migration's clock, so any start there at or
// after the migration ended means the VM came back to the old source, by an
// HA recovery or otherwise, before it reached the node it runs on now.
//
// The node the VM runs on has its own clock, so the window around the
// migration widens by the clock-skew tolerance on each side. A start there
// that began clearly before the migration did belongs to an earlier stay on
// that node. The target of a live migration may list one start of its own
// inside the window, the qm start --migratedfrom that received the VM, so one
// start inside the window is allowed. A second start inside it, or any start
// clearly after the migration ended, means the VM started again on that node.
// A start whose clock-skew window overlaps the migration's end can't be told
// from the incoming one, and it counts as that one.
func StartedAfter(migration pve.VMTask, onOld, onNode []pve.VMTask) bool {
	for _, task := range onOld {
		if task.StartTime >= migration.EndTime {
			return true
		}
	}
	tolerance := int64(aj.ReplicaClockSkewTolerance / time.Second)
	earliest, latest := migration.StartTime-tolerance, migration.EndTime+tolerance
	incoming := 0
	for _, task := range onNode {
		switch {
		case task.StartTime <= earliest:
			continue
		case task.StartTime > latest:
			return true
		}
		incoming++
	}
	return incoming > 1
}

// MigrationEvidenceOf judges the task lists of a VM that runs on the partner
// its pin names (see ProvingMigration and StartedAfter). Without a proving
// migration it is MigrationNone, and the start tasks don't matter.
func MigrationEvidenceOf(tasks PartnerTasks, changedAt time.Time) MigrationEvidence {
	migration, ok := ProvingMigration(tasks.FromOld, tasks.FromNode, changedAt)
	switch {
	case !ok:
		return MigrationNone
	case StartedAfter(migration, tasks.StartsOnOld, tasks.StartsOnNode):
		return MigrationRestarted
	}
	return MigrationProved
}

// ReadMigrationEvidence reads the task lists of VM vmid, which runs on node,
// the partner its pin names, with oldSource the node the pin names as its
// source, and judges them (see MigrationEvidenceOf). It reads the qmigrate
// tasks first and the qmstart tasks only when a migration explains the move,
// because without one the starts change nothing. A listing that fails is
// MigrationUnread with its cause, which names the node and the listing.
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
	return MigrationEvidenceOf(tasks, changedAt), nil
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
