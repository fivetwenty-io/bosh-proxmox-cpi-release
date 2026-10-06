package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// lockedRestorePVE is the cut-off restore's cluster with its sentinel pools on
// a store the test can fill, so a test can hold the parker's lock for another
// request, and with every VM config read reported along with whether the
// settler held the parker's lock at that moment.
type lockedRestorePVE struct {
	*hungRestorePVE
	locks *lockContention
	reads *parkerReads
}

func (c lockedRestorePVE) Pools() pve.PoolService {
	return contendedPools{PoolService: c.hungRestorePVE.Pools(), locks: c.locks}
}

func (c lockedRestorePVE) QEMU() qemu.Service {
	return lockWatchedQEMU{Service: c.hungRestorePVE.QEMU(), locks: c.locks, reads: c.reads}
}

// parkerRead is one config read of the parker: the node it went to, and
// whether the settler's claim on the parker's lock existed while it ran.
type parkerRead struct {
	node   string
	locked bool
}

type parkerReads struct {
	parker int
	mu     sync.Mutex
	seen   []parkerRead
}

func (r *parkerReads) all() []parkerRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]parkerRead(nil), r.seen...)
}

type lockWatchedQEMU struct {
	qemu.Service
	locks *lockContention
	reads *parkerReads
}

func (q lockWatchedQEMU) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if vmid == q.reads.parker {
		locked := settlerHoldsParkerLock(q.locks, vmid)
		q.reads.mu.Lock()
		q.reads.seen = append(q.reads.seen, parkerRead{node: node, locked: locked})
		q.reads.mu.Unlock()
	}
	return q.Service.Config(ctx, node, vmid)
}

// settlerHoldsParkerLock reports whether the parker's sentinel exists and
// names the settler as its owner.
func settlerHoldsParkerLock(locks *lockContention, parker int) bool {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	comment, held := locks.pools[pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", parker))]
	return held && strings.Contains(comment, fmt.Sprintf("settle_protection/%d", parker))
}

// watchParkerLock routes c's sentinel pools to locks and records every read
// of the parker's config.
func (c *cutOffRestore) watchParkerLock(t *testing.T) (*lockContention, *parkerReads) {
	t.Helper()
	locks := newLockContention(t)
	reads := &parkerReads{parker: c.parker}
	c.deps.PVE = lockedRestorePVE{hungRestorePVE: c.hung, locks: locks, reads: reads}
	return locks, reads
}

// assertReadUnderLock fails the test unless the settler read the parker's
// config on node while it held the parker's lock, and left the lock free.
func assertReadUnderLock(t *testing.T, locks *lockContention, reads *parkerReads, parker int, node string) {
	t.Helper()
	found := false
	for _, read := range reads.all() {
		if read.locked && read.node == node {
			found = true
		}
	}
	if !found {
		t.Fatalf("no read of parker %d on node %s ran under the settler's lock; reads were %+v", parker, node, reads.all())
	}
	if settlerHoldsParkerLock(locks, parker) {
		t.Fatalf("the settler left its claim on parker %d's lock behind", parker)
	}
}

// TestCutOffRestoreReadsTheParkerUnderItsLock checks that the settler reads
// the parker's protection only while it holds the parker's lock, on the node
// where it finds the parker now, so it can't see a window another request
// opened on purpose. A refusal for protection that reads off names the lock
// and tells the operator to confirm that no CPI operation holds it before
// running qm set.
func TestCutOffRestoreReadsTheParkerUnderItsLock(t *testing.T) {
	t.Parallel()
	t.Run("protected on its recorded node", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		locks, reads := c.watchParkerLock(t)
		c.setProtection(true)
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with the parker protected: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the parker protected", got)
		}
		assertReadUnderLock(t, locks, reads, c.parker, "n1")
	})
	t.Run("protection off names the lock", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		locks, reads := c.watchParkerLock(t)
		c.setProtection(false)
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("protection is off on parker %d; confirm with pvesh get /pools --poolid bosh-lock-vm-%d that no CPI operation holds the parker's lock, and only when it answers that the pool does not exist, run qm set %d --protection 1 on node n1, then retry", c.parker, c.parker, c.parker))
		assertReadUnderLock(t, locks, reads, c.parker, "n1")
	})
	t.Run("moved and protected", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		locks, reads := c.watchParkerLock(t)
		c.setProtection(true)
		c.moveParkerToN2()
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with the parker protected on n2: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the parker protected on n2", got)
		}
		assertReadUnderLock(t, locks, reads, c.parker, "n2")
	})
	t.Run("moved with protection off", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		locks, reads := c.watchParkerLock(t)
		c.setProtection(false)
		c.moveParkerToN2()
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("VM %d on node n2 carries the bosh-parker tag and its protection is off, and the parker's recorded node is n1", c.parker),
			fmt.Sprintf("confirm with pvesh get /pools --poolid bosh-lock-vm-%d that no CPI operation holds the parker's lock, and only when it answers that the pool does not exist, ", c.parker),
			fmt.Sprintf("run qm set %d --protection 1 on node n2, then retry", c.parker))
		assertReadUnderLock(t, locks, reads, c.parker, "n2")
	})
	t.Run("untagged with protection off", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		locks, reads := c.watchParkerLock(t)
		delete(c.client.state.configs[c.parker], "tags")
		c.setProtection(false)
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("qm config %d", c.parker),
			fmt.Sprintf("confirm with pvesh get /pools --poolid bosh-lock-vm-%d that no CPI operation holds the parker's lock, and only when it answers that the pool does not exist, ", c.parker),
			fmt.Sprintf("qm set %d --protection 1 on node n1", c.parker))
		assertReadUnderLock(t, locks, reads, c.parker, "n1")
	})
}

