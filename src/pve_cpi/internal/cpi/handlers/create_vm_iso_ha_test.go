// Package handlers — internal tests for the config-drive ISO migration-safety
// check (checkISOStorageForHA / haRegistrationFeatures).
package handlers

import (
	"context"
	"strings"
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
