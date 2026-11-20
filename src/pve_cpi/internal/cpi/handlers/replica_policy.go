package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haReplicaTag marks a VM that the replicate local policy set up a
// replication job for. The teardown and the calls that touch the VM read it to
// know that a job may exist, so a VM without it costs them no replication
// read. create_vm writes it before it creates the job, so a crash between the
// two leaves a tag and no job, never a job the teardown cannot find.
const haReplicaTag = "bosh-ha-replica"

// replicaOp prefixes the logs of the replication steps that run inside other
// handlers.
const replicaOp = "replicate"

// hasReplicaTag reports whether the PVE tags field carries haReplicaTag.
func hasReplicaTag(tags string) bool {
	return tagsContainFold(parseTagsField(tags), haReplicaTag)
}

// configHasReplicaTag reports whether a VM config's tags carry haReplicaTag.
func configHasReplicaTag(cfg map[string]any) bool {
	tags, _ := pve.ConfigString(cfg, "tags")
	return hasReplicaTag(tags)
}

// replicationAPIFor returns the replication surface of the request: the one
// Deps carries, or the one over the request's client. The managed lifecycle
// guard admits replication calls, so the wrapped client serves them as it is.
func replicationAPIFor(deps Deps) ha.ReplicationAPI {
	if deps.Replication != nil {
		return deps.Replication
	}
	return ha.NewReplicationAPI(deps.PVE)
}

// replicaJobOwned reports whether a job's comment carries this Director's
// replication marker.
func replicaJobOwned(job ha.ReplicaJobState, owner string) bool {
	marker, ok := parseReplicaJobMarker(job.Comment)
	return ok && owner != "" && marker.Owner == markerValue(owner)
}

// guestReplicaJobs splits the jobs of guest vmid into the one this Director
// owns and the others. More than one owned job is an error, because the CPI
// creates one job per VM and cannot tell which of several the pin speaks for,
// and so is a faulted row that may be the guest's (see ha.GuestJobs).
func guestReplicaJobs(
	jobs []ha.ReplicaJobState, vmid int, owner string,
) (owned *ha.ReplicaJobState, foreign []ha.ReplicaJobState, err error) {
	mine, err := ha.GuestJobs(jobs, vmid)
	if err != nil {
		return nil, nil, err
	}
	for _, j := range mine {
		if !replicaJobOwned(j, owner) {
			foreign = append(foreign, j)
			continue
		}
		if owned != nil {
			return nil, nil, &replicaDuplicateJobsError{vmid: vmid, first: owned.ID, extra: j.ID}
		}
		job := j
		owned = &job
	}
	return owned, foreign, nil
}

// replicaDuplicateJobsError is guestReplicaJobs' error for a VM with more
// than one job that carries this Director's marker. The jobs sort by job
// number, and the CPI gives a VM's job the lowest free number, so extra is
// most likely the one an operator added.
type replicaDuplicateJobsError struct {
	vmid         int
	first, extra string
}

func (e *replicaDuplicateJobsError) Error() string {
	return fmt.Sprintf("VM %d has replication jobs %s and %s that both carry this Director's marker", e.vmid,
		e.first, e.extra)
}

// replicaOtherNode returns the node that holds the copy of a job whose VM runs
// on live: the job's target, or its recorded source when PVE has not swapped
// the job's direction after a failover yet. It is empty when the job names no
// other node.
func replicaOtherNode(job ha.ReplicaJobState, live string) string {
	if job.Target != live {
		return job.Target
	}
	if job.Source != live {
		return job.Source
	}
	return ""
}

// narrowedReplicaPin is pin narrowed to node alone, with the partner token
// removed and the time of the change recorded, so the next sync must start
// after it before the pin may name a partner again.
func narrowedReplicaPin(pin *ha.Rule, node string, changedAt time.Time) *ha.Rule {
	out := *pin
	out.Nodes = map[string]int{node: pin.Nodes[node]}
	out.Resources = slices.Clone(pin.Resources)
	out.Comment = ha.WithReplicaChanged(ha.WithReplicaPartner(pin.Comment, ""), changedAt)
	out.Errors = nil
	return &out
}

// widenedReplicaPin is pin with partner added at the lowest priority the pin
// gives any node, so the partner never outranks the source, and with the
// partner token that says a verified replica lives there.
func widenedReplicaPin(pin *ha.Rule, partner string) *ha.Rule {
	out := *pin
	out.Nodes = maps.Clone(pin.Nodes)
	out.Resources = slices.Clone(pin.Resources)
	lowest, first := 0, true
	for _, p := range pin.Nodes {
		if first || p < lowest {
			lowest, first = p, false
		}
	}
	out.Nodes[partner] = lowest
	out.Comment = ha.WithReplicaPartner(pin.Comment, partner)
	out.Errors = nil
	return &out
}

// writeReplicaPin writes pin over the VM's pin under the rules lock, only
// while the pin in PVE is still expect, and verifies it. A pin that changed
// since the caller read it is left alone and the error says so, because the
// caller's evidence was gathered against the pin it read.
func writeReplicaPin(
	ctx context.Context, deps Deps, profile *ha.Profile, logger *log.Logger, vmid int, expect, pin *ha.Rule,
) error {
	_, err := writeReplicaPinWith(ctx, deps, profile, logger, vmid, expect, pin, replicaPinWrite{})
	return err
}

// replicaPinWrite is how writeReplicaPinWith writes a pin. The zero value
// writes at once and accepts no pin but expect.
type replicaPinWrite struct {
	// guard runs under the rules lock and wraps the write. It gets the write
	// and must either call it, which writes the pin, or return an error and
	// leave the pin as it is, and that error is returned as it stands. A
	// guard that returns nil without calling the write is an error too. A
	// nil guard writes at once.
	guard func(ctx context.Context, write func(context.Context) error) error
	// standIn reports whether cur, a pin in PVE that is neither expect nor
	// pin, stands in for expect. Such a pin is left as it is, nothing is
	// written, and it is returned, so the caller can decide again from it.
	// A nil standIn accepts no other pin, and the write fails as changed.
	standIn func(cur *ha.Rule) bool
	// around wraps the hold of the rules lock and the write under it, but
	// not the verification after. It must call locked once, or return an
	// error without calling it. A nil around takes the rules lock at once.
	// The widening pass takes the VM's claim here, so the claim's pool calls
	// run before and after the rules lock rather than while every other rule
	// write in the cluster waits on it.
	around func(ctx context.Context, locked func(context.Context) error) error
	// confirmLost makes a write whose answer was lost, such as one whose
	// context ended while the update was in flight, read the pin again on a
	// fresh context. A pin that is pin by then counts as written, so the
	// caller reports what is in PVE rather than a failure.
	confirmLost bool
	// beforeWrite runs under the rules lock, after the guard let the write
	// go ahead and the rule writer found the pin in PVE to be expect, and
	// right before the update of the pin goes out. An error leaves the pin as
	// it is and is returned as it stands. The rule writer plans again after a
	// write PVE refused because the rules changed, so beforeWrite may run more
	// than once and must do nothing the second time that it did the first.
	beforeWrite func(ctx context.Context) error
}

// replicaPinWritten is what writeReplicaPinWith did.
type replicaPinWritten struct {
	// wrote reports whether this call changed the pin in PVE, which is false
	// when the pin was written already.
	wrote bool
	// standIn is the pin found in place of expect that the write's standIn
	// accepted, or nil. Nothing was written when it is set.
	standIn *ha.Rule
}

// writeReplicaPinWith is writeReplicaPin with the options of how (see
// replicaPinWrite). A pin that stands in for expect is returned and not
// verified, because this call wrote nothing.
func writeReplicaPinWith(
	ctx context.Context, deps Deps, profile *ha.Profile, logger *log.Logger, vmid int, expect, pin *ha.Rule,
	how replicaPinWrite,
) (out replicaPinWritten, err error) {
	owner := deps.RequestDirectorUUID
	var res *ha.ApplyResult
	called, confirmed := false, false
	write := func(ctx context.Context) error {
		called = true
		writer := newHARuleWriterFor(deps, profile, logger, nil)
		var err error
		res, err = writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			out.standIn = nil
			cur := set.Find(pin.ID)
			switch {
			case cur == nil:
				return nil, fmt.Errorf("node-affinity: pin %q of VM %d is gone", pin.ID, vmid)
			case haOwnershipOf(cur.Comment, owner) != haOwnedByUs:
				return nil, fmt.Errorf("node-affinity: pin %q does not carry this Director's ownership marker; "+
					"leaving it alone", pin.ID)
			case ha.SameRule(cur, pin):
				// The pin is written already: an earlier pass of this write
				// landed, or another call wrote the same pin.
				return nil, nil
			case !ha.SameRule(cur, expect) && how.standIn != nil && how.standIn(cur):
				found := cur.Clone()
				out.standIn = &found
				return nil, nil
			case !ha.SameRule(cur, expect):
				return nil, fmt.Errorf("node-affinity: pin %q of VM %d changed since it was read (%s); the next "+
					"call that touches the VM settles it", pin.ID, vmid, strings.Join(ha.RuleDiff(expect, cur), ","))
			}
			if how.beforeWrite != nil {
				if err := how.beforeWrite(ctx); err != nil {
					return nil, err
				}
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *pin}}, nil
		})
		if err != nil && how.confirmLost && out.standIn == nil && replicaPinLanded(ctx, deps, pin) {
			logger.Warn(replicaOp+": the pin write's answer was lost, and the pin in PVE is the one it wrote",
				log.Int(metadataKeyVMID, vmid), log.Err(err))
			confirmed = true
			return nil
		}
		return err
	}
	locked := func(ctx context.Context) error {
		return withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
			if how.guard == nil {
				return write(ctx)
			}
			if err := how.guard(ctx, write); err != nil {
				return err
			}
			if !called {
				return fmt.Errorf("node-affinity: the guard of the pin write of VM %d returned without writing", vmid)
			}
			return nil
		})
	}
	if how.around != nil {
		err = how.around(ctx, locked)
	} else {
		err = locked(ctx)
	}
	healAfterRepair(ctx, deps, res, logger)
	if err != nil {
		return replicaPinWritten{}, err
	}
	out.wrote = confirmed || (res != nil && len(res.Wrote) > 0)
	if out.standIn != nil {
		return out, nil
	}
	stager := &ha.Stager{
		ReadRules: func(ctx context.Context) (*ha.RuleSet, error) { return readHARuleSet(ctx, deps.PVE.Cluster()) },
		Managed:   deps.Config.HAModeValue() == config.HAModeManaged,
		Logger:    logger,
	}
	return out, stager.Verify(ctx, &ha.VMDecision{VMID: vmid, SID: haResourceSid(vmid)}, pin, "")
}

