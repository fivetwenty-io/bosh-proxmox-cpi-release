package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	ce "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	"strconv"
	"strings"
	"testing"
)

type deleteManagedClient struct {
	*resumeVMClient
	stopCount, destroyCount int
	unknownDestroy          bool
	stopped                 bool
}

func (c *deleteManagedClient) QEMU() qemu.Service {
	return &deleteManagedQEMU{Service: c.resumeVMClient.QEMU(), c: c}
}
func (c *deleteManagedClient) Nodes() ns.Service {
	return &deleteManagedNodes{Service: c.resumeVMClient.Nodes(), c: c}
}
func (c *deleteManagedClient) Cluster() cluster.Service {
	return &deleteManagedCluster{Service: c.resumeVMClient.Cluster()}
}
func (c *deleteManagedClient) Tasks() tasks.Service { return &deleteManagedTasks{} }

type deleteManagedCluster struct{ cluster.Service }

func (c *deleteManagedCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	rows := cluster.ListHaRulesResponse{}
	return &rows, nil
}

func (c *deleteManagedCluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	return nil, ce.VMNotFound("ha")
}

type deleteManagedQEMU struct {
	qemu.Service
	c *deleteManagedClient
}

func (q *deleteManagedQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if _, ok := q.c.configs[vmid]; !ok {
		return nil, ce.VMNotFound(strconv.Itoa(vmid))
	}
	return q.Service.Config(ctx, node, vmid)
}
func (q *deleteManagedQEMU) Status(context.Context, string, int) (map[string]any, error) {
	status := "running"
	if q.c.stopped {
		status = "stopped"
	}
	return map[string]any{"status": status}, nil
}
func (q *deleteManagedQEMU) Stop(context.Context, string, int) (string, error) {
	q.c.stopCount++
	q.c.stopped = true
	return "UPID:pve1:stop", nil
}

type deleteManagedNodes struct {
	ns.Service
	c *deleteManagedClient
}

