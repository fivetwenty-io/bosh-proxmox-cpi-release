package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// failPersistentHandoff replaces the create_vm disk attach for one test. The
// real attach runs, and then its outcome is reported to the VM allocation the
// way wrap chooses, which is how a test reaches a VM record whose only open
// step is the persistent disk handoff.
func failPersistentHandoff(t *testing.T, wrap func(slot string, err error) (string, error)) {
	t.Helper()
	previous := attachExistingDiskForVM
	attachExistingDiskForVM = func(ctx context.Context, deps Deps, handle *aj.Handle, disk resolvedDisk, node string, vmid int) (string, error) {
		return wrap(previous(ctx, deps, handle, disk, node, vmid))
	}
	t.Cleanup(func() { attachExistingDiskForVM = previous })
}

// attestedCleanupDeps gives cleanup the settled task evidence that the CLI's
// client reads from PVE, so the attested path can observe it.
func attestedCleanupDeps(deps Deps) Deps {
	cleanup := deps
	cleanup.PVE = &cleanupTaskClient{Client: deps.PVE}
	return cleanup
}

// handoffRecord returns the VM record create_vm left behind and checks that its
// only open step is the planned persistent disk handoff.
func handoffRecord(t *testing.T, journal *aj.Journal) aj.Record {
	t.Helper()
	vm, found, err := journal.InspectVM("disk-agent")
	if err != nil || !found {
		t.Fatalf("VM record: found=%t err=%v", found, err)
	}
	if vm.State != aj.ReconciliationRequired {
		t.Fatalf("the VM record is %s, want %s", vm.State, aj.ReconciliationRequired)
	}
	open := 0
	for i := range vm.Steps {
		if vm.Steps[i].State == aj.Observed {
			continue
		}
		open++
		if !strings.HasPrefix(vm.Steps[i].Kind, managedVMPersistentHandoffPrefix) || vm.Steps[i].State != aj.Planned {
			t.Fatalf("unexpected open VM step %s (%s) %s", vm.Steps[i].ID, vm.Steps[i].Kind, vm.Steps[i].State)
		}
	}
	if open != 1 {
		t.Fatalf("the VM record has %d open steps, want only the handoff: %+v", open, vm.Steps)
	}
	return vm
}

// TestCreateVMDiskNonCleanTimeoutVMCleanupPath is the record a create_vm
// pre-attach leaves when its parker lock wait runs out after the disk lifecycle
// changed something. The disk goes back to its parker, and the VM record keeps
// one planned handoff step. Plain cleanup and delete_vm stay refused, and the
// attested cleanup closes the VM generation without touching the disk.
func TestCreateVMDiskNonCleanTimeoutVMCleanupPath(t *testing.T) {
	locks := newLockContention(t)
	deps, client, journal, cid, parker := createVMDiskFixture(t, locks, true)
	locks.reset()
	plantHeldParkerLock(locks, parker)
	shortenManagedLockWait(t, 1500*time.Millisecond)
	// The wrapper drops the returned-disk marker, so the VM allocation sees
	// the uncertain outcome a pre-wait disk mutation would give it.
	failPersistentHandoff(t, func(slot string, err error) (string, error) {
		if err != nil {
			return slot, errors.New(err.Error())
		}
		return slot, nil
	})
	if _, err := createVM(t.Context(), deps, createVMArgs(t, cid)); err == nil {
		t.Fatal("create_vm succeeded behind a held parker lock")
	}
	vm := handoffRecord(t, journal)
	vmid, ok := managedVMRootVMID(vm)
	if !ok {
		t.Fatal("the VM record has no root VMID")
	}
	locks.reset()
	cleanup := attestedCleanupDeps(deps)

	_, err := CleanupStorageAllocation(t.Context(), cleanup, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: vm.ID, DecisionID: "plain-handoff"})
	if reason := StorageAllocationDecisionFailure(err); err == nil || !strings.HasPrefix(reason, "cleanup_pending_mutation_settlement") {
		t.Fatalf("plain cleanup was not refused at pending mutation settlement: %v (%s)", err, reason)
	}
	if _, err := HandleDeleteVM(deps).Handle(t.Context(), []json.RawMessage{json.RawMessage(fmt.Sprintf("%q", strconv.Itoa(vmid)))}, jsonrpc.Context{}); err == nil {
		t.Fatal("delete_vm accepted the VM record with an open handoff step")
	}
	if after := handoffRecord(t, journal); after.State != aj.ReconciliationRequired {
		t.Fatalf("a refused path changed the VM record to %s", after.State)
	}

	cleaned, err := CleanupStorageAllocation(t.Context(), cleanup, journal, []string{"n1"}, cleanupAttestedDecision(vm.ID))
	if err != nil {
		t.Fatalf("attested cleanup refused the handoff record: %v (%s)", err, StorageAllocationDecisionFailure(err))
	}
	if cleaned.State != aj.Cleaned {
		t.Fatalf("attested cleanup left the VM record %s", cleaned.State)
	}
	if _, present := client.state.configs[vmid]; present {
		t.Fatalf("attested cleanup left VM %d on PVE", vmid)
	}
	assertParkedDiskUntouched(t, client, journal, parker)
}

