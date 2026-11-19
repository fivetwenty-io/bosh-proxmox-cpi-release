package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// attachHAOp prefixes the errors and logs of the attach-time HA decision.
const attachHAOp = "attach_disk"

// attachHAAction is what the attach-time HA decision makes the attach do.
type attachHAAction int

const (
	// attachHANone attaches as before: the VM is not HA-managed by this
	// Director, or the disk's storage is already in its pin's volume record.
	attachHANone attachHAAction = iota

	// attachHALeave removes the VM from PVE HA and waits until the CRM lets go
	// of it before the attach, because the VM may no longer run under HA with
	// the new disk. It also finishes a removal an earlier attach left part
	// way, whose HA resource is gone while the VM's pin is still there.
	attachHALeave

	// attachHAGuarded runs the attach with the pin the decision gives for the
	// VM's volumes and the new disk. A pin that narrows where the VM may run
	// is written inside the narrowing barrier, before the attach. A pin that
	// does not narrow is written after the attach.
	attachHAGuarded
)

// attachHA is the attach-time HA decision for one attach_disk call. It is made
// before the attach's first mutation, the resume of an interrupted transfer
// included, and it only reads.
type attachHA struct {
	deps     Deps
	logger   *log.Logger
	vmCID    string
	disk     resolvedDisk
	vmid     int
	sid      string
	node     string
	action   attachHAAction
	profile  *ha.Profile
	decision *ha.VMDecision
	oldPin   *ha.Rule
	newPin   *ha.Rule

	// narrow holds the reads and writes of the barrier and of the removal
	// from HA, wired to the VM's client.
	narrow ha.NarrowDeps

	// replicaChanged is when the attach changed a replicate VM's volume set,
	// zero for any other VM.
	replicaChanged time.Time

	// replicaTagged is true when the VM carries the replicate tag, so a
	// removal from HA retires the VM's replication job too.
	replicaTagged bool

	// leftReplica is the replication job the removal from HA retired, nil
	// when it retired none. The journal records it after the disk's lifecycle
	// ends (see recordLeftReplica).
	leftReplica *retiredReplica

	// failbackOff is set on an attach that skips the HA decision while the
	// VM's resource reads failback on and the disk's storage is not proven
	// shared. The attach then turns failback off before the disk lands.
	failbackOff bool
}

// decideAttachHA makes the attach-time HA decision for attaching the disk rd
// to the VM vmCID. Mode off reads nothing. A VM whose HA resource is missing,
// or carries no marker of this Director, gets no HA action, because the CPI
// may change only what it owns. A VM whose pin already records the disk's
// storage needs no change, unless an earlier barrier stopped part way. Every
// other attach to an HA-managed VM needs the profile to let the CPI write HA
// state, and otherwise it returns a retriable error that names what is
// missing, so the attach never adds a volume the VM's confinement does not
// account for. The decision then runs over the
// VM's volumes and the new disk:
//
//   - A failing decision returns its error.
//   - A decision that refuses HA for a reason that may pass, such as a node
//     that is down, in maintenance, or unreadable, or a storage read that
//     failed, returns a retriable error and changes nothing in HA.
//   - A decision that gives the VM no HA on proven facts, such as a local
//     disk under pve.ha.local_policy none, makes the attach remove the VM
//     from HA first.
//   - A decision that registers the VM makes the attach run inside the
//     narrowing barrier with the pin it gives.
//
// Every read failure is retriable.
func decideAttachHA(ctx context.Context, deps Deps, vmCID string, rd resolvedDisk) (*attachHA, error) {
	const op = attachHAOp
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff || deps.PVE == nil {
		return nil, nil
	}
	parsed, err := vmcid.Parse(vmCID)
	if err != nil {
		// The node lookup that follows answers VMNotFound for the same CID.
		return nil, nil
	}
	vmid := parsed.VMID
	sid := haResourceSid(vmid)
	logger := deps.Log(ctx)
	owner := deps.RequestDirectorUUID
	svc := deps.PVE.Cluster()

	res, err := svc.GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return pendingAttachLeave(ctx, deps, vmCID, vmid, rd)
	case err != nil:
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA resource of VM %d", op, vmid))
	case res == nil:
		return nil, cpierrors.Retriable("%s: the read of the HA resource of VM %d returned nothing", op, vmid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	if owner == "" || haOwnershipOf(comment, owner) != haOwnedByUs {
		logger.Info(op+": the VM's HA resource is not this Director's, so the attach changes nothing in HA",
			log.Int(metadataKeyVMID, vmid))
		return nil, nil
	}

	storage, _, ok := strings.Cut(rd.volid, ":")
	if !ok || storage == "" {
		return nil, cpierrors.DiskNotFound(rd.volid)
	}
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA rules for VM %d", op, vmid))
	}
	oldPin := set.Find(naRuleNameFor(vmid))
	if oldPin != nil && haOwnershipOf(oldPin.Comment, owner) == haOwnedByOther {
		return nil, cpierrors.Cloud("%s: the pin %s of VM %d carries another Director's ownership marker", op,
			oldPin.ID, vmid)
	}
	legacyManaged := rd.stableID == "" && deps.Config.HAModeValue() == config.HAModeManaged
	if attachSkipsHADecision(oldPin, res, storage, legacyManaged) {
		return skippedAttachFailback(ctx, deps, vmCID, rd, vmid, res, oldPin, storage), nil
	}

	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: resolve the HA profile for VM %d", op, vmid))
	}
	if err := attachHAWritable(profile, vmid); err != nil {
		return nil, err
	}

	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate VM %d for its HA decision", op, vmid))
	}
	if !loc.Found || loc.Node == "" {
		return nil, cpierrors.VMNotFound(vmCID)
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, loc.Node, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the config of VM %d for its HA decision", op, vmid))
	}

	in := attachHAInput(deps, vmid, loc.Node, cfg, res, oldPin, rd)
	d, err := profile.DecideVM(in)
	if err != nil {
		return nil, err
	}
	narrow := attachNarrowDeps(deps.PVE, logger, haMotionWait(deps))
	if len(d.Outside) > 0 {
		// The VM runs under HA on a node outside the set it may run on with
		// the new disk, which is where an HA migration the CRM had queued
		// leaves it. PVE HA moves it into the set first, so it keeps HA, and
		// the decision runs again on the node it moved to.
		if loc.Node, cfg, d, err = attachDecideInsideSet(ctx, deps, narrow, profile, attachOutside{
			vmid: vmid, node: loc.Node, cfg: cfg, decision: d, res: res, oldPin: oldPin, disk: rd,
		}); err != nil {
			return nil, err
		}
		in = attachHAInput(deps, vmid, loc.Node, cfg, res, oldPin, rd)
	}
	for _, w := range d.Warnings {
		logger.Warn(op+": "+w, log.Int(metadataKeyVMID, vmid))
	}
	h := &attachHA{
		deps: deps, logger: logger, vmCID: vmCID, disk: rd, vmid: vmid, sid: sid, node: loc.Node,
		profile: profile, decision: d, oldPin: oldPin, narrow: narrow,
		replicaTagged: configHasReplicaTag(cfg),
	}
	// A transient refusal and a placeable shortfall come before the failure,
	// because in managed mode a refusal for a reason that may pass is also a
	// failure, and it must defer rather than fail the attach for good.
	switch {
	case d.Transient:
		// The decision refused HA for a reason that may pass, such as a node
		// of the VM's CPU domain that is down or unreadable, or a storage read
		// that failed. Leaving HA cannot be undone by the next attach, so the
		// attach waits and the VM keeps its HA resource, its tag, and its pin.
		msg := fmt.Sprintf("%s: VM %d is HA-managed, and the HA decision refused HA with the new disk for a "+
			"reason that may pass; the attach waits rather than take the VM out of HA for it", op, vmid)
		if failure := d.Failure(); failure != nil {
			return nil, cpierrors.WrapAs(failure, cpierrors.TypeRetriableCloud, msg)
		}
		return nil, cpierrors.Retriable("%s (%s)", msg, d.Reason)
	case d.PlaceableShortfall:
		// Enough nodes reach the VM's volumes, and the VM misses full HA only
		// because some of them are unhealthy or in maintenance now. A VM that
		// already runs under HA does not leave HA, or get pinned to one node,
		// for an outage, so the attach waits for the nodes to come back.
		return nil, cpierrors.Retriable("%s: VM %d is HA-managed, and nodes %s reach all of its volumes with the "+
			"new disk, but fewer than two of them can take the VM now; the attach waits until enough of them "+
			"are healthy again rather than take the VM out of HA for a temporary outage", op, vmid,
			strings.Join(d.Access, ","))
	case d.Outcome == ha.OutcomeFail:
		if failure := d.Failure(); failure != nil {
			return nil, failure
		}
		return nil, cpierrors.Cloud("%s: VM %d: the HA decision failed the call: %s", op, vmid, d.Reason)
	case d.Outcome == ha.OutcomeRegister:
		newPin, err := attachNewPin(d, oldPin, profile, set, ha.VolumeStorages(in.Volumes), owner)
		if err != nil {
			return nil, err
		}
		if d.Policy == ha.PolicyReplicate {
			// The pin names the VM's node alone while the new disk has no
			// replica, and it records when the volume set changed, which the
			// next verified sync must follow.
			h.replicaChanged = time.Now()
			newPin.Comment = ha.WithReplicaChanged(ha.WithReplicaPartner(newPin.Comment, ""), h.replicaChanged)
		}
		h.action, h.newPin = attachHAGuarded, newPin
	default:
		logger.Warn(op+": the VM leaves PVE HA before the attach, because the HA decision gives it no HA with "+
			"the new disk", log.Int(metadataKeyVMID, vmid), log.String("disk", rd.volid),
			log.String("reason", d.Reason))
		h.action = attachHALeave
	}
	return h, nil
}

