package ha

import (
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
	// pin's last change, and the VM has not migrated away since.
	MigrationProved
)

// MigrationEvidenceOf judges the qmigrate tasks of a VM that runs on the
// partner its pin names. fromOld is the old source's list and onNode the list
// of the node the VM runs on now. A planned migration, whether by qm migrate
// or by an HA migrate or relocate, leaves a qmigrate task on the node it
// leaves, and an HA recovery leaves none. The move is proved when a task on
// the old source succeeded and ended clearly after changedAt, by more than the
// clock-skew tolerance, and no task on the node the VM runs on succeeded after
// it, which would mean the VM left that node again and came back some other
// way. Anything else is MigrationNone.
func MigrationEvidenceOf(fromOld, onNode []pve.VMTask, changedAt time.Time) MigrationEvidence {
	if changedAt.IsZero() {
		return MigrationNone
	}
	earliest := changedAt.Add(aj.ReplicaClockSkewTolerance)
	var arrived time.Time
	for _, task := range fromOld {
		if task.Succeeded() && task.End().After(earliest) && task.End().After(arrived) {
			arrived = task.End()
		}
	}
	if arrived.IsZero() {
		return MigrationNone
	}
	for _, task := range onNode {
		if task.Succeeded() && task.End().After(arrived) {
			return MigrationNone
		}
	}
	return MigrationProved
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
	// PartnerMovePlanned means a reversed sync is verified and a migration
	// task explains the move, which loses no writes. The pin keeps both nodes.
	PartnerMovePlanned
)

// KeepsBothNodes reports whether the verdict keeps both nodes in the pin.
func (v PartnerMoveVerdict) KeepsBothNodes() bool {
	return v == PartnerMoveRecovered || v == PartnerMoveUnproven || v == PartnerMovePlanned
}

// JudgePartnerMove decides how a VM that runs on its pin's partner got there,
// and so how its pin is reconciled. It is the one rule the CPI and the doctor
// both apply. A verdict that keeps both nodes needs a verified reversed sync,
// and only a migration task makes it a planned migration.
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
