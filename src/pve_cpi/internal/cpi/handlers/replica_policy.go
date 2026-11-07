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
	return slices.Contains(parseTagsField(tags), haReplicaTag)
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
			return nil, nil, fmt.Errorf("VM %d has replication jobs %s and %s that both carry this Director's "+
				"marker", vmid, owned.ID, j.ID)
		}
		job := j
		owned = &job
	}
	return owned, foreign, nil
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

// replicaPinShape reads the pin of a replicate VM. A pin that names one node
// is the source-only pin, and a pin that names two nodes and carries the
// partner token is the widened pin. Any other pin is not one the replicate
// policy wrote, and ok is false.
func replicaPinShape(pin *ha.Rule) (source, partner string, ok bool) {
	if pin == nil || !pin.Strict || pin.Disabled {
		return "", "", false
	}
	nodes := pin.NodeNames()
	partner, widened := ha.ReplicaPartner(pin.Comment)
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
	owner := deps.RequestDirectorUUID
	err := withHARulesLock(ctx, deps, vmid, logger, func() error {
		writer := newHARuleWriterFor(deps, profile, logger, nil)
		_, err := writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
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
			case !ha.SameRule(cur, expect):
				return nil, fmt.Errorf("node-affinity: pin %q of VM %d changed since it was read (%s); the next "+
					"call that touches the VM settles it", pin.ID, vmid, strings.Join(ha.RuleDiff(expect, cur), ","))
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *pin}}, nil
		})
		return err
	})
	if err != nil {
		return err
	}
	stager := &ha.Stager{
		ReadRules: func(ctx context.Context) (*ha.RuleSet, error) { return readHARuleSet(ctx, deps.PVE.Cluster()) },
		Managed:   deps.Config.HAModeValue() == config.HAModeManaged,
		Logger:    logger,
	}
	return stager.Verify(ctx, &ha.VMDecision{VMID: vmid, SID: haResourceSid(vmid)}, pin, "")
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
	source, partner, ok := replicaPinShape(pin)
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
}