// replicaPinConfirmTimeout bounds the read that checks whether a pin write
// whose answer was lost landed. It runs on a context detached from the
// write's, because that context may be what ended.
const replicaPinConfirmTimeout = 10 * time.Second

// replicaPinLanded reports whether the pin in PVE is pin, read on a context
// detached from ctx. A read that fails reports false.
func replicaPinLanded(ctx context.Context, deps Deps, pin *ha.Rule) bool {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replicaPinConfirmTimeout)
	defer cancel()
	set, err := readHARuleSet(readCtx, deps.PVE.Cluster())
	if err != nil {
		return false
	}
	cur := set.Find(pin.ID)
	return cur != nil && ha.SameRule(cur, pin)
}

// readOwnedReplicaPin reads the VM's pin and returns it when it carries this
// Director's marker and has the shape the replicate policy writes.
func readOwnedReplicaPin(ctx context.Context, deps Deps, vmid int) (pin *ha.Rule, source, partner string, err error) {
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return nil, "", "", err
	}
	pin = set.Find(naRuleNameFor(vmid))
	if pin == nil || haOwnershipOf(pin.Comment, deps.RequestDirectorUUID) != haOwnedByUs {
		return nil, "", "", nil
	}
	source, partner, ok := ha.ReplicaPinShape(pin)
	if !ok {
		return nil, "", "", nil
	}
	return pin, source, partner, nil
}

// replicaVolumesOn returns the replicated volumes of the VM as its config on
// node holds them now, and the gaps, the node-local volumes its job leaves on
// the source (see ha.ReplicaVolumes).
func replicaVolumesOn(
	ctx context.Context, deps Deps, profile *ha.Profile, node string, vmid int,
) (volids, gaps []string, err error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return nil, nil, fmt.Errorf("read the config of VM %d on node %s: %w", vmid, node, pve.WrapError(err))
	}
	volids, gaps = ha.ReplicaVolumes(vmid, haVolumesOfConfig(cfg, node, nil), profile.Storages)
	return volids, gaps, nil
}

// replicaNodeOnline answers whether node is an online member, from the
// profile's node facts. A node the profile does not list is not a member and
// so not online. A profile that could not be resolved is an error, which the
// retirement treats as a node that may be up.
func replicaNodeOnline(profile *ha.Profile, profileErr error) func(context.Context, string) (bool, error) {
	return func(_ context.Context, node string) (bool, error) {
		if profileErr != nil {
			return false, profileErr
		}
		if profile == nil || len(profile.Nodes) == 0 {
			return false, errors.New("the HA profile lists no node")
		}
		for i := range profile.Nodes {
			if profile.Nodes[i].Name == node {
				return profile.Nodes[i].Online, nil
			}
		}
		return false, nil
	}
}

// replicaWiden is one check of a replicate VM's replica, made after a change
// to its volumes or its job, that widens the pin to the partner once a sync
// is verified.
type replicaWiden struct {
	deps    Deps
	logger  *log.Logger
	api     ha.ReplicationAPI
	profile *ha.Profile
	vmid    int
	// node is where the VM runs.
	node string
	// handle is the VM's journal record, nil when the VM has none. A record
	// must carry the evidence before the pin names the partner.
	handle *aj.Handle
	now    func() time.Time
	// guard runs under the rules lock and wraps the write of the widened
	// pin, with what the check verified. It calls write to widen the pin, or
	// returns an error and leaves the pin as it is: a *replicaWidenSkip says
	// why the VM's state no longer lets the pin widen, and any other error is
	// a read that failed. A nil guard writes at once, which is the touch
	// path's check.
	guard func(ctx context.Context, proof replicaWidenProof, write func(context.Context) error) error
	// claim wraps the hold of the rules lock and the guarded write, but not
	// the verification after, the way replicaPinWrite.around does. The
	// widening pass takes the VM's claim here. A nil claim takes the rules
	// lock at once. With a claim set, a write whose answer was lost reads the
	// pin again, so the check reports what is in PVE.
	claim func(ctx context.Context, locked func(context.Context) error) error
	// wrote is set by check when its write widened the pin in PVE. A pin that
	// named the partner already, or that another call widened first, leaves
	// it false.
	wrote bool
}

// replicaWidenProof is what a check of a replica verified before it widens
// the pin: the job, the partner it copies to, and the volumes it covers.
type replicaWidenProof struct {
	jobID  string
	target string
	volids []string
}

// replicaWidenSkip is a guard's answer that the VM's state changed since the
// check read it, so the pin keeps the source alone.
type replicaWidenSkip struct{ reason string }

func (e *replicaWidenSkip) Error() string { return e.reason }

// run checks the replica once and widens the pin when the check passes. It
// reports whether the pin names the partner afterwards. A replica that is not
// yet verified, or a VM whose state does not let the policy widen, is no
// error: the pin keeps the source alone and a later call that touches the VM
// checks again. The pin stays on the source while the job is disabled or
// marked for removal, and while the VM holds a node-local volume the job does
// not copy, such as one on storage other than a ZFS pool. A persistent disk on
// a ZFS pool is no such volume once attach_disk has reassigned it into the
// VM, because the VM then owns it and the job copies it. A pin that records no time of its last change gets that
// time now, so a later sync can cover it. An error is a read or a write that
// failed.
func (w *replicaWiden) run(ctx context.Context) (bool, error) {
	widened, _, err := w.check(ctx)
	return widened, err
}

// check is run with the reason the pin names the partner or keeps the source
// alone, which a sweep reports. The reasons are logged here, as run logs
// them.
func (w *replicaWiden) check(ctx context.Context) (widened bool, reason string, err error) {
	if w.now == nil {
		w.now = time.Now
	}
	logger := w.logger.With(log.Int(metadataKeyVMID, w.vmid), log.String("node", w.node))
	owner := w.deps.RequestDirectorUUID
	pin, source, partner, err := readOwnedReplicaPin(ctx, w.deps, w.vmid)
	switch {
	case err != nil:
		return false, "", fmt.Errorf("read the pin of VM %d: %w", w.vmid, err)
	case pin == nil:
		logger.Info(replicaOp + ": the VM has no pin of this Director in the replicate shape; nothing to widen")
		return false, "the VM has no pin of this Director in the replicate shape", nil
	}
	if _, rebuilding := ha.ReplicaRebuild(pin.Comment); rebuilding {
		logger.Info(replicaOp + ": the VM's pin carries the replica rebuild token, so its replica is being " +
			"rebuilt; the pin keeps the source alone")
		return false, "the pin carries the replica rebuild token, so the VM's replica is being rebuilt", nil
	}
	switch {
	case partner != "":
		return true, "the pin names the partner " + partner + " already", nil
	case source != w.node:
		logger.Info(replicaOp+": the VM runs away from its pinned node; the pin stays as it is",
			log.String("pinned", source))
		return false, "the VM runs away from its pinned node " + source, nil
	}
	changedAt, ok := ha.ReplicaChanged(pin.Comment)
	if !ok {
		stamped := *pin
		stamped.Comment = ha.WithReplicaChanged(pin.Comment, w.now())
		if err := writeReplicaPin(ctx, w.deps, w.profile, logger, w.vmid, pin, &stamped); err != nil {
			return false, "", fmt.Errorf("record the time of the last change on the pin of VM %d: %w", w.vmid, err)
		}
		logger.Info(replicaOp + ": the VM's pin recorded no time of its last change, and records now as that " +
			"time; a sync that follows it can widen the pin")
		return false, "the pin recorded no time of its last change and records one now, so a sync that follows " +
			"it can widen the pin", nil
	}
	jobs, err := w.api.ListJobs(ctx)
	if err != nil {
		return false, "", fmt.Errorf("list the replication jobs: %w", err)
	}
	job, _, err := guestReplicaJobs(jobs, w.vmid, owner)
	if err != nil {
		return false, "", err
	}
	if job == nil {
		logger.Info(replicaOp + ": the VM has no replication job of this Director; the pin keeps the source alone")
		return false, "the VM has no replication job of this Director", nil
	}
	if job.Disabled || job.RemoveJob != "" {
		logger.Info(replicaOp+": the VM's replication job is disabled or marked for removal; the pin keeps the "+
			"source alone", log.String("job", job.ID), log.String("remove_job", job.RemoveJob))
		return false, "the VM's replication job " + job.ID + " is disabled or marked for removal", nil
	}
	target := replicaOtherNode(*job, w.node)
	if target == "" {
		return false, "the VM's replication job " + job.ID + " names no node other than " + w.node, nil
	}
	synced, held, err := w.verifySync(ctx, *job, target, changedAt, logger)
	if err != nil {
		return false, "", err
	}
	if synced == nil {
		held.log(logger, "the pin keeps the source alone")
		return false, held.reason, nil
	}
	var guard func(context.Context, func(context.Context) error) error
	if w.guard != nil {
		proof := replicaWidenProof{jobID: job.ID, target: target, volids: synced.volids}
		guard = func(ctx context.Context, write func(context.Context) error) error {
			return w.guard(ctx, proof, write)
		}
	}
	written, err := writeReplicaPinWith(ctx, w.deps, w.profile, logger, w.vmid, pin, widenedReplicaPin(pin, target),
		replicaPinWrite{guard: guard, around: w.claim, confirmLost: w.claim != nil,
			beforeWrite: w.failbackOffBeforePartner})
	w.wrote = written.wrote
	if skip := (*replicaWidenSkip)(nil); errors.As(err, &skip) {
		logger.Info(replicaOp+": the VM's state changed since its replica was checked; the pin keeps the source "+
			"alone", log.String("reason", skip.reason))
		return false, skip.reason, nil
	}
	if err != nil {
		return false, "", err
	}
	if !w.wrote {
		return true, "another call widened the pin to the partner " + target + " first", nil
	}
	logger.Info(replicaOp+": a verified sync covers the VM's volumes; the pin names the partner now",
		log.String("job", job.ID), log.String("partner", target),
		log.String("last_sync", synced.lastSync.Format(time.RFC3339)))
	return true, fmt.Sprintf("a sync of job %s at %s covers the VM's volumes", job.ID,
		synced.lastSync.UTC().Format(time.RFC3339)), nil
}

