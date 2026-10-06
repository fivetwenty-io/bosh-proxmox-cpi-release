package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// settleProtectionForTest runs the protection settler on its own against a
// held record.
func settleProtectionForTest(ctx context.Context, deps Deps, handle *aj.Handle) (map[string]error, error) {
	return settlePlannedProtectionSteps(ctx, deps, handle, nil, nil)
}

// logTo points every request c makes at a debug logger and returns the
// buffer it writes to.
func (c *cutOffRestore) logTo(t *testing.T) *bytes.Buffer {
	t.Helper()
	logged := &bytes.Buffer{}
	logger, err := log.NewLogger("debug", logged)
	if err != nil {
		t.Fatal(err)
	}
	c.deps.Logger = logger
	return logged
}

// moveParkerToN2 places the parker's config on n2, the way a migration from
// its recorded node n1 does.
func (c *cutOffRestore) moveParkerToN2() {
	if c.client.vmNodes == nil {
		c.client.vmNodes = map[int]string{}
	}
	c.client.vmNodes[c.parker] = "n2"
}

// settledWarning finds the warning the settler logged for step, failing the
// test when there is none, checks that it names every part of want, and
// returns it.
func settledWarning(t *testing.T, logged *bytes.Buffer, stepID string, want ...string) string {
	t.Helper()
	for _, line := range strings.Split(logged.String(), "\n") {
		if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, "settled the parker protection restore of step "+stepID+" ") {
			continue
		}
		t.Logf("warning: %s", line)
		for _, part := range want {
			if !strings.Contains(line, part) {
				t.Errorf("the settlement warning does not contain %q: %s", part, line)
			}
		}
		return line
	}
	t.Fatalf("no warning names the settlement of step %s; the log was:\n%s", stepID, logged.String())
	return ""
}

// assertNoClaim fails the test when text says any of claims, each of which
// the reads behind it never proved.
func assertNoClaim(t *testing.T, text string, claims ...string) {
	t.Helper()
	for _, claim := range claims {
		if strings.Contains(text, claim) {
			t.Errorf("%q claims %q, which the reads don't prove", text, claim)
		}
	}
}

// recordedNodeDown fails every read of a VM config sent to the parker's
// recorded node n1 with the status PVE's proxy answers for a node that
// doesn't respond, whichever VM the read is for. A VM that must stay
// readable while n1 is down has to live on another node.
func (c *cutOffRestore) recordedNodeDown() {
	c.client.onNodeConfigRead = func(node string, _ int) error {
		if node == "n1" {
			return sdkerrors.ParseAPIError(595, []byte(`{"message":"Connection refused"}`))
		}
		return nil
	}
}

// moveDiskHolderToN2 places VM 777, which holds the disk, on the live node
// n2, so that its config still reads while the recorded node n1 is down.
func (c *cutOffRestore) moveDiskHolderToN2() {
	if c.client.vmNodes == nil {
		c.client.vmNodes = map[int]string{}
	}
	c.client.vmNodes[777] = "n2"
}

// assertRefusedAndPlanned checks that err is a refusal that contains every
// part of want, and that the restore step is still planned.
func assertRefusedAndPlanned(t *testing.T, c *cutOffRestore, step aj.Step, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the call succeeded, want a refusal containing %q", want)
	}
	t.Logf("refusal: %v", err)
	for _, part := range want {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("refusal %q does not contain %q", err, part)
		}
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
		t.Fatalf("restore step settled to %s, want it left planned", got)
	}
}

// TestCutOffRestoreFollowsAParkerThatMoved covers a parker that a migration
// took to another node after its restore was cut off. The settler finds it
// there, settles the step when it reads protected, and names its new node in
// the qm set command when it doesn't. The probe leg finds it when the cluster
// index hasn't caught up.
func TestCutOffRestoreFollowsAParkerThatMoved(t *testing.T) {
	t.Parallel()
	t.Run("protected settles", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		logged := c.logTo(t)
		step := c.restoreStep(t)
		c.setProtection(true)
		c.moveParkerToN2()
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with the parker protected on n2: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the parker protected on n2", got)
		}
		line := settledWarning(t, logged, step.ID,
			fmt.Sprintf("VM %d on node n2 carries the bosh-parker tag and reads protected", c.parker),
			"the parker's recorded node is n1")
		// A VM the CPI created later at the same VMID reads the same way, so
		// the warning never says the parker moved.
		assertNoClaim(t, line, "now on node", "moved")
	})
	t.Run("protection off names its new node", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(false)
		c.moveParkerToN2()
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("VM %d on node n2 carries the bosh-parker tag and its protection is off, and the parker's recorded node is n1", c.parker),
			fmt.Sprintf("run qm set %d --protection 1 on node n2, then retry", c.parker))
		if err != nil {
			assertNoClaim(t, err.Error(), "now on node", "moved")
		}
	})
	t.Run("found only by the probe", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(true)
		c.moveParkerToN2()
		c.client.unlisted = map[int]bool{c.parker: true}
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with the parker on n2 and missing from the index: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the parker found only by the probe", got)
		}
	})
}

