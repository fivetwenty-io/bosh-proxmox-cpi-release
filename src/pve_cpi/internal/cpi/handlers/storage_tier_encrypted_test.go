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
)

// ---------------------------------------------------------------------------
// Encrypted storage-tier tests: §7.49
// ---------------------------------------------------------------------------

// encryptedCSEntries returns a test cluster storage list with three pools:
//
//   - "enc-pool"    — type rbd, shared, encrypted tier
//   - "plain-pool"  — type rbd, shared, no encrypted flag
//   - "local-plain" — type lvm, local, no encrypted flag
func encryptedCSEntries() []map[string]any {
	return []map[string]any{
		{"storage": "enc-pool", "type": "rbd", "shared": 1},
		{"storage": "plain-pool", "type": "rbd", "shared": 1},
		{"storage": "local-plain", "type": "lvm", "shared": 0},
	}
}

// cfgWithEncryptedTiers builds a CPIConfig whose storage_tiers mark "enc-tier"
// as encrypted and "plain-tier" as not encrypted. Both tiers have Types set so
// they are valid (at least one of Types/Shared must be set per validateStorageTiers).
func cfgWithEncryptedTiers() *config.CPIConfig {
	return &config.CPIConfig{
		Node: testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"enc-tier":   {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
			"plain-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(false)},
			"any-tier":   {Types: []string{"rbd"}},
		},
	}
}

// ---------------------------------------------------------------------------
// StorageTierCriteria.Encrypted: zero-value deserialisation backward compat.
// ---------------------------------------------------------------------------