// skippedAttachFailback guards failback on an attach that skips the HA
// decision because the VM's pin already records the disk's storage. A pin
// that records a storage says nothing about the failback flag, which an
// operator or an older release may have turned on, so a resource that reads
// failback on under a strict pin, while the disk's storage is not proven
// shared, gets an attach that turns it off before the disk lands. A storage
// the profile cannot classify counts as not shared. A non-strict pin needs no
// check and no profile: the decision writes one only when every node reaches
// every volume, so each storage it records was shared when it was recorded.
// Every other skip gets no HA action.
func skippedAttachFailback(ctx context.Context, deps Deps, vmCID string, rd resolvedDisk, vmid int,
	res *cluster.GetHaResourcesResponse, pin *ha.Rule, storage string,
) *attachHA {
	if !ha.FailbackOf(res.Failback) || pin == nil || !pin.Strict {
		return nil
	}
	logger := deps.Log(ctx)
	why := "the HA profile could not be resolved"
	if profile, err := resolveHAProfile(ctx, deps); err == nil {
		shared, known := profile.StorageShared(deps.Config, storage)
		if shared {
			return nil
		}
		why = "the storage is node-local"
		if !known {
			why = "the storage definitions do not classify the storage"
		}
	}
	logger.Info(attachHAOp+": the VM's pin records the disk's storage, and its HA resource has failback on "+
		"while the storage is not proven shared, so the attach turns failback off before the disk lands",
		log.Int(metadataKeyVMID, vmid), log.String("storage", storage), log.String("reason", why))
	return &attachHA{
		deps: deps, logger: logger, vmCID: vmCID, disk: rd, vmid: vmid, sid: haResourceSid(vmid),
		action: attachHANone, failbackOff: true, narrow: attachNarrowDeps(deps.PVE, logger, haMotionWait(deps)),
	}
}

// attachNarrowDeps wires the barrier, the removal from HA, and the move into
// a node set to a client. Tests replace it to script PVE HA.
var attachNarrowDeps = ha.NarrowDepsFor

// attachOutside is the HA decision of a VM that DecideVM found outside its
// admissible set, with what attachDecideInsideSet needs to decide again.
type attachOutside struct {
	vmid     int
	node     string
	cfg      map[string]any
	decision *ha.VMDecision
	res      *cluster.GetHaResourcesResponse
	oldPin   *ha.Rule
	disk     resolvedDisk
}

// attachDecideInsideSet has PVE HA move a VM that runs outside its admissible
// set into it, and decides again on the node the VM reaches. It returns the
// VM's node, its config, and the decision that apply from then on. When the
// move fails and the VM leaves HA, it returns the first decision unchanged.
func attachDecideInsideSet(ctx context.Context, deps Deps, narrow ha.NarrowDeps, profile *ha.Profile,
	o attachOutside,
) (string, map[string]any, *ha.VMDecision, error) {
	const op = attachHAOp
	moved, err := attachMoveIntoSet(ctx, deps, narrow, profile, o.vmid, o.node, o.decision.Outside, o.oldPin,
		o.disk)
	if err != nil || moved == "" {
		return o.node, o.cfg, o.decision, err
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, moved, o.vmid)
	if err != nil {
		return "", nil, nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the config of VM %d for its HA decision after PVE HA moved it", op, o.vmid))
	}
	d, err := profile.DecideVM(attachHAInput(deps, o.vmid, moved, cfg, o.res, o.oldPin, o.disk))
	if err != nil {
		return "", nil, nil, err
	}
	if len(d.Outside) > 0 {
		return "", nil, nil, cpierrors.Retriable("%s: PVE HA moved VM %d to node %s, and the VM is still outside "+
			"its admissible set %s with the new disk, so the attach waits and the VM keeps its HA resource, its "+
			"tag, and its pin. %s", op, o.vmid, moved, strings.Join(d.Outside, ","), attachMoveAction(o.vmid,
			d.Outside))
	}
	return moved, cfg, d, nil
}

