package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	pveerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

type managedDiskTestPVE struct {
	pve.Client
	state *managedDiskTestState
}
type managedDiskTestState struct {
	volumes    map[string]*nodes.GetStorageContentResponse
	created    []string
	createErr  error
	readErr    error
	before     func(string)
	beforeList func(string)
	badReturn  bool
	partial    bool
	failFirst  bool
	configs    map[int]map[string]any
	pools      map[string]string
	// poolMembers records which guests each pool holds, which is what the
	// parker pool sweep reads and writes once the guarded window has closed.
	poolMembers   map[string]map[int64]bool
	parkMutations int
	parkErr       error
	existsErr     error
	guestErr      error
}
type managedDiskTestStorage struct {
	storage.Service
	state *managedDiskTestState
}
type managedDiskTestNodes struct {
	nodes.Service
	state *managedDiskTestState
}
type managedDiskTestCluster struct {
	cluster.Service
	state *managedDiskTestState
}
type managedDiskTestDefinitions struct{ clusterstorage.Service }
type managedDiskTestQEMU struct {
	qemu.Service
	state *managedDiskTestState
}
type managedDiskTestPools struct {
	pve.PoolService
	state *managedDiskTestState
}

func (c managedDiskTestPVE) Storage() storage.Service { return managedDiskTestStorage{state: c.state} }
func (c managedDiskTestPVE) Nodes() nodes.Service     { return managedDiskTestNodes{state: c.state} }
func (c managedDiskTestPVE) Cluster() cluster.Service { return managedDiskTestCluster{state: c.state} }
func (c managedDiskTestPVE) ClusterStorage() clusterstorage.Service {
	return managedDiskTestDefinitions{}
}
func (c managedDiskTestPVE) QEMU() qemu.Service     { return managedDiskTestQEMU{state: c.state} }
func (c managedDiskTestPVE) Pools() pve.PoolService { return managedDiskTestPools{state: c.state} }
func (c managedDiskTestPVE) Tasks() tasks.Service   { return &diskSizingTasks{} }
func (q managedDiskTestQEMU) Create(_ context.Context, _ string, params map[string]any) (string, error) {
	q.state.parkMutations++
	vmid, ok := params["vmid"].(int)
	if !ok {
		return "", fmt.Errorf("missing creation VMID")
	}
	if q.state.configs == nil {
		q.state.configs = map[int]map[string]any{}
	}
	q.state.configs[vmid] = map[string]any{}
	for key, value := range params {
		if key != "vmid" {
			q.state.configs[vmid][key] = value
		}
	}
	return "UPID:n1:123:456:789:qmcreate:90000:user:", nil
}
func (p managedDiskTestPools) CreatePool(_ context.Context, id, comment string) error {
	if p.state.pools == nil {
		p.state.pools = map[string]string{}
	}
	p.state.pools[id] = comment
	return nil
}
func (p managedDiskTestPools) DeletePool(_ context.Context, id string) error {
	delete(p.state.pools, id)
	return nil
}
func (p managedDiskTestPools) GetPoolComment(_ context.Context, id string) (string, bool, error) {
	v, ok := p.state.pools[id]
	return v, ok, nil
}
func (p managedDiskTestPools) AddVM(_ context.Context, id string, vmid int64) error {
	if p.state.poolMembers == nil {
		p.state.poolMembers = map[string]map[int64]bool{}
	}
	if p.state.poolMembers[id] == nil {
		p.state.poolMembers[id] = map[int64]bool{}
	}
	p.state.poolMembers[id][vmid] = true
	return nil
}
func (p managedDiskTestPools) PoolHasVM(_ context.Context, id string, vmid int64) (bool, error) {
	return p.state.poolMembers[id][vmid], nil
}
func (q managedDiskTestQEMU) AttachDisk(_ context.Context, _ string, vmid int, volume, bus string, opts *qemu.AttachOpts) (string, error) {
	q.state.parkMutations++
	if q.state.parkErr != nil {
		return "", q.state.parkErr
	}
	q.state.configs[vmid][opts.DiskID] = volume
	return opts.DiskID, nil
}
func (n managedDiskTestNodes) UpdateQemuConfig(_ context.Context, _ string, vmid string, params *nodes.UpdateQemuConfigParams) error {
	n.state.parkMutations++
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		n.state.configs[id][key] = value
	}
	return nil
}
func (q managedDiskTestQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	cloned := map[string]any{}
	for key, value := range q.state.configs[vmid] {
		cloned[key] = value
	}
	return cloned, nil
}
func (c managedDiskTestCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	r := make(cluster.ListResourcesResponse, 0, len(c.state.configs))
	for vmid, vm := range c.state.configs {
		raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": "n1", "type": "qemu", "tags": vm["tags"]})
		r = append(r, raw)
	}
	return &r, nil
}
func (managedDiskTestCluster) ListConfigNodes(context.Context) (*cluster.ListConfigNodesResponse, error) {
	r := cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"n1"}`)}
	return &r, nil
}
func (managedDiskTestCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := cluster.ListStatusResponse{json.RawMessage(`{"type":"cluster","quorate":1}`), json.RawMessage(`{"type":"node","name":"n1","online":1}`)}
	return &r, nil
}
func (managedDiskTestDefinitions) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	r := make(clusterstorage.ListStorageResponse, 0, 2)
	for _, id := range []string{"a", "b"} {
		raw, _ := json.Marshal(map[string]any{"storage": id, "type": "nfs", "server": "nas", "export": "/" + id, "shared": 1, "content": "images"})
		r = append(r, raw)
	}
	return &r, nil
}
func (n managedDiskTestNodes) ListQemu(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	if n.state.guestErr != nil {
		return nil, n.state.guestErr
	}
	r := nodes.ListQemuResponse{}
	for vmid, vm := range n.state.configs {
		raw, _ := json.Marshal(map[string]any{"vmid": vmid, "tags": vm["tags"], "name": vm["name"]})
		r = append(r, raw)
	}
	return &r, nil
}
func (managedDiskTestNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	r := nodes.ListNodesResponse{json.RawMessage(`{"node":"n1","status":"online"}`)}
	return &r, nil
}
func (n managedDiskTestNodes) ListStorage(context.Context, string, *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	r := make(nodes.ListStorageResponse, 0, 2)
	for _, id := range []string{"a", "b"} {
		raw, _ := json.Marshal(map[string]any{"storage": id, "active": 1, "enabled": 1, "total": 100 << 30, "avail": 80 << 30})
		r = append(r, raw)
	}
	return &r, nil
}
func (n managedDiskTestNodes) ListStorageContent(_ context.Context, _ string, pool string, _ *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if n.state.beforeList != nil {
		n.state.beforeList(pool)
	}
	if n.state.existsErr != nil {
		return nil, n.state.existsErr
	}
	r := nodes.ListStorageContentResponse{}
	for id := range n.state.volumes {
		if strings.HasPrefix(id, pool+":") {
			raw, _ := json.Marshal(map[string]any{"volid": id, "content": "images"})
			r = append(r, raw)
		}
	}
	return &r, nil
}
func (n managedDiskTestNodes) GetStorageContent(ctx context.Context, _ string, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.state.readErr != nil {
		return nil, n.state.readErr
	}
	v, ok := n.state.volumes[pool+":"+volume]
	if !ok {
		return nil, fmt.Errorf("missing test volume")
	}
	cloned := *v
	return &cloned, nil
}
func (s managedDiskTestStorage) Exists(_ context.Context, _ string, _ string, volume string) (bool, error) {
	if s.state.existsErr != nil {
		return false, s.state.existsErr
	}
	_, ok := s.state.volumes[volume]
	return ok, nil
}
func (s managedDiskTestStorage) CreateVolume(_ context.Context, _ string, pool string, gib int, format string, vmid int, name string) (string, error) {
	volume := fmt.Sprintf("%s:%d/%s", pool, vmid, name)
	if s.state.before != nil {
		s.state.before(volume)
	}
	s.state.created = append(s.state.created, volume)
	if s.state.partial || s.state.createErr == nil {
		s.state.volumes[volume] = &nodes.GetStorageContentResponse{Format: format, Size: sdk.PVEInt(uint64(gib) << 30)}
	}
	if s.state.createErr != nil {
		err := s.state.createErr
		if s.state.failFirst {
			s.state.createErr = nil
		}
		return "", err
	}
	if s.state.badReturn {
		return "foreign:9000/disk.qcow2", nil
	}
	return volume, nil
}
func (s managedDiskTestStorage) DeleteVolumeAsync(context.Context, string, string, string) (string, error) {
	panic("managed allocation must not run unverified legacy cleanup")
}

func managedDiskFixture(t *testing.T, strategy string, plural bool) (*managedDiskRequest, *aj.Handle, *managedDiskTestState) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	names := []string{"a"}
	if plural {
		names = append(names, "b")
	}
	cfg := &config.CPIConfig{Node: "n1", DiskStorage: "legacy-forbidden", DetachedDiskStrategy: "free", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: dir, PersistentStorageSet: "P", StorageSets: map[string]config.StorageSet{"P": {Names: names, Strategy: config.StoragePlacementStrategy{Name: strategy, Version: 1}}}}
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{}}
	deps := Deps{Config: cfg, PVE: managedDiskTestPVE{state: state}, Logger: log.NewNopLogger()}
	selection, err := ResolveStoragePlacementSelectors(cfg, "create_disk", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newLayeredResolver(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := prepareManagedDisk(t.Context(), deps, selection, 1025, createDiskCloudProperties{}, "", r)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(t.Context(), dir, "namespace", aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "complete-fixture-audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	intent, err := storageJournalIntent("create_disk", []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}, selection, m.inventory, m.plan)
	if err != nil {
		t.Fatal(err)
	}
	h, err := journal.CreateDisk(t.Context(), m.id, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	m.journal = journal
	state.before = func(volume string) {
		rec := h.Record()
		if len(rec.Steps) == 0 || rec.Steps[len(rec.Steps)-1].Target.IntendedVolume != volume || rec.Steps[len(rec.Steps)-1].State != aj.Planned {
			t.Fatal("mutation preceded exact durable intent")
		}
	}
	return m, h, state
}

func TestManagedDiskSelectedTargetAndFreeCID(t *testing.T) {
	for _, strategy := range []string{"spread", "weighted_free_space", "least_utilized"} {
		for _, plural := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", strategy, plural), func(t *testing.T) {
				m, h, state := managedDiskFixture(t, strategy, plural)
				value, err := m.execute(t.Context(), h)
				if err != nil {
					t.Fatal(err)
				}
				cid := value.(string)
				volume, meta, err := pve.ParseEncodedDiskCID(cid)
				if err != nil {
					t.Fatal(err)
				}
				if meta.ID != "" || meta.Anchor || !strings.HasPrefix(volume, m.plan.Targets[0].StorageID+":") || len(cid) > 255 {
					t.Fatalf("wrong free CID %q %+v", cid, meta)
				}
				if len(state.created) != 1 || len(state.volumes) != 1 {
					t.Fatalf("wrong resource count %+v", state.created)
				}
				rec := h.Record()
				if rec.State != aj.ReadyToReturn || rec.CID != cid || rec.Steps[0].Charges[0].AcquiredBytes != 2<<30 {
					t.Fatalf("wrong durable return evidence %+v", rec)
				}
				if !m.ledger.Records()[0].Acquired {
					t.Fatal("completion proof not applied to ledger")
				}
			})
		}
	}
}

func TestManagedDiskUncertainFailuresNeverRetryOrCleanup(t *testing.T) {
	for _, failure := range []string{"transport", "quota", "readonly", "readback", "wrong-return", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			m, h, state := managedDiskFixture(t, "spread", true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "transport", "quota", "readonly":
				state.createErr = errors.New(failure)
				state.partial = true
			case "readback":
				state.readErr = errors.New("read failed")
			case "wrong-return":
				state.badReturn = true
			case "cancelled":
				before := state.before
				state.before = func(v string) { before(v); cancel() }
			}
			_, err := m.execute(ctx, h)
			if err == nil {
				t.Fatal("expected reconciliation")
			}
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
				t.Fatalf("ambiguous outcome advertised retry: %v", err)
			}
			if h.Record().State != aj.ReconciliationRequired || len(state.created) != 1 || len(state.volumes) != 1 {
				t.Fatalf("evidence/resources lost: %+v %+v", h.Record(), state.created)
			}
		})
	}
}

func TestManagedDiskSizeAndCIDBeforeMutation(t *testing.T) {
	for _, size := range []int{0, -1, math.MaxInt} {
		if _, _, err := managedDiskSize(size); err == nil {
			t.Errorf("accepted size %d", size)
		}
	}
	bytes, gib, err := managedDiskSize(1025)
	if err != nil || bytes != 2<<30 || gib != 2 {
		t.Fatalf("rounding %d %d %v", bytes, gib, err)
	}
	m, h, state := managedDiskFixture(t, "spread", false)
	var noise strings.Builder
	for i := range 600 {
		fmt.Fprintf(&noise, "%x", i*i*7919)
	}
	m.opts = map[string]string{"metadata": noise.String()}
	if _, err := m.execute(t.Context(), h); err == nil {
		t.Fatal("oversized CID accepted")
	}
	if len(state.created) != 0 {
		t.Fatal("CID size failure mutated PVE")
	}
}

func TestManagedDiskValidationRejectionIsNarrow(t *testing.T) {
	good := &pveerrors.APIError{HTTPCode: 400, Message: "Parameter verification failed.", Errors: map[string]string{"filename": "rejected"}}
	if _, ok := managedDiskValidationRejection(fmt.Errorf("wrapped: %w", good)); !ok {
		t.Fatal("lost typed preexecution rejection")
	}
	for _, err := range []error{errors.New(good.Error()), &pveerrors.APIError{HTTPCode: 400, Message: "other", Errors: good.Errors}, &pveerrors.APIError{HTTPCode: 500, Message: good.Message, Errors: good.Errors}, &pveerrors.APIError{HTTPCode: 400, Message: good.Message}, &pveerrors.APIError{HTTPCode: 400, Message: good.Message, Errors: map[string]string{"unknown": "x"}}} {
		if _, ok := managedDiskValidationRejection(err); ok {
			t.Fatalf("unsafe rejection classification: %v", err)
		}
	}
}

func TestManagedDiskVerifiedFallbackRetainsAllocationIdentity(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", true)
	state.createErr = &pveerrors.APIError{HTTPCode: 400, Message: "Parameter verification failed.", Errors: map[string]string{"filename": "rejected"}}
	state.failFirst = true
	id, token, fp := m.id, m.token, h.Record().Intent.FrozenInputsFingerprint
	if _, err := m.execute(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	if record.ID != id || record.DiskToken != token || record.Intent.FrozenInputsFingerprint != fp || record.ActiveAttempt() != 1 || len(state.created) != 2 || len(state.volumes) != 1 {
		t.Fatalf("retry identity/evidence %+v resources %+v", record, state.created)
	}
	if strings.Split(state.created[0], ":")[0] == strings.Split(state.created[1], ":")[0] {
		t.Fatal("failed target was selected again")
	}
}

func TestManagedDiskParkedCIDAndPoison(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			m, h, state := managedDiskFixture(t, "spread", false)
			m.deps.Config.DetachedDiskStrategy = "parked"
			vmid := m.deps.Config.ParkedDiskVMIDRangeStartValue()
			state.configs = map[int]map[string]any{vmid: {"name": fmt.Sprintf("bosh-parker-%d", vmid), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci"}}
			if fail {
				state.parkErr = errors.New("lost attach response")
			}
			result, err := m.execute(t.Context(), h)
			if fail {
				if err == nil || h.Record().State != aj.ReconciliationRequired || state.parkMutations != 1 || len(state.volumes) != 1 {
					t.Fatalf("park uncertainty continued mutation or lost evidence: %v %+v", err, h.Record())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cid := result.(string)
			volume, meta, err := pve.ParseEncodedDiskCID(cid)
			if err != nil {
				t.Fatal(err)
			}
			if !meta.Anchor || meta.ID != m.token || !strings.HasPrefix(meta.ID, "bpd-") || len(meta.ID) != 20 || len(cid) > 255 {
				t.Fatalf("invalid parked CID %+v", meta)
			}
			if err := pve.VerifyAllocationParked(t.Context(), m.deps.PVE, m.deps.Log(t.Context()), volume, m.token, m.plan.Namespace, m.id, parkerReadConfigFor(m.deps)); err != nil {
				t.Fatal(err)
			}
			if h.Record().State != aj.ReadyToReturn {
				t.Fatal("parked CID not persisted before return")
			}
		})
	}
}

func TestManagedDiskCreatesParkerWithDurableProvenance(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	m.deps.Config.DetachedDiskStrategy = "parked"
	result, err := m.execute(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	cid := result.(string)
	volume, meta, err := pve.ParseEncodedDiskCID(cid)
	if err != nil {
		t.Fatal(err)
	}
	if err = pve.VerifyAllocationParked(t.Context(), m.deps.PVE, m.deps.Log(t.Context()), volume, meta.ID, m.plan.Namespace, m.id, parkerReadConfigFor(m.deps)); err != nil {
		t.Fatal(err)
	}
	if len(state.configs) != 1 {
		t.Fatal("expected exactly one created parker")
	}
	found := false
	for _, step := range h.Record().Steps {
		if step.Kind == "park_QEMU_Create" {
			found = true
			if step.State != aj.Observed || step.UPID == "" || step.Target.VMID == 0 {
				t.Fatalf("missing parker task evidence %+v", step)
			}
		}
	}
	if !found {
		t.Fatal("parker creation not journaled")
	}
}

func TestManagedDiskIndependentSameSizeCallsRemainDistinct(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	first, err := m.execute(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := newLayeredResolver(nil, m.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	next, err := prepareManagedDisk(t.Context(), m.deps, m.selection, 1025, createDiskCloudProperties{}, "", resolver)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := storageJournalIntent("create_disk", []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}, next.selection, next.inventory, next.plan)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := m.journal.CreateDisk(t.Context(), next.id, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h2.Close(); err != nil {
			t.Error(err)
		}
	})
	state.before = nil
	second, err := next.execute(t.Context(), h2)
	if err != nil {
		t.Fatal(err)
	}
	if next.id == m.id || next.token == m.token || first == second || len(state.volumes) != 2 {
		t.Fatal("independent create_disk requests converged")
	}
}

func TestManagedDiskSubsetAliasBoundaryAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []string
		pool    string
		size    int
		want    string
		fail    bool
	}{
		{"subset", []string{"b"}, "", 1025, "b", false},
		{"same-backing-policy-alias", []string{"a"}, "", 1025, "a", false},
		{"outside-boundary", []string{"a"}, "b", 1025, "", true},
		{"insufficient-capacity", []string{"a"}, "", 81 * 1024, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _, state := managedDiskFixture(t, "spread", true)
			cfg := base.deps.Config
			cfg.StorageSets["pin"] = config.StorageSet{Names: tc.members, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}
			cp := map[string]any{"storage_set": "pin"}
			if tc.pool != "" {
				cfg.PersistentStorageSet = "pin"
				cp = map[string]any{"storage_pool": tc.pool}
			}
			selection, err := ResolveStoragePlacementSelectors(cfg, "create_disk", cp, false)
			if err != nil {
				t.Fatal(err)
			}
			r, err := newLayeredResolver(cp, cfg)
			if err != nil {
				t.Fatal(err)
			}
			m, err := prepareManagedDisk(t.Context(), base.deps, selection, tc.size, createDiskCloudProperties{}, "", r)
			if tc.fail {
				if err == nil {
					t.Fatal("invalid placement admitted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if m.plan.Targets[0].StorageID != tc.want {
					t.Fatalf("selected %s want %s", m.plan.Targets[0].StorageID, tc.want)
				}
			}
			if len(state.created) != 0 {
				t.Fatal("selection validation mutated storage")
			}
		})
	}
}

func TestManagedDiskAtomicLegacyTierWinnerSurvivesFallback(t *testing.T) {
	cfg := selectorConfig()
	cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: map[string]any{"storage_set": "p"}}
	cfg.DiskTypes["d"] = config.TypeProfile{CloudProperties: map[string]any{"storage_pool": "lower-pool"}}
	cp := map[string]any{"vm_type": "v", "disk_type": "d", "storage_tier": "tier"}
	selection := requireSelectors(t, cfg, "create_disk", cp, false)
	if selection.SetManaged || !selection.Persistent.Atomic {
		t.Fatal("fixture did not exercise atomic legacy winner")
	}
	r, err := newLayeredResolver(cp, cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved := managedDiskLegacyResolver(r, selection)
	if pool, ok := resolved.String("storage_pool", "storage"); ok {
		t.Fatalf("lower pool %q escaped atomic selection", pool)
	}
	if tier, ok := resolved.String("storage_tier"); !ok || tier != "tier" {
		t.Fatal("atomic tier winner lost")
	}
	if pool, ok := r.String("storage_pool"); !ok || pool != "lower-pool" {
		t.Fatal("legacy resolver mutated")
	}
}

func TestManagedDiskUnreturnedCIDRequiresAuditInsteadOfImplicitResume(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	value, err := m.execute(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := m.journal.Acquire(t.Context(), m.id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = m.execute(t.Context(), reopened); err == nil {
		t.Fatal("independent create_disk silently adopted unreturned allocation")
	}
	if len(state.created) != 1 || reopened.Record().CID != value.(string) || reopened.Record().State != aj.ReconciliationRequired {
		t.Fatal("unreturned disk evidence lost or duplicate allocated")
	}
}

func TestManagedDiskPreallocationErrorsRedactSecrets(t *testing.T) {
	const secret = "Authorization: Bearer secret-token password=secret-password"
	for _, source := range []string{"exists", "hint", "vmid"} {
		t.Run(source, func(t *testing.T) {
			m, h, state := managedDiskFixture(t, "spread", false)
			var err error
			if source == "exists" {
				state.existsErr = errors.New(secret)
				_, err = m.execute(t.Context(), h)
			} else {
				state.guestErr = errors.New(secret)
				if source == "hint" {
					_, err = managedDiskHintNode(t.Context(), m.deps, "321")
				} else {
					_, err = m.execute(t.Context(), h)
				}
			}
			if err == nil || strings.Contains(err.Error(), "secret-") || strings.Contains(err.Error(), "Authorization") {
				t.Fatalf("unsafe observation error: %v", err)
			}
			if len(state.created) > 0 {
				t.Fatal("failed observation mutated PVE")
			}
		})
	}
}

func (c managedDiskTestPVE) StorageAuditVisibility(context.Context) error { return nil }

func TestManagedPersistentBirthRefusesPreexistingSameSizeName(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	m.deps.Config.DiskVMIDRangeStart = 20000
	m.deps.Config.DiskVMIDRangeEnd = 20001
	target := m.plan.Targets[0]
	name, err := pve.AllocationVolumeName(20000, m.plan.Namespace, m.id, m.format)
	if err != nil {
		t.Fatal(err)
	}
	volume := fmt.Sprintf("%s:20000/%s", target.StorageID, name)
	calls := 0
	state.beforeList = func(pool string) {
		if pool == target.StorageID {
			calls++
			if calls == 2 {
				state.volumes[volume] = &nodes.GetStorageContentResponse{Format: m.format, Size: sdk.PVEInt(uint64(m.sizeGiB) << 30)}
				name2, e := pve.AllocationVolumeName(20001, m.plan.Namespace, m.id, m.format)
				if e != nil {
					t.Fatal(e)
				}
				state.volumes[fmt.Sprintf("%s:20001/%s", target.StorageID, name2)] = &nodes.GetStorageContentResponse{Format: m.format, Size: sdk.PVEInt(uint64(m.sizeGiB) << 30)}
			}
		}
	}
	if _, err = m.executeAttempt(t.Context(), h); err == nil {
		t.Fatal("preexisting same-size UUID target accepted")
	}
	if calls < 2 || state.volumes[volume] == nil {
		t.Fatal("collision observation not exercised")
	}
	if len(state.created) != 0 || len(h.Record().Steps) != 0 {
		t.Fatal("collision submitted or acquired a birth intent")
	}
}
