package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

type managedVMGuardFixture struct {
	pve.Client
	cfg              map[string]any
	size             uint64
	definitions      map[string]pve.StorageInfo
	storage          string
	creates, resizes int
	unknown          bool
	running          bool
	failStatus       bool
	uploads          int
	volumes          map[string]uint64
	allocations      int
	visibilityErr    error
	visibilityChecks int
	taskWaits        int
}
type managedVMGuardQEMU struct {
	qemu.Service
	f *managedVMGuardFixture
}
type managedVMGuardStorage struct {
	storage.Service
	f *managedVMGuardFixture
}

type managedVMGuardNodes struct {
	nodes.Service
	f *managedVMGuardFixture
}

func (f *managedVMGuardFixture) StorageAuditVisibility(context.Context) error {
	f.visibilityChecks++
	return f.visibilityErr
}
func (n *managedVMGuardNodes) ListStorageContent(_ context.Context, node, pool string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if node != "n1" {
		return nil, fmt.Errorf("unknown fixture node")
	}
	volumes := make(map[string]uint64, len(n.f.volumes)+1)
	for volume, size := range n.f.volumes {
		volumes[volume] = size
	}
	if n.f.size > 0 {
		volumes[n.f.storage+":101/vm-101-disk-0.qcow2"] = n.f.size
	}
	result := nodes.ListStorageContentResponse{}
	for volume, size := range volumes {
		if !strings.HasPrefix(volume, pool+":") {
			continue
		}
		content := "images"
		if strings.HasPrefix(volume, pool+":iso/") {
			content = "iso"
		}
		if params != nil && params.Content != nil && *params.Content != content {
			continue
		}
		raw, err := json.Marshal(map[string]any{"volid": volume, "size": size, "content": content, "format": content, "ctime": 1})
		if err != nil {
			return nil, err
		}
		result = append(result, raw)
	}
	return &result, nil
}

func (f *managedVMGuardFixture) Storage() storage.Service { return &managedVMGuardStorage{f: f} }
func (s *managedVMGuardStorage) CreateVolume(_ context.Context, _ string, pool string, size int, _ string, _ int, name string) (string, error) {
	s.f.allocations++
	if !strings.HasSuffix(name, ".qcow2") {
		return "", fmt.Errorf("file plugin requires matching format extension")
	}
	id := pool + ":101/" + name
	if _, exists := s.f.volumes[id]; exists {
		return "", fmt.Errorf("volume already exists")
	}
	if s.f.volumes == nil {
		s.f.volumes = map[string]uint64{}
	}
	s.f.volumes[id] = uint64(size) << 30
	return id, nil
}
func (q *managedVMGuardQEMU) AttachDisk(_ context.Context, _ string, _ int, volume, bus string, opts *qemu.AttachOpts) (string, error) {
	q.f.cfg[opts.DiskID] = volume
	return opts.DiskID, nil
}

func (s *managedVMGuardStorage) Exists(_ context.Context, _ string, _ string, volume string) (bool, error) {
	_, ok := s.f.volumes[volume]
	return ok, nil
}
func (s *managedVMGuardStorage) Upload(_ context.Context, _ string, pool, content, name string, body io.Reader) (string, error) {
	s.f.uploads++
	size, err := io.Copy(io.Discard, body)
	if err != nil {
		return "", err
	}
	if s.f.volumes == nil {
		s.f.volumes = map[string]uint64{}
	}
	s.f.volumes[pool+":"+content+"/"+name] = uint64(size)
	return "UPID:n1:upload", nil
}

type managedVMGuardCluster struct{ cluster.Service }

func (f *managedVMGuardFixture) Cluster() cluster.Service { return &managedVMGuardCluster{} }
func (c *managedVMGuardCluster) ListSdnVnets(context.Context, *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	r := cluster.ListSdnVnetsResponse{}
	return &r, nil
}

func (f *managedVMGuardFixture) QEMU() qemu.Service   { return &managedVMGuardQEMU{f: f} }
func (f *managedVMGuardFixture) Nodes() nodes.Service { return &managedVMGuardNodes{f: f} }

type managedVMGuardTasks struct {
	tasks.Service
	f *managedVMGuardFixture
}

func (s *managedVMGuardTasks) Wait(context.Context, string, string, *tasks.WaitOptions) (*tasks.Status, error) {
	s.f.taskWaits++
	return &tasks.Status{ExitStatus: "OK"}, nil
}

