package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// ---------------------------------------------------------------------------
// multiClusterStorage fakes ClusterStorage().ListStorage with N entries.
// ---------------------------------------------------------------------------

type multiClusterStorage struct {
	entries []map[string]any
	callCnt int
	listErr error
}

func (m *multiClusterStorage) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	m.callCnt++
	if m.listErr != nil {
		return nil, m.listErr
	}
	resp := make(clusterstorage.ListStorageResponse, 0, len(m.entries))
	for _, e := range m.entries {
		raw, _ := json.Marshal(e)
		resp = append(resp, raw)
	}
	return &resp, nil
}

func (m *multiClusterStorage) CreateStorage(_ context.Context, _ *clusterstorage.CreateStorageParams) (*clusterstorage.CreateStorageResponse, error) {
	panic("multiClusterStorage.CreateStorage: not expected")
}
func (m *multiClusterStorage) DeleteStorage(_ context.Context, _ string) error {
	panic("multiClusterStorage.DeleteStorage: not expected")
}
func (m *multiClusterStorage) GetStorage(_ context.Context, _ string) (*clusterstorage.GetStorageResponse, error) {
	panic("multiClusterStorage.GetStorage: not expected")
}
func (m *multiClusterStorage) UpdateStorage(_ context.Context, _ string, _ *clusterstorage.UpdateStorageParams) (*clusterstorage.UpdateStorageResponse, error) {
	panic("multiClusterStorage.UpdateStorage: not expected")
}

var _ clusterstorage.Service = (*multiClusterStorage)(nil)

// depsWithTierCS builds Deps wired with the given ClusterStorage for tier-resolution tests.
func depsWithTierCS(cfg *config.CPIConfig, cs *multiClusterStorage, storageSvc *mockStorageService) handlers.Deps {
	if storageSvc == nil {
		storageSvc = &mockStorageService{}
	}
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			storageSvc:        storageSvc,
			clusterSvc:        &mockClusterSvc{},
			clusterStorageSvc: cs,
		},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// create_disk storage_tier tests
// ---------------------------------------------------------------------------

// TestStorageTier_TypeMatch verifies that a tier with Types:["lvmthin"] picks
// the lvmthin storage pool when multiple pools are present.
func TestStorageTier_TypeMatch(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-lvm", "type": "lvm", "shared": 0},
			{"storage": "thin-pool", "type": "lvmthin", "shared": 0},
			{"storage": "nfs-store", "type": "nfs", "shared": 1},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"block": {Types: []string{"lvmthin"}},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "block"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "thin-pool" {
		t.Errorf("storage_tier=block (lvmthin): want thin-pool, got %q", capturedStorage)
	}
}

// TestStorageTier_SharedMatch verifies that a tier with Shared:true picks a
// shared storage pool and skips local ones.
func TestStorageTier_SharedMatch(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-lvm", "type": "lvm", "shared": 0},
			{"storage": "ceph-pool", "type": "rbd", "shared": 1},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"shared": {Shared: boolPtr(true)},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "shared"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "ceph-pool" {
		t.Errorf("storage_tier=shared: want ceph-pool, got %q", capturedStorage)
	}
}

// TestStorageTier_CombinedCriteria verifies that a tier with both Types and
// Shared set matches only entries satisfying both predicates.
func TestStorageTier_CombinedCriteria(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-zfs", "type": "zfspool", "shared": 0},  // type matches but not shared
			{"storage": "shared-zfs", "type": "zfspool", "shared": 1}, // both match
			{"storage": "ceph-pool", "type": "rbd", "shared": 1},      // shared but wrong type
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"shared-zfs": {Types: []string{"zfspool"}, Shared: boolPtr(true)},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "shared-zfs"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "shared-zfs" {
		t.Errorf("combined criteria: want shared-zfs, got %q", capturedStorage)
	}
}

