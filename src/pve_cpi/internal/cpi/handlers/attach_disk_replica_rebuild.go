package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// to the same partner, which is the replica rebuild.
//
// The rebuild is one step that a call may stop in and a later call resumes.
// Before it removes anything, the attach makes its HA decision and runs its
// read-only refusals, so an attach that would be refused anyway keeps the job
// and the partner's copy. The rebuild then narrows the pin to the source with
// a token that names the partner, so PVE HA never fails the VM over to a
// partner whose copy the removal deletes, and the next verified sync widens
// the pin again. The token stays on the pin until the job is back, so any
// later attach_disk to the VM, whatever disk it attaches, finishes the
// rebuild, and so does the next reboot_vm or set_vm_metadata (see
// finishReplicaRebuild). The pin and the job are written outside the disk's
// lifecycle, because the VM's journal record, which the removal and the setup
// hold, is never acquired while a disk's record is held. The lifecycle guard
// would admit the pin write, since it judges an HA rule write by its owner
// (see admitLifecycleHAWrite), but the job's removal and setup could not run
// there.

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

// attachReplicaRebuild is a replica rebuild an attach started or resumes. The
// VM's job to partner is gone or on its way out, or the job stayed and only
// the token is left, and the attach sets the job up again, or keeps it, once
// the disk is in.
type attachReplicaRebuild struct {
	vmCID   string
	vmid    int
	partner string
}

