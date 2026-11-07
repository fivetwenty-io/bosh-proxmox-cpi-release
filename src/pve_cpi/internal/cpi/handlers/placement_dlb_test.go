// Package handlers — internal tests for the DLB CRS check and CRS helpers.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
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
	// haComment is the comment GetHaResources reports for an existing
	// resource; nil reports a resource with no comment.
	haComment *string
	// getHAErr, when non-nil, makes GetHaResources fail.
	getHAErr error
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

func (s *dlbClusterStub) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	if s.getHAErr != nil {
		return nil, s.getHAErr
	}
	return &cluster.GetHaResourcesResponse{Sid: sid, Comment: s.haComment}, nil
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
	// Default: crs=ha=dynamic,ha-auto-rebalance=1,ha-rebalance-on-start=1, in
	// the parsed-datacenter.cfg shape PVE returns it.
	return dlbListOptions(`{"ha":"dynamic","ha-auto-rebalance":1,"ha-rebalance-on-start":1}`), nil
}

// dlbListOptions builds a /cluster/options response carrying the given crs
// JSON, so a test can pick either shape the field has been seen to take: the
// object of sub-options PVE parses out of datacenter.cfg, or a bare property
// string.
func dlbListOptions(crsJSON string) *cluster.ListOptionsResponse {
	return &cluster.ListOptionsResponse{Crs: json.RawMessage(crsJSON)}
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

// dlbConfigWithDLB returns a minimal CPIConfig with pve.ha.mode auto and the
// DLB master switch on.
func dlbConfigWithDLB() *config.CPIConfig {
	vFalse := false
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
		HA:            haDLBEnabled(),
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

// dlbDecision is the eligibility decision of a VM registered in a DLB AZ.
func dlbDecision(vmid int) *ha.VMDecision {
	return &ha.VMDecision{VMID: vmid, SID: haResourceSid(vmid), Outcome: ha.OutcomeRegister, DLB: true}
}

func TestDLBMembership_NoDLBDecision_ReadsNothing(t *testing.T) {
	// A nil decision, a skipped VM, and a registered VM outside a DLB AZ make no
	// CRS read: the stub panics on any cluster call it was not configured for,
	// and ListOptions fails so a read would surface as an error.
	clusterSvc := &dlbClusterStub{listOptionsErr: fmt.Errorf("must not be read")}
	deps := dlbSharedDeps(clusterSvc)
	cases := map[string]*ha.VMDecision{
		"nil":         nil,
		"skip in dlb": {VMID: 120, SID: "vm:120", Outcome: ha.OutcomeSkip, DLB: true},
		"register":    {VMID: 121, SID: "vm:121", Outcome: ha.OutcomeRegister},
		"fail in dlb": {VMID: 122, SID: "vm:122", Outcome: ha.OutcomeFail, DLB: true},
	}
	for name, d := range cases {
		if err := ensureDLBMembership(context.Background(), deps, d, log.NewNopLogger()); err != nil {
			t.Errorf("%s: want no CRS read, got %v", name, err)
		}
	}
	if len(clusterSvc.updateOptsCRS) != 0 {
		t.Errorf("UpdateOptions must NOT be called, got %v", clusterSvc.updateOptsCRS)
	}
}

func TestDLBMembership_ErrorNamesTheCRSCheck(t *testing.T) {
	clusterSvc := &dlbClusterStub{listOptionsErr: fmt.Errorf("boom")}
	err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(123), log.NewNopLogger())
	if err == nil || !strings.HasPrefix(err.Error(), "DLB: cluster CRS check: ") {
		t.Errorf("want the CRS check prefix, got %v", err)
	}
}
func TestDLBMembership_CRS_NotDynamic_NeverWritesAndRegistersNothing(t *testing.T) {
	// The CPI reads the cluster CRS mode and warns. It never writes it, because
	// the mode affects every HA guest in the cluster. The check registers no
	// resource either: the staging order registers the VM before it runs.
	svc := &dlbClusterStub{listOptionsRaw: dlbListOptions(`{"ha":"static"}`)}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}

	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	deps := dlbDeps(svc, nodesSvc, storageSvc, dlbConfigWithDLB())
	if err := ensureDLBMembership(context.Background(), deps, dlbDecision(106), logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(svc.updateOptsCRS) != 0 {
		t.Errorf("UpdateOptions must NOT be called, got %d calls", len(svc.updateOptsCRS))
	}
	if len(svc.createHACalls) != 0 || len(svc.updateHACalls) != 0 {
		t.Errorf("the CRS check must write no HA resource; got %d creates and %d updates",
			len(svc.createHACalls), len(svc.updateHACalls))
	}
	warned := false
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "cluster crs is not dynamic") {
			warned = true
		}
	}
	if !warned {
		t.Error("a non-dynamic cluster must produce the CRS warning")
	}
}