// failbackOffBeforePartner turns failback off on the VM's HA resource right
// before the pin names a replication partner, when it reads on. A pin over the
// source and its partner is what turns failback on into a loss of writes: after
// a failover, the old source's return would pull the VM back and run the
// reversed job, whose first run receives with zfs recv -F. The reconcile only
// logs a failback write that fails, so the widening checks the flag itself and
// fails closed: a write that fails returns a retriable error, and the pin keeps
// the source alone until a later call. It runs as the pin write's beforeWrite,
// so it writes only once the guard has let the widening go ahead and the rule
// writer has found the pin the check read, and a widening that is skipped
// writes nothing to the HA resource. It writes only while the flag reads on,
// so a second plan of the same write only reads.
func (w *replicaWiden) failbackOffBeforePartner(ctx context.Context) error {
	if _, err := clearVMFailback(ctx, w.deps.PVE.Cluster(), w.deps.RequestDirectorUUID, w.vmid); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("%s: turn off failback on the HA "+
			"resource of VM %d before its pin names the replication partner", replicaOp, w.vmid))
	}
	return nil
}

// replicaHeld says why a check of a replica did not verify it.
type replicaHeld struct {
	reason string
	// warn marks a reason an operator must act on, such as a volume the job
	// cannot copy.
	warn bool
	// logged marks a reason that was logged where it was decided, which log
	// then leaves alone.
	logged bool
}

// log logs the reason with the consequence the caller names.
func (h replicaHeld) log(logger *log.Logger, consequence string) {
	switch {
	case h.reason == "", h.logged:
	case h.warn:
		logger.Warn(replicaOp + ": " + h.reason + "; " + consequence)
	default:
		logger.Info(replicaOp + ": " + h.reason + "; " + consequence)
	}
}

// replicaSynced is a sync that verifySync found to cover the VM's volumes.
type replicaSynced struct {
	lastSync time.Time
	// volids are the volumes the sync covers.
	volids []string
}

// verifySync checks once that a sync of job from w.node to target covers the
// volumes the VM holds on w.node, and records the verified sync in the VM's
// journal record. A VM that holds a node-local volume the job does not copy,
// or no volume the job copies, is held, and so is a sync that the journal
// record or the sync gate does not let count. An error is a read or a write
// that failed.
func (w *replicaWiden) verifySync(
	ctx context.Context, job ha.ReplicaJobState, target string, changedAt time.Time, logger *log.Logger,
) (*replicaSynced, replicaHeld, error) {
	volids, gaps, err := replicaVolumesOn(ctx, w.deps, w.profile, w.node, w.vmid)
	if err != nil {
		return nil, replicaHeld{}, err
	}
	if len(gaps) > 0 {
		return nil, replicaHeld{warn: true, reason: fmt.Sprintf("the VM holds node-local volumes its replication "+
			"job %s does not copy (%s), so node %s would hold no copy of them", job.ID, strings.Join(gaps, "; "),
			target)}, nil
	}
	if len(volids) == 0 {
		return nil, replicaHeld{warn: true,
			reason: "the VM holds no volume on node-local ZFS storage that the profile knows"}, nil
	}
	evidence, why, err := w.journalEvidence(ctx, job, target, volids, changedAt, logger)
	if err != nil {
		return nil, replicaHeld{}, err
	}
	if evidence == nil {
		return nil, replicaHeld{reason: "the journal record does not let the pin widen yet: " + why, logged: true}, nil
	}
	verdict, err := ha.CheckReplicaSync(ctx, w.api, ha.ReplicaSyncCheck{
		JobID: job.ID, Source: w.node, Target: target, VolIDs: volids, ChangedAt: evidence.changedAt,
	})
	if err != nil {
		return nil, replicaHeld{}, err
	}
	if !verdict.Verified {
		return nil, replicaHeld{reason: fmt.Sprintf("the replica on node %s is not verified yet (job %s: %s)",
			target, job.ID, verdict.Reason)}, nil
	}
	if w.handle != nil {
		if err := w.handle.RecordReplicaSync(aj.ReplicaSync{
			JobID: job.ID, JobGeneration: evidence.jobGeneration, Source: w.node, Target: target,
			VolumeSetGeneration: evidence.setGeneration, LastSync: verdict.Status.LastSync, VerifiedAt: w.now(),
		}); err != nil {
			return nil, replicaHeld{}, fmt.Errorf("record the verified sync of job %s: %w", job.ID, err)
		}
	}
	return &replicaSynced{lastSync: verdict.Status.LastSync, volids: volids}, replicaHeld{}, nil
}

// replicaEvidence is what the journal binds a sync to.
type replicaEvidence struct {
	changedAt     time.Time
	jobGeneration uint64
	setGeneration uint64
}

// journalEvidence returns the generations a sync must name, and the time of
// change it must follow. A VM with no journal record needs only the pin's
// time of change. A record must name the job in this direction, with the
// marker's generation, no generation in flight, and the volume set the VM
// holds now; a set that differs is recorded, before the sync check, as the
// set changed at the pin's time of change. A record that names the job in the
// other direction, after a failover PVE has swapped the job for, is moved to
// this direction first (see adoptSwappedReplicaJob). nil with no error means
// the record does not let the pin widen now, which is logged, and why says
// what the record lacks.
func (w *replicaWiden) journalEvidence(
	ctx context.Context, job ha.ReplicaJobState, target string, volids []string, changedAt time.Time,
	logger *log.Logger,
) (evidence *replicaEvidence, why string, err error) {
	if w.handle == nil {
		return &replicaEvidence{changedAt: changedAt}, "", nil
	}
	refuse := func(why string) (*replicaEvidence, string, error) {
		logger.Info(replicaOp+": the journal record does not let the pin widen yet: "+why, log.String("job", job.ID))
		return nil, why, nil
	}
	adopted, why, err := w.adoptSwappedReplicaJob(ctx, job, target, logger)
	switch {
	case err != nil:
		return nil, "", err
	case why != "":
		return refuse(why)
	}
	job = adopted
	rep := w.handle.Record().Replication
	marker, ok := parseReplicaJobMarker(job.Comment)
	if !ok || marker.Generation != rep.Job.Generation {
		return refuse(fmt.Sprintf("the job's marker does not carry generation %d", rep.Job.Generation))
	}
	if _, pending := rep.PendingVolumeSet(); pending {
		return refuse("a volume change is pending")
	}
	set, ok := rep.CommittedVolumeSet()
	if !ok {
		return refuse("it records no volume set")
	}
	if !slices.Equal(set.VolIDs, normalizedVolIDs(volids)) {
		if set.ChangedAt.After(changedAt) {
			changedAt = set.ChangedAt
		}
		gen, err := w.handle.PrepareReplicaVolumeSet(volids, changedAt, nil)
		if err != nil {
			return nil, "", fmt.Errorf("record the VM's volume set: %w", err)
		}
		if err := w.handle.CommitReplicaVolumeSet(gen); err != nil {
			return nil, "", fmt.Errorf("commit the VM's volume set: %w", err)
		}
		rep = w.handle.Record().Replication
		if set, ok = rep.CommittedVolumeSet(); !ok || set.Generation != gen {
			return nil, "", fmt.Errorf("the journal does not show volume set %d as committed", gen)
		}
	}
	if set.ChangedAt.After(changedAt) {
		changedAt = set.ChangedAt
	}
	return &replicaEvidence{changedAt: changedAt, jobGeneration: rep.Job.Generation, setGeneration: set.Generation},
		"", nil
}

// adoptSwappedReplicaJob checks that the VM's journal record names job as it
// runs now, from w.node to target, and returns the job as PVE then lists it.
// A record that names the same job from target to w.node is the state after a
// failover: PVE swapped the job's direction when HA moved the VM, and the CPI
// narrowed the pin to w.node. The record moves to the new direction under the
// next job generation, which is recorded as pending, written into the job's
// marker, read back, and confirmed, so a crash at any step resumes with the
// same generation. The old direction is accepted only while PVE's job still
// carries the record's generation, or the pending one an earlier attempt
// wrote, which proves the job is the one the record names. why is non-empty
// when the record does not let the pin widen now.
func (w *replicaWiden) adoptSwappedReplicaJob(
	ctx context.Context, job ha.ReplicaJobState, target string, logger *log.Logger,
) (ha.ReplicaJobState, string, error) {
	rep := w.handle.Record().Replication
	if rep == nil || rep.Job == nil || rep.Retired {
		return job, "it records no live replication job", nil
	}
	forward := func(j *aj.ReplicaJob) bool {
		return j != nil && j.ID == job.ID && j.Source == w.node && j.Target == target
	}
	reversed := func(j *aj.ReplicaJob) bool {
		return j != nil && j.ID == job.ID && j.Source == target && j.Target == w.node
	}
	switch {
	case rep.PendingJob == nil && forward(rep.Job):
		return job, "", nil
	case rep.PendingJob != nil && (!forward(rep.PendingJob) || !reversed(rep.Job)):
		return job, "a job generation is pending", nil
	case rep.PendingJob == nil && !reversed(rep.Job):
		return job, fmt.Sprintf("it records job %s from %s to %s", rep.Job.ID, rep.Job.Source, rep.Job.Target), nil
	case job.Target != target:
		// PVE has not swapped the job yet, so it does not run from w.node.
		return job, fmt.Sprintf("PVE still runs job %s toward node %s", job.ID, job.Target), nil
	}
	marker, ok := parseReplicaJobMarker(job.Comment)
	pendingGen := uint64(0)
	if rep.PendingJob != nil {
		pendingGen = rep.PendingJob.Generation
	}
	if !ok || marker.Owner != markerValue(rep.Owner) ||
		(marker.Generation != rep.Job.Generation && marker.Generation != pendingGen) {
		return job, fmt.Sprintf("the job's marker carries neither generation %d nor a pending one",
			rep.Job.Generation), nil
	}
	gen, err := w.handle.PrepareReplicationJob(aj.ReplicaJob{
		ID: rep.Job.ID, Guest: rep.Job.Guest, JobNum: rep.Job.JobNum, Source: w.node, Target: target,
	})
	if err != nil {
		return job, "", fmt.Errorf("record the new direction of replication job %s: %w", job.ID, err)
	}
	comment := renderReplicaJobMarker(rep.Owner, gen)
	// The write also runs when PVE encoded the comment again, so the job
	// carries the canonical marker for an older CPI and for operators.
	if marker.Generation != gen || marker.Encoded {
		if err := w.api.SetComment(ctx, job.ID, comment); err != nil {
			return job, "", fmt.Errorf("write generation %d into the marker of replication job %s: %w", gen, job.ID,
				pve.WrapError(err))
		}
	}
	jobs, err := w.api.ListJobs(ctx)
	if err != nil {
		return job, "", fmt.Errorf("read replication job %s back after its marker write: %w", job.ID, err)
	}
	owned, _, err := guestReplicaJobs(jobs, w.vmid, rep.Owner)
	if err != nil {
		return job, "", err
	}
	if owned == nil || owned.ID != job.ID || owned.Target != target {
		return job, "", fmt.Errorf("replication job %s is not listed from node %s to node %s after its marker write",
			job.ID, w.node, target)
	}
	if m, ok := parseReplicaJobMarker(owned.Comment); !ok || m.Generation != gen {
		return job, "", fmt.Errorf("replication job %s does not carry generation %d after its marker write", job.ID,
			gen)
	}
	if err := w.handle.ConfirmReplicationJob(gen); err != nil {
		return job, "", fmt.Errorf("confirm the new direction of replication job %s: %w", job.ID, err)
	}
	logger.Info(replicaOp+": the journal record follows the replication job's new direction after a failover",
		log.String("job", job.ID), log.String("source", w.node), log.String("target", target),
		log.Any("generation", gen))
	return *owned, "", nil
}