// TestCutOffRestoreSettlesAGoneParkerOnlyWhenTheDiskIsAccountedFor covers a
// parker that is gone from the cluster. The step settles when the disk is on
// another VM. It stays planned when the disk is on no VM, because the parker
// may have taken it, and when the cluster could not be searched, because the
// parker may still exist.
func TestCutOffRestoreSettlesAGoneParkerOnlyWhenTheDiskIsAccountedFor(t *testing.T) {
	t.Parallel()
	t.Run("disk on its VM settles", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		logged := c.logTo(t)
		step := c.restoreStep(t)
		delete(c.client.state.configs, c.parker)
		next, err := c.adopt(t)
		if err != nil {
			t.Fatalf("adopt with the parker gone and the disk on VM 777: %v", err)
		}
		if next.State != aj.Adopted {
			t.Fatalf("adoption produced %s", next.State)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the parker gone and the disk on VM 777", got)
		}
		settledWarning(t, logged, step.ID, fmt.Sprintf("parker %d is gone from the cluster", c.parker), "the disk is on VM 777")
	})
	t.Run("disk nowhere refuses", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		delete(c.client.state.configs, c.parker)
		delete(c.client.state.configs[777], "scsi1")
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("parker %d is gone from the cluster", c.parker), "may have taken the disk with it",
			`see "Parker anchor missing (parked disk with no holder)" in docs/troubleshooting.md of bosh-proxmox-cpi-release`)
	})
	t.Run("unsearchable cluster refuses", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		delete(c.client.state.configs, c.parker)
		// The read on the recorded node answers missing, and every probe of
		// the other members fails, so the search can't prove anything.
		var reads atomic.Int32
		c.client.onConfigRead = func(vmid int) error {
			if vmid != c.parker || reads.Add(1) == 1 {
				return nil
			}
			return sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed"}`))
		}
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("parker %d wasn't found on its recorded node n1, and the cluster could not be searched for it", c.parker),
			"retry once every node answers")
	})
}

