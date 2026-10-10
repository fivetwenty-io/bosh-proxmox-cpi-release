package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// detachLockBugRecord attaches a fresh managed disk to VM 777 and then
// detaches it under the parked strategy while every sentinel create fails with
// an outcome the guard cannot classify. The first lock the detach takes is the
// disk's own per-disk transfer lock, so the record it leaves behind holds one
// planned detach lock step whose sentinel is bosh-lock-disk-<stable ID>.
func detachLockBugRecord(t *testing.T) (*parkedFlowDisk, *lockContention, aj.Step) {
	t.Helper()
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	d.parker = parker
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()
	locks.createErr = errors.New("connection reset by peer")
	if err := d.detach(t); err == nil {
		t.Fatal("the replayed lock failure did not fail the detach")
	}
	locks.reset()
	record := d.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("replayed record is %s, want %s", record.State, aj.ReconciliationRequired)
	}
	var planned []aj.Step
	for i := range record.Steps {
		if record.Steps[i].Attempt == record.ActiveAttempt() && record.Steps[i].State != aj.Observed {
			planned = append(planned, record.Steps[i])
		}
	}
	if len(planned) != 1 || planned[0].Kind != "lifecycle_detach_disk_Pool_CreatePool" || planned[0].State != aj.Planned {
		t.Fatalf("replayed record does not hold one planned detach lock step: %+v", record.Steps)
	}
	return d, locks, planned[0]
}

func (d *parkedFlowDisk) detach(t *testing.T) error {
	t.Helper()
	_, err := HandleDetachDisk(d.deps).Handle(t.Context(), []json.RawMessage{planJSON(t, "777"), planJSON(t, d.cid)}, jsonrpc.Context{})
	return err
}

// TestLockStepSentinelsTakeTheDiskLockFromTheStep pins where the per-disk
// transfer lock candidate comes from. A planned lock step adds the disk lock
// its own parameters name, whatever its operation or record kind, and a step
// that names none, an observed one, a non-lock step, and parameters that are
// anything but the guard's exact shape all leave it out.
func TestLockStepSentinelsTakeTheDiskLockFromTheStep(t *testing.T) {
	const token = "bpd-00112233aabbccdd"
	diskLock := pve.DiskTransferLockPoolName(token)
	params := diskTransferLockStepParameters(diskLock)
	if params == nil {
		t.Fatal("the guard records no parameters for a per-disk lock sentinel")
	}
	if got := diskTransferLockStepParameters(pve.ClusterLockPoolName("vm-90000")); got != nil {
		t.Fatalf("a parker lock sentinel recorded parameters %s", got)
	}
	if _, err := aj.MutationParameters(params); err != nil {
		t.Fatalf("the journal refuses the per-disk lock parameters: %v", err)
	}
	step := func(kind string, state aj.State, parameters json.RawMessage) aj.Step {
		return aj.Step{Kind: kind, State: state, Target: aj.Target{Node: "n1", VMID: 90000}, Parameters: parameters}
	}
	raw := func(text string) json.RawMessage { return json.RawMessage(text) }
	for name, tc := range map[string]struct {
		record aj.Record
		want   bool
	}{
		"detach lock step naming the disk":           {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, params)}}, true},
		"detach unlock step naming the disk":         {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_DeletePool", aj.Planned, params)}}, true},
		"preserve lock step naming the disk":         {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_delete_vm.preserve_disk_Pool_CreatePool", aj.Planned, params)}}, true},
		"attach resume lock step naming the disk":    {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_attach_disk_Pool_CreatePool", aj.Planned, params)}}, true},
		"disk record without a token":                {aj.Record{Kind: "disk", Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, params)}}, true},
		"detach lock step naming no disk":            {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, nil)}}, false},
		"attach lock step naming no disk":            {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_attach_disk_Pool_CreatePool", aj.Planned, nil)}}, false},
		"create_disk park lock step":                 {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("park_Pool_CreatePool", aj.Planned, nil)}}, false},
		"observed lock step naming the disk":         {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Observed, params), step("lifecycle_attach_disk_Pool_CreatePool", aj.Planned, nil)}}, false},
		"configuration step with lock parameters":    {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Nodes_UpdateQemuConfig", aj.Planned, params)}}, false},
		"lock step of another kind of parameters":    {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, raw(`{"kind":"parker_protection_on","resources":"`+token+`","version":1}`))}}, false},
		"lock step whose token needs sanitizing":     {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, raw(`{"kind":"disk_transfer_lock","resources":"bpd/0011","version":1}`))}}, false},
		"lock step with an extra parameter":          {aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, raw(`{"comment":"x","kind":"disk_transfer_lock","resources":"`+token+`","version":1}`))}}, false},
		"lock step from an attempt no longer active": {aj.Record{Kind: "disk", DiskToken: token, Attempts: []aj.Attempt{{}, {}}, Steps: []aj.Step{step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, params), {Kind: "lifecycle_attach_disk_Pool_CreatePool", State: aj.Planned, Attempt: 1, Target: aj.Target{Node: "n1", VMID: 90000}}}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := lockStepSentinels(tc.record)
			if slices.Contains(got, diskLock) != tc.want {
				t.Fatalf("sentinel candidates = %v, want the per-disk lock: %t", got, tc.want)
			}
		})
	}

	t.Run("one candidate per disk", func(t *testing.T) {
		other := pve.DiskTransferLockPoolName("bpd-99887766ffeeddcc")
		record := aj.Record{Kind: "disk", DiskToken: token, Steps: []aj.Step{
			step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, params),
			step("lifecycle_detach_disk_Pool_DeletePool", aj.Planned, params),
			step("lifecycle_detach_disk_Pool_CreatePool", aj.Planned, diskTransferLockStepParameters(other)),
		}}
		got := lockStepSentinels(record)
		want := []string{diskLock, other, pve.ClusterLockPoolName("vm-90000")}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("sentinel candidates = %v, want %v", got, want)
		}
	})
}

