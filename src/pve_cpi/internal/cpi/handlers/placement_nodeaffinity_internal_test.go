package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// naClusterStub captures the full CreateHaRules params so node-affinity tests can
// assert Type/Nodes/Strict, which aaClusterStub does not track.
type naClusterStub struct {
	cluster.Service
	resources   map[string]bool
	rules       map[string]cluster.CreateHaRulesParams
	createCalls int
	deleteCalls int
	resDelCalls int
}

var _ cluster.Service = (*naClusterStub)(nil)

func newNAStub() *naClusterStub {
	return &naClusterStub{resources: map[string]bool{}, rules: map[string]cluster.CreateHaRulesParams{}}
}

func (s *naClusterStub) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	s.resources[p.Sid] = true
	return nil
}

func (s *naClusterStub) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	s.resDelCalls++
	delete(s.resources, sid)
	return nil
}

func (s *naClusterStub) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	out := make(cluster.ListHaRulesResponse, 0, len(s.rules))
	for name, p := range s.rules {
		raw, _ := json.Marshal(map[string]any{"rule": name, "type": p.Type, "resources": p.Resources})
		out = append(out, raw)
	}
	return &out, nil
}

func (s *naClusterStub) CreateHaRules(_ context.Context, p *cluster.CreateHaRulesParams) error {
	s.createCalls++
	s.rules[p.Rule] = *p
	return nil
}

func (s *naClusterStub) DeleteHaRules(_ context.Context, rule string) error {
	s.deleteCalls++
	delete(s.rules, rule)
	return nil
}

func naDeps(stub *naClusterStub) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

func TestEnsureNodeAffinityPin_CreatesStrictRule(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"pve02", "pve01"}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rule, ok := stub.rules["bosh-na-100"]
	if !ok {
		t.Fatal("expected rule bosh-na-100")
	}
	if rule.Type != "node-affinity" {
		t.Errorf("Type = %q; want node-affinity", rule.Type)
	}
	if rule.Resources != "vm:100" {
		t.Errorf("Resources = %q; want vm:100", rule.Resources)
	}
	if rule.Nodes == nil || *rule.Nodes != "pve01,pve02" {
		t.Errorf("Nodes = %v; want sorted pve01,pve02", rule.Nodes)
	}
	if rule.Strict == nil || !*rule.Strict {
		t.Errorf("Strict = %v; want true", rule.Strict)
	}
	if !stub.resources["vm:100"] {
		t.Error("vm:100 must be registered as an HA resource")
	}
}

func TestEnsureNodeAffinityPin_NonStrict(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 7,
		[]string{"pve01"}, false, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rule := stub.rules["bosh-na-7"]
	if rule.Strict == nil || *rule.Strict {
		t.Errorf("Strict = %v; want false (preferred pin)", rule.Strict)
	}
}

func TestEnsureNodeAffinityPin_EmptyNodesNoOp(t *testing.T) {
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"", "  "}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("empty node set must create no rule; createCalls=%d", stub.createCalls)
	}
}

func TestEnsureNodeAffinityPin_RefreshesExisting(t *testing.T) {
	stub := newNAStub()
	// Pre-seed a stale rule (e.g. a create_vm retry).
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: "node-affinity"}
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100,
		[]string{"pve03"}, true, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.deleteCalls != 1 {
		t.Errorf("stale rule must be deleted before recreate; deleteCalls=%d", stub.deleteCalls)
	}
	if got := stub.rules["bosh-na-100"]; got.Nodes == nil || *got.Nodes != "pve03" {
		t.Errorf("refreshed rule Nodes = %v; want pve03", got.Nodes)
	}
}

