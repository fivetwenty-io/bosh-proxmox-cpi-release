package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// replicaJobsCluster answers PVE's replication job list over another cluster
// service and counts the reads, so a test can prove which rules read it.
type replicaJobsCluster struct {
	sdkcluster.Service
	jobs  []map[string]any
	err   error
	reads int
}

func (c *replicaJobsCluster) ListReplication(context.Context) (*sdkcluster.ListReplicationResponse, error) {
	c.reads++
	if c.err != nil {
		return nil, c.err
	}
	out := sdkcluster.ListReplicationResponse{}
	for _, job := range c.jobs {
		raw, err := json.Marshal(job)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

// replicaAuditClient is the audit fixture's client with the replication
// job list added.
type replicaAuditClient struct {
	*allocationAuditClient
	replication *replicaJobsCluster
}

func (c replicaAuditClient) Cluster() sdkcluster.Service {
	c.replication.Service = c.allocationAuditClient.Cluster()
	return c.replication
}

const replicaOwner = "5b0e2f4c-9d1a-4c3e-8f7a-6b5c4d3e2f1a"

var replicaEpoch = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

// replicaFixture is VM 123 of a returned VM allocation created on pve1, whose
// root and persistent disk sit on the node-local storage "local" and are
// replicated from pve1 to pve2, and whose ephemeral disk sits on the shared
// storage "a". node is where the VM is found. rep and pveJob describe what
// the journal and PVE hold, and build turns them into records, the VM
// configuration, and listings that agree with each other.
type replicaFixture struct {
	t    *testing.T
	deps Deps
	c    *allocationAuditClient
	jobs *replicaJobsCluster

	node string
	rep  aj.Replication
	// pveJob is the job PVE lists; nil lists none.
	pveJob map[string]any
	// attached adds node-local volumes to the VM configuration.
	attached []string
	// listedOn adds nodes whose listings hold every node-local volume.
	listedOn []string
	// otherJobs are further entries in PVE's job list, after pveJob.
	otherJobs []map[string]any

	vm, disk          aj.Record
	root, eph, pdisk  string
	frozenDefinitions map[string]pve.StorageInfo
}

func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	deps, _, c := auditFixture(t)
	jobs := &replicaJobsCluster{}
	deps.PVE = replicaAuditClient{allocationAuditClient: c, replication: jobs}
	f := &replicaFixture{t: t, deps: deps, c: c, jobs: jobs, node: "pve2", root: "local:123/vm-123-disk-0.qcow2", eph: "a:123/vm-123-disk-1.qcow2", pdisk: "local:123/vm-123-disk-2.qcow2"}
	job := aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve1", Target: "pve2", Generation: 1}
	f.rep = aj.Replication{
		Owner:      replicaOwner,
		Job:        &job,
		VolumeSets: []aj.ReplicaVolumeSet{{Generation: 1, VolIDs: []string{f.root, f.pdisk}, ChangedAt: replicaEpoch, Charges: []aj.ReplicaCharge{{CapacityKey: "pve2/local", DomainKey: "pve2", Bytes: 1 << 30}}}},
		Sync:       &aj.ReplicaSync{JobID: "123-0", JobGeneration: 1, Source: "pve1", Target: "pve2", VolumeSetGeneration: 1, LastSync: replicaEpoch.Add(time.Hour), VerifiedAt: replicaEpoch.Add(time.Hour + time.Minute)},
	}
	f.pveJob = map[string]any{"id": "123-0", "type": "local", "guest": 123, "jobnum": 0, "target": "pve2", "source": "pve1", "schedule": "*/15", "comment": renderReplicaJobMarker(replicaOwner, 1)}
	return f
}

func (f *replicaFixture) build() []aj.Record {
	t := f.t
	f.frozenDefinitions = map[string]pve.StorageInfo{"a": moveDefinition(t, moveDefinitions["a"]), "local": moveDefinition(t, moveDefinitions["local"])}
	f.c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	owned := append([]string{f.root, f.eph}, f.attached...)
	f.vm = moveVMRecord(t, "agent", "pve1", 123, f.frozenDefinitions, owned...)
	rep := f.rep
	f.vm.Replication = &rep
	f.disk = moveDiskRecord(t, "pve1", f.pdisk, f.frozenDefinitions)

	config := map[string]any{"virtio0": f.root, "scsi1": f.eph, "scsi2": f.pdisk + ",serial=bpd-0"}
	for index, volume := range f.attached {
		config[fmt.Sprintf("scsi%d", index+3)] = volume
	}
	provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: f.disk.ID, AllocationNamespace: "director", Volid: f.pdisk, Node: "pve1", Backing: f.frozenDefinitions["local"].BackingKey()}
	config["description"] = moveDescription(t, moveMarker(t, f.vm.ID, "agent"), provenance)
	f.c.configs[123] = config
	f.c.clusterRead = &allocationAuditCluster{Service: f.c.idFakeClient.Cluster(), members: moveNodes}
	f.c.nodesRead.nodeNames = moveNodes
	f.c.nodesRead.guestNodes = map[int]string{123: f.node}

	// Shared volumes list on every node. A node-local volume lists on the
	// node the VM runs on, on every node the replication keeps a copy on,
	// and on each extra node a case names.
	local := append([]string{f.root, f.pdisk}, f.attached...)
	f.c.nodesRead.volumesByNode = map[string][]string{}
	for _, node := range moveNodes {
		f.c.nodesRead.volumesByNode[node] = append(f.c.nodesRead.volumesByNode[node], f.eph)
		for _, volume := range local {
			if node == f.node || slices.Contains(rep.CopyNodes(volume), node) || slices.Contains(f.listedOn, node) {
				f.c.nodesRead.volumesByNode[node] = append(f.c.nodesRead.volumesByNode[node], volume)
			}
		}
	}
	f.jobs.jobs = nil
	if f.pveJob != nil {
		f.jobs.jobs = []map[string]any{f.pveJob}
	}
	f.jobs.jobs = append(f.jobs.jobs, f.otherJobs...)
	return []aj.Record{f.vm, f.disk}
}