// TestCutOffRestoreWhenTheRecordedNodeDoesNotAnswer covers a parker whose
// recorded node fails the read without saying the VM is missing, the way a
// node that is down fails it. The settler searches the cluster anyway and
// follows a VM the cluster places on another node, as it does for a parker
// that moved. When the cluster places the VMID on the recorded node, on no
// node at all, or can't be searched, the step stays planned and the refusal
// names the recorded node, because a node that doesn't answer may still hold
// the parker and its disk.
func TestCutOffRestoreWhenTheRecordedNodeDoesNotAnswer(t *testing.T) {
	t.Parallel()
	unreadable := func(c *cutOffRestore) string {
		return fmt.Sprintf("the config of parker %d could not be read on its recorded node n1", c.parker)
	}
	t.Run("moved parker settles", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		logged := c.logTo(t)
		step := c.restoreStep(t)
		c.setProtection(true)
		c.moveParkerToN2()
		c.moveDiskHolderToN2()
		c.recordedNodeDown()
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with n1 not answering and the parker protected on n2: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with n1 not answering and the parker protected on n2", got)
		}
		settledWarning(t, logged, step.ID,
			fmt.Sprintf("VM %d on node n2 carries the bosh-parker tag and reads protected", c.parker),
			"the parker's recorded node is n1")
	})
	t.Run("moved parker with protection off names its new node", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(false)
		c.moveParkerToN2()
		c.moveDiskHolderToN2()
		c.recordedNodeDown()
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err, fmt.Sprintf("run qm set %d --protection 1 on node n2, then retry", c.parker))
	})
	t.Run("parker listed on the recorded node refuses", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(true)
		c.client.offlineNodes = map[string]bool{"n1": true}
		c.recordedNodeDown()
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err, unreadable(c), "the cluster places it on no other node", "retry once node n1 answers")
	})
	t.Run("VMID listed nowhere refuses instead of settling as gone", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		// The disk is on VM 777, which would settle the step of a parker the
		// cluster proved gone. The search skips n1 because the cluster
		// reports it offline, so it proves nothing about the parker there.
		delete(c.client.state.configs, c.parker)
		c.client.offlineNodes = map[string]bool{"n1": true}
		c.recordedNodeDown()
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err, unreadable(c), "the cluster places it on no other node", "retry once node n1 answers")
	})
	t.Run("VMID listed nowhere refuses while the disk is on a live VM", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		// The disk's VM answers on n2 and the parker is listed nowhere, so
		// the disk is accounted for and the step would settle as a gone
		// parker if the settler took the recorded node's failure for a
		// missing answer. Only the cluster search keeps it planned.
		c.moveDiskHolderToN2()
		delete(c.client.state.configs, c.parker)
		c.client.offlineNodes = map[string]bool{"n1": true}
		c.recordedNodeDown()
		_, err := c.adopt(t)
		t.Logf("adopt: %v", err)
		if got := stepState(t, c.record(t), step.ID); got != aj.Planned {
			t.Fatalf("restore step is %s with the parker's node down and the parker listed nowhere, want planned", got)
		}
	})
	t.Run("unsearchable cluster refuses", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		// n1 is not reported offline, so the search probes it, and the
		// probe fails as the first read did, after retries that run without
		// waiting.
		delete(c.client.state.configs, c.parker)
		c.recordedNodeDown()
		c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err, unreadable(c), "the cluster could not be searched for it", "retry once node n1 answers")
	})
}

// TestCutOffRestoreOnAnUntaggedVM covers a VM at the parker's VMID that no
// longer carries the parker tag. It settles when it reads protected, which
// leaves nothing unprotected whoever the VM is, and refuses with the operator
// route when it doesn't.
func TestCutOffRestoreOnAnUntaggedVM(t *testing.T) {
	t.Parallel()
	t.Run("protected settles", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		logged := c.logTo(t)
		step := c.restoreStep(t)
		delete(c.client.state.configs[c.parker], "tags")
		c.setProtection(true)
		if _, err := c.adopt(t); err != nil {
			t.Fatalf("adopt with the untagged VM protected: %v", err)
		}
		if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
			t.Fatalf("restore step left %s with the untagged VM protected", got)
		}
		line := settledWarning(t, logged, step.ID,
			fmt.Sprintf("VM %d on node n1 doesn't carry the bosh-parker tag and reads protected", c.parker))
		// The VM read back protected on the recorded node, so the warning
		// never says it didn't.
		assertNoClaim(t, line, "without a protected readback")
	})
	t.Run("protection off refuses", func(t *testing.T) {
		t.Parallel()
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		delete(c.client.state.configs[c.parker], "tags")
		c.setProtection(false)
		_, err := c.adopt(t)
		assertRefusedAndPlanned(t, c, step, err,
			fmt.Sprintf("VM %d on node n1 no longer carries the bosh-parker tag and its protection is off", c.parker),
			fmt.Sprintf("qm config %d", c.parker),
			fmt.Sprintf("qm set %d --protection 1 on node n1", c.parker))
	})
}