func TestRemoveNodeAffinityPin_DeletesRuleAndResource(t *testing.T) {
	stub := newNAStub()
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100"}
	stub.resources["vm:100"] = true

	if err := removeNodeAffinityPin(context.Background(), naDeps(stub), 100, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stub.rules["bosh-na-100"]; ok {
		t.Error("rule bosh-na-100 must be deleted")
	}
	if stub.resources["vm:100"] {
		t.Error("HA resource vm:100 must be deregistered")
	}
}

func TestRemoveNodeAffinityPin_IdempotentWhenAbsent(t *testing.T) {
	stub := newNAStub() // nothing to delete
	if err := removeNodeAffinityPin(context.Background(), naDeps(stub), 100, log.NewNopLogger()); err != nil {
		t.Fatalf("absent rule/resource must be a no-op; got %v", err)
	}
}

// naPinConfig builds a Config with pinning enabled and the given AZ map.
func naPinConfig(azMap map[string][]string) *config.CPIConfig {
	cfg := icMinConfig()
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: boolPtrHelper(true),
		PinAZStrict:     boolPtrHelper(true),
		AZMap:           azMap,
	}
	return cfg
}

func boolPtrHelper(b bool) *bool { return &b }

func TestPinAZForNode_SingularAndPlural(t *testing.T) {
	cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}, "z2": {"pve03"}})
	// Singular form.
	if got := pinAZForNode(createVMCloudProps{AvailabilityZone: "z1"}, cfg, "pve02"); got != "z1" {
		t.Errorf("singular: pinAZForNode = %q; want z1", got)
	}
	// Plural form: the VM requested [z2, z1] and landed on pve03 (in z2).
	if got := pinAZForNode(createVMCloudProps{AvailabilityZones: []string{"z2", "z1"}}, cfg, "pve03"); got != "z2" {
		t.Errorf("plural: pinAZForNode = %q; want z2", got)
	}
	// Node not in any requested AZ (e.g. config.node fallback) → "".
	if got := pinAZForNode(createVMCloudProps{AvailabilityZone: "z1"}, cfg, "pve09"); got != "" {
		t.Errorf("non-member node: pinAZForNode = %q; want empty", got)
	}
}

// warnLogger builds a *log.Logger backed by buf at "warn" level, so Warn calls
// are captured and Debug/Info noise is not, keeping assertions focused.
func warnLogger(t *testing.T, buf *bytes.Buffer) *log.Logger {
	t.Helper()
	logger, err := log.NewLogger("warn", buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	return logger
}

func TestWarnSingleNodeAZPin_SingleNodeStrict_Warns(t *testing.T) {
	var buf bytes.Buffer
	warnSingleNodeAZPin("z1", []string{"pve01"}, true, warnLogger(t, &buf))
	out := buf.String()
	if !strings.Contains(out, "small node set") {
		t.Errorf("expected small-node-set warning, got %q", out)
	}
	if !strings.Contains(out, "z1") || !strings.Contains(out, "pve01") {
		t.Errorf("expected warning to name AZ z1 and node pve01, got %q", out)
	}
}

func TestWarnSingleNodeAZPin_TwoNodeStrict_Warns(t *testing.T) {
	// A two-node pinned set still wedges if both nodes are simultaneously
	// down or drained, so it must warn just like the single-node case.
	var buf bytes.Buffer
	warnSingleNodeAZPin("z1", []string{"pve01", "pve02"}, true, warnLogger(t, &buf))
	out := buf.String()
	if !strings.Contains(out, "small node set") {
		t.Errorf("expected small-node-set warning for a two-node AZ, got %q", out)
	}
	if !strings.Contains(out, "pve01") || !strings.Contains(out, "pve02") {
		t.Errorf("expected warning to name both nodes, got %q", out)
	}
}

func TestWarnSingleNodeAZPin_ThreeOrMoreNodes_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	warnSingleNodeAZPin("z1", []string{"pve01", "pve02", "pve03"}, true, warnLogger(t, &buf))
	if out := buf.String(); out != "" {
		t.Errorf("AZ with >= 3 nodes must not warn, got %q", out)
	}
}

func TestWarnSingleNodeAZPin_NonStrict_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	// Single-node AZ, but the pin is preferred (non-strict): HA can relocate
	// off-AZ on failure, so the hazard this warning describes does not apply.
	warnSingleNodeAZPin("z1", []string{"pve01"}, false, warnLogger(t, &buf))
	if out := buf.String(); out != "" {
		t.Errorf("non-strict pin must not warn, got %q", out)
	}
}