// attachMoveIntoSet asks PVE HA to move an HA-managed VM that runs on node,
// outside set, its admissible set with the new disk, to a node of set (see
// ha.MoveIntoSet). It returns the node the VM moved to.
//
// When the move does not happen, the VM keeps HA wherever leaving HA would not
// help. A node that is down defers the attach in every mode, because PVE HA
// recovers the VM by itself. Mode managed defers too, because it never takes a
// VM out of HA. Mode auto takes the VM out of HA only when its node reaches the
// new disk's storage, so the attach can then run there. It logs a warning that
// names the VM, the set, and why the move did not happen, and returns "" with
// no error, which leaves the decision's skip in place. A node that does not
// reach the storage defers the attach, because the VM would leave HA and the
// attach would still fail. Every error is retriable and names the set and what
// an operator can do.
func attachMoveIntoSet(ctx context.Context, deps Deps, narrow ha.NarrowDeps, profile *ha.Profile, vmid int,
	node string, set []string, pin *ha.Rule, rd resolvedDisk,
) (string, error) {
	const op = attachHAOp
	logger := deps.Log(ctx)
	where := strings.Join(set, ",")
	logger.Info(op+": the HA-managed VM runs outside its admissible set with the new disk, so PVE HA moves it "+
		"into the set before the attach", log.Int(metadataKeyVMID, vmid), log.String("node", node),
		log.String("set", where))
	var holdErr error
	loc, err := ha.MoveIntoSet(ctx, narrow, ha.MoveInput{
		VMID: vmid, Node: node, Set: set, Targets: ha.MoveTargets(profile, set, pin, node),
		Budget: haMotionWait(deps),
		BeforeMigrate: func(ctx context.Context) error {
			holdErr = holdFailbackForMove(ctx, deps, narrow, vmid)
			return holdErr
		},
	})
	if err == nil {
		return loc.Node, nil
	}
	action := attachMoveAction(vmid, set)
	keep := func(why string, cause error) error {
		msg := fmt.Sprintf("%s: VM %d is HA-managed and runs on node %s, outside its admissible set %s with the new "+
			"disk, and PVE HA did not move it into the set%s, so the attach waits and the VM keeps its HA resource, "+
			"its tag, and its pin", op, vmid, node, where, why)
		return cpierrors.WrapAs(fmt.Errorf("%w. %s", cause, action), cpierrors.TypeRetriableCloud, msg)
	}
	var moveErr *ha.MoveError
	if holdErr != nil {
		// A failback flag that may still be on would undo the move, so the
		// move never started, and leaving HA for that would be out of all
		// proportion.
		return "", keep(", because the attach could not turn off failback first", err)
	}
	if errors.As(err, &moveErr) && moveErr.Failure == ha.MoveNodeDown {
		return "", keep("", err)
	}
	if deps.Config.HAModeValue() == config.HAModeManaged {
		return "", keep(", and pve.ha.mode managed never takes a VM out of HA", err)
	}
	if fitErr := storageFitsVMNode(ctx, deps, op, rd.volid, "", node); fitErr != nil {
		logger.Warn(op+": PVE HA did not move the VM into its admissible set, and its node does not reach the new "+
			"disk's storage, so the VM keeps HA and the attach waits", log.Int(metadataKeyVMID, vmid),
			log.String("node", node), log.String("set", where), log.String("reason", err.Error()),
			log.String("storage", fitErr.Error()))
		return "", keep(", and leaving HA would not let the attach run there because the node does not reach the "+
			"new disk's storage", err)
	}
	logger.Warn(op+": PVE HA did not move the VM into its admissible set, and its node reaches the new disk's "+
		"storage, so the VM leaves PVE HA before the attach",
		log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("set", where),
		log.String("reason", err.Error()))
	return "", nil
}

// holdFailbackForMove turns failback off on the VM's HA resource right before
// PVE HA is asked to move the VM into its admissible set. The VM's pin still
// names its old placement then, and with failback on the CRM would pull the
// VM back to the node that pin prefers as soon as the move lands, and the
// attach would move it again on its retry. The barrier writes the new pin,
// which records the moved-to node, and the attach turns failback on again
// after that, when its decision allows it (see attachHA.syncFailback).
func holdFailbackForMove(ctx context.Context, deps Deps, narrow ha.NarrowDeps, vmid int) error {
	if _, err := setVMFailback(ctx, narrow.Resources, deps.RequestDirectorUUID, vmid, false); err != nil {
		return fmt.Errorf("turn off failback on the HA resource of VM %d before the move: %w", vmid, err)
	}
	return nil
}

// attachMoveAction says what an operator does about an HA-managed VM outside
// its admissible set. The attach turned failback off before it asked for the
// move, so a manual migration stays where it lands, and the attach's retry
// writes the pin for the new node and turns failback on again.
func attachMoveAction(vmid int, set []string) string {
	example := "<node>"
	if len(set) > 0 {
		example = set[0]
	}
	return fmt.Sprintf("To fix it, migrate the VM to a node of %s, for example with ha-manager migrate %s %s, "+
		"and retry the attach. If the VM's HA resource has failback on, set it off first with ha-manager set "+
		"%s --failback 0, or the CRM moves the VM back to the node its pin prefers; the attach "+
		"turns failback on again once the pin names the new node", strings.Join(set, ","), haResourceSid(vmid),
		example, haResourceSid(vmid))
}

// pendingAttachLeave finishes, on the next attach, a removal from HA that an
// earlier attach left part way. That attach removed the VM's HA resource and
// then failed before it removed the VM's pin, which goes last, so a VM with no
// HA resource that still has this Director's pin is a pending leave. Mode off
// reaches nothing here. A rules read that fails, or a pin that is missing or
// not this Director's, gives no HA action, because a pin that names no
// resource confines nothing and delete_vm removes it.
func pendingAttachLeave(ctx context.Context, deps Deps, vmCID string, vmid int, rd resolvedDisk) (*attachHA, error) {
	const op = attachHAOp
	owner := deps.RequestDirectorUUID
	if owner == "" {
		return nil, nil
	}
	logger := deps.Log(ctx)
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		logger.Debug(op+": the HA rules could not be read to look for a pin left by an earlier removal from HA",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
		return nil, nil
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin == nil || haOwnershipOf(pin.Comment, owner) != haOwnedByUs {
		return nil, nil
	}
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate VM %d to finish its removal from HA", op, vmid))
	}
	if !loc.Found || loc.Node == "" {
		return nil, cpierrors.VMNotFound(vmCID)
	}
	if tag, _, tagged := ha.ReaddTagOf(parseTagsField(loc.Tags)); tagged {
		// The VM's resource is gone because an HA re-registration removed it,
		// and that order, not a removal from HA, finishes it. Its decision
		// runs over the VM's volumes then, the new disk included.
		logger.Info(op+": the VM's HA resource is gone for a re-registration that is not finished, so the "+
			"attach leaves its pin to that order", log.Int(metadataKeyVMID, vmid), log.String("tag", tag))
		return nil, nil
	}
	logger.Warn(op+": the VM's HA resource is gone but its pin is still there, so the attach finishes its "+
		"removal from HA first", log.Int(metadataKeyVMID, vmid), log.String("pin", pin.ID))
	return &attachHA{
		deps: deps, logger: logger, vmCID: vmCID, disk: rd, vmid: vmid, sid: haResourceSid(vmid), node: loc.Node,
		action: attachHALeave, oldPin: pin, narrow: ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps)),
		replicaTagged: hasReplicaTag(loc.Tags),
	}, nil
}

// attachHAWritable returns the retriable error of an attach that must change a
// VM's HA state when the profile does not let the CPI write it. A node known to
// run below the PVE 9 floor comes first, named with its version, because the
// VM then keeps its HA registration as it is until the node is upgraded. The
// error names the Sys.Console privilege when the token lacks it, and the
// profile's first gap otherwise.
func attachHAWritable(profile *ha.Profile, vmid int) error {
	const op = attachHAOp
	if err := profile.HAFloorError(vmid); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op)
	}
	if profile.WritesAllowed() {
		return nil
	}
	if !profile.SysConsole {
		return cpierrors.Retriable("%s: VM %d is HA-managed and the new disk changes where it may run, but the "+
			"token lacks the Sys.Console privilege on / that the HA state change needs", op, vmid)
	}
	reason := profile.Reason
	if reason == "" {
		reason = strings.Join(profile.Gaps, "; ")
	}
	return cpierrors.Retriable("%s: VM %d is HA-managed and the new disk changes where it may run, but the CPI "+
		"may not write HA state: %s", op, vmid, reason)
}