// TestRerunDeleteDiskSettlesOnceTheParkerIsGone cuts off the restore of a
// delete_disk that deleted the disk on its parker, and then the parker goes
// away. The rerun finishes the deletion, because the record's delete of the
// disk completed and its volume reads absent.
func TestRerunDeleteDiskSettlesOnceTheParkerIsGone(t *testing.T) {
	t.Parallel()
	c := newOwnedParkerRestoreDisk(t, 200*time.Millisecond)
	logged := c.logTo(t)
	deleteArgs := []json.RawMessage{planJSON(t, c.cid)}
	c.hung.arm(true)
	_, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{})
	c.hung.arm(false)
	if err == nil {
		t.Fatal("delete_disk succeeded although its protection restore was cut off")
	}
	var step aj.Step
	record := c.record(t)
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_delete_disk_Nodes_UpdateQemuConfig" && record.Steps[i].State == aj.Planned {
			step = record.Steps[i]
		}
	}
	if step.ID == "" || step.Target.VMID != c.parker {
		t.Fatalf("the cut-off delete left no planned protection step on parker %d: %+v", c.parker, record.Steps)
	}
	delete(c.client.state.configs, c.parker)

	if _, err := HandleDeleteDisk(c.deps).Handle(c.ctx, deleteArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun delete_disk once the parker is gone: %v", err)
	}
	record = c.record(t)
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the rerun delete_disk", step.ID, got)
	}
	if record.State != aj.Deleted {
		t.Fatalf("allocation state after the rerun delete_disk = %s (reason %q), want %s", record.State, record.Reason, aj.Deleted)
	}
	line := settledWarning(t, logged, step.ID, fmt.Sprintf("parker %d is gone from the cluster", c.parker),
		"the disk's volume reads absent", "the record observed a delete_disk write on the parker for that volume")
	// A detach observed without its sweep reads the same way once the parker
	// is destroyed, so the warning never says the deletion completed.
	assertNoClaim(t, line, "deletion of the disk completed")
}

// moverRestoreHangPVE is the lifecycle flow cluster with every protection
// write landing on the VM's config. Once armed, the write that puts a mover's
// protection back after the disk has left it never answers, so a cross-node
// attach lands the disk on its VM and then has the mover's restore cut off.
type moverRestoreHangPVE struct {
	*lifecycleFlowPVE

	mu    sync.Mutex
	armed bool
	mover int
}

func (c *moverRestoreHangPVE) Nodes() nodes.Service {
	return moverRestoreHangNodes{
		lifecycleFlowNodes: lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c.lifecycleFlowPVE},
		owner:              c,
	}
}

type moverRestoreHangNodes struct {
	lifecycleFlowNodes
	owner *moverRestoreHangPVE
}

func (n moverRestoreHangNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return err
	}
	cfg := n.owner.state.configs[id]
	tags, _ := pve.ConfigString(cfg, "tags")
	n.owner.mu.Lock()
	armed := n.owner.armed
	n.owner.mu.Unlock()
	if armed && p.Protection != nil && *p.Protection && pve.TagsMarkDiskMover(tags) && !lifecycleConfigHasAnyVolume(cfg) {
		<-ctx.Done()
		n.owner.mu.Lock()
		n.owner.mover = id
		n.owner.mu.Unlock()
		return ctx.Err()
	}
	if err := n.lifecycleFlowNodes.UpdateQemuConfig(ctx, node, vmid, p); err != nil {
		return err
	}
	if p.Protection != nil && cfg != nil {
		protection := 0
		if *p.Protection {
			protection = 1
		}
		cfg["protection"] = protection
	}
	return nil
}

// TestRerunAttachSettlesOnceTheMoverIsDestroyed attaches a parker-named disk
// to a VM on another node through a mover, with the mover's restore cut off
// after the disk landed, and then destroys the mover. The rerun finds the
// disk on its VM under the name the migration gave it, settles the step, and
// returns the disk.
func TestRerunAttachSettlesOnceTheMoverIsDestroyed(t *testing.T) {
	t.Parallel()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DiskMigration = "on_attach"
	ctx := pve.WithParkerProtectionRestoreTimeoutForTest(pve.WithTestBackoff(t.Context(), func(int) time.Duration { return 0 }), 200*time.Millisecond)
	parkRenameCycle(t, ctx, deps, client, cid)
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	hang := &moverRestoreHangPVE{lifecycleFlowPVE: client}
	deps.PVE = hang
	logged := &bytes.Buffer{}
	logger, err := log.NewLogger("debug", logged)
	if err != nil {
		t.Fatal(err)
	}
	deps.Logger = logger
	attachArgs := []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}

	hang.mu.Lock()
	hang.armed = true
	hang.mu.Unlock()
	_, attachErr := HandleAttachDisk(deps).Handle(ctx, attachArgs, jsonrpc.Context{})
	hang.mu.Lock()
	hang.armed = false
	mover := hang.mover
	hang.mu.Unlock()
	if mover == 0 {
		t.Fatalf("no mover restore was cut off; the attach returned %v", attachErr)
	}
	if attachErr == nil {
		t.Fatal("attach_disk succeeded although the mover's protection restore was cut off")
	}
	t.Logf("cut-off attach: %s", log.ScrubMessage(attachErr.Error()))
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	var step aj.Step
	for i := range record.Steps {
		if IsParkerProtectionStep(record, record.Steps[i]) && record.Steps[i].Target.VMID == mover {
			step = record.Steps[i]
		}
	}
	if step.ID == "" {
		t.Fatalf("the cut-off attach left no planned protection step on mover %d: %+v", mover, record.Steps)
	}
	delete(client.state.configs, mover)
	delete(client.vmNodes, mover)

	if _, err := HandleAttachDisk(deps).Handle(ctx, attachArgs, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun attach once the mover is destroyed: %s", log.ScrubMessage(err.Error()))
	}
	record, err = journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the rerun", step.ID, got)
	}
	assertReturnedRecord(t, "rerun", record)
	settledWarning(t, logged, step.ID, fmt.Sprintf("parker %d is gone from the cluster", mover), "the disk is on VM 888")

	// The resolution the settler relies on finds the disk on its VM after
	// the migration renamed the volume.
	rd, err := resolveDeleteDiskCID(ctx, deps, cid)
	if err != nil {
		t.Fatal(err)
	}
	if rd.holder == nil || rd.holder.VMID != 888 || rd.holder.Node != "n2" || rd.volid == rd.birth || !strings.Contains(rd.volid, "vm-888-") {
		t.Fatalf("the disk resolved to holder %+v under %q (birth %q), want VM 888 on n2 under a renamed volume", rd.holder, rd.volid, rd.birth)
	}
}