// prepareAttachReplicaRebuild starts the replica rebuild of an attach of the
// disk rd into vmCID when the attach needs one, resumes one that an earlier
// call left part way, and returns it, or nil when the attach has none. It
// runs before the disk's lifecycle with deps that the lifecycle has not
// wrapped. A rebuild left part way is resumed whatever the disk is, because
// its token on the VM's pin is what records it. A new rebuild needs a
// stable-ID disk on shared storage that cannot replicate, which a parker or a
// mover holds under the holder's own name, a VM with the replicate tag that
// is under this Director's HA, and a job of this Director and no other job.
// A VM that has failed over to its partner has its pin reconciled first. A
// replicated VM that left HA under an earlier release, and kept its job and
// tag, has the job retired and the tag removed, and needs no rebuild. Every
// other shape returns nil, and the attach's own checks answer it. A read or a
// write that fails is retriable.
func prepareAttachReplicaRebuild(
	ctx context.Context, deps Deps, vmCID string, rd resolvedDisk,
) (*attachReplicaRebuild, error) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff || deps.PVE == nil ||
		deps.RequestDirectorUUID == "" {
		return nil, nil
	}
	parsed, err := vmcid.Parse(vmCID)
	if err != nil {
		return nil, nil
	}
	const op = replicaRebuildOp
	vmid := parsed.VMID
	candidate, err := replicaRebuildCandidate(ctx, deps, vmid, rd)
	if err != nil {
		return nil, err
	}
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate replicated VM %d before its disk enters by reassignment", op, vmid))
	}
	if !loc.Found || loc.Node == "" || !hasReplicaTag(loc.Tags) {
		return nil, nil
	}
	logger := deps.Log(ctx).With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	pin, source, pinPartner, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the pin of replicated VM %d", op, vmid))
	}
	if pin == nil {
		if candidate {
			if err := retireStrandedReplica(ctx, deps, logger, op, vmCID, vmid, loc); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	pending, rebuilding := ha.ReplicaRebuild(pin.Comment)
	if !candidate && !rebuilding {
		return nil, nil
	}
	inHA, err := replicaVMInOwnHA(ctx, deps, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA resource of replicated VM %d", op, vmid))
	}
	if !inHA {
		// A VM with no HA resource of this Director has no failover for its
		// replica to serve, and the attach's HA decision finishes a removal
		// from HA that left the pin (see pendingAttachLeave).
		return nil, nil
	}
	if source != loc.Node {
		if pinPartner == "" || pinPartner != loc.Node {
			return nil, nil
		}
		pin, err = narrowReplicaPinAfterFailover(ctx, deps, logger, vmCID, vmid, loc)
		if err != nil {
			return nil, err
		}
		pending, rebuilding = ha.ReplicaRebuild(pin.Comment)
	}
	owned, partner, err := replicaRebuildJob(ctx, deps, vmid, loc.Node, pending, rebuilding)
	if err != nil || partner == "" {
		return nil, err
	}
	rb := &attachReplicaRebuild{vmCID: vmCID, vmid: vmid, partner: partner}
	switch {
	case owned == nil:
		// The job is gone. A token means an earlier call removed it and the
		// attach sets it up again; no token means no rebuild of this attach
		// removed it, and nothing here sets it up.
		if rebuilding {
			return rb, nil
		}
		return nil, nil
	case owned.RemoveJob != "":
		// pvesr removes the job on its next run, and a reassignment waits for
		// that. The removal is not asked for again, only waited on.
		if err := rb.retire(ctx, deps, loc.Node, pin); err != nil {
			return nil, err
		}
		return rb, nil
	case candidate:
		if err := gateReplicaRebuild(ctx, deps, logger, vmCID, vmid, loc.Node, rd); err != nil {
			if errors.Is(err, errLeaveRetiresReplica) {
				return nil, nil
			}
			return nil, err
		}
		if err := rb.retire(ctx, deps, loc.Node, pin); err != nil {
			return nil, err
		}
		return rb, nil
	}
	// The job stayed, as after a removal that never reached PVE, and the disk
	// needs no rebuild now. The finish keeps the job, writes its marker for the
	// journal's generation, and removes the token.
	return rb, nil
}

// replicaRebuildJob reads the replication jobs of the VM vmid on node and
// returns this Director's job, if it has one, and the partner the rebuild
// sets the job up toward. That partner is the one the token names while a
// rebuild is under way, and otherwise the job's other node. A VM with any job
// of another Director, or with no partner to name, returns no partner, and
// the attach's own checks answer it. A read that fails is retriable.
func replicaRebuildJob(
	ctx context.Context, deps Deps, vmid int, node, pending string, rebuilding bool,
) (*ha.ReplicaJobState, string, error) {
	const op = replicaRebuildOp
	jobs, err := replicationAPIFor(deps).ListJobs(ctx)
	if err != nil {
		return nil, "", cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs of VM %d", op, vmid))
	}
	owned, foreign, err := guestReplicaJobs(jobs, vmid, deps.RequestDirectorUUID)
	if err != nil {
		return nil, "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": read the VM's replication jobs")
	}
	if len(foreign) > 0 {
		return nil, "", nil
	}
	if !rebuilding && owned != nil {
		return owned, replicaOtherNode(*owned, node), nil
	}
	return owned, pending, nil
}

// narrowReplicaPinAfterFailover handles a VM that PVE HA failed over to its
// pin's partner when no call has narrowed the pin to the node the VM runs on
// now. The touch calls' reconcile narrows it, and the rebuild then runs from
// the new source. It returns the pin as it reads after the reconcile. A pin
// that still does not name the VM's node as its source is retriable, because
// the attach waits until the pin is reconciled.
func narrowReplicaPinAfterFailover(
	ctx context.Context, deps Deps, logger *log.Logger, vmCID string, vmid int, loc pve.VMLocation,
) (*ha.Rule, error) {
	const op = replicaRebuildOp
	if err := reconcileVMReplica(ctx, deps, op, vmCID, vmid, loc, logger); err != nil {
		return nil, err
	}
	pin, source, _, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the pin of replicated VM %d after its failover", op, vmid))
	}
	if pin == nil || source != loc.Node {
		return nil, cpierrors.Retriable("%s: VM %d runs on node %s after a failover, and its pin does not name "+
			"that node as its source yet; the attach waits until the pin is reconciled", op, vmid, loc.Node)
	}
	return pin, nil
}

