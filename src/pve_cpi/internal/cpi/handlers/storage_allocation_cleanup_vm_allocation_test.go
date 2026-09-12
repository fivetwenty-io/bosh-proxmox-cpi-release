package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"reflect"
	"strings"
	"testing"
)

type unknownVMCleanupClient struct {
	*deleteManagedClient
	unattached    string
	volumeDeletes int
	loseDestroy   bool
	keepDestroy   bool
	loseVolume    bool
	keepVolume    bool
}

func (c *unknownVMCleanupClient) Nodes() ns.Service {
	return &unknownVMCleanupNodes{Service: c.deleteManagedClient.Nodes(), c: c}
}
func (c *unknownVMCleanupClient) Storage() storage.Service { return &unknownVMCleanupStorage{c: c} }

type unknownVMCleanupNodes struct {
	ns.Service
	c *unknownVMCleanupClient
}

func (n *unknownVMCleanupNodes) DeleteQemu(ctx context.Context, node, vmid string, params *ns.DeleteQemuParams) (*ns.DeleteQemuResponse, error) {
	if n.c.keepDestroy {
		n.c.destroyCount++
		return nil, fmt.Errorf("destroy response lost before effect")
	}
	result, err := n.Service.DeleteQemu(ctx, node, vmid, params)
	if n.c.unattached != "" {
		raw, _ := json.Marshal(map[string]any{"volid": n.c.unattached})
		n.c.nodesRead.content = ns.ListStorageContentResponse{raw}
	}
	if n.c.loseDestroy {
		return nil, fmt.Errorf("destroy response lost after effect")
	}
	if err == nil {
		raw := json.RawMessage(`"UPID:pve1:00088AE5:03547171:6AA18319:qmdestroy:123:pmx@pve!pmx:"`)
		result = &raw
	}
	return result, err
}

type unknownVMCleanupStorage struct {
	storage.Service
	c *unknownVMCleanupClient
}

