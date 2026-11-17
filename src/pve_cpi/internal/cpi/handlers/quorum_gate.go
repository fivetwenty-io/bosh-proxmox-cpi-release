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

// clusterLostQuorum reads the cluster's status once and reports whether it
// proves that the cluster has lost quorum. It returns false when the client
// has no cluster service or the read fails.
func clusterLostQuorum(ctx context.Context, deps Deps, logger *log.Logger, caller string) bool {
	if deps.PVE == nil {
		return false
	}
	clusterAPI := deps.PVE.Cluster()
	if clusterAPI == nil {
		return false
	}
	q, err := pve.ReadClusterQuorum(ctx, clusterAPI)
	if err != nil {
		if logger != nil {
			logger.Debug(caller+": cluster status unreadable before the quorum check, carrying on", log.Err(err))
		}
		return false
	}
	return q.Lost()
}

// deleteQuorumDeferral is the HA deferral a delete of vmid returns when the
// cluster has lost quorum. Its text is the one an unknown HA state gives,
// because the HA state of every VM is unknown while the entry node does not
// report the cluster quorate.
func deleteQuorumDeferral(caller string, vmid int) error {
	return deferHA(haUnknownDeferral(caller, vmid, pve.ReasonNotQuorate))
}

// deferDeleteWithoutQuorum returns the quorum deferral when the cluster's
// status proves it has lost quorum, and nil otherwise. The deferral is
// recorded as an HA recovery deferral, because the members on the other side
// of the split may hold quorum and be recovering the VM, and a delete that
// reaches it once quorum returns must let it settle (see
// awaitHARecoverySettle).
func deferDeleteWithoutQuorum(ctx context.Context, deps Deps, caller string, vmid int, logger *log.Logger) error {
	if !clusterLostQuorum(ctx, deps, logger, caller) {
		return nil
	}
	if logger != nil {
		logger.Warn(caller+": deferred because the entry node does not report the cluster quorate",
			log.Int(metadataKeyVMID, vmid))
	}
	recordHARecoveryDeferralOf(deps, caller, pve.HAServiceSID(vmid), logger)
	return deleteQuorumDeferral(caller, vmid)
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
	return deleteQuorumDeferral(caller, vmid)
}

// createQuorumError is the retriable error create_vm returns when the cluster
// has lost quorum. It names quorum whether or not the call pins a node,
// because no node can receive a VM until quorum returns.
func createQuorumError() error {
	return cpierrors.Retriable("create_vm: the cluster has lost quorum (%s), so no node can receive a VM; "+
		"retry once 'pvecm status' reports the cluster quorate", pve.ReasonNotQuorate)
}
