// Package handlers -- internal tests for cloneFromTemplate's template-storage
// awareness (§1.3): a linked clone's overlay always lands on the TEMPLATE's
// own storage pool, never on vm_storage, so clone_mode decisions must be keyed
// off the template's storage, not vm_storage alone.
package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// buildTemplateQEMU returns an etQEMU fake whose Config reports virtio0
// pointing at templateStorage (mirroring the "<storage>:base-<vmid>-disk-0,..."
// shape a converted template carries). When configErr is non-nil, Config
// fails instead -- simulating an undeterminable template storage (the
// resolveTemplateDiskStorage fail-open path).
func buildTemplateQEMU(templateStorage string, templateVMID int, configErr error) *etQEMU {
	return &etQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			if configErr != nil {
				return nil, configErr
			}
			return map[string]any{
				diskKeyVirtio0: fmt.Sprintf("%s:base-%d-disk-0,size=5G", templateStorage, templateVMID),
			}, nil
		},
	}
}

// buildCloneDepsMultiStorage builds Deps with a multi-entry cluster storage
// index -- so vm_storage and the template's own storage can be looked up
// independently, each with its own type -- and a QEMU fake reporting the
// template's virtio0 volid (or failing, when templateConfigErr is set, to
// exercise the "undeterminable" fail-open path).
func buildCloneDepsMultiStorage(
	n *cloneNodes, cloneMode string, entries map[string]dlbStorageEntry,
	templateStorage string, templateVMID int, templateConfigErr error,
) Deps {
	cfg := &config.CPIConfig{Node: "pve", CloneMode: cloneMode}
	cfg.ApplyDefaults()
	cfg.CloneMode = cloneMode

	return Deps{
		Config: cfg,
		PVE: &cloneClient{
			etClient: etClient{
				nodes: n,
				qemu:  buildTemplateQEMU(templateStorage, templateVMID, templateConfigErr),
			},
			clusterStorageSvc: &dlbMultiStorageStub{entries: entries},
			clusterSvc:        &cloneClusterSvc{nodeCount: 1},
		},
		Logger: log.NewNopLogger(),
	}
}

// capturingLogger returns a *log.Logger that writes to buf, for tests
// asserting on Warn/Info message content.
func capturingLogger(t *testing.T, buf *bytes.Buffer) *log.Logger {
	t.Helper()
	l, err := log.NewLogger("info", buf)
	if err != nil {
		t.Fatalf("capturingLogger: NewLogger: %v", err)
	}
	return l
}

// ---------------------------------------------------------------------------
// Same pool as template: no behavior change.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_SamePool_AutoLinkedCapable verifies that when
// vm_storage IS the template's own pool and that pool supports linked clones,
// auto mode still produces a linked clone (no downgrade) -- the mismatch
// check must not fire on a genuine match.
func TestCloneFromTemplate_SamePool_AutoLinkedCapable(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"shared-pool": {storageType: "nfs", shared: true},
	}
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "shared-pool", 6042, nil)
	shape := buildCloneShape("shared-pool", "nfs", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 400, "vm-400", "pve", 6042)
	if err != nil {
		t.Fatalf("same pool, linked-capable: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full != nil {
		t.Errorf("same pool: Full must be nil (linked clone), got %v", *p.Full)
	}
	if p.Storage != nil {
		t.Errorf("same pool: Storage must be nil for linked clone, got %q", *p.Storage)
	}
}

// TestCloneFromTemplate_SamePool_ForcedLinkedSucceeds verifies forced
// clone_mode=linked succeeds (no error, no mutation blocked) when vm_storage
// is genuinely the template's own linked-capable pool.
func TestCloneFromTemplate_SamePool_ForcedLinkedSucceeds(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"shared-pool": {storageType: "nfs", shared: true},
	}
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "shared-pool", 6042, nil)
	shape := buildCloneShape("shared-pool", "nfs", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 401, "vm-401", "pve", 6042)
	if err != nil {
		t.Fatalf("same pool, forced linked: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	if n.calls[0].Full != nil {
		t.Error("same pool, forced linked: Full must be nil")
	}
}