func TestWarnSingleNodeAZPin_DedupCollapsesToSingleNode(t *testing.T) {
	var buf bytes.Buffer
	// Blank and duplicate entries collapse to one effective node; the hazard
	// still applies and must be reported on the effective, not raw, count.
	warnSingleNodeAZPin("z1", []string{"pve01", "", "pve01", "  "}, true, warnLogger(t, &buf))
	if out := buf.String(); !strings.Contains(out, "small node set") {
		t.Errorf("expected small-node-set warning after dedup, got %q", out)
	}
}

func TestWarnSingleNodeAZPin_EmptyNodesNoOp(t *testing.T) {
	var buf bytes.Buffer
	warnSingleNodeAZPin("z1", []string{"", "  "}, true, warnLogger(t, &buf))
	if out := buf.String(); out != "" {
		t.Errorf("empty AZ node set must not warn (nothing will be pinned), got %q", out)
	}
}

func TestApplyAZNodeAffinityPin_SingleNodeAZ_WarnsEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	logger := warnLogger(t, &buf)
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "small node set") {
		t.Errorf("expected small-node-set warning through applyAZNodeAffinityPin, got %q", out)
	}
}

func TestApplyAZNodeAffinityPin_TwoNodeAZ_WarnsEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	logger := warnLogger(t, &buf)
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "small node set") {
		t.Errorf("expected small-node-set warning for a two-node AZ through applyAZNodeAffinityPin, got %q", out)
	}
}

func TestApplyAZNodeAffinityPin_ThreeNodeAZ_NoSmallNodeSetWarn(t *testing.T) {
	var buf bytes.Buffer
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01", "pve02", "pve03"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	logger := warnLogger(t, &buf)
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "small node set") {
		t.Errorf(">=3-node AZ must not emit the small-node-set warning, got %q", out)
	}
}

func TestApplyAZNodeAffinityPin_PluralAZ_PinsChosenAZ(t *testing.T) {
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}, "z2": {"pve02", "pve03"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	// Plural-AZ VM placed on pve03 (in z2). This is the regression the CRITICAL
	// fix closes: the old code read the singular AvailabilityZone (empty here)
	// and never pinned.
	cp := createVMCloudProps{AvailabilityZones: []string{"z1", "z2"}}
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100, cp, "pve03", log.NewNopLogger()); err != nil {
		t.Fatalf("pin must succeed for plural-AZ placement: %v", err)
	}

	rule, ok := stub.rules["bosh-na-100"]
	if !ok {
		t.Fatal("plural-AZ VM must be pinned (bosh-na-100 missing)")
	}
	if rule.Nodes == nil || *rule.Nodes != "pve02,pve03" {
		t.Errorf("pin Nodes = %v; want z2 node set pve02,pve03", rule.Nodes)
	}
}

func TestApplyAZNodeAffinityPin_DisabledNoOp(t *testing.T) {
	stub := newNAStub()
	deps := naDeps(stub) // icMinConfig has no Placement → pin disabled
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", log.NewNopLogger()); err != nil {
		t.Fatalf("disabled pin must be a no-op, not an error: %v", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("pin disabled must create no rule; createCalls=%d", stub.createCalls)
	}
}

func TestApplyAZNodeAffinityPin_FallbackNodeSkips(t *testing.T) {
	stub := newNAStub()
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01"}}),
		PVE:    &icPVEClient{clusterSvc: stub},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	// VM requested z1 but landed on config.node "pve99" (fallback) → no AZ
	// membership → no pin (skipped, not errored).
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve99", log.NewNopLogger()); err != nil {
		t.Fatalf("fallback-node skip must not error: %v", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("fallback node outside AZ must not pin; createCalls=%d", stub.createCalls)
	}
}

func TestNARuleNameFor(t *testing.T) {
	if got := naRuleNameFor(100); got != "bosh-na-100" {
		t.Errorf("naRuleNameFor(100) = %q; want bosh-na-100", got)
	}
}

