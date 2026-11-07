package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// storageAuditReasonReplicationUnread marks a replicated move the audit could
// neither accept nor refuse, because the read of PVE's replication jobs
// failed. A gate that refuses only on such moves defers with a retriable
// error, since the next read can decide them.
const storageAuditReasonReplicationUnread = "replication_unread"

// The directions a replication job can report against the job the record
// names. PVE reverses a job when its guest migrates or recovers on the job's
// target, so the reversed direction is as legitimate as the original.
const (
	storageReplicaDirectionOriginal = "original"
	storageReplicaDirectionReversed = "reversed"
)

// StorageReplicaMove is what the audit proved about a replicated VM before it
// accepted the VM on a node its steps do not name. JobID and JobGeneration
// name the job whose marker the proof read, and Direction is how PVE ran it,
// both empty when the VM sat on its replication source and no job read was
// needed. VolumeSetGeneration is the volume set the VM's node-local volumes
// matched. Failover is true when the VM was found on the job's target.
type StorageReplicaMove struct {
	JobID               string `json:"job_id,omitempty"`
	JobGeneration       uint64 `json:"job_generation,omitempty"`
	Direction           string `json:"direction,omitempty"`
	VolumeSetGeneration uint64 `json:"volume_set_generation"`
	Failover            bool   `json:"failover"`
}

// replicaJobMarker is the ownership marker in a replication job's comment.
// Generation is the CPI's generation of the job, which the journal records
// before it writes the comment.
type replicaJobMarker struct {
	Owner      string
	Generation uint64
}

// renderReplicaJobMarker renders the comment of a replication job the CPI
// owns, "bosh-cpi:v1 owner=<director-uuid> gen=<n>".
func renderReplicaJobMarker(owner string, generation uint64) string {
	return haMarkerPrefix + " owner=" + markerValue(owner) + " gen=" + strconv.FormatUint(generation, 10)
}

// parseReplicaJobMarker reads the ownership marker at the start of a
// replication job's comment. It reports false when the comment carries no
// marker, a marker without an owner or a generation, a repeated owner or
// generation, or a generation that is not a positive integer, because each of
// those could be a reused job ID or a hand edit, and neither proves the job is
// the one the journal recorded. Unknown keys are ignored so a later field
// does not orphan the job.
func parseReplicaJobMarker(comment string) (replicaJobMarker, bool) {
	fields := strings.Fields(comment)
	if len(fields) == 0 || fields[0] != haMarkerPrefix {
		return replicaJobMarker{}, false
	}
	var marker replicaJobMarker
	seenOwner, seenGeneration := false, false
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "owner":
			if seenOwner || value == "" || value == haUnsetField {
				return replicaJobMarker{}, false
			}
			seenOwner, marker.Owner = true, value
		case "gen":
			generation, err := strconv.ParseUint(value, 10, 64)
			if seenGeneration || err != nil || generation == 0 {
				return replicaJobMarker{}, false
			}
			seenGeneration, marker.Generation = true, generation
		}
	}
	if !seenOwner || !seenGeneration {
		return replicaJobMarker{}, false
	}
	return marker, true
}

// storageAuditReplicated reports whether a record carries a replication that
// still names a job, so its VM's sightings follow the replicated move rule. A
// replication whose only job was abandoned names no node, and its VM follows
// the shared-storage rule like any other.
func storageAuditReplicated(record aj.Record) bool {
	rep := record.Replication
	return record.Kind == "vm" && rep != nil && (rep.Job != nil || rep.PendingJob != nil)
}

// storageAuditReplicaJobOf returns the job that places the VM: the confirmed
// job, or the pending first job while its marker is unconfirmed.
func storageAuditReplicaJobOf(rep aj.Replication) aj.ReplicaJob {
	if rep.Job != nil {
		return *rep.Job
	}
	return *rep.PendingJob
}

