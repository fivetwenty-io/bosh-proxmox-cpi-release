package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// hungRestorePVE is the lifecycle flow cluster with two changes. Every
// protection write that PVE answers lands on the VM's config, as it does on a
// real cluster. And once armed, a write that turns protection back on for the
// parker never answers: it blocks until its context ends and then fails with
// the context's error, the way a request to a PVE that stopped responding
// does.
type hungRestorePVE struct {
	*lifecycleFlowPVE
	parker int

	mu      sync.Mutex
	armed   bool
	endedBy error
	// refuse, when set, makes PVE answer the parker's protection restore
	// with this error instead of hanging.
	refuse error
	// dropped counts down the parker protection restores that fail in
	// transport, with no answer from PVE, before the rest go through.
	dropped int
	// protectionWrites lists, in order, the protection value of every write
	// that landed on the parker's config.
	protectionWrites []bool
	// parkerDeletes counts the deletes of the parker that reached PVE.
	parkerDeletes int
}

// landedProtectionWrites returns the protection writes that landed on the
// parker so far, oldest first.
func (c *hungRestorePVE) landedProtectionWrites() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.protectionWrites...)
}

// forgetParkerWrites clears the recorded protection writes and deletes, so a
// test sees only what its own call sends.
func (c *hungRestorePVE) forgetParkerWrites() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.protectionWrites, c.parkerDeletes = nil, 0
}

func (c *hungRestorePVE) deletesOfParker() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parkerDeletes
}

func (c *hungRestorePVE) Nodes() nodes.Service {
	return hungRestoreNodes{
		lifecycleFlowNodes: lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c.lifecycleFlowPVE},
		owner:              c,
	}
}

func (c *hungRestorePVE) outcome() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endedBy
}

func (c *hungRestorePVE) arm(armed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = armed
}

type hungRestoreNodes struct {
	lifecycleFlowNodes
	owner *hungRestorePVE
}

func (n hungRestoreNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	restore := p.Protection != nil && *p.Protection && vmid == strconv.Itoa(n.owner.parker)
	n.owner.mu.Lock()
	armed, refuse := n.owner.armed, n.owner.refuse
	drop := restore && n.owner.dropped > 0
	if drop {
		n.owner.dropped--
	}
	n.owner.mu.Unlock()
	if drop {
		return &sdkerrors.ConnectionError{Host: "pve", Port: 8006, Message: "connection reset by peer"}
	}
	if refuse != nil && restore {
		return refuse
	}
	if armed && p.Protection != nil && *p.Protection && vmid == strconv.Itoa(n.owner.parker) {
		<-ctx.Done()
		n.owner.mu.Lock()
		n.owner.endedBy = ctx.Err()
		n.owner.mu.Unlock()
		return ctx.Err()
	}
	if err := n.lifecycleFlowNodes.UpdateQemuConfig(ctx, node, vmid, p); err != nil {
		return err
	}
	if p.Protection != nil {
		id, err := strconv.Atoi(vmid)
		if err != nil {
			return err
		}
		protection := 0
		if *p.Protection {
			protection = 1
		}
		cfg, ok := n.owner.state.configs[id]
		if !ok {
			return fmt.Errorf("fake: no config for vmid %s", vmid)
		}
		cfg["protection"] = protection
		if id == n.owner.parker {
			n.owner.mu.Lock()
			n.owner.protectionWrites = append(n.owner.protectionWrites, *p.Protection)
			n.owner.mu.Unlock()
		}
	}
	return nil
}

func (n hungRestoreNodes) DeleteQemu(ctx context.Context, node, vmid string, p *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	if vmid == strconv.Itoa(n.owner.parker) {
		n.owner.mu.Lock()
		n.owner.parkerDeletes++
		n.owner.mu.Unlock()
	}
	return n.lifecycleFlowNodes.DeleteQemu(ctx, node, vmid, p)
}