// readHARuleSet reads and parses the cluster's HA rules.
func readHARuleSet(ctx context.Context, svc cluster.Service) (*ha.RuleSet, error) {
	resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, pve.WrapError(err)
	}
	return ha.ParseRuleList(resp)
}

// attachHAInput builds the decision input of an attach: the VM's volumes and
// the new disk, homed on the VM's node because the attach puts a node-local
// disk there. A strict pin is a constraint, so the decision never widens the
// set the VM may run on, and a PCI passthrough device is one too. A pin that
// is not strict, such as the record pin or the placed-node pin a cluster with
// ha-rebalance-on-start gets, states a preference and confines nothing, so it
// is no constraint, and only the volumes' access set bounds the decision. A VM
// whose resource rebalances automatically is a DLB VM. The VM runs under HA,
// so the decision defers rather than take it out of HA when it runs outside its
// strict pin, and it turns only the dynamic load balancer off when a node runs
// below PVE 9.2. The existing volumes were judged when the VM registered, so
// only the new disk can be legacy.
//
// The input carries no CRM state, so the decision has no DecisionUID, and the
// rule writer's repair stays fail-closed at attach. A pin repair that cannot
// be verified inside the barrier then returns a retriable error and leaves the
// resource ignored with its token, which the next attach finishes, rather
// than removing a resource the CRM is acting on.
func attachHAInput(
	deps Deps, vmid int, node string, cfg map[string]any, res *cluster.GetHaResourcesResponse, oldPin *ha.Rule,
	rd resolvedDisk,
) ha.VMInput {
	return attachDecisionInput(deps, vmid, node, cfg, res.AutoRebalance != nil && res.AutoRebalance.Bool(), oldPin,
		rd)
}

// attachDecisionInput is attachHAInput with the resource's auto-rebalance flag
// given as inDLB, for a caller whose resource is gone and whose record keeps
// the flag.
func attachDecisionInput(
	deps Deps, vmid int, node string, cfg map[string]any, inDLB bool, oldPin *ha.Rule, rd resolvedDisk,
) ha.VMInput {
	volumes := haVolumesOfConfig(cfg, node, nil)
	if !slices.ContainsFunc(volumes, func(v ha.VMVolume) bool { return v.Volume == rd.volid }) {
		volumes = append(volumes, ha.VMVolume{
			Volume: rd.volid, HomeNode: node, Kind: ha.VolumeDisk, Legacy: rd.stableID == "",
		})
	}
	liveCPU, _ := pve.ConfigString(cfg, "cpu")
	in := ha.VMInput{
		Config:    deps.Config,
		VMID:      vmid,
		Node:      node,
		Volumes:   volumes,
		CPUType:   ha.EffectiveCPUType(deps.Config, nil, liveCPU),
		InDLBAZ:   inDLB,
		HAManaged: true,
	}
	if oldPin != nil && oldPin.Strict && !oldPin.Disabled {
		in.Constraints = append(in.Constraints, ownPinConstraint(vmid, oldPin.NodeNames()))
	}
	if configHasPCIPassthrough(cfg) {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{node}})
	}
	return in
}

// configHasPCIPassthrough reports whether a VM config carries a hostpci entry.
func configHasPCIPassthrough(cfg map[string]any) bool {
	for key := range cfg {
		if strings.HasPrefix(key, "hostpci") {
			return true
		}
	}
	return false
}

// attachNewPin returns the pin the VM needs with the new disk. A strict pin
// that is in effect keeps its priorities and loses the nodes the decision no
// longer admits, and it stays strict. Any other pin, and a VM with no pin, gets
// the decision's pin, which is what create_vm writes for the same volumes, so
// a pin that only stated a preference is never made strict by itself. The
// decision's pin is strict only when the volumes' access set, a PCI device, or
// the CPU domain confines the VM. The record names every storage the VM's
// volumes use. A pin that would name no node is a retriable error.
func attachNewPin(
	d *ha.VMDecision, oldPin *ha.Rule, profile *ha.Profile, set *ha.RuleSet, storages []string, owner string,
) (*ha.Rule, error) {
	ruleName := naRuleNameFor(d.VMID)
	configured := profileNodeNames(profile)
	if oldPin == nil || !oldPin.Strict || oldPin.Disabled {
		return ha.DecisionPin(d, ruleName, haIdentity{Owner: owner}.marker(), storages, configured,
			profileRebalancesOnStart(profile), inResourceAffinityRule(set, d.SID))
	}
	if len(storages) == 0 {
		return nil, cpierrors.Cloud("pin %s: the VM holds no volume to record", ruleName)
	}
	pin := *oldPin
	pin.Nodes = maps.Clone(oldPin.Nodes)
	pin.Resources = slices.Clone(oldPin.Resources)
	for n := range pin.Nodes {
		if !slices.Contains(d.Admissible, n) {
			delete(pin.Nodes, n)
		}
	}
	if len(pin.Nodes) == 0 {
		return nil, cpierrors.Retriable("pin %s: no node of the VM's pin is admissible with the new disk", ruleName)
	}
	pin.Comment = ha.WithVolumeRecord(oldPin.Comment, storages)
	pin.Errors = nil
	return &pin, nil
}

