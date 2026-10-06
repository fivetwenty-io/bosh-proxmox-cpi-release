package handlers

// These tests build the state a managed transfer to a parker leaves when its
// call stops right after PVE applies the source slot delete. VM 777 moved from
// n1 to n2 on shared storage while it held the disk, and the transfer runs on
// n2. The parker keeps the transfer record, the volume sits in no slot, and
// the slot delete's step stays planned in the disk's record. The journal and
// every VM configuration are captured as the delete reaches PVE, the delete
// is applied, and the call gets a 500. The captured state is restored after
// the call returns, so whatever the call did next is undone.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// stopAfterTransferSlotDelete is a flow cluster that runs stop, once, as a
// transfer's delete of 777's slot reaches PVE, and then applies the delete and
// answers it with a 500.
//
// While unreadParkers is set, every read of a parker's config that delete_vm
// makes to find the transfers off 777 fails.
type stopAfterTransferSlotDelete struct {
	*lifecycleFlowPVE
	mu            sync.Mutex
	stop          func()
	unreadParkers bool
}

func (c *stopAfterTransferSlotDelete) QEMU() qemu.Service {
	return answeredConfigQEMU{Service: c.lifecycleFlowPVE.QEMU(), answer: func(vmid int) error {
		c.mu.Lock()
		unread := c.unreadParkers
		c.mu.Unlock()
		if unread && vmid != 777 && strings.Contains(string(debug.Stack()), "managedSourceTransferRecords") {
			return errors.New("connection reset by peer")
		}
		return nil
	}}
}

func (c *stopAfterTransferSlotDelete) Nodes() nodes.Service {
	return stopAfterTransferSlotDeleteNodes{Service: c.lifecycleFlowPVE.Nodes(), c: c}
}

type stopAfterTransferSlotDeleteNodes struct {
	nodes.Service
	c *stopAfterTransferSlotDelete
}

func (n stopAfterTransferSlotDeleteNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	if vmid == "777" && p != nil && p.Delete != nil && strings.Contains(string(debug.Stack()), "transferIntoParker") {
		n.c.mu.Lock()
		stop := n.c.stop
		n.c.stop = nil
		n.c.mu.Unlock()
		if stop != nil {
			if err := n.Service.UpdateQemuConfig(ctx, node, vmid, p); err != nil {
				return err
			}
			stop()
			return &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		}
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
}

// stoppedSlotDelete is the state a transfer left when its call stopped after
// the slot delete, and the calls that act on it.
type stoppedSlotDelete struct {
	cluster    *stopAfterTransferSlotDelete
	deps       Deps
	client     *lifecycleFlowPVE
	journal    *aj.Journal
	diskID     string
	vmID       string
	cid        string
	persistent string
	step       aj.Step
}

func (s stoppedSlotDelete) call(t *testing.T, handler func(Deps) cpi.Handler, args ...string) error {
	t.Helper()
	return s.callContext(t.Context(), t, handler, args...)
}

func (s stoppedSlotDelete) callContext(ctx context.Context, t *testing.T, handler func(Deps) cpi.Handler, args ...string) error {
	t.Helper()
	params := make([]json.RawMessage, 0, len(args))
	for _, arg := range args {
		params = append(params, planJSON(t, arg))
	}
	_, err := handler(s.deps).Handle(ctx, params, jsonrpc.Context{})
	return err
}

func (s stoppedSlotDelete) resolve(t *testing.T) resolvedDisk {
	t.Helper()
	bare, meta, err := decodeDiskCID(t.Context(), s.deps, "test", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(t.Context(), s.deps, "test", s.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// requireSettled checks that the disk's record holds no unsettled step, that
// the stopped slot delete is observed with its volume, and that the record is
// back in ready_to_return.
func (s stoppedSlotDelete) requireSettled(t *testing.T) {
	t.Helper()
	record, err := s.journal.Inspect(s.diskID)
	if err != nil {
		t.Fatal(err)
	}
	if unsettled, ok := unsettledRecordStep(record); ok {
		t.Fatalf("the disk's record kept step %s unsettled (record %s, reason %q)", unsettledStepName(unsettled), record.State, record.Reason)
	}
	if settled := stepByID(t, record, s.step.ID); settled.State != aj.Observed || !containsString(settled.VolIDs, s.persistent) {
		t.Fatalf("the stopped slot delete = %+v, want it observed with volume %s", settled, s.persistent)
	}
	if record.State != aj.ReadyToReturn {
		t.Fatalf("the disk's record is %s (reason %q), want %s", record.State, record.Reason, aj.ReadyToReturn)
	}
}

// stopTransferAfterSlotDelete runs cut against 777's disk on a cluster that
// stops the call right after PVE applies the transfer's slot delete, restores
// the state captured at that moment, and checks that it is the stranded state
// this file covers.
func stopTransferAfterSlotDelete(t *testing.T, cut string) stoppedSlotDelete {
	t.Helper()
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
	cluster := &stopAfterTransferSlotDelete{lifecycleFlowPVE: client}
	deps.PVE = cluster
	s := stoppedSlotDelete{cluster: cluster, deps: deps, client: client, journal: journal, diskID: diskID, vmID: vmID, cid: cid, persistent: persistent}

	var stoppedDisk, stoppedVM aj.Record
	var configs map[int]map[string]any
	cluster.stop = func() {
		var err error
		if stoppedDisk, err = journal.Inspect(diskID); err != nil {
			t.Errorf("the disk's record can't be read as the slot delete arrives: %v", err)
		}
		if stoppedVM, err = journal.Inspect(vmID); err != nil {
			t.Errorf("the VM's record can't be read as the slot delete arrives: %v", err)
		}
		configs = map[int]map[string]any{}
		for vmid, cfg := range client.state.configs {
			configs[vmid] = maps.Clone(cfg)
		}
	}
	switch cut {
	case "delete_vm":
		_ = s.call(t, HandleDeleteVM, "777")
	case "detach_disk":
		_ = s.call(t, HandleDetachDisk, "777", cid)
	default:
		t.Fatalf("unknown call %s", cut)
	}
	if configs == nil {
		t.Fatalf("%s never sent the transfer's slot delete on 777", cut)
	}
	for vmid := range client.state.configs {
		if _, ok := configs[vmid]; !ok {
			delete(client.state.configs, vmid)
		}
	}
	maps.Copy(client.state.configs, configs)
	rewriteJournalRecord(t, deps, diskID, stoppedDisk)
	rewriteJournalRecord(t, deps, vmID, stoppedVM)

	step, ok := unsettledRecordStep(stoppedDisk)
	wantKind := "lifecycle_" + map[string]string{"delete_vm": "delete_vm.preserve_disk", "detach_disk": "detach_disk"}[cut] + "_Nodes_UpdateQemuConfig"
	if !ok || step.State != aj.Planned || step.Kind != wantKind || step.Target.VMID != 777 || step.Target.IntendedVolume != persistent {
		t.Fatalf("the stopped %s left unsettled step %+v (found %v), want a planned %s on 777 for %s", cut, step, ok, wantKind, persistent)
	}
	s.step = step
	if held := heldAllocatedVolume(client); held != "" {
		t.Fatalf("777 still names %s after the applied slot delete", held)
	}
	resolved := s.resolve(t)
	if resolved.holder != nil || resolved.intent == nil || resolved.intent.SourceVMCID != "777" || resolved.intent.Volid != persistent {
		t.Fatalf("the stopped transfer resolves with holder %+v and intent %+v, want no holder and the transfer from 777 in flight", resolved.holder, resolved.intent)
	}
	return s
}

// TestADiskCallFinishesATransferStoppedAfterTheSlotDelete stops a transfer of
// 777's disk to a parker, started by delete_vm or by detach_disk, right after
// PVE applies the source slot delete. Before the fix, every call on the disk
// refused on the planned slot delete, and a delete_vm rerun destroyed 777 and
// left the disk in no slot with that step planned. Now the disk's next call
// and a delete_vm rerun each settle the slot delete from a readback under the
// disk's allocation lock, finish the transfer, and then do their own work.
func TestADiskCallFinishesATransferStoppedAfterTheSlotDelete(t *testing.T) {
	for _, cut := range []string{"delete_vm", "detach_disk"} {
		t.Run(cut+" stopped then delete_vm reruns", func(t *testing.T) {
			s := stopTransferAfterSlotDelete(t, cut)
			if err := s.call(t, HandleDeleteVM, "777"); err != nil {
				t.Fatalf("the delete_vm rerun failed: %v", err)
			}
			requireDeleteVMParkedTheDisk(t, s.deps, s.client, s.journal, s.vmID, s.cid)
			s.requireSettled(t)
		})
		t.Run(cut+" stopped then attach_disk", func(t *testing.T) {
			s := stopTransferAfterSlotDelete(t, cut)
			if err := s.call(t, HandleAttachDisk, "777", s.cid); err != nil {
				t.Fatalf("attach_disk failed: %v", err)
			}
			if resolved := s.resolve(t); resolved.intent != nil || resolved.holder == nil || resolved.holder.VMID != 777 {
				t.Fatalf("attach_disk left the disk with holder %+v and intent %+v, want it on 777 with no transfer in flight", resolved.holder, resolved.intent)
			}
			s.requireSettled(t)
		})
		t.Run(cut+" stopped then detach_disk", func(t *testing.T) {
			s := stopTransferAfterSlotDelete(t, cut)
			if err := s.call(t, HandleDetachDisk, "777", s.cid); err != nil {
				t.Fatalf("detach_disk failed: %v", err)
			}
			if resolved := s.resolve(t); resolved.intent != nil || resolved.holder == nil || !resolved.holder.IsParker || resolved.holder.Node != "n2" {
				t.Fatalf("detach_disk left the disk with holder %+v and intent %+v, want it parked on n2", resolved.holder, resolved.intent)
			}
			s.requireSettled(t)
		})
		t.Run(cut+" stopped then delete_disk", func(t *testing.T) {
			s := stopTransferAfterSlotDelete(t, cut)
			if err := s.call(t, HandleDeleteDisk, s.cid); err != nil {
				t.Fatalf("delete_disk failed: %v", err)
			}
			if s.client.state.volumes[s.persistent] != nil {
				t.Fatalf("delete_disk left volume %s in place", s.persistent)
			}
			record, err := s.journal.Inspect(s.diskID)
			if err != nil {
				t.Fatal(err)
			}
			if record.State != aj.Deleted {
				t.Fatalf("delete_disk left the disk's record %s (reason %q), want it deleted", record.State, record.Reason)
			}
		})
	}
}

// TestDeleteVMKeepsTheVMWhileItCantReadTheParkersTransferRecords stops
// delete_vm's transfer of 777's disk right after the slot delete, and then
// fails the parker reads that look for transfers off 777. Before the fix, the
// rerun found no slot of the disk and destroyed 777. Now it refuses, keeps 777
// and the transfer as they were, and a rerun once the parkers read finishes
// the transfer and destroys 777.
func TestDeleteVMKeepsTheVMWhileItCantReadTheParkersTransferRecords(t *testing.T) {
	s := stopTransferAfterSlotDelete(t, "delete_vm")
	s.cluster.mu.Lock()
	s.cluster.unreadParkers = true
	s.cluster.mu.Unlock()
	err := s.call(t, HandleDeleteVM, "777")
	if err == nil || !strings.Contains(err.Error(), "delete_vm: refusing to destroy VM 777 because the parkers' transfer records could not be read, so nothing was destroyed; retry delete_vm once the parkers' configs can be read") {
		t.Fatalf("delete_vm with unreadable parkers returned %v, want the refusal that keeps 777", err)
	}
	if s.client.state.configs[777] == nil {
		t.Fatal("delete_vm destroyed 777 while it couldn't read the parkers")
	}
	if resolved := s.resolve(t); resolved.holder != nil || resolved.intent == nil || resolved.intent.SourceVMCID != "777" {
		t.Fatalf("the refused delete_vm left the disk with holder %+v and intent %+v, want the transfer from 777 still in flight", resolved.holder, resolved.intent)
	}
	s.cluster.mu.Lock()
	s.cluster.unreadParkers = false
	s.cluster.mu.Unlock()
	if err := s.call(t, HandleDeleteVM, "777"); err != nil {
		t.Fatalf("the delete_vm rerun once the parkers read failed: %v", err)
	}
	requireDeleteVMParkedTheDisk(t, s.deps, s.client, s.journal, s.vmID, s.cid)
	s.requireSettled(t)
}

// TestAttachDiskFinishesAStoppedTransferWhoseSourceIsGone covers a disk an
// earlier delete_vm rerun stranded by destroying 777 after the stopped slot
// delete. The disk's next attach_disk to another VM settles the slot delete,
// because the cluster no longer finds 777, lands the transfer on the parker,
// and then attaches the disk.
func TestAttachDiskFinishesAStoppedTransferWhoseSourceIsGone(t *testing.T) {
	s := stopTransferAfterSlotDelete(t, "delete_vm")
	delete(s.client.state.configs, 777)
	s.client.state.configs[779] = map[string]any{"name": "workload2", "digest": "1"}
	if err := s.call(t, HandleAttachDisk, "779", s.cid); err != nil {
		t.Fatalf("attach_disk to 779 failed: %v", err)
	}
	if resolved := s.resolve(t); resolved.intent != nil || resolved.holder == nil || resolved.holder.VMID != 779 {
		t.Fatalf("attach_disk left the disk with holder %+v and intent %+v, want it on 779 with no transfer in flight", resolved.holder, resolved.intent)
	}
	s.requireSettled(t)
}

// TestAttestedAdoptLeavesAStoppedTransferToTheNextDiskCall checks that adopt,
// which writes nothing to PVE, refuses a slot delete a stopped transfer left
// planned, changes nothing, and names the calls that finish the transfer. The
// detach_disk it names then parks the disk.
func TestAttestedAdoptLeavesAStoppedTransferToTheNextDiskCall(t *testing.T) {
	s := stopTransferAfterSlotDelete(t, "delete_vm")
	before := map[int]map[string]any{}
	for vmid, cfg := range s.client.state.configs {
		before[vmid] = maps.Clone(cfg)
	}
	decision := StorageAllocationDecision{Action: decisionActionAdopt, AllocationID: s.diskID, ExpectedCID: s.cid, DecisionID: "fenced-writer-adoption", PreviousWriterFenced: true, RemoteTasksSettled: true}
	_, err := ApplyStorageAllocationDecision(t.Context(), s.deps, s.journal, []string{"n1", "n2"}, decision)
	if err == nil {
		t.Fatal("adopt settled a slot delete whose transfer is still in flight")
	}
	for _, want := range []string{"still in flight", "next attach_disk, detach_disk, or delete_disk", "a rerun of delete_vm on VM 777"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("adopt's refusal %q doesn't say %q", err, want)
		}
	}
	if !reflect.DeepEqual(s.client.state.configs, before) {
		t.Fatalf("adopt changed VM configurations to %v, want %v", s.client.state.configs, before)
	}
	record, err := s.journal.Inspect(s.diskID)
	if err != nil {
		t.Fatal(err)
	}
	if step := stepByID(t, record, s.step.ID); step.State != aj.Planned {
		t.Fatalf("the refused adopt left the slot delete %s, want it planned", step.State)
	}
	if err := s.call(t, HandleDetachDisk, "777", s.cid); err != nil {
		t.Fatalf("detach_disk after the refused adopt failed: %v", err)
	}
	if resolved := s.resolve(t); resolved.intent != nil || resolved.holder == nil || !resolved.holder.IsParker {
		t.Fatalf("detach_disk left the disk with holder %+v and intent %+v, want it parked", resolved.holder, resolved.intent)
	}
	s.requireSettled(t)
}

// TestTransferSlotDeleteSettlementNeedsTheParkerRecordWrite checks that the
// settler leaves a planned source write alone when the step journaled before
// it isn't the write of the transfer record on the parker, because then
// nothing shows that the planned write is the transfer's slot delete.
func TestTransferSlotDeleteSettlementNeedsTheParkerRecordWrite(t *testing.T) {
	intent := pve.DiskTransferIntent{ParkerVMID: 90001, ParkerNode: "n2", Slot: "scsi0", Volid: "a:123/vm-123-disk.raw", SourceVMCID: "777"}
	kind := "lifecycle_detach_disk_Nodes_UpdateQemuConfig"
	planned := aj.Step{ID: "s2", Kind: kind, State: aj.Planned, Target: aj.Target{Node: "n2", VMID: 777, IntendedVolume: intent.Volid}}
	record := func(steps ...aj.Step) aj.Record {
		return aj.Record{Kind: allocationKindDisk, Steps: steps}
	}
	parkerWrite := aj.Step{ID: "s1", Kind: kind, State: aj.Observed, Target: aj.Target{Node: "n2", VMID: 90001, IntendedVolume: intent.Volid}}
	sourceWrite := aj.Step{ID: "s1", Kind: kind, State: aj.Observed, Target: aj.Target{Node: "n2", VMID: 777, IntendedVolume: intent.Volid}}
	otherParker := parkerWrite
	otherParker.Target.VMID = 90002
	otherOperation := parkerWrite
	otherOperation.Kind = "lifecycle_attach_disk_Nodes_UpdateQemuConfig"
	cases := []struct {
		name   string
		record aj.Record
		want   bool
	}{
		{"after the parker record write", record(parkerWrite, planned), true},
		{"after a source write that follows the parker record write", record(parkerWrite, sourceWrite, planned), true},
		{"with no parker record write", record(sourceWrite, planned), false},
		{"after a write to another parker", record(otherParker, planned), false},
		{"after another operation's write", record(otherOperation, planned), false},
		{"with a later step", record(parkerWrite, planned, sourceWrite), false},
	}
	for _, tc := range cases {
		if _, got := transferSourceWriteCandidate(tc.record, intent); got != tc.want {
			t.Errorf("%s: candidate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// unreadSourceForSettler is the stopped cluster with 777's readback, inside
// the settler's check of the source, made to fail one of two ways. With
// memberUnread unset, the read of 777's pending view fails. With it set, 777
// reads as not found on n2, the cluster's listing leaves it out, and the
// authoritative lookup's config probe on each member fails.
type unreadSourceForSettler struct {
	*stopAfterTransferSlotDelete
	memberUnread bool
}

func inSettlerReadback() bool {
	return strings.Contains(string(debug.Stack()), "transferSourceReleased")
}

func (c unreadSourceForSettler) Nodes() nodes.Service {
	return unreadSourceForSettlerNodes{Service: c.stopAfterTransferSlotDelete.Nodes(), memberUnread: c.memberUnread}
}

func (c unreadSourceForSettler) QEMU() qemu.Service {
	return answeredConfigQEMU{Service: c.stopAfterTransferSlotDelete.QEMU(), answer: func(vmid int) error {
		if !c.memberUnread || vmid != 777 || !inSettlerReadback() {
			return nil
		}
		if strings.Contains(string(debug.Stack()), "FindVMAuthoritative") {
			return errors.New("injected member config read failure")
		}
		return sdkerrors.ParseAPIError(404, []byte(`{"message":"VM not found"}`))
	}}
}

func (c unreadSourceForSettler) Cluster() cluster.Service {
	return unreadSourceForSettlerCluster{Service: c.stopAfterTransferSlotDelete.Cluster(), memberUnread: c.memberUnread}
}

type unreadSourceForSettlerNodes struct {
	nodes.Service
	memberUnread bool
}

func (n unreadSourceForSettlerNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	if vmid == "777" && inSettlerReadback() {
		if n.memberUnread {
			return nil, sdkerrors.ParseAPIError(404, []byte(`{"message":"VM not found"}`))
		}
		return nil, errors.New("connection reset by peer")
	}
	return n.Service.ListQemuPending(ctx, node, vmid)
}

type unreadSourceForSettlerCluster struct {
	cluster.Service
	memberUnread bool
}

func (c unreadSourceForSettlerCluster) ListResources(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	rows, err := c.Service.ListResources(ctx, p)
	if err != nil || rows == nil || !c.memberUnread || !inSettlerReadback() {
		return rows, err
	}
	kept := make(cluster.ListResourcesResponse, 0, len(*rows))
	for _, raw := range *rows {
		var row struct {
			VMID int `json:"vmid"`
		}
		if json.Unmarshal(raw, &row) == nil && row.VMID == 777 {
			continue
		}
		kept = append(kept, raw)
	}
	return &kept, nil
}

// TestTransferSlotDeleteSettlementFailsClosed stops detach_disk's transfer
// right after the slot delete and then gives the settler's readback of 777 a
// state that doesn't prove the delete applied. A slot delete still pending, a
// slot that still names the volume, a failed read of 777, and a 777 that reads
// as not found while a member's probe for it fails each leave the step planned
// and make attach_disk refuse. The settler logs its reason at Warn each time.
func TestTransferSlotDeleteSettlementFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		stage func(s *stoppedSlotDelete)
	}{
		{"the slot delete is still pending", func(s *stoppedSlotDelete) {
			s.client.pending = newFakePendingModel()
			cfg := s.client.state.configs[777]
			cfg["virtio5"] = s.persistent
			s.client.pending.holdDelete(777, cfg, "virtio5")
		}},
		{"a slot still names the volume", func(s *stoppedSlotDelete) {
			s.client.state.configs[777]["virtio5"] = s.persistent
		}},
		{"the source can't be read", func(s *stoppedSlotDelete) {
			s.deps.PVE = unreadSourceForSettler{stopAfterTransferSlotDelete: s.cluster}
		}},
		{"the source reads as not found and a member can't be probed", func(s *stoppedSlotDelete) {
			s.deps.PVE = unreadSourceForSettler{stopAfterTransferSlotDelete: s.cluster, memberUnread: true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := stopTransferAfterSlotDelete(t, "detach_disk")
			if reason := transferSourceReleased(t.Context(), s.deps.PVE, "n2", 777, s.persistent); reason != "" {
				t.Fatalf("before staging, the settler kept the step planned because %s", reason)
			}
			tc.stage(&s)
			reason := transferSourceReleased(t.Context(), s.deps.PVE, "n2", 777, s.persistent)
			if reason == "" {
				t.Fatal("the settler counted the slot delete as applied")
			}
			logger, observed := log.NewObservedLogger(log.LevelWarn)
			if err := s.callContext(log.IntoContext(t.Context(), logger), t, HandleAttachDisk, "777", s.cid); err == nil {
				t.Fatal("attach_disk succeeded while the slot delete stayed unproven")
			}
			record, err := s.journal.Inspect(s.diskID)
			if err != nil {
				t.Fatal(err)
			}
			if step := stepByID(t, record, s.step.ID); step.State != aj.Planned {
				t.Fatalf("the slot delete is %s, want it planned", step.State)
			}
			logged := false
			for _, entry := range observed.All() {
				if entry.Message == "planned transfer slot delete left planned" && entry.Level == log.LevelWarn && entry.Attrs["reason"] == reason {
					logged = true
				}
			}
			if !logged {
				t.Fatalf("the settler didn't log %q at Warn with reason %q; logged %+v", "planned transfer slot delete left planned", reason, observed.All())
			}
		})
	}
}

// TestDeleteVMPassesOverALandedTransferOffTheVM stops delete_vm's transfer of
// 777's disk after the slot delete. A second managed disk is then attached to
// 777 and detached, so its parker keeps the record of a finished move off 777.
// A copy of that parker keeps a record of the same disk naming a volume the
// copy still holds, so the second disk can't be resolved. Before the fix,
// delete_vm resolved the landed record's disk, failed, and refused to destroy
// 777 on every rerun. Now it leaves the landed record out before it resolves
// anything, finishes the stopped transfer, and destroys 777.
func TestDeleteVMPassesOverALandedTransferOffTheVM(t *testing.T) {
	s := stopTransferAfterSlotDelete(t, "delete_vm")
	landed := journalLifecycleFlowDisk(t, s.journal, s.client.state, true, false)
	if err := s.call(t, HandleAttachDisk, "777", landed.cid); err != nil {
		t.Fatalf("attach the second disk to 777: %v", err)
	}
	if err := s.call(t, HandleDetachDisk, "777", landed.cid); err != nil {
		t.Fatalf("detach the second disk from 777: %v", err)
	}
	if resolved := (stoppedSlotDelete{deps: s.deps, cid: landed.cid}).resolve(t); resolved.intent != nil || resolved.holder == nil || !resolved.holder.IsParker {
		t.Fatalf("the second disk resolves with holder %+v and intent %+v, want it landed on a parker", resolved.holder, resolved.intent)
	}
	// A copy of the second disk's parker keeps a record of the same disk,
	// with its source on another VM, naming a volume the copy still holds.
	copied := "a:90999/vm-90999-disk-0.raw"
	s.client.state.volumes[copied] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	entry, err := json.Marshal(map[string]any{"bosh_parked_disks": map[string]any{landed.token: map[string]any{
		"disk_cid": landed.cid, "source_vm_cid": "778", "parked_at": "2026-10-01T00:00:00Z", "node": "n2", "volid": copied, "slot": "scsi0",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	s.client.state.configs[90999] = map[string]any{
		"name": "bosh-parker-90999", "tags": "bosh-cpi;bosh-parker;vm-prefix--bosh", "protection": "1", "digest": "1",
		"description": "<!--BOSH:" + string(entry) + "-->", "scsi0": copied + ",size=5G",
	}
	if s.client.vmNodes == nil {
		s.client.vmNodes = map[int]string{}
	}
	s.client.vmNodes[90999] = "n2"
	bare, meta, err := decodeDiskCID(t.Context(), s.deps, "test", landed.cid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDiskForOp(t.Context(), s.deps, "test", landed.cid, bare, meta); err == nil {
		t.Fatal("the second disk still resolves with its serial on 90999 too")
	}
	if err := s.call(t, HandleDeleteVM, "777"); err != nil {
		t.Fatalf("delete_vm refused over a landed transfer off 777: %v", err)
	}
	requireDeleteVMParkedTheDisk(t, s.deps, s.client, s.journal, s.vmID, s.cid)
	s.requireSettled(t)
}
