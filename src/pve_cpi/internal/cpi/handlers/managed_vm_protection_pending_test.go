package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// protectionHangPVE is the create_vm rollback fixture with the parker's
// protection writes made real, and with the restore that puts protection back
// on the parker hung while armed, so the attach that moves the parked disk
// onto the new VM lands the disk and then has its restore cut off.
type protectionHangPVE struct {
	rollbackFlowPVE
	parker int

	mu    sync.Mutex
	armed bool
	hung  int
}

func (c *protectionHangPVE) Nodes() nodes.Service {
	return protectionHangNodes{Service: c.rollbackFlowPVE.Nodes(), owner: c}
}

func (c *protectionHangPVE) arm(armed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = armed
}

type protectionHangNodes struct {
	nodes.Service
	owner *protectionHangPVE
}

func (n protectionHangNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	n.owner.mu.Lock()
	armed := n.owner.armed
	n.owner.mu.Unlock()
	if armed && p.Protection != nil && *p.Protection && vmid == strconv.Itoa(n.owner.parker) {
		<-ctx.Done()
		n.owner.mu.Lock()
		n.owner.hung++
		n.owner.mu.Unlock()
		return ctx.Err()
	}
	if err := n.Service.UpdateQemuConfig(ctx, node, vmid, p); err != nil {
		return err
	}
	if p.Protection != nil {
		id, err := strconv.Atoi(vmid)
		if err != nil {
			return err
		}
		cfg, ok := n.owner.state.configs[id]
		if !ok {
			return fmt.Errorf("fake: no config for vmid %s", vmid)
		}
		protection := 0
		if *p.Protection {
			protection = 1
		}
		cfg["protection"] = protection
	}
	return nil
}

// protectionPendingFlow is a managed create_vm whose parked persistent disk
// lands on the new VM while the parker's protection restore is cut off.
type protectionPendingFlow struct {
	*rollbackFlow
	pve *protectionHangPVE
	ctx context.Context
}

// newProtectionPendingFlow builds the flow, with one fallback placement
// attempt left when fallback is set, so the tests show create_vm keeps the VM
// whether or not it could place another.
func newProtectionPendingFlow(t *testing.T, fallback bool) *protectionPendingFlow {
	t.Helper()
	flow := newRollbackFlow(t)
	if fallback {
		limit := 1
		flow.parked.deps.Config.Placement.FallbackMax = &limit
	}
	// Nobody else holds the parker's lock: the attach enters its window, and
	// only the restore at the end of it is cut off.
	flow.locks.reset()
	hang := &protectionHangPVE{rollbackFlowPVE: flow.parked.deps.PVE.(rollbackFlowPVE), parker: flow.parked.parker}
	flow.parked.deps.PVE = hang
	return &protectionPendingFlow{rollbackFlow: flow, pve: hang, ctx: pve.WithParkerProtectionRestoreTimeoutForTest(t.Context(), 200*time.Millisecond)}
}

func (f *protectionPendingFlow) createVM() (any, error) {
	return createVM(f.ctx, f.parked.deps, f.args)
}

func (f *protectionPendingFlow) setParkerProtection(on bool) {
	value := 0
	if on {
		value = 1
	}
	f.parked.client.state.configs[f.parked.parker]["protection"] = value
}

// cutOff runs the first create_vm with the restore hung and checks the state
// it must leave: the disk on the one VM built, nothing destroyed, a retriable
// error naming the cut-off restore, and a generation with every step observed
// and nothing that asks for reconciliation.
func (f *protectionPendingFlow) cutOff(t *testing.T) (aj.Record, int) {
	t.Helper()
	f.pve.arm(true)
	_, err := f.createVM()
	f.pve.arm(false)
	f.pve.mu.Lock()
	hung := f.pve.hung
	f.pve.mu.Unlock()
	if hung == 0 {
		t.Fatalf("the parker's protection restore never hung: %v", err)
	}
	if err == nil {
		t.Fatal("create_vm succeeded although the parker's protection restore was cut off")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf("protection restore on parker vmid %d did not finish within", f.parked.parker)) {
		t.Fatalf("create_vm error %q does not name the cut-off restore", msg)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm error %q is not retriable", msg)
	}
	created := f.created(t)
	if len(created) != 1 || len(f.vms.destroyed) != 0 || f.vms.disposals != 0 {
		t.Fatalf("a landed disk with a cut-off restore disposed of the VM: created=%v destroyed=%v disposals=%d", created, f.vms.destroyed, f.vms.disposals)
	}
	vmid := created[0]
	if holder := f.holderOf(t, f.parked.cid); holder == nil || holder.VMID != vmid {
		t.Fatalf("the parked disk is not on VM %d: %+v", vmid, holder)
	}
	generation := f.generation(t)
	if generation.State == aj.ReconciliationRequired || hasDisposalAdmission(generation) {
		t.Fatalf("the generation is %s (reason %q, disposal admitted: %t), want it resumable", generation.State, generation.Reason, hasDisposalAdmission(generation))
	}
	handoff := false
	for i := range generation.Steps {
		step := &generation.Steps[i]
		if step.State != aj.Observed {
			t.Fatalf("generation step %s (%s) left %s; a resumable generation has every step observed", step.ID, step.Kind, step.State)
		}
		handoff = handoff || strings.HasPrefix(step.Kind, managedVMPersistentHandoffPrefix)
	}
	if !handoff {
		t.Fatal("the generation never journaled the persistent disk handoff")
	}
	if record := f.parked.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("the parked disk's record is %s, want %s with the restore planned", record.State, aj.ReconciliationRequired)
	}
	return generation, vmid
}