// run runs attach, the rest of the attach, as the HA decision requires, and
// returns its result. attach returns the node the VM is on when it is done.
// The attach's read-only refusals run first, before anything changes in HA,
// so an attach they refuse leaves the VM's HA state, pin, and tag as they
// were (see preflight).
func (h *attachHA) run(ctx context.Context, attach func(context.Context) (string, error)) error {
	const op = attachHAOp
	if h != nil && h.failbackOff {
		return h.runFailbackOff(ctx, attach)
	}
	if h == nil || h.action == attachHANone {
		_, err := attach(ctx)
		return err
	}
	if err := h.preflight(ctx); err != nil {
		return err
	}
	narrowDeps := h.narrow
	owned := func(comment string) bool {
		return haOwnershipOf(comment, h.deps.RequestDirectorUUID) == haOwnedByUs
	}
	if h.action == attachHALeave {
		loc, cleared, err := h.leaveErrored(ctx)
		if err != nil {
			return err
		}
		if !cleared {
			loc, err = ha.LeaveHA(ctx, narrowDeps, h.vmid, h.node, haMotionWait(h.deps), owned)
			if err != nil {
				return err
			}
		}
		if err := h.retireLeftReplica(ctx, loc.Node); err != nil {
			return err
		}
		// The pin goes last, so a removal that stops part way leaves the pin,
		// and the next attach finds it and finishes (see pendingAttachLeave).
		label := op + ": remove the HA-managed tag"
		if err := removeHAManagedTag(ctx, h.deps, loc.Node, h.vmid, label, h.logger); err != nil {
			return err
		}
		if err := removeNodeAffinityPin(ctx, h.deps, h.vmid, h.logger); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: remove the pin of VM %d after it left HA", op, h.vmid))
		}
		_, err = attach(ctx)
		return err
	}
	// A VM that may not fail back with the new disk, such as one that gains a
	// node-local or replicated disk, has failback turned off before the disk
	// lands, so PVE HA never moves it back while it holds that disk.
	if !h.decision.Failback {
		if err := h.lowerFailback(ctx); err != nil {
			return err
		}
	}
	// The barrier's mutation is the whole rest of the attach, which locates the
	// VM itself, so the node the drain found is only where it starts.
	var landed string
	narrowDeps.Mutate = func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
		if err := m.Apply(ctx, m.Node); err != nil {
			return pve.VMLocation{}, err
		}
		return pve.VMLocation{Node: landed, Found: true}, nil
	}
	_, err := ha.NarrowActive(ctx, narrowDeps, ha.NarrowInput{
		VMID:   h.vmid,
		Node:   h.node,
		OldPin: h.oldPin,
		NewPin: h.newPin,
		WritePin: func(ctx context.Context, pin *ha.Rule) error {
			return h.writePin(ctx, pin)
		},
		Mutation: pve.VMConfigMutation{
			VMID:       h.vmid,
			Node:       h.node,
			MotionWait: haMotionWait(h.deps),
			Label:      op + ": attach the disk inside the HA barrier",
			Apply: func(ctx context.Context, _ string) error {
				if !h.decision.Failback {
					// A reconcile in another process may have turned failback
					// on since the write before the barrier, from a config
					// without this disk.
					if err := h.lowerFailback(ctx); err != nil {
						return err
					}
				}
				node, err := attach(ctx)
				landed = node
				if err == nil && !h.decision.Failback {
					h.recheckFailbackOff(ctx)
				}
				return err
			},
		},
		CheckReach: h.checkReach,
		Unchanged:  h.attachUnchanged,
		Moving:     h.attachMoving,
		Reregister: h.reregister,
		MoveInto: func(ctx context.Context, node string, set []string) (pve.VMLocation, error) {
			return ha.MoveIntoSet(ctx, narrowDeps, ha.MoveInput{
				VMID: h.vmid, Node: node, Set: set, Targets: ha.MoveTargets(h.profile, set, h.newPin, node),
				Budget: haMotionWait(h.deps),
				BeforeMigrate: func(ctx context.Context) error {
					return holdFailbackForMove(ctx, h.deps, narrowDeps, h.vmid)
				},
			})
		},
		Failback:      h.decision.Failback,
		AutoRebalance: h.decision.DLB,
		SysConsole:    h.profile.SysConsole,
		Budget:        haMotionWait(h.deps),
	})
	if err != nil {
		return err
	}
	if h.decision.Failback {
		h.syncFailback(ctx, landed)
	}
	if !h.replicaChanged.IsZero() {
		h.restampReplica(ctx, landed)
	}
	return nil
}

// runFailbackOff runs an attach that skipped the HA decision with failback
// turned off before the disk lands, and fails it with a retriable error when
// the flag cannot be turned off. Once the disk has landed it turns failback
// off again when the flag reads on, because a call in another process may have
// turned it on in between from a config without the disk. That second write
// only logs a failure, because the disk is in place and that call checks its
// own write against the config once more (see reconcileVMFailback).
func (h *attachHA) runFailbackOff(ctx context.Context, attach func(context.Context) (string, error)) error {
	if err := h.lowerFailback(ctx); err != nil {
		return err
	}
	if _, err := attach(ctx); err != nil {
		return err
	}
	h.recheckFailbackOff(ctx)
	return nil
}

// lowerFailback turns failback off on the VM's HA resource before a disk that
// may not fail back lands, and returns a retriable error when it cannot.
func (h *attachHA) lowerFailback(ctx context.Context) error {
	if _, err := setVMFailback(ctx, h.narrow.Resources, h.deps.RequestDirectorUUID, h.vmid, false); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: turn off failback on the HA resource of VM %d before the attach", attachHAOp, h.vmid))
	}
	return nil
}

// recheckFailbackOff turns failback off once more after a disk that may not
// fail back has landed, when the flag reads on, and logs a write that fails.
func (h *attachHA) recheckFailbackOff(ctx context.Context) {
	changed, err := setVMFailback(ctx, h.narrow.Resources, h.deps.RequestDirectorUUID, h.vmid, false)
	logger := h.logger.With(log.Int(metadataKeyVMID, h.vmid))
	switch {
	case err != nil:
		logger.Warn(attachHAOp+": failback could not be checked after the disk landed; a later call sets it",
			log.Err(err))
	case changed:
		logger.Warn(attachHAOp + ": failback was on again when the disk landed, which a concurrent call set, " +
			"and the attach turned it off")
	}
}

// syncFailback decides failback afresh after an attach whose decision allows
// it, and sets it through the barrier's resource client (see syncVMFailback).
// The fresh decision applies every check the reconcile makes, the replicate
// tag and the replica tokens on the pin included, and it reads the VM's pin as
// the barrier left it. A write that fails is logged, and the next call that
// touches the VM sets the flag.
func (h *attachHA) syncFailback(ctx context.Context, node string) {
	if node == "" {
		node = h.node
	}
	syncVMFailback(ctx, h.deps, h.narrow.Resources, h.profile, attachHAOp, h.vmid,
		pve.VMLocation{Node: node, Found: true}, h.logger)
}

// restampReplica records on a replicate VM's pin, after the attach, the time
// the attach finished as the time its volume set changed. The barrier wrote
// the pin before the attach with the time the decision ran, and a sync must
// start after the change has landed, not only after it was decided. The
// attach does not wait for a sync and never widens the pin: a later call that
// touches the VM checks the replica. A persistent disk on ZFS pool storage
// enters a replicated VM by a reassignment that names the volume for the VM,
// so the job copies it, and the pin names the partner again only after a
// sync that started after this time covers it. A pin that changed since, or a
// write that fails, is logged, and the earlier time stays on the pin.
func (h *attachHA) restampReplica(ctx context.Context, node string) {
	if node == "" {
		node = h.node
	}
	logger := h.logger.With(log.Int(metadataKeyVMID, h.vmid), log.String("node", node))
	pin, source, partner, err := readOwnedReplicaPin(ctx, h.deps, h.vmid)
	switch {
	case err != nil:
		logger.Warn(attachHAOp+": the replicate VM's pin could not be read to record when the attach finished",
			log.Err(err))
		return
	case pin == nil || source != node || partner != "":
		logger.Info(attachHAOp + ": the replicate VM's pin is not the source-only pin on its node; the time of " +
			"the change stays as it is")
		return
	}
	stamped := *pin
	stamped.Comment = ha.WithReplicaChanged(pin.Comment, time.Now())
	if err := writeReplicaPin(ctx, h.deps, h.profile, logger, h.vmid, pin, &stamped); err != nil {
		logger.Warn(attachHAOp+": the replicate VM's pin could not record when the attach finished; it keeps the "+
			"time the attach was decided", log.Err(err))
	}
}

