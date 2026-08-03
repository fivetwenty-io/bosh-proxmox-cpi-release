// Package handlers — internal tests for the config-drive ISO migration-safety
// check (checkISOStorageForHA / haRegistrationFeatures).
package handlers

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
)

// isoHAGroupEnv builds a minimal env map that instanceGroupName resolves to
// groupName via the create-env instance-name fallback path.
func isoHAGroupEnv(groupName string) map[string]any {
	return map[string]any{
		"bosh": map[string]any{
			"instance": map[string]any{
				"name": groupName,
			},
		},
	}
}

// isoHABaseConfig returns a minimal valid CPIConfig with the given iso_storage
// pool and no HA-driven feature enabled. Tests opt individual features in.
func isoHABaseConfig(isoStorage string) *config.CPIConfig {
	vFalse := false
	return &config.CPIConfig{
		Host:          "pve.test.local",
		Port:          8006,
		User:          "root",
		APIToken:      "test-token",
		Node:          "pve01",
		VMStorage:     "vm-storage",
		DiskStorage:   "vm-storage",
		ISOStorage:    isoStorage,
		NetworkBridge: "vmbr0",
		AgentMode:     "noagent",
		VMDiskFormat:  "qcow2",
		VerifySSL:     &vFalse,
	}
}

// panicStorageStub implements clusterstorage.Service and fails the test if
// ListStorage (or any other method) is ever called — used to assert the "no
// HA feature active" fast path in checkISOStorageForHA makes zero PVE calls.
type panicStorageStub struct{ t *testing.T }

var _ clusterstorage.Service = panicStorageStub{}

func (s panicStorageStub) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	s.t.Helper()
	s.t.Fatal("ListStorage must not be called when no HA-registration feature is active")
	return nil, nil
}

func (s panicStorageStub) CreateStorage(context.Context, *clusterstorage.CreateStorageParams) (*clusterstorage.CreateStorageResponse, error) {
	panic("panicStorageStub.CreateStorage: not expected")
}

func (s panicStorageStub) DeleteStorage(context.Context, string) error {
	panic("panicStorageStub.DeleteStorage: not expected")
}

func (s panicStorageStub) GetStorage(context.Context, string) (*clusterstorage.GetStorageResponse, error) {
	panic("panicStorageStub.GetStorage: not expected")
}

func (s panicStorageStub) UpdateStorage(context.Context, string, *clusterstorage.UpdateStorageParams) (*clusterstorage.UpdateStorageResponse, error) {
	panic("panicStorageStub.UpdateStorage: not expected")
}

func TestHARegistrationFeatures_NoneActive_Empty(t *testing.T) {
	cfg := isoHABaseConfig("local")
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{AvailabilityZone: ""}
	got := haRegistrationFeatures(deps, cp, "pve01", nil)
	if len(got) != 0 {
		t.Fatalf("expected no active features, got %v", got)
	}
}

func TestHARegistrationFeatures_DLBEligible(t *testing.T) {
	enabled := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{AvailabilityZone: ""}
	got := haRegistrationFeatures(deps, cp, "pve01", nil)
	if len(got) != 1 || got[0] != haFeatureDLB {
		t.Fatalf("expected [%s], got %v", haFeatureDLB, got)
	}
}

func TestHARegistrationFeatures_AntiAffinity(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &trueVal, UseHaRules: &trueVal},
	}
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{}
	env := isoHAGroupEnv("web")
	got := haRegistrationFeatures(deps, cp, "pve01", env)
	if len(got) != 1 || got[0] != haFeatureAntiAffinity {
		t.Fatalf("expected [%s], got %v", haFeatureAntiAffinity, got)
	}
}

func TestHARegistrationFeatures_AntiAffinity_NoGroupName_Inactive(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &trueVal, UseHaRules: &trueVal},
	}
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{}
	// No env.bosh.instance.name and no env.bosh.group -> instanceGroupName == "".
	got := haRegistrationFeatures(deps, cp, "pve01", nil)
	if len(got) != 0 {
		t.Fatalf("expected no active features (empty group name), got %v", got)
	}
}

func TestHARegistrationFeatures_AZPin(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: &trueVal,
		AZMap:           map[string][]string{"z1": {"pve01"}},
	}
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	got := haRegistrationFeatures(deps, cp, "pve01", nil)
	if len(got) != 1 || got[0] != haFeatureAZPin {
		t.Fatalf("expected [%s], got %v", haFeatureAZPin, got)
	}
}