// TestCutOffRestoreWaitsOutAnotherRequestsParkerLock holds the parker's lock
// for another request whose window has protection off on purpose. The rerun's
// attach_disk must neither settle the step nor tell the operator to run qm
// set. It returns a retriable error that says another operation holds the
// parker's lock, and once that operation ends and protection is back, the
// next rerun settles the step and returns the disk. The parker is on another
// node than the one the record names, so the wait also covers a parker the
// lookup finds elsewhere.
func TestCutOffRestoreWaitsOutAnotherRequestsParkerLock(t *testing.T) {
	t.Parallel()
	for _, moved := range []bool{false, true} {
		name := "on its recorded node"
		if moved {
			name = "moved to another node"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newCutOffRestore(t, 200*time.Millisecond)
			step := c.restoreStep(t)
			locks, reads := c.watchParkerLock(t)
			if moved {
				c.moveParkerToN2()
			}
			c.setProtection(false)
			plantHeldParkerLock(locks, c.parker)
			ctx := shortenManagedLockWait(c.ctx, testManagedLockWait)

			_, err := HandleAttachDisk(c.deps).Handle(ctx, c.attachArgs, jsonrpc.Context{})
			if err == nil {
				t.Fatal("the rerun attach succeeded while another request held the parker's lock")
			}
			t.Logf("refusal: %v", err)
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Errorf("the refusal is not retriable: %v", err)
			}
			want := fmt.Sprintf("another operation on parker %d is in progress and holds the parker's lock bosh-lock-vm-%d; retry once that operation finishes", c.parker, c.parker)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not contain %q", want)
			}
			if strings.Contains(err.Error(), "qm set") {
				t.Errorf("the refusal tells the operator to run qm set while another request holds the lock: %v", err)
			}
			if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
				t.Fatalf("restore step settled to %s while another request held the parker's lock", got)
			}
			for _, read := range reads.all() {
				if read.locked {
					t.Fatalf("the settler read the parker while another request held its lock: %+v", reads.all())
				}
			}

			locks.reset()
			c.setProtection(true)
			if _, err := HandleAttachDisk(c.deps).Handle(ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
				t.Fatalf("rerun attach after the other request finished: %v", err)
			}
			if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
				t.Fatalf("restore step left %s after the other request finished", got)
			}
		})
	}
}