// run checks the replica once and widens the pin when the check passes. It
// reports whether the pin names the partner afterwards. A replica that is not
// yet verified, or a VM whose state does not let the policy widen, is no
// error: the pin keeps the source alone and a later call that touches the VM
// checks again. The pin stays on the source while the job is disabled or
// marked for removal, and while the VM holds a node-local volume the job does
// not copy, such as a persistent disk, which replication never copies because
// a disk ID owns it. A pin that records no time of its last change gets that
// time now, so a later sync can cover it. An error is a read or a write that
// failed.
func (w *replicaWiden) run(ctx context.Context) (bool, error) {
	if w.now == nil {
		w.now = time.Now
	}
	logger := w.logger.With(log.Int(metadataKeyVMID, w.vmid), log.String("node", w.node))
	owner := w.deps.RequestDirectorUUID
	pin, source, partner, err := readOwnedReplicaPin(ctx, w.deps, w.vmid)
	switch {
	case err != nil:
		return false, fmt.Errorf("read the pin of VM %d: %w", w.vmid, err)
	case pin == nil:
		logger.Info(replicaOp + ": the VM has no pin of this Director in the replicate shape; nothing to widen")
		return false, nil
	case partner != "":
		return true, nil
	case source != w.node:
		logger.Info(replicaOp+": the VM runs away from its pinned node; the pin stays as it is",
			log.String("pinned", source))
		return false, nil
	}
	changedAt, ok := ha.ReplicaChanged(pin.Comment)
	if !ok {
		stamped := *pin
		stamped.Comment = ha.WithReplicaChanged(pin.Comment, w.now())
		if err := writeReplicaPin(ctx, w.deps, w.profile, logger, w.vmid, pin, &stamped); err != nil {
			return false, fmt.Errorf("record the time of the last change on the pin of VM %d: %w", w.vmid, err)
		}
		logger.Info(replicaOp + ": the VM's pin recorded no time of its last change, and records now as that " +
			"time; a sync that follows it can widen the pin")
		return false, nil
	}
	jobs, err := w.api.ListJobs(ctx)
	if err != nil {
		return false, fmt.Errorf("list the replication jobs: %w", err)
	}
	job, _, err := guestReplicaJobs(jobs, w.vmid, owner)
	if err != nil {
		return false, err
	}
	if job == nil {
		logger.Info(replicaOp + ": the VM has no replication job of this Director; the pin keeps the source alone")
		return false, nil
	}
	if job.Disabled || job.RemoveJob != "" {
		logger.Info(replicaOp+": the VM's replication job is disabled or marked for removal; the pin keeps the "+
			"source alone", log.String("job", job.ID), log.String("remove_job", job.RemoveJob))
		return false, nil
	}
	target := replicaOtherNode(*job, w.node)
	if target == "" {
		return false, nil
	}
	volids, gaps, err := replicaVolumesOn(ctx, w.deps, w.profile, w.node, w.vmid)
	if err != nil {
		return false, err
	}
	if len(gaps) > 0 {
		logger.Warn(replicaOp+": the VM holds node-local volumes its replication job does not copy, so the "+
			"partner would hold no copy of them; the pin keeps the source alone", log.String("job", job.ID),
			log.String("gaps", strings.Join(gaps, "; ")))
		return false, nil
	}
	if len(volids) == 0 {
		logger.Warn(replicaOp + ": the VM holds no volume on node-local ZFS storage that the profile knows; the " +
			"pin keeps the source alone")
		return false, nil
	}
	evidence, err := w.journalEvidence(ctx, *job, target, volids, changedAt, logger)
	if err != nil || evidence == nil {
		return false, err
	}
	verdict, err := ha.CheckReplicaSync(ctx, w.api, ha.ReplicaSyncCheck{
		JobID: job.ID, Source: w.node, Target: target, VolIDs: volids, ChangedAt: evidence.changedAt,
	})
	if err != nil {
		return false, err
	}
	if !verdict.Verified {
		logger.Info(replicaOp+": the replica is not verified yet; the pin keeps the source alone until a later "+
			"call sees a sync that covers the VM's volumes", log.String("job", job.ID),
			log.String("partner", target), log.String("reason", verdict.Reason))
		return false, nil
	}
	if w.handle != nil {
		if err := w.handle.RecordReplicaSync(aj.ReplicaSync{
			JobID: job.ID, JobGeneration: evidence.jobGeneration, Source: w.node, Target: target,
			VolumeSetGeneration: evidence.setGeneration, LastSync: verdict.Status.LastSync, VerifiedAt: w.now(),
		}); err != nil {
			return false, fmt.Errorf("record the verified sync of job %s: %w", job.ID, err)
		}
	}
	if err := writeReplicaPin(ctx, w.deps, w.profile, logger, w.vmid, pin, widenedReplicaPin(pin, target)); err != nil {
		return false, err
	}
	logger.Info(replicaOp+": a verified sync covers the VM's volumes; the pin names the partner now",
		log.String("job", job.ID), log.String("partner", target),
		log.String("last_sync", verdict.Status.LastSync.Format(time.RFC3339)))
	return true, nil
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
// the record does not let the pin widen now, which is logged.
func (w *replicaWiden) journalEvidence(
	ctx context.Context, job ha.ReplicaJobState, target string, volids []string, changedAt time.Time,
	logger *log.Logger,
) (*replicaEvidence, error) {
	if w.handle == nil {
		return &replicaEvidence{changedAt: changedAt}, nil
	}
	refuse := func(why string) (*replicaEvidence, error) {
		logger.Info(replicaOp+": the journal record does not let the pin widen yet: "+why, log.String("job", job.ID))
		return nil, nil
	}
	adopted, why, err := w.adoptSwappedReplicaJob(ctx, job, target, logger)
	switch {
	case err != nil:
		return nil, err
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
			return nil, fmt.Errorf("record the VM's volume set: %w", err)
		}
		if err := w.handle.CommitReplicaVolumeSet(gen); err != nil {
			return nil, fmt.Errorf("commit the VM's volume set: %w", err)
		}
		rep = w.handle.Record().Replication
		if set, ok = rep.CommittedVolumeSet(); !ok || set.Generation != gen {
			return nil, fmt.Errorf("the journal does not show volume set %d as committed", gen)
		}
	}
	if set.ChangedAt.After(changedAt) {
		changedAt = set.ChangedAt
	}
	return &replicaEvidence{changedAt: changedAt, jobGeneration: rep.Job.Generation, setGeneration: set.Generation}, nil
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
	if marker.Generation != gen {
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
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: retire replication job %s of VM %d", r.caller, owned.ID, r.vmid))
	}
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
	if d == nil || d.Policy != ha.PolicyReplicate || d.Partner == "" {
		return nil
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", node), log.String("partner", d.Partner))
	managed := deps.Config.HAModeValue() == config.HAModeManaged
	degrade := func(what string, err error) error {
		switch {
		case !replicaRefusalProven(err):
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "create_vm: "+what)
		case managed:
			return err
		}
		logger.Warn("create_vm: "+what+"; the VM runs under PVE HA pinned to its node without a replica",
			log.Err(err))
		return nil
	}
	if profile == nil {
		return degrade("the HA profile is not known", cpierrors.Retriable("create_vm: the HA profile of VM %d "+
			"is not known, so its replication job cannot be set up", vmid))
	}
	if err := mutateVMTags(ctx, deps, node, vmid, "create_vm: tag the VM as replicated", logger,
		func(tags []string) []string {
			if slices.Contains(tags, haReplicaTag) {
				return tags
			}
			return append(slices.Clone(tags), haReplicaTag)
		}); err != nil {
		return degrade("the bosh-ha-replica tag could not be written", err)
	}
	volids, gaps, err := replicaVolumesOn(ctx, deps, profile, node, vmid)
	if err != nil {
		return degrade("the VM's volumes could not be read", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			"create_vm: read the replicated volumes"))
	}
	switch {
	case len(gaps) > 0:
		return degrade("the VM holds node-local volumes replication does not copy", cpierrors.Cloud("create_vm: "+
			"VM %d holds node-local volumes its replication job would not copy: %s", vmid, strings.Join(gaps, "; ")))
	case len(volids) == 0:
		return degrade("the VM holds no volume on node-local ZFS storage", cpierrors.Cloud("create_vm: VM %d "+
			"holds no volume for its replication job to copy", vmid))
	}
	changedAt := time.Now()
	api := replicationAPIFor(deps)
	job, err := ensureCreatedReplicaJob(ctx, deps, api, vmid, node, d.Partner, volids, changedAt, handle)
	if err != nil {
		return degrade("the replication job could not be created", err)
	}
	pin, source, partner, err := readOwnedReplicaPin(ctx, deps, vmid)
	switch {
	case err != nil:
		return degrade("the pin could not be read to record the job's start", err)
	case pin == nil || source != node:
		return degrade("the VM's pin is not the source-only pin", cpierrors.Cloud("create_vm: the pin of VM %d "+
			"does not name node %s alone", vmid, node))
	case partner == "":
		if _, recorded := ha.ReplicaChanged(pin.Comment); !recorded {
			stamped := *pin
			stamped.Comment = ha.WithReplicaChanged(pin.Comment, changedAt)
			if err := writeReplicaPin(ctx, deps, profile, logger, vmid, pin, &stamped); err != nil {
				// A later call that touches the VM records the time on a pin
				// that has none, so the job stands.
				logger.Warn("create_vm: the pin could not record the job's start; the next call that touches the "+
					"VM records it", log.Err(err))
			}
		}
	}
	if err := api.ScheduleNow(ctx, node, job); err != nil {
		logger.Info("create_vm: the request to run the replication job now failed; its schedule runs it",
			log.String("job", job), log.Err(err))
	}
	logger.Info("create_vm: the VM's replication job is configured; the pin names the source alone until a call "+
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
// over. The journal records the job's generation before the create and
// confirms it after.
func ensureCreatedReplicaJob(
	ctx context.Context, deps Deps, api ha.ReplicationAPI, vmid int, node, partner string, volids []string,
	changedAt time.Time, handle *aj.Handle,
) (string, error) {
	owner := deps.RequestDirectorUUID
	if owner == "" {
		return "", cpierrors.Cloud("create_vm: the request carries no director_uuid to mark the replication job "+
			"of VM %d with", vmid)
	}
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return "", cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: list the replication jobs before VM %d gets one", vmid))
	}
	owned, foreign, err := guestReplicaJobs(jobs, vmid, owner)
	if err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "create_vm: set up the replication job")
	}
	if len(foreign) > 0 {
		return "", cpierrors.Cloud("create_vm: VM %d already has replication job %s, which does not carry this "+
			"Director's marker", vmid, foreign[0].ID)
	}
	id := ha.ReplicaJobID(vmid, ha.NextJobNum(jobs, vmid))
	if owned != nil {
		id = owned.ID
	}
	gen, err := prepareCreatedReplicaJob(handle, owner, id, vmid, node, partner, volids, changedAt)
	if err != nil {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: record replication job %s in the journal", id))
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
			if err := settleFailedReplicaCreate(ctx, api, handle, id, owner, gen, cerr); err != nil {
				return "", err
			}
		}
	}
	if handle != nil {
		if err := handle.ConfirmReplicationJob(gen); err != nil {
			return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("create_vm: confirm replication job %s in the journal", id))
		}
	}
	return id, nil
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
	ctx context.Context, api ha.ReplicationAPI, handle *aj.Handle, id, owner string, gen uint64, createErr error,
) error {
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(createErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: create replication job %s (and the re-read of the jobs failed: %v)", id, err))
	}
	guest, _, _ := strings.Cut(id, "-")
	vmid, convErr := strconv.Atoi(guest)
	if convErr != nil {
		return cpierrors.Cloud("create_vm: replication job ID %q names no guest", id)
	}
	mine, err := ha.GuestJobs(jobs, vmid)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(createErr), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: create replication job %s (and the re-read of the jobs is unusable: %v)", id, err))
	}
	for _, j := range mine {
		if j.ID != id {
			continue
		}
		if m, ok := parseReplicaJobMarker(j.Comment); ok && m.Owner == markerValue(owner) && m.Generation == gen {
			return nil
		}
		return cpierrors.Cloud("create_vm: replication job %s exists after the create failed and does not carry "+
			"this Director's marker: %v", id, createErr)
	}
	wrapped := pve.WrapError(createErr)
	if cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
		return cpierrors.WrapAs(wrapped, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: create replication job %s", id))
	}
	if handle != nil {
		if err := handle.AbandonReplicationJob(gen); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("create_vm: drop the refused replication job %s from the journal", id))
		}
	}
	return cpierrors.Wrap(wrapped, fmt.Sprintf("create_vm: PVE refused replication job %s", id))
}

