package handlers

// This test builds the state a delete_vm leaves when its process stops as
// the preservation's write of a persistent disk's provenance entry reaches
// PVE, before PVE applies it. VM 777 moved from n1 to n2 on shared storage
// while it held the disk, so the preservation rewrites the entry on n2
// before it parks the disk, and that write is a configuration write
// delete_vm plans on 777. The journal and every VM configuration are
// captured at that moment and restored after the delete_vm returns, so the
// disk's record stays under the preservation's admission with that write
// planned, and 777 still holds the disk with the entry naming n1.

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// stopAtMovedProvenanceWrite is a flow cluster that runs stop, once, as
// delete_vm's rewrite of a moved holder's provenance entry reaches PVE, and
// then answers that write with a 500 without applying it.
type stopAtMovedProvenanceWrite struct {
	*lifecycleFlowPVE
	mu   sync.Mutex
	stop func()
}

func (c *stopAtMovedProvenanceWrite) Nodes() nodes.Service {
	return stopAtMovedProvenanceNodes{Service: c.lifecycleFlowPVE.Nodes(), c: c}
}

type stopAtMovedProvenanceNodes struct {
	nodes.Service
	c *stopAtMovedProvenanceWrite
}

func (n stopAtMovedProvenanceNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	if vmid == "777" && p != nil && p.Description != nil && strings.Contains(string(debug.Stack()), "healMovedDiskProvenance") {
		n.c.mu.Lock()
		stop := n.c.stop
		n.c.stop = nil
		n.c.mu.Unlock()
		if stop != nil {
			stop()
			return &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		}
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
}

// TestAttestedAdoptSettlesAStoppedDeleteVMPreservationThenDeleteVMRuns stops
// a delete_vm of the moved VM 777 at its preservation's provenance write. The
// next delete_vm refuses the disk, because no readback settles that write.
// The attested adopt settles the write from a readback of 777's slot on n2,
// writing nothing to PVE, and adopts the record. The next delete_vm then
// rewrites the entry on n2, parks the disk there, and destroys 777.
func TestAttestedAdoptSettlesAStoppedDeleteVMPreservationThenDeleteVMRuns(t *testing.T) {
	deps, client, journal, diskID, cid := lifecycleFlowFixture(t)
	attachMovedDisk(t, deps, client, cid)
	deps.Config.DetachedDiskStrategy = "parked"
	prior, err := journal.Inspect(diskID)
	if err != nil {
		t.Fatal(err)
	}
	persistent := heldAllocatedVolume(client)
	if persistent == "" || client.state.volumes[persistent] == nil {
		t.Fatalf("777 holds no allocated disk after the move: %v", client.state.configs[777])
	}
	vmID := stageMovedVMRecord(t, deps, client, journal, prior, persistent)

	cluster := &stopAtMovedProvenanceWrite{lifecycleFlowPVE: client}
	deps.PVE = cluster
	deleteVM := func() error {
		_, err := HandleDeleteVM(deps).Handle(t.Context(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		return err
	}
	stopDeleteVMAtProvenanceWrite(t, deps, client, journal, cluster, diskID, vmID, deleteVM)

	stopped, err := journal.Inspect(diskID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != aj.Observed && stopped.State != aj.Planned || stopped.Reason != "lifecycle delete_vm.preserve_disk admitted; completion pending" {
		t.Fatalf("the stopped delete_vm left the disk's record %s (reason %q), want it under the preservation's admission", stopped.State, stopped.Reason)
	}
	unsettled, ok := unsettledRecordStep(stopped)
	if !ok || unsettled.State != aj.Planned || unsettled.Kind != "lifecycle_delete_vm.preserve_disk_Nodes_UpdateQemuConfig" || unsettled.Target.VMID != 777 || unsettled.Target.Node != "n2" {
		t.Fatalf("the stopped delete_vm left unsettled step %+v (found %v), want a planned delete_vm configuration write on 777 on n2", unsettled, ok)
	}
	if entry := heldDiskProvenance(t, client, 777); entry.Node != "n1" {
		t.Fatalf("777's entry names %s after the stopped rewrite, want n1", entry.Node)
	}

	if err := deleteVM(); err == nil {
		t.Fatal("delete_vm accepted 777 while the stopped preservation's write is planned")
	}
	if heldAllocatedVolume(client) != persistent || client.state.configs[777] == nil {
		t.Fatalf("the refused delete_vm moved the disk or destroyed 777 (held %q)", heldAllocatedVolume(client))
	}

	before := map[int]map[string]any{}
	for vmid, cfg := range client.state.configs {
		before[vmid] = maps.Clone(cfg)
	}
	decision := StorageAllocationDecision{Action: decisionActionAdopt, AllocationID: diskID, ExpectedCID: cid, DecisionID: "fenced-writer-adoption", PreviousWriterFenced: true, RemoteTasksSettled: true}
	adopted, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1", "n2"}, decision)
	if err != nil {
		t.Fatalf("attested adopt refused the write a stopped delete_vm preservation left planned: %v", err)
	}
	if adopted.State != aj.Adopted {
		t.Fatalf("adopt left the record %s (reason %q), want it adopted", adopted.State, adopted.Reason)
	}
	if settled := stepByID(t, adopted, unsettled.ID); settled.State != aj.Observed || !containsString(settled.VolIDs, persistent) {
		t.Fatalf("the stopped preservation's write = %+v, want it observed with volume %s", settled, persistent)
	}
	if !reflect.DeepEqual(client.state.configs, before) {
		t.Fatalf("adopt changed VM configurations to %v, want %v", client.state.configs, before)
	}

	if err := deleteVM(); err != nil {
		t.Fatalf("delete_vm after the attested adopt failed: %v", err)
	}
	requireDeleteVMParkedTheDisk(t, deps, client, journal, vmID, cid)
}

// stageMovedVMRecord gives 777 a retained ephemeral disk and a VM record
// that delete_vm treats as its own, and returns that record's ID.
func stageMovedVMRecord(t *testing.T, deps Deps, client *lifecycleFlowPVE, journal *aj.Journal, prior aj.Record, persistent string) string {
	t.Helper()
	ephemeral := "a:777/vm-777-ephemeral-0.raw"
	info := *client.state.volumes[persistent]
	client.state.volumes[ephemeral] = &info
	client.state.configs[777]["scsi2"] = ephemeral + ",size=5G"
	client.state.configs[777]["tags"] = tagRetainEphemeral
	plan, err := activeStorageAllocationPlan(prior)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := managedDiskActualDefinition(t.Context(), deps, "a")
	if err != nil {
		t.Fatal(err)
	}
	plan.Definitions["a"] = definition
	plan.AllocationKey = "vm-agent"
	intent := prior.Intent
	if intent.Plan, err = json.Marshal(plan); err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(t.Context(), "vm-agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	vmID := handle.Record().ID
	step, err := storageMutationIntent(handle, "vm.ephemeral.scsi2", aj.Target{Node: "n1", VMID: 777, Storage: "a", Backing: definition.BackingKey(), IntendedVolume: ephemeral}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{ephemeral}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State, record.CID = aj.ReadyToReturn, "777"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	client.state.configs[777]["description"] = moveMarker(t, vmID, "vm-agent") + "\n" + pve.DescriptionFromConfig(client.state.configs[777])
	return vmID
}

// stopDeleteVMAtProvenanceWrite runs deleteVM until its rewrite of 777's
// provenance entry reaches PVE, then puts the journal and every VM
// configuration back to what they were at that moment.
func stopDeleteVMAtProvenanceWrite(t *testing.T, deps Deps, client *lifecycleFlowPVE, journal *aj.Journal, cluster *stopAtMovedProvenanceWrite, diskID, vmID string, deleteVM func() error) {
	t.Helper()
	var stoppedDisk, stoppedVM aj.Record
	var configs map[int]map[string]any
	cluster.stop = func() {
		var err error
		if stoppedDisk, err = journal.Inspect(diskID); err != nil {
			t.Errorf("the disk's record can't be read as the provenance write arrives: %v", err)
		}
		if stoppedVM, err = journal.Inspect(vmID); err != nil {
			t.Errorf("the VM's record can't be read as the provenance write arrives: %v", err)
		}
		configs = map[int]map[string]any{}
		for vmid, cfg := range client.state.configs {
			configs[vmid] = maps.Clone(cfg)
		}
	}
	_ = deleteVM()
	if configs == nil {
		t.Fatal("delete_vm never sent the rewrite of 777's provenance entry")
	}
	for vmid := range client.state.configs {
		if _, ok := configs[vmid]; !ok {
			delete(client.state.configs, vmid)
		}
	}
	maps.Copy(client.state.configs, configs)
	rewriteJournalRecord(t, deps, diskID, stoppedDisk)
	rewriteJournalRecord(t, deps, vmID, stoppedVM)
}

// requireDeleteVMParkedTheDisk checks that delete_vm deleted 777's record
// with the ephemeral retained and parked the disk on n2 with nothing left
// unsettled.
func requireDeleteVMParkedTheDisk(t *testing.T, deps Deps, client *lifecycleFlowPVE, journal *aj.Journal, vmID, cid string) {
	t.Helper()
	vm, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != aj.VMDeletedRetained || client.state.configs[777] != nil {
		t.Fatalf("delete_vm after the adopt left the VM's record %s (reason %q) with 777 present %v, want it deleted with the ephemeral retained", vm.State, vm.Reason, client.state.configs[777] != nil)
	}
	bare, meta, err := decodeDiskCID(t.Context(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(t.Context(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.holder == nil || !resolved.holder.IsParker || resolved.holder.Node != "n2" {
		t.Fatalf("the disk isn't parked on 777's current node: %+v", resolved.holder)
	}
	if unsettled, ok := unsettledRecordStep(resolved.allocation.record); ok {
		t.Fatalf("delete_vm left step %s in the disk's record", unsettledStepName(unsettled))
	}
}

// heldAllocatedVolume returns the allocated persistent disk's volume from
// 777's slots, or "" when no slot holds it. 777 holds other disks too, so we
// match the allocation's name instead of taking whichever slot comes first.
func heldAllocatedVolume(client *lifecycleFlowPVE) string {
	for key, value := range client.state.configs[777] {
		text, ok := value.(string)
		if !ok || !isDiskOptionKey(key) {
			continue
		}
		if volume := strings.Split(text, ",")[0]; strings.Contains(volume, "-alloc-") {
			return volume
		}
	}
	return ""
}