func (f *replicaFixture) audit() StorageAllocationAudit {
	f.t.Helper()
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, f.build(), nil, []string{"pve1"})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

// requireClean fails unless the audit is complete with no finding.
func requireClean(t *testing.T, report StorageAllocationAudit) {
	t.Helper()
	if len(report.Conflicts) != 0 || len(report.Issues) != 0 || !report.Complete || !report.VMScanComplete {
		t.Fatalf("audit not clean:\nconflicts: %s\nissues: %s", strings.Join(report.Conflicts, "\n"), strings.Join(report.Issues, "\n"))
	}
}

// requireMoveRefused fails unless the VM's move raised a conflict with reason.
func requireMoveRefused(t *testing.T, report StorageAllocationAudit, vmID, reason string) {
	t.Helper()
	if !slices.ContainsFunc(report.Conflicts, func(conflict string) bool {
		return strings.Contains(conflict, vmID) && strings.Contains(conflict, "not accepted as a move because "+reason)
	}) {
		t.Fatalf("no refusal %q:\nconflicts: %s\nissues: %s", reason, strings.Join(report.Conflicts, "\n"), strings.Join(report.Issues, "\n"))
	}
	if slices.ContainsFunc(report.ObservedMoves, func(move StorageAllocationMove) bool { return move.AllocationID == vmID }) {
		t.Fatalf("a refused VM was accepted: %+v", report.ObservedMoves)
	}
}

func (f *replicaFixture) vmMove(report StorageAllocationAudit) (StorageAllocationMove, bool) {
	index := slices.IndexFunc(report.ObservedMoves, func(move StorageAllocationMove) bool {
		return move.AllocationID == f.vm.ID && move.Kind == "vm"
	})
	if index < 0 {
		return StorageAllocationMove{}, false
	}
	return report.ObservedMoves[index], true
}

func TestReplicaJobMarkerRoundTripAndStrictParse(t *testing.T) {
	comment := renderReplicaJobMarker(replicaOwner, 7)
	if comment != "bosh-cpi:v1 owner="+replicaOwner+" gen=7" {
		t.Fatalf("marker = %q", comment)
	}
	marker, ok := parseReplicaJobMarker(comment + " note=kept")
	if !ok || marker.Owner != replicaOwner || marker.Generation != 7 {
		t.Fatalf("parse = %+v, %v", marker, ok)
	}
	for _, bad := range []string{
		"",
		"nightly replication",
		"bosh-cpi:v2 owner=x gen=1",
		"bosh-cpi:v1 gen=1",
		"bosh-cpi:v1 owner=x",
		"bosh-cpi:v1 owner=- gen=1",
		"bosh-cpi:v1 owner=x gen=0",
		"bosh-cpi:v1 owner=x gen=-1",
		"bosh-cpi:v1 owner=x gen=one",
		"bosh-cpi:v1 owner=x gen=1 gen=2",
		"bosh-cpi:v1 owner=x owner=y gen=1",
	} {
		if marker, ok := parseReplicaJobMarker(bad); ok {
			t.Errorf("%q parsed as %+v", bad, marker)
		}
	}
}