// TestCutOffRestoreRefusesRetriablyWhenTheParkerLockCannotBeTaken fails the
// settler's acquire of the parker's lock in ways that leave its holder
// unknown. The settler must not read the parker, and the rerun gets a
// retriable error that says the lock could not be taken and why, with the
// lock error's cause in both the refusal and the log line. It must neither
// claim that another operation holds the lock nor give a qm set command.
//
//nolint:gocognit // Keep each lock failure's refusal and log assertions in one table.
func TestCutOffRestoreRefusesRetriablyWhenTheParkerLockCannotBeTaken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// fail breaks the acquire on locks for parker.
		fail func(locks *lockContention, parker int)
		// why is what the refusal says failed, and cause is how it renders
		// the lock error behind that.
		why, cause string
	}{
		{
			name: "create without an answer",
			fail: func(locks *lockContention, _ int) {
				locks.mu.Lock()
				locks.createErr = &sdkerrors.ConnectionError{Host: "pve", Port: 8006, Message: "connection reset by peer"}
				locks.mu.Unlock()
			},
			why:   "PVE never confirmed who holds it",
			cause: "connection to pve:8006 failed",
		},
		{
			name: "read of the lock fails",
			fail: func(locks *lockContention, parker int) {
				plantHeldParkerLock(locks, parker)
				locks.mu.Lock()
				locks.plainRead = func(context.Context, string) error {
					return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(403, []byte(`{"data":null,"message":"Permission check failed (/pool/x, Pool.Audit)\n"}`)))
				}
				locks.mu.Unlock()
			},
			why:   "PVE never confirmed who holds it",
			cause: "HTTP 403: Permission check failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCutOffRestore(t, 200*time.Millisecond)
			logged := c.logTo(t)
			step := c.restoreStep(t)
			locks, reads := c.watchParkerLock(t)
			c.setProtection(false)
			tc.fail(locks, c.parker)
			ctx := shortenManagedLockWait(c.ctx, testManagedLockWait)

			_, err := HandleAttachDisk(c.deps).Handle(ctx, c.attachArgs, jsonrpc.Context{})
			if err == nil {
				t.Fatal("the rerun attach succeeded although the parker's lock could not be taken")
			}
			t.Logf("refusal: %v", err)
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Errorf("the refusal is not retriable: %v", err)
			}
			want := []string{
				"the parker's lock could not be taken, so retry",
				fmt.Sprintf("the parker's lock bosh-lock-vm-%d could not be taken to read parker %d, because %s", c.parker, c.parker, tc.why),
				tc.cause,
			}
			for _, part := range want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal does not contain %q", part)
				}
			}
			for _, wrong := range []string{"another operation", "qm set"} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("the refusal says %q although the lock's holder is unknown: %v", wrong, err)
				}
			}
			line := ""
			for _, l := range strings.Split(logged.String(), "\n") {
				if strings.Contains(l, "left the parker protection restore planned because ") {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no log line says why the restore was left planned; logged:\n%s", logged)
			}
			for _, part := range []string{tc.cause, `"held_by_another_operation":false`} {
				if !strings.Contains(line, part) {
					t.Errorf("the log line %s does not contain %q", line, part)
				}
			}
			if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
				t.Fatalf("restore step settled to %s without a read of the parker", got)
			}
			for _, read := range reads.all() {
				if read.locked {
					t.Fatalf("the settler read the parker under a claim it never confirmed: %+v", reads.all())
				}
			}
		})
	}
}