func (s *unknownVMCleanupStorage) DeleteVolumeAsync(_ context.Context, node, pool, volume string) (string, error) {
	if volume != s.c.unattached || volume == "" {
		return "", fmt.Errorf("unexpected volume deletion")
	}
	s.c.volumeDeletes++
	if s.c.keepVolume {
		return "", fmt.Errorf("volume delete response lost before effect")
	}
	s.c.unattached = ""
	s.c.nodesRead.content = ns.ListStorageContentResponse{}
	if s.c.loseVolume {
		return "", fmt.Errorf("volume delete response lost after effect")
	}
	return "UPID:pve1:00088AE5:03547171:6AA18319:imgdel:123@a:pmx@pve!pmx:", nil
}
func unknownVMAllocationFixture(t *testing.T, role string, mechanism ...string) (Deps, *aj.Journal, *unknownVMCleanupClient, aj.Record, StorageAllocationDecision) {
	return unknownVMAllocationFixtureWithBirthProof(t, role, true, mechanism...)
}
func unknownVMAllocationFixtureWithBirthProof(t *testing.T, role string, birthProof bool, mechanism ...string) (Deps, *aj.Journal, *unknownVMCleanupClient, aj.Record, StorageAllocationDecision) {
	t.Helper()
	deps, j, base := auditFixture(t)
	resume := &resumeVMClient{allocationAuditClient: base, volumes: &resumeVMNodes{Service: base.Nodes()}}
	client := &unknownVMCleanupClient{deleteManagedClient: &deleteManagedClient{resumeVMClient: resume}}
	deps.PVE = &cleanupTaskClient{Client: client}
	def, err := pve.ParseStorageEntry(base.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	root := StoragePlanTarget{Role: storageRoleRoot, Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30, Mechanism: "import", Source: &StorageRootSource{Node: "pve1", StorageID: "a", VolumeID: "a:import/source.qcow2", VirtualBytes: 1 << 30}}
	if len(mechanism) > 0 {
		root.Mechanism = mechanism[0]
		root.Source.TemplateVMID = 100
		root.Source.VolumeID = "a:100/vm-100-disk-0.qcow2"
	}
	ephemeral := StoragePlanTarget{Role: storageRoleEphemeral, Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, Targets: []StoragePlanTarget{root, ephemeral}, VMExecution: &StorageVMExecution{Version: 1, RootDevice: "virtio0", DiskFormat: "qcow2"}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: strings.Repeat("b", 64), PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	target := aj.Target{Node: "pve1", VMID: 123, Storage: "a", Backing: def.BackingKey()}
	kind := "vm." + managedVMCallCreate
	if len(mechanism) > 0 {
		kind = "vm." + managedVMCallClone
	}
	if role == storageRoleEphemeral {
		id, err := storageMutationIntent(h, "vm.root.virtio0", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = storageMutationObserved(h, id, []string{"a:123/vm-123-disk-0.qcow2"}, false); err != nil {
			t.Fatal(err)
		}
		_, suffix, err := pve.ManagedEphemeralVolumeName(def.Type, "qcow2", 123, h.Record().Namespace, h.Record().ID)
		if err != nil {
			t.Fatal(err)
		}
		target.IntendedVolume = "a:" + suffix
		client.unattached = target.IntendedVolume
		kind = "vm." + managedVMCallCreateVolume
	}
	var parameters []byte
	if role == storageRoleEphemeral && birthProof {
		parameters, err = aj.MutationParameters(map[string]any{"version": 1, "kind": "ephemeral_birth", "absence_verified": true})
		if err != nil {
			t.Fatal(err)
		}
	}
	step, err := storageMutationIntent(h, kind, target, nil, parameters)
	if err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "response lost"
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("agent"))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: record.Namespace, Kind: "vm", AllocationID: record.ID, AgentSHA256: hex.EncodeToString(hash[:])})
	if err != nil {
		t.Fatal(err)
	}
	client.configs[123] = map[string]any{"description": marker, "virtio0": "a:123/vm-123-disk-0.qcow2"}
	client.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`)}
	if client.unattached != "" {
		raw, _ := json.Marshal(map[string]any{"volid": client.unattached})
		client.nodesRead.content = append(client.nodesRead.content, raw)
	}
	decision := cleanupAttestedDecision(record.ID)
	if role == storageRoleRoot {
		decision.RecoveredTaskStep = step
		decision.RecoveredTaskUPID = "UPID:pve1:00088AE5:03547171:6AA18319:qmcreate:123:pmx@pve!pmx:"
		if len(mechanism) > 0 {
			decision.RecoveredTaskUPID = "UPID:pve1:00088AE5:03547171:6AA18319:qmclone:100:pmx@pve!pmx:"
		}
	}
	if decision.RecoveredTaskStep != "" {
		decision.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, record, decision)
	}
	return deps, j, client, record, decision
}
func TestCleanupUnknownVMAllocationProvesOwnershipBeforeDisposal(t *testing.T) {
	for _, role := range []string{storageRoleRoot, storageRoleEphemeral} {
		t.Run(role, func(t *testing.T) {
			deps, j, c, before, decision := unknownVMAllocationFixture(t, role)
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if err != nil {
				t.Fatalf("%v: %v", err, unwrapCleanupTest(err))
			}
			if result.State != aj.Cleaned || c.destroyCount != 1 || c.unattached != "" {
				t.Fatal("allocation not completely disposed")
			}
			for i, step := range before.Steps {
				if !reflect.DeepEqual(result.Steps[i], step) {
					t.Fatal("unknown history rewritten")
				}
			}
			if role == storageRoleEphemeral && c.volumeDeletes != 1 {
				t.Fatal("unattached full-UUID volume not explicitly removed")
			}
		})
	}
}

func TestCleanupUnknownRootCanProveCompleteAbsence(t *testing.T) {
	deps, j, c, before, decision := unknownVMAllocationFixture(t, storageRoleRoot)
	delete(c.configs, 123)
	c.nodesRead.content = ns.ListStorageContentResponse{}
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if err != nil {
		t.Fatalf("%v: %v", err, unwrapCleanupTest(err))
	}
	if result.State != aj.Cleaned || c.destroyCount != 0 || c.volumeDeletes != 0 || !reflect.DeepEqual(before.Steps[0], result.Steps[0]) {
		t.Fatal("absence cleanup mutated resources or history")
	}
}
func TestCleanupUnknownVMAllocationRefusesIncompleteOwnership(t *testing.T) {
	for _, role := range []string{storageRoleRoot, storageRoleEphemeral} {
		for _, mode := range []string{"marker missing", "wrong marker", "changed backing", "foreign reference", "task visibility", "unsettled request", "orphan volume"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				deps, j, c, record, decision := unknownVMAllocationFixture(t, role)
				switch mode {
				case "marker missing":
					delete(c.configs[123], "description")
				case "wrong marker":
					c.configs[123]["description"] = "unrelated VM"
				case "changed backing":
					c.storageRead.definitions[0] = json.RawMessage(`{"storage":"a","type":"nfs","server":"other","export":"/a","content":"images","shared":1}`)
				case "foreign reference":
					volume := "a:123/vm-123-disk-0.qcow2"
					if role == storageRoleEphemeral {
						volume = c.unattached
					}
					c.configs[999] = map[string]any{"name": "foreign", "scsi0": volume}
				case "task visibility":
					deps.PVE.(*cleanupTaskClient).incomplete = true
				case "unsettled request":
					decision.RemoteTasksSettled = false
				case "orphan volume":
					delete(c.configs, 123)
				}
				before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
				if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
					t.Fatal("unsafe disposition accepted")
				}
				if c.destroyCount != 0 || c.volumeDeletes != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
					t.Fatal("refusal changed resources or journal")
				}
				after, err := j.Inspect(record.ID)
				if err != nil || after.State == aj.Cleaned {
					t.Fatal("uncertain record discarded")
				}
			})
		}
	}
}

func (n *unknownVMCleanupNodes) ListLxc(context.Context, string) (*ns.ListLxcResponse, error) {
	rows := ns.ListLxcResponse{}
	return &rows, nil
}

func TestCleanupUnknownRootDestroyResponseCannotReplay(t *testing.T) {
	for _, remains := range []bool{false, true} {
		t.Run(fmt.Sprint(remains), func(t *testing.T) {
			deps, j, c, before, decision := unknownVMAllocationFixture(t, storageRoleRoot)
			c.stopped = true
			c.loseDestroy = true
			c.keepDestroy = remains
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("lost response hidden")
			}
			if c.destroyCount != 1 {
				t.Fatal("first destroy missing")
			}
			prior, err := j.Inspect(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			decision.DecisionID += "-second"
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if remains {
				if err == nil {
					t.Fatal("present VM admitted after unknown destruction")
				}
			} else if err != nil || result.State != aj.Cleaned {
				t.Fatalf("absent VM not finalized: %v (%v)", err, unwrapCleanupTest(err))
			}
			if c.destroyCount != 1 {
				t.Fatal("unknown destruction replayed")
			}
			after, e := j.Inspect(before.ID)
			if e != nil {
				t.Fatal(e)
			}
			for i, step := range prior.Steps {
				if !reflect.DeepEqual(step, after.Steps[i]) {
					t.Fatal("unknown history rewritten")
				}
			}
		})
	}
}

func TestCleanupUnknownEphemeralAfterLostDestroyRetainsExactOwnership(t *testing.T) {
	deps, j, c, before, decision := unknownVMAllocationFixture(t, storageRoleEphemeral)
	c.stopped = true
	c.loseDestroy = true
	if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
		t.Fatal("lost destruction hidden")
	}
	if c.destroyCount != 1 || c.volumeDeletes != 0 || c.unattached == "" {
		t.Fatal("fixture did not preserve unreferenced volume")
	}
	prior, err := j.Inspect(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision.DecisionID += "-second"
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if err != nil || result.State != aj.Cleaned {
		t.Fatalf("supported orphan cleanup failed: %v (%v)", err, unwrapCleanupTest(err))
	}
	if c.destroyCount != 1 || c.volumeDeletes != 1 {
		t.Fatal("cleanup replayed destruction or omitted orphan")
	}
	for i, step := range prior.Steps {
		if !reflect.DeepEqual(step, result.Steps[i]) {
			t.Fatal("history changed")
		}
	}
}

func TestCleanupUnknownEphemeralDeleteResponseRequiresAbsence(t *testing.T) {
	for _, remains := range []bool{false, true} {
		t.Run(fmt.Sprint(remains), func(t *testing.T) {
			deps, j, c, before, decision := unknownVMAllocationFixture(t, storageRoleEphemeral)
			c.stopped = true
			c.loseVolume = true
			c.keepVolume = remains
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("lost deletion hidden")
			}
			if c.destroyCount != 1 || c.volumeDeletes != 1 {
				t.Fatal("first cleanup not exercised")
			}
			prior, err := j.Inspect(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			decision.DecisionID += "-second"
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if remains {
				if err == nil {
					t.Fatal("remaining volume admitted")
				}
			} else if err != nil || result.State != aj.Cleaned {
				t.Fatalf("absent volume not finalized: %v (%v)", err, unwrapCleanupTest(err))
			}
			if c.destroyCount != 1 || c.volumeDeletes != 1 {
				t.Fatal("unknown deletion replayed")
			}
			after, e := j.Inspect(before.ID)
			if e != nil {
				t.Fatal(e)
			}
			for i, step := range prior.Steps {
				if !reflect.DeepEqual(step, after.Steps[i]) {
					t.Fatal("history changed")
				}
			}
		})
	}
}

func TestCleanupFailedRootTaskStillRequiresActualOwnership(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			deps, j, c, r, decision := unknownVMAllocationFixture(t, storageRoleRoot)
			deps.PVE.(*cleanupTaskClient).taskExit = "allocation failed"
			if !owned {
				delete(c.configs[123], "description")
			}
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if owned {
				if err != nil || result.State != aj.Cleaned || c.destroyCount != 1 {
					t.Fatalf("owned failed allocation not cleaned: %v", err)
				}
			} else if err == nil || c.destroyCount != 0 {
				t.Fatal("failed task used as ownership")
			}
			after, e := j.Inspect(r.ID)
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(after.Steps[0], r.Steps[0]) {
				t.Fatal("failure history rewritten")
			}
		})
	}
}

func TestCleanupRecoveredCloneUsesSourceTaskAndOwnedDestination(t *testing.T) {
	for _, mechanism := range []string{"full_clone", "linked_clone"} {
		t.Run(mechanism, func(t *testing.T) {
			deps, j, c, r, decision := unknownVMAllocationFixture(t, storageRoleRoot, mechanism)
			result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
			if err != nil || result.State != aj.Cleaned || c.destroyCount != 1 {
				t.Fatalf("owned clone cleanup failed: %v (%v)", err, unwrapCleanupTest(err))
			}
			if !reflect.DeepEqual(result.Steps[0], r.Steps[0]) {
				t.Fatal("unknown clone history rewritten")
			}
		})
	}
}

func TestCleanupUnknownEphemeralRequiresRetainedBirthAbsence(t *testing.T) {
	_, _, _, record, _ := unknownVMAllocationFixture(t, storageRoleEphemeral)
	step := record.Steps[len(record.Steps)-1]
	if !cleanupPendingVMAllocation(step, record) {
		t.Fatal("valid birth proof refused")
	}
	for _, raw := range [][]byte{nil, []byte(`{"version":1,"kind":"ephemeral_birth","absence_verified":false}`), []byte(`{"version":1,"kind":"persistent_birth","absence_verified":true}`)} {
		changed := step
		changed.Parameters = raw
		if cleanupPendingVMAllocation(changed, record) {
			t.Fatal("missing or different pre-submission absence accepted")
		}
	}
}

func TestCleanupFailedRootCanDisposeMarkedEmptyVM(t *testing.T) {
	deps, j, c, _, decision := unknownVMAllocationFixture(t, storageRoleRoot)
	deps.PVE.(*cleanupTaskClient).taskExit = "import failed before disk creation"
	delete(c.configs[123], "virtio0")
	c.nodesRead.content = ns.ListStorageContentResponse{}
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
	if err != nil || result.State != aj.Cleaned || c.destroyCount != 1 {
		t.Fatalf("marked empty VM not disposed: %v (%v)", err, unwrapCleanupTest(err))
	}
}

func TestExplicitCleanupCannotAdoptSameSizeEphemeralWithoutBirthAbsence(t *testing.T) {
	deps, j, c, record, decision := unknownVMAllocationFixtureWithBirthProof(t, storageRoleEphemeral, false)
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
		t.Fatal("same-size volume adopted without original absence")
	}
	if c.destroyCount != 0 || c.volumeDeletes != 0 || c.unattached == "" || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("refusal changed original artifacts or journal")
	}
	after, err := j.Inspect(record.ID)
	if err != nil || after.State == aj.Cleaned {
		t.Fatal("unproven birth discarded")
	}
}

func TestCleanupMarkedEmptyRootRefusesOtherDevicesOrVMIDArtifacts(t *testing.T) {
	for _, mode := range []string{"other device", "orphan file"} {
		t.Run(mode, func(t *testing.T) {
			deps, j, c, _, decision := unknownVMAllocationFixture(t, storageRoleRoot)
			deps.PVE.(*cleanupTaskClient).taskExit = "import failed before disk creation"
			delete(c.configs[123], "virtio0")
			c.nodesRead.content = ns.ListStorageContentResponse{}
			if mode == "other device" {
				c.configs[123]["scsi2"] = "a:999/vm-999-disk-0.qcow2"
			} else {
				c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-unrecorded.qcow2"}`)}
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
				t.Fatal("incomplete empty-root proof admitted")
			}
			if c.destroyCount != 0 || c.volumeDeletes != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refusal changed resources or history")
			}
		})
	}
}
