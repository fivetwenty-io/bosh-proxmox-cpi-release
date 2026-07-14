// Package handlers — internal tests for DLB membership, version guard, CRS helpers.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// --------------------------------------------------------------------------
// dlbClusterStub — in-process cluster.Service for DLB tests
// --------------------------------------------------------------------------

// dlbClusterStub implements cluster.Service with configurable HA and options
// methods. Non-overridden methods panic (embedded nil interface).
type dlbClusterStub struct {
	cluster.Service

	createHACalls  []*cluster.CreateHaResourcesParams
	updateHACalls  []dlbUpdateHACall
	listConfigResp *cluster.ListConfigNodesResponse
	listConfigErr  error
	listOptionsRaw *cluster.ListOptionsResponse
	listOptionsErr error
	updateOptsCRS  []string // collected Crs values passed to UpdateOptions
	updateOptsErr  error
}

type dlbUpdateHACall struct {
	Sid    string
	Params *cluster.UpdateHaResourcesParams
}

var _ cluster.Service = (*dlbClusterStub)(nil)

func (s *dlbClusterStub) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	s.createHACalls = append(s.createHACalls, p)
	return nil
}

func (s *dlbClusterStub) UpdateHaResources(_ context.Context, sid string, p *cluster.UpdateHaResourcesParams) error {
	s.updateHACalls = append(s.updateHACalls, dlbUpdateHACall{Sid: sid, Params: p})
	return nil
}

func (s *dlbClusterStub) ListConfigNodes(_ context.Context) (*cluster.ListConfigNodesResponse, error) {
	if s.listConfigErr != nil {
		return nil, s.listConfigErr
	}
	if s.listConfigResp != nil {
		return s.listConfigResp, nil
	}
	// Default: two-node cluster so multi-node guard passes.
	raw1, _ := json.Marshal(map[string]any{"node": "pve01"})
	raw2, _ := json.Marshal(map[string]any{"node": "pve02"})
	resp := cluster.ListConfigNodesResponse{raw1, raw2}
	return &resp, nil
}

func (s *dlbClusterStub) ListOptions(_ context.Context) (*cluster.ListOptionsResponse, error) {
	if s.listOptionsErr != nil {
		return nil, s.listOptionsErr
	}
	if s.listOptionsRaw != nil {
		return s.listOptionsRaw, nil
	}
	// Default: crs=ha=dynamic,ha-auto-rebalance=1,ha-rebalance-on-start=1.
	raw, _ := json.Marshal(map[string]any{
		"crs": "ha=dynamic,ha-auto-rebalance=1,ha-rebalance-on-start=1",
	})
	resp := cluster.ListOptionsResponse(raw)
	return &resp, nil
}

func (s *dlbClusterStub) UpdateOptions(_ context.Context, p *cluster.UpdateOptionsParams) error {
	if s.updateOptsErr != nil {
		return s.updateOptsErr
	}
	if p != nil && p.Crs != nil {
		s.updateOptsCRS = append(s.updateOptsCRS, *p.Crs)
	}
	return nil
}

// dlbNodesStub — configurable nodes.Service for version checks.
type dlbNodesStub struct {
	nodes.Service // nil — panics on unconfigured methods
	versionResp   *nodes.ListVersionResponse
	versionErr    error
}

var _ nodes.Service = (*dlbNodesStub)(nil)

func (s *dlbNodesStub) ListVersion(_ context.Context, _ string) (*nodes.ListVersionResponse, error) {
	if s.versionErr != nil {
		return nil, s.versionErr
	}
	if s.versionResp != nil {
		return s.versionResp, nil
	}
	// Default: PVE 9.2.
	return &nodes.ListVersionResponse{Release: "9", Version: "9.2-1", Repoid: "abc"}, nil
}

// --------------------------------------------------------------------------
// dlbStorageStub — configurable clusterstorage.Service for shared-storage guard
// --------------------------------------------------------------------------

type dlbStorageStub struct {
	storageType string
	shared      bool
	listErr     error
}

var _ clusterstorage.Service = (*dlbStorageStub)(nil)

func (s *dlbStorageStub) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	sharedInt := 0
	if s.shared {
		sharedInt = 1
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": "vm-storage",
		"type":    s.storageType,
		"shared":  sharedInt,
	})
	resp := clusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}

