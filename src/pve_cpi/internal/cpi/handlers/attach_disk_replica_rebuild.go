package handlers

import (
	"context"
	"fmt"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// replicaRebuildOp names the replica rebuild of an attach in errors and logs.
const replicaRebuildOp = "attach_disk"

// A disk that a detach parked from a VM whose own name it carried is renamed
// for its parker, because PVE frees an owned volume that leaves its VM any
// other way, so the disk can only enter a VM again by reassignment, which
// renames it for that VM. PVE refuses that reassignment into a VM with a
// replication job when the disk's storage cannot replicate, and a config
// edit of a volume named for the parker would leave the parker owning it,
// whose removal frees it. create_vm attaches the persistent disks it is given
// before the replicate policy sets up the job, so a recreate, a stemcell
// update, and a resurrection never meet the refusal. An attach_disk of such a
// disk into a VM that already runs with its job does, as in a reattach after a
// detach, bosh attach-disk, and cck's reattach. For that attach the CPI
// removes the VM's job, lets the reassignment run, and sets the job up again
// to the same partner, which is the replica rebuild. The rebuild narrows the
// pin to the source first, so PVE HA never fails the VM over to a partner
// whose copy the removal deletes, and the next verified sync widens the pin
// again. The partner rides the pin as a token until the job is back, so a
// call that stops in between leaves the next attach, or the next call that
// touches the VM, to finish it (see finishReplicaRebuild).

// replicaRebuildKey marks a context whose attach removed the VM's
// replication job for its reassignment.
type replicaRebuildKey struct{}

// withReplicaRebuild marks ctx as the context of an attach whose replica
// rebuild removed the VM's job.
func withReplicaRebuild(ctx context.Context) context.Context {
	return context.WithValue(ctx, replicaRebuildKey{}, true)
}

// replicaRebuildInFlight reports whether ctx belongs to an attach whose
// replica rebuild removed the VM's job, so a refusal made from facts read
// before that removal no longer holds.
func replicaRebuildInFlight(ctx context.Context) bool {
	marked, _ := ctx.Value(replicaRebuildKey{}).(bool)
	return marked
}

// attachReplicaRebuild is a replica rebuild an attach started. The VM's job
// to partner is gone, and the attach sets it up again once the disk is in.
type attachReplicaRebuild struct {
	vmCID   string
	vmid    int
	partner string
}

// prepareAttachReplicaRebuild starts the replica rebuild of an attach of the
// stable-ID disk rd into vmCID when the attach needs one, and returns it, or
// nil when it does not. It runs before the disk's lifecycle, because it holds
// the VM's journal record for the removal, and the VM's record is never
// acquired while a disk's is held. The attach needs a rebuild when the disk
// is on shared storage that cannot replicate, a parker or a mover holds it
// under the holder's own name, and the VM carries the replicate tag, runs on
// the source its pin names, and has a job of this Director and no other job.
// A rebuild a call left part way, whose job is gone and whose pin still names
// the partner, is returned as it is. Any other shape returns nil, and the
// attach's own checks answer it. A read or a write that fails is retriable.
func prepareAttachReplicaRebuild(
	ctx context.Context, deps Deps, vmCID string, rd resolvedDisk,
) (*attachReplicaRebuild, error) {
	vmid, ok := replicaRebuildCandidate(ctx, deps, vmCID, rd)
	if !ok {
		return nil, nil
	}
	const op = replicaRebuildOp
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate replicated VM %d before its disk enters by reassignment", op, vmid))
	}
	if !loc.Found || loc.Node == "" || !hasReplicaTag(loc.Tags) {
		return nil, nil
	}
	pin, source, _, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the pin of replicated VM %d", op, vmid))
	}
	if pin == nil || source != loc.Node {
		return nil, nil
	}
	api := replicationAPIFor(deps)
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs of VM %d", op, vmid))
	}
	owned, foreign, err := guestReplicaJobs(jobs, vmid, deps.RequestDirectorUUID)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": read the VM's replication jobs")
	}
	pending, rebuilding := ha.ReplicaRebuild(pin.Comment)
	switch {
	case len(foreign) > 0:
		return nil, nil
	case owned == nil && rebuilding:
		return &attachReplicaRebuild{vmCID: vmCID, vmid: vmid, partner: pending}, nil
	case owned == nil:
		return nil, nil
	}
	partner := replicaOtherNode(*owned, loc.Node)
	if partner == "" {
		return nil, nil
	}
	rb := &attachReplicaRebuild{vmCID: vmCID, vmid: vmid, partner: partner}
	if err := rb.retire(ctx, deps, loc.Node, pin); err != nil {
		return nil, err
	}
	return rb, nil
}