// cutOffRestore is a journal-managed disk whose transfer off its parker
// completed while the write that puts the parker's protection back was cut
// off by its deadline.
type cutOffRestore struct {
	// ctx carries the shortened restore deadline; every call the test makes
	// runs on it.
	ctx        context.Context
	deps       Deps
	client     *lifecycleFlowPVE
	hung       *hungRestorePVE
	journal    *aj.Journal
	id, cid    string
	parker     int
	attachArgs []json.RawMessage
	attachErr  error
	elapsed    time.Duration
}

// newCutOffRestore parks a fresh managed disk, then attaches it again with the
// parker's protection restore hung, and returns the state that leaves. The
// restore deadline is shortened to timeout for the whole test.
func newCutOffRestore(t *testing.T, timeout time.Duration) *cutOffRestore {
	t.Helper()
	c := newParkedRestoreDisk(t, timeout)
	c.hung.arm(true)
	start := time.Now()
	_, c.attachErr = HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	c.elapsed = time.Since(start)
	c.hung.arm(false)
	return c
}

// newParkedRestoreDisk attaches a fresh managed disk to VM 777 and parks it
// again, through the fake that can hang or refuse the parker's protection
// restore. Nothing is armed yet.
func newParkedRestoreDisk(t *testing.T, timeout time.Duration) *cutOffRestore {
	t.Helper()

	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	delete(client.state.configs[777], "scsi1")
	deps.Config.DetachedDiskStrategy = "parked"
	parker := deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{
		"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1,
		"scsihw": "virtio-scsi-pci", "digest": "1",
	}
	c := &cutOffRestore{ctx: pve.WithParkerProtectionRestoreTimeoutForTest(t.Context(), timeout), deps: deps, client: client, journal: journal, id: id, cid: cid, parker: parker}
	c.hung = &hungRestorePVE{lifecycleFlowPVE: client, parker: parker}
	c.deps.PVE = c.hung
	c.attachArgs = []json.RawMessage{json.RawMessage(`"777"`), json.RawMessage(fmt.Sprintf("%q", cid))}
	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	if _, err := HandleDetachDisk(c.deps).Handle(c.ctx, []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup detach into the parker: %v", err)
	}
	if !parkerHolds(client.state.configs[parker]) {
		t.Fatalf("the detach did not park the disk on parker %d: %v", parker, client.state.configs[parker])
	}
	if record := c.record(t); record.State != aj.ReadyToReturn {
		t.Fatalf("parked allocation state = %s, want %s before the hung attach", record.State, aj.ReadyToReturn)
	}
	if desc, _ := pve.ConfigString(client.state.configs[parker], "description"); !strings.Contains(desc, c.record(t).DiskToken) {
		t.Fatalf("the parker records no provenance entry for the disk: %q", desc)
	}
	return c
}

func (c *cutOffRestore) record(t *testing.T) aj.Record {
	t.Helper()
	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// restoreStep returns the planned step the cut-off restore left, failing the
// test when there is none.
func (c *cutOffRestore) restoreStep(t *testing.T) aj.Step {
	t.Helper()
	record := c.record(t)
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Kind == "lifecycle_attach_disk_Nodes_UpdateQemuConfig" && step.Target.VMID == c.parker && step.State == aj.Planned {
			return *step
		}
	}
	t.Fatalf("the cut-off restore left no planned configuration step on parker %d: %+v", c.parker, record.Steps)
	return aj.Step{}
}

func (c *cutOffRestore) setProtection(on bool) {
	value := 0
	if on {
		value = 1
	}
	c.client.state.configs[c.parker]["protection"] = value
}

func (c *cutOffRestore) adopt(t *testing.T) (aj.Record, error) {
	t.Helper()
	return ApplyStorageAllocationDecision(c.ctx, c.deps, c.journal, []string{"n1"},
		StorageAllocationDecision{Action: "adopt", AllocationID: c.id, ExpectedCID: c.cid, DecisionID: "restore-cut-off"})
}

