package handlers

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestConcurrentDetachesOfOneManagedDiskParkItOnce runs the field race at the
// handler level. Two detach_disk calls for one journal-managed disk, the way a
// dynamic-disk broker task and a deploy task send them, run at the same time
// under the parked strategy. One parks the disk, and the other finds it parked
// and returns success, so the disk lands on the parker once, the record
// closes, and no sentinel is left behind.
func TestConcurrentDetachesOfOneManagedDiskParkItOnce(t *testing.T) {
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { errs[i] = d.detach(t) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("detach %d: %v", i, err)
		}
	}

	parked := 0
	for vmid, cfg := range client.state.configs {
		for key, value := range cfg {
			drive, ok := value.(string)
			if ok && strings.Contains(drive, "serial=") && !strings.HasPrefix(key, "unused") {
				if vmid == 777 {
					t.Fatalf("the disk is still on VM 777 slot %s", key)
				}
				if vmid == parker {
					parked++
				}
			}
		}
	}
	if parked != 1 {
		t.Fatalf("the parker holds the disk on %d slots, want exactly 1: %v", parked, client.state.configs[parker])
	}
	record := d.record(t)
	for i := range record.Steps {
		if record.Steps[i].State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", record.Steps[i].ID, record.Steps[i].Kind, record.Steps[i].State)
		}
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.pools) != 0 {
		t.Fatalf("sentinel pools left behind: %v", locks.pools)
	}
}

// TestDetachOfADiskThatLeftForAnotherVMIsADetach covers a detach whose disk
// another operation moved to a different VM while the detach waited for the
// per-disk lock. The transfer reports the disk attached elsewhere, and the
// detach treats it as already detached from this VM, the way it answers a
// stale Director whose disk it finds on another VM up front. Nothing moves to
// the parker.
func TestDetachOfADiskThatLeftForAnotherVMIsADetach(t *testing.T) {
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	d.deps.Logger = logger
	// The other operation moves the disk to VM 778 at the moment this detach
	// takes the per-disk lock, which is the first point the transfer can see.
	locks.afterCreate = func(pool string) {
		if !strings.HasPrefix(pool, pve.DiskTransferLockPoolName("")) {
			return
		}
		drive := client.state.configs[777]["scsi1"]
		delete(client.state.configs[777], "scsi1")
		client.state.configs[778] = map[string]any{"name": "other", "digest": "1", "scsi2": drive}
		locks.afterCreate = nil
	}

	if err := d.detach(t); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if _, ok := client.state.configs[778]["scsi2"]; !ok {
		t.Fatalf("the disk left VM 778: %v", client.state.configs[778])
	}
	for key := range client.state.configs[parker] {
		if strings.HasPrefix(key, "scsi") && key != "scsihw" {
			t.Fatalf("the detach moved something onto the parker: %v", client.state.configs[parker])
		}
	}
	for _, entry := range obs.All() {
		if strings.Contains(entry.Message, "disk attached to a different VM") && entry.Attrs["holder_vmid"] == int64(778) {
			return
		}
	}
	t.Fatalf("the detach did not warn that VM 778 holds the disk: %+v", obs.All())
}