// TestStorageTierCriteria_EncryptedNilOnAbsent verifies that a StorageTierCriteria
// decoded from JSON without an "encrypted" key leaves the field nil (backward compat).
func TestStorageTierCriteria_EncryptedNilOnAbsent(t *testing.T) {
	t.Parallel()

	var c config.StorageTierCriteria
	if err := json.Unmarshal([]byte(`{"types":["rbd"]}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Encrypted != nil {
		t.Errorf("Encrypted should be nil when absent from JSON; got %v", *c.Encrypted)
	}
}

// TestStorageTierCriteria_EncryptedTrue verifies that encrypted:true decodes correctly.
func TestStorageTierCriteria_EncryptedTrue(t *testing.T) {
	t.Parallel()

	var c config.StorageTierCriteria
	if err := json.Unmarshal([]byte(`{"types":["rbd"],"encrypted":true}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Encrypted == nil || !*c.Encrypted {
		t.Errorf("encrypted:true must decode to *true; got %v", c.Encrypted)
	}
}

// TestStorageTierCriteria_EncryptedFalse verifies that encrypted:false decodes correctly.
func TestStorageTierCriteria_EncryptedFalse(t *testing.T) {
	t.Parallel()

	var c config.StorageTierCriteria
	if err := json.Unmarshal([]byte(`{"types":["rbd"],"encrypted":false}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Encrypted == nil || *c.Encrypted {
		t.Errorf("encrypted:false must decode to *false; got %v", c.Encrypted)
	}
}

// ---------------------------------------------------------------------------
// resolveEncrypted helper
// ---------------------------------------------------------------------------

// TestResolveEncrypted_NilNil verifies that nil/nil resolves to false (no-op path).
func TestResolveEncrypted_NilNil(t *testing.T) {
	t.Parallel()
	if handlers.ResolveEncrypted(nil, nil) {
		t.Error("nil/nil must resolve to false")
	}
}

// TestResolveEncrypted_GlobalTrue verifies that a global *true is honoured when
// callLevel is nil.
func TestResolveEncrypted_GlobalTrue(t *testing.T) {
	t.Parallel()
	if !handlers.ResolveEncrypted(boolPtr(true), nil) {
		t.Error("global *true with nil call must resolve to true")
	}
}

// TestResolveEncrypted_CallFalseOverridesGlobalTrue verifies that an explicit
// per-call false overrides global true (per-call > global precedence).
func TestResolveEncrypted_CallFalseOverridesGlobalTrue(t *testing.T) {
	t.Parallel()
	if handlers.ResolveEncrypted(boolPtr(true), boolPtr(false)) {
		t.Error("per-call false must override global true")
	}
}

// TestResolveEncrypted_CallTrueOverridesGlobalNil verifies that per-call true
// works even when global is unset.
func TestResolveEncrypted_CallTrueOverridesGlobalNil(t *testing.T) {
	t.Parallel()
	if !handlers.ResolveEncrypted(nil, boolPtr(true)) {
		t.Error("per-call true must resolve to true when global is nil")
	}
}

// ---------------------------------------------------------------------------
// create_disk: encrypted=true → encrypted tier selected.
// ---------------------------------------------------------------------------

// TestStorageTierEncrypted_EncryptedTierSelected verifies that when
// cloud_properties.encrypted=true is set and the tier has Encrypted:*true,
// the matching encrypted pool is selected.
func TestStorageTierEncrypted_EncryptedTierSelected(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := cfgWithEncryptedTiers()
	cfg.DiskStorage = "plain-pool"

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
		marshal(map[string]any{"storage_tier": "enc-tier", "encrypted": true}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "enc-pool" {
		t.Errorf("encrypted tier: want enc-pool, got %q", capturedStorage)
	}
}

// TestStorageTierEncrypted_NoEncryptedTierError verifies that when encrypted=true
// is requested but no tier in config is marked Encrypted:*true, a non-retriable
// CloudError is returned naming the requirement.
func TestStorageTierEncrypted_NoEncryptedTierError(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := &config.CPIConfig{
		Node: testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			// no encrypted:true marker on any tier
			"plain-tier": {Types: []string{"rbd"}},
		},
	}
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{"storage_tier": "plain-tier", "encrypted": true}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected non-retriable error when encrypted=true but tier not marked encrypted")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("error must not be retriable")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should mention 'encrypted'", err.Error())
	}
}

// TestStorageTierEncrypted_NilEncryptedByteIdentical verifies that when neither
// cloud_properties nor global config sets encrypted, storage selection is
// byte-identical to the pre-feature behavior (no encrypted filter applied).
func TestStorageTierEncrypted_NilEncryptedByteIdentical(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := cfgWithEncryptedTiers()
	cfg.DiskStorage = "plain-pool"

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
		// any-tier has Types:[rbd], no Encrypted flag — both enc-pool and plain-pool match.
		// lex-first winner is enc-pool (e < p).
		marshal(map[string]any{"storage_tier": "any-tier"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// both enc-pool and plain-pool are rbd; lex-first is "enc-pool".
	if capturedStorage != "enc-pool" {
		t.Errorf("no encrypted flag: want lex-first match enc-pool, got %q", capturedStorage)
	}
}

// TestStorageTierEncrypted_CallFalseOverridesGlobalTrue verifies that a per-call
// encrypted:false overrides a global Encrypted:*true, leaving the unencrypted
// path active.
func TestStorageTierEncrypted_CallFalseOverridesGlobalTrue(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	// Global encrypted=true, but call says false.
	cfg := cfgWithEncryptedTiers()
	cfg.Encrypted = boolPtr(true)
	cfg.DiskStorage = "plain-pool"

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
		// any-tier has no encrypted flag; per-call encrypted:false overrides global
		marshal(map[string]any{"storage_tier": "any-tier", "encrypted": false}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// unencrypted path: any-tier without filter → lex-first rbd pool = enc-pool.
	if capturedStorage != "enc-pool" {
		t.Errorf("call false overrides global true: want lex-first rbd enc-pool, got %q", capturedStorage)
	}
}

// TestStorageTierEncrypted_NamedTierNotEncryptedConflictsError verifies that
// when encrypted=true is requested and an explicit tier is named but that tier
// is not marked Encrypted:*true, a non-retriable CloudError is returned.
func TestStorageTierEncrypted_NamedTierNotEncryptedConflictsError(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := cfgWithEncryptedTiers()

	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		// plain-tier is Encrypted:*false — contradiction with encrypted:true.
		marshal(map[string]any{"storage_tier": "plain-tier", "encrypted": true}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error: named tier not marked encrypted contradicts encrypted=true")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("contradiction error must not be retriable")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should mention 'encrypted'", err.Error())
	}
}

// TestStorageTierEncrypted_GlobalTrueCallTrueEncryptedTierSelected verifies that
// global encrypted=true is honoured for the tier path when no per-call override.
func TestStorageTierEncrypted_GlobalTrueCallTrueEncryptedTierSelected(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := cfgWithEncryptedTiers()
	cfg.Encrypted = boolPtr(true)

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
		marshal(map[string]any{"storage_tier": "enc-tier"}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedStorage != "enc-pool" {
		t.Errorf("global encrypted + enc-tier: want enc-pool, got %q", capturedStorage)
	}
}

// ---------------------------------------------------------------------------
// create_vm ephemeral disk: encrypted flag honoured.
// ---------------------------------------------------------------------------

// TestStorageTierEncrypted_EphemeralEncryptedTierSelected verifies that when
// create_vm cloud_properties carry ephemeral_disk_size_mb + encrypted=true, the
// ephemeral disk is created on the encrypted storage tier.
func TestStorageTierEncrypted_EphemeralEncryptedTierSelected(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}

	cfg := &config.CPIConfig{
		Node:          vmNode,
		VMStorage:     "plain-pool",
		NetworkBridge: "vmbr0",
		AgentMode:     "noagent",
		VMIDRangeStart: 100,
		StorageTiers: map[string]config.StorageTierCriteria{
			"enc-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
		},
	}

	var capturedStorages []string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorages = append(capturedStorages, storage)
			return storage + ":vm-9000-disk-0", nil
		},
	}
	qemuSvc := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "UPID:pve:create:ok", nil
		},
	}

	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:           qemuSvc,
			nodesSvc:          &vmMockNodes{},
			clusterSvc:        &vmMockCluster{},
			tasksSvc:          &mockTasksService{},
			storageSvc:        storageSvc,
			clusterStorageSvc: cs,
		},
		Agent:  &vmMockAgent{},
		Logger: log.NewNopLogger(),
	}

	args := mkArgs(
		"agent-uuid-enc-ephemeral",
		testStemcellCID,
		map[string]any{
			"cores":                  1,
			"memory":                 512,
			"ephemeral_disk_size_mb": 4096,
			"ephemeral_storage_tier": "enc-tier",
			"encrypted":              true,
		},
		map[string]any{"default": map[string]any{
			"type":    "manual",
			"ip":      "10.0.1.6",
			"netmask": "255.255.255.0", "gateway": "10.0.1.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{},
		map[string]any{},
	)

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("enc-ephemeral-vm"))
	if err != nil {
		t.Fatalf("HandleCreateVM with encrypted ephemeral: unexpected error: %v", err)
	}

	// At least one CreateVolume call for the ephemeral disk must target enc-pool.
	encPoolUsed := false
	for _, s := range capturedStorages {
		if s == "enc-pool" {
			encPoolUsed = true
		}
	}
	if !encPoolUsed {
		t.Errorf("encrypted ephemeral disk must use enc-pool; got storages: %v", capturedStorages)
	}
}

// ---------------------------------------------------------------------------
// F1: auto-select encrypted tier when no tier/pool named (create_disk path).
// ---------------------------------------------------------------------------

// TestStorageTierEncrypted_AutoSelectNoTierNamed verifies that encrypted=true with
// no storage_tier and no storage_pool auto-selects the lex-first encrypted tier
// from config and places the disk on the matching pool.
func TestStorageTierEncrypted_AutoSelectNoTierNamed(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	// Config has two encrypted tiers; lex-first is "a-enc-tier" → enc-pool.
	cfg := &config.CPIConfig{
		Node: testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"a-enc-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
			"b-enc-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
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
		// No storage_tier, no storage_pool; encrypted=true triggers auto-select.
		marshal(map[string]any{"encrypted": true}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// a-enc-tier is lex-first; its Types:[rbd] matches enc-pool.
	if capturedStorage != "enc-pool" {
		t.Errorf("auto-select: want enc-pool (lex-first encrypted tier a-enc-tier), got %q", capturedStorage)
	}
}

// TestStorageTierEncrypted_AutoSelectDeterministicTwoTiers verifies that when two
// encrypted tiers both match the live storage list, the lex-first tier name wins.
func TestStorageTierEncrypted_AutoSelectDeterministicTwoTiers(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{
		entries: []map[string]any{
			{"storage": "enc-a", "type": "rbd", "shared": 1},
			{"storage": "enc-b", "type": "lvm", "shared": 0},
		},
	}
	cfg := &config.CPIConfig{
		Node: testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			// lex order: "tier-a" < "tier-b"
			"tier-a": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
			"tier-b": {Types: []string{"lvm"}, Encrypted: boolPtr(true)},
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
		marshal(map[string]any{"encrypted": true}),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// tier-a is lex-first → Types:[rbd] matches enc-a.
	if capturedStorage != "enc-a" {
		t.Errorf("auto-select determinism: want enc-a (tier-a lex-first), got %q", capturedStorage)
	}
}

// TestStorageTierEncrypted_AutoSelectNoEncryptedTierInConfig verifies that
// encrypted=true with no explicit tier/pool and no encrypted tier in config
// returns a non-retriable CloudError.
func TestStorageTierEncrypted_AutoSelectNoEncryptedTierInConfig(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := &config.CPIConfig{
		Node: testNode,
		StorageTiers: map[string]config.StorageTierCriteria{
			"plain-tier": {Types: []string{"rbd"}}, // no Encrypted:*true
		},
	}
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{"encrypted": true}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error: no encrypted tier in config")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("error must not be retriable")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should mention 'encrypted'", err.Error())
	}
}

// TestStorageTierEncrypted_ExplicitPoolWithEncryptedError verifies that
// encrypted=true + explicit storage_pool returns a non-retriable CloudError
// (CPI cannot verify the named pool is encrypted).
func TestStorageTierEncrypted_ExplicitPoolWithEncryptedError(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := cfgWithEncryptedTiers()
	deps := depsWithTierCS(cfg, cs, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]any{"storage_pool": "some-pool", "encrypted": true}),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error: explicit storage_pool + encrypted=true is a contradiction")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("contradiction error must not be retriable")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should mention 'encrypted'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// F1 ephemeral: explicit pool contradiction + auto-select (create_vm path).
// ---------------------------------------------------------------------------

// TestStorageTierEncrypted_EphemeralExplicitPoolContradictsEncrypted verifies
// that create_vm with encrypted=true + ephemeral_storage_pool returns a
// non-retriable CloudError.
func TestStorageTierEncrypted_EphemeralExplicitPoolContradictsEncrypted(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := &config.CPIConfig{
		Node:          vmNode,
		VMStorage:     "plain-pool",
		NetworkBridge: "vmbr0",
		AgentMode:     "noagent",
		VMIDRangeStart: 100,
		StorageTiers: map[string]config.StorageTierCriteria{
			"enc-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
		},
	}

	qemuSvc := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "UPID:pve:create:ok", nil
		},
	}
	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:           qemuSvc,
			nodesSvc:          &vmMockNodes{},
			clusterSvc:        &vmMockCluster{},
			tasksSvc:          &mockTasksService{},
			storageSvc:        &mockStorageService{},
			clusterStorageSvc: cs,
		},
		Agent:  &vmMockAgent{},
		Logger: log.NewNopLogger(),
	}

	args := mkArgs(
		"agent-uuid-eph-pool-conflict",
		testStemcellCID,
		map[string]any{
			"cores":                  1,
			"memory":                 512,
			"ephemeral_disk_size_mb": 4096,
			"ephemeral_storage_pool": "some-unencrypted-pool",
			"encrypted":              true,
		},
		map[string]any{"default": map[string]any{
			"type":    "manual",
			"ip":      "10.0.1.7",
			"netmask": "255.255.255.0", "gateway": "10.0.1.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{},
		map[string]any{},
	)

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("eph-pool-conflict"))
	if err == nil {
		t.Fatal("expected error: ephemeral_storage_pool + encrypted=true is a contradiction")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("contradiction error must not be retriable")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should mention 'encrypted': %v", err.Error(), err)
	}
}

// TestStorageTierEncrypted_EphemeralAutoSelectNoTierNamed verifies that
// encrypted=true with no ephemeral_storage_tier and no ephemeral_storage_pool
// auto-selects the lex-first encrypted tier from config.
func TestStorageTierEncrypted_EphemeralAutoSelectNoTierNamed(t *testing.T) {
	t.Parallel()

	cs := &multiClusterStorage{entries: encryptedCSEntries()}
	cfg := &config.CPIConfig{
		Node:          vmNode,
		VMStorage:     "plain-pool",
		NetworkBridge: "vmbr0",
		AgentMode:     "noagent",
		VMIDRangeStart: 100,
		StorageTiers: map[string]config.StorageTierCriteria{
			"enc-tier": {Types: []string{"rbd"}, Encrypted: boolPtr(true)},
		},
	}

	var capturedStorages []string
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, _ int, _ string) (string, error) {
			capturedStorages = append(capturedStorages, storage)
			return storage + ":vm-9000-disk-0", nil
		},
	}
	qemuSvc := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "UPID:pve:create:ok", nil
		},
	}
	deps := handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc:           qemuSvc,
			nodesSvc:          &vmMockNodes{},
			clusterSvc:        &vmMockCluster{},
			tasksSvc:          &mockTasksService{},
			storageSvc:        storageSvc,
			clusterStorageSvc: cs,
		},
		Agent:  &vmMockAgent{},
		Logger: log.NewNopLogger(),
	}

	args := mkArgs(
		"agent-uuid-eph-auto",
		testStemcellCID,
		map[string]any{
			"cores":                  1,
			"memory":                 512,
			"ephemeral_disk_size_mb": 4096,
			// no ephemeral_storage_tier, no ephemeral_storage_pool
			"encrypted": true,
		},
		map[string]any{"default": map[string]any{
			"type":    "manual",
			"ip":      "10.0.1.8",
			"netmask": "255.255.255.0", "gateway": "10.0.1.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{},
		map[string]any{},
	)

	h := handlers.HandleCreateVM(deps)
	_, err := h.Handle(context.Background(), args, mkCtx("eph-auto"))
	if err != nil {
		t.Fatalf("HandleCreateVM ephemeral auto-select: unexpected error: %v", err)
	}

	encPoolUsed := false
	for _, s := range capturedStorages {
		if s == "enc-pool" {
			encPoolUsed = true
		}
	}
	if !encPoolUsed {
		t.Errorf("auto-select ephemeral: must use enc-pool; got storages: %v", capturedStorages)
	}
}