// TestDetachLockBugRecordReadsThePerDiskLock replays a detach whose per-disk
// transfer lock create failed with an outcome the guard could not classify.
// The record's candidates include the per-disk lock, and when PVE does not
// answer that read exactly the refusal names it, so settlement never passes a
// step whose own sentinel it did not read.
func TestDetachLockBugRecordReadsThePerDiskLock(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	record := disk.record(t)
	diskLock := pve.DiskTransferLockPoolName(record.DiskToken)
	if got := lockStepSentinels(record); !slices.Contains(got, diskLock) {
		t.Fatalf("settlement reads %v for the detach record, want it to include %s", got, diskLock)
	}
	locks.readErr = poolVerdictError("permission check failed for /pool/" + diskLock + " (Pool.Audit)")
	_, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err == nil || !strings.Contains(err.Error(), "step "+planned.ID+" (lifecycle_detach_disk_Pool_CreatePool) is planned") {
		t.Fatalf("adopt did not name the unsettled detach lock step: %v", err)
	}
	if !strings.Contains(err.Error(), "PVE did not answer exactly for sentinel "+diskLock) {
		t.Fatalf("adopt did not name the per-disk lock it could not read: %v", err)
	}
	if step := stepByID(t, disk.record(t), planned.ID); step.State != aj.Planned {
		t.Fatalf("an ambiguous read settled the detach lock step: %s", step.State)
	}
}

// TestDetachLockBugRecordSettlesByOwnerlessReadback covers the same record
// once PVE answers. No sentinel holds a claim, so the readback proves the
// planned create left nothing behind, settles the step, and lets the adopt
// through.
func TestDetachLockBugRecordSettlesByOwnerlessReadback(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	next, err := ApplyStorageAllocationDecision(t.Context(), disk.deps, disk.journal, []string{"n1"}, adoptDecision(disk))
	if err != nil {
		t.Fatalf("adopt refused the detach record: %v", err)
	}
	if step := stepByID(t, next, planned.ID); step.State != aj.Observed {
		t.Fatalf("adoption left the detach lock step %s", step.State)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("settlement created or left a sentinel: %v", locks.pools)
	}
}