// TestCutOffRestoreOnAClusterWithoutTheParkerLock refuses the create of the
// parker's lock the way a cluster that can't host it does, so the settler
// reads the parker without the lock. A refusal for protection that reads off
// must not send the operator to a lock pool that never exists there. It says
// that the CPI can't take the lock and to put protection back only once no
// BOSH task or storage-journal command runs.
func TestCutOffRestoreOnAClusterWithoutTheParkerLock(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	locks, _ := c.watchParkerLock(t)
	c.setProtection(false)
	locks.mu.Lock()
	locks.createErr = fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(403, []byte(`{"data":null,"message":"Permission check failed (/pool, Pool.Allocate)\n"}`)))
	locks.mu.Unlock()

	_, err := c.adopt(t)
	assertRefusedAndPlanned(t, c, step, err,
		fmt.Sprintf("protection is off on parker %d; the CPI can't take the parker's lock on this cluster, so only when bosh tasks lists no running task and no storage-journal command is running, run qm set %d --protection 1 on node n1, then retry", c.parker, c.parker))
	if strings.Contains(err.Error(), "pvesh") || strings.Contains(err.Error(), "bosh-lock-vm-") {
		t.Errorf("the refusal sends the operator to a lock pool on a cluster that has none: %v", err)
	}
}

// TestParkerLockBusyNeedsEveryUnsettledStepToWaitOnTheLock pins that a
// refusal turns retriable only when every unsettled step of the active
// attempt is a protection step the settler left planned because it could not
// take the parker's lock. A protection step whose parker read off, or any
// other planned step beside it, keeps the refusal as it was, and the
// refusal's own type stays in the chain either way.
func TestParkerLockBusyNeedsEveryUnsettledStepToWaitOnTheLock(t *testing.T) {
	t.Parallel()
	protection := []byte(`{"version":1,"kind":"parker_protection_on"}`)
	record := aj.Record{
		Attempts: []aj.Attempt{{}},
		Steps: []aj.Step{
			{ID: "s0", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Observed, Target: aj.Target{Node: "n1", VMID: 90000}},
			{ID: "s1", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 90000}, Parameters: protection},
			{ID: "s2", Attempt: 0, Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 777}},
		},
	}
	busy := map[string]error{"s1": parkerLockGap(90000, errors.Join(errors.New("held"), pve.ErrClusterLockTimeout))}
	refusal := storageRefusal("lifecycle has unresolved mutation evidence")
	if cpierrors.IsType(parkerLockBusyOr(record, busy, refusal), cpierrors.TypeRetriableCloud) {
		t.Fatal("a refusal with a planned move beside the busy protection step turned retriable")
	}
	record.Steps = record.Steps[:2]
	got := parkerLockBusyOr(record, busy, refusal)
	if !cpierrors.IsType(got, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a refusal whose only unsettled step waits on the parker's lock is not retriable: %v", got)
	}
	var typed *storageRefusalError
	if !errors.As(got, &typed) {
		t.Fatalf("the retriable refusal lost its own type: %v", got)
	}
	if !errors.Is(busy["s1"], pve.ErrClusterLockTimeout) {
		t.Fatalf("the busy gap does not carry the lock timeout: %v", busy["s1"])
	}
	off := map[string]error{"s1": &protectionSettlementGap{text: "protection is off on parker 90000"}}
	if cpierrors.IsType(parkerLockBusyOr(record, off, refusal), cpierrors.TypeRetriableCloud) {
		t.Fatal("a refusal for a parker that read protection off turned retriable")
	}
}

// settlerAcquires counts the claims the settler made on parker's lock, so a
// test can see how often it took the lock.
func settlerAcquires(locks *lockContention, parker int) func() int {
	var mu sync.Mutex
	acquires := 0
	locks.mu.Lock()
	locks.afterCreate = func(pool string) {
		if strings.Contains(locks.pools[pool], fmt.Sprintf("settle_protection/%d", parker)) {
			mu.Lock()
			acquires++
			mu.Unlock()
		}
	}
	locks.mu.Unlock()
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return acquires
	}
}