// assertParkedDiskUntouched checks that the persistent disk is still on its
// parker, its volume still exists, and its own record is still returned.
func assertParkedDiskUntouched(t *testing.T, client *createVMDiskClient, journal *aj.Journal, parker int) {
	t.Helper()
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if records[i].Kind == "disk" && records[i].State != aj.ReadyToReturn {
			t.Fatalf("the disk record is %s (reason %q)", records[i].State, records[i].Reason)
		}
	}
	held := 0
	for key := range client.state.configs[parker] {
		if !isDiskOptionKey(key) {
			continue
		}
		value, _ := pve.ConfigString(client.state.configs[parker], key)
		if _, exists := client.state.volumes[strings.Split(value, ",")[0]]; !exists {
			t.Fatalf("the parked volume %s is gone", value)
		}
		held++
	}
	if held != 1 {
		t.Fatalf("the parker holds %d volumes, want the one disk", held)
	}
}

// TestAttestedHandoffCleanupKeepsABoundPersistentDisk is the touched shape. The
// disk attach landed and the volume sits in a slot on the VM, but the VM
// allocation could not read the binding back, so it left the handoff step
// planned. Attested cleanup must not take the persistent volume with the VM.
// VM cleanup runs delete_vm's preservation before the destroy, so the disk is
// parked again under its own allocation and the VM goes away without it.
func TestAttestedHandoffCleanupKeepsABoundPersistentDisk(t *testing.T) {
	shape := newBoundHandoffShape(t)
	deps, client, journal, cid, parker, vm, vmid := shape.deps, shape.client, shape.journal, shape.cid, shape.parker, shape.vm, shape.vmid

	cleaned, err := CleanupStorageAllocation(t.Context(), attestedCleanupDeps(deps), journal, []string{"n1"}, cleanupAttestedDecision(vm.ID))
	if err != nil {
		t.Fatalf("attested cleanup refused the bound handoff record: %v (%s)", err, StorageAllocationDecisionFailure(err))
	}
	if cleaned.State != aj.Cleaned {
		t.Fatalf("attested cleanup left the VM record %s", cleaned.State)
	}
	if _, present := client.state.configs[vmid]; present {
		t.Fatalf("attested cleanup left VM %d on PVE", vmid)
	}
	assertParkedDiskUntouched(t, client, journal, parker)
	bare, meta, err := decodeDiskCID(t.Context(), deps, "attach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), deps, "attach_disk", cid, bare, meta)
	if err != nil || rd.holder == nil || !rd.holder.IsParker || rd.holder.VMID != parker {
		t.Fatalf("the disk no longer resolves onto its parker: holder=%+v err=%v", rd.holder, err)
	}
}

// boundHandoffShape is a VM record whose only open step is the persistent disk
// handoff, while the disk's volume sits in a slot on that VM.
type boundHandoffShape struct {
	deps    Deps
	client  *createVMDiskClient
	journal *aj.Journal
	cid     string
	parker  int
	vm      aj.Record
	vmid    int
	slot    string
	volume  string
}

// newBoundHandoffShape runs create_vm with a disk in disk_cids whose attach
// lands, and then reports a slot that names nothing, so the VM allocation's
// binding readback fails and leaves the handoff step planned.
func newBoundHandoffShape(t *testing.T) boundHandoffShape {
	t.Helper()
	locks := newLockContention(t)
	deps, client, journal, cid, parker := createVMDiskFixture(t, locks, true)
	locks.reset()
	failPersistentHandoff(t, func(slot string, err error) (string, error) {
		if err != nil {
			return slot, err
		}
		return "scsi29", nil
	})
	if _, err := createVM(t.Context(), deps, createVMArgs(t, cid)); err == nil {
		t.Fatal("create_vm succeeded with an unreadable binding")
	}
	vm := handoffRecord(t, journal)
	vmid, ok := managedVMRootVMID(vm)
	if !ok {
		t.Fatal("the VM record has no root VMID")
	}
	shape := boundHandoffShape{deps: deps, client: client, journal: journal, cid: cid, parker: parker, vm: vm, vmid: vmid}
	for key := range client.state.configs[vmid] {
		value, _ := pve.ConfigString(client.state.configs[vmid], key)
		if isDiskOptionKey(key) && strings.HasPrefix(value, "b:") {
			shape.slot, shape.volume = key, strings.Split(value, ",")[0]
		}
	}
	if shape.slot == "" {
		t.Fatalf("the fixture did not bind the persistent volume to VM %d: %v", vmid, client.state.configs[vmid])
	}
	return shape
}

// openDiskStep leaves the disk's own record unsettled. It records a planned
// config write against the VM, the way a disk attach that never read its
// result back leaves it, and returns the record as saved.
func openDiskStep(t *testing.T, journal *aj.Journal, vmid int) aj.Record {
	t.Helper()
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for i := range records {
		if records[i].Kind == "disk" {
			id = records[i].ID
		}
	}
	handle, err := journal.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "outcome requires reconciliation at lifecycle attach_disk operation did not complete"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	record = handle.Record()
	record.Steps = append(record.Steps, aj.Step{ID: fmt.Sprintf("attempt-%d-step-%d", record.ActiveAttempt(), len(record.Steps)), Attempt: record.ActiveAttempt(), Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: vmid}})
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	return handle.Record()
}