// TestDetachLockBugRecordHealsOnDetachRetry is the Director's retry of the
// detach that left the record behind. The planned lock step is settled by
// readback, the detach parks the disk, and no sentinel is left behind.
func TestDetachLockBugRecordHealsOnDetachRetry(t *testing.T) {
	disk, locks, planned := detachLockBugRecord(t)
	if err := disk.detach(t); err != nil {
		t.Fatalf("the retried detach failed: %v", err)
	}
	record := disk.record(t)
	if step := stepByID(t, record, planned.ID); step.State != aj.Observed {
		t.Fatalf("the retry left the detach lock step %s", step.State)
	}
	if _, attached := disk.client.state.configs[777]["scsi1"]; attached {
		t.Fatal("the retried detach left the disk on VM 777")
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("the retried detach left a sentinel behind: %v", locks.pools)
	}
}

// diskLockReadPools serves a fixture's sentinels, logs each raw read, and
// fails the read of one sentinel with an answer settlement can't classify
// while fail is set.
type diskLockReadPools struct {
	pve.PoolService
	sentinel string
	fail     error
	log      *sentinelReadLog
}

func (p diskLockReadPools) ReadPoolComment(ctx context.Context, id string) (string, error) {
	p.log.mu.Lock()
	p.log.reads = append(p.log.reads, id)
	p.log.mu.Unlock()
	if id == p.sentinel && p.fail != nil {
		return "", p.fail
	}
	reader, ok := p.PoolService.(pve.RawPoolCommentReader)
	if !ok {
		return "", errors.New("the fixture's pool service cannot read sentinels")
	}
	return reader.ReadPoolComment(ctx, id)
}

// diskLockReadClient is a fixture client whose pool service is pools.
type diskLockReadClient struct {
	pve.Client
	pools diskLockReadPools
}

func (c diskLockReadClient) Pools() pve.PoolService { return c.pools }

// TestDiskLockStepSettlesForEveryTransferOperation covers each operation that
// takes a disk's per-disk transfer lock: detach_disk and
// delete_vm.preserve_disk under the disk's record, and
// delete_vm_retain_ephemeral and delete_vm_preserve_legacy under the VM's. A
// record holds one planned create of that lock in the shape the lifecycle
// guard writes, with the disk's stable ID in its parameters. While PVE answers
// that sentinel's read ambiguously, the step stays planned and the gap names
// the sentinel, so the step never passes without its own sentinel read back.
// Once PVE answers that no such sentinel exists, the ownerless readback
// settles the step and leaves no sentinel behind.
func TestDiskLockStepSettlesForEveryTransferOperation(t *testing.T) {
	const vmToken = "bpd-00112233aabbccdd"
	diskRecord := func(operation string) func(t *testing.T) (Deps, *aj.Journal, *lockContention, string, string) {
		return func(t *testing.T) (Deps, *aj.Journal, *lockContention, string, string) {
			var step, token string
			deps, client, journal, id, _ := lifecycleFlowFixtureWith(t, false, false, func(h *aj.Handle) {
				birth := h.Record().Steps[0].Target
				token = h.Record().DiskToken
				var err error
				step, err = storageMutationIntent(h, "lifecycle_"+operation+"_Pool_CreatePool",
					aj.Target{Node: birth.Node, Storage: birth.Storage, Backing: birth.Backing, IntendedVolume: birth.IntendedVolume},
					nil, diskTransferLockStepParameters(pve.DiskTransferLockPoolName(token)))
				if err != nil {
					t.Fatal(err)
				}
			})
			locks := newLockContention(t)
			deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks}
			return deps, journal, locks, id, step + "|" + token
		}
	}
	vmRecord := func(operation string) func(t *testing.T) (Deps, *aj.Journal, *lockContention, string, string) {
		return func(t *testing.T) (Deps, *aj.Journal, *lockContention, string, string) {
			var step string
			deps, j, client, record := vmPoolStepRecord(t, nil, true, func(h *aj.Handle) {
				backing := h.Record().Steps[0].Target.Backing
				var err error
				step, err = storageMutationIntent(h, "lifecycle_"+operation+"_Pool_CreatePool",
					aj.Target{Node: "pve1", Storage: "a", Backing: backing, IntendedVolume: "a:123/vm-123-disk-1.qcow2"},
					nil, diskTransferLockStepParameters(pve.DiskTransferLockPoolName(vmToken)))
				if err != nil {
					t.Fatal(err)
				}
			})
			return deps, j, client.locks, record.ID, step + "|" + vmToken
		}
	}
	for _, tc := range []struct {
		operation, kind string
		build           func(t *testing.T) (Deps, *aj.Journal, *lockContention, string, string)
	}{
		{"detach_disk", "disk", diskRecord("detach_disk")},
		{"delete_vm.preserve_disk", "disk", diskRecord("delete_vm.preserve_disk")},
		{"delete_vm_retain_ephemeral", "vm", vmRecord("delete_vm_retain_ephemeral")},
		{"delete_vm_preserve_legacy", "vm", vmRecord("delete_vm_preserve_legacy")},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			deps, journal, locks, id, stepAndToken := tc.build(t)
			step, token, _ := strings.Cut(stepAndToken, "|")
			assertDiskLockStepSettles(t, deps, journal, locks, tc.kind, id, step, token)
		})
	}
}