// replicaRebuildCandidate reports, from what the attach resolved, whether the
// attach of rd into the VM vmid needs a new replica rebuild. A disk does when
// it has a stable ID, a parker or a mover holds it under the holder's own
// name, and its storage is shared and cannot replicate. It reads only the
// storage index, and a lookup that fails is retriable.
func replicaRebuildCandidate(ctx context.Context, deps Deps, vmid int, rd resolvedDisk) (bool, error) {
	if rd.stableID == "" || rd.holder == nil || !rd.holder.Found || !rd.holder.IsParker || vmid == rd.holder.VMID {
		return false, nil
	}
	embedded, named := pve.EmbeddedDiskVMID(rd.volid)
	if (!named || embedded != rd.holder.VMID) && !pve.TagsMarkDiskMover(rd.holder.Tags) {
		return false, nil
	}
	info, known, err := diskStorageInfo(ctx, deps, rd.volid)
	if err != nil {
		return false, retriableUnlessPermanent(err, fmt.Sprintf("%s: classify the storage of disk %s before it "+
			"enters replicated VM %d", replicaRebuildOp, rd.diskCID, vmid))
	}
	if !known || !info.IsShared() || storageCanReplicate(info) {
		return false, nil
	}
	return true, nil
}

// gateReplicaRebuild runs, before the rebuild removes the VM's job, the
// attach's HA decision and its read-only refusals, under a context that marks
// the rebuild, so the refusal of the replicated VM does not count the job the
// rebuild is about to remove. An attach that one of them refuses keeps the
// job and the partner's copy. A decision to take the VM out of HA returns
// errLeaveRetiresReplica, because that removal retires the job by itself. The
// attach makes the decision and runs the refusals again under the disk's
// lifecycle.
func gateReplicaRebuild(
	ctx context.Context, deps Deps, logger *log.Logger, vmCID string, vmid int, node string, rd resolvedDisk,
) error {
	marked := withReplicaRebuild(ctx)
	h, err := decideAttachHA(marked, deps, vmCID, rd)
	if err != nil {
		return err
	}
	switch {
	case h != nil && h.action == attachHALeave:
		return errLeaveRetiresReplica
	case h != nil && h.action != attachHANone:
		return h.preflight(marked)
	}
	if err := refuseOwnedLegacyAttach(replicaRebuildOp, rd, node, vmid); err != nil {
		return err
	}
	return attachDiskSnapshotGuard(marked, deps, vmCID, node, vmid, deps.Config, logger)
}

// errLeaveRetiresReplica is gateReplicaRebuild's answer for an attach whose
// HA decision takes the VM out of HA, which retires the VM's job by itself
// (see retireLeftReplica), so the attach needs no rebuild.
var errLeaveRetiresReplica = errors.New(replicaRebuildOp + ": the VM leaves HA, which retires its replication job")

// retire narrows the VM's pin to node with the rebuild token, then removes the
// VM's job with cleanup, recorded in the VM's journal record first. The
// narrowing comes first, so PVE HA never moves the VM to the partner whose
// copy the removal deletes. A job PVE has marked for removal already is only
// waited on. The removal is never forced, so a partner that cannot clean up
// makes the attach retriable, and the pin keeps the token, so the retry, or a
// call that touches the VM, finds the rebuild.
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
	err = withVMReplicaRecord(ctx, deps, rb.vmCID, func(handle *aj.Handle) error {
		r := &replicaRetire{
			deps: deps, logger: logger, api: replicationAPIFor(deps), caller: op, vmid: rb.vmid, node: node,
			handle: handle, online: replicaNodeOnline(profile, nil),
		}
		return r.run(ctx)
	})
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: remove the replication job of VM %d for the "+
			"reassignment of its disk", op, rb.vmid))
	}
	return nil
}

