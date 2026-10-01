// managed_pending_delete_internal_test.go holds the journal-managed rows for a
// slot delete that PVE could only record as pending. A managed operation that
// failed only because of a delete it reverted returns its allocation
// unchanged, a busy delete settles through the guard instead of poisoning it,
// and the VM allocation guard never observes a pending delete as a delete.
//
// Several rows swap the parker pool sweep seam, so none of them may call
// t.Parallel.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// reconciliationsRequired wires storage metrics into deps and returns a
// function that counts the "required" reconciliations recorded so far.
func reconciliationsRequired(t *testing.T, deps *Deps) func() int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	metrics, err := NewStoragePlacementMetrics(provider.Meter("pending-delete"))
	if err != nil {
		t.Fatal(err)
	}
	deps.StorageMetrics = metrics
	return func() int64 {
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || m.Name != "cpi.storage.reconciliations" {
					continue
				}
				for _, point := range sum.DataPoints {
					if value, found := point.Attributes.Value("storage.reconciliation.outcome"); found && value.AsString() == "required" {
						total += point.Value
					}
				}
			}
		}
		return total
	}
}

// managedPendingWorld is what managedPendingFixture sets up.
type managedPendingWorld struct {
	deps            Deps
	client          *lifecycleFlowPVE
	journal         *aj.Journal
	id, cid, volume string
}

// managedPendingFixture is the flow fixture with VM 777 running under the
// pending model, a hotplug setting of hotplug, and the parked strategy.
func managedPendingFixture(t *testing.T, hotplug string) managedPendingWorld {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	if hotplug != "" {
		client.state.configs[777]["hotplug"] = hotplug
	}
	client.pending = newFakePendingModel()
	client.pending.run(777)
	deps.Config.DetachedDiskStrategy = "parked"
	return managedPendingWorld{deps: deps, client: client, journal: journal, id: id, cid: cid, volume: volume}
}

// requireRecordState checks the allocation record's state.
func requireRecordState(t *testing.T, journal *aj.Journal, id string, want aj.State, where string) {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != want {
		t.Fatalf("%s: record state = %s, want %s: %+v", where, record.State, want, record)
	}
}

// TestManagedDetach_HotplugPendingDeleteRecordsNoReconciliation covers the
// hotplug case. The detach fails on the reverted pending delete, returns the
// allocation unchanged, and records no reconciliation.
func TestManagedDetach_HotplugPendingDeleteRecordsNoReconciliation(t *testing.T) {
	captureParkerPoolSweep(t)
	world := managedPendingFixture(t, "network,usb")
	deps, client, journal, id, cid, volume := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	required := reconciliationsRequired(t, &deps)

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "managed hotplug detach")
	requireRecordState(t, journal, id, aj.ReadyToReturn, "managed hotplug detach")
	requireManagedPendingSettled(t, journal, id, volume, "managed hotplug detach", 1)
	if got := required(); got != 0 {
		t.Fatalf("reconciliations required = %d, want none", got)
	}
	if len(client.pending.reverts) != 1 {
		t.Fatalf("reverts = %v, want exactly one", client.pending.reverts)
	}
}

// TestManagedDetach_BusyGuestRevertsThroughTheGuard covers a managed detach on
// a guest that holds the device. Each busy delete leaves a pending delete and
// fails, and the guard settles it as not applied instead of poisoning, so the
// helper's busy retries pass the guard, and so does its revert, which the
// guard observes. The allocation comes back unchanged with no reconciliation.
func TestManagedDetach_BusyGuestRevertsThroughTheGuard(t *testing.T) {
	captureParkerPoolSweep(t)
	world := managedPendingFixture(t, "")
	deps, client, journal, id, cid, volume := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	client.pending.busy[777] = true
	required := reconciliationsRequired(t, &deps)

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requireRetriable(t, err, "managed busy detach")
	requireText(t, err, "managed busy detach", []string{"VM 777", "scsi1", "still holds the disk"})
	deletes := len(client.pending.deleteCalls)
	if deletes < 2 {
		t.Fatalf("busy deletes = %v, want the helper's retries to pass the guard", client.pending.deleteCalls)
	}
	if len(client.pending.reverts) != 1 || client.pending.pendingDelete(777, "scsi1") {
		t.Fatalf("reverts = %v pending=%v, want one revert that cleared the pending delete", client.pending.reverts, client.pending.pendingDelete(777, "scsi1"))
	}
	if value, _ := client.state.configs[777]["scsi1"].(string); !strings.HasPrefix(value, volume+",") {
		t.Fatalf("VM 777 scsi1 = %q, want the disk back", value)
	}
	requireNoParkerSlot(t, client.state.configs, "managed busy detach")
	requireRecordState(t, journal, id, aj.ReadyToReturn, "managed busy detach")
	requireManagedPendingSettled(t, journal, id, volume, "managed busy detach", deletes)
	if got := required(); got != 0 {
		t.Fatalf("reconciliations required = %d, want none", got)
	}
}