// TestCreateVMResumesALandedDiskOnceProtectionIsBack is the Director's retry
// after an operator puts the parker's protection back. The retry resumes the
// same generation on the same VM with no second create and no destroy, the
// disk's readmission settles the restore step, and both records finish.
func TestCreateVMResumesALandedDiskOnceProtectionIsBack(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			resumeLandedDisk(t, newProtectionPendingFlow(t, fallback))
		})
	}
}

func resumeLandedDisk(t *testing.T, flow *protectionPendingFlow) {
	t.Helper()
	generation, vmid := flow.cutOff(t)

	flow.setParkerProtection(true)
	result, err := flow.createVM()
	if err != nil {
		t.Fatalf("the Director's retry did not resume the generation: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	if cid, _ := values[0].(string); cid != strconv.Itoa(vmid) {
		t.Fatalf("the retry returned VM %v, want the kept VM %d", values[0], vmid)
	}
	if created := flow.created(t); len(created) != 1 || len(flow.vms.destroyed) != 0 {
		t.Fatalf("the retry rebuilt the VM: created=%v destroyed=%v", created, flow.vms.destroyed)
	}
	resumed := flow.generation(t)
	if resumed.ID != generation.ID || resumed.State != aj.ReadyToReturn {
		t.Fatalf("the retry did not finish the same generation: was %s, now %s in %s", generation.ID, resumed.ID, resumed.State)
	}
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))
	assertReturnedRecord(t, "attached disk", flow.diskRecord(t, flow.free.id))
	for name, cid := range map[string]string{"first": flow.free.cid, "second": flow.parked.cid} {
		if holder := flow.holderOf(t, cid); holder == nil || holder.VMID != vmid {
			t.Fatalf("the %s disk is not attached to VM %d: %+v", name, vmid, holder)
		}
	}
}

// TestCreateVMKeepsTheVMWhileProtectionIsOff is the same retry with the
// parker still unprotected. The retry is refused with the qm set command, the
// VM is kept, and the generation stays resumable, which the next retry after
// protection is back then proves by finishing it.
func TestCreateVMKeepsTheVMWhileProtectionIsOff(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			keepVMWhileProtectionIsOff(t, newProtectionPendingFlow(t, fallback))
		})
	}
}

func keepVMWhileProtectionIsOff(t *testing.T, flow *protectionPendingFlow) {
	t.Helper()
	generation, vmid := flow.cutOff(t)

	flow.setParkerProtection(false)
	_, err := flow.createVM()
	want := fmt.Sprintf("run qm set %d --protection 1", flow.parked.parker)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the retry with protection off = %v, want a refusal containing %q", err, want)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the refusal %q is not retriable, so the Director would not try again", err)
	}
	if created := flow.created(t); len(created) != 1 || len(flow.vms.destroyed) != 0 || flow.vms.disposals != 0 {
		t.Fatalf("the refused retry did not keep the VM: created=%v destroyed=%v disposals=%d", created, flow.vms.destroyed, flow.vms.disposals)
	}
	kept := flow.generation(t)
	if kept.ID != generation.ID || kept.State == aj.ReconciliationRequired || hasDisposalAdmission(kept) {
		t.Fatalf("the refused retry left generation %s in %s, want %s resumable", kept.ID, kept.State, generation.ID)
	}
	for i := range kept.Steps {
		if step := &kept.Steps[i]; step.State != aj.Observed {
			t.Fatalf("generation step %s (%s) left %s after the refused retry", step.ID, step.Kind, step.State)
		}
	}

	flow.setParkerProtection(true)
	result, err := flow.createVM()
	if err != nil {
		t.Fatalf("the retry after protection was put back failed: %v", err)
	}
	if values, ok := result.([]any); !ok || len(values) == 0 || values[0] != strconv.Itoa(vmid) {
		t.Fatalf("the retry returned %v, want the kept VM %d", result, vmid)
	}
	if resumed := flow.generation(t); resumed.ID != generation.ID || resumed.State != aj.ReadyToReturn {
		t.Fatalf("the retry did not finish the kept generation: %s in %s", resumed.ID, resumed.State)
	}
}