// replicaRebuildCandidate reports, from what the attach resolved, whether the
// attach of rd into vmCID may need a replica rebuild, and the VM's ID. It
// reads only the storage index.
func replicaRebuildCandidate(ctx context.Context, deps Deps, vmCID string, rd resolvedDisk) (int, bool) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff || deps.PVE == nil ||
		deps.RequestDirectorUUID == "" || rd.stableID == "" || rd.holder == nil || !rd.holder.Found ||
		!rd.holder.IsParker {
		return 0, false
	}
	parsed, err := vmcid.Parse(vmCID)
	if err != nil || parsed.VMID == rd.holder.VMID {
		return 0, false
	}
	embedded, named := pve.EmbeddedDiskVMID(rd.volid)
	if (!named || embedded != rd.holder.VMID) && !pve.TagsMarkDiskMover(rd.holder.Tags) {
		return 0, false
	}
	info, known := diskStorageInfo(ctx, deps, rd.volid)
	if !known || !info.IsShared() || storageCanReplicate(info) {
		return 0, false
	}
	return parsed.VMID, true
}

// retire narrows the VM's pin to node with the rebuild token, then removes the
// VM's job with cleanup, recorded in the VM's journal record first. The
// narrowing comes first, so PVE HA never moves the VM to the partner whose
// copy the removal deletes. The removal is never forced, so a partner that cannot
// clean up makes the attach retriable, and the pin keeps the token, so the
// retry, or a call that touches the VM, finds the rebuild.
func (rb *attachReplicaRebuild) retire(ctx context.Context, deps Deps, node string, pin *ha.Rule) error {
	const op = replicaRebuildOp
	logger := deps.Log(ctx).With(log.Int(metadataKeyVMID, rb.vmid), log.String("node", node),
		log.String("partner", rb.partner))
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: resolve the HA profile to rebuild the replica of VM %d", op, rb.vmid))
	}
	narrowed := narrowedReplicaPin(pin, node, time.Now())
	narrowed.Comment = ha.WithReplicaRebuild(narrowed.Comment, rb.partner)
	if err := writeReplicaPin(ctx, deps, profile, logger, rb.vmid, pin, narrowed); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: narrow the pin of VM %d to its source before its replication job is removed", op,
				rb.vmid))
	}
	logger.Warn(op + ": the VM's replication job is removed so that the disk can enter by reassignment, which PVE " +
		"refuses into a replicated VM; the job is set up again to the same partner after the attach, and the next " +
		"sync copies the VM in full")
	return withVMReplicaRecord(ctx, deps, rb.vmCID, func(handle *aj.Handle) error {
		r := &replicaRetire{
			deps: deps, logger: logger, api: replicationAPIFor(deps), caller: op, vmid: rb.vmid, node: node,
			handle: handle, online: replicaNodeOnline(profile, nil),
		}
		return r.run(ctx)
	})
}

// finish sets the VM's job up again once the attach is done, whether the
// attach landed or not, because the VM must not stay without its replica. It
// runs after the disk's lifecycle has ended, so it may hold the VM's journal
// record. An attach that landed and whose job could not be set up again
// returns a retriable error, and its retry finds the rebuild on the pin. An
// attach that failed keeps its own error, and the rebuild's failure is
// logged.
func (rb *attachReplicaRebuild) finish(ctx context.Context, deps Deps, attachErr error) error {
	logger := deps.Log(ctx)
	err := finishReplicaRebuild(ctx, deps, logger, replicaRebuildOp, rb.vmCID, rb.vmid, rb.partner)
	switch {
	case err == nil:
		return attachErr
	case attachErr != nil:
		logger.Warn(replicaRebuildOp+": the attach failed, and the VM's replication job could not be set up "+
			"again either; the next attach or call that touches the VM sets it up", log.Int(metadataKeyVMID, rb.vmid),
			log.String("partner", rb.partner), log.Err(err))
		return attachErr
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("%s: the disk is in VM %d, and its "+
		"replication job to %s could not be set up again; the retry sets it up", replicaRebuildOp, rb.vmid,
		rb.partner))
}