func (n *deleteManagedNodes) DeleteQemu(_ context.Context, _ string, vmid string, params *ns.DeleteQemuParams) (*ns.DeleteQemuResponse, error) {
	n.c.destroyCount++
	if params == nil || params.DestroyUnreferencedDisks == nil || *params.DestroyUnreferencedDisks || params.Purge == nil || !*params.Purge {
		return nil, errors.New("unsafe destroy parameters")
	}
	id, _ := strconv.Atoi(vmid)
	delete(n.c.configs, id)
	n.c.nodesRead.content = ns.ListStorageContentResponse{}
	if n.c.unknownDestroy {
		return nil, errors.New("lost response secret")
	}
	raw := json.RawMessage(`"UPID:pve1:destroy"`)
	return &raw, nil
}
func deleteManagedFixture(t *testing.T) (Deps, *aj.Journal, *deleteManagedClient, aj.Record) {
	deps, j, base, record, _ := resumeVMFixture(t)
	c := &deleteManagedClient{resumeVMClient: base}
	deps.PVE = c
	c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`), json.RawMessage(`{"volid":"a:123/vm-123-disk-1.qcow2"}`)}
	return deps, j, c, record
}
func TestManagedVMDeleteRemovedSetsWaitsAndCloses(t *testing.T) {
	deps, j, c, record := deleteManagedFixture(t)
	deps.Config.StorageSets = nil
	deps.Config.EphemeralStorageSet = "removed"
	handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if err != nil || !handled {
		t.Fatalf("delete: %v %v", handled, err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.Deleted || c.stopCount != 1 || c.destroyCount != 1 {
		t.Fatalf("state %s stop %d destroy %d", after.State, c.stopCount, c.destroyCount)
	}
	if len(after.Verifications) == 0 || !after.Verifications[len(after.Verifications)-1].AbsenceVerified {
		t.Fatal("missing durable whole-allocation absence")
	}
	for _, step := range after.Steps {
		if step.Kind == "vm.delete.destroy" && (step.UPID == "" || step.State != aj.Observed) {
			t.Fatal("destroy task evidence missing")
		}
	}
}
func TestManagedVMDeleteLostDestroyResponseNeverResubmits(t *testing.T) {
	deps, j, c, record := deleteManagedFixture(t)
	c.unknownDestroy = true
	if handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); !handled || err == nil {
		t.Fatal("unknown destruction succeeded")
	}
	after, _ := j.Inspect(record.ID)
	if after.State != aj.ReconciliationRequired {
		t.Fatal("unknown outcome not retained")
	}
	if _, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); err == nil {
		t.Fatal("unknown task treated as known absence")
	}
	if c.destroyCount != 1 {
		t.Fatal("destruction resubmitted")
	}
}
func TestManagedVMDeleteUnsettledDoesNotMutate(t *testing.T) {
	deps, j, c, record := deleteManagedFixture(t)
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := h.Record()
	r.State = aj.ReconciliationRequired
	r.Reason = "unknown"
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	r = h.Record()
	r.Steps = append(r.Steps, aj.Step{ID: "unknown", Kind: "vm.start", Target: aj.Target{Node: "pve1", VMID: 123}, State: aj.Planned})
	if err = h.Save(r); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); err == nil {
		t.Fatal("unsettled deletion admitted")
	}
	if c.stopCount != 0 || c.destroyCount != 0 {
		t.Fatal("unsettled allocation mutated")
	}
}

type deleteManagedTasks struct{ tasks.Service }

func (t *deleteManagedTasks) Wait(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
	return &tasks.Status{Status: "stopped", ExitStatus: "OK"}, nil
}
func (t *deleteManagedTasks) GetStatus(context.Context, string, string) (*tasks.Status, error) {
	return &tasks.Status{Status: "stopped", ExitStatus: "OK"}, nil
}

func TestManagedVMDeletedCIDCannotDeleteReusedUnmarkedVM(t *testing.T) {
	deps, journal, client, record := deleteManagedFixture(t)
	if handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); err != nil || !handled {
		t.Fatalf("first deletion: %v %v", handled, err)
	}
	terminal, err := journal.Inspect(record.ID)
	if err != nil || terminal.State != aj.Deleted {
		t.Fatalf("terminal state: %+v %v", terminal, err)
	}
	client.configs[123] = map[string]any{"name": "unrelated-reused-id", "virtio0": "a:123/unrelated.raw"}
	count := client.destroyCount
	if handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); !handled || err == nil {
		t.Fatalf("reused identity not intercepted: %v %v", handled, err)
	}
	if client.destroyCount != count {
		t.Fatal("terminal CID destroyed reused VM")
	}
	delete(client.configs, 123)
	if handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); !handled || err != nil {
		t.Fatalf("absent terminal CID not idempotent: %v %v", handled, err)
	}
}

func TestManagedVMDefinitionScopeIsRecheckedBeforeMutation(t *testing.T) {
	deps, _, client, record := deleteManagedFixture(t)
	def := pve.StorageInfo{Name: "a", Type: "dir", Path: "/same", Content: "images", Shared: true}
	client.storageRead.definitions = []json.RawMessage{planJSON(t, map[string]any{"storage": "a", "type": "dir", "path": "/same", "content": "images", "shared": 0})}
	target := record.Steps[0].Target
	target.Backing = def.BackingKey()
	target.IntendedVolume = record.Steps[0].VolIDs[0]
	if _, err := managedVMVerifyCleanupVolume(t.Context(), deps, target, def, false); err == nil {
		t.Fatal("shared-to-local definition change accepted")
	}
	if client.stopCount != 0 || client.destroyCount != 0 {
		t.Fatal("definition rejection mutated guest")
	}
}

func TestLegacySweepProtectsJournalIdentityAfterMarkerRemoval(t *testing.T) {
	deps, _, client, _ := deleteManagedFixture(t)
	client.configs[123]["description"] = ""
	client.configs[123]["tags"] = tagDeletingVM
	sweepFastDeleteStragglers(t.Context(), deps, deps.Log(t.Context()))
	if client.stopCount != 0 || client.destroyCount != 0 {
		t.Fatal("legacy sweep bypassed retained journal authority")
	}
}

type mirroredOrphanClient struct {
	*deleteManagedClient
	removed      map[string]bool
	deletes      int
	missingReads int
}

func (c *mirroredOrphanClient) Nodes() ns.Service {
	return &mirroredOrphanNodes{Service: c.deleteManagedClient.Nodes(), c: c}
}
func (c *mirroredOrphanClient) Storage() storage.Service {
	return &mirroredOrphanStorage{c: c}
}

type mirroredOrphanNodes struct {
	ns.Service
	c *mirroredOrphanClient
}

func (n *mirroredOrphanNodes) ListNodes(context.Context) (*ns.ListNodesResponse, error) {
	r := ns.ListNodesResponse{json.RawMessage(`{"node":"pve1"}`), json.RawMessage(`{"node":"pve2"}`)}
	return &r, nil
}
func (n *mirroredOrphanNodes) GetStorageContent(_ context.Context, node, store, bare string) (*ns.GetStorageContentResponse, error) {
	// Simulate a stale node-specific read after another node removed shared data.
	if n.c.removed[store+":"+bare] && node == "pve1" {
		n.c.missingReads++
		return nil, errors.New("volume_size_info failed - no format")
	}
	return &ns.GetStorageContentResponse{Size: 1 << 30, Format: "qcow2"}, nil
}

type mirroredOrphanStorage struct {
	storage.Service
	c *mirroredOrphanClient
}

func (s *mirroredOrphanStorage) DeleteVolumeAsync(_ context.Context, _ string, _ string, volume string) (string, error) {
	s.c.deletes++
	s.c.removed[volume] = true
	var remaining ns.ListStorageContentResponse
	for _, raw := range s.c.nodesRead.content {
		var item struct {
			VolID string `json:"volid"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return "", err
		}
		if item.VolID != volume {
			remaining = append(remaining, raw)
		}
	}
	if remaining == nil {
		remaining = ns.ListStorageContentResponse{}
	}
	s.c.nodesRead.content = remaining
	return "UPID:pve1:shared-orphan-delete", nil
}
func TestManagedVMSharedOrphanIsDeletedOnceAcrossNodeSightings(t *testing.T) {
	deps, journal, base, record := deleteManagedFixture(t)
	delete(base.configs, 123)
	client := &mirroredOrphanClient{deleteManagedClient: base, removed: map[string]bool{}}
	deps.PVE = client
	result, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"pve1", "pve2"}, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "shared-orphan"})
	if err != nil {
		t.Fatalf("shared orphan cleanup: %v source=%v", err, errors.Unwrap(err))
	}
	if result.State != aj.Cleaned || client.deletes != 2 || client.missingReads != 0 {
		t.Fatalf("shared physical volumes duplicated: state=%s deletes=%d", result.State, client.deletes)
	}
}