// TestAllocationAuditAcceptsVerifiedReplicaFailover is the case the rule
// exists for. PVE HA recovered VM 123 on the replication partner, the last
// verified sync covers every node-local volume it holds, and the job's marker
// names the recorded owner and generation. The VM and its persistent disk are
// accepted, and the replicas on both nodes are the records' own volumes.
func TestAllocationAuditAcceptsVerifiedReplicaFailover(t *testing.T) {
	f := newReplicaFixture(t)
	report := f.audit()
	requireClean(t, report)
	move, found := f.vmMove(report)
	if !found {
		t.Fatalf("failover not accepted: %+v", report.ObservedMoves)
	}
	want := StorageReplicaMove{JobID: "123-0", JobGeneration: 1, Direction: storageReplicaDirectionOriginal, VolumeSetGeneration: 1, Failover: true}
	if move.Replication == nil || *move.Replication != want || move.ObservedNode != "pve2" || !slices.Equal(move.RecordedNodes, []string{"pve1"}) {
		t.Fatalf("move = %+v (replication %+v)", move, move.Replication)
	}
	if !slices.Equal(move.Volumes, []string{f.eph, f.root, f.pdisk}) {
		t.Fatalf("move volumes = %v", move.Volumes)
	}
	if !report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
		t.Fatalf("the persistent disk did not move with its holder: %+v", report.ObservedMoves)
	}
	if f.jobs.reads != 1 {
		t.Fatalf("replication jobs read %d times", f.jobs.reads)
	}
	if err := storageAuditGateError(context.Background(), f.deps, "VM cleanup", report, storageAuditGateAll); err != nil {
		t.Fatalf("VM cleanup gate refused an accepted failover: %v", err)
	}
	proof, err := storageAllocationVerification(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proof.EvidenceJSON, `"replication":{"direction":"original","failover":true,"job_generation":1,"job_id":"123-0","volume_set_generation":1}`) {
		t.Fatalf("verification evidence lost the replication proof: %s", proof.EvidenceJSON)
	}
}

// TestAllocationAuditAcceptsReplicaFailoverAfterPVEReversal covers PVE
// reversing the job when it recovered the guest while no CPI ran: the job now
// replicates back to the old source, and its comment is untouched.
func TestAllocationAuditAcceptsReplicaFailoverAfterPVEReversal(t *testing.T) {
	for _, source := range []string{"pve2", ""} {
		t.Run("source="+source, func(t *testing.T) {
			f := newReplicaFixture(t)
			f.pveJob["target"], f.pveJob["source"] = "pve1", source
			if source == "" {
				delete(f.pveJob, "source")
			}
			f.pveJob["disable"] = 1
			report := f.audit()
			requireClean(t, report)
			move, found := f.vmMove(report)
			if !found || move.Replication.Direction != storageReplicaDirectionReversed {
				t.Fatalf("reversed job not accepted: %+v", report.ObservedMoves)
			}
		})
	}
}

// TestAllocationAuditRefusesReplicaSightingOnAThirdNode needs no PVE read:
// the journal names pve1 and pve2, so a VM on pve3 is refused at once.
func TestAllocationAuditRefusesReplicaSightingOnAThirdNode(t *testing.T) {
	f := newReplicaFixture(t)
	f.node = "pve3"
	report := f.audit()
	requireMoveRefused(t, report, f.vm.ID, "pve3 is neither the replication source pve1 nor its partner pve2 that the record names")
	if f.jobs.reads != 0 {
		t.Fatalf("a journal-only refusal read the replication jobs %d times", f.jobs.reads)
	}
	if report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve3") {
		t.Fatal("the disk moved with a refused holder")
	}
	if err := storageAuditGateError(context.Background(), f.deps, "VM cleanup", report, storageAuditGateAll); cpiErrorType(err) != cpierrors.TypeCloud {
		t.Fatalf("a refused replica sighting did not refuse cleanup: %v", err)
	}
}