func (f *managedVMGuardFixture) Tasks() tasks.Service { return &managedVMGuardTasks{f: f} }
func (q *managedVMGuardQEMU) Create(_ context.Context, _ string, params map[string]any) (string, error) {
	q.f.creates++
	if q.f.unknown {
		return "", fmt.Errorf("connection lost")
	}
	q.f.cfg = map[string]any{"description": params["description"], "virtio0": q.f.storage + ":101/vm-101-disk-0.qcow2,size=5G"}
	q.f.size = 5 << 30
	return "UPID:n1:create", nil
}

func (q *managedVMGuardQEMU) Status(context.Context, string, int) (map[string]any, error) {
	if q.f.failStatus {
		q.f.failStatus = false
		return nil, fmt.Errorf("status temporarily unreadable")
	}
	state := "stopped"
	if q.f.running {
		state = "running"
	}
	return map[string]any{"status": state}, nil
}
func (q *managedVMGuardQEMU) Start(context.Context, string, int) (string, error) {
	q.f.running = true
	return "UPID:n1:start", nil
}
func (q *managedVMGuardQEMU) Config(context.Context, string, int) (map[string]any, error) {
	return q.f.cfg, nil
}

func (n *managedVMGuardNodes) CreateQemuClone(_ context.Context, _ string, _ string, params *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
	n.f.creates++
	n.f.size = 5 << 30
	n.f.cfg = map[string]any{"description": "inherited template metadata", "virtio0": n.f.storage + ":101/vm-101-disk-0.qcow2,size=5G", "efidisk0": n.f.storage + ":101/vm-101-disk-1.raw,size=1M"}
	if n.f.volumes == nil {
		n.f.volumes = map[string]uint64{}
	}
	n.f.volumes[n.f.storage+":101/vm-101-disk-1.raw"] = 1 << 20
	raw := json.RawMessage(`"UPID:n1:clone"`)
	return &raw, nil
}
func (n *managedVMGuardNodes) UpdateQemuConfig(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
	fields, err := managedEvidenceObject(params)
	if err != nil {
		return err
	}
	for key, value := range fields {
		if key == "delete" {
			for _, field := range strings.Split(value.(string), ",") {
				delete(n.f.cfg, field)
			}
		} else if key != "digest" {
			n.f.cfg[key] = value
			if managedVMVolumeDevice(key) {
				if drive, ok := value.(string); ok && !strings.Contains(drive, "size=") {
					n.f.cfg[key] = fmt.Sprintf("%s,size=%d", drive, n.f.size)
				}
			}
		}
	}
	return nil
}
func (n *managedVMGuardNodes) GetStorageContent(_ context.Context, _ string, pool string, volume string) (*nodes.GetStorageContentResponse, error) {
	format := "qcow2"
	size, ok := n.f.volumes[pool+":"+volume]
	if !ok {
		if pool != n.f.storage || volume != "101/vm-101-disk-0.qcow2" {
			return nil, fmt.Errorf("unknown fixture volume")
		}
		size = n.f.size
	}
	if strings.HasSuffix(volume, ".raw") {
		format = "raw"
	}
	if strings.HasPrefix(volume, "iso/") {
		format = "raw"
	}
	return &nodes.GetStorageContentResponse{Size: sdkclient.PVEInt(size), Format: format}, nil
}
func (n *managedVMGuardNodes) UpdateQemuResize(_ context.Context, _ string, _ string, params *nodes.UpdateQemuResizeParams) (*nodes.UpdateQemuResizeResponse, error) {
	n.f.resizes++
	if params.Size != "8G" {
		return nil, fmt.Errorf("not an absolute planned size")
	}
	n.f.size = 8 << 30
	n.f.cfg["virtio0"] = n.f.storage + ":101/vm-101-disk-0.qcow2,size=8G"
	raw := json.RawMessage(`"UPID:n1:resize"`)
	return &raw, nil
}
func TestManagedVMGuardRootGrowthConservationAndUnknown(t *testing.T) {
	for _, tc := range []managedVMGuardCase{{}, {unknown: true}, {ephemeral: true}, {iso: true}, {ephemeral: true, iso: true}, {ephemeral: true, iso: true, resume: true}, {ephemeral: true, iso: true, keep: true}, {clone: true}, {clone: true, ephemeral: true, iso: true, resume: true}, {routes: true}, {routes: true, routeUnknown: true}} {
		unknown := tc.unknown
		t.Run(fmt.Sprintf("unknown=%t,ephemeral=%t,iso=%t,resume=%t,keep=%t,clone=%t,routes=%t,routeUnknown=%t", unknown, tc.ephemeral, tc.iso, tc.resume, tc.keep, tc.clone, tc.routes, tc.routeUnknown), func(t *testing.T) {
			runManagedVMGuardCase(t, tc)
		})
	}
}

