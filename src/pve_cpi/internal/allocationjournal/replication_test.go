package allocationjournal

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

var replicaEpoch = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func replicaJob(source, target string) ReplicaJob {
	return ReplicaJob{ID: "101-0", Guest: 101, JobNum: 0, Source: source, Target: target}
}

// replicatedVM returns a handle on a VM record whose replication has a
// confirmed first job from node-a to node-b and one committed volume set.
func replicatedVM(t *testing.T, j *Journal) *Handle {
	t.Helper()
	h := acquireVM(t, j)
	if err := h.BeginReplication("director-1", replicaJob("node-a", "node-b"), []string{"zfs:vm-101-disk-1", "zfs:vm-101-disk-0", "zfs:vm-101-disk-0"}, replicaEpoch, []ReplicaCharge{{CapacityKey: "node-b/zfs", Bytes: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfirmReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	return h
}

func replicaSyncOf(job ReplicaJob, set uint64, lastSync time.Time) ReplicaSync {
	return ReplicaSync{JobID: job.ID, JobGeneration: job.Generation, Source: job.Source, Target: job.Target, VolumeSetGeneration: set, LastSync: lastSync, VerifiedAt: lastSync.Add(time.Second)}
}

func TestReplicationBeginConfirmAndPersist(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer closeHandle(t, h)
	if err := h.BeginReplication("director-1", replicaJob("node-a", "node-b"), []string{"zfs:b", "zfs:a"}, replicaEpoch, nil); err != nil {
		t.Fatal(err)
	}
	rep := h.Record().Replication
	if rep.Job != nil || rep.PendingJob == nil || rep.PendingJob.Generation != 1 || !slices.Equal(rep.VolumeSets[0].VolIDs, []string{"zfs:a", "zfs:b"}) {
		t.Fatalf("begin recorded %+v", rep)
	}
	if got := rep.Candidates(); len(got) != 1 || got[0].Generation != 1 {
		t.Fatalf("candidates before the marker is confirmed: %+v", got)
	}
	if err := h.BeginReplication("director-1", replicaJob("node-a", "node-b"), []string{"zfs:a"}, replicaEpoch, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second begin: %v", err)
	}
	if err := h.ConfirmReplicationJob(2); !errors.Is(err, ErrConflict) {
		t.Fatalf("confirming a generation that is not pending: %v", err)
	}
	if err := h.ConfirmReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	if err := h.ConfirmReplicationJob(1); err != nil {
		t.Fatalf("confirming again is a no-op: %v", err)
	}
	stored, err := j.Inspect(h.Record().ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Replication, h.Record().Replication) || stored.Replication.Job.Generation != 1 || stored.Replication.PendingJob != nil {
		t.Fatalf("stored replication %+v, held %+v", stored.Replication, h.Record().Replication)
	}
}

func TestReplicationIsImmutableThroughSave(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	r := h.Record()
	r.Replication.Job.Generation = 9
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("Save changed the replication section: %v", err)
	}
	r = h.Record()
	r.Replication = nil
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("Save removed the replication section: %v", err)
	}
	// An ordinary evidence write keeps the section as it was.
	r = h.Record()
	r.Steps = []Step{step()}
	save(t, h, r)
	if h.Record().Replication.Job.Generation != 1 {
		t.Fatal("an evidence write lost the replication section")
	}
}

func TestReplicationWritesBeforeReconciliationOnAResumedHandle(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	id := h.Record().ID
	closeHandle(t, h)
	resumed, err := j.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, resumed)
	r := resumed.Record()
	r.Reason = "changed"
	r.State = Observed
	if err := resumed.Save(r); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("a resumed handle saved ordinary evidence: %v", err)
	}
	if _, err := resumed.PrepareReplicaVolumeSet([]string{"zfs:vm-101-disk-0", "zfs:vm-101-disk-1", "zfs:vm-101-disk-2"}, replicaEpoch.Add(time.Minute), nil); err != nil {
		t.Fatalf("a resumed handle could not record the pending volume set: %v", err)
	}
}