// TestStorageTier_LexicographicFirst verifies that when multiple storages match,
// the lexicographically first name is returned for determinism.
func TestStorageTier_LexicographicFirst(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		// Order intentionally non-alphabetical to ensure sorting is real.
		entries: []map[string]any{
			{"storage": "zfs-c", "type": "zfspool", "shared": 0},
			{"storage": "zfs-a", "type": "zfspool", "shared": 0},
			{"storage": "zfs-b", "type": "zfspool", "shared": 0},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"zfs": {Types: []string{"zfspool"}},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "zfs"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "zfs-a" {
		t.Errorf("lexicographic first: want zfs-a, got %q", capturedStorage)
	}
}

// TestStorageTier_UnknownTier verifies that an unknown tier name (not in
// config.StorageTiers) returns a non-retriable CloudError without issuing
// a live storage query.
func TestStorageTier_UnknownTier(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers:         map[string]config.StorageTierCriteria{},
	}
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "nonexistent"}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for unknown tier, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("unknown tier error must not be retriable")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q should mention unknown tier name", err.Error())
	}
	// ListStorage must NOT be called when the tier is absent from config.
	if cs.callCnt != 0 {
		t.Errorf("ListStorage called %d times; want 0 when tier absent from config", cs.callCnt)
	}
}

// TestStorageTier_ZeroMatches verifies that a tier with no matching storages
// returns a non-retriable CloudError naming the tier.
func TestStorageTier_ZeroMatches(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-lvm", "type": "lvm", "shared": 0},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"ssd": {Types: []string{"rbd"}},
		},
	}
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "ssd"}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for zero matches, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("zero-match tier error must not be retriable")
	}
	if !strings.Contains(err.Error(), "ssd") {
		t.Errorf("error %q should mention tier name", err.Error())
	}
}

// TestStorageTier_ExplicitPoolWinsNoLiveQuery verifies that when an explicit
// storage_pool is present alongside storage_tier, the explicit pool wins and
// ListStorage is NOT called.
func TestStorageTier_ExplicitPoolWinsNoLiveQuery(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "ceph-pool", "type": "rbd", "shared": 1},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"shared": {Shared: boolPtr(true)},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		// discard/ssd explicitly disabled so the disk-performance
		// auto-resolution (which independently needs a live storage-type
		// lookup when either is left unset — see
		// needsDiskPerfStorageTypeLookup) does not introduce an unrelated
		// ListStorage call, keeping this test isolated to storage_tier
		// resolution behavior alone.
		marshal(map[string]any{"storage_pool": "explicit-pool", "storage_tier": "shared", "discard": false, "ssd": false}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "explicit-pool" {
		t.Errorf("explicit storage_pool must win over storage_tier: got %q, want explicit-pool", capturedStorage)
	}
	// The live cluster storage query must not have run.
	if cs.callCnt != 0 {
		t.Errorf("ListStorage called %d times; want 0 when explicit pool is set", cs.callCnt)
	}
}

// TestStorageTier_NoKey_NoLiveQuery verifies that when storage_tier is absent,
// no live ClusterStorage query is issued and existing fallback behavior is preserved.
func TestStorageTier_NoKey_NoLiveQuery(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		DiskStorage:          storageName,
	}
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		// discard/ssd explicitly disabled so the disk-performance
		// auto-resolution does not introduce an unrelated ListStorage call
		// of its own — see the comment in
		// TestStorageTier_ExplicitPoolWinsNoLiveQuery.
		marshal(map[string]any{"discard": false, "ssd": false}), // no storage_tier key
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.callCnt != 0 {
		t.Errorf("ListStorage called %d times; want 0 when storage_tier not set", cs.callCnt)
	}
}

// TestStorageTier_ListAPIError verifies that a ClusterStorage list failure
// surfaces as an error.
func TestStorageTier_ListAPIError(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		listErr: errors.New("PVE cluster unreachable"),
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"fast": {Types: []string{"lvmthin"}},
		},
	}
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "fast"}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from list API failure, got nil")
	}
}

