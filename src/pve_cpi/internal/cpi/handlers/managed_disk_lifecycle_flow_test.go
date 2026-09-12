package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

type lifecycleFlowPVE struct {
	localStorage  bool
	volumeNodes   map[string]string
	foreignUnlink bool
	moveErr       error
	vmNodes       map[int]string
	migrations    int
	deletes       int
	visibilityErr error
	generation    int
	moves         int
	managedDiskTestPVE
	resizeCalls int
	dropResize  bool
	snapshots   []map[string]any
}

func (c *lifecycleFlowPVE) Nodes() nodes.Service {
	return lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c}
}
func (c *lifecycleFlowPVE) QEMU() qemu.Service {
	return lifecycleFlowQEMU{managedDiskTestQEMU: managedDiskTestQEMU{state: c.state}, c: c}
}

type lifecycleFlowNodes struct {
	managedDiskTestNodes
	c *lifecycleFlowPVE
}

func (n lifecycleFlowNodes) ListStorageContent(ctx context.Context, node, pool string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	listing, err := n.managedDiskTestNodes.ListStorageContent(ctx, node, pool, params)
	if err != nil || !n.c.localStorage {
		return listing, err
	}
	local := nodes.ListStorageContentResponse{}
	for _, raw := range *listing {
		var row struct {
			Volid string `json:"volid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		if n.c.volumeNodes[row.Volid] == node {
			local = append(local, raw)
		}
	}
	return &local, nil
}

func (n lifecycleFlowNodes) ListCertificatesInfo(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) {
	raw, _ := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("11:", 32), ":")})
	r := nodes.ListCertificatesInfoResponse{raw}
	return &r, nil
}

type lifecycleFlowQEMU struct {
	managedDiskTestQEMU
	c *lifecycleFlowPVE
}

func (q lifecycleFlowQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return q.c.snapshots, nil
}
func (q lifecycleFlowQEMU) ResizeDisk(_ context.Context, _ string, vmid int, slot string, delta int) (string, error) {
	q.c.resizeCalls++
	value := q.c.state.configs[vmid][slot].(string)
	old, err := parseDiskSizeGiB(value)
	if err != nil {
		return "", err
	}
	q.c.state.configs[vmid][slot] = strings.Replace(value, fmt.Sprintf("size=%dG", old), fmt.Sprintf("size=%dG", old+delta), 1)
	volume := strings.Split(value, ",")[0]
	q.c.state.volumes[volume].Size = sdk.PVEInt(old+delta) << 30
	if q.c.dropResize {
		return "", fmt.Errorf("connection lost after external side effect")
	}
	return "UPID:n1:resize", nil
}
func (q lifecycleFlowQEMU) Snapshot(_ context.Context, _ string, _ int, name string, _ map[string]any) (string, error) {
	q.c.snapshots = append(q.c.snapshots, map[string]any{"name": name})
	return "UPID:n1:snapshot", nil
}

func lifecycleFlowFixture(t *testing.T) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	return lifecycleFlowFixtureState(t, true)
}
func lifecycleFlowFixtureState(t *testing.T, returned bool, anchored ...bool) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := planFixture(t, func(_ *planFixtureSource, cfg *config.CPIConfig) {
		cfg.EphemeralStorageSet = ""
		cfg.PersistentStorageSet = "E"
	})
	selection, err := ResolveStoragePlacementSelectors(request.Selection.Policy, "create_disk", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	request.Selection = selection
	request.RootBytes = 0
	request.Sources = nil
	request.PersistentBytes = 5 << 30
	request.AllocationKey = id
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := storageJournalIntent("create_disk", []json.RawMessage{planJSON(t, 5120)}, selection, request.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	state := &managedDiskTestState{configs: map[int]map[string]any{}, volumes: map[string]*nodes.GetStorageContentResponse{}, pools: map[string]string{}}
	client := &lifecycleFlowPVE{managedDiskTestPVE: managedDiskTestPVE{state: state}}
	identity, err := pve.ObserveStorageClusterIdentity(context.Background(), client.Nodes(), []string{"n1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(context.Background(), dir, plan.Namespace, aj.Enrollment{ClusterID: identity.ID(), AuthorityID: "authority", AuditID: "audit", PreviousWriterFenced: true, CompleteHistoricalAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	handle, err := journal.CreateDisk(context.Background(), id, intent)
	if err != nil {
		t.Fatal(err)
	}
	name, err := pve.AllocationVolumeName(123, plan.Namespace, id, "raw")
	if err != nil {
		t.Fatal(err)
	}
	target := plan.Targets[0]
	volume := target.StorageID + ":123/" + name
	token := handle.Record().DiskToken
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw", Anchor: len(anchored) > 0 && anchored[0]})
	if err != nil {
		t.Fatal(err)
	}
	step, err := storageMutationIntent(handle, "create", aj.Target{Node: "n1", Storage: target.StorageID, Backing: target.BackingKey, VMID: 123, IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	if returned {
		record.State = aj.ReadyToReturn
		record.CID = cid
	}
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	state.configs[777] = map[string]any{"name": "workload", "digest": "1", "scsi1": volume + ",serial=" + token + ",size=5G"}
	state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	// All original storage sets and role bindings have been removed.
	deps := Deps{PVE: client, Config: &config.CPIConfig{Node: "n1", DiskStorage: target.StorageID, StoragePlacementNamespace: plan.Namespace, StorageAllocationJournalDir: dir}}
	return deps, client, journal, id, cid
}
func TestManagedDiskResizeAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	args := []json.RawMessage{planJSON(t, cid), planJSON(t, 6144)}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.resizeCalls != 1 || record.State != aj.ReadyToReturn || len(record.Steps) != 2 {
		t.Fatalf("incomplete lifecycle: calls=%d record=%+v", client.resizeCalls, record)
	}
	step := record.Steps[1]
	if step.UPID != "UPID:n1:resize" || len(step.Charges) != 1 || step.Charges[0].AcquiredBytes != 1<<30 {
		t.Fatalf("resize evidence lost: %+v", step)
	}
}
func TestManagedDiskDroppedResizeCannotReplay(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	client.dropResize = true
	args := []json.RawMessage{planJSON(t, cid), planJSON(t, 6144)}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("lost response accepted")
	}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("unknown old resize replay accepted")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.resizeCalls != 1 || record.State != aj.ReconciliationRequired || record.Steps[1].State != aj.Planned {
		t.Fatalf("unknown evidence replayed: calls=%d record=%+v", client.resizeCalls, record)
	}
}
func TestManagedDiskOwnershipRequiresActualVolume(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	for volume := range client.state.volumes {
		delete(client.state.volumes, volume)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "has_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDiskForOp(context.Background(), deps, "has_disk", cid, bare, meta); err == nil {
		t.Fatal("dangling VM entry claimed live disk ownership")
	}
	if client.resizeCalls != 0 {
		t.Fatal("read-only lookup submitted mutation")
	}
}

func (n lifecycleFlowNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return err
	}
	cfg := n.c.state.configs[vmid]
	if p.Digest != nil && *p.Digest != cfg["digest"] {
		return fmt.Errorf("config generation conflict")
	}
	deletedVolume := ""
	if p.Delete != nil && strings.HasPrefix(*p.Delete, "unused") {
		value, _ := pve.ConfigString(cfg, *p.Delete)
		deletedVolume = strings.Split(value, ",")[0]
	}
	fake := newIDFakeClient(n.c.state.configs)
	if err := (&idFakeNodes{c: fake}).UpdateQemuConfig(ctx, node, vmidText, p); err != nil {
		return err
	}
	fields, err := lifecycleMutationFields(p)
	if err != nil {
		return err
	}
	for key, value := range fields {
		if isDiskOptionKey(key) {
			cfg[key] = value
		}
	}
	if owner, ok := pve.EmbeddedDiskVMID(deletedVolume); ok && owner == vmid {
		delete(n.c.state.volumes, deletedVolume)
	}
	if n.c.foreignUnlink && p.Delete != nil && !strings.HasPrefix(*p.Delete, "unused") {
		for slot := range pve.FindUnusedDiskEntries(cfg) {
			delete(cfg, slot)
		}
	}
	n.c.generation++
	cfg["digest"] = fmt.Sprint(n.c.generation + 100)
	return nil
}
func (n lifecycleFlowNodes) CreateQemuMoveDisk(_ context.Context, _ string, sourceText string, p *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	if n.c.moveErr != nil {
		return nil, n.c.moveErr
	}
	sourceID, _ := strconv.Atoi(sourceText)
	source := n.c.state.configs[sourceID]
	targetID := int(*p.TargetVmid)
	target := n.c.state.configs[targetID]
	if p.Digest != nil && *p.Digest != source["digest"] || p.TargetDigest != nil && *p.TargetDigest != target["digest"] {
		return nil, fmt.Errorf("move generation conflict")
	}
	value, _ := pve.ConfigString(source, p.Disk)
	old := strings.Split(value, ",")[0]
	storage, _, err := pve.ParseDiskCID(old)
	if err != nil {
		return nil, err
	}
	info := n.c.state.volumes[old]
	if info == nil {
		return nil, fmt.Errorf("source volume missing")
	}
	n.c.moves++
	landed := fmt.Sprintf("%s:%d/vm-%d-disk-%d.raw", storage, targetID, targetID, n.c.moves)
	opts := strings.TrimPrefix(value, old)
	if strings.HasPrefix(p.Disk, "unused") {
		opts = ""
	}
	delete(source, p.Disk)
	target[*p.TargetDisk] = landed + opts
	delete(n.c.state.volumes, old)
	n.c.state.volumes[landed] = info
	if n.c.volumeNodes != nil {
		n.c.volumeNodes[landed] = n.c.volumeNodes[old]
		delete(n.c.volumeNodes, old)
	}
	n.c.generation++
	source["digest"] = fmt.Sprint(n.c.generation + 100)
	n.c.generation++
	target["digest"] = fmt.Sprint(n.c.generation + 100)
	raw := json.RawMessage(`"UPID:n1:move"`)
	return &raw, nil
}
func (q lifecycleFlowQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	upid, err := q.managedDiskTestQEMU.Create(ctx, node, params)
	if err == nil {
		id := params["vmid"].(int)
		q.c.state.configs[id]["digest"] = "1"
		if q.c.vmNodes == nil {
			q.c.vmNodes = map[int]string{}
		}
		q.c.vmNodes[id] = node
	}
	return upid, err
}
func TestManagedDiskSnapshotAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	if _, err := HandleSnapshotDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.snapshots) != 1 || record.State != aj.ReadyToReturn || record.Steps[1].UPID != "UPID:n1:snapshot" {
		t.Fatalf("snapshot lifecycle incomplete: %+v", record)
	}
}
func TestManagedDiskAttachAndDetachAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(client.state.configs[777]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("receiving provenance absent: %v %v", entries, err)
	}
	deps.Config.DetachedDiskStrategy = "parked"
	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || client.moves != 1 {
		t.Fatalf("detach lifecycle incomplete: moves=%d record=%+v", client.moves, record)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.volid == bare || resolved.allocation == nil || resolved.allocation.record.ID != id {
		t.Fatal("rename lost full allocation identity")
	}
}

func (c *lifecycleFlowPVE) StorageAuditVisibility(context.Context) error { return c.visibilityErr }
func (c *lifecycleFlowPVE) Storage() storage.Service {
	return lifecycleFlowStorage{managedDiskTestStorage: managedDiskTestStorage{state: c.state}, c: c}
}

type lifecycleFlowStorage struct {
	managedDiskTestStorage
	c *lifecycleFlowPVE
}

func (s lifecycleFlowStorage) DeleteVolumeAsync(_ context.Context, node, pool, volume string) (string, error) {
	s.c.deletes++
	delete(s.c.state.volumes, volume)
	return fmt.Sprintf("UPID:%s:000573BD:03504636:6AA1786A:imgdel:123@%s:pmx@pve!pmx:", node, pool), nil
}
func TestManagedDiskDeleteRequiresCompleteAuditAndPreservesTombstone(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	client.visibilityErr = errors.New("restricted visibility")
	args := []json.RawMessage{planJSON(t, cid)}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("partial audit certified deletion")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired || client.deletes != 1 {
		t.Fatalf("partial deletion state: %+v deletes=%d", record, client.deletes)
	}
	client.visibilityErr = nil
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("ordinary retry accepted unresolved deletion")
	}
	deps.PVE = &cleanupTaskClient{Client: deps.PVE}
	if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(id)); err != nil {
		t.Fatalf("explicit cleanup of successfully deleted disk: %v", err)
	}
	record, err = journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	wantUPID := fmt.Sprintf("UPID:n1:000573BD:03504636:6AA1786A:imgdel:123@%s:pmx@pve!pmx:", record.Steps[0].Target.Storage)
	if record.State != aj.Cleaned || client.deletes != 1 || record.Steps[1].UPID != wantUPID || record.Steps[1].State != aj.Submitted {
		t.Fatalf("verified deletion lost evidence: %+v deletes=%d", record, client.deletes)
	}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("idempotent tombstone delete: %v", err)
	}
	bare, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[bare] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil || client.deletes != 1 {
		t.Fatal("terminal journal authorized deletion of new live resource")
	}
}
func TestManagedDiskBareCIDRecoversJournalToken(t *testing.T) {
	deps, _, journal, id, cid := lifecycleFlowFixture(t)
	bare, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "has_disk", bare, bare, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if rd.stableID != record.DiskToken || rd.holder == nil || rd.holder.VMID != 777 {
		t.Fatalf("bare CID lost stable ownership: %+v", rd)
	}
}

func (n lifecycleFlowNodes) GetStorageContent(ctx context.Context, node, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	if _, exists := n.c.state.volumes[pool+":"+volume]; !exists || n.c.localStorage && n.c.volumeNodes[pool+":"+volume] != node {
		return nil, sdkerrors.ParseAPIError(404, []byte(`{"message":"volume not found"}`))
	}
	return n.managedDiskTestNodes.GetStorageContent(ctx, node, pool, volume)
}

func (c *lifecycleFlowPVE) vmNode(vmid int) string {
	if node := c.vmNodes[vmid]; node != "" {
		return node
	}
	return "n1"
}
func (q lifecycleFlowQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if _, exists := q.c.state.configs[vmid]; !exists || q.c.vmNode(vmid) != node {
		return nil, sdkerrors.ParseAPIError(404, []byte(`{"message":"VM not found"}`))
	}
	return q.managedDiskTestQEMU.Config(ctx, node, vmid)
}
func (n lifecycleFlowNodes) ListQemu(_ context.Context, node string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	rows := nodes.ListQemuResponse{}
	for id, cfg := range n.c.state.configs {
		if n.c.vmNode(id) != node {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"vmid": id, "tags": cfg["tags"], "name": cfg["name"]})
		rows = append(rows, raw)
	}
	return &rows, nil
}
func (n lifecycleFlowNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	rows := nodes.ListNodesResponse{json.RawMessage(`{"node":"n1","status":"online"}`), json.RawMessage(`{"node":"n2","status":"online"}`)}
	return &rows, nil
}
func (c *lifecycleFlowPVE) Cluster() cluster.Service {
	return lifecycleFlowCluster{managedDiskTestCluster: managedDiskTestCluster{state: c.state}, c: c}
}

type lifecycleFlowCluster struct {
	managedDiskTestCluster
	c *lifecycleFlowPVE
}

func (c lifecycleFlowCluster) ListConfigNodes(context.Context) (*cluster.ListConfigNodesResponse, error) {
	r := cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"n1"}`), json.RawMessage(`{"name":"n2"}`)}
	return &r, nil
}
func (c lifecycleFlowCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := cluster.ListStatusResponse{json.RawMessage(`{"type":"cluster","quorate":1}`), json.RawMessage(`{"type":"node","name":"n1","online":1}`), json.RawMessage(`{"type":"node","name":"n2","online":1}`)}
	return &r, nil
}
func (c lifecycleFlowCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	r := make(cluster.ListResourcesResponse, 0, len(c.c.state.configs))
	for id, cfg := range c.c.state.configs {
		raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": id, "node": c.c.vmNode(id), "tags": cfg["tags"]})
		r = append(r, raw)
	}
	return &r, nil
}
func (n lifecycleFlowNodes) CreateQemuMigrate(_ context.Context, node, vmidText string, p *nodes.CreateQemuMigrateParams) (*nodes.CreateQemuMigrateResponse, error) {
	id, _ := strconv.Atoi(vmidText)
	if n.c.vmNode(id) != node {
		return nil, fmt.Errorf("migration source node changed")
	}
	if n.c.vmNodes == nil {
		n.c.vmNodes = map[int]string{}
	}
	n.c.vmNodes[id] = p.Target
	if n.c.localStorage {
		for _, value := range qemu.ParseDisks(n.c.state.configs[id]) {
			n.c.volumeNodes[strings.Split(value, ",")[0]] = p.Target
		}
	}
	n.c.migrations++
	raw := json.RawMessage(`"UPID:n1:migrate"`)
	return &raw, nil
}
func (n lifecycleFlowNodes) DeleteQemu(_ context.Context, node, vmidText string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	id, _ := strconv.Atoi(vmidText)
	if n.c.vmNode(id) != node {
		return nil, fmt.Errorf("delete node changed")
	}
	if lifecycleConfigHasAnyVolume(n.c.state.configs[id]) {
		return nil, fmt.Errorf("mover not empty")
	}
	delete(n.c.state.configs, id)
	delete(n.c.vmNodes, id)
	raw := json.RawMessage(`"UPID:n2:destroy"`)
	return &raw, nil
}
func TestManagedDiskSharedMigrationAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskMigration = "on_attach"
	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.migrations != 1 || record.State != aj.ReadyToReturn {
		t.Fatalf("migration incomplete: count=%d record=%+v", client.migrations, record)
	}
	for _, step := range record.Steps {
		if strings.Contains(step.Kind, "CreateQemuMigrate") && len(step.Charges) != 0 {
			t.Fatal("shared metadata migration charged destination bytes")
		}
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.holder == nil || rd.holder.Node != "n2" || rd.holder.VMID != 888 || rd.allocation.record.ID != id {
		t.Fatalf("migration lost identity: %+v", rd)
	}
}