func TestReplicationJobGenerationsAcrossAFailover(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	swapped := replicaJob("node-b", "node-a")
	generation, err := h.PrepareReplicationJob(swapped)
	if err != nil || generation != 2 {
		t.Fatalf("prepare = %d, %v", generation, err)
	}
	again, err := h.PrepareReplicationJob(swapped)
	if err != nil || again != 2 {
		t.Fatalf("preparing the same job again = %d, %v", again, err)
	}
	if _, err := h.PrepareReplicationJob(replicaJob("node-b", "node-c")); !errors.Is(err, ErrConflict) {
		t.Fatalf("another job while one is pending: %v", err)
	}
	other := replicaJob("node-b", "node-a")
	other.Guest, other.ID = 102, "102-0"
	// Both generations are candidates until the marker write is confirmed.
	got := h.Record().Replication.Candidates()
	if len(got) != 2 || got[0].Generation != 2 || got[1].Generation != 1 {
		t.Fatalf("candidates during the crash window: %+v", got)
	}
	if err := h.ConfirmReplicationJob(2); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PrepareReplicationJob(other); !errors.Is(err, ErrConflict) {
		t.Fatalf("a job for another guest: %v", err)
	}
	rep := h.Record().Replication
	if rep.Job.Source != "node-b" || rep.Job.Generation != 2 || rep.PendingJob != nil {
		t.Fatalf("confirmed swap %+v", rep.Job)
	}
	if _, err := h.PrepareReplicationJob(replicaJob("node-b", "node-c")); err != nil {
		t.Fatal(err)
	}
	if err := h.AbandonReplicationJob(4); !errors.Is(err, ErrConflict) {
		t.Fatalf("abandoning a generation that is not pending: %v", err)
	}
	if err := h.AbandonReplicationJob(3); err != nil {
		t.Fatal(err)
	}
	if rep := h.Record().Replication; rep.PendingJob != nil || rep.Job.Generation != 2 || rep.Retired {
		t.Fatalf("abandon left %+v", rep)
	}
}