func TestAllocationAuditRefusesUnprovenReplicaJobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *replicaFixture)
		reason string
	}{
		{"stale generation", func(f *replicaFixture) {
			job := *f.rep.Job
			job.Generation = 2
			f.rep.Job = &job
			f.rep.Sync.JobGeneration = 2
		}, "replication job 123-0 carries generation 1, not 2"},
		{"reused job ID without marker", func(f *replicaFixture) { f.pveJob["comment"] = "nightly replication" }, "replication job 123-0 carries no CPI ownership marker"},
		{"reused job ID without comment", func(f *replicaFixture) { delete(f.pveJob, "comment") }, "replication job 123-0 carries no CPI ownership marker"},
		{"reused job ID of another owner", func(f *replicaFixture) {
			f.pveJob["comment"] = renderReplicaJobMarker("other-director", 1)
		}, "replication job 123-0 is marked for Director other-director, not " + replicaOwner},
		{"job missing", func(f *replicaFixture) { f.pveJob = nil }, "replication job 123-0 is not defined in PVE"},
		{"job marked for removal", func(f *replicaFixture) { f.pveJob["remove_job"] = "full" }, "replication job 123-0 is marked for removal"},
		{"job for another guest", func(f *replicaFixture) { f.pveJob["guest"] = 124 }, "replication job 123-0 names guest 124 job 0, not guest 123 job 0"},
		{"job to a third node", func(f *replicaFixture) { f.pveJob["target"] = "pve3" }, "replication job 123-0 runs from pve1 to pve3, neither its recorded direction from pve1 to pve2 nor the reverse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplicaFixture(t)
			tc.mutate(f)
			report := f.audit()
			requireMoveRefused(t, report, f.vm.ID, tc.reason)
			if report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
				t.Fatal("the disk moved with a refused holder")
			}
			if !findingWith(report.Conflicts, "disk ownership provenance disagrees", "the replicated move of holder VM 123 was not accepted") {
				t.Fatalf("the disk's refusal does not name its holder:\n%s", strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

func TestAllocationAuditRefusesReplicaTheLastSyncDoesNotCover(t *testing.T) {
	newVolume := "local:123/vm-123-disk-3.qcow2"
	for _, tc := range []struct {
		name   string
		mutate func(f *replicaFixture)
		reason string
	}{
		{"newly attached volume after the last sync", func(f *replicaFixture) {
			f.attached = []string{newVolume}
			f.rep.VolumeSets = append(f.rep.VolumeSets, aj.ReplicaVolumeSet{Generation: 2, VolIDs: []string{f.root, f.pdisk, newVolume}, ChangedAt: replicaEpoch.Add(2 * time.Hour)})
		}, "the last verified sync covered volume set 1, not the current volume set 2"},
		{"sync started before the attach", func(f *replicaFixture) {
			f.attached = []string{newVolume}
			f.rep.VolumeSets = append(f.rep.VolumeSets, aj.ReplicaVolumeSet{Generation: 2, VolIDs: []string{f.root, f.pdisk, newVolume}, ChangedAt: replicaEpoch.Add(2 * time.Hour)})
			f.rep.Sync.VolumeSetGeneration = 2
		}, "the last verified sync ran at 2026-11-01T09:00:00Z, before volume set 2 changed at 2026-11-01T10:00:00Z"},
		{"attach the journal never recorded", func(f *replicaFixture) { f.attached = []string{newVolume} },
			"VM 123 holds the node-local volumes [" + "local:123/vm-123-disk-0.qcow2 local:123/vm-123-disk-2.qcow2 " + newVolume + "], which match no replica volume set the record allows (set 1 [local:123/vm-123-disk-0.qcow2 local:123/vm-123-disk-2.qcow2])"},
		{"no verified sync", func(f *replicaFixture) { f.rep.Sync = nil }, "no sync of the replication was ever verified"},
		{"sync through another direction", func(f *replicaFixture) { f.rep.Sync.Target = "pve3" }, "the last verified sync ran through job 123-0 from pve1 to pve3, not through job 123-0 from pve1 to pve2"},
		{"retired job", func(f *replicaFixture) { f.rep.Retired = true }, "the replication job to pve2 is retired, so its replica was not kept current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplicaFixture(t)
			tc.mutate(f)
			report := f.audit()
			requireMoveRefused(t, report, f.vm.ID, tc.reason)
			if f.jobs.reads != 0 {
				t.Fatalf("a journal-only refusal read the replication jobs %d times", f.jobs.reads)
			}
		})
	}
}

// TestAllocationAuditAcceptsEitherGenerationAcrossTheMarkerCrashWindow covers
// a CPI that recorded the swapped job's pending generation and died before it
// wrote the comment, or after it wrote the comment and before it confirmed.
// Either marker proves the replica.
func TestAllocationAuditAcceptsEitherGenerationAcrossTheMarkerCrashWindow(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		generation             uint64
		target, source, wantID string
	}{
		{"comment not yet written", 1, "pve2", "pve1", "123-0"},
		{"comment written, not confirmed", 2, "pve1", "pve2", "123-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplicaFixture(t)
			f.rep.PendingJob = &aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve2", Target: "pve1", Generation: 2}
			f.pveJob["comment"] = renderReplicaJobMarker(replicaOwner, tc.generation)
			f.pveJob["target"], f.pveJob["source"] = tc.target, tc.source
			report := f.audit()
			requireClean(t, report)
			move, found := f.vmMove(report)
			if !found || move.Replication.JobGeneration != tc.generation || move.Replication.Direction != storageReplicaDirectionOriginal {
				t.Fatalf("crash window not accepted: %+v", report.ObservedMoves)
			}
		})
	}
	// A third generation proves neither.
	f := newReplicaFixture(t)
	f.rep.PendingJob = &aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve2", Target: "pve1", Generation: 2}
	f.pveJob["comment"] = renderReplicaJobMarker(replicaOwner, 3)
	requireMoveRefused(t, f.audit(), f.vm.ID, "replication job 123-0 carries generation 3, not 2; replication job 123-0 carries generation 3, not 1")
}