// TestCreateVMRetryCompletesOnceTheParkerIsGone is the Director's retry of a
// create_vm whose disk landed on the new VM while the parker's restore was
// cut off, after the parker was destroyed. The retry settles the step and
// finishes the same generation on the same VM.
func TestCreateVMRetryCompletesOnceTheParkerIsGone(t *testing.T) {
	t.Parallel()
	flow := newProtectionPendingFlow(t, false)
	generation, vmid := flow.cutOff(t)
	delete(flow.parked.client.state.configs, flow.parked.parker)

	result, err := flow.createVM()
	if err != nil {
		t.Fatalf("the Director's retry after the parker was destroyed: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 || values[0] != strconv.Itoa(vmid) {
		t.Fatalf("the retry returned %v, want the kept VM %d", result, vmid)
	}
	if created := flow.created(t); len(created) != 1 || len(flow.vms.destroyed) != 0 {
		t.Fatalf("the retry rebuilt the VM: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	if resumed := flow.generation(t); resumed.ID != generation.ID || resumed.State != aj.ReadyToReturn {
		t.Fatalf("the retry did not finish the same generation: was %s, now %s in %s", generation.ID, resumed.ID, resumed.State)
	}
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))
}

// TestCleanupSettlesTheRestoreOfAGoneParker runs an attested cleanup on a
// record whose parker is gone and whose disk is on VM 777. The cleanup's
// settlement settles the protection step, so the step no longer stands in
// the cleanup's way.
func TestCleanupSettlesTheRestoreOfAGoneParker(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	delete(c.client.state.configs, c.parker)
	deps := c.deps
	deps.PVE = &cleanupTaskClient{Client: c.deps.PVE}
	_, err := CleanupStorageAllocation(c.ctx, deps, c.journal, []string{"n1"}, cleanupAttestedDecision(c.id))
	t.Logf("cleanup: %v", err)
	if err != nil && strings.Contains(err.Error(), "pending_mutation_settlement") {
		t.Fatalf("cleanup still refused at the pending mutation settlement: %v", err)
	}
	if err != nil && strings.Contains(StorageAllocationDecisionFailure(err), "parker protection write could not be settled") {
		t.Fatalf("cleanup refused on the protection step of a gone parker: %s", StorageAllocationDecisionFailure(err))
	}
	if got := stepState(t, c.record(t), step.ID); got != aj.Observed {
		t.Fatalf("restore step left %s after the cleanup", got)
	}
}