func TestReplicationAbandonFirstJobAndRetire(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer closeHandle(t, h)
	if err := h.BeginReplication("director-1", replicaJob("node-a", "node-b"), []string{"zfs:a"}, replicaEpoch, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.AbandonReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	rep := h.Record().Replication
	if !rep.Retired || rep.Job != nil || len(rep.Candidates()) != 0 || rep.Recoverable() {
		t.Fatalf("abandoned first job left %+v", rep)
	}
	// A retired replication can start a new job, which revives it once its
	// marker is confirmed.
	generation, err := h.PrepareReplicationJob(replicaJob("node-a", "node-c"))
	if err != nil || generation != 1 {
		t.Fatalf("re-create = %d, %v", generation, err)
	}
	if err := h.ConfirmReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	if err := h.RetireReplicationJob(); err != nil {
		t.Fatal(err)
	}
	rep = h.Record().Replication
	if !rep.Retired || len(rep.Candidates()) != 0 {
		t.Fatalf("retire left %+v", rep)
	}
	if nodes := rep.CopyNodes("zfs:a"); !slices.Equal(nodes, []string{"node-a"}) {
		t.Fatalf("a retired job keeps only its source: %v", nodes)
	}
}

func TestReplicaVolumeSetsAndSyncEvidence(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	job := *h.Record().Replication.Job
	if err := h.RecordReplicaSync(replicaSyncOf(job, 1, replicaEpoch)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a sync that started at the change: %v", err)
	}
	if err := h.RecordReplicaSync(replicaSyncOf(job, 2, replicaEpoch.Add(time.Minute))); !errors.Is(err, ErrConflict) {
		t.Fatalf("a sync of an unknown set: %v", err)
	}
	wrong := replicaSyncOf(job, 1, replicaEpoch.Add(time.Hour))
	wrong.Target = "node-c"
	if err := h.RecordReplicaSync(wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("a sync in another direction: %v", err)
	}
	if err := h.RecordReplicaSync(replicaSyncOf(job, 1, replicaEpoch.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	rep := h.Record().Replication
	if !rep.Recoverable() {
		t.Fatalf("a verified sync of the current set is not recoverable: %+v", rep)
	}

	// The pending set makes the VM configured, not recoverable, and a sync
	// cannot cover it until its change is committed.
	attached := []string{"zfs:vm-101-disk-0", "zfs:vm-101-disk-1", "zfs:vm-101-disk-2"}
	changed := replicaEpoch.Add(2 * time.Minute)
	generation, err := h.PrepareReplicaVolumeSet(attached, changed, nil)
	if err != nil || generation != 2 {
		t.Fatalf("prepare set = %d, %v", generation, err)
	}
	if again, err := h.PrepareReplicaVolumeSet(attached, changed, nil); err != nil || again != 2 {
		t.Fatalf("preparing the same set again = %d, %v", again, err)
	}
	if _, err := h.PrepareReplicaVolumeSet(attached[:1], changed, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("another set while one is pending: %v", err)
	}
	if h.Record().Replication.Recoverable() {
		t.Fatal("a pending volume change left the VM recoverable")
	}
	if latest, _ := h.Record().Replication.LatestVolumeSet(); latest.Generation != 2 || !latest.Pending {
		t.Fatalf("latest set %+v", latest)
	}
	if err := h.RecordReplicaSync(replicaSyncOf(job, 1, changed.Add(time.Minute))); !errors.Is(err, ErrConflict) {
		t.Fatalf("a sync recorded while a change is pending: %v", err)
	}
	if err := h.CommitReplicaVolumeSet(2); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitReplicaVolumeSet(2); err != nil {
		t.Fatalf("committing again is a no-op: %v", err)
	}
	if err := h.AbandonReplicaVolumeSet(2); !errors.Is(err, ErrConflict) {
		t.Fatalf("abandoning a committed set: %v", err)
	}
	if h.Record().Replication.Recoverable() {
		t.Fatal("the last sync covered set 1, not the committed set 2")
	}
	// A run that began before the change cannot cover it.
	if err := h.RecordReplicaSync(replicaSyncOf(job, 2, changed.Add(-time.Second))); !errors.Is(err, ErrConflict) {
		t.Fatalf("a sync that began before the change: %v", err)
	}
	if err := h.RecordReplicaSync(replicaSyncOf(job, 2, changed.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if !h.Record().Replication.Recoverable() {
		t.Fatal("a sync of set 2 is not recoverable")
	}

	generation, err = h.PrepareReplicaVolumeSet(attached[:2], changed.Add(time.Hour), nil)
	if err != nil || generation != 3 {
		t.Fatalf("prepare set 3 = %d, %v", generation, err)
	}
	if err := h.AbandonReplicaVolumeSet(3); err != nil {
		t.Fatal(err)
	}
	rep = h.Record().Replication
	if committed, _ := rep.CommittedVolumeSet(); committed.Generation != 2 || !rep.Recoverable() {
		t.Fatalf("an abandoned change moved the committed set: %+v", rep)
	}
	if err := h.CommitReplicaVolumeSet(7); !errors.Is(err, ErrConflict) {
		t.Fatalf("committing an unknown set: %v", err)
	}
	if next, err := h.PrepareReplicaVolumeSet(attached[:2], changed.Add(2*time.Hour), nil); err != nil || next != 4 {
		t.Fatalf("an abandoned generation was reused: %d, %v", next, err)
	}
}

func TestReplicaCopiesAndCopyNodes(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	orphan := ReplicaCopy{Kind: ReplicaCopyOrphan, Node: "node-c", JobID: "101-0", VolIDs: []string{"zfs:vm-101-disk-0"}, RecordedAt: replicaEpoch}
	if err := h.RecordReplicaCopy(orphan); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordReplicaCopy(orphan); err != nil {
		t.Fatal(err)
	}
	rep := h.Record().Replication
	if len(rep.Copies) != 1 {
		t.Fatalf("recording a copy twice kept %d", len(rep.Copies))
	}
	if nodes := rep.CopyNodes("zfs:vm-101-disk-0"); !slices.Equal(nodes, []string{"node-a", "node-b", "node-c"}) {
		t.Fatalf("copy nodes %v", nodes)
	}
	if nodes := rep.CopyNodes("zfs:unknown"); len(nodes) != 0 {
		t.Fatalf("an unknown volume has copy nodes %v", nodes)
	}
	if err := h.RecordReplicaCopy(ReplicaCopy{Kind: "stale", Node: "node-c", JobID: "101-0", VolIDs: []string{"zfs:x"}, RecordedAt: replicaEpoch}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an unknown copy kind: %v", err)
	}
	if err := h.ReleaseReplicaCopy(ReplicaCopyOrphan, "node-c", "101-0"); err != nil {
		t.Fatal(err)
	}
	if err := h.ReleaseReplicaCopy(ReplicaCopyOrphan, "node-c", "101-0"); err != nil {
		t.Fatal(err)
	}
	if rep := h.Record().Replication; rep.Copies != nil {
		t.Fatalf("release left %+v", rep.Copies)
	}
}

func TestReplicationRefusesDiskAndTerminalRecords(t *testing.T) {
	j, _ := fixture(t)
	id, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := j.CreateDisk(context.Background(), id, intent())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, disk)
	if err := disk.BeginReplication("director-1", replicaJob("node-a", "node-b"), []string{"zfs:a"}, replicaEpoch, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("a disk record took replication: %v", err)
	}
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	terminalize(t, h, Deleted)
	if err := h.RetireReplicationJob(); !errors.Is(err, ErrConflict) {
		t.Fatalf("a terminal record took a replication write: %v", err)
	}
	// The agent's lock is still held above, so the plain record lives in a
	// journal of its own.
	other, _ := fixture(t)
	vm := acquireVM(t, other)
	if err := vm.RetireReplicationJob(); !errors.Is(err, ErrConflict) {
		t.Fatalf("a record without replication took a replication write: %v", err)
	}
	closeHandle(t, vm)
	if err := vm.RetireReplicationJob(); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed handle wrote: %v", err)
	}
}

func TestReplicationRecordValidation(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	base := h.Record()
	if err := validateRecord(base); err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string]func(*Replication){
		"blank owner":           func(r *Replication) { r.Owner = "" },
		"owner with whitespace": func(r *Replication) { r.Owner = "a b" },
		"no job":                func(r *Replication) { r.Job = nil },
		"job id":                func(r *Replication) { r.Job.ID = "101-1" },
		"same source and target": func(r *Replication) {
			r.Job.Target = r.Job.Source
		},
		"zero generation": func(r *Replication) { r.Job.Generation = 0 },
		"pending skips":   func(r *Replication) { job := *r.Job; job.Generation = 3; r.PendingJob = &job },
		"pending another guest": func(r *Replication) {
			job := replicaJob("node-a", "node-b")
			job.Guest, job.ID, job.Generation = 7, "7-0", 2
			r.PendingJob = &job
		},
		"no volume set":          func(r *Replication) { r.VolumeSets = nil },
		"unsorted volids":        func(r *Replication) { r.VolumeSets[0].VolIDs = []string{"zfs:b", "zfs:a"} },
		"generation gap":         func(r *Replication) { r.VolumeSets[0].Generation = 2 },
		"first set pending":      func(r *Replication) { r.VolumeSets[0].Pending = true },
		"zero change time":       func(r *Replication) { r.VolumeSets[0].ChangedAt = time.Time{} },
		"zero charge":            func(r *Replication) { r.VolumeSets[0].Charges = []ReplicaCharge{{CapacityKey: "k"}} },
		"sync before its change": func(r *Replication) { s := replicaSyncOf(*r.Job, 1, replicaEpoch); r.Sync = &s },
		"sync within the skew": func(r *Replication) {
			s := replicaSyncOf(*r.Job, 1, replicaEpoch.Add(ReplicaClockSkewTolerance))
			r.Sync = &s
		},
		"sync of no set": func(r *Replication) { s := replicaSyncOf(*r.Job, 4, replicaEpoch.Add(time.Hour)); r.Sync = &s },
		"copy without volids": func(r *Replication) {
			r.Copies = []ReplicaCopy{{Kind: ReplicaCopyOrphan, Node: "node-c", JobID: "101-0", RecordedAt: replicaEpoch}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := cloneRecord(base)
			corrupt(r.Replication)
			if err := validateRecord(r); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("validateRecord = %v", err)
			}
		})
	}
	// A pending set in the middle of the history is corrupt too.
	r := cloneRecord(base)
	r.Replication.VolumeSets = append(r.Replication.VolumeSets,
		ReplicaVolumeSet{Generation: 2, VolIDs: []string{"zfs:a"}, ChangedAt: replicaEpoch, Pending: true},
		ReplicaVolumeSet{Generation: 3, VolIDs: []string{"zfs:a"}, ChangedAt: replicaEpoch})
	if err := validateRecord(r); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a pending set mid-history: %v", err)
	}
}