// TestManagedAttachRecordsAHungProtectionRestoreAsUncertain attaches a parked,
// journal-managed disk against a PVE that never answers the write that puts
// the parker's protection back. The restore's deadline has to end that write,
// the allocation has to be left reconciliation_required with the restore step
// planned, because nobody knows whether protection is back on, and the error
// has to name the protection restore and say that the transfer completed.
func TestManagedAttachRecordsAHungProtectionRestoreAsUncertain(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	c := newCutOffRestore(t, timeout)

	if !errors.Is(c.hung.outcome(), context.DeadlineExceeded) {
		t.Fatalf("the hung restore ended with %v, want the restore deadline", c.hung.outcome())
	}
	if c.elapsed < timeout {
		t.Fatalf("attach returned after %s, before the %s restore deadline", c.elapsed, timeout)
	}
	if c.attachErr == nil {
		t.Fatal("attach_disk succeeded although the protection restore's outcome is unknown")
	}
	msg := log.ScrubMessage(c.attachErr.Error())
	want := fmt.Sprintf("protection restore on parker vmid %d", c.parker)
	if !strings.Contains(msg, want) || !strings.Contains(msg, "outcome is unknown") {
		t.Fatalf("error %q does not name the protection restore (%q)", msg, want)
	}
	if !strings.Contains(msg, "the disk transfer to vm 777 slot ") || !strings.Contains(msg, " completed; ") {
		t.Fatalf("error %q does not say that the transfer itself completed", msg)
	}
	t.Logf("attach_disk error: %s", msg)

	if !strings.HasPrefix(msg, "attach_disk: parker protection restore cut off after the disk reached VM 777 as ") {
		t.Fatalf("error %q does not lead with the cut-off restore", msg)
	}
	var typed *cpierrors.Error
	if !errors.As(c.attachErr, &typed) || !typed.OkToRetry() {
		t.Fatalf("error %q is not retriable", msg)
	}

	// The receiving side was recorded after the disk landed: the disk is on
	// VM 777 under its new name, the VM carries the allocation's provenance
	// and the Director's CID, and the parker's entry for the disk is gone.
	record := c.record(t)
	vm := c.client.state.configs[777]
	slot, _ := pve.ConfigString(vm, "scsi1")
	if !strings.Contains(slot, ":777/vm-777-disk-") {
		t.Fatalf("VM 777 scsi1 = %q, want the transferred disk", slot)
	}
	vmDesc, _ := pve.ConfigString(vm, "description")
	if !strings.Contains(vmDesc, c.id) || !strings.Contains(vmDesc, c.cid) {
		t.Fatalf("VM 777's description lacks the allocation provenance or the CID: %q", vmDesc)
	}
	if parkerDesc, _ := pve.ConfigString(c.client.state.configs[c.parker], "description"); strings.Contains(parkerDesc, record.DiskToken) {
		t.Fatalf("the parker still records the disk: %q", parkerDesc)
	}

	// The record needs reconciliation, and the restore is the only thing
	// that does.
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state after the hung restore = %s (reason %q), want %s",
			record.State, record.Reason, aj.ReconciliationRequired)
	}
	step := c.restoreStep(t)
	if got := string(step.Parameters); got != `{"kind":"parker_protection_on","version":1}` {
		t.Fatalf("restore step recorded parameters %s, want the protection-only record", got)
	}
	for i := range record.Steps {
		other := &record.Steps[i]
		if other.ID != step.ID && other.State != aj.Observed {
			t.Fatalf("step %s (%s) is %s; the restore should be the only unsettled step", other.ID, other.Kind, other.State)
		}
	}
}

// TestManagedAttachSurvivesARefusedProtectionRestore has PVE answer the
// parker's protection restore with a failure. The write did not apply, so its
// outcome is known: the attach succeeds and returns the disk, the restore's
// step is observed rather than planned, the guard stays usable for the
// bookkeeping after it, and the warning tells the operator to put protection
// back by hand.
func TestManagedAttachSurvivesARefusedProtectionRestore(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	var logged bytes.Buffer
	logger, err := log.NewLogger("debug", &logged)
	if err != nil {
		t.Fatal(err)
	}
	c.deps.Logger = logger
	c.hung.mu.Lock()
	c.hung.refuse = &sdkerrors.APIError{HTTPCode: 500, Message: "unable to update VM 90000 protection"}
	c.hung.mu.Unlock()

	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("attach_disk failed on a restore PVE refused: %v", err)
	}
	record := c.record(t)
	assertReturnedRecord(t, "refused restore", record)
	refused := 0
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Kind == "lifecycle_attach_disk_Nodes_UpdateQemuConfig" && step.Target.VMID == c.parker && isParkerProtectionParameters(step.Parameters) {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("no protection write was journaled on parker %d: %+v", c.parker, record.Steps)
	}
	if !strings.Contains(logged.String(), "could not restore protection on parker") {
		t.Fatalf("the refused restore logged no warning: %s", logged.String())
	}
	if v, _ := pve.ConfigString(c.client.state.configs[c.parker], "protection"); v != "0" {
		t.Fatalf("parker protection = %q, want it left off by the refused restore", v)
	}
}