func (s *dlbStorageStub) CreateStorage(_ context.Context, _ *clusterstorage.CreateStorageParams) (*clusterstorage.CreateStorageResponse, error) {
	panic("dlbStorageStub.CreateStorage: not expected")
}
func (s *dlbStorageStub) DeleteStorage(_ context.Context, _ string) error {
	panic("dlbStorageStub.DeleteStorage: not expected")
}
func (s *dlbStorageStub) GetStorage(_ context.Context, _ string) (*clusterstorage.GetStorageResponse, error) {
	panic("dlbStorageStub.GetStorage: not expected")
}
func (s *dlbStorageStub) UpdateStorage(_ context.Context, _ string, _ *clusterstorage.UpdateStorageParams) (*clusterstorage.UpdateStorageResponse, error) {
	panic("dlbStorageStub.UpdateStorage: not expected")
}

// --------------------------------------------------------------------------
// dlbPVEClient — wires cluster, nodes, and clusterstorage services
// --------------------------------------------------------------------------

type dlbPVEClient struct {
	icPVEClient    // reuse nil-returning stubs for unused services
	clusterSvc     cluster.Service
	nodesSvc       nodes.Service
	clusterStorage clusterstorage.Service
}

func (c *dlbPVEClient) Cluster() cluster.Service               { return c.clusterSvc }
func (c *dlbPVEClient) Nodes() nodes.Service                   { return c.nodesSvc }
func (c *dlbPVEClient) ClusterStorage() clusterstorage.Service { return c.clusterStorage }

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// dlbConfigWithDLB returns a minimal CPIConfig with the DLB block configured.
// manage: whether manage_cluster_crs is true.
// requireShared: whether require_shared_storage is true.
func dlbConfigWithDLB(manage, requireShared bool) *config.CPIConfig {
	vFalse := false
	manageCRS := manage
	reqShared := requireShared
	enabled := true
	c := &config.CPIConfig{
		Host:          "pve.test.local",
		Port:          8006,
		User:          "root",
		APIToken:      "test-token",
		Node:          "pve01",
		VMStorage:     "vm-storage",
		DiskStorage:   "vm-storage",
		NetworkBridge: "vmbr0",
		AgentMode:     "noagent",
		VMDiskFormat:  "qcow2",
		VerifySSL:     &vFalse,
		Placement: &config.PlacementConfig{
			DLB: &config.DLBConfig{
				Enabled:              &enabled,
				ManageClusterCRS:     &manageCRS,
				RequireSharedStorage: &reqShared,
			},
		},
	}
	return c
}