func TestDLBMembership_CRS_AlreadyDynamic_NoUpdateOptions(t *testing.T) {
	// PVE returns crs as an object of sub-options with the flags as unquoted
	// integers. That is already the required setting, so nothing may be written.
	clusterSvc := &dlbClusterStub{
		listOptionsRaw: dlbListOptions(`{"ha":"dynamic","ha-rebalance-on-start":1,"ha-auto-rebalance":1}`),
	}
	nodesSvc := &dlbNodesStub{}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	cfg := dlbConfigWithDLB()

	deps := dlbDeps(clusterSvc, nodesSvc, storageSvc, cfg)
	if err := ensureDLBMembership(context.Background(), deps, dlbDecision(107), log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(clusterSvc.updateOptsCRS) != 0 {
		t.Errorf("crs already dynamic: UpdateOptions must NOT be called, got %v", clusterSvc.updateOptsCRS)
	}
}

func dlbSharedDeps(clusterSvc *dlbClusterStub) Deps {
	return dlbDeps(clusterSvc, &dlbNodesStub{}, &dlbStorageStub{storageType: "nfs", shared: true}, dlbConfigWithDLB())
}

func TestDLBMembership_CRS_UndecodableCRS_ErrorsWithoutWriting(t *testing.T) {
	// A crs value the CPI cannot decode must never merge from an empty base:
	// that write would drop every sub-option the operator configured.
	for _, raw := range []string{`["ha=dynamic"]`, `{"ha":{"mode":"dynamic"}}`, `{`} {
		clusterSvc := &dlbClusterStub{listOptionsRaw: dlbListOptions(raw)}
		err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(111), log.NewNopLogger())
		if err == nil || !strings.Contains(err.Error(), "decode cluster crs") {
			t.Errorf("crs %s: want a decode error, got %v", raw, err)
		}
		if len(clusterSvc.updateOptsCRS) != 0 {
			t.Errorf("crs %s: UpdateOptions must NOT be called, got %v", raw, clusterSvc.updateOptsCRS)
		}
	}
}

func TestDLBMembership_CRS_UndecodableCRS_Errors(t *testing.T) {
	clusterSvc := &dlbClusterStub{listOptionsRaw: dlbListOptions(`["ha=dynamic"]`)}
	err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(112), log.NewNopLogger())
	if err == nil || !strings.Contains(err.Error(), "decode cluster crs") {
		t.Errorf("want a decode error, got %v", err)
	}
}

func TestDLBMembership_CRS_ListOptionsError_Wrapped(t *testing.T) {
	clusterSvc := &dlbClusterStub{listOptionsErr: fmt.Errorf("boom")}
	err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(113), log.NewNopLogger())
	if err == nil || !strings.Contains(err.Error(), "read cluster options: boom") {
		t.Errorf("want the wrapped ListOptions error, got %v", err)
	}
	if len(clusterSvc.updateOptsCRS) != 0 {
		t.Errorf("UpdateOptions must NOT be called after a failed read, got %v", clusterSvc.updateOptsCRS)
	}
}

func TestDLBMembership_CRS_WarnHintPreservesOperatorSubOptions(t *testing.T) {
	// pvesh set --crs replaces the whole property string, so the corrective
	// command in the WARN must carry the operator's other sub-options too.
	clusterSvc := &dlbClusterStub{
		listOptionsRaw: dlbListOptions(`{"ha":"static","ha-auto-rebalance-margin":50}`),
	}
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	if err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(115), logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var warn *log.Entry
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "cluster crs is not dynamic") {
			entry := e
			warn = &entry
		}
	}
	if warn == nil {
		t.Fatalf("no crs WARN logged; entries: %v", obs.All())
	}
	if got, want := warn.Attrs["current_crs"], "ha=static,ha-auto-rebalance-margin=50"; got != want {
		t.Errorf("current_crs: want %q, got %v", want, got)
	}
	wantFix := "pvesh set /cluster/options --crs ha=dynamic,ha-auto-rebalance=1,ha-auto-rebalance-margin=50,ha-rebalance-on-start=1"
	if got := warn.Attrs["fix"]; got != wantFix {
		t.Errorf("fix: want %q, got %v", wantFix, got)
	}
}

