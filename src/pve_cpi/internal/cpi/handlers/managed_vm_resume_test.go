package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type resumeVMNodes struct {
	ns.Service
	missing bool
}

func (n *resumeVMNodes) ListNodes(context.Context) (*ns.ListNodesResponse, error) {
	response := ns.ListNodesResponse{json.RawMessage(`{"node":"pve1","status":"online"}`)}
	return &response, nil
}

func (n *resumeVMNodes) GetStorageContent(context.Context, string, string, string) (*ns.GetStorageContentResponse, error) {
	if n.missing {
		return nil, errors.New("backend-secret")
	}
	return &ns.GetStorageContentResponse{Size: 1 << 30, Format: "qcow2"}, nil
}

type resumeVMClient struct {
	*allocationAuditClient
	volumes *resumeVMNodes
}

func (c *resumeVMClient) Nodes() ns.Service { return c.volumes }

func resumeVMFixture(t *testing.T, withISO ...bool) (Deps, *aj.Journal, *resumeVMClient, aj.Record, []json.RawMessage) {
	t.Helper()
	deps, j, base := auditFixture(t)
	client := &resumeVMClient{allocationAuditClient: base, volumes: &resumeVMNodes{Service: base.Nodes()}}
	deps.PVE = client
	def, err := pve.ParseStorageEntry(base.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, Targets: []StoragePlanTarget{{Role: "root", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}, {Role: "ephemeral", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}}}
	if len(withISO) > 0 && withISO[0] {
		plan.Targets = append(plan.Targets, StoragePlanTarget{Role: storageRoleISO, Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), CapacityKey: def.BackingKey(), Mechanism: "upload", VirtualBytes: 10 << 20, ChargeBytes: 10 << 20})
	}
	args := []json.RawMessage{json.RawMessage(`"agent"`)}
	fp, err := storageCallerIntentFingerprint("create_vm", args)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(context.Background(), "agent", aj.Intent{IntentFingerprint: fp, PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []struct{ kind, volume string }{{"vm.root.virtio0", "a:123/vm-123-disk-0.qcow2"}, {"vm.ephemeral.scsi1", "a:123/vm-123-disk-1.qcow2"}} {
		id, err := storageMutationIntent(h, binding.kind, aj.Target{Node: "pve1", VMID: 123, Storage: "a", Backing: def.BackingKey()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = storageMutationObserved(h, id, []string{binding.volume}, false); err != nil {
			t.Fatal(err)
		}
	}
	record := h.Record()
	record.CID = "123"
	record.State = aj.ReadyToReturn
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("agent"))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: "director", Kind: "vm", AllocationID: record.ID, AgentSHA256: hex.EncodeToString(hash[:])})
	if err != nil {
		t.Fatal(err)
	}
	client.configs[123] = map[string]any{"description": marker, "virtio0": "a:123/vm-123-disk-0.qcow2", "scsi1": "a:123/vm-123-disk-1.qcow2"}
	return deps, j, client, record, args
}
func TestManagedVMResumeReturnsRecordedCIDWithoutReplanning(t *testing.T) {
	deps, j, c, record, args := resumeVMFixture(t)
	// An unrelated storage-content outage must not turn verified ownership into
	// an absence claim or prevent returning this existing allocation.
	c.nodesRead.failure = errors.New("unrelated NAS unavailable")
	deps.Config.StorageSets = nil
	deps.Config.EphemeralStorageSet = "removed"
	result, err := resumeManagedVM(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, args, nil)
	response, ok := result.([]any)
	if err != nil || !ok || len(response) != 2 || response[0] != "123" {
		t.Fatalf("resume = %v, %v", result, err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := response[1].(map[string]createVMNetworkSpec); !ok {
		t.Fatal("VM response lost networks map")
	}
	if len(after.Verifications) != 1 || !after.Verifications[0].OwnershipVerified || after.Verifications[0].AbsenceVerified {
		t.Fatal("ownership proof not durably retained")
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("resume mutated VM")
	}
}
func TestManagedVMResumeRejectsLostOrChangedOwnership(t *testing.T) {
	cases := []struct {
		name   string
		change func(*resumeVMClient)
	}{
		{"missing marker", func(c *resumeVMClient) { c.configs[123]["description"] = "" }},
		{"missing VM", func(c *resumeVMClient) { delete(c.configs, 123) }},
		{"rebound device", func(c *resumeVMClient) { c.configs[123]["scsi1"] = "a:123/vm-123-disk-0.qcow2" }},
		{"unreadable volume", func(c *resumeVMClient) { c.volumes.missing = true }},
		{"repointed backing", func(c *resumeVMClient) {
			c.storageRead.definitions[0] = json.RawMessage(`{"storage":"a","type":"nfs","server":"other","export":"/a","content":"images","shared":1}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, j, c, record, args := resumeVMFixture(t)
			tc.change(c)
			result, err := resumeManagedVM(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, args, nil)
			if err == nil || result != nil || strings.Contains(err.Error(), "backend-secret") {
				t.Fatalf("unsafe recovery: %v %v", result, err)
			}
			after, e := j.Inspect(record.ID)
			if e != nil || len(after.Verifications) != 0 {
				t.Fatal("failed proof changed authority")
			}
			if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
				t.Fatal("failed recovery mutated VM")
			}
		})
	}
}
func TestManagedVMResumeCallerConflictDoesNotTouchVM(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	_, err := resumeManagedVM(context.Background(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, []json.RawMessage{json.RawMessage(`"changed"`)}, nil)
	if err == nil || len(c.descWrites) != 0 {
		t.Fatal("changed caller resumed generation")
	}
}

func TestManagedVMResumeMarkerCountExcludesOwnedVolumeRows(t *testing.T) {
	deps, journal, client, record, _ := resumeVMFixture(t)
	client.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`), json.RawMessage(`{"volid":"a:123/vm-123-disk-1.qcow2"}`)}
	observed, err := observeManagedVMRecord(t.Context(), deps, journal, record)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Volumes) != 2 || !observed.Verification.OwnershipVerified {
		t.Fatal("volume rows changed unique-marker proof")
	}
	client.configs[123]["unused0"] = "a:123/vm-123-disk-0.qcow2"
	if _, err = observeManagedVMRecord(t.Context(), deps, journal, record); err == nil {
		t.Fatal("duplicate recorded volume device accepted")
	}
}

func TestExistingManagedVMResumesBeforeRemovedPolicyResolution(t *testing.T) {
	deps, journal, client, record, args := resumeVMFixture(t)
	deps.Config.StorageSets = nil
	deps.Config.EphemeralStorageSet = ""
	deps.Config.RootStorageSet = ""
	deps.Config.PersistentStorageSet = ""
	parsed := &createVMParsedArgs{agentID: "agent", cloudPropsMap: map[string]any{"ephemeral_storage_set": "removed"}}
	result, found, err := resumeExistingManagedVM(context.Background(), deps, args, parsed)
	if err != nil || !found {
		t.Fatalf("existing generation hidden by removed policy: found=%v err=%v", found, err)
	}
	response, ok := result.([]any)
	if !ok || len(response) != 2 || response[0] != record.CID {
		t.Fatalf("wrong retained result: %#v", result)
	}
	current, err := journal.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Verifications) != 1 || !current.Verifications[0].OwnershipVerified {
		t.Fatal("fresh ownership proof not retained")
	}
	if len(client.descWrites) != 0 || len(client.destroyed) != 0 {
		t.Fatal("ready resume mutated PVE")
	}
}

func TestExistingManagedVMLegacyWithoutAuthoritySkipsLookup(t *testing.T) {
	_, found, err := resumeExistingManagedVM(context.Background(), Deps{}, nil, nil)
	if err != nil || found {
		t.Fatalf("legacy lookup: found=%v err=%v", found, err)
	}
}

func TestManagedVMResumeRejectsChangedLocalityBehindHAMarker(t *testing.T) {
	deps, journal, client, _, _ := resumeVMFixture(t)
	delete(client.configs, 123)
	old := pve.StorageInfo{Name: "a", Type: "dir", Path: "/a", Content: "images", Shared: true}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "second-agent", PolicyFingerprint: strings.Repeat("b", 64), Node: "pve2", HANodes: []string{"pve1"}, Definitions: map[string]pve.StorageInfo{"a": old}, Targets: []StoragePlanTarget{{Role: "root", Node: "pve2", StorageID: "a", BackingKey: old.BackingKey(), VirtualBytes: 1 << 30}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	h, err := journal.AcquireVM(t.Context(), "second-agent", aj.Intent{IntentFingerprint: strings.Repeat("b", 64), PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	volume := "a:456/vm-456-disk-0.raw"
	step, err := storageMutationIntent(h, "vm.root.virtio0", aj.Target{Node: "pve2", VMID: 456, Storage: "a", Backing: old.BackingKey()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = storageMutationObserved(h, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	record.State = aj.ReadyToReturn
	record.CID = "456"
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(record.AgentID))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: record.Namespace, Kind: "vm", AllocationID: record.ID, AgentSHA256: hex.EncodeToString(hash[:])})
	if err != nil {
		t.Fatal(err)
	}
	client.configs[456] = map[string]any{"description": marker, "virtio0": volume}
	client.storageRead.definitions = []json.RawMessage{json.RawMessage(`{"storage":"a","type":"dir","path":"/a","content":"images","shared":0}`)}
	if _, err = observeManagedVMRecord(t.Context(), deps, journal, record); err == nil {
		t.Fatal("HA marker authorized a different node-local physical copy")
	}
}

func TestManagedVMResumeIgnoresExternalPreservationTargets(t *testing.T) {
	deps, j, _, record, _ := resumeVMFixture(t)
	record.Steps = append(record.Steps, aj.Step{ID: "external-preserve", Kind: "disk.park", State: aj.Observed, Target: aj.Target{External: true, Node: "pve1", VMID: 999, Storage: "foreign", Backing: "foreign", IntendedVolume: "foreign:999/disk.raw"}, VolIDs: []string{"foreign:999/disk.raw"}})
	if _, err := observeManagedVMRecord(t.Context(), deps, j, record); err != nil {
		t.Fatalf("external disk polluted VM ownership: %v", err)
	}
	record.State = aj.VMDeletedRetained
	if _, err := observeManagedVMRecord(t.Context(), deps, j, record); err == nil {
		t.Fatal("retained-only generation resumed as live VM")
	}
}
