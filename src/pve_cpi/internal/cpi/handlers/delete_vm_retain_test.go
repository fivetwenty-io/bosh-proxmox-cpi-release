package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
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
// Retention must reassign the physical volume and verify its receiving parker.
func TestDetachRetainedEphemeralDisk_Happy(t *testing.T) {
	deps, client, volume := legacyRetainFlowFixture(t)
	retained, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger())
	if err != nil || !retained {
		t.Fatalf("retention failed: %v", err)
	}
	if client.moves != 1 || len(client.state.volumes) != 1 || client.state.volumes[volume] != nil {
		t.Fatal("ephemeral was not physically preserved by reassignment")
	}
	for _, info := range client.state.volumes {
		if info.Size != 5<<30 {
			t.Fatal("retention lost volume contents/size")
		}
	}
	if _, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if client.moves != 1 {
		t.Fatal("retry duplicated retention transfer")
	}
}

// ---------------------------------------------------------------------------
// Missing source evidence prevents retention mutations.
func TestDetachRetainedEphemeralDisk_UnlinkFails(t *testing.T) {
	deps, client, volume := legacyRetainFlowFixture(t)
	delete(client.state.volumes, volume)
	if _, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger()); err == nil {
		t.Fatal("missing physical source was accepted")
	}
	if client.moves != 0 {
		t.Fatal("missing volume triggered mutation")
	}
}

// ---------------------------------------------------------------------------
// Interrupted retention keeps the original volume and resumes its durable intent.
func TestDetachRetainedEphemeralDisk_SweepFails(t *testing.T) {
	deps, client, volume := legacyRetainFlowFixture(t)
	client.moveErr = errors.New("transfer refused")
	if _, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger()); err == nil {
		t.Fatal("failed retention transfer reported success")
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("failed transfer swept the VM-owned volume")
	}
	client.moveErr = nil
	if _, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	if client.moves != 1 || len(client.state.volumes) != 1 {
		t.Fatal("interrupted retention did not resume exactly once")
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

	// Nil retain_on_delete → no opts key added.
	withNil := mustEncodeDiskCID(t, &pve.DiskCIDMeta{
		Pool: "local-lvm",
		Node: "pve1",
		Opts: nil,
	})
	// Simulate what HandleCreateDisk produces when RetainOnDelete is nil:
	// diskPerfOpts is nil → EncodeDiskCID gets nil Opts → omitempty omits.
	withoutFlag := mustEncodeDiskCID(t, &pve.DiskCIDMeta{
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

	opts := map[string]string{
		diskOptRetainOnDelete: "1",
	}

	encoded := mustEncodeDiskCID(t, &pve.DiskCIDMeta{
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
// createVMCloudProps.RetainEphemeralOnDelete → initialTags is covered by a
// real handler-driven test: TestCreateVM_RetainEphemeralOnDelete_TagOnCreate
// in create_vm_retain_ephemeral_test.go (package handlers_test), which drives
// HandleCreateVM end to end and asserts the tag on the captured create call.
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

func legacyRetainFlowFixture(t *testing.T) (Deps, *lifecycleFlowPVE, string) {
	t.Helper()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:vm-777-ephemeral-0"
	client.state.volumes[volume] = client.state.volumes[original]
	delete(client.state.volumes, original)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	client.state.configs[777]["tags"] = tagRetainEphemeral
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	return deps, client, volume
}