// normalizedVolIDs is volids sorted with repeats removed, the form the
// journal keeps.
func normalizedVolIDs(volids []string) []string {
	out := slices.Clone(volids)
	slices.Sort(out)
	return slices.Compact(out)
}

// replicaRetire retires the replication jobs this Director owns for a VM.
type replicaRetire struct {
	deps    Deps
	logger  *log.Logger
	api     ha.ReplicationAPI
	caller  string
	vmid    int
	node    string
	volids  []string
	handle  *aj.Handle
	online  func(context.Context, string) (bool, error)
	destroy bool
	now     func() time.Time

	// retiredJob and retiredOther name, once run returns nil, the job it
	// removed and the node that held its copy. Both are empty when the VM had
	// no job of this Director.
	retiredJob, retiredOther string
}

// run retires every job of the VM that carries this Director's marker. A job
// another Director or an operator owns stays, and is logged: the destroy's
// purge drops its config and leaves its copies alone. The journal records the
// retirement first, and a forced removal records its orphan copy before the
// force. A job is dropped with force=1 only when the VM is being destroyed;
// otherwise a target that cannot clean up makes the call retry. Every failure
// is retriable, because the VM stays as it was.
func (r *replicaRetire) run(ctx context.Context) error {
	if r.now == nil {
		r.now = time.Now
	}
	logger := r.logger.With(log.Int(metadataKeyVMID, r.vmid), log.String("node", r.node))
	jobs, err := r.api.ListJobs(ctx)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs of VM %d before it is retired", r.caller, r.vmid))
	}
	owned, foreign, err := guestReplicaJobs(jobs, r.vmid, r.deps.RequestDirectorUUID)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, r.caller+": retire the replication job")
	}
	for _, j := range foreign {
		logger.Warn(r.caller+": the VM has a replication job that does not carry this Director's marker; the CPI "+
			"leaves it", log.String("job", j.ID), log.String("target", j.Target))
	}
	if owned == nil {
		// The job is gone already: an earlier attempt removed it, or someone
		// else did. A record that still names a live job is marked retired,
		// so the audit never counts a copy that no job keeps current.
		if rep := r.replication(); rep != nil && !rep.Retired {
			if err := r.handle.RetireReplicationJob(); err != nil {
				return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("%s: record that VM %d has no replication job", r.caller, r.vmid))
			}
		}
		return nil
	}
	if r.handle != nil {
		if rep := r.handle.Record().Replication; rep != nil && !rep.Retired {
			if err := r.handle.RetireReplicationJob(); err != nil {
				return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("%s: record the retirement of replication job %s", r.caller, owned.ID))
			}
		}
	}
	volids := r.volids
	if len(volids) == 0 && r.handle != nil {
		if rep := r.handle.Record().Replication; rep != nil {
			if set, ok := rep.LatestVolumeSet(); ok {
				volids = set.VolIDs
			}
		}
	}
	other := replicaOtherNode(*owned, r.node)
	in := ha.ReplicaRetirement{
		JobID: owned.ID, LiveNode: r.node, VolIDs: volids, Online: r.online, AllowForce: r.destroy,
		Budget: haMotionWait(r.deps), Now: r.now, Logger: logger,
	}
	if r.handle != nil {
		in.RecordOrphan = func(_ context.Context, node string, volids []string) error {
			if len(volids) == 0 {
				return fmt.Errorf("the journal record names no volume of job %s to record as an orphan", owned.ID)
			}
			return r.handle.RecordReplicaCopy(aj.ReplicaCopy{Kind: aj.ReplicaCopyOrphan, Node: node,
				JobID: owned.ID, VolIDs: volids, RecordedAt: r.now()})
		}
	}
	out, err := ha.RetireReplicaJob(ctx, r.api, in)
	if err != nil {
		msg := fmt.Sprintf("%s: retire replication job %s of VM %d", r.caller, owned.ID, r.vmid)
		if errors.Is(err, ha.ErrReplicaRetireIncomplete) && other != "" {
			// Only an operator can tell a partner that is down for a while
			// from one that is gone, so the CPI never forces this removal,
			// and the error says how to drop the job when the node is gone.
			msg += fmt.Sprintf(". The retry goes on once the removal can clean up the copy on node %s. If that "+
				"node will not come back, drop the job without its cleanup with 'pvesr delete %s --force 1' on "+
				"node %s; the copy on %s then stays there until it is removed by hand", other, owned.ID, r.node,
				other)
		}
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, msg)
	}
	r.retiredJob, r.retiredOther = owned.ID, other
	if r.handle != nil && other != "" {
		if err := r.handle.ReleaseReplicaCopy(aj.ReplicaCopySuperseded, other, owned.ID); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: release the superseded copy of job %s on node %s", r.caller, owned.ID, other))
		}
	}
	if out.Forced {
		logger.Warn(r.caller+": the replication job was dropped without cleanup; its copy stays on the node as an "+
			"orphan", log.String("job", owned.ID), log.String("orphan_node", out.Orphan),
			log.String("volumes", strings.Join(volids, ",")))
		return nil
	}
	logger.Info(r.caller+": retired the VM's replication job", log.String("job", owned.ID))
	return nil
}

// retiredReplica is a replication job a call retired without the VM's
// journal record, which the record learns of afterwards.
type retiredReplica struct {
	// job is the ID of the job removed, empty when the VM had none left.
	job string
	// other is the node that held the job's copy.
	other string
}

// recordLeftReplica records, in the VM's journal record, the retirement of
// the replication job that the VM's removal from HA made under a disk's
// lifecycle. The VM's record is never acquired while a disk's record is
// held, because create_vm and delete_vm hold the VM's record while they
// acquire a disk's, and the other order could deadlock against them. So the
// record learns of the retirement once the disk's lifecycle has ended, and
// until then it may still name a job that is gone, which overcharges the
// partner's capacity and leaves a failover plan the audit refuses on the live
// job list. A record that is already retired, or a VM with no record, needs
// nothing. A failure is logged, because the job is gone and delete_vm's
// retirement marks the record when it finds no job.
func recordLeftReplica(ctx context.Context, deps Deps, logger *log.Logger, vmCID string, vmid int,
	left *retiredReplica,
) {
	if left == nil {
		return
	}
	err := withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		if handle == nil || handle.Record().Replication == nil {
			return nil
		}
		if !handle.Record().Replication.Retired {
			if err := handle.RetireReplicationJob(); err != nil {
				return err
			}
		}
		if left.job == "" || left.other == "" {
			return nil
		}
		return handle.ReleaseReplicaCopy(aj.ReplicaCopySuperseded, left.other, left.job)
	})
	if err != nil {
		logger.Warn(attachHAOp+": the VM's journal record could not record the retirement of its replication job "+
			"after it left HA; delete_vm records it", log.Int(metadataKeyVMID, vmid), log.String("job", left.job),
			log.Err(err))
	}
}

// replication returns the replication of the VM's journal record, nil when
// the VM has no record or the record is not replicated.
func (r *replicaRetire) replication() *aj.Replication {
	if r.handle == nil {
		return nil
	}
	return r.handle.Record().Replication
}

// retireTornDownReplica is the teardown's retirement of a replicate VM's job,
// which runs after the ownership check and before the destroy. The destroy's
// purge would drop the job's config without cleanup, so the CPI removes the
// job first, which lets PVE remove the partner's copy, and records the copy
// a dead partner keeps. A VM without the replicate tag has no job of the
// policy, and nothing is read.
func retireTornDownReplica(
	ctx context.Context, deps Deps, logger *log.Logger, caller string, vmid int, node string, cfg map[string]any,
	handle *aj.Handle,
) error {
	if !configHasReplicaTag(cfg) && (handle == nil || handle.Record().Replication == nil) {
		return nil
	}
	if deps.RequestDirectorUUID == "" {
		return cpierrors.Retriable("%s: VM %d carries a replication job, and the request has no director_uuid to "+
			"prove the job is this Director's; the destroy waits", caller, vmid)
	}
	profile, profileErr := resolveHAProfile(ctx, deps)
	volids, err := tornDownReplicaVolumes(ctx, deps, profile, profileErr, vmid, cfg, node)
	if err != nil {
		logger.Warn(caller+": the replicated volumes of the VM could not be listed; the journal record names "+
			"them if it holds a volume set", log.Int(metadataKeyVMID, vmid), log.Err(err))
	}
	r := &replicaRetire{
		deps: deps, logger: logger, api: replicationAPIFor(deps), caller: caller, vmid: vmid, node: node,
		volids: volids, handle: handle, online: replicaNodeOnline(profile, profileErr), destroy: true,
	}
	return r.run(ctx)
}

// tornDownReplicaVolumes returns the volumes of a VM being torn down that its
// replication job copies, which a forced removal leaves on the other node. The
// storages come from the HA profile, and from the storage index when the
// profile holds none, as in mode off or a profile resolved in part.
func tornDownReplicaVolumes(
	ctx context.Context, deps Deps, profile *ha.Profile, profileErr error, vmid int, cfg map[string]any, node string,
) ([]string, error) {
	var storages []pve.StorageInfo
	if profileErr == nil && profile != nil && profile.StoragesErr == "" {
		storages = profile.Storages
	}
	if len(storages) == 0 {
		read, err := readStorageIndex(ctx, deps)
		if err != nil {
			return nil, err
		}
		storages = read
	}
	volids, _ := ha.ReplicaVolumes(vmid, haVolumesOfConfig(cfg, node, nil), storages)
	return volids, nil
}