// TestAttestedHandoffCleanupRefusesWhileTheDiskIsUnsettled pins the order the
// recovery depends on. The disk's own record has to be settled before the VM
// generation is cleaned up, because the cleanup preserves a bound disk through
// that disk's own lifecycle, and the lifecycle refuses a record with an open
// step. With the disk still bound to the VM and its record unsettled, attested
// cleanup of the VM record is refused, and the VM, the binding, and the disk
// record are all left as they were.
func TestAttestedHandoffCleanupRefusesWhileTheDiskIsUnsettled(t *testing.T) {
	shape := newBoundHandoffShape(t)
	before := openDiskStep(t, shape.journal, shape.vmid)

	_, err := CleanupStorageAllocation(t.Context(), attestedCleanupDeps(shape.deps), shape.journal, []string{"n1"}, cleanupAttestedDecision(shape.vm.ID))
	if err == nil {
		t.Fatal("attested cleanup removed the VM while the disk it holds had an unsettled record")
	}
	t.Logf("refused: %v (%s)", err, StorageAllocationDecisionFailure(err))
	cfg, present := shape.client.state.configs[shape.vmid]
	if !present {
		t.Fatalf("the refused cleanup destroyed VM %d", shape.vmid)
	}
	value, _ := pve.ConfigString(cfg, shape.slot)
	if strings.Split(value, ",")[0] != shape.volume {
		t.Fatalf("the refused cleanup moved the disk out of %s: now %q, want %s", shape.slot, value, shape.volume)
	}
	if _, exists := shape.client.state.volumes[shape.volume]; !exists {
		t.Fatalf("the refused cleanup removed the persistent volume %s", shape.volume)
	}
	after, err := shape.journal.Inspect(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, err := aj.Fingerprint(before)
	if err != nil {
		t.Fatal(err)
	}
	afterHash, err := aj.Fingerprint(after)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != afterHash {
		t.Fatalf("the refused cleanup changed the disk record: %s %q with %d steps, was %s %q with %d steps", after.State, after.Reason, len(after.Steps), before.State, before.Reason, len(before.Steps))
	}
}

// TestCleanupPersistentHandoffStepShape pins every condition the handoff
// predicate checks, so a step that differs in any one of them stays refused.
func TestCleanupPersistentHandoffStepShape(t *testing.T) {
	kind := managedVMPersistentHandoffPrefix + strings.Repeat("0a", 32)
	base := func() (aj.Step, aj.Record) {
		root := aj.Step{ID: "attempt-0-step-0", Kind: "vm." + managedVMCallCreate, State: aj.Observed, Target: aj.Target{Node: "n1", VMID: 101}}
		step := aj.Step{ID: "attempt-0-step-1", Kind: kind, State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 101}}
		return step, aj.Record{Kind: "vm", Steps: []aj.Step{root, step}}
	}
	step, record := base()
	if !cleanupPersistentHandoffStep(step, record) {
		t.Fatal("the plain handoff step was refused")
	}
	for name, mutate := range map[string]func(*aj.Step, *aj.Record){
		"disk record":        func(_ *aj.Step, r *aj.Record) { r.Kind = "disk" },
		"earlier attempt":    func(s *aj.Step, _ *aj.Record) { s.Attempt = 1 },
		"submitted":          func(s *aj.Step, _ *aj.Record) { s.State = aj.Submitted },
		"task":               func(s *aj.Step, _ *aj.Record) { s.UPID = "UPID:n1:0:0:0:qmmove:101:root@pam:" },
		"charge":             func(s *aj.Step, _ *aj.Record) { s.Charges = []aj.Charge{{Backing: "b"}} },
		"volume":             func(s *aj.Step, _ *aj.Record) { s.VolIDs = []string{"b:101/vm-101-disk-1.raw"} },
		"parameters":         func(s *aj.Step, _ *aj.Record) { s.Parameters = json.RawMessage(`{"version":1}`) },
		"external":           func(s *aj.Step, _ *aj.Record) { s.Target.External = true },
		"no node":            func(s *aj.Step, _ *aj.Record) { s.Target.Node = "" },
		"storage":            func(s *aj.Step, _ *aj.Record) { s.Target.Storage = "b" },
		"backing":            func(s *aj.Step, _ *aj.Record) { s.Target.Backing = "nfs://nas/b" },
		"intended volume":    func(s *aj.Step, _ *aj.Record) { s.Target.IntendedVolume = "b:101/vm-101-disk-1.raw" },
		"other VMID":         func(s *aj.Step, _ *aj.Record) { s.Target.VMID = 102 },
		"short digest":       func(s *aj.Step, _ *aj.Record) { s.Kind = kind[:len(kind)-1] },
		"long digest":        func(s *aj.Step, _ *aj.Record) { s.Kind = kind + "0" },
		"uppercase digest":   func(s *aj.Step, _ *aj.Record) { s.Kind = managedVMPersistentHandoffPrefix + strings.Repeat("0A", 32) },
		"other prefix":       func(s *aj.Step, _ *aj.Record) { s.Kind = "vm.persistentx" + strings.Repeat("0a", 32) },
		"no root create":     func(_ *aj.Step, r *aj.Record) { r.Steps = r.Steps[1:] },
		"two root creates":   func(_ *aj.Step, r *aj.Record) { r.Steps = append(r.Steps, r.Steps[0]) },
		"root on another VM": func(_ *aj.Step, r *aj.Record) { r.Steps[0].Target.VMID = 102 },
	} {
		t.Run(name, func(t *testing.T) {
			step, record := base()
			mutate(&step, &record)
			if cleanupPersistentHandoffStep(step, record) {
				t.Fatal("the predicate accepted a step outside the handoff shape")
			}
		})
	}
}