// TestDeleteVMPreserveOfADiskThatLeftForAnotherVM covers delete_vm's
// preserving transfer of a stable-ID disk that another operation moved to a
// different VM while the transfer waited for the per-disk lock. The destroy
// can't take a disk this VM no longer holds, so delete_vm warns and goes on
// without moving anything to the parker.
func TestDeleteVMPreserveOfADiskThatLeftForAnotherVM(t *testing.T) {
	locks := newLockContention(t)
	deps, client, _, _, _ := lifecycleFlowFixtureState(t, true)
	deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks}
	deps.Config.DetachedDiskStrategy = "parked"
	parker := deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps.Logger = logger
	locks.afterCreate = func(pool string) {
		if !strings.HasPrefix(pool, pve.DiskTransferLockPoolName("")) {
			return
		}
		drive := client.state.configs[777]["scsi1"]
		delete(client.state.configs[777], "scsi1")
		client.state.configs[778] = map[string]any{"name": "other", "digest": "1", "scsi2": drive}
		locks.afterCreate = nil
	}

	if err := detachForeignActiveDisks(t.Context(), deps, "n1", "777", 777, logger); err != nil {
		t.Fatalf("detachForeignActiveDisks: %v", err)
	}
	if _, ok := client.state.configs[778]["scsi2"]; !ok {
		t.Fatalf("the disk left VM 778: %v", client.state.configs[778])
	}
	for key := range client.state.configs[parker] {
		if strings.HasPrefix(key, "scsi") && key != "scsihw" {
			t.Fatalf("delete_vm moved something onto the parker: %v", client.state.configs[parker])
		}
	}
	for _, entry := range obs.All() {
		if strings.Contains(entry.Message, "left this VM for another one") && entry.Attrs["holder_vmid"] == int64(778) {
			return
		}
	}
	t.Fatalf("delete_vm did not warn that VM 778 holds the disk: %+v", obs.All())
}

// TestManagedDetachFollowsADiskDeleteVMParkedMeanwhile covers a managed
// detach whose disk delete_vm's preserving transfer parked, under a new
// volume name and without the allocation, while the detach waited for the
// per-disk lock. The detach moves nothing. It follows the disk to its new
// name through the managed lifecycle client's ObserveDiskRelocated, stamps
// its allocation onto the record delete_vm left, and passes the managed
// readback, so the detach succeeds.
func TestManagedDetachFollowsADiskDeleteVMParkedMeanwhile(t *testing.T) {
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()
	record := d.record(t)
	movesBefore := client.moves
	var landed string
	locks.afterCreate = func(pool string) {
		if pool != pve.DiskTransferLockPoolName(record.DiskToken) {
			return
		}
		locks.afterCreate = nil
		if err := client.reassignVolume(777, "scsi1", parker, "scsi0"); err != nil {
			t.Errorf("simulated delete_vm transfer: %v", err)
			return
		}
		landed = strings.Split(client.state.configs[parker]["scsi0"].(string), ",")[0]
		client.state.configs[parker]["description"] = `<!--BOSH:{"bosh_parked_disks":{"` + record.DiskToken + `":{"disk_cid":"` + cid +
			`","parked_at":"2026-10-09T00:00:00Z","node":"n1","volid":"` + landed + `","slot":"scsi0","source_vm_cid":"777"}}}-->`
	}

	if err := d.detach(t); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if landed == "" {
		t.Fatal("the simulated delete_vm transfer never ran")
	}
	if client.moves != movesBefore+1 {
		t.Fatalf("moves = %d, want only delete_vm's", client.moves-movesBefore)
	}
	if got := client.state.configs[parker]["scsi0"].(string); !strings.HasPrefix(got, landed+",") {
		t.Fatalf("the parker's slot changed to %q", got)
	}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(client.state.configs[parker]))
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &entries); err != nil {
		t.Fatal(err)
	}
	entry := entries[record.DiskToken]
	if entry["allocation_id"] != record.ID || entry["allocation_namespace"] != record.Namespace || entry["volid"] != landed {
		t.Fatalf("the parker's record = %v, want it naming %q and allocation %s/%s", entry, landed, record.Namespace, record.ID)
	}
	// The guard journals the record write on the parker under the name the
	// disk has now, which it can only know through ObserveDiskRelocated.
	followed := false
	for _, step := range d.record(t).Steps {
		if step.State != aj.Observed {
			t.Fatalf("step %s (%s) left %s", step.ID, step.Kind, step.State)
		}
		if step.Kind == "lifecycle_detach_disk_Nodes_UpdateQemuConfig" && step.Target.VMID == parker {
			followed = step.Target.IntendedVolume == landed
		}
	}
	if !followed {
		t.Fatalf("the managed lifecycle did not follow the disk to %q: %+v", landed, d.record(t).Steps)
	}
}
