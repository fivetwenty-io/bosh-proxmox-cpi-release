package handlers_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// These tests drive delete_vm through the handler across a node outage that
// PVE HA ends by restarting the VM in place, as the resurrector meets it.

// requireDeferralHeld fails unless the fixture's record holds a deferral of
// vm:101 exactly when want says so.
func (f *deadNodeDeleteFixture) requireDeferralHeld(want bool) {
	f.t.Helper()
	_, held, err := f.episodes.LastRecoveryDeferral("vm:101")
	if err != nil {
		f.t.Fatalf("read the recovery deferral: %v", err)
	}
	if held != want {
		f.t.Fatalf("recovery deferral of vm:101 held = %v, want %v", held, want)
	}
}

// TestHandleDeleteVM_HoldVMRestartedInPlaceSettlesBeforeTheDelete replays the
// outage of a VM whose strict pin admits its own node alone. While the node is
// down, delete_vm defers because no live node can take the config. The node
// comes back and PVE HA restarts the VM in place, and the resurrector's delete
// arrives 32 seconds later while the guest boots. That delete must stop and
// destroy nothing. Once the VM has run for five minutes, the next delete stops
// it through PVE HA, destroys it, and clears the deferral.
func TestHandleDeleteVM_HoldVMRestartedInPlaceSettlesBeforeTheDelete(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.pin(deadNode, nil)

	requireHARetriable(t, f.run(), "no deletion destination")
	f.requireNoWrites()
	f.requireDeferralHeld(true)

	f.scene.OnRead = nil
	f.scene.Do(func(s *handlers.HADeleteScene) {
		s.Offline[deadNode] = false
		s.NodeState[deadNode] = "online"
		s.LRMAge[deadNode] = 2
		s.Services["vm:101"] = handlers.HASceneService{State: "started", Node: deadNode, UID: "uid-restart"}
		s.IndexHAState[101] = "started"
		s.Uptime[101] = 32
	})
	requireHARetriable(t, f.run(), "PVE HA restarted it on node "+deadNode+" 32s ago", "has run for 5m0s")
	f.requireNoWrites()
	f.requireDeferralHeld(true)

	f.scene.Do(func(s *handlers.HADeleteScene) { s.Uptime[101] = 301 })
	if err := f.run(); err != nil {
		t.Fatalf("the delete of a VM that has run past the settle window must proceed: %v", err)
	}
	ev := f.events()
	stop := eventIndex(ev, "UpdateHaResources:vm:101=stopped")
	destroy := eventIndex(ev, "DeleteQemu:"+deadNode)
	if stop == -1 || destroy == -1 || stop > destroy {
		t.Fatalf("want the stop through PVE HA and then the destroy on %s; events: %v", deadNode, ev)
	}
	f.requireDeferralHeld(false)
}

// TestHandleDeleteVM_FreshHAVMWithoutDeferralsIsNotHeld deletes an HA VM that
// PVE HA started ten seconds ago and whose delete never deferred, as the
// cleanup of a failed deploy does. It proceeds on the first call.
func TestHandleDeleteVM_FreshHAVMWithoutDeferralsIsNotHeld(t *testing.T) {
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Uptime[101] = 10
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())
	d.deps.HAEpisodes = &ha.EpisodeStore{Dir: endpointcache.DirOptions{Override: episodeCacheDir(t)}}

	if err := d.run(); err != nil {
		t.Fatalf("a fresh HA VM with no recovery deferral must be deleted at once: %v", err)
	}
	if eventIndex(scene.EventsSnapshot(), "DeleteQemu") == -1 {
		t.Fatalf("the VM was not destroyed; events: %v", scene.EventsSnapshot())
	}
}

// TestHandleDeleteVM_DeadNodeCompletionClearsTheDeferral completes a delete
// past the grace window by moving the config onto a live node. The deferral
// the gate recorded on the way is spent, so the completion clears it.
func TestHandleDeleteVM_DeadNodeCompletionClearsTheDeferral(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.pin("pve-node1,pve-node2", nil)
	f.configs["pve-node1"] = map[string]any{"scsi0": "ceph:vm-101-disk-0,size=10G"}

	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete on a live node: %v", err)
	}
	f.requireCompletedOn("pve-node1")
	f.requireDeferralHeld(false)
}