func dlbDeps(clusterSvc cluster.Service, nodesSvc nodes.Service, storageSvc clusterstorage.Service, cfg *config.CPIConfig) Deps {
	return Deps{
		Config: cfg,
		PVE: &dlbPVEClient{
			clusterSvc:     clusterSvc,
			nodesSvc:       nodesSvc,
			clusterStorage: storageSvc,
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// ensureDLBMembership tests
// --------------------------------------------------------------------------

func TestDLBMembership_EligibleAllGuardsPass_RegistersWithAutoRebalance(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{} // default 9.2, multi-node
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(false, true)

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 100, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(clusterSvc.createHACalls) != 1 {
		t.Fatalf("expected 1 CreateHaResources call, got %d", len(clusterSvc.createHACalls))
	}
	p := clusterSvc.createHACalls[0]
	if p.Sid != "vm:100" {
		t.Errorf("sid: want %q, got %q", "vm:100", p.Sid)
	}
	if p.AutoRebalance == nil || !*p.AutoRebalance {
		t.Error("AutoRebalance must be true")
	}
	if p.State == nil || *p.State != "started" {
		t.Errorf("State: want %q, got %v", "started", p.State)
	}
}

func TestDLBMembership_SingleNode_SkipsRegistration(t *testing.T) {
	// Override ListConfigNodes to return one node.
	raw, _ := json.Marshal(map[string]any{"node": "pve01"})
	singleNode := cluster.ListConfigNodesResponse{raw}
	clusterSvc := &dlbClusterStub{listConfigResp: &singleNode}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(false, false)

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 101, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 0 {
		t.Errorf("single-node: CreateHaResources must not be called, got %d calls", len(clusterSvc.createHACalls))
	}
}

func TestDLBMembership_OldPVEVersion_Skips(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{
		versionResp: &nodes.ListVersionResponse{Version: "9.1-3"},
	}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(false, false)

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 102, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 0 {
		t.Errorf("old PVE: CreateHaResources must not be called")
	}
}

func TestDLBMembership_LocalStorage_RequireSharedTrue_Skips(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{}
	// dir-type is local (not shared by default flag).
	storageSvc := &dlbStorageStub{storageType: "dir", shared: false}
	cfg := dlbConfigWithDLB(false, true) // require_shared_storage=true

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 103, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 0 {
		t.Errorf("local storage: CreateHaResources must not be called")
	}
}

func TestDLBMembership_AlreadyExists_UpdatesFlags(t *testing.T) {
	// CreateHaResources returns "already defined" → UpdateHaResources must be called.
	// Override to return already-exists error.
	alreadyStub := &dlbClusterStubAlreadyExists{dlbClusterStub: dlbClusterStub{}}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(false, false)

	deps := dlbDeps(alreadyStub, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 104, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(alreadyStub.updateHACalls) != 1 {
		t.Fatalf("expected 1 UpdateHaResources call, got %d", len(alreadyStub.updateHACalls))
	}
	u := alreadyStub.updateHACalls[0]
	if u.Sid != "vm:104" {
		t.Errorf("update sid: want %q, got %q", "vm:104", u.Sid)
	}
	if u.Params.AutoRebalance == nil || !*u.Params.AutoRebalance {
		t.Error("UpdateHaResources AutoRebalance must be true")
	}
	if u.Params.State == nil || *u.Params.State != "started" {
		t.Errorf("UpdateHaResources State must be %q", "started")
	}
}

// dlbClusterStubAlreadyExists wraps dlbClusterStub to make CreateHaResources
// return "already defined" on the first call.
type dlbClusterStubAlreadyExists struct {
	dlbClusterStub
}

func (s *dlbClusterStubAlreadyExists) CreateHaResources(_ context.Context, _ *cluster.CreateHaResourcesParams) error {
	return fmt.Errorf("already defined")
}

func TestDLBMembership_ManageCRS_NotDynamic_CallsUpdateOptions(t *testing.T) {
	// crs is not dynamic; manage_cluster_crs=true → UpdateOptions must be called.
	raw, _ := json.Marshal(map[string]any{"crs": "ha=static"})
	resp := cluster.ListOptionsResponse(raw)
	clusterSvc := &dlbClusterStub{listOptionsRaw: &resp}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(true, false) // manage_cluster_crs=true

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 105, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.updateOptsCRS) == 0 {
		t.Error("manage_cluster_crs=true + non-dynamic: UpdateOptions must be called")
	}
	// Verify resulting CRS contains ha=dynamic.
	merged := parseCRS(clusterSvc.updateOptsCRS[0])
	if !crsHasDynamic(merged) {
		t.Errorf("UpdateOptions CRS must include ha=dynamic, got %q", clusterSvc.updateOptsCRS[0])
	}
	if merged["ha-rebalance-on-start"] != "1" {
		t.Errorf("UpdateOptions CRS must include ha-rebalance-on-start=1")
	}
	if merged["ha-auto-rebalance"] != "1" {
		t.Errorf("UpdateOptions CRS must include ha-auto-rebalance=1")
	}
}

func TestDLBMembership_ManageCRSFalse_NotDynamic_NoUpdateOptions(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"crs": "ha=static"})
	resp := cluster.ListOptionsResponse(raw)
	svc := &dlbClusterStub{listOptionsRaw: &resp}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB(false, false) // manage_cluster_crs=false

	deps := dlbDeps(svc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 106, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(svc.updateOptsCRS) != 0 {
		t.Errorf("manage_cluster_crs=false: UpdateOptions must NOT be called, got %d calls", len(svc.updateOptsCRS))
	}
	// CreateHaResources should still have been called (registration proceeds).
	if len(svc.createHACalls) != 1 {
		t.Errorf("CreateHaResources should still be called; got %d calls", len(svc.createHACalls))
	}
}

// --------------------------------------------------------------------------
// parseCRS / formatCRS / crsHasDynamic unit tests
// --------------------------------------------------------------------------

func TestParseCRS_Empty(t *testing.T) {
	m := parseCRS("")
	if len(m) != 0 {
		t.Errorf("empty: want empty map, got %v", m)
	}
}

func TestParseCRS_SingleKV(t *testing.T) {
	m := parseCRS("ha=dynamic")
	if m["ha"] != "dynamic" {
		t.Errorf("single kv: want ha=dynamic, got %q", m["ha"])
	}
}

func TestParseCRS_MultipleKV(t *testing.T) {
	m := parseCRS("ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1")
	if m["ha"] != "dynamic" {
		t.Errorf("ha: want dynamic, got %q", m["ha"])
	}
	if m["ha-rebalance-on-start"] != "1" {
		t.Errorf("ha-rebalance-on-start: want 1, got %q", m["ha-rebalance-on-start"])
	}
	if m["ha-auto-rebalance"] != "1" {
		t.Errorf("ha-auto-rebalance: want 1, got %q", m["ha-auto-rebalance"])
	}
}

func TestParseCRS_RoundTrip(t *testing.T) {
	input := "ha=dynamic,ha-auto-rebalance=1,ha-rebalance-on-start=1"
	// Parsed then formatted should produce the same sorted output.
	m := parseCRS(input)
	got := formatCRS(m)
	// formatCRS sorts keys; compare parsed maps.
	m2 := parseCRS(got)
	for k, v := range m {
		if m2[k] != v {
			t.Errorf("round-trip key %q: want %q, got %q", k, v, m2[k])
		}
	}
	for k, v := range m2 {
		if m[k] != v {
			t.Errorf("round-trip key %q: want %q, got %q", k, v, m[k])
		}
	}
}

func TestCRSHasDynamic_True(t *testing.T) {
	m := parseCRS("ha=dynamic,ha-rebalance-on-start=1")
	if !crsHasDynamic(m) {
		t.Error("crsHasDynamic: expected true")
	}
}

func TestCRSHasDynamic_False(t *testing.T) {
	m := parseCRS("ha=static")
	if crsHasDynamic(m) {
		t.Error("crsHasDynamic: expected false for ha=static")
	}
}

func TestCRSHasDynamic_Empty(t *testing.T) {
	m := parseCRS("")
	if crsHasDynamic(m) {
		t.Error("crsHasDynamic: expected false for empty map")
	}
}

func TestFormatCRS_Empty(t *testing.T) {
	got := formatCRS(map[string]string{})
	if got != "" {
		t.Errorf("empty map: want %q, got %q", "", got)
	}
}

func TestFormatCRS_Sorted(t *testing.T) {
	m := map[string]string{
		"z-key": "3",
		"a-key": "1",
		"m-key": "2",
	}
	got := formatCRS(m)
	want := "a-key=1,m-key=2,z-key=3"
	if got != want {
		t.Errorf("formatCRS sorted: want %q, got %q", want, got)
	}
}

func TestFormatCRS_KeyWithoutValue(t *testing.T) {
	m := map[string]string{"ha": "dynamic", "bare": ""}
	got := formatCRS(m)
	// "bare" has no "=" emitted; "ha=dynamic" is included.
	parsed := parseCRS(got)
	if parsed["ha"] != "dynamic" {
		t.Errorf("ha: want dynamic, got %q", parsed["ha"])
	}
	if _, ok := parsed["bare"]; !ok {
		t.Error("bare key must be preserved in round-trip")
	}
}

// --------------------------------------------------------------------------
// pveVersionAtLeast tests
// --------------------------------------------------------------------------

func TestPVEVersionAtLeast(t *testing.T) {
	cases := []struct {
		version string
		major   int
		minor   int
		want    bool
	}{
		{"9.2-1", 9, 2, true},
		{"9.1-3", 9, 2, false},
		{"9.3-0", 9, 2, true},
		{"10.0", 9, 2, true},
		{"10.0-1", 9, 2, true},
		{"8.4-2", 9, 2, false},
		{"9.2", 9, 2, true},
		{"", 9, 2, false},
		{"9", 9, 2, false}, // no minor segment → unknown → false
		{"abc", 9, 2, false},
		{"9.abc-1", 9, 2, false},
		{"9.2-1", 10, 0, false}, // 9.x < 10.x
		{"10.0", 10, 0, true},
		{"10.1-2", 10, 0, true},
	}
	for _, tc := range cases {
		got := pveVersionAtLeast(tc.version, tc.major, tc.minor)
		if got != tc.want {
			t.Errorf("pveVersionAtLeast(%q, %d, %d): want %v, got %v",
				tc.version, tc.major, tc.minor, tc.want, got)
		}
	}
}

// --------------------------------------------------------------------------
// dlbMultiStorageStub — name-aware storage list for the root+disk pool guard.
// Embeds dlbStorageStub to inherit the panic stubs for the unused methods and
// shadows ListStorage to return a distinct entry per configured pool name.
// --------------------------------------------------------------------------

type dlbStorageEntry struct {
	storageType string
	shared      bool
}

type dlbMultiStorageStub struct {
	dlbStorageStub
	entries map[string]dlbStorageEntry
}

func (s *dlbMultiStorageStub) ListStorage(_ context.Context, _ *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	resp := make(clusterstorage.ListStorageResponse, 0, len(s.entries))
	for name, e := range s.entries {
		sharedInt := 0
		if e.shared {
			sharedInt = 1
		}
		raw, _ := json.Marshal(map[string]any{
			"storage": name,
			"type":    e.storageType,
			"shared":  sharedInt,
		})
		resp = append(resp, json.RawMessage(raw))
	}
	return &resp, nil
}

// TestDLBMembership_LocalPersistentDisk_RequireSharedTrue_Skips verifies the
// shared-storage guard checks the persistent disk pool too: a VM with a shared
// root pool but a node-local persistent disk pool must NOT be DLB-registered,
// since a local disk blocks live migration.
func TestDLBMembership_LocalPersistentDisk_RequireSharedTrue_Skips(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{} // 9.2, multi-node
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"shared-root": {storageType: "rbd", shared: true},
		"local-data":  {storageType: "lvm", shared: false},
	}}
	cfg := dlbConfigWithDLB(false, true) // require_shared_storage=true
	cfg.VMStorage = "shared-root"
	cfg.DiskStorage = "local-data"

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 104, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 0 {
		t.Errorf("local persistent disk: CreateHaResources must not be called, got %d", len(clusterSvc.createHACalls))
	}
}