// parkerHolds reports whether a parker config has a disk on any bus slot.
func parkerHolds(cfg map[string]any) bool {
	for key := range cfg {
		if isDiskOptionKey(key) && strings.HasPrefix(key, "scsi") {
			return true
		}
	}
	return false
}

// TestManagedAttachSettlesARestoreThatFailedInTransport drops the parker's
// first protection restore in transport, with no answer from PVE, and lets
// the retry through. The dropped write's outcome is unknown, so its step is
// left planned without locking the guard, the retry lands, and the lifecycle
// settles the dropped step by reading the parker back before it completes. The
// attach returns the disk with every step observed.
func TestManagedAttachSettlesARestoreThatFailedInTransport(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.hung.mu.Lock()
	c.hung.dropped = 1
	c.hung.mu.Unlock()

	if _, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("attach_disk failed after a restore that failed once in transport: %v", err)
	}
	record := c.record(t)
	assertReturnedRecord(t, "transport retry", record)
	restores := 0
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Target.VMID == c.parker && step.Parameters != nil && string(step.Parameters) == `{"kind":"parker_protection_on","version":1}` {
			restores++
		}
	}
	if restores < 2 {
		t.Fatalf("journaled %d protection restores on parker %d, want the dropped one and the retry", restores, c.parker)
	}
	if v, _ := pve.ConfigString(c.client.state.configs[c.parker], "protection"); v != "1" {
		t.Fatalf("parker protection = %q after the retry, want it on", v)
	}
}

// TestManagedAttachReportsARestoreThatNeverGotAnAnswer drops every protection
// restore in transport. The outcome stays unknown, so the attach records the
// disk on its VM and returns the retriable cut-off error, and every unsettled
// step the record keeps is a protection restore.
func TestManagedAttachReportsARestoreThatNeverGotAnAnswer(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.hung.mu.Lock()
	c.hung.dropped = 1000
	c.hung.mu.Unlock()

	_, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	if err == nil {
		t.Fatal("attach_disk succeeded although no restore got an answer")
	}
	msg := log.ScrubMessage(err.Error())
	t.Logf("attach_disk error: %s", msg)
	for _, want := range []string{"parker protection restore cut off after the disk reached VM 777", "ended without an answer from PVE", "the disk transfer to vm 777 slot scsi1 completed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("error %q is not retriable", msg)
	}
	record := c.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed && !isParkerProtectionParameters(step.Parameters) {
			t.Fatalf("step %s (%s) is %s; only protection restores may be unsettled", step.ID, step.Kind, step.State)
		}
	}
}

// TestManagedAttachSaysARestoreWasNotAttempted fails the disk move itself in
// transport, which locks the guard, as any failed non-protection write does.
// The protection restore that follows is then refused by the guard before it
// reaches PVE, so the error must say the restore was not attempted because the
// operation was already uncertain, not that it went without an answer, which
// would send an operator to look at the network.
func TestManagedAttachSaysARestoreWasNotAttempted(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.client.moveErr = &sdkerrors.ConnectionError{Host: "pve", Port: 8006, Message: "connection reset by peer"}

	_, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	if err == nil {
		t.Fatal("attach_disk succeeded although its move failed")
	}
	msg := log.ScrubMessage(err.Error())
	t.Logf("attach_disk error: %s", msg)
	want := fmt.Sprintf("protection restore on parker vmid %d was not attempted because the operation was already uncertain", c.parker)
	if !strings.Contains(msg, want) {
		t.Fatalf("error %q does not contain %q", msg, want)
	}
	if strings.Contains(msg, "without an answer from PVE") {
		t.Fatalf("error %q blames PVE for a restore that was never sent", msg)
	}
	if record := c.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
}

