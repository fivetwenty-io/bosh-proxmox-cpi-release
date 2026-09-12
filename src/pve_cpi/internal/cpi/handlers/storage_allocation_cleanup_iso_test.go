package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

const cleanupTestISO = "a:iso/vm-123-config.iso"
const cleanupTestUploadUPID = "UPID:pve1:00088AE5:03547171:6AA18319:imgcopy::pmx@pve!pmx:"

type cleanupISOClient struct {
	*deleteManagedClient
	iso         bool
	ctime       int64
	size        sdk.PVEInt
	deleted     int
	lostDestroy bool
	lostDelete  bool
	keepDelete  bool
}

func (c *cleanupISOClient) Nodes() ns.Service {
	return &cleanupISONodes{Service: c.deleteManagedClient.Nodes(), c: c}
}
func (c *cleanupISOClient) Storage() storage.Service {
	return &cleanupISOStorage{c: c}
}

type cleanupISONodes struct {
	ns.Service
	c *cleanupISOClient
}

func (n *cleanupISONodes) ListStorageContent(ctx context.Context, node, store string, params *ns.ListStorageContentParams) (*ns.ListStorageContentResponse, error) {
	rows, err := n.Service.ListStorageContent(ctx, node, store, params)
	if err != nil {
		return nil, err
	}
	out := append(ns.ListStorageContentResponse{}, (*rows)...)
	if n.c.iso {
		raw, _ := json.Marshal(map[string]any{"volid": cleanupTestISO, "content": "iso", "format": "iso", "size": n.c.size, "ctime": n.c.ctime})
		out = append(out, raw)
	}
	return &out, nil
}
func (n *cleanupISONodes) GetStorageContent(ctx context.Context, node, store, volume string) (*ns.GetStorageContentResponse, error) {
	if strings.HasPrefix(volume, "iso/") {
		return &ns.GetStorageContentResponse{Format: "raw", Size: n.c.size}, nil
	}
	return n.Service.GetStorageContent(ctx, node, store, volume)
}

type cleanupISOStorage struct {
	storage.Service
	c *cleanupISOClient
}

func (s *cleanupISOStorage) DeleteVolumeAsync(_ context.Context, node, store, volume string) (string, error) {
	if node != "pve1" || store != "a" || volume != cleanupTestISO {
		return "", fmt.Errorf("unexpected deletion")
	}
	s.c.deleted++
	if s.c.keepDelete {
		return "", fmt.Errorf("delete response lost before effect")
	}
	s.c.iso = false
	if s.c.lostDelete {
		return "", fmt.Errorf("delete response lost after effect")
	}
	return "UPID:pve1:00088AE5:03547171:6AA18319:imgdel:a:pmx@pve!pmx:", nil
}

