package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The failback flag of a CPI HA resource follows the VM's volumes. create_vm
// writes the HA decision's flag when it stages the resource. attach_disk turns
// failback off before a disk that leaves the VM without it lands, and decides
// the flag afresh after a disk that allows it has landed. detach_disk and every
// call that reconciles a touched VM set the flag the decision gives for the
// volumes the VM has now, so a VM that loses its last node-local disk gets
// failback back.
//
// No lock covers both an attach and a reconcile of the same VM: the attach
// holds the disk's journal record and the rules lock only around a pin write,
// and the rules lock is on only with pve.cluster_lock_mode pool. The two keep
// failback off under a node-local disk this way instead:
//
//   - The reconcile raises the flag only against the digest of resources.cfg
//     it read before the rules and the config (see ha.RaiseFailback), and
//     leaves a resource the narrowing barrier holds alone. It never raises
//     the flag while a fresh hold token says PVE HA is moving the VM into its
//     admissible set and the pin does not name that set yet (see
//     holdFailbackForMove).
//   - The attach turns failback off before the barrier, again right before
//     the disk lands, and once more after it has landed.
//   - A raise that lands reads the VM's config again and turns failback off
//     when the VM's volumes changed in between.

// vmFailbackWant is what the failback reconcile found for a VM.
type vmFailbackWant struct {
	// decided is false when the facts did not settle the flag, and the
	// reconcile then writes nothing.
	decided bool
	want    bool
	reason  string

	// current is the flag the read of the HA resource gave, and digest is the
	// digest of resources.cfg at that read. A raise names that digest, so it
	// lands only while nothing else has written resources.cfg since.
	current bool
	digest  string

	// basis is what the decision read from the VM's config: its volumes and
	// its replicate tag (see failbackBasis).
	basis string
}

// reconcileVMFailback sets the failback flag of the HA resource of VM vmid to
// what its volumes and policy call for, and writes only when the flag reads
// otherwise. It runs for a VM with the HA-managed or the replicate tag, in mode
// auto or managed, outside a managed allocation guard, once the profile lets
// the CPI write HA state. A resource that is missing, not this Director's, or
// held by a narrowing barrier is left alone. Failback is off when
// pve.ha.failback is false, when the VM replicates, and when the HA decision
// over the VM's volumes gives it a policy other than shared, an admissible set
// of one node, or no HA at all; it is on when the decision registers the VM
// under the shared policy with somewhere to fail back to. A decision that
// refuses HA for a reason that may pass leaves the flag as it is.
//
// The reconcile holds no lock that an attach in another process also holds,
// so it guards the unsafe direction against one (see syncVMFailback). Every
// failure is logged and never fails the call. The flag is never a safety fault
// the reconcile alone repairs, because create_vm writes failback off for every
// VM that may not have it, and attach_disk turns it off before such a disk
// lands or fails the attach.
func reconcileVMFailback(ctx context.Context, deps Deps, op string, vmid int, loc pve.VMLocation,
	logger *log.Logger,
) {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff ||
		loc.Node == "" || deps.RequestDirectorUUID == "" {
		return
	}
	if !hasReplicaTag(loc.Tags) && !slices.Contains(parseTagsField(loc.Tags), haManagedTag) {
		return
	}
	if _, wrapped := deps.PVE.(guardWrappedClient); wrapped {
		// A managed allocation guard admits only the writes of its own
		// lifecycle, and the next call outside one sets the flag.
		return
	}
	syncVMFailback(ctx, deps, deps.PVE.Cluster(), nil, op, vmid, loc, logger)
}