// TestManagedAttachKeepsAnAdoptedMoverWhoseRestoreIsCutOff attaches a
// journal-managed disk off a mover an earlier request left on the VM's node,
// and hangs the write that puts the mover's protection back until its
// deadline cuts it off. The record then holds that restore as a planned step,
// and only a read of the mover can settle it, so the attach has to keep the
// mover and send it nothing more: no protection-off write and no delete. The
// attach fails with the retriable cut-off error rather than with a guard that
// refused a delete, and once protection reads on, adopt settles the step.
func TestManagedAttachKeepsAnAdoptedMoverWhoseRestoreIsCutOff(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	c, logged := newAdoptedMoverDisk(t, timeout)
	c.hung.arm(true)
	_, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	c.hung.arm(false)
	writes := c.hung.landedProtectionWrites()
	t.Logf("protection writes that landed on mover %d, oldest first: %v", c.parker, writes)
	logMoverLines(t, logged)

	if !errors.Is(c.hung.outcome(), context.DeadlineExceeded) {
		t.Fatalf("the hung restore ended with %v, want the restore deadline", c.hung.outcome())
	}
	if err == nil {
		t.Fatal("attach_disk succeeded although the mover's protection restore was cut off")
	}
	msg := log.ScrubMessage(err.Error())
	t.Logf("attach_disk error: %s", msg)
	if !strings.Contains(msg, "parker protection restore cut off after the disk reached VM 777") {
		t.Errorf("error %q does not lead with the cut-off restore", msg)
	}
	if strings.Contains(msg, "blocked before Nodes.DeleteQemu") {
		t.Errorf("error %q comes from a delete the guard refused", msg)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Errorf("error %q is not retriable", msg)
	}

	// The transfer's window opens with one protection-off write, and the hung
	// restore never lands, so any write after that first one came after the
	// cut-off.
	if len(writes) != 1 || writes[0] {
		t.Errorf("protection writes that landed on mover %d = %v, want only the transfer's [false]", c.parker, writes)
	}
	if n := c.hung.deletesOfParker(); n != 0 {
		t.Errorf("%d deletes reached mover %d", n, c.parker)
	}
	kept, ok := c.client.state.configs[c.parker]
	if !ok {
		t.Fatalf("mover %d is gone, so nothing can settle its restore", c.parker)
	}
	if lifecycleConfigHasAnyVolume(kept) {
		t.Errorf("mover %d still holds a volume: %v", c.parker, kept)
	}
	if !strings.Contains(logged.String(), "because its protection restore was cut off") {
		t.Errorf("the attach logged no warning saying why it kept mover %d", c.parker)
	}
	if want := fmt.Sprintf("only after a retry of this attach has succeeded and the mover holds no disks, run qm set %d --protection 0 and then qm destroy %d", c.parker, c.parker); !strings.Contains(logged.String(), want) {
		t.Errorf("the keep warning does not say to remove mover %d only after a retry of the attach has succeeded", c.parker)
	}

	record := c.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s (reason %q), want %s", record.State, record.Reason, aj.ReconciliationRequired)
	}
	step := c.restoreStep(t)
	for i := range record.Steps {
		other := &record.Steps[i]
		if other.ID != step.ID && other.State != aj.Observed {
			t.Errorf("step %s (%s, vmid %d) is %s; the restore should be the only unsettled step", other.ID, other.Kind, other.Target.VMID, other.State)
		}
	}

	c.setProtection(true)
	next, err := c.adopt(t)
	if err != nil {
		t.Fatalf("adopt refused a record whose mover reads protected: %s", log.ScrubMessage(err.Error()))
	}
	if next.State != aj.Adopted {
		t.Errorf("adoption produced %s, want %s", next.State, aj.Adopted)
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
		t.Errorf("restore step %s left %s after a protected readback", step.ID, got)
	}
}