// TestStorageTier_EmptyTypesWithSharedConstraint verifies that a tier with empty
// Types slice (no type restriction) combined with a Shared constraint filters correctly.
func TestStorageTier_EmptyTypesWithSharedConstraint(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-dir", "type": "dir", "shared": 0},
			{"storage": "nfs-a", "type": "nfs", "shared": 1},
			{"storage": "nfs-b", "type": "nfs", "shared": 1},
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		// No Types restriction — any storage type; Shared must be true.
		StorageTiers: map[string]config.StorageTierCriteria{
			"any-shared": {Shared: boolPtr(true)},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "any-shared"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nfs-a and nfs-b both match; lexicographically first is nfs-a.
	if capturedStorage != "nfs-a" {
		t.Errorf("any-shared tier: want nfs-a (lex-first shared), got %q", capturedStorage)
	}
}

// TestStorageTier_LocalTierFilter verifies that Shared:false selects only local
// (non-shared) storages and skips shared ones.
func TestStorageTier_LocalTierFilter(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "ceph-pool", "type": "rbd", "shared": 1},      // shared — skip
			{"storage": "local-lvm", "type": "lvm", "shared": 0},      // local — match
			{"storage": "local-thin", "type": "lvmthin", "shared": 0}, // local — match
		},
	}
	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"local": {Shared: boolPtr(false)},
		},
	}
	var capturedStorage string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorage = storage
			return storage + ":vm-9000-disk-0", nil
		},
	}
	deps := depsWithTierCS(cfg, cs, storageSvc)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{"storage_tier": "local"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// local-lvm and local-thin both match; lex first is local-lvm.
	if capturedStorage != "local-lvm" {
		t.Errorf("local tier: want local-lvm (lex-first local), got %q", capturedStorage)
	}
}

// ---------------------------------------------------------------------------
// create_vm storage_tier integration test (via resolveVMShapeStorage).
// ---------------------------------------------------------------------------

// TestStorageTier_CreateVM_TierResolvesPool verifies that storage_tier in
// create_vm cloud_properties resolves to a matching storage pool and the
// QEMU create params carry the tier-resolved pool name.
func TestStorageTier_CreateVM_TierResolvesPool(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "local-lvm", "type": "lvm", "shared": 0},
			{"storage": "ceph-rbd", "type": "rbd", "shared": 1},
		},
	}

	cfg := &config.CPIConfig{
		// Opt out of the parked default; parker paths have dedicated tests.
		DetachedDiskStrategy: "free",
		Node:                 vmNode,
		VMStorage:            "", // force tier resolution path
		NetworkBridge:        "vmbr0",
		AgentMode:            "noagent",
		VMIDRangeStart:       100,
		StorageTiers: map[string]config.StorageTierCriteria{
			"shared": {Shared: boolPtr(true)},
		},
	}

	var capturedParams map[string]any
	qemuSvc := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			capturedParams = params
			return "UPID:pve:create:ok", nil
		},
	}
	nodesSvc := &vmMockNodes{}
	clusterSvc := &vmMockCluster{}
	agentSvc := &vmMockAgent{}

	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:           qemuSvc,
			nodesSvc:          nodesSvc,
			clusterSvc:        clusterSvc,
			tasksSvc:          &mockTasksService{},
			storageSvc:        &mockStorageService{},
			clusterStorageSvc: cs,
		},
		Agent:  agentSvc,
		Logger: log.NewNopLogger(),
	}

	args := mkArgs(
		"agent-uuid-tier",
		testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "storage_tier": "shared"},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": "10.0.1.5",
			"netmask": "255.255.255.0", "gateway": "10.0.1.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{},
		map[string]any{},
	)

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("tier-vm-1"))
	if err != nil {
		t.Fatalf("HandleCreateVM with storage_tier: unexpected error: %v", err)
	}
	if capturedParams == nil {
		t.Fatal("QEMU Create was not called")
	}

	// The disk import string (virtio0 or scsi0) must reference ceph-rbd.
	diskParam := ""
	for _, k := range []string{"virtio0", "scsi0"} {
		if v, ok := capturedParams[k].(string); ok && v != "" {
			diskParam = v
			break
		}
	}
	if diskParam == "" {
		t.Fatalf("QEMU Create params missing disk param (virtio0/scsi0): %v", capturedParams)
	}
	if !strings.Contains(diskParam, "ceph-rbd") {
		t.Errorf("disk param %q: expected ceph-rbd as tier-resolved pool; storage_tier wiring broken in resolveVMShapeStorage", diskParam)
	}
}
