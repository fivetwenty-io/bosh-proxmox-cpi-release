// tracing_test.go — white-box tests for the pve service tracing decorators.
// Uses package pve (internal) so the unexported traced*Service types can be
// constructed and exercised directly against small in-package fakes.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// tracingTestConfig returns a minimal valid CPIConfig for NewClient/
// NewClientWithTracer construction tests. NewClient never dials the network
// at construction time, so a non-resolvable host is safe to use here.
func tracingTestConfig() *config.CPIConfig {
	verify := true
	return &config.CPIConfig{
		Host:      "pve.example.invalid",
		Port:      8006,
		User:      "root",
		Password:  "secret",
		Realm:     "pam",
		VerifySSL: &verify,
	}
}

// newTestTracer returns a real SDK tracer wired to an in-memory exporter via
// a SimpleSpanProcessor (synchronous export — no batching/flush needed for
// assertions to see spans immediately after span.End()).
func newTestTracer(t *testing.T) (trace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("pve-tracing-test"), exporter
}

// fakeQEMUService lives in fakes_qemu_test.go.

// --------------------------------------------------------------------------
// (a) overridden method success: exactly one span, correct name, ctx
// parenting, no error status.
// --------------------------------------------------------------------------

func TestTracedQEMUService_Config_Success_OneSpanParentedAndOK(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantCfg := map[string]interface{}{"name": "vm-1"}
	fake := &fakeQEMUService{
		configFn: func(_ context.Context, node string, vmid int) (map[string]interface{}, error) {
			if node != "pve1" || vmid != 42 {
				t.Errorf("Config called with node=%q vmid=%d, want pve1/42", node, vmid)
			}
			return wantCfg, nil
		},
	}
	traced := &tracedQEMUService{Service: fake, tracer: tracer}

	parentCtx, parentSpan := tracer.Start(context.Background(), "test-parent")
	parentSpanID := parentSpan.SpanContext().SpanID()

	cfg, err := traced.Config(parentCtx, "pve1", 42)
	parentSpan.End()

	if err != nil {
		t.Fatalf("Config returned error: %v", err)
	}
	if cfg["name"] != "vm-1" {
		t.Fatalf("Config returned %v, want %v", cfg, wantCfg)
	}

	spans := exporter.GetSpans()
	// Two spans expected: the child "pve.qemu.config" span and the manually
	// started "test-parent" span (ended above). Isolate the child.
	var child *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "pve.qemu.config" {
			child = &spans[i]
		}
	}
	if child == nil {
		t.Fatalf("no span named pve.qemu.config found in %d exported spans", len(spans))
	}
	if child.Parent.SpanID() != parentSpanID {
		t.Errorf("child span parent SpanID = %s, want %s (ctx parenting broken)", child.Parent.SpanID(), parentSpanID)
	}
	if child.Status.Code == codes.Error {
		t.Errorf("child span status = Error on success path, want Unset/Ok: %+v", child.Status)
	}
	var sawNode, sawVMID bool
	for _, kv := range child.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve1"
		case "pve.vmid":
			sawVMID = kv.Value.AsString() == "42"
		}
	}
	if !sawNode {
		t.Error("child span missing/incorrect pve.node attribute")
	}
	if !sawVMID {
		t.Error("child span missing/incorrect pve.vmid attribute")
	}

	// Only one span for the overridden call itself (the manual parent span
	// is a separate, deliberately-started span, not a second decorator span).
	count := 0
	for _, s := range spans {
		if s.Name == "pve.qemu.config" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d spans named pve.qemu.config, want exactly 1", count)
	}
}

// --------------------------------------------------------------------------
// (b) overridden method error: span carries error status + recorded error.
// --------------------------------------------------------------------------