// ---------------------------------------------------------------------------
// Different pools: the misplacement scenario §1.3 fixes.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_DifferentPools_Auto_DowngradesToFull is the primary
// acceptance scenario: stemcell_storage=cephfs-artifacts (the template's
// pool), vm_storage=rbd-fast. auto mode must produce a FULL clone with
// Storage=vm_storage (rbd-fast), not a linked clone silently placed on
// cephfs-artifacts.
func TestCloneFromTemplate_DifferentPools_Auto_DowngradesToFull(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"rbd-fast":         {storageType: "rbd", shared: true},
		"cephfs-artifacts": {storageType: "cephfs", shared: true},
	}
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "cephfs-artifacts", 7000, nil)
	shape := buildCloneShape("rbd-fast", "rbd", "qcow2")

	var buf bytes.Buffer
	err := cloneFromTemplate(context.Background(), deps, capturingLogger(t, &buf), shape, 500, "vm-500", "pve", 7000)
	if err != nil {
		t.Fatalf("different pools, auto: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Fatal("different pools, auto: Full must be &true (downgrade to full clone)")
	}
	if p.Storage == nil || *p.Storage != "rbd-fast" {
		t.Errorf("different pools, auto: Storage must be vm_storage %q, got %v", "rbd-fast", p.Storage)
	}
	if p.Format == nil || *p.Format != "qcow2" {
		t.Errorf("different pools, auto: Format must be set, got %v", p.Format)
	}
	logged := buf.String()
	if !strings.Contains(logged, "rbd-fast") || !strings.Contains(logged, "cephfs-artifacts") {
		t.Errorf("auto downgrade must be logged at Info naming both pools, got log: %s", logged)
	}
}

// TestCloneFromTemplate_DifferentPools_ForcedLinked_ErrorsBeforeAnyMutation
// verifies forced clone_mode=linked on mismatched pools fails with a clear,
// non-retriable CloudError BEFORE any PVE mutation (CreateQemuClone is never
// called), naming both pools and the two available fixes.
func TestCloneFromTemplate_DifferentPools_ForcedLinked_ErrorsBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"rbd-fast":         {storageType: "rbd", shared: true},
		"cephfs-artifacts": {storageType: "cephfs", shared: true},
	}
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "cephfs-artifacts", 7001, nil)
	shape := buildCloneShape("rbd-fast", "rbd", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 501, "vm-501", "pve", 7001)
	if err == nil {
		t.Fatal("different pools, forced linked: expected error, got nil")
	}
	if len(n.calls) != 0 {
		t.Fatalf("different pools, forced linked: CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
	msg := err.Error()
	for _, want := range []string{"rbd-fast", "cephfs-artifacts", "clone_mode: auto", "clone_mode: full"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing expected content %q: %s", want, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Capability keyed off the TEMPLATE's storage type, not vm_storage's.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_ForcedLinked_CapabilityKeyedOffTemplateStorage
// verifies that the linked-clone capability check for a SAME-pool forced
// clone re-resolves the pool's type fresh from the template, rather than
// trusting the precomputed shape.vmStorageType. shape.vmStorageType is
// deliberately set to "rbd" (linked-capable) here while the fake cluster
// storage index reports the actual pool as "lvm" (NOT linked-capable) -- if
// the implementation used shape.vmStorageType directly instead of
// re-resolving from templateStorage, this would incorrectly succeed.
func TestCloneFromTemplate_ForcedLinked_CapabilityKeyedOffTemplateStorage(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"shared-pool": {storageType: "lvm", shared: false},
	}
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "shared-pool", 6042, nil)
	shape := buildCloneShape("shared-pool", "rbd", "qcow2") // deliberately wrong/stale type

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 502, "vm-502", "pve", 6042)
	if err == nil {
		t.Fatal("expected error: the template's real pool type (lvm) does not support linked clones")
	}
	if len(n.calls) != 0 {
		t.Errorf("CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
	msg := err.Error()
	for _, want := range []string{"shared-pool", "lvm", "does not support linked clones"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing expected content %q: %s", want, msg)
		}
	}
}

// TestCloneFromTemplate_Auto_TemplateOnLVM_FullEvenWhenShapeTypeSaysRBD is the
// auto-mode counterpart: same deliberately-stale shape.vmStorageType ("rbd")
// on the SAME pool as the template, whose real type (per the fake cluster
// storage index) is "lvm". Auto mode must still choose a full clone, driven
// by the template's real (freshly-resolved) type, not the stale shape field.
func TestCloneFromTemplate_Auto_TemplateOnLVM_FullEvenWhenShapeTypeSaysRBD(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"shared-pool": {storageType: "lvm", shared: false},
	}
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "shared-pool", 6043, nil)
	shape := buildCloneShape("shared-pool", "rbd", "qcow2") // deliberately wrong/stale type

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 503, "vm-503", "pve", 6043)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Error("template's real pool type is lvm (not linked-capable): Full must be &true")
	}
}