// storageAuditReplicaAway reports whether a replicated record places its VM on
// a node other than node. The steps keep the node the VM was created on, so a
// sighting there after the replication moved the VM away is judged by the
// replicated move rule rather than accepted by the step.
func storageAuditReplicaAway(record aj.Record, node string) bool {
	return storageAuditReplicated(record) && storageAuditReplicaJobOf(*record.Replication).Source != node
}

// storageAuditReplicaIndex maps each node-local volume a live replicated VM
// record names to the nodes where its replication keeps a copy.
type storageAuditReplicaIndex map[string][]string

// storageAuditReplicaIndexFor indexes the copy nodes of every volume the live
// replicated VM records name, and returns, per storage, the nodes whose
// listings must run so each copy is inspected.
func storageAuditReplicaIndexFor(records []aj.Record) (storageAuditReplicaIndex, map[string]map[string]bool) {
	index := storageAuditReplicaIndex{}
	listings := map[string]map[string]bool{}
	for recordIndex := range records {
		record := records[recordIndex]
		if record.Kind != "vm" || record.Replication == nil || record.State == aj.Deleted || record.State == aj.Cleaned {
			continue
		}
		for _, volume := range storageAuditReplicaVolumes(*record.Replication) {
			nodes := record.Replication.CopyNodes(volume)
			index[volume] = append(index[volume], nodes...)
			storage, _, err := pve.ParseDiskCID(volume)
			if err != nil {
				continue
			}
			if listings[storage] == nil {
				listings[storage] = map[string]bool{}
			}
			for _, node := range nodes {
				listings[storage][node] = true
			}
		}
	}
	for volume, nodes := range index {
		slices.Sort(nodes)
		index[volume] = slices.Compact(nodes)
	}
	return index, listings
}

// storageAuditReplicaVolumes returns every volume a replication names, in its
// volume sets and its recorded copies.
func storageAuditReplicaVolumes(rep aj.Replication) []string {
	var volumes []string
	for _, set := range rep.VolumeSets {
		volumes = append(volumes, set.VolIDs...)
	}
	for _, copied := range rep.Copies {
		volumes = append(volumes, copied.VolIDs...)
	}
	slices.Sort(volumes)
	return slices.Compact(volumes)
}

// copyAt reports whether a replication keeps a copy of volume on node. The
// record's own replication counts as well as the index, so a record that a
// second journal read found answers for itself.
func (index storageAuditReplicaIndex) copyAt(record aj.Record, node, volume string) bool {
	if slices.Contains(index[volume], node) {
		return true
	}
	if record.Kind != "vm" || record.Replication == nil || record.State == aj.Deleted || record.State == aj.Cleaned {
		return false
	}
	return slices.Contains(record.Replication.CopyNodes(volume), node)
}

// storageAuditReplicaTargetMatches is storageAuditVolumeTargetMatches that
// also accepts a node-local volume on a node where a replication keeps its
// copy. Only the node may differ: the step must name the volume, and the
// storage and its backing must still match what the step froze.
func storageAuditReplicaTargetMatches(record aj.Record, step aj.Step, stores map[string]pve.StorageInfo, replicas storageAuditReplicaIndex, node, volume string) (bool, string) {
	match, reason := storageAuditVolumeTargetMatches(record, step, stores, node, volume)
	if !match && reason == storageAuditReasonNodeLocalElsewhere && replicas.copyAt(record, node, volume) {
		return true, ""
	}
	return match, reason
}

// storageAuditReplicationRead is the one read of PVE's replication jobs that
// an audit makes, taken the first time a move rule needs it.
type storageAuditReplicationRead struct {
	jobs    map[string]sdkcluster.GetReplicationResponse
	failure string
}

// replicationJobs returns PVE's replication jobs by ID, reading them on first
// use. On a failed or malformed read it returns the reason, which leaves
// every move that needs a job undecided rather than refused, because a
// failed read proves nothing about the job.
func (r *StorageAllocationAudit) replicationJobs(ctx context.Context, deps Deps) (map[string]sdkcluster.GetReplicationResponse, string) {
	if r.replication == nil {
		r.replication = readStorageAuditReplication(ctx, deps)
	}
	return r.replication.jobs, r.replication.failure
}