// setUpCreatedReplica runs the replicate policy's steps of create_vm once the
// VM runs under PVE HA with its source-only pin. It tags the VM, creates the
// replication job with the configured schedule and rate and this Director's
// marker, records the job's start on the pin, and asks the job to run now. It
// does not wait for that first sync: a sync counts only when it starts after
// the change by more than the clock-skew tolerance, so the pin widens to the
// partner on a later call that touches the VM, once a sync is verified. A VM
// with a journal record gets its replication recorded before each PVE write.
// A read or write that fails transiently, or for a reason it cannot classify,
// fails the call with a retriable error in every mode, so the Director
// retries and the rollback's teardown retires a job that was created. A
// proven refusal, such as a VM with no volume to copy, fails the call in
// managed; in auto it leaves the VM under HA pinned to its node with a
// warning, and no later call creates the job.
func setUpCreatedReplica(
	ctx context.Context, deps Deps, logger *log.Logger, node string, vmid int, d *ha.VMDecision,
	profile *ha.Profile, handle *aj.Handle,
) error {
	return setUpReplica(ctx, deps, logger, "create_vm", node, vmid, d, profile, handle)
}

// setUpReplica is setUpCreatedReplica for the call op, which names it in
// errors and logs. attach_disk sets up again, through it, the job its replica
// rebuild removed (see attachReplicaRebuild).
func setUpReplica(
	ctx context.Context, deps Deps, logger *log.Logger, op, node string, vmid int, d *ha.VMDecision,
	profile *ha.Profile, handle *aj.Handle,
) error {
	if d == nil || d.Policy != ha.PolicyReplicate || d.Partner == "" {
		return nil
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("partner", d.Partner))
	managed := deps.Config.HAModeValue() == config.HAModeManaged
	degrade := func(what string, err error) error {
		switch {
		case !replicaRefusalProven(err):
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": "+what)
		case managed:
			return err
		}
		logger.Warn(op+": "+what+"; the VM runs under PVE HA pinned to its node without a replica",
			log.Err(err))
		return nil
	}
	if profile == nil {
		return degrade("the HA profile is not known", cpierrors.Retriable("%s: the HA profile of VM %d "+
			"is not known, so its replication job cannot be set up", op, vmid))
	}
	if err := mutateVMTags(ctx, deps, node, vmid, op+": tag the VM as replicated", logger,
		func(tags []string) []string {
			if tagsContainFold(tags, haReplicaTag) {
				return tags
			}
			return append(slices.Clone(tags), haReplicaTag)
		}); err != nil {
		return degrade("the bosh-ha-replica tag could not be written", err)
	}
	volids, gaps, err := replicaVolumesOn(ctx, deps, profile, node, vmid)
	if err != nil {
		return degrade("the VM's volumes could not be read", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			op+": read the replicated volumes"))
	}
	switch {
	case len(gaps) > 0:
		return degrade("the VM holds node-local volumes replication does not copy", cpierrors.Cloud(op+": "+
			"VM %d holds node-local volumes its replication job would not copy: %s", vmid, strings.Join(gaps, "; ")))
	case len(volids) == 0:
		return degrade("the VM holds no volume on node-local ZFS storage", cpierrors.Cloud(op+": VM %d "+
			"holds no volume for its replication job to copy", vmid))
	}
	changedAt := time.Now()
	api := replicationAPIFor(deps)
	job, err := ensureCreatedReplicaJob(ctx, deps, api, op, vmid, node, d.Partner, volids, changedAt, handle)
	if err != nil {
		return degrade("the replication job could not be created", err)
	}
	pin, source, partner, err := readOwnedReplicaPin(ctx, deps, vmid)
	switch {
	case err != nil:
		return degrade("the pin could not be read to record the job's start", err)
	case pin == nil || source != node:
		return degrade("the VM's pin is not the source-only pin", cpierrors.Cloud(op+": the pin of VM %d "+
			"does not name node %s alone", vmid, node))
	case partner == "":
		if _, recorded := ha.ReplicaChanged(pin.Comment); !recorded {
			stamped := *pin
			stamped.Comment = ha.WithReplicaChanged(pin.Comment, changedAt)
			if err := writeReplicaPin(ctx, deps, profile, logger, vmid, pin, &stamped); err != nil {
				// A later call that touches the VM records the time on a pin
				// that has none, so the job stands.
				logger.Warn(op+": the pin could not record the job's start; the next call that touches the "+
					"VM records it", log.Err(err))
			}
		}
	}
	if err := api.ScheduleNow(ctx, node, job); err != nil {
		logger.Info(op+": the request to run the replication job now failed; its schedule runs it",
			log.String("job", job), log.Err(err))
	}
	logger.Info(op+": the VM's replication job is configured; the pin names the source alone until a call "+
		"that touches the VM sees a verified sync", log.String("job", job))
	return nil
}

// replicaRefusalProven reports whether err is a refusal that rests on proven
// facts, which a retry would meet again. A retriable error, and an error that
// carries no CPI type, are not.
func replicaRefusalProven(err error) bool {
	var typed *cpierrors.Error
	if err == nil || !errors.As(err, &typed) {
		return false
	}
	return !typed.OkToRetry() && !cpierrors.IsType(err, cpierrors.TypeRetriableCloud)
}

// ensureCreatedReplicaJob creates the VM's replication job, or finds the one
// an earlier attempt of the same create made, and returns its ID. A job of
// the guest that this Director does not own is a refusal, because the VM
// cannot have two jobs to one partner and the CPI may not take the other one
// over. A job of this Director that PVE has marked for removal is not kept,
// because pvesr deletes it on its next run, and the call returns a retriable
// error until it is gone. The journal records the job's generation before
// the create and confirms it after. A job that is kept while the journal
// moves to a new generation, as after a retirement the journal recorded and
// PVE never carried out, gets that generation written into its marker and
// read back before the journal confirms it, so the marker and the record
// never disagree.
func ensureCreatedReplicaJob(
	ctx context.Context, deps Deps, api ha.ReplicationAPI, op string, vmid int, node, partner string, volids []string,
	changedAt time.Time, handle *aj.Handle,
) (string, error) {
	owner := deps.RequestDirectorUUID
	if owner == "" {
		return "", cpierrors.Cloud("%s: the request carries no director_uuid to mark the replication job "+
			"of VM %d with", op, vmid)
	}
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return "", cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs before VM %d gets one", op, vmid))
	}
	owned, foreign, err := guestReplicaJobs(jobs, vmid, owner)
	if err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": set up the replication job")
	}
	if len(foreign) > 0 {
		return "", cpierrors.Cloud("%s: VM %d already has replication job %s, which does not carry this "+
			"Director's marker", op, vmid, foreign[0].ID)
	}
	if owned != nil && owned.RemoveJob != "" {
		return "", cpierrors.Retriable("%s: replication job %s of VM %d is marked for removal, and pvesr has not "+
			"removed it yet; the job is set up again once it is gone", op, owned.ID, vmid)
	}
	id := ha.ReplicaJobID(vmid, ha.NextJobNum(jobs, vmid))
	if owned != nil {
		id = owned.ID
	}
	gen, err := prepareCreatedReplicaJob(handle, owner, id, vmid, node, partner, volids, changedAt)
	if err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: record replication job %s in the journal", op, id))
	}
	if gen == 0 {
		gen = 1
		if owned != nil {
			if m, ok := parseReplicaJobMarker(owned.Comment); ok {
				gen = m.Generation
			}
		}
	}
	if owned == nil {
		spec := ha.ReplicaJobSpec{
			ID: id, Target: partner, Schedule: deps.Config.HAReplicationScheduleValue(),
			RateMbps: float64(deps.Config.HAReplicationRateMbpsValue()), Comment: renderReplicaJobMarker(owner, gen),
		}
		if cerr := api.CreateJob(ctx, spec); cerr != nil {
			if err := settleFailedReplicaCreate(ctx, api, op, handle, id, owner, gen, cerr); err != nil {
				return "", err
			}
		}
	} else if handle != nil {
		if err := markKeptReplicaJob(ctx, api, op, *owned, vmid, owner, gen); err != nil {
			return "", err
		}
	}
	if handle != nil {
		if err := handle.ConfirmReplicationJob(gen); err != nil {
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: confirm replication job %s in the journal", op, id))
		}
	}
	return id, nil
}

// markKeptReplicaJob writes generation gen into the marker of job, a job of
// this Director that the setup keeps, when the marker carries another one or
// PVE encoded the comment again, and reads the job back to prove the write.
// The journal confirms gen only after this, so the widening's check that the
// marker carries the record's generation holds (see journalEvidence). Every
// failure is retriable, and the journal keeps gen pending, so the retry
// writes the same marker. The read back accepts a marker PVE encoded again
// meanwhile, because the CPI reads such a marker as its own.
func markKeptReplicaJob(
	ctx context.Context, api ha.ReplicationAPI, op string, job ha.ReplicaJobState, vmid int, owner string, gen uint64,
) error {
	if m, ok := parseReplicaJobMarker(job.Comment); ok && m.Generation == gen && !m.Encoded {
		return nil
	}
	if err := api.SetComment(ctx, job.ID, renderReplicaJobMarker(owner, gen)); err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: write generation %d into the marker of replication job %s", op, gen, job.ID))
	}
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read replication job %s back after its marker write", op, job.ID))
	}
	owned, _, err := guestReplicaJobs(jobs, vmid, owner)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": read the replication job back")
	}
	if owned == nil || owned.ID != job.ID {
		return cpierrors.Retriable("%s: replication job %s is not listed after its marker write", op, job.ID)
	}
	if m, ok := parseReplicaJobMarker(owned.Comment); !ok || m.Generation != gen {
		return cpierrors.Retriable("%s: replication job %s does not carry generation %d after its marker write", op,
			job.ID, gen)
	}
	return nil
}

