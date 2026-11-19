package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

// The tests in this file swap the process-wide emptied-parker retirement to
// see which parker each emptying path hands it, so none of them may call
// t.Parallel.

// retiredParker is one call of the emptied-parker retirement.
type retiredParker struct {
	node    string
	vmid    int
	trigger string
}

// captureRetirements replaces the retirement with a recorder for the rest of
// the test and returns the calls it saw.
func captureRetirements(t *testing.T) func() []retiredParker {
	t.Helper()
	var mu sync.Mutex
	var seen []retiredParker
	restore := setRetireEmptiedParkerForTest(func(_ context.Context, _ Deps, node string, vmid int, trigger string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, retiredParker{node: node, vmid: vmid, trigger: trigger})
	})
	t.Cleanup(restore)
	return func() []retiredParker {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func requireRetirements(t *testing.T, got []retiredParker, want ...retiredParker) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("retirements %+v, want %+v", got, want)
	}
}

// TestRetireEmptiedParker_AttachMigrationRetiresTheSourceParker proves the
// attach that migrates a disk off a parker of another node hands that parker
// to the retirement, and never the mover, which the attach destroys itself.
func TestRetireEmptiedParker_AttachMigrationRetiresTheSourceParker(t *testing.T) {
	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
			"scsi0":      "data:vm-90000-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90000: "pve1"})
	deps := migTestDeps(c)
	holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0",
		Tags: "bosh-cpi;bosh-parker"}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90000-disk-0",
		meta: &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}, stableID: migTestToken, holder: &holder,
	}
	calls := captureRetirements(t)

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil || !plan.destroyMover {
		t.Fatalf("want the mover plan; plan %+v err %v", plan, err)
	}
	requireRetirements(t, calls())
	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd,
		plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	requireRetirements(t, calls(), retiredParker{node: "pve1", vmid: 90000, trigger: "attach_disk"})
}

// TestRetireEmptiedParker_AttachUnparkRetiresTheParker proves the attach that
// unparks a shared birth-named disk from a parker of another node hands that
// parker to the retirement once the attach has run.
func TestRetireEmptiedParker_AttachUnparkRetiresTheParker(t *testing.T) {
	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
			"scsi0":      "data:vm-9001-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90000: "pve1"})
	deps := migTestDeps(c)
	deps.Resolver = &migFakeBackendResolver{kind: pve.BackendShared, node: "pve1"}
	holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0",
		Tags: "bosh-cpi;bosh-parker"}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-9001-disk-0",
		meta: &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}, stableID: migTestToken, holder: &holder,
	}
	calls := captureRetirements(t)

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil || plan.viaTransfer {
		t.Fatalf("want the config-edit plan; plan %+v err %v", plan, err)
	}
	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd,
		plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	requireRetirements(t, calls(), retiredParker{node: "pve1", vmid: 90000, trigger: "attach_disk"})
}

// TestRetireEmptiedParker_ManagedPathsRetireTheParker proves the attach that
// reassigns a journal-managed disk off a parker on the VM's node, the
// delete_disk of a parked managed disk, and the explicit cleanup of one each
// hand the parker to the retirement.
func TestRetireEmptiedParker_ManagedPathsRetireTheParker(t *testing.T) {
	t.Run("attach reassignment", func(t *testing.T) {
		d := newParkedFlowDisk(t, newLockContention(t))
		const node = "n1"
		calls := captureRetirements(t)
		if err := d.attach(t.Context()); err != nil {
			t.Fatalf("attach: %v", err)
		}
		requireRetirements(t, calls(), retiredParker{node: node, vmid: d.parker, trigger: "attach_disk"})
	})
	t.Run("delete_disk", func(t *testing.T) {
		d := newParkedFlowDisk(t, newLockContention(t))
		const node = "n1"
		calls := captureRetirements(t)
		if _, err := HandleDeleteDisk(d.deps).Handle(t.Context(), []json.RawMessage{planJSON(t, d.cid)},
			jsonrpc.Context{}); err != nil {
			t.Fatalf("delete_disk: %v", err)
		}
		requireRetirements(t, calls(), retiredParker{node: node, vmid: d.parker, trigger: "delete_disk"})
	})
	t.Run("explicit cleanup", func(t *testing.T) {
		d := newParkedFlowDisk(t, newLockContention(t))
		const node = "n1"
		markRecordUncertain(t, d, "operator cleanup")
		calls := captureRetirements(t)
		if _, err := CleanupStorageAllocation(t.Context(), attestedCleanupDeps(d.deps), d.journal,
			[]string{"n1"}, cleanupAttestedDecision(d.id)); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		requireRetirements(t, calls(),
			retiredParker{node: node, vmid: d.parker, trigger: "storage allocation cleanup"})
	})
}

// freshRollbackClient is the migration fake with the storage a create_disk
// rollback deletes the volume through.
type freshRollbackClient struct {
	*migFakeClient
	storage *rollbackStorage
}

func (c *freshRollbackClient) Storage() sdkstorage.Service { return c.storage }

// TestRetireEmptiedParker_CreateDiskRollbackRetiresTheParker proves the
// rollback of a create_disk whose park had landed unparks the disk, deletes
// the volume, and then hands the parker to the retirement, and that a
// rollback whose disk was on no parker retires nothing.
func TestRetireEmptiedParker_CreateDiskRollbackRetiresTheParker(t *testing.T) {
	c := newMigFakeClient(map[int]map[string]any{
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, "scsi0": "data:vm-9001-disk-0"},
	}, map[int]string{90000: "pve1"})
	storage := &rollbackStorage{}
	deps := migTestDeps(c)
	deps.PVE = &freshRollbackClient{migFakeClient: c, storage: storage}
	deps.Config.DetachedDiskStrategy = "parked"
	calls := captureRetirements(t)

	rollbackFreshDisk(context.Background(), deps, "pve1", "data", "data:vm-9001-disk-0", "data:vm-9001-disk-0")
	if _, still := c.configs[90000]["scsi0"]; still {
		t.Fatal("the rollback must unpark the disk before the delete")
	}
	if len(storage.deleteAsyncCalls) != 1 {
		t.Fatalf("the rollback must delete the volume; deletes %+v", storage.deleteAsyncCalls)
	}
	requireRetirements(t, calls(), retiredParker{node: "pve1", vmid: 90000, trigger: "create_disk rollback"})

	rollbackFreshDisk(context.Background(), deps, "pve1", "data", "data:vm-9001-disk-1", "data:vm-9001-disk-1")
	requireRetirements(t, calls(), retiredParker{node: "pve1", vmid: 90000, trigger: "create_disk rollback"})
}