func (q lifecycleFlowQEMU) Status(context.Context, string, int) (map[string]any, error) {
	return map[string]any{"status": "stopped"}, nil
}
func (c lifecycleFlowCluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	return nil, &sdkerrors.APIError{Code: 404, Message: "not found"}
}

func (c lifecycleFlowCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	r := cluster.ListHaRulesResponse{}
	return &r, nil
}

func (c *lifecycleFlowPVE) ClusterStorage() clusterstorage.Service {
	return lifecycleFlowDefinitions{c: c}
}

type lifecycleFlowDefinitions struct {
	managedDiskTestDefinitions
	c *lifecycleFlowPVE
}

func (d lifecycleFlowDefinitions) ListStorage(ctx context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if !d.c.localStorage {
		return d.managedDiskTestDefinitions.ListStorage(ctx, params)
	}
	r := clusterstorage.ListStorageResponse{}
	for _, id := range []string{"a", "b"} {
		raw, err := json.Marshal(map[string]any{"storage": id, "type": "dir", "path": "/mnt/" + id, "shared": 0, "content": "images", "nodes": "n1,n2"})
		if err != nil {
			return nil, err
		}
		r = append(r, raw)
	}
	return &r, nil
}
func (s lifecycleFlowStorage) Exists(ctx context.Context, node, pool, volume string) (bool, error) {
	if s.c.localStorage {
		return s.c.state.volumes[volume] != nil && s.c.volumeNodes[volume] == node, nil
	}
	return s.managedDiskTestStorage.Exists(ctx, node, pool, volume)
}

