package allocationjournal

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ReplicaCopyKind names why a copy of a replicated VM's volumes outlives the
// job that made it.
type ReplicaCopyKind string

// A superseded copy is an older copy that a failover or a retarget left on a
// node, which a later retirement removes or records as an orphan. An orphan
// is a copy that a forced job removal left behind, which the operator
// removes by hand.
const (
	ReplicaCopySuperseded ReplicaCopyKind = "superseded"
	ReplicaCopyOrphan     ReplicaCopyKind = "orphan"
)

// ReplicaJob is one PVE storage replication job as the CPI created or
// accepted it. ID is "<Guest>-<JobNum>", the form PVE requires. Source and
// Target are the direction the CPI intends, and Generation is the CPI's own
// counter that the job's comment marker carries.
type ReplicaJob struct {
	ID         string `json:"id"`
	Guest      int    `json:"guest"`
	JobNum     int    `json:"jobnum"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Generation uint64 `json:"generation"`
}

// ReplicaCharge is the capacity a volume set's replica takes on the job's
// target, keyed the way placement keys a storage charge.
type ReplicaCharge struct {
	CapacityKey string `json:"capacity_key"`
	DomainKey   string `json:"domain_key,omitempty"`
	Bytes       uint64 `json:"bytes"`
}

// ReplicaVolumeSet is one generation of the volumes a replicated VM holds on
// node-local storage, which are the volumes replication copies. Volumes on
// shared storage are not part of a set. ChangedAt is the time of the change
// on the clock of the node that ran the CPI. A pending set is recorded before
// its change is made, and an abandoned set is one whose change never
// happened.
type ReplicaVolumeSet struct {
	Generation uint64          `json:"generation"`
	VolIDs     []string        `json:"volids"`
	ChangedAt  time.Time       `json:"changed_at"`
	Pending    bool            `json:"pending,omitempty"`
	Abandoned  bool            `json:"abandoned,omitempty"`
	Charges    []ReplicaCharge `json:"charges,omitempty"`
}

// ReplicaSync is the evidence of the last verified sync. It names the job,
// its generation and direction at the check, the volume-set generation the
// sync covered, the last_sync time the job status showed, and when the CPI
// verified it.
type ReplicaSync struct {
	JobID               string    `json:"job_id"`
	JobGeneration       uint64    `json:"job_generation"`
	Source              string    `json:"source"`
	Target              string    `json:"target"`
	VolumeSetGeneration uint64    `json:"volume_set_generation"`
	LastSync            time.Time `json:"last_sync"`
	VerifiedAt          time.Time `json:"verified_at"`
}

// ReplicaCopy is a copy of volumes on a node that no live job keeps current.
type ReplicaCopy struct {
	Kind       ReplicaCopyKind `json:"kind"`
	Node       string          `json:"node"`
	JobID      string          `json:"job_id"`
	VolIDs     []string        `json:"volids"`
	RecordedAt time.Time       `json:"recorded_at"`
}

// Replication is the replicated allocation of a VM record. Job is the job
// whose marker the CPI has confirmed in PVE, and PendingJob is the next
// generation, recorded before its marker is written, so a crash between the
// two writes leaves both generations acceptable. Retired means the CPI has
// begun removing Job, so it no longer names a usable replica. VolumeSets is
// append-only history, Sync is the last verified sync, and Copies lists the
// copies that outlive their job until their retirement.
type Replication struct {
	Owner      string             `json:"owner"`
	Job        *ReplicaJob        `json:"job,omitempty"`
	PendingJob *ReplicaJob        `json:"pending_job,omitempty"`
	Retired    bool               `json:"retired,omitempty"`
	VolumeSets []ReplicaVolumeSet `json:"volume_sets"`
	Sync       *ReplicaSync       `json:"sync,omitempty"`
	Copies     []ReplicaCopy      `json:"copies,omitempty"`
}

// LatestVolumeSet returns the newest volume set that is not abandoned, which
// is a pending set while a change is in flight.
func (r Replication) LatestVolumeSet() (ReplicaVolumeSet, bool) {
	for i := len(r.VolumeSets) - 1; i >= 0; i-- {
		if !r.VolumeSets[i].Abandoned {
			return cloneVolumeSet(r.VolumeSets[i]), true
		}
	}
	return ReplicaVolumeSet{}, false
}

// CommittedVolumeSet returns the newest volume set whose change is done.
func (r Replication) CommittedVolumeSet() (ReplicaVolumeSet, bool) {
	for i := len(r.VolumeSets) - 1; i >= 0; i-- {
		if set := r.VolumeSets[i]; !set.Abandoned && !set.Pending {
			return cloneVolumeSet(set), true
		}
	}
	return ReplicaVolumeSet{}, false
}

// PendingVolumeSet returns the volume set recorded for a change in flight.
func (r Replication) PendingVolumeSet() (ReplicaVolumeSet, bool) {
	if n := len(r.VolumeSets); n > 0 && r.VolumeSets[n-1].Pending {
		return cloneVolumeSet(r.VolumeSets[n-1]), true
	}
	return ReplicaVolumeSet{}, false
}

// Candidates returns the jobs whose marker may stand in PVE now: the pending
// job, then the confirmed job unless it is retired.
func (r Replication) Candidates() []ReplicaJob {
	var jobs []ReplicaJob
	if r.PendingJob != nil {
		jobs = append(jobs, *r.PendingJob)
	}
	if r.Job != nil && !r.Retired {
		jobs = append(jobs, *r.Job)
	}
	return jobs
}

// CoveredVolumeSet returns the volume set that the last verified sync of
// job's direction covered, and reports false unless the replica on
// job.Target is recoverable. It is recoverable only when no volume change is
// in flight that could outdate it, when the sync covered
// the newest committed set through this job in this direction, and when its
// last_sync is later than that set's change.
func (r Replication) CoveredVolumeSet(job ReplicaJob) (ReplicaVolumeSet, bool) {
	if _, pending := r.PendingVolumeSet(); pending || r.Sync == nil {
		return ReplicaVolumeSet{}, false
	}
	set, ok := r.CommittedVolumeSet()
	if !ok || r.Sync.VolumeSetGeneration != set.Generation || !r.Sync.LastSync.After(set.ChangedAt) {
		return ReplicaVolumeSet{}, false
	}
	if r.Sync.JobID != job.ID || r.Sync.Source != job.Source || r.Sync.Target != job.Target {
		return ReplicaVolumeSet{}, false
	}
	return set, true
}

// Recoverable reports whether the confirmed job's target holds a replica
// that covers every volume the VM holds now.
func (r Replication) Recoverable() bool {
	if r.Job == nil || r.Retired || r.PendingJob != nil {
		return false
	}
	_, ok := r.CoveredVolumeSet(*r.Job)
	return ok
}

// CopyNodes returns the sorted nodes on which the record expects a copy of
// volume: the source and target of each candidate job when a volume set that
// is not abandoned holds the volume, the source of a retired job, and every
// node a recorded copy of the volume names.
func (r Replication) CopyNodes(volume string) []string {
	var nodes []string
	inSet := slices.ContainsFunc(r.VolumeSets, func(set ReplicaVolumeSet) bool {
		return !set.Abandoned && slices.Contains(set.VolIDs, volume)
	})
	if inSet {
		for _, job := range r.Candidates() {
			nodes = append(nodes, job.Source, job.Target)
		}
		if r.Job != nil && r.Retired {
			nodes = append(nodes, r.Job.Source)
		}
	}
	for _, copied := range r.Copies {
		if slices.Contains(copied.VolIDs, volume) {
			nodes = append(nodes, copied.Node)
		}
	}
	slices.Sort(nodes)
	return slices.Compact(nodes)
}

func cloneVolumeSet(set ReplicaVolumeSet) ReplicaVolumeSet {
	set.VolIDs = slices.Clone(set.VolIDs)
	set.Charges = slices.Clone(set.Charges)
	return set
}

func cloneReplication(r *Replication) *Replication {
	if r == nil {
		return nil
	}
	out := *r
	if r.Job != nil {
		job := *r.Job
		out.Job = &job
	}
	if r.PendingJob != nil {
		job := *r.PendingJob
		out.PendingJob = &job
	}
	if r.Sync != nil {
		sync := *r.Sync
		out.Sync = &sync
	}
	out.VolumeSets = slices.Clone(r.VolumeSets)
	for i := range out.VolumeSets {
		out.VolumeSets[i] = cloneVolumeSet(out.VolumeSets[i])
	}
	out.Copies = slices.Clone(r.Copies)
	for i := range out.Copies {
		out.Copies[i].VolIDs = slices.Clone(out.Copies[i].VolIDs)
	}
	return &out
}

// replicaToken reports whether s is one nonblank token with no whitespace, so
// it fits the space-separated job marker and a PVE node name.
func replicaToken(s string) bool {
	fields := strings.Fields(s)
	return len(fields) == 1 && fields[0] == s
}

// replicaVolIDs reports whether volids is a nonempty, sorted list of unique
// nonblank volume IDs.
func replicaVolIDs(volids []string) bool {
	if len(volids) == 0 || !slices.IsSorted(volids) {
		return false
	}
	for i, volume := range volids {
		if !replicaToken(volume) || i > 0 && volids[i-1] == volume {
			return false
		}
	}
	return true
}

func validateReplicaJob(job ReplicaJob) error {
	if job.Guest <= 0 || job.JobNum < 0 || job.ID != strconv.Itoa(job.Guest)+"-"+strconv.Itoa(job.JobNum) {
		return fmt.Errorf("%w: replication job identity", ErrCorrupt)
	}
	if !replicaToken(job.Source) || !replicaToken(job.Target) || job.Source == job.Target || job.Generation == 0 {
		return fmt.Errorf("%w: replication job direction or generation", ErrCorrupt)
	}
	return nil
}

// validateReplication checks the invariants of a record's replication
// section. A record without one is valid as it always was.
func validateReplication(r Record) error {
	rep := r.Replication
	if rep == nil {
		return nil
	}
	if r.Kind != "vm" || !replicaToken(rep.Owner) {
		return fmt.Errorf("%w: replication belongs to a VM record with an owner", ErrCorrupt)
	}
	if rep.Job == nil && rep.PendingJob == nil && !rep.Retired {
		return fmt.Errorf("%w: live replication names no job", ErrCorrupt)
	}
	for _, job := range []*ReplicaJob{rep.Job, rep.PendingJob} {
		if job == nil {
			continue
		}
		if err := validateReplicaJob(*job); err != nil {
			return err
		}
	}
	switch {
	case rep.Job != nil && rep.PendingJob != nil:
		if rep.PendingJob.Generation != rep.Job.Generation+1 || rep.PendingJob.Guest != rep.Job.Guest {
			return fmt.Errorf("%w: pending replication job generation", ErrCorrupt)
		}
	case rep.PendingJob != nil:
		if rep.PendingJob.Generation != 1 {
			return fmt.Errorf("%w: first replication job generation", ErrCorrupt)
		}
	}
	if err := validateReplicaVolumeSets(rep.VolumeSets); err != nil {
		return err
	}
	if err := validateReplicaSync(*rep); err != nil {
		return err
	}
	for _, copied := range rep.Copies {
		if copied.Kind != ReplicaCopySuperseded && copied.Kind != ReplicaCopyOrphan || !replicaToken(copied.Node) || !replicaToken(copied.JobID) || !replicaVolIDs(copied.VolIDs) || copied.RecordedAt.IsZero() {
			return fmt.Errorf("%w: replica copy", ErrCorrupt)
		}
	}
	return nil
}

func validateReplicaVolumeSets(sets []ReplicaVolumeSet) error {
	if len(sets) == 0 || sets[0].Pending || sets[0].Abandoned {
		return fmt.Errorf("%w: replication lacks its first committed volume set", ErrCorrupt)
	}
	for i, set := range sets {
		if set.Generation != uint64(i)+1 || !replicaVolIDs(set.VolIDs) || set.ChangedAt.IsZero() || set.Pending && set.Abandoned || set.Pending && i != len(sets)-1 {
			return fmt.Errorf("%w: replica volume set %d", ErrCorrupt, i+1)
		}
		for _, charge := range set.Charges {
			if !nonblank(charge.CapacityKey) || charge.Bytes == 0 {
				return fmt.Errorf("%w: replica charge", ErrCorrupt)
			}
		}
	}
	return nil
}

func validateReplicaSync(rep Replication) error {
	sync := rep.Sync
	if sync == nil {
		return nil
	}
	if !replicaToken(sync.JobID) || sync.JobGeneration == 0 || !replicaToken(sync.Source) || !replicaToken(sync.Target) || sync.Source == sync.Target || sync.LastSync.IsZero() || sync.VerifiedAt.IsZero() {
		return fmt.Errorf("%w: replica sync evidence", ErrCorrupt)
	}
	if sync.VolumeSetGeneration == 0 || sync.VolumeSetGeneration > uint64(len(rep.VolumeSets)) {
		return fmt.Errorf("%w: replica sync names no volume set", ErrCorrupt)
	}
	set := rep.VolumeSets[sync.VolumeSetGeneration-1]
	if set.Pending || set.Abandoned || !sync.LastSync.After(set.ChangedAt) {
		return fmt.Errorf("%w: replica sync predates or misses its volume set", ErrCorrupt)
	}
	return nil
}

// normalizeReplicaVolIDs returns volids sorted and without duplicates.
func normalizeReplicaVolIDs(volids []string) []string {
	out := slices.Clone(volids)
	slices.Sort(out)
	return slices.Compact(out)
}

// saveReplication applies mutate to a copy of the record's replication
// section and persists the result. Only the section changes, so the write
// moves no state, adds no evidence or step, and certifies nothing about an
// earlier operation; it records the CPI's intent before the PVE change it
// guards, or evidence the CPI read just now. That is why a handle that Acquire
// opened may write it before reconciliation, as it may write a settlement.
func (h *Handle) saveReplication(begin bool, mutate func(rep *Replication) error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	next := cloneRecord(h.record)
	if next.Kind != "vm" {
		return fmt.Errorf("%w: only a VM allocation is replicated", ErrConflict)
	}
	if terminal(next.State) {
		return fmt.Errorf("%w: terminal evidence is immutable", ErrConflict)
	}
	switch {
	case begin && next.Replication != nil:
		return fmt.Errorf("%w: allocation is already replicated", ErrConflict)
	case begin:
		next.Replication = &Replication{}
	case next.Replication == nil:
		return fmt.Errorf("%w: allocation is not replicated", ErrConflict)
	}
	if err := mutate(next.Replication); err != nil {
		return err
	}
	return h.saveLocked(next, saveReplicationTransition)
}

func utc(t time.Time) time.Time { return t.UTC() }

// BeginReplication records the replicated allocation before the CPI creates
// its first job. owner is the Director UUID the job marker carries, job is the
// job the CPI is about to create, and volids with changedAt and charges is
// the first volume set. The journal assigns the job generation 1 and records
// it as pending, so the job's comment is written only after this write.
func (h *Handle) BeginReplication(owner string, job ReplicaJob, volids []string, changedAt time.Time, charges []ReplicaCharge) error {
	return h.saveReplication(true, func(rep *Replication) error {
		job.Generation = 1
		rep.Owner = owner
		rep.PendingJob = &job
		rep.VolumeSets = []ReplicaVolumeSet{{Generation: 1, VolIDs: normalizeReplicaVolIDs(volids), ChangedAt: utc(changedAt), Charges: slices.Clone(charges)}}
		return nil
	})
}

// PrepareReplicationJob records the next job generation as pending before the
// CPI writes it into a job's comment, for a retarget, a recreated job, or an
// accepted failover that swaps the direction. It returns the generation the
// marker must carry. Preparing the same job again returns the same
// generation, so a resumed call writes the same marker. The guest never
// changes.
func (h *Handle) PrepareReplicationJob(job ReplicaJob) (uint64, error) {
	var generation uint64
	err := h.saveReplication(false, func(rep *Replication) error {
		next := uint64(1)
		if rep.Job != nil {
			next = rep.Job.Generation + 1
			if job.Guest != rep.Job.Guest {
				return fmt.Errorf("%w: a replication job keeps its guest", ErrConflict)
			}
		}
		job.Generation = next
		if rep.PendingJob != nil {
			if *rep.PendingJob != job {
				return fmt.Errorf("%w: another replication job generation is pending", ErrConflict)
			}
		}
		rep.PendingJob = &job
		generation = next
		return nil
	})
	return generation, err
}

// ConfirmReplicationJob records that PVE carries the pending generation's
// marker, which makes the pending job the confirmed one. A generation that is
// already confirmed is a no-op, so a resumed call can confirm again.
func (h *Handle) ConfirmReplicationJob(generation uint64) error {
	return h.saveReplication(false, func(rep *Replication) error {
		if rep.PendingJob == nil && rep.Job != nil && rep.Job.Generation == generation {
			return nil
		}
		if rep.PendingJob == nil || rep.PendingJob.Generation != generation {
			return fmt.Errorf("%w: replication job generation %d is not pending", ErrConflict, generation)
		}
		rep.Job, rep.PendingJob, rep.Retired = rep.PendingJob, nil, false
		return nil
	})
}

// AbandonReplicationJob drops a pending generation whose marker the CPI
// proved PVE never carries, such as a job create PVE refused. The confirmed
// job stays as it was; dropping the first pending job retires the record's
// replication.
func (h *Handle) AbandonReplicationJob(generation uint64) error {
	return h.saveReplication(false, func(rep *Replication) error {
		if rep.PendingJob == nil || rep.PendingJob.Generation != generation {
			return fmt.Errorf("%w: replication job generation %d is not pending", ErrConflict, generation)
		}
		rep.PendingJob = nil
		if rep.Job == nil {
			rep.Retired = true
		}
		return nil
	})
}

// RetireReplicationJob records, before the CPI deletes the job, that the job
// no longer names a usable replica. A pending generation is dropped with it,
// because the job it would mark is being removed.
func (h *Handle) RetireReplicationJob() error {
	return h.saveReplication(false, func(rep *Replication) error {
		rep.Retired, rep.PendingJob = true, nil
		return nil
	})
}

// PrepareReplicaVolumeSet records the next volume set as pending before the
// CPI changes the VM's volume set, which leaves the VM not recoverable until a
// sync covers it. It returns the new generation. Preparing the same volumes
// again while they are pending returns the same generation.
func (h *Handle) PrepareReplicaVolumeSet(volids []string, changedAt time.Time, charges []ReplicaCharge) (uint64, error) {
	var generation uint64
	err := h.saveReplication(false, func(rep *Replication) error {
		volids = normalizeReplicaVolIDs(volids)
		if pending, ok := rep.PendingVolumeSet(); ok {
			if !slices.Equal(pending.VolIDs, volids) {
				return fmt.Errorf("%w: another replica volume set is pending", ErrConflict)
			}
			generation = pending.Generation
			return nil
		}
		generation = uint64(len(rep.VolumeSets)) + 1
		rep.VolumeSets = append(rep.VolumeSets, ReplicaVolumeSet{Generation: generation, VolIDs: volids, ChangedAt: utc(changedAt), Pending: true, Charges: slices.Clone(charges)})
		return nil
	})
	return generation, err
}

// CommitReplicaVolumeSet records that the pending change is made. A
// generation that is already committed is a no-op.
func (h *Handle) CommitReplicaVolumeSet(generation uint64) error {
	return h.saveReplication(false, func(rep *Replication) error {
		return settleReplicaVolumeSet(rep, generation, false)
	})
}

// AbandonReplicaVolumeSet records that the pending change never happened. The
// set stays in the history, so its generation is never reused.
func (h *Handle) AbandonReplicaVolumeSet(generation uint64) error {
	return h.saveReplication(false, func(rep *Replication) error {
		return settleReplicaVolumeSet(rep, generation, true)
	})
}

func settleReplicaVolumeSet(rep *Replication, generation uint64, abandon bool) error {
	if generation == 0 || generation > uint64(len(rep.VolumeSets)) {
		return fmt.Errorf("%w: replica volume set %d is unknown", ErrConflict, generation)
	}
	set := &rep.VolumeSets[generation-1]
	if !set.Pending {
		if set.Abandoned == abandon {
			return nil
		}
		return fmt.Errorf("%w: replica volume set %d is already settled", ErrConflict, generation)
	}
	set.Pending, set.Abandoned = false, abandon
	return nil
}

// RecordReplicaSync records the evidence of a verified sync before the CPI
// widens the VM's pin. The sync must name the confirmed job in its recorded
// direction and the newest committed volume set, with a last_sync later than
// that set's change, and no change or job generation may be in flight. The
// caller has checked the rest of the evidence in PVE just before.
func (h *Handle) RecordReplicaSync(sync ReplicaSync) error {
	return h.saveReplication(false, func(rep *Replication) error {
		if rep.Job == nil || rep.Retired || rep.PendingJob != nil {
			return fmt.Errorf("%w: a sync needs a confirmed live job", ErrConflict)
		}
		if _, pending := rep.PendingVolumeSet(); pending {
			return fmt.Errorf("%w: a sync cannot cover a pending volume set", ErrConflict)
		}
		set, ok := rep.CommittedVolumeSet()
		job := rep.Job
		if !ok || sync.VolumeSetGeneration != set.Generation || sync.JobID != job.ID || sync.JobGeneration != job.Generation || sync.Source != job.Source || sync.Target != job.Target {
			return fmt.Errorf("%w: the sync does not name the current job and volume set", ErrConflict)
		}
		if !sync.LastSync.After(set.ChangedAt) {
			return fmt.Errorf("%w: the sync started before volume set %d changed", ErrConflict, set.Generation)
		}
		sync.LastSync, sync.VerifiedAt = utc(sync.LastSync), utc(sync.VerifiedAt)
		rep.Sync = &sync
		return nil
	})
}

// RecordReplicaCopy lists a superseded copy or an orphan replica. Recording
// the same copy again is a no-op.
func (h *Handle) RecordReplicaCopy(copied ReplicaCopy) error {
	return h.saveReplication(false, func(rep *Replication) error {
		copied.VolIDs = normalizeReplicaVolIDs(copied.VolIDs)
		copied.RecordedAt = utc(copied.RecordedAt)
		for _, known := range rep.Copies {
			if known.Kind == copied.Kind && known.Node == copied.Node && known.JobID == copied.JobID && slices.Equal(known.VolIDs, copied.VolIDs) {
				return nil
			}
		}
		rep.Copies = append(rep.Copies, copied)
		return nil
	})
}

// ReleaseReplicaCopy drops the copies of kind on node from jobID once a
// retirement has removed them or recorded them as orphans. Releasing a copy
// that is not listed is a no-op.
func (h *Handle) ReleaseReplicaCopy(kind ReplicaCopyKind, node, jobID string) error {
	return h.saveReplication(false, func(rep *Replication) error {
		rep.Copies = slices.DeleteFunc(rep.Copies, func(copied ReplicaCopy) bool {
			return copied.Kind == kind && copied.Node == node && copied.JobID == jobID
		})
		if len(rep.Copies) == 0 {
			rep.Copies = nil
		}
		return nil
	})
}

// validateReplicationTransition checks that next differs from old only in its
// replication section, and that the section moved forward. The owner never
// changes, the section is never removed, job generations never go back,
// volume sets are append-only apart from settling the pending one, and the
// sync evidence never moves to an older volume set.
func validateReplicationTransition(old, next Record) error {
	rest, nextRest := cloneRecord(old), cloneRecord(next)
	rest.Replication, nextRest.Replication = nil, nil
	rest.UpdatedAt, nextRest.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(rest, nextRest) {
		return fmt.Errorf("%w: a replication write changed more than the replication section", ErrConflict)
	}
	was, now := old.Replication, next.Replication
	if now == nil {
		return fmt.Errorf("%w: replication evidence removed", ErrConflict)
	}
	if was == nil {
		return nil
	}
	if was.Owner != now.Owner {
		return fmt.Errorf("%w: replication owner changed", ErrConflict)
	}
	if was.Job != nil && (now.Job == nil || now.Job.Generation < was.Job.Generation || now.Job.Guest != was.Job.Guest) {
		return fmt.Errorf("%w: confirmed replication job went back", ErrConflict)
	}
	if len(now.VolumeSets) < len(was.VolumeSets) {
		return fmt.Errorf("%w: replica volume set history removed", ErrConflict)
	}
	for i := range was.VolumeSets {
		before, after := was.VolumeSets[i], now.VolumeSets[i]
		if before.Pending && !after.Pending {
			before.Pending, before.Abandoned = false, after.Abandoned
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("%w: replica volume set %d changed", ErrConflict, i+1)
		}
	}
	if was.Sync != nil && (now.Sync == nil || now.Sync.VolumeSetGeneration < was.Sync.VolumeSetGeneration) {
		return fmt.Errorf("%w: replica sync evidence went back", ErrConflict)
	}
	return nil
}
