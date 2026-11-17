package handlers_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// TestHandleDeleteVM_EntryNodeShuttingDownStillDeletes covers a delete whose
// entry node is in the middle of a shutdown. The entry node still answers
// every cluster read, but its own clock read fails with a 500 because
// timedatectl fails once systemd starts shutting down. The HA view must stay
// readable on another member's clock, so the delete stops the VM through PVE
// HA and destroys it exactly as it does with a healthy entry node.
func TestHandleDeleteVM_EntryNodeShuttingDownStillDeletes(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, broken ...string) (*legacyHADelete, error) {
		t.Helper()
		scene := handlers.NewHADeleteScene()
		scene.Manage(101, "pve-node2", "started", "started")
		for _, node := range broken {
			scene.BrokenClocks[node] = true
		}
		d := newLegacyHADelete(t, scene, "pve-node2", dlbEnabledConfig())
		err := d.run()
		return d, err
	}
	healthy, err := run(t)
	if err != nil {
		t.Fatalf("healthy entry: unexpected error: %v", err)
	}
	want := healthy.scene.EventsSnapshot()

	d, err := run(t, "pve-node1")
	if err != nil {
		t.Fatalf("an entry node whose clock read fails must not defer the delete: %v", err)
	}
	if got := d.scene.EventsSnapshot(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the delete must match a healthy entry's\n got: %v\nwant: %v", got, want)
	}
	if !d.purged {
		t.Fatal("the destroy must purge, which removes the HA resource")
	}
}

// TestHandleDeleteVM_UnreadableClockDefersCleanlyAndRetries checks the
// recreate that failed when no member's clock answered. The deferral must
// write nothing, and the Director's retry of the same delete must then run
// the whole delete once a clock answers again.
func TestHandleDeleteVM_UnreadableClockDefersCleanlyAndRetries(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node2", "started", "started")
	for _, node := range scene.Members {
		scene.BrokenClocks[node] = true
	}
	d := newLegacyHADelete(t, scene, "pve-node2", dlbEnabledConfig())

	err := d.run()
	requireHARetriable(t, err, "HA state is unknown", "entry node clock unreadable", "timedatectl")
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a deferred delete must write nothing; events: %v", events)
	}
	if d.gone.Load() {
		t.Fatal("a deferred delete must leave the VM in place")
	}

	scene.Do(func(s *handlers.HADeleteScene) { s.BrokenClocks = map[string]bool{"pve-node1": true} })
	if err := d.run(); err != nil {
		t.Fatalf("the retry must delete the VM once a member's clock answers: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, destroy := eventIndex(events, "UpdateHaResources:vm:101=stopped"), eventIndex(events, "DeleteQemu")
	if stop == -1 || destroy == -1 || stop > destroy {
		t.Fatalf("the retry must stop through PVE HA before the destroy; events: %v", events)
	}
	if !d.purged || !d.gone.Load() {
		t.Fatal("the retry must destroy and purge the VM")
	}
}

// TestHandleDeleteVM_VMOnTheFailingEntryNodeStillDeletes is the shape of the
// lab's failure. The VM sits on the entry node itself, which is in HA
// maintenance with a fresh LRM, and the entry node's own clock read fails with
// the shutdown 500. The HA view must stay readable on a peer's clock, and the
// delete must stop the VM through PVE HA and destroy it.
func TestHandleDeleteVM_VMOnTheFailingEntryNodeStillDeletes(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.NodeState["pve-node1"] = "maintenance"
	scene.BrokenClocks["pve-node1"] = true
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())

	if err := d.run(); err != nil {
		t.Fatalf("a VM on a failing entry node in maintenance must still delete: %v", err)
	}
	events := scene.EventsSnapshot()
	stop, destroy := eventIndex(events, "UpdateHaResources:vm:101=stopped"), eventIndex(events, "DeleteQemu")
	if stop == -1 || destroy == -1 || stop > destroy {
		t.Fatalf("the delete must stop through PVE HA before the destroy; events: %v", events)
	}
	if !d.purged || !d.gone.Load() {
		t.Fatal("the delete must destroy and purge the VM")
	}
}

// TestHandleDeleteVM_StaleCRMOnABorrowedClockDefers checks that the delete
// still reads the age of the CRM timestamp when the clock is borrowed. The peer
// whose clock the read borrows runs 95 seconds ahead of the CRM's last round,
// so the view is stale and the delete must wait without writing anything.
func TestHandleDeleteVM_StaleCRMOnABorrowedClockDefers(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node2", "started", "started")
	scene.BrokenClocks["pve-node1"] = true
	scene.NodeNow["pve-node2"] = scene.Now + 90
	d := newLegacyHADelete(t, scene, "pve-node2", dlbEnabledConfig())

	err := d.run()
	requireHARetriable(t, err, "HA state is unknown", "CRM timestamp", "on node pve-node2's clock")
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a deferred delete must write nothing; events: %v", events)
	}
	if d.gone.Load() {
		t.Fatal("a deferred delete must leave the VM in place")
	}
}