func TestManagedDiskLocalMigrationChargesDestinationAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	birth, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	pool, _, err := pve.ParseDiskCID(birth)
	if err != nil {
		t.Fatal(err)
	}
	// Model a separately audited external relocation onto a local backing.
	// Its evidence is appended; the original NFS policy and birth stay immutable.
	client.localStorage = true
	client.volumeNodes = map[string]string{birth: "n1"}
	definition, err := managedDiskActualDefinition(context.Background(), deps, pool)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	session, err := beginStorageLifecycle(handle, "audited_external_relocation", lifecycleProof(t, "external-relocation-start", false))
	if err != nil {
		t.Fatal(err)
	}
	step, err := session.Intent("relocation", aj.Target{Node: "n1", VMID: 777, Storage: pool, Backing: definition.BackingKey(), IntendedVolume: birth})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Observed(step, []string{birth}); err != nil {
		t.Fatal(err)
	}
	if err := session.Finish(lifecycleProof(t, "external-relocation-complete", false), false); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	deps.Resolver = pve.NewBackendResolver(client, nil, "n1")
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskMigration = "on_attach"
	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if strings.Contains(step.Kind, "CreateQemuMigrate") {
			found = true
			if step.Target.Node != "n2" || len(step.Charges) != 1 || step.Charges[0].AcquiredBytes != 5<<30 || step.Charges[0].OutstandingBytes != 0 {
				t.Fatalf("local copy lost destination accounting: %+v", step)
			}
		}
	}
	if !found || client.migrations != 1 || record.State != aj.ReadyToReturn {
		t.Fatalf("local migration incomplete: %+v", record)
	}
}

func (c lifecycleFlowCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	return &rows, nil
}