// assertDiskLockStepSettles settles the record id twice: first while PVE
// answers the step's own per-disk lock sentinel ambiguously, which must leave
// the step planned with a gap naming that sentinel, and then while PVE answers
// that the sentinel is missing, which must settle the step and leave no
// sentinel behind.
func assertDiskLockStepSettles(t *testing.T, deps Deps, journal *aj.Journal, locks *lockContention,
	kind, id, step, token string,
) {
	t.Helper()
	diskLock := pve.DiskTransferLockPoolName(token)
	log := &sentinelReadLog{}
	pools := diskLockReadPools{PoolService: deps.PVE.Pools(), sentinel: diskLock, log: log,
		fail: poolVerdictError("permission check failed for /pool/" + diskLock + " (Pool.Audit)")}
	deps.PVE = diskLockReadClient{Client: deps.PVE, pools: pools}

	settle := func() (aj.Record, map[string]error) {
		t.Helper()
		handle, err := journal.Acquire(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := handle.Close(); err != nil {
				t.Error(err)
			}
		}()
		if handle.Record().Kind != kind {
			t.Fatalf("the record is a %s record, want %s", handle.Record().Kind, kind)
		}
		gaps, err := settlePlannedLockSteps(t.Context(), deps, handle)
		if err != nil {
			t.Fatalf("settlement failed: %v", err)
		}
		return handle.Record(), gaps
	}

	record, gaps := settle()
	if !slices.Contains(log.names(), diskLock) {
		t.Fatalf("settlement read %v, never the step's own sentinel %s", log.names(), diskLock)
	}
	if stepByID(t, record, step).State != aj.Planned {
		t.Fatal("an ambiguous read of the step's own sentinel settled the step")
	}
	if gap := gaps[step]; gap == nil || !strings.Contains(gap.Error(), "PVE did not answer exactly for sentinel "+diskLock) {
		t.Fatalf("gap = %v, want it to name the per-disk lock it could not read", gap)
	}

	pools.fail = nil
	deps.PVE = diskLockReadClient{Client: deps.PVE.(diskLockReadClient).Client, pools: pools}
	record, gaps = settle()
	if len(gaps) != 0 {
		t.Fatalf("gaps = %v, want the ownerless readback to settle the step", gaps)
	}
	if stepByID(t, record, step).State != aj.Observed {
		t.Fatalf("the readback left the step %s", stepByID(t, record, step).State)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("settlement created or left a sentinel: %v", locks.pools)
	}
}