func cleanupISOFixture(t *testing.T, responseLost ...bool) (Deps, *aj.Journal, *cleanupISOClient, aj.Record, aj.Step) {
	t.Helper()
	deps, j, base, record, _ := resumeVMFixture(t, true)
	c := &cleanupISOClient{deleteManagedClient: &deleteManagedClient{resumeVMClient: base}, iso: true, ctime: 1788969753, size: 10 << 20}
	c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`), json.RawMessage(`{"volid":"a:123/vm-123-disk-1.qcow2"}`)}
	deps.PVE = &cleanupTaskClient{Client: c}
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.State = aj.ReconciliationRequired
	r.Reason = "uploaded ISO readback rejected"
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	plan, err := activeStorageAllocationPlan(r)
	if err != nil {
		t.Fatal(err)
	}
	iso, _ := managedVMRoleTarget(plan, storageRoleISO)
	step := aj.Step{ID: "pending-iso", Kind: "vm." + managedVMCallUpload, State: aj.Submitted, UPID: cleanupTestUploadUPID, Target: aj.Target{Node: "pve1", Storage: "a", Backing: iso.BackingKey, VMID: 123, IntendedVolume: cleanupTestISO}, Charges: []aj.Charge{{Backing: iso.CapacityKey, PlannedBytes: 10 << 20, OutstandingBytes: 10 << 20}}}
	planned := step
	planned.State = aj.Planned
	planned.UPID = ""
	r.Steps = append(r.Steps, planned)
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	if len(responseLost) > 0 && responseLost[0] {
		step = planned
	}
	r.Steps[len(r.Steps)-1] = step
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	return deps, j, c, r, step
}

func TestCleanupSubmittedISOUploadPreservesHistory(t *testing.T) {
	deps, j, c, r, step := cleanupISOFixture(t)
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(r.ID))
	if err != nil {
		t.Fatalf("cleanup: %v cause %v", err, unwrapCleanupTest(err))
	}
	if result.State != aj.Cleaned || c.destroyCount != 1 || c.deleted != 1 || c.iso {
		t.Fatalf("cleanup state %s destroy %d iso-delete %d", result.State, c.destroyCount, c.deleted)
	}
	found := 0
	for _, got := range result.Steps {
		if got.ID == step.ID {
			found++
		}
		if got.ID == step.ID && !reflect.DeepEqual(got, step) {
			t.Fatal("submitted upload history changed")
		}
	}
	if found != 1 {
		t.Fatal("submitted history missing or duplicated")
	}
	if storageLifecycleSettled(result) == nil {
		t.Fatal("ordinary lifecycle accepted submitted history")
	}
}
func unwrapCleanupTest(err error) error {
	for {
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = next.Unwrap()
	}
}

func TestCleanupSubmittedISORefusesIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"timestamp", "size", "missing ISO", "missing marker", "task failure"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, c, r, _ := cleanupISOFixture(t)
			switch mode {
			case "timestamp":
				c.ctime++
			case "size":
				c.size--
			case "missing ISO":
				c.iso = false
			case "missing marker":
				delete(c.configs[123], "description")
			case "task failure":
				deps.PVE.(*cleanupTaskClient).taskErr = fmt.Errorf("task not settled")
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(r.ID)); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if c.destroyCount != 0 || c.deleted != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refusal mutated remote or authority")
			}
		})
	}
}
func TestCleanupSubmittedISORejectsUnrelatedTaskAndTarget(t *testing.T) {
	_, _, _, record, step := cleanupISOFixture(t)
	for _, mode := range []string{"node", "task type", "task owner", "filename", "charge", "unsubmitted"} {
		t.Run(mode, func(t *testing.T) {
			s := step
			s.Charges = append([]aj.Charge(nil), step.Charges...)
			switch mode {
			case "node":
				s.Target.Node = "other"
			case "task type":
				s.UPID = strings.Replace(s.UPID, "imgcopy", "qmclone", 1)
			case "task owner":
				s.UPID = strings.Replace(s.UPID, "imgcopy::", "imgcopy:123:", 1)
			case "filename":
				s.Target.IntendedVolume = "a:iso/vm-124-config.iso"
			case "charge":
				s.Charges[0].OutstandingBytes--
			case "unsubmitted":
				s.State = aj.Planned
			}
			if _, ok := cleanupSubmittedISOUpload(s, record); ok {
				t.Fatal("unrelated upload accepted")
			}
		})
	}
}

func (n *cleanupISONodes) ListLxc(context.Context, string) (*ns.ListLxcResponse, error) {
	rows := ns.ListLxcResponse{}
	return &rows, nil
}
func (n *cleanupISONodes) DeleteQemu(ctx context.Context, node, vmid string, params *ns.DeleteQemuParams) (*ns.DeleteQemuResponse, error) {
	result, err := n.Service.DeleteQemu(ctx, node, vmid, params)
	if n.c.lostDestroy {
		return nil, fmt.Errorf("destroy response lost after effect")
	}
	if err == nil {
		raw := json.RawMessage(`"UPID:pve1:00088AE5:03547171:6AA18319:qmdestroy:123:pmx@pve!pmx:"`)
		result = &raw
	}
	return result, err
}

// PVE's receiving API node owns the upload worker, independently of the
// destination in the exact retained request receipt.
type cleanupUploadWorkerClient struct {
	*cleanupTaskClient
	foreign bool
}

func (c *cleanupUploadWorkerClient) Nodes() ns.Service {
	return &cleanupUploadWorkerNodes{Service: c.cleanupTaskClient.Nodes(), foreign: c.foreign}
}

type cleanupUploadWorkerNodes struct {
	ns.Service
	foreign bool
}

func (n *cleanupUploadWorkerNodes) ListNodes(context.Context) (*ns.ListNodesResponse, error) {
	rows := ns.ListNodesResponse{json.RawMessage(`{"node":"pve1","status":"online"}`)}
	if !n.foreign {
		rows = append(rows, json.RawMessage(`{"node":"pve0","status":"online"}`))
	}
	return &rows, nil
}
func TestCleanupRecoveredISOUploadWorkerIdentity(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			deps, j, c, r, step := cleanupISOFixture(t, true)
			worker := &cleanupUploadWorkerClient{cleanupTaskClient: deps.PVE.(*cleanupTaskClient), foreign: foreign}
			deps.PVE = worker
			decision := cleanupAttestedDecision(r.ID)
			decision.RecoveredTaskStep = step.ID
			decision.RecoveredTaskUPID = strings.Replace(cleanupTestUploadUPID, "UPID:pve1:", "UPID:pve0:", 1)
			decision.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, r, decision)
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			_, proof, err := admitStorageCleanupSettlement(t.Context(), deps, r, decision)
			if (err == nil) == foreign {
				t.Fatalf("foreign=%v proof=%+v err=%v", foreign, proof, err)
			}
			if !foreign && (len(proof.TaskObservation.Tasks) != 1 || proof.TaskObservation.Tasks[0].Node != "pve0") {
				t.Fatal("worker identity lost")
			}
			if foreign && worker.taskCalls != 0 {
				t.Fatal("foreign worker queried")
			}
			if c.destroyCount != 0 || c.deleted != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("admission changed original history or resources")
			}
			_ = j
		})
	}
}
func TestCleanupUploadedISORecordedInterval(t *testing.T) {
	for _, mode := range []string{"completion second", "before start", "after end", "inverted interval"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, c, r, _ := cleanupISOFixture(t)
			deps.PVE.(*cleanupTaskClient).uploadEndOffset = 1
			c.ctime++
			switch mode {
			case "before start":
				c.ctime -= 2
			case "after end":
				c.ctime++
			case "inverted interval":
				deps.PVE.(*cleanupTaskClient).uploadEndOffset = -1
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(r.ID))
			if mode == "completion second" {
				if err != nil || result.State != aj.Cleaned || c.destroyCount != 1 || c.deleted != 1 {
					t.Fatalf("recorded completion timestamp refused: %v", err)
				}
			} else if err == nil || c.destroyCount != 0 || c.deleted != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("unproven timestamp mutated resources or history")
			}
		})
	}
}