// finish sets the VM's job up again once the attach is done, whether the
// attach landed or not, because the VM must not stay without its replica. It
// runs after the disk's lifecycle has ended, so it may hold the VM's journal
// record. An attach that landed and whose job could not be set up again
// returns a retriable error. The token stays on the pin then, so a retry of
// the attach, or any later attach_disk to the VM, sets the job up, and so
// does the next reboot_vm or set_vm_metadata. An attach that failed keeps its
// own error, and the rebuild's failure is logged.
func (rb *attachReplicaRebuild) finish(ctx context.Context, deps Deps, attachErr error) error {
	logger := deps.Log(ctx)
	err := finishReplicaRebuild(ctx, deps, logger, replicaRebuildOp, rb.vmCID, rb.vmid)
	switch {
	case err == nil:
		return attachErr
	case attachErr != nil:
		logger.Warn(replicaRebuildOp+": the attach failed, and the VM's replication job could not be set up "+
			"again either; the next attach_disk to the VM, reboot_vm, or set_vm_metadata sets it up",
			log.Int(metadataKeyVMID, rb.vmid), log.String("partner", rb.partner), log.Err(err))
		return attachErr
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("%s: the disk is in VM %d, and its "+
		"replication job to %s could not be set up again; a retry of this attach_disk sets it up, and so does "+
		"the next attach_disk to the VM, reboot_vm, or set_vm_metadata, because the VM's pin keeps the rebuild "+
		"until then", replicaRebuildOp, rb.vmid, rb.partner))
}

// finishReplicaRebuild finishes the replica rebuild that the token on VM
// vmid's pin records. It sets up again, through the replicate policy's own
// setup, the job to the partner the token names, and then removes the token.
// A job of this Director that is in place already is kept, and its marker
// carries the journal's generation. Nothing is set up for a VM that is no
// longer under this Director's HA, because the removal from HA retired its
// replica on purpose; the token goes then, and the drop is logged. A VM still
// under this Director's HA gets its job back, and its replicate tag back too
// if someone removed it, because the CPI removes that tag only when the VM
// leaves HA. A VM that does not run on the source its pin
// names returns an error, because the setup needs the VM there, as create_vm's
// does. In auto the setup leaves the VM pinned to its source without a
// replica for a refusal on proven facts, such as a node-local volume the job
// could not copy, and the token then goes too, because no later call could
// set the job up either. A read or write that fails returns its error, and
// the token stays. A pin without the token needs nothing.
func finishReplicaRebuild(ctx context.Context, deps Deps, logger *log.Logger, op, vmCID string, vmid int) error {
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return fmt.Errorf("locate VM %d: %w", vmid, pve.WrapError(err))
	}
	if !loc.Found || loc.Node == "" {
		return fmt.Errorf("VM %d is not in the cluster index", vmid)
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	pin, source, _, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return fmt.Errorf("read the pin of VM %d: %w", vmid, err)
	}
	if pin == nil {
		// The pin goes last when a VM leaves HA, so a VM without one is out
		// of HA, and its replica went with it.
		return nil
	}
	partner, rebuilding := ha.ReplicaRebuild(pin.Comment)
	if !rebuilding {
		return nil
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("resolve the HA profile: %w", err)
	}
	inHA, err := replicaVMInOwnHA(ctx, deps, vmid)
	if err != nil {
		return fmt.Errorf("read the HA resource of VM %d: %w", vmid, err)
	}
	if !inHA {
		if err := clearReplicaRebuildToken(ctx, deps, profile, logger, vmid, pin); err != nil {
			return err
		}
		logger.Warn(op+": the VM left HA while its replica was rebuilt, so its replication job is not set up again",
			log.String("partner", partner))
		return nil
	}
	if source != loc.Node {
		return fmt.Errorf("VM %d runs on node %s, and its pin names node %s as its source; the job is set up "+
			"again once the pin is reconciled", vmid, loc.Node, source)
	}
	d := &ha.VMDecision{VMID: vmid, SID: haResourceSid(vmid), Policy: ha.PolicyReplicate, Partner: partner}
	err = withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		return setUpReplica(ctx, deps, logger, op, loc.Node, vmid, d, profile, handle)
	})
	if err != nil {
		return err
	}
	pin, _, _, err = readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		return fmt.Errorf("read the pin of VM %d: %w", vmid, err)
	}
	if pin == nil {
		return nil
	}
	if err := clearReplicaRebuildToken(ctx, deps, profile, logger, vmid, pin); err != nil {
		return err
	}
	logger.Info(op+": the VM's replication job is set up again after its disk entered by reassignment; the pin "+
		"names the source alone until a sync is verified", log.String("partner", partner))
	return nil
}

