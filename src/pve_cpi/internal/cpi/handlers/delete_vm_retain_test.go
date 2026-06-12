package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// Minimal fakes for retain-ephemeral tests.
// ---------------------------------------------------------------------------

// retainQEMU provides a configurable Config sequence and records no calls to
// other QEMU methods (they panic on accidental invocation).
type retainQEMU struct {
	sdkqemu.Service
	configCalls int
	configs     []map[string]any
}

func (q *retainQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	if q.configCalls < len(q.configs) {
		cfg := q.configs[q.configCalls]
		q.configCalls++
		return cfg, nil
	}
	q.configCalls++
	return map[string]any{}, nil
}

// retainNodes intercepts UpdateQemuUnlink and UpdateQemuConfig; all other
// methods panic to catch unintended calls.
type retainNodes struct {
	sdknodes.Service
	unlinkCalls  []sdknodes.UpdateQemuUnlinkParams
	configCalls  []string // Delete values passed to UpdateQemuConfig
	unlinkErr    error
	configDelErr error
}

func (n *retainNodes) UpdateQemuUnlink(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuUnlinkParams) error {
	if params != nil {
		n.unlinkCalls = append(n.unlinkCalls, *params)
	}
	return n.unlinkErr
}

func (n *retainNodes) UpdateQemuConfig(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	if params != nil && params.Delete != nil {
		n.configCalls = append(n.configCalls, *params.Delete)
	}
	return n.configDelErr
}

type retainClient struct {
	qemu  sdkqemu.Service
	nodes sdknodes.Service
}

func (c *retainClient) QEMU() sdkqemu.Service                     { return c.qemu }
func (c *retainClient) Nodes() sdknodes.Service                   { return c.nodes }
func (c *retainClient) Storage() sdkstorage.Service               { return nil }
func (c *retainClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *retainClient) Tasks() sdktasks.Service                   { return nil }
func (c *retainClient) Cluster() sdkcluster.Service               { return nil }
func (c *retainClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *retainClient) Pools() pve.PoolService                    { return nil }

