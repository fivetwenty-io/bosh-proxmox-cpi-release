// Package handlers -- internal tests for backing-identity awareness in the
// clone path: the storageMismatch-by-backing fix (Kevin's "two names, one
// export" trap) and the K3 Target-validation-direction fix (the shared-
// storage-allows-Target rule must consult the TEMPLATE's storage, not the
// destination vm_storage).
package handlers

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// twoIDsOneExportEntries returns the canonical fixture used across these
// tests: two distinct PVE storage IDs ("nfs-a", "nfs-b") configured against
// the identical NFS server+export, so BackingKey() reports them as the same
// physical backing despite the different IDs.
func twoIDsOneExportEntries() map[string]dlbStorageEntry {
	return map[string]dlbStorageEntry{
		"nfs-a": {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"nfs-b": {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
	}
}

// buildCloneDepsMultiStorageWithTopology extends buildCloneDepsMultiStorage
// (clone_from_template_storage_internal_test.go) with a configurable cluster
// node count, needed for the K3 cross-node regression below (the existing
// helper hardcodes nodeCount=1, which makes templateNode != shape.node
// meaningless for the multi-node Target enforcement path).
func buildCloneDepsMultiStorageWithTopology(
	n *cloneNodes, cloneMode string, entries map[string]dlbStorageEntry,
	templateStorage string, templateVMID int, templateConfigErr error, nodeCount int,
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
			clusterSvc:        &cloneClusterSvc{nodeCount: nodeCount},
		},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// storageMismatch by backing (Kevin's trap): two IDs, one physical export.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_TwoIDsOneExport_Auto_StaysLinked verifies clone_mode:
// auto does NOT downgrade to a full clone when the template's storage and
// vm_storage are different PVE storage IDs that share one physical NFS
// export: the storageMismatch check must recognize the shared backing and
// keep the linked clone.
func TestCloneFromTemplate_TwoIDsOneExport_Auto_StaysLinked(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := twoIDsOneExportEntries()
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "nfs-a", 7100, nil)
	shape := buildCloneShape("nfs-b", "nfs", "qcow2")

	var buf bytes.Buffer
	err := cloneFromTemplate(context.Background(), deps, capturingLogger(t, &buf), shape, 600, "vm-600", "pve", 7100)
	if err != nil {
		t.Fatalf("two IDs, one export, auto: unexpected error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	p := n.calls[0]
	if p.Full != nil {
		t.Errorf("two IDs, one export: Full must be nil (linked clone, no downgrade), got %v", *p.Full)
	}
	if p.Storage != nil {
		t.Errorf("two IDs, one export: Storage must be nil for linked clone, got %q", *p.Storage)
	}
	logged := buf.String()
	if !strings.Contains(logged, "nfs-a") || !strings.Contains(logged, "nfs-b") {
		t.Errorf("expected an Info log naming both storage IDs sharing one backing, got: %s", logged)
	}
	if strings.Contains(logged, "downgrading linked clone to full clone") {
		t.Errorf("must NOT log a downgrade: same backing is not a mismatch, got: %s", logged)
	}
}

// TestCloneFromTemplate_TwoIDsOneExport_ForcedLinked_Succeeds verifies
// clone_mode: linked is accepted (not rejected as a mismatch) when the two
// storage IDs share one physical export.
func TestCloneFromTemplate_TwoIDsOneExport_ForcedLinked_Succeeds(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := twoIDsOneExportEntries()
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "nfs-a", 7101, nil)
	shape := buildCloneShape("nfs-b", "nfs", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 601, "vm-601", "pve", 7101)
	if err != nil {
		t.Fatalf("two IDs, one export, forced linked: expected success, got error: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatalf("expected 1 CreateQemuClone call, got %d", len(n.calls))
	}
	if n.calls[0].Full != nil {
		t.Error("two IDs, one export, forced linked: Full must be nil (not rejected as a mismatch)")
	}
}

// TestCloneFromTemplate_TwoIDsOneExport_DistinctBacking_Auto_StillDowngrades
// is the (b) half of the required fixture proof: when the two storage IDs
// genuinely do NOT share a backing (different NFS exports), auto mode must
// still downgrade to a full clone exactly as before this fix — distinct-
// backing behavior is unchanged.
func TestCloneFromTemplate_TwoIDsOneExport_DistinctBacking_Auto_StillDowngrades(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"nfs-a": {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"nfs-c": {storageType: "nfs", shared: true, server: "10.0.0.9", export: "/tank/other"},
	}
	deps := buildCloneDepsMultiStorage(n, "auto", entries, "nfs-a", 7102, nil)
	shape := buildCloneShape("nfs-c", "nfs", "qcow2")

	var buf bytes.Buffer
	err := cloneFromTemplate(context.Background(), deps, capturingLogger(t, &buf), shape, 602, "vm-602", "pve", 7102)
	if err != nil {
		t.Fatalf("distinct backing, auto: unexpected error: %v", err)
	}
	p := n.calls[0]
	if p.Full == nil || !*p.Full {
		t.Fatal("distinct backing, auto: Full must be &true (genuine mismatch, downgrade preserved)")
	}
	if p.Storage == nil || *p.Storage != "nfs-c" {
		t.Errorf("distinct backing, auto: Storage must be vm_storage %q, got %v", "nfs-c", p.Storage)
	}
	if !strings.Contains(buf.String(), "downgrading linked clone to full clone") {
		t.Errorf("expected the downgrade Info log, got: %s", buf.String())
	}
}

// TestCloneFromTemplate_TwoIDsOneExport_DistinctBacking_ForcedLinked_StillErrors
// is the distinct-backing counterpart for clone_mode: linked -- must still be
// rejected exactly as before this fix.
func TestCloneFromTemplate_TwoIDsOneExport_DistinctBacking_ForcedLinked_StillErrors(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"nfs-a": {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"nfs-c": {storageType: "nfs", shared: true, server: "10.0.0.9", export: "/tank/other"},
	}
	deps := buildCloneDepsMultiStorage(n, "linked", entries, "nfs-a", 7103, nil)
	shape := buildCloneShape("nfs-c", "nfs", "qcow2")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 603, "vm-603", "pve", 7103)
	if err == nil {
		t.Fatal("distinct backing, forced linked: expected error, got nil")
	}
	if len(n.calls) != 0 {
		t.Errorf("distinct backing, forced linked: CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
}

// ---------------------------------------------------------------------------
// K3: cross-node Target validation must consult the TEMPLATE's storage.
// ---------------------------------------------------------------------------

// TestCloneFromTemplate_K3_TemplateLocal_VMStorageShared_TargetRejected is the
// primary K3 regression: the template lives on LOCAL storage while
// vm_storage is a DIFFERENT, SHARED storage. Before the fix this checked
// vm_storage's shared-ness (true) and would have set Target on a clone whose
// source is local -- exactly the configuration PVE itself rejects (Target
// requires the ORIGINAL VM, i.e. the template, to be on shared storage).
// After the fix, the pre-flight check consults the template's own storage
// (local) and rejects with an actionable cross-node error before any PVE
// mutation.
func TestCloneFromTemplate_K3_TemplateLocal_VMStorageShared_TargetRejected(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"tmpl-local": {storageType: "dir", shared: false},
		"vm-shared":  {storageType: "nfs", shared: true},
	}
	// Multi-node cluster; template built on "pve01", VM wants "pve02" — a
	// genuine cross-node clone.
	deps := buildCloneDepsMultiStorageWithTopology(n, "auto", entries, "tmpl-local", 7200, nil, 2)
	shape := buildCloneShapeWithNode("vm-shared", "nfs", "qcow2", "pve02")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 700, "vm-700", "pve01", 7200)
	if err == nil {
		t.Fatal("K3: expected cross-node rejection (template is on local storage), got nil")
	}
	if len(n.calls) != 0 {
		t.Errorf("K3: CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
	msg := err.Error()
	if !strings.Contains(msg, "tmpl-local") {
		t.Errorf("K3: error must name the TEMPLATE's storage (tmpl-local), not just vm_storage: %v", err)
	}
	if !strings.Contains(msg, "local") {
		t.Errorf("K3: error must state the local-storage constraint: %v", err)
	}
}

// TestCloneFromTemplate_K3_TemplateShared_VMStorageLocal_Rejected is the
// mirror case: the template lives on SHARED storage while vm_storage is a
// DIFFERENT, LOCAL storage. An earlier revision expected Target to be set
// here on the theory that only the ORIGINAL/template VM's storage must be
// shared — live PVE disproved that: the clone POST fails with "can't clone
// to non-shared storage '<vm_storage>'", because PVE also requires the
// DESTINATION storage of a cross-node clone to be shared. The pre-flight
// must reject with the replica remedy instead of letting PVE burn the VMID.
func TestCloneFromTemplate_K3_TemplateShared_VMStorageLocal_Rejected(t *testing.T) {
	t.Parallel()
	n := &cloneNodes{}
	entries := map[string]dlbStorageEntry{
		"tmpl-shared": {storageType: "nfs", shared: true},
		"vm-local":    {storageType: "dir", shared: false},
	}
	deps := buildCloneDepsMultiStorageWithTopology(n, "auto", entries, "tmpl-shared", 7201, nil, 2)
	shape := buildCloneShapeWithNode("vm-local", "dir", "qcow2", "pve02")

	err := cloneFromTemplate(context.Background(), deps, log.NewNopLogger(), shape, 701, "vm-701", "pve01", 7201)
	if err == nil {
		t.Fatal("K3 mirror: expected rejection — PVE cannot write a cross-node clone's disks to local storage")
	}
	if len(n.calls) != 0 {
		t.Fatalf("K3 mirror: CreateQemuClone must not be called, got %d calls", len(n.calls))
	}
	msg := err.Error()
	if !strings.Contains(msg, "vm-local") {
		t.Errorf("K3 mirror: error must name the destination storage (vm-local): %v", err)
	}
	if !strings.Contains(msg, "stemcell_replicate_local") {
		t.Errorf("K3 mirror: error must offer the replica remedy: %v", err)
	}
}