// TestSettlerTakesTheParkerLockOncePerParker drops every protection restore
// of an attach in transport, which leaves several planned protection steps on
// the same parker. Adopt settles all of them under a single acquire of the
// parker's lock, so the journal lock it holds meanwhile waits at most one lock
// wait per parker.
func TestSettlerTakesTheParkerLockOncePerParker(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.hung.mu.Lock()
	c.hung.dropped = 1000
	c.hung.mu.Unlock()
	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err == nil {
		t.Fatal("attach_disk succeeded although no restore got an answer")
	}
	c.hung.mu.Lock()
	c.hung.dropped = 0
	c.hung.mu.Unlock()
	var planned []string
	for _, step := range c.record(t).Steps {
		if step.State == aj.Planned && step.Target.VMID == c.parker && isParkerProtectionParameters(step.Parameters) {
			planned = append(planned, step.ID)
		}
	}
	if len(planned) < 2 {
		t.Fatalf("the dropped restores left %d planned protection steps on parker %d, want at least two", len(planned), c.parker)
	}

	locks, reads := c.watchParkerLock(t)
	acquires := settlerAcquires(locks, c.parker)
	c.setProtection(true)
	if _, err := c.adopt(t); err != nil {
		t.Fatalf("adopt with the parker protected: %v", err)
	}
	if got := acquires(); got != 1 {
		t.Fatalf("the settler took parker %d's lock %d times for %d steps, want once", c.parker, got, len(planned))
	}
	record := c.record(t)
	for _, id := range planned {
		if got := stepState(t, record, id); got != aj.Observed {
			t.Fatalf("restore step %s left %s with the parker protected", id, got)
		}
	}
	assertReadUnderLock(t, locks, reads, c.parker, "n1")
}

// releasePlantingPVE is lockedRestorePVE with a hook that runs once the
// parker's lock sentinel is deleted, which is where another request can take
// the lock between a window's release and the settler's acquire.
type releasePlantingPVE struct {
	lockedRestorePVE
	released func(pool string)
}

func (c releasePlantingPVE) Pools() pve.PoolService {
	return releasePlantingPools{contendedPools: contendedPools{PoolService: c.hungRestorePVE.Pools(), locks: c.locks}, released: c.released}
}

type releasePlantingPools struct {
	contendedPools
	released func(pool string)
}

func (p releasePlantingPools) DeletePool(ctx context.Context, id string) error {
	err := p.contendedPools.DeletePool(ctx, id)
	if err == nil {
		p.released(id)
	}
	return err
}

// TestCompletionWaitsOutAnotherRequestsParkerLock drops the attach's first
// protection restore in transport and lets the retry land, which leaves the
// dropped step for the completion to settle. Another request takes the
// parker's lock as soon as the window releases it, so the completion can't
// read the parker. The attach returns a retriable error that says another
// operation holds the lock, with no qm set command, and the rerun attach
// settles the step once that operation ends.
func TestCompletionWaitsOutAnotherRequestsParkerLock(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.hung.mu.Lock()
	c.hung.dropped = 1
	c.hung.mu.Unlock()
	locks, _ := c.watchParkerLock(t)
	sentinel := pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", c.parker))
	var once sync.Once
	c.deps.PVE = releasePlantingPVE{
		lockedRestorePVE: c.deps.PVE.(lockedRestorePVE),
		released: func(pool string) {
			if pool == sentinel {
				once.Do(func() { plantHeldParkerLock(locks, c.parker) })
			}
		},
	}
	ctx := shortenManagedLockWait(c.ctx, testManagedLockWait)

	_, err := HandleAttachDisk(c.deps).Handle(ctx, c.attachArgs, jsonrpc.Context{})
	if err == nil {
		t.Fatal("attach_disk succeeded while another request held the parker's lock")
	}
	t.Logf("attach_disk error: %v", err)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("the error is not retriable: %v", err)
	}
	for _, want := range []string{
		"another operation on the parker is in progress, so retry",
		fmt.Sprintf("another operation on parker %d is in progress and holds the parker's lock %s; retry once that operation finishes", c.parker, sentinel),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not contain %q", want)
		}
	}
	if strings.Contains(err.Error(), "qm set") {
		t.Errorf("the error tells the operator to run qm set while another request holds the lock: %v", err)
	}
	planned := 0
	for _, step := range c.record(t).Steps {
		if step.State == aj.Planned && isParkerProtectionParameters(step.Parameters) {
			planned++
		}
	}
	if planned == 0 {
		t.Fatal("the dropped restore's step settled while another request held the parker's lock")
	}

	locks.reset()
	if _, err := HandleAttachDisk(c.deps).Handle(ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun attach after the other request finished: %v", err)
	}
	assertReturnedRecord(t, "rerun", c.record(t))
}