type managedVMGuardCase struct{ unknown, ephemeral, iso, resume, keep, clone, routes, routeUnknown bool }

func newManagedVMGuardCase(t *testing.T, tc managedVMGuardCase) (*managedVMAllocation, *managedVMGuardFixture, *managedVMRouteCluster, *aj.Journal) {
	t.Helper()
	req, collector, snapshot := managedVMGuardRequest(t, tc)
	iterator, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ledger := inv.NewLedger()
	for i := range plan.Charges {
		record := &plan.Charges[i]
		ledger, err = ledger.WithPlanned(snapshot, record.Charge)
		if err != nil {
			t.Fatal(err)
		}
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(context.Background(), directory, plan.Namespace, aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	intent, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{}`)}, req.Selection, snapshot, plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(context.Background(), "agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := &managedVMGuardFixture{storage: plan.Targets[0].StorageID, unknown: tc.unknown, definitions: plan.Definitions}
	deps := Deps{PVE: fixture, Config: req.Selection.Policy, Logger: log.NewNopLogger()}
	routeService := &managedVMRouteCluster{unknown: tc.routeUnknown}
	if tc.routes {
		deps.PVE = &managedVMRouteClient{Client: fixture, service: routeService}
	}
	parsed := &createVMParsedArgs{agentID: "agent"}
	if tc.routes {
		parsed.cloudProps.AdvertisedRoutes = []AdvertisedRoute{{VNet: "router", Destination: "10.60.0.0/16"}}
	}
	if tc.iso {
		parsed.networks = map[string]createVMNetworkSpec{"default": {Type: "dynamic"}}
	}
	shape := &createVMShape{node: plan.Node, vmStorage: plan.Targets[0].StorageID, vmDiskFormat: "qcow2", rootDiskKey: "virtio0", rootDiskGiB: 8, cores: 2, sockets: 1, memMiB: 1024}
	if tc.ephemeral {
		target, _ := managedVMRoleTarget(plan, storageRoleEphemeral)
		shape.ephemeralDiskGiB = 2
		shape.ephemeralStorage = target.StorageID
	}
	m, err := newManagedVMAllocation(deps, parsed, shape, &managedVMPlan{selection: req.Selection, collector: collector, inventory: snapshot, plan: plan, ledger: ledger}, handle)
	if err != nil {
		t.Fatal(err)
	}
	m.vmid = 101
	parsed.storageRuntime = m
	if err := m.newGuard(); err != nil {
		t.Fatal(err)
	}
	return m, fixture, routeService, journal
}
func resumeManagedVMGuardCase(t *testing.T, m *managedVMAllocation, fixture *managedVMGuardFixture, previousErr error) (*managedVMAllocation, any, error) {
	t.Helper()
	deps, parsed, shape, handle := m.deps, m.parsed, m.shape, m.handle
	if previousErr == nil || handle.Record().State != aj.ReconciliationRequired {
		t.Fatal("interrupted prefix did not require reconciliation")
	}
	restored, restoreErr := newManagedVMAllocation(deps, parsed, shape, m.prepared, handle)
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	restored.vmid = 101
	restored.rootCreated = true
	observation := &managedVMObservation{Node: shape.node, VMID: 101, Config: fixture.cfg}
	evidenceID, evidenceJSON, proofErr := aj.VerificationEvidence(map[string]any{"scope": "exact fixture VM marker and allocation volume readback", "vmid": 101, "config": fixture.cfg})
	if proofErr != nil {
		t.Fatal(proofErr)
	}
	observation.Verification = aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, OwnershipVerified: true}
	if proofErr = saveManagedVMOwnership(handle, observation); proofErr != nil {
		t.Fatal(proofErr)
	}

	if restoreErr = restored.restoreAcquiredCharges(context.Background(), observation); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	m = restored
	if guardErr := m.newGuard(); guardErr != nil {
		t.Fatal(guardErr)
	}
	result, err := m.execute(context.Background(), observation)
	if fixture.creates != 1 || fixture.resizes != 1 || fixture.allocations != 1 || fixture.uploads != 1 {
		t.Fatalf("resume repeated allocations: create=%d resize=%d E=%d ISO=%d", fixture.creates, fixture.resizes, fixture.allocations, fixture.uploads)
	}
	return m, result, err
}

func runManagedVMGuardCase(t *testing.T, tc managedVMGuardCase) {
	t.Helper()
	unknown := tc.unknown
	m, fixture, routeService, journal := newManagedVMGuardCase(t, tc)
	deps, parsed, shape, plan, handle := m.deps, m.parsed, m.shape, m.prepared.plan, m.handle
	guarded := deps
	guarded.PVE = m.guard.Client()
	var err error
	err = createManagedVMRoot(context.Background(), guarded, parsed, shape, plan.Targets[0], 101, m.marker)
	if unknown {
		if err == nil || handle.Record().State != aj.ReconciliationRequired {
			t.Fatal("unknown write not preserved")
		}
		if _, err = m.guard.Client().QEMU().Create(context.Background(), shape.node, nil); err == nil {
			t.Fatal("retry accepted")
		}
		if fixture.creates != 1 || fixture.resizes != 0 {
			t.Fatal("unknown caused second mutation")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if fixture.taskWaits != 1 {
		t.Fatalf("root creation task observed %d times", fixture.taskWaits)
	}
	assertManagedVMRootGrowth(t, m, fixture, guarded)

	fixture.failStatus = tc.resume || tc.keep
	result, err := m.execute(context.Background(), nil)
	if tc.routeUnknown {
		if err == nil || handle.Record().State != aj.ReconciliationRequired || routeService.creates != 1 || routeService.applies != 1 || routeService.deletes != 0 {
			t.Fatalf("unknown route outcome lost evidence: %v creates=%d applies=%d deletes=%d", err, routeService.creates, routeService.applies, routeService.deletes)
		}
		return
	}

	if tc.keep {
		assertManagedVMRetained(t, m, fixture, journal, err)
		return
	}
	if tc.resume {
		m, result, err = resumeManagedVMGuardCase(t, m, fixture, err)
	}

	if err != nil {
		t.Fatalf("%v guard=%v", err, m.guard.Err())
	}
	values, ok := result.([]any)
	if !ok || values[0] != "101" || handle.Record().State != aj.ReadyToReturn {
		t.Fatalf("VM not ready: result=%v state=%s", result, handle.Record().State)
	}
	recorded := handle.Record()
	for i := range recorded.Steps {
		step := &recorded.Steps[i]
		if step.State != aj.Observed {
			t.Fatalf("unobserved step %+v", step)
		}
	}
}

func assertManagedVMRootGrowth(t *testing.T, m *managedVMAllocation, fixture *managedVMGuardFixture, guarded Deps) {
	t.Helper()
	deps, parsed, shape := m.deps, m.parsed, m.shape
	records := m.prepared.ledger.Records()
	base, growth := false, false
	for i := range records {
		record := &records[i]
		switch record.Charge.ID {
		case "root_base":
			base = record.Acquired
		case "root_growth":
			growth = record.Acquired
		}
	}
	if !base || growth {
		t.Fatalf("base/growth allocation not conserved: %+v", records)
	}

	if err := resizeCreatedVMRoot(context.Background(), guarded, deps.Logger, parsed, shape, 101); err != nil {
		t.Fatal(err)
	}
	ledgerRecords := m.prepared.ledger.Records()
	for i := range ledgerRecords {
		record := &ledgerRecords[i]
		if record.Charge.Role == storageRoleRoot && !record.Acquired {
			t.Fatal("proven growth remains unacquired")
		}
	}
	if fixture.creates != 1 || fixture.resizes != 1 {
		t.Fatalf("creates=%d resize=%d", fixture.creates, fixture.resizes)
	}

}

func assertManagedVMRetained(t *testing.T, m *managedVMAllocation, fixture *managedVMGuardFixture, journal *aj.Journal, err error) {
	t.Helper()
	deps, parsed, plan, handle := m.deps, m.parsed, m.prepared.plan, m.handle
	if err == nil {
		t.Fatal("retained failure unexpectedly succeeded")
	}
	retained := false
	recorded := handle.Record()
	for i := range recorded.Steps {
		step := &recorded.Steps[i]
		retained = retained || step.Kind == "vm.keep_failed"
	}
	if !retained || handle.Record().State != aj.ReconciliationRequired {
		t.Fatalf("retention disposition not durable: %v", err)
	}
	_, resumeErr := continueManagedVM(context.Background(), deps, parsed, m.prepared.selection, journal, handle, plan, nil)
	if resumeErr == nil || !strings.Contains(resumeErr.Error(), "retains a failed attempt") {
		t.Fatalf("retained attempt resumed: %v", resumeErr)
	}
	if fixture.creates != 1 || fixture.allocations != 1 || fixture.uploads != 1 {
		t.Fatal("retention attempted replacement")
	}
}

func managedVMGuardRequest(t *testing.T, tc managedVMGuardCase) (StoragePlanRequest, *inv.Collector, *inv.Snapshot) {
	t.Helper()
	req, _, source := planFixture(t, func(_ *planFixtureSource, cfg *config.CPIConfig) { cfg.AgentMode = config.AgentModeNoAgent })
	collector, err := inv.NewCollector(source, inv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collector.Discover(context.Background(), req.Selection.Policy, inv.Request{Nodes: []string{"n1", "n2"}, CompanionStorageIDs: []string{"source"}})
	if err != nil {
		t.Fatal(err)
	}
	req.Inventory = snapshot
	req.Clock = time.Now
	req.RootBytes = 8 << 30
	if tc.clone {
		req.CloneMode = "full"
		req.Sources[0].TemplateVMID = 9000
		req.Sources[0].VolumeID = "source:9000/base-9000-disk-0.qcow2"
		req.Sources[0].AuxiliaryBytes = 1 << 20
		req.Sources[0].AuxiliaryVolumes = []StorageExistingVolume{{StorageID: "source", Node: "n1", VolumeID: "source:9000/base-9000-disk-1.raw", Device: "efidisk0", VirtualBytes: 1 << 20}}
	}

	if tc.keep {
		value := true
		req.Selection.Policy.Debug = &config.DebugConfig{KeepFailedVMs: &value}
	}
	if tc.iso {
		req.Selection.Policy.AgentMode = config.AgentModeAuto
		req.Selection.Policy.ISOStorage = "source"
		req.Selection.Policy.AgentMBus = "nats://agent:password@bus:4222"
		req.Selection.Policy.AgentBlobstore = map[string]any{"provider": "local", "options": map[string]any{"blobstore_path": "/var/vcap/data/blobs"}}
		req.ISOBytes = configdrive.AllocationBytes()
		req.OriginalISOStorage = "source"
	}
	if tc.ephemeral {
		req.Selection, err = ResolveStoragePlacementSelectors(req.Selection.Policy, "create_vm", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		req.EphemeralBytes = 2 << 30
	}
	return req, collector, snapshot
}

func TestManagedVMGuardISOUnknownVisibilityPreventsUpload(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{iso: true})
	fixture.visibilityErr = fmt.Errorf("fixture storage visibility restricted")
	if _, err := m.execute(t.Context(), nil); err == nil {
		t.Fatal("managed ISO accepted unknown collision visibility")
	}
	if fixture.uploads != 0 || fixture.visibilityChecks == 0 {
		t.Fatal("managed ISO upload did not exercise negative membership visibility")
	}
}

func (f *managedVMGuardFixture) ClusterStorage() clusterstorage.Service {
	return &managedVMGuardDefinitions{f: f}
}

type managedVMGuardDefinitions struct {
	clusterstorage.Service
	f *managedVMGuardFixture
}

func (d *managedVMGuardDefinitions) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	rows := make(clusterstorage.ListStorageResponse, 0, len(d.f.definitions))
	for index := range d.f.definitions {
		def := d.f.definitions[index]
		raw, _ := json.Marshal(map[string]any{"storage": def.Name, "type": def.Type, "shared": def.Shared, "nodes": strings.Join(def.Nodes, ","), "server": def.Server, "export": def.Export, "path": def.Path, "content": def.Content})
		rows = append(rows, raw)
	}
	return &rows, nil
}