// clearReplicaRebuildToken removes the rebuild token from pin, the VM's pin
// as it was read, and does nothing when the pin carries none.
func clearReplicaRebuildToken(
	ctx context.Context, deps Deps, profile *ha.Profile, logger *log.Logger, vmid int, pin *ha.Rule,
) error {
	if _, rebuilding := ha.ReplicaRebuild(pin.Comment); !rebuilding {
		return nil
	}
	cleared := *pin
	cleared.Comment = ha.WithReplicaRebuild(pin.Comment, "")
	if err := writeReplicaPin(ctx, deps, profile, logger, vmid, pin, &cleared); err != nil {
		return fmt.Errorf("remove the rebuild token from the pin of VM %d: %w", vmid, err)
	}
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
	if err := finishReplicaRebuild(ctx, deps, logger, op, vmCID, vmid); err != nil {
		logger.Warn(op+": the VM's replication job could not be set up again; a later call that touches the VM "+
			"tries again", log.String("partner", partner), log.Err(err))
	}
	return true
}

// replicaVMInOwnHA reports whether VM vmid has an HA resource that carries
// this Director's marker. A resource that is not there reads as false.
func replicaVMInOwnHA(ctx context.Context, deps Deps, vmid int) (bool, error) {
	res, err := deps.PVE.Cluster().GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return false, nil
	case err != nil:
		return false, pve.WrapError(err)
	case res == nil:
		return false, fmt.Errorf("the read of the HA resource of VM %d returned nothing", vmid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	return haOwnershipOf(comment, deps.RequestDirectorUUID) == haOwnedByUs, nil
}

// retireStrandedReplica retires the replication job of a VM that left HA
// under a release that kept its job and its replicate tag, and removes the
// tag. Such a VM has the replicate tag, no pin, no HA resource, no HA-managed
// tag, and no pending re-registration, which no other shape of a VM the CPI
// manages has: a removal from HA removes the HA-managed tag before the pin,
// and the pin goes last. The job copies the VM to a partner it can never fail
// over to, and it makes PVE refuse every volume that cannot replicate, so
// the CPI retires it the way a removal from HA does now, with cleanup and
// never forced, recorded in the VM's journal record. A job without this
// Director's marker stays. It runs only outside the disk's lifecycle, and
// every failure is retriable.
func retireStrandedReplica(
	ctx context.Context, deps Deps, logger *log.Logger, op, vmCID string, vmid int, loc pve.VMLocation,
) error {
	tags := parseTagsField(loc.Tags)
	if !hasReplicaTag(loc.Tags) || slices.Contains(tags, haManagedTag) {
		return nil
	}
	if _, _, readding := ha.ReaddTagOf(tags); readding {
		return nil
	}
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA rules of replicated VM %d", op, vmid))
	}
	if set.Find(naRuleNameFor(vmid)) != nil {
		return nil
	}
	_, err = deps.PVE.Cluster().GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err == nil:
		return nil
	case !isHaNotFound(err) && !pve.IsNotFound(err):
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA resource of replicated VM %d", op, vmid))
	}
	profile, profileErr := resolveHAProfile(ctx, deps)
	var retired *replicaRetire
	err = withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		retired = &replicaRetire{
			deps: deps, logger: logger, api: replicationAPIFor(deps), caller: op, vmid: vmid, node: loc.Node,
			handle: handle, online: replicaNodeOnline(profile, profileErr),
		}
		return retired.run(ctx)
	})
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: retire the replication job of VM %d, which is out "+
			"of HA", op, vmid))
	}
	if err := mutateVMTags(ctx, deps, loc.Node, vmid, op+": remove the replicate tag of a VM out of HA", logger,
		func(tags []string) []string {
			return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == haReplicaTag })
		}); err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: remove the replicate tag of VM %d, which is out of "+
			"HA", op, vmid))
	}
	logger.Warn(op+": the VM is out of HA and still had the replicate tag, which an earlier release left when the "+
		"VM left HA; its replication job is retired and the tag removed", log.String("job", retired.retiredJob),
		log.String("copy_node", retired.retiredOther))
	return nil
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