func retainDeps(qemu sdkqemu.Service, nodes sdknodes.Service) Deps {
	return Deps{
		Config: &config.CPIConfig{
			Node:        "pve-node1",
			DiskStorage: "zfs-1",
		},
		PVE:    &retainClient{qemu: qemu, nodes: nodes},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// TestFindEphemeralActiveDisks: basic slot discovery.
// ---------------------------------------------------------------------------

func TestFindEphemeralActiveDisks_Found(t *testing.T) {
	t.Parallel()

	const vmid = 101
	cfg := map[string]any{
		"virtio0": "zfs-1:vm-101-disk-0",
		"scsi1":   "zfs-1:vm-101-ephemeral-0,size=10G",
		"scsi2":   "zfs-1:vm-9999-disk-0", // foreign persistent disk — NOT ephemeral
	}

	result := findEphemeralActiveDisks(cfg, vmid)
	if len(result) != 1 {
		t.Fatalf("want 1 ephemeral slot, got %d: %v", len(result), result)
	}
	if volid, ok := result["scsi1"]; !ok || volid != "zfs-1:vm-101-ephemeral-0" {
		t.Errorf("want scsi1=zfs-1:vm-101-ephemeral-0, got %v", result)
	}
}

func TestFindEphemeralActiveDisks_NonePresent(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{
		"virtio0": "zfs-1:vm-101-disk-0",
		"scsi1":   "zfs-1:vm-9999-disk-0",
	}

	result := findEphemeralActiveDisks(cfg, 101)
	if len(result) != 0 {
		t.Errorf("expected no ephemeral slots, got %v", result)
	}
}

func TestFindEphemeralActiveDisks_WrongOwner(t *testing.T) {
	t.Parallel()

	// Ephemeral volid with VMID 200, but owner is 101 — must NOT match.
	cfg := map[string]any{
		"scsi1": "zfs-1:vm-200-ephemeral-0",
	}

	result := findEphemeralActiveDisks(cfg, 101)
	if len(result) != 0 {
		t.Errorf("expected no match for wrong owner VMID, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// TestDetachRetainedEphemeralDisk_NoFlag: when the tag is absent, the function
// makes exactly one Config read and no unlink or config-delete calls.
// ---------------------------------------------------------------------------

func TestDetachRetainedEphemeralDisk_NoFlag(t *testing.T) {
	t.Parallel()

	// Config has no bosh-retain-ephemeral tag.
	cfg := map[string]any{
		"virtio0":   "zfs-1:vm-101-disk-0",
		"scsi1":     "zfs-1:vm-101-ephemeral-0,size=10G",
		jsonKeyTags: "bosh-cpi", // no bosh-retain-ephemeral
	}

	q := &retainQEMU{configs: []map[string]any{cfg}}
	n := &retainNodes{}

	retained, err := detachRetainedEphemeralDisk(context.Background(), retainDeps(q, n), "pve-node1", "101", 101, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if retained {
		t.Error("retained must be false when tag is absent")
	}
	if len(n.unlinkCalls) != 0 {
		t.Errorf("expected no Unlink calls, got %d", len(n.unlinkCalls))
	}
	if len(n.configCalls) != 0 {
		t.Errorf("expected no UpdateQemuConfig Delete calls, got %d", len(n.configCalls))
	}
}

// ---------------------------------------------------------------------------
// TestDetachRetainedEphemeralDisk_Happy: tag present, ephemeral slot found,
// unlink demotes to unusedN, sweep removes the unusedN config entry, returned
// retained=true signals the caller to set DestroyUnreferencedDisks=false.
//
// Two-read sequence models PVE's demotion behaviour:
//   Read 1: scsi1 = ephemeral volid + bosh-retain-ephemeral tag.
//   Unlink: recorded; scsi1 becomes unused0.
//   Read 2: config has unused0 = ephemeral volid; scsi1 is gone.
//   Config Delete: "unused0" removed from config; storage untouched.
// ---------------------------------------------------------------------------

func TestDetachRetainedEphemeralDisk_Happy(t *testing.T) {
	t.Parallel()

	const vmid = 101
	const ephemeralVolid = "zfs-1:vm-101-ephemeral-0"
	const ephemeralOptStr = "zfs-1:vm-101-ephemeral-0,size=10G"

	initCfg := map[string]any{
		"virtio0":   "zfs-1:vm-101-disk-0",
		"scsi1":     ephemeralOptStr,
		jsonKeyTags: "bosh-cpi;bosh-retain-ephemeral",
	}
	postUnlinkCfg := map[string]any{
		"virtio0": "zfs-1:vm-101-disk-0",
		"unused0": ephemeralVolid,
	}

	q := &retainQEMU{configs: []map[string]any{initCfg, postUnlinkCfg}}
	n := &retainNodes{}

	retained, err := detachRetainedEphemeralDisk(context.Background(), retainDeps(q, n), "pve-node1", "101", vmid, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if !retained {
		t.Error("retained must be true when ephemeral disk was unlinked and swept")
	}

	if len(n.unlinkCalls) != 1 {
		t.Fatalf("want 1 Unlink call, got %d", len(n.unlinkCalls))
	}
	if n.unlinkCalls[0].Idlist != "scsi1" {
		t.Errorf("Unlink: want Idlist=%q, got %q", "scsi1", n.unlinkCalls[0].Idlist)
	}
	if n.unlinkCalls[0].Force != nil && *n.unlinkCalls[0].Force {
		t.Error("Unlink: Force must be nil/false; true would destroy the volume")
	}

	if len(n.configCalls) != 1 {
		t.Fatalf("want 1 UpdateQemuConfig Delete call, got %d", len(n.configCalls))
	}
	if n.configCalls[0] != "unused0" {
		t.Errorf("UpdateQemuConfig Delete: want %q, got %q", "unused0", n.configCalls[0])
	}
}

// ---------------------------------------------------------------------------
// TestDetachRetainedEphemeralDisk_UnlinkFails: Unlink returns error → retriable.
// ---------------------------------------------------------------------------

func TestDetachRetainedEphemeralDisk_UnlinkFails(t *testing.T) {
	t.Parallel()

	const vmid = 101
	initCfg := map[string]any{
		"scsi1":     "zfs-1:vm-101-ephemeral-0,size=10G",
		jsonKeyTags: "bosh-retain-ephemeral",
	}

	q := &retainQEMU{configs: []map[string]any{initCfg}}
	n := &retainNodes{unlinkErr: errRetainTest("pve: lock timeout")}

	_, err := detachRetainedEphemeralDisk(context.Background(), retainDeps(q, n), "pve-node1", "101", vmid, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when Unlink fails, got nil")
	}
	type retriableChecker interface{ OkToRetry() bool }
	if rc, ok := err.(retriableChecker); !ok || !rc.OkToRetry() {
		t.Errorf("error must be retriable; got: %v (type %T)", err, err)
	}
	// No sweep call must have been made.
	if len(n.configCalls) != 0 {
		t.Errorf("expected no sweep calls after Unlink failure, got %d", len(n.configCalls))
	}
}

// ---------------------------------------------------------------------------
// TestDetachRetainedEphemeralDisk_SweepFails: Unlink ok, sweep returns error → retriable.
// ---------------------------------------------------------------------------

func TestDetachRetainedEphemeralDisk_SweepFails(t *testing.T) {
	t.Parallel()

	const vmid = 101
	initCfg := map[string]any{
		"scsi1":     "zfs-1:vm-101-ephemeral-0,size=10G",
		jsonKeyTags: "bosh-retain-ephemeral",
	}
	postUnlinkCfg := map[string]any{
		"unused0": "zfs-1:vm-101-ephemeral-0",
	}

	q := &retainQEMU{configs: []map[string]any{initCfg, postUnlinkCfg}}
	n := &retainNodes{configDelErr: errRetainTest("pve: storage lock")}

	_, err := detachRetainedEphemeralDisk(context.Background(), retainDeps(q, n), "pve-node1", "101", vmid, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when sweep fails, got nil")
	}
	type retriableChecker interface{ OkToRetry() bool }
	if rc, ok := err.(retriableChecker); !ok || !rc.OkToRetry() {
		t.Errorf("error must be retriable; got: %v (type %T)", err, err)
	}
}

// ---------------------------------------------------------------------------
// TestDetachRetainedEphemeralDisk_NoEphemeralSlot: tag set but no active
// ephemeral slot. A prior attempt may already have unlinked+swept the disk,
// leaving the volume unreferenced with a matching VMID — exactly what
// DestroyUnreferencedDisks=true frees. Tag presence therefore forces
// retained=true even with no slot, so a retried delete (or the straggler
// sweep) cannot destroy the volume the first attempt preserved.
// ---------------------------------------------------------------------------

func TestDetachRetainedEphemeralDisk_NoEphemeralSlot(t *testing.T) {
	t.Parallel()

	// Config has the tag but no scsiN containing "-ephemeral-".
	cfg := map[string]any{
		"virtio0":   "zfs-1:vm-101-disk-0",
		jsonKeyTags: "bosh-retain-ephemeral",
	}

	q := &retainQEMU{configs: []map[string]any{cfg}}
	n := &retainNodes{}

	retained, err := detachRetainedEphemeralDisk(context.Background(), retainDeps(q, n), "pve-node1", "101", 101, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error when no ephemeral slot found, got: %v", err)
	}
	if !retained {
		t.Error("retained must be true when tag is present, even with no active slot (re-entry safety: a prior attempt may have already unlinked the disk)")
	}
	if len(n.unlinkCalls) != 0 {
		t.Errorf("expected no Unlink calls, got %d", len(n.unlinkCalls))
	}
}

// ---------------------------------------------------------------------------
// TestCreateDiskCloudProperties_RetainOnDelete_OmitEmpty: nil RetainOnDelete
// produces a CID byte-identical to one without the field.
// ---------------------------------------------------------------------------

func TestCreateDiskCloudProperties_RetainOnDelete_OmitEmpty(t *testing.T) {
	t.Parallel()

	bareCID := "local-lvm:vm-9001-disk-0"

	// Nil retain_on_delete → no opts key added.
	withNil := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{
		Pool: "local-lvm",
		Node: "pve1",
		Opts: nil,
	})
	// Simulate what HandleCreateDisk produces when RetainOnDelete is nil:
	// diskPerfOpts is nil → EncodeDiskCID gets nil Opts → omitempty omits.
	withoutFlag := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{
		Pool: "local-lvm",
		Node: "pve1",
	})

	if withNil != withoutFlag {
		t.Errorf("nil RetainOnDelete must produce identical CID:\n  nil    = %q\n  absent = %q", withNil, withoutFlag)
	}
}

// TestCreateDiskCloudProperties_RetainOnDelete_EncodedInOpts: *true encodes
// retain_on_delete:1 into DiskCIDMeta.Opts, readable by ParseEncodedDiskCID.
func TestCreateDiskCloudProperties_RetainOnDelete_EncodedInOpts(t *testing.T) {
	t.Parallel()

	bareCID := "local-lvm:vm-9001-disk-0"
	opts := map[string]string{
		diskOptRetainOnDelete: "1",
	}

	encoded := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{
		Pool: "local-lvm",
		Node: "pve1",
		Opts: opts,
	})

	_, meta, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("ParseEncodedDiskCID: %v", err)
	}
	if meta == nil {
		t.Fatal("meta is nil after encoding with retain_on_delete opt")
	}
	if meta.Opts[diskOptRetainOnDelete] != "1" {
		t.Errorf("meta.Opts[%q] = %q; want %q", diskOptRetainOnDelete, meta.Opts[diskOptRetainOnDelete], "1")
	}
}

// ---------------------------------------------------------------------------
// TestRetainEphemeralTagInCreateVM: createVMCloudProps.RetainEphemeralOnDelete
// flows into initialTags when *true (struct-level contract test).
// ---------------------------------------------------------------------------

func TestRetainEphemeralTagInCreateVM_True(t *testing.T) {
	t.Parallel()

	b := true
	cp := createVMCloudProps{RetainEphemeralOnDelete: &b}
	if cp.RetainEphemeralOnDelete == nil || !*cp.RetainEphemeralOnDelete {
		t.Error("RetainEphemeralOnDelete *true not preserved after assignment")
	}

	// Simulate the initialTags construction logic from createVM.
	baseRetainTags := buildCustomTags(cp.Tags)
	if cp.RetainEphemeralOnDelete != nil && *cp.RetainEphemeralOnDelete {
		baseRetainTags = append(baseRetainTags, tagRetainEphemeral)
	}
	tags := mergeTagList([]string{"bosh-cpi"}, baseRetainTags, maxTagLength)

	if !strings.Contains(tags, tagRetainEphemeral) {
		t.Errorf("initialTags must contain %q when RetainEphemeralOnDelete is true; got %q", tagRetainEphemeral, tags)
	}
}

func TestRetainEphemeralTagInCreateVM_Nil(t *testing.T) {
	t.Parallel()

	cp := createVMCloudProps{RetainEphemeralOnDelete: nil}

	baseRetainTags := buildCustomTags(cp.Tags)
	if cp.RetainEphemeralOnDelete != nil && *cp.RetainEphemeralOnDelete {
		baseRetainTags = append(baseRetainTags, tagRetainEphemeral)
	}
	tags := mergeTagList([]string{"bosh-cpi"}, baseRetainTags, maxTagLength)

	if strings.Contains(tags, tagRetainEphemeral) {
		t.Errorf("initialTags must NOT contain %q when RetainEphemeralOnDelete is nil; got %q", tagRetainEphemeral, tags)
	}
}

// ---------------------------------------------------------------------------
// TestForeignDiskAlreadyPreserved: a disk with retain_on_delete in opts AND
// a foreign VMID is already preserved by detachForeignActiveDisks. This test
// confirms the foreign-disk guard fires for it (no second unlink pass needed).
// ---------------------------------------------------------------------------

func TestForeignDiskAlreadyPreserved_ForeignGuardFires(t *testing.T) {
	t.Parallel()

	// VM 100 has a foreign disk (VMID 9999, as created by create_disk).
	// The disk CID carries retain_on_delete in Opts but this is stored in
	// BOSH Director state; only the bare PVE volid is in the VM config.
	initCfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
		"scsi1":   "zfs-1:vm-9999-disk-0,size=64G", // foreign VMID → detachForeignActiveDisks fires
	}
	// After detach, scsi1 is gone (DetachDisk swept the unusedN entry).
	postCfg := map[string]any{
		"virtio0": "zfs-1:vm-100-disk-0",
	}

	var detachSlots []string
	q := &fdQEMU{
		configs: []map[string]any{initCfg, postCfg},
		detachFn: func(_ context.Context, _ string, _ int, slot string) error {
			detachSlots = append(detachSlots, slot)
			return nil
		},
	}

	err := detachForeignActiveDisks(context.Background(), fdDeps(q), "pve-node1", "100", 100, log.NewNopLogger())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(detachSlots) != 1 || detachSlots[0] != "scsi1" {
		t.Errorf("DetachDisk: want [scsi1], got %v", detachSlots)
	}
	// Volume is preserved because DetachDisk was called (not DeleteQemu).
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// errRetainTest is a plain error for retain-path test cases.
type errRetainTest string

func (e errRetainTest) Error() string { return string(e) }
