package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
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
	return captureRetirementsAt(t, nil)
}

// captureRetirementsAt is captureRetirements that also runs check inside each
// call, so a test can assert what had landed by the time the retirement ran.
func captureRetirementsAt(t *testing.T, check func(retiredParker)) func() []retiredParker {
	t.Helper()
	var mu sync.Mutex
	var seen []retiredParker
	restore := setRetireEmptiedParkerForTest(func(_ context.Context, _ Deps, node string, vmid int, trigger string) {
		call := retiredParker{node: node, vmid: vmid, trigger: trigger}
		if check != nil {
			check(call)
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, call)
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

// requireAttachLanded reports an error unless parker 90000 holds no disk and VM
// 700 holds one that carries the test's serial, which is what the attach has
// written by the time it hands the parker to the retirement.
func requireAttachLanded(t *testing.T, c *migFakeClient) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, still := c.configs[90000]["scsi0"]; still {
		t.Error("the retirement ran while the parker still held the disk")
	}
	for _, v := range c.configs[700] {
		if s, ok := v.(string); ok && strings.Contains(s, migTestToken) {
			return
		}
	}
	t.Errorf("the retirement ran before VM 700 held the disk; config %v", c.configs[700])
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
	calls := captureRetirementsAt(t, func(retiredParker) { requireAttachLanded(t, c) })

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
	calls := captureRetirementsAt(t, func(retiredParker) { requireAttachLanded(t, c) })

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
		calls := captureRetirementsAt(t, func(retiredParker) {
			if n := flowDiskSlots(d.client.state.configs[d.parker]); n != 0 {
				t.Errorf("the retirement ran while the parker still held %d disks", n)
			}
			if n := flowDiskSlots(d.client.state.configs[777]); n == 0 {
				t.Error("the retirement ran before VM 777 held the disk")
			}
		})
		if err := d.attach(t.Context()); err != nil {
			t.Fatalf("attach: %v", err)
		}
		requireRetirements(t, calls(), retiredParker{node: node, vmid: d.parker, trigger: "attach_disk"})
	})
	t.Run("delete_disk", func(t *testing.T) {
		d := newParkedFlowDisk(t, newLockContention(t))
		const node = "n1"
		calls := captureRetirementsAt(t, func(retiredParker) {
			if d.client.deletes == 0 {
				t.Error("the retirement ran before delete_disk deleted the volume")
			}
		})
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
		calls := captureRetirementsAt(t, func(retiredParker) {
			if d.client.deletes == 0 {
				t.Error("the retirement ran before the cleanup deleted the volume")
			}
		})
		if _, err := CleanupStorageAllocation(t.Context(), attestedCleanupDeps(d.deps), d.journal,
			[]string{"n1"}, cleanupAttestedDecision(d.id)); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		requireRetirements(t, calls(),
			retiredParker{node: node, vmid: d.parker, trigger: "storage allocation cleanup"})
	})
}

// flowDiskSlots counts the bus slots and unused entries a lifecycle flow
// config holds, which are the keys that can name a volume.
func flowDiskSlots(cfg map[string]any) int {
	n := 0
	for key := range cfg {
		for _, prefix := range []string{"scsi", "virtio", "sata", "ide", "unused"} {
			if rest, ok := strings.CutPrefix(key, prefix); ok && rest != "" {
				if _, err := strconv.Atoi(rest); err == nil {
					n++
				}
			}
		}
	}
	return n
}

// TestRetireEmptiedParker_AttachRetiresOnceItsGateAndLockAreReleased proves
// the attach that empties a parker retires it only after it has released the
// node's in-flight slot and the disk's lifecycle lock, so the retirement's
// budget is not spent while other requests wait on them.
func TestRetireEmptiedParker_AttachRetiresOnceItsGateAndLockAreReleased(t *testing.T) {
	d := newParkedFlowDisk(t, newLockContention(t))
	registry := NewInflightRegistry()
	d.deps.Inflight = registry
	d.deps.Config.MaxInflightPerNode = 1
	checked := false
	calls := captureRetirementsAt(t, func(retiredParker) {
		checked = true
		registry.mu.Lock()
		for node, slots := range registry.m {
			if len(slots) != 0 {
				t.Errorf("the retirement ran while the attach held node %s's in-flight slot", node)
			}
		}
		registry.mu.Unlock()
		release, held, err := d.journal.TryHoldShared(d.id)
		if err != nil {
			t.Errorf("probe the disk's lifecycle lock: %v", err)
			return
		}
		if !held {
			t.Error("the retirement ran while the attach held the disk's lifecycle lock")
			return
		}
		if err := release(); err != nil {
			t.Errorf("release the probe: %v", err)
		}
	})
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !checked {
		t.Fatal("the attach must hand the emptied parker to the retirement")
	}
	requireRetirements(t, calls(), retiredParker{node: "n1", vmid: d.parker, trigger: "attach_disk"})
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.m) == 0 {
		t.Fatal("the attach must take the node's in-flight slot")
	}
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
	calls := captureRetirementsAt(t, func(retiredParker) {
		if _, still := c.configs[90000]["scsi0"]; still {
			t.Error("the retirement ran before the rollback unparked the disk")
		}
		if len(storage.deleteAsyncCalls) != 1 {
			t.Errorf("the retirement ran before the rollback deleted the volume; deletes %+v",
				storage.deleteAsyncCalls)
		}
	})

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

// TestDeferParkerRetirements_TheOutermostHandlerDrainsEachParkerOnce proves
// a queued retirement waits for the drain, a nested handler leaves the drain
// to the outermost one, a parker emptied twice is retired once, and a call
// made free of the request's cancellation is drained free of it too.
func TestDeferParkerRetirements_TheOutermostHandlerDrainsEachParkerOnce(t *testing.T) {
	var mu sync.Mutex
	var seen []retiredParker
	canceled := map[int]bool{}
	restore := setRetireEmptiedParkerForTest(func(ctx context.Context, _ Deps, node string, vmid int, trigger string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, retiredParker{node: node, vmid: vmid, trigger: trigger})
		canceled[vmid] = ctx.Err() != nil
	})
	t.Cleanup(restore)
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }

	outer, cancel := context.WithCancel(context.Background())
	ctx, drain := deferParkerRetirements(outer)
	inner, innerDrain := deferParkerRetirements(ctx)
	retireEmptiedParker(inner, Deps{}, "n1", 90000, "attach_disk")
	retireEmptiedParker(ctx, Deps{}, "n1", 90000, "attach_disk")
	retireEmptiedParker(context.WithoutCancel(inner), Deps{}, "n2", 90001, "create_disk rollback")
	innerDrain()
	if n := count(); n != 0 {
		t.Fatalf("no retirement may run before the outermost drain; ran %d", n)
	}
	cancel()
	drain()
	mu.Lock()
	defer mu.Unlock()
	want := []retiredParker{
		{node: "n1", vmid: 90000, trigger: "attach_disk"},
		{node: "n2", vmid: 90001, trigger: "create_disk rollback"},
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("retirements %+v, want %+v", seen, want)
	}
	if !canceled[90000] || canceled[90001] {
		t.Fatalf("only the call made free of cancellation runs free of it; canceled %v", canceled)
	}
}