// TestAllocationAuditJudgesAPendingVolumeSetByTheNode covers a crash after
// the journal recorded a pending volume set and before the attach ran. On the
// source the VM may hold either set, because the attach may or may not have
// happened. On the partner no verified sync can cover the change, so the
// sighting is refused.
func TestAllocationAuditJudgesAPendingVolumeSetByTheNode(t *testing.T) {
	newVolume := "local:123/vm-123-disk-3.qcow2"
	swapped := func(f *replicaFixture) {
		// The VM failed over to pve2 earlier and the CPI confirmed the swap,
		// so pve2 is its source while its steps still name pve1.
		f.rep.Job = &aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve2", Target: "pve1", Generation: 2}
		f.rep.Sync = &aj.ReplicaSync{JobID: "123-0", JobGeneration: 2, Source: "pve2", Target: "pve1", VolumeSetGeneration: 1, LastSync: replicaEpoch.Add(time.Hour), VerifiedAt: replicaEpoch.Add(time.Hour + time.Minute)}
		f.rep.VolumeSets = append(f.rep.VolumeSets, aj.ReplicaVolumeSet{Generation: 2, VolIDs: []string{f.root, f.pdisk, newVolume}, ChangedAt: replicaEpoch.Add(2 * time.Hour), Pending: true})
		f.jobs.err = errors.New("replication reads are not needed on the source")
	}
	for _, attached := range [][]string{nil, {newVolume}} {
		t.Run(fmt.Sprintf("attached=%d", len(attached)), func(t *testing.T) {
			f := newReplicaFixture(t)
			swapped(f)
			f.attached = attached
			report := f.audit()
			requireClean(t, report)
			move, found := f.vmMove(report)
			want := uint64(1 + len(attached))
			if !found || move.Replication.Failover || move.Replication.VolumeSetGeneration != want || move.Replication.JobID != "" {
				t.Fatalf("source sighting = %+v, want set %d", report.ObservedMoves, want)
			}
			if f.jobs.reads != 0 {
				t.Fatalf("a sighting on the source read the replication jobs %d times", f.jobs.reads)
			}
		})
	}
	f := newReplicaFixture(t)
	f.rep.VolumeSets = append(f.rep.VolumeSets, aj.ReplicaVolumeSet{Generation: 2, VolIDs: []string{f.root, f.pdisk, newVolume}, ChangedAt: replicaEpoch.Add(2 * time.Hour), Pending: true})
	requireMoveRefused(t, f.audit(), f.vm.ID, "volume set 2 of the replication is still pending, so no verified sync covers the VM's volumes")
}

// TestAllocationAuditJudgesAFailbackOnAStepNode covers a VM that returned to
// the node its steps name after the CPI confirmed the swap to pve2. The step
// alone would accept it, so the replicated rule judges it as a failover.
func TestAllocationAuditJudgesAFailbackOnAStepNode(t *testing.T) {
	f := newReplicaFixture(t)
	f.node = "pve1"
	f.rep.Job = &aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve2", Target: "pve1", Generation: 2}
	f.rep.Sync = &aj.ReplicaSync{JobID: "123-0", JobGeneration: 2, Source: "pve2", Target: "pve1", VolumeSetGeneration: 1, LastSync: replicaEpoch.Add(time.Hour), VerifiedAt: replicaEpoch.Add(time.Hour + time.Minute)}
	f.pveJob["comment"] = renderReplicaJobMarker(replicaOwner, 2)
	f.pveJob["target"], f.pveJob["source"] = "pve1", "pve2"
	report := f.audit()
	requireClean(t, report)
	move, found := f.vmMove(report)
	if !found || !move.Replication.Failover || move.ObservedNode != "pve1" {
		t.Fatalf("failback not judged as a failover: %+v", report.ObservedMoves)
	}
	f = newReplicaFixture(t)
	f.node = "pve1"
	f.rep.Job = &aj.ReplicaJob{ID: "123-0", Guest: 123, JobNum: 0, Source: "pve2", Target: "pve1", Generation: 2}
	f.rep.Sync = nil
	requireMoveRefused(t, f.audit(), f.vm.ID, "no sync of the replication was ever verified")
}

// TestAllocationAuditAcceptsRecordedOrphanReplica lists the root volume on
// pve3 as well. A recorded orphan copy there is the record's own volume; the
// same listing without the record is a volume outside its targets.
func TestAllocationAuditAcceptsRecordedOrphanReplica(t *testing.T) {
	f := newReplicaFixture(t)
	f.node = "pve1"
	f.rep.Copies = []aj.ReplicaCopy{{Kind: aj.ReplicaCopyOrphan, Node: "pve3", JobID: "123-1", VolIDs: []string{f.root}, RecordedAt: replicaEpoch}}
	report := f.audit()
	requireClean(t, report)
	if len(report.ObservedMoves) != 0 {
		t.Fatalf("a VM on its source moved: %+v", report.ObservedMoves)
	}
	f = newReplicaFixture(t)
	f.node = "pve1"
	f.listedOn = []string{"pve3"}
	report = f.audit()
	if !findingWith(report.Issues, "known volume "+f.root+` on storage "local" on node "pve3"`, f.vm.ID) {
		t.Fatalf("an unrecorded copy was not flagged:\nissues: %s\nconflicts: %s", strings.Join(report.Issues, "\n"), strings.Join(report.Conflicts, "\n"))
	}
}