// TestDLBMembership_SharedRootAndDisk_Registers verifies that when BOTH the
// root and persistent disk pools are shared, DLB registration proceeds.
func TestDLBMembership_SharedRootAndDisk_Registers(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"shared-root": {storageType: "rbd", shared: true},
		"shared-data": {storageType: "nfs", shared: true},
	}}
	cfg := dlbConfigWithDLB(false, true)
	cfg.VMStorage = "shared-root"
	cfg.DiskStorage = "shared-data"

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 105, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 1 {
		t.Fatalf("shared root+disk: expected 1 CreateHaResources call, got %d", len(clusterSvc.createHACalls))
	}
}

// TestDLBMembership_LocalISOStorage_RequireSharedTrue_Skips verifies the
// shared-storage guard also checks the resolved ConfigDrive ISO pool: a VM
// with a shared root pool and shared disk pool but a node-local iso_storage
// pool must NOT be DLB-registered, since the scsi30 CD-ROM cannot follow the
// VM across live migration.
func TestDLBMembership_LocalISOStorage_RequireSharedTrue_Skips(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{} // 9.2, multi-node
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"shared-root": {storageType: "rbd", shared: true},
		"shared-data": {storageType: "nfs", shared: true},
		"local":       {storageType: "dir", shared: false},
	}}
	cfg := dlbConfigWithDLB(false, true) // require_shared_storage=true
	cfg.VMStorage = "shared-root"
	cfg.DiskStorage = "shared-data"
	cfg.ISOStorage = "local"

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 106, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 0 {
		t.Errorf("local iso_storage: CreateHaResources must not be called, got %d", len(clusterSvc.createHACalls))
	}
}

// TestDLBMembership_SharedRootDiskAndISO_Registers verifies that when the
// root pool, persistent disk pool, AND ConfigDrive ISO pool are all shared,
// DLB registration proceeds.
func TestDLBMembership_SharedRootDiskAndISO_Registers(t *testing.T) {
	clusterSvc := &dlbClusterStub{}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"shared-root": {storageType: "rbd", shared: true},
		"shared-data": {storageType: "nfs", shared: true},
		"shared-iso":  {storageType: "nfs", shared: true},
	}}
	cfg := dlbConfigWithDLB(false, true)
	cfg.VMStorage = "shared-root"
	cfg.DiskStorage = "shared-data"
	cfg.ISOStorage = "shared-iso"

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, 107, "dlb", log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.createHACalls) != 1 {
		t.Fatalf("shared root+disk+iso: expected 1 CreateHaResources call, got %d", len(clusterSvc.createHACalls))
	}
}