// TestManagedDetach_UnconfirmedRevertGoesUncertain covers a revert that PVE
// refuses. The pending delete may still be there, so the allocation goes to
// reconciliation, and the reconciliation is recorded as required.
func TestManagedDetach_UnconfirmedRevertGoesUncertain(t *testing.T) {
	captureParkerPoolSweep(t)
	world := managedPendingFixture(t, "network,usb")
	deps, client, journal, id, cid, _ := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	client.pending.revertErr = errors.New("revert refused by the fake")
	required := reconciliationsRequired(t, &deps)

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("a detach whose revert failed succeeded")
	}
	requireRecordState(t, journal, id, aj.ReconciliationRequired, "unconfirmed revert")
	if got := required(); got == 0 {
		t.Fatal("no reconciliation was recorded as required")
	}
}

// managedPendingLifecycle opens the managed lifecycle detach_disk would, on the
// fixture's disk, and returns the guarded deps with it.
func managedPendingLifecycle(t *testing.T, deps Deps, cid string) (Deps, *managedDiskLifecycle) {
	t.Helper()
	ctx := pendingRowContext()
	bare, meta, err := decodeDiskCID(ctx, deps, "detach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, deps, "detach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	managed, lifecycle, err := managedDiskOperation(ctx, deps, rd, "detach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("managed lifecycle: %v", err)
	}
	return managed, lifecycle
}

// TestManagedDetach_RevertedPendingDeleteWithAnUnobservedStepGoesUncertain
// covers a reverted pending delete together with a step the operation never
// observed. The clean return needs every journaled step observed, so the
// allocation goes to reconciliation.
func TestManagedDetach_RevertedPendingDeleteWithAnUnobservedStepGoesUncertain(t *testing.T) {
	world := managedPendingFixture(t, "network,usb")
	deps, client, journal, id, cid, volume := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	required := reconciliationsRequired(t, &deps)
	managed, lifecycle := managedPendingLifecycle(t, deps, cid)

	detachErr := detachDriveSlot(pendingRowContext(), managed, "n1", 777, "scsi1", volume)
	if pending, ok := pve.IsDriveDeletePending(detachErr); !ok || pending.Reason != pve.DriveDeletePendingHotplug {
		t.Fatalf("detach = %v, want the reverted hotplug pending delete", detachErr)
	}
	if _, err := storageMutationIntent(lifecycle.handle, "lifecycle_detach_disk_unobserved", aj.Target{Node: "n1", VMID: 777}, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.finish(pendingRowContext(), driveDeletePendingDiskError("detach_disk", detachErr), false); err == nil {
		t.Fatal("finish returned no error")
	}
	requireRecordState(t, journal, id, aj.ReconciliationRequired, "reverted pending delete with an unobserved step")
	if got := required(); got == 0 {
		t.Fatal("no reconciliation was recorded as required")
	}
	if !client.pending.running[777] {
		t.Fatal("setup: VM 777 stopped")
	}
}

// TestManagedDetach_FailedDeleteWithAnotherReadbackPoisons covers a failed
// slot delete whose readback isn't exactly a pending delete of that slot, here
// a delete PVE refused before it wrote anything. The guard can't settle it, so
// it poisons as before.
func TestManagedDetach_FailedDeleteWithAnotherReadbackPoisons(t *testing.T) {
	world := managedPendingFixture(t, "")
	deps, client, _, _, cid, volume := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	client.pending.refuseDelete = errors.New("API request failed: unable to delete the drive (code: 500)")
	managed, lifecycle := managedPendingLifecycle(t, deps, cid)

	if err := detachDriveSlot(pendingRowContext(), managed, "n1", 777, "scsi1", volume); err == nil {
		t.Fatal("a refused delete succeeded")
	}
	if lifecycle.guard.Err() == nil {
		t.Fatal("the guard stayed usable after a failed delete whose readback shows no pending delete")
	}
	if len(client.pending.reverts) != 0 {
		t.Fatalf("reverts = %v, want none", client.pending.reverts)
	}
}

// TestPendingDeleteOnlyChange_RefusesAnyOtherChange pins the readback check
// both guards share. Only a pending delete of exactly the named keys, with
// every other key and every current value unchanged, counts.
func TestPendingDeleteOnlyChange_RefusesAnyOtherChange(t *testing.T) {
	before := map[string]any{"digest": "1", "scsi1": "a:1/vm-1-disk-0.raw,size=5G", "cores": float64(2)}
	views := func(items ...string) pve.QemuViews {
		resp := make(nodes.ListQemuPendingResponse, 0, len(items))
		for _, item := range items {
			resp = append(resp, json.RawMessage(item))
		}
		got, err := pve.ReadQemuViews(context.Background(), &pendingRowClient{resp: resp}, "n1", 1)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for name, tc := range map[string]struct {
		views pve.QemuViews
		want  bool
	}{
		"only the pending delete": {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-0.raw,size=5G","delete":1}`, `{"key":"cores","value":2}`), true},
		"no pending delete":       {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-0.raw,size=5G"}`, `{"key":"cores","value":2}`), false},
		"another pending value":   {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-0.raw,size=5G","delete":1}`, `{"key":"cores","value":2,"pending":4}`), false},
		"a changed current value": {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-9.raw,size=5G","delete":1}`, `{"key":"cores","value":2}`), false},
		"a new key":               {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-0.raw,size=5G","delete":1}`, `{"key":"cores","value":2}`, `{"key":"scsi2","value":"a:1/vm-1-disk-2.raw"}`), false},
		"another pending delete":  {views(`{"key":"digest","value":"2"}`, `{"key":"scsi1","value":"a:1/vm-1-disk-0.raw,size=5G","delete":1}`, `{"key":"cores","value":2,"delete":1}`), false},
	} {
		if got := pendingDeleteOnlyChange(before, tc.views, []string{"scsi1"}); got != tc.want {
			t.Errorf("%s: pendingDeleteOnlyChange = %v, want %v", name, got, tc.want)
		}
	}
}

// pendingRowClient serves one hand-built pending response.
type pendingRowClient struct {
	pve.Client
	resp nodes.ListQemuPendingResponse
}

func (c *pendingRowClient) Nodes() nodes.Service { return &pendingRowNodes{c: c} }

type pendingRowNodes struct {
	nodes.Service
	c *pendingRowClient
}

func (n *pendingRowNodes) ListQemuPending(context.Context, string, string) (*nodes.ListQemuPendingResponse, error) {
	out := append(nodes.ListQemuPendingResponse(nil), n.c.resp...)
	return &out, nil
}

// TestManagedVMGuard_PendingDeleteIsNotObservedAsADelete covers the VM
// allocation guard. A slot delete on the running VM stays pending, and the
// guard settles it as not applied rather than observing it as a delete, stays
// usable, and admits and observes the revert after it.
func TestManagedVMGuard_PendingDeleteIsNotObservedAsADelete(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	ctx := context.Background()
	if err := createManagedVMRoot(ctx, guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
		t.Fatal(err)
	}
	extra := fixture.storage + ":9001/vm-9001-disk-0.raw,size=1G"
	fixture.cfg["scsi3"] = extra
	fixture.cfg["hotplug"] = "network"
	fixture.pending = newFakePendingModel()
	fixture.pending.run(101)

	slot := "scsi3"
	if err := guarded.PVE.Nodes().UpdateQemuConfig(ctx, m.shape.node, "101", &nodes.UpdateQemuConfigParams{Delete: &slot}); err != nil {
		t.Fatalf("guarded delete that stayed pending: %v", err)
	}
	if err := m.guard.Err(); err != nil {
		t.Fatalf("the guard was poisoned by a pending delete: %v", err)
	}
	if !fixture.pending.pendingDelete(101, "scsi3") {
		t.Fatal("setup: the delete applied")
	}
	settled := m.handle.Record().Steps[len(m.handle.Record().Steps)-1]
	if settled.State != aj.Observed || len(settled.VolIDs) != 0 {
		t.Fatalf("delete step = %+v, want it settled with no volume", settled)
	}

	if err := guarded.PVE.Nodes().UpdateQemuConfig(ctx, m.shape.node, "101", &nodes.UpdateQemuConfigParams{Revert: &slot}); err != nil {
		t.Fatalf("guarded revert of the pending delete: %v", err)
	}
	if err := m.guard.Err(); err != nil {
		t.Fatalf("the guard was poisoned by the revert: %v", err)
	}
	if fixture.cfg["scsi3"] != extra || fixture.pending.pendingDelete(101, "scsi3") {
		t.Fatalf("scsi3 after the revert = %v pending=%v, want it back with nothing pending", fixture.cfg["scsi3"], fixture.pending.pendingDelete(101, "scsi3"))
	}
	reverted := m.handle.Record().Steps[len(m.handle.Record().Steps)-1]
	if reverted.State != aj.Observed {
		t.Fatalf("revert step = %+v, want it observed", reverted)
	}

	other := "scsi4"
	if err := guarded.PVE.Nodes().UpdateQemuConfig(ctx, m.shape.node, "101", &nodes.UpdateQemuConfigParams{Revert: &other}); err == nil {
		t.Fatal("the guard admitted a revert of a key with no pending delete")
	}
}

// TestManagedConfigWriteObservation_SkipsTheMatcherOnlyForARevertAlone covers
// when the lifecycle guard's readback skips the field matcher. It skips it only
// for a write that carries a revert and no other field, and it still checks the
// revert itself. A write with neither a delete nor a revert reaches the matcher
// and fails there, as an empty one always has.
func TestManagedConfigWriteObservation_SkipsTheMatcherOnlyForARevertAlone(t *testing.T) {
	world := managedPendingFixture(t, "network,usb")
	deps, client, _, _, _, volume := world.deps, world.client, world.journal, world.id, world.cid, world.volume
	g := &managedDiskLifecycleGuard{lifecycle: &managedDiskLifecycle{deps: deps, disk: resolvedDisk{volid: volume}}}
	ctx := context.Background()
	observation := managedDiskMutationObservation{node: "n1", vmid: 777}
	cfg := client.state.configs[777]

	observation.fields = map[string]any{}
	if _, err := g.observeConfigWrite(ctx, observation, cfg); err == nil || !strings.Contains(err.Error(), "empty or malformed") {
		t.Fatalf("a write with neither a delete nor a revert = %v, want the matcher's refusal", err)
	}

	observation.fields = map[string]any{"revert": "scsi1"}
	volumes, err := g.observeConfigWrite(ctx, observation, cfg)
	if err != nil || len(volumes) != 1 || volumes[0] != volume {
		t.Fatalf("a revert alone with the key back = %v, %v; want it observed naming %s", volumes, err, volume)
	}

	client.pending.holdDelete(777, cfg, "scsi1")
	if _, err := g.observeConfigWrite(ctx, observation, client.state.configs[777]); err == nil || !strings.Contains(err.Error(), "revert not observed") {
		t.Fatalf("a revert alone while the delete is still pending = %v, want it refused", err)
	}
}

// TestManagedEphemeralAttach_FailsClosedOnAPendingDeleteSlot covers the managed
// ephemeral attach, which runs on a VM that has never started and so can't
// have a pending delete. If the ephemeral volume turns up on a slot whose
// delete is pending anyway, the attach fails closed rather than attach or
// revert.
func TestManagedEphemeralAttach_FailsClosedOnAPendingDeleteSlot(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{ephemeral: true})
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	if err := createManagedVMRoot(t.Context(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
		t.Fatal(err)
	}
	target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleEphemeral)
	volume := target.StorageID + ":101/vm-101-disk-9.raw"
	m.volumes[storageRoleEphemeral] = volume
	fixture.cfg["scsi1"] = volume
	fixture.pending = newFakePendingModel()
	fixture.pending.run(101)
	fixture.pending.holdDelete(101, fixture.cfg, "scsi1")

	_, err := attachCreatedVMEphemeral(t.Context(), guarded, m.deps.Logger, m.parsed, m.shape, 101)
	if err == nil || !strings.Contains(err.Error(), "pending delete") {
		t.Fatalf("ephemeral attach onto a pending-deleted slot = %v, want the fail-closed refusal", err)
	}
	if len(fixture.pending.reverts) != 0 {
		t.Fatalf("reverts = %v, want none", fixture.pending.reverts)
	}
	for key, raw := range fixture.cfg {
		if text, _ := raw.(string); strings.Split(text, ",")[0] == volume {
			t.Fatalf("the ephemeral volume was attached on %s", key)
		}
	}
}

// TestManagedVMGuard_BusyDeleteSettlesThroughTheGuard covers the VM allocation
// guard on a guest that holds the device. The delete records a pending delete
// and fails busy, and the guard settles it as not applied instead of
// poisoning, hands the busy error back unchanged, and then admits and observes
// the revert. When the readback after the failed delete also shows another
// change, the guard poisons as before.
func TestManagedVMGuard_BusyDeleteSettlesThroughTheGuard(t *testing.T) {
	setup := func(t *testing.T) (*managedVMAllocation, *managedVMGuardFixture, Deps, string) {
		t.Helper()
		m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
		guarded := m.deps
		guarded.PVE = m.guard.Client()
		if err := createManagedVMRoot(context.Background(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
			t.Fatal(err)
		}
		extra := fixture.storage + ":9001/vm-9001-disk-0.raw,size=1G"
		fixture.cfg["scsi3"] = extra
		fixture.pending = newFakePendingModel()
		fixture.pending.run(101)
		fixture.pending.busy[101] = true
		return m, fixture, guarded, extra
	}
	slot := "scsi3"

	t.Run("settles and admits the revert", func(t *testing.T) {
		m, fixture, guarded, extra := setup(t)
		ctx := context.Background()
		err := guarded.PVE.Nodes().UpdateQemuConfig(ctx, m.shape.node, "101", &nodes.UpdateQemuConfigParams{Delete: &slot})
		if !pve.IsHotUnplugBusy(err) {
			t.Fatalf("guarded busy delete = %v, want PVE's busy error back unchanged", err)
		}
		if guardErr := m.guard.Err(); guardErr != nil {
			t.Fatalf("the guard was poisoned by a busy delete it could settle: %v", guardErr)
		}
		record := m.handle.Record()
		settled := record.Steps[len(record.Steps)-1]
		if settled.State != aj.Observed || len(settled.VolIDs) != 0 {
			t.Fatalf("delete step = %+v, want it settled with no volume", settled)
		}
		if err := guarded.PVE.Nodes().UpdateQemuConfig(ctx, m.shape.node, "101", &nodes.UpdateQemuConfigParams{Revert: &slot}); err != nil {
			t.Fatalf("guarded revert after the busy delete: %v", err)
		}
		if guardErr := m.guard.Err(); guardErr != nil {
			t.Fatalf("the guard was poisoned by the revert: %v", guardErr)
		}
		if fixture.cfg["scsi3"] != extra || fixture.pending.pendingDelete(101, "scsi3") {
			t.Fatalf("scsi3 after the revert = %v, want it back with nothing pending", fixture.cfg["scsi3"])
		}
		record = m.handle.Record()
		if reverted := record.Steps[len(record.Steps)-1]; reverted.State != aj.Observed {
			t.Fatalf("revert step = %+v, want it observed", reverted)
		}
	})

	t.Run("poisons when the readback shows another change", func(t *testing.T) {
		m, fixture, guarded, _ := setup(t)
		fixture.pending.afterHold = func(cfg map[string]any) {
			cfg["scsi4"] = fixture.storage + ":9004/vm-9004-disk-0.raw,size=1G"
		}
		err := guarded.PVE.Nodes().UpdateQemuConfig(context.Background(), m.shape.node, "101", &nodes.UpdateQemuConfigParams{Delete: &slot})
		if err == nil {
			t.Fatal("the busy delete succeeded")
		}
		if m.guard.Err() == nil {
			t.Fatal("the guard stayed usable after a failed delete whose readback shows another change")
		}
	})
}