func TestDLBMembership_CRS_Dynamic_NoWarn(t *testing.T) {
	// A cluster already running ha=dynamic must not be warned about, whatever
	// the rebalance flags say; that is the noise the object decode removes.
	clusterSvc := &dlbClusterStub{listOptionsRaw: dlbListOptions(`{"ha":"dynamic","ha-auto-rebalance":0}`)}
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	if err := ensureDLBMembership(context.Background(), dlbSharedDeps(clusterSvc), dlbDecision(116), logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "cluster crs") {
			t.Errorf("unexpected crs WARN: %v", e)
		}
	}
}
func TestParseCRSRaw(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "object with integer flags",
			raw:  `{"ha":"dynamic","ha-rebalance-on-start":1,"ha-auto-rebalance":0}`,
			want: map[string]string{"ha": "dynamic", "ha-rebalance-on-start": "1", "ha-auto-rebalance": "0"},
		},
		{
			name: "object with string flags",
			raw:  `{"ha":"dynamic","ha-rebalance-on-start":"1"}`,
			want: map[string]string{"ha": "dynamic", "ha-rebalance-on-start": "1"},
		},
		{
			name: "object with boolean flags",
			raw:  `{"ha":"dynamic","ha-rebalance-on-start":true,"ha-auto-rebalance":false}`,
			want: map[string]string{"ha": "dynamic", "ha-rebalance-on-start": "1", "ha-auto-rebalance": "0"},
		},
		{
			name: "bare property string",
			raw:  `"ha=dynamic,ha-auto-rebalance=1"`,
			want: map[string]string{"ha": "dynamic", "ha-auto-rebalance": "1"},
		},
		{
			name: "object with numeric sub-option",
			raw:  `{"ha":"dynamic","ha-auto-rebalance-margin":50}`,
			want: map[string]string{"ha": "dynamic", "ha-auto-rebalance-margin": "50"},
		},
		{
			name: "object values and keys are trimmed like property-string tokens",
			raw:  `{" ha ":" dynamic "}`,
			want: map[string]string{"ha": "dynamic"},
		},
		{
			name: "object with null sub-option",
			raw:  `{"ha":"dynamic","ha-auto-rebalance":null}`,
			want: map[string]string{"ha": "dynamic", "ha-auto-rebalance": ""},
		},
		{name: "empty string value", raw: `""`, want: map[string]string{}},
		{name: "absent", raw: ``, want: map[string]string{}},
		{name: "null", raw: `null`, want: map[string]string{}},
		{name: "unexpected array", raw: `["ha=dynamic"]`, wantErr: true},
		{name: "unexpected number", raw: `1`, wantErr: true},
		{name: "malformed", raw: `{`, wantErr: true},
		{name: "object sub-option value", raw: `{"ha":{"mode":"dynamic"}}`, wantErr: true},
		{name: "array sub-option value", raw: `{"ha":["dynamic"]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCRSRaw(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got map %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("parseCRSRaw must never return a nil map on success")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseCRSRaw_ObjectRoundTripsThroughFormatCRS(t *testing.T) {
	// The WARN log renders the map back to a property string; the object shape
	// must survive that as the equivalent crs setting.
	parsed, err := parseCRSRaw(json.RawMessage(`{"ha":"dynamic","ha-auto-rebalance":1}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := formatCRS(parsed)
	const want = "ha=dynamic,ha-auto-rebalance=1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

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
	// The crs format declares no default key, so a bare token is rejected by
	// PVE; the write side must drop it rather than emit it.
	m := map[string]string{"ha": "dynamic", "bare": ""}
	if got, want := formatCRS(m), "ha=dynamic"; got != want {
		t.Errorf("formatCRS: want %q, got %q", want, got)
	}
	if got := formatCRS(map[string]string{"bare": ""}); got != "" {
		t.Errorf("formatCRS of only value-less keys: want empty, got %q", got)
	}
}

func TestParseCRS_BareTokenIsKeptOnRead(t *testing.T) {
	parsed := parseCRS("ha=dynamic,bare")
	if v, ok := parsed["bare"]; !ok || v != "" {
		t.Errorf("bare token must be kept on read with an empty value, got %v", parsed)
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
	// server/export/path are optional backing-identity fields (see
	// pve.StorageInfo.BackingKey). Left empty they encode nothing extra —
	// every pre-existing fixture that doesn't set them is unaffected.
	server string
	export string
	path   string
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
		row := map[string]any{
			"storage": name,
			"type":    e.storageType,
			"shared":  sharedInt,
		}
		if e.server != "" {
			row["server"] = e.server
		}
		if e.export != "" {
			row["export"] = e.export
		}
		if e.path != "" {
			row["path"] = e.path
		}
		raw, _ := json.Marshal(row)
		resp = append(resp, json.RawMessage(raw))
	}
	return &resp, nil
}