// syncVMFailback decides the failback flag of VM vmid's HA resource afresh
// and sets it through api, which reads and writes the resource, while deps
// reads the rules and the VM's config. A nil profile is resolved. A flag that
// must be off is written plainly, because off is always safe. A flag that must
// be on is raised only against the digest of the resource read the decision
// started from (see ha.RaiseFailback), so an attach that turned failback off,
// or a barrier that holds the resource, after that read makes PVE refuse the
// raise. A disk that lands between the decision's config read and the raise
// changes no HA state, so once the raise has landed the config is read again,
// and a change in the VM's volumes or replicate tag, or a read that fails,
// turns failback off again for the next call to decide. Every failure is
// logged.
func syncVMFailback(ctx context.Context, deps Deps, api ha.FailbackAPI, profile *ha.Profile, op string, vmid int,
	loc pve.VMLocation, logger *log.Logger,
) {
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	w, err := wantVMFailback(ctx, deps, api, profile, vmid, loc)
	if err != nil {
		logger.Warn(op+": the failback flag of the VM's HA resource could not be checked; a later call checks "+
			"again", log.Err(err))
		return
	}
	if !w.decided {
		logger.Debug(op+": the failback flag of the VM's HA resource stays as it is", log.String("reason", w.reason))
		return
	}
	if !w.want {
		changed, err := clearVMFailback(ctx, api, deps.RequestDirectorUUID, vmid)
		switch {
		case err != nil:
			logger.Warn(op+": the failback flag of the VM's HA resource could not be set; a later call sets it",
				log.Bool("failback", false), log.String("reason", w.reason), log.Err(err))
		case changed:
			logger.Info(op+": set the failback flag of the VM's HA resource", log.Bool("failback", false),
				log.String("reason", w.reason))
		}
		return
	}
	if w.current {
		return
	}
	raised, err := ha.RaiseFailback(ctx, api, haResourceSid(vmid), w.digest)
	switch {
	case err != nil:
		logger.Warn(op+": the failback flag of the VM's HA resource could not be set; a later call sets it",
			log.Bool("failback", true), log.String("reason", w.reason), log.Err(err))
		return
	case !raised:
		logger.Info(op+": the VM's HA resource changed after it was read, or its read named no digest, so "+
			"failback stays off; a later call decides it again", log.String("reason", w.reason))
		return
	}
	logger.Info(op+": set the failback flag of the VM's HA resource", log.Bool("failback", true),
		log.String("reason", w.reason))
	confirmFailbackBasis(ctx, deps, api, op, vmid, loc.Node, w.basis, logger)
}

// confirmFailbackBasis reads VM vmid's config again after failback was turned
// on from basis, and turns failback off again when the VM's volumes or its
// replicate tag changed since, or when the read fails.
func confirmFailbackBasis(ctx context.Context, deps Deps, api ha.FailbackAPI, op string, vmid int, node,
	basis string, logger *log.Logger,
) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	why := ""
	switch {
	case err != nil:
		why = "the VM config could not be read again: " + err.Error()
	case failbackBasis(cfg, node) != basis:
		why = "the VM's volumes changed after the flag was decided"
	default:
		return
	}
	if _, err := clearVMFailback(ctx, api, deps.RequestDirectorUUID, vmid); err != nil {
		logger.Warn(op+": failback was turned on, and the VM's config changed or could not be read again, and "+
			"failback could not be turned off again; the next attach or reconcile of the VM sets it",
			log.String("reason", why), log.Err(err))
		return
	}
	logger.Warn(op+": failback was turned on, and the VM's config changed or could not be read again, so the "+
		"reconcile turned failback off again; a later call decides it again", log.String("reason", why))
}

// failbackBasis is what a failback decision reads from a VM's config: the
// volumes it classifies, and whether the VM carries the replicate tag.
func failbackBasis(cfg map[string]any, node string) string {
	vols := haVolumesOfConfig(cfg, node, nil)
	ids := make([]string, 0, len(vols))
	for _, v := range vols {
		ids = append(ids, v.Volume)
	}
	slices.Sort(ids)
	return fmt.Sprintf("volumes=%s replicate=%v", strings.Join(ids, ","), configHasReplicaTag(cfg))
}