// attachSkipsHADecision reports whether an attach to a VM under HA needs no
// HA decision, because the VM's pin already records the disk's storage. A
// barrier an earlier attach left part way must be finished, so only a
// resource with no pending barrier skips the decision. A legacy disk in
// managed mode never skips it, because the decision is what refuses a disk
// with no stable ID there. A replicate VM's volume set is what its replica
// must cover, so every attach to it is decided, and its pin keeps the source
// alone until a sync covers the new set.
func attachSkipsHADecision(oldPin *ha.Rule, res *cluster.GetHaResourcesResponse, storage string, legacyManaged bool) bool {
	if oldPin == nil || legacyManaged || ha.NarrowPending(res.State, res.Comment) ||
		replicaPinComment(oldPin.Comment) {
		return false
	}
	recorded, ok := ha.VolumeRecord(oldPin.Comment)
	return ok && slices.Contains(recorded, storage)
}

// replicaPinComment reports whether a pin's comment carries a token of the
// replicate policy.
func replicaPinComment(comment string) bool {
	_, partner := ha.ReplicaPartner(comment)
	_, changed := ha.ReplicaChanged(comment)
	return partner || changed
}

// errAttachReaddGuarded is the refusal of an HA re-registration under the
// managed disk lifecycle. Its guard admits this Director's pin writes and
// requested-state updates (see admitLifecycleHAWrite), but no HA resource
// creation and no description write, so a re-registration there would poison
// the lifecycle.
func (h *attachHA) errAttachReaddGuarded() error {
	sid := haResourceSid(h.vmid)
	return cpierrors.Retriable("%s: the HA resource of VM %d is in the error state, and the attach of a "+
		"managed disk cannot clear it inside its lifecycle; a reboot_vm clears it, or clear it without stopping "+
		"the guest by running 'ha-manager remove %s' and then 'ha-manager add %s --state started', and retry",
		attachHAOp, h.vmid, sid, sid)
}

// reregister is the barrier's answer to a resource it finds in the error
// state. It clears the error by removing the resource and registering it
// again, with the attach between the two and the pin the attach decision gives
// written in the confinement step, so the resource is never asked for
// disabled. An order that ends without running the attach, such as one a
// concurrent call finished, returns a retriable error, so the Director retries
// the attach.
func (h *attachHA) reregister(
	ctx context.Context, st *pve.HAServiceState, between func(ctx context.Context, node string) (pve.VMLocation, error),
) (pve.VMLocation, error) {
	if _, wrapped := h.deps.PVE.(guardWrappedClient); wrapped {
		return pve.VMLocation{}, h.errAttachReaddGuarded()
	}
	readd := &attachReadd{h: h, register: true}
	var landed pve.VMLocation
	node := h.node
	if st != nil && st.Node != "" {
		node = st.Node
	}
	res, err := reregisterErroredVM(ctx, h.deps, attachHAOp, h.vmid, node, h.logger, readdOptions{
		decided: st,
		profile: h.profile,
		between: func(ctx context.Context, node string) (pve.VMLocation, error) {
			readd.ran = true
			loc, err := between(ctx, node)
			landed = loc
			return loc, err
		},
		decide:   readd.decide,
		writePin: readd.writePin,
	})
	if err != nil {
		return pve.VMLocation{}, err
	}
	if !readd.ran {
		return pve.VMLocation{}, cpierrors.Retriable("%s: the HA re-registration of VM %d ended (%s) without "+
			"running the attach; retry", attachHAOp, h.vmid, res.Outcome)
	}
	if res.Node != "" {
		landed.Node = res.Node
	}
	landed.Found = true
	return landed, nil
}

// attachReadd is the attach's part of a re-registration that runs the attach
// between the removal and the registration, or that removes the VM from HA
// before the attach. Its decision runs under the VM's claim, over the VM's
// config, pin, and record as they are there, with the new disk added, so it
// never acts on what the attach read before the claim.
type attachReadd struct {
	h *attachHA
	// register says the attach, deciding before the claim, kept the VM in HA
	// with the new disk.
	register bool
	// ran says the attach has run between the removal and the registration.
	ran bool
	// oldPin and set are the VM's strict pin and the rules the last decision
	// read, which the pin write builds on.
	oldPin *ha.Rule
	set    *ha.RuleSet
}

// decide makes the attach decision again under the claim. A decision before
// the attach ran that no longer matches the one the attach planned with, which
// kept the VM in HA or took it out, returns a retriable error before anything
// changes, so the next attach plans again. A deferring or failing decision is
// left to the order, which defers or fails the call.
func (a *attachReadd) decide(
	ctx context.Context, node string, _ *pve.HAServiceState, rec ha.ReaddRecord,
) (*ha.VMDecision, []ha.VMVolume, error) {
	const op = attachHAOp
	h := a.h
	cfg, err := h.deps.PVE.QEMU().Config(ctx, node, h.vmid)
	if err != nil {
		return nil, nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the config of VM %d for its HA decision", op, h.vmid))
	}
	set, err := readHARuleSet(ctx, h.deps.PVE.Cluster())
	if err != nil {
		return nil, nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA rules for VM %d", op, h.vmid))
	}
	pin := set.Find(naRuleNameFor(h.vmid))
	if pin != nil && haOwnershipOf(pin.Comment, h.deps.RequestDirectorUUID) == haOwnedByOther {
		return nil, nil, cpierrors.Cloud("%s: the pin %s of VM %d carries another Director's ownership marker", op,
			pin.ID, h.vmid)
	}
	var strict *ha.Rule
	if pin != nil && pin.Strict && !pin.Disabled {
		strict = pin
	}
	in := attachDecisionInput(h.deps, h.vmid, node, cfg, rec.AutoRebalance, strict, h.disk)
	if c, ok := recordedPinConstraint(h.vmid, rec); ok && strict == nil {
		// PVE pruned the pin together with the resource, and the record keeps
		// its nodes.
		in.Constraints = append(in.Constraints, c)
	}
	d, err := h.profile.DecideVM(in)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range d.Warnings {
		h.logger.Warn(op+": "+w, log.Int(metadataKeyVMID, h.vmid))
	}
	settled := !d.Transient && !d.PlaceableShortfall && d.Outcome != ha.OutcomeFail
	if !a.ran && settled && d.Registers() != a.register {
		return nil, nil, cpierrors.Retriable("%s: the HA decision for VM %d with the new disk changed since the "+
			"attach made it (%s); nothing changed, and a retry decides again", op, h.vmid, d.Reason)
	}
	a.oldPin, a.set = strict, set
	return d, in.Volumes, nil
}

// writePin writes the pin the decision gives with the new disk, recording
// storages, and verifies it.
func (a *attachReadd) writePin(ctx context.Context, d *ha.VMDecision, storages []string) (*ha.Rule, error) {
	h := a.h
	if a.set == nil {
		return nil, cpierrors.Cloud("%s: VM %d: the pin was asked for before its decision", attachHAOp, h.vmid)
	}
	pin, err := attachNewPin(d, a.oldPin, h.profile, a.set, storages, h.deps.RequestDirectorUUID)
	if err != nil {
		return nil, err
	}
	if err := h.writePinFor(ctx, d, pin); err != nil {
		return nil, err
	}
	return pin, nil
}