// reconcileTouchedVMHA runs the HA catch-up a touch call owes a located VM:
// it finishes a pending re-registration first, then reconciles the pin of a
// replicate VM with where it runs and what its replica covers.
func reconcileTouchedVMHA(ctx context.Context, deps Deps, op, vmCID string, vmid int, loc pve.VMLocation,
	logger *log.Logger,
) error {
	if err := finishPendingHAReadd(ctx, deps, op, vmid, loc, logger); err != nil {
		return err
	}
	return reconcileVMReplica(ctx, deps, op, vmCID, vmid, loc, logger)
}

// reconcileVMReplica is the replicate policy's step of a call that touches a
// VM. It runs only for a VM with the replicate tag, in mode auto or managed,
// outside the managed lifecycle guard. A VM that runs on the partner its pin
// names has failed over, and its pin is narrowed to the node it runs on now
// through the narrowing barrier, because the old source's copy is behind the
// partner's and the next sync rolls it back. The barrier's change asks the
// job for a sync from the new source. A VM on its source with the
// source-only pin gets one check of its replica, and the pin widens when a
// sync is verified. A pin that cannot be read, a profile that cannot be
// resolved for a narrowing, and a narrowing that cannot finish return a
// retriable error, so the Director retries the call; a widening that cannot
// finish, the profile it needs included, is logged.
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
		return nil
	}
	api := replicationAPIFor(deps)
	switch loc.Node {
	case source:
		if partner != "" {
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
		return narrowAfterFailover(ctx, deps, logger, op, api, narrow, profile, vmid, loc.Node, pin)
	}
	logger.Warn(op+": the VM runs on a node its pin does not name; the CPI leaves its HA state as it is",
		log.String("pinned_source", source), log.String("partner", partner))
	return nil
}