// TestAllocationAuditDefersReplicaMoveOnFailedReplicationRead leaves the move
// undecided, and the cleanup gate defers with a retriable error. A conflict
// beside it still refuses for good.
func TestAllocationAuditDefersReplicaMoveOnFailedReplicationRead(t *testing.T) {
	f := newReplicaFixture(t)
	f.jobs.err = errors.New("596 connection timed out")
	report := f.audit()
	if len(report.Conflicts) != 0 || report.Complete {
		t.Fatalf("failed read refused or completed the audit: %+v", report.Conflicts)
	}
	if !findingWith(report.Issues, "remote allocation "+f.vm.ID, "move undecided ("+storageAuditReasonReplicationUnread+"): replication jobs could not be listed") {
		t.Fatalf("no undecided issue: %s", strings.Join(report.Issues, "\n"))
	}
	if !findingWith(report.Issues, "disk ownership provenance disagrees", "the replicated move of holder VM 123 is undecided") {
		t.Fatalf("the disk was not left undecided with its holder: %s", strings.Join(report.Issues, "\n"))
	}
	err := storageAuditGateError(context.Background(), f.deps, "VM cleanup", report, storageAuditGateAll)
	if cpiErrorType(err) != cpierrors.TypeRetriableCloud || !strings.Contains(err.Error(), "VM cleanup deferred") {
		t.Fatalf("failed read did not defer: %v (%s)", err, cpiErrorType(err))
	}
	if err := storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts); err != nil {
		t.Fatalf("an undecided move refused admission: %v", err)
	}
	report.addConflict("another conflict", "")
	if err := storageAuditGateError(context.Background(), f.deps, "VM cleanup", report, storageAuditGateAll); cpiErrorType(err) != cpierrors.TypeCloud {
		t.Fatalf("a conflict beside the deferral did not refuse: %v", err)
	}
	// A malformed listing is a failed read too.
	f = newReplicaFixture(t)
	f.pveJob = map[string]any{"guest": 123}
	report = f.audit()
	if !findingWith(report.Issues, "remote allocation "+f.vm.ID, "replication jobs could not be listed: entry 0 is malformed") {
		t.Fatalf("malformed listing: %s", strings.Join(report.Issues, "\n"))
	}
}

// TestAllocationAuditIgnoresMalformedJobsOfAnotherGuest pins that an entry in
// PVE's job list that provably belongs to another VMID does not defer this
// VM's audit, however malformed it is, while an entry that names this VM or
// no readable VMID still does.
func TestAllocationAuditIgnoresMalformedJobsOfAnotherGuest(t *testing.T) {
	foreign := map[string]map[string]any{
		"unusable body, guest field":     {"id": "456-0", "guest": 456, "type": []int{1}},
		"no id, guest field":             {"guest": 456, "target": "pve2"},
		"no id, string guest field":      {"guest": "456"},
		"no guest field, id names guest": {"id": "456-0", "type": []int{1}},
	}
	for name, entry := range foreign {
		t.Run("foreign "+name, func(t *testing.T) {
			f := newReplicaFixture(t)
			f.otherJobs = []map[string]any{entry}
			report := f.audit()
			requireClean(t, report)
			if _, found := f.vmMove(report); !found {
				t.Fatalf("failover not accepted beside a foreign malformed job: %+v", report.ObservedMoves)
			}
			if err := storageAuditGateError(context.Background(), f.deps, "VM cleanup", report, storageAuditGateAll); err != nil {
				t.Fatalf("a foreign malformed job deferred the cleanup gate: %v", err)
			}
		})
	}
	t.Run("foreign repeated ID", func(t *testing.T) {
		f := newReplicaFixture(t)
		other := map[string]any{"id": "456-0", "type": "local", "guest": 456, "jobnum": 0, "target": "pve3", "schedule": "*/15"}
		f.otherJobs = []map[string]any{other, other}
		requireClean(t, f.audit())
	})
	deferring := map[string]struct {
		entry  map[string]any
		reason string
	}{
		"names this VM":                 {map[string]any{"id": "123-1", "guest": 123, "type": []int{1}}, "entry 1 is malformed"},
		"names this VM only by its ID":  {map[string]any{"id": "123-1", "type": []int{1}}, "entry 1 is malformed"},
		"names no guest":                {map[string]any{"target": "pve2"}, "entry 1 is malformed"},
		"guest field and ID disagree":   {map[string]any{"id": "123-1", "guest": 456, "type": []int{1}}, "entry 1 is malformed"},
		"guest field names no VMID":     {map[string]any{"id": "x-1", "guest": "none", "type": []int{1}}, "entry 1 is malformed"},
		"repeats this VM's job ID":      {map[string]any{"id": "123-0", "type": "local", "guest": 123, "jobnum": 0, "target": "pve2"}, "job 123-0 is listed twice"},
		"repeats an ID, guest disagree": {map[string]any{"id": "123-0", "type": "local", "guest": 456, "jobnum": 0, "target": "pve2"}, "job 123-0 is listed twice"},
	}
	for name, tc := range deferring {
		t.Run("defers "+name, func(t *testing.T) {
			f := newReplicaFixture(t)
			f.otherJobs = []map[string]any{tc.entry}
			report := f.audit()
			if len(report.Conflicts) != 0 || report.Complete {
				t.Fatalf("a job of this VM or of no known guest did not defer: %+v", report.Conflicts)
			}
			if !findingWith(report.Issues, "remote allocation "+f.vm.ID, "replication jobs could not be listed: "+tc.reason) {
				t.Fatalf("no undecided issue: %s", strings.Join(report.Issues, "\n"))
			}
		})
	}
}