// leaveErrored clears the error of a resource the attach removes from HA, by
// removing the resource without ever asking for disabled, which would stop
// the guest. It reports cleared when the resource is gone, so the attach
// skips its own removal, and not cleared when the resource is not in the
// error state, which the attach's own removal then handles.
func (h *attachHA) leaveErrored(ctx context.Context) (pve.VMLocation, bool, error) {
	if h.narrow.ReadState == nil {
		return pve.VMLocation{}, false, nil
	}
	st, err := h.narrow.ReadState(ctx, h.vmid, h.node)
	if err != nil {
		return pve.VMLocation{}, false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA state of VM %d before it leaves HA", attachHAOp, h.vmid))
	}
	if st == nil || !st.Known || st.Raw != pveHAStateError {
		return pve.VMLocation{}, false, nil
	}
	if _, wrapped := h.deps.PVE.(guardWrappedClient); wrapped {
		return pve.VMLocation{}, false, h.errAttachReaddGuarded()
	}
	opts := readdOptions{decided: st, stopAfterRemoval: true, profile: h.profile}
	if h.decision != nil {
		opts.decide = (&attachReadd{h: h}).decide
	}
	node := h.node
	if st.Node != "" {
		node = st.Node
	}
	res, err := reregisterErroredVM(ctx, h.deps, attachHAOp, h.vmid, node, h.logger, opts)
	if err != nil {
		return pve.VMLocation{}, false, err
	}
	switch res.Outcome {
	case ha.ReaddRemoved, ha.ReaddWithoutHA:
		if res.Node != "" {
			node = res.Node
		}
		return pve.VMLocation{Node: node, Found: true}, true, nil
	case ha.ReaddNothing:
		return pve.VMLocation{}, false, nil
	default:
		return pve.VMLocation{}, false, cpierrors.Retriable("%s: the HA error of VM %d was cleared (%s) while the "+
			"attach removes it from HA; retry", attachHAOp, h.vmid, res.Outcome)
	}
}

// pveHAStateError is the raw CRM state of a resource in the error state.
const pveHAStateError = "error"

// preflight runs the attach's refusals that only read, before the attach
// changes anything in HA. They are the refusal of a legacy disk named for the
// VM, the snapshot guard, the refusal of a disk another VM holds, and the
// refusal of a parked disk that PVE would not let into the VM because the VM
// is replicated. The attach runs each of them again in its own order, so a
// refusal here is the same answer the attach would give, only earlier. A disk
// stranded on an unused entry, or one whose transfer is still in flight, skips
// the holder refusals, because the attach settles it first and only then knows
// its holder.
func (h *attachHA) preflight(ctx context.Context) error {
	const op = attachHAOp
	rd := h.disk
	if err := refuseOwnedLegacyAttach(op, rd, h.node, h.vmid); err != nil {
		return err
	}
	cfg := h.deps.Config
	if cfg == nil {
		return nil
	}
	if err := attachDiskSnapshotGuard(ctx, h.deps, h.vmCID, h.node, h.vmid, cfg, h.logger); err != nil {
		return err
	}
	if _, stranded, ambiguous := strandedEntry(h.deps, rd); stranded || ambiguous || rd.intent != nil {
		return nil
	}
	var holder pve.DiskHolder
	if rd.stableID != "" {
		if rd.holder == nil {
			return nil
		}
		holder = *rd.holder
	} else {
		found, err := pve.ResolveDiskHolder(ctx, h.deps.PVE, h.logger, rd.volid, parkerReadConfigFor(h.deps))
		if err != nil {
			return wrapHolderScanError(err, fmt.Sprintf("%s: resolve current holder of disk %s", op, rd.diskCID))
		}
		holder = found
	}
	if holder.Found && !holder.IsParker && holder.VMID != h.vmid {
		return foreignHolderError(ctx, h.deps, op, rd.diskCID, h.vmid, holder)
	}
	return h.refuseReplicatedAttach(ctx, holder)
}

// refuseReplicatedAttach is the replicated-VM refusal of the attach guard
// (see replicatedTargetRoute), made from the jobs and storages the HA profile
// read, so it costs no API call in the common case. A refusal here keeps the
// VM in HA with its pin, where the same refusal after a leave would have
// taken the VM out of HA for an attach that can never land. A stable-ID disk
// on a parker gets the verdict of the reassignment, and any other disk that
// is not in the VM yet is refused when its storage is node-local and cannot
// replicate, because PVE refuses that volume on a replicated VM, and the CPI
// does not exclude it from replication. Facts the profile lacks skip it, and
// the attach's own check then runs. So does an attach whose replica rebuild
// retired the VM's job after the profile was read. A removal from HA of a
// replicate VM retires the VM's own job before the attach (see
// retireLeftReplica), so for it the refusal counts only the jobs that do not
// carry this Director's marker, which the removal leaves, and it reads the
// job list for them, because the profile's jobs carry no marker. A list that
// cannot be read is retriable, and the VM stays in HA.
func (h *attachHA) refuseReplicatedAttach(ctx context.Context, holder pve.DiskHolder) error {
	rd := h.disk
	if replicaRebuildInFlight(ctx) || holder.Found && holder.VMID == h.vmid {
		return nil
	}
	profileJobs, ok := profileReplicaJobs(h.profile, h.vmid)
	if !ok || len(profileJobs) == 0 {
		return nil
	}
	storage, _, _ := strings.Cut(rd.volid, ":")
	info, ok := profileStorage(h.profile, storage)
	if !ok || storageCanReplicate(info) {
		return nil
	}
	parked := holder.Found && holder.IsParker && rd.stableID != ""
	if !parked && info.IsShared() {
		return nil
	}
	jobs := profileJobs
	if h.action == attachHALeave && h.replicaTagged {
		foreign, err := h.foreignReplicaJobs(ctx)
		if err != nil {
			return err
		}
		if len(foreign) == 0 {
			return nil
		}
		jobs = foreign
	}
	if parked {
		_, err := replicaReassignVerdict(attachHAOp, rd, holder, info, h.vmid, describeReplicaJobs(jobs))
		return err
	}
	return replicatedLocalAddRefusal(attachHAOp, rd.diskCID, h.vmCID, describeReplicaJobs(jobs), info)
}

// foreignReplicaJobs returns the IDs of the VM's replication jobs that do not
// carry this Director's marker, read from the job list. A read that fails is
// retriable.
func (h *attachHA) foreignReplicaJobs(ctx context.Context) ([]string, error) {
	all, err := replicationAPIFor(h.deps).ListJobs(ctx)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs of VM %d before it leaves HA", attachHAOp, h.vmid))
	}
	_, foreign, err := guestReplicaJobs(all, h.vmid, h.deps.RequestDirectorUUID)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the replication jobs of VM %d before it leaves HA", attachHAOp, h.vmid))
	}
	return replicaJobIDs(foreign), nil
}

// retireLeftReplica retires the replication job of a replicate VM that has
// just left HA, and removes the VM's replicate tag. Nothing registers a VM in
// HA again after a removal, so the VM has no HA failover for the replica to
// serve, and a job that stayed would keep copying the VM to the partner, keep
// a copy of its volumes there, bind its live migration to the job's target,
// and make PVE refuse every later volume whose storage cannot replicate. The
// job goes with cleanup and is never forced, so a partner that cannot clean
// up makes the attach retriable, and the next attach finds the pin the
// removal left and runs this again (see pendingAttachLeave). The VM's journal
// record is not held under the disk's lifecycle, so the retirement reaches
// the record after the lifecycle ends (see recordLeftReplica). A VM without
// the replicate tag has no job of the policy, and nothing is read.
func (h *attachHA) retireLeftReplica(ctx context.Context, node string) error {
	if !h.replicaTagged {
		return nil
	}
	profile, profileErr := h.profile, error(nil)
	if profile == nil {
		profile, profileErr = resolveHAProfile(ctx, h.deps)
	}
	r := &replicaRetire{
		deps: h.deps, logger: h.logger, api: replicationAPIFor(h.deps), caller: attachHAOp, vmid: h.vmid,
		node: node, online: replicaNodeOnline(profile, profileErr),
	}
	if err := r.run(ctx); err != nil {
		return err
	}
	h.leftReplica = &retiredReplica{job: r.retiredJob, other: r.retiredOther}
	return mutateVMTags(ctx, h.deps, node, h.vmid, attachHAOp+": remove the replicate tag", h.logger,
		func(tags []string) []string {
			return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == haReplicaTag })
		})
}