// encodedOwnedReplicaJob lists the replication jobs and returns the job of
// guest vmid that carries owner's marker when PVE percent-encoded its comment
// again, and nil when the guest has no such job. The job must be the guest's
// own row, by the VMID PVE lists for it, and its decoded marker must name
// owner, so the result is never another Director's job or a job the CPI did
// not make. A job PVE marked for removal is left out, because pvesr deletes
// it on its next run, and the doctor leaves it out the same way. Rows of the
// guest that cannot be told apart are an error.
func encodedOwnedReplicaJob(
	ctx context.Context, api ha.ReplicationAPI, vmid int, owner string,
) (*ha.ReplicaJobState, error) {
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return nil, pve.WrapError(err)
	}
	owned, _, err := guestReplicaJobs(jobs, vmid, owner)
	if err != nil || owned == nil || owned.RemoveJob != "" {
		return nil, err
	}
	if m, ok := parseReplicaJobMarker(owned.Comment); !ok || !m.Encoded {
		return nil, nil
	}
	return owned, nil
}

// repairTouchedReplicaMarker writes the plain form of the marker back over
// the comment of the replicate VM's job when PVE percent-encoded it again,
// whether or not an allocation journal is configured. Every call that touches
// a replicate VM in HA runs it (see reconcileVMReplica), so the repair lands
// on the next set_vm_metadata, which BOSH sends on every deploy, or reboot_vm,
// or on an attach_disk after a failover, before more rounds of encoding can
// pile up. It changes the comment alone, to ha.DecodedMarkerComment of what
// PVE lists, which keeps the owner, the generation, and any other field the
// marker holds. It writes nothing for a job that is plain already, another
// Director's job, or a job the CPI did not make (see encodedOwnedReplicaJob).
//
// The first read takes no lock, so a VM whose job is plain costs one job
// list. A VM with a journal record has the write made while the record is
// held, after a second read, and only while the marker carries the record's
// confirmed generation with no other one pending. Those are the conditions
// under which no generation write can race it, because adoptSwappedReplicaJob
// and markKeptReplicaJob write a new generation only while they hold the
// record and rewrite an encoded marker themselves. Without a record no other
// path writes a new generation, so the job read first is written. A failure
// is logged and leaves the job as it is, because the CPI reads the encoded
// marker as its own and the next call that touches the VM tries again.
func repairTouchedReplicaMarker(
	ctx context.Context, deps Deps, api ha.ReplicationAPI, logger *log.Logger, op, vmCID string, vmid int,
) {
	owner := deps.RequestDirectorUUID
	job, err := encodedOwnedReplicaJob(ctx, api, vmid, owner)
	if err != nil {
		logger.Info(op+": the VM's replication job could not be read to check its marker; a later call checks "+
			"again", log.Err(err))
		return
	}
	if job == nil {
		return
	}
	var written *ha.ReplicaJobState
	err = withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		var err error
		written, err = writePlainReplicaMarker(ctx, api, logger, op, vmid, owner, *job, handle)
		return err
	})
	switch {
	case err != nil:
		logger.Warn(op+": the VM's replication job keeps its encoded marker, which this CPI still reads as its "+
			"own; a later call that touches the VM rewrites it", log.String("job", job.ID), log.Err(err))
	case written != nil:
		logger.Info(op+": PVE had percent-encoded the VM's replication job marker again, and the marker is "+
			"written back in its plain form", log.String("job", written.ID), log.String("encoded", written.Comment),
			log.String("comment", ha.DecodedMarkerComment(written.Comment)))
	}
}

// writePlainReplicaMarker writes ha.DecodedMarkerComment over the comment of
// job, the encoded job of this Director that encodedOwnedReplicaJob found for
// guest vmid, and returns the job as it was before the write. handle is the
// VM's journal record, held by the caller, or nil when the VM has none. With
// a record the job is read again under it and written only while the record
// holds that job settled at the marker's generation, because otherwise a
// generation write of the record's own is due and rewrites the marker itself.
// It returns nil when it writes nothing.
func writePlainReplicaMarker(
	ctx context.Context, api ha.ReplicationAPI, logger *log.Logger, op string, vmid int, owner string,
	job ha.ReplicaJobState, handle *aj.Handle,
) (*ha.ReplicaJobState, error) {
	if handle != nil {
		current, err := encodedOwnedReplicaJob(ctx, api, vmid, owner)
		if err != nil || current == nil {
			return nil, err
		}
		rep := handle.Record().Replication
		m, _ := parseReplicaJobMarker(current.Comment)
		if rep == nil || rep.Job == nil || rep.Retired || rep.PendingJob != nil || rep.Job.ID != current.ID ||
			rep.Job.Generation != m.Generation {
			logger.Info(op+": the VM's replication job marker is encoded, and the journal record does not "+
				"hold the job settled at the marker's generation, so the record's own marker write rewrites it",
				log.String("job", current.ID))
			return nil, nil
		}
		job = *current
	}
	if err := api.SetComment(ctx, job.ID, ha.DecodedMarkerComment(job.Comment)); err != nil {
		return nil, fmt.Errorf("write the plain marker into replication job %s: %w", job.ID, pve.WrapError(err))
	}
	return &job, nil
}

// prepareCreatedReplicaJob records the job in the VM's journal record and
// returns the generation its marker carries, or 0 when the VM has no record.
// A record that already holds the pending job, from an earlier attempt,
// returns that generation.
func prepareCreatedReplicaJob(
	handle *aj.Handle, owner, id string, vmid int, node, partner string, volids []string, changedAt time.Time,
) (uint64, error) {
	if handle == nil {
		return 0, nil
	}
	_, num, _ := strings.Cut(id, "-")
	jobnum := 0
	if _, err := fmt.Sscanf(num, "%d", &jobnum); err != nil {
		return 0, fmt.Errorf("replication job ID %q has no job number: %w", id, err)
	}
	job := aj.ReplicaJob{ID: id, Guest: vmid, JobNum: jobnum, Source: node, Target: partner}
	rep := handle.Record().Replication
	switch {
	case rep == nil:
		if err := handle.BeginReplication(owner, job, volids, changedAt, nil); err != nil {
			return 0, err
		}
		return 1, nil
	case rep.PendingJob != nil && rep.PendingJob.ID == id:
		return rep.PendingJob.Generation, nil
	case rep.Job != nil && rep.Job.ID == id && !rep.Retired:
		return rep.Job.Generation, nil
	}
	return handle.PrepareReplicationJob(job)
}

// settleFailedReplicaCreate decides a job create PVE answered with an error.
// The job list is read again, because PVE may have applied the create: a job
// that carries this Director's marker is the job, and the create succeeded. A
// job that is absent after a refusal the error classifies as permanent drops
// the pending generation from the journal; any other answer keeps it, so a
// retry writes the same marker, and returns a retriable error.
func settleFailedReplicaCreate(
	ctx context.Context, api ha.ReplicationAPI, op string, handle *aj.Handle, id, owner string, gen uint64,
	createErr error,
) error {
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(createErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: create replication job %s (and the re-read of the jobs failed: %v)", op, id, err))
	}
	guest, _, _ := strings.Cut(id, "-")
	vmid, convErr := strconv.Atoi(guest)
	if convErr != nil {
		return cpierrors.Cloud("%s: replication job ID %q names no guest", op, id)
	}
	mine, err := ha.GuestJobs(jobs, vmid)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(createErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: create replication job %s (and the re-read of the jobs is unusable: %v)", op, id, err))
	}
	for _, j := range mine {
		if j.ID != id {
			continue
		}
		if m, ok := parseReplicaJobMarker(j.Comment); ok && m.Owner == markerValue(owner) && m.Generation == gen {
			return nil
		}
		return cpierrors.Cloud("%s: replication job %s exists after the create failed and does not carry "+
			"this Director's marker: %v", op, id, createErr)
	}
	wrapped := pve.WrapError(createErr)
	if cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
		return cpierrors.WrapAs(wrapped, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: create replication job %s", op, id))
	}
	if handle != nil {
		if err := handle.AbandonReplicationJob(gen); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: drop the refused replication job %s from the journal", op, id))
		}
	}
	return cpierrors.Wrap(wrapped, fmt.Sprintf("%s: PVE refused replication job %s", op, id))
}

// reconcileTouchedVMHA runs the HA catch-up a touch call owes a located VM.
// It finishes a pending re-registration first, then gives a strict pin back
// the nodes it left out only because they could not be read when it was
// written, then sets the failback flag the VM's volumes call for, and then
// reconciles the pin of a replicate VM with where it runs and what its
// replica covers. The widening and the failback step only log when they fail.
func reconcileTouchedVMHA(ctx context.Context, deps Deps, op, vmCID string, vmid int, loc pve.VMLocation,
	logger *log.Logger,
) error {
	if err := finishPendingHAReadd(ctx, deps, op, vmid, loc, logger); err != nil {
		return err
	}
	widenNarrowedPin(ctx, deps, op, vmid, loc, logger)
	reconcileVMFailback(ctx, deps, op, vmid, loc, logger)
	return reconcileVMReplica(ctx, deps, op, vmCID, vmid, loc, logger)
}