// TestAllocationAuditReplicaMovesKeepTheSharedRule pins that a record whose
// replication names no job, like every record without one, follows the
// shared-storage rule exactly, and that a shared move serializes without a
// replication field.
func TestAllocationAuditReplicaMovesKeepTheSharedRule(t *testing.T) {
	plain := newMoveFixture(t)
	want := plain.audit()
	if len(want.ObservedMoves) == 0 || want.ObservedMoves[0].Replication != nil {
		t.Fatalf("shared move = %+v", want.ObservedMoves)
	}
	raw, err := json.Marshal(want.ObservedMoves[0])
	if err != nil || strings.Contains(string(raw), "replication") {
		t.Fatalf("shared move serialized %s, %v", raw, err)
	}
	refused := newMoveFixture(t)
	refused.rootStorage = "local"
	got := refused.audit()
	if !findingWith(got.Conflicts, "remote allocation ", "not accepted as a move because volume local:123/vm-123-disk-0.qcow2 is node-local") {
		t.Fatalf("shared rule changed: %s", strings.Join(got.Conflicts, "\n"))
	}

	f := newReplicaFixture(t)
	f.rep.Job, f.rep.Retired, f.rep.Sync = nil, true, nil
	report := f.audit()
	requireMoveRefused(t, report, f.vm.ID, "volume "+f.root+" is node-local")
	if f.jobs.reads != 0 {
		t.Fatal("the shared rule read the replication jobs")
	}
}

// seedReplicatedVMRecord writes, through the journal's own methods, a
// returned VM allocation for VM 123 created on pve1 with root on the
// node-local storage "local" and eph on the shared storage "a", whose root
// is replicated from pve1 to pve2 by a confirmed job with a verified sync.
func seedReplicatedVMRecord(t *testing.T, journal *aj.Journal, c *allocationAuditClient, root, eph string) string {
	t.Helper()
	frozen := map[string]pve.StorageInfo{"a": moveDefinition(t, moveDefinitions["a"]), "local": moveDefinition(t, moveDefinitions["local"])}
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	intent := movePlan(t, "agent", "pve1", frozen, root, eph)
	intent.IntentFingerprint = strings.Repeat("b", 64)
	handle, err := journal.AcquireVM(context.Background(), "agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range []string{root, eph} {
		storage := moveStorage(volume)
		target := aj.Target{Node: "pve1", VMID: 123, Storage: storage, Backing: frozen[storage].BackingKey(), IntendedVolume: volume}
		step, err := storageMutationIntent(handle, "vm.QEMU.Create", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
			t.Fatal(err)
		}
	}
	record := handle.Record()
	record.State, record.CID = aj.ReadyToReturn, "123"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().UTC().Add(-2 * time.Hour)
	job := aj.ReplicaJob{ID: "123-0", Guest: 123, Source: "pve1", Target: "pve2"}
	if err := handle.BeginReplication(replicaOwner, job, []string{root}, changed, nil); err != nil {
		t.Fatal(err)
	}
	if err := handle.ConfirmReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	sync := aj.ReplicaSync{JobID: "123-0", JobGeneration: 1, Source: "pve1", Target: "pve2", VolumeSetGeneration: 1, LastSync: changed.Add(time.Hour), VerifiedAt: changed.Add(time.Hour + time.Minute)}
	if err := handle.RecordReplicaSync(sync); err != nil {
		t.Fatal(err)
	}
	vmID := handle.Record().ID
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return vmID
}