// attachUnchanged answers the barrier's Unchanged after an attach that
// returned an error. The attach changed nothing that matters to where the VM
// may run when no volume in either view of the VM's config sits on the new
// disk's storage, because the decision ran only for a storage the VM's pin did
// not record. Every read failure is returned, and the barrier then keeps the
// narrower pin.
func (h *attachHA) attachUnchanged(ctx context.Context, node string) (bool, error) {
	storage, _, ok := strings.Cut(h.disk.volid, ":")
	if !ok || storage == "" {
		return false, fmt.Errorf("disk %s names no storage", h.disk.volid)
	}
	holding, err := pve.ReadQemuHolding(ctx, h.deps.PVE, node, h.vmid)
	if err != nil {
		return false, fmt.Errorf("read the config of VM %d: %w", h.vmid, pve.WrapError(err))
	}
	for _, view := range []map[string]any{holding.Config, holding.Replaced} {
		for _, v := range pve.ConfigAccessVolumes(view, node) {
			if s, _, _ := strings.Cut(v.Volume, ":"); s == storage {
				return false, nil
			}
		}
	}
	return true, nil
}

// attachMoving answers the barrier's Moving after an attach that returned an
// error. A failed wait does not stop a PVE task, so the disk may still land
// after the attach gave up on it (see vmMoveInFlight). An error keeps the
// narrower pin.
func (h *attachHA) attachMoving(ctx context.Context, node string) (bool, error) {
	return vmMoveInFlight(ctx, h.deps, h.vmid, node)
}

// vmMoveInFlight reports whether the VM vmid on node holds a config lock or
// has a disk move task running. A config lock, such as the migrate lock a
// migration or an HA relocation holds, and a qmmove task in node's active task
// list that moves a disk from or onto the VM, such as the transfer from a
// parker, each answer true. PVE lists a task as active until it archives it,
// so a task that has just ended still answers true, and the caller then
// defers once more. The listing needs Sys.Audit on the node. A client that
// cannot list tasks, a missing privilege, and a failed read are errors.
func vmMoveInFlight(ctx context.Context, deps Deps, vmid int, node string) (bool, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return false, fmt.Errorf("read the config of VM %d: %w", vmid, pve.WrapError(err))
	}
	if lock, _ := pve.ConfigString(cfg, "lock"); strings.TrimSpace(lock) != "" {
		return true, nil
	}
	reader, ok := deps.PVE.(pve.ActiveMoveTaskReader)
	if !ok {
		return false, fmt.Errorf("the PVE client cannot list the active move tasks on node %s", node)
	}
	tasks, err := reader.ActiveMoveTasks(ctx, node)
	if err != nil {
		return false, fmt.Errorf("list the active move tasks on node %s: %w", node, err)
	}
	for _, task := range tasks {
		if pve.MoveTaskIDTouchesVM(task.ID, vmid) {
			return true, nil
		}
	}
	return false, nil
}

// writePin writes the VM's pin under the rules lock and verifies it. A nil pin
// is the barrier putting back a VM that had no pin, after an attach that
// provably changed nothing, so it removes only the pin this attach wrote (see
// removeAttachPin) and never the VM's HA resource.
func (h *attachHA) writePin(ctx context.Context, pin *ha.Rule) error {
	if pin == nil {
		return h.removeAttachPin(ctx)
	}
	return h.writePinFor(ctx, h.decision, pin)
}

// writePinFor writes pin under the rules lock and verifies it against the
// decision d.
func (h *attachHA) writePinFor(ctx context.Context, d *ha.VMDecision, pin *ha.Rule) error {
	owner := h.deps.RequestDirectorUUID
	var res *ha.ApplyResult
	err := withHARulesLock(ctx, h.deps, h.vmid, h.logger, func(ctx context.Context) error {
		writer := newHARuleWriterFor(h.deps, h.profile, h.logger, d)
		var err error
		res, err = writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			cur := set.Find(pin.ID)
			switch {
			case cur == nil:
				return []ha.RuleOp{{Kind: ha.OpCreate, Rule: *pin}}, nil
			case haOwnershipOf(cur.Comment, owner) == haOwnedByOther:
				return nil, fmt.Errorf("node-affinity: rule %q carries another Director's ownership marker; "+
					"leaving it alone", pin.ID)
			case cur.Type != ha.RuleTypeNodeAffinity:
				return nil, fmt.Errorf("node-affinity: rule %q is a %s rule, not a node-affinity pin; leaving it alone",
					pin.ID, cur.Type)
			case ha.SameRule(pin, cur):
				return nil, nil
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *pin}}, nil
		})
		return err
	})
	healAfterRepair(ctx, h.deps, res, h.logger)
	if err != nil {
		return err
	}
	stager := &ha.Stager{
		ReadRules: func(ctx context.Context) (*ha.RuleSet, error) { return readHARuleSet(ctx, h.deps.PVE.Cluster()) },
		Managed:   h.deps.Config.HAModeValue() == config.HAModeManaged,
		Logger:    h.logger,
	}
	return stager.Verify(ctx, d, pin, "")
}

// removeAttachPin deletes the node-affinity pin this attach wrote for a VM
// that had none before. It never touches the VM's HA resource, which stays
// registered and is restored by the barrier. A rule that is not this
// Director's, or not the pin this attach wrote, stays, and the error says so,
// because the barrier then keeps what is there, which over-confines the VM at
// worst.
func (h *attachHA) removeAttachPin(ctx context.Context) error {
	if h.newPin == nil {
		return fmt.Errorf("node-affinity: VM %d has no pin from this attach to remove", h.vmid)
	}
	owner := h.deps.RequestDirectorUUID
	return deleteNodeAffinityRule(ctx, h.deps, h.vmid, h.logger, func(cur *ha.Rule) error {
		switch {
		case owner == "" || haOwnershipOf(cur.Comment, owner) != haOwnedByUs:
			return fmt.Errorf("rule %q does not carry this Director's ownership marker; leaving it", cur.ID)
		case cur.Type != ha.RuleTypeNodeAffinity:
			return fmt.Errorf("rule %q is a %s rule, not a node-affinity pin; leaving it", cur.ID, cur.Type)
		case !ha.SameRule(h.newPin, cur):
			return fmt.Errorf("rule %q is not the pin this attach wrote; leaving it", cur.ID)
		}
		return nil
	})
}

// checkReach re-reads the VM's config after the attach and checks that every
// node of the new pin reaches every volume the VM now holds.
func (h *attachHA) checkReach(ctx context.Context, node string) error {
	return checkPinReach(ctx, h.deps, h.profile, h.vmid, node, h.newPin.NodeNames())
}