func TestTracedQEMUService_Config_Error_SpanRecordsError(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantErr := errors.New("boom: pve unreachable")
	fake := &fakeQEMUService{
		configFn: func(context.Context, string, int) (map[string]interface{}, error) {
			return nil, wantErr
		},
	}
	traced := &tracedQEMUService{Service: fake, tracer: tracer}

	_, err := traced.Config(context.Background(), "pve1", 7)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Config returned err=%v, want %v", err, wantErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.qemu.config" {
		t.Fatalf("span name = %q, want pve.qemu.config", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", span.Status.Code)
	}
	if span.Status.Description != wantErr.Error() {
		t.Errorf("span status description = %q, want %q", span.Status.Description, wantErr.Error())
	}
	if len(span.Events) == 0 {
		t.Fatal("expected span.RecordError to add an exception event, got none")
	}
}

// --------------------------------------------------------------------------
// (b') overridden method error carrying a credential-bearing URL: the span
// status and recorded error must be scrubbed before export. Spans are an
// external sink, and must not leak what the logs deliberately mask via
// ErrScrubbed.
// --------------------------------------------------------------------------

func TestTracedQEMUService_Config_Error_SpanScrubsCredentialURLs(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	rawErr := errors.New("import failed: GET https://bosh:s3cretpw@blob.lab.internal/stemcell?X-Amz-Signature=deadbeef1234 returned 403")
	fake := &fakeQEMUService{
		configFn: func(context.Context, string, int) (map[string]interface{}, error) {
			return nil, rawErr
		},
	}
	traced := &tracedQEMUService{Service: fake, tracer: tracer}

	if _, err := traced.Config(context.Background(), "pve1", 7); !errors.Is(err, rawErr) {
		t.Fatalf("Config returned err=%v, want the caller to still receive the raw %v", err, rawErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]

	for _, secret := range []string{"s3cretpw", "deadbeef1234"} {
		if strings.Contains(span.Status.Description, secret) {
			t.Errorf("span status description leaks credential %q: %q", secret, span.Status.Description)
		}
		for _, ev := range span.Events {
			for _, attr := range ev.Attributes {
				if v := attr.Value.AsString(); strings.Contains(v, secret) {
					t.Errorf("span event attribute %s leaks credential %q: %q", attr.Key, secret, v)
				}
			}
		}
	}
	if !strings.Contains(span.Status.Description, log.RedactedPlaceholder) {
		t.Errorf("span status description not scrubbed, want %q marker: %q", log.RedactedPlaceholder, span.Status.Description)
	}
}

// --------------------------------------------------------------------------
// (c) embedding passthrough: a non-overridden method (Clone) still works
// through the decorator and produces no decorator span (proves embedding
// didn't silently drop the method, and that undecorated methods stay
// zero-overhead).
// --------------------------------------------------------------------------

func TestTracedQEMUService_Clone_PassthroughUnaffected(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeQEMUService{
		cloneFn: func(_ context.Context, node string, vmid int, params map[string]interface{}) (string, error) {
			if node != "pve2" || vmid != 99 {
				t.Errorf("Clone called with node=%q vmid=%d, want pve2/99", node, vmid)
			}
			return "UPID:pve2:clone", nil
		},
	}
	traced := &tracedQEMUService{Service: fake, tracer: tracer}

	// Compile-time proof the decorator still satisfies qemu.Service despite
	// only overriding 12 of 15 methods.
	var _ qemu.Service = traced

	upid, err := traced.Clone(context.Background(), "pve2", 99, map[string]interface{}{"newid": 100})
	if err != nil {
		t.Fatalf("Clone returned error: %v", err)
	}
	if upid != "UPID:pve2:clone" {
		t.Fatalf("Clone returned %q, want UPID:pve2:clone", upid)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("got %d spans for a non-overridden passthrough call, want 0", got)
	}
}

// --------------------------------------------------------------------------
// (d) nil-tracer construction: NewClient (no tracer) stores raw, undecorated
// services — asserted via a type assertion on the concrete service returned
// from Client.QEMU(), which must NOT be a *tracedQEMUService.
// --------------------------------------------------------------------------

func TestNewClient_NilTracer_StoresRawServices(t *testing.T) {
	t.Parallel()
	cfg := tracingTestConfig()
	c, err := NewClient(cfg, log.NewNopLogger())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, isTraced := c.QEMU().(*tracedQEMUService); isTraced {
		t.Fatal("NewClient (no tracer) returned a *tracedQEMUService — expected raw undecorated service")
	}
	if _, isTraced := c.Pools().(*tracedPoolService); isTraced {
		t.Fatal("NewClient (no tracer) returned a *tracedPoolService — expected raw undecorated service")
	}
}

// TestNewClientWithTracer_NonNilTracer_StoresDecoratedServices is the
// positive counterpart: a non-nil tracer causes NewClientWithTracer to store
// tracing-decorated services for every traced accessor.
func TestNewClientWithTracer_NonNilTracer_StoresDecoratedServices(t *testing.T) {
	t.Parallel()
	tracer, _ := newTestTracer(t)
	cfg := tracingTestConfig()
	c, err := NewClientWithTracer(cfg, log.NewNopLogger(), tracer)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	if _, isTraced := c.QEMU().(*tracedQEMUService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate QEMU service")
	}
	if _, isTraced := c.Storage().(*tracedStorageService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate Storage service")
	}
	if _, isTraced := c.Tasks().(*tracedTasksService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate Tasks service")
	}
	if _, isTraced := c.Nodes().(*tracedNodesService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate Nodes service")
	}
	if _, isTraced := c.Cluster().(*tracedClusterService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate Cluster service")
	}
	if _, isTraced := c.ClusterStorage().(*tracedClusterStorageService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate ClusterStorage service")
	}
	if _, isTraced := c.Pools().(*tracedPoolService); !isTraced {
		t.Fatal("NewClientWithTracer (non-nil tracer) did not decorate Pools service")
	}
	// CloudInit is intentionally never decorated (zero production call
	// sites); it must remain the raw SDK service either way.
	if _, isTraced := c.CloudInit().(interface{ traced() }); isTraced {
		t.Fatal("CloudInit unexpectedly satisfies a traced marker — no CloudInit decorator should exist")
	}
}

// --------------------------------------------------------------------------
// Coverage for the local-var-indirection call surface (SDN + HA on
// Cluster(), firewall on Nodes()) that a plain `.Cluster().X(` grep
// undercounts. fakeClusterService (fakes_cluster_test.go) and
// fakeNodesService (fakes_nodes_test.go) embed the real generated interface
// (nil) and override only the single method each test needs — the same
// embedding idiom tracedClusterService/tracedNodesService themselves use, so
// no 60+-method hand-written stub is required.
// --------------------------------------------------------------------------

// TestTracedClusterService_SDN_CreateSdnZones_Traced covers the SDN call
// surface (reached in production only via `svc := c.Cluster()` /
// `clusterSvc := deps.PVE.Cluster()` indirection — sdn.go, create_network.go).
func TestTracedClusterService_SDN_CreateSdnZones_Traced(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeClusterService{
		createSdnZonesFn: func(context.Context, *cluster.CreateSdnZonesParams) error { return nil },
	}
	traced := &tracedClusterService{Service: fake, tracer: tracer}

	if err := traced.CreateSdnZones(context.Background(), &cluster.CreateSdnZonesParams{}); err != nil {
		t.Fatalf("CreateSdnZones returned error: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	if spans[0].Name != "pve.cluster.create_sdn_zones" {
		t.Fatalf("span name = %q, want pve.cluster.create_sdn_zones", spans[0].Name)
	}
	if spans[0].Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", spans[0].Status)
	}
}

// TestTracedClusterService_HA_DeleteHaRules_Traced covers the HA call
// surface (reached in production only via local-var indirection —
// placement_nodeaffinity.go, placement_antiaffinity.go).
func TestTracedClusterService_HA_DeleteHaRules_Traced(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantErr := errors.New("ha rule not found")
	fake := &fakeClusterService{
		deleteHaRulesFn: func(_ context.Context, rule string) error {
			if rule != "vm-100-affinity" {
				t.Errorf("DeleteHaRules called with rule=%q, want vm-100-affinity", rule)
			}
			return wantErr
		},
	}
	traced := &tracedClusterService{Service: fake, tracer: tracer}

	err := traced.DeleteHaRules(context.Background(), "vm-100-affinity")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DeleteHaRules returned err=%v, want %v", err, wantErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.cluster.delete_ha_rules" {
		t.Fatalf("span name = %q, want pve.cluster.delete_ha_rules", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", span.Status.Code)
	}
	var sawRule bool
	for _, kv := range span.Attributes {
		if string(kv.Key) == "pve.ha_rule" && kv.Value.AsString() == "vm-100-affinity" {
			sawRule = true
		}
	}
	if !sawRule {
		t.Error("span missing/incorrect pve.ha_rule attribute")
	}
}

// TestTracedNodesService_Firewall_CreateQemuFirewallRules_Traced covers the
// Nodes firewall call surface (reached in production only via local-var
// indirection — create_vm_firewall.go's `nodeSvc := deps.PVE.Nodes()`).
func TestTracedNodesService_Firewall_CreateQemuFirewallRules_Traced(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	fake := &fakeNodesService{
		createQemuFirewallRulesFn: func(_ context.Context, node, vmid string, _ *nodes.CreateQemuFirewallRulesParams) error {
			if node != "pve1" || vmid != "100" {
				t.Errorf("CreateQemuFirewallRules called with node=%q vmid=%q, want pve1/100", node, vmid)
			}
			return nil
		},
	}
	traced := &tracedNodesService{Service: fake, tracer: tracer}

	if err := traced.CreateQemuFirewallRules(context.Background(), "pve1", "100", &nodes.CreateQemuFirewallRulesParams{}); err != nil {
		t.Fatalf("CreateQemuFirewallRules returned error: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.nodes.create_qemu_firewall_rules" {
		t.Fatalf("span name = %q, want pve.nodes.create_qemu_firewall_rules", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("span status = Error on success path, want Unset/Ok: %+v", span.Status)
	}
	var sawNode, sawVMID bool
	for _, kv := range span.Attributes {
		switch string(kv.Key) {
		case "pve.node":
			sawNode = kv.Value.AsString() == "pve1"
		case "pve.vmid":
			sawVMID = kv.Value.AsString() == "100"
		}
	}
	if !sawNode || !sawVMID {
		t.Error("span missing/incorrect pve.node or pve.vmid attribute")
	}
}

// --------------------------------------------------------------------------
// ClusterStorage + Pools decorators: exemplar coverage for the two remaining
// decorator types, exercising every overridden method of each (ListStorage;
// AddVM, CreatePool, DeletePool, GetPoolComment) for span name, attribute,
// and status on both success and error paths.
// --------------------------------------------------------------------------

// tracingStubClusterStorageService embeds the generated interface (nil) and overrides
// only ListStorage — the single method tracedClusterStorageService decorates.
type tracingStubClusterStorageService struct {
	clusterstorage.Service
	listErr error
}

func (f *tracingStubClusterStorageService) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &clusterstorage.ListStorageResponse{}, nil
}

func TestTracedClusterStorageService_ListStorage_Traced(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)
	traced := &tracedClusterStorageService{Service: &tracingStubClusterStorageService{}, tracer: tracer}

	if _, err := traced.ListStorage(context.Background(), nil); err != nil {
		t.Fatalf("ListStorage returned err=%v, want nil", err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	if spans[0].Name != "pve.clusterstorage.list_storage" {
		t.Fatalf("span name = %q, want pve.clusterstorage.list_storage", spans[0].Name)
	}
	if spans[0].Status.Code == codes.Error {
		t.Fatalf("success span carries Error status: %v", spans[0].Status)
	}
}

func TestTracedClusterStorageService_ListStorage_Error(t *testing.T) {
	t.Parallel()
	tracer, exporter := newTestTracer(t)

	wantErr := errors.New("list storage failed: connection refused")
	traced := &tracedClusterStorageService{
		Service: &tracingStubClusterStorageService{listErr: wantErr},
		tracer:  tracer,
	}

	if _, err := traced.ListStorage(context.Background(), nil); !errors.Is(err, wantErr) {
		t.Fatalf("ListStorage returned err=%v, want %v", err, wantErr)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d exported spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "pve.clusterstorage.list_storage" {
		t.Fatalf("span name = %q, want pve.clusterstorage.list_storage", span.Name)
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("span status code = %v, want Error", span.Status.Code)
	}
	wantDescription := log.ScrubMessage(wantErr.Error())
	if span.Status.Description != wantDescription {
		t.Errorf("span status description = %q, want %q", span.Status.Description, wantDescription)
	}
	if len(span.Events) == 0 {
		t.Fatal("expected span.RecordError to add an exception event, got none")
	}
}

// fakePoolService implements the full 4-method CPI-owned PoolService.
type fakePoolService struct {
	err error
}

func (f *fakePoolService) AddVM(context.Context, string, int64) error       { return f.err }
func (f *fakePoolService) CreatePool(context.Context, string, string) error { return f.err }
func (f *fakePoolService) DeletePool(context.Context, string) error         { return f.err }
func (f *fakePoolService) GetPoolComment(context.Context, string) (string, bool, error) {
	return "", false, f.err
}

func TestTracedPoolService_AllMethods_Traced(t *testing.T) {
	t.Parallel()

	calls := []struct {
		name     string
		wantSpan string
		invoke   func(*tracedPoolService) error
	}{
		{"AddVM", "pve.pools.add_vm", func(s *tracedPoolService) error {
			return s.AddVM(context.Background(), "bosh-pool", 101)
		}},
		{"CreatePool", "pve.pools.create_pool", func(s *tracedPoolService) error {
			return s.CreatePool(context.Background(), "bosh-pool", "comment")
		}},
		{"DeletePool", "pve.pools.delete_pool", func(s *tracedPoolService) error {
			return s.DeletePool(context.Background(), "bosh-pool")
		}},
		{"GetPoolComment", "pve.pools.get_pool_comment", func(s *tracedPoolService) error {
			_, _, err := s.GetPoolComment(context.Background(), "bosh-pool")
			return err
		}},
	}

	for _, tc := range calls {
		t.Run(tc.name+"_success", func(t *testing.T) {
			tracer, exporter := newTestTracer(t)
			traced := &tracedPoolService{PoolService: &fakePoolService{}, tracer: tracer}
			if err := tc.invoke(traced); err != nil {
				t.Fatalf("%s returned err=%v, want nil", tc.name, err)
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			if spans[0].Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", spans[0].Name, tc.wantSpan)
			}
			foundPool := false
			for _, attr := range spans[0].Attributes {
				if string(attr.Key) == "pve.pool_id" && attr.Value.AsString() == "bosh-pool" {
					foundPool = true
				}
			}
			if !foundPool {
				t.Errorf("span missing pve.pool_id attribute: %v", spans[0].Attributes)
			}
		})
		t.Run(tc.name+"_error", func(t *testing.T) {
			tracer, exporter := newTestTracer(t)
			wantErr := errors.New("pool op failed")
			traced := &tracedPoolService{PoolService: &fakePoolService{err: wantErr}, tracer: tracer}
			if err := tc.invoke(traced); !errors.Is(err, wantErr) {
				t.Fatalf("%s returned err=%v, want %v", tc.name, err, wantErr)
			}
			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			if spans[0].Status.Code != codes.Error {
				t.Fatalf("error span status = %v, want Error", spans[0].Status.Code)
			}
		})
	}
}