func TestReplicationTransitionRules(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	old := h.Record()
	for name, change := range map[string]func(*Record){
		"state changed":    func(r *Record) { r.Reason = "other" },
		"section removed":  func(r *Record) { r.Replication = nil },
		"owner changed":    func(r *Record) { r.Replication.Owner = "director-2" },
		"job went back":    func(r *Record) { r.Replication.Job = nil },
		"set rewritten":    func(r *Record) { r.Replication.VolumeSets[0].VolIDs = []string{"zfs:z"} },
		"set history lost": func(r *Record) { r.Replication.VolumeSets = nil },
	} {
		t.Run(name, func(t *testing.T) {
			next := cloneRecord(old)
			change(&next)
			if err := validateReplicationTransition(old, next); !errors.Is(err, ErrConflict) {
				t.Fatalf("validateReplicationTransition = %v", err)
			}
		})
	}
	withSync := cloneRecord(old)
	s := replicaSyncOf(*old.Replication.Job, 1, replicaEpoch.Add(time.Hour))
	withSync.Replication.Sync = &s
	dropped := cloneRecord(withSync)
	dropped.Replication.Sync = nil
	if err := validateReplicationTransition(withSync, dropped); !errors.Is(err, ErrConflict) {
		t.Fatalf("sync evidence dropped: %v", err)
	}
}