// finishReplicaRebuild sets up again the replication job to partner that a
// replica rebuild removed from VM vmid, through the replicate policy's own
// setup, and then removes the rebuild token from the VM's pin. A job of this
// Director that is in place already is kept. The setup needs the VM on the
// source its source-only pin names, as create_vm's does; in auto it leaves
// the VM pinned to its source without a replica for a refusal on proven
// facts, such as a node-local volume the job could not copy, and the token
// then goes too, because no later call could set the job up either. A read
// or write that fails returns its error, and the token stays.
func finishReplicaRebuild(
	ctx context.Context, deps Deps, logger *log.Logger, op, vmCID string, vmid int, partner string,
) error {
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return fmt.Errorf("locate VM %d: %w", vmid, pve.WrapError(err))
	}
	if !loc.Found || loc.Node == "" {
		return fmt.Errorf("VM %d is not in the cluster index", vmid)
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("resolve the HA profile: %w", err)
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	d := &ha.VMDecision{VMID: vmid, SID: haResourceSid(vmid), Policy: ha.PolicyReplicate, Partner: partner}
	err = withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		return setUpReplica(ctx, deps, logger, op, loc.Node, vmid, d, profile, handle)
	})
	if err != nil {
		return err
	}
	pin, _, _, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return fmt.Errorf("read the pin of VM %d: %w", vmid, err)
	}
	if pin == nil {
		return nil
	}
	if _, rebuilding := ha.ReplicaRebuild(pin.Comment); !rebuilding {
		return nil
	}
	cleared := *pin
	cleared.Comment = ha.WithReplicaRebuild(pin.Comment, "")
	if err := writeReplicaPin(ctx, deps, profile, logger, vmid, pin, &cleared); err != nil {
		return fmt.Errorf("remove the rebuild token from the pin of VM %d: %w", vmid, err)
	}
	logger.Info(op+": the VM's replication job is set up again after its disk entered by reassignment; the pin "+
		"names the source alone until a sync is verified", log.String("partner", partner))
	return nil
}

// finishPendingReplicaRebuild finishes, on a call that touches a replicate VM
// on its source, a replica rebuild an earlier attach left part way, which its
// pin's token shows. It reports whether the pin carried the token. A failure
// is logged, and a later call that touches the VM tries again.
func finishPendingReplicaRebuild(
	ctx context.Context, deps Deps, logger *log.Logger, op, vmCID string, vmid int, pin *ha.Rule,
) bool {
	partner, rebuilding := ha.ReplicaRebuild(pin.Comment)
	if !rebuilding {
		return false
	}
	logger.Warn(op+": an earlier attach removed the VM's replication job for a reassignment and did not set it "+
		"up again; setting it up now", log.String("partner", partner))
	if err := finishReplicaRebuild(ctx, deps, logger, op, vmCID, vmid, partner); err != nil {
		logger.Warn(op+": the VM's replication job could not be set up again; a later call that touches the VM "+
			"tries again", log.String("partner", partner), log.Err(err))
	}
	return true
}

// finishAttachReplica runs the replicate policy's steps an attach owes after
// the disk's lifecycle has ended, when the VM's journal record may be held.
// The record learns of a job the VM's removal from HA retired, and a replica
// rebuild sets the VM's job up again. It returns the attach's answer.
func finishAttachReplica(
	ctx context.Context, deps Deps, vmCID string, rebuild *attachReplicaRebuild, h *attachHA, attachErr error,
) error {
	if h != nil && h.leftReplica != nil {
		recordLeftReplica(ctx, deps, deps.Log(ctx), vmCID, h.vmid, h.leftReplica)
	}
	if rebuild == nil {
		return attachErr
	}
	return rebuild.finish(ctx, deps, attachErr)
}