// reconcileVMReplica is the replicate policy's step of a call that touches a
// VM. It runs only for a VM with the replicate tag, in mode auto or managed,
// outside the managed lifecycle guard. A VM that runs on the partner its pin
// names moved there by a planned migration or a failover. When the job runs
// back to the old source and a sync from the VM's node is verified, the pin
// keeps both nodes and names the old source as the partner, and the VM's
// migration tasks decide whether the CPI warns of lost writes. After a
// failover the job has not reversed for, or before that sync, the pin is
// narrowed to the node the VM runs on now through the narrowing
// barrier, because the old source's copy may be behind and the next sync
// rolls it back (see reconcileOnPartner). The barrier's change asks the job
// for a sync from the new source. A VM on its source with the
// source-only pin gets one check of its replica, and the pin widens when a
// sync is verified. A VM with the replicate tag that left HA under an earlier
// release, and kept its job, has the job retired (see retireStrandedReplica).
// A pin that cannot be read, a profile that cannot be
// resolved for a narrowing, and a narrowing that cannot finish return a
// retriable error, so the Director retries the call; a widening that cannot
// finish, the profile it needs included, is logged.
// After all of that, a job of this Director whose marker PVE percent-encoded
// again gets its plain marker back, with or without an allocation journal
// (see repairTouchedReplicaMarker).
func reconcileVMReplica(ctx context.Context, deps Deps, op, vmCID string, vmid int, loc pve.VMLocation,
	logger *log.Logger,
) error {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff ||
		!hasReplicaTag(loc.Tags) || loc.Node == "" || deps.RequestDirectorUUID == "" {
		return nil
	}
	if _, wrapped := deps.PVE.(guardWrappedClient); wrapped {
		return nil
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	pin, source, partner, err := readOwnedReplicaPin(ctx, deps, vmid)
	if err != nil {
		// The VM may have failed over, and a narrowing it owes must not be
		// skipped for a read that failed.
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the pin of replicated VM %d to reconcile it with where the VM runs", op, vmid))
	}
	if pin == nil {
		// A VM that left HA under an earlier release kept its job and its
		// replicate tag, and has no pin. The job copies the VM to a partner
		// it can never fail over to, so it is retired here.
		if err := retireStrandedReplica(ctx, deps, logger, op, vmCID, vmid, loc); err != nil {
			logger.Warn(op+": the VM is out of HA and keeps the replicate tag, and its replication job could not "+
				"be retired; a later call that touches the VM tries again", log.Err(err))
		}
		return nil
	}
	api := replicationAPIFor(deps)
	// The marker repair runs last, so a narrowing the VM owes after a
	// failover never waits on it.
	defer repairTouchedReplicaMarker(ctx, deps, api, logger, op, vmCID, vmid)
	switch loc.Node {
	case source:
		if finishPendingReplicaRebuild(ctx, deps, logger, op, vmCID, vmid, pin) || partner != "" {
			return nil
		}
		profile, err := resolveHAProfile(ctx, deps)
		if err != nil {
			logger.Warn(op+": the HA profile could not be resolved to check the VM's replica; a later call checks "+
				"again", log.Err(err))
			return nil
		}
		err = withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
			w := &replicaWiden{deps: deps, logger: logger, api: api, profile: profile, vmid: vmid, node: loc.Node,
				handle: handle}
			_, err := w.run(ctx)
			return err
		})
		if err != nil {
			logger.Warn(op+": the VM's replica could not be checked; the pin keeps the source alone", log.Err(err))
		}
		return nil
	case partner:
		profile, err := resolveHAProfile(ctx, deps)
		if err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("%s: resolve the HA profile to "+
				"narrow the pin of VM %d after its failover", op, vmid))
		}
		narrow := ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps))
		return reconcileOnPartner(ctx, deps, logger, op, vmCID, api, narrow, profile, vmid, loc.Node, pin, source)
	}
	logger.Warn(op+": the VM runs on a node its pin does not name; the CPI leaves its HA state as it is",
		log.String("pinned_source", source), log.String("partner", partner))
	return nil
}

// reconcileOnPartner reconciles the pin of a VM that runs on the partner its
// pin names, after a planned migration or a failover. A planned live
// migration runs a last sync, hands the job's state to the node the VM moves
// to, and swaps the job's direction, so both nodes hold the same data. The
// pin then keeps both nodes, and its partner token moves to the old source
// (see swapAfterMigration). A failover whose first reversed sync has run
// looks the same, so a verified sync keeps both nodes either way, and only a
// qmigrate task on the old source spares the warning that writes were lost.
// Anything else is read as a failover and the pin narrows to this node (see
// narrowAfterFailover). A job that runs from this
// node to the old source with no sync from this node verified yet could be
// either, and is read as a failover, because a pin that names the old source
// while its copy is behind lets HA start the VM on stale data. The next
// verified sync widens that pin back to the old source. A swap whose pin write
// fails returns a retriable error.
func reconcileOnPartner(
	ctx context.Context, deps Deps, logger *log.Logger, op, vmCID string, api ha.ReplicationAPI,
	narrow ha.NarrowDeps, profile *ha.Profile, vmid int, node string, pin *ha.Rule, oldSource string,
) error {
	var swapped bool
	var undecided string
	err := withVMReplicaRecord(ctx, deps, vmCID, func(handle *aj.Handle) error {
		w := &replicaWiden{deps: deps, logger: logger, api: api, profile: profile, vmid: vmid, node: node,
			handle: handle}
		var err error
		swapped, undecided, err = w.swapAfterMigration(ctx, pin, oldSource)
		return err
	})
	switch {
	case err != nil:
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: move the partner of VM %d to its old source after a migration", op, vmid))
	case swapped:
		return nil
	}
	return narrowAfterFailover(ctx, deps, logger, op, api, narrow, profile, vmid, node, pin, undecided)
}

// swappedReplicaPin is the widened pin of a VM that moved from its source to
// its partner node: both nodes stay, node takes the highest priority the pin
// gives any node and the old source the lowest, so the old source never
// outranks node, and the partner token names the old source.
func swappedReplicaPin(pin *ha.Rule, node, oldSource string) *ha.Rule {
	out := *pin
	out.Resources = slices.Clone(pin.Resources)
	highest, lowest, first := 0, 0, true
	for _, p := range pin.Nodes {
		if first || p > highest {
			highest = p
		}
		if first || p < lowest {
			lowest = p
		}
		first = false
	}
	out.Nodes = map[string]int{node: highest, oldSource: lowest}
	out.Comment = ha.WithReplicaPartner(pin.Comment, oldSource)
	out.Errors = nil
	return &out
}

// swapAfterMigration checks whether a VM that runs on w.node, the partner its
// widened pin names, may keep both nodes in its pin, and if so writes the
// swapped pin (see swappedReplicaPin). It applies ha.JudgePartnerMove, the
// rule the doctor applies too. Both nodes stay only when this Director's job
// is enabled and runs from w.node to an online oldSource, and a sync from
// w.node is verified to cover the VM's volumes on oldSource after the pin's
// last change, because only then does the old source hold the VM's data. A
// qmigrate task on oldSource that ended after that change, with no qmstart
// task that shows the VM started outside a migration since the change, makes
// the move a planned migration. With no such task, the move was a failover
// whose first reversed sync has run. With such a start, which an HA recovery
// leaves, or with a task list the CPI cannot read, the move may have been
// one. Each keeps both nodes, because the old source's copy is current again,
// and the CPI warns that writes were or may have been lost. swapped is false when
// the pin narrows instead, and undecided then names why when the job does run
// from w.node to an online oldSource, the case the CPI cannot tell from a
// failover yet. A failed read is undecided too, and only a failed pin write is
// an error.
func (w *replicaWiden) swapAfterMigration(
	ctx context.Context, pin *ha.Rule, oldSource string,
) (swapped bool, undecided string, err error) {
	if w.now == nil {
		w.now = time.Now
	}
	logger := w.logger.With(log.Int(metadataKeyVMID, w.vmid), log.String("node", w.node),
		log.String("old_source", oldSource))
	jobs, err := w.api.ListJobs(ctx)
	if err != nil {
		return false, "the replication jobs could not be listed: " + err.Error(), nil
	}
	job, _, err := guestReplicaJobs(jobs, w.vmid, w.deps.RequestDirectorUUID)
	if err != nil {
		return false, err.Error(), nil
	}
	online, err := replicaNodeOnline(w.profile, nil)(ctx, oldSource)
	move := ha.PartnerMove{Node: w.node, OldSource: oldSource, Job: job, OldSourceOnline: err == nil && online}
	move.ChangedAt, _ = ha.ReplicaChanged(pin.Comment)
	switch ha.JudgePartnerMove(move) {
	case ha.PartnerMoveFailover:
		return false, "", nil
	case ha.PartnerMoveUndecided:
		if move.ChangedAt.IsZero() {
			return false, "the pin records no time of its last change", nil
		}
	}
	synced, held, err := w.verifySync(ctx, *job, oldSource, move.ChangedAt, logger)
	switch {
	case err != nil:
		return false, err.Error(), nil
	case synced == nil && held.reason == "":
		return false, "the journal record does not bind a sync from this node yet", nil
	case synced == nil:
		return false, held.reason, nil
	}
	move.Synced = true
	var unread error
	move.Migration, unread = w.migrationEvidence(ctx, oldSource, move.ChangedAt)
	verdict := ha.JudgePartnerMove(move)
	if !verdict.KeepsBothNodes() {
		return false, "the move could not be judged", nil
	}
	if err := w.failbackOffBeforePartner(ctx); err != nil {
		return false, "", err
	}
	if err := writeReplicaPin(ctx, w.deps, w.profile, logger, w.vmid, pin,
		swappedReplicaPin(pin, w.node, oldSource)); err != nil {
		return false, "", err
	}
	fields := []log.Field{log.String("job", job.ID), log.String("last_sync", synced.lastSync.Format(time.RFC3339))}
	switch verdict {
	case ha.PartnerMovePlanned:
		logger.Info(replicaOp+": the VM moved to its replication partner by a planned migration, and a verified "+
			"sync from this node covers its volumes on the old source; the pin keeps both nodes and names the "+
			"old source as the partner now", fields...)
	case ha.PartnerMoveRecovered:
		logger.Warn(replicaOp+": the VM runs on its replication partner after a failover, since no migration "+
			"task on the old source explains the move; a verified sync from this node covers its volumes on the "+
			"old source again, so the pin keeps both nodes and names the old source as the partner now, but "+
			"writes made on the old source after its last sync before the failover were lost", fields...)
	case ha.PartnerMoveRestarted:
		logger.Warn(replicaOp+": a migration from the old source brought the VM to its replication partner, but "+
			"a start task shows that the VM also started outside a migration since the pin last changed, which "+
			"an HA recovery does; a verified sync from this node covers its volumes on the old source again, so "+
			"the pin keeps both nodes and names the old source as the partner now, but if that start was an HA "+
			"recovery, writes made after the last sync before it were lost", fields...)
	default:
		logger.Warn(replicaOp+": the VM runs on its replication partner, and a verified sync from this node "+
			"covers its volumes on the old source, so the pin keeps both nodes and names the old source as the "+
			"partner now; the CPI could not read the task lists or task logs that would prove a planned migration, "+
			"so if "+
			"this was a failover, writes made on the old source after its last sync before it were lost",
			append(fields, log.Err(unread))...)
	}
	return true, "", nil
}

// migrationLookback is how far before the pin's last change the migration
// task listings reach (see ha.MigrationLookback).
const migrationLookback = ha.MigrationLookback