func readStorageAuditReplication(ctx context.Context, deps Deps) *storageAuditReplicationRead {
	service := deps.PVE.Cluster()
	if service == nil {
		return &storageAuditReplicationRead{failure: "the PVE client cannot read replication jobs (CPI defect)"}
	}
	response, err := service.ListReplication(ctx)
	if err != nil {
		return &storageAuditReplicationRead{failure: "replication jobs could not be listed: " + pve.DescribeAuditError(err)}
	}
	if response == nil {
		return &storageAuditReplicationRead{failure: "replication jobs could not be listed: PVE returned no listing"}
	}
	jobs := map[string]sdkcluster.GetReplicationResponse{}
	for entry, raw := range *response {
		var job sdkcluster.GetReplicationResponse
		if err := json.Unmarshal(raw, &job); err != nil || job.Id == "" {
			return &storageAuditReplicationRead{failure: fmt.Sprintf("replication jobs could not be listed: entry %d is malformed", entry)}
		}
		if _, repeated := jobs[job.Id]; repeated {
			return &storageAuditReplicationRead{failure: fmt.Sprintf("replication jobs could not be listed: job %s is listed twice", storageAuditField(job.Id))}
		}
		jobs[job.Id] = job
	}
	return &storageAuditReplicationRead{jobs: jobs}
}

// deferReplication records an issue that a failed replication read raised.
func (r *StorageAllocationAudit) deferReplication(issue string) {
	if r.deferred == nil {
		r.deferred = map[string]bool{}
	}
	r.deferred[issue] = true
}

// storageAuditReplicaPlan is what the journal alone says about a replicated
// VM's sighting. sets are the volume sets the VM's node-local volumes may
// match. failover is true when the VM sits on the job's target, which needs
// the PVE job's marker as well.
type storageAuditReplicaPlan struct {
	failover bool
	job      aj.ReplicaJob
	sets     []aj.ReplicaVolumeSet
}

// storageAuditReplicaPlanFor decides from the journal alone whether node can
// hold the replicated VM, and against which volume sets. On the source, the
// VM holds the committed set, or the pending set when a crash left a volume
// change in flight and may or may not have made it. On the confirmed job's
// target, the VM holds what the last verified sync covered, and nothing
// newer may be in flight. Every other node is refused without a read.
func storageAuditReplicaPlanFor(rep aj.Replication, node string, vmid int) (storageAuditReplicaPlan, string) {
	job := storageAuditReplicaJobOf(rep)
	if job.Guest != vmid {
		return storageAuditReplicaPlan{}, fmt.Sprintf("the record's replication job %s names guest %d, not VM %d", job.ID, job.Guest, vmid)
	}
	if node == job.Source {
		var sets []aj.ReplicaVolumeSet
		if committed, ok := rep.CommittedVolumeSet(); ok {
			sets = append(sets, committed)
		}
		if pending, ok := rep.PendingVolumeSet(); ok {
			sets = append(sets, pending)
		}
		if len(sets) == 0 {
			return storageAuditReplicaPlan{}, "the record's replication holds no volume set"
		}
		return storageAuditReplicaPlan{job: job, sets: sets}, ""
	}
	if rep.Job == nil || node != rep.Job.Target {
		if rep.Job != nil && !rep.Retired {
			return storageAuditReplicaPlan{}, fmt.Sprintf("%s is neither the replication source %s nor its partner %s that the record names", node, job.Source, rep.Job.Target)
		}
		return storageAuditReplicaPlan{}, fmt.Sprintf("%s is not the replication source %s that the record names", node, job.Source)
	}
	if rep.Retired {
		return storageAuditReplicaPlan{}, fmt.Sprintf("the replication job to %s is retired, so its replica was not kept current", node)
	}
	covered, ok := rep.CoveredVolumeSet(*rep.Job)
	if !ok {
		return storageAuditReplicaPlan{}, storageAuditReplicaCoverageRefusal(rep)
	}
	return storageAuditReplicaPlan{failover: true, job: *rep.Job, sets: []aj.ReplicaVolumeSet{covered}}, ""
}