// clearVMFailback turns off the failback flag of the HA resource of VM vmid
// that Director owner owns when it reads on, through api (see
// ha.ReconcileFailback). Raising the flag goes through ha.RaiseFailback with
// the digest of the read the decision rested on, never through this helper.
func clearVMFailback(ctx context.Context, api ha.FailbackAPI, owner string, vmid int) (bool, error) {
	return ha.ReconcileFailback(ctx, api, haResourceSid(vmid), false, func(comment string) bool {
		return owner != "" && haOwnershipOf(comment, owner) == haOwnedByUs
	})
}

// wantVMFailback finds the failback flag VM vmid's HA resource should carry.
// It reads the resource through api before it reads the rules and the config,
// so the digest it returns predates every fact the decision rests on. A nil
// profile is resolved.
func wantVMFailback(ctx context.Context, deps Deps, api ha.FailbackAPI, profile *ha.Profile, vmid int,
	loc pve.VMLocation,
) (vmFailbackWant, error) {
	if profile == nil {
		var err error
		if profile, err = resolveHAProfile(ctx, deps); err != nil {
			return vmFailbackWant{}, fmt.Errorf("resolve the HA profile: %w", err)
		}
	}
	if !profile.WritesAllowed() {
		return vmFailbackWant{reason: "the HA profile does not let the CPI write HA state"}, nil
	}
	off := func(reason string) (vmFailbackWant, error) {
		return vmFailbackWant{decided: true, reason: reason}, nil
	}
	if !deps.Config.HAFailbackEnabled() {
		return off("pve.ha.failback is false")
	}
	if hasReplicaTag(loc.Tags) {
		return off("the VM replicates its volumes to a partner node")
	}
	res, err := api.GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return vmFailbackWant{reason: "the VM has no HA resource"}, nil
	case err != nil:
		return vmFailbackWant{}, fmt.Errorf("read the HA resource: %w", pve.WrapError(err))
	case res == nil:
		return vmFailbackWant{}, cpierrors.Retriable("the read of the HA resource of VM %d returned nothing", vmid)
	case res.Comment == nil || haOwnershipOf(*res.Comment, deps.RequestDirectorUUID) != haOwnedByUs:
		return vmFailbackWant{reason: "the VM's HA resource is not this Director's"}, nil
	case ha.NarrowPending(res.State, res.Comment):
		return vmFailbackWant{reason: "a narrowing barrier holds the VM's HA resource, and the call that holds " +
			"it sets the flag"}, nil
	}
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("read the HA rules: %w", err)
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin != nil && replicaPinComment(pin.Comment) {
		return off("the VM's pin carries a token of the replicate policy")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, loc.Node, vmid)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("read the VM config: %w", pve.WrapError(err))
	}
	if configHasReplicaTag(cfg) {
		return off("the VM replicates its volumes to a partner node")
	}
	in := pinWidenInput(deps, vmid, loc.Node, cfg, res)
	if pin != nil && pin.Strict && !pin.Disabled && haOwnershipOf(pin.Comment, deps.RequestDirectorUUID) == haOwnedByUs {
		in.Constraints = append(in.Constraints, ownPinConstraint(vmid, pin.NodeNames()))
	}
	d, err := profile.DecideVM(in)
	if err != nil {
		return vmFailbackWant{}, fmt.Errorf("decide HA for the VM: %w", err)
	}
	switch {
	case d.Registers() && d.Failback && ha.FailbackHeld(res.Comment, time.Now()):
		return vmFailbackWant{reason: "a move into the VM's admissible set holds failback off until the VM's " +
			"pin names the node it moved to, and the call that moves it sets the flag"}, nil
	case d.Registers():
		return vmFailbackWant{decided: true, want: d.Failback, reason: d.Reason,
			current: ha.FailbackOf(res.Failback), digest: res.Digest, basis: failbackBasis(cfg, loc.Node)}, nil
	case d.Transient || d.PlaceableShortfall:
		return vmFailbackWant{reason: "the HA decision refused HA for a reason that may pass: " + d.Reason}, nil
	}
	return off("the HA decision gives the VM no HA: " + d.Reason)
}