// migrationEvidence reads the qmigrate and qmstart tasks of the VM on
// oldSource and on w.node and judges them with the logs it needs (see
// ha.ReadMigrationEvidence). A client that cannot list them, or a listing or
// log read that fails, is MigrationUnread with the cause.
func (w *replicaWiden) migrationEvidence(
	ctx context.Context, oldSource string, changedAt time.Time,
) (ha.MigrationEvidence, error) {
	reader, ok := unguardedPVE(w.deps.PVE).(pve.MigrationTaskReader)
	if !ok {
		return ha.MigrationUnread, errors.New("the PVE client cannot list migration and start tasks")
	}
	return ha.ReadMigrationEvidence(ctx, reader, w.vmid, w.node, oldSource, changedAt)
}

// narrowAfterFailover narrows the pin of a VM that runs on its partner to
// that node alone, through the narrowing barrier that narrow reads and writes
// with, and asks the job for a sync from the new source inside it. A resource
// the barrier finds in the error state is registered again under the VM's
// claim, with the narrowed pin written in the confinement step. The barrier
// checks after the request that node reaches every volume. A failed request
// is logged and never undoes the narrowing, because the wide pin names the
// old source, whose copy is behind. undecided is empty for a failover, and
// otherwise names why a move that may have been a planned migration could not
// be proved one, which the warning says.
func narrowAfterFailover(
	ctx context.Context, deps Deps, logger *log.Logger, op string, api ha.ReplicationAPI, narrow ha.NarrowDeps,
	profile *ha.Profile, vmid int, node string, pin *ha.Rule, undecided string,
) error {
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: list the replication jobs of VM %d after its failover", op, vmid))
	}
	job, _, err := guestReplicaJobs(jobs, vmid, deps.RequestDirectorUUID)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": narrow the pin after a failover")
	}
	newPin := narrowedReplicaPin(pin, node, time.Now())
	if undecided == "" {
		logger.Warn(op+": the VM runs on its replication partner after a failover; its pin narrows to this node, "+
			"and writes made on the old source after its last sync are lost when the next sync runs",
			log.String("old_pin", strings.Join(pin.NodeNames(), ",")))
	} else {
		logger.Warn(op+": the VM runs on its replication partner, and its replication job runs from this node "+
			"now, but the CPI cannot tell a planned migration from a failover until a sync from this node is "+
			"verified; its pin narrows to this node, and the next verified sync widens it back to the old source. "+
			"If this was a failover, writes made on the old source after its last sync are lost when the next "+
			"sync runs", log.String("old_pin", strings.Join(pin.NodeNames(), ",")), log.String("reason", undecided))
	}
	writePin := func(ctx context.Context, p *ha.Rule) error {
		cur, _, _, err := readOwnedReplicaPin(ctx, deps, vmid)
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("node-affinity: the pin of VM %d is not this Director's replicate pin", vmid)
		}
		return writeReplicaPin(ctx, deps, profile, logger, vmid, cur, p)
	}
	// The barrier's removal fallback registers the resource again with these
	// flags. A replicate VM keeps failback off, and auto-rebalance stays what
	// the resource carries, on when it is unset where the release has the key,
	// as PVE reads it. A resource that is gone is left to the barrier, which
	// reports it.
	res, err := narrow.Resources.GetHaResources(ctx, haResourceSid(vmid))
	if err != nil && !isHaNotFound(err) && !pve.IsNotFound(err) {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA resource of VM %d before its pin narrows after a failover", op, vmid))
	}
	keys := profile.ResourceKeys()
	rebalance := keys.AutoRebalance
	if err == nil && res != nil {
		rebalance = keys.AutoRebalanceOf(res.AutoRebalance)
	}
	_, err = narrowActive(ctx, narrow, ha.NarrowInput{
		VMID:     vmid,
		Node:     node,
		OldPin:   pin,
		NewPin:   newPin,
		WritePin: writePin,
		Mutation: pve.VMConfigMutation{
			VMID: vmid, Node: node, MotionWait: haMotionWait(deps),
			Label: op + ": request a sync from the new source inside the HA barrier",
			Apply: func(ctx context.Context, at string) error {
				if job == nil {
					logger.Info(op + ": the VM has no replication job of this Director to request a sync from")
					return nil
				}
				if err := api.ScheduleNow(ctx, at, job.ID); err != nil {
					logger.Info(op+": the sync request from the new source failed; the job's schedule runs it",
						log.String("job", job.ID), log.Err(err))
				}
				return nil
			},
		},
		CheckReach: func(ctx context.Context, at string) error {
			return checkPinReach(ctx, deps, profile, vmid, at, newPin.NodeNames())
		},
		// Unchanged stays nil: the barrier then never writes the old pin
		// back, which would name the old source again.
		Reregister: func(ctx context.Context, st *pve.HAServiceState,
			between func(ctx context.Context, node string) (pve.VMLocation, error),
		) (pve.VMLocation, error) {
			return reregisterAfterFailover(ctx, deps, logger, op, profile, vmid, node, st, between)
		},
		Failback:      false,
		AutoRebalance: rebalance,
		Keys:          keys,
		SysConsole:    profile.SysConsole,
		Budget:        haMotionWait(deps),
	})
	return err
}

// reregisterAfterFailover is the failover narrowing's answer to a resource
// the barrier finds in the error state, which a VM that has just failed over
// is the most likely to be in. It removes the resource and registers it
// again under the VM's claim (see reregisterErroredVM), which reads the VM's
// state again there, with the sync request between the two. The decision
// under the claim confines the VM to the node it runs on, and the pin it
// writes records the time of the change, so a sync must follow it before the
// pin names a partner again.
func reregisterAfterFailover(
	ctx context.Context, deps Deps, logger *log.Logger, op string, profile *ha.Profile, vmid int, node string,
	st *pve.HAServiceState, between func(ctx context.Context, node string) (pve.VMLocation, error),
) (pve.VMLocation, error) {
	if st != nil && st.Node != "" {
		node = st.Node
	}
	var landed pve.VMLocation
	res, err := reregisterErroredVM(ctx, deps, op, vmid, node, logger, readdOptions{
		decided: st,
		profile: profile,
		between: func(ctx context.Context, node string) (pve.VMLocation, error) {
			loc, err := between(ctx, node)
			landed = loc
			return loc, err
		},
		writePin: func(ctx context.Context, d *ha.VMDecision, storages []string) (*ha.Rule, error) {
			pin, err := writeMergedPinRule(ctx, deps, profile, vmid, pinRequest{Decision: d, Storages: storages},
				logger)
			if err != nil || pin == nil {
				return pin, err
			}
			stamped := *pin
			stamped.Comment = ha.WithReplicaChanged(ha.WithReplicaPartner(pin.Comment, ""), time.Now())
			if err := writeReplicaPin(ctx, deps, profile, logger, vmid, pin, &stamped); err != nil {
				return nil, err
			}
			return &stamped, nil
		},
	})
	if err != nil {
		return pve.VMLocation{}, err
	}
	if res.Node != "" {
		landed.Node = res.Node
	}
	if landed.Node == "" {
		landed.Node = node
	}
	landed.Found = true
	return landed, nil
}

// checkPinReach re-reads the VM's config on node and checks that every node
// of want reaches every volume the VM holds.
func checkPinReach(ctx context.Context, deps Deps, profile *ha.Profile, vmid int, node string, want []string) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read the config of VM %d: %w", vmid, pve.WrapError(err))
	}
	index, err := ha.NewStorageIndex(profile.Storages)
	if err != nil {
		return err
	}
	reach, err := pve.VolumeAccessSet(ctx, index, want, pve.ConfigAccessVolumes(cfg, node))
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range want {
		if !slices.Contains(reach, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("nodes %s of the pin of VM %d do not reach all of its volumes",
			strings.Join(missing, ","), vmid)
	}
	return nil
}

// withVMReplicaRecord runs fn with the VM's journal record when the journal
// is configured and holds a live record for vmCID, and with nil otherwise.
// The record is acquired for fn and released after it.
func withVMReplicaRecord(ctx context.Context, deps Deps, vmCID string, fn func(*aj.Handle) error) error {
	if vmCID == "" {
		return fn(nil)
	}
	pick := func(records []aj.Record) (*aj.Record, error) { return managedVMRecordForCID(records, vmCID) }
	return withVMRecordWhere(ctx, deps, pick, 0, fn)
}

// errVMRecordBusy says another call held the VM's journal record for longer
// than the caller would wait.
var errVMRecordBusy = errors.New("another call holds the VM's journal record")

// vmRecordTryOnce is the wait that tells withVMRecordWhere to try the record's
// lock once and return errVMRecordBusy at once when another call holds it.
const vmRecordTryOnce time.Duration = -1

// withVMRecordWhere runs fn with the journal record pick selects when the
// journal is configured and the record is live, and with nil otherwise. The
// record is acquired for fn and released after it. A positive wait bounds the
// wait for the record's lock, and a lock held past it returns an error that
// matches errVMRecordBusy, and fn does not run. A wait of vmRecordTryOnce
// tries the lock once and returns errVMRecordBusy at once when it is held, and
// a wait of zero waits for as long as ctx allows.
func withVMRecordWhere(
	ctx context.Context, deps Deps, pick func([]aj.Record) (*aj.Record, error), wait time.Duration,
	fn func(*aj.Handle) error,
) (retErr error) {
	if deps.Config == nil ||
		deps.Config.StoragePlacementNamespace == "" && deps.Config.StorageAllocationJournalDir == "" {
		return fn(nil)
	}
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, nodes)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	records, err := journal.List()
	if err != nil {
		return err
	}
	selected, err := pick(records)
	if err != nil {
		return err
	}
	if selected == nil || vmRecordTerminal(selected.State) {
		return fn(nil)
	}
	if wait == vmRecordTryOnce {
		handle, ok, err := journal.TryAcquire(ctx, selected.ID)
		if err != nil {
			return err
		}
		if !ok {
			return errVMRecordBusy
		}
		defer func() { retErr = errors.Join(retErr, handle.Close()) }()
		return fn(handle)
	}
	actx, cancel := ctx, context.CancelFunc(func() {})
	if wait > 0 {
		actx, cancel = context.WithTimeout(ctx, wait)
	}
	handle, err := journal.Acquire(actx, selected.ID)
	cancel()
	if err != nil {
		if wait > 0 && ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", errVMRecordBusy, err)
		}
		return err
	}
	defer func() { retErr = errors.Join(retErr, handle.Close()) }()
	return fn(handle)
}

// vmRecordTerminal reports whether a VM record's state ends its life: the VM
// is deleted, cleaned up, or deleted with its disks retained.
func vmRecordTerminal(state aj.State) bool {
	return state == aj.Deleted || state == aj.Cleaned || state == aj.VMDeletedRetained
}