// storageAuditReplicaCoverageRefusal names why the last verified sync does not
// cover the confirmed job's replica.
func storageAuditReplicaCoverageRefusal(rep aj.Replication) string {
	if pending, ok := rep.PendingVolumeSet(); ok {
		return fmt.Sprintf("volume set %d of the replication is still pending, so no verified sync covers the VM's volumes", pending.Generation)
	}
	if rep.Sync == nil {
		return "no sync of the replication was ever verified"
	}
	committed, _ := rep.CommittedVolumeSet()
	sync, job := *rep.Sync, *rep.Job
	switch {
	case sync.VolumeSetGeneration != committed.Generation:
		return fmt.Sprintf("the last verified sync covered volume set %d, not the current volume set %d", sync.VolumeSetGeneration, committed.Generation)
	case !sync.LastSync.After(committed.ChangedAt):
		return fmt.Sprintf("the last verified sync ran at %s, before volume set %d changed at %s", sync.LastSync.UTC().Format(time.RFC3339), committed.Generation, committed.ChangedAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("the last verified sync ran through job %s from %s to %s, not through job %s from %s to %s", sync.JobID, sync.Source, sync.Target, job.ID, job.Source, job.Target)
}

// storageAuditObservedReplicatedMove decides whether a replicated VM sighted
// where its steps do not place it, or away from its replication source, is
// there legitimately. It returns the accepted move, or the reason the
// sighting stays a conflict, and a move it can only leave undecided because a
// listing or the replication read failed.
//
// The record checks of the shared rule have passed. The journal must place
// the VM on node (storageAuditReplicaPlanFor), which refuses a third node,
// a retired job's target, and a target that the last verified sync does not
// cover. The VM's node-local volumes, which are every volume it holds on
// storage that is not shared, must equal one of the volume sets the plan
// allows, and each must sit on storage that was node-local with the same
// backing when its owner froze it and be listed on node. Its shared volumes
// meet the shared-storage conditions. A failover also needs PVE's job
// (storageAuditReplicaJobRefusal). The journal checks run first, and the job
// read runs only when nothing the audit already holds refuses the move.
func storageAuditObservedReplicatedMove(ctx context.Context, deps Deps, result *StorageAllocationAudit, index storageAuditMoveIndex, evidence StorageAllocationEvidence, record aj.Record, recorded []string) storageAuditMoveVerdict {
	refused := func(reason string) storageAuditMoveVerdict {
		return storageAuditMoveVerdict{refusal: reason, replicated: true}
	}
	rep := *record.Replication
	plan, refusal := storageAuditReplicaPlanFor(rep, evidence.Node, evidence.VMID)
	if refusal != "" {
		return refused(refusal)
	}
	held, volumes, refusal := storageAuditMovedVMVolumes(ctx, deps, result, index, evidence, record)
	if refusal != "" {
		return refused(refusal)
	}
	local := map[string]bool{}
	for _, volume := range volumes {
		storage, _, err := pve.ParseDiskCID(volume)
		if current, defined := index.stores[storage]; err == nil && defined && !current.IsShared() {
			local[volume] = true
		}
	}
	heldLocal := slices.Sorted(maps.Keys(local))
	set, matched := storageAuditReplicaSetMatch(plan.sets, heldLocal)
	if !matched {
		return refused(fmt.Sprintf("VM %d holds the node-local volumes [%s], which match no replica volume set the record allows (%s)", evidence.VMID, strings.Join(heldLocal, " "), storageAuditReplicaSetsText(plan.sets)))
	}
	unlisted := ""
	for _, volume := range volumes {
		refusal, failed := storageAuditVolumeRefusal(result, index, evidence.Node, volume, held[volume], local[volume])
		switch {
		case refusal == "":
		case !failed:
			return refused(refusal)
		case unlisted == "":
			unlisted = refusal
		}
	}
	detail := &StorageReplicaMove{VolumeSetGeneration: set.Generation, Failover: plan.failover}
	if plan.failover {
		jobs, failure := result.replicationJobs(ctx, deps)
		if failure != "" {
			return storageAuditMoveVerdict{refusal: failure, deferred: true, replicated: true}
		}
		proof, refusal := storageAuditReplicaJobRefusal(jobs, rep, evidence.Node, evidence.VMID)
		if refusal != "" {
			return refused(refusal)
		}
		detail.JobID, detail.JobGeneration, detail.Direction = proof.JobID, proof.JobGeneration, proof.Direction
	}
	if unlisted != "" {
		return storageAuditMoveVerdict{refusal: unlisted, unread: true, replicated: true}
	}
	sorted := slices.Clone(volumes)
	slices.Sort(sorted)
	move := StorageAllocationMove{AllocationID: record.ID, Kind: "vm", VMID: evidence.VMID, RecordedNodes: recorded, ObservedNode: evidence.Node, Volumes: sorted, Replication: detail}
	return storageAuditMoveVerdict{move: move, replicated: true}
}

// storageAuditReplicaSetMatch returns the first set whose volids equal held.
func storageAuditReplicaSetMatch(sets []aj.ReplicaVolumeSet, held []string) (aj.ReplicaVolumeSet, bool) {
	for _, set := range sets {
		if slices.Equal(set.VolIDs, held) {
			return set, true
		}
	}
	return aj.ReplicaVolumeSet{}, false
}

func storageAuditReplicaSetsText(sets []aj.ReplicaVolumeSet) string {
	parts := make([]string, 0, len(sets))
	for _, set := range sets {
		state := ""
		if set.Pending {
			state = " pending"
		}
		parts = append(parts, fmt.Sprintf("set %d%s [%s]", set.Generation, state, strings.Join(set.VolIDs, " ")))
	}
	return strings.Join(parts, ", ")
}

// storageAuditReplicaJobRefusal proves from PVE's jobs that the replication
// that kept the replica on node is the one the journal recorded. One of the
// record's candidate jobs (Replication.Candidates) must stand in PVE under
// its ID, not marked for removal, for this guest and job number, with a
// comment whose marker names the record's owner and that candidate's
// generation. Both candidates count while a pending generation is recorded,
// because a crash can come between that record and the comment write. The job
// must run in its recorded direction or the reverse, which PVE writes when
// the guest recovers on the target, and the direction must name node. A
// disabled job still proves the replica. It returns the proof, or every
// candidate's reason joined.
func storageAuditReplicaJobRefusal(jobs map[string]sdkcluster.GetReplicationResponse, rep aj.Replication, node string, vmid int) (StorageReplicaMove, string) {
	var reasons []string
	for _, candidate := range rep.Candidates() {
		proof, reason := storageAuditReplicaCandidateProof(jobs, rep.Owner, candidate, node, vmid)
		if reason == "" {
			return proof, ""
		}
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 {
		return StorageReplicaMove{}, "the record names no live replication job"
	}
	return StorageReplicaMove{}, strings.Join(reasons, "; ")
}

func storageAuditReplicaCandidateProof(jobs map[string]sdkcluster.GetReplicationResponse, owner string, candidate aj.ReplicaJob, node string, vmid int) (StorageReplicaMove, string) {
	job, found := jobs[candidate.ID]
	switch {
	case !found:
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s is not defined in PVE", candidate.ID)
	case job.RemoveJob != nil && *job.RemoveJob != "":
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s is marked for removal", candidate.ID)
	case job.Guest.Int() != int64(vmid) || job.Guest.Int() != int64(candidate.Guest) || job.Jobnum.Int() != int64(candidate.JobNum):
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s names guest %d job %d, not guest %d job %d", candidate.ID, job.Guest.Int(), job.Jobnum.Int(), candidate.Guest, candidate.JobNum)
	}
	comment := ""
	if job.Comment != nil {
		comment = *job.Comment
	}
	marker, marked := parseReplicaJobMarker(comment)
	switch {
	case !marked:
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s carries no CPI ownership marker", candidate.ID)
	case marker.Owner != owner:
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s is marked for Director %s, not %s", candidate.ID, storageAuditField(marker.Owner), owner)
	case marker.Generation != candidate.Generation:
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s carries generation %d, not %d", candidate.ID, marker.Generation, candidate.Generation)
	}
	source := ""
	if job.Source != nil {
		source = *job.Source
	}
	direction := ""
	switch {
	case job.Target == candidate.Target && (source == "" || source == candidate.Source):
		direction = storageReplicaDirectionOriginal
	case job.Target == candidate.Source && (source == "" || source == candidate.Target):
		direction = storageReplicaDirectionReversed
	default:
		return StorageReplicaMove{}, fmt.Sprintf("replication job %s runs from %s to %s, neither its recorded direction from %s to %s nor the reverse", candidate.ID, storageAuditField(source), storageAuditField(job.Target), candidate.Source, candidate.Target)
	}
	if node != candidate.Source && node != candidate.Target {
		return StorageReplicaMove{}, fmt.Sprintf("generation %d of replication job %s does not name %s", candidate.Generation, candidate.ID, node)
	}
	return StorageReplicaMove{JobID: candidate.ID, JobGeneration: candidate.Generation, Direction: direction}, ""
}

// storageAuditHolderKey names the VM a pending disk move's holder is.
type storageAuditHolderKey struct {
	vmid int
	node string
}

// storageAuditReplicatedDiskMove decides a persistent disk whose provenance
// names another node, held by a VM that the replicated move rule judged on
// this node. The disk moves with its holder or not at all: a holder accepted
// with this volume among the volumes it proved carries the disk, an undecided
// holder leaves the disk undecided for the same reason, and a refused holder
// refuses it. The disk record itself must be a live disk record of this
// namespace that froze the volume's storage.
func storageAuditReplicatedDiskMove(index storageAuditMoveIndex, pending storageAuditPendingMove, holder storageAuditMoveVerdict) storageAuditMoveVerdict {
	evidence := pending.evidence
	switch {
	case holder.deferred:
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the replicated move of holder VM %d is undecided: %s", evidence.VMID, holder.refusal), deferred: true}
	case holder.unread:
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the replicated move of holder VM %d is undecided: %s", evidence.VMID, holder.refusal), unread: true}
	case holder.refusal != "":
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the replicated move of holder VM %d was not accepted: %s", evidence.VMID, holder.refusal)}
	case !slices.Contains(holder.move.Volumes, evidence.VolumeID):
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the replicated move of holder VM %d does not cover volume %s", evidence.VMID, evidence.VolumeID)}
	}
	record, found := index.byID[evidence.AllocationID]
	if !found || record.Kind != allocationKindDisk || record.Namespace != index.namespace {
		return storageAuditMoveVerdict{refusal: "no disk record of this namespace owns the allocation"}
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the disk record is in state %s", record.State)}
	}
	if !storageAuditDiskFrozen(record, evidence.VolumeID).found {
		return storageAuditMoveVerdict{refusal: fmt.Sprintf("the disk record froze no definition of the storage of volume %s", evidence.VolumeID)}
	}
	detail := *holder.move.Replication
	move := StorageAllocationMove{AllocationID: record.ID, Kind: allocationKindDisk, VMID: evidence.VMID, RecordedNodes: []string{pending.recorded}, ObservedNode: evidence.Node, Volumes: []string{evidence.VolumeID}, Replication: &detail}
	return storageAuditMoveVerdict{move: move}
}

// storageAuditGateDeferred reports whether every finding that makes gate
// refuse report is a move that a failed replication read left undecided. A
// conflict, an incomplete VM scan, or any other issue refuses for good.
func storageAuditGateDeferred(report StorageAllocationAudit, gate storageAuditGate) bool {
	if len(report.deferred) == 0 || gate&storageAuditGateComplete == 0 {
		return false
	}
	if len(report.Conflicts) > 0 || !report.VMScanComplete {
		return false
	}
	for _, issue := range report.Issues {
		if !report.deferred[issue] {
			return false
		}
	}
	return true
}