func TestSortedNodesCSV_DedupAndSort(t *testing.T) {
	if got := sortedNodesCSV([]string{"pve02", "pve01", "pve02", " ", "pve03"}); got != "pve01,pve02,pve03" {
		t.Errorf("sortedNodesCSV = %q; want pve01,pve02,pve03", got)
	}
}

// ---------------------------------------------------------------------------
// F3: an enabled pin that resolves no AZ must be visible above Debug.
//
// BOSH does not pass the cloud-config AZ name to create_vm — the AZ only
// selects the CPI and the subnet — so the CPI learns the AZ solely from
// cloud_properties.availability_zone. An operator who sets
// pin_az_via_ha_rules and az_map but never adds availability_zone to a
// vm_type got a completely inert feature: pinAZForNode returned "",
// applyAZNodeAffinityPin skipped at Debug, and the VM was created with no HA
// registration and no signal at the default log level.
// ---------------------------------------------------------------------------

// TestApplyAZNodeAffinityPin_NoResolvableAZ_Warns pins the promotion from
// Debug to Warn: the config says "pin every VM" and the CPI is pinning none.
func TestApplyAZNodeAffinityPin_NoResolvableAZ_Warns(t *testing.T) {
	cases := []struct {
		name string
		cp   createVMCloudProps
		node string
	}{
		{
			// The live V7 shape: cloud_properties carried no availability_zone
			// at all, so there is no AZ to resolve.
			name: "no availability_zone in cloud_properties",
			cp:   createVMCloudProps{},
			node: "pve01",
		},
		{
			// AZ requested, but the VM landed outside its node set (operator
			// target_node override, local-disk pin, or config.node fallback).
			name: "placed node outside the requested AZ node set",
			cp:   createVMCloudProps{AvailabilityZone: "z1"},
			node: "pve99",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			deps := Deps{
				Config: naPinConfig(map[string][]string{"z1": {"pve01", "pve02", "pve03"}}),
				PVE:    &icPVEClient{clusterSvc: newNAStub()},
				Agent:  &icAgentStub{},
				Logger: log.NewNopLogger(),
			}
			if err := applyAZNodeAffinityPin(context.Background(), deps, 100, tc.cp, tc.node,
				warnLogger(t, &buf)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out := buf.String()
			if out == "" {
				t.Fatal("an enabled pin that resolves no AZ must warn, not skip at Debug")
			}
			// The message has to name the cloud-config prerequisite, or the
			// operator has no way to act on it.
			if !strings.Contains(out, "availability_zone") {
				t.Errorf("warning must name cloud_properties.availability_zone, got %q", out)
			}
		})
	}
}

// TestApplyAZNodeAffinityPin_Disabled_StaysSilent keeps the warning scoped to
// operators who actually asked for pinning: with the feature off there is
// nothing inert to report.
func TestApplyAZNodeAffinityPin_Disabled_StaysSilent(t *testing.T) {
	var buf bytes.Buffer
	cfg := naPinConfig(map[string][]string{"z1": {"pve01"}})
	cfg.Placement.PinAZViaHARules = nil // feature not enabled
	deps := Deps{
		Config: cfg,
		PVE:    &icPVEClient{clusterSvc: newNAStub()},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{}, "pve01", warnLogger(t, &buf)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); out != "" {
		t.Errorf("pin disabled: expected no warning, got %q", out)
	}
}

// TestApplyAZNodeAffinityPin_ResolvableAZ_NoInertWarn guards against the
// warning firing on the healthy path.
func TestApplyAZNodeAffinityPin_ResolvableAZ_NoInertWarn(t *testing.T) {
	var buf bytes.Buffer
	deps := Deps{
		Config: naPinConfig(map[string][]string{"z1": {"pve01", "pve02", "pve03"}}),
		PVE:    &icPVEClient{clusterSvc: newNAStub()},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
	if err := applyAZNodeAffinityPin(context.Background(), deps, 100,
		createVMCloudProps{AvailabilityZone: "z1"}, "pve01", warnLogger(t, &buf)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "availability_zone") {
		t.Errorf("a resolvable AZ must not emit the inert-pin warning, got %q", out)
	}
}
