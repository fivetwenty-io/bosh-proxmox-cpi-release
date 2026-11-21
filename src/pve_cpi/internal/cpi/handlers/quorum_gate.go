package handlers

import (
	"context"
	"errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A cluster that has lost quorum refuses every write to /etc/pve, so a call
// that must write can only fail, and the reads it makes first can take minutes
// against members that no longer answer. These helpers read the cluster's
// status once, with a single attempt, and let a call stop before that work.
// A status read that fails proves nothing, so the call carries on as it did
// before, and a standalone host never counts as having lost quorum.

// clusterLostQuorum reads the cluster's status once and, when it proves that
// the cluster has lost quorum, returns the reason the call gives (see
// quorumLossReason). It returns "" when the client has no cluster service,
// the read fails, or the cluster is quorate.
func clusterLostQuorum(ctx context.Context, deps Deps, logger *log.Logger, caller string) string {
	q, ok := readClusterQuorum(ctx, deps, logger, caller)
	if !ok || !q.Lost() {
		return ""
	}
	return quorumLossReason(q)
}

// readClusterQuorum reads the cluster's status once, with a single attempt,
// and returns what it says about quorum and about the members it reports
// offline. ok is false when the client has no cluster service or the read
// fails, and the caller then carries on as if it knew nothing.
func readClusterQuorum(ctx context.Context, deps Deps, logger *log.Logger, caller string) (pve.ClusterQuorum, bool) {
	if deps.PVE == nil {
		return pve.ClusterQuorum{}, false
	}
	clusterAPI := deps.PVE.Cluster()
	if clusterAPI == nil {
		return pve.ClusterQuorum{}, false
	}
	q, err := pve.ReadClusterQuorum(ctx, clusterAPI)
	if err != nil {
		if logger != nil {
			logger.Debug(caller+": cluster status unreadable before the quorum check, carrying on", log.Err(err))
		}
		return pve.ClusterQuorum{}, false
	}
	return q, true
}

// quorumLossReason names the entry node that answered when the status marks
// one local. With several endpoints, a minority node that still answers sees
// the cluster without quorum while 'pvecm status' on a majority node reports
// it quorate, so the operator needs to know which node was asked.
func quorumLossReason(q pve.ClusterQuorum) string {
	if q.EntryNode == "" {
		return pve.ReasonNotQuorate
	}
	return "the entry node " + q.EntryNode + " does not report the cluster quorate"
}

// deleteQuorumDeferral is the HA deferral a delete of vmid returns when the
// cluster has lost quorum, for the reason given. Its text is the one an
// unknown HA state gives, because the HA state of every VM is unknown while
// the entry node does not report the cluster quorate.
func deleteQuorumDeferral(caller string, vmid int, reason string) error {
	return deferHA(haUnknownDeferral(caller, vmid, reason))
}

// deferDeleteWithoutQuorum returns the quorum deferral when the cluster's
// status proves it has lost quorum, and nil otherwise. The deferral is
// recorded as an HA recovery deferral, because the members on the other side
// of the split may hold quorum and be recovering the VM, and a delete that
// reaches it once quorum returns must let it settle (see
// awaitHARecoverySettle).
func deferDeleteWithoutQuorum(ctx context.Context, deps Deps, caller string, vmid int, logger *log.Logger) error {
	reason := clusterLostQuorum(ctx, deps, logger, caller)
	if reason == "" {
		return nil
	}
	if logger != nil {
		logger.Warn(caller+": deferred because "+reason, log.Int(metadataKeyVMID, vmid))
	}
	recordHARecoveryDeferralOf(deps, caller, pve.HAServiceSID(vmid), logger)
	return deleteQuorumDeferral(caller, vmid, reason)
}

// deleteErrorWithoutQuorum turns a delete error that carries a lock create PVE
// refused for lack of quorum into the quorum deferral, and records it as the
// quorum gate does. Quorum can be lost after the delete's own check, and the
// lock is then the first write to meet it. Every other error comes back
// unchanged.
func deleteErrorWithoutQuorum(deps Deps, caller string, vmid int, err error, logger *log.Logger) error {
	if err == nil || !errors.Is(err, pve.ErrClusterLockNoQuorum) {
		return err
	}
	if logger != nil {
		logger.Warn(caller+": deferred because PVE refused a lock for lack of quorum",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
	}
	recordHARecoveryDeferralOf(deps, caller, pve.HAServiceSID(vmid), logger)
	// No status was read here, so the reason cannot name the node that
	// refused the lock.
	return deleteQuorumDeferral(caller, vmid, pve.ReasonNotQuorate)
}

// createQuorumError is the retriable error create_vm returns when the cluster
// has lost quorum, for the reason given. It names quorum whether or not the
// call pins a node, because no node can receive a VM until quorum returns.
func createQuorumError(reason string) error {
	return cpierrors.Retriable("create_vm: the cluster has lost quorum (%s), so no node can receive a VM; "+
		"retry once 'pvecm status' reports the cluster quorate", reason)
}

// createOfflineTargetError is the retriable error create_vm returns when the
// status it already read reports the pinned target_node offline. Placement
// words the same condition for a disk's home node, and PVE would otherwise
// fail the VMID allocation on that node only after its retry budget.
func createOfflineTargetError(node string) error {
	return cpierrors.Retriable("create_vm: target_node %q is offline; bring the node online or remove "+
		"target_node so placement can pick another node", node)
}

// createMaintenanceTargetError is the retriable error create_vm returns when
// the pinned target_node is in maintenance, for the reason node health gives.
// A pinned node never gives way to another one, so the call waits for the node.
func createMaintenanceTargetError(node, reason string) error {
	return cpierrors.Retriable("create_vm: target_node %q is in maintenance (%s), so it cannot take a new VM; "+
		"retry once the node leaves maintenance, or remove target_node so placement can pick another node",
		node, reason)
}

// createUnplaceableTargetError is the retriable error create_vm returns when
// the pinned target_node is not in maintenance but node health still says it
// cannot take new work, for example because it is fenced.
func createUnplaceableTargetError(node, reason string) error {
	return cpierrors.Retriable("create_vm: target_node %q cannot take a new VM (%s); "+
		"retry once the node can take new work, or remove target_node so placement can pick another node",
		node, reason)
}

// createMaintenanceUnknownError is the retriable error create_vm returns when
// the health of the pinned target_node cannot be read, so the call cannot tell
// whether the node is in maintenance.
func createMaintenanceUnknownError(node string, cause error) error {
	return cpierrors.Retriable("create_vm: cannot tell whether target_node %q is in maintenance, because "+
		"the node health read failed (%v); retry once the cluster answers", node, cause)
}

// refuseMaintenanceTarget fails a create whose pinned target_node is in
// maintenance. It reads the node health the way placement does, so the HA
// state and the maintenance marker in the node's notes both count, in every HA
// mode, and a node that health calls not placeable is refused for the reason
// health gives. A pinned node has no alternative, so a failed health read, or
// a failed read of the pinned node's notes, refuses the create with a
// retriable error. A node the report does not mention, or a client without a
// cluster service, passes.
func refuseMaintenanceTarget(ctx context.Context, deps Deps, target string, logger *log.Logger) error {
	if deps.PVE == nil || deps.PVE.Cluster() == nil || deps.Config == nil {
		return nil
	}
	report, err := pve.NodeHealth(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), pve.NodeHealthOptions{
		MaintenanceTags: deps.Config.MaintenanceNodeTagsValue(),
	})
	if err != nil {
		logger.Warn("create_vm: node health unavailable, so target_node "+target+" cannot be checked for maintenance",
			log.Err(err))
		return createMaintenanceUnknownError(target, err)
	}
	entry, ok := report.Node(target)
	if !ok {
		return nil
	}
	if entry.NotesUnread {
		logger.Warn("create_vm: the notes of target_node "+target+" could not be read, so it cannot be checked "+
			"for maintenance", log.Err(report.NotesErr))
		return createMaintenanceUnknownError(target, report.NotesErr)
	}
	refusal := targetNodeRefusal(target, entry)
	switch {
	case refusal == nil:
	case entry.Maintenance:
		logger.Warn("create_vm: target_node " + target + " is in maintenance, so no VM can be created on it")
	default:
		logger.Warn("create_vm: target_node " + target + " cannot take new work, so no VM can be created on it")
	}
	return refusal
}

// targetNodeRefusal returns the error that refuses a create on the pinned
// target node whose health entry is entry, or nil when the node is
// placeable. A node in maintenance gets the maintenance error, and any other
// node that cannot take work gets the unplaceable one. Each names the reason
// health gives, or a default one when health gives none.
func targetNodeRefusal(target string, entry pve.NodeHealthEntry) error {
	if entry.Placeable {
		return nil
	}
	reason := entry.Reason
	if reason == "" {
		reason = "the node health check does not call it placeable"
	}
	if entry.Maintenance {
		return createMaintenanceTargetError(target, reason)
	}
	return createUnplaceableTargetError(target, reason)
}