func TestManagedVMDestroyRequiresUnchangedAgentProvenance(t *testing.T) {
	deps, _, client, record := deleteManagedFixture(t)
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: record.Namespace, Kind: "vm", AllocationID: record.ID, AgentSHA256: strings.Repeat("f", 64)})
	if err != nil {
		t.Fatal(err)
	}
	client.configs[123]["description"] = marker
	owned := map[string]bool{}
	for index := range record.Steps {
		for _, volume := range record.Steps[index].VolIDs {
			owned[volume] = true
		}
	}
	if err = verifyManagedVMDestroyDevices(t.Context(), deps, record, "pve1", 123, owned); err == nil {
		t.Fatal("changed agent provenance authorized destruction")
	}
}

func TestManagedVMCleanupMissingNFSVolumeUsesVisibleListing(t *testing.T) {
	deps, _, base, record := deleteManagedFixture(t)
	base.nodesRead.content = ns.ListStorageContentResponse{}
	volume := record.Steps[0].VolIDs[0]
	client := &mirroredOrphanClient{deleteManagedClient: base, removed: map[string]bool{volume: true}}
	deps.PVE = client
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		t.Fatal(err)
	}
	target := record.Steps[0].Target
	target.IntendedVolume = volume
	target.Storage = strings.SplitN(volume, ":", 2)[0]
	definition := plan.Definitions[target.Storage]
	target.Backing = definition.BackingKey()
	present, err := managedVMVerifyCleanupVolume(t.Context(), deps, target, definition, false)
	if err != nil || present || client.missingReads != 0 {
		t.Fatalf("missing NFS image queried as physical content: present=%t err=%v reads=%d", present, err, client.missingReads)
	}
}

func TestRetainedCleanupMissingNFSArtifactUsesVisibleListing(t *testing.T) {
	deps, _, base, record := deleteManagedFixture(t)
	delete(base.configs, 123)
	base.nodesRead.content = ns.ListStorageContentResponse{}
	target := record.Steps[0].Target
	target.IntendedVolume = record.Steps[0].VolIDs[0]
	client := &mirroredOrphanClient{deleteManagedClient: base, removed: map[string]bool{target.IntendedVolume: true}}
	deps.PVE = client
	_, payload, err := aj.VerificationEvidence(aj.VMRetentionEvidence{VMID: 123, RetainedArtifacts: []aj.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	record.Verifications = append(record.Verifications, aj.Verification{EvidenceJSON: payload, VMAbsenceVerified: true, ArtifactDispositionVerified: true})
	proof, err := retainedCleanupDecisionAdmission(t.Context(), deps, record, StorageAllocationAudit{}, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "missing-nfs"})
	if err != nil || !proof.AbsenceVerified || client.missingReads != 0 {
		t.Fatalf("retained missing NFS artifact not resolved: proof=%+v err=%v reads=%d", proof, err, client.missingReads)
	}
}

func (c *deleteManagedCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	return &rows, nil
}

type managedDeleteForeignAgent struct {
	agent.Agent
	removes int
}

func (a *managedDeleteForeignAgent) Remove(context.Context, string, int) error {
	a.removes++
	return errors.New("legacy agent points at an unrelated current ISO target")
}
func TestManagedVMDeleteDoesNotInvokeCurrentLegacyAgent(t *testing.T) {
	deps, j, client, record := deleteManagedFixture(t)
	foreign := &managedDeleteForeignAgent{}
	deps.Agent = foreign
	deps.Config.ISOStorage = "unrelated-current-iso-storage"
	for i := 0; i < 2; i++ {
		handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
		if err != nil || !handled {
			t.Fatalf("managed deletion failed: %v %v", handled, err)
		}
	}
	after, err := j.Inspect(record.ID)
	if err != nil || after.State != aj.Deleted || client.destroyCount != 1 {
		t.Fatalf("managed disposal was not durable: %v %s", err, after.State)
	}
	if foreign.removes != 0 {
		t.Fatal("managed cleanup invoked unfrozen legacy agent target")
	}
}