// TestGoneParkerSettlementKeepsTheRecordShape settles the step of a gone
// parker and checks that the record differs from the one before only where a
// protected readback would change it: the step is observed and names the
// disk's volume. No field, verification, or state is added, so a release
// that predates the settlement still reads the record.
func TestGoneParkerSettlementKeepsTheRecordShape(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	delete(c.client.state.configs, c.parker)
	before := c.record(t)
	rawBefore, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}

	handle, err := c.journal.Acquire(c.ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	gaps, settleErr := settleProtectionForTest(c.ctx, c.deps, handle)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if settleErr != nil || len(gaps) != 0 {
		t.Fatalf("settling the gone parker's step: gaps %v, error %v", gaps, settleErr)
	}
	after := c.record(t)

	want := before
	if err := json.Unmarshal(rawBefore, &want); err != nil {
		t.Fatal(err)
	}
	for i := range want.Steps {
		if want.Steps[i].ID == step.ID {
			want.Steps[i].State = aj.Observed
			want.Steps[i].VolIDs = append(want.Steps[i].VolIDs, step.Target.IntendedVolume)
		}
	}
	want.UpdatedAt = after.UpdatedAt
	if !reflect.DeepEqual(want, after) {
		gotJSON, _ := json.Marshal(after)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("the settled record differs from a readback settlement:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	var keysBefore, keysAfter map[string]json.RawMessage
	rawAfter, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(rawBefore, &keysBefore) != nil || json.Unmarshal(rawAfter, &keysAfter) != nil {
		t.Fatal("the record does not decode as an object")
	}
	for key := range keysAfter {
		if _, ok := keysBefore[key]; !ok {
			t.Errorf("the settled record carries field %q that it did not carry before", key)
		}
	}
}

// TestRecordDeletedDiskNeedsAnObservedDeletion pins the two shapes of a
// delete_disk that reached the disk. A delete_disk window on the parker counts
// only when PVE answered a write inside it on a volume the parker owns, and
// each shape says what the record observed.
func TestRecordDeletedDiskNeedsAnObservedDeletion(t *testing.T) {
	t.Parallel()
	const parker = 90000
	owned, foreign := "b:90000/vm-90000-disk-2.raw", "b:777/vm-777-disk-1.raw"
	off := json.RawMessage(`{"kind":"parker_protection_off","version":1}`)
	on := json.RawMessage(`{"kind":"parker_protection_on","version":1}`)
	step := func(kind string, state aj.State, vmid int, volume string, params json.RawMessage) aj.Step {
		return aj.Step{Kind: kind, State: state, Target: aj.Target{VMID: vmid, IntendedVolume: volume}, Parameters: params}
	}
	const config = "lifecycle_delete_disk_Nodes_UpdateQemuConfig"
	const storage, window = "the record observed its storage delete", "the record observed a delete_disk write on the parker for that volume"
	for _, tc := range []struct {
		name  string
		steps []aj.Step
		want  string
	}{
		{"storage delete observed", []aj.Step{step("lifecycle_delete_disk_Storage_DeleteVolumeAsync", aj.Observed, 0, owned, nil)}, storage},
		{"storage delete planned", []aj.Step{step("lifecycle_delete_disk_Storage_DeleteVolumeAsync", aj.Planned, 0, owned, nil)}, ""},
		{"write inside the window", []aj.Step{step(config, aj.Observed, parker, owned, off), step(config, aj.Observed, parker, owned, nil), step(config, aj.Planned, parker, owned, on)}, window},
		{"window with no observed write", []aj.Step{step(config, aj.Observed, parker, owned, off), step(config, aj.Planned, parker, owned, nil), step(config, aj.Planned, parker, owned, on)}, ""},
		{"write outside any window", []aj.Step{step(config, aj.Observed, parker, owned, nil), step(config, aj.Planned, parker, owned, on)}, ""},
		{"write after the window closed", []aj.Step{step(config, aj.Observed, parker, owned, off), step(config, aj.Observed, parker, owned, on), step(config, aj.Observed, parker, owned, nil)}, ""},
		{"write on a volume the parker doesn't own", []aj.Step{step(config, aj.Observed, parker, foreign, off), step(config, aj.Observed, parker, foreign, nil)}, ""},
		{"attach window", []aj.Step{step("lifecycle_attach_disk_Nodes_UpdateQemuConfig", aj.Observed, parker, owned, off), step("lifecycle_attach_disk_Nodes_UpdateQemuConfig", aj.Observed, parker, owned, nil)}, ""},
		{"window on another VM", []aj.Step{step(config, aj.Observed, 777, owned, off), step(config, aj.Observed, 777, owned, nil)}, ""},
	} {
		if got := recordDeletedDisk(aj.Record{Steps: tc.steps}, parker); got != tc.want {
			t.Errorf("%s: recordDeletedDisk = %q, want %q", tc.name, got, tc.want)
		}
	}
}