// TestManagedVMCleanupGateWithReplicatedMove runs the gate of a managed VM
// cleanup over a journal whose VM record carries a replication written by
// the journal's own methods. A verified failover passes, an unproven one
// refuses, and a failed replication read defers.
func TestManagedVMCleanupGateWithReplicatedMove(t *testing.T) {
	for _, tc := range []struct {
		name     string
		comment  string
		readErr  error
		wantType cpierrors.Type
	}{
		{"verified", renderReplicaJobMarker(replicaOwner, 1), nil, ""},
		{"unmarked job", "hand made", nil, cpierrors.TypeCloud},
		{"failed read", "", errors.New("596 connection timed out"), cpierrors.TypeRetriableCloud},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, journal, c := auditFixture(t)
			jobs := &replicaJobsCluster{err: tc.readErr, jobs: []map[string]any{{"id": "123-0", "type": "local", "guest": 123, "jobnum": 0, "target": "pve2", "comment": tc.comment}}}
			deps.PVE = replicaAuditClient{allocationAuditClient: c, replication: jobs}
			root, eph := "local:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"
			vmID := seedReplicatedVMRecord(t, journal, c, root, eph)
			c.configs[123] = map[string]any{"virtio0": root, "scsi1": eph, "description": moveMarker(t, vmID, "agent")}
			c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: moveNodes}
			c.nodesRead.nodeNames = moveNodes
			c.nodesRead.guestNodes = map[int]string{123: "pve2"}
			c.nodesRead.volumesByNode = map[string][]string{"pve1": {root, eph}, "pve2": {root, eph}, "pve3": {eph}}

			nodes, audit, err := auditManagedVMCleanup(context.Background(), deps, journal)
			if tc.wantType == "" {
				if err != nil {
					t.Fatalf("verified replicated move refused cleanup: %v", err)
				}
				if len(nodes) == 0 || !audit.observedMove("vm", vmID, 123, "pve2") {
					t.Fatalf("cleanup audit = %v %+v", nodes, audit.ObservedMoves)
				}
				return
			}
			if cpiErrorType(err) != tc.wantType || nodes != nil || audit.Records != nil {
				t.Fatalf("cleanup gate = %v (%s), nodes %v", err, cpiErrorType(err), nodes)
			}
			if !strings.Contains(directorMessage(err), "VM 123") {
				t.Fatalf("refusal does not name the VM: %q", directorMessage(err))
			}
		})
	}
}

func cpiErrorType(err error) cpierrors.Type {
	var typed *cpierrors.Error
	if errors.As(err, &typed) {
		return typed.Type()
	}
	return ""
}

// TestSiblingChargesCountUnverifiedReplicas charges the replica against the
// partner until a verified sync covers the newest set, and for the skew
// window after it.
func TestSiblingChargesCountUnverifiedReplicas(t *testing.T) {
	f := newReplicaFixture(t)
	f.build()
	snapshot := replicaEpoch.Add(48 * time.Hour)
	charge := func(mutate func(rep *aj.Replication)) uint64 {
		t.Helper()
		record := f.vm
		rep := f.rep
		sync := *f.rep.Sync
		rep.Sync = &sync
		rep.VolumeSets = slices.Clone(f.rep.VolumeSets)
		mutate(&rep)
		record.Replication = &rep
		member, domain, _, err := siblingCharges([]aj.Record{record}, "self", "", "", snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if member["pve2/local"] != domain["pve2"] {
			t.Fatalf("member %v and domain %v disagree", member, domain)
		}
		return member["pve2/local"]
	}
	if got := charge(func(*aj.Replication) {}); got != 0 {
		t.Fatalf("a replica verified long ago still charges %d", got)
	}
	if got := charge(func(rep *aj.Replication) { rep.Sync.VerifiedAt = snapshot.Add(-time.Minute) }); got != 1<<30 {
		t.Fatalf("a replica verified within the skew window charges %d", got)
	}
	if got := charge(func(rep *aj.Replication) { rep.Sync = nil }); got != 1<<30 {
		t.Fatalf("an unsynced replica charges %d", got)
	}
	if got := charge(func(rep *aj.Replication) {
		rep.PendingJob = &aj.ReplicaJob{ID: "123-0", Guest: 123, Source: "pve2", Target: "pve1", Generation: 2}
	}); got != 1<<30 {
		t.Fatalf("a replica under a pending job charges %d", got)
	}
	if got := charge(func(rep *aj.Replication) {
		rep.VolumeSets = append(rep.VolumeSets, aj.ReplicaVolumeSet{Generation: 2, VolIDs: []string{f.root}, ChangedAt: snapshot, Pending: true, Charges: []aj.ReplicaCharge{{CapacityKey: "pve2/local", DomainKey: "pve2", Bytes: 2 << 30}}})
	}); got != 2<<30 {
		t.Fatalf("a pending volume set charges %d", got)
	}
	if got := charge(func(rep *aj.Replication) { rep.Retired, rep.Sync = true, nil }); got != 0 {
		t.Fatalf("a retired replication charges %d", got)
	}
	record := f.vm
	record.State = aj.Deleted
	rep := f.rep
	rep.Sync = nil
	record.Replication = &rep
	if member, _, _, err := siblingCharges([]aj.Record{record}, "self", "", "", snapshot); err != nil || len(member) != 0 {
		t.Fatalf("a deleted record charges %v, %v", member, err)
	}
	if member, _, _, err := siblingCharges([]aj.Record{record}, record.ID, "", "", snapshot); err != nil || len(member) != 0 {
		t.Fatalf("our own record charges %v, %v", member, err)
	}
}