// ---------------------------------------------------------------------------
// Template storage undeterminable: fail open to pre-1.3 behavior.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_TemplateStorageUndeterminable_Auto_FailsOpen verifies
// that when the template's config cannot be read, auto mode falls back to the
// pre-1.3 vm_storage-only capability check (no mismatch enforcement) and logs
// a Warn -- never a hard failure on missing facts.
func TestCloneFromTemplate_TemplateStorageUndeterminable_Auto_FailsOpen(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"rbd-fast": {storageType: "rbd", shared: true},
	}
	configErr := fmt.Errorf("simulated PVE API failure reading template config")
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "cephfs-artifacts", 7002, configErr)
	shape := buildCloneShape("rbd-fast", "rbd", "qcow2")

	var buf bytes.Buffer
	err := cloneFromTemplate(context.Background(), deps, capturingLogger(t, &buf), shape, 504, "vm-504", "pve", 7002)
	if err != nil {
		t.Fatalf("undeterminable template storage must fail open, not error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	// Pre-1.3 behavior: rbd (vm_storage's own type) supports linked clones,
	// and there is no mismatch check to override that when undeterminable.
	if p.Full != nil {
		t.Errorf("undeterminable template storage: expected linked clone (pre-1.3 behavior), got Full=%v", *p.Full)
	}
	if !strings.Contains(buf.String(), "could not determine template's storage pool") {
		t.Errorf("expected a Warn naming the undeterminable condition, got log: %s", buf.String())
	}
}

// TestCloneFromTemplate_TemplateStorageUndeterminable_ForcedLinked_FailsOpen
// verifies the same fail-open contract for forced clone_mode=linked: when the
// template's storage cannot be determined, the capability check falls back to
// vm_storage's own type (pre-1.3 behavior) and a linked-capable vm_storage
// still succeeds.
func TestCloneFromTemplate_TemplateStorageUndeterminable_ForcedLinked_FailsOpen(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"rbd-fast": {storageType: "rbd", shared: true},
	}
	configErr := fmt.Errorf("simulated PVE API failure reading template config")
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "cephfs-artifacts", 7003, configErr)
	shape := buildCloneShape("rbd-fast", "rbd", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 505, "vm-505", "pve", 7003)
	if err != nil {
		t.Fatalf("undeterminable template storage must fail open to vm_storage capability check, got error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	if n.calls[0].Full != nil {
		t.Error("undeterminable template storage: expected linked clone (vm_storage=rbd is linked-capable)")
	}
}

// TestCloneFromTemplate_TemplateStorageUndeterminable_ForcedLinked_StillRejectsIncapableVMStorage
// verifies the fail-open path still enforces the original (pre-1.3) capability
// error when vm_storage itself does not support linked clones and the
// template's storage cannot be determined.
func TestCloneFromTemplate_TemplateStorageUndeterminable_ForcedLinked_StillRejectsIncapableVMStorage(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"local-lvm": {storageType: "lvm", shared: false},
	}
	configErr := fmt.Errorf("simulated PVE API failure reading template config")
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "unknown-template-pool", 7004, configErr)
	shape := buildCloneShape("local-lvm", "lvm", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 506, "vm-506", "pve", 7004)
	if err == nil {
		t.Fatal("expected error: vm_storage (lvm) does not support linked clones, and template storage is undeterminable")
	}
	if len(n.calls) != 0 {
		t.Errorf("CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
	if !strings.Contains(err.Error(), "local-lvm") {
		t.Errorf("error should reference vm_storage (the fail-open capability check target): %v", err)
	}
}