func TestHARegistrationFeatures_AZPin_NodeNotInAZ_Inactive(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: &trueVal,
		AZMap:           map[string][]string{"z1": {"pve01"}},
	}
	deps := Deps{Config: cfg}
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	// Placed node "pve02" is not in z1's candidate set -> pinAZForNode returns "".
	got := haRegistrationFeatures(deps, cp, "pve02", nil)
	if len(got) != 0 {
		t.Fatalf("expected no active features (node not in AZ), got %v", got)
	}
}

func TestCheckISOStorageForHA_NoFeatureActive_NoPVECallsNoError(t *testing.T) {
	cfg := isoHABaseConfig("local")
	deps := dlbDeps(nil, nil, panicStorageStub{t: t}, cfg)
	cp := createVMCloudProps{}
	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckISOStorageForHA_DLBActive_LocalISO_WarnOnly(t *testing.T) {
	enabled := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	storageSvc := &dlbStorageStub{storageType: "dir", shared: false}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}
	// dlbStorageStub always names its single entry "vm-storage"; point
	// ISOStorage at it so dlbStorageIsShared resolves a definite non-shared
	// classification rather than a "not found" lookup error.
	deps.Config.ISOStorage = "vm-storage"

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("require_shared_iso_for_ha is false: expected nil error (warn-only), got %v", err)
	}
}

func TestCheckISOStorageForHA_DLBActive_SharedISO_NoWarnNoError(t *testing.T) {
	enabled := true
	cfg := isoHABaseConfig("vm-storage")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("shared iso_storage: expected nil error, got %v", err)
	}
}

func TestCheckISOStorageForHA_UndeterminableSharedNess_FailsOpen(t *testing.T) {
	enabled := true
	cfg := isoHABaseConfig("does-not-exist")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	// dlbStorageStub's single entry is named "vm-storage"; "does-not-exist"
	// is absent from the index, so dlbStorageIsShared returns a lookup error.
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("undeterminable shared-ness must fail open (nil error), got %v", err)
	}
}

func TestCheckISOStorageForHA_RequireSharedISOForHA_EscalatesToCloudError(t *testing.T) {
	enabled := true
	requireShared := true
	cfg := isoHABaseConfig("vm-storage")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	cfg.RequireSharedISOForHA = &requireShared
	storageSvc := &dlbStorageStub{storageType: "dir", shared: false}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err == nil {
		t.Fatal("require_shared_iso_for_ha=true: expected a CloudError, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected TypeCloud error, got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "vm-storage") {
		t.Errorf("error message must name the non-shared pool, got: %s", msg)
	}
	if !strings.Contains(msg, string(haFeatureDLB)) {
		t.Errorf("error message must name the triggering feature, got: %s", msg)
	}
}

func TestCheckISOStorageForHA_AntiAffinityActive_LocalISO_WarnOnly(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("vm-storage")
	cfg.Placement = &config.PlacementConfig{
		AntiAffinity: &config.AntiAffinityConfig{Enabled: &trueVal, UseHaRules: &trueVal},
	}
	storageSvc := &dlbStorageStub{storageType: "dir", shared: false}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}
	env := isoHAGroupEnv("web")

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", env, log.NewNopLogger())
	if err != nil {
		t.Fatalf("require_shared_iso_for_ha is false: expected nil error (warn-only), got %v", err)
	}
}

// ---------------------------------------------------------------------------
// isoStorageScanTarget (P5.3: create_vm VMID-allocation ISO-pool scan target)
// ---------------------------------------------------------------------------

func TestIsoStorageScanTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cfg         *config.CPIConfig
		vmStorage   string
		wantStorage string
	}{
		{
			name:        "nil config",
			cfg:         nil,
			vmStorage:   "vm-storage",
			wantStorage: "",
		},
		{
			name:        "distinct iso pool returned",
			cfg:         &config.CPIConfig{ISOStorage: "iso-nfs", DiskStorage: "disk-storage"},
			vmStorage:   "vm-storage",
			wantStorage: "iso-nfs",
		},
		{
			name:        "empty iso_storage is a no-op",
			cfg:         &config.CPIConfig{ISOStorage: "", DiskStorage: "disk-storage"},
			vmStorage:   "vm-storage",
			wantStorage: "",
		},
		{
			name:        "iso_storage equal to vmStorage is a no-op (already scanned)",
			cfg:         &config.CPIConfig{ISOStorage: "vm-storage", DiskStorage: "disk-storage"},
			vmStorage:   "vm-storage",
			wantStorage: "",
		},
		{
			name:        "iso_storage equal to DiskStorage is a no-op",
			cfg:         &config.CPIConfig{ISOStorage: "disk-storage", DiskStorage: "disk-storage"},
			vmStorage:   "vm-storage",
			wantStorage: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := Deps{Config: tc.cfg}
			if got := isoStorageScanTarget(deps, tc.vmStorage); got != tc.wantStorage {
				t.Errorf("isoStorageScanTarget() = %q; want %q", got, tc.wantStorage)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D11: HA-vs-resurrector one-per-process warning
// ---------------------------------------------------------------------------

// resetHAResurrectorWarnOnce lets each test observe a fresh first-fire,
// keeping the suite repeat-safe under -count=N (mirrors the
// vniZoneListWarnOnce reset in internal/pve/export_test.go).
func resetHAResurrectorWarnOnce(t *testing.T) {
	t.Helper()
	haResurrectorWarnOnce = sync.Once{}
	t.Cleanup(func() { haResurrectorWarnOnce = sync.Once{} })
}

func TestWarnHAResurrectorConflictOnce_EmptyFeatures_NoWarn(t *testing.T) {
	resetHAResurrectorWarnOnce(t)
	var buf bytes.Buffer
	warnHAResurrectorConflictOnce(100, nil, warnLogger(t, &buf))
	if out := buf.String(); out != "" {
		t.Errorf("empty feature list must not warn, got %q", out)
	}
}

func TestWarnHAResurrectorConflictOnce_NilLogger_NoPanic(t *testing.T) {
	resetHAResurrectorWarnOnce(t)
	warnHAResurrectorConflictOnce(100, []haRegistrationFeature{haFeatureDLB}, nil)
}

func TestWarnHAResurrectorConflictOnce_FiresOncePerProcess(t *testing.T) {
	resetHAResurrectorWarnOnce(t)
	var buf bytes.Buffer
	logger := warnLogger(t, &buf)
	features := []haRegistrationFeature{haFeatureDLB, haFeatureAZPin}

	warnHAResurrectorConflictOnce(100, features, logger)
	first := buf.String()
	if !strings.Contains(first, "update-resurrection off") {
		t.Errorf("expected the warning to name the bosh update-resurrection off remediation, got %q", first)
	}
	if !strings.Contains(first, string(haFeatureDLB)) || !strings.Contains(first, string(haFeatureAZPin)) {
		t.Errorf("expected the warning to name both triggering features, got %q", first)
	}

	// A second call, even with a different vmid/feature set, must not repeat
	// the warning: it is process-scoped, not per-VM or per-feature-set.
	warnHAResurrectorConflictOnce(200, []haRegistrationFeature{haFeatureAntiAffinity}, logger)
	if got := buf.String(); got != first {
		t.Errorf("second call must not emit an additional warning; buffer grew: %q -> %q", first, got)
	}
}

func TestCheckISOStorageForHA_DLBActive_SharedISO_StillWarnsHAResurrector(t *testing.T) {
	resetHAResurrectorWarnOnce(t)
	enabled := true
	cfg := isoHABaseConfig("vm-storage")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	storageSvc := &dlbStorageStub{storageType: "nfs", shared: true}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	var buf bytes.Buffer
	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, warnLogger(t, &buf))
	if err != nil {
		t.Fatalf("shared iso_storage: expected nil error, got %v", err)
	}
	// The ISO-storage migration-safety warning must NOT fire (shared pool),
	// but the HA-vs-resurrector warning fires regardless of ISO sharing.
	if strings.Contains(buf.String(), "config-drive ISO on non-shared storage") {
		t.Errorf("did not expect the ISO migration-safety warning for a shared pool, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "update-resurrection off") {
		t.Errorf("expected the HA-vs-resurrector warning even with a shared iso_storage pool, got %q", buf.String())
	}
}

func TestCheckISOStorageForHA_NoFeatureActive_NoHAResurrectorWarn(t *testing.T) {
	resetHAResurrectorWarnOnce(t)
	cfg := isoHABaseConfig("local")
	deps := dlbDeps(nil, nil, panicStorageStub{t: t}, cfg)
	cp := createVMCloudProps{}

	var buf bytes.Buffer
	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, warnLogger(t, &buf))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); out != "" {
		t.Errorf("no HA feature active: expected no warning, got %q", out)
	}
}

func TestCheckISOStorageForHA_AZPinActive_LocalISO_WarnOnly(t *testing.T) {
	trueVal := true
	cfg := isoHABaseConfig("vm-storage")
	cfg.Placement = &config.PlacementConfig{
		PinAZViaHARules: &trueVal,
		AZMap:           map[string][]string{"z1": {"pve01"}},
	}
	storageSvc := &dlbStorageStub{storageType: "dir", shared: false}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{AvailabilityZone: "z1"}

	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("require_shared_iso_for_ha is false: expected nil error (warn-only), got %v", err)
	}
}