// narrowAfterFailover narrows the pin of a VM that runs on its partner to
// that node alone, through the narrowing barrier that narrow reads and writes
// with, and asks the job for a sync from the new source inside it. A resource
// the barrier finds in the error state is registered again under the VM's
// claim, with the narrowed pin written in the confinement step. The barrier
// checks after the request that node reaches every volume. A failed request
// is logged and never undoes the narrowing, because the wide pin names the
// old source, whose copy is behind.
func narrowAfterFailover(
	ctx context.Context, deps Deps, logger *log.Logger, op string, api ha.ReplicationAPI, narrow ha.NarrowDeps,
	profile *ha.Profile, vmid int, node string, pin *ha.Rule,
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
	logger.Warn(op+": the VM runs on its replication partner after a failover; its pin narrows to this node, and "+
		"writes made on the old source after its last sync are lost when the next sync runs",
		log.String("old_pin", strings.Join(pin.NodeNames(), ",")))
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
	_, err = ha.NarrowActive(ctx, narrow, ha.NarrowInput{
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
		SysConsole: profile.SysConsole,
		Budget:     haMotionWait(deps),
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
func withVMReplicaRecord(ctx context.Context, deps Deps, vmCID string, fn func(*aj.Handle) error) (retErr error) {
	if deps.Config == nil || vmCID == "" ||
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
	selected, err := managedVMRecordForCID(records, vmCID)
	if err != nil {
		return err
	}
	if selected == nil || selected.State == aj.Deleted || selected.State == aj.Cleaned ||
		selected.State == aj.VMDeletedRetained {
		return fn(nil)
	}
	handle, err := journal.Acquire(ctx, selected.ID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, handle.Close()) }()
	return fn(handle)
}