// TestAbsentDiskFinalizationWaitsOutAnotherRequestsParkerLock cuts off the
// protection restore of a delete_disk that removed a volume the parker owns,
// so the rerun finds the volume gone and only finalizes the record. While
// another request holds the parker's lock, that rerun returns a retriable
// error that says so, with no qm set command, and leaves the step planned.
// Once the lock is free, the next rerun settles the step and deletes the
// record.
func TestAbsentDiskFinalizationWaitsOutAnotherRequestsParkerLock(t *testing.T) {
	t.Parallel()
	c := newOwnedParkerRestoreDisk(t, 200*time.Millisecond)
	token := c.record(t).DiskToken
	deleteArgs := []json.RawMessage{planJSON(t, c.cid)}
	c.hung.arm(true)
	_, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{})
	c.hung.arm(false)
	if err == nil {
		t.Fatal("delete_disk with a hung restore succeeded")
	}
	if slots := parkerSlotsCarrying(c.client, c.parker, token); len(slots) != 0 {
		t.Fatalf("parker %d still holds the disk in %v after the cut-off delete", c.parker, slots)
	}
	var step aj.Step
	for _, s := range c.record(t).Steps {
		if s.State == aj.Planned && s.Target.VMID == c.parker && isParkerProtectionParameters(s.Parameters) {
			step = s
		}
	}
	if step.ID == "" {
		t.Fatalf("the cut-off delete left no planned protection step on parker %d", c.parker)
	}

	locks, _ := c.watchParkerLock(t)
	c.setProtection(false)
	plantHeldParkerLock(locks, c.parker)
	ctx := shortenManagedLockWait(c.ctx, testManagedLockWait)
	_, err = HandleDeleteDisk(c.deps).Handle(ctx, deleteArgs, jsonrpc.Context{})
	if err == nil {
		t.Fatal("the rerun delete_disk succeeded while another request held the parker's lock")
	}
	t.Logf("rerun delete_disk error: %v", err)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("the error is not retriable: %v", err)
	}
	for _, want := range []string{
		"another operation on the parker is in progress, so retry",
		fmt.Sprintf("another operation on parker %d is in progress and holds the parker's lock bosh-lock-vm-%d; retry once that operation finishes", c.parker, c.parker),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not contain %q", want)
		}
	}
	if strings.Contains(err.Error(), "qm set") {
		t.Errorf("the error tells the operator to run qm set while another request holds the lock: %v", err)
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
		t.Fatalf("restore step settled to %s while another request held the parker's lock", got)
	}

	locks.reset()
	c.setProtection(true)
	if _, err := HandleDeleteDisk(c.deps).Handle(ctx, deleteArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun delete_disk after the other request finished: %v", err)
	}
	record := c.record(t)
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the rerun delete_disk", step.ID, got)
	}
	if record.State != aj.Deleted {
		t.Fatalf("allocation state after the rerun delete_disk = %s (reason %q), want %s", record.State, record.Reason, aj.Deleted)
	}
}