func TestReplicationCrashBetweenPendingAndConfirm(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	id := h.Record().ID
	if _, err := h.PrepareReplicationJob(replicaJob("node-b", "node-a")); err != nil {
		t.Fatal(err)
	}
	// The process dies before it writes the comment or confirms.
	closeHandle(t, h)
	stored, err := j.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	got := stored.Replication.Candidates()
	if len(got) != 2 || got[0].Generation != 2 || got[0].Source != "node-b" || got[1].Generation != 1 || got[1].Source != "node-a" {
		t.Fatalf("a resumed audit would not accept either generation: %+v", got)
	}

	// A failed write poisons the handle and leaves the previous record.
	ctx := WithSaveFaultForTest(context.Background(), func(Record) error { return errors.New("disk full") })
	resumed, err := j.Acquire(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, resumed)
	if err := resumed.ConfirmReplicationJob(2); err == nil {
		t.Fatal("the faulted confirm succeeded")
	}
	if err := resumed.ConfirmReplicationJob(2); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("a poisoned handle wrote again: %v", err)
	}
	stored, err = j.Inspect(id)
	if err != nil || stored.Replication.PendingJob == nil || stored.Replication.Job.Generation != 1 {
		t.Fatalf("the faulted confirm changed the record: %+v, %v", stored.Replication, err)
	}
}

func TestReplicaSyncOfAnOlderJobGenerationDoesNotCover(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	first := *h.Record().Replication.Job
	if err := h.RecordReplicaSync(replicaSyncOf(first, 1, replicaEpoch.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if !h.Record().Replication.Recoverable() {
		t.Fatal("a sync of the current job generation is not recoverable")
	}
	// The job comes back under the same ID and direction as generation 2,
	// which can have removed the replica the first generation's sync covered.
	generation, err := h.PrepareReplicationJob(replicaJob("node-a", "node-b"))
	if err != nil || generation != 2 {
		t.Fatalf("prepare job = %d, %v", generation, err)
	}
	if err := h.ConfirmReplicationJob(2); err != nil {
		t.Fatal(err)
	}
	rep := h.Record().Replication
	if rep.Job.Generation != 2 || rep.Sync == nil || rep.Sync.JobGeneration != 1 {
		t.Fatalf("expected the generation 1 sync to outlive the confirm: %+v", rep)
	}
	if _, covered := rep.CoveredVolumeSet(*rep.Job); covered || rep.Recoverable() {
		t.Fatal("a sync of job generation 1 covers generation 2")
	}
	if err := h.RecordReplicaSync(replicaSyncOf(*rep.Job, 1, replicaEpoch.Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if !h.Record().Replication.Recoverable() {
		t.Fatal("a sync of generation 2 is not recoverable")
	}
}

func TestReplicaSyncMustFollowTheChangeBeyondTheClockSkew(t *testing.T) {
	j, _ := fixture(t)
	h := replicatedVM(t, j)
	defer closeHandle(t, h)
	job := *h.Record().Replication.Job
	for name, lastSync := range map[string]time.Time{
		"at the change":       replicaEpoch,
		"inside the skew":     replicaEpoch.Add(ReplicaClockSkewTolerance - time.Second),
		"exactly at the skew": replicaEpoch.Add(ReplicaClockSkewTolerance),
	} {
		if err := h.RecordReplicaSync(replicaSyncOf(job, 1, lastSync)); !errors.Is(err, ErrConflict) {
			t.Fatalf("a sync %s: %v", name, err)
		}
	}
	if err := h.RecordReplicaSync(replicaSyncOf(job, 1, replicaEpoch.Add(ReplicaClockSkewTolerance+time.Second))); err != nil {
		t.Fatalf("a sync beyond the skew: %v", err)
	}
	rep := h.Record().Replication
	if !rep.Recoverable() {
		t.Fatalf("a sync beyond the skew does not cover the set: %+v", rep)
	}
	// A record that carries a sync inside the skew covers nothing.
	rep.Sync.LastSync = replicaEpoch.Add(ReplicaClockSkewTolerance - time.Second)
	if _, covered := rep.CoveredVolumeSet(*rep.Job); covered {
		t.Fatal("a sync inside the clock skew covers the set")
	}
}
